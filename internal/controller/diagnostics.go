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
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
	"github.com/numspot/cluster-api-provider-composite/internal/capi"
	"github.com/numspot/cluster-api-provider-composite/internal/mint"
	"github.com/numspot/cluster-api-provider-composite/internal/profile"
)

// diagnosticMessage classifies errors without copying their potentially secret
// details. Pin diagnostics carry only explicitly allowlisted, bounded identity.
//
//nolint:gocyclo // Keep the fixed diagnostic allowlist and its precedence together.
func diagnosticMessage(err error) string {
	_, syntaxError := errors.AsType[*json.SyntaxError](err)
	_, typeError := errors.AsType[*json.UnmarshalTypeError](err)
	networkError, isNetworkError := errors.AsType[net.Error](err)
	pinError, isPinError := errors.AsType[*pinDiagnosticError](err)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, capi.ErrChildNotOwned), errors.Is(err, capi.ErrNoControllerOwner), errors.Is(err, capi.ErrFieldOwnership):
		return "Child ownership is unsafe; check controller owner references and field ownership before retrying."
	case errors.Is(err, mint.ErrInvalidFailureDomains):
		return "Failure-domain configuration is invalid; check unique native zones and provider/status capacity limits."
	case errors.Is(err, errNodePoolStatusCapacity):
		return "Node-pool inventory exceeds the status reference limit; inspect native MachineDeployments."
	case errors.Is(err, errInvalidComposition):
		return "Provider composition is invalid; check append-only bindings, distinct child identities and canonical infrastructure names."
	case apierrors.IsForbidden(err):
		return "API access is forbidden; check the controller service account's RBAC permissions for the target resource and namespace."
	case apierrors.IsUnauthorized(err):
		return "API authentication failed; check the controller's credentials."
	case apierrors.IsConflict(err):
		return "API update conflicted; retry reconciliation against the latest resource version."
	case errors.Is(err, errProfileUnavailable):
		return "Referenced profile is missing, deleting or awaiting catalog protection; existing infrastructure is retained."
	case errors.Is(err, errTemplateUnavailable), errors.Is(err, errInvalidTemplate):
		return "Referenced source template is missing, deleting or invalid; check its kind, namespace, name and template payload."
	case isPinError:
		return pinError.message
	case errors.Is(err, errProfileMismatch), errors.Is(err, errProfilePinMissing):
		return "Profile or source-template revision does not match its durable pin; review recovery evidence without rebinding existing infrastructure."
	case errors.Is(err, profile.ErrInvalidCredentials):
		return "Profile credential reference is invalid; check its Secret name and native target paths."
	case errors.Is(err, profile.ErrOverrideDenied),
		errors.Is(err, profile.ErrProtectedOverride),
		errors.Is(err, profile.ErrInvalidPolicy),
		errors.Is(err, profile.ErrInvalidJSON):
		return "Profile defaults or consumer overrides are invalid; check allowed paths, protected configuration and JSON payload shape."
	case errors.Is(err, capi.ErrCRDNotInstalled):
		return "Provider API is not installed; install the required provider CRDs."
	case errors.Is(err, capi.ErrNoContractLabel), errors.Is(err, capi.ErrInvalidContract), errors.Is(err, capi.ErrUnknownContract):
		return "Provider API contract is invalid or unsupported; check CRD contract labels, served versions and namespaced scope."
	case capi.IsPayloadRejection(err), syntaxError, typeError:
		return "Payload rendering or schema validation failed; check provider and bootstrap configuration against the target schema."
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), isNetworkError && networkError.Timeout():
		return "API request timed out; check API availability and connectivity, then retry reconciliation."
	case errors.Is(err, context.Canceled):
		return "Reconciliation was canceled; retry when the controller context is active."
	default:
		return "Reconciliation failed; check controller configuration and API availability."
	}
}

type diagnosticError struct {
	message string
	cause   error
}

func (e *diagnosticError) Error() string {
	return e.message
}

func (e *diagnosticError) Unwrap() error {
	return e.cause
}

func safeReconcileError(err error) error {
	if err == nil {
		return nil
	}
	return &diagnosticError{message: diagnosticMessage(err), cause: err}
}

func childDiagnostic(obj *unstructured.Unstructured, err error) string {
	return clip(fmt.Sprintf("%s %s/%s: %s", obj.GetKind(), obj.GetNamespace(), obj.GetName(), diagnosticMessage(err)))
}

type diagnosticState struct {
	ready                         metav1.ConditionStatus
	reason                        string
	paused, deleting, provisioned bool
}

func stateForDiagnostics(cc *infrav1.CompositeCluster) diagnosticState {
	state := diagnosticState{
		ready:       metav1.ConditionUnknown,
		paused:      isTrue(cc.Status.Conditions, infrav1.PausedCondition),
		deleting:    !cc.DeletionTimestamp.IsZero() || isTrue(cc.Status.Conditions, infrav1.DeletingCondition),
		provisioned: cc.Status.Initialization.Provisioned != nil && *cc.Status.Initialization.Provisioned,
	}
	if ready := apimeta.FindStatusCondition(cc.Status.Conditions, infrav1.ReadyCondition); ready != nil {
		state.reason = ready.Reason
		switch ready.Status {
		case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
			state.ready = ready.Status
		}
	}
	return state
}

func (r *CompositeClusterReconciler) emitStateChange(ctx context.Context, before, after *infrav1.CompositeCluster) {
	state := stateForDiagnostics(after)
	if stateForDiagnostics(before) == state {
		return
	}
	typ, reason := diagnosticEvent(state)
	message := fmt.Sprintf("Ready=%s, provisioned=%t, paused=%t, deleting=%t. Inspect CompositeCluster status for provider references.",
		state.ready, state.provisioned, state.paused, state.deleting)
	ctrl.LoggerFrom(ctx).Info("Composite state changed", "reason", reason, "ready", state.ready,
		"provisioned", state.provisioned, "paused", state.paused, "deleting", state.deleting)
	if r.Recorder != nil {
		r.Recorder.Eventf(after, nil, typ, reason, "Reconcile", "%s", message)
	}
}

func diagnosticEvent(state diagnosticState) (typ, reason string) {
	switch {
	case state.paused:
		return corev1.EventTypeNormal, "Paused"
	case warningDiagnosticReason(state.reason):
		return corev1.EventTypeWarning, state.reason
	case state.deleting:
		return corev1.EventTypeNormal, "Deleting"
	case state.ready == metav1.ConditionUnknown:
		return corev1.EventTypeWarning, "ObservationFailed"
	case state.ready == metav1.ConditionTrue:
		return corev1.EventTypeNormal, "Ready"
	}
	return corev1.EventTypeNormal, "Progressing"
}

func warningDiagnosticReason(reason string) bool {
	switch reason {
	case infrav1.InvalidCompositionReason, infrav1.ChildNotOwnedReason,
		infrav1.SchemaValidationFailedReason, infrav1.RenderFailedReason,
		infrav1.ProfileMismatchReason, infrav1.OverrideRejectedReason:
		return true
	default:
		return false
	}
}

func (r *CompositeClusterReconciler) emitDeletionCompleted(ctx context.Context, cc *infrav1.CompositeCluster) {
	ctrl.LoggerFrom(ctx).Info("Composite deletion completed")
	if r.Recorder != nil {
		r.Recorder.Eventf(
			cc,
			nil,
			corev1.EventTypeNormal,
			"Deleted",
			"Delete",
			"Owned infrastructure children and bootstrap templates have completed deletion.",
		)
	}
}
