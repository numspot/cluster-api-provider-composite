// Copyright 2026 Numspot.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

func pinnedProviderFixture(t *testing.T) (*infrav1.CompositeCluster, *infrav1.CompositeProvider, *attachmentBinding) {
	t.Helper()
	cc, bindings := attachmentBindingsFixture()
	previous := bindings.Providers["regional"]
	previous.CredentialsName, previous.Zones = "tenant-credentials", []string{"zone-a", "zone-b"}
	raw, _ := json.Marshal(previous) // Only bounded strings, integers, booleans and slices.
	p := &infrav1.CompositeProvider{ObjectMeta: metav1.ObjectMeta{
		Name: "regional", Namespace: cc.Namespace, UID: previous.UID,
		Annotations: map[string]string{infrav1.ProviderBindingAnnotation: string(raw)},
		Finalizers:  []string{infrav1.ProviderAttachmentFinalizer},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: infrav1.GroupVersion.String(), Kind: compositeClusterKind,
			Name: cc.Name, UID: cc.UID, Controller: new(true),
		}},
	}}
	return cc, p, previous
}

func TestAttachmentPinAcceptsEquivalentJSON(t *testing.T) {
	cc, p, previous := pinnedProviderFixture(t)
	raw := p.Annotations[infrav1.ProviderBindingAnnotation]
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &fields))
	reordered, err := json.Marshal(fields)
	require.NoError(t, err)
	formatted, err := json.MarshalIndent(fields, "", "  ")
	require.NoError(t, err)
	for name, equivalent := range map[string]string{
		"compact": raw, "trailing-newline": raw + "\n", "surrounding-whitespace": " \n\t" + raw + "\r\n ",
		"reordered": string(reordered), "formatted": string(formatted),
		"escaped-identity": strings.Replace(raw, "infra-uid", `infra\u002duid`, 1),
		"explicit-default": strings.TrimSuffix(raw, "}") + `,"retired":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			provider := p.DeepCopy()
			provider.Annotations[infrav1.ProviderBindingAnnotation] = equivalent
			before, pinned := provider.DeepCopy(), *previous
			reason, err := checkAttachmentPin(cc, provider, previous)
			require.NoError(t, err)
			assert.Empty(t, reason)
			assert.Equal(t, before, provider, "validation must not mutate provider metadata")
			assert.Equal(t, pinned, *previous, "validation must not rewrite durable identity")
		})
	}
}

func TestAttachmentPinRejectsInvalidJSON(t *testing.T) {
	cc, p, previous := pinnedProviderFixture(t)
	raw := p.Annotations[infrav1.ProviderBindingAnnotation]
	for name, invalid := range map[string]string{
		"malformed": `{"uid":"RAW-SECRET`, "null": `null`, "missing-fields": `{}`, "empty": "", "whitespace-only": " \n\t",
		"unknown-field":          strings.TrimSuffix(raw, "}") + `,"fingerprint":"RAW-SECRET"}`,
		"unknown-binding-field":  strings.Replace(raw, `"profileUID":`, `"profileHash":"RAW-SECRET","profileUID":`, 1),
		"unknown-revision-field": strings.Replace(raw, `"generation":1`, `"generation":1,"hash":"RAW-SECRET"`, 1),
		"trailing-object":        raw + " {}", "trailing-null": raw + " null", "trailing-garbage": raw + " RAW-SECRET",
		"fractional-generation": strings.Replace(raw, `"generation":1`, `"generation":1.1`, 1),
		"overflow-generation":   strings.Replace(raw, `"generation":1`, `"generation":9223372036854775808`, 1),
		"duplicate-uid":         strings.Replace(raw, `"uid":"attachment-uid"`, `"uid":"RAW-SECRET","uid":"attachment-uid"`, 1),
		"escaped-duplicate-uid": strings.Replace(raw, `"uid":"attachment-uid"`, `"u\u0069d":"RAW-SECRET","uid":"attachment-uid"`, 1),
		"case-alias-uid":        strings.Replace(raw, `"uid":"attachment-uid"`, `"UID":"RAW-SECRET","uid":"attachment-uid"`, 1),
		"duplicate-generation":  strings.Replace(raw, `"generation":1`, `"generation":2,"generation":1`, 1),
		"duplicate-binding":     strings.Replace(raw, `"binding":`, `"binding":null,"binding":`, 1),
		"non-json-whitespace":   raw + "\u00a0",
		"oversized":             raw + strings.Repeat(" ", maxProviderBindingsBytes),
	} {
		t.Run(name, func(t *testing.T) {
			provider := p.DeepCopy()
			provider.Annotations[infrav1.ProviderBindingAnnotation] = invalid
			reason, err := checkAttachmentPin(cc, provider, previous)
			require.ErrorIs(t, err, errProfilePinMissing)
			assert.Equal(t, infrav1.ProfileMismatchReason, reason)
			message := diagnosticMessage(err)
			assert.Contains(t, message, "Provider-local")
			assert.Contains(t, message, "CompositeCluster")
			assert.NotContains(t, message, "RAW-SECRET")
		})
	}
}

