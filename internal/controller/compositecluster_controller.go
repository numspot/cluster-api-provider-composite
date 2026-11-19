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
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
	"github.com/numspot/cluster-api-provider-composite/internal/capi"
	"github.com/numspot/cluster-api-provider-composite/internal/mint"
)

// kubeadmConfigTemplateGVK is the one CAPI kind this provider writes besides
// the provider children, at the ONE wire version it supports. requireServed
// refuses to start on a core that does not serve it: writing at a version the
// server merely converts can cause repeated writes and field-ownership drift.
var kubeadmConfigTemplateGVK = schema.FromAPIVersionAndKind(mint.KubeadmConfigTemplateAPIVersion, mint.KubeadmConfigTemplateKind)

// requireServed fails unless the API server serves gvk exactly.
func requireServed(mgr ctrl.Manager, gvk schema.GroupVersionKind) error {
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return fmt.Errorf("this management cluster does not serve %s (Cluster API v1.11+ is required): %w", gvk, err)
	}

	return nil
}

// messageLimit caps status messages at the CRD's MaxLength; API server
// validation errors can run long.
const messageLimit = 1024

// CompositeClusterReconciler mints and lifecycles one provider InfraCluster
// child per CompositeProvider attachment, fans the control plane endpoint out to all
// of them, and mints the per-provider KubeadmConfigTemplates from the
// bootstrap cascade — in that order, because the ordering is the contract
// (bootstrap material only after EndpointPropagated).
type CompositeClusterReconciler struct {
	client.Client

	// APIReader supplies authoritative profile and ownership observations
	// independently of the manager's informer cache.
	APIReader client.Reader

	// Recorder publishes state transitions after successful status persistence.
	Recorder events.EventRecorder

	// WatchFilterValue is CAPI's optional per-controller resource filter.
	WatchFilterValue string

	// Contracts resolves each child kind's served version and contract from
	// its CRD labels.
	Contracts *capi.ContractCache

	// Clusters reads the owning CAPI Cluster at whatever API version the
	// server serves. Set by SetupWithManager; tests set it directly.
	Clusters *ClusterReader
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machinedeployments;machinesets;machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigs,verbs=get
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigtemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// Reconcile drives one CompositeCluster to its desired state.
func (r *CompositeClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	ctx = ctrl.LoggerInto(ctx, ctrl.LoggerFrom(ctx).WithValues("compositeCluster", req.String()))
	defer func() { err = safeReconcileError(err) }()
	cc := &infrav1.CompositeCluster{}
	if err := r.Get(ctx, req.NamespacedName, cc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("CompositeClusterReconciler: getting %s: %w", req.NamespacedName, err)
	}
	if !r.shouldReconcile(cc) {
		return ctrl.Result{}, nil
	}
	ctrl.LoggerFrom(ctx).V(1).Info("Reconciling CompositeCluster")
	if isPaused(nil, cc) {
		return ctrl.Result{}, r.reconcilePaused(ctx, cc)
	}

	cluster, err := r.Clusters.getOwner(ctx, cc)
	if err != nil {
		original := cc.DeepCopy()
		markObservationFailed(cc, err)
		return ctrl.Result{}, errors.Join(err, r.patchStatus(ctx, original, cc))
	}

	if isPaused(cluster, cc) {
		return ctrl.Result{}, r.reconcilePaused(ctx, cc)
	}

	plan, err := r.prepareReconcile(ctx, cc, cluster)
	if err != nil {
		return r.preparationError(ctx, cc, err)
	}

	if !cc.DeletionTimestamp.IsZero() {
		original := cc.DeepCopy()
		setCondition(&cc.Status.Conditions, cc.Generation,
			infrav1.PausedCondition, metav1.ConditionFalse, infrav1.NotPausedReason, "")

		res, err := r.reconcileDelete(ctx, cc)
		if !controllerutil.ContainsFinalizer(cc, infrav1.ClusterFinalizer) {
			// The finalizer just came off: the object is gone and a status
			// patch would only report NotFound.
			return res, err
		}

		err = errors.Join(err, r.refreshProviderStatuses(ctx, cc, cluster))
		return res, errors.Join(err, r.patchStatus(ctx, original, cc))
	}

	return r.reconcileNormalStatus(ctx, cc, cluster, plan)
}

// SetupWithManager wires the controller: our own objects, the minted
// KubeadmConfigTemplates, and the owning CAPI Clusters at whatever version
// this cluster serves. Provider children have no watches yet — their dynamic
// GVKs come from profiles and progress is noticed by the settling
// requeue.
func (r *CompositeClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapping, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: clusterv1.GroupVersion.Group, Kind: clusterv1.ClusterKind})
	if err != nil {
		return fmt.Errorf("SetupWithManager: resolving the served Cluster version: %w", err)
	}

	r.APIReader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorder("capi-composite")
	r.Clusters = NewClusterReader(mgr.GetAPIReader(), mapping.GroupVersionKind)

	if err := requireServed(mgr, kubeadmConfigTemplateGVK); err != nil {
		return fmt.Errorf("SetupWithManager: %w", err)
	}

	clusterObj := &unstructured.Unstructured{}
	clusterObj.SetGroupVersionKind(mapping.GroupVersionKind)

	kct := &unstructured.Unstructured{}
	kct.SetGroupVersionKind(kubeadmConfigTemplateGVK)
	poolGVK, err := r.nodePoolGVK()
	if err != nil {
		return fmt.Errorf("SetupWithManager: %w", err)
	}
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(poolGVK)

	// Primary metadata events include pause/filter changes and deletion timestamps.
	err = ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.CompositeCluster{}, builder.WithPredicates(predicates.ResourceHasFilterLabel(mgr.GetScheme(), mgr.GetLogger(), r.WatchFilterValue))).
		Watches(&infrav1.CompositeProviderTemplate{}, handler.EnqueueRequestsFromMapFunc(r.profileToComposites)).
		Watches(&infrav1.CompositeProvider{}, handler.EnqueueRequestsFromMapFunc(r.attachmentToComposite)).
		Watches(pool, handler.EnqueueRequestsFromMapFunc(r.nodePoolToComposite)).
		Owns(kct).
		Watches(clusterObj,
			handler.EnqueueRequestsFromMapFunc(r.clusterToComposite),
			builder.WithPredicates(predicate.Funcs{UpdateFunc: clusterChanged})).
		Complete(r)
	if err != nil {
		return fmt.Errorf("SetupWithManager: building the CompositeCluster controller: %w", err)
	}

	return nil
}

