---
title: Ruleset baseline — what `main`'s protection should be, and why
audience: ops
status: stable
area: ops
sinceVersion: 0.18.0
owner: znas
---

# Ruleset baseline — what `main`'s protection should be, and why

**Status:** living record. Edit it when a ruleset's intended state changes.
**Asserted by:** `scripts/ci/ruleset-drift.sh`, run once per ruleset by
the native pipeline’s `repository-audit` stage (daily and on manual runs).
**Origin:** memql#3836 (rule membership), memql#3837 (enforcement).

This file is the *intended* configuration. The native audit notices when
the repository stops matching it. Neither is useful alone: a document nobody
falsifies drifts into fiction, and a check with no recorded intent can only
assert that today looks like yesterday.

---

## Why a recorded baseline at all

`PATCH` on a ruleset **replaces the whole rules array**. An edit that does not
re-send every existing rule silently deletes the ones it omits. That is the
API's default behaviour, not a mistake to be more careful about next time, so
the only reliable defence is to notice afterwards.

It has already cost this repository twice, and the two losses are worth
contrasting because they are the same event with different alarms attached:

| what went | symptom | how long |
|---|---|---|
| `required_status_checks` | merges stop being gated — visible, immediate | restored same day |
| `merge_queue` | merges get *slower* — no failure, no red | 8 days |
| ruleset 19450314 switched off | a review stops being requested — nothing at all | unnoticed until someone read the API for an unrelated reason |

Every one of these was invisible in proportion to how quietly it failed. The
baseline exists so that "quietly" stops being a property the repository has.

---

## Ruleset 16630577 — `default`