func TestAttachmentPinRejectsInvalidUTF8(t *testing.T) {
	cc, p, previous := pinnedProviderFixture(t)
	previous.Binding.Account = "\ufffd"
	raw, _ := json.Marshal(previous)
	p.Annotations[infrav1.ProviderBindingAnnotation] = strings.Replace(string(raw), "\ufffd", "\xff", 1)
	_, err := checkAttachmentPin(cc, p, previous)
	require.ErrorIs(t, err, errProfilePinMissing, "invalid UTF-8 must not be repaired into a matching identity")
}

func TestAttachmentPinRejectsChangedIdentity(t *testing.T) {
	for name, change := range map[string]func(*attachmentBinding){
		"provider-uid":         func(b *attachmentBinding) { b.UID = "replacement-provider" },
		"cluster-uid":          func(b *attachmentBinding) { b.ClusterUID = "replacement-cluster" },
		"profile-uid":          func(b *attachmentBinding) { b.Binding.ProfileUID = "replacement-profile" },
		"profile-name":         func(b *attachmentBinding) { b.Binding.ProfileName = "another-profile" },
		"infra-uid":            func(b *attachmentBinding) { b.Binding.InfrastructureRevision.UID = "replacement-infra" },
		"infra-generation":     func(b *attachmentBinding) { b.Binding.InfrastructureRevision.Generation++ },
		"bootstrap-uid":        func(b *attachmentBinding) { b.Binding.BootstrapRevision.UID = "replacement-bootstrap" },
		"bootstrap-generation": func(b *attachmentBinding) { b.Binding.BootstrapRevision.Generation++ },
		"group":                func(b *attachmentBinding) { b.Binding.APIGroup = "another.test" },
		"kind":                 func(b *attachmentBinding) { b.Binding.Kind = "OtherCluster" },
		"child-name":           func(b *attachmentBinding) { b.Binding.ChildName = "another-child" },
		"bootstrap-name":       func(b *attachmentBinding) { b.Binding.BootstrapName = "another-bootstrap" },
		"logical-provider":     func(b *attachmentBinding) { b.Binding.Provider = "another" },
		"account":              func(b *attachmentBinding) { b.Binding.Account = "another-account" },
		"region":               func(b *attachmentBinding) { b.Binding.Region = "another-region" },
		"credentials":          func(b *attachmentBinding) { b.CredentialsName = "another-secret" },
		"zones":                func(b *attachmentBinding) { b.Zones = []string{"zone-c"} },
		"zone-order":           func(b *attachmentBinding) { b.Zones = []string{"zone-b", "zone-a"} },
		"retirement":           func(b *attachmentBinding) { b.Retired = true },
	} {
		t.Run(name, func(t *testing.T) {
			cc, p, previous := pinnedProviderFixture(t)
			changed := *previous
			change(&changed)
			raw, err := json.Marshal(changed)
			require.NoError(t, err)
			p.Annotations[infrav1.ProviderBindingAnnotation] = string(raw)
			reason, err := checkAttachmentPin(cc, p, previous)
			require.ErrorIs(t, err, errProfilePinMissing)
			assert.Equal(t, infrav1.ProfileMismatchReason, reason)
			assert.Contains(t, diagnosticMessage(err), "Provider-local")
		})
	}
}

func TestAttachmentPinRetainsOwnershipAndHistoryGuards(t *testing.T) {
	cc, p, previous := pinnedProviderFixture(t)
	foreign := p.DeepCopy()
	foreign.OwnerReferences[0].UID = "foreign-cluster"
	reason, err := checkAttachmentPin(cc, foreign, previous)
	require.ErrorIs(t, err, errProfileMismatch)
	assert.Equal(t, infrav1.ChildNotOwnedReason, reason)
	_, err = checkAttachmentPin(cc, p, nil)
	require.ErrorIs(t, err, errProfilePinMissing)
	assert.Contains(t, diagnosticMessage(err), "CompositeCluster")
	delete(p.Annotations, infrav1.ProviderBindingAnnotation)
	_, err = checkAttachmentPin(cc, p, previous)
	require.ErrorIs(t, err, errProfilePinMissing)
	assert.Contains(t, diagnosticMessage(err), "Provider-local")
}

