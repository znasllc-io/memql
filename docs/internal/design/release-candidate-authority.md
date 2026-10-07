---
title: Release candidate authority
audience: internal
status: draft
area: design
sinceVersion: 0.25.0
owner: znas
---

# Release candidate authority

The native candidate journal separates verification, owner approval and an
external publication effect. It is infrastructure for the installed release
workflow, not an alternative workflow. Owner-gated builtins provide preparation,
review, approval, publication and retirement. Scoped operations are registered
only to support the installed template; direct calls refuse even for an owner.

The BFF and agent wire the same native pipeline artifact store and configured
blob client that hold the original upload receipts. Without that binding,
artifact operations refuse. Existing review history remains readable from the
database even if blob storage or operator configuration is unavailable. No
schedule or publication is enabled merely by registering these capabilities.

`MEMQL_RELEASE_CANDIDATES` is an explicit global variable containing a bounded
JSON object with `formatVersion: 1`, `sources` and at least one target in
`registries` or `releaseAssets`. Source entries
carry `component`, `repository`, `path`, optional `jsonField` and optional
`credentialSecret`. Registry entries use the connection and credential-reference
contract in `candidate_targets.go`; no secret value belongs in this JSON.
Configuration is reread without the engine's result cache for each operation.
Unknown or duplicate JSON fields, duplicate source/target identities and unsafe destinations fail
before effects. There is no environment or plaintext credential fallback.

Release asset targets bind an existing draft ID, tag, exact source repository
and commit, filename, API/upload/download origins, trust and credential
reference (`candidate_file_targets.go`). Their namespace is shared with registry
targets; duplicate IDs refuse across both formats. A file destination must
match its component's source, requires a file smaller than 2 GiB and cannot
consume an OCI image as an ordinary release asset. Configuration discovery
exposes the kind, draft, tag and filename for review, never credentials.
Draft/tag creation and release promotion are separate effects, not side effects
of this adapter. They remain required for the complete release-assembly workflow.

`releaseCandidateConfiguration()` exposes source rules and target identity,
origin and repository, never credential values. `releasePrepareCandidate`
accepts a candidate object and returns its verified immutable manifest and
digest. `releaseCandidates` returns owner-scoped summaries with a bounded limit
(default 20, maximum 50) and opaque keyset continuation cursor. Immutable
creation time and candidate identity determine order, so status changes do not
move entries between pages. Each summary revalidates its stored manifest;
interrupted preparations remain discoverable. New entries created after the
first page appear on a fresh listing. `releaseGetCandidate` reads that exact candidate; `releaseApproveCandidate`
reverifies it and returns its separately persisted approval ID.
`releasePublishCandidate` requires both identities and the exact target,
component and artifact. `releaseRetireCandidate` fences only unpublished
candidates and releases their references. Every entry rejects non-owners before
configuration, database or network access. Review history is scoped to its owner.

`component/pipelines.ReleaseCandidate` binds independently versioned components,
exact repository commits, compatibility ranges, immutable artifact receipts,
work receipt digests, the installed workflow identity and operator destinations.
Canonical ordering and a domain-separated SHA-256 identify the complete review
boundary. Changing any of these inputs creates a different candidate. A target
digest must cover its resolved configuration, including allowed download
origins and credential references; credential values must never enter the
manifest. The native contract checks structure and compatibility; it does not
decide which checks a release requires.

The release evidence reader uses fresh caller-owned work queries and requires
a successful live pipeline run and a successful deterministic execution step.
Artifact production proof refuses skipped work, failed or unfinished runs, cancellation, replay,
unknown execution definitions and missing or nonzero exit codes. It verifies
source, attempt, execution definition and the full recorded call/result. A
step ID alone is mutable; the receipt digest also binds its recorded version.
The complete-run reader reconstructs every declared step in its original order
and checks the run's definition fingerprint. Evidence must include every step
of every selected run, and every artifact producer must belong to that evidence.
The installed `releasePrepareCandidateWorkflow` requires full-mode runs before
recording preparation. Each declaration records whether it is a command or a
notification. A delivered notification retains its actual delivery identities;
it never becomes a command exit or an artifact producer. A skipped step must
match its originally recorded selection code and reason. The installed recipe
permits only `pipeline_not_affected` (an empty planned selection in a full run),
while preserving that skip in the complete evidence set. It refuses carried
`pipeline_passed_earlier` receipts until the original execution chain is
verified, and unplanned or blocked skips fail natively. A successful aggregate
cannot silently replace missing individual receipts. Existing runs without
typed declarations require fresh qualification; missing type is not inferred.

The preparation scope snapshots the installed preparation and publication
templates and configured version sources, binding them plus the immutable
engine commit into `workflowDigest`.
Children and logic bodies are refused until their definitions can also be
included in that identity. Native version reads use the exact source commit and
operator-selected `VERSION` or top-level JSON version property. Candidate
arguments cannot choose a file, credential or moving branch. Registry targets
bind their exact connection settings, download origins, certificate roots and
credential references. Credentials resolve only from the secret provider and
never enter the manifest. A changed target refuses before credential lookup.

