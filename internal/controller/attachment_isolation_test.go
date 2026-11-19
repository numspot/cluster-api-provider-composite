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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

type isolatedChildFailureClient struct {
	client.Client
	name    string
	failure error
}

func (c isolatedChildFailureClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "FakeCluster" && key.Name == c.name {
		return c.failure
	}
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return fmt.Errorf("reading isolated test child: %w", err)
	}
	return nil
}

func TestAttachmentIsolationContinuesAfterNativeOperationError(t *testing.T) {
	r, c, cc, first := attachmentObservationFixture(t)
	second := *first
	second.provider = first.provider.DeepCopy()
	second.provider.Name = "second"
	second.provider.UID = "second-uid"
	second.provider.ResourceVersion = ""
	second.plan.spec = &infrav1.ProviderSpec{Name: "second"}
	second.plan.binding.ChildName = cc.Name + "-second"
	second.plan.binding.BootstrapName = cc.Name + "-second"
	require.NoError(t, c.Client.Create(t.Context(), second.provider))
	failure := errors.New("native API unavailable")
	r.Client = isolatedChildFailureClient{Client: c, name: first.plan.binding.ChildName, failure: failure}
	plan := &attachmentPlan{providers: []plannedAttachment{*first, second}}
	pending, err := r.reconcileAttachments(
		t.Context(),
		cc,
		&clusterFacts{Name: "owner", Namespace: cc.Namespace, UID: "owner-uid"},
		infrav1.APIEndpoint{Host: "api.example.test", Port: 6443},
		plan,
	)
	assert.True(t, pending)
	require.ErrorIs(t, err, failure)
	stored := &infrav1.CompositeProvider{}
	require.NoError(t, c.Client.Get(t.Context(), client.ObjectKeyFromObject(second.provider), stored))
	ready := apimeta.FindStatusCondition(stored.Status.Conditions, infrav1.ReadyCondition)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, stored.Generation, ready.ObservedGeneration, "one native API error must not leave another attachment stale")
	assert.Equal(t, second.plan.binding.ChildName, stored.Status.InfrastructureRef.Name)
}
