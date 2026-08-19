---
title: CI Tier 3 — GitHub merge queue
audience: ops
status: stable
area: ops
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

## Status — enabled; verify it rather than trust this line

**Do not trust this heading. Run the command.** That is the entire lesson of
this section, which has now been wrong twice:

```
$ gh api repos/znasllc-io/memql/rulesets/16630577 \
    --jq '.name + " (" + .enforcement + ") rules: " + ([.rules[].type]|sort|join(", "))'
default (active) rules: deletion, merge_queue, non_fast_forward, pull_request, required_status_checks
```

Five rules including `merge_queue` — measured 2026-08-14, immediately after the
repository owner re-enabled it. If that output ever lacks `merge_queue`, this
heading is stale again and `scripts/ci/ruleset-drift.sh` will already be
failing, which is the point of memql#3836.

**It was off for eight days before anyone asked.** From 2026-08-06 to
2026-08-14 this section read *"Status — enabled and verified ... The merge queue
is **live** ... This doc itself landed through the queue"* while the rule was
absent from the ruleset. Nobody had turned it off deliberately, so nobody
thought to falsify the claim.

### How it went missing

memql#1539 (`ci: unwedge the merge queue`) is a commit about operating a
queue that existed, so the rule was configured, removed, and has now been
restored. The claim that went stale was never aspirational — it described
something real that quietly stopped being real.

### When it went, and why only half of it came back

[`docs/ci-audit.md` §2.3](ci-audit.md) caught the moment without
recognising it:

> ### 2.3 REQUIRED STATUS CHECKS — removed 2026-08-06T19:29:07Z
> ```
> $ gh api repos/znasllc-io/memql/rules/branches/main
> [copilot_code_review, deletion, non_fast_forward, pull_request]
> ```

At that instant the ruleset had **neither** `required_status_checks`
**nor** `merge_queue`. So one edit dropped both — and only
`required_status_checks` came back on its own.

> **Read that output carefully — it is not what it looks like.**
> `gh api repos/.../rules/branches/main` returns the **union across every
> active ruleset** and names no ruleset per rule. So `copilot_code_review`
> in that list did **not** come from `16630577`: it is the only rule in
> ruleset `19450314` ("Code Quality Copilot review for default branch"),
> which is `enforcement: disabled` today. Its absence is that ruleset
> being switched off — a separate event, not part of this edit.
>
> Two rules were dropped from `16630577`, not three. Anything asserting
> what one ruleset contains must ask **per-ruleset**:
> `gh api repos/znasllc-io/memql/rulesets/16630577`. Reading a per-branch
> aggregate as evidence about one ruleset cannot distinguish "this rule was
> deleted" from "the ruleset that contributed it was disabled" — and both
> of those are real things that happened here.

The mechanism is almost certainly that **`PATCH` on a ruleset REPLACES
the rules array** — an edit that does not re-send every existing rule
silently deletes the ones it omits. Anyone scripting a ruleset change
must read the current rules and re-send all of them plus the new one.

**And then the asymmetry did the rest:**

| dropped | symptom | outcome |
|---|---|---|
| `required_status_checks` | merges stop being gated — visible, alarming, immediate | restored |
| `merge_queue` | merges get **slower** — no failure, no red, no alert | gone 8 days; restored 2026-08-14 only after someone asked the API |

The loss with a visible symptom was repaired the same day. The loss whose
only symptom was latency sat for eight days — and because nobody *chose*
to turn the queue off, nobody updated this file. The status block above
was not left stale through negligence; it was never falsified by anyone,
because the change that falsified it was an accident with no alarm
attached. It took a session hitting the latency, disbelieving the
document, and querying the ruleset directly.