The DSL orders evidence, source-version and destination checks, preparation
intent, artifact pins, byte verification and readiness. Native operations retain
each prerequisite: another recipe cannot skip verification and mark ready.
Every evidence artifact is pinned, not only the image selected for publication.
Product OCI archives undergo complete archive, platform, image and layer checks.
Evidence and destination reads repeat immediately before readiness; publication
must repeat verification again because later journal/configuration changes cannot
be locked by a completed read. Preparation itself publishes nothing.

Artifact bytes come from the native upload journal, not an editable Library
row. `OpenRunFileReceipt` repeats owner, internal-origin and producing-attempt
checks before resolving its object. `OpenReceiptStream` reads only the recorded
ETag from the configured storage account and verifies size and SHA-256 before
successful EOF. A consumer must close the stream and finish verification
before any external effect. OCI structure/platform verification is separate.
Consumers must pin every needed artifact before opening its bytes.

The native SQL journal provides these transitions across replicas:

- `preparing` records exact inputs before artifact pinning or verification.
- `ready` is written only by the trusted preparer after complete verification.
- `approved` records the owner's separate decision for exactly that candidate.
  Concurrent replicas recover the same approval identity.
- `retired` permanently prevents late verification or approval from reviving it.
  Retirement must precede releasing all consumer references, including pins
  that have not arrived yet.

A publisher records an intent for the exact candidate, approval, target and
artifact before an external write. Lost responses recover that same identity.
Only independent registry readback may complete it. An existing publication
intent fences candidate retirement, including after successful publication;
this initial ledger deliberately does not implement post-publication retention.
Deleting an uncertain intent or releasing its artifacts is not recovery.
Every authority read recomputes the stored manifest's identity and checks its
owner. Migration rollback refuses to discard candidate history.

The owner-gated approval entry reads an existing candidate by exact digest and
repeats preparation before committing separate owner approval. A changed policy,
engine revision or source configuration refuses before creating a replacement
candidate or artifact pin. Publication repeats the same verification against
the owned snapshot it will execute. The installed
`releasePublishCandidateWorkflow` composes candidate verification, durable
intent, artifact verification, destination write/readback and completion. It
selects separate OCI and file operations by the approved artifact kind. Mandatory
native prerequisites prevent another recipe from omitting these operations and
claiming success. Credentials resolve only after the approved intent exists.
The native OCI handle owns temporary verified bytes and closes on success and
failure. An uncertain write retains its intent and all candidate pins; another
host reconciles the same immutable image without selecting another effect.
A completed intent is historical publication evidence, not a promise that a
destination administrator has never subsequently removed those bytes.

The file path uses `integrations/githubrelease` to snapshot receipt-bound bytes,
publish only to the selected existing draft and verify a full independent
download. It repeats draft/tag checks and never creates, promotes, deletes or
overwrites a release or asset. A failed readback retains the same pending SQL
intent and all pins. Another engine can reconcile the same destination and
finish it without uploading a second asset. Both verified snapshot types close
on success and failure; publication cancellation includes time waiting for an
already busy handle. GitHub has no atomic upload precondition for draft/tag
state, so release controllers must coordinate promotion and tag mutation.

Database-backed tests exercise concurrent approval, changed destinations,
retirement and late calls, stored-manifest tampering, lost publication replies
and mismatched completion receipts. Real engine/journal tests cover scoped
evidence and changed receipts. Storage tests include actual Azurite version
reads and a 128 MiB journal-to-object read across independent clients.

Preparation tests use real candidate storage, the ordinary DSL interpreter,
HTTP source-version fixtures and actual OCI verification. They cover another
host with no local scope state, changed policy/engine identity, omitted native
checks, corrupted or unavailable artifacts and retirement during verification.
Artifact storage and work-evidence adapters have their own real DB/Azurite tests;
this is not yet an integrated release rehearsal.

Publication tests use the same DSL host and real SQL journal with an OCI
registry. They cover lost final replies, corrupt readback, recovery from another
host, wrong approval, changed inputs, omitted operations and temporary-file
cleanup. A separate opt-in test accepts a disposable loopback Distribution
registry; it has also verified the retained rootless BuildKit archive by its
independently recorded archive and image digests. These tests use fixture
work-evidence and artifact-storage ports, so they do not establish the complete
Workbench-to-Library-to-publication path.

The app-level opt-in proof extends that evidence through actual engine builtins,
SQL work receipts, the native upload journal and Library store, independent
Azure clients against Azurite, and a disposable Distribution registry. One
engine prepares; a second approves, discovers and publishes; the first recovers
the same persisted approval. It publishes the retained rootless BuildKit archive
against independently recorded archive and image digests. Its original work
executions and source-version HTTP service remain fixtures: it does not claim an
installed Workbench release or self-update. The test confirms deletion of its
own Azurite container and removes only its own journal rows. Process-crash
scratch recovery and post-publication retention remain unfinished.

File workflow tests use the ordinary DSL interpreter, real SQL candidate
authority and an HTTP draft-release fixture. Lost responses, corrupt readback,
second-host recovery, changed authority, omitted native verification, retained
pins and scratch cleanup are covered. Artifact/work ports are fixtures in these
tests; they make no GitHub writes.

Still required for the complete delivery path: original-receipt
verification for carried attempts, release assembly/promotion,
owner review UI, installation/recovery control, signed
provenance and complete release qualification. No candidate approval or
publication is implied by these tests.