func (r *CompositeClusterReconciler) reconcileNormalStatus(
	ctx context.Context, cc *infrav1.CompositeCluster, cluster *clusterFacts, plan *compositionPlan,
) (ctrl.Result, error) {
	// Metadata updates must precede the status snapshot and all status mutation.
	original := cc.DeepCopy()
	setCondition(&cc.Status.Conditions, cc.Generation,
		infrav1.PausedCondition, metav1.ConditionFalse, infrav1.NotPausedReason, "")
	res, err := r.reconcileNormal(ctx, cc, cluster, plan)
	// Attachment operation errors are reported on each provider; they do not
	// invalidate the independently observed CompositeCluster infrastructure.
	err = errors.Join(err, r.refreshProviderStatuses(ctx, cc, cluster))
	return res, errors.Join(err, r.patchStatus(ctx, original, cc))
}

func (r *CompositeClusterReconciler) reconcilePaused(ctx context.Context, cc *infrav1.CompositeCluster) error {
	original := cc.DeepCopy()
	setCondition(&cc.Status.Conditions, cc.Generation, infrav1.PausedCondition, metav1.ConditionTrue, infrav1.PausedReason, "")
	return r.patchStatus(ctx, original, cc)
}

func (r *CompositeClusterReconciler) shouldReconcile(obj *infrav1.CompositeCluster) bool {
	return !isExternallyManaged(obj) && (r.WatchFilterValue == "" || obj.GetLabels()[clusterv1.WatchLabel] == r.WatchFilterValue)
}

func (r *CompositeClusterReconciler) prepareReconcile(
	ctx context.Context, cc *infrav1.CompositeCluster, cluster *clusterFacts,
) (*compositionPlan, error) {
	if !cc.DeletionTimestamp.IsZero() {
		if _, err := readAttachmentBindings(cc); err != nil {
			return nil, err
		}
		return &compositionPlan{}, nil
	}
	if cluster != nil && !cluster.Deleting {
		return r.prepareComposition(ctx, cc)
	}
	return &compositionPlan{}, nil
}

