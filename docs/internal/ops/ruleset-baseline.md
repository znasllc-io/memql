# Ruleset baseline — what `main`'s protection should be, and why

**Status:** living record. Edit it when a ruleset's intended state changes.
**Asserted by:** `scripts/ci/ruleset-drift.sh`, run once per ruleset by
`.github/workflows/ruleset-drift.yml` (daily 07:00 UTC + on demand).
**Origin:** memql#3836 (rule membership), memql#3837 (enforcement).

This file is the *intended* configuration. The workflow is what notices when
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
**Rules:** `deletion`, `merge_queue`, `non_fast_forward`, `pull_request`,
`required_status_checks`
**Asserted by:** the `ruleset-drift` job.

This is the ruleset that makes `main` refuse a direct push and routes every
change through a PR (CLAUDE.md, "Branch Workflow"). All five rules are required
to be present; there are no recorded gaps.

The full narrative of the 2026-08-06 double drop and the 2026-08-14 restore is
in [merge-queue.md](merge-queue.md); the point-in-time evidence is in
[`docs/ci-audit.md`](../../ci-audit.md) §2.2–§2.3. Note that `ci-audit.md` is a
**dated audit**, not a live status page — it correctly records 2026-08-06 and
should not be edited to match today.

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
ruleset 16630577: enforcement active (want active), 5 rule(s) asserted, 0 known-absent, actual: deletion merge_queue non_fast_forward pull_request required_status_checks
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
