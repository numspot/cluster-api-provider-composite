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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
	"github.com/numspot/cluster-api-provider-composite/internal/mint"
	"github.com/numspot/cluster-api-provider-composite/internal/profile"
)

type attachmentBinding struct {
	UID             types.UID       `json:"uid"`
	ClusterUID      types.UID       `json:"clusterUID"`
	Binding         providerBinding `json:"binding"`
	CredentialsName string          `json:"credentialsName,omitempty"`
	Zones           []string        `json:"zones,omitempty"`
	Retired         bool            `json:"retired,omitempty"`
}

type attachmentBindings struct {
	Version   int                           `json:"version"`
	Providers map[string]*attachmentBinding `json:"providers"`
}

type plannedAttachment struct {
	provider *infrav1.CompositeProvider
	plan     providerPlan
	paused   bool
	failure  *attachmentFailure
}

type attachmentFailure struct {
	reason string
	cause  error
}

type attachmentPlan struct {
	bindings  attachmentBindings
	providers []plannedAttachment
	retiring  map[string]*infrav1.CompositeProvider
	failure   *attachmentFailure
}

func readAttachmentBindings(cc *infrav1.CompositeCluster) (attachmentBindings, error) {
	b := attachmentBindings{Version: 1, Providers: map[string]*attachmentBinding{}}
	raw := cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
	if raw == "" {
		return b, nil
	}
	if len(raw) > maxProviderBindingsBytes {
		return b, errInvalidComposition
	}
	// Decode into a zero value so null or missing envelope fields cannot inherit
	// the defaults reserved for an absent annotation.
	b = attachmentBindings{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b); err != nil {
		return b, errInvalidComposition
	}
	if _, err := encodeAttachmentBindings(cc, b); err != nil {
		return b, err
	}
	if strings.TrimSpace(raw[decoder.InputOffset():]) != "" {
		return b, errInvalidComposition
	}
	return b, nil
}

func encodeAttachmentBindings(cc *infrav1.CompositeCluster, b attachmentBindings) (string, error) {
	if b.Version != 1 || b.Providers == nil || len(b.Providers) > maxProviderBindings {
		return "", errInvalidComposition
	}
	for name, entry := range b.Providers {
		if !validAttachmentBinding(cc, name, entry) {
			return "", errInvalidComposition
		}
	}
	raw, _ := json.Marshal(b) // Only bounded strings, integers, booleans and maps.
	if len(raw) > maxProviderBindingsBytes {
		return "", errInvalidComposition
	}
	return string(raw), nil
}

func validAttachmentBinding(cc *infrav1.CompositeCluster, name string, entry *attachmentBinding) bool {
	if entry == nil || len(validation.IsDNS1123Label(name)) != 0 || !validSourceUID(entry.UID) || entry.ClusterUID != cc.UID {
		return false
	}
	if !validProviderTarget(&entry.Binding) || !validBindingRecord(&entry.Binding) ||
		entry.Binding.BootstrapName != mint.ClusterKCTName(cc, &infrav1.ProviderSpec{Name: infrav1.ProviderName(name)}) {
		return false
	}
	return validAttachmentFields(entry)
}

func validAttachmentFields(entry *attachmentBinding) bool {
	if len(entry.Zones) > 16 || (entry.CredentialsName != "" && len(validation.IsDNS1123Subdomain(entry.CredentialsName)) != 0) {
		return false
	}
	for _, zone := range entry.Zones {
		if zone == "" || len(zone) > 256 {
			return false
		}
	}
	return true
}

func (r *CompositeClusterReconciler) attachmentProviders(ctx context.Context, cc *infrav1.CompositeCluster) ([]infrav1.CompositeProvider, error) {
	var providers []infrav1.CompositeProvider
	continuation := ""
	for {
		list := &infrav1.CompositeProviderList{}
		if err := r.apiReader().List(ctx, list, client.InNamespace(cc.Namespace), client.Limit(100), client.Continue(continuation)); err != nil {
			return nil, fmt.Errorf("reading provider attachments: %w", err)
		}
		for i := range list.Items {
			if list.Items[i].Spec.ClusterRef.Name == cc.Name {
				providers = append(providers, list.Items[i])
			}
		}
		if list.Continue == "" {
			break
		}
		continuation = list.Continue
	}
	slices.SortFunc(providers, func(a, b infrav1.CompositeProvider) int { return strings.Compare(a.Name, b.Name) })
	return providers, nil
}

