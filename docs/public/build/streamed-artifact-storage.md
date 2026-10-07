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

Admission counts the owner's current files, retained versions, open upload
sessions and all streaming journal entries before moving bytes. A current file
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
Physical retirement requires a separately proven producer fence and conditional
object retirement; no periodic deletion is implied by this implementation.

DSL owns artifact selection, required/optional failure handling, retries and
retention policy. Native code owns the journal, generation fencing, capacity
admission and byte verification needed to make one storage operation safe.
See [integration boundaries](integration-boundary.md).
