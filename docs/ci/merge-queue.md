# CI Tier 3 — GitHub merge queue

Part of the CI-acceleration epic (#854). This is the workflow-side wiring
plus the **operator steps** to actually turn the queue on (a repo-admin
action that cannot be done from a workflow file).

## Why

Without a merge queue, the full CI suite re-runs on every push to every
open PR, and again implicitly when each PR merges. The heavy lanes
(CodeQL `Analyze` across go/js/python, the CGO voice build, the full
`go test`) are the long pole. A merge queue runs the **fast affected
subset on the PR** (Tier 1 routing, #856) and the **full suite once on
the batched merge candidate** — so the expensive coverage happens a
single time on the exact tree that will land, not N times across PR
pushes.

## What shipped in the workflows (this PR, #858)

`merge_group:` triggers were added to the three workflows that produce
checks gating a merge:

- `.github/workflows/ci.yml` — produces the single required `ci-required`
  aggregator (and all the lanes behind it).
- `.github/workflows/codeql.yml` — the `Analyze` security checks.
- `.github/workflows/gitleaks.yml` — the secret scan.

Key behaviors that make this correct:

- **Full coverage on the candidate.** The Tier-1 affected-lane routing in
  `ci.yml` narrows work only on `pull_request` events (`if: github.event_name
  != 'pull_request' || ...`). A `merge_group` event is not a `pull_request`,
  so every lane runs — the merge candidate always gets the full suite.
- **Concurrency.** The `concurrency.group` keys off
  `github.event.pull_request.number || github.sha`. A merge_group candidate
  has no PR number, so it keys off the unique queue merge commit SHA and
  is never cancelled by an unrelated run.

## Operator steps to ENABLE the queue (repo admin)

These are GitHub repo settings, not code:

1. **Settings → Branches → Branch protection rule / Ruleset for `main`** →
   enable **"Require merge queue"**.
2. In the same rule, set **Required status checks** to the checks that run
   on `merge_group`:
   - `ci-required` (the Tier-1 aggregator — the primary gate)
   - the CodeQL `Analyze (...)` checks, if you keep them required
   - `gitleaks`, if you keep it required
   Do **not** mark the individual `ci.yml` lanes (build/test/vet/...) as
   required — only `ci-required`. A path-skipped lane reports `skipped`,
   which would stall a required check (this is exactly what the Tier-1
   aggregator exists to avoid).
3. Merge-queue build settings: a reasonable starting point is
   - Maximum PRs to build together: 5
   - Minimum: 1, wait up to ~5 min to batch
   - "Only merge non-failing PRs" / build in the merge group.

## Required-checks caveat (read before flipping it on)

A merge queue stalls if a **required** check never reports a result for
the merge candidate. So the rule is: **every required check must trigger
on `merge_group`.** This PR covers `ci-required` + CodeQL + gitleaks.

The path-gated deploy/release workflows
(`deploy-drift`, `deploy-gate-image`, `release-lockfile`) deliberately do
**not** trigger on `merge_group` — they are scoped quality gates that only
fire when their own paths change, and they are **not** intended to be
required checks for a general merge. If any of them is currently marked
required in branch protection, either (a) remove it from required checks,
or (b) add a `merge_group:` trigger + a path-independent "no-op success"
job so the queue always gets a result. Recommended: (a).

## Verifying after enabling

- Open a trivial PR, click "Merge when ready". Confirm a
  `gh-readonly-queue/main/...` ref appears and the full CI suite runs
  against it (every lane, not the narrowed subset).
- Confirm the PR merges only after the merge-group run is green.
