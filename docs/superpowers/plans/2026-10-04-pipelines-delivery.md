# Pipelines delivery (epic memql#5480) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pipeline announces its runs on a channel (Discord webhook over a globalSecret reference, or email through the cluster's sender) with the run page as the report link, verifies a rollout from outside with typed evidence, and carries the code half of milestones M1-M4 -- in ONE pull request, with every calendar- and release-gated milestone left open and stated.

**Architecture:** The notify stage is executed by the run driver itself (component/pipelinerun) -- never by the step runner -- and delivers through the existing outbound worker, which gains a send-time secret reference so a webhook URL is never written to a row. verify-rollout is a command step (`memql-verify`, a new `cmd/`) that probes the cluster's public front door: `/healthz` now reports the release it runs, the OS bundle carries a build-time manifest of its own bytes, and the docs site carries its version meta. The message composer and the false-green count are pure functions in the standard-library leaf `component/pipelines`.

**Tech Stack:** Go 1.26 (root module), MemQL DSL (`dsl/pipelines`, `dsl/platform`), Node stdlib (the OS bundle manifest script), React/TypeScript (MemQL OS, after epic 4 merges).

**Spec:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md` (D16; section 4 epic 5; section 7; section 9 for the seam's settled readings) and `docs/superpowers/specs/2026-09-27-documentation-program-design.md` (D15, D16). Visual requirements: `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, `clients/os/DESIGN.md`.

## Owner decisions this plan rests on (2026-10-04)

