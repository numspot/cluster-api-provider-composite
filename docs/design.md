# Design

Composite implements the Cluster API InfraCluster contract. It coordinates native
infrastructure and bootstrap objects; native providers perform cloud operations,
and CAPI manages workers. Adding a provider should require configuration and its
CRDs, not a cloud SDK or provider-specific Go translation layer.

Project rationale and the bootstrap approach are recorded in the
[project foundations ADR](architecture/decisions/0001-project-foundation.md).
[ADR-0002](architecture/decisions/0002-cncf-sigs-conventions.md) records the
CNCF/SIG, contribution and distribution conventions.

## Resources and ownership

```text
Cluster/demo
  controlPlaneRef    -> hosted control plane
  infrastructureRef -> CompositeCluster/demo-infra
                         -> OscCluster/demo-infra
                         -> OpenStackCluster/demo-infra
                         -> KubeadmConfigTemplate/demo-infra-outscale
                         -> KubeadmConfigTemplate/demo-infra-openstack

CompositeProviderTemplate -> native InfraClusterTemplate + KubeadmConfigTemplate references
CompositeProvider -> templateRef + clusterRef + credentialsRef
MachineDeployment -> native MachineTemplate + selected bootstrap template
```

Each generated child has a controller ownerReference to CompositeCluster and a
non-controller ownerReference to its CAPI Cluster. Its Cluster label contains the
actual Cluster name, which can differ from the infrastructure name. Profiles own
neither clusters nor native resources.

Each selected native GroupKind retains one canonical InfraCluster named after
`Cluster.spec.infrastructureRef.name`, because the examined native machine
controllers use that name for lookup. Additional profile-backed attachments may
set immutable `infraClusterName` values. Native identities are unique by GroupKind
and name within the CompositeCluster namespace; a group with no canonical child
is rejected before binding or child writes.

This supports separate regional infrastructure objects without adding a worker
controller. It does not change native worker lookup: regional MachineTemplates
must explicitly select credentials, networks, subnets and security groups where
the native provider supports those overrides. CAPO still consults the canonical
OpenStackCluster for shared readiness/defaulting. CAPOSC's subregion alone does
not select another regional API client. Provider-managed auxiliary objects can
also have names derived from the owning CAPI Cluster, so features such as regional
bastions require separate native-provider compatibility review.

## Configuration

- `CompositeProviderTemplate` is a cluster-scoped, immutable provider/account/region
  revision selecting namespaced source templates. Payloads live in those templates,
  not in the profile CR. All namespaces can reference it. Publish changed configuration under
  a new name; existing attachments do not advance automatically.
- `CompositeProvider` is the sole attachment path. Its immutable `clusterRef`
  selects a local CompositeCluster and `templateRef` selects a reusable profile.
  Membership records are append-only; deleting a provider retires its identity
  through ordered teardown rather than erasing binding history.
- Profile-backed `infraSpec` and `bootstrap` fields are overrides, allowed only by
  the profile's JSON-pointer policy. Region-to-account/profile selection belongs
  to the caller; Composite does not infer it from region names.
- Infrastructure uses ordinary JSON Merge Patch over the native source template's
  `spec.template.spec`. Bootstrap merges shared values, then referenced template
  defaults, then attachment overrides. Objects merge recursively;
  null deletes a member; arrays replace. Bootstrap alone merges two nonempty lists
  by string `name` when every element has one: whole matching entries are replaced
  and new entries appended. Empty lists replace. Numbers retain their exact value.
- Protected settings and endpoint targets cannot be overridden, including through
  an allowed ancestor. Policy checks compare effective changes, not just patch keys.
- Native Secret references follow the native provider's rules. A profile pin does
  not freeze Secret contents or prove which account its credentials access.
