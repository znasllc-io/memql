---
title: Account export -- one account's rows and bytes in one file another instance can import
audience: internal
status: draft
area: design
sinceVersion: 0.31.35
owner: znas
---

# Account export

Owner brief, 2026-11-11 and 2026-11-12, in chat. This is the second piece of the
[accounts program](2026-11-12-accounts-program-design.md). An owner or developer
exports one account from a MemQL instance into one file, and another instance can
later import that file as its primary account or as another account. The first use
is a client's file taken from the owner's production instance.

This piece builds the export, the governed act that moves a row between accounts,
and the account page they live on. It does not build the import, which is piece 5.
Because the importer comes three pieces later, this piece has to prove a file
complete without one. An export is a copy: it removes nothing from the source
(program P15).

It stands on [account row ownership](2026-11-12-account-row-ownership-design.md):
a file holds the rows whose recorded account is the one being exported, decided
through each concept's `@belongsTo` declaration and never by a field name.

**Approvals.** The owner, in chat on 2026-11-12, answered twenty-one questions one
at a time: seven about refinements of piece 1 that they had not read, and fourteen
about this design. Each answer is a row of the table below. The owner then approved
the rest of the design as it was summarized (E22) and asked for this record. The
HTTP route in section G was approved explicitly (E17). The record is a draft until
the owner has read it.

## Locked decisions

| # | Question | Chosen |
|---|---|---|
| E1 | Rows of the 39 kinds that state their own account | The file believes the account each row states. A row that states none is in nobody's file. Those rows are counted on the Export tab and in the file's report, and stay behind until someone places them |
| E2 | Rows still waiting for an account | The run checks inside its own snapshot, asks the job for a full pass, waits, and refuses with the count if any remain. A file is never produced with a row missing |
| E3 | Which versions of a row travel | Every version, decided by the newest one |
| E4 | The cluster's own rows that an account's rows point at | Left out, and listed in the file so an import can match them |
| E5 | Concepts with no declaration | An export refuses to run while any loaded concept has none. The two-release window is entered in this push: warned from 0.32, refused at load from 0.34 |
| E6 | An instance that is not ready | This piece installs with the hold still on and changes nothing until it is released. The Export tab lists each unmet condition and offers no Export control until all hold |
| E7 | The five account lists | A list holds at most one account, the row's own. A tag means "shared with the members of its account". Tags naming other accounts are removed from every stored version. The field is dropped later, by the sign-in piece |
| E8 | Moving and placing rows | A governed act, offered in Accounts from the inventory. A row's children follow it |
| E9 | Mirrored records | Included, marked as mirrors |
| E10 | Bytes | What current rows point at: Library files with every kept version, attachments, each site's live bundle and its candidate. Attachments are counted and not carried in this piece ("Decided without asking") |
| E11 | People | Members named. An owner or developer who is not a member appears as an id only, and the request context of what they wrote is removed |
| E12 | Secret values | None leaves the cluster in this piece. The file lists the connections. Carrying secrets is designed with import |
| E13 | Website statistics | The summaries travel. The raw request log does not |
| E14 | Cluster-wide email blocks | The ones that match the account's own recipients travel |
| E15 | The container | A tar file, wrapped with age when protected |
| E16 | The passphrase | Made by the engine, shown once. Protection is on unless switched off |
| E17 | A new HTTP endpoint | `GET /accounts/exports/{id}/content`, approved |
| E18 | Who may start one | A signed-in person only. Audited, and every owner is mailed when an export starts |
| E19 | Kinds that never travel | Ten, each declared by its concept |
| E20 | The account page | Five tabs: Details, People, Data, Export, Settings |
| E21 | Starting an export | The shared wizard |
| E22 | Everything else in this record | Approved as summarized: how a run works, how completeness is proven, the declarations, followers, the smaller items in section J |

## What this changes in earlier records

| Record | Said | Now |
|---|---|---|
| Program P10 | A password is optional. With one, the portable secrets travel | Protection is on by default and can be switched off (E16). No secret value travels in this piece (E12); the format reserves the place and the import piece decides |
| Program P11 | No sign-in material travels (proposed) | Confirmed. Three kinds stay for that reason (section C). A site's access codes travel, because they are stored as digests that cannot be reversed |
| Program P12 | The request context is not exported for writes by primary users (proposed) | Extended (E11): such a person appears as an id only, and the writer's email is replaced by that id |
| Program P16 | A versioned archive following the backup's row shape (proposed) | Confirmed, with the container chosen (E15) |
| Program P17 | Data outside the row table is classified here | Classified in section B |
| Row ownership D9, section G | The lists are removed in the export piece's push, and `@rowAuthz` moves to `accountId` | Replaced by E7 and section J. Sharing by the recorded account would have opened every file, goal and composition to all members of its account |
| Row ownership D12, R17 | The deprecation-window entry is left to this piece | Entered (E5) |
| Row ownership section F | A write that commits more than a minute late is not caught, and that is accepted | Closed (section F), because E2 depends on it |
| Row ownership section C | Re-tagging a list row moves that row alone, and a tag for an account the tagger is not in is stored without moving the row | The row's children follow it, and such a tag is refused (section I) |
| Row ownership section H | The inventory counts a row under any account one of its versions names | It counts a row once, under the account its newest account-naming version names (section B, "Two readings") |

## Words

| Word | Meaning |
|---|---|
| Kind | A concept, as its rows are spoken of on screen |
| Travels, stays | A kind travels unless its concept declares `stays` (section C) |
| The engine records its account | An account-scoped kind that is not `stated`: the engine fills `accountId` and the job assigns existing rows. The row-ownership record calls it engine-managed |
| States its own account | A `stated` kind: its own mutations name the account |
| The cluster's own row | A row with no account on a `shared` kind |
| In no account | A row of an account-scoped kind that has no account and is not the cluster's own: one that states none, what follows such a row, or a conflict left out for good |
| Operator | An owner or developer who has no membership row in the account being exported |

