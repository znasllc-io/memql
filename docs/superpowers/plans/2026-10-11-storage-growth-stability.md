# Storage Growth Stability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop an idle MemQL installation from filling its database with its own bookkeeping, make retention actually run, and replace ingress-nginx's bare 503 with a maintenance page.

**Architecture:** Five independent engine changes, each in the package that already owns the behaviour: a continuous superseded-version purge in the topology reconciler (`component/node`), held journals for scheduled runs (`component/automations`), bounded retention statements (`integrations/work`, `component/logstore`), log-level demotions, and an `error-pages` subcommand wired as every front-door Ingress's `default-backend`.

**Tech Stack:** Go 1.27 (multi-module workspace), bun + pgdriver over PostgreSQL 16 + TimescaleDB 2.29, kustomize manifests under `deploy/k8s`, ingress-nginx annotations.

**Spec:** `docs/superpowers/specs/2026-10-11-storage-growth-stability-design.md`

## Global Constraints

- Every new or changed SQL statement on a retention/purge path is bounded (a LIMIT and, where it scans history, a constant `createdAt` window) so it finishes well inside the request pool's 8 s `statement_timeout` and pgdriver's 10 s read deadline on a table of 4 million `"MemoryNodes"` rows.
- Never delete the latest version of any id. Purges touch only the closed concept lists named in this plan.
- Tests: `make test` for unit tests. Db-gated tests: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<real Postgres+TimescaleDB> go test -count=1 <package>`. Never `go test ./...` (CLAUDE.md, memql#4032).
- Stage files by explicit path (`git add <file>`), never `git add -A` or `git add .`.
- No emojis in code, comments, commits or docs. Comments follow the surrounding style: they state why, name the issue or incident, and record measurements.
- Pre-release rule: no compatibility shims for Go seams; change callers in the same commit.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- A heartbeat concept id whose ONLY versions are older than the grace window: its latest version must survive (Task 1, `TestStatusHistoryPurgeKeepsLatestEvenWhenOld`).
- A scheduled automation whose only effect is a raw-SQL builtin (retention sweeps, purges): it writes no journal; that is the intended trade-off, and a FAILURE of such a run must still be journaled (Task 2, `TestScheduledRunFailureIsJournaled`).
- A `Host` header carrying a port or upper case (`OS.Memql.Example:443`) must still classify as a platform host (Task 6, `TestClassifyHost`).
- An API client (`Accept: application/json`, or the `api.` host) must get JSON, not HTML, with status 503 (Task 6, `TestErrorPagesJSONForAPIClients`).
- An archived log day whose chunk does not lie wholly inside the day must still be emptied by the batched-delete fallback (Task 4, `TestSweepFallsBackToDeleteWhenNoWholeChunk`).

---

### Task 1: Continuous superseded status-history purge

**Files:**
- Create: `component/node/status_history_purge.go`
- Create: `component/node/status_history_purge_test.go`
- Create: `component/node/status_history_purge_db_test.go`
- Modify: `component/node/reconciler.go` (the leader tick that already calls `sweepReadinessRowsOfStoppedNodes`; add the new sweep beside it, and a `lastStatusHistoryPurge time.Time` field on `TopologyReconciler`)
- Modify: `component/metrics` (add `StatusHistoryVersionsPurged(concept string, n int64)`, a counter `memql_status_history_versions_purged_total{concept}`, next to `ModuleReadinessRowsPurged`)

**Interfaces:**
- Produces:
  - `var statusHistoryConcepts = []string{"v1:worker:registration", "v1:cluster:node", "v1:platform:moduleReadiness"}` (use the `memorynodes.Concept*` constants where they exist)
  - consts `statusHistoryGrace = time.Hour`, `statusHistoryPurgeInterval = 10 * time.Minute`, `statusHistoryBatch = 500`, `statusHistoryStatementTimeout = 7 * time.Second`. There is no run budget: each leader tick issues at most ONE batch statement, round-robin over the closed list, continuing on following ticks while any concept's last batch came back full and resting for `statusHistoryPurgeInterval` once every concept's last batch is short (execution ruling: the first draft's 25 s on-tick budget exceeded the reconciler's 20 s liveness grace)
  - `type statusHistoryPurger interface { purgeSuperseded(ctx context.Context, concept string, olderThan time.Time, limit int) (int64, error) }`
  - `type sqlStatusHistoryPurger struct{ db *bun.DB }` built from the same `r.dbGetter()` handle `purger()` uses
  - `func (r *TopologyReconciler) sweepStatusHistory(ctx context.Context)`, called from the leader tick

- [ ] **Step 1: Write the failing unit tests** in `status_history_purge_test.go` with a fake purger:
  - `TestStatusHistorySweepRunsAtMostOncePerInterval`: two calls 5 minutes apart under a fake clock make one round of purges; a third at +11 minutes makes a second.
  - `TestStatusHistorySweepLoopsUntilABatchComesBackShort`: the fake returns 500, 500, 12 for the first concept; over successive leader ticks that concept is called three times before the round rests.
  - `TestStatusHistoryTickIssuesAtMostOneStatement`, `TestStatusHistoryConceptsTakeTurnsWhileTwoHaveBacklogs` and `TestStatusHistoryRoundKeepsOneCutoff`: one statement per tick, concepts alternating while more than one has a backlog, and one cutoff for the whole round.
  - `TestStatusHistorySweepLogsAndContinuesOnError`: an error for one concept is logged at Warn and the other concepts still run.
- [ ] **Step 2: Run them and see them fail**
  Run: `go test -count=1 -run StatusHistory github.com/znasllc-io/memql/component/node/...`
  Expected: FAIL (undefined: `statusHistoryPurger`).
- [ ] **Step 3: Implement** `sweepStatusHistory` and `sqlStatusHistoryPurger.purgeSuperseded`. The statement, one per batch:

```sql
DELETE FROM "MemoryNodes" m
USING (
  SELECT n.id, n."createdAt"
  FROM "MemoryNodes" n
  CROSS JOIN LATERAL (SELECT 1 AS found FROM "MemoryNodes" newer
                       WHERE newer.concept = n.concept AND newer.id = n.id
                         AND newer."createdAt" > n."createdAt" LIMIT 1) x
  WHERE n.concept = ? AND n."createdAt" < ?
  LIMIT ?
) d
WHERE m.concept = ? AND m.id = d.id AND m."createdAt" = d."createdAt"
```

  "Has a newer version" is a LATERAL probe with `LIMIT 1`, not an `EXISTS` (execution ruling, measured): an `EXISTS` is planned as a semi join that hashed every version of the concept for each batch on the incident's shape, while a subquery with a `LIMIT` cannot be pulled up and runs once per candidate as an index probe. Gate on the interval exactly as `sweepReadinessRowsOfStoppedNodes` does; each call gets its own `context.WithTimeout(ctx, statusHistoryStatementTimeout)`; log one Info line per sweep with the per-concept totals when any were deleted; feed the metric. The header comment records the 2026-10-11 incident (738,626 superseded registration versions for 13 workers), why there is no archive, and why running continuously matters (space freed inside the current chunk is reused; space freed in a closed chunk is not).
- [ ] **Step 4: Write the db test** `status_history_purge_db_test.go` (copy the setup of `readiness_row_purge_db_test.go`):
  - `TestStatusHistoryPurgeDeletesOnlyOldSuperseded`: for `v1:worker:registration` id A insert versions at now-3h, now-2h, now-10m; purge with `olderThan = now-1h` deletes exactly the two old ones; the now-10m version remains.
  - `TestStatusHistoryPurgeKeepsLatestEvenWhenOld`: id B with versions at now-5h and now-4h: only the now-5h version goes; now-4h (the latest) stays.
  - `TestStatusHistoryPurgeLeavesOtherConcepts`: a `v1:identity:identity` id with two old versions is untouched.
- [ ] **Step 5: Run unit and db tests; see them pass**
  Run: `go test -count=1 -run StatusHistory github.com/znasllc-io/memql/component/node/...` then the same with `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=...`.
  Expected: PASS, and the db run reports the three cases as run, not skipped.
- [ ] **Step 6: Commit** `fix(node): purge superseded status history continuously` naming the three files, `reconciler.go` and the metrics file.

### Task 2: A scheduled firing that writes nothing leaves no journal

**Files:**
- Modify: `component/automations/executor.go` (the block that calls `journal.openRun`, today around line 778)
- Modify: `component/automations/journal.go` (header comment: add the scheduled-run hold beside "A SANDBOXED DRY-RUN WRITES NOTHING")
- Modify: `dsl/work/mutations.memql` (`createWorkRun` takes an optional `heartbeatAt`, so a held run that opens late opens live; execution ruling after review: a held open replayed stale heartbeats, which a second replica could read as a dead run)
- Create: `component/automations/journal_scheduled_hold_test.go`

**Interfaces:**
- Consumes: `heldWrites`, `(*workJournal).release()`, `(*workJournal).openRunArgs`, `common.ContextWithWriteObserver` (all exist; see `logic_statements.go` around line 160 for the identical pattern).
- Produces: `func holdsScheduledJournal(a *Automation, triggeredBy string, adopt *RunAdoption) bool`, true exactly when `triggeredBy == "schedule" && adopt == nil && !a.JournalRequired`.

- [ ] **Step 1: Write the failing tests** with the fake journal executor the other journal tests use (`journal_test.go`):
  - `TestScheduledRunWithoutWritesLeavesNoJournal`: a schedule-triggered automation whose single step performs no graph write completes; the fake recorded zero `createWorkRun`, `createWorkStep`, `updateWorkStep` and `updateWorkRun` calls.
  - `TestScheduledRunThatWritesIsJournaledInOrder`: the step fires the context's write observer once; recorded calls start with `createWorkRun`, followed by the step intent and receipt and the closing `updateWorkRun`, in that order.
  - `TestScheduledRunFailureIsJournaled`: the step returns an error without writing; `createWorkRun` and a closing `updateWorkRun` carrying `status: "failed"` are recorded.
  - `TestScheduledJournalRequiredIsNotHeld`: the same no-write automation with `JournalRequired: true` records `createWorkRun` at open.
  - `TestEventRunJournalUnchanged`: the no-write automation triggered by `event:graph.node.created...` records `createWorkRun` at open.
- [ ] **Step 2: Run them and see the first and third fail**
  Run: `go test -count=1 -run 'ScheduledRun|ScheduledJournal|EventRunJournal' github.com/znasllc-io/memql/component/automations/...`
- [ ] **Step 3: Implement.** When `holdsScheduledJournal(...)`, replace `journal` with a copy (`h := *journal`) whose `hold` opens the run with `h.write(openCtx, "createWorkRun", h.openRunArgs(automation, exec, triggeringEvent, parentCause))`, install `common.ContextWithWriteObserver(ctx, func(string, string) { h.release() })`, and skip the immediate `openRun`. On the failure path, call `release()` before the run record closes so the failure is written. A held run opens live: the open stamps `heartbeatAt`, pulses before the open are dropped rather than replayed, and replayed receipts are restamped (`TestScheduledJournalOpensLiveAndNeverReplaysStaleHeartbeats`). Leave every other path byte-identical. Demote `starting automation execution` to Debug in the same file (Task 5 owns the rest of the log changes; this line is here because the hold changes what that line used to promise).
- [ ] **Step 4: Run the package's unit tests and its db-gated journal tests; see them pass**
  Run: `go test -count=1 github.com/znasllc-io/memql/component/automations/...` and the same with `MEMQL_REQUIRE_DB=1 ... -run 'Journal'`.
- [ ] **Step 5: Commit** `fix(automations): a scheduled firing that writes nothing leaves no journal`.

### Task 3: Work retention selects candidates in bounded day windows

**Files:**
- Modify: `integrations/work/operational_retention.go` (`operationalRetention`, and `retireVerified`'s version load and delete guard)
- Modify: `integrations/work/sweep.go` (`RetentionSweep` runs the journal and operational halves independently and joins their errors)
- Modify: `integrations/work/operational_retention_db_test.go`, `integrations/work/operational_retention_policy_test.go`

**Interfaces:**
- Produces: consts `retentionCandidateWindow = 24 * time.Hour`, `retentionCandidateLimit = 100`, `retentionRunBudget = 4 * time.Minute`; each policy gets a fair share of what remains of the budget (remaining time divided by the policies left, unused time rolling forward), and reports `BudgetExhausted` for itself.
- Execution rulings: EVERY statement of the pass is bounded, not only candidate selection (production's 2026-10-10 sweep found 1,045 candidates and then timed out inside `retireVerified`; the per-run version load became a LATERAL children load, 2.2-8 s to 9 ms seeded). An empty window skips ahead to the day of the next stored version. The goal policy's read probe and delete guard stay O(run table) this release (0.34 s warm, 1.26 s cold at 600k run versions, documented in the header); its partial index is a release-2 migration.

- [ ] **Step 1: Write the failing db test** `TestOperationalRetentionWalksDayWindows`: 250 eligible scheduled runs spread over five days older than the cutoff, archive fake wired; one pass retires all 250 and every statement issued carries both a lower and an upper `createdAt` bound (assert through the query recorder the existing db tests use, or by asserting the result's per-window counts if no recorder exists).
- [ ] **Step 2: Write** `TestOperationalRetentionStopsAtBudgetAndResumes`: with a fake clock advanced 2 minutes per window, a pass stops after the window that crosses 4 minutes; a second pass retires the remainder.
- [ ] **Step 3: Run them and see them fail.**
- [ ] **Step 4: Implement.** For each policy: find the oldest candidate day (`SELECT min("createdAt") FROM "MemoryNodes" WHERE concept = ? AND "createdAt" < ?`, bounded by the cutoff), then walk day windows `[start, start+24h)` up to the cutoff, running the existing candidate query with an added `n."createdAt" >= ?` lower bound and `LIMIT 100`, retiring each batch with the existing `retireVerified`, repeating a window while it returns a full batch, until the cutoff or the 4-minute budget. Unchanged: the predicates, the archive-before-delete rule, the guards inside `retireVerified`.
- [ ] **Step 5: Run the package's unit and db-gated tests; see them pass.**
- [ ] **Step 6: Commit** `fix(work): bound retention candidate selection to day windows`.

### Task 4: The log sweep drops an archived day's chunk

**Files:**
- Modify: `component/logstore/sweep.go` (`Run`, `rowSource` interface, `pgSource`)
- Modify: `component/logstore/sweep_test.go`, `component/logstore/store_db_test.go`

**Interfaces:**
- Produces: `rowSource.dropDay(ctx context.Context, day time.Time) (chunksDropped int64, err error)`; `Report.ChunksDropped int64` (json `chunksDropped`, camelCase like the reply's other fields; execution ruling, the first draft spelled it `chunks_dropped`).

- [ ] **Step 1: Write the failing unit tests** in `sweep_test.go` with the fake source:
  - `TestSweepDropsArchivedDayChunk`: after a successful archive of day D the sweep calls `dropDay(D)`; when the fake then reports no rows left for D, `deleteDay` is not called and `RowsDeleted` equals the archived count.
  - `TestSweepFallsBackToDeleteWhenNoWholeChunk`: `dropDay` returns 0 and rows remain; the sweep calls `deleteDay` until empty.
  - `TestSweepDoesNotDropBeforeArchive`: an archive failure for D leaves `dropDay` uncalled.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** `pgSource.dropDay` as `SELECT count(*) FROM drop_chunks('log_line', older_than => $2::timestamptz, newer_than => $1::timestamptz)` with `$1 = day`, `$2 = day + 24h`; then check `SELECT EXISTS (SELECT 1 FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2)` and fall back to `deleteDay` only when it is true. The header comment records the 2026-10-10/11 failures (archived, then the DELETE over a compressed chunk hit the statement timeout) and why dropping an archived whole chunk keeps "no archive, no delete".
- [ ] **Step 4: Write the db test** `TestPgSourceDropDayDropsTheDaysChunk` in `store_db_test.go`: insert rows two days back, call `dropDay`, assert the chunk is gone from `timescaledb_information.chunks` and no rows remain for that day, and rows of the neighbouring day are untouched.
- [ ] **Step 5: Run unit and db-gated tests; see them pass.**
- [ ] **Step 6: Commit** `fix(logstore): drop an archived day's chunk instead of deleting its rows`.