- Optional `credentialsRef` selects a Secret name and explicit `namePaths` relative
  to the native infrastructure spec. Composite injects that name into decoded
  source defaults before applying consumer overrides; those target fields are
  automatically protected. Paths must address string properties, never arrays,
  and cannot overlap each other or endpoint targets. Missing object properties
  are created. Templates must use namespace-local native references. Composite
  does not read, validate, copy, own or watch Secret contents, and it does not
  infer cloud-specific credential fields. Other identity settings remain native
  template configuration and should be protected by the profile policy.
- References specify exact source API versions and namespaces. Infrastructure
  template kind/group matches the declared native kind with a `Template` suffix;
  bootstrap sources are v1beta2 KubeadmConfigTemplates. Sources are read through
  the direct API reader and never mutated, adopted or deleted by Composite.

`CompositeClusterTemplate` carries shared bootstrap and endpoint configuration for
ClusterClass and has an immutable template body. Start with an empty
CompositeCluster; provider attachments are separate CompositeProvider resources,
not Cluster topology variables or ClusterClass patches. CAPI manages the Cluster's
infrastructure/control-plane lifecycle, and Composite manages native children.
Full integration needs testing with the intended native stack.

A cluster without CompositeProvider resources is allowed: the hosted control plane
can become available before profiles or worker pools exist. Publishing a profile
does not attach it automatically; create a CompositeProvider referencing it.