func TestAttachmentProviderIdentityDiagnostics(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(fmt.Sprintf("retired=%t", retired), func(t *testing.T) {
			cc, p, previous := pinnedProviderFixture(t)
			previous.Retired = retired
			p.UID = "replacement-provider"
			bindings := attachmentBindings{Version: 1, Providers: map[string]*attachmentBinding{p.Name: previous}}
			plan := newAttachmentPlan([]infrav1.CompositeProvider{*p}, bindings)
			r := &CompositeClusterReconciler{}
			failure := r.prepareAttachmentPlans(t.Context(), cc, []infrav1.CompositeProvider{*p}, plan)
			require.NotNil(t, failure)
			require.ErrorIs(t, failure.cause, errProfileMismatch)
			assert.Equal(t, infrav1.ProfileMismatchReason, failure.reason)
			message := attachmentDiagnostic(failure)
			for _, identity := range []string{"CompositeCluster", p.Namespace + "/" + p.Name, string(previous.UID), string(p.UID)} {
				assert.Contains(t, message, identity)
			}
			if retired {
				assert.Contains(t, message, "retired")
				assert.Contains(t, message, "reserved")
			} else {
				assert.Contains(t, message, "UID mismatch")
				assert.NotContains(t, message, "source-template")
			}
			wrapped := errors.Join(errors.New("RAW-SECRET"), fmt.Errorf("RAW-SECRET: %w", failure.cause))
			safe := safeReconcileError(wrapped)
			assert.ErrorIs(t, safe, errProfileMismatch)
			assert.Equal(t, message, safe.Error())
			assert.NotContains(t, safe.Error(), "RAW-SECRET")
			assert.Empty(t, plan.providers)
			assert.Equal(t, previous, plan.bindings.Providers[p.Name])
		})
	}
}

func TestProfileRevisionMismatchDiagnostics(t *testing.T) {
	for _, scenario := range []string{"profile", "infrastructure-uid", "infrastructure-generation", "bootstrap-uid", "bootstrap-generation"} {
		t.Run(scenario, func(t *testing.T) {
			catalog := catalogProfile()
			cc, bindings := attachmentBindingsFixture()
			previous := bindings.Providers["regional"].Binding
			previous.ProfileName, previous.ProfileUID = catalog.Name, catalog.UID
			previous.APIGroup, previous.Kind = catalog.Spec.InfraClusterKind.APIGroup, catalog.Spec.InfraClusterKind.Kind
			previous.Provider, previous.Account, previous.Region = string(catalog.Spec.Provider), catalog.Spec.Account, catalog.Spec.Region
			sources := &profileTemplates{infrastructure: previous.InfrastructureRevision, bootstrap: previous.BootstrapRevision}
			entry := &infrav1.ProviderSpec{Name: "regional"}
			switch scenario {
			case "profile":
				catalog.UID = "replacement-profile"
			case "infrastructure-uid":
				sources.infrastructure.UID = "replacement-infra"
			case "infrastructure-generation":
				sources.infrastructure.Generation++
			case "bootstrap-uid":
				sources.bootstrap.UID = "replacement-bootstrap"
			case "bootstrap-generation":
				sources.bootstrap.Generation++
			}
			want := revisionDiagnosticEvidence(scenario, catalog, &previous, sources)
			_, err := profileBindingFor(cc, entry, catalog, sources, &previous)
			require.ErrorIs(t, err, errProfileMismatch)
			for _, candidate := range []error{err, fmt.Errorf("RAW-SECRET: %w", err), errors.Join(errors.New("RAW-SECRET"), err)} {
				message := diagnosticMessage(candidate)
				for _, text := range want {
					assert.Contains(t, message, text)
				}
				assert.LessOrEqual(t, len(message), 1024)
				assert.NotContains(t, message, "RAW-SECRET")
				assert.ErrorIs(t, safeReconcileError(candidate), errProfileMismatch)
				assert.Equal(t, message, safeReconcileError(candidate).Error())
			}
		})
	}
}

func revisionDiagnosticEvidence(
	scenario string,
	catalog *infrav1.CompositeProviderTemplate,
	previous *providerBinding,
	sources *profileTemplates,
) []string {
	want := []string{"pinned", "live"}
	if scenario == "profile" {
		return append(want, "CompositeProviderTemplate", catalog.Name, string(previous.ProfileUID), string(catalog.UID))
	}
	label, ref := "Infrastructure source", catalog.Spec.InfrastructureTemplateRef
	pinned, live := previous.InfrastructureRevision, sources.infrastructure
	if strings.HasPrefix(scenario, "bootstrap") {
		label, ref = "Bootstrap source", catalog.Spec.BootstrapTemplateRef
		pinned, live = previous.BootstrapRevision, sources.bootstrap
	}
	return append(want, label, ref.APIVersion, ref.Kind, ref.Namespace+"/"+ref.Name,
		string(pinned.UID), string(live.UID), fmt.Sprintf("generation=%d", pinned.Generation), fmt.Sprintf("generation=%d", live.Generation))
}