**Enforcement:** `active`
**Rules:** `deletion`, `non_fast_forward`, `pull_request`,
`required_status_checks`. `merge_queue` was removed when release trains were
switched on, 2026-10-29 ([below](#release-trains-replace-the-merge-queue-2026-10-27))
**Asserted by:** the `ruleset-drift` job.

This is the ruleset that makes `main` refuse a direct push and routes every
change through a PR (CLAUDE.md, "Branch Workflow"). The four asserted rules are
required to be present; there are no recorded gaps. `merge_queue` was removed
at the switch and is deliberately not asserted.

The full narrative of the 2026-08-06 double drop and the 2026-08-14 restore is
in [merge-queue.md](merge-queue.md); the point-in-time evidence is in
[`ci-audit.md`](ci-audit.md) §2.2–§2.3. Note that `ci-audit.md` is a
**dated audit**, not a live status page — it correctly records 2026-08-06 and
should not be edited to match today.

### `strict_required_status_checks_policy`: off

**Decided 2026-09-27 (memql#5481, epic memql#5476; design record
`docs/superpowers/specs/2026-09-16-pipelines-program-design.md`, section 1).**
The original change kept `ci-required` and turned off only the "branch must
be up to date" parameter. The native cutover below replaces that context with
the App-bound native check. Read the live value rather than this line:

```
$ gh api repos/znasllc-io/memql/rulesets/16630577 \
    --jq '.rules[] | select(.type=="required_status_checks") | .parameters.strict_required_status_checks_policy'
false
```

**Why it can be off.** The merge queue builds every candidate on
`gh-readonly-queue/*` -- the base plus the pull request, exactly the tree that
lands -- and runs the native full-mode suite on it before anything merges
(the pipeline's `merge_group` event). The strict rule enforced the same property a second
time, from the pull request's side, and charged for it on every sibling
merge: each merge to `main` flipped every open pull request to `BEHIND`, which
meant `update-branch` and a fresh full run -- the "two or three runs per
change" the design record measures in its section 1. That is the cost
memql#5476 removes.

**What still enforces up-to-dateness, and must keep doing so.** The owner
bypass (`scripts/dev/merge-as-owner.sh`, CLAUDE.md "Branch Workflow" item 2)
merges immediately rather than through the queue, so on that path nothing
else tests the combination. The script therefore MEASURES the drift itself
(`compare/<base>...<head>`) and refuses a branch that is behind, or whose
drift it cannot read. That refusal never depended on this parameter --
`BLOCKED` hides `BEHIND` on every pull request here anyway -- and it is what
keeps the bypass from landing an untested tree with the parameter off.

**What the drift check asserts, deliberately.** `scripts/ci/ruleset-drift.sh`
reads `.rules[].type` only, so this parameter is outside it in either state;
confirmed when this was recorded. That is the right scope: the check exists to
catch a write that silently DROPS rules (the 2026-08-06 failure shape), and a
rule's parameters are decisions, recorded here. If they ever need asserting,
that is a new axis with its own baseline, as enforcement is.

**To reverse it:** set the parameter back with a PUT that re-sends EVERY rule
(see "Why a recorded baseline at all" -- a write that omits a rule deletes it),
confirm `ruleset-drift.sh` still reports five rules asserted, and edit this
section.

### Native required check: October 18, 2026

The owner authorized migration to native MemQL CI/CD in
[memql#5480](https://github.com/znasllc-io/memql/issues/5480). The active ruleset
initially required both `ci-required` and **`MemQL / memql`**. After the
October 19 full merge-candidate qualification, it requires only **`MemQL / memql`**,
bound to GitHub App **4822538**, **MemQL for ZNAS LLC**. A status with the same
name from another integration cannot satisfy it.

Native run `67491bba23d2ec5113b7a7264d3aad4e`
passed all **48 steps**, with 48 retained logs and no skips, on candidate
`46cfeade6fea0c874b45952b6b6539f7a8f6532b`. Storage
[PR #6171](https://github.com/znasllc-io/memql/pull/6171) then landed through
the ordinary merge queue as `46cfeade6fea0c874b45952b6b6539f7a8f6532b`. The qualified and landed Git tree
identities match. Check `118127603094` was independently verified as a
successful result from App 4822538 for that exact native attempt. The ruleset
was compared before the write and read back afterward; only the legacy
required context was removed.

Native PR [#6176](https://github.com/znasllc-io/memql/pull/6176) passed on
`fa2ce0b8e5277969838d9d026f537a44742f6f2e`, check
[`117335797383`](https://github.com/znasllc-io/memql/runs/117335797383),
with 35 passed steps and four expected path skips. The complete native
v0.29.31 release on `1d087ac973e5fd1e620f99f93ef42dba1f3a3642` subsequently
passed all 57 steps without skips, retained 57 logs and 136 artifacts, and
deployed through instance
[PR #226](https://github.com/znasllc-io/memql-znas/pull/226).

The queue builds **one candidate at a time** (`max_entries_to_build: 1`) and
allows **180 minutes** for its required checks. This bounds simultaneous
candidate demand on the existing single-node build pool; it does not add
compute or change any individual test deadline. The former 60-minute queue
deadline did not allow for full native validation plus waiting behind other
bounded jobs. `ALLGREEN`, the five-minute batch window, merge batch size,
non-fast-forward/deletion protection, code-owner review, admin bypass actors
and the disabled strict up-to-date parameter are preserved. The entire
ruleset was re-read and compared after the update.

The daily drift job above checks rule membership and enforcement. It does
not claim to validate these parameters. Inspect the current bindings and
queue values directly:

```bash
gh api repos/znasllc-io/memql/rulesets/16630577 \
  --jq '.rules[] | select(.type=="required_status_checks" or .type=="merge_queue")'
```

### Release trains replace the merge queue (2026-10-27)

**Decided by the owner on 2026-10-27** (design record
`docs/superpowers/specs/2026-10-27-release-trains-design.md`, sections 8 and
12). Every pull request here is the owner's own, so none can be approved and
each merged through the bypass, which skips the queue: the queue batched
nothing. A release train now batches instead, and the owner's approval of the
App-authored train pull request is the review that lands it
([pipelines.md](../../public/operate/pipelines.md#release-trains)).

**The fast phase (2026-10-28, owner-approved, ended the same day).** The
required context was **`MemQL / memql / push`** (App 4822538) instead of
`MemQL / memql`, so a release could be cut from `main`'s own push run while pull
requests merged at once through the bypass. It returned to `MemQL / memql` at
14:04 UTC on 2026-10-28, which is step 3's context.
Separately, memql-znas's ruleset gained the same admin bypass
(`RepositoryRole` 5, `pull_request`) so its pin pull requests can merge. Both
rulesets were saved before those writes.

**The switch** (spec section 12, step 1), in this order:

1. `ruleset-drift.sh` stops asserting `merge_queue` (done in the trains
   change), so removing the rule does not turn the daily audit red.
2. CODEOWNERS gives every path to the owner alone, so only the owner's review
   counts.
3. The ruleset is re-read, then written once with every rule re-sent: the
   required context back to **`MemQL / memql`** (App 4822538), which the train
   pull request's run reports, and `merge_queue` removed. `pull_request` keeps
   `require_code_owner_review: true` and the admin bypass stays, for
   `merge-as-owner.sh --emergency` only. Read the result back.
4. `MEMQL_PIPELINES_TRAINS` turns trains on for `znasllc-io/memql`.

**Fallback** (step 2): turn the train setting off, restore the saved ruleset
(re-send every rule, `merge_queue` included, and add `merge_queue` back to
`EXPECTED` in `ruleset-drift.sh`), and merge with
`merge-as-owner.sh --emergency` until the train is fixed.

**Switched on 2026-10-29, with step 2 still to come.**

- Step 1 shipped with the trains change (memql#6339).
- Step 3: the required context was already `MemQL / memql` (above). The owner
  removed `merge_queue` in GitHub's ruleset editor at 05:20 UTC, because an
  agent session's API write was refused by its auto-mode safety check as a
  CI bypass. The read-back showed every other rule and the admin bypass
  unchanged, and `ruleset-drift.sh` reports 4 rules asserted, no drift.
- Step 4: `MEMQL_PIPELINES_TRAINS` was set at 05:21 UTC: `znasllc-io/memql`,
  departures 09:00 and 15:00 `America/Los_Angeles`, channel `znas-instance`,
  `maxRebuilds` 2, no `ownerDiscordUserId` yet. The `znas-instance` channel
  was allowed on the `memql` pipeline (`pipelinesChannelAllow`), because a
  train refuses to post to a channel its pipeline does not accept.
- Step 2 shipped with the consolidated 0.31.8 release: the CODEOWNERS
  catch-all moved from `human-developers` to `human-lead-developers`, today the
  owner alone (memql#6371), so only the owner's review counts on every path.
  The owner approves an App-authored train pull request, which the owner did
  not author; an owner-authored pull request merges through the bypass.

---

## Ruleset 19450314 — `Code Quality Copilot review for default branch`

**Enforcement:** `disabled` — deliberately. See the decision below.
**Rules:** `copilot_code_review`
**Asserted by:** the `ruleset-enforcement-copilot` job.

### The decision

**Recorded 2026-08-15 by the repository owner: the disabled state is
intentional, and is the state this repository asserts.**

Being *asserted* rather than merely tolerated is the whole content of the
decision. The check now fails if the ruleset is re-enabled — which is good news
arriving as a red job on purpose, because it means the recorded decision no
longer describes the repository, and re-enabling should be a new decision
somebody makes rather than a state the repository drifts into.

To reverse it: flip the ruleset to `active` in GitHub settings, change
`RULESET_DRIFT_ENFORCEMENT` to `active` in the workflow job, and edit this
section. All three, together — the job is what stops any one of them from being
forgotten.

### When it happened, and what is not recoverable

The API pins the *when* precisely, which nothing in the tree had recorded:

```
$ gh api repos/znasllc-io/memql/rulesets/19450314 \
    --jq '{created_at, updated_at, enforcement, rules: [.rules[].type]}'
{
  "created_at": "2026-07-21T10:53:27.362-07:00",
  "updated_at": "2026-08-06T15:34:01.047-07:00",
  "enforcement": "disabled",
  "rules": ["copilot_code_review"]
}
```

The ruleset has been written exactly twice: created 2026-07-21, and modified
once on **2026-08-06T15:34:01-07:00**. Its single rule is unchanged between
those two points, so that second write is the enforcement flip.

Two independent observations bracket it to the same afternoon. `ci-audit.md`
§2.2 records **both rulesets active** on 2026-08-06, and §2.3's
`gh api repos/.../rules/branches/main` output lists `copilot_code_review` — and
that endpoint returns the union across **active** rulesets only, so a disabled
19450314 could not have contributed it. The audit therefore ran while the
ruleset was still on, and the flip falls between that reading and 15:34:01.

Which places it about three hours after the 12:29:07-07:00 edit that dropped
`required_status_checks` and `merge_queue` from the `default` ruleset — the same
afternoon, during the same platform incident, though the two are separate
rulesets and separate writes.

**The `why` is not recoverable.** Nothing in the tree, the ruleset, or the audit
records a rationale, and the API exposes no actor or reason for a ruleset write.
This record does not invent one. What it records is that the state was
*reviewed* on 2026-08-15 and *chosen*, which is a different and honest claim.

---

## How the assertions are split

Two axes, asserted separately, one job each:

| axis | what fails | why not one check |
|---|---|---|
| rule **membership** | a rule was dropped from the array | — |
| **enforcement** | the ruleset carries its rules and applies none of them | membership stays perfectly TRUE while the ruleset does nothing |

19450314 is the case that forces the split: `copilot_code_review` is present and
correct, and the review is not happening. A single verdict over both axes would
have been green on the half that was true and silent on the half that was not —
the same defect as reading the per-branch rule aggregate, one level up.

The same split applies to running one script over two rulesets. Both jobs invoke
`scripts/ci/ruleset-drift.sh`, but as **separately named jobs with their own
baselines**, so each prints its own coverage line naming the ruleset it asserted:

```
ruleset 16630577: enforcement active (want active), 4 rule(s) asserted, 0 known-absent, actual: deletion non_fast_forward pull_request required_status_checks
ruleset 19450314: enforcement disabled (want disabled), 1 rule(s) asserted, 0 known-absent, actual: copilot_code_review
```

One job speaking for two rulesets would produce one verdict that a reader could
not attribute. Two checks wearing one name is worse than two checks.

---

## Permissions

Both jobs need **READ** only (`gh api repos/{owner}/{repo}/rulesets/{id}`), which
is deliberate and worth preserving: the check that notices a ruleset regression
must not itself hold the permission to cause one.

A read failure exits `2`, distinct from the `1` that means drift, so "I could not
ask" can never be reported as "nothing has changed".
