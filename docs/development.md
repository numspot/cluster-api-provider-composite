# Development

## Prerequisites

Install Go **1.26.11**, golangci-lint **2.12.2** and Helm **4.2.0**. The module
toolchain directives select the Go patch version automatically when supported.
[hack/tools.env](../hack/tools.env) is the source of truth for local verification
and generation tools, including controller-gen **v0.22.0**. Both Go modules require
Go 1.26.0 or newer and select the pinned local toolchain. Container builds use the
separate digest-pinned Go **1.27.4-alpine** image in
[build/operator.Dockerfile](../build/operator.Dockerfile).
Git, Bash and jq are needed for the offline release-policy tests; public network
access is needed for tools/modules and vulnerability data. Docker is required for
image builds; kind/kubectl/clusterctl are optional for local installation work.

```sh
make envtest-download
make verify
make verify-hygiene
```

`make verify` checks tools, Go headers, API dependency isolation, generated output,
contract labels, both-module lint, API lint, race-enabled tests, packaging renders
and vulnerabilities. `verify-hygiene` checks workflow syntax, rejects tracked private
credential/config files, tests the hygiene policy and scans deliverable source for
secrets, private endpoints and personal workstation paths. Classification and
production details still need contextual review; see
[source publication](../SECURITY.md#source-publication). GitHub Actions splits these
checks into CI and Security workflows rather than hiding test results behind a
vulnerability scan.

## GitHub automation

CI verifies the operator, the separate API module and native provider packaging.
Release publication uses reusable verification followed by promotion of the same
tested artifacts. There is no UI bundle, deployment step or private workflow/chart
dependency.

| Workflow | Purpose |
|---|---|
| [CI](../.github/workflows/verify.yml) | Independent Go, Helm/Kustomize and Docker jobs; source/API/generated checks, vet, build, race/envtest tests, coverage and restricted image smoke tests; reusable for releases |
| [Commit Lint](../.github/workflows/commit-lint.yml) | Conventional Commits and PR titles, including the eventual squash subject |
| [DCO](../.github/workflows/dco.yml) | Author-matching sign-off trailers on every contribution commit in PRs targeting any branch |
| [Security](../.github/workflows/security.yml) | Both-module govulncheck/gosec, source hygiene and Trivy filesystem/IaC/image scans; weekly and manually dispatchable as well as PR/main runs |
| [release-please](../.github/workflows/release-please.yml) | Maintains stable release PRs from Conventional Commits on main once GitHub App credentials are configured |
| [Release](../.github/workflows/release.yml) | On annotated version tags, validates source/commit/series, reuses CI/Security, stages tested artifacts in a bound draft and publishes it immutable only after every upload succeeds |

Actions are pinned to immutable SHAs. The shared [tool setup](../.github/actions/setup-tools/action.yml)
reads [tool pins](../hack/tools.env); Go caching covers both module checksums.
CI checks generated output in scratch space and never repairs the working source.
`make test COVERPROFILE=coverage.out` writes separate root/API coverage profiles,
which CI uploads even after a test failure. Docker jobs build and smoke-test the
operator image.

The `DCO` check runs on PR creation, updates, reopen and edits for every target
branch, using read-only permissions and complete Git history. Shallow repositories
are rejected rather than silently omitting earlier commits. It checks the event's
base-to-head commit range, not GitHub's synthetic merge commit. Every contribution
commit, including real merge commits and bot-authored commits, needs a footer
`Signed-off-by` matching the commit author's name and email. Earlier unsigned
history already reachable from the base is not retroactively checked. Failures
report bounded commit IDs without publishing commit messages or author data.
Run the same checker locally with:

```sh
go run ./hack/check-dco -base "$(git rev-parse origin/main)" -head "$(git rev-parse HEAD)"
```

Use `git commit --signoff` for new commits. See
[contributing](../CONTRIBUTING.md#pull-requests) for correcting existing commits.
The checker does not alter commits, grant a bot exemption, or verify GPG/SSH
signatures. Keep automated commits signed off too. The real-Git regressions in
[check-dco](../hack/check-dco/) cover missing/mismatched trailers, earlier unsigned
commits, merge histories, large ranges, invalid refs and safe diagnostics.

The release caller supplies an exact `release-version`. Reusable CI builds and
smoke-tests amd64/arm64 images with that version, then retains image archives,
commit/version records and image IDs. Reusable Security validates and scans those
same images. Publication validates their identities again and assembles the
multiarch index without a rebuild. One final digest-pinned chart archive feeds
both GitHub and OCI; `make helm-package VERSION=... IMAGE=...` stages image defaults
in scratch, without modifying source values or CRDs. Offline tests in
[hack](../hack/) cover event/ref gates, artifact identity and packaging behavior.

A protected metadata gate binds the unique reviewed draft's numeric ID before any
registry writes. GoReleaser stages only into that same live draft and preserves
its notes. The final protected job verifies all eight uploads and the tag again,
publishes, and requires immutable-release evidence. Offline fake-GitHub tests cover
pagination, name/tag collisions, recreated drafts, incomplete assets and safe
verification-only retries. The optional release-please action maintains PRs only;
maintainers prepare the draft and annotated tag in the documented order.

Security scan findings are not suppressed: govulncheck and fixable high/critical
image findings fail their jobs. Gosec and filesystem/IaC findings are advisory
reports, matching the upstream workflow. SARIF is always retained as workflow
artifacts when available. The [SARIF normalizer](../hack/normalize-sarif/) rebases
API-module source locations to repository-relative `api/...` paths, preserving
report fields and exact JSON numbers so Code Scanning links/fingerprints work. Code Scanning upload is opt-in through
`ENABLE_CODE_SCANNING=true`, requires repository support, and runs only for standalone
trusted non-PR events. Reusable release scans retain per-architecture reports without
Code Scanning uploads. No release App credentials are exposed to pull requests. Secret
scanning uses the existing pinned gitleaks CLI and needs no action license.

The [clusterctl provider config](../clusterctl.yaml) registers the custom
infrastructure provider. Release manifests and provider metadata are validated
with the real clusterctl processing path, including namespace/RBAC rewriting and
an unset watch-filter default. Local repositories are generated in scratch for
tests; their absolute paths must not become public registration URLs. The envtest
Init regression uses the real installer/inventory client and checks repeat-init
idempotency. Only cert-manager initialization is stubbed because envtest runs no
pods; deployment availability and native-cloud provisioning are not established
by that test. Test-only clusterctl dependencies stay in the root module, never the
consumer API module. Focused checks use `make test TEST=TestClusterctl`.

See [release setup](operations.md#automated-releases) before enabling publishing.
A workflow definition does not prove a GitHub run or a publication has succeeded.
Update required branch checks when replacing the former single verification job.

## Working loop

```sh
make test TEST=TestProfileAttachments   # root selection; API tests also run
make test TEST=TestOperatorRuntime     # actual operator against isolated envtest
make lint
make fmt                              # explicit source formatting
make gen                              # after changing generated inputs
make verify-generated                 # compare against scratch generation
make verify-vulnerabilities
```

Use failing regression tests before behavioral fixes. Prefer table-driven Go
tests and existing helpers. Test ownership/retry/error paths as well as success.
Envtest covers API admission and reconciliation, and runtime tests execute the
operator under its shipped RBAC. Runtime checks wait for current-generation Ready
observations on the provider and CompositeCluster independently; one resource's
readiness or latched initialization does not establish the other's current health.
These tests do not inherit workstation kubeconfigs or provision cloud VMs. Real
native worker/network/storage integration is separate.

Direct root `go test ./...` needs `KUBEBUILDER_ASSETS`; `make test` resolves installed
assets. `SKIP_ENVTEST=1` is a local-only shortcut, rejected in CI. Run API-module
checks separately when using Go commands directly.

## Layout and conventions

| Path | Responsibility |
|---|---|
| `api/` | Standalone CRD types and API machinery |
| `internal/controller/` | Reconciliation, profile planning, bindings, status and deletion |
| `internal/capi/` | Native contracts, ownership-safe writes and status reading |
| `internal/mint/` | Pure native/bootstrap rendering and merge rules |
| `internal/profile/` | Exact JSON handling and allowed/protected overrides over resolved template payloads |
| `configuration/`, `cmd/operator/` | Flags/environment and manager wiring |
| `config/`, `helm/` | clusterctl/Kustomize and Helm installation inputs |
| `tests/envtest/` | API-server, lifecycle and actual-operator regressions |

Keep `api/` free of CAPI, controller-runtime and generator/linter dependencies.
Use straightforward Go, errors with preserved causes, and safe diagnostic categories.
Current [design decisions](design.md) describe the invariants that tests must retain.

First-party Go files use the Apache-2.0 header with holder **Numspot**, from
[hack/boilerplate.go.txt](../hack/boilerplate.go.txt). Preserve third-party notices.
`make headers-fix` is an explicit helper; generated files get headers through their
generator. YAML/Markdown/shell files are covered by the repository LICENSE.

Generation produces API deepcopy code, all four `v1beta2` CRDs, contract labels,
RBAC and Helm fragments from shared inputs:

1. Edit types/markers in `api/v1beta2/`, controller RBAC markers, or the generator
   pin in `hack/tools.env`.
2. Run `make gen`. Canonical schemas go to `api/config/crd/bases/`; generation
   copies them into `helm/crds/` and refreshes the shared RBAC output.
3. Run `make verify-generated` and update the [API reference](usage.md#api-reference),
   relevant guides, and examples for any field, validation or lifecycle change.

The generated resources are `CompositeCluster`, `CompositeClusterTemplate` and
`CompositeProvider` (namespaced), plus `CompositeProviderTemplate` (cluster-scoped).
Only the cluster and its template advertise
the v1beta2 InfraCluster contract. API module dependency versions are distinct from
the CRDs' served API version and from the workload Kubernetes version.

Include all generated output in changes. Verification regenerates in scratch
space and does not repair working files; do not run generation first in CI to hide
stale output. Historical cluster provider-bindings formats are not a supported
migration path; use fresh resource-based attachments.

Named infrastructure regressions cover same-kind regions, canonical lookup,
immutable names, source UID/generation drift, status recovery, ownership rejection
and source-independent teardown. Current attachment/source pins and name rules
remain covered without silently rebinding recreated providers or templates.
`TestAttachmentPin` unit tests exercise equivalent JSON encodings versus malformed,
unknown/duplicate/trailing data and changes to every pinned identity field. Pin
diagnostic regressions preserve error identity and reject unsafe/oversized diagnostic
components without losing both source revisions. The provider attachment envtests
cover formatting-only convergence, conflicting pins, recreated source evidence and
retired-name reservation without modifying durable identity or existing children.
No recovery/rebind mechanism is implied by these tests.

`make test TEST=TestProviderAttachment` covers namespaced attachments, independent
empty-cluster readiness, credential injection/protection, source drift, reference
immutability, identity collisions, shared worker consumers and ordered teardown.
`make test TEST=TestProviderStatus` covers the CompositeCluster provider summary,
exact per-provider native MachineDeployment membership, independent cluster
readiness, stable ordering, pool removal and draining-provider visibility. Unit
regressions also cover paginated direct reads, partial-read failures, stale
conditions, status capacity and replacement-UID isolation; API tests cover node-pool
references and deepcopy independence. Teardown regressions keep inventory current
while native finalizers hold deletion. Workers remain read-only observations.

`TestOperatorRuntime/provider-attachments` runs the actual operator under generated RBAC
and namespace/filter scope, including native pool create/delete watch events.
Composite observes workers without managing them.

`make test TEST=TestCredentialsRef` covers native credential-name injection,
protected overrides, endpoint/path conflicts, source isolation, credential-field
admission and immutable updates/patches. Secret contents are never inputs to
Composite. API-module tests also cover credential-reference deepcopy and JSON
round trips. Credential selection preserves template/source UID pins.

`make test TEST=TestEmptyComposition` exercises empty infrastructure readiness
without an endpoint or native children, stable status, ownership/pause gates,
empty template admission, initialization latching and teardown. Provider coverage
uses absent cluster provider configuration and independent CompositeProvider
creation, failure/recovery and retirement. Historical cluster provider-bindings records and old configuration are not
supported attachment paths.

## Docker-provider quickstart

Use the real Cluster API Docker provider (CAPD) to test Composite without cloud
credentials or publishing an image. This creates a disposable kind management
cluster named `composite-quickstart` and a workload named `composite-docker`, with
one kubeadm control-plane node and one native Docker worker. It is a development
fixture, not a production installation or a multi-provider compatibility test.

### Prerequisites

- A running [Docker Engine or Docker Desktop](https://docs.docker.com/get-started/get-docker/)
  in Linux-container mode, with Buildx and an amd64 or arm64 daemon. Use the local
  standard `/var/run/docker.sock`; rootless/custom-socket and remote daemons are
  outside this quickstart. Allow at least 6 GB of Docker memory; 8 GB is recommended.
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) **v0.31.0 or newer**,
  [kubectl](https://kubernetes.io/docs/tasks/tools/) **1.34.x**,
  [clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start#install-clusterctl)
  **v1.12.4**, Helm **4.2.0**, Make and Bash on `PATH`.
- Public network access for the pinned builder/runtime images, provider releases,
  cert-manager, node images and Calico. No local Go installation is needed.

**Security:** CAPD runs privileged with the host Docker socket mounted through the
[kind configuration](../local/quickstart-kind.yaml). That is effectively root access
to the Docker host. Only run this in a trusted local development environment;
Composite itself keeps the shipped non-root security settings and generated RBAC.

### Run and inspect

From the repository root:

```sh
make quickstart
make quickstart-status
kubectl --kubeconfig out/quickstart/workload.kubeconfig get nodes
```

The [runner](../hack/quickstart.sh) builds the existing operator Dockerfile for the
Docker daemon's architecture, checks its binary version and loads it into kind.
It installs CAPI core, kubeadm bootstrap/control-plane and CAPD **v1.12.4**, then
Composite through the local Helm chart. `CLUSTER_TOPOLOGY=true` is set during
provider installation because CAPD requires it to admit DockerClusterTemplates,
even though these manifests do not use ClusterClass.

The [native source templates and profile](../local/quickstart-templates.yaml) are
created first. The [workload manifest](../local/quickstart.yaml) references Composite
as infrastructure, attaches that Docker profile and uses the generated
`composite-docker-infra-docker` KubeadmConfigTemplate for its MachineDeployment.
The node image is Kubernetes **v1.34.0**, pinned to its
[published multiarch digest](https://hub.docker.com/v2/repositories/kindest/node/tags/v1.34.0).
The runner installs [Calico **v3.31.0**](https://docs.tigera.io/calico/3.31/getting-started/kubernetes/requirements)
on the workload and waits for the attachment
Ready condition, control-plane initialization, native worker availability and Ready
workload nodes. CompositeCluster Ready alone does not prove any of those outcomes.

**Endpoint handoff:** CAPD discovers its load-balancer address on DockerCluster.
Composite deliberately does not read that address upward, and KubeadmControlPlane
cannot initialize without a Cluster endpoint. The runner waits for CAPD's actual
IP/port and publishes that exact endpoint on CompositeCluster once. It does not
modify sources, native child ownership, durable pins or status, and it does not add
Docker-specific behavior to the operator. This is explicit test-harness wiring,
not automatic endpoint discovery. See the [upstream CAPD quickstart](https://github.com/kubernetes-sigs/cluster-api/blob/v1.12.4/docs/book/src/user/quick-start.md).

Management access is isolated in ignored `out/quickstart/kubeconfig`; workload
access is in ignored `out/quickstart/workload.kubeconfig`. Your default kubeconfig
and current context are not changed. Startup refuses to reuse either reserved
cluster name. Use one Docker attachment only: CAPD keys the load-balancer container
by the actual CAPI Cluster name, not by attachment/region or namespace.

For a failed startup, keep the controllers running and inspect:

```sh
kubectl --kubeconfig out/quickstart/kubeconfig -n composite-system logs deployment/composite --tail=100
kubectl --kubeconfig out/quickstart/kubeconfig -n composite-quickstart get cluster,cocl,cprov,dockercluster,machine
kubectl --kubeconfig out/quickstart/kubeconfig -n composite-quickstart describe compositeprovider docker
kubectl --kubeconfig out/quickstart/kubeconfig -n capd-system logs deployment/capd-controller-manager --tail=100
```

### Cleanup and rebuild

```sh
make quickstart-down
make quickstart   # Rebuild from the current checkout on a fresh management cluster.
```

Cleanup deletes the owning CAPI Cluster and waits for normal native/Composite
teardown **before** deleting kind. CAPD workload nodes and its load balancer are
sibling Docker containers; deleting only the management kind cluster would strand
them. If workload deletion fails, cleanup stops and retains the management cluster
for diagnosis/retry. Do not strip finalizers or delete CRDs/controllers first.
Local images, caches and ignored output files remain. Do not apply the whole
`local/` directory: other examples require clouds, and the Tilt manifests are not
the installation baseline. See [operations](operations.md#upgrade-and-uninstall)
for lifecycle constraints.

## Build and publish an image

Build for the management nodes' architecture, not necessarily the workstation's.
For a single-platform image, from the repository root:

```sh
REGISTRY=registry.example.com
IMAGE_REPOSITORY="$REGISTRY/team/cluster-api-provider-composite"
VERSION="v0.1.0-dev.git$(git rev-parse --short=12 HEAD)"
IMAGE="$IMAGE_REPOSITORY:$VERSION"

docker login "$REGISTRY"
make docker-build PLATFORM=linux/amd64 VERSION="$VERSION" IMAGE="$IMAGE"
docker run --rm --platform linux/amd64 --network=none "$IMAGE" --version
docker push "$IMAGE"
docker buildx imagetools inspect "$IMAGE"
```

`make docker-build` embeds `VERSION` in the binary. Record the registry-reported
digest for deployment through Helm's `image.digest` value. Registry login
authenticates the workstation; Kubernetes needs its own read-only pull credentials
in the operator namespace when the image is private.

To publish an image for both supported architectures and record the source revision:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --file build/operator.Dockerfile \
  --build-arg "version=$VERSION" --build-arg "revision=$(git rev-parse HEAD)" \
  --tag "$IMAGE" --push .
```

Use a builder configured for the selected platforms. `make docker-build-multiarch`
instead writes `out/composite-image.tar`; it is a local archive target, not the
registry publication step. Follow [operations](operations.md#install) to install
the image with CRDs and RBAC from the same source revision.

## Local artifacts

```sh
make build
make docker-build PLATFORM=linux/arm64 IMAGE=composite:dev
make docker-build-multiarch IMAGE=composite:dev
make release-manifests VERSION="$VERSION" IMAGE="$IMAGE"
make helm-package VERSION=v0.1.0
```

Images use pinned public builder/runtime bases and run non-root. These commands
produce local artifacts and do not push or tag Git. `out/`, `.tools/`, caches and
private local material are excluded from source delivery. See
[operations](operations.md) for using artifacts on a management cluster.

Keep documentation focused on current behavior, usable commands and important
constraints. Explain changes in their PR and update the relevant guide; historical
plans and review reports are not part of the starting-point documentation.