## A. What exists today

- **The record.** Every concept declares `@belongsTo`. A row of an account-scoped
  concept records its account in `accountId`, on its newest version. The partial
  index `idx_memorynodes_account` finds the versions that name an account.
  `Concept.AccountScoped`, `AccountEngineManaged`, `SharedRows` and `ClusterWide`
  answer what a declaration means (`component/database/memory-nodes/concept_belongsto.go`).
- **Two row stores and an observation store.** `"MemoryNodes"` and
  `"SecretMemoryNodes"` hold one physical row per version. For six concepts the
  newest version can sit in `memory_liveness` instead, and the view
  `"MemoryNodesObserved"` is their union. Two account-scoped concepts are among
  the six: `v1:platform:customDomain` and `v1:pipelines:run`. Nothing routes an
  account-scoped concept to the secret table today.
- **How the row table is stored.** `"MemoryNodes"` is a hypertable on `createdAt`
  with seven-day chunks. Chunks older than ninety days are compressed, segmented
  by concept and ordered by `createdAt`
  (`20260609000000_document_version_history`). A compressed chunk has no index on
  id and none on the account: a read by id filters the concept's batches there,
  and a read by account decompresses the concept's segment. A read of one id's
  newest version written `ORDER BY "createdAt" DESC LIMIT 1` was planned as an
  ordered walk of the chunks and read a whole chunk on an installation; the shape
  that is safe matches the id with `= ANY` and sorts by the id first
  (`component/memql/executor.go`).
- **Schemas by reference.** A version's `schema` column is usually
  `{"$ref": "memql-schema:<fingerprint>"}`, and the document sits once in
  `memory_node_schemas`.
- **The whole-cluster backup** (`component/backup`). One stream of JSON lines,
  manifest first, read under one snapshot. Its promise runs one way: a newer
  engine reads every older backup. It carries rows only and writes to a file from
  a command run inside a pod.
- **Bytes.** One blob container, separated by key prefix: `library/` for Library
  files, `spaces/` for attachments, `sites/` for bundles, `packages/` for package
  outputs. The only constant-memory writer needs the size and checksum before the
  first byte and stops at 2 GiB (`CreateVerifiedStream`).
- **No large answer over gRPC.** A message is at most 32 MiB and no builtin answers
  in several messages. Library bytes leave through `GET /artifacts/{id}/content`,
  and the OS saves them to disk as they arrive through a service worker
  (`clients/os/src/apps/files/actions/downloadWorker.ts`).
- **Measured on the owner's instance, 2026-11-12, read-only.** About 0.9 million
  rows (2.9 million versions) belong to kinds whose account the engine records.
  93% are run and step journals, most written by system automations, which the
  job's last rule places in the primary account. Newest versions total about
  2.9 GB, and the largest single one seen was about 0.4 MB. Settling after the
  hold is released takes one to two days, because a step walked before its run
  has an account waits for the next daily pass.

## B. What a file holds

### The rule

For the account being exported, a file holds:

1. **The account's own row**, `v1:accounts:account:<id>`, with every version.
2. **Every row of an account-scoped kind that travels, whose newest version names
   the account**, with every version (E3). The newest version is read across the
   row table and the observation store. Both spellings of the id match.
3. **What those rows need to be understood**: the people they name, a list of what
   they point at outside the file, a list of connections, the bytes they point at,
   and the two hand-overs in "Outside the row table".

Nothing else. In particular no row of a cluster-wide kind, no row of another
account, and no row that names no account.

Mirrored kinds travel like any other (E9). The manifest marks each with its
origin, so an import lands it read-only and the connector reconciles it
afterwards.

One reader answers "which rows are this account's" for the export, the inventory
and the move act (`component/memql/account_rows.go`). It reads rows directly, so
two things are said here once. A staged concept's rows are the account's all the
same and are carried; and the capability in section H is the wall, so no per-row
admission is applied.

**Two readings.** The rule above is exact: the newest version decides, whatever
it says. Deciding it for one row costs a read of that row in every chunk (section
A), so the reader never does that once per row of an account.

- **The export** reads a kind in one statement: it finds the kind's rows whose
  newest version names the account and streams every version of them in
  `(id, createdAt)` order. Its second count is one grouped statement over the
  same kinds: rows and versions per kind, and the rows whose newest version names
  no account. Both are exact, and both cost a pass over the kind, which a run
  that takes minutes can afford.
- **The move** decides the rows a person selected, at most 500, one read each.
  A list of a kind's rows decides one page.
- **Counts on screen** (the inventory, the totals beside the assignment status)
  are read from the account index with no read per row. A row counts once, under
  the account its newest version that names an account names. That differs from
  the exact rule for one kind of row only: one whose account a later version
  removed is in no export, and is still counted on screen under the account it
  last named. A moved row is counted once, in its new account.

The reader covers the row table and the observation store. Nothing writes an
account-scoped kind to the secret table, and nothing routes a concept there by
its declaration. A change that does has to extend the reader in the same push.

### What is left out

| Left out | Why | In the file's report |
|---|---|---|
| Rows of the ten kinds declared `stays` (section C) | Sign-in material and visits, work in transit, or bound to a machine | Counted per kind, with the reason |
| The cluster's own rows of the ten `shared` kinds | They belong to the cluster (E4) | Each one a carried row points at is listed |
| Rows of cluster-wide kinds | People, sign-in, roles, nodes, configuration | People appear in `people.jsonl`; credentials and connections in `connections.jsonl` |
| Rows in no account | Nobody's (E1) | Counted per kind, as they stood on the source |
| Secret values | E12 | The names are listed |
| Bytes only an older version of a row points at, package snapshots, and attachments | E10, and "Decided without asking" for attachments | Counted, with their size where storage answers |
| Vectors, email statistics, site health | Rebuilt on the target from what travels | Named once |
| Logs, quota ledgers, release and pipeline journals, machine backups | The cluster's own machinery, or a person's own machine | Not mentioned |

