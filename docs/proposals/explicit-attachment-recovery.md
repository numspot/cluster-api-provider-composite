# Proposal: explicit attachment recovery

**Status: proposed; not implemented.** This document is a review proposal, not an
operator runbook. No recovery annotation, command or API described here is available.
Current [recovery precautions](../operations.md#recovery) remain authoritative.

## Problem and scope

[Issue #10](https://github.com/numspot/cluster-api-provider-composite/issues/10)
identifies two different lifecycle needs:

| Operation | Existing resources | Requested change |
|---|---|---|
| Source rebind | The same live CompositeProvider and its children | Accept explicitly selected new source UID/generation pairs |
| Retired attachment replacement | The old provider completed retirement; a same-name provider has a new UID | Start a new, explicitly authorized attachment incarnation |

The [retired-provider follow-up](https://github.com/numspot/cluster-api-provider-composite/issues/10#issuecomment-6993037251)
is the second case, not source drift. The [newline follow-up](https://github.com/numspot/cluster-api-provider-composite/issues/10#issuecomment-6989501152)
and [attachment isolation issue #14](https://github.com/numspot/cluster-api-provider-composite/issues/14)
need independent fixes; harmless JSON formatting must never require recovery consent.

**Proposed default: no automatic rebind and no automatic retired-record deletion.**
An operator must authorize one exact transition. Acknowledgment permits a desired
configuration change; it does not prove that two source payloads or cloud accounts
are equivalent.

## Current constraints

The CompositeCluster attachment annotation retains provider and cluster UIDs,
profile identity, both source UID/generation pairs and retirement state. The
provider-local annotation mirrors its record. Status does not replace either pin.

Retirement waits for observed worker consumers and dependent regional attachments,
then infrastructure absence, then bootstrap-template absence. It records retirement
before releasing the provider finalizer. Missing APIs and unknown Machine bootstrap
provenance retain protection. This is not proof that every cloud resource is absent.

A fresh attachment name can be used after clean retirement, subject to ownership,
canonical-name and capacity checks. It changes the generated bootstrap-template name
and therefore worker references. Retired records still count toward the limit of
**eight retained attachment identities and 16 KiB** per CompositeCluster. This is a
bounded alternative, not unlimited replacement support.

## Explicit intent

Prefer an immutable, namespaced recovery request with its own UID over a bare
"acknowledged" annotation. The API name and storage format need review before
implementation. Its immutable intent must identify:

- The operation type and exact CompositeCluster namespace/name/UID.
- The provider name and expected old/new provider UIDs. For source rebind, these
  UIDs must be equal; for retired replacement, they must differ.
- The complete expected committed and desired attachment records, including
  retirement state, profile UID, credential name and native/bootstrap identities.
- Both expected old and desired new source references and UID/generation pairs,
  including an unchanged pair. No wildcard, "latest", name-only or hash consent.

A request authorizes only that transition. Another source edit, provider recreation,
cluster replacement or competing request invalidates it. Completion cannot authorize
a later transition, even if the same names are reused.

Request creation is privileged: accepting changed source revisions can change native
infrastructure and bootstrap configuration. The API and RBAC review must define who
can grant that authority. Composite still never reads Secret contents, translates
cloud APIs or modifies shared source templates.

## Operation-specific gates

### Source rebind

Keep the provider, cluster, profile UID, credential selection, native GroupKind/name
and bootstrap name unchanged. Initial scope does not support profile recreation,
changing immutable references or adopting another provider's resources.

Resolve the exact acknowledged sources through direct reads. Validate policy,
protected fields, exact JSON values, endpoint rules and installed native schemas.
Directly verify any existing children's dual ownership and UIDs. Normal guarded
creation may recreate an absent child; it must not adopt an unexpected incarnation.

Workers may still consume the existing bootstrap template. Consent does not imply
worker rollout, native-provider compatibility or cloud-side safety; those remain
caller/native-stack responsibilities. Only committed recovery may resume normal
child reconciliation, which continues to report current health independently of
latched initialization.

### Retired attachment replacement

Require an intact retired record and the exact old/new provider UIDs. Recheck worker
consumers, dependent canonical attachments and confirmed absence of **both** old
infrastructure and bootstrap objects. A previous retirement observation or successful
Delete call is not enough for the new transition. Unavailable APIs fail closed.

Validate the replacement as a new attachment against current protected profiles,
source revisions, canonical lookup, distinct native identities, ownership and all
capacity limits. Do not infer a new pin from status or accept leftover owned objects
as evidence of a fresh binding. Preserve the old incarnation in durable history;
replacement must not overwrite or delete its retirement record.

## Crash-safe commit protocol

Updating the cluster record and provider pin in one reconcile is **not atomic**.
The implementation needs a versioned durable operation journal, not two best-effort
annotation updates. Proposed phases are:

1. **Validate:** directly read the complete committed composition and exact request;
   check all gates and bound the full proposed history/journal before writes.
2. **Prepare:** persist the operation UID, immutable old/new records and phase on the
   cluster with a resourceVersion precondition. The old binding remains committed.
   Normal writes for the affected attachment must now stop.
3. **Stage:** persist the provider-side staged pin/protection with UID/resourceVersion
   guards. Repeated staging must be idempotent. Other attachments may proceed only
   when composition-wide identity and canonical checks remain valid.
4. **Commit:** revalidate live identities and gates, then atomically switch the
   cluster's committed record and journal phase in one guarded object update.
   This cluster update is the commit point; it does not mean provisioning is Ready.
5. **Finalize:** converge the provider mirror to the committed record, mark the
   request complete and emit a bounded recovery Event after successful persistence.
   Normal child writes resume only when the committed identity and mirror agree.

After a crash, a reader must recognize the exact operation UID and resume its phase.
An unknown journal, mismatched staged pin, stale consent or conflicting incarnation
must retain protection and publish an actionable failure, never guess a winner.
Retries must not commit twice. Event delivery is best-effort, not a transaction log.

Deletion takes precedence over new recovery work. Before commit, abandon staging
through a durable abort path that restores the old mirror without native writes.
After commit, teardown must resolve the committed record plus retained history
without a live profile or a completed request status. Pause/scope rules still apply.
Both paths need an explicit restart/retry transition table before implementation.

Direct rechecks are not a cross-resource lock: another controller can create a worker
consumer after an absence check. Retired replacement needs a reviewed consumer-fencing
or admission mechanism, including how Composite verifies that it is in effect. A
caller merely promising quiescence is not controller-verified absence. Do not enable
replacement until this race and the fence's release/restart behavior are resolved.

## History, compatibility and limits

The current map cannot represent several incarnations under one provider name. A
replacement design therefore needs a versioned history format and explicit upgrade
rules for current attachment records. Unknown formats must fail closed. Historical
cluster provider-bindings and legacy hash pins remain unsupported.

Every retained incarnation counts toward the eight-identity limit; source rebind
history and in-progress journals also consume the 16 KiB record budget. Reject a
transition that cannot fit **before** staging either object. Recovery is not a
capacity bypass. Supporting repeated replacement beyond these bounds would require
a separately reviewed history store/compaction and retention policy, not silent GC.

Older controllers must not interpret staged or newly committed records as ordinary
old-format pins. Define controller/CRD upgrade and downgrade restrictions before
shipping; keeping the served API version unchanged is not a migration strategy.

## Review decisions and acceptance tests

Before implementation, agree on the immutable request API/RBAC, journal storage and
versioned record schema, abort/deletion transition table, and history/capacity policy.
Do not publish an annotation-surgery runbook while these decisions are unresolved.

Required behavioral coverage:

- Exact source rebind, unchanged source pairs and stale/partial acknowledgment.
- Clean retired replacement; UID mismatch, lost pins and unexpected leftover objects.
- Restart or write conflict before/after every persistence boundary; duplicate and
  competing requests; no child writes while staged or partially committed.
- Foreign ownership, unavailable APIs, unreadable worker provenance, late consumers,
  canonical dependencies and active native-identity collisions.
- Pause, scope and deletion at every phase; source-independent teardown and safe
  catalog protection throughout preparation, commit and abort.
- Entry/byte-limit rejection before writes; unsupported record versions/downgrades.
- Current-generation conditions, bounded non-secret diagnostics and post-write
  transition Events without heartbeat status churn.

Native-stack integration must separately establish what changing an existing native
spec can safely do. Passing API-server tests cannot establish cloud-side equivalence
or successful worker rollout.