That is the argument for detection rather than for a better document
(memql#3836): a scheduled read of the ruleset asserting the expected rule
set would have caught **both** drops on 2026-08-06, not just the one that
hurt immediately, and would have said so daily for the eight days nobody
noticed. It needs only READ on rulesets, and now exists:
`scripts/ci/ruleset-drift.sh`.

The intended state it asserts — for this ruleset and for the Copilot-review
one, whose `enforcement: disabled` noted above turned out to be a second
invisible loss of exactly this shape (memql#3837) — is recorded in
[ruleset-baseline.md](ruleset-baseline.md).

### What its absence costs, which is why it is worth noticing

Without a queue, and with `strict_required_status_checks_policy` on the
required check, **N open PRs cost N full CI cycles**. Merging any one of
them moves `main`, which makes every other branch `BEHIND`, which
requires an update, which restarts that branch's `db-tests` — about 11
minutes. Nine open PRs is roughly 100 minutes of strictly serialised
waiting, and no amount of client-side cleverness removes it:

- **auto-merge does not help.** It waits for currency; it does not create
  it. A `BEHIND` PR with auto-merge armed sits forever.
- **stacking does not help.** After the parent merges, the stacked branch
  still lacks `main`'s new tip.
- **batching does not help unaided.** The cost is per-merge, not
  per-window, so four PRs in one "window" is still four cycles.

A queue is what makes the cost per-BATCH instead of per-PR. That is a
mechanical property nothing else substitutes for, which is why four
sessions independently invented scheduling ceremony around its absence
before anyone checked whether it was on.

### The intended configuration, for whoever turns it back on

Recorded as the *previous* configuration rather than the current one:

| Setting | Value |
|---|---|
| `merge_method` | `MERGE` (matches the ruleset's allowed methods; squash/auto are blocked org-wide) |
| `grouping_strategy` | `ALLGREEN` — a batch merges only if every entry passes (no optimistic merging) |
| `check_response_timeout_minutes` | 60 |
| `min_entries_to_merge` / wait | 1 / ~5 min batching window |
| `max_entries_to_build` / `merge` | 5 / 5 |

Ruleset `16630577` requires exactly one status check: `ci-required`
(measured 2026-08-06, still true 2026-08-14). It triggers on
`merge_group`, so a queued candidate always gets a result and never
stalls.

### The repository already knew, in a document nobody reads for this

[`docs/ci-audit.md`](ci-audit.md)'s finding **W6** says it plainly:

> **W6. Dead `merge_group` triggers in four workflows.**
> No merge queue is configured; four workflows still declare `merge_group:`.
> Harmless today, misleading when reading the pipeline.

So the tree contained both "the queue is live" and "no merge queue is
configured" simultaneously, and the correct one sat under a heading about
dead workflow triggers while the wrong one sat under a heading called
"Status — enabled and verified".

That is worth more than either document. **A reader checking whether the
queue is on goes to the file named `merge-queue.md`**, finds a confident
answer with a configuration table, and stops. The audit's finding is
correct, older, and filed as a cosmetic cleanup — its own summary calls
the consequence "harmless today", which it was for the pipeline it was
describing and is not for anyone trying to merge nine PRs.

W6 is also now half-actionable in the other direction: if the queue is
turned back on, those four `merge_group:` triggers stop being dead and
become required.

### Why this document went stale twice

It has now carried two corrections, and the shape is the same both times:
a **measured** claim, stated in the register that earns trust, falsified
by a change made somewhere this file cannot see. The previous one was
about which checks are required; this one is about whether the queue
exists at all.

The paragraph this replaces read *"The merge queue is **live** ... The
queue has merged PRs end-to-end ... This doc itself landed through the
queue."* Every clause was true when written, and the last one is the
dangerous kind — a **citation of evidence**, which is exactly what a
reader does not re-derive, because citing evidence is what a trustworthy
claim looks like.

So this section now leads with the command that establishes its own
claim. A reader who runs it learns the answer in five seconds; a reader
who does not is no worse off than they were trusting the old paragraph.
That is the general rule this repository keeps arriving at: **state a
measured property together with the measurement, or do not state it.**

This paragraph previously read "Required status checks are
`ci-required`, `scan`, and `Analyze (go)`", which was stale twice over
(corrected in memql#3210). The required set was reduced to
`ci-required` alone — see [ci-design.md](ci-design.md) for
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
