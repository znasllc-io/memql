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
workflow, not an alternative workflow or a public approval API. The current
methods are private and are not registered as DSL capabilities.

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
It refuses skipped work, failed or unfinished runs, cancellation, replay,
unknown execution definitions and missing or nonzero exit codes. It verifies
source, attempt, execution definition and the full recorded call/result. A
step ID alone is mutable; the receipt digest also binds its recorded version.
This does not prove complete release coverage: the sealed DSL must select the
required evidence and enforce its complete policy before preparing a candidate.

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

Database-backed tests exercise concurrent approval, changed destinations,
retirement and late calls, stored-manifest tampering, lost publication replies
and mismatched completion receipts. Real engine/journal tests cover scoped
evidence and changed receipts. Storage tests include actual Azurite version
reads and a 128 MiB journal-to-object read across independent clients.

Still required before exposing this path: sealed workflow selection and full
coverage validation, trusted target resolution, durable artifact pinning,
native OCI publication wiring, owner review UI, installation/recovery control,
signed provenance and complete release qualification. No candidate approval or
publication is implied by these primitive tests.
