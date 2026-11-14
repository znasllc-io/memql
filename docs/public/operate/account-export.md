---
title: Account export -- one account's rows and bytes in one file
audience: public
status: stable
area: operate
sinceVersion: 0.31.37
owner: znas
description: Export everything one account holds into one checked file, move rows between accounts, and read what a file contains and leaves out.
---

# Account export

An **account export** is one file that holds everything one account holds on an
installation: every version of every row that belongs to the account, and the
bytes those rows point at. It is a copy. Nothing is removed from the
installation it was taken from.

The file is made to be imported by another MemQL installation, as that
installation's primary account or as one more account. Import is not built yet.
Until it is, a file is the account's own copy of its data, and it is checked
against the installation it came from before it is offered for download.

Exports are started in **Accounts** in MemQL OS, on an account's **Export** tab.
Rows are moved between accounts on its **Data** tab.

**Audience:** owners and developers of an installation.

Related: [Organization ownership](auth/organization-ownership.md) ·
[Access model](auth/access-model.md) · [Library](library.md) ·
[Deployables](deployables.md)

Design record: `docs/superpowers/specs/2026-11-12-account-export-design.md`.

---

## Before the first export

An export reads the account each row records
([which account a row belongs to](auth/organization-ownership.md#which-account-a-row-belongs-to)).
The Export tab shows five conditions and offers no export until all of them
hold:

| Condition | What makes it hold |
|---|---|
| Recording accounts is on | The installation records an account on every new row. An operator can hold this with `MEMQL_ACCOUNT_STAMPING_ACTIVATE=false`; releasing the hold is one-way |
| Existing rows have their account | The background job has finished its first pass over every kind of row, no row is still waiting, and no pass that was asked for is unfinished. On a large installation the first pass takes a day or two after recording is turned on |
| Every concept declares | No loaded concept is missing its `@belongsTo` declaration. The tab names the ones that are |
| Nothing about this account awaits a decision | Rows the job would not guess at have been placed, or left out |
| No row is still following a moved parent | Rows that hang off a row which changed account have caught up |

The tab also shows how many rows are in no account at all. They are left out of
every export until someone places them (see [Moving rows](#moving-rows)).

## Starting an export

The plus on the Exports heading opens **Export this account**:

1. **Contents.** How much the account holds (its kinds, its rows and the
   bytes of its files), and what a file leaves out.
2. **Protection.** On unless you switch it off. A protected file is encrypted
   and cannot be opened without its passphrase.
3. **Passphrase.** The installation makes the passphrase and shows it once.
   Save it before going on, and start the export within fifteen minutes. The
   passphrase is kept sealed only until the export ends and is then erased:
   after that not even the installation can open the file.
4. **Export.** The installation writes the file in the background. You can
   leave; the export's line on the tab opens the wizard again where it stands.
5. **Download.** The file's name, size and checksum, and the download.

One export runs at a time on an installation, and an account has at most one in
progress. If the node running an export stops, another node starts it again from
the beginning, three times at most. A finished file stays until a person
deletes it; the record of the export, with who started, downloaded and deleted
it, stays for good.

## What a file holds

- **The account's own row.**
- **Every row whose newest version names the account**, with every version it
  has, for every kind of row that belongs to accounts. That includes mirrored
  records of a connected store, marked as mirrors.
- **People.** The account's members, past and present, with name, email and the
  role they hold. An owner or developer of the installation who is not a member
  appears as an id only, and the request details of what they wrote (email, IP
  address, location, browser, serving node) are removed.
- **Bytes.** Every Library file with every kept version, and each site's live
  bundle and its candidate.
- **Website statistics** for the account's sites: the hourly and daily
  summaries. Not the raw request log.
- **Email blocks** that the installation keeps for everyone and that match the
  account's own recipients.
- **A list of what the rows point at outside the file**, and a list of
  connections to set up again. A row the installation provides to every account
  is listed with its title; anything else is listed by kind and id.

## What a file leaves out

| Left out | Why |
|---|---|
| Sign-in material and visits: site access sessions, preview grants, pending invitations | They only mean something on the installation that issued them. People sign in afresh on another installation |
| Work in transit: uploads in progress, the outbound delivery queue, an email connection's sealed state | It describes this installation's work, not the account's data |
| What is bound to a machine: workbench workspaces, watched folders | The machine is not part of the account |
| The installation's own rows: seeded skills and agents, global roles, shared knowledge | They belong to the installation. The file lists each one the account's rows point at |
| Secret values | No secret leaves the installation. A field declared secret is removed from every version, at any depth, and the file names each connection so it can be set up again |
| Attachment bytes, earlier site bundles and package build outputs | Counted in the file's report with their size |
| Bytes a row points at that are not its own | A row's bytes live in a place its kind declares. A reference outside that place is recorded as refused and is not read |
| Search vectors, email statistics, site health | Rebuilt from what the file carries |
| What a group may do (its grants) | Grants are not yet scoped to an account |

A site's access codes do travel. They are stored as digests that cannot be
reversed, so visitors keep the codes they hold.

Work that was running or queued when the file was taken travels as it stood.

## Reading and checking a file

An unprotected file is a tar archive. Unpacked, it is one folder:

```
<account>-<date>.memql-account/
  manifest.json      what this is, from where, when, and the format version
  account.jsonl      the account's own row
  rows/              one folder per kind; JSON lines in gzip parts
  schemas/           each schema a version was written under
  people.jsonl
  references.jsonl   rows outside the file that its rows point at
  connections.jsonl  what has to be set up again
  blocks.jsonl
  tables/            website statistics
  bytes/             file bytes, exactly as stored
  bytes.jsonl        which row each one belongs to, its size and checksum
  report.json        counts expected and written; what was left out, by reason
  README.txt
  SHA256SUMS
```

Within a kind, a row's versions sit together, oldest first.

To check every member with a stock tool:

```bash
cd <account>-<date>.memql-account && sha256sum -c SHA256SUMS
```

The checksum of the whole file is shown beside the download, to compare with
`sha256sum <file>` after saving it.

A protected file ends in `.tar.age`. It is the same archive wrapped with
[age](https://age-encryption.org) in passphrase mode:

```bash
age --decrypt --output export.tar <file>.tar.age
```

Before a file is offered for download the installation has already read it
back from storage, opened it with the passphrase when it is protected, and
checked every member and every count. A newer engine reads every older file; an
older engine refuses a newer one outright.

## How a file is known to be complete

- Every row in it was read at one instant.
- Inside that instant the rows and versions of each kind are counted twice, by
  two separate reads. If the counts differ the export fails and there is no
  file.
- An export refuses to run while any row is still waiting for its account, and
  while any loaded concept says nothing about accounts.
- Everything that was left out is in `report.json`, with its reason and count,
  including the rows of each kind that are in no account.

## Moving rows

A person's work is recorded in the account they act in. An owner or developer
acts in the primary account, so work they do for a client under their own name
sits in the primary account until it is moved.

On an account's **Data** tab an area opens to its kinds, and a kind opens to its
rows. Select rows, choose **Move to account**, review what moves, and confirm.

A row takes with it everything that hangs off it in the same account: a goal
takes its runs and their steps. Those rows follow in the background, carried by
the same job that gives existing rows their account. The review names the kinds
that can follow and does not count them. On a large installation following can
take as long as a pass over the kind, and an export waits until it has
finished.

What follows is what hung off the row when it moved. A row added to it
afterwards is filed where it is written. A row that a person has since moved or
shared somewhere else stays where they put it.

The same act **places** a row that is in no account. On the primary account's
Data tab, rows awaiting a decision and rows in no account are listed; each can
be placed in an account, and a row awaiting a decision can also be left out for
good.

The list of kinds in no account covers the kinds handled by automatic
assignment. To check another kind, choose **Browse** under **In no
account** and select its name. This works even if the account inventory could
not be read. The cluster checks whether the chosen kind supports accounts;
opening it changes nothing. Select its unassigned rows and use the same move
review and confirmation to place them.

To find rows already in an account when its inventory cannot be counted, use
**Browse** in **Inventory** and choose the kind. This opens only that account's
rows; **Browse** under **In no account** opens unassigned rows instead.

When you know a row's id, open **Refine** and paste its full id or UUID into
**Name or ID**. This reads that row directly, even if the initial list could
not load, and shows it only if it is currently in the account being viewed (or
in no account on that page). Each row shows its id so equal names can be told
apart. Select a match, then search for the next id; the selection stays until
you clear it or leave the rows page. Review the selected rows together before
confirming their move. A name search filters each page separately; the list
continues loading later pages even when a page has no matches.

What the act does not do:

- It does not move a row that follows a parent in the same account: move the
  parent.
- It does not move a row of a kind that states its own account, such as a
  campaign or a site, once it has one: change its organization in its own app.
- It does nothing while recording accounts is off.

A move is written to the audit trail. It changes the account on the stored row
in place and creates no new version of it.

### Files, goals and compositions

Files, goals, compositions, compose templates and recipes can be shared with an
account. A row is shared with at most one account, and it is the account the row
belongs to: sharing it with an account you belong to files it there. Sharing a
row with an account you are not a member of is refused. When a row leaves an
account, that account's members stop reading it.

## Who may

Exports and moves are for owners and developers. Two capabilities hold them,
`execute app:accounts/export` and `execute app:accounts/move`.

Starting, cancelling, downloading or deleting an export, and moving or placing
rows, need a person's own signed-in browser session. An access token, a service
account, an app session and anything running inside an automation are refused.

Every one of those acts is written to the audit trail, and every owner is sent
an email when an export starts.

## The download

`GET /accounts/exports/{id}/content`, on the API host. It is a route for an
owner or developer signed in from a browser. A signed-in person who may not
export gets "not found", and so does a request for an export that does not
exist, was removed or is not finished. A request with no sign-in is refused
before the route is reached.

The bytes are streamed, and a single transfer may take up to six hours. The
route sends an `ETag` made from the file's checksum and honours one `Range`
with `If-Range`, so a client that asks for a range can continue an interrupted
transfer. MemQL OS does not do that yet: it reaches the route on its own origin
and saves the file as it arrives, and an interrupted download starts again from
the beginning.

## Limits

- No importer exists yet.
- MemQL OS starts an interrupted download again from the beginning.
- Journals that system automations wrote before recording was on have no owner,
  so they are in the primary account, including those that ran on another
  account's rows.
- No secret value travels. Every connection is set up again by a person.
- Attachment bytes are counted and not carried.
- What a row's payload says is carried as written, including any mention of
  another account.
- A file taken before a privacy erasure still holds what was erased. Whoever
  holds the file is responsible for it; the export's record shows who
  downloaded it.
- History that retention has archived or deleted is not in a file.
- On an account that holds most of a large installation, the Data tab's counts
  take several seconds and can answer that the account is too large to count.
  The export itself does not depend on them.
- Rows that follow a moved row are carried by a background pass, so a large move
  takes time to settle before an export can start.
