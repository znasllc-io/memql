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

The private [installed preparation recipe](../../../dsl/installation/automations.memql)
now composes the bounded operations through the ordinary automation interpreter.
Its native receiving host and evidence gates are described under
[receiving configuration](#receiving-configuration-and-preparation-composition).
It is not exposed as a public update capability.

Starting an authorized update records the operation before calling the ArgoCD
adapter. A short reconciliation recipe reads its observed facts, chooses
whether to continue verification, records failure or recovery required, and
performs explicitly owned cleanup. ArgoCD continues applying the requested
revision independently of the serving engine. The replacement engine runs a
fresh observation recipe against the durable intent; it must not replay an old
approval against changed executable workflow definitions.

The journal now has an internal reservation/start/observation foundation,
described below. Source/render verification is composed in the private preparation recipe. Workload and continuity probes, the reconciliation scheduler, rollback workflow and UI remain to be implemented.
Neither acceptance of a patch nor `Healthy` plus `Synced` completes an update.

## Native installation journal

`integrations/installation` is private and unregistered. Its native plan binds
the requesting operator, installed workflow, candidate and owner approval,
publication evidence, rendered resources/diff, immutable starting revision and
rollback render, and the complete Argo protocol intent. Persisting these
bindings does not verify their evidence. Preparation composes the native candidate,
source, render and compatibility verifiers and requires their opaque results
before promotion; a caller-supplied digest is not proof.

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
explicit rollback, continuity qualification, cleanup scheduling and the public
surface remain unfinished; no engine rollout is enabled by this journal alone.

### Scoped revision execution and observation

The private installed `installationRevisionWorkflow` performs one bounded pass:
read the native entry mode, persist start before applying the exact recorded
Argo intent, then observe its current controller facts. The preparation and
plan bind a separate `ExecutionWorkflowDigest`, derived from the owned installed
recipe and immutable engine revision. A preparation recipe fingerprint cannot
substitute for this execution binding. Promotion verifies both bindings.

Starting requires the original resolved operator, an actual promoted preparation,
fresh authenticated receiving configuration, and unexpired artifact evidence.
An uncertain started operation keeps its slot. Repeating the same write scope
reconciles the recorded intent through the Argo adapter; a lost committed reply
does not create another sync. A different installed engine can run the explicit
observation mode, but its operations cannot acquire write authority even if the
recipe asks to apply. Every controller observation uses the journal version read
before that request, preserving protection against late observers.

Required-database race tests combine native promotion and revision journals with
a JSON Patch controller fixture. They exercise lost apply replies, fresh-host
recovery, changed execution recipes, stale configuration, omitted start, forged
workflow outputs and unbound children. A successful owned Argo operation with
`Healthy` and `Synced` still leaves the installation `applying` and its slot
occupied. These are controller protocol and journal tests, not serving continuity
or installed rollout evidence. The host remains private and unregistered.

### Explicit reversal journal

The private Argo adapter can now derive a rollback from one exact owned terminal
operation. It reads the current Application, requires no pending operation,
accepts only the owned `Succeeded`, `Failed` or `Error` outcome, and derives the
previous immutable commit from the original intent. The reversal preserves the
complete source, destination, project, sync policy and prune choice. A changed
Application, foreign marker, unowned outcome or mutable baseline refuses before
a write. Restoring a persisted reversal revalidates its complete relation to the
parent intent.

The native rollback journal retains that parent's installation head. Reserving
the reversal permanently fences the forward recipe, starting commits before an
external request, and observations have their own monotonic version. Another
replica recovers the same intent after a lost response; a late observer cannot
replace newer facts. Failed and successful reverse syncs both retain the head
until independent continuity and cleanup verification. Schema rollback refuses
to erase reversal history.

Required-database race tests cover concurrent reservations through independent
connections, the durable start before the controller PATCH, lost committed
replies, changed authority or stored bindings, stale observations and retained
ownership after failure. The isolated installed Argo ConfigMap test also ran
the new derived reversal path, passed under race in 3.749 seconds, and confirmed
UID-guarded deletion of its Application, AppProject and namespace. This is
controller protocol evidence, not an engine-update or serving-continuity result.
The journal remains private: its native host must still compose fresh receiving,
artifact and preservation verifiers before any possible write. Digest fields
in a journal row alone do not satisfy those proofs.

### Preparation before source dispatch

The private preparation journal reserves that same installation head before
source acquisition. A native scope binds the original requesting operator,
stable request ID, selected workflow and configuration digests, published
candidate, and exact candidate/rollback capture specifications. Both captures
share one preparation-owned run with distinct fixed step keys. A replacement
host looks up the original scope by request ID before resolving a new start
time or configuration. Changed inputs cannot reuse an existing request.
The scope also records the complete native Argo intent before capture, including
the Application namespace/name/UID, generation, destination cluster, full baseline
spec, sync options and operation identity. Receiving admission must authenticate
that live Application and configuration; persisting it is not verification.

The journal commits dispatch intent before contacting Workbench. Retries ask
the receiver to recover the same attempt; they never authorize a fresh replay.
If a process dies between committing intent and dispatch, and the receiver has
neither a queued record nor a Job outcome, the result remains uncertain and the
installation head stays occupied. Absence of a Job is not proof of no effect.
Only confirmed fenced retirement may release that started preparation. The
private retirement operations below retain the slot throughout cleanup;
cancellation before dispatch remains a separate immediate journal transition.

Execution receipts are persisted before artifact verification, and verified
source/receipt fingerprints before Job acknowledgement. Artifact verification
always reopens the pinned archive and repeats the native closure proof, even
when an earlier host recorded matching digests. Neither stored fingerprints nor
successful Job cleanup reconstructs verification authority. Each effect
rechecks the original currently admitted operator and workflow; reads may be
performed by another currently admitted native operator.

The new database tests cover independent connections, concurrent dispatch
claims, request recovery, changed authority, and the shared revision/preparation
fence. A separate test reaches the real Executor, Workbench forward handler and
fresh Runner instances over an HTTP Kubernetes fixture, distinguishing an
existing durable outcome from an uncertain missing attempt. These tests passed
in required-database CI and in the October 7 local installation suite against
an isolated PostgreSQL/TimescaleDB fixture with `MEMQL_REQUIRE_DB=1` and the race
detector enabled (19.784 seconds). This slice remains private and unregistered.
The scoped cleanup recipe, current configuration verification, sealed DSL
wiring and installed recovery qualification remain required before exposing
preparation.

### Producer retirement foundation

The Library adapter can permanently fence an authorized owner/run/step/attempt
before enumerating its artifacts. The fence shares the upload admission lock,
including an SQL trigger that refuses an older replica's delayed reservation
or recovery during a rolling update. A replacement can repeat the fence after
a lost response. Inventory is bounded to 128 ordered intent IDs per call and
includes reserved and retired records, so a lost upload response cannot hide
bytes from cleanup. Migration rollback refuses nonempty fence history.

This closes admission, not the entire cleanup operation. A previously admitted
provider request may still be in flight. The owning workflow must first retire
the producer, then fence it, release its durable consumer pins, and reconcile
every inventoried intent through the permanent provider tombstone before
releasing the installation slot. Neither an empty unfenced inventory nor an
elapsed timeout can prove completion. No public cleanup action is registered.

The local required-database race tests cover lost upload receipts, replacement
hosts, concurrent reservations, pagination and owner authorization. A separate
regression observes the older writer waiting on the actual native PostgreSQL
advisory lock, then refusing after the fence commits. The real Azurite test
resumes an admitted upload after cleanup establishes its leased tombstone and
confirms that the provider refuses it. These tests passed; they do not qualify
the unfinished installation cleanup workflow.

### Retiring a started preparation

The private journal now commits `retiring` before cleanup, keeping the original
dispatch history and installation head. That transition blocks late source
dispatch, receipt, verification, acknowledgement and promotion callbacks. It
binds the original requesting operator and preparation workflow plus a separate
selected cleanup workflow fingerprint; persisted fingerprints alone do not
authorize an external caller or replace the native workflow host.

Separate bounded operations stop the preparation-owned Workbench run, fence
each capture's artifact scope, permanently release its exact source consumer,
load one inventory page, and retire one inventoried artifact. The native gates
enforce those prerequisites; the cleanup recipe owns ordering and iteration.
A capture without a recorded receipt can still have unknown uploads, so both
capture scopes must reach a confirmed empty inventory after retirement. Pinning
requires a committed capture receipt, and retirement closes any known consumer
before a delayed verifier can acquire another pin.

Each page holds at most 128 intent IDs and native retirement receipt digests.
A replacement host recovers a pending page after a lost reply. Advancing its
cursor requires every receipt, and an external error preserves the unconfirmed
entry. Parallel callbacks on different connections preserve each other's
receipts. Only confirmed producer stop and completion of both capture scopes
release the installation slot as `cancelled`. A lost final reply returns that
history without touching a successor's head. Migration rollback refuses to
erase cleanup history.

The private installed `installationRetirementWorkflow` now composes these
operations as one bounded reconciliation pass. It processes one page per
capture, then observes whether native cleanup receipts permit final release.
A returned `retiring` record means more work remains, never completion. A
fresh host resumes its pending page and the caller schedules another pass.
The native host owns the recipe snapshot, binds its fingerprint and immutable
engine revision, refuses unbound operations/children before effects, rechecks
the original operator on each callback, and ignores claimed DSL return proofs.
Local required-database race tests exercise the installed recipe and a second
recipe with reversed role order and joined parallel artifact calls, including
lost provider replies and recovery on an independent connection. The
reconciliation scheduler, public surface and installed recovery qualification
remain unfinished; these operations remain private and unregistered.

### Atomic preparation handoff

The private promotion operation transfers the shared installation head directly
from preparation to one revision attempt in a PostgreSQL transaction. It binds
the original requester, workflow and configuration to the exact signed
publication, acknowledged source captures, candidate and rollback renders,
resource diff, and storage/credential observations. A supplied digest cannot
replace those native verifier results. The Argo intent must match the complete
reserved protocol intent as well as the captured source, project, namespace and
revision. Artifact pins remain with
the durable source consumer throughout the handoff and rollback lifetime.

The preparation records the promoted plan ID in the same transaction. A lost
commit reply can be recovered through the stable request ID on another replica;
concurrent promoters return the same plan. A changed plan refuses, late source
callbacks cannot reopen preparation, and a historical retry cannot take a
successor's slot. A failed head update rolls back both journal writes. Migration
rollback refuses to remove a recorded promotion.

The promotion tests passed in required-database CI and in the same local race
suite. Independent review is complete, including a second-connection PostgreSQL
regression for stable JSONB intent identity and exact large integers. They cover
those transaction and authority boundaries using real PostgreSQL and native
verifiers over protocol fixtures.
This is a journal handoff, not complete installation admission: actual receiving
configuration/renderer identity, compatibility, artifact availability and the
live rollout/continuity/recovery workflow remain required before registration.

## Immutable desired state

### Artifact and declared compatibility admission

The private `artifactAdmission` scope binds native receiving configuration,
signed candidate and installed rollback publications, both complete renders,
the resource diff, platform, image slots and required component dependencies.
Configured slots must match signed OCI artifacts in both publications. All
remaining workload images must stay at their immutable baseline. Every image
in either render, including unchanged infrastructure, requires a complete fresh
OCI read through an explicitly configured registry. A signed location cannot
change that registry's transport or supply credentials.

The scope returns immutable image obligations. The installed
`dsl/installation/automations.memql` recipe reads those obligations, invokes one
bounded check for each, and requests completion. The ordinary `workflowhost`
interpreter executes that sequence; a second recipe can compose the same ports.
Registry trust and opaque observations never enter DSL arguments. Native
completion refuses omitted, duplicated, expired or rebound observations and
requires the opaque result from actual manifest/config/layer verification.
The observation's registry, root digest and platform must match its obligation.
Scopes expire after one hour; evidence expires no later than thirty minutes
after its earliest registry read began. Recovery constructs another scope and
rereads the required bytes, without trusting serialized receipt assertions or
another process's local state. Every callback requires the original identified
operator under already-admitted internal origin. A recipe cannot grant that
origin, bind a different operator, load an unbound child or return its own proof.

The private host snapshots the installed recipe before execution. Its digest
binds the complete definition, native contract and exact engine commit, and the
final evidence binds that digest and the operator. The full receiving preparer
must incorporate it into the durable workflow identity before dispatch. Changing
the recipe or native revision cannot reuse that binding; loader mutation cannot
replace a recipe already executing. A returned success value or ignored failed
check is insufficient without native completeness.

`pipelines.CheckPublishedReleaseOverlap` evaluates the existing signed version
ranges against both old and new dependency versions. Both publications must
have the same component identities. Every required dependency and every pair
declared by either publication must be declared by both. Republishing different
source/artifact bytes under the same component version is refused; adding a
mirror for the same bytes is allowed.

This is declared dependency compatibility and current artifact readability,
not installation approval, registry retention, image unpackability, replica
wire compatibility or database migration safety. The receiving host must still
authenticate and freeze the full configuration, including trust roots and
credential versions, bind this evidence into preparation, enforce its deadline
before effects, and require separate migration and continuity qualifications.
The recipe and builtin declarations are installed, but no integration provider
or public artifact-admission entry point is registered yet.

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

### Live storage preservation

A separate private read-only verifier checks storage against the receiving
cluster, including resources whose committed spec has not changed. Software
updates currently require the same desired storage declarations in both
renders and exact agreement with live requested and reported capacity. A
larger live volume cannot silently inherit a smaller committed value. Pending
expansion and resizing refuse: rolling back to the original overlay after an
expansion needs a separately qualified maintenance plan. Positive integer
binary and SI capacity forms are compared exactly, without floating point;
other quantity forms refuse until qualified.

The verifier reads each declared PVC and its bound PV, verifies both identities,
their matching capacity and the PV's claim reference, and binds the backing
volume specification. For
CloudNativePG it reads the complete bounded namespace PVC inventory, verifies
controller ownership against the current database UID and checks data/WAL
roles, capacity and enough claims for the live instance count. Candidate,
rollback and live replica counts must agree; scaling requires its own
maintenance contract because it creates or retires persistent storage. Missing labels
cannot hide an owned claim. Truncated lists, replaced ownership, deleting or
unbound volumes, missing WAL, changed bootstrap/storage declarations, and
unqualified persistent controllers refuse. New persistent storage, standalone
PV management, StatefulSet claim templates and tablespaces require additional
codecs and maintenance contracts. Ordinary formatting contains no resource
bodies or storage paths.

The October 7 read-only local rehearsal used the installed repo-server's TLS
identity to render commits `3f00b7a9a37ce6fb414f50b0ae9d74ef44bc5fb8` and
`9a7fecc14987c9fa240f7e5eef3fd6ba083b7b65`. Independent local Kubernetes reads
agreed on four current PVC/PV bindings. The live case passed in 2.87 seconds;
the required-database installation race suite, including the live case, passed
in 6.898 seconds. The rehearsal caught a real protocol detail: Kubernetes
omits type metadata inside a typed PVC list. The codec derives absent type
fields from the verified collection while refusing contradictory fields.

This storage proof grants no database restart, resizing or installation
authority. Compatibility and continuity, artifact availability, actual
renderer configuration, durable preparation and the scoped installation
workflow remain unfinished.

### Live credentials and namespace preservation

A separate private read-only verifier compares every rendered Secret and
Namespace, the namespaces containing rendered objects, and native-configured
protected resources seeded outside Git. Its result binds fresh UIDs, logical
credential fingerprints, namespace state and ownership to both complete
renders. A second replica obtains fresh observations; replacement or rotation
invalidates earlier evidence. The configured protected-resource list must come
from the receiving installation, never a client's claimed allowlist.

Rendered credentials must equal both the rollback declaration and the live
Secret, including type and immutability. The codec observes Kubernetes's
`stringData` precedence and decoded `data` semantics. Creation, removal,
ownership changes, terminating namespaces, destructive Argo resource options
and hooks refuse ordinary update qualification. Every protected object outside
Git, including namespaces and owner ancestors, requires explicit `Prune=false`
and `IgnoreExtraneous` protection. Conflicting duplicate options cannot
establish that protection.

Owner references are resolved through fresh API discovery, including
cluster-scoped owners of namespaced resources. Bounded ancestor reads verify
UIDs and reject missing/deleting owners, cycles and changes to rendered owners
that could garbage-collect otherwise unchanged credentials. Unchanged rendered
owners must also match live material: restoring drifted Certificate or other
controller configuration can rotate credentials. Unknown controller defaults
require a qualified codec rather than being silently ignored. Evidence retains
only resource identities and fingerprints. Arbitrary annotations, including
last-applied configuration containing Secret data, are never retained in the
observation or formatted as diagnostics.

This is a preservation observation, not an installation authority or a promise
against concurrent external changes. The admitted preparer must still verify
Application-level sync policy and renderer identity, bind all evidence to its
durable preparation, and recheck it before an effect. Actual installed update,
rollback, credential/session continuity and recovery remain separate gates.

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

## Receiving configuration and preparation composition

The private receiving host now reads `installation.json` from one named
operator-managed Kubernetes ConfigMap. Its constructor receives the namespace
and name from native wiring; a preparation request selects only installation,
request, candidate/catalog identities, an exact overlay commit, and prune
intent. It cannot supply trust, connection material, a rendered inventory, or
an assertion that verification succeeded. The host and its DSL ports remain
unregistered while the installed update/recovery qualification is unfinished.

`receiverConfiguration` in `integrations/installation/receiver_config.go` is
the version-1 schema. It includes:

- `installationId`, `platform`, and the named Argo `application`;
- `catalog` publisher identity, HTTPS endpoint, signing keys, and named
  credential/optional CA Secret keys; `rollback` names its exact signed
  candidate and catalog digest;
- `renderer` Deployment, Service, container, immutable platform image,
  `argocd-2.13.3-kustomize-5.4.3` profile, configuration ConfigMap and TLS Secret;
- `collector` immutable image, GitHub App installation ID and optional pull
  credential Secret key;
- `registries` with explicit HTTPS routes and optional named credential/CA
  keys; `imageBindings`, required component `dependencies`, and `protected`
  Secrets/namespaces.

Secret references resolve in the receiving ConfigMap's namespace, except
renderer material and workload references, which resolve in the Argo
Application's namespace. API access must use the existing projected-service-
account `deploycontrol.ClusterAPI`, with its cluster CA and rotating token.
Grant GET only on the named configuration, Application, AppProject, renderer
Deployment/Service, its Pods/ReplicaSets and referenced ConfigMaps/Secrets;
renderer EndpointSlice discovery and repository Secret selection need bounded
namespace LISTs. Repository selection observes the registered repository/cache
project independently of the Application project, following the pinned
[Argo repository selection](https://github.com/argoproj/argo-cd/blob/v2.13.3/util/db/repository_secrets.go); a complete empty inventory
permits the public repository default. Only Secret identities and versions enter
that durable binding. API/version
and resource discovery are read-only. The reader never seeds these resources,
writes credentials, changes RBAC or alters the serving cluster.

Configuration and Secret UIDs/resourceVersions bind change-away-and-back and
credential rotation without saving credential values or their hashes. Every
read set is reobserved before it is accepted. Application, Deployment and Pod
status-only RV churn does not alter persistent configuration identity; their
identity, spec and generation remain bound, and ready state and the actual
runtime image are freshly checked. EndpointSlice list RVs can advance because
of unrelated writes, so their complete ordered contents bind the inventory.
Missing/truncated collections, replacements and inconsistent endpoints refuse.

The renderer must be fully ready, with service endpoints addressing Pods owned
through ReplicaSets by the configured Deployment. Each observed runtime image
must match the configured immutable platform image. That image/profile pairing
is an operator-reviewed trust input; a version label is not an executable
verification result. Custom binary mounts, sidecars, commands, Kustomize
versions and unsupported transports require their own qualified profile.
Mutable image tags do not satisfy this contract. Renderer environment names
are qualified explicitly; inline passwords and telemetry credentials refuse
in favor of versioned references. The fixed copy-helper init command may inherit
the main container's literal `ARGOCD_EXEC_TIMEOUT`; other init environment,
indirect values and changed commands remain unqualified.

The native connection verifies the renderer's certificate chain and service
DNS identity and additionally pins the exact leaf from its named Secret. It
never inherits Argo's permissive default TLS behavior. The configured
certificate must actually be mounted and served; Argo 2.13 requires repo-server
restart after certificate changes ([Argo TLS configuration](https://argo-cd.readthedocs.io/en/release-2.13/operator-manual/tls/)).
The catalog port freezes one authenticated source using the existing SDK and
signed-envelope reader; it does not introduce another transport or treat
browser-supplied release fields as evidence.

`installationPrepareWorkflow` now expresses publication lookup, compatibility,
reservation, capture/verification/acknowledgment/render of each source,
resource/storage/protected-resource checks, the artifact child recipe,
configuration reobservation and atomic promotion. The native host binds both
installed recipes and the engine revision before source dispatch. Recovery
looks up the existing request before selecting a new start time and cannot
recycle a request with changed inputs. A fresh host repeats private source and
artifact verification; successful serialized fingerprints are not proof.

Promotion binds the fresh configuration observation, artifact recipe/operator,
rollback publication and artifact expiry alongside the existing source,
render, storage and sensitive-resource bindings. An expired artifact binding
cannot begin a new revision effect. A previously started revision retains its
history and recovery identity after expiry; this is not permission for a new
attempt. Migration compatibility, serving continuity, complete update and
rollback orchestration, operator seeding/wiring and the installed multi-repo
rehearsal remain required before registering the update entry point.


The local protocol recovery test runs the installed preparation and artifact
recipes through real journal connections, signed publications, private source
archives, a certificate-verified gRPC renderer and an HTTPS OCI registry. A
candidate-render interruption leaves both captures durably acknowledged. A
replacement host reopens both archives and rerenders both revisions, reads
complete candidate and rollback image bytes, and promotes without launching or
acknowledging duplicate captures. This establishes composition and recovery of
preparation. It does not substitute for the installed serving-engine update and
rollback rehearsal. The automation corpus records the bounded operation order
with stubbed effects; native authority and evidence are exercised separately.


The private `installation.NewPreparer` constructor accepts all native execution
and source-lifecycle ports together and rejects an incomplete host. Its
identifiers-only `PrepareSelection` and `PreparationResult` expose no credential,
rendered material, source archive or caller-provided proof. The app adapter uses
`buildinfo.Commit()`, the projected cluster API/namespace, the existing serving
Library uploader, the existing Workbench forwarder and repository token minter,
and `releasecatalog.NewSnapshot`. `MEMQL_INSTALLATION_CONFIG_NAME` explicitly
selects the receiving ConfigMap; absence disables preparation. Constructing this
adapter does not register a capability, provide a fleet fallback or authorize a
revision write. The revision recipe's separate immutable execution binding must
be connected before public activation.

### Installed renderer qualification

The October 7 local renderer bootstrap created its named TLS Secret, pinned the
existing Argo platform image in its normal and copy-helper containers, and added
one ingress policy permitting only MemQL BFF/agent pods on TCP 8081. The renderer
became ready, all three existing Argo Applications stayed Healthy/Synced, and
the 26 captured serving Deployment, engine Pod and database objects retained
their identities and specifications. The Application baseline, receiver
configuration, preparation RBAC and protected-resource annotations were held.

`TestReceivingRendererThroughInstalledArgo` then read 12 actual renderer inputs
through an explicitly selected authenticated Kubernetes connection, checked
Deployment/ReplicaSet/Pod ownership and image identity, verified the live TLS
leaf against the named Secret, and reobserved the complete read set. It passed
with the race detector in 2.715 seconds. Its operator-authenticated loopback
forward is a diagnostic transport; this result does not qualify serving-node
RBAC, network-policy reachability, a candidate publication or an engine update.
The opt-in test requires `MEMQL_INSTALLATION_RENDERER_TEST_KUBECONFIG`,
`MEMQL_INSTALLATION_RENDERER_TEST_ADDRESS` and the reviewed platform digest in
`MEMQL_INSTALLATION_RENDERER_TEST_IMAGE`. It only reads the installed renderer.

### Receiving configuration after an owned revision change

Preparation binds both the full baseline configuration digest and a separate
configuration invariant digest. The latter retains every configuration, Secret,
renderer, project, repository and discovery binding, while binding the
Application's authenticated name, namespace and UID separately from its mutable
spec, generation and operation. Both digests originate from the same reobserved
read set and are carried through the durable preparation and promotion.

The private continuation reader observes the actual current Application. It
requires the native Argo verifier to match the complete desired spec, exact
intent marker and an owned pending or recorded operation against the supplied
journal-bound intent. A failed owned operation is eligible for observation;
an unrelated operation or a forged healthy baseline is not. The reader never
rewrites current Application bytes to recreate the old baseline. It reobserves
the Application's exact UID/resource version together with every other input.

Its opaque evidence binds the complete admitted plan, invariant digest, active
intent digest, exact target and observation time. Each use recomputes those
identities and refuses expired or substituted evidence. Only a native host may
select the journal-bound forward intent or a separately validated reversal.
The private snapshot retains current renderer and catalog connections for
reopening retained source and rerendering. Configuration continuity alone does
not replace fresh artifact, storage or protected-resource verification, grant
rollback authority, complete an installation or release its active slot.
