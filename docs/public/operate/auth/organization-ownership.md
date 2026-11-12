---
title: Organization ownership
audience: public
status: stable
area: operate
sinceVersion: 0.22.8
owner: znas
---

# Organization ownership

Accounts represent organizations and clients. The reserved `self` account is
this installation's operating company. Ownership setup requires its name,
records the verified claim owner's user ID, and configures that existing account
rather than creating a second organization model. The optional profile title
(Owner, Founder, CEO, or another job title) does not assign authorization rights.
The boot placeholder leaves the company's domain unset and unverified. A person
can supply it later and complete the ordinary domain-verification process; the
installation hostname alone proves no company-domain ownership.

Setup completes only after the required passkey proof, organization update,
account group, and owner membership have persisted. Local installations do not
verify the contact email; hosted installations verify email before passkey
registration. A failed organization write leaves setup resumable with the same
claim and credential, including on another replica.

## Selecting an organization

Cluster operators default to `self` and can explicitly select an active client
organization. A client with one authorized organization defaults to it. A client
with several memberships must deliberately select one. The server resolves
these defaults and validates the final row independently of the OS picker.
Missing, inactive, or unauthorized organization selections are refused.

A resource retains its original owning user alongside its organization. Peer
edits preserve that owner and record the current writer on the new row version.
Assigning an organization does not transfer authorship or allow a caller to
impersonate another user. Ownership of the row itself does not permit
access outside the caller's current organization permissions.

## Resource scope

| Resources | Organization contract |
| --- | --- |
| Campaigns, audiences, templates, sender identities, event-email rules | One required organization on creation; reads, updates and actions require the appropriate organization and app access. References used together must belong to that organization. |
| Recipients, deliveries, consent and engagement events | Inherit the relevant parent organization. Sending checks the resource organizations again before delivering. |
| Deployable sites and packages | One required organization on creation, including sites created by package installation. |
| Package deployment records | Inherit the package's organization. |
| Custom domains and account front doors | Cluster-controlled DNS infrastructure associated with its parent site/account. Organization membership alone cannot administer cluster DNS. |
| Groups and memberships | Account groups belong to their account; membership records follow the group. Untied system groups retain their existing purpose. |
| Files/library artifacts | Multiple account IDs support organization sharing alongside user ownership. They are not a single organization owner and are not defaulted to `self`. |
| Knowledge domains | Public knowledge catalog; the organization tag is classification, not private ownership. |
| Compose resources and work goals | Optional account lists classify work while retaining their existing user ownership and access contracts. |
| Personal notes, tasks, calendar, profile, passkeys, sessions and tokens | Personal ownership and existing access rules. |
| Fleet, cluster infrastructure, providers, system settings and role definitions | Cluster administration; organization membership does not grant access. |

For client campaigns, select a sender identity owned by that organization. The
installation's configured default mailbox is available to `self`. Access to two
organizations does not permit sending one organization's content to another's
recipients or using its mailbox. Scheduled and queued sends recheck the relevant
resources and current write permission when they execute. Reading campaign
content alone does not authorize a test message or a single-recipient send.

## Which account a row belongs to

