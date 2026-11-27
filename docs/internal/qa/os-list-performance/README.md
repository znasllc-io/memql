---
title: "OS resource-list execution evidence"
audience: internal
status: stable
area: build
sinceVersion: 0.31.23
owner: znas
---

# OS resource-list execution evidence

Measured locally on 2026-11-07, comparing baseline `8b7b3a483` with this branch.
This is reproducible development evidence, not a production measurement.

The subsequent [branch review](second-review.md) records additional pagination,
session, authorization and live-update regressions, their failing-before
evidence and validation after the fixes. The measurements below remain the
original performance comparison; they were not rerun during that review.

The same disposable PostgreSQL/Timescale database held 2,000 pipeline runs,
each with a deterministic 14 KiB policy snapshot. The container used the CI
Timescale image, 2 CPUs and 2 GiB memory. Both runs used fresh engine reads
(result cache bypassed), a warmed database, nine samples per query, and Go GC
before each sample. The database and fixture rows were unchanged between the
two runs. SQL EXPLAIN was collected separately from the timed samples.

| Read | Median | Slowest of 9 | Shaped response | SQL page rows returned to Go | Median Go allocation |
| --- | ---: | ---: | ---: | ---: | ---: |
| Baseline, existing full 50-run query | 347.31 ms | 370.97 ms | 778,496 B | 2,000 | 220,132,496 B |
| Branch, same full 50-run query | 116.15 ms | 120.07 ms | 778,496 B | 50 | 14,616,168 B |
| Branch, 10-run summary browse | 105.82 ms | 123.36 ms | 9,600 B | 10 | 3,263,544 B |
| Branch, 10 older search matches | 221.87 ms | 228.41 ms | 9,690 B | 10 | 3,292,352 B |

The existing payload timestamp sort previously materialized every candidate
up to the engine window, sorted in Go, and returned no continuation cursor.
The branch pushes the declared text/datetime ordering and continuation into
SQL. The browser's browse query returns ten summaries and a cursor. Compared
with the baseline query, this fixture's initial list response is 98.8% smaller
and its Go allocations are 98.5% lower. The old UI requested up to four pages,
but the baseline query emitted no cursor, so it actually stopped after 50.

These numbers do **not** imply that PostgreSQL reads only ten inputs. EXPLAIN
shows latest-row payload probes across the candidate set before sorting;
literal substring search also examines candidate text. No new database index,
larger container, production setting or resource allocation was introduced.
The demonstrated bound is the SQL result page, Go materialization and shaped
response. A production-shaped distribution, concurrent writes and cold
database cache can change elapsed time substantially.

## Regression coverage

`component/memql/os_resource_browse_db_test.go` executes the actual named
queries against PostgreSQL. It covers 10/10/5 traversal, timestamp ties,
continuation on a second independent engine, foreign-owner exclusion,
case-insensitive search for older records with literal `%`/`_`, revoked fleet
records, lightweight presence, response byte budgets, heavy-field exclusion,
and SQL `LIMIT 10` before materialization. A real `createPipelineRun` /
`updatePipelineRun` sequence verifies event version precision, receiving-engine
cache invalidation, updated server facets, and full detail availability.

`component/memql/payload_cursor_db_test.go` compares SQL cursor order with the
existing in-process comparator in both directions, including missing/null
values, Unicode whitespace, accents, case and ties. Historical versions and
ownership changes cannot resurrect old matches. Invalid cursors and sort
mismatches are refused; unsupported/unbound sorts retain their old behavior.
Existing intrinsic-cursor DB regressions remain included.

`includes_fold_test.go` and the SQL/IR/evaluator agreement matrix exercise the
new optional `includes(needle, true)` form. The language corpus, generated
grammar and SDK are checked independently. Wall-clock timing is informational;
row, cursor, payload, authority and ordering assertions are deterministic.

## Reproduce

Use a disposable database with the CI-required extensions and apply the normal
test migrations. Set `MEMQL_REQUIRE_DB=1` so a missing database fails instead
of skipping. `MEMQL_DATABASE_DSN` must name that disposable database.

Copy `component/memql/os_list_performance_test.go` unchanged into a detached
baseline checkout. Run the following in that checkout:

```sh
MEMQL_OS_LIST_BASELINE=1 MEMQL_OS_LIST_REVISION=8b7b3a483 \
MEMQL_OS_LIST_EVIDENCE_FILE=/absolute/path/baseline.json \
go test ./component/memql -run '^TestOSListPerformanceEvidence$' -count=1 -v
```

Then run in this branch, using the same database:

```sh
MEMQL_OS_LIST_REVISION=working-branch \
MEMQL_OS_LIST_EVIDENCE_FILE=/absolute/path/branch.json \
go test ./component/memql -run '^TestOSListPerformanceEvidence$' -count=1 -v
```

