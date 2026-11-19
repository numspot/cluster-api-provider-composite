# Operations

Use an explicit management-cluster kubeconfig. Install compatible CAPI core,
kubeadm bootstrap, hosted-control-plane and native infrastructure providers first.
Composite uses v1beta2 KubeadmConfigTemplates and independently resolves native
contracts. Development tests use CAPI 1.12.4 and Kubernetes 1.34.1 envtest; actual
native worker/network/storage compatibility must be verified for your stack.

For Kamaji, include `CompositeCluster` in its
`--dynamic-infrastructure-clusters` setting and grant its controller permission
to publish the endpoint on CompositeCluster.

For a disposable local installation with the real Docker infrastructure provider,
use the [Docker-provider quickstart](development.md#docker-provider-quickstart).
It installs CAPI/CAPD, Composite and a one-control-plane/one-worker test workload;
no cloud credentials are used. Its privileged Docker-socket access and one-time
endpoint publication are development-only setup, not production defaults.

## Install

Choose one installer for each controller installation. Both use the same generated
CRDs/RBAC; neither installs native providers or cloud credentials. Supply an image
that exists in your registry or local kind nodes. Candidate tags in source do not
imply published images.

Build and publish from the source revision you are installing using the
[image workflow](development.md#build-and-publish-an-image). Match the image
platform to the management nodes and record the published digest. A workstation's
`docker login` does not configure Kubernetes image pulls: private images need a
registry pull Secret in the operator's namespace, supplied through your usual
secret-management process.

### CRDs only

The canonical directory contains exactly four CRDs:

| CRD | Scope | Served/storage version |
|---|---|---|
| `compositeclusters.infrastructure.cluster.x-k8s.io` | Namespaced | `v1beta2` |
| `compositeclustertemplates.infrastructure.cluster.x-k8s.io` | Namespaced | `v1beta2` |
| `compositeproviders.infrastructure.cluster.x-k8s.io` | Namespaced | `v1beta2` |
| `compositeprovidertemplates.infrastructure.cluster.x-k8s.io` | Cluster | `v1beta2` |

Use an explicit kubeconfig to validate and install them:

```sh
KUBECONFIG=/path/to/management.yaml
kubectl --kubeconfig "$KUBECONFIG" apply --dry-run=server -f api/config/crd/bases
kubectl --kubeconfig "$KUBECONFIG" apply -f api/config/crd/bases
kubectl --kubeconfig "$KUBECONFIG" wait --for=condition=Established --timeout=60s \
  crd/compositeclusters.infrastructure.cluster.x-k8s.io \
  crd/compositeclustertemplates.infrastructure.cluster.x-k8s.io \
  crd/compositeproviders.infrastructure.cluster.x-k8s.io \
  crd/compositeprovidertemplates.infrastructure.cluster.x-k8s.io
```

This registers or updates the APIs; install the operator separately to reconcile
their resources. Existing CompositeClusters can remain in place during a compatible
CRD update. See [upgrade and uninstall](#upgrade-and-uninstall) for schema-history
and controller/RBAC requirements.

### Helm

See [chart values](../helm/README.md). Set `image.repository` and `image.tag` to the
published image in your values file, or use `image.digest` to pin the exact image.
Charts packaged by the release workflow default to the released image's immutable
digest. To intentionally switch such a chart to a tag, clear `image.digest` as well as
setting the repository/tag; the digest takes precedence. For a private registry,
also set `imagePullSecrets` to an existing Secret in `composite-system`. From the
repository root:

```sh
KUBECONFIG=/path/to/management.yaml
helm upgrade --install composite ./helm --namespace composite-system \
  --kubeconfig "$KUBECONFIG" --create-namespace \
  --values /path/to/values.yaml --wait --timeout 5m
kubectl --kubeconfig "$KUBECONFIG" rollout status \
  deployment/composite -n composite-system --timeout=5m
```

For local images, build with `make docker-build IMAGE=composite:dev` using the
node's architecture, then `kind load docker-image composite:dev --name CLUSTER`.
Set the chart image repository/tag accordingly.

The default Deployment and ServiceAccount names match the release name
(`composite` above). Its ClusterRole/ClusterRoleBinding name is prefixed with the
release namespace. The metrics Service is `composite-metrics`.

### clusterctl

Use the checked-in [provider configuration](../clusterctl.yaml), which adds
`composite` as an `InfrastructureProvider` without replacing clusterctl's built-in
providers. Its GitHub URL is a version-aware repository URL, not a direct download
link. The matching release must contain both `infrastructure-components.yaml` and
`metadata.yaml`; source versions and draft releases are not installable releases.

After the requested version is successfully published, from the repository root:

```sh
KUBECONFIG=/path/to/management.yaml
VERSION=v0.1.0
clusterctl config repositories --config ./clusterctl.yaml
clusterctl generate provider --infrastructure "composite:$VERSION" \
  --config ./clusterctl.yaml --target-namespace composite-system
clusterctl init --infrastructure "composite:$VERSION" \
  --config ./clusterctl.yaml --kubeconfig "$KUBECONFIG"
kubectl --kubeconfig "$KUBECONFIG" get providers.clusterctl.cluster.x-k8s.io -A
kubectl --kubeconfig "$KUBECONFIG" -n composite-system rollout status \
  deployment/composite-controller-manager --timeout=5m
```

On a first management-cluster initialization, clusterctl also installs its default
core, bootstrap and control-plane providers. For a hosted control plane, select
the intended providers explicitly; `--control-plane -` skips the default kubeadm
control-plane provider, not your separately installed hosted control plane. Native
infrastructure providers and their credentials remain separate prerequisites.
`--target-namespace` relocates provider resources and their ServiceAccount binding
subjects; it is not the operator's consumer watch scope. Do not install a second
Composite instance to change namespace or switch installers.

The inventory record is `infrastructure-composite` in the installation namespace,
with provider name `composite` and type `InfrastructureProvider`. This clusterctl
`Provider` is not a namespaced `CompositeProvider` attachment. The shipped manager
has a concrete empty `WATCH_FILTER`: installation needs no cloud account or
watch-filter substitution variable. For scoped startup, customize `WATCH_FILTER`
in a source Kustomize overlay or a [clusterctl component override](https://cluster-api.sigs.k8s.io/clusterctl/configuration#overrides-layer)
before installing; runtime filtering/pause semantics are unchanged.

Before a public release, validate/install from a local repository using an image
that actually exists in your registry or management nodes:

```sh
make release-manifests VERSION="$VERSION" IMAGE="$IMAGE"
clusterctl generate provider --infrastructure "composite:$VERSION" --config out/clusterctl.yaml
clusterctl init --infrastructure "composite:$VERSION" \
  --config out/clusterctl.yaml --kubeconfig "$KUBECONFIG"
```

The generated local config contains an absolute workstation path and is not a
portable release asset. To use an existing clusterctl config, merge the provider
entry from the checked-in file rather than replacing other settings. Composite is
not yet registered in upstream clusterctl's built-in catalog: `--config` is required
for discovery. A separate upstream registration would require review and the
current organization-prefixed naming convention; it is not implied by this custom
configuration. Provider templates still declare native GroupKinds; no cloud
translation layer or worker controller is installed.

## Automated releases

The [release workflow](../.github/workflows/release.yml) stages artifacts in a
reviewed GitHub draft, then publishes only after every stage succeeds. It starts
from an annotated `v*` tag push; recovery dispatches must also use that tag ref,
not a branch. Publication is last so GitHub can freeze the completed release's
assets and tag. Composite has one coupled provider/chart version and no private
workflow dependency. Configure these settings before publication:

- For automated release PRs, install a GitHub App with **Contents: read/write** and
  **Pull requests: read/write**. Store its ID as `RELEASE_PLEASE_APP_ID` and its PEM
  private key as `RELEASE_PLEASE_APP_PRIVATE_KEY` in Actions secrets. The scoped App
  token lets release PR changes trigger normal CI. The bot maintains version and
  changelog PRs only: `skip-github-release` prevents it from publishing before the
  artifact pipeline. It neither creates release tags nor publishes releases.
  Do not reuse a developer's SSH/GPG key. With both secrets absent, PR automation
  reports a notice and does nothing; only one supplied secret fails the job.
  Release PRs explicitly target `main`, independently of the default branch.
- Allow GitHub Actions and the required pinned actions. Permit the repository's
  `GITHUB_TOKEN` to publish its GHCR image and chart packages; existing packages may
  need to grant this repository Actions access. Only protected release jobs receive
  package/content write permissions. The protected draft preflight also needs
  Contents write because GitHub requires push access to list draft releases;
  source verification and scanners remain read-only.
- Require CI, commit-lint, `DCO` and applicable Security checks before merging.
  The `DCO` check validates author-matching sign-off trailers on all PR contribution
  commits, including automation, without modifying commits or repository rulesets.
  Protect `main` and `v*` tags, restricting tag creation, mutation and deletion to
  reviewed release automation. Configure the `release` environment used by all publication
  jobs with required reviewers and appropriate tag rules before enabling it.
  Ancestry checks alone are not a trust boundary: workflows and local actions come
  from the tagged commit, and someone able to replace that workflow could replace
  its guard. These repository/environment protections must be configured separately.
- Enable GitHub immutable releases. Drafts remain mutable while assets are staged;
  only the final job publishes and requires explicit `immutable: true` evidence.
  Missing or false evidence is a failure, not proof of protection. Do not manually
  publish, rename, recreate or edit the draft/assets while the pipeline is running.
- Optionally enable Code Scanning and set the Actions repository variable
  `ENABLE_CODE_SCANNING` to `true`. Without it, SARIF reports remain downloadable
  workflow artifacts, including for PRs/forks. Dependabot configuration covers both
  Go modules and Actions; enable dependency/security updates in repository settings.

The [release manifest](../.release-please-manifest.json) records `0.1.0` for the
initial release. A source version is not proof that its assets were published;
check the GitHub Release and every publication job before installing. The
[configuration](../release-please-config.json) uses normal SemVer release versioning
without forcing an alpha suffix or GitHub prerelease status. Release PRs update
the manifest/changelog, Make's default version and both Helm version fields
together. Pre-1.0 versions remain experimental; a stable tag is not a claim of
production support or API stability.

The App is optional for a maintainer-authored first release. After explicit review
and successful checks, merge the version/changelog preparation and record its exact
merged SHA. Check the declared provider major/minor series and its CAPI contract in
[metadata.yaml](../metadata.yaml); a new series requires an explicit contract review.
Then prepare the release in this order:

1. Save a draft with its **title and tag both exactly the version**, reviewed
   nonempty notes, the merged preparation SHA as target, and the correct prerelease
   flag. Both name and tag must be unique; draft discovery lists all pages and binds
   the numeric release ID.
2. Confirm that draft exists before creating/pushing an annotated tag on that same
   SHA. Never tag an older alpha-candidate ancestor as a stable release. The tag
   must equal the source manifest version; CI checks the chart/Make coupling too.
3. Let the pipeline verify, stage and publish. Do not press GitHub's Publish button
   first: immutability would prevent the subsequent asset uploads.

For a reviewed stable release, the preparation commands are:

```sh
VERSION=v0.1.0
RELEASE_SHA=FULL_REVIEWED_MERGED_SHA
# Use maintainer credentials; save the draft before the tag push starts CI.
gh release create "$VERSION" --draft --prerelease=false --target "$RELEASE_SHA" \
  --title "$VERSION" --notes-file /path/to/reviewed-notes.md
git tag -a "$VERSION" "$RELEASE_SHA" -m "$VERSION"
git push origin "refs/tags/$VERSION"
```

Maintainer or appropriately scoped App credentials must create the tag; a tag
created with the default `GITHUB_TOKEN` does not trigger another workflow. The
optional PR bot deliberately does not create releases/tags, avoiding draft/tag
creation races and premature publication. Normal releases use stable SemVer;
explicit prerelease tags remain supported when their source version and draft
flag agree. A `release.published` event never restarts staging.

The read-only validation job checks the annotated tag and main ancestry before
repository tools, requires the tag/source version to match, and preflights packages
and all binary architectures without publishing. Its placeholder digest tests
packaging only, not registry availability. Reusable CI and Security run on the
exact tagged commit, including both-module lint/race/envtest and vulnerability
checks. CI builds and smoke-tests release-version Linux amd64/arm64 images once;
Security scans those same archives. Only after those gates and the protected
live-draft identity check pass can publication jobs stage these outputs:

- `ghcr.io/<owner>/cluster-api-provider-composite:<tag>` for Linux amd64/arm64,
  with and without the leading `v`. Prereleases do not overwrite `latest` or stable
  major/minor aliases.
- Linux/macOS amd64/arm64 binary archives and checksums through GoReleaser.
- One matching chart archive as both a GitHub release asset and
  `oci://ghcr.io/<owner>/charts/cluster-api-provider-composite` (chart versions omit `v`).
  Both publishers download the same packaged archive; neither repackages it.
- `infrastructure-components.yaml` and the chart's default image pinned to the
  published multiarch index digest, plus matching `metadata.yaml` for clusterctl.
  Final packaging stages a scratch chart with the actual image repository/digest;
  it never rewrites source values. The generated local `out/clusterctl.yaml`
  contains an absolute path and is never published.

Before registry login, the image publisher checks both archives' commit, version,
image ID, platform and OCI source/revision/version labels. It pushes those unchanged
images and assembles the multiarch index without rebuilding. Image publication must
succeed before final installation packaging and both chart/release-asset publishers.
These are separate remote operations, not an atomic transaction; the reviewed
GitHub Release remains a draft during staging. GoReleaser preserves that draft's
identity/notes and can replace known partial uploads only after a fresh draft
check. The final protected job requires exactly eight completed assets, preserves
their identity/size/available digest evidence and notes across publication, checks
the tag's reviewed commit again, and requires explicit immutable-release evidence.

After partial staging failure, inspect the bound draft and registry digests before
retrying. After an ambiguous final publication request, re-read that same release
ID: an already-published immutable release permits verification only, never another
GoReleaser upload. Do not delete/recreate the release, move/reuse its tag, or undo
protection. GitHub immutability locks assets and the tag, not all editable release
metadata. Do not manually change the draft or publish it while jobs are staging.
No workflow deploys or upgrades a Kubernetes cluster. Existing vulnerability
findings still require remediation; workflow reuse does not waive Security checks.

After a successful release, use its actual tag and package visibility. For example:

```sh
VERSION=v0.1.0  # Use an actual successfully published release.
helm upgrade --install composite \
  oci://ghcr.io/numspot/charts/cluster-api-provider-composite \
  --version "${VERSION#v}" --namespace composite-system \
  --kubeconfig "$KUBECONFIG" --create-namespace --values /path/to/values.yaml
```

Apply matching CRDs before upgrades as described under
[upgrade and uninstall](#upgrade-and-uninstall). For CAPI-managed installation use
the [clusterctl registration and commands](#clusterctl). Its repository URL is not
proof of an existing release. For alpha releases, select an actual published
prerelease explicitly with `--infrastructure composite:vX.Y.Z-alpha.N`.

## Provider attachments

Install all four generated CRDs and operator/RBAC from the same source revision.
Use an empty CompositeCluster to start a hosted control plane before provider
attachments. Install reusable CompositeProviderTemplates and native sources, then
create namespaced CompositeProviders and native MachineDeployments separately.
See [cluster attachments](usage.md#create-a-cluster-attachment) and
[native workers](usage.md#templates-and-workers) for manifests.

Observe provider Ready and its generation separately from Cluster InfrastructureReady.
Address template, credential-path and native failures on the provider/native
resources; CompositeCluster has no provider-selection field. Publishing a profile
does not attach it. Composite needs read-only worker/bootstrap provenance permissions
for retirement, not Secret read permissions.

Keep provider credentials in each consumer's namespace throughout teardown.
Use profile names as immutable revisions. A profile's `ConfigurationValid`
condition and schema/render checks do not establish working regional cloud
connectivity; verify the tenant API, each pool's join, and CNI/CSI behavior against
the intended native stack.

Profiles may select native Secret names with `credentialsRef.name` and explicit
`credentialsRef.namePaths`. These names resolve in the consuming namespace; shared
source templates must use namespace-local references. Composite requires no Secret
read permissions and does not test whether the Secret exists, contains the required
keys, or authenticates to the declared account/region. Check those through the
native provider. Rotate Secret contents using its supported process; selecting a
different Secret name or mapping requires a new immutable profile revision.

## Scope, diagnostics and metrics

### Verify the operator

After the rollout completes, inspect the selected pods and their image IDs:

```sh
kubectl --kubeconfig "$KUBECONFIG" get pods -n composite-system \
  -l app.kubernetes.io/instance=composite -o wide
kubectl --kubeconfig "$KUBECONFIG" logs deployment/composite \
  -n composite-system --tail=100
kubectl --kubeconfig "$KUBECONFIG" get cprov
kubectl --kubeconfig "$KUBECONFIG" get cocl,cct -A
```

`rollout status` checks the desired Deployment revision; an Available condition
can still describe a previous healthy replica during an incomplete rollout.
Operator probes are `/ping` and `/ready` on port 2112. They establish process/cache
health, not native infrastructure or worker readiness.

| Rollout symptom | Check |
|---|---|
| `ImagePullBackOff` / `ErrImagePull` | Image tag/digest, node architecture, registry reachability and same-namespace pull Secret |
| `FailedToRetrieveImagePullSecret` | The referenced Secret exists in the pod namespace and grants pull access to the image registry |
| `FailedCreatePodSandBox` / missing pod IP | Management-cluster CNI and node networking |
| Cache/list/watch forbidden | ServiceAccount binding and generated RBAC, including the global profile catalog |
| Probe failure | Listen addresses, probe paths/ports, configuration validation and operator logs |
| Operator healthy, CompositeCluster unready | The resource's conditions and referenced native children; see [usage](usage.md#observe-and-troubleshoot) |

### Informer startup

The operator always initializes informer caches with LIST followed by WATCH.
Streaming initial-event WatchList is disabled in the binary before Kubernetes
clients or caches are constructed, avoiding startup stalls waiting for the
initial-events-end bookmark. Normal watches, reconciliation, scope and provider
readiness semantics remain unchanged.

This policy applies to Helm, clusterctl and direct binary deployments. There is no
Helm toggle or environment opt-in: `KUBE_FEATURE_WatchListClient=true` cannot
re-enable it. Other client-go feature gates retain their defaults and environment
configuration. The policy requires an image built from this source; changing
installation manifests alone does not change an older binary's behavior.

A repeated `Could not wait for Cache to sync` followed by process exit indicates
an informer startup failure, not a requirement to add native providers to an empty
CompositeCluster. Verify `Starting workers` in the operator logs and current
CompositeCluster conditions; process probes and Deployment availability alone do
not establish successful cache synchronization. Increasing `--sync-period` changes
resync frequency, not the startup sync deadline.

### Configuration

| Setting | Default / behavior |
|---|---|
| `--namespace` / `WATCH_NAMESPACE` | Empty watches all namespaces; catalog protection always reads globally |
| `--watch-filter` / `WATCH_FILTER` | Empty selects all; otherwise matches CAPI's watch-filter label |
| `--leader-elect` / `LEADER_ELECTION` | Enabled; replicas coordinate with a Lease |
| `--leader-election-id` | `capi-composite.infrastructure.cluster.x-k8s.io` |
| `--leader-election-namespace` | Pod namespace by default; separate from watch namespace |
| Health | Port 2112, `/ping` and `/ready`; process health, not workload readiness |
| Metrics | Port 5555, TLS + authentication/authorization; HTTP/2 disabled |
| `--metrics-cert-dir` | Directory with `tls.crt`/`tls.key`; configure scraper trust |
| `--tls-min-version` | TLS 1.2 by default; 1.3 supported |

Explicit flags override environment values, including empty/false settings.
Install, watch and lease namespaces are distinct. A watch filter is not an RBAC
security boundary. Externally managed and paused resources are respected. Native
InfraClusters are polled every 30 seconds while settling and five minutes when
Ready; profile, provider, bootstrap and native MachineDeployment events also trigger
reconciliation. `CompositeCluster.status.providers` mirrors attachment observations
and lists their native node pools. Inspect the referenced MachineDeployments for
worker availability; it does not gate CompositeCluster Ready. Failed inventory
reads retain the last complete provider/pool references and mark provider
observations Unknown rather than publishing an empty or partial inventory. The
same rule applies when a provider exceeds the 100-reference status capacity;
inspect native MachineDeployments directly. Inventory also refreshes during
provider/Cluster teardown while native finalizers hold deletion.

Bind the generated metrics-reader ClusterRole to intended scrapers. Expected
responses are 401 without credentials, 403 without authorization and 200 with
both. The current delegated authenticator can return 500 for a malformed token
while still denying access. `--insecure-diagnostics=true` enables unauthenticated
HTTP for isolated development only.

Operator status/logs/Events use bounded categories rather than copying arbitrary
native errors, bootstrap data or warning headers. Verbosity is restricted to 0–4.
Inspect native resources and provider logs with appropriate permissions for the
underlying details. See [security](../SECURITY.md) for the trust boundary.

## Upgrade and uninstall

Before a schema/controller upgrade, preserve the existing CRDs, Composite resources,
binding annotations and native owner references in your normal backup workflow.
The same `v1beta2` API version can acquire new fields or stricter validation; compare
the actual schemas and migration constraints rather than relying only on the version.

For Helm, use all four CRDs from the same source as the chart before
upgrade. Helm's `crds/` directory installs missing CRDs but does not upgrade them:

```sh
kubectl --kubeconfig "$KUBECONFIG" apply --dry-run=server -f api/config/crd/bases
kubectl --kubeconfig "$KUBECONFIG" apply -f api/config/crd/bases
helm upgrade composite ./helm --namespace composite-system \
  --kubeconfig "$KUBECONFIG" --values /path/to/values.yaml --wait --timeout 5m
```

Upgrade controller permissions with the image. In particular, older two-CRD
installations may lack the provider/template APIs and their status/finalizer permissions,
global reference reads, `events.k8s.io` event writes, or the
TokenReview/SubjectAccessReview permissions needed for authenticated metrics.
The shared [generated role](../config/rbac/role.yaml)
is the installation baseline; custom native groups need explicit additional rules.

For manually applied or GitOps-managed installations, update the existing owner's
manifests. Preserve Deployment selectors and bind the generated rules to the
existing ServiceAccount. Labels and names from an older chart may differ from the
current standalone chart, so an image update and a change of installer are separate
operations. Verify the new replica's rollout before treating the upgrade as complete.

Compared with earlier development snapshots, this release omits
`CompositeCluster.spec.providers` and historical cluster provider bindings. This is
a breaking change for legacy development installations: finish teardown of their
compositions using the old controller before upgrading. There is no automatic
migration or adoption of their existing children. Create new attachments as
CompositeProvider resources after installing matching CRDs and controller. Do not
rely on schema pruning to migrate a running composition.

For named regional attachments, keep the canonical InfraCluster of each kind and
apply regional native worker templates explicitly. Bootstrap names remain
`<composite>-<attachment>`; infrastructure may have a different persisted name.
Inspect CompositeProvider `status.infrastructureRef` rather than assuming all native children
share the CompositeCluster name. Do not downgrade schemas over attachments using
credential references: pruning a reference can silently lose credential selection.

For clusterctl, use `clusterctl upgrade apply` with the actual target version and
custom configuration. Back up CRDs, resources, pins and owner references first;
compare native child/template identities and readiness afterwards.

`helm uninstall composite -n composite-system` and default
`clusterctl delete --infrastructure composite` retain CRDs and customer resources.
An absent operator cannot process finalizers. Complete normal Cluster lifecycle
first, or reinstall a compatible operator. Deleting CRDs is not workload cleanup.

## Move and restore

Automatic cross-management-cluster moves are unsupported for template-backed profiles
in this pre-1.0 provider. Attachment bindings retain provider, cluster and profile UIDs and
source-template UID/generation. Recreated objects have different UIDs, even with
identical manifests, and are not silently accepted.

There is no automatic rebind procedure. A future migration must verify native ownership,
source configuration and rewritten identities explicitly. Do not clear binding metadata
to bypass the guard. Helm does not register clusterctl inventory either; inventory
enrollment alone does not solve source identity or CRD discovery and does not transfer
Helm ownership to clusterctl.

## Recovery

### Attachment pin mismatches

`ProfileMismatch` distinguishes several guards. Read the CompositeProvider condition
message, not CompositeCluster `Ready` or `ProvidersReady`, to identify the blocker:
attachment readiness is independent of cluster infrastructure readiness.

| Diagnostic evidence | Meaning and safe next step |
|---|---|
| Provider-local pin missing/invalid/conflicting | The provider's `compositeprovider.infrastructure.cluster.x-k8s.io/binding` annotation does not match its entry in the CompositeCluster's `compositecluster.infrastructure.cluster.x-k8s.io/provider-attachments` annotation. Preserve both and compare verified backups; neither is editable configuration. |
| CompositeProvider UID mismatch | The cluster-level record belongs to a different provider incarnation. A recreated provider with the same name/spec cannot inherit that identity. Inspect the recorded and live UIDs before planning replacement. |
| Name reserved by a retired record | Teardown completed, but the cluster-level record still reserves that attachment name. A fresh provider may have no local pin at all. Retired history is not garbage-collected; use a new attachment name only after checking native child-name uniqueness, canonical-child requirements and capacity. |
| Profile UID or infrastructure/bootstrap source revision mismatch | The message identifies the offending profile/source and pinned versus live UID (plus generation for a source). An identical manifest under a new UID is still a replacement. Preserve existing infrastructure and investigate the source lifecycle; this is not a provider-name reservation. |

The cluster annotation is a versioned map of current and retired records; the
provider-local annotation is that provider's matching record. Local pin comparison
is semantic: ordinary JSON formatting, key order, escaping and a trailing newline
are accepted within the size limit. Unknown/duplicate keys, malformed/null/trailing
JSON and actual identity changes are rejected. Diagnostic identity components are
bounded and allowlisted; unsafe or overlong components are shown as `<omitted>`,
never replaced with native error text or template/credential payloads.

There is **no supported annotation-editing or rebind procedure**, for either an
active source revision or a retired/recreated provider. Do not remove a retired
record, clear a pin/finalizer or copy current status into identity metadata.
Version source templates and profiles under new immutable names for new attachments;
this does not migrate already-bound infrastructure. Recreating a source from a
backup manifest does not recover its original UID. Retired identities continue to
count toward the eight-attachment and combined 16 KiB record limits, even after
children and live status entries disappear.

These separate failure modes are illustrated in [issue #10](https://github.com/numspot/cluster-api-provider-composite/issues/10),
the [trailing-newline report](https://github.com/numspot/cluster-api-provider-composite/issues/10#issuecomment-6989501152)
and the [retired-provider report](https://github.com/numspot/cluster-api-provider-composite/issues/10#issuecomment-6993037251).
Their manual annotation edits are not a supported recovery interface.

### Preserve recovery evidence

Keep backups of profiles, source templates, binding annotations and native owner references.
Status can be rebuilt, but lost pins must not be guessed from today's profile or
from resource names. Restore only verified metadata/configuration with matching
identity. If original evidence is unavailable, investigate the native/cloud state
before proceeding; do not strip finalizers to force progress.

An invalid binding record holds reconciliation/deletion and may hold catalog
deletion globally. The writer rejects records above 16 KiB or with invalid names
before persistence; already-corrupt records still need verified recovery. Choose
short enough Composite/attachment names for `<composite>-<attachment>` to be a
valid Kubernetes resource name. Never truncate an established child's identity.

Profiles require source-template references instead of embedded defaults.
Preserve attachment bindings and `infraClusterName` declarations; downgrading CRDs
may prune fields, and reinstalling a schema cannot restore lost data. Historical
cluster provider bindings are unsupported; use the old controller for their teardown.
Automatic conversion and native renaming remain unsupported.

Retain source templates as immutable named revisions and keep their referenced API
versions served through native-provider upgrades. Editing or recreating a source blocks
bound consumers; metadata-only updates are allowed. Source-template deletion is not
protected by the profile finalizer, and Composite never deletes shared sources.
