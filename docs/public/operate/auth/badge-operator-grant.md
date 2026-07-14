---
audience: public
status: current
area: operate
sinceVersion: 0.13.0
owner: platform
---

# Badge operator grants (shared-terminal attribution)

Products built on the engine commonly run **shared terminals**: a kiosk or
station authenticated with one long-lived station account, operated by many
different humans over a shift. Without help, every write from that terminal
attributes to the station (`actor.userId` = the station user), because the
engine stamps actors server-side and clients must not self-attribute.

The **badge operator grant** (memql#2513) closes that gap with a generic
primitive: a registered badge/device identifier, presented at an
already-authenticated terminal, exchanges for a **short-lived, narrowly-scoped
JWT representing the human operator**. The terminal rotates that grant onto
its live stream, so writes during the operator's window carry the operator's
identity; tap-out or expiry rotates back to the station's own bearer.

## The flow

```
kiosk (station account, bearer T)          identity service
  |                                             |
  |-- POST /auth/badge/grant ------------------>|   Authorization: Bearer T
  |   { "badgeId": "<tap>" }                    |   - verify T (user-class only)
  |                                             |   - SHA-256 lookup -> badge row -> operator
  |                                             |   - rate limit + audit
  |<-- { accessToken, expiresAt, operator } ----|   - mint class="badge" JWT:
  |                                             |       sub          = operator userId
  |                                             |       role_ceiling = T's role
  |                                             |       exp          = now + TTL (default 600s)
  |
  |-- RotateAuth(accessToken) on the live /memql/ws or gRPC stream
  |     ... writes now attribute to the operator ...
  |-- tap-out or expiry: RotateAuth(T) rotates back to the station
```

Renewal is an instant re-tap: call the grant again, rotate again. The sdk/ts
auto-rotation loop already schedules exp-driven rotation client-side.

## Registering badges

Badges are `v1:identity:identity` rows of `identityType="badge"`, carrying
only the SHA-256 hash of the badge/device id. Register and revoke over the
standard stream surface (mirrors worker tokens):

- `CreateBadgeMsg { badge_id, label, owner_user_id? }` -- registers a badge
  for the caller, or (admin-only) for another user, which is the common
  shared-terminal flow: an admin taps each operator's card at a registration
  station. Duplicate badge ids are refused.
- `RevokeBadgeMsg { identity_id }` -- flips `active=false`. Future grants are
  blocked immediately; outstanding grant tokens (short TTL) die at their exp.

## Scoping: attribution without elevation

Two guards make the grant attribution-grade rather than a full session:

1. **Role ceiling.** The grant token carries `role_ceiling` = the terminal's
   own role at grant time. At AccessContext resolution the operator's
   row-resolved role is clamped to at most that ceiling, so an admin badging
   into a writer kiosk acts as a writer. A grant with a missing or malformed
   ceiling fails closed to `reader`.
2. **Per-envelope expiry gate.** Stream rotation is durable and session
   revocation runs at stream-open only, so the engine additionally gates
   every envelope on a badge session against the grant's `exp` (an in-memory
   check, no DB hit). After expiry, work is rejected with
   `badge_grant_expired` while the control frames a client needs to recover
   (RotateAuth, hello, ack, cancel, unsubscribe) stay admitted.

Additional containment:

- **Restricted surface while live.** A badge session is pinned away from
  credential/session/cluster management (worker-token and badge mint/revoke,
  session revocation, guest invites, bundle promotion, node maintenance) for
  the whole grant window -- rejected with `badge_grant_restricted`. Anything
  durable a walked-away kiosk could mint would outlive the TTL containment,
  so none of it is reachable under a grant.
- Grants require a **user-class** terminal bearer -- a grant cannot mint from
  another grant (no chaining), and machine classes (service_account,
  voice_agent, node) are refused.
- Grant attempts are rate-limited per source IP and audited
  (`badge_grant_issued` / `badge_grant_denied` on `v1:identity:auditEvent`,
  with the terminal identity, badge row, and source IP).

## Security posture (read before deploying)

- **Badge ids are physical-world identifiers, not secrets.** An NFC UID can
  be cloned or guessed; the SHA-256 hash in the database is a lookup key, not
  a brute-force-resistant credential store. The system is safe because the
  badge id alone authenticates NOTHING: a grant additionally requires an
  authenticated terminal session, and the blast radius of a stolen badge is
  bounded by the terminal's role ceiling, the short TTL, and revocation.
- **Revocation blocks future grants immediately**; an outstanding grant
  survives until its exp (default 10 minutes). This is the same
  revoke-by-expiry tradeoff class="service_account" tokens document.
- The per-IP rate limiter is in-memory per replica (the same caveat as the
  magic-link limiter).

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `MEMQL_IDENTITY_BADGE_GRANT_TTL_SECONDS` | 600 | Grant lifetime. Request bodies may override per exchange (`ttlSeconds`), capped at 3600. |
| `MEMQL_IDENTITY_BADGE_GRANT_PER_HOUR` | 240 | Per-IP grant-attempt budget on the identity replica. |

The `/auth/badge/grant` endpoint requires HTTPS (same
`MEMQL_IDENTITY_ALLOW_INSECURE_PAIR=1` dev escape as the pairing endpoints).

## Downstream modeling

Product DSL needs nothing special: writer fields defaulting to
`actor.userId` become truthful the moment the operator's grant is rotated in.
A kiosk client typically:

1. Dials `/memql/ws` with the station bearer (subprotocol auth).
2. On badge tap: `POST /auth/badge/grant`, then `rotateAuth(accessToken)`.
3. Decodes `sub` / `exp` from the token for its "signed in as X until T" UI.
4. On tap-out, idle timeout, or `badge_grant_expired` errors:
   `rotateAuth(stationBearer)` (and optionally prompts for a re-tap).

## Naming note

The scheme is deliberately named **badge**, not "operator":
`Authorization: Operator <key>` is the pre-existing master-key scheme
(synthetic cluster owner) and is unrelated.
