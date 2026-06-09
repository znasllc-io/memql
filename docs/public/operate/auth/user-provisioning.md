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
underlying mutations on `v1:identity:user` / `v1:identity:identity`
/ `v1:identity:partitionAccess`.

## Registration modes

Set via `IDENTITY_REGISTRATION_MODE`. Captured by the first-run
wizard if the env var is unset.

| Mode                 | Who can register                                                        |
|----------------------|-------------------------------------------------------------------------|
| `open`               | Anyone with any email. Default for new clusters.                        |
| `domain_restricted`  | Email must match `IDENTITY_REGISTRATION_DOMAINS`.                       |
| `invite_only`        | No self-registration. Users only enter via admin invitations.           |
| `waitlist`           | Users submit access requests; admins approve into invitations.          |

Mode is read by the identity web app (registration form) and by
the magic-link issuer (rejects new emails when the mode forbids
them).

## Magic-link flow (the primary path)

1. User visits the identity web app and enters their email at
   `/auth/login`.
2. The form posts to `/auth/magic-link`. The handler runs the
   anti-abuse middleware (per-IP rate limit, optional Cloudflare
   Turnstile, disposable-email blocklist, MX-record validation,
   risk score). On rejection an audit event with
   `action=magic_link_blocked` and a `failureReason` is recorded
   and the form returns a generic message.
3. The magic-link issuer mints a single-use token, stores its
   SHA-256 hash on a fresh `v1:identity:magiclink` row, and sends
   the email via the `email` integration plug-in.
4. The user clicks the link, landing at `/auth/complete?token=...`.
5. The verifier consumes the token atomically (sets `consumedAt`),
   resolves the underlying email, and either:
   - **Existing user**: looks up `v1:identity:user` by email,
     issues a new access + refresh token pair, and creates a
     `v1:identity:authSession` row.
   - **New user**: provisions `v1:identity:user` and
     `v1:identity:identity` (`identityType="magic_link"`), then
     issues tokens. Internal-domain users get
     `IDENTITY_INTERNAL_DEFAULT_ROLE`; external users start with
     no cluster role and an owner grant on a fresh personal
     partition.
6. Browser receives the access JWT and starts using it as
   `Authorization: Bearer ...` against bff/voice/etc.

## First-user-is-owner

The first user to register (regardless of mode) is bumped to
cluster `role=owner` so the cluster has a manageable admin from
the start. Subsequent registrations use the configured defaults.

## Invitations

`v1:identity:invitation` is an identity primitive used by two
flows:

- **Guest invites** (CoPresent): a space owner sends a guest a
  link via `SendGuestInviteMsg`. Guests authenticate with
  `Authorization: Guest <token>` (the gRPC stream interceptor's
  guest-aware path).
- **User invitations** (admin / waitlist mode): an admin issues a
  user-targeted invitation. The recipient lands in the registration
  flow with the invitation token pre-bound; on completion the
  identity service stamps the issuing admin's specified role + any
  partition grants.

Tokens are stored as SHA-256 hashes (column: `tokenHash`); the
plaintext is shown only once at issuance.

## Personal partitions for external users

External users (email did not match `IDENTITY_INTERNAL_DOMAINS`)
do not get a cluster-wide role. Instead the
`provisionPersonalPartitionOnFirstLogin` automation creates a
personal partition for them on the first session and stamps a
`v1:identity:partitionAccess(role=owner)` grant against it. From
the user's perspective they own their own workspace; from the
cluster's perspective they have no global visibility.

## Internal users

When `IDENTITY_INTERNAL_DOMAINS` matches the registering email's
domain, the user is flagged `internal=true` and assigned the
cluster-wide `IDENTITY_INTERNAL_DEFAULT_ROLE` (default `writer`).
This is captured at registration so policy decisions stay stable
even if the configuration drifts later.

## User-row creation

Users are created in exactly one place: the magic-link
verification path inside the identity service
(`Store.CreateUserOnFirstLogin`). When a fresh email completes a
magic-link flow, the verifier inserts the `v1:identity:user`,
matching `v1:identity:identity` (variant=magic_link), and
`v1:identity:partitionAccess` rows in one go. The
`provisionPersonalPartitionOnFirstLogin` automation reacts to
the new user-row event and materialises a personal partition
for external users.

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

Users request deletion via `/me/delete` in the identity web app.
The mutation stamps `deletionScheduledAt` on the user row but
does not hard-delete; an `accountDeletionSweep` cron runs after
`IDENTITY_DELETION_COOLDOWN_DAYS` and performs the cascade:

- User row hard-deleted
- All `v1:identity:identity` / `v1:identity:partitionAccess` /
  `v1:identity:authSession` rows for the user hard-deleted
- Audit / access-request / invitation references to the user are
  tombstoned (`<deleted:hash>`) rather than removed, preserving
  the audit trail

The user can call `mutationCancelScheduledDeletion` any time
during the cooldown to abort the deletion.

## Related

- [access-model.md](access-model.md) -- enforcement layer + role
  spectrum.
- [identity-service.md](identity-service.md) -- env vars, key
  rotation, anti-abuse tuning.
