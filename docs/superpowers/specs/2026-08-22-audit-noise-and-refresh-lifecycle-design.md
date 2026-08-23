# Audit noise and the refresh-token lifecycle -- an activity log, a skew-proof timer, and reuse detection

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project D of nine)
**Owner:** `component/identity` (refresh, audit, PAT), `sdk/ts`, `clients/portal`

Sub-project D of the 2026-08-22 backlog brief. The operator saw
`session_refresh` rows every couple of seconds in the portal's Audit Trail
and asked three things: what they mean, whether they need to be that
frequent, and whether refresh-token minting is logged at all. The answers
are in section 1; the design fixes the cause, moves the mechanics out of the
trail, and adds the one lifecycle event that is actually missing.

---

## 1. What the rows are, and why there are so many

The action is `session_refreshed` (`component/identity/refresh/rotate.go:271`;
its sibling is `session_refresh_blocked`, `:306`). One row is one
**refresh-token rotation**: a client presented the `memql_refresh` cookie (or
a `refresh_token` grant) to `POST /auth/refresh` (`http/refresh.go:63`) or
`POST /oauth/token` (`http/token.go:231`), and got a new 15-minute access
token and a brand-new refresh token; the previous one stays valid for a
30-second grace window (`rotate.go:41-59`). It is not a heartbeat, and
nothing else writes it: not JWT verification, not a WebSocket reconnect, not
the verifier's five-minute JWKS refresh, not node bootstrap.

The designed cadence is **once per ~10.5 minutes per open tab or SDK
connection** -- the SDK rotates at 70% of the access TTL
(`sdk/ts/src/client/connection.ts:193-203`, default TTL 900 s,
`component/identity/config.go:59`). Two verified defects turn that into
"every couple of seconds":

1. **`computeRotateDelayMs` has no floor and compares clocks across
   machines** (`connection.ts:309-317`): delay = `0.7 * (exp*1000 - Date.now())`.
   `exp` is the identity service's clock; `Date.now()` is the browser's. A
   browser running ahead by a little less than the TTL sees every fresh token
   as nearly expired and rotates every few seconds, forever; ahead by more
   than the TTL, the delay is `0` and it spins at network speed. The retry
   path (`:209-247`) spends a full rotation per attempt at a one-second floor.
2. **The portal refreshes on every navigation** (`clients/portal/src/auth/AuthProvider.tsx:245-258`):
   the session probe calls `authSource.refresh()` in an effect whose
   dependencies include `location.pathname`. The comment says "one
   unconditional request per cold load"; the dependency array says per route
   change. Clicking a row in the Trail writes a row in the Trail.

Three smaller facts make the rows read as noise even at the right cadence:
the rotator never stamps `ActorEmail` / `ActorRole` (`rotate.go:264-275`), so
the Trail's actor column is blank; the PAT verifier writes `pat_auth_accepted`
on **every** PAT-authenticated request (`component/identity/pat/verifier.go:169-179`);
and the audit log is never pruned -- the daily `auditEventRetentionSweep`
only counts (`dsl/identity/logic.memql:266-293`, "MemQL has no delete()").

**Is refresh-token minting logged?** Not as its own event. The first refresh
token is minted inside `mintSessionTokens` and is covered only by
`session_created` (`http/token_session.go:142`); rotation *is*
`session_refreshed`. **Reuse of a rotated token is not detected at all**: the
store keeps one previous hash for the grace window and nothing older, so a
replayed token resolves to no session and becomes
`session_refresh_blocked{session_not_found}` -- indistinguishable from a
stale cookie. `ErrTokenMismatch` is declared (`rotate.go:38`) and never
returned; the package doc's "revoke the whole session on theft" does not
exist (`rotate.go:196-206` says so); the concept description names
`refresh_succeeded` and `refresh_token_theft_detected`
(`dsl/identity/concepts.memql:43`), which nothing emits.

---

## 2. What the tree already has

- **The writer is one function** with one sink chain: `SlogAuditLogger.Log`
  (`component/identity/audit.go:83`) -> `EngineAuditSink.WriteAuditEvent`
  (`audit_db.go:39-89`) -> `mutation createAuditEvent`. Routing a second
  stream is a branch in one place.
- **The Trail is the generic concept walk** over `v1:identity:auditEvent`
  (`clients/portal/src/views/AuditView.tsx:67-82`, `useViewRows.ts:49`,
  page size 100 ascending), with no filter of any kind; the concept walk
  accepts only `order` and `pageSize` (`sdk/ts/src/client/conceptBrowser.ts:32-35`).
  Hiding rows client-side would break its pagination. The admin console's
  `recentAuditEvents` filters by the six-value `category` only
  (`dsl/identity/queries.memql:1318-1329`); `session_refreshed` is
  `category: auth`, the same as a login.
