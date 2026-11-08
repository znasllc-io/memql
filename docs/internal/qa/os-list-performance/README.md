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
