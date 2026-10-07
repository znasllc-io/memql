---
title: Installation revision effects
audience: internal
status: draft
area: planning
sinceVersion: 0.25.0
owner: znas
---

# Installation revision effects

This is one unfinished part of [self-hosted delivery](pipelines-self-hosting.md).
The bounded adapter in `integrations/argocd` can observe and change an existing
Application's Git revision. It is not registered as a public capability, wired
to an update button, or an installation controller. Protocol doubles cover
failure cases; an opt-in test also exercises the installed controller against
an isolated ConfigMap. Neither qualifies a serving-engine rollout.

## Workflow boundary

Installation policy remains in sealed DSL. The intended preparation recipe is
to resolve a published candidate, resolve the installation's reviewed overlay
commit, verify compatibility, render and review the complete resource diff,
capture an immutable rollback point, and persist the exact authorized intent.
Owner release approval and permission to update this installation remain
separate gates. Native calls enforce those identities and bind every operation
to the durable installation record.

The following is the proposed scoped recipe, not a registered automation yet:

```memql
@template
automation installationPrepareWorkflow {
  builtin installationResolvePublishedCandidate()
  builtin installationResolveOverlayCommit()
  builtin installationCheckCompatibility()
  builtin installationVerifyResourceDiff()
  builtin installationCaptureRollbackPoint()
  builtin installationRecordPreparedIntent()
}
```

Starting an authorized update records the operation before calling the ArgoCD
adapter. A short reconciliation recipe reads its observed facts, chooses
whether to continue verification, records failure or recovery required, and
performs explicitly owned cleanup. ArgoCD continues applying the requested
revision independently of the serving engine. The replacement engine runs a
fresh observation recipe against the durable intent; it must not replay an old
approval against changed executable workflow definitions.

The journal now has an internal reservation/start/observation foundation,
described below. Source/render verification, workload and continuity probes,
reconciliation scheduler, rollback workflow and UI remain to be implemented.
Neither acceptance of a patch nor `Healthy` plus `Synced` completes an update.

## Native installation journal

`integrations/installation` is private and unregistered. Its native plan binds
the requesting operator, installed workflow, candidate and owner approval,
publication evidence, rendered resources/diff, immutable starting revision and
rollback render, and the complete Argo protocol intent. Persisting these
bindings does not verify their evidence. The candidate, source, render and
compatibility verifiers must be implemented before exposing preparation or
an update action; a caller-supplied digest is not proof.

All replicas serialize on one installation head in PostgreSQL. Reserving the
same plan returns the same intent; a different plan cannot take an active slot.
The native start operation commits `applying` before any external write.
Cancellation is allowed only from `prepared`, is permanent for that plan, and
cannot release a successor's slot. A timeout, crash or uncertain external reply
does not release a started attempt.

Each authority read recomputes the plan identity and rejects unexpected fields
or changed bindings. The current resolved developer/admin/owner role is checked
before storage; a new write also requires the original requesting identity and
workflow digest. Read-only observation remains available to another currently
authorized operator without reloading that old recipe. Observations use a
version captured before the external read, so a late result cannot overwrite a
newer result. Argo success remains an observation and cannot mark an installation
complete or permit the next update.

Real database tests exercise independent connections, concurrent reservations,
lost start-reply recovery, late starts after cancellation, stale observations,
changed stored authority, and migration rollback refusing nonempty history.
The package is in the canonical required-database CI selector. Completion,
explicit rollback/recovery, cleanup evidence and the scoped DSL/public surface
remain unfinished; no engine rollout is enabled by this journal alone.

## Immutable desired state

