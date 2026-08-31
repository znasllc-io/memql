# MemQL OS -- the Users App -- Design

- **Date:** 2026-08-31
- **Status:** approved (epic memql#4733; written alongside the implementation)
- **Scope:** the Users app (`clients/os/src/apps/users/`), a kit promotion out
  of `apps/fleet/`, **two new DSL constructs plus a shape on an existing
  query** (`dsl/identity/`), and **one TS SDK method** that the proto already
  carried. No Go behaviour change, no proto change, no wire-contract change --
  so no frontend-team ping: this IS the frontend, and the one new query is
  additive.
- **Depends on:** epic memql#4710 (the foundation) and memql#4729 (Fleet),
  both merged.
- **Issues closed:** #4733 (epic), #4734 (People, live), #4735 (Invites),
  #4736 (Admin actions).

## Why

The foundation shipped Users as a stub with a promise on it: "a list that
updates the moment someone accepts." That sentence is the whole epic. The
People list is a `LiveList` over `v1:identity:user`, the Invites list is a
`LiveList` over `v1:identity:invitation`, and both ride the shell's single
`Connection` -- so one acceptance takes a row off one list and puts a row on
the other, with the arrival cue, and neither list refetches.

Everything else here is the surface that makes that list worth having: issue,
re-send and cancel an invitation; change a role; rescue somebody out of
`passkey_only`; mint an enrolment link.

## The exemplar, and the two broadcast rules that make it real

`component/node/routing.go` decides what a browser can see live, and the two
concepts this app renders are declared **differently on purpose**:

| Concept | created | updated |
|---|---|---|
| `v1:identity:user` | broadcast | **no rule** |
| `v1:identity:invitation` | broadcast | broadcast |

The asymmetry is a volume decision and this epic must not "fix" it. A user row
churns on `lastSeenAt` and on preferences, so an update rule would push an
event per person per heartbeat across the mesh forever -- the same reasoning
that keeps `v1:worker:invocation` off the list. An invitation is a human
action, so it is cheap.

What that buys and what it costs:

- **Buys:** the acceptance exemplar. `markUserInvitationAccepted` writes
  `status: "accepted"` (invitation `updated` -> the row leaves the invites
  list) and the registration writes a user row (user `created` -> the row
  arrives on the People list with the cue). One connection, two lists, zero
  refreshes. `test/users/invites.test.tsx` drives exactly that and asserts
  both seeds ran once.
- **Costs:** an admin action's effect does NOT arrive as an event. So the
  detail panel re-reads its person **on open** -- a panel is opened
  deliberately, once, about one row, which is the right place to pay for one
  authorized read and the wrong place to poll -- and every write hands its own
  accepted value straight back to the panel. Nothing polls, and nothing
  refetches after a write.

### A heartbeat is not news

`lastSeenAt` is deliberately absent from the People list's `fingerprint`.
Naming it would announce every person on a timer, which is the standing badge
the arrival cue exists not to be. The fingerprint is what a *person* would call
a change: a rename, a role change, a sign-in-policy flip, a deactivation.
`test/users/people.test.tsx` pins both directions -- the cue fires on a role
change and stays silent on a heartbeat.

## Two reads were carrying credential digests to the browser

Both reads this app needed projected token hashes, and neither had any use for
them. Fixed at the DSL, not worked around in the client.

**`pendingUserInvitations` declared no shape at all.** A query with no shape
projects the bound concept's *every* field, so it handed `tokenHash`,
`previousTokenHash` and `bindingHash` to the browser of every owner and admin
who opened the portal's People page. It now projects
`invitationAdminSummary`, which drops all three -- and *adds* the three
delivery fields (`deliveryState` / `deliveryError` / `deliveryAttemptedAt`)
that `invitationFull` never carried, so nothing which named a shape here could
have shown whether the mail went out either.

**`authSessionsForSubject` could not be used at all.** It filters on `subject`
and nothing else -- no role gate -- and projects `authSessionFull`, hashes
included. It is a SERVER read: its one caller is the all-sessions revoke
handler passing the caller's own JWT `sub`, so the argument is never
attacker-chosen *there*. This panel passes an id somebody clicked, from a
browser.

Narrowing it in place was not available in either direction. Adding
`requiresOwnerOrAdmin` would refuse the self-service revoke-all path for every
non-admin in the cluster; dropping the hashes from `authSessionFull` would
break the auth hot path, which looks rows up by exactly those digests. So the
browser gets its own read, `sessionsForSubjectAdmin`: same rows, its own
`requiresOwnerOrAdmin` conjunct, and `authSessionAdminSummary` -- the
cross-account sibling of the existing `authSessionSelf`, which already records
this rule for the self case.

`v1:identity:authSession` still declares no `@rowAuthz` tier and **cannot**:
`authSessionByTokenHash` is the pre-actor read that builds the actor, so an
owner tier would compare every session against `actor.userId == ""` and match
nothing -- every request in the cluster would fail to authenticate. That is why
the new query carries its own gate, and why it is registered in
`component/memql/rowauthz_undeclared_gate_test.go` with a reason of its own
rather than the grandfather marker.

**Not fixed here, and reported separately:** `authSessionsForSubject` remains
reachable from any signed-in browser with any user id. It is on the
grandfathered undeclared list, and closing it means stamping internal origin at
its Go call site and marking the query `@serverOnly` -- an auth hot-path change
that deserves its own PR and its own review, not a ride inside a client epic.

## Reset is one direction, and the message has no value field

`reset_sign_in_policy` was on the `IdentityAdminMsg` oneof and missing from the
TS SDK, so this epic adds `IdentityAdminClient.resetSignInPolicy`.

The shape is the specification: an admin can turn sign-in links back **on** for
somebody who chose `passkey_only` and then lost their passkey; there is no call
that turns them **off** for another person, because that is a one-request
lockout of a colleague's own account. A message with no policy field cannot
express the wrong direction, so there is no rule here that can be got wrong.

The panel follows it: the reset control renders **only** when the target is
actually on `passkey_only`, and where it would otherwise be, the surface says
in words that turning links off is self-service and needs the person's own
active passkey.

## The invitation link, and the three delivery states

`issueUserInvitation` returns a URL that is a credential -- it exists nowhere
else, the server kept only its SHA-256 hash, and no later call retrieves it.
The initial task text said not to render it. That is right in the common case
and wrong in one that matters, so the rule is conditional on the three states
`UserInvitationResult` was built to distinguish (memql#4584):

| `emailSent` | `emailError` | means | link shown |
|---|---|---|---|
| true | -- | delivered | **no** -- it is on its way; it has no reason to be on screen |
| false | empty | no mail wired on this cluster | **yes** -- it is the only delivery mechanism |
| false | set | the send failed | **yes** -- it is how the operator rescues it by hand |

Withholding it in row two would leave an invitation nobody can act on, on
exactly the clusters where `LogSender` is the legitimate configuration. Shown,
it is shown once, with a copy control, in component state that dies with the
panel -- never in storage, never on a row, never in a URL of our own. The same
rule governs the enrolment link.

Re-send is **issue-then-revoke**, in that order, because there is no dedicated
resend op on the oneof (verified). The order is the one that is safe to
interrupt: revoking first and then failing to issue leaves the person with
nothing, while issuing first and then failing to revoke leaves two live
invitations -- untidy, still working, and the stale one expires on its own.

## The kit grew, because this is the second app

`clients/os/src/styles/index.css` said what to do the day a second app wanted
one of Fleet's app-local classes: *promote it upward rather than copy it
sideways*. Users is that second app, so four things moved rather than being
duplicated:

| was | is | why |
|---|---|---|
| `.os-machine*` | `.os-row*` + `kit.Row` | a line in a live list is not a fleet idea |
| `apps/fleet/liveView.ts` | `live/liveView.ts` | a view over a live source belongs with the live substrate |
| `apps/fleet/useNow.ts` | `kit/useNow.ts` | one clock per section, for every section |
| `apps/fleet/format.ts` | `kit/format.ts` | the shell's time voice, not the Fleet's |

`.os-machine` and `.os-fleet` stay as CSS aliases: the shared *behaviour* is
what had to move, and sweeping every selector in a working app is a diff with
no behavioural content. The Users app ships **two** classes of its own
(`.os-users-inactive-tag`, `.os-users-secret`), which is the measure of whether
the promotion was the right size.

`kit.Row` carries two independent axes rather than one. `current` is liveness
(online for a machine, active for a person) and takes the row to full ink;
`dim` is "still true, no longer live" (revoked, deactivated). They are separate
because a deactivated account can still hold a live session.

## Presentation gating, and where the authority actually is

The manifest declares `roles: { min: "admin" }` and the role select hides
grants at or above the viewer's own rank. All of it is presentation (spec
section E):

- `searchUsers` and `pendingUserInvitations` carry `requiresOwnerOrAdmin` as
  top-level conjuncts in their own filters.
- `sessionsForSubjectAdmin` now does too.
- `adminops.authorize` gates every write, against the role the stream
  interceptor verified.
- Row admission gates the subscriptions on the same function that gates the
  reads (memql#4309).

Two details are load-bearing.

**The UI mirrors the server rule rather than the task text.** The task asked
for grants at or above `admin` to be owner-gated. The server's rule is
different -- `role_above_inviter` refuses a grant STRICTLY ABOVE the inviter's
own role, so an admin granting admin succeeds -- and the task's version is the
wrong direction to differ in: hiding a control that would have worked teaches
an operator a restriction the cluster does not have, with no way to discover
otherwise. Hiding is only honest for what always fails, which is the reasoning
`IdentityAdminClient` already records for this whole surface. So the select
disables strictly-above-own-rank and nothing else.

**An option for a role above the viewer is `disabled`, not absent.** A person whose current role outranks the viewer must
still see *their own* role in the select -- an option removed would leave the
box showing somebody else's, which is a worse lie than a choice that cannot be
made.

## Errors render in surface, and the detail is printed once

Every refusal is a `Notice` beside the control that produced it, carrying the
server's own sentence and the `auditEventId` (populated on refusals too,
because a denial is audited). Never a toast.

One exception, and it is the opposite of an oversight: the section-level feed
error passes **no** `detail`, because `LiveList` already prints
`snapshot.error` verbatim directly beneath the list. Passing it again puts one
sentence on screen twice a few lines apart, which reads as two failures. The
framing goes in the Notice; the words stay where the feed put them.

## What is deliberately not here

- **Guest invitations** (`SendGuestInviteMsg` family) -- space-scoped, a
  product surface, not cluster people management.
- **Suspension and profile editing** -- `set_user_suspended` and
  `update_user_profile` exist on the oneof; deferred.
- **Session and PAT revocation, recovery-key rotation, the audit-trail
  browser, an invitation history archive.**
- **A `graph.node.updated.v1:identity:user` broadcast rule.** See above: its
  absence is the design.
