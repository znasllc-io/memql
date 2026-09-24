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