// Attachment configuration failures become provider conditions, not contract failures.
// Unreadable durable identity remains a hard error and cannot authorize writes.
func (r *CompositeClusterReconciler) planAttachments(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
) (*attachmentPlan, error) {
	bindings, err := readAttachmentBindings(cc)
	if err != nil {
		return nil, err
	}
	if failure := validateAttachmentPlan(cc, nil, bindings, nil); failure != nil {
		return nil, failure.cause
	}
	providers, err := r.attachmentProviders(ctx, cc)
	if err != nil {
		return nil, err
	}
	original := attachmentBindings{Version: bindings.Version, Providers: maps.Clone(bindings.Providers)}
	result := newAttachmentPlan(providers, bindings)
	selection := r.prepareAttachmentPlans(ctx, cc, providers, result)
	result.failure = validateAttachmentPlan(cc, providers, selection, result.providers)
	if result.failure != nil {
		result.bindings = original
	}
	return result, nil
}

func newAttachmentPlan(providers []infrav1.CompositeProvider, bindings attachmentBindings) *attachmentPlan {
	result := &attachmentPlan{bindings: bindings, retiring: map[string]*infrav1.CompositeProvider{}}
	seen := map[string]bool{}
	for i := range providers {
		p := &providers[i]
		seen[p.Name] = true
		if !p.DeletionTimestamp.IsZero() {
			result.retiring[p.Name] = p
		}
	}
	for name, binding := range bindings.Providers {
		if !seen[name] && !binding.Retired {
			result.retiring[name] = nil
		}
	}
	return result
}

func (r *CompositeClusterReconciler) prepareAttachmentPlans(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	providers []infrav1.CompositeProvider,
	result *attachmentPlan,
) attachmentBindings {
	// Failed known providers retain their durable identities. Also validate any
	// newly resolved identity whose payload is rejected, without persisting its pin.
	selection := attachmentBindings{Version: result.bindings.Version, Providers: maps.Clone(result.bindings.Providers)}
	for i := range providers {
		p := &providers[i]
		entry := plannedAttachment{provider: p}
		previous := result.bindings.Providers[p.Name]
		switch {
		case previous != nil && (previous.UID != p.UID || (previous.Retired && p.DeletionTimestamp.IsZero())):
			entry.failure = &attachmentFailure{infrav1.ProfileMismatchReason, errProfileMismatch}
			delete(result.retiring, p.Name)
		case !p.DeletionTimestamp.IsZero():
			continue
		case isPaused(nil, p):
			entry.paused = true
		default:
			prepared, binding, reason, err := r.prepareAttachment(ctx, cc, p, previous)
			entry.plan = prepared
			if binding != nil {
				selection.Providers[p.Name] = binding
			}
			if err != nil {
				entry.failure = &attachmentFailure{reason, err}
			} else {
				result.bindings.Providers[p.Name] = binding
			}
		}
		result.providers = append(result.providers, entry)
	}
	return selection
}

func validateAttachmentPlan(
	cc *infrav1.CompositeCluster,
	providers []infrav1.CompositeProvider,
	bindings attachmentBindings,
	plans []plannedAttachment,
) *attachmentFailure {
	names := make(map[string]bool, len(bindings.Providers))
	for name := range bindings.Providers {
		names[name] = true
	}
	for i := range providers {
		names[providers[i].Name] = true
	}
	err := validateAttachmentIdentities(cc, bindings)
	if len(names) > maxProviderBindings {
		err = errInvalidComposition
	}
	projection := attachmentProjection(bindings)
	for i := range plans {
		entry := &plans[i]
		if bindings.Providers[entry.provider.Name] == nil && entry.plan.spec != nil {
			// Resolving a profile establishes its declared native identity even
			// when a missing source or invalid local pin prevents a new binding.
			projection.providers = append(projection.providers, entry.plan)
		}
	}
	if err == nil {
		err = validateNativeIdentities(cc, projection)
	}
	if err == nil {
		_, err = projection.failureDomains()
	}
	if err == nil {
		return nil
	}
	return &attachmentFailure{infrav1.InvalidCompositionReason, errors.Join(errInvalidComposition, err)}
}

type providerProjection struct{ providers []providerPlan }

func attachmentProjection(bindings attachmentBindings) *providerProjection {
	result := &providerProjection{}
	for name, binding := range bindings.Providers {
		if !binding.Retired {
			result.providers = append(
				result.providers,
				providerPlan{
					spec:    &infrav1.ProviderSpec{Name: infrav1.ProviderName(name), Region: binding.Binding.Region, Zones: binding.Zones},
					binding: binding.Binding,
				},
			)
		}
	}
	return result
}

