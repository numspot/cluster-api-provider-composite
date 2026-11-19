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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

func (r *CompositeClusterReconciler) attachmentToComposite(_ context.Context, obj client.Object) []reconcile.Request {
	p, ok := obj.(*infrav1.CompositeProvider)
	if !ok || p.Spec.ClusterRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ClusterRef.Name}}}
}

func (r *CompositeClusterReconciler) finishNormalReconcile(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	endpoint infrav1.APIEndpoint,
	plan *attachmentPlan,
) (reconcile.Result, error) {
	r.protectAttachments(ctx, cc, cluster, plan)
	pending, err := r.reconcileAttachments(ctx, cc, cluster, endpoint, plan)
	if err != nil {
		return reconcile.Result{}, err
	}
	if pending {
		return settling, nil
	}
	if isTrue(cc.Status.Conditions, infrav1.ReadyCondition) {
		return steady, nil
	}
	return settling, nil
}

func (r *CompositeClusterReconciler) protectAttachments(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	plan *attachmentPlan,
) {
	if plan == nil || plan.failure != nil {
		return
	}
	for i := range plan.providers {
		entry := &plan.providers[i]
		if entry.paused || entry.failure != nil {
			continue
		}
		if err := r.protectAttachment(ctx, cc, cluster, entry.provider, plan.bindings.Providers[entry.provider.Name]); err != nil {
			entry.failure = &attachmentFailure{cause: err}
		}
	}
}

func (r *CompositeClusterReconciler) protectAttachment(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	p *infrav1.CompositeProvider,
	binding *attachmentBinding,
) error {
	before := p.DeepCopy()
	encoded, _ := json.Marshal(binding)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[infrav1.ProviderBindingAnnotation] = string(encoded)
	controllerutil.AddFinalizer(p, infrav1.ProviderAttachmentFinalizer)
	if err := controllerutil.SetControllerReference(cc, p, r.Scheme()); err != nil {
		return fmt.Errorf("protecting provider ownership: %w", err)
	}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[clusterv1.ClusterNameLabel] = cluster.Name
	if !reflect.DeepEqual(before.ObjectMeta, p.ObjectMeta) {
		if err := r.Update(ctx, p); err != nil {
			return fmt.Errorf("persisting provider pin and finalizer: %w", err)
		}
	}
	return nil
}

func (r *CompositeClusterReconciler) reconcileAttachments(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	endpoint infrav1.APIEndpoint,
	plan *attachmentPlan,
) (bool, error) {
	if plan == nil {
		return false, nil
	}
	if plan.failure != nil {
		return true, r.reportAttachmentCompositionFailure(ctx, plan)
	}
	pending := false
	var failures []error
	for i := range plan.providers {
		entry := &plan.providers[i]
		ready, err := r.reconcilePlannedAttachment(ctx, cc, cluster, endpoint, entry)
		failures = append(failures, err)
		pending = pending || !ready || err != nil
	}
	retiring, err := r.retireAttachments(ctx, cc, plan)
	return pending || retiring, errors.Join(append(failures, err)...)
}

func (r *CompositeClusterReconciler) reportAttachmentCompositionFailure(ctx context.Context, plan *attachmentPlan) error {
	var failures []error
	for i := range plan.providers {
		entry := &plan.providers[i]
		if entry.paused {
			continue
		}
		failure := entry.failure
		if failure == nil {
			failure = plan.failure
		}
		failures = append(failures, r.reportAttachmentPlanFailure(ctx, entry.provider, failure))
	}
	return errors.Join(failures...)
}

func (r *CompositeClusterReconciler) reconcilePlannedAttachment(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	endpoint infrav1.APIEndpoint,
	entry *plannedAttachment,
) (bool, error) {
	if entry.failure != nil {
		return false, r.reportAttachmentPlanFailure(ctx, entry.provider, entry.failure)
	}
	return r.reconcileAttachment(ctx, cc, cluster, endpoint, entry)
}

func (r *CompositeClusterReconciler) reportAttachmentPlanFailure(
	ctx context.Context,
	p *infrav1.CompositeProvider,
	failure *attachmentFailure,
) error {
	if !expectedAttachmentFailure(failure.cause) {
		return r.reportAttachmentObservationError(ctx, p, failure.cause)
	}
	return r.reportAttachmentFailure(ctx, p, failure.reason, attachmentDiagnostic(failure))
}

func (r *CompositeClusterReconciler) reconcileAttachment(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	endpoint infrav1.APIEndpoint,
	entry *plannedAttachment,
) (bool, error) {
	if entry.paused {
		before := entry.provider.DeepCopy()
		setCondition(
			&entry.provider.Status.Conditions,
			entry.provider.Generation,
			infrav1.PausedCondition,
			metav1.ConditionTrue,
			infrav1.PausedReason,
			"",
		)
		return true, r.patchAttachmentStatus(ctx, before, entry.provider)
	}
	ps, err := r.reconcileProvider(ctx, cc, &entry.plan, cluster, endpoint)
	if err != nil {
		return false, r.reportAttachmentObservationError(ctx, entry.provider, err)
	}
	statuses := []infrav1.ProviderStatus{*ps}
	bootstrapReady, err := r.reconcileBootstrapTemplates(ctx, cc, cluster, statuses, endpoint, []providerPlan{entry.plan})
	if err != nil {
		return false, r.reportAttachmentObservationError(ctx, entry.provider, err)
	}
	return r.observeAttachment(ctx, entry.provider, &statuses[0], endpoint, bootstrapReady)
}

