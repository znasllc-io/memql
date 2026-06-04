# CI Tier 4 (north star) — build graph + remote cache

Part of the CI-acceleration epic (#854). This is the **plan + decision
record + a runnable spike bootstrap** for the durable endgame, per the
issue's "spike first" framing (#859). It is deliberately NOT a big-bang
migration: nothing at the repo root changes, the existing Go + CI build
is untouched, and the executable spike runs on a throwaway branch.

## Goal

Make CI time a function of **change size, not repo size**: model the repo
as a dependency graph, compute the affected target set from the PR diff,
build/test only that, and back it with a **content-addressed remote
cache** so every unchanged target is a cache hit across machines, PRs, and
branches. This is the model Google (Bazel) and Meta (Buck2) run.

Tiers 0–3 (#855–#858) already capture most of the practical win at low
risk: concurrency cancellation, affected-lane routing, a shared Go build
cache, and a merge queue. Tier 4 is the structural ceiling-raiser to adopt
when repo growth makes per-lane routing too coarse.

## Where Tier 4 fits vs. Tiers 0–3

| | Granularity | Cache | Effort | Risk |
|---|---|---|---|---|
| Tier 1 (routing) | whole lanes by changed path globs | none | low | low |
| Tier 2 (warm cache) | package (Go build/test cache) | per-run / per-key | low | low |
| **Tier 4 (Bazel)** | **per-target, transitive rdeps of the diff** | **content-addressed, cross-machine** | **high** | **medium** |

Tier 4 supersedes 1+2 for the Go tree once it's in place; keep the merge
queue (Tier 3) regardless.

## Recommended stack

- **Go:** Bazel (Bzlmod / `MODULE.bazel`) + `rules_go` + **Gazelle**
  (auto-generates and maintains `BUILD.bazel` from the existing Go source
  and `go.mod`, so we do not hand-write build files).
- **Remote cache:** BuildBuddy (hosted, free tier) or self-hosted
  `bazel-remote` (S3/Azure-blob backed). CI runs `bazel test //...
  --config=remote`; unchanged targets are cache hits.
- **sdk/ts:** Nx or Turborepo `affected` graph + its own remote cache.
- **Affected targets:** compute from `git diff` base..head →
  `bazel query 'rdeps(//..., set(<changed targets>))'` (or
  `target-determinator`) so CI runs only the impacted targets.

## memQL-specific hard parts (must be handled in the spike)

These are the reasons this is a spike, not a drop-in:

1. **`go.work` multi-module workspace.** `go.work` uses `./memql`,
   `./memql-cockpit`, `./memql-bff-copresent`. Bazel/Bzlmod resolves Go
   deps via `go.mod` per module + `use_repo(go_deps, ...)`. The spike
   should scope to the `memql` module first (the bulk of CI), not all
   three at once.
2. **`//go:embed` of the DSL tree (and ~11 embed sites).** rules_go needs
   each embedded fileset declared as `embedsrcs` on the `go_library`.
   Gazelle handles `//go:embed` directives, but verify the DSL tree
   (`dsl/**`, read via `component/memql/dslfs`) and the `prompts/*.tmpl`
   files are captured — a missed embed is a runtime "file not found", not
   a build error, so test the engine-load path under Bazel.
3. **CGO voice path behind `//go:build voice`.** `integrations/voice/**`
   pulls libopus/opusfile/soxr via cgo. Model this as a separate
   `config_setting` + `go test` with `--define gotags=voice` and the C
   deps provided via a toolchain or `cc_library`; keep it off the default
   `//...` target so the CGO-free graph stays hermetic.
4. **Generated SDK (`sdk/go`) + the `sdk-gen` / `dsl-lint` /
   `engine-load` checks.** These are `go_test` / `go_binary` runs today;
   they become Bazel test targets. The `sdk-gen-check` "no drift" check
   maps to a `bazel test` that runs the generator and diffs — or stays a
   non-Bazel lane initially.
5. **`MEMQL_DSL_PATH` override + embedded-vs-disk FS.** Tests that read
   the on-disk tree need the runfiles path; default (embedded) tests are
   hermetic. Validate both.

## Incremental migration plan (no big-bang)

1. **Spike on a branch** (`scripts/bazel-spike/bootstrap.sh`): generate
   `MODULE.bazel` + `.bazelrc` + run Gazelle over the `memql` module,
   `bazel build //component/memql/...` then `//...` (CGO-free), and prove
   a remote-cache hit by building twice (second run all cached) and from a
   second machine/CI runner.
2. **Add a NON-required `bazel` CI lane** gated behind Tier-1 routing
   (`go` bucket), running `bazel test //...  --config=remote` in parallel
   with the existing Go lanes. Compare wall-clock + cache-hit-rate for a
   few weeks. It does not gate merges yet.
3. **Promote** once the Bazel lane is faster and stable: make it the
   `go`-bucket lane (the existing build/test/vet/dsl-lint/engine-load
   lanes fold into `bazel test` targets), keep `ci-required` aggregating.
4. **Affected-targets**: switch the lane from `//...` to the rdeps of the
   diff so CI scales with change size.
5. **sdk/ts**: add Nx/Turborepo affected + remote cache as a sibling lane.
6. **Tooling**: commit the Gazelle-generated `BUILD.bazel` files and add a
   `gazelle` CI check that fails if they're stale (the analog of
   `sdk-gen-check`).

## Decision record

- **Decision:** land this plan + an inert spike bootstrap now; execute the
  build-proof spike as a follow-up on a branch. Do NOT commit root-level
  Bazel files to `main` until the spike proves a green `bazel test //...`
  + a remote-cache hit, because a partial Bazel setup at the repo root
  would confuse Go tooling and the existing CI.
- **Why now:** the plan + scaffold de-risk and schedule the work; the
  practical CI win is already delivered by Tiers 0–3.
- **Blocked-on for the build-proof:** a Bazel/bazelisk toolchain and a
  remote-cache backend (BuildBuddy API key or a self-hosted `bazel-remote`
  endpoint) — neither is available in the environment that authored this
  plan, so the proof is the documented next action, not part of this PR.

## Running the spike

See `scripts/bazel-spike/` — `bootstrap.sh` writes the root Bazel files
into the working tree (on a throwaway branch), installs bazelisk if
needed, runs Gazelle, and attempts `bazel build //component/memql/...`.
It is experimental and intentionally not referenced by any CI workflow or
Makefile target.

## Acceptance status (#859)

- [x] Migration plan documented (this file) with the memQL-specific
      hard parts and an incremental, non-big-bang path.
- [x] Spike scaffold provided (`scripts/bazel-spike/`) — runnable on a
      branch.
- [ ] Build-proof: `bazel test //...` green + a demonstrated cross-CI
      remote-cache hit. Deferred to the spike branch (needs a Bazel
      toolchain + remote-cache backend; see Decision record).