func (p *compositionPlan) allFailureDomains() ([]infrav1.FailureDomain, error) {
	if p.attachments == nil {
		return nil, nil
	}
	// Failed known attachments retain their durable placement observations;
	// successfully prepared attachments can add their own independent placement.
	domains, err := attachmentProjection(p.attachments.bindings).failureDomains()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(domains, func(a, b infrav1.FailureDomain) int { return strings.Compare(a.Name, b.Name) })
	return domains, nil
}

func (r *CompositeClusterReconciler) prepareAttachment(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	p *infrav1.CompositeProvider,
	previous *attachmentBinding,
) (providerPlan, *attachmentBinding, string, error) {
	if previous != nil {
		if reason, err := checkAttachmentPin(cc, p, previous); err != nil {
			return providerPlan{}, nil, reason, err
		}
	}
	template, err := r.readUsableProfile(ctx, p.Spec.TemplateRef.Name, previous != nil)
	if err != nil {
		return providerPlan{}, nil, infrav1.ProfileUnavailableReason, err
	}
	prepared := attachmentIdentityPlan(cc, p, template)
	if reason, err := checkAttachmentPin(cc, p, previous); err != nil {
		return prepared, nil, reason, err
	}
	return r.prepareAttachmentProfile(ctx, cc, p, template, &prepared, previous)
}

func attachmentIdentityPlan(
	cc *infrav1.CompositeCluster,
	p *infrav1.CompositeProvider,
	template *infrav1.CompositeProviderTemplate,
) providerPlan {
	spec := attachmentSpec(p, template)
	return providerPlan{spec: spec, binding: providerBinding{
		APIGroup: template.Spec.InfraClusterKind.APIGroup, Kind: template.Spec.InfraClusterKind.Kind,
		ChildName: mint.ClusterInfraName(cc, spec), Provider: string(template.Spec.Provider),
		Account: template.Spec.Account, Region: template.Spec.Region,
	}}
}

func (r *CompositeClusterReconciler) prepareAttachmentProfile(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	p *infrav1.CompositeProvider,
	template *infrav1.CompositeProviderTemplate,
	prepared *providerPlan,
	previous *attachmentBinding,
) (providerPlan, *attachmentBinding, string, error) {
	sources, err := resolveProfileTemplates(ctx, r.apiReader(), template)
	if err != nil {
		return *prepared, nil, infrav1.ProfileUnavailableReason, err
	}
	if len(template.Spec.CredentialsNamePaths) != 0 || p.Spec.CredentialsRef.Name != "" {
		sources.configuration.CredentialsRef = infrav1.ProviderCredentialsReference{
			Name:      p.Spec.CredentialsRef.Name,
			NamePaths: slices.Clone(template.Spec.CredentialsNamePaths),
		}
	}
	var old *providerBinding
	if previous != nil {
		old = &previous.Binding
	}
	native, err := profileBindingFor(cc, prepared.spec, template, sources, old)
	if err != nil {
		return *prepared, nil, profileBindingFailureReason(err), err
	}
	return r.resolveAttachmentPayload(ctx, cc, p, prepared.spec, sources, &native, previous)
}

func checkAttachmentPin(cc *infrav1.CompositeCluster, p *infrav1.CompositeProvider, previous *attachmentBinding) (string, error) {
	if owner := metav1.GetControllerOf(p); owner != nil && !attachmentOwnerMatches(cc, owner) {
		return infrav1.ChildNotOwnedReason, errProfileMismatch
	}
	if raw := p.Annotations[infrav1.ProviderBindingAnnotation]; raw != "" {
		encoded, _ := json.Marshal(previous)
		if previous == nil || raw != string(encoded) {
			return infrav1.ProfileMismatchReason, errProfilePinMissing
		}
	} else if previous != nil && controllerutil.ContainsFinalizer(p, infrav1.ProviderAttachmentFinalizer) {
		return infrav1.ProfileMismatchReason, errProfilePinMissing
	}
	return "", nil
}

func attachmentOwnerMatches(cc *infrav1.CompositeCluster, owner *metav1.OwnerReference) bool {
	return owner.UID == cc.UID && owner.Name == cc.Name && owner.Kind == compositeClusterKind &&
		schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind).Group == infrav1.GroupVersion.Group
}