Grants are cluster-wide until the sign-in piece scopes them
(row ownership R14), so what an account's groups may do is not in a file yet.

### People (E11)

People are cluster-wide and their rows never travel. `people.jsonl` has one line
per person the carried rows name:

- **Who is found.** The owner field each concept names in `@rowAuthz(owner=...)`,
  every field with an outgoing `@relationship` to `v1:identity:user`, the
  account's membership rows, and the writer of each version.
- **Members**, past and present, carry their id, name, email, the role they hold
  in the account, and whether the membership is active.
- **An operator** is an owner or developer who has no membership row in the
  account. The line is the id and the word `operator`: no name, no email.
- **Anyone else** a row names carries id, name and email.
- **The primary account's own file has no operators.** Its owners and developers
  are its people and are named.

A version written by an operator has its request context removed: the writer's
name and email, the IP address and everything derived from it, the browser, and
the serving node (`identity.displayName`, `identity.email`, `geo.*`,
`request.userAgent`, `server.*`). The email in `createdBy` is replaced by the
person's id. A version counts as an operator's when the subject it records is an
operator, or when it records no subject that resolves to a person and the role it
recorded at the time was owner or developer. Every other version's metadata
travels as stored.

No sign-in material is read at all: no passkey, token, session or link request.

### References and connections (E4, E12)

`references.jsonl` has one line per row outside the file that a carried row's
newest version points at through a declared field (`via`, or an outgoing
`@relationship`). A row the cluster provides is listed with its kind, id and
title. A row that is in another account, is of a kind that stays, or does not
exist is listed with kind and id only, which the pointing row already holds.

`connections.jsonl` lists what has to be set up again on a target:

- one line per secret a carried row names through a field declared `@secretName`:
  the name and the row, never the value and never its fingerprint;
- one line per pointed-at row of a cluster-wide kind that holds a `@secret` field
  (a source credential, for one): kind and id only;
- one line per kind that stays, with its count.

### Bytes (E10)

A field declared `@bytes` points at bytes in blob storage. For the newest version
of each carried row, the object or tree the field names is copied into the file
once, under `bytes/`, and `bytes.jsonl` records its original key, size, checksum
and the rows that use it.

A row is believed about which bytes are its own only as far as its concept
allows. The declaration names the place a row's bytes live in, built from the
row's own values (`under`, section C), and a reference that points anywhere else
is not read: the line in `bytes.jsonl` says it was refused and the report counts
it. This is the rule each of today's readers already applies to its own kind (a
Library file's key sits under its owner's prefix, a site names only its own
bundle), stated once where the export can read it.

- **Carried:** every Library file with every kept version (each version is a row
  of its own, and a binned file keeps its bytes), each site's live bundle and its
  candidate.
- **Counted, not carried:** bytes that only an older version of a row points at,
  and fields declared `@bytes(counted)`: package snapshots, and attachments,
  whose keys no declaration can tie to their rows ("Decided without asking").
- **Skipped:** a reference to files shipped inside the engine image.
- **Missing at the source.** A row can point at bytes that are gone. The line in
  `bytes.jsonl` says so, the report counts it, and the run goes on: the source
  does not hold them.

### Outside the row table (E13, E14, P17)

A component that keeps account data in tables of its own hands it over through a
registered hand-over (section C). Two exist in this piece:

- **Website statistics** (`component/sitetraffic`): the hourly and daily summaries
  and their ranked values, for the account's sites. Not the raw request log, whose
  per-visitor hashes are made with a daily secret that means nothing elsewhere.
- **Email blocks** (`component/campaigns`): for each address in the account's own
  recipient lists that is blocked cluster-wide, the address digest, the reason and
  the date, marked as coming from the cluster. Nothing about any other account's
  recipients.

## C. Declarations

The export reads declarations and nothing else. Three are new.

```memql
@belongsTo(account, via="siteId", stays="signin")   // belongs to an account, and never leaves this cluster
concept siteAccessSession { ... }

concept file {
  blobUrl  string!  @bytes(under="library/{ownerUserId}")     // one object, in its owner's place
}

concept site {
  bundleRef  string  @bytes(tree, under="sites/{id}")         // a tree of objects, in the site's own place
}

concept packageDeployment {
  snapshotArtifactId  string  @bytes(counted, under="packages/snapshots/{packageId}")   // counted in a file's report, not carried
}

concept attachment {
  blobUrl  string!  @bytes(counted, under="spaces")           // counted: nothing in the row ties the key to it
}

concept store {
  adminTokenRef  string  @secretName    // the name of a secret; the value never travels
}
```

**`stays`** is a seventh key of `@belongsTo`, legal beside `account` in any of its
forms. Its value is one of three reasons, which the file's report prints. Ten
kinds declare it (E19):

| Reason | Kinds |
|---|---|
| `signin` | `v1:platform:siteAccessSession`, `v1:platform:sitePreviewGrant`, `v1:identity:invitation` |
| `transit` | `v1:library:uploadSession`, `v1:platform:outboxEntry`, `v1:email:connectionState`, `v1:email:sendOperation` |
| `machine` | `v1:workbench:workspace`, `v1:workbench:executionBinding`, `v1:library:watchedFolder` |

Everything else travels, including the history of runs, steps and deliveries, and
work that is still running or queued. How such work lands is the import piece's
question (program P21).

**`@bytes`** and **`@secretName`** are field annotations on a string or a list of
strings. `@bytes` takes two optional flags, `tree` and `counted`, and one
required key, `under`.

**`under`** is the place a row's bytes live in: path segments, where a segment
written `{id}` stands for the row's own id and `{<field>}` for the value of one
of the row's own string fields. A reference is the row's own when its key lies
strictly inside that place, compared segment by segment.