- ONE pull request for the whole epic (not one per milestone).
- "Ship code, gates stay open": #5504 and #5505 close with the PR; #5506-#5509 get a comment each naming what landed and the exact gate that remains. Nothing in this PR deletes `ci.yml` or `build-engine-images.yml`, edits the ruleset, or touches memql-znas / memql-project.
- Peer agreements (sessions memql-38 = epic 4 #5479, memql-d9 = epic 3 #5478):
  - Channel management lives in **Deployables > Settings** (the app's own settings section, beside its Sources group). PipelinePage only SHOWS which channels a pipeline's notify stages name, whether each exists and is allowed, with a text link to manage them.
  - The capability part `execute app:deployables/channels` is ALREADY seeded by epic 4 (owner + developer, mirror in `component/auth/rbac_model.go`, OS parts list). Do not add another; put `@requiresCapability("execute", "app:deployables/channels")` on the channel builtins.
  - `dsl/pipelines/*` and `component/pipelines/refusal.go` / `summary.go`: APPEND only, never rewrite a file header.
  - The root `memql-package.yaml` and `memql_package_manifest_test.go` are epic 3's; this plan ADDS the docs, deploy and notify stages after epic 3 merges and keeps the test's holds (dbGated vs `scripts/ci/db-gated-packages.sh`, gate packages vs `ci.yml`, postgres by digest), updating its pinned stage order.
  - The toolchain image (`deploy/toolchain-image/`) is epic 3's; add `memql-verify` as a digest-pinned FROM stage copied by stage name, plus one line each in its smoke test and its workflow guard.

## Phases (the dependency order)

| Phase | Starts | Tasks |
|---|---|---|
| A, engine | now, on `origin/main` | 1-9 |
| B, the engine's own manifest | after epic 3 (#5478) merges; rebase first | 10-11 |
| C, MemQL OS | after epic 4 (#5479) merges; rebase first | 12-17 |
| D, finish | after A-C | 18 |

Rebase onto `origin/main` at the start of B and of C (`git -C ../epic-pipelines-delivery rebase origin/main`), regenerate the derived artifacts (`make arch-model`, `make platform-graph`, `make sdk-gen`, `make concept-snapshot`) and re-run the touched packages' tests before continuing.

## Global Constraints

- Notifications ride the outbound path: retries, rate limits and the audit trail come from `v1:platform:outboundRequest` and `component/outbound`, never a second outbox.
- `allowed_mentions` is always `{"parse": []}`: no @everyone, no @here, no user or role pings.
- The report link exists the moment the run row does, so a FAILED run's message carries it too; a notify stage is NOT blocked by an earlier failed stage. A CANCELLED run sends nothing.
- A secret VALUE is never written to a row, a log line, a receipt, a check run or an error string. Channels and outbound rows carry the globalSecret NAME only.
- verify-rollout probes the target from OUTSIDE (the public front door) and never manufactures a failure to test itself.
- Probe limits (kept from the observer): HTML and API responses at most 2 MB; same-origin JS/CSS assets at most 16 MB, non-empty, never an HTML fallback; redirects refused.
- A message's fields are named `Version`, `MemQL OS` and `Deployment details` (the run page), plus `Docs` and `Artifacts` (documentation program D15).
- No new HTTP endpoint. `/healthz` gains two fields; the OS bundle manifest is a static file inside the site the edge already serves.
- No `if env == ...` branching, no new env var unless registered in `scripts/secrets/manifest.yaml` (none is planned).
- Every new `pipeline_*` code is appended to `component/pipelines/refusal.go`, spelled in `component/packages/refusal.go`'s literal block, given check-run copy in `summary.go` where it can end a run, and given OS copy in `clients/os/src/apps/deployables/packages/refusals.ts` (phase C).
- No emojis anywhere; no new HTTP route; stage files by explicit path; never `git add -A`.

## Review Focus

1. **A webhook URL leaking through an error string.** `net/http` wraps failures in `*url.Error`, whose message embeds the full URL (and a Discord webhook's token is in its path). A transport error, a log line or `lastError` must never contain it. Pinned in Task 5 (a dial failure against a secret target stamps a `lastError` with no URL and no token).
2. **The notify step after a failed stage.** The driver blocks every stage after a failed one; a notify stage must run anyway and say what failed, while a cancelled run sends nothing. Pinned in Task 6 (driver tests for failed-then-notify and cancelled-then-no-notify).
3. **A driver that dies between staging and delivery.** The replica that takes over re-sends the step with the SAME attempt; the notification must not be delivered twice. Pinned in Task 6 (deterministic request id; a resumed drive re-stages onto the same row and observes `sent`).
4. **A partially rolled-out deployment read as verified.** One request to `api.<domain>/healthz` hits one replica. Pinned in Task 4 (three samples per host; a mixed answer fails `rollout_version_mismatch` naming both node ids and versions).
5. **Markdown and mention injection from a commit message.** A title like `[click](https://evil) @everyone` must render as text. Pinned in Task 6a (escaped title, `allowed_mentions.parse == []`).

---

## File structure

| Path | Responsibility | Phase |
|---|---|---|
| `component/pipelines/contract.go`, `environment.go` | `StepRequest.Domain` -> `MEMQL_DOMAIN`; `Step.Links` | A |
| `component/pipelines/spec.go`, `validate.go`, `compile.go` | `links:` on a notify stage | A |
| `component/pipelines/notify.go` (new) | Pure message composition: Discord JSON, email text, headlines | A |
| `component/pipelines/falsegreen.go` (new) | Pure false-green count (M1) | A |
| `component/pipelines/refusal.go`, `summary.go` | New codes + check-run copy (append) | A |
| `component/packages/refusal.go` | Literal block for the new codes (parity) | A |
| `component/pipelinerun/notify.go` (new) | The notify executor inside the driver | A |
| `component/pipelinerun/driver.go` | runStep dispatch; notify not blocked; domain fact; artifact ids kept | A |
| `component/pipelinerun/ports.go`, `store_dsl.go`, `types.go`, `integration.go` | Store methods for channels, previous run, outbound staging/status, file names; `Deps.Domain` | A |
| `app/integrations_pipelines.go` | `d.Domain` from `MEMQL_DOMAIN` | A |
| `component/outbound/*.go`, `app/engine.go` | Secret-reference webhook targets, URL-free errors | A |
| `dsl/platform/concepts.memql`, `mutations.memql`, `queries.memql` | `outboundRequest.targetSecret`; `stageOutboundRequestToSecret`; `outboundRequestById` | A |
| `dsl/pipelines/mutations.memql`, `queries.memql`, `shapes.memql` | Channel writers (@serverOnly), reads, shape; previous-run read | A |
| `component/server/http_contract.go`, `health.go` | `/healthz` `version` + `commit` | A |
| `scripts/os/bundle-manifest.mjs` (new), `scripts/os/build.sh`, `scripts/os/bundle_manifest_test.go` (new) | `dist/memql-bundle.json` | A |
| `cmd/memql-verify/` (new) | verify-rollout | A |
| `docs/public/operate/pipelines.md`, `docs/internal/ops/pipelines-milestones.md` (new), record section 10 | Docs | A, D |
| `memql-package.yaml`, `memql_package_manifest_test.go` | docs/deploy/notify stages | B |
| `deploy/toolchain-image/Dockerfile`, `smoke-test.sh`, `scripts/ci/toolchain_image_workflow_test.go` | `memql-verify` in the toolchain | B |
| `dsl/pipelines/builtins.memql`, `component/pipelinerun/channels.go` (new), `capabilities.go` | Channel builtins, false-green builtin | C |
| `clients/os/src/apps/deployables/**`, `clients/os/src/apps/files/**`, `clients/os/src/main.tsx` | Channels settings, fact lines, run-page delivery line, `?libraryFile=` | C |

---

## Phase A -- engine (on origin/main)

### Task 1: The step knows its cluster's front door (`MEMQL_DOMAIN`)

**Files:**
- Modify: `component/pipelines/contract.go` (StepRequest), `component/pipelines/environment.go`
- Modify: `component/pipelinerun/integration.go` (Deps), `component/pipelinerun/driver.go` (requestFacts, setFacts, request)
- Modify: `app/integrations_pipelines.go`
- Test: `component/pipelines/environment_test.go` (or the existing test file holding `Environment()`; find with `grep -rn "Environment()" component/pipelines/*_test.go`), `component/pipelinerun/driver_test.go`

**Interfaces:**
- Produces: `pipelines.StepRequest.Domain string` (json `domain,omitempty`); `Environment()["MEMQL_DOMAIN"]` when non-empty; `pipelinerun.Deps.Domain func() string`.

- [ ] **Step 1: Failing test (pipelines)** -- an Environment with `Domain: "example.test"` carries `MEMQL_DOMAIN=example.test`; with `Domain: ""` the key is absent; a secret named `MEMQL_DOMAIN` never overrides a set domain.

```go
func TestEnvironmentCarriesTheClusterDomain(t *testing.T) {
	req := StepRequest{Domain: "example.test", Secrets: map[string]string{"MEMQL_DOMAIN": "evil.test"}}
	if got := req.Environment()["MEMQL_DOMAIN"]; got != "example.test" {
		t.Fatalf("MEMQL_DOMAIN = %q, want example.test", got)
	}
	if _, ok := (StepRequest{}).Environment()["MEMQL_DOMAIN"]; ok {
		t.Fatal("an empty domain must leave MEMQL_DOMAIN unset, not empty")
	}
}
```

- [ ] **Step 2: Run** `go test ./component/pipelines/ -run TestEnvironmentCarriesTheClusterDomain` -- FAIL (no field).
- [ ] **Step 3: Implement.** Add to `StepRequest` (after `Version`):

```go
	// Domain is the front-door domain of the cluster running the pipeline
	// (its MEMQL_DOMAIN), "" when none is configured. A step reads it as
	// MEMQL_DOMAIN to reach this cluster's public hosts -- verify-rollout's
	// api.<domain>, identity.<domain> and os.<domain> -- from outside, the
	// way any client does; a step reaches nothing inside the cluster.
	Domain string `json:"domain,omitempty"`
```

In `Environment()`, after the map literal: `if r.Domain != "" { env["MEMQL_DOMAIN"] = r.Domain }` (before the secrets loop, so the reserved check keeps a secret from shadowing it). Update the doc comment's list of names.

- [ ] **Step 4: Driver.** `Deps.Domain func() string` (doc: "the cluster's MEMQL_DOMAIN, read at each call; nil or "" sends no MEMQL_DOMAIN"); default in `snapshot()` to `func() string { return "" }`. Add `domain string` to `requestFacts`, set in `setFacts` from `dr.d.Domain()`, and `Domain: f.domain` in `request()`. Driver test: a fake executor records the request; with `Deps.Domain` returning `"example.test"` the recorded `StepRequest.Domain` equals it.
- [ ] **Step 5: app.** In `wirePipelines`, `d.Domain = pipelinesDomain` where

```go
// pipelinesDomain is this cluster's front-door domain, MEMQL_DOMAIN, read at
// each call: what a step receives as MEMQL_DOMAIN. "" sends none.
func pipelinesDomain() string { return strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")) }
```

- [ ] **Step 6: Run** `go test ./component/pipelines/ ./component/pipelinerun/ ./app/ -count=1` -- PASS. Check `component/pipelines/contract_test.go` pins the JSON wire names; add `domain`.
- [ ] **Step 7: Commit** `git add component/pipelines/contract.go component/pipelines/environment.go component/pipelines/*_test.go component/pipelinerun/integration.go component/pipelinerun/driver.go component/pipelinerun/driver_test.go app/integrations_pipelines.go` -- `Issue #5505: a step receives its cluster's front-door domain as MEMQL_DOMAIN`.

### Task 2: `/healthz` reports the release it runs (the deploy-version endpoint)

**Files:** Modify `component/server/http_contract.go` (both `GetHealthz200JSONResponse` and `GetHealthz503JSONResponse`), `component/server/health.go`; Test: the existing healthz test in `component/server` (`grep -ln "buildHealthResponse\|getHealthz" component/server/*_test.go`).

**Interfaces:** Produces JSON `version` (= `buildinfo.Version()`) and `commit` (= `buildinfo.ShortCommit()`, omitted when empty) on every node's `/healthz`, 200 and 503 alike.

- [ ] **Step 1: Failing test** -- decode `buildHealthResponse()`'s 200 body; `version == buildinfo.Version()`; `commit` equals `buildinfo.ShortCommit()` (absent when that is "").
- [ ] **Step 2: Run** `go test ./component/server/ -run Healthz -count=1` -- FAIL.
- [ ] **Step 3: Implement** two fields on both response types:

```go
		// Version is the release this binary was cut from (buildinfo.Version):
		// what verify-rollout compares from outside, through the front door, to
		// say a deployment is the release it claims (epic memql#5480). Public
		// like the rest of this body; the repository and its tags are public.
		Version string `json:"version,omitempty"`
		// Commit is the short revision it was built from, "" when unknown.
		Commit string `json:"commit,omitempty"`
```

and set them in `buildHealthResponse()` for both branches.

- [ ] **Step 4: Run** the test -- PASS; `grep -rn "healthz" docs/public/operate/*.md | head` and add the two fields where the response is documented (if it is).
- [ ] **Step 5: Commit** -- `Issue #5505: /healthz reports the release and commit the node runs`.

### Task 3: The OS bundle carries a manifest of its own bytes

**Files:** Create `scripts/os/bundle-manifest.mjs`, `scripts/os/bundle_manifest_test.go`; Modify `scripts/os/build.sh` (`run_build`).

**Interfaces:** Produces `clients/os/dist/memql-bundle.json` (and `/app/os/memql-bundle.json` in the edge image, via the Dockerfile's existing `bash scripts/os/build.sh build`):

```json
{"schema": 1, "algorithm": "sha256", "files": {"assets/index-abc.js": "<64 hex>", "index.html": "<64 hex>"}}
```

Paths are dist-relative with `/` separators, sorted; the manifest never lists itself.

- [ ] **Step 1: Failing test** (`package osbuild_test`; skip when `node` is not on PATH): make a temp dist with `index.html`, `assets/a.js`, `assets/sub/b.css`; run `node scripts/os/bundle-manifest.mjs <dir>`; assert the JSON parses, `files` has exactly those three keys with the `sha256.Sum256` hex of each, and running it twice yields byte-identical output (determinism) without listing `memql-bundle.json`.
- [ ] **Step 2: Run** `go test ./scripts/os/ -count=1` -- FAIL (no script).
- [ ] **Step 3: Implement** the script (Node stdlib only):

```js
#!/usr/bin/env node
// scripts/os/bundle-manifest.mjs <dist-dir>
//
// Writes <dist-dir>/memql-bundle.json: the sha256 of every file of the built
// MemQL OS bundle, by dist-relative path. The edge image's layer carries it
// at /app/os/memql-bundle.json, so verify-rollout (cmd/memql-verify) can read
// what the image holds and compare it with what the public front door serves
// (epic memql#5480). Node stdlib only; deterministic (sorted keys, no
// timestamps), and it never lists itself.
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, relative, sep } from "node:path";

const NAME = "memql-bundle.json";
const root = process.argv[2];
if (!root) {
  console.error("usage: bundle-manifest.mjs <dist-dir>");
  process.exit(2);
}
const files = {};
function walk(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) walk(path);
    else if (entry.isFile()) {
      const rel = relative(root, path).split(sep).join("/");
      if (rel === NAME) continue;
      files[rel] = createHash("sha256").update(readFileSync(path)).digest("hex");
    }
  }
}
walk(root);
const sorted = Object.fromEntries(Object.keys(files).sort().map((k) => [k, files[k]]));
writeFileSync(join(root, NAME), JSON.stringify({ schema: 1, algorithm: "sha256", files: sorted }) + "\n");
console.error(`INFO: ${NAME}: ${Object.keys(sorted).length} files`);
```

In `run_build`, after `npm run build`: `node "${SCRIPT_DIR}/bundle-manifest.mjs" "${OS_DIR}/dist"` with an `info` line. The Dockerfile already copies `scripts/os` into the spa-build stage; confirm with `grep -n "COPY scripts/os" Dockerfile`.

- [ ] **Step 4: Run** the test -- PASS; run `bash scripts/os/build.sh build` once if Node is available and confirm `clients/os/dist/memql-bundle.json` exists.
- [ ] **Step 5: Commit** -- `Issue #5505: the OS bundle carries a sha256 manifest of its own files`.

### Task 4: `memql-verify` -- verify-rollout from outside

**Files:** Create `cmd/memql-verify/main.go`, `cmd/memql-verify/verify.go`, `cmd/memql-verify/probes.go`, `cmd/memql-verify/verify_test.go`.

**Interfaces:**
- CLI: `memql-verify --domain=<d> --version=<v> [--docs=<url>] [--wait=15m] [--interval=30s] [--samples=3] [--evidence=verify-rollout.json] [--request-timeout=20s]`, plus test-only overrides `--api-url`, `--identity-url`, `--os-url` (each defaults to `https://<role>.<domain>`; `--domain` may be omitted only when all three are given). Exit 0 verified, 1 not verified, 2 bad flags.
- Codes (printed, and in the evidence JSON): `rollout_unreachable`, `rollout_redirect_refused`, `rollout_health_failed`, `rollout_version_unreported`, `rollout_version_mismatch`, `rollout_os_invalid`, `rollout_bundle_unreported`, `rollout_bundle_mismatch`, `rollout_asset_invalid`, `rollout_docs_version_mismatch`, and the summary code `rollout_unverified`.
- Evidence JSON: `{"version": "...", "verified": bool, "attempts": n, "startedAt": "...", "finishedAt": "...", "checks": [{"check": "api health", "url": "...", "passed": bool, "findings": [{"code", "expected", "observed", "detail"}], "samples": [{"nodeId", "version", "commit", "status"}]}]}`.

Behavior:
- Version matching: a release (`^v?\d+\.\d+\.\d+([-+].*)?$`) compares `/healthz` `version` with the leading `v` stripped on both sides; anything else is a commit, matched as a prefix either way with at least 7 hex characters against `/healthz` `commit`.
- Health (api, identity, os hosts): `--samples` GETs of `<origin>/healthz`; each must be 200 JSON with `status == "ok"` and a matching version; the check records every sample's `nodeId`, `version`, `commit`.
- OS: GET `<os-origin>/` must be 200 `text/html`, at most 2 MB, containing `<title>MemQL OS</title>`. GET `<os-origin>/memql-bundle.json` (at most 2 MB; 404 is `rollout_bundle_unreported`). Every same-origin `<script src>` and `<link rel="stylesheet|modulepreload" href>` in the served HTML, except the edge's injected `/_memql/` paths, is fetched (at most 16 MB, non-empty, content type not `text/html`, body not starting with `<!doctype` or `<html` after trimming, case-insensitive) and its sha256 must equal the manifest's entry for that path (`rollout_bundle_mismatch` when it differs or the manifest lacks it). Cross-origin references are ignored, as the observer ignored them. HTML documents are never byte-compared: the edge injects its refresh tag into them.
- Docs: only for a release version and only when `--docs` is given: the page must be 200, at most 2 MB, and carry `<meta name="memql-docs-version" content="X">` with `X` equal to the bare version.
- Redirects are refused (`CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is `rollout_redirect_refused` with its `Location`). Transport failures are `rollout_unreachable` with the error class (DNS, TLS, timeout, refused), never a body.
- The loop: probe everything; if all pass, exit 0. Otherwise sleep `--interval` and retry until `--wait` elapses; then print every failing finding as `<code> <check>: <detail>` (last lines of output, which the run page shows inline) followed by `rollout_unverified: <n> of <m> checks failing after <attempts> attempts over <duration>`, write the evidence file, exit 1. Evidence is written on success too.

- [ ] **Step 1: Write the failing tests** (`package main`, `httptest.NewServer` per role, a fake `sleep` and clock injected through a `config` struct so the loop runs instantly):
  1. all pass -> exit 0, evidence `verified: true`, `attempts: 1`.
  2. api reports `v0.24.0` for `--version=v0.24.1` -> exit 1; `rollout_version_mismatch` names expected and observed.
  3. mixed rollout: sample 2 of 3 answers another version and node id -> mismatch naming both node ids.
  4. `/healthz` 503 `{"status":"draining"}` -> `rollout_health_failed`.
  5. `/healthz` without `version` -> `rollout_version_unreported`.
  6. a 302 on the OS page -> `rollout_redirect_refused` with the Location.
  7. a script asset served as `text/html` with an HTML body -> `rollout_asset_invalid`.
  8. a script asset whose bytes differ from the manifest -> `rollout_bundle_mismatch` with both hashes.
  9. `memql-bundle.json` 404 -> `rollout_bundle_unreported`.
  10. docs meta `0.24.0` for `v0.24.1` -> `rollout_docs_version_mismatch`; a SHA `--version` skips the docs check (recorded as skipped, not passed).
  11. a commit `--version=1a2b3c4d5e` matches `/healthz` `commit: "1a2b3c4"`.
  12. first attempt fails, second passes -> exit 0, `attempts: 2`.
  13. never passes -> exit 1, the last stdout line starts `rollout_unverified:`, the evidence file has `verified: false`.
  14. bad flags (no version) -> exit 2.
- [ ] **Step 2: Run** `go test ./cmd/memql-verify/ -count=1` -- FAIL.
- [ ] **Step 3: Implement** `probes.go` (one function per probe returning `checkResult`), `verify.go` (`type config struct`, `func run(ctx context.Context, cfg config, stdout, stderr io.Writer) int`), `main.go` (flag parsing into `config`, `os.Exit(run(...))`). Bound every body read with `io.LimitReader(limit+1)` and fail when the limit is exceeded (`rollout_os_invalid` / `rollout_asset_invalid` / `rollout_health_failed` naming "larger than N bytes").
- [ ] **Step 4: Run** -- PASS. Also `go vet ./cmd/memql-verify/`.
- [ ] **Step 5: Commit** -- `Issue #5505: memql-verify probes a rollout from outside and fails typed with its evidence`.

### Task 5: Outbound webhook targets by secret reference

**Files:**
- Modify: `dsl/platform/concepts.memql` (outboundRequest: `targetSecret`), `dsl/platform/mutations.memql` (`stageOutboundRequestToSecret`), `dsl/platform/queries.memql` (`outboundRequestById`)
- Modify: `component/outbound/transport.go`, `component/outbound/worker.go`, `component/outbound/config.go` (only if a helper fits there), `app/engine.go` (pass the resolver)
- Modify: `component/memql/rowauthz_undeclared_gate_test.go` (one entry for `outboundRequestById`, citing the follow-up issue filed in Step 0), `test/dslconformance/server_only_parsed_test.go` (`want` map)
- Test: `component/outbound/worker_test.go`

**Interfaces:**
- Produces: mutation `stageOutboundRequestToSecret(requestId, targetSecret, subject?, body, dedupeKey?, requestedBy?)` (@serverOnly, @createOnly("status","attempts")) writing `medium: "webhook"`, `target: "secret:" + args.targetSecret`, `targetSecret`; query `outboundRequestById(requestId)` (@serverOnly) shaped like `outboundRequestsByStatus`; `outbound.Worker` field `Secrets func(ctx context.Context, name string) (string, error)`; `outbound.Request.TargetSecret string`.

- [ ] **Step 0: File the follow-up** (the ratchet requires an issue for a new undeclared-tier read): `gh issue create --repo znasllc-io/memql --title "outboundRequest rows (delivery bodies and webhook targets) are readable by any signed-in user" --label claude,area/engine` with a body mirroring #5802 for the outbound concept (the grandfathered queries, the writers that must keep working, the done-when). Record the number as `<OUTBOUND_TIER_ISSUE>`.
- [ ] **Step 1: Failing worker tests:**
  1. a pending row with `targetSecret: "DISCORD_X"`, the resolver answering `https://discord.com/api/webhooks/1/tok`, the allowlist `https://discord.com/api/webhooks/` -> the fake HTTP server receives the POST at `/api/webhooks/1/tok`; the row is stamped `sent`; no stamp ever carries the URL.
  2. the resolver errors or answers "" -> stamped `failed`, permanent, `lastError` names `DISCORD_X` and contains no URL.
  3. the resolved URL is outside the allowlist -> `failed`, `lastError` "webhook: target not in allowlist" (no URL).
  4. a dial failure against the resolved URL -> the stamped `lastError` contains neither the host path nor the token (strip `*url.Error`'s URL with `errors.As`, keep its `.Err`).
  5. `targetSecret` on a `medium: "email"` row -> `failed`, permanent.
  6. a row with no `targetSecret` behaves exactly as before (existing tests stay green).
- [ ] **Step 2: Run** `go test ./component/outbound/ -count=1` -- FAIL.
- [ ] **Step 3: DSL.** Concept field, appended after `requestedBy`:

```memql
  targetSecret  string  @description("For a webhook row staged by server code only: the v1:platform:globalSecret NAME whose value is the URL to POST to (memql#5480). The worker resolves it at send time and never writes the value back; `target` then holds the descriptor secret:<NAME>, so the row, its audit and every error name the secret and never the URL. Written only by stageOutboundRequestToSecret (@serverOnly).")
```

Mutation (anchored after `stageOutboundRequest`'s closing brace, with its `///` doc ABOVE the annotations -- see the fan-out memory's insertion trap):

```memql
/// Stage a webhook delivery whose URL is a secret (memql#5480): a Discord webhook's token is in its
/// path, so the URL itself is a credential and must never sit on this row. The row names the
/// globalSecret instead (targetSecret) and target carries the descriptor secret:<NAME>; the outbound
/// worker resolves the value at send time, checks it against the deployment's webhook allowlist and
/// POSTs to it. Idempotent by requestId like stageOutboundRequest.
///
/// @serverOnly rather than actor scoping: a client able to stage a row naming any secret could make the
/// worker POST a body of its choosing to whatever URL a cluster owner stored, and scoping the row to
/// the caller would not change which secret it names. The writers are server-side Go -- the pipelines
/// notify stage -- after they have checked the caller's right to the channel that names the secret.
@serverOnly
@createOnly("status", "attempts")
mutation outboundRequest stageOutboundRequestToSecret {
  args {
    requestId     string!
    targetSecret  string!
    subject       string
    body          string!
    dedupeKey     string
    requestedBy   string
  }
  insert {
    accept { targetSecret, subject, body, dedupeKey, requestedBy }
    stamp {
      id: args.requestId
      medium: "webhook"
      target: "secret:" + args.targetSecret
      status: "pending"
      attempts: 0
    }
  }
}
```

Query (after `outboundRequestsByStatus`):

```memql
/// One outbound request by its id (memql#5480): how the pipelines notify stage learns whether the
/// delivery it staged was sent. @serverOnly because the concept declares no tier yet
/// (memql#<OUTBOUND_TIER_ISSUE>): until it does, a client-reachable by-id read would hand any caller any
/// delivery's body.
@serverOnly
query outboundRequest outboundRequestById {
  args {
    requestId  string!
  }
  filter  row => row.id == args.requestId
}
```

(Match `outboundRequestsByStatus`'s shape line if it has one.) If `"secret:" + args.targetSecret` does not lower in a stamp, compute the descriptor in Go instead: add `target string!` to the args, accept it, and have the Go caller pass `"secret:" + name`; the transport ignores `target` whenever `targetSecret` is set.

- [ ] **Step 4: Go.** `requestFromRow` reads `targetSecret`. `Worker.Secrets` (exported field, set by `app/engine.go` from the same system-secret resolver the plug-in context uses -- find it with `grep -n "ResolveSystemSecret" app/*.go`). In `admit`: when `req.TargetSecret != ""` -- medium must be webhook (else permanent); resolve (missing resolver, error or "" -> permanent `webhook: target secret <NAME> did not resolve`); match the resolved URL against the allowlist; return the transport and a COPY of the request whose `Target` is the resolved URL (memory only). In `WebhookTransport.Deliver`, wrap a client error through `redactURLError(err)`:

```go
// redactURLError drops the request URL net/http embeds in *url.Error: a
// webhook URL can carry its credential in the path (Discord's token), and
// this error is stamped into lastError and logged.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
```

- [ ] **Step 5: Gates.** Add `stageOutboundRequestToSecret` and `outboundRequestById` to `test/dslconformance/server_only_parsed_test.go`'s `want` map with their reasons; add `outboundRequestById` to `undeclaredRowAuthzConstructs` with `"memql#<OUTBOUND_TIER_ISSUE>"`; run `go run ./cmd/memqllint dsl/`, `make sdk-gen` (the concept field changes the SDK types), `make concept-snapshot`, then `go test ./component/outbound/ ./test/dslconformance/ -count=1` and `go test ./component/memql/ -run 'TestUndeclaredRowAuthz|TestEngineInitLoadsFullDSL' -count=1`.
- [ ] **Step 6: Docs.** Append a "Secret targets" section to `docs/internal/design/outbound-delivery-adr.md` (why, the descriptor, the resolver, the redaction).
- [ ] **Step 7: Commit** (named paths, including the regenerated SDK and snapshot files) -- `Issue #5504: outbound webhooks can target a globalSecret, resolved at send time`.

### Task 6a: The notification message (pure)

**Files:** Create `component/pipelines/notify.go`, `component/pipelines/notify_test.go`.

**Interfaces (produced):**

```go
// NotifyOutcome is what a notification announces.
type NotifyOutcome string

const (
	NotifyPassed    NotifyOutcome = "passed"
	NotifyFailed    NotifyOutcome = "failed"
	NotifyRecovered NotifyOutcome = "recovered" // passed, and the previous run of this pipeline and event did not
)

// Link is a labelled URL in a notification.
type Link struct {
	Label string `json:"label" yaml:"label"`
	URL   string `json:"url" yaml:"url"`
}

// Notification is everything a message says. Every string is already masked
// by the caller; the composers escape what they render.
type Notification struct {
	Pipeline    string // the pipeline's name: "memql"
	Event       Event
	Version     string // MEMQL_VERSION: the tag for a release, the SHA otherwise
	SHA         string
	Branch      string
	PullRequest int
	Title       string // the commit message's first line or the pull request's title
	Outcome     NotifyOutcome
	Stages      int   // stages before the notify stage that ran
	DurationMs  int64 // the run so far
	// The first failed step, for NotifyFailed: "deploy/verify-rollout", its code and message.
	FailedStep, FailedCode, FailedMessage string
	OSOrigin   string // "https://os.<domain>", "" when unknown
	RunPageURL string // "" when unknown
	Links      []Link // the stage's own links (Docs)
	Artifacts  []Link // the run's Library files
	At         time.Time
}

func DiscordMessage(n Notification) ([]byte, error)
func EmailMessage(n Notification) (subject, body string)
func Headline(n Notification) string
```

Rendering rules:
- Headline subject by event: release `Release <version>`; push `Push to <branch>` (or `Push <sha7>` without a branch); pull_request `Pull request #<n>`; merge_group `Merge queue <sha7>`. Then ` passed`, ` failed at <stage>` (the part of FailedStep before `/`), or ` recovered`.
- Discord: `{"username": "MemQL Pipelines", "allowed_mentions": {"parse": []}, "embeds": [{"title": "<pipeline> · <headline>", "url": <RunPageURL when set>, "description": ..., "color": 3066993 green for passed/recovered, 15158332 red for failed, "fields": [...], "timestamp": RFC3339}]}`. Description: line 1 the escaped title plus `(<sha7>)`; line 2 `All <n> stages passed in <duration>.` / `<FailedStep> failed: <FailedCode>. <FailedMessage truncated to 300 runes>` / `Passed after the previous run failed. All <n> stages passed in <duration>.`. Fields in order: `Version` (inline, the version; a SHA shown as its first 7), `MemQL OS` (`[Open](<origin>/)` when OSOrigin set; omitted otherwise -- never invented), `Deployment details` (`[Open the run](<RunPageURL>)`, or the plain text `Not available: this cluster has no MemQL OS domain configured.`), one field per `Links` entry (`[<host><path>](<url>)`), `Artifacts` (at most 5 `[name](url)` lines plus `and <k> more on the run page` when there are more).
- Limits enforced, cutting on rune boundaries with `...`: title 256, description 4096, field name 256, field value 1024, at most 25 fields, the whole embed at most 6000 characters (drop Artifacts lines first, then shorten the description).
- Escaping: every text that came from a repository or a person (Title, Branch, FailedMessage, link and artifact labels) has the Discord markdown characters backslash-escaped: backslash, `*`, `_`, `~`, backtick, `>`, `|`, `[`, `]`, `(`, `)`. `@` is left as written: `allowed_mentions.parse: []` is what stops a ping, and rewriting the text would misquote the commit.
- Email: subject `"<pipeline> · <headline>"`; body plain text: the description lines, then `Version: ...`, `MemQL OS: <origin>/`, `Deployment details: <run page URL>`, each link `<label>: <url>`, `Artifacts:` lines `- <name>: <url>`.

- [ ] **Step 1: Failing tests:** golden assertions for each outcome x event; `allowed_mentions.parse` is an empty array (not null, not absent); a title `[click](https://evil.test) @everyone **x**` renders escaped (`\[click\]\(https://evil.test\) @everyone \*\*x\*\*`); no `MemQL OS` field when OSOrigin is ""; `Deployment details` text when RunPageURL is ""; 9 artifacts -> 5 lines + "and 4 more"; a 10 000-rune FailedMessage keeps the embed under 6000 and every value under its cap; email subject equals the Discord title.
- [ ] **Step 2: Run** `go test ./component/pipelines/ -run 'Discord|EmailMessage|Headline' -count=1` -- FAIL.
- [ ] **Step 3: Implement** with `encoding/json` over small structs (field order fixed by struct order). The package stays standard-library only.
- [ ] **Step 4: Run** -- PASS.
- [ ] **Step 5: Commit** -- `Issue #5504: compose a notification for Discord and email`.

### Task 6b: `links:` on a notify stage

**Files:** Modify `component/pipelines/spec.go` (StageSpec.Links), `validate.go`, `compile.go`, `contract.go` (Step.Links); Tests in `validate_test.go`, `compile_test.go`.

**Interfaces:** `StageSpec.Links []Link` (yaml `links`); `Step.Links []Link` (json `links,omitempty`) on the compiled notify step.

- [ ] **Step 1: Failing tests:** a notify stage with `links: [{label: Docs, url: "https://memql.io/docs/"}]` compiles to a notify step carrying it; refused with `pipeline_stage_invalid` (scope the stage name): links on a stage with steps; more than 5; an empty label or one over 40 runes; a URL that is not absolute `https`, carries userinfo, or is over 512 bytes.
- [ ] **Step 2-4:** run (FAIL), implement in `validateStage`'s notify branch, run (PASS). Check `component/packages`' strict manifest decoder accepts the new key (it decodes into these types; run `go test ./component/packages/ -run Manifest -count=1`).
- [ ] **Step 5: Commit** -- `Issue #5504: a notify stage may carry its own links`.

### Task 6c: Channel and outbound rows the driver reads and writes

**Files:**
- Modify: `dsl/pipelines/mutations.memql` (append `createPipelineChannel`, `updatePipelineChannel`), `dsl/pipelines/queries.memql` (append `pipelineChannelsForOwner`, `pipelineChannelForOwnerByName`, `pipelineRunsForPipelineEvent`), `dsl/pipelines/shapes.memql` (append `pipelineChannelFull`)
- Modify: `component/pipelinerun/types.go` (Channel, OutboundStatus, NotificationRequest), `ports.go` (Store methods), `store_dsl.go` (+ constants), `fakes_test.go`, `store_dsl_test.go`
- Modify: `test/dslconformance/server_only_parsed_test.go`

**Interfaces (produced):**

```go
type Channel struct {
	ID, OwnerUserID, AccountID, Name, Kind, SecretRef, Status string
	Recipients []string
}
type ChannelPatch struct { // nil = not written; the read-merge keeps it
	Name, Kind, SecretRef, Status *string
	Recipients *[]string
}
type OutboundStatus struct {
	ID, Status, LastError string
	Attempts int
	SentAt time.Time
}
type NotificationRequest struct {
	RequestID, Medium, Target, TargetSecret, Subject, Body, DedupeKey, RequestedBy string
}

// Store additions:
ChannelsForOwner(ctx context.Context) ([]Channel, error)                       // person-facing
ChannelForOwnerByName(ctx context.Context, owner, name string) (*Channel, error) // under owner's borrowed actor
CreateChannel(ctx context.Context, c Channel) error                             // owner write
UpdateChannel(ctx context.Context, owner, channelID string, patch ChannelPatch) error
PreviousRuns(ctx context.Context, pipelineID string, event pipelines.Event) ([]Run, error) // system read, newest first
StageNotification(ctx context.Context, n NotificationRequest) error            // system write, internal origin
OutboundStatuses(ctx context.Context, ids []string) ([]OutboundStatus, error)  // system read
LibraryFileNames(ctx context.Context, owner string, ids []string) (map[string]string, error) // under owner's borrowed actor, libraryFileById
```

DSL (append; channels are a person's, written only by component/pipelinerun after the channel builtins validate -- the header of `mutations.memql` already says why every write is @serverOnly):

```memql
/// Create a notification channel (D16, memql#5480) at a caller-chosen id. The owner is stamped from the
/// actor, which under the borrowed authority is the person saving it.
///
/// @serverOnly rather than actor.userId scoping: a channel names a globalSecret the notify stage will
/// resolve and POST to, and is unique by name per owner; both checks are made by the channel builtin in
/// component/pipelinerun before this write, and a client-written row would skip them.
@serverOnly
@actor
mutation channel createPipelineChannel {
  args {
    channelId   string!
    accountId   string
    name        string!
    kind        string!
    secretRef   string
    recipients  []string
  }
  insert {
    accept { accountId, name, kind, secretRef, recipients }
    stamp {
      id: args.channelId
      ownerUserId: actor.userId
      status: "active"
    }
  }
}

/// Change a channel: its name, kind, secret reference, recipients or status. Read-merge with no
/// defaults, so a field not named keeps its value.
///
/// @serverOnly for createPipelineChannel's reason: the builtin checks the name stays unique and the secret
/// name is outside the platform's MEMQL_ namespace before it writes here.
@serverOnly
mutation channel updatePipelineChannel {
  args {
    channelId   string!
    name        string
    kind        string
    secretRef   string
    recipients  []string
    status      string
  }
  update {
    accept { name, kind, secretRef, recipients, status }
    stamp {
      id: args.channelId
    }
  }
}
```

Queries: `pipelineChannelsForOwner` (@actor, `filter row => row.ownerUserId == actor.userId`, `sort "name", "asc"`, `paginate 100`, `shape pipelineChannelFull`); `pipelineChannelForOwnerByName(name)` (@actor, owner conjunct, `sort "row.createdAt", "desc"`, `paginate 1`); `pipelineRunsForPipelineEvent(pipelineId, event)` (@serverOnly, `actor.isClusterOwner == true` conjunct like the other server reads, `sort "queuedAt", "desc"`, `paginate 20`, `shape` = the run shape the other run reads use). Shape `pipelineChannelFull`: `row.id`, `ownerUserId`, `accountId`, `name`, `kind`, `secretRef`, `recipients`, `status`, `row.createdAt`.

- [ ] **Step 1: Failing tests:** `store_dsl_test.go`'s rendered-call check (it reads the .memql files) covers the new constants; fakes implement the methods; a borrowed-actor test: `ChannelForOwnerByName` renders under `auth.ContextWithUserActor(owner)` (assert the fake engine saw that actor).
- [ ] **Step 2-4:** run (FAIL), implement, run (PASS): `go test ./component/pipelinerun/ -count=1`, `go run ./cmd/memqllint dsl/`, `make sdk-gen` (createPipelineChannel/updatePipelineChannel are @serverOnly and excluded; `pipelineChannelsForOwner` and `pipelineChannelForOwnerByName` appear), `go test ./test/dslconformance/ -count=1`, and `go test ./component/memql/ -run 'TestUndeclaredRowAuthz|TestEngineInitLoadsFullDSL|TestRowAuthz' -count=1`.
- [ ] **Step 5: Commit** -- `Issue #5504: channel rows and the reads the notify stage needs`.

### Task 6d: The notify executor

**Files:** Create `component/pipelinerun/notify.go`, `component/pipelinerun/notify_test.go`; Modify `component/pipelinerun/driver.go` (runStep, execute, settle keeps artifact ids), `types.go` (StepState.ArtifactFileIDs), `component/pipelines/refusal.go` (+ codes), `component/pipelines/summary.go` (copy), `component/packages/refusal.go` (literals).

**Interfaces:**
- Consumes: Task 6a (`pipelines.DiscordMessage`, `EmailMessage`, `Notification`), 6b (`Step.Links`), 6c (Store), 5 (`stageOutboundRequestToSecret` via `StageNotification` with `TargetSecret`).
- Produces codes (append to refusal.go, all `ClassFailure`): `CodeChannelMissing = "pipeline_channel_missing"`, `CodeChannelArchived = "pipeline_channel_archived"`, `CodeChannelNotAllowed = "pipeline_channel_not_allowed"`, `CodeChannelInvalid = "pipeline_channel_invalid"`, `CodeNotifyFailed = "pipeline_notify_failed"`, `CodeNotifyUndelivered = "pipeline_notify_undelivered"`. DELETE `CodeNotifyUnavailable` and its literal/copy (nothing produces it once the executor exists; pre-release, no stored rows carry it).

Behavior of `func (dr *runDriver) runNotify(ctx context.Context, t *stepTrack)`:
1. Intent: `t.handle = dr.work().Step(ctx, step.Key)`; status running.
2. Channel: `Store.ChannelForOwnerByName(ctx, owner, step.Channel)`. None -> fail `pipeline_channel_missing` ("No channel named %s belongs to this pipeline's owner. Create it in Deployables > Settings > Channels, then re-run."). `status == "archived"` -> `pipeline_channel_archived`. Its id (compare with `sameID`/`bareID`) not in `dr.p.ChannelIDs` -> `pipeline_channel_not_allowed` ("Channel %s exists, but pipeline %s is not one it accepts. Allow the pipeline on the channel in Deployables > Settings > Channels, then re-run.").
3. Discord: `SecretRef` must be non-empty and not `MEMQL_`-prefixed (`pipeline_channel_invalid`). Resolve it under the owner's borrowed actor with `dr.d.Secrets` ONLY to check the value is a Discord webhook URL (`https://` + host in {discord.com, discordapp.com, ptb.discord.com, canary.discord.com} + path prefix `/api/webhooks/`); `dr.remember(value)`; failure -> `pipeline_channel_invalid` naming the secret, never the value; an unresolved secret -> `pipeline_secret_missing` (existing code, its existing sentence shape). Stage ONE row: `NotificationRequest{Medium: "webhook", TargetSecret: SecretRef, Body: discordJSON, ...}`.
4. Email: one row per recipient: `Medium: "email", Target: <address>, Subject, Body`.
5. Request id per row: `"pn" + hex(sha256(runID + "|" + stepKey + "|" + attempt + "|" + targetOrSecret))[:40]`; `DedupeKey` = the same; `RequestedBy` = `"pipelines:notify:" + runID`. A resumed drive re-sends the same attempt and so re-stages onto the same rows (`@createOnly` keeps their status): nothing is delivered twice.
6. The Notification: outcome FAILED when any step of an earlier stage failed, was refused or was cancelled by its runner (first such step in declared order supplies FailedStep `stage/name`, Code, masked Message); else RECOVERED when `PreviousRuns(pipelineID, event)`'s newest COMPLETED run other than this one, queued before it, concluded anything but success; else PASSED. Stages = earlier stages that have a plan; DurationMs since `dr.run.StartedAt`; OSOrigin `dr.d.OSOrigin()`; RunPageURL `pipelines.RunPageURL(origin, bareID(dr.run.ID))` when origin set; Links `step.Links`; Artifacts from every earlier step's `StepState.ArtifactFileIDs`, named by `LibraryFileNames` (an unnamed id is labelled `artifact <n>`), URL `origin + "/?libraryFile=" + bareID(id)` (omitted when no origin).
7. Wait: poll `OutboundStatuses(ids)` -- 2s, then x1.5 up to 15s -- until every row is `sent` (success), any is `failed` (fail `pipeline_notify_failed` with "Delivery to %s failed after %d attempts: %s", the lastError masked), the step's timeout passes (fail `pipeline_notify_undelivered`: "Not delivered to %s within %s; the outbound worker is still retrying (attempt %d: %s) and keeps trying after this step ends."), the lease is lost (return, writing nothing) or the run is cancelled (cancel receipt).
8. Success receipt: `status WorkStepSucceeded`, report `StepSucceeded`, message `Delivered to <name> (Discord|email, <n> recipients)`, result `{"channel": name, "kind": kind, "requestIds": [...], "sentAt": latest}`.

Driver changes:
- `runStep`: `case step.Kind == pipelines.StepNotify: dr.runNotify(ctx, t); return`.
- `execute`: a stage whose tracks are all `StepNotify` RUNS even when `blockedBy != ""`; every other blocked stage keeps `pipeline_stage_blocked`. Cancelled: unchanged (the cancel path settles notify with a cancel receipt; nothing is sent).
- `settle`: copy `rec.artifactFileIDs` into the track's `StepState.ArtifactFileIDs`.

- [ ] **Step 1: Failing tests** (driver over fakes; reuse `fakes_test.go`'s engine/store/journal):
  1. passed run, Discord channel allowed -> one staged row with `TargetSecret`, `Body` JSON containing the run page URL; the fake worker flips it `sent` on the 2nd status read -> the notify step succeeded, message names the channel.
  2. a failed tests stage, then deploy blocked `pipeline_stage_blocked`, then notify RUNS and its body names the failed step and code.
  3. a cancelled run -> notify settled cancelled; nothing staged.
  4. channel missing / archived / not allowed -> each its code; nothing staged.
  5. secret resolves to `https://example.test/hook` -> `pipeline_channel_invalid`; the receipt message contains neither the URL nor the host.
  6. email channel with 2 recipients -> 2 rows; both `sent` -> success.
  7. outbound row `failed` with lastError containing the resolved webhook value -> `pipeline_notify_failed` whose message is masked.
  8. never sent within a 1s test timeout -> `pipeline_notify_undelivered`.
  9. RECOVERED: previous run of (pipeline, push) concluded failure -> title says recovered.
  10. HOP: the staging replica's driver and a "worker" on another fake replica share only the store; the driver concludes from the row alone (no in-memory channel between them).
  11. RESUME: a first drive stages, its lease is lost; a second drive with the same attempt re-stages onto the same request id (the fake store records one row, `@createOnly` semantics) and concludes `sent` without a second delivery.
- [ ] **Step 2: Run** `go test ./component/pipelinerun/ -run Notify -count=1` -- FAIL.
- [ ] **Step 3: Implement** notify.go; driver edits; codes + summary copy + packages literals (run `go test ./component/packages/ -run Refusal -count=1` for the parity test).
- [ ] **Step 4: Run** `go test ./component/pipelinerun/ ./component/pipelines/ ./component/packages/ -count=1` -- PASS; then the db-gated run of `component/pipelinerun` against a real Postgres (see "Testing against Postgres" below).
- [ ] **Step 5: Commit** -- `Issue #5504: the notify stage delivers over the outbound path with the run page as its report link`.

### Task 7: The false-green count (M1's measurement)

**Files:** Create `component/pipelines/falsegreen.go`, `falsegreen_test.go`.

**Interfaces (produced):**

```go
// RunFacts is what the count reads off a v1:pipelines:run row.
type RunFacts struct {
	ID, SHA, HeadBranch, Title, Conclusion string
	Event       Event
	Mode        Mode
	PullRequest int
	QueuedAt    time.Time
}
// StepFacts is one work step of a run: its key and how it ended.
type StepFacts struct{ Key, Status, Code string }

type FalseGreen struct {
	PullRequest int    `json:"pullRequest"`
	PRRunID     string `json:"prRunId"`
	FullRunID   string `json:"fullRunId"`
	// Steps are the full run's failed steps with what the pull request's run did with each:
	// "not selected" (skipped pipeline_not_affected, or absent) or "passed".
	Steps []FalseGreenStep `json:"steps"`
}
type FalseGreenStep struct{ Key, OnPullRequest string }

type FalseGreenReport struct {
	FullRuns         int          `json:"fullRuns"`         // concluded full-mode runs in the window
	FullRunsFailed   int          `json:"fullRunsFailed"`
	AfterGreen       int          `json:"afterGreen"`       // failed full runs whose PR's last run passed
	FalseGreens      []FalseGreen `json:"falseGreens"`      // of those, a failed step the PR run did not select
	SiblingOrFlake   []FalseGreen `json:"siblingOrFlake"`   // of those, every failed step passed on the PR
}

// PullRequestOf names the pull request a full-mode run landed: a merge
// group's head branch gh-readonly-queue/<base>/pr-<n>-<sha>, or a push whose
// head commit is "Merge pull request #<n> from ..." or ends "(#<n>)".
func PullRequestOf(r RunFacts) (int, bool)

func CountFalseGreens(runs []RunFacts, steps map[string][]StepFacts) FalseGreenReport
```

Definition (record section 7 and #5506): a false green is a pull-request run that reported success on content whose full-mode run then failed, NOT a sibling merge. A failed full run is paired with its pull request's newest `pull_request` run queued before it. When that run concluded success, the pair is counted in `AfterGreen`; it is a FALSE GREEN when at least one failed step of the full run (shard suffix `#i` stripped, so `tests.go-tests#2` compares as `tests.go-tests`) was not executed by the pull-request run -- the affected selection missed it, which is what the affected-subset decision rests on. When every failed step ran and passed on the pull request, the content passed and the queue's base differed: a sibling merge or a flake, listed apart.

- [ ] **Step 1: Failing tests:** `PullRequestOf` for each title/branch form and a non-PR push; a selection miss; a sibling (step passed on the PR); a failed PR run (not counted); a full run with no matching PR run (not counted); shard suffix normalization; window counts.
- [ ] **Step 2-4:** run (FAIL), implement (stdlib only), run (PASS).
- [ ] **Step 5: Commit** -- `Issue #5506: count false greens from run rows`.

### Task 8: Docs for phase A

**Files:** Modify `docs/public/operate/pipelines.md`; Create `docs/internal/ops/pipelines-milestones.md` (front-matter per DOCS_STANDARD: copy the block from a sibling `docs/internal/ops/*.md`); Modify `GLOSSARY.md` (one line for the new doc, if GLOSSARY indexes internal ops docs -- check with `grep -n "internal/ops" GLOSSARY.md | head`); Modify the record: append `## 10. Implementation notes (epic 5)` to `docs/superpowers/specs/2026-09-16-pipelines-program-design.md`.

- [ ] **Step 1:** pipelines.md -- new sections: "Notifications" (channels: kinds, the secret reference, which pipelines a channel accepts; the outbound worker's `MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST` must admit `https://discord.com/api/webhooks/` and the email allowlist the recipients; the message fields; outcomes passed/failed/recovered; that a notify stage runs after a failed stage and not after a cancel; the six codes and their remedies; `links:`), "Verifying a rollout" (`memql-verify` flags, what each probe compares, codes, evidence artifact, `MEMQL_DOMAIN`, `/healthz` `version`/`commit`, `memql-bundle.json`), "False greens" (the definition, the builtin), and a "Milestones" pointer to the ops doc.
- [ ] **Step 2:** pipelines-milestones.md -- one section per milestone, each a checklist of the REMAINING gate with exact commands (release cut; ZNAS pin in memql-znas; GHCR `memql-toolchain` and `ci-timescaledb` public; dispatch `build-toolchain-image.yml` and pin its digest; connect the engine pipeline on ZNAS with `delivery: webhook`; create the `releases` channel on ZNAS (globalSecret from Key Vault `memql-discord-deployment-webhook`), allow the pipeline on it, add `https://discord.com/api/webhooks/` to the ZNAS outbound webhook allowlist; add the check `MemQL / memql` to ruleset 16630577 beside `ci-required` and record it in `ruleset-baseline.md`; two weeks; read `pipelinesFalseGreens`; M2: drop `merge_group`/`push` from `ci.yml` (blocked while merges bypass the queue, D16); M3: three releases with verify-rollout and notify green, observer to zero replicas in memql-znas `deploy/notifications/kustomization.yaml`, retained one release, then removed; placeholder in memql-project `docs/design/deployment-notifications` replaced by the run-page link; docs deploy stage commits the docs set into the instance repo and `publish-docs-bundle.yml` + the instance's `docs-sync.yml` retire; M4: engine image build as a pipeline stage (needs Docker: a fleet step `needs: { docker: true }`), then delete `ci.yml` and `build-engine-images.yml` last, point `scripts/release/release-engine.sh` at the pipeline, decide `publish-sdk-core.yml`, `publish-vscode-extension.yml`, `dispatch-engine-images-on-release.yml`).
- [ ] **Step 3:** record section 10 -- the readings this epic settled: notify executed by the driver (not the runner) and not blocked by a failed stage; Discord over a secret reference resolved at send time; channels accept pipelines by `pipeline.channelIds`; `links:`; MEMQL_DOMAIN in the step contract; `/healthz` as the deploy-version endpoint; `memql-bundle.json` as the OS bundle hash from the edge image layer; verify-rollout as a command step; false-green definition; what the engine manifest's stages are; owner decision on the milestones.
- [ ] **Step 4:** run `go test -count=1 -run 'TestDocs|Boundary|FrontMatter' .` and `go test -count=1 ./docs/... 2>/dev/null` (whichever the docs gates are: `grep -ln "func TestDocs" *.go docs/*.go`), fix any finding.
- [ ] **Step 5: Commit** -- `Issue #5480: document notifications, verify-rollout, false greens and the milestone gates`.

### Task 9: Phase A sweep

- [ ] Run, in the worktree, and fix every finding: `go build github.com/znasllc-io/memql/...`, `go vet` on the touched packages, `go run ./cmd/memqllint dsl/`, `make sdk-gen-check`, `make concept-snapshot-check`, `make arch-model` then `make arch-model-check`, `make platform-graph` then `make platform-graph-check`, `make env-registry-check`, `make frontdoor-paths-check`, `make test` (redirect to a file; read the failures), and the db-gated packages `component/pipelinerun`, `component/memql` (only the tests whose names this epic's constructs touch) against a real Postgres.
- [ ] Commit the regenerated artifacts by path -- `Issue #5480: regenerate the architecture model, platform graph and SDK`.
- [ ] Push the branch: `git -C ../epic-pipelines-delivery push -u origin epic/pipelines-delivery`.

**Testing against Postgres.** Use the throwaway the memory notes describe (`db-gated-tests-real-postgres-is-port-15434`), never the k3d front door: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://...:15434/... go test -count=1 ./component/pipelinerun/`.

---

## Phase B -- the engine's own manifest (after epic 3 merges)

### Task 10: docs, deploy and notify stages in `memql-package.yaml`

**Files:** Modify `memql-package.yaml`, `memql_package_manifest_test.go`.

Stages appended after epic 3's `gates` stage (the comments in the file explain each; keep the file's comment style):

```yaml
    # The docs stage (documentation program D15): the generated pages and the
    # graphs the docs draw are checked on a push and a release, and a release
    # builds the bundle the site serves, kept as the run's Library files.
    # During M1 the GitHub Actions publish workflow stays authoritative:
    # nothing here publishes.
    - name: docs
      on: [push, release]
      steps:
        - name: docs-checks
          run: |
            set -eu
            bash scripts/identity/build-css.sh
            make docs-matrix-check docs-grammar-check arch-model-check platform-graph-check
            bash scripts/docs/build-docs-bundle.sh --version="$(cat VERSION)" --check
    # A step has no `on` of its own, so the release-only bundle is a stage.
    # The bundle dates every page by its last commit, which a shallow clone
    # cannot answer: the step deepens its own working copy first (a public
    # repository, so no credential is involved).
    - name: bundle
      on: [release]
      steps:
        - name: docs-bundle
          artifacts: [docs-*.tgz, docs-out/manifest.json, docs-out/llms-full.txt]
          run: |
            set -eu
            if [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
              git fetch --unshallow --tags origin
            fi
            bash scripts/docs/build-docs-bundle.sh --version="${MEMQL_VERSION#v}"
            mkdir -p docs-out
            tar -xzf "docs-${MEMQL_VERSION#v}.tgz" -C docs-out ./manifest.json ./llms-full.txt
```

Before writing this stage, read epic 3's runner (`integrations/pipelinesteps/jobspec.go`) for how the working copy is made: if it is a tarball rather than a git clone, `git rev-parse` fails and the step must instead `git clone --filter=blob:none https://github.com/<owner>/<repo>.git` at `$MEMQL_SHA` into a temp directory and build from there; write whichever is true, with the comment saying so.

```yaml
    # THE BOOTSTRAP RULE (D16): this pipeline runs on a RELEASED instance and
    # never deploys the tree under test. A release reaches that instance by
    # the existing release path (images built from the tag, digests pinned in
    # the instance repository, Argo); this stage verifies it from outside --
    # the same probe any deploy target gets -- and fails typed with its
    # evidence when the instance is not serving the release within the wait.
    - name: deploy
      on: [release]
      steps:
        - name: verify-rollout
          timeout: 2h
          artifacts: [verify-rollout.json]
          run: |
            memql-verify --domain="$MEMQL_DOMAIN" --version="$MEMQL_VERSION" \
              --docs=https://memql.io/docs/ --wait=110m --evidence=verify-rollout.json
    - name: notify
      on: [release]
      channel: releases
      links:
        - label: Docs
          url: https://memql.io/docs/
```

- [ ] **Step 1:** Update the test's pinned stage order per opening: pull_request `checks, tests, gates`; merge_group `checks, tests`; push `checks, tests, docs`; release `checks, tests, docs, bundle, deploy, notify`. Add: the verify step names `memql-verify` with `$MEMQL_DOMAIN` and `$MEMQL_VERSION`; the notify stage names a channel and the docs link; the bundle stage's artifacts.
- [ ] **Step 2-4:** run `go test -count=1 -run 'TestEngineManifest' .` (FAIL), edit the manifest, run (PASS).
- [ ] **Step 5: Commit** -- `Issue #5506: the engine's pipeline gains its docs, deploy and notify stages`.

### Task 11: `memql-verify` in the toolchain image

**Files:** Modify `deploy/toolchain-image/Dockerfile`, `deploy/toolchain-image/smoke-test.sh`, `scripts/ci/toolchain_image_workflow_test.go` (one line each, following epic 3's pattern for the other binaries).

- [ ] Add a digest-pinned Go build stage (`FROM golang:<the toolchain's Go>@sha256:... AS memql-verify`) that builds `./cmd/memql-verify` with `CGO_ENABLED=0` from the repository context, and `COPY --from=memql-verify /out/memql-verify /usr/local/bin/memql-verify` in the final stage. If the workflow's build context is not the repository root, build from a `git archive` of the tag instead and say so in a comment.
- [ ] Smoke test: `memql-verify --version= 2>&1; test $? -eq 2` (bad flags exit 2).
- [ ] Run `go test -count=1 ./scripts/ci/ -run Toolchain`; build the image locally if Docker is available: `docker build -f deploy/toolchain-image/Dockerfile .` then run the smoke test in it.
- [ ] Commit -- `Issue #5505: the toolchain image carries memql-verify`. The image must be rebuilt and its digest re-pinned after merge: a gate line in `pipelines-milestones.md`.

---

## Phase C -- MemQL OS (after epic 4 merges)

Read first: `clients/os/README.md` (live collections, arrival cues), `clients/os/DESIGN.md` (the twelve rules), `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, and epic 4's `clients/os/src/apps/deployables/pipelines/{words,rows,stops,acts,calls}.ts`. Use `words.ts` as the one vocabulary; add words there, never inline.

### Task 12: Channel and false-green builtins

**Files:** Modify `dsl/pipelines/builtins.memql` (append), Create `component/pipelinerun/channels.go`, `channels_test.go`, `falsegreens.go`, `falsegreens_test.go`; Modify `component/pipelinerun/capabilities.go` (register executors).

```memql
@sdk
@executor("integration.pipelines.channelSave")
@args(profile="object")
@requiresCapability("execute", "app:deployables/channels")
@description("Create or change one of the caller's notification channels (D16). A channel is where a notify stage delivers: a Discord webhook, named by the globalSecret holding its URL, or an email list delivered through the cluster's sender. Its name is what a manifest's notify stage says, unique among the caller's channels. pipelineIds is the whole set of the caller's pipelines this channel accepts deliveries from; a pipeline not listed is refused pipeline_channel_not_allowed. Answers {channelId, name, kind, status, pipelineIds}.")
builtin pipelinesChannelSave {
  channelId    string    @description("The channel to change. Omitted to create one.")
  name         string!   @description("What a manifest's notify stage names: letters, digits and hyphens.")
  kind         string!   @description("discord or email.")
  secretRef    string    @description("For discord: the globalSecret NAME holding the webhook URL. Never the URL.")
  recipients   []string  @description("For email: 1 to 20 addresses.")
  pipelineIds  []string  @description("The caller's pipelines this channel accepts deliveries from.")
}

@sdk
@executor("integration.pipelines.channelArchive")
@args(profile="object")
@requiresCapability("execute", "app:deployables/channels")
@description("Archive one of the caller's channels: it delivers nothing more, and a notify stage naming it fails pipeline_channel_archived. The row stays for the runs that name it.")
builtin pipelinesChannelArchive {
  channelId  string!  @description("The channel to archive. Must be the caller's own.")
}

@sdk
@executor("integration.pipelines.channelTest")
@args(profile="object")
@requiresCapability("execute", "app:deployables/channels")
@description("Send a test message to one of the caller's channels now, over the same outbound path a notify stage uses, and wait up to 30 seconds for the worker. Answers {state: sent|failed|pending, attempts, lastError}; pending means staged and not yet delivered, never success.")
builtin pipelinesChannelTest {
  channelId  string!  @description("The channel to test. Must be the caller's own.")
}

@sdk
@executor("integration.pipelines.falseGreens")
@args(profile="object")
@requiresCapability("read", "app:deployables")
@description("Count false greens for one of the caller's pipelines over the last `days` days (M1, design record section 7): full-mode runs that failed although the pull request they landed had passed, split into selection misses (a failed step the pull request's run did not select) and the rest (every failed step passed on the pull request: a sibling merge or a flake). Answers the FalseGreenReport.")
builtin pipelinesFalseGreens {
  pipelineId  string!  @description("The pipeline to count. Must be the caller's own.")
  days        int      @description("The window, 1 to 90; 14 when omitted (M1's two weeks).")
}
```

(Confirm the read part name epic 4 uses for viewing runs -- `grep -n "app:deployables" dsl/rbac/seeds.memql` -- and use it for falseGreens.)

Handler rules for `channelSave`: name matches the manifest name grammar (`pipelines.ValidName` or the validator's `manifestNameRe`; export a helper if needed); kind in {discord, email}; discord: `secretRef` matches `^[A-Z][A-Z0-9_]{0,63}$`, not `MEMQL_`-prefixed, recipients empty; email: 1-20 bare mailboxes (`net/mail`, no display names), secretRef empty; unique name per owner checked under the gate `"pipelines:channel:" + owner + ":" + name` with a fresh read; every pipelineId resolves through `PipelineForOwner` (refused by name otherwise); write the channel (create at `"pch" + random hex` id, or update); then for each of the caller's pipelines set `channelIds` to include or exclude this channel per `pipelineIds` (`UpdatePipeline` under the owner). `channelTest` stages a Notification with Title "Test message from MemQL", Outcome passed, Event push, and waits with the same poll loop as the notify executor (factor the loop into one function both use). `falseGreens` reads the caller's pipeline (owner-scoped), its runs in the window (add a server read if `pipelineRunsForOwner`'s page is too small: `pipelineRunsForPipelineSince(pipelineId, since)`, @serverOnly, paginate 500), the work steps of the paired runs, and returns `pipelines.CountFalseGreens`.

- [ ] Tests: each refusal; a second channel with a taken name; a pipeline id that is not the caller's; the allowlist update adds and removes; channelTest answers pending/sent/failed from the fake; falseGreens over fixtures.
- [ ] `go run ./cmd/memqllint dsl/`, `make sdk-gen` (all four appear), `go test ./component/pipelinerun/ -count=1`, `go test ./component/memql/ -run 'TestEngineInitLoadsFullDSL|Capability' -count=1`.
- [ ] Commit -- `Issue #5504: channels are saved, archived and tested from the client; Issue #5506: the false-green count is a builtin`.

### Task 13: Deployables > Settings > Channels

Invoke `frontend-design:frontend-design` before building. Clean and minimal, every action reachable by keyboard, real data only.

**Files:** Create `clients/os/src/apps/deployables/channels/ChannelsSection.tsx`, `ChannelForm.tsx`, `channels.ts` (pure: row projection, validation mirror for inline hints, summary words); Modify `clients/os/src/apps/deployables/settings.ts` (the section beside Sources, `requires: "app:deployables/channels"`), `pipelines/words.ts`, `pipelines/calls.ts`; Tests under `clients/os/test/deployables/channels/`.

Layout and behavior:
- List: one line per channel -- name; `Discord · secret DISCORD_RELEASES` or `Email · 2 recipients`; the pipelines it accepts (`memql, shop-site`, or `No pipeline yet`); status (archived lines dimmed, labelled `Archived`). A retained live collection over `pipelineChannelsForOwner` (README live-collection contract); arrival cue only on a new channel.
- Empty state: one sentence ("A channel is where a pipeline's notify stage announces its runs.") plus the `New channel` action and an info detail naming the manifest syntax (`channel: <name>`).
- Form (guided, one column): Name; Kind as a two-option segmented control (Discord webhook / Email); for Discord, the secret NAME as a text field with the hint "The globalSecret holding the webhook URL. Ask a cluster owner to store it."; for Email, recipients as a chip input; "Accepts deliveries from" as a checklist of the caller's pipelines (switches are for true on/off settings; this is multi-selection -- checkboxes). Save / Cancel. Inline refusal copy from the engine's code (never a generic error). Drafts kept when the window loses focus.
- Per channel: `Send test message` (shows Sending..., then `Delivered` / `Failed: <reason>` / `Staged, not delivered yet` -- success only after `sent`), `Edit`, `Archive` (confirm inline with the consequence: "Runs naming it will fail pipeline_channel_archived.").
- Light and dark from tokens only; narrow width collapses the line to two rows.

- [ ] Tests (vitest, from inside `clients/os`): list renders projections; empty state; form validation hints; save calls `pipelinesChannelSave` with the exact args; test-send states; archive confirm; keyboard reachability of every action (role/name queries).
- [ ] `make os-typecheck`, `cd clients/os && npx vitest run test/deployables`, `make os-build`.
- [ ] Commit -- `Issue #5504: manage notification channels in Deployables settings`.

### Task 14: PipelinePage fact lines (channels, false greens)

**Files:** Modify `clients/os/src/apps/deployables/pipelines/PipelinePage.tsx` and its test.

- Channels: for each notify stage of the pipeline's plan (from epic 4's `pipelinesPreview` stages, or the last run's stages when the preview does not carry channels -- check what it returns), one fact: `Notifies releases -- ready` / `-- no such channel` / `-- archived` / `-- does not accept this pipeline`, with a text link `Manage channels` to Deployables > Settings > Channels (an in-app navigation that keeps a contextual return).
- False greens: `False greens, last 14 days: 0 of 9 failed queue runs` with an info detail stating the definition; when there are any, a disclosure listing each pair (PR number, the run links, the steps and `not selected` / `passed on the PR`). When no full runs exist: `No full runs in the last 14 days` (never `0`).

- [ ] Tests for each state; commit -- `Issue #5506: the pipeline page shows its channels and its false greens`.

### Task 15: The run page shows delivery; OS copy for the new codes

**Files:** Modify `clients/os/src/apps/deployables/pipelines/StageSteps.tsx` (or wherever epic 4 renders a step), `clients/os/src/apps/deployables/packages/refusals.ts` (copy for the six new codes; remove `pipeline_notify_unavailable`), tests.

- A notify step that succeeded reads `Delivered to releases (Discord) at 14:02` from the step's result; a failed one shows its code's copy and remedy. A verify-rollout step needs no special case: its log tail is the typed lines.

- [ ] Tests; `make os-typecheck`; commit -- `Issue #5504: the run page says where a notification went`.

### Task 16: `?libraryFile=<id>` opens a Library file

**Files:** Create `clients/os/src/apps/files/openFile.ts` (modelled line for line on `apps/concepts/openConcept.ts`: read, scrub with `replaceState`, park, take once); Modify `clients/os/src/main.tsx` (capture at module scope beside the other captures), the Files app (consume the parked id once: open the file's Inspector); tests modelled on `openConcept`'s.

- [ ] Commit -- `Issue #5504: a notification's artifact link opens the file in the Library`.

### Task 17: Visual QA in a real browser

- [ ] Bring the OS up against real services (`make dev NODE=edge` and the bff, or the Vite QA harness from the memory notes for fixture-only screens), and check Deployables > Settings > Channels, PipelinePage and the run page at desktop and narrow widths, light and dark, empty and populated, keyboard focus on every action. State in the PR which screens were judged against real services and which against fixtures. Do not mint credentials, pair machines or alter owner data to populate a screen.
- [ ] Fix what the pass finds; commit by path.

---

## Phase D -- finish

### Task 18: Verify, review, merge, close, clean

- [ ] Full sweep on the rebased branch (Task 9's list plus `make os-typecheck`, `make os-test`, `make os-build`, `cd clients/os && npx vitest run`).
- [ ] Whole-branch review by a fresh reviewer (`superpowers:requesting-code-review`); fix findings.
- [ ] Delete this plan in the last commit (`git rm docs/superpowers/plans/2026-10-04-pipelines-delivery.md`).
- [ ] Open the PR against `main`: body lists what landed per issue, `Closes #5504`, `Closes #5505` (one keyword per line), `Refs #5506`, `Refs #5507`, `Refs #5508`, `Refs #5509`, `Refs #5480`, the gates that remain (link `docs/internal/ops/pipelines-milestones.md`), the frontend note (new client-visible builtins; no changed wire field), and what was verified against real services vs fixtures.
- [ ] Watch `ci-required` (statusCheckRollup); merge with `scripts/dev/merge-as-owner.sh --pr=<n> --check` then `--pr=<n>`.
- [ ] After the merge: compare `main^{tree}` with the branch head's tree; if they differ, run the sweep on main and watch main's push CI by full SHA.
- [ ] Comment on #5506-#5509 (what landed, the remaining gate, the ops doc link) and on #5480; confirm #5504/#5505 closed; leave the epic open.
- [ ] Clean up: remove `../epic-pipelines-delivery`, delete the local branch; re-survey worktrees and branches (`git worktree list`, `git branch`, `git ls-remote --heads origin`) and remove only what is merged, clean and not a live session's (ask the peers first).
