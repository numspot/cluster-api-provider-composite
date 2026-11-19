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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

func TestProviderAttachment_SemanticPinAndConflict(t *testing.T) {
	template := newRegionalProfile(t)
	cluster := newCAPICluster(t, newNamespace(t), "semantic-pin")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	setEndpoint(t, cc, cluster, "192.0.2.70", 6443)
	p := createProviderAttachment(t, cc, template.Name, "paris")
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	markFakeClusterReady(t, cc.Namespace, cc.Name)
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	provider := getProviderAttachment(t, p)
	requireCondition(t, provider.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	pin, record := provider.Annotations[infrav1.ProviderBindingAnnotation], cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
	child := getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(pin), &fields))
	formatted, err := json.MarshalIndent(fields, "", "  ")
	require.NoError(t, err)
	for _, equivalent := range []string{pin + "\n", "\n" + string(formatted) + "\n"} {
		provider = getProviderAttachment(t, p)
		provider.Annotations[infrav1.ProviderBindingAnnotation] = equivalent
		require.NoError(t, k8s.Update(t.Context(), provider))
		_, cc, err = reconcileCluster(t, r, cc)
		require.NoError(t, err)
		provider = getProviderAttachment(t, p)
		requireCondition(t, provider.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
		assert.Equal(t, record, cc.Annotations[infrav1.ProviderAttachmentsAnnotation])
		assert.Equal(t, child.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name).Object)
	}
	provider.Annotations[infrav1.ProviderBindingAnnotation] = strings.Replace(pin, `"profileUID":"`, `"profileUID":"replacement-`, 1)
	require.NoError(t, k8s.Update(t.Context(), provider))
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	provider = getProviderAttachment(t, p)
	requireCondition(t, provider.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionFalse, infrav1.ProfileMismatchReason)
	assert.Contains(t, condition(provider.Status.Conditions, infrav1.ReadyCondition).Message, "Provider-local")
	assert.Equal(t, record, cc.Annotations[infrav1.ProviderAttachmentsAnnotation])
	assert.Equal(t, child.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name).Object)
	before := provider.DeepCopy()
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	assert.Equal(t, before, getProviderAttachment(t, p), "unchanged failures are observations, not heartbeat writes")

	provider.Annotations[infrav1.ProviderBindingAnnotation] = pin
	require.NoError(t, k8s.Update(t.Context(), provider))
	_, _, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	recovered := getProviderAttachment(t, p)
	requireCondition(t, recovered.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionTrue, infrav1.ReadyReason)
	assert.Equal(t, before.Generation, recovered.Generation, "pin encoding does not require a provider spec update")
}

func TestProviderAttachment_RecreatedSourceDiagnostic(t *testing.T) {
	for _, sourceKind := range []string{"infrastructure", "bootstrap"} {
		t.Run(sourceKind, func(t *testing.T) {
			template := newRegionalProfile(t)
			cluster := newCAPICluster(t, newNamespace(t), "recreated-source")
			cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
			p := createProviderAttachment(t, cc, template.Name, "paris")
			r := newClusterReconciler()
			_, cc, err := reconcileCluster(t, r, cc)
			require.NoError(t, err)
			before := getProviderAttachment(t, p)
			record := cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
			child := getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name)
			ref := template.Spec.InfrastructureTemplateRef
			if sourceKind == "bootstrap" {
				ref = template.Spec.BootstrapTemplateRef
			}
			source := getUnstructured(t, schemaForSource(ref), ref.Namespace, ref.Name)
			require.NotNil(t, source)
			payload, found, err := unstructured.NestedMap(source.Object, "spec", "template", "spec")
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, k8s.Delete(t.Context(), source))
			replacement := profileSourceObject(ref, payload)
			require.NoError(t, k8s.Create(t.Context(), replacement))
			require.NotEqual(t, source.GetUID(), replacement.GetUID())
			_, cc, err = reconcileCluster(t, r, cc)
			require.NoError(t, err)
			provider := getProviderAttachment(t, p)
			requireCondition(t, provider.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionFalse, infrav1.ProfileMismatchReason)
			message := condition(provider.Status.Conditions, infrav1.ReadyCondition).Message
			for _, identity := range []string{ref.Kind, ref.Namespace + "/" + ref.Name, string(source.GetUID()), string(replacement.GetUID()), "generation=1"} {
				assert.Contains(t, message, identity)
			}
			assert.Equal(t, record, cc.Annotations[infrav1.ProviderAttachmentsAnnotation])
			assert.Equal(t, before.Annotations[infrav1.ProviderBindingAnnotation], provider.Annotations[infrav1.ProviderBindingAnnotation])
			assert.Equal(t, child.Object, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name).Object)
		})
	}
}

func TestProviderAttachment_RetiredNameDiagnostic(t *testing.T) {
	template := newRegionalProfile(t)
	cluster := newCAPICluster(t, newNamespace(t), "retired-pin")
	cc := newCompositeCluster(t, cluster, infrav1.CompositeClusterSpec{})
	p := createProviderAttachment(t, cc, template.Name, "paris")
	r := newClusterReconciler()
	_, cc, err := reconcileCluster(t, r, cc)
	require.NoError(t, err)
	require.NoError(t, k8s.Delete(t.Context(), p))
	key := client.ObjectKeyFromObject(p)
	require.Eventually(t, func() bool {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cc)})
		return err == nil && apierrors.IsNotFound(k8s.Get(t.Context(), key, &infrav1.CompositeProvider{}))
	}, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, k8s.Get(t.Context(), client.ObjectKeyFromObject(cc), cc))
	record := cc.Annotations[infrav1.ProviderAttachmentsAnnotation]
	require.Contains(t, record, `"retired":true`)
	replacement := createProviderAttachment(t, cc, template.Name, p.GetName())
	_, cc, err = reconcileCluster(t, r, cc)
	require.NoError(t, err)
	provider := getProviderAttachment(t, replacement)
	requireCondition(t, provider.Status.Conditions, infrav1.ReadyCondition, metav1.ConditionFalse, infrav1.ProfileMismatchReason)
	message := condition(provider.Status.Conditions, infrav1.ReadyCondition).Message
	for _, evidence := range []string{"retired", "reserved", "CompositeCluster", string(p.GetUID()), string(provider.UID)} {
		assert.Contains(t, message, evidence)
	}
	assert.Empty(t, provider.Annotations[infrav1.ProviderBindingAnnotation])
	assert.Empty(t, provider.Finalizers)
	assert.Equal(t, record, cc.Annotations[infrav1.ProviderAttachmentsAnnotation], "diagnostics never authorize retired-record deletion")
	assert.Nil(t, getUnstructured(t, fakeClusterGVK, cc.Namespace, cc.Name))
	assert.Nil(t, getUnstructured(t, kubeadmConfigTemplateGVK, cc.Namespace, cc.Name+"-paris"))
}
