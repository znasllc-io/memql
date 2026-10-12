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
  - consts `statusHistoryGrace = time.Hour`, `statusHistoryPurgeInterval = 10 * time.Minute`, `statusHistoryBatch = 500`, `statusHistoryRunBudget = 25 * time.Second`
  - `type statusHistoryPurger interface { purgeSuperseded(ctx context.Context, concept string, olderThan time.Time, limit int) (int64, error) }`
  - `type sqlStatusHistoryPurger struct{ db *bun.DB }` built from the same `r.dbGetter()` handle `purger()` uses
  - `func (r *TopologyReconciler) sweepStatusHistory(ctx context.Context)`, called from the leader tick

- [ ] **Step 1: Write the failing unit tests** in `status_history_purge_test.go` with a fake purger:
  - `TestStatusHistorySweepRunsAtMostOncePerInterval`: two calls 5 minutes apart under a fake clock make one round of purges; a third at +11 minutes makes a second.
  - `TestStatusHistorySweepLoopsUntilABatchComesBackShort`: the fake returns 500, 500, 12 for the first concept; the sweep calls it three times for that concept, then moves to the next concept.
  - `TestStatusHistorySweepStopsAtTheRunBudget`: the fake always returns 500 and advances the fake clock 10 s per call; the sweep stops after the call that crosses 25 s and leaves the rest for the next interval.
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
  WHERE n.concept = ? AND n."createdAt" < ?
    AND EXISTS (SELECT 1 FROM "MemoryNodes" newer
                WHERE newer.concept = n.concept AND newer.id = n.id
                  AND newer."createdAt" > n."createdAt")
  LIMIT ?
) d
WHERE m.concept = ? AND m.id = d.id AND m."createdAt" = d."createdAt"
```

  Gate on the interval exactly as `sweepReadinessRowsOfStoppedNodes` does; each call gets its own `context.WithTimeout(ctx, 7*time.Second)`; log one Info line per sweep with the per-concept totals when any were deleted; feed the metric. The header comment records the 2026-10-11 incident (738,626 superseded registration versions for 13 workers), why there is no archive, and why running continuously matters (space freed inside the current chunk is reused; space freed in a closed chunk is not).
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
- [ ] **Step 3: Implement.** When `holdsScheduledJournal(...)`, replace `journal` with a copy (`h := *journal`) whose `hold` opens the run with `h.write(openCtx, "createWorkRun", h.openRunArgs(automation, exec, triggeringEvent, parentCause))`, install `common.ContextWithWriteObserver(ctx, func(string, string) { h.release() })`, and skip the immediate `openRun`. On the failure path, call `release()` before the run record closes so the failure is written. Leave every other path byte-identical. Demote `starting automation execution` to Debug in the same file (Task 5 owns the rest of the log changes; this line is here because the hold changes what that line used to promise).
- [ ] **Step 4: Run the package's unit tests and its db-gated journal tests; see them pass**
  Run: `go test -count=1 github.com/znasllc-io/memql/component/automations/...` and the same with `MEMQL_REQUIRE_DB=1 ... -run 'Journal'`.
- [ ] **Step 5: Commit** `fix(automations): a scheduled firing that writes nothing leaves no journal`.

### Task 3: Work retention selects candidates in bounded day windows

**Files:**
- Modify: `integrations/work/operational_retention.go` (`operationalRetention`)
- Modify: `integrations/work/operational_retention_db_test.go`

**Interfaces:**
- Produces: consts `retentionCandidateWindow = 24 * time.Hour`, `retentionCandidateLimit = 100`, `retentionRunBudget = 4 * time.Minute`.

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
- Produces: `rowSource.dropDay(ctx context.Context, day time.Time) (chunksDropped int64, err error)`; `Report.ChunksDropped int64` (json `chunks_dropped`).

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
- Modify: `component/automations/scheduler.go` (`event trigger fired`: Info to Debug; `automation execution completed`: Debug when the status is `completed`, unchanged otherwise)
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
  - `type Kind int` with `KindPlatform`, `KindSite`
  - `func ClassifyHost(host, domain string) Kind`: lower-cases, strips a port, and answers `KindPlatform` for `domain` and for `api.`, `identity.`, `mcp.`, `os.`, `vscode.` under it; everything else (other subdomains, any other domain, an empty domain) is `KindSite`.
  - `func Handler(domain string) http.Handler`: serves every path with status 503 and `Retry-After: 120`, `Cache-Control: no-store`; JSON `{"error":"unavailable","status":503,"message":"<text>"}` when `Accept` includes `application/json` or the host is `api.<domain>`; otherwise the platform page (MemQL mark inline, the word MemQL, "MemQL is being updated. It will be back in a few minutes.") or the site page ("This site is temporarily unavailable. It will be back soon.", no MemQL mark, name or link). `GET /healthz` answers 200 `ok`.
  - `runErrorPagesSubcommand(args []string) int`: listens on `MEMQL_ERROR_PAGES_ADDR` (default `:8080`), domain from `MEMQL_DOMAIN`; no database, no secrets.

- [ ] **Step 1: Write the failing tests** in `errorpages_test.go`:
  - `TestClassifyHost`: table with `memql.example` and `OS.Memql.Example:443` as platform, `api.memql.example` platform, `shop.memql.example` site, `customer.com` site, any host with empty domain site.
  - `TestPlatformPageIsBranded`: GET with `Host: os.memql.example` returns 503, `Retry-After: 120`, an HTML body containing `MemQL` and the `<svg` mark.
  - `TestSitePageIsNeutral`: `Host: www.customer.com` returns 503 and a body that contains neither `MemQL` nor `memql` (case-insensitive) nor `<svg`.
  - `TestErrorPagesJSONForAPIClients`: `Accept: application/json` on a site host, and any request to `api.memql.example`, return `application/json` with `"status":503`.
  - `TestHealthz`: 200 `ok`.
  - `TestMarkMatchesBrand`: `component/errorpages/mark.svg` is byte-identical to `brand/mark.svg` (read via a relative path from the test; the copy drifting is a build failure, as the identity favicon copy is).
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** with `html/template` and `go:embed`; the pages are self-contained (inline CSS, no external fonts or scripts), use the brand colours from the identity stylesheet, and stay under 20 KB.
- [ ] **Step 4: Implement the subcommand**, following `subcommand_dsl_fetch.go`'s help and exit-code style (exit 0 on clean shutdown, 1 on listen failure), with graceful shutdown on SIGTERM.
- [ ] **Step 5: Run** `go test -count=1 github.com/znasllc-io/memql/component/errorpages/...` and build both binaries (`go build ./` and `go build -tags identity ./` or the Makefile's node-type build) to prove the case statements compile; expected PASS.
- [ ] **Step 6: Commit** `feat(errorpages): maintenance and neutral site pages for an unavailable front door`.

### Task 7: Route every front door's no-endpoint case to the error pages

**Files:**
- Create: `deploy/k8s/base/error-pages.yaml` (Deployment `error-pages`, 2 replicas, image `acrmemql.azurecr.io/memql-edge` so every overlay's images block retargets it, `command: ["/app/memql", "error-pages"]`, `MEMQL_DOMAIN` from ConfigMap `memql-domain`, requests 10m CPU / 16Mi, limits 64Mi, readiness and liveness on `/healthz` port 8080, the security context edge uses; Service `error-pages` port 80 to 8080; PodDisruptionBudget `minAvailable: 1`)
- Modify: `deploy/k8s/base/kustomization.yaml` (add the resource)
- Modify: `cmd/frontdoorhosts/manifest.go` (every generated Ingress gets `nginx.ingress.kubernetes.io/default-backend: error-pages`), then regenerate with `make frontdoor`
- Modify: `integrations/customdomain/provision.go` (`IngressObject`) and `integrations/customdomain/accountdoor_provision.go` (the `doorIngress*` builders): the same annotation
- Test: `deploy/k8s/overlays/frontdoor_hosts_test.go` (or a new `frontdoor_error_pages_test.go`), `deploy/k8s/overlays/render_cloud_entry_test.go`, the customdomain package tests

**Interfaces:**
- Consumes: the `error-pages` subcommand and `/healthz` from Task 6.
- Produces: annotation key/value constants `ErrorPagesBackendAnnotation = "nginx.ingress.kubernetes.io/default-backend"`, `ErrorPagesServiceName = "error-pages"` in `integrations/customdomain` (and the same literal in the generator, with a test asserting they match).

- [ ] **Step 1: Write the failing tests**:
  - `TestEveryFrontDoorIngressHasErrorPagesBackend`: parse each `deploy/k8s/overlays/*/front-door.generated.yaml`; every `Ingress` carries the annotation with value `error-pages`.
  - `TestCloudEntryRendersErrorPages`: the rendered cloud-entry overlay contains Deployment, Service and PDB `error-pages` in namespace `memql`, with the image retargeted by the overlay's images block (same digest as `edge`).
  - `TestCustomDomainIngressHasErrorPagesBackend` and `TestAccountDoorIngressHasErrorPagesBackend` in the customdomain package.
  - Leave the existing staleness test to prove the regenerated files match the generator.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** the manifest, the generator change, `make frontdoor`, and the two builders. ingress-nginx resolves `default-backend` in the Ingress's OWN namespace: custom-domain Ingresses are created in `MEMQL_CUSTOM_DOMAIN_NAMESPACE`, defaulting to the engine namespace (`integrations/customdomain/plugin.go:114`), where `error-pages` runs. Say so in the builders' comment; an installation that moves custom domains elsewhere needs its own `error-pages` Service there, which this release does not add.
- [ ] **Step 4: Run** `go test -count=1 github.com/znasllc-io/memql/deploy/k8s/overlays/... github.com/znasllc-io/memql/integrations/customdomain/... github.com/znasllc-io/memql/cmd/frontdoorhosts/...`; expected PASS.
- [ ] **Step 5: Commit** `feat(deploy): serve the error pages when a front door has no ready endpoints`.

---

## After the tasks

1. `make test` and the db-gated lane (`scripts/ci/db-gated-packages.sh --trees`) green on the branch.
2. One PR `fix/storage-growth-stability` against `main`; CI green; owner merge (`scripts/dev/merge-as-owner.sh --pr=<n>` or the queue).
3. Release: `VERSION` PR to `0.25.1`, merge, dry-run cut, cut (`docs/public/operate/release-cutting.md`), confirm the images exist.
4. Production (`znasllc-io/memql-znas`): AKS Blob CSI driver and the `memql-pipelines` AppProject destination (prerequisites of the v0.25 `cloud-entry`), import by digest, bump `?ref=`, `ENGINE_REF`, digests and notifications version, preflight, sync under the endpoint probe.
5. Template (`znasllc-io/memql-project`): move its engine pin to the new release.