func attachmentSpec(p *infrav1.CompositeProvider, template *infrav1.CompositeProviderTemplate) *infrav1.ProviderSpec {
	return &infrav1.ProviderSpec{
		Name: infrav1.ProviderName(p.Name), ProviderRef: p.Spec.TemplateRef,
		InfraClusterName: p.Spec.InfraClusterName, InfraSpec: p.Spec.InfraSpec, Bootstrap: p.Spec.Bootstrap,
		Region: template.Spec.Region, Zones: slices.Clone(template.Spec.Zones), EndpointPaths: slices.Clone(template.Spec.EndpointPaths),
	}
}

func (r *CompositeClusterReconciler) resolveAttachmentPayload(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	p *infrav1.CompositeProvider,
	spec *infrav1.ProviderSpec,
	sources *profileTemplates,
	native *providerBinding,
	previous *attachmentBinding,
) (providerPlan, *attachmentBinding, string, error) {
	zones := slices.Clone(spec.Zones)
	slices.Sort(zones)
	binding := &attachmentBinding{UID: p.UID, ClusterUID: cc.UID, Binding: *native, CredentialsName: p.Spec.CredentialsRef.Name, Zones: zones}
	if previous != nil && !reflect.DeepEqual(binding, previous) {
		return providerPlan{}, nil, infrav1.ProfileMismatchReason, errProfileMismatch
	}
	payload, err := profile.Resolve(&sources.configuration, cc.Spec.Bootstrap, &spec.InfraSpec, spec.Bootstrap)
	if err != nil {
		return providerPlan{}, binding, infrav1.OverrideRejectedReason, fmt.Errorf("resolving attachment overrides: %w", err)
	}
	if previous == nil {
		if err := r.checkUnboundProfileResources(ctx, cc, native); err != nil {
			return providerPlan{}, binding, profileBindingFailureReason(err), err
		}
	}
	return providerPlan{spec: spec, payload: payload, binding: *native}, binding, "", nil
}

func validateAttachmentIdentities(cc *infrav1.CompositeCluster, attachments attachmentBindings) error {
	if err := validateNativeIdentities(cc, attachmentProjection(attachments)); err != nil {
		return err
	}
	_, err := encodeAttachmentBindings(cc, attachments)
	return err
}

func validateNativeIdentities(cc *infrav1.CompositeCluster, projection *providerProjection) error {
	combined := make(map[string]providerBinding, len(projection.providers))
	used := map[nativeChildIdentity]bool{}
	canonical := map[schema.GroupKind]bool{}
	for i := range projection.providers {
		p := &projection.providers[i]
		binding := p.binding
		combined[string(p.spec.Name)] = binding
		identity := nativeChildIdentity{groupKind: binding.groupKind(), name: binding.ChildName}
		if used[identity] {
			return errInvalidComposition
		}
		used[identity] = true
		canonical[binding.groupKind()] = canonical[binding.groupKind()] || identity.name == cc.Name
	}
	return validateCanonicalBindings(combined, canonical)
}

func attachmentDiagnostic(failure *attachmentFailure) string {
	if errors.Is(failure.cause, profile.ErrInvalidCredentials) {
		return "Credential reference is invalid; check the Secret name and the template's native target paths."
	}
	return diagnosticMessage(failure.cause)
}

func attachmentBindingsForPersistence(cc *infrav1.CompositeCluster, plan *attachmentPlan) (string, error) {
	raw, err := encodeAttachmentBindings(cc, plan.bindings)
	if err != nil {
		return "", err
	}
	if len(plan.bindings.Providers) == 0 && slices.ContainsFunc(plan.providers, func(entry plannedAttachment) bool { return entry.failure != nil }) {
		// A failed first attachment does not create binding history. A truly
		// empty composition still persists its normal empty envelope.
		return cc.Annotations[infrav1.ProviderAttachmentsAnnotation], nil
	}
	return raw, nil
}

// Persist the complete attachment identity and cluster finalizer atomically,
// before any normal status snapshot or native child write.
func (r *CompositeClusterReconciler) persistCompositionBindings(ctx context.Context, cc *infrav1.CompositeCluster, plan *attachmentPlan) error {
	if plan == nil || plan.failure != nil {
		return nil
	}
	raw, err := attachmentBindingsForPersistence(cc, plan)
	if err != nil {
		return err
	}
	added := cc.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(cc, infrav1.ClusterFinalizer)
	if raw == cc.Annotations[infrav1.ProviderAttachmentsAnnotation] && !added {
		return nil
	}
	if cc.Annotations == nil {
		cc.Annotations = map[string]string{}
	}
	if raw != "" {
		cc.Annotations[infrav1.ProviderAttachmentsAnnotation] = raw
	}
	if err := r.Update(ctx, cc); err != nil {
		return fmt.Errorf("persisting provider attachments: %w", err)
	}
	return nil
}
