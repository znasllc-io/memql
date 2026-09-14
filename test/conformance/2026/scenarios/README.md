# Scenarios

Whole automations as the product ships them: the decide-and-apply sweeps, the
forge state machine and the deployment pipeline, each run over a real database
by `test/conformance/scenarios_db_test.go`. A scenario names the automations
and the mutations it uses. It never copies them, because a copy can pass while
the shipped automation breaks. The same scenarios run before and after the
body language's migration, over the legacy bodies and then over the statement
bodies, so they hold the migration to what the product does to rows.

## A suite

A suite is a directory holding one `scenario.json` and nothing else. The
verdict runner reads only `expect.json` directories, and a scenario directory
has none, nor any `.memql` file.

```json
{
  "description": "What the suite covers.",
  "scenarios": [
    {
      "name": "model-pull-stale-sweep",
      "description": "What this scenario shows.",
      "seed": [
        { "mutation": "createModelPull", "args": { "pullId": { "$id": "stale" }, "requestedAt": { "$ago": "PT10M" } } }
      ],
      "fire": [
        { "automation": "workerModelPullStaleSweep", "schedule": true }
      ],
      "expect": {
        "rows": [
          { "concept": "v1:worker:modelPull", "id": { "$id": "stale" }, "payload": { "status": "failed" } }
        ]
      },
      "dryRun": { "fire": 0, "writes": 1 },
      "resume": { "fire": 0, "failAt": 2 }
    }
  ]
}
```

- **`seed`** writes rows through shipped mutations, as a cluster owner at
  internal origin, or as the actor `"as": { "userId": ..., "role": ... }`
  names.
- **`fire`** is the steps after the seeds, in order. A step is either an
  automation run or a write through a mutation (the `seed` form). An
  automation runs on a schedule tick (`"schedule": true`) or on an event. The
  event is either the graph event the step before published for a row,
  `{ "action": "updated", "concept": "v1:forge:request", "id": ... }`, or,
  for a topic no write publishes, one the scenario publishes itself,
  `{ "topic": "deploy.requested", "payload": { ... } }`. A run must end
  `completed` unless the step names another `status`.
- **`actions`** answers the calls the automations' actions dispatch, keyed by
  capability, and by script for a `shell.script` call
  (`"shell.script:deploy.gate"`). A stub answers every call, so no script
  runs.
- **`expect.rows`** are rows read back from the store. Each is named by `id`,
  or found by `where` (the rows of the concept whose latest version holds
  those fields: `count` of them, one by default). A row holds `payload`'s
  fields, and a row named by id can list its whole `history`, oldest version
  first. **`expect.actions`** is every dispatched call, in order.
- **`dryRun`** replays the scenario on fresh rows and runs one fire through
  the sandbox. Its manifest must hold `writes` intercepted writes, and no row
  may change.
- **`resume`** replays the scenario, fails the `failAt`-th step of one fire's
  run once, before the step runs, and resumes the run from its journal. It
  must end with the same rows and calls.

Values: `{ "$id": "x" }` is a seeded id, tagged per run and per variant so
runs share a database safely. `{ "$ago": "PT10M" }` is the instant that long
before now. A `legacy` entry on a `where` row gives its count under the legacy
bodies, with the defect that makes it differ. The runner refuses the entry
once the tree is in statements.

A scenario naming an automation or a mutation the tree no longer ships fails,
with or without a database (`TestScenarioFilesNameShippedConstructs`).
