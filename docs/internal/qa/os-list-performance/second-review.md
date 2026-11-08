# Second review: OS lists and live sessions

Reviewed `codex/os-resource-lists-20261107` after feature commit
`4a7e30ead67a5a81defc56bff88a0aa01753bbaf`. All evidence uses local synthetic
fixtures. The database container retained the original 2 CPU / 2 GiB limits;
production data, settings and deployment pins were not changed.

## Reproduced regressions and fixes

| Area | Reproduced failure | Fix and regression coverage |
| --- | --- | --- |
| Training pagination | Approving an already displayed chunk on a second engine shifted a 500-row source page. An offset continuation returned 10/10/4 and skipped one unread row. | The established metadata query narrowed with `paginate(...,10)` returns 10/10/5 under the same concurrent approval, without adding a new authorization surface. Full text is still hydrated through required-domain reads for selected IDs only. Tests also refuse mismatched cursors and callers below the native review's writer floor. |
| Merged lists | A transport failure or overlapping click could reveal the next window before both sources finished. Live removals also let older rows appear ahead of unread newer rows, even before another click. | The session shares one pending advance. The visible prefix remains bounded by what both sources can prove; background filling repairs only the already-requested window and stops on failure. Each source must supply the requested frontier or be exhausted before the next window appears. Tests cover initial partial sources, interrupted retries, overlapping clicks, accepted invitations, shared windows and fresh identities. |
| Final page plus live events | An older row arriving during the last page could be discarded using the previous partial-page boundary. | Install the new continuation boundary before replaying events. A deferred-page test proves the row remains after exhaustion. |
| Session repair and event order | Older repair snapshots could roll back newer rows or restore deleted rows. An ID-only read returning null could erase a newer full event. Equal-version deletion copies could also reappear. | Version/tombstone checks now cover seeds, pages and events; full events trigger one coalesced authoritative follow-up to an outstanding ID-only read. Absent members still disappear, and newer recreations remain allowed. |
| Reopening computed apps | The session repair and pane-resume notification scheduled duplicate reads. | Courtesy resume reuses active work. Real changes during that work still queue a following read. Fake-clock tests exercise both sides of the five-second parked-session grace period. |
| Failed refresh and connection loss | A visible row whose authorized refresh failed still claimed to be live. Concepts could present an interrupted request as an empty or complete list. | Visible failures show generic degraded/disconnected state without exposing refusal details. Unseen refusals stay silent. Concepts retains loaded rows and explicitly reports reconnection. |
| Generic search permissions | A temporary named search query bypassed the generic PII restriction and returned foreign user records to a reader. | Apply the existing generic PII boundary before pagination. Tests cover reader/writer/developer self-only access with canonical and bare actor IDs, an older own row behind 25 foreign matches, and admin/owner full pagination. This was reproduced locally on the undeployed branch. |
| Intrinsic cursor order | A timestamp plus author sort used a timestamp/ID cursor and skipped tied rows. | The cursor records every declared sort key, with matching whitespace, null and bytewise ordering. Tests compare complete SQL traversal with the in-process comparator in both directions. |
| Full-name search | A person displayed using first and last name could not be found by that combined name when display name was empty. | Lower the existing typed text concatenation syntax into the existing substring filter before LIMIT. Tests cover older matches, literal wildcards/quotes, the developer role floor, 320 SQL/IR/evaluator comparisons, nested-field rewriting and cached-plan isolation. Numeric and untyped joins are refused. |

The SDK deletion test exercises its declared `NODE_DELETED` contract. Current
production node removal uses update events; this is not evidence of a
production hard-delete incident.

## Evidence

Failing-before logs and compact test reports are retained in
[second-review/](second-review/), with source and retained-file hashes in
[source-fingerprints.json](second-review/source-fingerprints.json). SDK tests
use controlled promises/event order; app lifecycle tests use fake clocks.
The Training mutation and permission/cursor/search tests use actual PostgreSQL
and independent engine instances where relevant. Assertions target row IDs,
ordering, request counts, role boundaries and payload bounds rather than
machine-dependent elapsed-time thresholds.

Eight browser checks cover interrupted initial and continuation reads in
Concepts at 390/1280 pixels in light/dark mode. All preserve truthful loading
state, avoid horizontal overflow and emit no browser errors. Four inspected
screenshots and all eight results are retained in
[the browser evidence](../../../../clients/os/qa/evidence/second-review-concepts-disconnected.json).

## Validation

The final combined validation results are recorded in
[second-review/verification.json](second-review/verification.json).

- OS: 6,901 tests across 485 files, zero failures or skips; full source/test
  typecheck and production build passed.
- TypeScript SDK: 489 tests and build passed.
- Required PostgreSQL: complete engine and knowledge packages passed; all
  engine subpackages passed (one has no test files). The final complete engine
  rerun took 297.300 seconds; knowledge took 44.918 seconds.
- Language conformance, DSL lint, compatibility and SDK generation checks passed.
- Browser: all eight disconnected-state checks passed.

The first broad database run failed two integration gates. That failed run is
retained in the evidence rather than relabeled as passing. Training's proposed
extra query was removed in favor of narrowing the existing named read, leaving
the authorization coverage rule unchanged. The corpus handler sweep now loads
isolated schemas before typed queries and rejects every load skip, matching
engine bootstrap and strengthening the original handler-resolution check.

Named-query page composition now uses the smaller of the caller and declared
limits. Database tests inspect SQL `LIMIT 10` and EXPLAIN's returned-row count,
walk 10/10/5 rows, prime privileged caches before testing role refusal, and
check independent page sizes. Unit tests retain duplicate/invalid-cap errors
and prove nested query gates and cached declarations remain unchanged.

The second broad run passed those integration gates but exposed a test-fixture
assumption: the shared bounded cache can reject new entries even when insertion
was queued. A controlled admission probe reproduced that behavior. The cache-hit
test now owns a private cache, waits for admission, and positively verifies
three stored pages before requiring zero-SQL replay. Its SQL bounds, cursor and
permission assertions remain intact. The second failed run is retained too;
the final complete engine rerun is reported separately.

The original [performance measurements](README.md) remain historical evidence
from the first implementation pass. This review adds correctness and request
count proof; it does not claim a newly measured production latency improvement.
No PR or deployment is part of this review.
