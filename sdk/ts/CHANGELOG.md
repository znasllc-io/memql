# @znasllc-io/memql-sdk-core

Notable changes to the client-agnostic runtime core. Entries are newest first.

## Unreleased

### Changed -- BREAKING for anyone importing the rotation helpers directly

**Bearer auto-rotation is scheduled from the token's own lifetime, with a
30-second floor** (memql#4326).

`computeRotateDelayMs` previously computed `0.7 * (exp * 1000 - Date.now())`.
`exp` is stamped by the identity service's clock and `Date.now()` is the
browser's, so the subtraction compared two different clocks. A browser running
ahead by a little less than the access-token TTL saw every freshly minted token
as nearly expired and rotated every few seconds, indefinitely; ahead by more
than the TTL, the delay was `0` and it rotated at network speed. Each rotation
is a real refresh-token rotation server-side, so the visible symptom was an
audit trail filling with `session_refreshed` rows.

The schedule now only ever subtracts SAME-CLOCK pairs:

    delay = max(floor, fraction * (exp - iat) - (now - receivedAt))

`exp - iat` is the token's lifetime on the server's clock; `now - receivedAt` is
elapsed time on this machine's clock. Browser skew cancels out entirely.

- `decodeJwtExp(jwt): number | null` is replaced by
  `decodeJwtLifetime(jwt): JwtLifetime | null`, returning `{ iat, exp }`. A
  token with a missing, non-numeric, or nonsensical `iat` (at or after `exp`)
  reports `iat: null` and falls back to the old wall-clock arithmetic -- still
  floored, which is what bounds the damage.
- `computeRotateDelayMs(lifetime, receivedAtMs, nowMs, fraction?, floorMs?)`
  replaces `computeRotateDelayMs(expSeconds, nowMs, fraction?)`.
- New: `remainingLifetimeMs`, `DEFAULT_ROTATE_FRACTION`,
  `DEFAULT_ROTATE_FLOOR_MS`, and the `JwtLifetime` type.
- The retry path after a failed rotation is floored at the same bound. It used
  to floor at one second, so a refresh outage against a short-lived token became
  three requests a second -- the retry path amplifying the storm the scheduled
  path was fixed to stop.
- An already-expired token now waits the floor rather than rotating
  immediately. Rotating instantly does not help (the refresh either succeeds in
  thirty seconds or it does not) and spinning is the failure the floor exists to
  prevent.

### Added

- `ConnectionAuth.rotateFloorMs` -- an explicit override for the rotation floor,
  defaulting to 30 000 ms. Intended for a harness driving deliberately
  short-lived tokens; lowering it in a browser re-opens the storm the floor
  closes. A non-positive or non-finite value is ignored rather than honoured.

### Not changed, and checked

The **Go SDK carries no rotation scheduler**, so it cannot have the same
defect. `sdk/go/client/dispatcher.go`'s `RotateAuth` is the wire call alone --
it swaps a bearer on an open stream and decides nothing about when. The cadence
belongs to the cockpit's background token refresher, which lives in
`github.com/znasllc-io/memql-cockpit`; if that scheduler compares `exp` against
a local clock it has the same bug, and the fix belongs in that repository. There
is nothing to change here.