Every concept says whether its rows belong to an account or to the cluster, with
one `@belongsTo` declaration
([language reference](../../language/memql.md#which-account-a-row-belongs-to)).
A row of an account-scoped concept records its account in `accountId`.

The resources in the table above that require an organization state it
themselves, and the rules on this page decide it. For every other account-scoped
concept (a note, a to-do, a file, a goal and its runs, a mirrored store record)
the engine records the account when the row is created, from the first of these
that gives one:

1. **The row it belongs to.** A run takes its goal's account and a file version
   its file's. A row whose parent has no account yet is written without one and
   follows the parent later.
2. **The account the call is acting in.** For a person this is `self` for a
   cluster operator, and the account of an account-scoped role for somebody who
   holds one. Anybody else acts in the account they belong to: the oldest
   membership of a person who belongs to several, and `self` for a person who
   belongs to none. An automation acts in the account of the row that triggered
   it, when the engine recorded that row's account or the rules on this page
   checked it, and a shopper form or an access-code visit acts in the account
   of its site.
3. **Nothing.** A seed, a scheduled sweep and any other write with no account
   behind it are written without one. On a concept declared `shared`, such a row
   is the cluster's own and is offered to every account.

The engine does not change a recorded account on an update. A write in which a
person names the account themselves, outside a mutation or through one that
accepts it, is refused when it names a different account from the stored one
(`account_change_refused`), an account the caller does not belong to
(`organization_forbidden`), or an account other than its parent's
(`account_mismatch`). Two things a person supplies are held to the
accounts that person belongs to without a refusal, and the row is recorded
under their own account instead: a tag in a list of accounts, and the parent
named by a write made outside a mutation.

Recording an account does not change who can read a row. Access is still
decided by row ownership, the sharing a concept declares, and capabilities.

### Turning it on

The release that records accounts also adds the `accountId` field to the
concepts that gain one, and an engine older than that release refuses to update
a row carrying a field it does not know. So the engine waits: each node
advertises that it knows the field, and recording starts, once for the whole
cluster, when every node that has not stopped has done so. Nothing turns it off
again.

Set `MEMQL_ACCOUNT_STAMPING_ACTIVATE=false` on every node to hold it
([environment variables](../env-vars.md)). While it is held on every node, no
row is changed and the previous release can still be restored. Until recording
starts, an account named in a write to one of these concepts is dropped, so
nothing can store one ahead of the checks above. Release the hold
by removing the setting and rolling the nodes. Take a database backup first:
from then on rows carry the field, and the release cannot be rolled back to one
that predates it.

### Rows written before

Once recording is on, a background job gives existing rows their account. It
changes the newest version of a row in place, publishes no event, and runs no
automation. It assigns parents before the rows that follow them, in small
batches, and resumes where it stopped after a restart. In order, a row takes:

1. an account its own list of accounts names, where the concept has one: among
   the listed accounts its owner may use (any, for a cluster operator), the
   owner's own when it is listed, otherwise the first;
2. its parent's account;
3. for a connected store, the account of the site bound to it;
4. its owner's account, by the rule for a person above;
5. `self`, or no account on a `shared` concept.

Two cases are never guessed. A store bound by sites of two different accounts,
and a Library row its retention rules do not allow to change, are listed for an
operator to decide and stay without an account until then.

The job rewrites one version of every row it assigns. Check the database has
room for that before releasing a hold on a large installation.

### What an account holds

In MemQL OS, an account's page in Accounts lists what the account holds, by
area, with the number of rows and the size of its files. The primary account's
page also shows how the assignment of existing rows is going and anything left
for a decision. Both need the `execute app:accounts/inventory` capability,
which owners and developers hold.

From the same page an owner or developer opens a kind to its rows, moves rows to
another account or places rows that have none, and exports the account into one
checked file. Moving needs `execute app:accounts/move` and exporting needs
`execute app:accounts/export`; both also need a person's own signed-in session.
[Account export](../account-export.md) describes both.

## Membership and delegated management

Every account, including `self`, has a deterministic account group. Creation and
startup reconciliation ensure that group idempotently; retries cannot create a
second membership group. Cluster operators are associated with the configured
`self` organization. Account creation does not grant every new group broad app
or administration rights.

Open an account's People section to reach its groups and memberships. From the
group, operators can open its access grants and define an account-scoped role.
Grant the required app capabilities explicitly. An account-scoped role is held
only with active membership in that account. Delegated group management is
bounded to that organization; it cannot assign cluster roles or promote somebody
to cluster administrator. Group grants for one organization do not make another
organization's app available. The OS discovers an app when at least one
authorized organization allows it, while its actions use the selected
organization's permissions. The global permissions view remains distinct.

Membership and grants are resolved from shared persisted state. A request routed
to another node receives the same boundaries; process-local membership state is
not an authority. Removing membership removes that organization's access.

## Existing installations

Existing accounts receive their missing account group through reconciliation.
The child-attribution migration fills missing organization IDs only where an
existing parent explicitly identifies the organization. It preserves explicit
assignments, unrelated concepts, and historical versions. It does not guess
which organization owns an unattributed root resource.

An older unattributed root remains private under its existing owner contract.
A standalone record can be assigned an organization before editing it through
an organization-owned flow. Linked legacy records require an operator-led,
coordinated migration after the operator identifies their organization: the
normal picker deliberately refuses changes that would leave parent and child
ownership inconsistent. The automatic migration does not guess this choice,
and ambiguous parent relationships remain unattributed. A partially attributed
campaign cannot mix with unattributed or differently attributed sending
resources.