func TestPinDiagnosticsBoundAndAllowlistUntrustedIdentities(t *testing.T) {
	cc, p, previous := pinnedProviderFixture(t)
	previous.UID = "pinned\nRAW-SECRET"
	p.UID = "live\x00RAW-SECRET"
	p.Name, p.Namespace = strings.Repeat("n", 1024)+"RAW-SECRET", "outside\nRAW-SECRET"
	plan := newAttachmentPlan(
		[]infrav1.CompositeProvider{*p},
		attachmentBindings{Version: 1, Providers: map[string]*attachmentBinding{p.Name: previous}},
	)
	r := &CompositeClusterReconciler{}
	failure := r.prepareAttachmentPlans(t.Context(), cc, []infrav1.CompositeProvider{*p}, plan)
	require.NotNil(t, failure)
	message := attachmentDiagnostic(failure)
	assert.Contains(t, message, "UID mismatch")
	assert.NotContains(t, message, "RAW-SECRET")
	assert.NotContains(t, message, "\n")
	assert.NotContains(t, message, "\x00")
	assert.LessOrEqual(t, len(message), 1024)

	catalog := catalogProfile()
	sources := &profileTemplates{
		infrastructure: sourceRevision{UID: "live\nRAW-SECRET", Generation: 2},
		bootstrap:      previous.Binding.BootstrapRevision,
	}
	initial, err := profileBindingFor(cc, &infrav1.ProviderSpec{Name: "regional"}, catalog, sources, nil)
	require.NoError(t, err)
	initial.InfrastructureRevision = sourceRevision{UID: "pinned\nRAW-SECRET", Generation: 1}
	catalog.Spec.InfrastructureTemplateRef = infrav1.ProviderTemplateReference{
		APIVersion: "infra.test/v1\nRAW-SECRET", Kind: "FakeTemplate\nRAW-SECRET",
		Namespace: "outside\nRAW-SECRET", Name: strings.Repeat("n", 1024) + "RAW-SECRET",
	}
	_, err = profileBindingFor(cc, &infrav1.ProviderSpec{Name: "regional"}, catalog, sources, &initial)
	require.ErrorIs(t, err, errProfileMismatch)
	message = diagnosticMessage(err)
	assert.Contains(t, message, "Infrastructure source")
	assert.Contains(t, message, "generation=1")
	assert.Contains(t, message, "generation=2")
	assert.NotContains(t, message, "RAW-SECRET")
	assert.NotContains(t, message, "\n")
	assert.LessOrEqual(t, len(message), 1024)
}

func TestPinDiagnosticsBoundMaximumIdentity(t *testing.T) {
	catalog := catalogProfile()
	cc, _ := attachmentBindingsFixture()
	entry := &infrav1.ProviderSpec{Name: "regional"}
	sources := &profileTemplates{
		infrastructure: sourceRevision{UID: types.UID(strings.Repeat("a", 128)), Generation: 9223372036854775807},
		bootstrap:      sourceRevision{UID: "bootstrap-uid", Generation: 1},
	}
	initial, err := profileBindingFor(cc, entry, catalog, sources, nil)
	require.NoError(t, err)
	initial.InfrastructureRevision = sourceRevision{UID: types.UID(strings.Repeat("b", 128)), Generation: 9223372036854775806}
	catalog.Spec.InfrastructureTemplateRef = infrav1.ProviderTemplateReference{
		APIVersion: strings.Repeat("g", 124) + "/v1", Kind: strings.Repeat("K", 63),
		Namespace: strings.Repeat("n", 63), Name: strings.Repeat("n", 128),
	}
	_, err = profileBindingFor(cc, entry, catalog, sources, &initial)
	require.ErrorIs(t, err, errProfileMismatch)
	message := diagnosticMessage(err)
	assert.Contains(t, message, "Infrastructure source")
	assert.Contains(t, message, "generation=9223372036854775806")
	assert.Contains(t, message, "generation=9223372036854775807")
	assert.Contains(t, message, string(initial.InfrastructureRevision.UID))
	assert.Contains(t, message, string(sources.infrastructure.UID))
	assert.LessOrEqual(t, len(message), 1024, "bounded identities must not rely on clipping away the live revision")
	assert.Equal(t, message, safeReconcileError(err).Error())
}
