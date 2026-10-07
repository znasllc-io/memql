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

### Repository rendering

`argocd.RenderRevision` is a bounded, read-only gRPC adapter to the existing
ArgoCD repo-server. Its request carries the complete supported Git source at a
full commit, application tracking identity, destination namespace, project and
render settings. Both repository caches are bypassed. A response must name that
exact resolved commit and the Kustomize source type. The owned result binds the
canonical request and complete resource bytes; it rejects ambiguous JSON,
duplicate resource identities, empty inventories and excessive output. Numbers
remain exact, and Secret values remain available only to native verification.
Credentials and upstream diagnostic text never enter the result. Generic
formatting reports only the resource count and digest.

The wire projection is pinned to ArgoCD v2.13.3 and generated by the normal
`proto-gen` drift gate. It currently supports HTTPS Git repositories with an
optional resolved username/token, ordinary Kustomize, and explicit namespace or
version selection. Image/patch overrides belong in the committed overlay;
unsupported source fields, credential transports and build options are refused.
The caller owns the trusted TLS connection and the configuration snapshot.

Do not replace this with `ApplicationService.GetManifests`. In the pinned server,
that preview uses the Application informer, redacts Secret data and returns only
the manifests, dropping the resolved revision. Reading the Kubernetes
Application before and after that preview does not bind its cached source or
recover the missing bytes.
[Pinned preview implementation](https://github.com/argoproj/argo-cd/blob/v2.13.3/server/application/application.go).

This adapter does not yet authorize preparation. Its consumer must obtain and
bind the actual Argo configuration and renderer identity, prove that source
inputs are closed and immutable (including remote bases, source parameter files,
generators and submodules), and verify every candidate image, resource change
and rollback dependency. It must fence configuration changes before applying.
A supplied source/configuration or the adapter's digest alone is not that proof.
No public render or installation capability is registered.

The read-only local rehearsal passed against the installed v2.13.3 repo-server:
two independent TLS clients rendered 68 resources from commit
`3f00b7a9a37ce6fb414f50b0ae9d74ef44bc5fb8` and obtained the same bound digest.
The complete Argo adapter suite, including that live read and its failure-case
tests, passed with the race detector in 2.974 seconds. The test used a public
certificate pinned through an authenticated Kubernetes port-forward; no TLS
verification was disabled and no Application or rendered resource was applied.
This proves the wire codec and actual rendering, not immutable image inputs or
the installation verification/rollout workflow. The test also caught and fixed
an incorrect assumption that a repository inherits the Application's project:
its credential/cache project is a separate explicit input and may be empty.

### Committed source closure

`argocd.VerifySourceClosure` verifies raw Git commit, tree and blob object
bodies against the exact requested Git object identities. It walks the
supported Kustomize dependency graph directly from those objects; it does not
read a checkout, trust an archive's commit label or execute Git filters.
The owned result binds the canonical render specification and each input's
path, mode, blob identity and native content digest. File contents and commit
messages are absent from its public projection and generic formatting.

The supported graph includes local resources, bases, components, patches,
generator files and explicit environment values, transformer configurations,
and the built-in namespace transformer. Remote inputs, symlinks, submodules,
Argo source-parameter overrides, checkout filters, executable generators and
unqualified Kustomize fields refuse verification. Object bytes, aggregate
bytes, object/file counts, directory depth and YAML structure are bounded.
New file-bearing features need an input codec before they can be admitted.
Inline strategic patches must use the explicit `patches.patch` field: the
legacy `patchesStrategicMerge` decoder can fall back from inline parsing to
file loading. This codec is qualified against the installed Kustomize v5.4.3
implementation; the consumer must still authenticate that renderer.
[Pinned strategic-patch loader](https://github.com/kubernetes-sigs/kustomize/blob/kustomize/v5.4.3/api/internal/builtins/PatchStrategicMergeTransformer.go).

A read-only committed-object rehearsal verified 61 local-overlay inputs and
43 cloud-overlay inputs at `23de433a7e9bbf606ed645d81369c27e86b0284e`, with
repeatable digests. This reads the cloud overlay's source only; it neither
contacts nor changes a cloud installation. The `git cat-file` reader used by
that opt-in test is test scaffolding, not the production acquisition adapter.

This is still a private verification primitive. Its production provider must
constrain authenticated source acquisition and honor cancellation throughout
reads. Preparation must bind this result to the actual renderer/configuration
snapshot and render result, verify candidate images and protected resources,
and repeat the relevant checks before an effect. A source-closure digest alone
does not grant installation approval or prove rollback readiness.

### Private source transport

`cmd/installation-source` supplies the bounded raw-object reader in the
Workbench runtime image. An isolated Job can run `/app/installation-source
--repository=/workspace --spec=/inputs/render.json --output=/artifacts/source.tar`
against its pinned checkout. The specification comes from native installation
configuration. Git replacement refs, global configuration, hooks, filesystem
monitors, transport and lazy fetch are disabled; only raw object reads run,
with no inherited clone credentials. Dirty working-tree files do not affect
the result. Cancellation and byte limits terminate and reap the reader process.

The collector writes a private, mode-0600 uncompressed USTAR artifact and one
content-free JSON result on stdout. The artifact contains only the verified
commit/tree/blob objects consumed by the input graph; source bytes and commit
messages can be confidential, so it must not become a public release asset.
Output installation is atomic and never overwrites an existing file. A repeat
verifies the existing archive; failure removes the collector's partial file.

`argocd.VerifySourceArchive` independently repeats the object-identity and
source-closure proof against the engine's own specification. It bounds entries
and bytes, rejects extraction/link/PAX tricks, duplicate or unused objects,
trailing data and truncated end records, and extracts nothing. Zero-filled
file content or tar padding cannot substitute for the final two records.

On October 7 the compiled collector captured the same committed local and
cloud input sets above (61 and 43 files), producing 319,488-byte and
291,328-byte private archives. A second process independently verified each
existing output and reported no change. Real-Git race tests also exercise dirty
worktrees, replacement refs, inherited Git overrides, concurrent collectors,
cancellation and partial-output cleanup. This is local CLI evidence, not an
installed Workbench Job or a cloud deployment.

The private `sourceCapture` adapter connects that collector to the existing
pipeline executor and immutable artifact ports. It requires an already-admitted
native context under the matching identified operator; it never upgrades an
inbound client's origin. It constructs one fixed,
cluster-only request: a pinned Workbench image, exact GitHub commit, five-minute
command deadline, 1 CPU and 512 MiB, no caches, services, fleet execution or
repository scripts. Its complete request and credential-free render specification
are bound into the scope digest. A new directory refuses collisions with
repository paths. Clone credentials remain in the clone container; an optional
image-pull credential reaches kubelet only. Other authenticated Git hosts need a
separate qualified acquisition codec.

Preparation must persist `sourceCaptureSpec` before dispatch: the requesting
operator, preparation-owned run and work-run IDs, step key and attempt, original
run start, repository and GitHub App installation ID, immutable collector image,
platform, image-pull secret **name**, and receiver-owned `RenderSpec`. It must
also bind the resulting scope digest. A replacement replica reconstructs this
scope from the journal and uses recovery-only dispatch; it cannot choose a new
attempt or silently restart an uncertain capture. The total native call is
bounded at ten minutes, including queueing and collection.

The returned `sourceCaptureReceipt` contains only the scope digest and immutable
artifact intent ID. The journal records it before acknowledging the Job. Source
verification separately pins that private receipt, opens the exact owner/run/
step/attempt and provider version, reads through EOF, checks size and SHA-256,
and repeats `VerifySourceArchive` against the receiver's specification. It
returns opaque native evidence; neither the collector's stdout nor a mutable
Library row is a verdict. Verification can be repeated after Job cleanup.

Cleanup is explicit. A durable stopped preparation may cancel its owned run;
that run ID must never be reused by a successor. Artifact references remain
until the journal durably retires their consumer and explicitly releases them.
A failure or uncertain reply never implies permission to unpin or retry work.
The adapter owns no second authority journal and exposes no public capability.

Local race tests exercise the real executor/Workbench authority hop with a
replacement receiver lacking the originating context, reconstruction from
serialized scope, receipt re-verification, changed inputs, wrong ownership,
truncation, failed EOF verification and unconfirmed cleanup. The external Job
and object store are protocol fixtures in this test; an installed collector Job
and the durable preparation/DSL wiring still need joint qualification. A
successful capture alone never authorizes an installation update.

### Signed images and complete resource inventory

The private installation verifier now resolves every rendered object's API
address through fresh Kubernetes discovery. It binds both complete inventories,
including unchanged objects' scope and paths, to the signed publication,
platform, native image bindings, both renders, and a sorted create/update/delete
diff. Discovery is bounded and shared only within one invocation. Another
replica can reproduce the evidence without the first process's cache.

Every workload image in the candidate and rollback render must name an exact
SHA-256 digest. A configured candidate image slot must match the signed
component, artifact, platform and published registry location. Added or changed
unbound images refuse. The codec includes normal, init and ephemeral containers,
image volumes, and the CloudNativePG image; the database's PostgreSQL-version
tag can coexist with its immutable digest. Nested Argo Applications and other
unqualified controller kinds refuse rather than hiding an additional source or
image input. CloudNativePG extension volumes, image catalogs and additional
image fields also refuse until their separate artifact inputs are qualified;
checking only the database's `imageName` would miss those images.
Native configuration supplies image bindings; a caller cannot
submit its own inventory or claim it was verified.

The full diff retains changes to Secrets, storage, routing and RBAC even when
they contain no images. Secret bytes are absent from ordinary formatting.
This is observation only: live protected-resource preservation, compatibility,
artifact availability, renderer/configuration identity and the scoped
preparation/installation workflow remain required. In particular, recording a
Secret deletion does not authorize it, and a signed image does not prove that
its registry is reachable or that the currently installed database can safely
change. No public capability or Argo write is enabled by this verifier.

Race tests cover signed versus substituted/mutable/unbound image inputs,
image-volume artifacts, API discovery failures and scope changes, complete
resource diffs and reproducibility. The installation journal's publication
binding uses the catalog's SHA-256 digest; native render/diff bindings continue
to use their own `memql-id` domain. The journal suite runs with a required real
database and continues to exercise independent connections and effect fencing.

### Update and rollback source

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