### Task 5: Per-tick log lines move to Debug

**Files:**
- Modify: `component/automations/cluster_guard.go` (`duplicate automation execution prevented by cluster guard`: Warn to Debug; the `prevented` counter stays)
- Modify: `component/automations/scheduler.go` (`event trigger fired`: Info to Debug; `automation execution completed`: Debug when the status is `completed` or `skipped`, unchanged otherwise; execution ruling: a cluster-guard or precondition skip is a normal outcome, about 71k lines in six hours, and the precondition miss logs its own line)
- Modify: `component/memql/seed_materializer.go` (`seed materializer: global row written`: Info to Debug)
- Test: the existing tests of those packages

- [ ] **Step 1: Search for tests asserting these lines or levels** (`grep -rn "prevented by cluster guard\|event trigger fired\|execution completed\|global row written" --include=*_test.go`) and update any that pin the old level, in the same commit.
- [ ] **Step 2: Make the four changes**, each with a one-line comment naming the measurement: 71,105 duplicate-prevented warnings in six hours on an idle instance, about a million log lines a day in total (2026-10-11).
- [ ] **Step 3: Run** `go test -count=1 github.com/znasllc-io/memql/component/automations/... github.com/znasllc-io/memql/component/memql/...`; expected PASS.
- [ ] **Step 4: Commit** `fix(logs): move per-tick automation lines to debug`.