A place has to tie the bytes to the row, so three things are held at load:

- A place has at least one placeholder. A place made of fixed words would let
  any row of the kind name any key under it.
- A placeholder names the row's id, or the field the concept's
  `@rowAuthz(owner=...)` names, which the write guard already holds to the
  writer's own id. Any other field is one its writer chooses.
- A field declared `counted` is exempt from both, because its bytes are never
  read, only sized.

An id in a key and an id on a row are the same id when they are written the same,
or when they have the same short id and neither names another concept than the
one expected: the row's own concept for `{id}`, a person for the owner field.
This is the comparison `core/librarykey` makes for a Library key.

A bare top-level place such as `library` cannot be named by a tree. The blob
reader adds its own floor whatever a declaration says: it reads nothing outside
the configured container, nothing under `exports/`, and no tree shallower than
two segments.

**A `@secret` field never travels.** The value is removed from every carried
version, and the report names the field. A build gate keeps the engine's own
tree, its packs and the fleet bundle free of `@secret` fields on kinds that
travel.

**Hand-overs.** `accountexport.RegisterHandOver(name, fn)` is called from an
`init()` by the component that owns the tables. The function receives the run's
snapshot and the ids of the carried rows of the kinds it names, and writes its
own members.

**Checked at load**

| Check | Refusal |
|---|---|
| `stays` beside `cluster` | `belongs_to_conflict` |
| `stays` with a value outside the three | `belongs_to_stays_unknown` |
| A kind that travels follows, through `via`, a kind that stays | `belongs_to_via_stays` |
| `@bytes` or `@secretName` on a field that is not a string or a list of strings | `field_annotation_type` |
| `@bytes` with no `under`, an empty or relative segment, a place with no placeholder, or a placeholder that names neither the row's id nor the concept's owner field (a `counted` field may have no placeholder, and may name any string field) | `bytes_under` |

Each has its cells in the conformance corpus.

**Concepts with no declaration (E5)**

- **An export refuses** while any registered concept outside the engine's own
  tree declares nothing. The refusal names each one.
- **The window.** `concept_belongs_to_undeclared` joins the registry in
  `component/language/deprecation` with `DeprecatedIn: "0.32.0"`, so a mounted
  bundle or a pack with such a concept stops loading from 0.34. Until then a
  load warns once, naming all of them, as it does today, and each is counted
  under the rule on `memql_dsl_deprecated_uses_total`. The warning stays off the
  load report, for the reason row ownership R3 gives. Only a release-stamped
  build refuses, as for every deprecated form.
- **A concept authored at runtime** must declare from this piece on. Promotion
  refuses one that does not, and runs the cross-concept checks a load runs.
  Nothing outside this repository can have written one, so it needs no window.

## D. The file

A tar file, read and written as a stream. Unpacked, it is one folder:

```
acme-2026-11-12.memql-account/
  manifest.json                    first: what this is, from where, when, format version
  account.jsonl                    the account's own row, every version
  rows/<namespace>/<name>/part-000001.jsonl.gz
  schemas/<fingerprint>.json       each schema a version was written under, once
  people.jsonl
  references.jsonl
  connections.jsonl
  blocks.jsonl                     hand-over: email blocks
  tables/site-statistics/...       hand-over: website statistics
  bytes/000001 ...                 bytes exactly as stored
  bytes.jsonl
  report.json                      counts expected and written; left out, by reason
  README.txt                       plain words, written from the report
  SHA256SUMS                       last: one line per member
```

- **The manifest is first** and carries the format version, so a reader can refuse
  before reading anything else. It also carries the engine release, the snapshot
  instant, the source instance's domain and its `MEMQL_CLUSTER_ID` where one is
  set, the account's id and name, whether it is the primary account, whether the
  file is protected, the declaration of every kind in the file (its `@belongsTo`
  form, and `@origin` or `@mirroredTo` where it has one, so an import knows a
  mirror from an origin), the language line of every mounted DSL domain with its
  content hash where the engine holds one, the payload-repairing migrations
  applied at that instant (program P22), and the embedder in use.
- **A row line** follows the backup's row shape: kind, concept, id, `createdAt`,
  `createdBy`, type, schema, payload, metadata, provenance. JSON columns travel
  as raw JSON, untouched, with the two exceptions section B names. A version
  held in the observation store is marked `observed`. Lines are ordered by id,
  then `createdAt`, so each row's history reads oldest first.
- **Parts.** A tar member needs its size before its bytes, so a kind's lines are
  cut into parts of bounded size, each a complete gzip member of whole lines. A
  part is buffered in memory; nothing is spooled to disk.
- **Schemas.** A line keeps the schema reference it was stored with, and each
  referenced document is in the file once.
- **`SHA256SUMS` is last**, in the format `sha256sum -c` reads. A file without it
  is cut short.
- **Deterministic.** Two files of an unchanged account differ only in the
  manifest, the report and the README.

