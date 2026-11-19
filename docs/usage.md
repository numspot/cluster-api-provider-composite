# Usage

Composite combines provider-native infrastructure behind one CAPI Cluster. Start
with [installation](operations.md), an empty cluster and [provider attachments](#create-a-cluster-attachment).
Provider attachments use namespaced `CompositeProvider` resources exclusively.
The examples need compatible native providers, credentials, a hosted
control plane and networking; they are not a complete cloud deployment.

After a successful automated release, install the matching GitHub/OCI artifacts as
explained in [automated releases](operations.md#automated-releases). Image, chart
and clusterctl assets share a provider version; candidate source versions do not
prove publication. None of the release workflows creates providers, worker pools
or workload clusters on your behalf. Choose either the
[clusterctl installation](operations.md#clusterctl), using the checked-in custom
provider config and a published version, or Helm—not both for the same controller.
clusterctl's provider inventory is installation metadata, not a provider attachment.

To exercise a real provider locally without cloud credentials, start with the
[Docker-provider quickstart](development.md#docker-provider-quickstart). Its
[native Docker profile](../local/quickstart-templates.yaml) and
[consumer/worker manifest](../local/quickstart.yaml) use the same attachment API
shown below. The runner explicitly publishes CAPD's discovered endpoint and checks
native worker availability as well as CompositeProvider Ready.

Select the management kubeconfig for this shell before running the examples:

```sh
export KUBECONFIG=/path/to/management.yaml
```

## API reference

All four Composite resources use `infrastructure.cluster.x-k8s.io/v1beta2`.
The current [generated CRDs](../api/config/crd/bases/) define admission rules;
[Go types](../api/v1beta2/) describe their wire fields. Native child API versions
are resolved separately from the native CRDs' contract labels.

### CompositeProviderTemplate

`CompositeProviderTemplate` is cluster-scoped (`kubectl get cpt`). Its entire `spec` is
immutable, including source-template references and override policy. Native payloads
live in native infrastructure/bootstrap templates, not in the profile CR. Publish
new named templates and a new profile for a changed revision.

| Field under `spec` | Required | Meaning |
|---|---|---|
| `provider` | Yes | Logical provider identifier; a DNS label of at most 63 characters |
| `account`, `region` | Yes | Administrative identities, each 1–63 characters |
| `credentialsRef` | No | Immutable namespace-local Secret `name` and 1–8 `namePaths` receiving that name in the native infrastructure spec |
| `credentialsNamePaths` | No | Protected native Secret-name paths; the namespaced provider supplies the name instead of fixed template credentials |
| `infraClusterKind.apiGroup`, `infraClusterKind.kind` | Yes | Native InfraCluster GroupKind; excludes Composite resource kinds |
| `zones` | No | Up to 16 distinct native failure-domain names |
| `infrastructureTemplateRef` | Yes | Explicit `apiVersion`, `kind`, `namespace`, `name` of a native InfraClusterTemplate |
| `bootstrapTemplateRef` | Yes | Explicit reference to a v1beta2 KubeadmConfigTemplate |
| `endpointPaths` | No | Up to eight dotted native paths; empty uses `spec.controlPlaneEndpoint` |
| `overridePolicy` | No | Allowed and protected infrastructure/bootstrap subtrees |

The policy fields are `infraSpecPaths`, `bootstrapPaths`,
`protectedInfraSpecPaths` and `protectedBootstrapPaths`, each with up to 32 JSON
pointers. Pointers are relative to their payload: `/network/net/ipRange`, not
`/spec/network/net/ipRange`. Escape `~` and `/` as `~0` and `~1`; root/empty
pointers, empty segments and wildcards are unsupported. An omitted/empty policy
allows no effective consumer changes. Protected paths and endpoint targets remain
protected under an allowed ancestor.

Infrastructure template kind/group must match `infraClusterKind` with a `Template`
suffix. References select exact served API versions and namespaces. Only
`spec.template.spec` is copied; Composite never adopts, mutates or deletes shared
source templates. Native Secret references still resolve in the consumer's context.

Use `credentialsRef` to select credentials independently of shared source-template
payloads. For example, an Outscale profile can contain this spec fragment:

```yaml
provider: outscale
account: account-a
region: eu-west-2
credentialsRef:
  name: outscale-account-a-eu-west-2
  namePaths: [/credentials/fromSecret]
```

For OpenStack, a typical target is `/identityRef/name`; keep `type`, `cloudName`
and `region` in the native template. `namePaths` are JSON pointers relative to the
native `spec`, with the same escaping and 512-character limit as policy pointers.
They must be distinct, non-overlapping object-property paths outside endpoint
targets. Existing leaves must be strings or null; missing objects/properties are
created. Array traversal and replacing scalar parents are rejected.

The selected Secret belongs in the consuming namespace and must have the native
provider's expected format. Templates must use namespace-local references.
Composite injects only its name and automatically protects each target from
consumer changes, including deletion/replacement through an allowed ancestor.
Protect additional identity fields, such as region, cloud name or explicit
namespace, with the ordinary policy. `account` remains descriptive, not a lookup
or authentication mechanism. Composite does not read Secrets or check their
existence/contents; `ConfigurationValid` is not credential validation.

Omitting `credentialsRef` preserves source-template credential references. Its
name/path set cannot be added, removed or changed on an existing immutable profile;
publish a new profile revision instead. Secret contents can rotate in place
according to native-provider behavior without changing profile/source pins.

### CompositeProvider

`CompositeProvider` is namespaced (`kubectl get cprov -n <namespace>`). Create one
per provider attachment; several native worker pools may consume its bootstrap
output. Provider names are DNS labels up to 63 characters.

| Field under `spec` | Required | Meaning |
|---|---|---|
| `clusterRef.name` | Yes | CompositeCluster in the provider's namespace |
| `templateRef.name` | Yes | Cluster-wide CompositeProviderTemplate revision |
| `credentialsRef.name` | When template credential paths are declared | Namespace-local native Secret name; Composite does not read its contents |
| `infraClusterName` | No | Explicit additional regional native name; omission keeps the canonical cluster name |
| `infraSpec`, `bootstrap` | No | Overrides allowed by the template policy |

Cluster/template/credential references and the native name are immutable. Source
UID/generation changes block reconciliation rather than transparently rebinding.
`status.conditions` and `observedGeneration` report current attachment health;
`status.infrastructureRef` and `bootstrapTemplateRef` identify generated resources.
`status.initialization.provisioned` latches success independently of current Ready.
Worker availability is reported by native CAPI MachineDeployments.

### CompositeCluster

`CompositeCluster` is namespaced (`kubectl get cocl`). Its spec carries shared
bootstrap and endpoint configuration, not provider attachments. Use `spec: {}` to start
without either setting. Create a CompositeProvider in the same namespace for each
attachment; there is no `spec.providers` configuration.

Each native GroupKind requires a canonical child named after the CompositeCluster.
Additional regional providers may set immutable `infraClusterName` values. Native
GroupKind/name identities must be distinct, with at most eight retained attachments
and 100 published failure domains. Profile-derived zones and native identities are
checked during reconciliation.

`spec.bootstrap` is optional shared kubeadm configuration. Each generated template
uses shared bootstrap, then referenced template defaults, then attachment overrides.
`spec.controlPlaneEndpoint` is optional; when present it must contain a nonempty
`host` (up to 512 characters), a `port` in 1–65535, or both. Endpoint propagation
requires a complete endpoint, with the owning Cluster providing the fallback.

Endpoint paths use dot notation beginning with `spec.`, such as
`spec.controlPlaneEndpoint` or `spec.apiServerFixedIP`; they are not policy JSON
pointers. There are at most eight paths of at most 512 characters per provider
profile. The endpoint value selected by each path is described in
[design](design.md#reconciliation-and-lifecycle).

Provider configuration stays outside the cluster spec, but
`status.providers` summarizes each current or draining CompositeProvider:
attachment conditions, profile identity, infrastructure/bootstrap references and
latched initialization. Its `nodePools` list contains versioned references to the
native MachineDeployments consuming that attachment's bootstrap template:

```yaml
status:
  providerNames: outscale-main
  providers:
    - name: outscale-main
      nodePools:
        - apiVersion: cluster.x-k8s.io/v1beta2
          kind: MachineDeployment
          name: demo-workers
```

Membership requires the actual owning Cluster reference, the same namespace and
the exact generated bootstrap GroupKind/name from a verified attachment binding.
Provider labels or matching regions do not establish membership. Node pools remain
native: inspect their own status for replica counts, rollout and availability.
Neither pool availability nor attachment readiness gates CompositeCluster Ready.
Draining providers stay visible until retirement completes; retired identities
remain in binding history rather than the live provider inventory. Each provider
status holds at most 100 pool references. An unreadable or larger inventory retains
the last complete snapshot with provider Ready=Unknown; this is a status capacity
limit, not a limit on creating native worker pools.

### CompositeClusterTemplate

`CompositeClusterTemplate` is namespaced (`kubectl get cct`).
`spec.template.spec` contains the same `CompositeClusterSpec` described above.
Optional `spec.template.metadata` accepts labels and annotations. The entire
`spec.template`, including this metadata, is immutable. Templates have no status
subresource and do not attach or retain provider profiles.

To inspect the schema actually installed on a management cluster:

```sh
kubectl explain compositeproviders.spec --api-version=infrastructure.cluster.x-k8s.io/v1beta2
kubectl explain compositeprovidertemplates.spec --api-version=infrastructure.cluster.x-k8s.io/v1beta2
kubectl explain compositeclusters.spec --api-version=infrastructure.cluster.x-k8s.io/v1beta2
kubectl explain compositeclustertemplates.spec.template --api-version=infrastructure.cluster.x-k8s.io/v1beta2
```

## Create a profile

Create sources from [local/compositeprovider-templates.yaml](../local/compositeprovider-templates.yaml),
then profiles from [local/compositeprovider-sample.yaml](../local/compositeprovider-sample.yaml).
Edit account/region, source references, native Secret references and template payloads
before their first use. Store credentials in each native provider's supported format.

```sh
kubectl apply -f local/compositeprovider-templates.yaml
kubectl apply -f local/compositeprovider-sample.yaml
kubectl wait --for=condition=ConfigurationValid=True \
  compositeprovidertemplate/outscale-account-a-r1 --timeout=60s
```

`ConfigurationValid` checks source-template availability, payload shape and override
policy, not cloud credentials or working machines. Account/region fields are declarations:
the native credential/configuration must actually select them. Profiles can be
used from every namespace, but native
Secret references still obey their provider's namespace rules. Composite does not
copy Secrets.

Copyable consumers are in [the single-cloud example](../local/compositecluster-sample.yaml)
and [the spanning example](../local/compositecluster-spanning-sample.yaml). Both
include the owning CAPI Cluster and native worker templates; supply their referenced
hosted control planes and adapt worker images, keypairs and networking. These are
new-cluster examples, not migrations of existing durable bindings.

## Create a cluster attachment

Provide the hosted control-plane object separately. This example references a
KamajiControlPlane named `demo-control-plane`; configure its controller to support
Composite endpoint publication as described in [operations](operations.md).

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
  namespace: demo
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: CompositeCluster
    name: demo-infra
  controlPlaneRef:
    apiGroup: controlplane.cluster.x-k8s.io
    kind: KamajiControlPlane
    name: demo-control-plane
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeCluster
metadata:
  name: demo-infra
  namespace: demo
spec: {}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeProvider
metadata:
  name: outscale-main
  namespace: demo
spec:
  clusterRef: {name: demo-infra}
  templateRef: {name: outscale-account-a-r1}
  infraSpec:
    network:
      clusterName: demo
      internetService:
        name: demo
      net:
        ipRange: 10.42.0.0/16
```

```sh
kubectl apply -f demo-cluster.yaml
kubectl get cluster,compositecluster,compositeprovider -n demo
kubectl wait --for=condition=Ready=True \
  compositeprovider/outscale-main -n demo --timeout=10m
```

CAPI establishes the owner reference. Composite waits for a verified owner before
creating bindings or children. A successful attachment creates:

| Resource | Namespace/name |
|---|---|
| CAPI Cluster | `demo/demo` |
| CompositeCluster | `demo/demo-infra` |
| CompositeProvider | `demo/outscale-main` |
| OscCluster | `demo/demo-infra` |
| KubeadmConfigTemplate | `demo/demo-infra-outscale-main` |

Attachments need no `PROVIDER_KINDS` deployment mapping. Native kinds, regions,
zones, endpoints and source defaults come from the selected template. Custom
native API groups need explicit RBAC.

## Start before provider attachments

To bootstrap a hosted control plane before provider attachments and worker pools,
create the owning CAPI Cluster with its Composite infrastructure reference and use:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeCluster
metadata:
  name: demo-infra
  namespace: demo
spec: {}
```

After CAPI establishes ownership, Composite reports `Ready=True` and
`status.initialization.provisioned=true` without native children, bootstrap
templates or an endpoint. Pause, ownership and durable binding checks still apply.
This reports infrastructure readiness only; check the hosted control-plane
provider and CAPI Cluster status separately before treating the cluster as usable.

Provision the provider/region Secret and profile later, then create a
CompositeProvider referencing that CompositeCluster. Publishing a profile alone
does not attach it. Each provider requires native readiness and an endpoint for
its bootstrap template; observe its own Ready condition rather than assuming
cluster readiness proves attachment health. The first child of each GroupKind
must retain the canonical infrastructure name. Provider deletion follows ordered
retirement; its durable identity is retained rather than removed from history.

## Overrides, reuse and revisions

Edit permitted `infraSpec`/`bootstrap` overrides and reapply. Infrastructure arrays
replace; bootstrap has the named-list extension described in [design](design.md).
For the example profile, a bootstrap override can replace the command list:

```yaml
bootstrap:
  preKubeadmCommands:
    - echo preparing-worker
```

Forbidden effective changes are stored as desired configuration but reported as
`OverrideRejected` during reconciliation. Correct them and retry. Endpoint targets
are controller-owned even under an allowed ancestor.

Another cluster may reference the same profile with its own allowed overrides.
Native Secret references resolve according to that provider's namespace rules;
changing a protected credential reference requires another profile revision.
Native objects remain separately owned. To expand a cluster, create a
CompositeProvider for a different GroupKind or a profile-backed region with an explicit distinct native name as
described below. The canonical attachment for each GroupKind remains in place.

Profiles and source templates are versioned configurations. New bindings pin profile
UID and source-template UID/generation; no custom content fingerprint is computed.
Editing or recreating a source blocks an existing consumer even when its name or
content is identical. Metadata-only source changes are allowed. Create changed
templates and a new profile for new consumers. Existing provider names, cluster/
template/credential references and native names cannot be rebound. Revisions do
not advance existing pins; deleting a provider retires its identity safely.

The source-reference schema replaces embedded profile `defaults` in this pre-1.0
provider API. It does not automatically migrate previously created profiles or hash-based
bindings. Back up existing resources before changing CRDs; use fresh revisions and
consumers for evaluation. See [recovery](operations.md#recovery).

## Same-provider regional infrastructure

Keep a CompositeProviderTemplate revision per account/region and reference each regional
infrastructure attachment explicitly. For example, with profiles already created:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeProvider
metadata:
  name: tenant-regional-paris
  namespace: default
spec:
  clusterRef: {name: tenant-regional}
  templateRef: {name: openstack-account-a-r1}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeProvider
metadata:
  name: tenant-regional-varsovie
  namespace: default
spec:
  clusterRef: {name: tenant-regional}
  templateRef: {name: openstack-account-a-waw-r1}
  infraClusterName: tenant-regional-varsovie-infra
```

The [regional example](../local/compositecluster-regional-sample.yaml) includes the
additional Warsaw profile and owning CAPI Cluster. This produces:

| Attachment | Native infrastructure | Generated bootstrap template |
|---|---|---|
| Outscale | `OscCluster/tenant-regional` | `tenant-regional-tenant-regional-outscale` |
| Paris | `OpenStackCluster/tenant-regional` | `tenant-regional-tenant-regional-paris` |
| Varsovie | `OpenStackCluster/tenant-regional-varsovie-infra` | `tenant-regional-tenant-regional-varsovie` |

Native names and bootstrap names are independent. Use names unique to each
CompositeCluster within its namespace. With ClusterClass, create providers after
observing the generated infrastructure name; supply cluster-specific `clusterRef` and regional `infraClusterName` values,
not provider topology patches.

CAPI manages MachineDeployments/Machines and native providers manage their VMs.
There is no Composite node-pool resource. Native workers still perform the canonical
InfraCluster lookup; naming another regional object does not automatically select
it for a worker. With CAPO, use a native regional MachineTemplate with explicit
`identityRef` and `ports` selecting that region's network, subnets and security
groups, plus the matching bootstrap template. In particular, per-port security
groups avoid inheriting the canonical cluster's group. CAPO retains canonical
readiness/defaulting dependencies. A CAPOSC `subregionName` is an availability-zone
choice, not an override of the native cluster/controller's regional API client.

Verify the native stack's behavior for each enabled feature. Auxiliary resources
such as CAPO bastion OpenStackServers may derive their names from the CAPI Cluster;
keep bastions disabled for this multi-region pattern unless the native provider
supports distinct instances. Regional networks and credentials must actually
select the declared profile region.

Current attachments retain durable provider, cluster, template and source identities
for canonical and explicit native names. Historical cluster provider-bindings
formats are unsupported. Finish legacy teardown with the compatible controller
before upgrading; use fresh CompositeProvider resources rather than converting
old records or relying on status to recover pins.

## Templates and workers

An infrastructure template contains only shared cluster configuration:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: CompositeClusterTemplate
metadata:
  name: composite-template
  namespace: demo
spec:
  template:
    spec: {}
```

Create CompositeProvider resources separately, referencing the concrete generated
CompositeCluster name. ClusterClass does not patch provider selections.

Create ordinary MachineDeployments and native MachineTemplates through manifests,
GitOps or your orchestration layer. This MachineDeployment fragment selects the
generated bootstrap template:

```yaml
spec:
  clusterName: demo
  template:
    spec:
      clusterName: demo
      failureDomain: eu-west-2a
      bootstrap:
        configRef:
          apiGroup: bootstrap.cluster.x-k8s.io
          kind: KubeadmConfigTemplate
          name: demo-infra-outscale-main
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: OscMachineTemplate
        name: demo-workers
```

Supply version, replicas and the native MachineTemplate separately. A pool may
instead use its own bootstrap template. Composite does not create or roll workers;
its Ready condition is not evidence of a successful worker join. Provide compatible
node initialization, CNI, CSI and cloud-controller configuration for your stack.

## Observe and troubleshoot

```sh
kubectl get compositecluster demo-infra -n demo -o yaml
kubectl get osccluster demo-infra -n demo -o yaml
kubectl get kubeadmconfigtemplate demo-infra-outscale-main -n demo -o yaml
```

| Status field | Interpretation |
|---|---|
| Cluster `conditions` | Current `Ready`, `ProvidersReady`, `EndpointPropagated`, `BootstrapTemplatesReady`, `Paused` and `Deleting` observations |
| Cluster `initialization.provisioned` | Latched initial success; this replaces the old Composite `status.ready` field |
| Cluster `failureDomains[]` | Native `name`, optional `controlPlane` and attributes; published control-plane suitability is false |
| Cluster `providers[]` | Current/draining attachment `name`, mirrored `conditions`, native `childRef`, `bootstrapTemplateRef`, latched `provisioned`, `reason` and `message` |
| Cluster `providers[].nodePools[]` | Read-only native MachineDeployment references (`apiVersion`, `kind`, `name`); pool health remains on native resources |
| Profile-backed `providers[].profile` | Profile `name`, logical `provider`, `account` and `region`; source pins are durable metadata |
| Provider `conditions`, `observedGeneration` | Current attachment health, independent of cluster readiness |
| Cluster `providerNames`, `reason`, `message` | Printer-column and aggregate diagnostic values |
| Profile `conditions`, `observedGeneration` | Catalog `ConfigurationValid` observations; not infrastructure provisioning |

`providers[].outputs` is reserved and currently unpopulated. Conditions are keyed
by `type` and limited to 32 entries per condition list. Cluster and profile status
record `observedGeneration`; individual conditions also carry their generation.
Conditions under `providers[]` refer to the CompositeProvider generation, not the
CompositeCluster generation. Check generations, pause and deletion state before treating an observation as
current. Initialization stays latched after first success, whereas Ready tracks health.
Each attachment is prepared and observed independently: a local profile, source,
pin or native-operation failure does not prevent valid new attachments or another
provider's recovery. Global identity/canonical, zone and binding-record validation
still precedes every binding or native write. Failed known providers keep their
existing pins and references; failed first attachments do not gain invented history.
Conditions are not heartbeats: unchanged observations preserve transition times and
do not repeat status writes or state Events.

The controller-owned binding annotation is authoritative identity, not editable
configuration. See [operations](operations.md#recovery) for recovery precautions.

| Observation | Check |
|---|---|
| `WaitingForCluster` | CAPI owner, infrastructure reference and owner UID |
| `ProfileUnavailable` | Profile and source-template references, versions, namespaces, deletion state and permissions |
| `ProfileMismatch` | Profile UID or source UID/generation differs from the pin; do not rebind from status |
| `OverrideRejected` | Effective allowed/protected paths, shared bootstrap and JSON shape |
| `InvalidComposition` | Distinct GroupKind/name identities, a canonical child per kind, unique zones, immutable names and binding-size limits |
| `ProviderNotInstalled` | Native CRD and served contract labels |
| `SchemaValidationFailed` | Native payload versus installed schema |
| Native child unready | Native resource conditions and provider controller logs |
| Profile Terminating | Global CompositeProvider references and durable pins |

## Cleanup

Delete through the layer that owns the Cluster. For directly managed resources:

```sh
kubectl delete cluster demo -n demo --wait=false
kubectl get compositecluster demo-infra -n demo -o yaml
```

CAPI/native controllers handle workers. Composite waits for owned infrastructure,
then removes its bootstrap templates and finalizer. Unavailable APIs or ownership
conflicts retain protection.

After removing all provider consumers and releasing their durable pins:

```sh
kubectl delete compositeprovidertemplate outscale-account-a-r1 --wait=false
```

Referenced profiles remain Terminating; checks include durable pins across all
namespaces. Existing bound consumers may use a retained deleting profile, but new
bindings are refused. Let the controller release finalizers. Operator uninstall
alone does not clean up workloads.

Source templates are administrator-managed and are not covered by the profile's
deletion finalizer. Retain them while in use; deleting one blocks that attachment's
normal reconciliation but does not delete consumer infrastructure, block unrelated
valid attachments or prevent safely bound teardown. Global composition checks still
apply. A source bootstrap template cannot also be a consumer's generated output.
