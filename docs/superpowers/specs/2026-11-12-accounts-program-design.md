---
title: Accounts program -- every row belongs to an account, a person signs in to one, and an account can be exported and imported
audience: internal
status: approved
area: design
sinceVersion: 0.31.34
owner: znas
---

# Accounts program

Owner brief, 2026-11-11 and 2026-11-12, in chat. It began as one request: a client
is owed an export of everything related to their business, taken from an account on
the owner's cluster, in a form a developer can import into a new cluster either as
that cluster's primary account or as one more account. It grew, by the owner's
direction, into a program: complete separation between accounts, sign-in to one
account at a time, a Profile app, a reorganisation of the administrative apps, and
the interface work those touch.

This record holds every decision taken in that conversation, so that the sessions
which build the later pieces work from the same answers. The first piece has its own
spec: [2026-11-12 account row ownership](2026-11-12-account-row-ownership-design.md).

**Approvals.** Each decision below is marked **approved** (the owner chose it in
chat on the date given) or **proposed** (put forward in chat as a recommendation).
On 2026-11-12 the owner accepted every recommendation in this record and in the
row-ownership spec. A decision still marked proposed was accepted under that
statement and stays open to revision when its own piece is designed.

## Words

| Word | Meaning |
|---|---|
| Account | One `v1:accounts:account` row: the cluster operator's own company or one of its clients |
| Primary account | The literal row `v1:accounts:account:self`. There is exactly one |
| Accounts | Every other account. The word "secondary" is never used |
| Primary group | The primary account's own group. Every account has one; this one gets a name |
| Primary users | Owners and developers |
| Account administrator | What the `admin` role becomes: the person who manages one account's people |
| Cluster-wide | Belonging to no account: people, sign-in, roles, nodes, configuration |

## What was found

Four read-only research passes over the tree at `a03b360f6` (0.31.34). Only the
findings that shape a decision are kept here.

**Accounts**

- An account is a row; the primary account is seeded at first boot and configured
  at ownership setup (`dsl/accounts/automations.memql`, `dsl/accounts/mutations.memql`).
- Every account has a group at the derived id `acct-<accountShortId>`
  (`integrations/groups/ensure.go`). Cluster-wide roles at admin rank and above are
  standing members of every account's group by rule, not by membership rows
  (`component/memql/rowauthz_account.go`, "Staff are a RULE, not rows").
- About 40 concepts carry `accountId` and five carry the list `accountIds`. The rest
  carry no account at all: notes, to-dos, calendar entries, file bytes, runs and
  steps, the Shopify catalog. A Go list names 22 concepts as organization-owned
  (`component/memql/organization_ownership.go`).
- No session carries an account. A person in several accounts sees all of them at
  once, and a few forms carry an account picker.

**Storage**

- One row table, `"MemoryNodes"`, append-only, one physical row per version; no
  owner or account column. Deletes are payload conventions.
- Every row carries the writer's request context in `metadata` (email, IP address,
  serving node), and `createdBy` holds the writer's email when one is known.
- Fresh ids are UUIDs. Several ids are derived from other ids: a person's desktop
  from their user id, an account's group from the account id, a Shopify mirror row
  from the store id.
- File bytes live in blob storage under keys that embed the owner's user id; site
  bundles under keys that embed the site id.

**Secrets**

- One sealing scheme (`component/secret/encryption.go`): one cluster-wide key, no
  key id, no rotation, and no binding between a ciphertext and its row. Secrets are
  found by name.
- Token digests are plain SHA-256 with nothing cluster-specific in them, so an
  imported token row would make a token from the source cluster valid on the target.
- Most connections cannot be transplanted even with their secret: email sending is
  tagged with the source cluster's id, repository access belongs to the source
  cluster's GitHub App, Shopify webhooks point at the source cluster's address and a
  managed-app token rotates on use.
- Links in emails already sent are signed with a cluster-wide key and point at the
  source cluster.

**Versions**

- Payload-repairing migrations run once and are recorded as applied. Rows of an
  older shape that arrive later are not repaired by anything.
- Every row carries the schema it was written under, and a breaking-change
  classifier for concept schemas already exists
  (`component/memql/authoring_concept_diff.go`).
- A whole-cluster backup exists (`component/backup`, `memql backup export|restore`).
  It is unfiltered by design, moves row bytes only, and restores into an empty
  cluster. Its promise is one-directional: a newer engine reads every older backup.