- **`auditEvent.action` is an unconstrained string**; only `category`,
  `targetType` and `outcome` are enums, checked against the Go constants by
  `test/dslconformance/identity_audit_enum_contract_test.go`. There is no
  severity axis.
- **The JWT carries `iat` and `exp`**, both stamped by the identity service.
  Their difference is the TTL on the server's clock; the browser never needs
  to compare its clock with the server's.
- **Hard deletes exist in Go**: `component/node/delivery_store_pg.go` prunes
  the delivery cursor store on a schedule. The identity service has the same
  database access.
- **Sub-project B's composite tier** (`@rowAuthz(owner="<field>", clusterOwner)`,
  epic #4308 task #4312) is what lets a per-user activity log be read by its
  user and by an admin.
- **Sub-project A's new-sign-in notice** (epic #4300 task #4305) is the
  channel a reuse detection can reuse for a security notice.

---

## 3. Decisions

### D1 -- Schedule rotation from the token's own lifetime, with a floor

`delay = max(30 s, 0.7 * (exp - iat)) - (now - receivedAt)`, all measured
from the moment the token arrived. Both timestamps come from the same clock,
so browser skew changes nothing. A token with no `iat` falls back to today's
arithmetic, still floored. The retry path keeps its three bounded attempts,
never below the floor.

### D2 -- Probe the session once per cold load

The `AuthProvider` effect keeps a ref; route changes make no request;
sign-out resets it. Identity's own `/me/*` pages refresh once per page load
(`web/static/app.js:100`) and stay as they are -- page loads are rare.

### D3 -- Two concepts: decisions in `auditEvent`, mechanics in `authActivity`

Three options were considered: a second concept for routine mechanics; one
concept plus a `severity` field plus a server-side filter on the generic
browse walk; and not persisting rotations at all. The owner chose the first.
The filter is new engine, SDK and view-kit machinery (item 2 of epic #4274's
design agenda), and dropping rotations throws away the one signal that shows
which device refreshed when -- what you need when a refresh token is stolen.
Two concepts make the Trail clean by construction, let retention differ, and
keep a session's full story one join on the session id.

### D4 -- Every row names its actor

Email and role are stamped on activity rows and on every audit row the
identity service writes; the blank actor column ends.

### D5 -- Reuse detection, keyed on the retired hash the activity row records

The activity row for a rotation records the hash the rotation retired.
Detection becomes a lookup: a presented refresh token that resolves to no
current session but matches a retired hash is a replay. The session is
revoked, the event is a security signal in `auditEvent`, the user is told.

### D6 -- Activity has real retention; the audit log's sweep is untouched

`authActivity` rows older than `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS`
(default 30) are hard-deleted daily from Go. `auditEvent`'s observe-only
sweep, and whatever compliance story it serves, is out of scope here.

---

## 4. The change

### 4.1 `v1:identity:authActivity`

| Field | Type | Notes |
|---|---|---|
| `occurredAt` | datetime! | |
| `action` | enum(`session_refreshed`, `session_refresh_blocked`, `grace_window_accept`, `pat_auth_accepted`)! | a closed enum, unlike `auditEvent.action` -- this concept has four writers and will not grow by convention |
| `sessionId` | string | `v1:identity:authSession` row id |
| `actorUserId`, `actorEmail`, `actorRole`, `actorIdentityId` | string | always stamped |
| `sourceIP`, `userAgent`, `clientLabel` | string | |
| `outcome` | enum(`success`, `blocked`)! | |
| `failureReason` | string | the rotator's existing reasons: `session_not_found`, `previous_refresh_grace_expired`, `session_revoked`, `session_expired_absolute`, `session_idle_timeout`, `session_max_age_exceeded` |
| `retiredTokenHash` | string | SHA-256 of the refresh token this rotation retired; indexed. A retired token cannot be used, and a hash cannot be reversed |
| `detail` | object | |