func (r *CompositeClusterReconciler) preparationError(ctx context.Context, cc *infrav1.CompositeCluster, err error) (ctrl.Result, error) {
	if !errors.Is(err, errInvalidComposition) {
		// A previously healthy object must not retain current Ready=True after
		// failed observation. Uninitialized observation failures remain untouched.
		if isTrue(cc.Status.Conditions, infrav1.ReadyCondition) {
			original := cc.DeepCopy()
			markObservationFailed(cc, err)
			return ctrl.Result{}, errors.Join(err, r.patchStatus(ctx, original, cc))
		}
		return ctrl.Result{}, err
	}
	original := cc.DeepCopy()
	for _, typ := range []string{infrav1.ReadyCondition, infrav1.ProvidersReadyCondition, infrav1.EndpointPropagatedCondition, infrav1.BootstrapTemplatesReadyCondition} {
		setFalse(&cc.Status.Conditions, cc.Generation, typ, infrav1.InvalidCompositionReason, diagnosticMessage(err))
	}
	cc.Status.Reason, cc.Status.Message = infrav1.InvalidCompositionReason, diagnosticMessage(err)
	cc.Status.ObservedGeneration = cc.Generation
	markProviderObservationsUnknown(cc, infrav1.InvalidCompositionReason, diagnosticMessage(err))
	return settling, r.patchStatus(ctx, original, cc)
}

func markObservationFailed(cc *infrav1.CompositeCluster, err error) {
	for _, typ := range []string{infrav1.ReadyCondition, infrav1.ProvidersReadyCondition} {
		setCondition(&cc.Status.Conditions, cc.Generation, typ, metav1.ConditionUnknown, infrav1.WaitingForProvidersReason, diagnosticMessage(err))
	}
	cc.Status.Reason, cc.Status.Message = infrav1.WaitingForProvidersReason, diagnosticMessage(err)
	cc.Status.ObservedGeneration = cc.Generation
	markProviderObservationsUnknown(cc, infrav1.WaitingForProvidersReason, diagnosticMessage(err))
}

// clusterToComposite maps a Cluster event onto its CompositeCluster: the
// infrastructureRef target, matched on kind — the name is shared.
func (r *CompositeClusterReconciler) clusterToComposite(_ context.Context, obj client.Object) []reconcile.Request {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	kind, _, _ := unstructured.NestedString(u.Object, "spec", "infrastructureRef", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "infrastructureRef", "name")
	group, _, _ := unstructured.NestedString(u.Object, "spec", "infrastructureRef", "apiGroup")

	if kind != compositeClusterKind || name == "" || group != infrav1.GroupVersion.Group {
		return nil
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      name,
	}}}
}

// reconcileNormal is the create/update path: children, then the endpoint
// gate, then the bootstrap templates, then the summary.
func (r *CompositeClusterReconciler) reconcileNormal(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	plan *compositionPlan,
) (ctrl.Result, error) {
	if cluster == nil {
		r.markWaitingForCluster(cc)

		return settling, nil
	}

	if cluster.Deleting {
		// CAPI owns teardown from here: it deletes the descendants and, last,
		// this object — at which point our own finalizer path runs. Applying
		// children now would only re-create what CAPI is removing.
		r.markDeleting(cc)

		return settling, nil
	}
	setCondition(&cc.Status.Conditions, cc.Generation, infrav1.DeletingCondition, metav1.ConditionFalse, infrav1.NotDeletingReason, "")

	// The endpoint is written onto us by the control plane provider (we are
	// the infrastructureRef target); the Cluster's own field is the fallback
	// when it was set out-of-band.
	endpoint := cc.Spec.ControlPlaneEndpoint
	if !endpoint.IsValid() {
		endpoint = cluster.Endpoint
	}

	domains, err := plan.allFailureDomains()
	if err != nil {
		markObservationFailed(cc, err)
		return ctrl.Result{}, err
	}
	// Provider attachments have independent readiness; an empty infrastructure
	// contract must unblock the hosted control plane before attachments exist.
	summarize(cc, nil, summary{
		failureDomains: domains, providersReady: true,
		providersProvisioned: true, endpointPropagated: true, bootstrapReady: true,
	})
	return r.finishNormalReconcile(ctx, cc, cluster, endpoint, plan.attachments)
}

