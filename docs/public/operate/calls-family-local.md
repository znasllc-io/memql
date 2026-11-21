---
title: "Supervised family calls: local consent simulation"
audience: public
status: stable
area: operate
sinceVersion: 0.31.40
owner: znas
description: Operate the explicitly enabled supervised family consent simulation, its admission and closure safeguards, and the requirements before real child participation.
---

# Supervised family calls: local consent simulation

This capability is **local testing only**. It is not a verified parental-consent service and must not be used to admit real children. Adult identity/age verification, a Passkey, an invitation, and a parental-authority declaration are distinct from verified parental consent. No production consent provider is connected, and no live/sandbox family capability is offered. Production remains blocked until the consent provider, notices, parental rights, retention, jurisdiction and legal review are ready.

## Enable explicitly

The default is disabled. The operator must select `MEMQL_CALLS_FAMILY_MODE=mock` through the supported local mock-commerce helper, with an existing fresh mock-commerce envelope. The preserving toggle is `scripts/k3d/calls-mock.sh --family=mock`; `--family=disabled` switches it off. This must preserve the existing budgets, expiry and other Secret entries; it does not authorize replacing the financial envelope with defaults. Follow the helper's apply/restart instructions. Do not patch deployments or reset application data to enable it.

A missing flag, unsupported mode, sandbox/live commerce, or stale envelope denies new family quotes, invitations and admissions. Reconciliation also closes existing family media when this authority disappears. The UI capability is `capabilities.familyMode: "mock"`; it must describe the ceremony as a simulation.

## Immutable room policy

Start and Schedule use the same signed quote with `participationMode: "supervised_family"`. A family room requires at least two participant places, keeps its quoted limit/deadline, and cannot switch participation mode through rescheduling. Existing ordinary rooms remain ordinary. Only the owning verified adult account's designated initial human seat supervises; a guest/cohost link and another logged-in browser do not become a guardian. Native agents, delegated hosting, Resources, Insights, recording and transcription are unavailable in family mode. This policy does not infer a person's age or humanity from their video.

A family call ends for everyone when the guardian leaves, disconnects, loses required authority, or revokes consent. The ordinary Keep running and five-minute wrap-up options do not keep a family call open. Refreshing the guardian's page can therefore end the call. Rejoining does not reverse a terminal closure. Other children are never treated as substitute supervisors.

## Separate child device

The guardian creates a child-specific invitation from the active family call. Its token is returned only to that authorized guardian, stored server-side as a digest, and transported in a URL fragment (`#family=...`), never in a query string. The frontend clears the fragment before rendering.

`POST /_memql/calls/family` supports:

- Guardian `list`, `invite`, `mock_approve`, and `revoke`, bound to the exact `roomId` and media `identity` currently displayed. Invite, approval and revocation also carry stable idempotency keys.
- Child `claim` and `status`, bound to the invitation and the HttpOnly browser session. An optional `invitationId` is an expected-binding check, not authority to select someone else's invitation.

Claim creates no media seat and collects no child's name, date of birth, photo, voice or document. One invitation binds one browser session. A different browser cannot take it over. After the previous family call ends, the same child browser can claim a new invitation with a fresh session; the old call binding is never retargeted. Pending invitations expire after 30 minutes or the room deadline, whichever comes first. A lost first claim response whose cookie was never received may require a new invitation; the link cannot silently recover another browser's child session.

Approval is permitted only after the child browser has claimed the invitation. It requires a recent guardian Passkey assertion and the exact mock consent version `family-mock-v1`, plus the explicit parental-authority declaration. The private receipt binds that child session, guardian credential, call and consent version. The declaration does not purport to be real parental verification. Approved consent lasts only to the call's existing hard deadline.

Only then does `POST /_memql/calls/join` accept `family: true`, the expected `familyInvitationId`, and a display name. The invitation token is optional after binding. The server rechecks consent and actual guardian media presence before handing out a media pass. A child seat records `confirmAdult: false`; the child never declares themselves an adult. All renewals recheck authority. A known child session cannot escape into ordinary joins or register/claim/fund an account by asserting Adult.

`GET /_memql/calls/session` carries a small `family` marker even before a child occupies a seat, so a refresh resumes the waiting/approved state without saving invitation secrets in localStorage. No media, chat or call name collection occurs while consent is pending.

## Terminal media enforcement

LiveKit is pinned to v1.13.7 with `room.auto_create: false`. Every family placement attempt gets a fresh physical media-room name. No family renewal or rejoin calls CreateRoom. Once a room name has been used for admission, it is never recreated. This also prevents a late CreateRoom from a failed initial attempt from recreating a previously admitted room: that attempt had a different name and no issued participant token.

Terminal closure first persists an irrevocable closing marker under the call's gates, then deletes the media room and settles the existing usage journal once. Provider deletion failure keeps the marker: no new pass is issued, no return cancels it, and reconciliation retries. A delayed duplicate disconnect cannot move the first cutoff or extend charges. Consent revocation commits this marker before updating its separate receipt, so a partial write cannot leave old media running indefinitely. Invalidated consent credentials are checked during active-call reconciliation, independently of the guardian's own seat credential.

Signed media events trigger closure on the designated guardian's disconnect. Participant SID ordering rejects a stale connection's leave after a newer connection is established. The normal reconciliation automation repairs missed events every 30 seconds. Actual disconnection depends on successful provider control I/O; this is not a guarantee of instantaneous isolation during network/control-plane outages. Production assurance must validate and document that operational bound.

An isolated LiveKit test verified existing-connection disconnection on DeleteRoom and refusal of three still-unexpired token reconnects. Its positive control demonstrated that deliberately recreating the old name makes the old token valid again; consequently never recreating an admitted family name is an essential invariant, not merely a token TTL policy.

## Data and release gates

The server-only `familyState` journal stores opaque child session bindings and mock consent references, not child names, documents, birthdays or media. Mock journal versions expire after 30 days with no cold archive. This is a test retention rule, not a final legal retention policy. The journal stays with the originating account/cluster on account export and cannot grant consent in another installation.

The flow does not take screenshots, inspect video for apparent age, capture identity documents, or delegate supervision to a BYO agent. A client controlling or clearing a browser cookie is not proof of physical identity or age. Global guest age assurance, account sharing, consent revocation rights and real parental verification remain separate production requirements. The mock capability must remain off on production until those requirements and the provider/media contract are qualified.

The native delegated Start/Join preflight validates current terms, participation, display fields and the signed family policy before reserving credit or initiating an automatic top-up. `TestDelegationStartValidationPrecedesReservationAndAutomaticTopup` holds this rejection-without-effects invariant; a refused family/terms request cannot charge the account merely by being retried.
