# @znasllc-io/memql-sdk-core

Notable changes to the client-agnostic runtime core. Entries are newest first.

## Unreleased

### Added

**`materialize` and its parts -- the one way a client creates a file**
(`@znasllc-io/memql-sdk-core/client`, memql#6023). `materialize(ports, request)`
starts a composition, follows it until the file has its Files entry (the first
look 300 ms after the start, the rest one `intervalMs` apart, two seconds by
default), and returns `{ compositionId, fileId, artifactId, name }`. `startMaterialize` and
`followComposition` are its two halves, for a client that follows its own live
feed or polls; `isDescribedMarkdown` says which requests the cluster files empty;
`handOffDescription` gives such a file's description to the Library's revision
workflow as the person (a document-level comment with the `library_file` sources
the Library accepts attached, then a revision request) and returns `revisionId` or
`revisionFailure` without throwing, with the references it could not attach in
`unattached`. `composeMaterialize` gains `requestId`: repeating it returns the
same composition, so mint a new one for every person action. An aborted signal
cancels a composition that is still being made. Stopping never deletes a file
that was already made: it stays in Files, and `materialize` returns it when it is
already in Files at the moment of the stop (a described request then carries
`revisionFailure` "Stopped before the description was sent."), and throws
`MaterializeCancelled` when it is not yet. A described request's id may use
letters, digits, `-` and `_`, at most 111 characters. `fileNameProblem(name,
format)` is the name rule a client checks as a name is typed (no slash, no
control character, not empty, at most 150 characters, something before the
extension) in the words a person reads, and `fileNameStem(name, format)` the
name the cluster files it under (one trailing extension of the format dropped).

**`refusalSentence(error, construct?)`** (`@znasllc-io/memql-sdk-core/client`,
beside `QueryClient`) -- the sentence a person reads out of a refused call. A
refusal arrives as `<construct>: MemQL engine failed to execute query. Details:
compose: ...`; this takes off the call's name `executeNamed` puts in front, the
server's frame and a leading Go package name, once each, and leaves the rest as
the cluster wrote it. `startMaterialize` throws, and `handOffDescription`
reports, refusals in these words.

**`AiChatOptions.level`** -- how much intelligence a chat call needs: `""`
(Auto: the cluster's rules decide), `"fast"`, `"strong"` or `"reasoning"`. It
rides `AiChatMsg.level` beside `provider` on both `aiChat` and `aiChatStream`,
and like `provider` it is omitted from the wire when empty. MemQL OS's Ask
route picker sends a conversation's chosen source as `provider` and its Effort
as `level`.

**`Result.count()` and `CountAnswer`** (memql#6033) -- read the aggregate a
`count` query answers as `{ count, atLeast }`, or `null` when the result is
not a count. The engine reads a count up to its window
(`MEMQL_MEMORY_ENGINE_MAX_WINDOW`, 5,000 by default) and one row past it; a
larger set now answers the window with `atLeast: true`, read as "this many or
more". The window is configurable, so read `atLeast` rather than comparing the
figure with a number you assume. `countConcept` still answers the number
alone.

### Changed

**A `LiveCollection` seed keeps what arrives while it reads** (memql#6033).
A seed replaces the rows on screen with the read's answer, and the pages it
read were answered before anything that arrived during the walk -- so an update
that landed between two pages was overwritten by the older page until the next
reseed, an insert that sorted onto a page already read never appeared, and a
delete came back. Each seed now keeps the last arrival per row from before its
first page until its answer is installed, and replays it onto that answer
through the fold's own rules (a removal removes; a row is judged by the
ordering below, then by `inScope`). Arrivals still fold onto the rows on screen
as they come.

**A `LiveCollection` orders engine rows by their version by default**
(memql#6033). The engine delivers each graph event on its own goroutine and a
read can answer after an event it is newer than, so the last arrival is not
always the newest copy. With no `supersedes` of its own, a collection now
compares the `createdAt` each row and event carries at the top level -- the
time of the row's version, at every digit the wire carries -- and refuses an
arrival older than the row it holds; the same version or a newer one replaces
it, and where either side has no version time the arrival wins as before. A
copy refused as older is refused before `inScope` reads it, so it cannot take a
newer row out of the list either, and a removal always lands. Once a row is
out -- removed by an event that carried a version time, or taken out of the
read's scope by a newer copy -- the collection keeps that time, across seeds
(up to 1,024 rows, oldest forgotten first), so a late OLDER copy does not bring
it back; a copy at that version or a newer one is placed. Pass `supersedes` to order by something else, or to take
every arrival; such a feed keeps no tombstones. The engine's
`graph.node.created` and `graph.node.updated` events carry that `createdAt` to
the microsecond the row keeps (`.created` carried it to the second, `.updated`
not at all).

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
- `AuthoringDiagnostic.code` -- the failure's stable rule id
  (`lower_unknown_field`, an `annotation_*` refusal, a retired form's rule), or
  `""` when it carries none (memql#5435). It used to reach the client only
  inside `error`'s text, in brackets at the end; key on the field instead, since
  the id is stable and the wording is not. A consumer that builds an
  `AuthoringDiagnostic` literal (a test fixture) now supplies `code` too.

### Not changed, and checked

The **Go SDK carries no rotation scheduler**, so it cannot have the same
defect. `sdk/go/client/dispatcher.go`'s `RotateAuth` is the wire call alone --
it swaps a bearer on an open stream and decides nothing about when. The cadence
belongs to the cockpit's background token refresher, which lives in
`github.com/znasllc-io/memql-cockpit`; if that scheduler compares `exp` against
a local clock it has the same bug, and the fix belongs in that repository. There
is nothing to change here.