// reconcileProvider mints and applies one provider's InfraCluster child and
// reads its contract status back.
func (r *CompositeClusterReconciler) reconcileProvider(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	prepared *providerPlan,
	cluster *clusterFacts,
	endpoint infrav1.APIEndpoint,
) (*infrav1.ProviderStatus, error) {
	p := prepared.spec
	ps := &infrav1.ProviderStatus{Name: p.Name, Profile: prepared.binding.profileStatus()}

	gvk, contract, reason, err := r.resolveProviderPlanKind(ctx, prepared)
	if err != nil {
		return nil, err
	}

	if reason != "" {
		return r.unavailableProviderStatus(ctx, prepared, reason)
	}

	obj, err := prepared.renderInfrastructure(cc, gvk, endpoint)
	if err != nil {
		ps.Reason, ps.Message = infrav1.RenderFailedReason, diagnosticMessage(err)

		return ps, nil
	}

	applied, rejection, err := r.applyChild(ctx, cc, cluster, obj, gvk)
	if err != nil {
		return nil, err
	}

	if rejection != nil {
		ps.Reason, ps.Message = rejection.reason, rejection.message

		return ps, nil
	}

	ps.ChildRef = &infrav1.ChildReference{
		APIVersion: contract.APIVersion,
		Kind:       gvk.Kind,
		Name:       applied.GetName(),
	}

	st, err := capi.ReadInfraCluster(applied, contract.Contract)
	if err != nil {
		return nil, fmt.Errorf("reconcileProvider %q: %w", p.Name, err)
	}

	ps.Provisioned = new(st.Provisioned)
	if !st.Provisioned || !st.Ready {
		ps.Reason = infrav1.WaitingForProvidersReason
		ps.Message = clip(
			fmt.Sprintf(
				"Provider child %s %s/%s is not currently ready; inspect its status with appropriate permissions.",
				gvk.Kind,
				cc.Namespace,
				applied.GetName(),
			),
		)
	}

	return ps, nil
}

func (r *CompositeClusterReconciler) unavailableProviderStatus(
	ctx context.Context,
	prepared *providerPlan,
	reason string,
) (*infrav1.ProviderStatus, error) {
	name := prepared.spec.Name
	ps := &infrav1.ProviderStatus{Name: name, Reason: reason}
	binding := prepared.binding
	ps.Profile = binding.profileStatus()
	retained, err := r.Contracts.Get(ctx, binding.groupKind())
	if errors.Is(err, capi.ErrCRDNotInstalled) {
		return ps, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading retained provider %q: %w", name, err)
	}
	ps.ChildRef = &infrav1.ChildReference{APIVersion: retained.APIVersion, Kind: binding.Kind, Name: binding.ChildName}
	ps.Message = fmt.Sprintf("provider %q is unavailable; its bound child %s/%s is retained", name, binding.Kind, binding.ChildName)
	return ps, nil
}

func (r *CompositeClusterReconciler) resolveProviderPlanKind(
	ctx context.Context,
	p *providerPlan,
) (schema.GroupVersionKind, capi.ChildContract, string, error) {
	return r.resolveProviderGroupKind(ctx, p.binding.groupKind(), string(p.spec.Name))
}

func (r *CompositeClusterReconciler) resolveProviderGroupKind(
	ctx context.Context,
	gk schema.GroupKind,
	provider string,
) (schema.GroupVersionKind, capi.ChildContract, string, error) {
	contract, err := r.Contracts.Get(ctx, gk)
	if err != nil {
		if errors.Is(err, capi.ErrCRDNotInstalled) {
			return schema.GroupVersionKind{}, capi.ChildContract{}, infrav1.ProviderNotInstalledReason, nil
		}

		return schema.GroupVersionKind{}, capi.ChildContract{}, "", fmt.Errorf("resolving %s for provider %q: %w", gk, provider, err)
	}

	return schema.FromAPIVersionAndKind(contract.APIVersion, gk.Kind), contract, "", nil
}

// rejection is a payload-class apply failure: the tenant's problem to read
// in status, not the controller's to retry.
type rejection struct {
	reason  string
	message string
}

