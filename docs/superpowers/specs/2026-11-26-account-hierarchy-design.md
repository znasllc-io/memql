---
title: Account roles, sole ownership and people management
audience: internal
status: implementation
area: design
owner: znas
---

# Account hierarchy

The owner requested this change on 2026-11-26: one owner per account, including
the primary account; multiple developers without access to each other's private
data; client administration limited to the apps the cluster grants the account;
and Accounts, Users and Groups using Fleet's shared multi-select interaction.
Role names replace the old rank graphics. This supersedes the earlier permission
to create additional cluster owners and the rank-visible developer peer rule.

## Authority

- `accounts:account.ownerUserId` names the sole owner. Ownership is transferred
  as one transaction, with the previous owner retained as a developer. A normal
  role change, invitation, membership removal or user suspension cannot create
  another owner or strand an active account without its owner. A primary role
  change also advances the credential revocation epoch in the same transaction.
- The account's canonical group (`acct-<accountId>`) holds its people. A member's
  organization role is Developer, Admin, Member or Viewer; Owner is derived from
  the account's single owner reference. Custom groups organize those same people
  and grant subsets of the account's app access; they cannot promote a member.
- Primary-account roles retain the existing cluster-role integration. Roles in
  a client account grant authority there only, never cluster administration.
- Account app access is the upper bound set by the cluster owner. A client
  manager can distribute only that access, to people and groups in that account.
  Organization-scoped app operations check the current bound; hiding an app is
  not authorization. Personal apps retain their existing user access rules. Removing an entitlement
  takes effect without rewriting every subordinate grant. Account managers receive the account ceiling; Members and
  Viewers receive selected apps directly and through custom groups.
- A shared account or an equal developer rank does not share private data.
  The account owner retains oversight, even when developing in a different
  account alongside that person. Reads, pagination, subscriptions and
  writes must agree; a post-filter alone is insufficient.
- Existing ownership attribution is retained. The primary account's missing
  owner can be repaired only from one unambiguous active cluster owner. No
  client CEO is inferred from contact fields, names or email addresses.

## Interface

Use the existing Head, DetailsSection, SettingRow, CardCollectionSearch,
AnchoredPicker and card collection primitives. Extract a common multi-select
picker from the Fleet pattern, with search before paging, persistent checked
choices, keyboard navigation, one batch Add action and retained drafts on a
failed write. Group membership and role assignment use the same components.
Show concise role names, with no numeric levels or rank ladders. Mark the `self`
account explicitly as Primary account. Keep account scope visible beside its
people and app permissions, and refuse stale permission edits visibly.

## Verification

Required coverage includes concurrent owner assignment and transfer, last-owner
removal, cross-account and self-escalation refusals, permission revocation,
developer peer privacy through query and row gates, and atomic membership batches.
`component/memql/account_access_db_test.go` exercises these against isolated real
graphs, including ordinary-write bypasses, stale cached reads, scoped invitation
projection/revocation, and competing first-owner claims.
`component/identity/adminops/account_invitation_test.go` covers invitation
authority without changing the identity credential lifecycle.
UI coverage includes multi-selection through search and paging, keyboard/focus,
failed writes and scope changes. Review desktop and narrow layouts in both
themes. Validate with real local database tests as well as unit fixtures.

This change is source work, merged directly to main after checks. Releasing or
deploying it, account-bound sign-in and data export/import remain separate work.