The update names a full Git commit in the existing installation repository and
overlay path. That commit must already carry the verified image digests and
candidate reference. Repository changes use reviewed pull requests; this
adapter cannot create commits, bypass review, change repositories or install
live image overrides. ArgoCD's own documentation describes parameter overrides
as a second source of truth; retain the committed overlay as the image
authority. [ArgoCD parameter overrides](https://argo-cd.readthedocs.io/en/stable/user-guide/parameters/).

Before reaching the effect, preparation must prove that rendered resources
match the candidate and target values, that protected database, volume and
secret resources are preserved, and that any unavoidable disruption has its
required assessment. A mutable previous branch or image tag is not an
immutable rollback point. Merely copying the current Application spec cannot
prove rollback readiness.

The adapter accepts a single-source Git Application, records the complete
baseline spec, UID, generation and previous intent marker, and changes only
`source.targetRevision`. A fresh read verifies that baseline. The patch tests
UID, resource version, generation and spec atomically before replacing the
revision and starting the identified sync. A deleting or controller-owned
Application is refused, as is another active operation. Status-only generation
changes can invalidate preparation; callers must refresh and reassess instead
of weakening the comparison.

Argo's typed status encoding omits optional empty/false source fields. This
initial adapter refuses those explicit values before starting, including
`kustomize.images: []`, rather than waiting forever for a byte-identical source
that the controller cannot emit. It currently accepts Git/Kustomize sources
only, and also conservatively refuses meaningful zero-valued source map entries.
Supporting additional source forms requires a pinned, controller-qualified
codec; the complete baseline spec and write preconditions remain exact.

The sync preserves the Application's declared sync options. It clears the
previous operation state atomically so omitted Argo fields cannot inherit an
earlier selective resource list. Existing automated-sync policy is preserved;
the effect's explicit prune flag does not turn off automated pruning. Full
resource-diff validation is therefore required for both manual and automated
Applications. The existing `memql-secrets` protection remains required.

The installed local ArgoCD v2.13.3 Application CRD has no status subresource,
which permits this atomic patch. Qualify this contract when upgrading ArgoCD;
do not silently separate it into two writes.
[Pinned Application type](https://github.com/argoproj/argo-cd/blob/v2.13.3/pkg/apis/application/v1alpha1/types.go).

## Uncertain outcomes and observation

The intent digest covers the entire protocol request, including the exact
baseline, target, destination revision and prune flag. Its annotation and sync
operation marker identify an effect, not authority. Only the native journal
may authorize applying it. Recovery must verify the persisted digest and
identity before restoring the intent; serialized input is not approval.

A lost response is unconfirmed. The next host reads the same Application and
intent. A matching marker and desired spec produce facts without another
write, even when the prior sync failed. A changed baseline, successor marker,
replacement UID or foreign operation refuses. There is no detached watcher or
unconditional write retry. A failed sync needs a new reviewed attempt or the
explicit rollback path.

Observation binds the marker, operation revision, full desired spec, sync
result source and revision, and the revision/source Argo actually compared.
It does not borrow a previous successful sync. Inline manifests, selective
resource lists or a substituted operation source cannot establish success.
Workload readiness, authenticated sessions, subscriptions, website traffic,
migration compatibility and cleanup remain separate evidence.

## Local controller evidence, October 7

`TestRevisionThroughInstalledArgo` passed against the existing local ArgoCD
v2.13.3 controller with a separate Application, namespace and AppProject. The
project allowed only ConfigMaps in that one namespace and denied all
cluster-scoped resources. The source was the committed
`test/fixtures/argocd-revision` directory, not the MemQL deployment overlay.

The actual controller changed the ConfigMap from `baseline` to `candidate`
between two exact commits. The test deliberately discarded the successful
Kubernetes patch response, then reconstructed the intent and observed completion
through another client. A same-intent retry issued no second patch. The first
run also began with stale selective-resource and force-sync options in the
previous operation state; they were not inherited by the new operation.

The extended test created a separate explicit rollback intent, observed the
controller restoring the original commit and ConfigMap value, and verified
that replaying the superseded update refused before another patch. It passed
under the race detector in 3.727 seconds (2.09 seconds in the test). Fixture
cleanup used the recorded UIDs for the Application, AppProject and namespace;
local evidence retains their final state and deletion confirmations.

This proves the revision protocol, controller serialization and recovery
observation with a real Argo controller. It does not prove candidate authority,
engine pod replacement, database migration compatibility, user/session/site
continuity, or the installation journal and reconciliation workflow.