## Locked decisions

### The model

| # | Decision | Status |
|---|---|---|
| P1 | Every row belongs to exactly one account or is cluster-wide. The account is recorded on the row, in `accountId` | approved 2026-11-12 |
| P2 | People are cluster-wide: one person, one sign-in, possibly several accounts | approved 2026-11-12 |
| P3 | A row tagged for several accounts today ends up in one | approved 2026-11-12 |
| P4 | The vocabulary in "Words" above | approved 2026-11-12 |

### Order and delivery

| # | Decision | Status |
|---|---|---|
| P5 | Five pieces, in this order: row ownership, export, sign-in and separation, shell, import and top-ups. Export and sign-in do not depend on each other and may be built side by side | approved 2026-11-12 |
| P6 | One push to `main` per piece. Work is built on a branch, verified locally including the database-backed tests, and pushed once. Removing a field rides in the next piece's push | approved 2026-11-12 |
| P7 | Every screen the program touches is rebuilt to the interface standard in P33 | approved 2026-11-12 |

### Export

| # | Decision | Status |
|---|---|---|
| P8 | One export covers one account: complete for that account, with nothing from any other account or from the cluster itself | approved 2026-11-11 |
| P9 | What a person makes inside an account is that account's and travels with it. For rows written before row ownership exists, the assignment rules in the row-ownership spec decide | approved 2026-11-11, restated 2026-11-12 |
| P10 | A password is optional. With one, the whole file is encrypted and the portable secrets travel. Without one, the file carries no secrets. Either way the import ends with a list of connections to set up again | approved 2026-11-11; refined 2026-11-12: protection is on unless switched off, the engine makes the passphrase, and no secret value travels until the import piece decides how |
| P11 | No sign-in material ever travels: no passkeys, tokens, sessions or link requests. People sign in on the target by email link and register a new passkey | approved 2026-11-12. A site's access codes travel: they are stored as digests |
| P12 | The request context stored on each row (email, IP address, node) is not exported for writes made by primary users | approved 2026-11-12, extended: an owner or developer who is not a member of the account appears as an id only |
| P13 | Export runs from the Accounts app in the primary account's desktop, for owners and developers only, and is audited | approved 2026-11-11; 2026-11-12: a signed-in person only, and every owner is mailed when one starts |
| P14 | Moving a row from one account to another is a governed act, and it arrives with export | approved 2026-11-12 |
| P15 | An export is a copy. Nothing is removed from the source cluster | approved 2026-11-12 |
| P16 | Not a SQL dump. A versioned, self-describing archive: every version of every row, the file bytes, and a manifest with checksums, following the whole-cluster backup's row shape and its one-directional promise | approved 2026-11-12: a tar file, wrapped with age when protected |
| P17 | Data held outside the row table (site statistics and similar) is classified by the export piece | approved 2026-11-12; classified in the export design |

The [account export design](2026-11-12-account-export-design.md) settles P8 to
P17 in detail. Its own decisions are numbered E1 to E22.

### Import and top-ups

| # | Decision | Status |
|---|---|---|
| P18 | An archive imports as the primary account of a new cluster, or as one more account | approved 2026-11-11 |
| P19 | A later export of the same account can be imported over an earlier import and adds only what changed. Before the copy is activated it tracks the source. After activation, a row changed on this cluster keeps this cluster's version. A privacy redaction made on the source replaces the copy | approved 2026-11-11 |
| P20 | An import never grants access by itself. People arrive unable to sign in until someone lets them in, a role is capped at what the importer may assign, and nobody arrives as owner | proposed |
| P21 | The archive is loaded beside the live data, checked, and reported on before anything is committed. The commit is all-or-nothing and fires no automations. The account lands switched off | proposed |
| P22 | The target must run the same engine release or a newer one. Repairs written after the archive was made are replayed on it, and a build gate makes every future migration say whether it can be replayed | proposed |
| P23 | Rows of a concept the target has not loaded are held until it is | proposed |

### Sign-in and separation