`@rowAuthz(owner="actorUserId", clusterOwner)` (B's composite tier). Reads:
`recentAuthActivity` (owner/admin; optional `sessionId` / `actorUserId`
args; `sort occurredAt desc`, `paginate 50`), `authActivityForSelf`,
`authActivityByRetiredHash` (engine-internal, for 4.3). Writes:
`createAuthActivity`.

### 4.2 Routing the writers

`identity.AuditEvent` gains `Stream` (`StreamAudit` default, `StreamActivity`).
`SlogAuditLogger.Log` routes by it; `EngineAuditSink` gets a sibling
`ActivitySink` that writes `createAuthActivity`. Writers that move to the
activity stream: `Rotator.Rotate`'s two rows (`rotate.go:271`, `:306`) plus
the grace-window acceptance that is slog-only today (`:158-163`); the PAT
verifier's per-request row (`pat/verifier.go:169-179`). Everything else stays
on `auditEvent`. The `auditEvent.action` description lists what moved and
drops `refresh_succeeded` / `refresh_token_theft_detected`.
`session_created` gains `detail.refreshTokenIssued=true`, so the first mint is
explicit in the audit log.

### 4.3 Reuse detection

In `Rotate`, when the presented hash matches neither the current nor the
in-grace previous hash: look it up in `authActivity.retiredTokenHash`. On a
hit -- revoke that session (`revokedReason=reuse_detected`, a new enum
value), write `refresh_token_reuse_detected` to `auditEvent` (category
`auth`, outcome `blocked`, the session id, the presenting IP and user agent,
`detail.retiredAt` from the activity row), send the new-sign-in notice as a
security notice ("a sign-in token for your account was replayed; we signed
that device out"), return 401 with `ErrTokenMismatch`. A miss stays
`session_refresh_blocked{session_not_found}` on the activity stream. The
grace window is unchanged: a legitimate mid-rotation retry is a
`grace_window_accept` activity row, not reuse.

### 4.4 Retention

`MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS` (`component: identity`,
`scope: node`, default 30, bounds [1, 365]) registered in
`scripts/secrets/manifest.yaml` (synced to the embedded manifest). A daily
Go-side hard delete of `authActivity` rows older than the window, on the
`delivery_store_pg.go` pattern, with a `memql_auth_activity_pruned_total`
counter. Reuse detection therefore looks back exactly as far as the window;
the default exceeds the 14-day idle timeout and the 30-day refresh TTL, so
a token older than the window is dead on its own.

### 4.5 The Trail and the consoles

The Trail needs no code: it walks `auditEvent`, which no longer contains
mechanics. `authActivity` renders in the concept browser like any concept
and gets a `@displayCard`. The admin console's audit section gains nothing in
this epic; sub-project C's Sessions tab may later link "activity for this
device" through `recentAuthActivity(sessionId)`.

---

## 5. Testing

1. SDK: a +14-minute and a -14-minute browser skew against a 900 s token both
   schedule ~630 s; a 20 s token schedules 30 s (the floor); the retry path
   never fires under 30 s; the existing `authRotation.test.ts` fixture
   (400 ms TTL) is rewritten to the floor rather than deleted.
2. Portal: navigating between three routes after a cold load makes exactly
   one refresh request; sign-out then sign-in makes one more.
3. Identity: a rotation writes one `authActivity` row with actor email and
   role and the retired hash, and no `auditEvent` row; a PAT request writes
   activity only; a grace-window retry writes `grace_window_accept`.
4. Replay of a retired token: session revoked with `reuse_detected`,
   `refresh_token_reuse_detected` on `auditEvent` with IP/UA, the notice
   sent, 401; replay of a token that was never issued stays
   `session_not_found` on the activity stream.
5. Retention: rows older than the window are deleted, newer rows are not,
   the counter moves; the job is idempotent.
6. `session_created` carries `refreshTokenIssued`; the conformance enum test
   covers `authActivity.action` and `outcome`.
7. The Trail fixture (`clients/portal/test`) contains no `session_refreshed`
   row, and the predefined-view guard still passes.

---

## 6. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- cadence and attribution | D1, D2, actor stamping on the rotator's existing rows, `refreshTokenIssued` on `session_created` | nothing -- immediate relief |
| 2 -- the activity log | `authActivity` + routing + reuse detection + retention + docs | B's composite tier (#4312) on `main`; A's notice sender (#4305) for the security notice, else a plain log line until it lands |

One `Closes #N` line per issue.

---

## 7. Out of scope

- Retention or deletion for `auditEvent` (compliance posture; separate
  decision).
- A severity axis or a filter on the generic browse walk (epic #4274).
- Sampling the PAT activity rows (volume is now bounded by the activity
  window; revisit if a PAT-driven loop shows up).
- Tuning the access / refresh TTLs or the grace window.

---

## 8. References

- Code: `component/identity/refresh/rotate.go`, `component/identity/{audit,audit_db}.go`,
  `component/identity/pat/verifier.go`, `component/identity/http/{refresh,token,token_session}.go`,
  `sdk/ts/src/client/connection.ts`, `clients/portal/src/auth/AuthProvider.tsx`,
  `clients/portal/src/views/AuditView.tsx`, `dsl/identity/{concepts,mutations,queries,automations,logic}.memql`,
  `component/node/delivery_store_pg.go`, `scripts/secrets/manifest.yaml`.
- Related: epic #4300 (A: sessions, notices), epic #4308 (B: composite tier),
  epic #4315 (C: the Sessions tab), epic #4274 (dynamic views), memql#4158
  (the cold-load probe).
