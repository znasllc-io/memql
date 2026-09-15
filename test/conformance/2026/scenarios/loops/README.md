# Loop scenarios

These isolated DSL fixtures exercise three feedback shapes: a row rewriting
itself, two rows mirroring one another, and an automation publishing its own
trigger topic. Unlike the sibling product scenarios, these deliberately create
cycles that shipped product definitions must avoid.

Each shape's `scenario.json` contains a domain's source files and the automation
source for each scenario. `TestLoopScenarios` loads them through the strict
production loader and follows actual graph/publish events through the executor.
The prepared trigger filter uses the production expression evaluator and
`TriggerRow` mapping. A converging case must reach a false filter and the exact
expected number of completed runs. Removing its `@loop` must refuse the load.

`expect.counters` declares per-automation deltas for `depth`, `loop_bound`,
`echo`, `row_budget`, and `mode`. Converging scenarios expect zero for every
reason. Stopped scenarios require exactly one appropriate stop and read its
real journal row: terminal code, depth, cap, correlation, parent, and every
ordered chain entry.

The test-only opaque builtins represent Go integrations the graph cannot
inspect. One delegates to a real mutation; another publishes a real event.
Both preserve the run's cause. Their steps are registered as builtin functions,
so the static graph cannot see their feedback edge. These cases must reach the
global depth cap of 16; the visible self-update loop separately reaches its
`@loop(maxDepth=4)` bound.

Run against a migrated test database:

```sh
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='<test database DSN>' \
  go test -count=1 -p 1 github.com/znasllc-io/memql/test/conformance/ -run TestLoopScenarios
```

Without Postgres, the load checks still run and live subtests skip. With
`MEMQL_REQUIRE_DB=1`, a missing database fails. Each live scenario uses fresh
row identifiers and removes its rows and journal entries afterward.
