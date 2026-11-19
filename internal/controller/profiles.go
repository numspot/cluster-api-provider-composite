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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
	"github.com/numspot/cluster-api-provider-composite/internal/capi"
	"github.com/numspot/cluster-api-provider-composite/internal/mint"
	"github.com/numspot/cluster-api-provider-composite/internal/profile"
)

var (
	errProfileUnavailable = errors.New("referenced profile is unavailable or unprotected")
	errProfileMismatch    = errors.New("profile does not match its durable binding")
	errProfilePinMissing  = errors.New("existing resources have no durable profile pin")
)

type providerPlan struct {
	spec    *infrav1.ProviderSpec
	payload *profile.Resolved
	binding providerBinding
}

type compositionPlan struct {
	attachments *attachmentPlan
}

// prepareComposition resolves every profile and checks composition-wide identity
// before persisting any new pin/finalizer or writing a native child. Resolved
// values live only in this per-reconcile plan, never in the desired API object.
func (r *CompositeClusterReconciler) prepareComposition(ctx context.Context, cc *infrav1.CompositeCluster) (*compositionPlan, error) {
	attachments, err := r.planAttachments(ctx, cc)
	if err != nil {
		return nil, err
	}
	if err := r.persistCompositionBindings(ctx, cc, attachments); err != nil {
		return nil, err
	}
	return &compositionPlan{attachments: attachments}, nil
}

func (r *CompositeClusterReconciler) readUsableProfile(ctx context.Context, name string, bound bool) (*infrav1.CompositeProviderTemplate, error) {
	catalog := &infrav1.CompositeProviderTemplate{}
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, catalog); err != nil {
		if apierrors.IsNotFound(err) {
			err = errors.Join(errProfileUnavailable, err)
		}
		return nil, fmt.Errorf("reading referenced profile: %w", err)
	}
	if !controllerutil.ContainsFinalizer(catalog, infrav1.ProviderInUseFinalizer) || (!bound && !catalog.DeletionTimestamp.IsZero()) {
		return nil, errProfileUnavailable
	}
	return catalog, nil
}

func profileBindingFor(
	cc *infrav1.CompositeCluster,
	p *infrav1.ProviderSpec,
	catalog *infrav1.CompositeProviderTemplate,
	templates *profileTemplates,
	previous *providerBinding,
) (providerBinding, error) {
	if catalog.Spec.BootstrapTemplateRef.Namespace == cc.Namespace && catalog.Spec.BootstrapTemplateRef.Name == mint.ClusterKCTName(cc, p) {
		return providerBinding{}, errInvalidTemplate
	}
	binding := providerBinding{
		APIGroup: catalog.Spec.InfraClusterKind.APIGroup, Kind: catalog.Spec.InfraClusterKind.Kind,
		ChildName: mint.ClusterInfraName(cc, p), BootstrapName: mint.ClusterKCTName(cc, p),
		// Profile specs are immutable. Deletion can bump generation, so pin only
		// its UID; source templates retain both UID and generation checks.
		ProfileName: catalog.Name, ProfileUID: catalog.UID,
		InfrastructureRevision: templates.infrastructure, BootstrapRevision: templates.bootstrap,
		Provider: string(catalog.Spec.Provider), Account: catalog.Spec.Account, Region: catalog.Spec.Region,
	}
	if !validProviderTarget(&binding) || !validBindingRecord(&binding) {
		return providerBinding{}, errProfileMismatch
	}
	if previous != nil && *previous != binding {
		return providerBinding{}, profileRevisionDiagnostic(previous, &binding, catalog)
	}
	return binding, nil
}

func expectedAttachmentFailure(err error) bool {
	for _, expected := range []error{
		errProfileUnavailable, errProfileMismatch, errProfilePinMissing, errTemplateUnavailable, errInvalidTemplate,
		profile.ErrInvalidJSON, profile.ErrInvalidPolicy, profile.ErrInvalidCredentials,
		profile.ErrOverrideDenied, profile.ErrProtectedOverride, errInvalidComposition,
	} {
		if errors.Is(err, expected) {
			return true
		}
	}
	return false
}

func profileBindingFailureReason(err error) string {
	switch {
	case errors.Is(err, errProfileMismatch), errors.Is(err, errProfilePinMissing):
		return infrav1.ProfileMismatchReason
	case errors.Is(err, profile.ErrInvalidJSON):
		return infrav1.OverrideRejectedReason
	default:
		return infrav1.ProfileUnavailableReason
	}
}

// Even if all metadata history was lost, an already-owned object cannot be
// assigned today's profile configuration as a guessed replacement for its pin.
func (r *CompositeClusterReconciler) checkUnboundProfileResources(ctx context.Context, cc *infrav1.CompositeCluster, binding *providerBinding) error {
	contract, err := r.Contracts.Get(ctx, binding.groupKind())
	if err != nil && !errors.Is(err, capi.ErrCRDNotInstalled) {
		return fmt.Errorf("observing unbound native API: %w", err)
	}
	if err == nil {
		gvk := schema.FromAPIVersionAndKind(contract.APIVersion, binding.Kind)
		if err := r.rejectUnboundOwnedObject(ctx, cc, gvk, binding.ChildName); err != nil {
			return err
		}
	}
	return r.rejectUnboundOwnedObject(ctx, cc, kubeadmConfigTemplateGVK, binding.BootstrapName)
}

func (r *CompositeClusterReconciler) rejectUnboundOwnedObject(
	ctx context.Context,
	cc *infrav1.CompositeCluster,
	gvk schema.GroupVersionKind,
	name string,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: name}, obj); err != nil {
		if capi.IsObjectNotFound(err, name) {
			return nil
		}
		return fmt.Errorf("checking unbound %s: %w", gvk.Kind, err)
	}
	if owner := metav1.GetControllerOf(obj); owner != nil && owner.UID == cc.UID {
		return errProfilePinMissing
	}
	return nil
}

func (p *providerProjection) failureDomains() ([]infrav1.FailureDomain, error) {
	effective := make([]infrav1.ProviderSpec, len(p.providers))
	byName := make(map[string]*providerPlan, len(p.providers))
	for i := range p.providers {
		prepared := &p.providers[i]
		effective[i] = *prepared.spec
		byName[string(prepared.spec.Name)] = prepared
	}
	domains, err := mint.FailureDomains(effective)
	if err != nil {
		return nil, fmt.Errorf("projecting failure domains: %w", err)
	}
	for i := range domains {
		attributes := domains[i].Attributes
		prepared := byName[attributes["provider"]]
		attributes["provider"] = prepared.binding.Provider
		attributes["attachment"] = string(prepared.spec.Name)
		attributes["account"] = prepared.binding.Account
	}
	return domains, nil
}

func (p *providerPlan) renderInfrastructure(
	cc *infrav1.CompositeCluster,
	gvk schema.GroupVersionKind,
	endpoint infrav1.APIEndpoint,
) (*unstructured.Unstructured, error) {
	obj := mint.RenderInfraClusterSpec(cc, p.spec.Name, gvk, p.binding.ChildName, p.payload.InfraSpec)
	if endpoint.IsValid() {
		if err := mint.WriteEndpointPaths(obj, p.spec.EndpointPaths, endpoint); err != nil {
			return nil, fmt.Errorf("rendering profile endpoint: %w", err)
		}
	}
	return obj, nil
}

func (p *providerPlan) renderBootstrap(cc *infrav1.CompositeCluster) *unstructured.Unstructured {
	return mint.RenderClusterKCTSpec(cc, p.spec.Name, p.binding.BootstrapName, p.payload.Bootstrap)
}
