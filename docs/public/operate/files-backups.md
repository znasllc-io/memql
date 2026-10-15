---
title: Files backups and storage limits
audience: public
status: stable
area: operate
sinceVersion: 0.28.11
owner: znas
---

# Files backups and storage limits

Files → Backups offers a folder sync or a one-time archive upload from a paired
machine running MemQL Cockpit. Enter the full source path on that machine.
ZIP, TAR, TAR.GZ and TGZ archives upload as they are. Folder sync uploads
individual regular files, including hidden files, and does not create an archive.

## Folder sync

Sync is one-way from the machine to Files. New or changed files create cloud
copies or versions. Confirmed local deletions move the cloud copy to the Files
Bin, where it can be restored. A failed, incomplete or unavailable scan cannot
cause deletion. A cloud edit that differs from the worker's recorded revision
stops replacement and requires resolving the conflict.

Older watched-folder arrangements keep their existing retention behavior:
local deletions mark the copy as missing at the source without moving it to the
Bin. New arrangements explicitly store `mode: "sync"`.

The source is scanned on its configured schedule while Cockpit is running.
Regular file bytes are preserved; symbolic links, special files, filesystem
permissions, extended attributes and empty directories are not a filesystem
image. Use an archive created with the metadata-preservation options you need
when those details matter.

## Storage allowances

Limits use decimal units: 1 TB is 1,000,000,000,000 bytes.

- One archive or the current contents of one synced folder: at most 1 TB.
- Managed storage for the whole cluster: at most 5 TB.
- A new ordinary user: 10 GB. Owners and developers can change a user's
  allowance on the user's page.
- Owners and developers have no personal allowance ceiling, but the shared
  cluster and per-backup ceilings still apply.

Files, saved versions, Bin contents, paused or unfinished uploads and object
metadata all count. A new upload reserves capacity before sending bytes.
Lowering an allowance preserves existing files and reservations; it refuses
new capacity beyond the limit. Moving something to the Bin does not free space.
Archive backups can be permanently purged from Backups after moving them to the
Bin. Synced files use the existing Files Bin and its restore controls.

The capacity ledger is shared in PostgreSQL. All managed Blob write paths use
it, including ordinary uploads and generated files. Requests that cannot verify
available capacity fail without starting a new storage write. Initial inventory
counts existing objects, including staged blocks. Storage writes made outside
MemQL are outside this admission system and must be restricted operationally.

## Pause, resume and download

Cockpit transfers bytes directly to Blob Storage. Closing the browser does not
stop the worker. Pause preserves verified progress and the capacity reservation;
resume obtains fresh authorization. Retries reconcile storage receipts instead
of assuming a timed-out request failed. A restarted worker resumes from its
local durable journal and the server's shared receipts.

Uploads calculate checksums before transferring. Preparation may reread the
source after a crash. Changed source identity or bytes refuse completion; the
worker must start a new transfer for changed content. Upload memory is bounded
by a 64 MiB block, independent of the total file size. Folder manifests still
use memory proportional to their file count.

A completed archive or synced file can be downloaded to an explicit destination
on a paired machine. The destination directory must exist and have separate
local download consent. Downloads verify each range and the complete SHA-256,
sync progress to disk, and publish without overwriting an existing file.
A missing or damaged partial download restarts at a verified boundary. The
destination filesystem must support hard links for safe publication.

## Machine permissions

The machine must be paired and signed in as the owning person. Configure local
`policy.yaml` before starting:

```yaml
backup:
  roots:
    - /Users/you/Backups
  download_roots:
    - /Users/you/Downloads
```

Both lists default to empty. Read permission does not grant download permission.
Local filesystem deny rules still apply. The selected destination must have
enough free disk space; a disk error preserves the last durable checkpoint.

## Storage configuration

Direct transfers require Azure account-key request signing and HTTPS. The
worker receives authorization for an exact operation, object, block, byte count,
checksum and lease generation. It receives neither an account key nor a general
write SAS. Cancellation and pause fence earlier upload authorizations before
settling or retiring their storage.

The managed account must have Blob versioning and provider soft deletion
disabled: MemQL's versions and Bin provide application retention. Hidden
provider history cannot be released from the capacity ledger safely. Writes
fail closed when history is detected or retention settings cannot be checked.
Changing provider settings does not silently reset a blocked ledger; reconcile
and inventory the account before an operator clears its recorded block.

## Validation without large transfers

Tests exercise tiny blocks through interrupted requests, lost commit replies,
pauses, restart recovery, altered source bytes, corrupted download prefixes and
existing destinations. Real PostgreSQL tests use separate service instances and
virtual byte counts at the 1 TB, 5 TB and per-user boundaries. A gated Azure
protocol test verifies exact signed requests, lease fencing and range limits
using a tiny disposable object. These tests validate protocol and accounting;
they do not claim measured terabyte throughput or a particular network speed.