| # | Decision | Status |
|---|---|---|
| P24 | A person in several accounts chooses one when they sign in. A URL that belongs to an account skips the chooser. A person in one account never sees one | approved 2026-11-12 |
| P25 | The chooser comes after the person has proven who they are, not after they type an email, so nobody can read another person's accounts off the sign-in page | proposed |
| P26 | The session is bound to one account. Role, apps and rows are that account's, enforced by the engine on every read and write | approved 2026-11-12 |
| P27 | A role is held per account. An owner who enters another account is not an owner there | approved 2026-11-12 |
| P28 | Inside any account, an owner or developer has full access to that account and none of the cluster tools | approved 2026-11-12 |
| P29 | The primary account's desktop shows the primary account's own things. Another account's are reached by switching into it or through the Accounts app | proposed |
| P30 | The `admin` role becomes the account administrator: People and Groups for their own account, and nothing that can take over a person (passkey enrolment links, sign-in resets, ending sessions, revoking tokens) | approved 2026-11-12 |
| P31 | Switching accounts happens in the Profile app and reloads the desktop behind the loading screen | approved 2026-11-12 |
| P32 | Storage: no ceiling for owners and developers, 5 GB per person for everyone else, counted across all their accounts | proposed (read back to the owner, not contradicted) |

### Apps

| # | Decision | Status |
|---|---|---|
| P33 | Owners and developers only, in the primary account's desktop: Accounts, Identity, Cluster, Settings, Logs, Concepts, Pipelines, Fleet, Setup | approved 2026-11-12 |
| P34 | Per account, by the role held there, each showing only that account's things: Files, Bin, Deployables, Email, Materializer, Nexus, Ask, Training, and Users | approved 2026-11-12 |
| P35 | Profile is for everyone, opened only from the picture in the menu bar. It holds the personal sections, the account switcher (absent for a person with one account) and storage | approved 2026-11-12 |
| P36 | The eleven moves in "The app split" below | approved 2026-11-12 |
| P37 | Accounts > Credentials is removed: it issues a kind of token nothing in the engine accepts | proposed |

### Interface

| # | Decision | Status |
|---|---|---|
| P38 | The standard is the Fleet machine page (Details, Settings and its other tabs), with Deployables > a storefront > Statistics as the second reference | approved 2026-11-12 |
| P39 | A "More" button becomes the up-right arrow used in Fleet > Machines > Recent activity, starting with the Pages panel on Statistics | approved 2026-11-12 |
| P40 | Lists load the next page on scroll. No "Load more" button anywhere. A slow load holds its place, a failure offers a retry in place, and the end of the list is stated once | approved 2026-11-12 |
| P41 | The loading screen stays put, fades in and out, has a quiet animation and one short line of text | approved 2026-11-12 |

## The five pieces

### 1. Row ownership

Every concept declares whether its rows belong to an account or are cluster-wide.
The engine stamps the account at insert, existing rows are assigned, and the
Accounts app shows an exact inventory. Nothing anyone can see changes otherwise.
Spec: [account row ownership](2026-11-12-account-row-ownership-design.md).

### 2. Export

The client's file. Decided: P8 to P17. Designed in
[account export](2026-11-12-account-export-design.md), a draft until the owner
has read it. That design moves three things to later pieces:

- Carrying secret values goes to import, which has to seal them again on the
  target. The file lists the connections and reserves the place.
- The `accountIds` lists are narrowed to the row's own account, not removed.
  Dropping the field goes to sign-in and separation, which decides what sharing
  inside an account looks like.
- `campaignAccountId` stops being written in the export piece's push and is
  dropped in the push after it.

### 3. Sign-in and separation

Decided: P24 to P36. Still to design:

- How the account reaches the session: the token claim, the chooser page on the
  identity service, the switch.
- Narrowing every read to the session's account, and what a cluster-provided row
  looks like from inside an account.
- Which one-per-person rows become one per person per account. A desktop does;
  the row-ownership spec lists the others.
- Credentials that are not a browser session (access tokens, app sessions,
  machines): which account they act in.
- Whether cluster sign-in administration that has no screen today (the owner
  recovery key) gets one in Identity.
- What sharing inside an account looks like, and with it the end of the
  `accountIds` field (left by the export design).
- How a grant is scoped to an account, so that an export can carry what an
  account's groups may do.

### 4. Shell

The Profile app, the account switcher, the loading screen, load-on-scroll for every
list, and the arrow in place of "More" buttons. Decided: P35, P38 to P41. One shared
"Load more" control (`clients/os/src/kit/LoadMore.tsx`) is used by about 25 screens,
so most lists change in one place.