### Task 6: Maintenance pages server

**Files:**
- Create: `component/errorpages/errorpages.go`, `component/errorpages/errorpages_test.go`
- Create: `component/errorpages/mark.svg` (a byte copy of `brand/mark.svg`), `component/errorpages/maintenance.html.tmpl`, `component/errorpages/unavailable.html.tmpl`
- Create: `subcommand_error_pages.go` (repo root, package main)
- Modify: `subcommand_identity.go` and `subcommand_stub.go` (add `case "error-pages": return true, runErrorPagesSubcommand(args[1:])` beside `dsl-fetch`)

**Interfaces:**
- Produces:
  - `type Kind int` with `KindSite` (the zero value, so a zero `Kind` never brands) and `KindPlatform`
  - `func ClassifyHost(host, domain string) Kind`: lower-cases, strips a port, and answers `KindPlatform` for the front door's platform hosts under `domain` (`frontdoor.Roles()` plus `frontdoor.PlatformSites()`: `api.`, `identity.`, `mcp.`, `os.`, `vscode.`); everything else, the bare `domain` included, is `KindSite` (execution ruling: the engine's docs make the apex the cluster owner's main website, a deployable, and deployables show nothing MemQL).
  - `func Handler(domain string) http.Handler`: builds the host set once; serves every path, `/healthz` included, with status 503 and `Retry-After: 120`, `Cache-Control: no-store`; JSON `{"error":"unavailable","status":503,"message":"<text>"}` when `Accept` includes `application/json` or the host is `api.<domain>`; otherwise the platform page (MemQL mark inline, the word MemQL in the brand wordmark face, Squada One weight 400 embedded as a data URI, "MemQL is being updated. It will be back in a few minutes.") or the site page ("This site is temporarily unavailable. It will be back soon.", no MemQL mark, name, font or link).
  - `runErrorPagesSubcommand(args []string) int`: serves the pages on `MEMQL_ERROR_PAGES_ADDR` (default `:8080`) and answers `GET`/`HEAD /healthz` with 200 `ok` only on a separate probe listener `MEMQL_ERROR_PAGES_HEALTH_ADDR` (default `:8081`) that no Service lists (execution ruling: structurally immune to whatever headers a proxy forwards); the two must differ; domain from `MEMQL_DOMAIN`; no database, no secrets.