The [API reference](usage.md#api-reference) lists the four resources' fields,
scopes and limits. CRD admission enforces required fields, list bounds and immutable
configuration. Reference resolution additionally checks native GroupKinds, effective
override policy, profile-derived zones, and durable identity against live state.
An admitted manifest can therefore still report a reconciliation error.

## Reconciliation and lifecycle

### Namespaced provider attachments

`CompositeProvider` is the concrete provider resource created in the consumer's
namespace. Its immutable `clusterRef` selects the local CompositeCluster;
`templateRef` selects a cluster-wide CompositeProviderTemplate.
`credentialsRef.name` selects native credentials in the same target namespace;
the template declares protected `credentialsNamePaths`. Composite never reads Secrets.
Allowed `infraSpec` and `bootstrap` overrides remain native JSON.

The cluster controller validates the complete selection before writing durable
identity. New attachments pin provider UID, CompositeCluster UID, template UID and
both native-source UID/generation pairs. Cluster bindings and its finalizer are
persisted together; provider pins/finalizers also precede child writes. Source
metadata is not copied. Historical cluster provider-bindings formats are unsupported.

Provider observations live on `CompositeProvider.status`, including current Ready,
latched initialization and infrastructure/bootstrap references. CompositeCluster
mirrors current and draining attachments in `status.providers`, retaining the
existing provider status shape and adding `nodePools` references. Conditions in
that summary refer to the CompositeProvider generation and own their transition
times, including inventory failure/recovery and deletion observations. Attachment
failures and worker availability do not gate CompositeCluster Ready or initialization.
Native failure domains are still published with unchanged zone names; provider
configuration remains exclusively on CompositeProvider resources.

Several native worker pools may consume one provider bootstrap template.
The read-only node-pool inventory includes native MachineDeployments in the same
namespace whose actual Cluster reference and bootstrap GroupKind/name match the
attachment's durable identity. Labels, regions and naming conventions alone do not
establish membership. MachineDeployment events refresh the inventory; direct,
paginated reads produce a sorted snapshot without changing workers or their status.
Completed retirement removes the live status entry, not durable binding history.
MachineSets and Machine bootstrap provenance are additionally observed during
retirement; Composite never manages workers. Provider deletion waits for
consumers and dependent regions, then infrastructure absence, then bootstrap
deletion. Cluster teardown needs no live templates. Unclassifiable Machines and
unavailable APIs retain protection. Retired identities remain recorded; recreated
names/UIDs cannot silently rebind. The eight-attachment and combined 16 KiB record
limits include retained identities.

1. Honor scope, external management and pause; verify the owning Cluster's UID.
2. Resolve profiles and policy, check distinct native identities, canonical lookup
   names and native zone names,
   and reject contradictory ownership/history before new writes.
3. Persist bindings and the initial Composite finalizer in one resourceVersion-
   checked update. The complete serialized record must satisfy the reader's
   format, name, entry-count and **16 KiB** limits before metadata is changed.
4. Render native children, apply endpoint fields, dry-run against installed
   schemas, then create/apply with ownership and UID/resourceVersion checks.
5. Apply each attachment's bootstrap template after its native child was applied
   and an endpoint is available. Another pending attachment does not gate it.
6. Write observations once. A settled reconcile should not change resourceVersions.

Without attachments, the normal owner, pause and binding checks still run.
The controller persists an empty binding envelope and finalizer together, reports
Ready and latched initialization, and creates no native infrastructure or bootstrap
templates. No endpoint is required because there are no propagation targets.
The empty case retains normal five-minute steady polling and finalizer teardown.
Creating a CompositeProvider starts independent native readiness, endpoint and
bootstrap checks; attachment failure does not gate empty-cluster readiness.
Retired attachment identities remain recorded and cannot silently rebind.

The [Docker-provider quickstart](development.md#docker-provider-quickstart) exercises
this model with one canonical CAPD DockerCluster, native kubeadm control plane and
Docker worker. Its runner explicitly publishes CAPD's discovered load-balancer
endpoint on CompositeCluster once; this is test-fixture wiring, not native-to-Composite
endpoint discovery in the controller. Several DockerCluster attachments would share
CAPD's Cluster-name-keyed load balancer, so the fixture intentionally uses only one.

The endpoint comes from CompositeCluster, falling back to the owning Cluster.
Paths default to `spec.controlPlaneEndpoint`; suffixes `port`, `host`/`ip`, or
anything else select a port, host, or `{host, port}` value respectively.

Controller-owned attachment metadata records durable native/bootstrap names,
provider and cluster UIDs, template UID and both source UID/generation pairs.
The CompositeCluster's
`compositecluster.infrastructure.cluster.x-k8s.io/provider-attachments` annotation
holds the versioned map, including retired records. Each CompositeProvider's
`compositeprovider.infrastructure.cluster.x-k8s.io/binding` annotation holds its
matching record. The provider-local pin is strictly decoded and compared by value:
JSON field order, indentation, escaping and trailing whitespace do not change
identity. Unknown/duplicate fields (including escaped spellings), malformed or
trailing JSON, oversized records and changed identity still fail closed. Neither
annotation is user configuration or a supported rebind interface; status cannot
replace either pin.
No configuration hash is computed. Historical cluster provider-bindings records
are unsupported: finish legacy teardown using a compatible controller before
upgrading, then create fresh resource-based attachments. Do not rewrite old pins
into new records or treat status as replacement identity.

Ordinary source spec edits and recreation are detected; metadata-only changes are
allowed. The profile spec is immutable and is pinned by UID only, since deletion
can bump its generation. New UIDs do not match even for identical content; transparent
restore/move and automatic conversion of old profile pins are unsupported in this
pre-1.0 provider.

Status cannot authorize adoption or reconstruct a lost attachment/source pin.
Incomplete ownership or identity evidence stops reconciliation rather than guessing
from today's configuration. Restore verified metadata/backups before retrying.

Deletion waits for owned InfraClusters to disappear, then deletes owned bootstrap
templates, then removes the Composite finalizer. Teardown uses durable bindings
without requiring the profile. An unavailable API is not proof of deletion.

The catalog finalizer checks provider references and durable pins through
global direct reads, regardless of consumer watch scope. Unreadable references
retain protection. New bindings require a protected, non-deleting profile;
existing verified bindings may use a retained deleting profile. Reference creation
and deletion are not a cross-resource transaction: a late unresolved consumer can
outlive a deleted profile and waits with `ProfileUnavailable`, without provisioning.
Create and validate profiles before creating their consumers.

Source templates are administrator-managed and have no Composite deletion finalizer.
Missing/deleting sources block normal reconciliation while existing resources are
retained; teardown continues through durable child identity. Catalog observations
poll every 30 seconds while invalid and every five minutes when valid, including
source objects outside the manager's namespaced cache.

The credential reference is immutable as part of the profile and therefore needs
no additional durable binding format. Existing profiles without it keep their
native source-template references. Rotating Secret contents leaves profile/source
pins unchanged; native providers handle credential reload. A different Secret
name or target-path set requires a new profile revision.

## Contracts and observations

All Composite CRDs serve/store `infrastructure.cluster.x-k8s.io/v1beta2`.
CompositeCluster and its template advertise the v1beta2 InfraCluster contract;
CompositeProvider and CompositeProviderTemplate advertise only provider identity. Generated labels/RBAC/CRDs
are shared by Helm and clusterctl. The [GitHub automation](development.md#github-automation)
validates both Go modules and generated outputs without repairing working source.
One provider release version ties together the operator image/binaries, Helm chart
and clusterctl assets; the [draft-first release workflow](../.github/workflows/release.yml)
checks annotated tag/source identity, main ancestry and declared CAPI release-series
support, then requires CI/Security and the reviewed draft's identity before staging.
Only the final protected job publishes the completed draft and verifies immutability. Tested images are
promoted without rebuilding; both installation formats pin the resulting multiarch
digest and both chart publishers reuse one archive. Repository tag/environment
protections remain required. These pipelines do not deploy clusters or change
controller/runtime semantics.

[clusterctl registration](../clusterctl.yaml) adds the custom `composite`
infrastructure provider without changing CAPI's built-in catalog. Versioned
components include the controller, four CRDs and RBAC; metadata maps provider
series 0.1 to CAPI contract v1beta2. clusterctl handles namespace rewriting, labels
and its separate Provider inventory. Concrete component defaults need no optional
watch-filter variable; native credentials and workloads remain caller-managed.

`CompositeCluster`, `CompositeProvider` and `CompositeProviderTemplate` expose status subresources;
`CompositeClusterTemplate` does not. Profile status reports `ConfigurationValid`
and its observed generation for source/policy validity, not native provisioning.
CompositeCluster uses `status.initialization.provisioned` and current conditions;
the historical Composite `status.ready` field is not in the current schema.

Resolve each native contract and served version independently from current CRD
labels. v1beta1 initialization is `status.ready`; v1beta2 initialization is
`status.initialization.provisioned`. Bootstrap uses served v1beta2
KubeadmConfigTemplates. Writing through unintended conversions can cause field
ownership drift and repeated updates.

Initialization is latched after success; Ready reports current health. Explicit
False, Unknown and stale native conditions must not become healthy from an old
initialization bit. Check condition generations and pause/deletion state.
Profile-backed attachment conditions supplement aggregate health; failed expansion
can leave earlier observations at their previous generation.

Native failure-domain names remain unchanged and must be unique across the
composition. Limits are eight attachments, sixteen zones per attachment and one
hundred published domains. They are not control-plane placement domains.

## Boundaries to preserve

- No Composite worker controller, automatic worker rollout or cloud-specific SDKs.
- No ambiguous same-kind/name attachments, implicit native renaming or automatic
  legacy-to-provider conversion. Native workers retain their provider's capabilities
  and canonical-context dependencies.
- CNI, CSI, routing, node initialization and cloud-controller placement belong to
  the native stack. A cluster-wide cloud node controller must not act on another
  provider's nodes. Do not set `cloud-provider=external` without the corresponding
  initialization mechanism.
- Schema/API-version changes require round-trip and migration tests. Older
  controllers may not understand current attachment pins; downgrading CRDs can
  prune immutable fields and must not be used as a migration.
- Keep `api/` independent of CAPI, controller-runtime and generator/linter modules.
  Preserve optional boolean presence and existing wire shapes when evolving it.

Implementation: [controllers](../internal/controller/), [native contract/ownership](../internal/capi/),
[rendering](../internal/mint/), [profile resolution](../internal/profile/) and
[API types](../api/v1beta2/). Tests document the failure and recovery cases.
