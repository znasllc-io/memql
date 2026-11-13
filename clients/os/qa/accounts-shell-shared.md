# Accounts shell: shared continuation and entry

Implements the approved accounts-program decisions P38–P41 against engine
`e0b4bf22866146fc88c8c5d7b23eeadf14040cfa`. Profile navigation and administrative
section moves belong to separate changes. Account switching remains dependent on
an account-bound server session; this change does not simulate one.

| Requirement | Implementation | Meaningful verification |
| --- | --- | --- |
| P38, existing Fleet/Statistics pattern | Shared record rows, skeletons, notices and icon buttons; no new layout system | Existing list/Statistics tests and the fixture below |
| P39, arrow replaces More drilldowns | RankedList uses Fleet's ArrowUpRight IconButton, keeping the destination's accessible name and tooltip | Ranked-list action/reachability and Statistics page/Back tests |
| P40, every next page on scroll | LoadMore delegates to NextPage; Concepts, Training Review, Nexus usage, Email recipients/subscribers, Fleet sharing and personal-token pages use the same continuation | Actual feed cursor, error/retry, deduplication, retained rows and caller tests use explicit IntersectionObserver notifications |
| P40, no repeated hidden/stalled reads | Parked windows, hidden browser tabs and recovering feeds disarm; progress permits another visible page, a stalled read requires a new intersection; scope changes isolate old requests | NextPage lifecycle tests; shared live-collection tests |
| P40, personal token authority | Cursor reads still use the native identity endpoint and current bearer; rows append by ID, failures retain their cursor, navigation drops late responses | tokenPaging tests with real nativeIdentity transport and fixture tokens |
| P41, stationary entry and short text | EntryPending renders one short visible line and a quiet opacity pulse; a 160ms bounded exit precedes the destination; reduced motion is immediate | Entry transition cancellation/unmount/reduced-motion tests plus existing OAuth/PKCE/logout and 3-second readiness-bound tests |

Existing live lists retain their data/read authority. The compatibility name
`LoadMore` remains an import, never a rendered paging button. The separate card
carousel arrows remain because they navigate already loaded cards.

Run the established fixture harness with `node node_modules/vite/bin/vite.js
--config qa/vite.config.ts --port 5199 --strictPort`. Then inspect:

- `/accounts-shell-shared.html?view=paging&fail=true&mode=light`: scroll to the
  end, hold during the delayed response, retry once, and reach one end marker.
- The same in dark mode and at 390px; keyboard focus stays on its existing row.
- `?view=entry&hold=true`: stationary visible text and quiet mark; `hold=false`
  shows the fade and destination. Repeat with reduced motion enabled.
- `?view=ranked`: All pages is an up-right icon, reachable by keyboard, with
  destination tooltip; click and Back. Repeat with `empty=true`.
- Existing `statistics.html` and Fleet sharing fixtures retain their geometry.

These are fixtures, not production reads. Rendered browser QA must be recorded
separately; passing unit tests is not a screenshot review.

Recorded local checks: 1,241 tests in 101 files passed; the final lifecycle and
keyboard additions passed 56 targeted tests in four files. Production and test
typechecking, fixture typechecking and the production build passed under Node
22.23.4. Browser inspection is pending.

Independent review follow-up: browser offline now pauses native identity scroll
continuation and online re-arms it without losing rows. Nexus receipt paging
stays inside its disclosure and is disabled while closed; a failed head refresh
retries the head, while a failed older page retries its cursor. All three
regressions failed before the fix and passed afterwards; the focused combined
set passed 24 tests. This supplements, rather than replaces, the pending browser
review.
