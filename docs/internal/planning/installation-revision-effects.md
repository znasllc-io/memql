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
to an update button, or an installation controller. Its tests use a Kubernetes
API protocol double; they do not qualify an installed rollout.

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

The journal, source/render verification, workload and continuity probes,
reconciliation scheduler, rollback workflow and UI remain to be implemented.
Neither acceptance of a patch nor `Healthy` plus `Synced` completes an update.

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
