---
title: Library upload identity recovery and role checks
audience: internal
status: draft
area: ops
sinceVersion: 0.28.12
owner: znas
---

# Library upload identity recovery and role checks

An upload can appear in Files even when object storage rejected its bytes.
The Library keeps the failed file row for diagnosis, and its artifact index
can still be promoted. Opening that artifact reaches the editor, but its
content request cannot download the intended storage key.

## Recognize the identity mismatch

Correlate the file ID across the BFF upload log, the latest
`v1:library:file` row, the artifact's `sourceConceptRef`, and the subsequent
content request. The specific failure repaired here has all of these facts:

- Upload log: `storage destination belongs to another user`.
- File row: `status=failed`, a failure reason saying bytes were not stored,
  and `blobUrl` containing the intended `library/...` key instead of a URL.
- Content log: `not an azure blob URL (no downloadable object)`.

The storage capacity gate compared a bare caller ID with a canonical
`v1:identity:user:<id>` path owner. Those spellings refer to the same person.
The inventory also charged canonical IDs separately from the bare IDs used
for usage reads. This is independent of the person's role. Another user's
ID must still be refused, even when its suffix matches a different concept.

Storage accounting now normalizes only the identity:user prefix at admission,
inventory, usage and allowance writes. Object keys and URLs remain unchanged.
Migration `20261015190000_storage_owner_identity` merges existing user totals,
reservations and allowances; it preserves the cluster totals and every stored
object. When two allowances exist, the more recently updated one wins.

The editor and Files distinguish missing bytes from failed analysis: a failed
analysis with a real stored URL remains readable. Editor diagnostics retain
the error message even when a browser supplies a frame-only stack.

## Release and recovery

1. Build the engine and its embedded OS/editor assets through the normal
   GitHub release workflow. Pin the release in the installation repository;
   there is no product-specific Files or role implementation to patch.
2. Drain old storage writers before the accounting migration and keep uploads,
   backup writers and other storage-writing jobs paused until all such replicas
   run the corrected engine. An old writer can recreate canonical accounting
   entries or settle an in-flight reservation under the old owner spelling.
   Table locks make the migration atomic; they do not prevent a superseded
   binary from writing after the transaction completes.
3. Verify that no `storage_capacity_objects.owner_id`,
   `storage_capacity_users.owner_id`, or user total scope retains the canonical
   identity prefix. Compare the cluster's used/reserved totals before and after
   the migration; neither should change because of this repair.
4. Resume uploads and verify a new Markdown upload, browser-editor open,
   authorized save, reopen and download. Exercise requests through different
   BFF replicas. Compare downloaded bytes with the original.
5. Ask affected people to upload their original files again. A failed row is
   not a backup of its contents. Do not mark it ready, invent an object URL,
   or delete it as part of the accounting repair.

The down migration intentionally does not split merged identities. A rollback
to the affected engine requires keeping storage writers paused and a reviewed
recovery plan; it is not made safe by running the no-op down migration.

## Role audit boundaries

The seeded model is in `dsl/rbac/seeds.memql` and
`component/auth/rbac_model.go`. `reader` aliases Viewer and `writer` aliases
Member. Rank orders governance targets; capabilities decide the operation.
Developer ranks above Admin but does not inherit Admin's people-management
capabilities.

| Operation | Viewer | Member | Admin | Developer | Owner |
|---|---|---|---|---|---|
| Open Files and read authorized Markdown | Yes | Yes | Yes | Yes | Yes |
| Manage own files, folders, reviews and history | Yes | Yes | Yes | Yes | Yes |
| Start private Ask conversations and submit requests | Yes | Yes | Yes | Yes | Yes |
| Ordinary shared-data mutations | No | Yes | Yes | Yes | Yes |
| Campaign/audience creation | No by base role | Subject to organization scope | Subject to scope/grants | Subject to scope/grants | Subject to scope/grants |
| Manage people, groups and other people's sessions | No | No | Yes, within governance rules | No | Yes, within governance rules |
| Author constructs | No | No | No | Yes | Yes |
| Execute deployment | No | No | Yes | Yes | Yes |

This table describes base grants, not a bypass of row ownership, organization
membership, explicit grants/denials, token-class restrictions or governance
rules. The effective-capabilities reply and the server's target-specific
decision remain authoritative. Opening an app alone is not authorization for
every action in it.

The People page now consults `update principal` before reading sessions or
offering principal management, and `update group` before group changes.
It no longer sends the session query merely because Developer outranks the
person being viewed. The server's existing denial remains in place.

Viewer is read-only for shared business data, with personal Files and Ask
functionality. The transport admits an exact set of core owner-scoped folder,
file-organizing and conversation-creation mutations. It does not grant
`create data`, permit raw inserts, accept arbitrary Library mutations, or
expose server-owned transcript/version writes. The personal exception requires
a signed-in ranked user and effective read access to data and the app.
Document edit, feedback, history and revision handlers retain their existing
ownership, author, version and approval checks. The Library byte routes already
admit the human owner, including delegated `app_session` credentials.

Ask's action runner applies the same personal/shared-data distinction before
execution. Organization actions still use their target-specific grants and
membership checks. A prompt cannot grant campaign/audience creation, and the
assistant cannot forge conversation history.

One additional boundary needs acceptance testing when changing a person's role:

- **Existing sessions:** the stream resolves the user on connection and
  reloads on successful authentication rotation. HTTP handlers derive their
  actor from verified token claims. Refocusing OS refreshes effective grants
  but does not itself rotate the stream's identity. After a role change,
  obtain fresh credentials and reconnect both OS and editor before verifying
  the new role. Immediate promotion/demotion propagation in already-open tabs
  is not established by the storage tests.

## Validation

Viewer tests exercise folder creation/rename/archive/restore and conversation
creation through the browser request handler on two independent engines; they
refuse foreign folders, raw writes, business writes and forged transcripts.
Document tests cover Viewer editing, comments, history forks, note removal and
approval/application across replicas. Ask tests cover private histories, turn
retries and personal-versus-business action permissions.

The regression coverage also includes bare/canonical caller and path combinations
for every seeded role and alias; rejection of another identity; inventory
normalization; an idempotent repair with existing usage/reservations; and a
second database pool continuing a repaired object's operation. The database
tests must run with `MEMQL_REQUIRE_DB=1` and a reachable PostgreSQL instance.

Files and People interaction tests cover failed-upload recovery, readable
stored files whose analysis failed, Developer's restricted controls and
Admin/Owner session access. Productivity tests cover the missing-byte refusal
before download and frame-only browser diagnostics. Review the real components
at wide/narrow sizes in both themes using fixtures; this is not a claim of a
production browser login under each role.
