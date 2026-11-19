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

package envtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

func isolationProfile(t *testing.T, zone string) *infrav1.CompositeProviderTemplate {
	t.Helper()
	profile := regionalProfileDefinition()
	profile.Spec.Zones = []string{zone}
	createRegionalProfile(t, k8s, profile)
	protectRegionalProfile(t, profile)
	return profile
}

func attachmentRecord(t *testing.T, cc *infrav1.CompositeCluster) map[string]json.RawMessage {
	t.Helper()
	var record struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(cc.Annotations[infrav1.ProviderAttachmentsAnnotation]), &record))
	return record.Providers
}

func TestProviderAttachment_IsolatesPinFailureFromNewAndRecoveredProviders(t *testing.T) {
	firstProfile := isolationProfile(t, "zone-first")
	secondProfile := isolationProfile(t, "zone-second")
	thirdProfile := isolationProfile(t, "zone-third")
	cluster := newCAPICluster(t, newNamespace(t), "independent-attachments")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	setEndpoint(t, cc, cluster, "192.0.2.80", 6443)
	first := createProviderAttachment(t, cc, firstProfile.Name, "a-first")
	secondName := cc.Name + "-second"
	second := createProviderAttachment(t, cc, secondProfile.Name, "b-second", map[string]any{"infraClusterName": secondName})
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	initialRecord := attachmentRecord(t, cc)
	firstPin := getProviderAttachment(t, first).Annotations[infrav1.ProviderBindingAnnotation]
	secondPin := getProviderAttachment(t, second).Annotations[infrav1.ProviderBindingAnnotation]
	firstChild := getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name)
	require.NotNil(t, firstChild)
	requireCondition(
		t,
		getProviderAttachment(t, second).Status.Conditions,
		infrav1.ReadyCondition,
		metav1.ConditionFalse,
		infrav1.WaitingForProvidersReason,
	)

	broken := getProviderAttachment(t, first)
	broken.Annotations[infrav1.ProviderBindingAnnotation] = `{"invalid":true}`
	require.NoError(t, k8s.Update(t.Context(), broken))
	thirdName := cc.Name + "-third"
	third := createProviderAttachment(t, cc, thirdProfile.Name, "c-third", map[string]any{"infraClusterName": thirdName})
	otherFailure := createProviderAttachment(t, cc, "missing-template", "z-unavailable")
	markFakeClusterReady(t, cc.Namespace, secondName)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(
		t,
		getProviderAttachment(t, first).Status.Conditions,
		infrav1.ReadyCondition,
		metav1.ConditionFalse,
		infrav1.ProfileMismatchReason,
	)
	requireCondition(
		t,
		getProviderAttachment(t, otherFailure).Status.Conditions,
		infrav1.ReadyCondition,
		metav1.ConditionFalse,
		infrav1.ProfileUnavailableReason,
	)
	recovered := getProviderAttachment(t, second)
	requireCondition(t, recovered.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	require.NotNil(t, recovered.Status.Initialization.Provisioned)
	assert.True(t, *recovered.Status.Initialization.Provisioned)
	assert.Equal(t, secondPin, recovered.Annotations[infrav1.ProviderBindingAnnotation])
	requireCondition(t, cc.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	require.NotNil(t, getUnstructured(t, fakeClusterGVK, cc.Namespace, thirdName))
	newProvider := getProviderAttachment(t, third)
	assert.NotEmpty(t, newProvider.Annotations[infrav1.ProviderBindingAnnotation])
	assert.Contains(t, newProvider.Finalizers, infrav1.ProviderAttachmentFinalizer)
	require.NotNil(t, getUnstructured(t, kubeadmConfigTemplateGVK, cc.Namespace, cc.Name+"-c-third"))
	currentRecord := attachmentRecord(t, cc)
	assert.JSONEq(t, string(initialRecord["a-first"]), string(currentRecord["a-first"]))
	assert.JSONEq(t, string(initialRecord["b-second"]), string(currentRecord["b-second"]))
	assert.NotContains(t, currentRecord, "z-unavailable", "first failure must not manufacture durable history")
	assert.Equal(t, firstChild.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name).Object)
	assert.Equal(t, `{"invalid":true}`, getProviderAttachment(t, first).Annotations[infrav1.ProviderBindingAnnotation])

	markFakeClusterReady(t, cc.Namespace, thirdName)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(t, getProviderAttachment(t, third).Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	beforeCluster := cc.DeepCopy()
	beforeFirst := getProviderAttachment(t, first)
	beforeSecond := getProviderAttachment(t, second)
	beforeThird := getProviderAttachment(t, third)
	beforeOther := getProviderAttachment(t, otherFailure)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	assert.Equal(t, beforeCluster.ResourceVersion, cc.ResourceVersion, "unchanged failures are not heartbeat writes")
	for _, before := range []*infrav1.CompositeProvider{beforeFirst, beforeSecond, beforeThird, beforeOther} {
		assert.Equal(t, before, getProviderAttachment(t, before))
	}

	// Restore verified test metadata, without changing either durable identity.
	broken = getProviderAttachment(t, first)
	broken.Annotations[infrav1.ProviderBindingAnnotation] = firstPin
	require.NoError(t, k8s.Update(t.Context(), broken))
	markFakeClusterReady(t, cc.Namespace, cc.Name)
	_, _, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(t, getProviderAttachment(t, first).Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
}

func TestProviderAttachment_IsolatesUnavailableProfilesAndReportsEveryFailure(t *testing.T) {
	profile := isolationProfile(t, "zone-valid")
	cluster := newCAPICluster(t, newNamespace(t), "unavailable-attachments")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	setEndpoint(t, cc, cluster, "192.0.2.81", 6443)
	first := createProviderAttachment(t, cc, "missing-first", "a-unavailable")
	valid := createProviderAttachment(t, cc, profile.Name, "b-valid")
	last := createProviderAttachment(t, cc, "missing-last", "z-unavailable")
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	for _, failed := range []client.Object{first, last} {
		p := getProviderAttachment(t, failed)
		requireCondition(t, p.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionFalse, infrav1.ProfileUnavailableReason)
		assert.Empty(t, p.Annotations[infrav1.ProviderBindingAnnotation])
		assert.Empty(t, p.Finalizers)
	}
	require.NotNil(t, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name))
	markFakeClusterReady(t, cc.Namespace, cc.Name)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(t, getProviderAttachment(t, valid).Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	requireCondition(t, cc.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
}

func TestProviderAttachment_IsolationPreservesGlobalValidation(t *testing.T) {
	for _, scenario := range []string{"collision", "missing-canonical", "invalid-override-collision", "missing-source-collision", "pin-collision", "malformed-record"} {
		t.Run(scenario, func(t *testing.T) {
			profile := isolationProfile(t, "zone-first")
			otherProfile := isolationProfile(t, "zone-other")
			cluster := newCAPICluster(t, newNamespace(t), "global-validation")
			cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
			unavailable := createProviderAttachment(t, cc, "missing-template", "a-unavailable")
			name := cc.Name
			if scenario == "missing-canonical" {
				name += "-regional"
			}
			valid := createProviderAttachment(t, cc, profile.Name, "b-valid", map[string]any{"infraClusterName": name})
			providers := []client.Object{unavailable, valid}
			if collision := isolationCollision(t, cc, otherProfile, scenario); collision != nil {
				providers = append(providers, collision)
			}
			if scenario == "malformed-record" {
				cc.Annotations = map[string]string{infrav1.ProviderAttachmentsAnnotation: `{"version":1,"providers":{"invalid":null}}`}
				require.NoError(t, k8s.Update(t.Context(), cc))
			}
			before := cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
			_, cc, err := reconcileCluster(t, newClusterReconciler(), cc)
			require.NoError(t, err)
			assert.Equal(t, before, cc.Annotations[infrav1.ProviderAttachmentsAnnotation])
			assert.NotContains(t, cc.Finalizers, infrav1.ClusterFinalizer, "invalid global composition must precede every metadata write")
			for _, provider := range providers {
				p := getProviderAttachment(t, provider)
				if scenario == "pin-collision" && provider.GetName() == "c-collision" {
					assert.Equal(t, `{"invalid":true}`, p.Annotations[infrav1.ProviderBindingAnnotation])
				} else {
					assert.Empty(t, p.Annotations[infrav1.ProviderBindingAnnotation])
				}
				assert.Empty(t, p.Finalizers)
				assert.Empty(t, p.OwnerReferences)
				assert.Nil(t, getUnstructured(t, kubeadmConfigTemplateGVK, cc.Namespace, cc.Name+"-"+provider.GetName()))
			}
			assert.Nil(t, getUnstructured(t, fakeClusterGVK, cc.Namespace, name))
		})
	}
}

func isolationCollision(t *testing.T, cc *infrav1.CompositeCluster, profile *infrav1.CompositeProviderTemplate, scenario string) client.Object {
	t.Helper()
	if scenario == "missing-canonical" || scenario == "malformed-record" {
		return nil
	}
	override := map[string]any{}
	if scenario == "invalid-override-collision" {
		override["infraSpec"] = map[string]any{"identityRef": map[string]any{"name": "forbidden"}}
	}
	collision := createProviderAttachment(t, cc, profile.Name, "c-collision", override)
	switch scenario {
	case "missing-source-collision":
		ref := profile.Spec.InfrastructureTemplateRef
		require.NoError(t, k8s.Delete(t.Context(), getUnstructured(t, schemaForSource(ref), ref.Namespace, ref.Name)))
	case "pin-collision":
		p := getProviderAttachment(t, collision)
		p.Annotations = map[string]string{infrav1.ProviderBindingAnnotation: `{"invalid":true}`}
		require.NoError(t, k8s.Update(t.Context(), p))
	}
	return collision
}

type isolationProfileReader struct {
	client.Reader
	profile string
	failure error
}

func (r isolationProfileReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*infrav1.CompositeProviderTemplate); ok && key.Name == r.profile {
		return r.failure
	}
	if err := r.Reader.Get(ctx, key, obj, opts...); err != nil {
		return fmt.Errorf("reading isolated test profile: %w", err)
	}
	return nil
}

func TestProviderAttachment_IsolatesFailureWhileRespectingPauseAndRetirement(t *testing.T) {
	firstProfile := isolationProfile(t, "zone-first")
	retiringProfile := isolationProfile(t, "zone-retiring")
	pausedProfile := isolationProfile(t, "zone-paused")
	cluster := newCAPICluster(t, newNamespace(t), "attachment-lifecycle")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	setEndpoint(t, cc, cluster, "192.0.2.83", 6443)
	first := createProviderAttachment(t, cc, firstProfile.Name, "a-first")
	retiringName := cc.Name + "-retiring"
	retiring := createProviderAttachment(t, cc, retiringProfile.Name, "b-retiring", map[string]any{"infraClusterName": retiringName})
	pausedName := cc.Name + "-paused"
	paused := createProviderAttachment(t, cc, pausedProfile.Name, "c-paused", map[string]any{"infraClusterName": pausedName})
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	broken := getProviderAttachment(t, first)
	broken.Annotations[infrav1.ProviderBindingAnnotation] = `{"invalid":true}`
	require.NoError(t, k8s.Update(t.Context(), broken))
	pausedProvider := getProviderAttachment(t, paused)
	pausedPin := pausedProvider.Annotations[infrav1.ProviderBindingAnnotation]
	pausedProvider.Annotations["cluster.x-k8s.io/paused"] = ""
	require.NoError(t, k8s.Update(t.Context(), pausedProvider))
	pausedBefore := getUnstructured(t, fakeClusterGVK, cc.Namespace, pausedName)
	retiringChild := getUnstructured(t, fakeClusterGVK, cc.Namespace, retiringName)
	retiringChild.SetFinalizers([]string{"tests.example.org/native-cleanup"})
	require.NoError(t, k8s.Update(t.Context(), retiringChild))
	require.NoError(t, k8s.Delete(t.Context(), retiring))
	ref := retiringProfile.Spec.InfrastructureTemplateRef
	require.NoError(t, k8s.Delete(t.Context(), getUnstructured(t, schemaForSource(ref), ref.Namespace, ref.Name)))
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(t, getProviderAttachment(t, paused).Status.Conditions, infrav1.PausedCondition, metav1.ConditionTrue, infrav1.PausedReason)
	assert.Equal(t, pausedBefore.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, pausedName).Object)
	retiringChild = getUnstructured(t, fakeClusterGVK, cc.Namespace, retiringName)
	require.NotNil(t, retiringChild.GetDeletionTimestamp())
	require.NotNil(t, getUnstructured(t, kubeadmConfigTemplateGVK, cc.Namespace, cc.Name+"-b-retiring"))
	retiringChild.SetFinalizers(nil)
	require.NoError(t, k8s.Update(t.Context(), retiringChild))
	require.Eventually(t, func() bool {
		_, _, reconcileErr := reconcileCluster(t, r, cc)
		return reconcileErr == nil && apierrors.IsNotFound(k8s.Get(t.Context(), client.ObjectKeyFromObject(retiring), &infrav1.CompositeProvider{}))
	}, 5*time.Second, 50*time.Millisecond)
	assert.Nil(t, getUnstructured(t, kubeadmConfigTemplateGVK, cc.Namespace, cc.Name+"-b-retiring"))
	assert.Equal(t, pausedBefore.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, pausedName).Object)
	pausedProvider = getProviderAttachment(t, paused)
	assert.Equal(t, pausedPin, pausedProvider.Annotations[infrav1.ProviderBindingAnnotation])
	delete(pausedProvider.Annotations, "cluster.x-k8s.io/paused")
	require.NoError(t, k8s.Update(t.Context(), pausedProvider))
	markFakeClusterReady(t, cc.Namespace, pausedName)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	requireCondition(t, getProviderAttachment(t, paused).Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	requireCondition(t, cc.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
}

func TestProviderAttachment_IsolatesUnexpectedProfileReadErrors(t *testing.T) {
	firstProfile := isolationProfile(t, "zone-first")
	secondProfile := isolationProfile(t, "zone-second")
	cluster := newCAPICluster(t, newNamespace(t), "read-error-attachments")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	setEndpoint(t, cc, cluster, "192.0.2.82", 6443)
	first := createProviderAttachment(t, cc, firstProfile.Name, "a-first")
	secondName := cc.Name + "-second"
	second := createProviderAttachment(t, cc, secondProfile.Name, "b-second", map[string]any{"infraClusterName": secondName})
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	firstBefore := getProviderAttachment(t, first)
	beforeRecord := cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
	markFakeClusterReady(t, cc.Namespace, secondName)
	failure := apierrors.NewForbidden(
		schema.GroupResource{Group: infrav1.GroupVersion.Group, Resource: "compositeprovidertemplates"},
		firstProfile.Name,
		errors.New("SECRET-CANARY"),
	)
	r.APIReader = isolationProfileReader{Reader: k8s, profile: firstProfile.Name, failure: failure}
	_, cc, err = reconcileCluster(t, r, cc)
	require.ErrorIs(t, err, failure)
	assert.True(t, apierrors.IsForbidden(err))
	assert.NotContains(t, err.Error(), "SECRET-CANARY")
	firstAfter := getProviderAttachment(t, first)
	requireCondition(t, firstAfter.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionUnknown, infrav1.WaitingForProvidersReason)
	assert.Equal(t, firstBefore.Annotations, firstAfter.Annotations)
	assert.Equal(t, firstBefore.Finalizers, firstAfter.Finalizers)
	assert.Equal(t, firstBefore.OwnerReferences, firstAfter.OwnerReferences)
	assert.Equal(t, firstBefore.Status.Profile, firstAfter.Status.Profile)
	assert.Equal(t, firstBefore.Status.InfrastructureRef, firstAfter.Status.InfrastructureRef)
	assert.Equal(t, beforeRecord, cc.Annotations[infrav1.ProviderAttachmentsAnnotation])
	requireCondition(t, getProviderAttachment(t, second).Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	requireCondition(t, cc.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
}
