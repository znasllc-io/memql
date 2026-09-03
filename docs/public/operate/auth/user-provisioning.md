---
title: User Provisioning
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# User Provisioning

How users get into the cluster. The identity service
(`component/identity`) owns the registration surface end-to-end --
the web UI, the magic-link flow, the invitation flow, and the
underlying mutations on `v1:identity:user` / `v1:identity:identity`.

## Registration modes

Set via `MEMQL_IDENTITY_REGISTRATION_MODE`. Captured by the first-run
wizard if the env var is unset.

| Mode                 | Who can register                                                        |
|----------------------|-------------------------------------------------------------------------|
| `open`               | Anyone with any email. Default for new clusters.                        |
| `domain_restricted`  | Email must match `MEMQL_IDENTITY_REGISTRATION_DOMAINS`.                       |
| `invite_only`        | No self-registration. Users only enter via admin invitations.           |
| `waitlist`           | Users submit access requests; admins approve into invitations.          |
| `directory`          | **Directory membership is the invitation** (memql#4611). Nobody self-registers by email; a person who authenticates through the configured upstream provider is admitted. See [oidc-federation.md](oidc-federation.md). |

Mode is read by the identity web app (registration form) and by
the magic-link issuer (rejects new emails when the mode forbids
them).

## Magic-link flow (the primary path)

The flow is **device-bound** and **approve-on-click** (memql#4300, design
`docs/superpowers/specs/2026-08-22-magic-link-hardening-design.md`). One
sentence describes it:

> A session can only ever land on the device that asked for it.

If somebody else opens your link -- a colleague reading the same shared
mailbox, or a mail scanner -- they cannot sign in with it. What their click
does is *approve* the request, and the browser that asked finishes the
sign-in itself.

### The steps

1. The user enters their email at `/login` (or a client posts
   `/auth/magic-link`). Both paths run the anti-abuse middleware -- per-IP
   rate limit, optional Cloudflare Turnstile, disposable-email blocklist,
   MX-record validation, risk score. On rejection an audit event with
   `action=magic_link_blocked` and a `failureReason` is recorded and the
   caller gets a generic message.
2. The issuer mints the single-use token AND a 32-byte binding nonce. Only
   digests are stored: `tokenHash` and `bindingHash` on the
   `v1:identity:magicLinkRequest` row. The nonce goes to the requesting
   browser as `memql_ml` (`HttpOnly; Secure; SameSite=Lax; Path=/auth`,
   `Max-Age` = the link's TTL) and nowhere else.
3. The browser lands on `/check-email`, which renders the request id and
   starts polling `GET /auth/magic-link/status`. The id is not a credential
   -- the cookie is, and the status endpoint answers `404` to anybody who
   does not hold it.
4. The user opens the emailed link at `/auth/complete?ml=<token>`. **This
   renders a confirmation page and writes nothing.** A GET never changes
   state, which is what makes mail scanners and link prefetchers harmless.
5. The user presses **Continue**, posting `/auth/landing`:
   - **The browser holds the matching `memql_ml` cookie** (the same-device
     case -- a mail client opened a new tab of the same profile): the
     sign-in finishes right there.
   - **It does not** (the cross-device case): the request is stamped
     `approvedAt` with the approving device's IP and user agent, and the
     page says to go back to the device where the link was requested. The
     clicker is handed nothing: no cookie, no code, no session.
6. The waiting tab's poll sees `approved` and posts
   `/auth/magic-link/finish`. That consumes the request **exactly once**
   (compare-and-swap under a Postgres advisory lock) and mints what the
   row's OAuth context calls for: an auth code bound to the stored PKCE
   challenge when a client is named, otherwise a first-party browser
   session -- which now creates a `v1:identity:authSession` row with
   `source=oidc_cookie`, so it is listable and revocable like any other.
7. First-time addresses provision `v1:identity:user` +
   `v1:identity:identity` (`identityType="magic_link"`) at this point.
   Internal-domain users get `MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE`;
   external users get the default cluster-wide role, `reader`. There is no
   per-partition grant of any kind -- partitioning was retired in #56, and
   what bounds every user is the per-row check on each concept they read
   (see [access-model.md](access-model.md#what-the-role-actually-decides)).

### The cases that need spelling out

| Case | What happens |
|---|---|
| **Cross-device: request on the laptop, click on the phone** | Works as users expect. The phone approves; the laptop's `/check-email` tab notices within ~2 seconds and signs in. |
| **Somebody else on a shared mailbox clicks first** | They approve. *You* sign in, on your machine. The row records their IP under `approvedFromIP`, and the audit trail carries `magic_link_approved` naming it. |
| **The requesting tab was closed before the click** | The approval succeeds and nothing polls; the request expires at its TTL. The page says: request a new link from the device you want to use. |
| **A mail scanner or link prefetcher fetches the URL** | The page renders. Nothing changes. The link stays usable. |
| **The user requests a second link from the same browser** | The new cookie overwrites the old one, so the older link behaves as a no-cookie click -- approve only. Accepted and documented. |
| **JavaScript is off** | The poller does not run. The link still works; it has to be opened in the browser that asked for it, where the same-device branch finishes it directly. |
| **Two identity replicas** | No affinity is needed. The row is the state and the cookie's digest is on the row, so approve, poll and finish can each be served by a different pod. |
| **The env auto-bootstrap claim link** | Issued **unbound**, and completes for whoever opens it. It is emailed from a boot-time goroutine with no browser to bind to, so a binding would make it approvable from anywhere and completable nowhere — a cluster nobody can claim. Same trust this path always had: the address is the one the operator configured, on a cluster with no owner credential yet. Every other issue path answers a browser and is bound. |

### What still is not solved, and is not pretended to be

Device binding stops somebody riding *your* link. It does nothing about
somebody requesting *their own* link to a shared address they can also
read. Nothing does -- if you can read the mailbox, you can ask for a link.
The answers to that are the two controls in
[access-model.md](access-model.md#shared-mailboxes-and-passkey-only-sign-in):
the `sharedMailbox` flag makes the fact visible, and `passkey_only` makes
sign-in links stop working for the account.

## First-user-is-owner

The first user to register (regardless of mode) is bumped to
cluster `role=owner` so the cluster has a manageable admin from
the start. Subsequent registrations use the configured defaults.

## Invitations

`v1:identity:invitation` is an identity primitive used by two
flows:

- **Guest invites** (driven by the product frontend): a space owner
  sends a guest a link via `SendGuestInviteMsg`. Guests authenticate with
  `Authorization: Guest <token>` (the gRPC stream interceptor's
  guest-aware path).
- **User invitations** (memql#4270): an owner or admin issues one over
  `IdentityAdminMsg.issue_user_invitation`, and the reply carries the link
  ONCE -- only the token's SHA-256 digest is persisted, the same convention
  every other credential row in this domain follows. The recipient opens
  `/invitation?code=<token>` (memql#4601): the page resolves the token
  server-side and shows what it says, and the accept spends it once --
  provisioning the user row with the role the issuer chose, marking the
  invitation accepted, and minting a 15-minute enrolment token that lands on
  `/enroll` in the same window. The enrolment token's `issuedBy` names the
  INVITER, because the field is a parent edge onto `v1:identity:user` and
  the inviter's authority is what the invitation carried; the invitation
  itself is recorded on the token's `invitationId` (memql#4880). There is no
  separate partition grant to stamp -- the role is the whole of it.

  **What the registration mode does to it.** The policy is applied in the
  gate (`component/identity/adminops`), never by the console:

  | Mode | Issuing an invitation |
  |---|---|
  | `invite_only` | The normal path. The link is the only way in. |
  | `waitlist` | This verb mints the `invitationId` `approveAccessRequest` needs, which is what turns the queue into an admission. |
  | `domain_restricted` | **Refused** unless the address matches the allowlist. A link the recipient cannot redeem is worse than a refusal -- they only find out after clicking. |
  | `directory` | The normal path for somebody the directory does NOT cover -- a contractor, an auditor. Staff need no invitation, which is the mode's whole point; an invitation is the escape hatch for everyone else, and it still works. |
  | `open` | Permitted, and the reply says the mode so a console can tell the operator this is a courtesy: the recipient could have registered unaided. |

  An inviter cannot grant a role above their own, compared on the cluster's
  one ladder (`auth.RoleRank`). Note the ordering: **developer outranks
  admin**, so an admin cannot mint a developer invitation. That comparison used
  to run against a private table in `adminops` which ranked admin above
  developer, so an admin could mint a developer invitation -- a principal the
  canonical model ranks above them -- through the one check whose job is to
  refuse exactly that.

  **An enrolment link is bounded by WHO IT NAMES, not only by who mints it**
  (and this is stricter than it used to be). The link authorizes exactly one
  action, registering a passkey as the named user, and neither `/enroll` nor
  the WebAuthn ceremony compares ranks -- so whoever holds a link for an
  account can sign in as it. `IssueEnrolmentLink` therefore applies
  `auth.GovernPrincipal`: an owner-ranked target is reachable only by an
  owner, an owner reaches everyone, minting one for **yourself** is always
  allowed, and otherwise the caller must STRICTLY outrank the target.

  Before this, the target only had to exist. An admin could mint a link for
  the OWNER and take the account, and the admission capability would have
  extended that to developer. Two consequences to expect: an admin can no
  longer mint a link for another admin (peers do not outrank each other, which
  is what a role change on the same account already refuses), and nobody but
  an owner can mint one for an owner.

  `revoke_user_invitation` is the undo for a link sent to the wrong address.
  It is a SOFT cancel -- the row stays and its token hash stays taken, because
  revoking does not make the holder forget the token they were sent.

  **Redemption is validated** (memql#4282). The presented token is hashed and
  resolved to its row, which must be `kind="user"`, active, pending, unexpired,
  and issued **for the address registering**. Before that, the invitation was a
  presence check -- `strings.TrimSpace(x) != ""` -- so any non-empty string
  satisfied `invite_only` and bypassed `domain_restricted`. Each rejection is
  audited, and none of them fails the request: an unusable invitation means the
  caller has none, and the registration mode then decides on its own terms.

Tokens are stored as SHA-256 hashes (column: `tokenHash`); the
plaintext is shown only once at issuance.

## External users

External users (email did not match `MEMQL_IDENTITY_INTERNAL_DOMAINS`)
are flagged `internal=false` and take the default cluster-wide role,
`reader` (`Store.CreateUserOnFirstLogin` substitutes it when the
caller passes an empty role).

They get no separate workspace. The
`provisionPersonalPartitionOnFirstLogin` automation this section used
to describe was removed with partitioning in #56 and does not exist
in the tree; neither does the
`v1:identity:partitionAccess(role=owner)` grant it stamped. What
bounds an external user today is the per-row check on each concept
they read -- see
[access-model.md](access-model.md#what-the-role-actually-decides).

## Internal users

When `MEMQL_IDENTITY_INTERNAL_DOMAINS` matches the registering email's
domain, the user is flagged `internal=true` and assigned the
cluster-wide `MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE` (default `writer`).
This is captured at registration so policy decisions stay stable
even if the configuration drifts later.

## User-row creation

Users are created in exactly one place: the magic-link
verification path inside the identity service
(`Store.CreateUserOnFirstLogin`). When a fresh email completes a
magic-link flow, the verifier inserts the `v1:identity:user` row and
the matching `v1:identity:identity` row (variant=magic_link)
together. Those two rows are the whole of it -- there is no third
grant row, and no automation runs afterwards to materialise a
workspace.

There is **no** `session.opened` auto-provision automation. An
earlier `bootstrapIdentity` automation existed as a backstop for
legacy external subjects from the pre-identity-service era; it
was retired because every cluster now goes through the magic-
link flow and the automation kept creating phantom rows for
synthetic dev-mode subjects. If you encounter a stale row from
that automation in an existing deployment, hard-delete it
manually -- there is no migration path, since the row was never
something the modern identity model could bind real credentials
to.

## Account deletion

Users request deletion from `/me/settings` in the identity web app,
which reaches the `scheduleAccountDeletion` mutation
(`dsl/identity/mutations.memql`) -- there is no separate `/me/delete`
route. The mutation stamps `deletionScheduledAt` on the user row but
does not hard-delete; an `accountDeletionSweep` cron runs after
`MEMQL_IDENTITY_DELETION_COOLDOWN_DAYS` and performs the cascade:

- User row hard-deleted
- All `v1:identity:identity` / `v1:identity:authSession` rows for
  the user hard-deleted
- Audit / access-request / invitation references to the user are
  tombstoned (`<deleted:hash>`) rather than removed, preserving
  the audit trail

The user can call `cancelScheduledDeletion` any time
during the cooldown to abort the deletion.

## Related

- [access-model.md](access-model.md) -- enforcement layer + role
  spectrum.
- [identity-service.md](identity-service.md) -- env vars, key
  rotation, anti-abuse tuning.