- [ ] **Step 1: Write the failing tests** in `errorpages_test.go`:
  - `TestClassifyHost`: table with `OS.Memql.Example:443` and `api.memql.example` as platform, the apex `memql.example` as site, `shop.memql.example` site, `customer.com` site, any host with empty domain site; `TestTheApexGetsTheNeutralPage` and `TestZeroKindIsASite`.
  - `TestPlatformPageIsBranded`: GET with `Host: os.memql.example` returns 503, `Retry-After: 120`, an HTML body containing `MemQL` and the `<svg` mark.
  - `TestSitePageIsNeutral`: `Host: www.customer.com` returns 503 and a body that contains neither `MemQL` nor `memql` (case-insensitive) nor `<svg`.
  - `TestErrorPagesJSONForAPIClients`: `Accept: application/json` on a site host, and any request to `api.memql.example`, return `application/json` with `"status":503`.
  - `TestErrorPagesHealthzIsAnsweredOnlyOnTheProbeListener`: 200 `ok` on the probe listener, the 503 page on the public one; `TestWordmarkFaceMatchesBrand`: the embedded face is byte-identical to `brand/fonts`.
  - `TestMarkMatchesBrand`: `component/errorpages/mark.svg` is byte-identical to `brand/mark.svg` (read via a relative path from the test; the copy drifting is a build failure, as the identity favicon copy is).
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** with `html/template` and `go:embed`; the pages are self-contained (inline CSS, no external fonts or scripts), use the brand colours from the identity stylesheet, and stay under 20 KB.
- [ ] **Step 4: Implement the subcommand**, following `subcommand_dsl_fetch.go`'s help and exit-code style (exit 0 on clean shutdown, 1 on listen failure), with graceful shutdown on SIGTERM.
- [ ] **Step 5: Run** `go test -count=1 github.com/znasllc-io/memql/component/errorpages/...` and build both binaries (`go build ./` and `go build -tags identity ./` or the Makefile's node-type build) to prove the case statements compile; expected PASS.
- [ ] **Step 6: Commit** `feat(errorpages): maintenance and neutral site pages for an unavailable front door`.

### Task 7: Route every front door's no-endpoint case to the error pages

**Files:**
- Create: `deploy/k8s/components/error-pages/error-pages.yaml`, a kustomize Component that only `overlays/cloud` and `overlays/cloud-entry` include, since local pods served nothing behind traefik and crash-looped every release-image install on a published edge image without the subcommand (Deployment `error-pages`, 2 replicas, image `acrmemql.azurecr.io/memql-edge` so each composing overlay's images block retargets it, `command: ["/app/memql", "error-pages"]`, `MEMQL_DOMAIN` from ConfigMap `memql-domain`; container ports `http` 8080 and `health` 8081, readiness and liveness `GET /healthz` on `health`; requests 10m CPU / 64Mi, limit 128Mi, `GOMEMLIMIT=100MiB` (execution ruling: the first draft's 16Mi/64Mi predates a measured 53-55 MiB RSS); non-root, read-only root, all capabilities dropped, a soft spread over nodes, no service account token, and no `part-of` pod label so the node PodMonitor does not scrape a permanent 503 (edge had no security context to copy); Service `error-pages` with ONE port, 80 to `http`, because ingress-nginx sends a custom default backend to `Spec.Ports[0]`; PodDisruptionBudget `minAvailable: 1`)
- Create: `deploy/k8s/components/error-pages/kustomization.yaml` (the Component, listing the manifest); Modify: `deploy/k8s/overlays/cloud/kustomization.yaml` and `deploy/k8s/overlays/cloud-entry/kustomization.yaml` (add it under `components:`). `deploy/k8s/base/kustomization.yaml` is not modified
- Modify: `cmd/frontdoorhosts/manifest.go` (every generated Ingress with a plain-HTTP backend gets `nginx.ingress.kubernetes.io/default-backend: error-pages`; `api-front-door-grpc` (GRPC) and `identity-front-door` (HTTPS) do not, because ingress-nginx v1.15 builds a custom default backend from the location's own upstream and keeps its backend protocol, so a plain-HTTP page server behind them answers 502; execution rulings), then regenerate with `make frontdoor`
- Modify: `integrations/customdomain/provision.go` (`IngressObject`) and `integrations/customdomain/accountdoor_provision.go` (the `doorIngress*` builders): the same annotation by the same rule (a door's `-app` and `-api` get it, its `-id` and `-api-grpc` do not)
- Modify: `scripts/deploy/bind-custom-domain.sh` and `scripts/deploy/bind-account-front-door.sh` with their tests: the same rule, because both transports must write the same objects (execution ruling)
- Modify: `cmd/platformgraph/deploy.go` (a base Deployment running `memql <subcommand>` is recorded with a `subcommand` attribute instead of being refused for lacking `MEMQL_NODE_TYPE`) and regenerate `component/architecture/embedded/platform.graph.json`
- Modify: `.github/workflows/ci.yml` and `scripts/dev/gate_inputs_lane_scope_test.go`: route `component/errorpages/**` into the `go` and `gates` buckets (Task 6's embedded files reached no bucket)
- Modify: `docs/public/operate/front-door.md`: a section on the fallback, the excluded Ingresses, the overlays that run it (cloud, cloud-entry; a local traefik cluster has neither the pods nor the fallback), the namespace caveat and the one-off backfill below
- Test: `deploy/k8s/overlays/frontdoor_hosts_test.go` (or a new `frontdoor_error_pages_test.go`), `deploy/k8s/overlays/render_cloud_entry_test.go`, the customdomain package tests

**Interfaces:**
- Consumes: the `error-pages` subcommand and `/healthz` from Task 6.
- Produces: annotation key/value constants `ErrorPagesBackendAnnotation = "nginx.ingress.kubernetes.io/default-backend"`, `ErrorPagesServiceName = "error-pages"` in `integrations/customdomain` (and the same literal in the generator, with a test asserting they match).

- [ ] **Step 1: Write the failing tests**:
  - `TestEveryFrontDoorIngressHasErrorPagesBackend`: parse each `deploy/k8s/overlays/*/front-door.generated.yaml`; every plain-HTTP `Ingress` carries the annotation with value `error-pages`, the GRPC and HTTPS ones do not, and no Ingress carries `custom-http-errors`.
  - `TestCloudEntryRendersErrorPages`: the rendered cloud-entry overlay contains Deployment, Service and PDB `error-pages` in namespace `memql`, with the image retargeted by the overlay's images block (same digest as `edge`).
  - `TestCustomDomainIngressHasErrorPagesBackend` and `TestAccountDoorIngressHasErrorPagesBackend` in the customdomain package.
  - Leave the existing staleness test to prove the regenerated files match the generator.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** the manifest, the generator change, `make frontdoor`, and the two builders. ingress-nginx resolves `default-backend` in the Ingress's OWN namespace: custom-domain Ingresses are created in `MEMQL_CUSTOM_DOMAIN_NAMESPACE`, defaulting to the engine namespace (`integrations/customdomain/plugin.go:114`), where `error-pages` runs. Say so in the builders' comment; an installation that moves custom domains elsewhere needs its own `error-pages` Service there, which this release does not add. The reconciler applies an Ingress only while a binding is issuing, so custom domains and doors bound before this release gain the annotation on re-bind or through the one-off `kubectl annotate` the operator section documents (execution ruling: re-reconciling live bindings is a design change beyond this release).
- [ ] **Step 4: Run** `go test -count=1 github.com/znasllc-io/memql/deploy/k8s/overlays/... github.com/znasllc-io/memql/integrations/customdomain/... github.com/znasllc-io/memql/cmd/frontdoorhosts/...`; expected PASS.
- [ ] **Step 5: Commit** `feat(deploy): serve the error pages when a front door has no ready endpoints`.

---

## After the tasks

1. `make test` and the db-gated lane (`scripts/ci/db-gated-packages.sh --trees`) green on the branch.
2. One PR `fix/storage-growth-stability` against `main`; CI green; owner merge (`scripts/dev/merge-as-owner.sh --pr=<n>` or the queue).
3. Release: `VERSION` PR to `0.25.1`, merge, then `scripts/release/release-engine.sh --version=v0.25.1 --targetSha=<full merge sha> --notes=...` with `--dryRun` first (it publishes the GitHub Release at that commit, whose bridge dispatches the image build, and waits for the build to start); verify the tag contains the fix merge (`git merge-base --is-ancestor`), then confirm the images exist.
4. Production (`znasllc-io/memql-znas`): AKS Blob CSI driver and the `memql-pipelines` AppProject destination (prerequisites of the v0.25 `cloud-entry`), import by digest, bump `?ref=`, `ENGINE_REF`, digests and notifications version, preflight, sync under the endpoint probe.
5. Template (`znasllc-io/memql-project`): move its engine pin to the new release.