**Protected (E15, E16).** The same tar stream is wrapped with
[age](https://age-encryption.org) in passphrase mode, and the file is named
`.tar.age`. A person opens it with `age --decrypt` and then `tar -x`. age is a
new dependency of the engine (`filippo.io/age`).

**The promise.** `formatVersion` is 1. A newer engine reads every older file; an
older engine refuses a newer one outright. The repository keeps a sample file of
each format version under `component/accountexport/testdata`, and a test checks
every one of them on the current engine.

`component/accountexport` holds the format and its checker and reads no database:
the manifest, the line shape, the part writer, the checksum list, and
`Verify(reader)`, which walks a file and checks its structure, every member
against the checksum list, and the report's counts against the lines. The run
uses it to check its own file. The import piece will open files with it.

## E. How completeness is proven

1. **One instant.** Every line comes from one snapshot of the database.
2. **Counted twice.** Inside that snapshot a separate statement counts the rows
   and versions of each kind for the account. The writer counts what it wrote.
   Any difference fails the run, and there is no file.
3. **Nothing waiting.** Section F, "Caught up".
4. **Nothing invisible.** No concept is undeclared (section C).
5. **Nothing foreign.** A line is written only for a row the reader of section B
   returned. A test writes a row in each of two accounts for every account-scoped
   kind in the registry and checks that one account's file holds its row and not
   the other's; a kind added later is covered with no change to the test.
6. **Left out is said.** The report gives every exclusion with its reason and
   count.
7. **Read back.** Before a file counts as complete the cluster reads it back from
   storage, opens it with the passphrase when it is protected, and runs the
   checker over it.
8. **Checkable by anyone.** `sha256sum -c SHA256SUMS` checks every member with a
   stock tool. The checksum of the whole file is shown beside the download.

## F. Readiness and the run

### Ready (E6)

`accountExportReadiness` answers five conditions for an account, each with a
count. The Export tab shows them, and the run checks them again when it starts.

| Condition | Read from |
|---|---|
| Recording is on, and the node that answers is not held | `account_stamping_storage`, `MEMQL_ACCOUNT_STAMPING_ACTIVATE` |
| The first assignment has finished for every kind the engine records | `account_assignment_progress` |
| Every loaded concept declares | the concept registry |
| No row awaiting a decision names this account | `account_assignment_conflict` |
| No row is still following a moved parent into or out of this account | `account_follow_tasks` (section I) |

It also reports, without blocking, how many rows of account-scoped kinds are in
no account.

### Caught up (E2)

A file is produced only when no row is waiting for its account at the snapshot's
instant. Three changes to the assignment job make that checkable.

- **Late commits are no longer missed.** A later pass does not advance past the
  start of the oldest transaction still open on the database, less the existing
  slack. A row whose transaction commits late is then still ahead of the cursor
  when it becomes visible.
- **A full pass on request.** The run can ask for a full pass of a kind that has
  rows waiting, and the job honors it at once instead of a day later.
- **Conflicts record their candidates.** A row the job will not guess at records
  the accounts it could belong to, so "names this account" is a lookup. A
  conflict recorded before this piece gains its candidates on the job's next
  pass.

The run notes the time, waits until every kind is verified through that time with
nothing waiting, asking for full passes where needed, and opens its snapshot.
Inside the snapshot it looks at the versions written since that time, on kinds
the engine records, and judges them with the job's own rules. If any is the
newest version of a row the job would still assign, the run releases the
snapshot and repeats. After ten minutes it fails with the count per kind. Rows
the job leaves alone on purpose (the cluster's own, rows that state no account
and what follows them, recorded conflicts) are not waiting.

### The run

A person prepares and starts an export (section H). An agent node runs it.

| Phase | What happens |
|---|---|
| Checking | The five conditions, then "Caught up" |
| Rows | One read-only snapshot on a maintenance session: the count, the account's row, each kind in name order, schemas, people, references, connections, hand-overs |
| Bytes | After the snapshot is released: each object or tree the rows named |
| Sealing | The report, the README, the checksum list; the object is committed |
| Verifying | The file is read back and checked (section E, point 7) |

- **A stream end to end.** Lines go from the database through gzip, the tar
  writer and age into staged blocks in blob storage. Memory holds one part and
  one block. The snapshot is released before any byte of a file is copied. The
  rows phase is limited to sixty minutes, so a stalled upload cannot hold the
  snapshot open.
- **Where it is stored.** `exports/accounts/<exportId>/` in the cluster's
  container, written by a narrow writer of its own in `integrations/azureblob`.
  It is the cluster's storage and counts against no person's allowance.
- **One at a time.** The cluster runs one export at a time. An account has at
  most one that is prepared, queued or running. Others wait and say so.
- **If the node dies.** The run heartbeats every twenty seconds. One whose
  heartbeat is two minutes old is taken by another agent replica and started
  again from the beginning, three attempts at most. A snapshot cannot be handed
  over, so nothing is resumed part-way.
- **Cancel** is offered throughout. It stops the run and removes what was staged.
- **Kept until deleted.** A complete file stays until a person removes it. The
  record of the export stays for good, with who started, downloaded and removed
  it.

### The record

`account_exports` is a table of the cluster's own, not a row in the graph: an
export of one account must never be part of another account's data, a heartbeat
must not write a version, and the sealed passphrase must leave no history. A row
holds the account, who asked and when, state, phase and progress counts, the
attempt, the claiming node and its heartbeat, whether the file is protected, the
object's key, size and checksum, the counts from the report, what stopped a
failed run, and who removed the file.

### The passphrase (E16)

- **Prepared, then started.** `accountExportPrepare` creates the record as a
  draft. When protection is on it generates the passphrase, 25 characters in
  five groups from a 32-character alphabet, seals it under the cluster's key on
  the record, and returns it once. `accountExportStart` queues the run. A draft
  not started within fifteen minutes is removed.
- **Erased.** The sealed passphrase is cleared when the run reaches any final
  state, and from any draft that is removed. After that the cluster cannot open
  its own protected file.
- **Never logged, never in a run.** It is returned to one caller once and is in
  no log line, audit detail, journal or event.

## G. Getting the file out (E17)

`GET /accounts/exports/{id}/content`, on the bff. Approved by the owner on
2026-11-12 as an exception to the gRPC-first policy, because a browser can only
save bytes that arrive over HTTP and the stream carries no large answer.

- **An ordinary authenticated route**, declared by `server.AccountExportPaths()`
  so the front door's generated path list carries it.
- **Who.** A signed-in person (section H) who holds `execute app:accounts/export`.
  Every refusal answers 404.
- **How.** The bytes are streamed through the bff from blob storage, with a single
  `Range` honored, never a redirect and never a signed link. It uses the Library
  route's download deadlines (`core/httpstream`).
- **Only a complete file** that has not been removed.
- **Audited** on every request, with the range asked for.
- **From the OS** the page fetches `/_memql/accounts/exports/{id}/content` with
  its bearer and hands the body to the download worker, as Files does. The edge
  gives this path the long-download treatment it gives the Library's.

The row for the root `CLAUDE.md` table of allowed HTTP exceptions is written in
this push.

## H. Who may (E18)

- **Two parts**, seeded on owner and developer only: `execute app:accounts/export`
  for everything about exports, and `execute app:accounts/move` for listing an
  account's rows and for moving and placing them. The engine refuses every other
  caller before a handler runs.
- **A signed-in person only.** Preparing, starting, cancelling, downloading and
  removing an export, and moving or placing rows, need a person's own browser
  session. A personal access token, a service account, an app session, a worker
  token, the operator credential, delegated access and any call made inside a run
  are refused (`person_session_required`). The check moves out of
  `component/identity/roletest` into `component/auth`, so role testing and this
  piece share one.
- **Audited.** `v1:identity:auditEvent`, category `data`: an export prepared,
  started, completed, failed, cancelled, downloaded, removed; rows moved; a row
  placed; a conflict left out. Two target types join the closed enum,
  `accountExport` and `account`.
- **Owners are mailed.** When an export is started, every active owner gets an
  email saying who started it and for which account, with no link in it. A
  cluster that cannot send mail still starts the export, and the audit trail
  holds the record.

**The calls.** All are builtins of the `accounts` plug-in, declared in
`dsl/accounts/builtins.memql` and served by every node type, as the inventory's
are. The runner that works exports is wired on agent nodes only.

| Call | Part | Does |
|---|---|---|
| `accountExportReadiness` | export | The five conditions of section F for one account, and the count of rows in no account |
| `accountExportPrepare` | export | Creates a draft and, when protected, returns the passphrase once |
| `accountExportStart` | export | Queues a draft |
| `accountExports`, `accountExportStatus` | export | An account's exports, paged; one export's state, phase and counts |
| `accountExportCancel`, `accountExportRemove` | export | Stops a run; deletes a complete file and keeps the record |
| `accountRows` | move | The rows of one kind in an account, or in no account, paged: id, the kind's `@displayCard` fields, owner, dates |
| `accountRowsMove` | move | Moves or places rows. Asked to preview, it answers what would move with them, by kind, and writes nothing |
| `accountConflictLeaveOut` | move | Records that a row awaiting a decision stays in no account |
| `accountMoves` | move | What was moved, by whom and when, paged |

## I. Moving and placing rows (E8)

### The act

`accountRowsMove` takes rows and a destination account.

- **Moves** a row of a kind whose account the engine records, from the account it
  is in to another.
- **Places** a row of any account-scoped kind that has no account.
- **Refuses** a row of a cluster-wide kind; a row that follows a parent in the
  same account (move the parent); a row of a kind that states its own account
  and already has one, which is changed in its own app under the organization
  rules, as today; and a destination that is unknown or the same.
- **In place.** Like the assignment, it writes the account on the newest version
  and on the observation copy, appends no version, publishes no event and runs
  no automation. It invalidates the kind's cached reads.
- **Recorded.** One line per moved root in `account_row_moves` (from, to, who,
  when, how many followed, a note), and an audit event.

### Followers follow

Today nothing carries a row's children when its account changes: a goal moves and
its runs stay behind. From this piece, whenever a row's recorded account changes,
by the act, by a re-tag of a list row, or by an organization change in its own
app, the engine records a follow task. The assignment runner works tasks off in
batches: every row whose `via` field names the changed row, and whose own account
is the one the row left, takes the new one, and so on down the chain. Placing a
row that states its own account also places the rows the organization rules tie
to it, where they have none.

A row attached to a parent from another account stays where it is. Row ownership
allows that on purpose, and it is why a follower is recognized by the account it
shares with its parent, not by the link alone.

An export waits while a task is pending for its account.

### Rows awaiting a decision

`store_bound_twice` and `library_fence_refused` rows are listed on the primary
account's Data tab. Each is resolved by placing it in an account, which carries
what follows it, or by leaving it out for good, which records the decision and
leaves the row in no account. Exports of the accounts it names wait until then.

### The account lists (E7)

For the five kinds that carry `accountIds` (Library items, work goals,
compositions, compose templates, recipes), once recording is on:

- **A stored list is empty or holds the row's own account.** Empty means private
  to its owner. One entry means shared with the members of that account.
- **A write naming more than one account is refused** (`account_list_single`).
- **A write naming an account the caller may not write to is refused**
  (`organization_forbidden`). Until now such a tag was stored, and the row was
  filed under the caller's own account.
- **A write naming one account the caller may write to** files the row there, as
  it does today, and its followers follow.
- **Existing rows.** The job rewrites a newest version whose list holds anything
  else: the row's own account if the list named it, otherwise empty. It trims
  the same way in every older version that names another account, so no file
  carries another account's id through a list.
- **Nothing else changes.** `@rowAuthz(account="accountIds")` keeps reading the
  list, the mutations keep taking one, and the OS pickers are already single.
  Nobody sees more than before. A row that was tagged for several accounts stays
  shared with one (program P3).

While recording is off, the lists behave as they did.

## J. Smaller items

- **`campaigns.sendJob.campaignAccountId`.** Nothing writes it after this push.
  Its readers take `accountId` and fall back to the campaign's own account, which
  covers rows written before and an instance with the hold on. The field stays
  declared and is dropped in the next piece's push. A field can only be dropped
  one release after its last writer stops, and piece 1 kept the writers.
- **`accountIds`.** The field is dropped by the sign-in piece, which decides what
  sharing inside an account looks like. Its last writer stops then, and the strip
  follows one release later.
- **The inventory** counts through the reader of section B ("Two readings"), so
  a moved row is counted once, in its new account.
- **Load on scroll** (program P40) arrives in the kit as one control, used by this
  piece's lists: it loads the next page as the end comes into view, holds its
  place while loading, offers a retry in place after a failure, and states the
  end once. The shell piece moves the other screens to it.

## K. Screens

Built with the kit and the rules in `clients/os/DESIGN.md`. Every control below is
absent, not disabled, for a person who does not hold its part.

**The account page** (E20) takes the Fleet machine page's shape: `LocalTabs`
under the page heading, the trail reading `Accounts > <name> > <tab>`, each tab
keeping its own scroll position.

| Tab | Holds |
|---|---|
| Details | The account's facts, read-only, in `DetailsSection` and `Facts`: profile, domain and its proof, front door, status. Quiet copy on values worth pasting |
| People | Groups, members and guests, as today |
| Data | What the account holds, by area, from the inventory. An area opens to its kinds, and a kind opens to its rows as a page under the trail. On the primary account: how the assignment is going, rows awaiting a decision, and rows in no account |
| Export | Readiness, or the list of exports |
| Settings | Every edit: profile, domains, joining, and Archive |

**Rows of a kind.** A flat `RecordList`, titled by the kind's `@displayCard`,
loading on scroll, with `Refine` for search. Selecting rows brings one action bar
with one act, Move to account, which opens the wizard "Move rows": Destination,
Review (what moves with them, by kind), Move, Done.

**The Export tab**

- **Not ready.** The five conditions as facts, each with its state in words and
  its count. No Add control.
- **Ready.** A heading "Exports" with the Add control, and a flat list: when,
  state, size, whether protected, who started it. A complete export offers
  Download and a trash control that asks in place. A running one shows its phase
  and count, and opens the wizard where it stands. The list refreshes itself
  while a run is active and loads on scroll.
- **Below it**, one quiet line with the count of rows in no account, and an arrow
  to where they are placed.

**"Export this account"**, the kit wizard (E21):

| Step | Shows |
|---|---|
| Contents | Kinds, rows and file bytes from the inventory, and what is left out |
| Protection | One switch, on. Switching it off asks to confirm in the bar |
| Passphrase | Only when protected. Shown once, with Copy. Back is held, and the bar's act is absent until "I have saved it" is ticked |
| Export | The cluster's turn: phase, count and elapsed time in the bar. Leave keeps it running; Cancel asks first |
| Download | The file's name, size and checksum, anything the report flags, and Download |

States follow the rules: loading is the shape of the content, a refusal says so in
the engine's words and keeps saying so while it retries, an empty list is drawn
only after a read that settled empty, and a running export is progress, not a
skeleton.

Verified with tests under `clients/os/test/accounts/` and judged rendered, in
both modes, wide and narrow, empty and populated, before it is called done.

## L. Refusals and signals

| Signal | When |
|---|---|
| `export_not_ready` | A condition of section F does not hold; the answer names it |
| `export_in_progress` | The account already has an export prepared, queued or running |
| `export_rows_waiting` | "Caught up" timed out; with the count per kind |
| `export_census_mismatch` | The two counts differ |
| `export_verify_failed` | The file read back does not check |
| `person_session_required` | The caller is not a person's own browser session |
| `account_list_single` | A list write names more than one account |
| `move_refused` | With the reason: cluster-wide, follows a parent, states its own account, destination |
| `belongs_to_stays_unknown`, `belongs_to_via_stays`, `field_annotation_type` | A declaration is refused at load |
| `belongs_to_undeclared` | From 0.34, a mounted concept with no declaration |
| `memql_account_exports_total{outcome}` | Exports by outcome |
| `memql_account_follow_tasks` | Follow tasks pending |
| `memql_dsl_deprecated_uses_total{rule="concept_belongs_to_undeclared"}` | Undeclared mounted concepts, per load |

## M. Testing

**Build gates**

- The three declarations have their cells in the conformance corpus, accepted and
  refused, each refusal with its code and message.
- No kind that travels in `dsl/`, `packs/` or `deploy/fleet/dsl` declares a
  `@secret` field.
- The published deprecation table matches the registry, as its existing gate
  requires.
- Every sample file under `component/accountexport/testdata` checks on the
  current engine.

**Against a real database**

- **The reader:** a row whose older version names another account, a row whose
  newest version sits in the observation store, both spellings of the id, a row
  with no account, a staged concept.
- **The sweep:** for every account-scoped kind in the registry, a row in each of
  two accounts; one account's file holds its row and not the other's, and a kind
  that stays is in neither.
- **The file:** exact members for a fixture account, the counts, the checksums,
  two runs byte-identical in their row parts, a protected file that opens with
  its passphrase and not without.
- **People:** each of the four kinds of person, an operator's version with its
  context removed, the primary account's own file.
- **Bytes:** an object, a tree, a counted field, a missing object.
- **Caught up:** a waiting row holds a file back and the full pass releases it; a
  late commit is found; the timeout names the kinds.
- **The run:** each failure leaves no file; a cancelled run leaves nothing
  staged; the passphrase is cleared at every final state.
- **Followers:** a moved goal carries its runs and steps; a row attached from
  another account stays; a re-tag carries followers; an export waits for a
  pending task.
- **The lists:** each of the write rules, the job's rewrite, older versions
  trimmed, and nothing changed while recording is off.

**Across nodes.** The record is the only state a run needs, and it is in the
database for that reason. One test starts a run on one replica, stops it and has
a second replica take it over, in one process against a real table. Another
prepares on a node that is not an agent and reads the status from a third.

**The route:** the capability, the person check, a range, a removed file, and the
front door's generated list carrying the path.

**The OS:** each tab, each state of the Export tab, each wizard step, the
passphrase shown once, and every control absent without its part.

## N. Delivery

One push to `main` (program P6), built on a branch and verified locally with the
database-backed lanes. The order inside it:

1. The declarations, their checks and cells, and the ten `stays` declarations.
2. The reader, and the inventory moved onto it.
3. The job's changes, the follow tasks and the list rule.
4. The move act and its builtins.
5. The format, its checker and its sample file.
6. The run, the record, the blob writer and the route.
7. The account page, the Data tab, the Export tab and both wizards.
8. The documents: a public page `docs/public/operate/account-export.md`, the
   language reference and its deprecation table, the authoring rules,
   `dsl/_reference/_concept.memql`, the pack-building guide,
   `organization-ownership.md`, `clients/os/DESIGN.md` for load on scroll, the
   root `CLAUDE.md`, and the program record.

**On the owner's instance, by hand, not part of the push:**

1. Install this piece with the hold on. It changes nothing.
2. Declare the eight concepts production loads with no declaration: seven in the
   `memql-znas` bundle and one in `memql-fylo`.
3. Back up the database.
4. Release the hold. This is one-way.
5. Wait for the assignment to finish. The Export tab shows it.
6. Place the rows that await a decision and the rows in no account that belong
   somewhere. Move the work done for a client under an operator's own name.
7. Export.

Installing before releasing the hold matters: it means no list row is re-tagged
under piece 1's rule, which moves a row without its children.

## O. Known limits, and what is not verified

**Limits**

- **No importer exists.** A file is proven complete by the checks in section E,
  not by loading it.
- **Journals written before recording was on stay in the primary account.** Runs
  and steps of system automations have no owner, so the job places them there,
  including those that ran on a client's rows. A client's file does not hold
  them, and the primary account's file is mostly them.
- **A client's file is thin until the assignment has finished**, which takes one
  to two days after the hold is released.
- **Work an operator did for a client under their own name** stays in the primary
  account until someone moves it.
- **Grants do not travel** until the sign-in piece scopes them.
- **No secret value travels.** Every connection is set up again on the target.
- **A person named only by an undeclared field** is not in `people.jsonl`. The id
  is still in the row.
- **What a payload says is not policed.** A note that mentions another account,
  or the reuse evidence on a learned procedure, which lists account ids, travels
  as written.
- **Work in flight travels as it stands.** Landing it safely is the import
  piece's job.
- **A file taken before a privacy erasure still holds what was erased.** Whoever
  holds the file is responsible for it. The record shows who downloaded it.
- **A run restarts from the beginning** when its node dies.
- **History that retention archived or deleted** is not in the database and so
  not in a file.
- **A deleted row travels when its deletion is a version that still names the
  account**, which is how the DSL's kinds delete. A deletion marker that names no
  account leaves its row in no account.

**Not verified**

- How long a run takes, and how large a file is, for an account of real size.
- That the snapshot's hold on vacuum is acceptable for the largest account.
- A rolling deployment with a run in progress.
- The settling time after release, which is another session's measurement.
- The screens, which are described here and not drawn.

## Decided without asking

On 2026-11-12, after this record was written, the owner asked for the piece to be
built end to end with no further questions. These decisions were taken on the
owner's behalf from then on, each by the option this record's author would have
recommended. They are listed so the owner can read and overturn any of them.

| Decision | Reason |
|---|---|
| The download route of section G is built | The owner approved it by name while this design was settled (E17) |
| A run waits ten minutes for rows to get their account, its rows phase may take sixty minutes, a dead run is restarted three times at most, and a prepared export that is not started within fifteen minutes is removed | Each needs a number, and these fail safe: a run that stops says why and can be started again |
| The report's count of rows in no account is exact for every kind, taken by the same statement that counts the file's rows | The export reads each kind in one pass anyway (section B, "Two readings"), so the exact count costs nothing more |
| The reader has two readings of one rule: the export and the move decide by a row's newest version; counts on screen are read from the account index and decide by the newest version that names an account | Deciding the exact rule for every row of an account costs a read per row in every chunk of the row table, and the oldest chunks are compressed. On the owner's installation that is minutes to hours for a count a person is waiting for. The two differ only for a row whose account a later version removed |
| A row awaiting a decision can be left out for good, and that is recorded on the conflict with who decided | Section I needs a second way to resolve a conflict that cannot be placed |
| The move's history (`accountMoves`) has a call and no screen | The audit trail already shows it, and no screen was designed for it |
| The reuse evidence on a learned procedure keeps listing account ids | Changing it reaches into procedure learning, outside this piece; it is a known limit in section O |
| `@bytes` takes a required `under`, and a reference outside it is refused, not read | A row names its own bytes, and the export must not read whatever a row names. Each existing reader already holds its own kind to such a place |
| A place needs a placeholder, a placeholder names the row's id or its owner field, and ids are compared as `core/librarykey` compares them | A place that does not tie the bytes to the row is no place at all |
| Attachment bytes are counted in a file's report and not carried. This departs from E10's list | An attachment's key holds a partition and a digest of the run that made it, and neither is a field of the row, so no declaration can tie the key to the row. No bundle of the three installations at hand can create an attachment row, so nothing is lost today; carrying them needs the key recorded on the row first |
| An export may not carry a row larger than 64 MiB in one version, and its checker bounds every member it reads | The checker will read files from outside once import exists; the writer refuses what the checker would |
| A deleted row is decided like any other: by its newest version, which is its deletion | The reader has one rule, and a kind that deletes by an update keeps its account on that version |
| The spec and the plan were not reviewed by the owner before the build | The owner's instruction |

## P. Out of scope

- Import, top-ups, and anything that reads a file other than the checker.
- Carrying secret values.
- Removing an exported account from its source.
- An expiry on files, incremental exports, and a command-line path.
- Dropping the `accountIds` field.
- Any change to who can read a row, other than the two list refusals in section I.
- A personal "download my data".