// An operation error invalidates current readiness, not durable initialization or
// the last known child references. Never publish the native error's raw payload.
func (r *CompositeClusterReconciler) reportAttachmentObservationError(ctx context.Context, p *infrav1.CompositeProvider, cause error) error {
	before := p.DeepCopy()
	setCondition(&p.Status.Conditions, p.Generation, infrav1.ReadyCondition, metav1.ConditionUnknown,
		infrav1.WaitingForProvidersReason, diagnosticMessage(cause))
	p.Status.ObservedGeneration = p.Generation
	return errors.Join(cause, r.patchAttachmentStatus(ctx, before, p))
}

func (r *CompositeClusterReconciler) observeAttachment(
	ctx context.Context,
	p *infrav1.CompositeProvider,
	ps *infrav1.ProviderStatus,
	endpoint infrav1.APIEndpoint,
	bootstrapReady bool,
) (bool, error) {
	before := p.DeepCopy()
	ready := setAttachmentConditions(p, ps, endpoint, bootstrapReady)
	latched := providerProvisioned(ps) && endpoint.IsValid() || (p.Status.Initialization.Provisioned != nil && *p.Status.Initialization.Provisioned)
	p.Status.Initialization.Provisioned = &latched
	p.Status.Profile = ps.Profile
	if ps.ChildRef != nil {
		p.Status.InfrastructureRef = *ps.ChildRef
	}
	if ps.BootstrapTemplateRef != nil {
		p.Status.BootstrapTemplateRef = *ps.BootstrapTemplateRef
	}
	p.Status.ObservedGeneration = p.Generation
	return ready, r.patchAttachmentStatus(ctx, before, p)
}

func attachmentObservationReason(ps *infrav1.ProviderStatus, endpoint infrav1.APIEndpoint, ready bool) string {
	if ps.Reason != "" {
		return ps.Reason
	}
	if !endpoint.IsValid() {
		return infrav1.WaitingForEndpointReason
	}
	if !ready {
		return infrav1.WaitingForProvidersReason
	}
	return infrav1.ReadyReason
}

func setAttachmentConditions(p *infrav1.CompositeProvider, ps *infrav1.ProviderStatus, endpoint infrav1.APIEndpoint, bootstrapReady bool) bool {
	ready := providerHealthy(ps) && endpoint.IsValid() && bootstrapReady
	reason := attachmentObservationReason(ps, endpoint, ready)
	for _, observation := range []struct {
		condition string
		ready     bool
	}{
		{infrav1.ReadyCondition, ready},
		{infrav1.EndpointPropagatedCondition, endpoint.IsValid() && providerApplied(ps)},
		{infrav1.BootstrapTemplatesReadyCondition, bootstrapReady},
	} {
		if observation.ready {
			setTrue(&p.Status.Conditions, p.Generation, observation.condition)
		} else {
			setFalse(&p.Status.Conditions, p.Generation, observation.condition, reason, ps.Message)
		}
	}
	setCondition(&p.Status.Conditions, p.Generation, infrav1.PausedCondition, metav1.ConditionFalse, infrav1.NotPausedReason, "")
	setCondition(&p.Status.Conditions, p.Generation, infrav1.DeletingCondition, metav1.ConditionFalse, infrav1.NotDeletingReason, "")
	return ready
}

func (r *CompositeClusterReconciler) reportAttachmentFailure(ctx context.Context, p *infrav1.CompositeProvider, reason, message string) error {
	before := p.DeepCopy()
	setFalse(&p.Status.Conditions, p.Generation, infrav1.ReadyCondition, reason, message)
	setFalse(&p.Status.Conditions, p.Generation, infrav1.EndpointPropagatedCondition, reason, message)
	setFalse(&p.Status.Conditions, p.Generation, infrav1.BootstrapTemplatesReadyCondition, reason, message)
	if !p.DeletionTimestamp.IsZero() {
		setCondition(&p.Status.Conditions, p.Generation, infrav1.DeletingCondition, metav1.ConditionTrue, infrav1.DeletingReason, "")
	}
	p.Status.ObservedGeneration = p.Generation
	return r.patchAttachmentStatus(ctx, before, p)
}

func (r *CompositeClusterReconciler) patchAttachmentStatus(ctx context.Context, before, p *infrav1.CompositeProvider) error {
	if reflect.DeepEqual(before.Status, p.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, p, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("reporting provider attachment: %w", err)
	}
	previous := apimeta.FindStatusCondition(before.Status.Conditions, infrav1.ReadyCondition)
	current := apimeta.FindStatusCondition(p.Status.Conditions, infrav1.ReadyCondition)
	if r.Recorder != nil && current != nil && (previous == nil || previous.Status != current.Status || previous.Reason != current.Reason) {
		eventType := corev1.EventTypeNormal
		if current.Status != metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(
			p,
			nil,
			eventType,
			current.Reason,
			"Observe",
			"Provider Ready=%s; inspect its infrastructure and bootstrap references.",
			current.Status,
		)
	}
	return nil
}