// applyChild dry-run validates and applies one rendered object with dual
// ownership.
func (r *CompositeClusterReconciler) applyChild(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	obj *unstructured.Unstructured,
	gvk schema.GroupVersionKind,
) (*unstructured.Unstructured, *rejection, error) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[clusterv1.ClusterNameLabel] = cluster.Name
	obj.SetLabels(labels)
	own := capi.Ownership{
		Controller: cc, Cluster: ownerClusterStub(cluster), ClusterAPIVersion: r.Clusters.APIVersion(),
		Labels: obj.GetLabels(), Annotations: obj.GetAnnotations(),
	}
	if err := capi.PrepareChildForApply(ctx, r.Client, obj, own); err != nil {
		if errors.Is(err, capi.ErrChildNotOwned) {
			return nil, &rejection{infrav1.ChildNotOwnedReason, childDiagnostic(obj, err)}, nil
		}
		return nil, nil, fmt.Errorf("applyChild: preparing field ownership: %w", err)
	}
	if err := capi.DryRunChild(ctx, r.Client, obj); err != nil {
		if capi.IsPayloadRejection(err) {
			return nil, &rejection{infrav1.SchemaValidationFailedReason, childDiagnostic(obj, err)}, nil
		}

		return nil, nil, fmt.Errorf("applyChild: %w", err)
	}

	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, nil, fmt.Errorf("applyChild: reading rendered spec: %w", err)
	}

	applied, err := capi.ApplyChild(ctx, r.Client, gvk, obj.GetName(), obj.GetNamespace(), spec, own)
	if err != nil {
		if errors.Is(err, capi.ErrChildNotOwned) {
			return nil, &rejection{infrav1.ChildNotOwnedReason, childDiagnostic(obj, err)}, nil
		}

		return nil, nil, fmt.Errorf("applyChild: %w", err)
	}

	return applied, nil, nil
}

// reconcileBootstrapTemplates mints the stable per-provider
// KubeadmConfigTemplates from the bootstrap cascade. Called only after
// EndpointPropagated — the ordering the whole design hinges on.
func (r *CompositeClusterReconciler) reconcileBootstrapTemplates(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	cluster *clusterFacts,
	statuses []infrav1.ProviderStatus,
	endpoint infrav1.APIEndpoint,
	providers []providerPlan,
) (bool, error) {
	if len(providers) == 0 {
		return true, nil // No bootstrap templates require an endpoint yet.
	}
	if !endpoint.IsValid() {
		return false, nil
	}
	ready := true

	for i := range providers {
		prepared := &providers[i]

		if !providerApplied(&statuses[i]) {
			// The provider's own child failed; its bootstrap can wait.
			ready = false

			continue
		}

		obj := prepared.renderBootstrap(cc)

		_, rejected, err := r.applyChild(ctx, cc, cluster, obj, kubeadmConfigTemplateGVK)
		if err != nil {
			return false, err
		}

		if rejected != nil {
			statuses[i].Reason, statuses[i].Message = rejected.reason, rejected.message
			ready = false

			continue
		}

		statuses[i].BootstrapTemplateRef = &infrav1.ChildReference{
			APIVersion: mint.KubeadmConfigTemplateAPIVersion,
			Kind:       mint.KubeadmConfigTemplateKind,
			Name:       obj.GetName(),
		}
	}

	return ready, nil
}

// reconcileDelete tears the cluster down: the provider children, then — once
// every child is gone — the minted templates, and only then the finalizer.
// Workers are not this object's to tear down: CAPI deletes a Cluster's
// MachineDeployments before it deletes the infrastructure object, and the
// providers refuse to delete an InfraCluster under live machines, so the
// finalizer holds by construction. The templates go last so that a
// CompositeCluster deleted directly, ahead of its workers, does not strip the
// bootstrap of machines that still exist.
func (r *CompositeClusterReconciler) reconcileDelete(ctx context.Context, cc *infrav1.CompositeCluster) (ctrl.Result, error) {
	setCondition(&cc.Status.Conditions, cc.Generation,
		infrav1.DeletingCondition, metav1.ConditionTrue, infrav1.DeletingReason, "")
	setFalse(&cc.Status.Conditions, cc.Generation, infrav1.ReadyCondition, infrav1.DeletingReason, "")

	remaining, attachments, err := r.deleteCompositionInfrastructure(ctx, cc)
	if err != nil {
		return ctrl.Result{}, err
	}

	if remaining {
		return settling, nil
	}

	remaining, err = r.deleteAttachedBootstrap(ctx, cc, attachments)
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining {
		return settling, nil
	}

	if controllerutil.RemoveFinalizer(cc, infrav1.ClusterFinalizer) {
		if err := r.Update(ctx, cc); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcileDelete: removing finalizer: %w", err)
		}
		r.emitDeletionCompleted(ctx, cc)
	}

	return ctrl.Result{}, nil
}

