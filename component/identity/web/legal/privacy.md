---
documentType: privacy
version: 1.0.0
effectiveDate: 2026-05-04
---

# Privacy Notice

This default Privacy Notice is bundled with the identity service as a
starting point. Cluster operators are expected to replace it with a
document that reflects their own privacy practices, jurisdictional
obligations, and processor agreements before customer-facing launch.

Override at runtime by setting `MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH` to a
filesystem path holding a markdown document with the same front-matter
shape (`documentType`, `version`, `effectiveDate`).

## Information we collect

To authenticate you, the service collects:

- Your email address.
- Identity-credential metadata (the timestamp at which the magic link
  proved you control the address).
- Session metadata: device label parsed from your User-Agent, IP
  address at session creation, last-refresh timestamp.
- An audit trail of authentication-relevant events (login attempts,
  refresh, logout, role changes) with actor and source attribution.

## How we use it

Authentication, session management, audit, and operator-side abuse
prevention. We do not sell your data.

## Retention

- Audit events: retained for the period configured by
  `MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS` (default 365 days).
- Sessions: hard-removed when revoked or when their expiry passes.
- User records: retained until you initiate account deletion. After
  the cooldown configured by `MEMQL_IDENTITY_DELETION_COOLDOWN_DAYS`
  (default 30 days), your record is hard-deleted; references in audit
  rows are tombstoned but the audit trail is preserved.

## Your rights

You can:

- Export your identity-related data at any time from `/me/export`
  (rate-limited per `MEMQL_IDENTITY_DATA_EXPORT_RATE_LIMIT_HOURS`).
- Schedule your account for deletion from `/me/settings`. You can
  cancel during the cooldown window.
- Sign out a single device from `/me/devices`, or revoke every active
  session.

## Contact

For questions about this notice, contact the operator of this cluster.