### 5. Import and top-ups

Decided: P18 to P23. It is built last so that it lands people on the new sign-in
model instead of being built twice. The export design leaves it two things: which
secret values a protected file may carry and how they are sealed again, and how
work that was running or queued when the file was taken is landed.

## The app split

| Today | Goes to |
|---|---|
| Identity: Profile, Passkeys and sessions, Access tokens, Settings | Profile |
| Settings: About, Appearance, Ask, Apps, Diagnostics | Profile |
| Settings > Cluster: the sign-in policy and brand form | Identity |
| Settings > Keys and Settings > Tokens | Identity |
| Users > Access requests | Identity |
| Cluster > Audit trail | Identity |
| Settings > Cluster: the read-only facts | Cluster > Overview |
| Settings > Benchmarks | Cluster |
| Settings > Language | Concepts |
| Settings > Access | Users |
| Cluster > Data | Settings, beside Integrations |

What each app holds afterwards:

- **Identity:** sign-in policy, the brand on sign-in pages and emails, access
  requests, signing keys, tokens, the audit trail, and its logs.
- **Cluster:** overview and readiness with the cluster facts, nodes, capacity,
  modules, system automations, agents, updates, benchmarks, and logs.
- **Settings:** integrations, pipelines, connections, data, vendors, levels,
  procedures, and logs.
- **Users:** people, groups, roles, role testing, and access. An account
  administrator sees People and Groups for their own account only.
- **Profile:** every personal section, the account switcher, and storage.

## Edge cases

Collected during the research. Each names the piece that has to handle it.

| Case | Why it matters | Piece |
|---|---|---|
| A token row copied to another cluster makes the source token valid there | Token digests carry nothing cluster-specific | Export (P11) |
| A passkey is bound to the sign-in hostname and the user id | It cannot work on a different domain | Import |
| A managed-app Shopify token rotates on use | Two clusters cannot both hold one | Import |
| Email sending is tagged with the source cluster's id | It refuses to send from another cluster | Import |
| Unsubscribe and tracking links in sent mail point at the source cluster | The source must keep honoring them, and later opt-outs must reach the target or the client mails people who opted out | Import (top-ups) |
| Domain ownership is recorded as proven | A file could claim a domain it never proved; the target must re-check | Import |
| The primary account's id is a literal, and the group's id derives from the account's | Importing as primary changes both, and every reference to them | Import |
| A person's desktop and a few other rows take their id from the user id | A person who already exists on the target keeps the target's own | Import |
| A site's public forms run under the site owner's authority | A site whose owner cannot act stops accepting submissions | Import |
| Site hostnames carry the cluster's domain | They must be rewritten and may collide | Import |
| File keys embed the owner's user id, and some carry the storage account's URL | They must be recomputed, and the storage allowance updated | Import |
| A payload repair runs once per cluster | An older archive needs the repairs that came after it | Import (P22) |
| A privacy redaction rewrites a stored version in place | A top-up has to replace the copy, not skip it | Import (P19) |
| The retention sweep deletes old rows physically | A copy that is still tracking its source should lose them too | Import |
| A scheduled campaign, an active journey, a run in flight | Importing them live would act twice, or act at all | Import (P21) |
| Rows of a concept whose bundle is not deployed yet | They cannot be read or checked until it is | Import (P23) |
| A raw insert bypasses every write guard | The import has its own write path and needs tests that hold it to the same rules | Import |
| An archive is a file someone could edit | It may only hold account rows, may not raise a role, and may not name another account | Import |
| A store bound by sites of two accounts | It has no single owner | Row ownership |
| Old and new nodes run side by side during a rollout | A new field written by a new node is refused by an old one | Row ownership |
| Vectors depend on the embedder in use | They are rebuilt, or carried only when the embedder matches | Import |
| Rows written by connectors and automations have no person behind them | The account has to come from the row they act on | Row ownership |

## Neighbors and out of scope

- **A possible gap in who can read and write sealed secret rows** was inferred
  during the research and handed to a separate session to verify. It is not part of
  this program.
- **Removing an exported account from the source cluster** is a separate job.
- **Incremental exports, an expiry on export files, and a command-line path** are
  left for later. A top-up is a newer full export.
- **A personal "download my data"** exists as an unfinished page on the identity
  service and is not this program.