func (r *CompositeClusterReconciler) deleteCompositionInfrastructure(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
) (bool, attachmentBindings, error) {
	attachments, err := readAttachmentBindings(cc)
	if err != nil {
		return false, attachments, err
	}
	remaining, err := r.deleteAttachedInfrastructure(ctx, cc, attachments)
	return remaining, attachments, err
}

// childGone deletes the child if it still exists and reports whether it is
// fully gone.
func (r *CompositeClusterReconciler) childGone(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	gvk schema.GroupVersionKind,
	name string,
) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	err := r.apiReader().Get(ctx, client.ObjectKey{Name: name, Namespace: cc.Namespace}, obj)
	switch {
	case capi.IsObjectNotFound(err, name):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("childGone: getting %s %q: %w", gvk.Kind, name, err)
	}

	if err := capi.CheckChildOwnership(obj, compositeOwnership(cc), r.Scheme()); err != nil {
		return false, fmt.Errorf("checking child ownership: %w", err)
	}
	if obj.GetDeletionTimestamp() == nil {
		uid, version := obj.GetUID(), obj.GetResourceVersion()
		if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("childGone: deleting %s %q: %w", gvk.Kind, name, err)
		}
	}

	return false, nil
}

func (r *CompositeClusterReconciler) markWaitingForCluster(cc *infrav1.CompositeCluster) {
	for _, cond := range []string{
		infrav1.ReadyCondition,
		infrav1.ProvidersReadyCondition,
		infrav1.EndpointPropagatedCondition,
		infrav1.BootstrapTemplatesReadyCondition,
	} {
		setFalse(&cc.Status.Conditions, cc.Generation, cond, infrav1.WaitingForClusterReason, "")
	}

	cc.Status.Reason = infrav1.WaitingForClusterReason
	cc.Status.ObservedGeneration = cc.Generation
}

// markDeleting records that the owning Cluster is tearing down and this
// object is waiting its turn.
func (r *CompositeClusterReconciler) markDeleting(cc *infrav1.CompositeCluster) {
	setCondition(&cc.Status.Conditions, cc.Generation,
		infrav1.DeletingCondition, metav1.ConditionTrue, infrav1.DeletingReason, "the owning Cluster is being deleted")
	setFalse(&cc.Status.Conditions, cc.Generation, infrav1.ReadyCondition, infrav1.DeletingReason, "")
	cc.Status.Reason = infrav1.DeletingReason
	cc.Status.ObservedGeneration = cc.Generation
}

func (r *CompositeClusterReconciler) patchStatus(ctx context.Context, original, cc *infrav1.CompositeCluster) error {
	if err := r.Status().Patch(ctx, cc, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching CompositeCluster status: %w", err)
	}
	r.emitStateChange(ctx, original, cc)
	return nil
}

// summary is what the reconcile established, for the status writer.
type summary struct {
	failureDomains       []infrav1.FailureDomain
	providersReady       bool
	providersProvisioned bool
	endpointPropagated   bool
	bootstrapReady       bool
}

func providerApplied(ps *infrav1.ProviderStatus) bool {
	return ps.ChildRef != nil && ps.Reason != infrav1.ProviderNotInstalledReason
}

func providerProvisioned(ps *infrav1.ProviderStatus) bool {
	return providerApplied(ps) && ptr.Deref(ps.Provisioned, false)
}

func providerHealthy(ps *infrav1.ProviderStatus) bool {
	return providerProvisioned(ps) && ps.Reason == ""
}

func isExternallyManaged(cc *infrav1.CompositeCluster) bool {
	return cc.Labels[clusterv1.ManagedByAnnotation] != "" || cc.Annotations[clusterv1.ManagedByAnnotation] != ""
}

func clusterChanged(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
		!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
}

