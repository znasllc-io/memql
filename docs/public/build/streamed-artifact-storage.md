---
title: Durable streamed artifact storage
audience: public
status: stable
area: build
sinceVersion: 0.25.0
owner: znas
---

# Durable streamed artifact storage

`pipelinesteps.StreamLibraryStore` stores one declared artifact without buffering
the whole file. It is a separate port from the byte-slice `LibraryStore`; a
runner must explicitly use it. The app adapter implements both ports. A
successful stream call returns a ready Library file and an independent durable
object receipt. A storage error never returns a successful file ID.

The Job collector and native Cockpit runner use this port from private verified snapshots;
it does not fall back to buffering an artifact for the byte-slice port. It checks
the returned owner/run/step/attempt/path, size and digest before accepting the
receipt. The Step V6 transport retains opaque intent IDs in the step outcome and
the work-step result metadata. These IDs survive outcome-size trimming even if
some editable Library links must be omitted. A successful step declaring
artifacts is refused at the driver boundary if those references are absent or
malformed. Older workbench replicas cannot accept the V6 action.

The operator can raise the total streamed archive cap to 2 GiB; its default
remains 64 MiB. The existing bounded Library phase and Job deadline still apply.
Large-file throughput must be qualified on the installation before selecting a
larger cap. Native-host artifact transport remains capped at 256 MiB. Its base64/gzip
response is decoded into a bounded private snapshot; no second full archive is
buffered. Missing declared files, malformed archives, checksum errors or a
storage adapter without verified receipts fail the step. Every declaration must
match a regular file before any artifact is uploaded. Cancellation keeps its
cancelled status and records artifact failures as notes. The command exit code
is preserved independently of artifact success.

## Identity and recovery

The caller supplies owner, work run, step, positive attempt, canonical relative
artifact path, name, MIME type, exact size and lowercase SHA-256 before upload.
The native journal keys the operation by owner/run/step/attempt/path and refuses
a retry with different metadata. It allocates the Library file and object path
once. A changed attempt intentionally represents another artifact.

Short PostgreSQL transactions serialize reservations for the same owner and
advance an upload generation. A stale generation cannot record its blob receipt
or finalize the Library row. Finalization holds the journal row lock while
writing through the engine; another replica does not depend on process-local
state or a cached Library read. These transaction-scoped locks work through
transaction-mode pooling.

The object adapter streams blocks, checks exact length and digest, commits with
a create-only condition, and hashes the stored bytes under an ETag condition.
The maximum is the smaller of the Library file limit and the verified adapter's
2 GiB bound. Existing matching bytes are adopted without reading the caller's
source again. The caller still owns and closes that source.

The journal preserves the verified ETag before writing the Library row. If an
upload or row-write reply is lost, retry the same identity. A replacement engine
verifies the object and reads the existing row instead of generating another
file. The server-only recording mutation preserves existing fields on retry;
readback must match the intended file, and the ready transition must succeed.

## Receipts and capacity

`StoredFile.Receipt` carries the intent and file IDs, owner/run/step/attempt/path,
container, object, credential-free URL, ETag, size and SHA-256. It is persisted
outside the editable Library row. Publication code must verify this object
version and digest before using it; a Library file ID alone is not immutable
release evidence. A later ETag change is refused even if the bytes hash to the
same digest.

Step results may retain just the opaque intent IDs to keep their journal and
transport records bounded. `LibraryReceiptReader.ReadRunFileReceipts` resolves
up to 1024 unique IDs in the supplied order, under an exact owner/run/step/attempt
scope. It requires internal origin and the matching owner actor; its SQL repeats
the scope checks. Missing, not-ready or mismatched entries refuse the entire
read. There is no public HTTP or DSL endpoint for this native journal accessor.
The calling capability must first authorize the run, and must still verify the
returned object version and bytes before publication. Looking up a saved receipt
is not a new byte verification.

Admission counts the owner's current files, retained versions, open upload
sessions and every unretired streaming journal entry before moving bytes. A current file
is counted only once when its ID, URL, size and digest exactly match a journal
entry. A changed or removed Library row cannot free retained object capacity.
Superseded version rows are conservatively counted separately. Interrupted
uploads keep their reservations. An uncertain commit without a verified receipt
can conservatively count both a file and its reservation until reconciliation
completes. Admission and finalization acquire the same owner lock before the
intent lock, so a file cannot disappear between the two sources of quota data.

The reservation lock currently coordinates this streaming port. Existing
browser, chunked and byte-slice upload paths do not share its admission lock;
concurrent use of those paths is not covered by a global atomic-quota claim.

## Retention boundary

An upload intent, unknown commit and its reserved capacity do not expire merely
because a process stopped responding. A previous producer may still commit
after a timeout. This port does not delete pending objects or release their
reservations on an age or lease test. It recovers by retrying the same identity.
`LibraryArtifactLifecycle` supplies the bounded reference and retirement
operations. These native ports require internal origin and the exact owner,
run, step and attempt scope. The calling workflow authorizes that scope and
chooses retention policy; there is no public journal endpoint or automatic
age-based deletion.

Before verifying or consuming artifacts, a publication pins their exact intent
set with a durable `ReferenceID`. Pinning locks the same owner and intent rows
as retirement, and succeeds only for ready receipts. Repeating the same pin is
safe; changing its artifact set is refused. A candidate spanning producing steps
uses one scoped reference per step. Release only after the consumer is durably
retired. A release is recorded even when its pin has not arrived, and the same
reference identity can never be pinned again. A delayed message cannot resurrect
that consumer.

Retirement refuses active references, advances the upload generation and
persists a random private lease identity before contacting the provider. New
reservations, finalizers, pins and ready-receipt reads refuse that intent from
then on. The provider conditionally replaces only the observed object version
or absence with a zero-byte tombstone, acquires an infinite lease with the saved
identity, and commits an empty block list under that lease. This removes staging
that raced before lease acquisition; writers without the lease cannot stage or
commit afterward. Readback must confirm the matching tombstone, infinite lease
and empty uncommitted-block inventory. Download and verification paths refuse
tombstones even when the expected artifact was itself empty.

Only that confirmed fence allows the journal to mark the intent retired and
release its reserved capacity. A lost provider or journal reply keeps the intent
recoverable with the same lease identity. Retired intent and reference records
remain permanently. Do not delete their tombstones or break their leases: doing
so would admit delayed create-only producers again. Library presentation changes
remain the workflow's responsibility; mutable rows do not determine whether a
storage fence succeeded. Provider soft-delete history and snapshots may retain
old versions, so this receipt does not claim all account storage was purged.

The provider contracts are documented in Azure's
[Put Block](https://learn.microsoft.com/en-us/rest/api/storageservices/put-block)
and [Lease Blob](https://learn.microsoft.com/en-us/rest/api/storageservices/lease-blob)
references. Local qualification uses independent clients against Azurite and
real PostgreSQL, including lost responses, staging during lease acquisition,
late writers and concurrent pin/retire operations.

DSL owns artifact selection, required/optional failure handling, retries and
retention policy. Native code owns the journal, generation fencing, capacity
admission and byte verification needed to make one storage operation safe.
See [integration boundaries](integration-boundary.md).
