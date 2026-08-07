---
title: CI Tier 3 — GitHub merge queue
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

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

## Status — enabled and verified

The merge queue is **live** on the `default` ruleset (id 16630577). The
`merge_queue` rule is configured as:

| Setting | Value |
|---|---|
| `merge_method` | `MERGE` (matches the ruleset's allowed methods; squash/auto are blocked org-wide) |
| `grouping_strategy` | `ALLGREEN` — a batch merges only if every entry passes (no optimistic merging) |
| `check_response_timeout_minutes` | 60 |
| `min_entries_to_merge` / wait | 1 / ~5 min batching window |
| `max_entries_to_build` / `merge` | 5 / 5 |

Ruleset `16630577` requires exactly one status check: `ci-required`
(measured 2026-08-06). It triggers on `merge_group`, so a queued
candidate always gets a result and never stalls. The queue has merged
PRs end-to-end (each spawns a `gh-readonly-queue/main/...` candidate and
merges only when green). This doc itself landed through the queue.

This paragraph previously read "Required status checks are
`ci-required`, `scan`, and `Analyze (go)`", which was stale twice over
(corrected in memql#3210). The required set was reduced to
`ci-required` alone — see [../../ci-design.md](../../ci-design.md) for
why one aggregator beats three independent names. And the secret-scan
check is no longer called `scan`: `gitleaks.yml` and `govulncheck.yml`
both published a check-run under that one name, so they were renamed to
`gitleaks` and `govulncheck` respectively.
`scripts/dev/workflow_check_name_uniqueness_test.go` fails the build if
any two jobs across `.github/workflows/` ever share a name again. If a
scan lane is made required in future, name it by its check-run name from
that guard's inventory, not by its workflow or job key.

**`Analyze (go)` is a deliberate no-op on `merge_group`.** It reports the
required context green without analysing the candidate: a SARIF upload
keyed to the torn-down `gh-readonly-queue` ref wedges the queue (#1539),
so `.github/workflows/codeql.yml` gates checkout, init, autobuild and
analyze on `github.event_name != 'merge_group'` and runs a single `echo`
instead. CodeQL coverage comes from the `pull_request`, `push: main` and
weekly `schedule` triggers instead, and
`scripts/dev/codeql_merge_group_coverage_test.go` fails the build if any
of those is removed while the no-op remains.

This paragraph previously said the queue "runs the full suite on it",
which was read as covering all three contexts and is not true of
`Analyze (go)` (corrected in memql#2973). The guard's own header cited
this page as the authority for the no-op, which pointed a reader at a
page that contradicted it.