// summarize writes the conditions and contract readiness from one
// reconcile's outcome.
func summarize(cc *infrav1.CompositeCluster, statuses []infrav1.ProviderStatus, s summary) {
	gen := cc.Generation
	conds := &cc.Status.Conditions

	if s.providersReady {
		setTrue(conds, gen, infrav1.ProvidersReadyCondition)
	} else {
		setFalse(conds, gen, infrav1.ProvidersReadyCondition,
			worstReason(statuses, infrav1.WaitingForProvidersReason), joinMessages(statuses))
	}

	if s.endpointPropagated {
		setTrue(conds, gen, infrav1.EndpointPropagatedCondition)
	} else {
		setFalse(conds, gen, infrav1.EndpointPropagatedCondition, infrav1.WaitingForEndpointReason, "")
	}

	if s.bootstrapReady {
		setTrue(conds, gen, infrav1.BootstrapTemplatesReadyCondition)
	} else {
		setFalse(conds, gen, infrav1.BootstrapTemplatesReadyCondition,
			bootstrapNotReadyReason(s.endpointPropagated, statuses), "")
	}

	// Provider and node-pool inventory is observed independently after attachment
	// reconciliation; it must not alter the infrastructure contract's readiness.
	cc.Status.FailureDomains = s.failureDomains

	// Initialization is one-shot; Ready below follows current child health.
	provisioned := s.providersProvisioned && s.endpointPropagated
	latched := provisioned || (cc.Status.Initialization.Provisioned != nil && *cc.Status.Initialization.Provisioned)
	cc.Status.Initialization.Provisioned = &latched

	cc.Status.Reason = worstReason(statuses, "")
	cc.Status.Message = clip(joinMessages(statuses))
	cc.Status.ObservedGeneration = gen

	if s.providersReady && s.endpointPropagated && s.bootstrapReady {
		setTrue(conds, gen, infrav1.ReadyCondition)

		return
	}

	setFalse(conds, gen, infrav1.ReadyCondition,
		worstReason(statuses, infrav1.WaitingForProvidersReason), "")
}

// ownerClusterStub builds the minimal typed Cluster ApplyChild's Ownership
// needs (name + UID for the non-controller ownerRef). Never written to the
// API server; the ownerRef's apiVersion comes from Ownership.ClusterAPIVersion,
// the served version, not from this struct's compiled-in one.
func ownerClusterStub(f *clusterFacts) *clusterv1.Cluster {
	return &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name:      f.Name,
		Namespace: f.Namespace,
		UID:       f.UID,
	}}
}

// worstReason picks the first non-empty per-provider reason, falling back to
// def. "Worst" is positional: attachments are reconciled in name order and any
// failure at all makes the summary unhealthy.
func worstReason(statuses []infrav1.ProviderStatus, def string) string {
	for i := range statuses {
		if statuses[i].Reason != "" {
			return statuses[i].Reason
		}
	}

	return def
}

// bootstrapNotReadyReason explains why the bootstrap templates are not
// minted: the endpoint gate first, then whatever broke a provider.
func bootstrapNotReadyReason(endpointPropagated bool, statuses []infrav1.ProviderStatus) string {
	if !endpointPropagated {
		return infrav1.WaitingForEndpointReason
	}

	return worstReason(statuses, infrav1.WaitingForProvidersReason)
}

// providerNames joins the provider names for the printer column.
func providerNames(statuses []infrav1.ProviderStatus) string {
	names := make([]string, 0, len(statuses))
	for i := range statuses {
		names = append(names, string(statuses[i].Name))
	}

	return strings.Join(names, ",")
}

func joinMessages(statuses []infrav1.ProviderStatus) string {
	var parts []string

	for i := range statuses {
		switch {
		case statuses[i].Message != "":
			parts = append(parts, fmt.Sprintf("%s: %s: %s", statuses[i].Name, statuses[i].Reason, statuses[i].Message))
		case statuses[i].Reason != "":
			parts = append(parts, fmt.Sprintf("%s: %s", statuses[i].Name, statuses[i].Reason))
		}
	}

	return strings.Join(parts, "; ")
}

// clip bounds a message to the CRD's MaxLength.
func clip(s string) string {
	if len(s) <= messageLimit {
		return s
	}

	const suffix = "..."
	end := messageLimit - len(suffix)
	for !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + suffix
}