The opt-in fixture uses fixed IDs and conflict-ignore inserts, so it reuses
identical rows. It intentionally leaves those rows for the second run; discard
the test database afterward. The normal test suite skips this harness unless
the evidence output path is set. Raw samples, allocation counts, SQL and
EXPLAIN plans are in `baseline.json` and `branch.json`; execution logs accompany
them. These artifacts contain synthetic fixture data only.

## Library database-work follow-up, 2026-11-26

This follow-up targets database work that the original ten-row response bound
above did not remove. A read-only investigation of memql-znas found a saturated
2-vCPU database node (Standard_D2as_v7, about 8 GiB node RAM, PostgreSQL limited
to 4 GiB). The data and WAL volumes were each 64 GiB Premium SSD v2 at 3,000
IOPS / 125 MB/s. The sampled node CPU was 99.39% busy, with zero I/O wait and
both disks below 1% busy. This sample supports a CPU/planning/lock problem,
not the initial suspicion of storage latency. PostgreSQL had 82–93 sessions
against a 400-connection limit; raising that limit was not indicated.

The sampled production folder plan enumerated 14,041 artifact candidates to
return eleven: planning 302.672 ms, execution 3499.297 ms. One latest-row lookup
spent 392.92 ms planning and 14.517 ms executing. These are representative SQL
measurements, not browser end-to-end timings. Nine sites and nine packages
made deployable-directory overfetch secondary in that sample; its existing
multi-source reducer still reads its authorized metadata population.

The local regression fixtures use the CI TimescaleDB image with
`random_page_cost=4`, JIT enabled and 256 MiB shared buffers, without a CPU cap.
Fourteen time chunks hold 14,000 artifacts; the point fixture holds 7,000
versions of one ID across fourteen chunks. Existing catalog indexes and
additional fixture chunks are present. Representative warm runs:

| Read | Before | After | Database buffer work |
| --- | ---: | ---: | --- |
| Ten newest Library artifacts | 222 ms | 9.4 ms | 44,005 → 54 execution buffers |
| Selected run's artifacts | whole-artifact enumeration | 9–10 ms | about 420–520 execution buffers |
| Oldest-first Library page | whole-artifact enumeration | 8.9 ms | 348 execution buffers |
| Latest single ID | 12.1 ms | 0.8 ms with reused plan; 4.6 ms first call | 3,051 → 60 total planning + execution buffers |

Timing varies by machine and concurrent work; tests assert the relative buffer
work and exact rows, rather than millisecond deadlines. The point-read totals
include planning buffers because removing repeated planning is its purpose.
The function's internal planning appears under execution in EXPLAIN, so
comparing only top-level execution buffers would be misleading.

Reproduce against a disposable database with the normal migrations:

```sh
MEMQL_REQUIRE_DB=1 go test ./component/memql -count=1 -v \
  -run 'Test(LibraryPagesAndRunFilesDoNotReadTheWholeLibrary|LatestPointReadAvoidsHistoricalPayloadPlanning)'
MEMQL_REQUIRE_DB=1 go test ./integrations/library -count=1 -v -run 'TestLibraryBrowse'
```

Set `MEMQL_DATABASE_DSN` to that disposable database. Additional regressions
cover current versions, changed owners/concepts/folders, timestamp ties,
continuation on another replica, sparse file-type/date matches, private file
sizes, request cancellation, and concurrent observation replacement. The Files
browser requests one derived ten-row page rather than fetching the entire
place for Name, Kind, Size or Added filters. Facets reduce a single authorized
source read instead of walking 500-row pages. These reductions still cost the
matching population on the server and fail above 100,000 source rows.

Validation completed across 312 default-tag Go test packages with
`MEMQL_REQUIRE_DB=1` and separate databases per package, 8,106 OS tests and
491 TypeScript SDK tests. All seven node variants built; OS build/type checks,
Go vet, module boundaries, SDK generation, DSL lint, concept snapshots and
the authoring compatibility check passed. Database suites used CI planner
defaults; the two performance fixtures explicitly use production settings.
Two Calls cases that timed out during fixture writes passed isolated retries.
The campaign scenario was rerun after supplying the active account owner and
explicit Email allowance required by the account hierarchy. UI behavior was
checked through fixtures; live browser latency and production rollout remain
unverified.

Production resources were not changed during this work. After the normal
manual release, compare matched fifteen-minute windows using
`memql_query_duration_seconds`, PostgreSQL/node CPU and wait events, and actual
folder/pipeline interactions. The measured local improvement is not evidence
that production has already improved. Decide on a CPU increase from residual
sustained CPU pressure and user-visible latency after that comparison; current
evidence does not justify increasing disk IOPS or the connection limit.
