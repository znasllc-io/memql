# Archived issue backlog -> epic transfer map

`ISSUE-BACKLOG-TRANSFER.md` held the engineering substance of the 39 issues open in
`memql` when the project moved off GitHub-issue tracking. Those issues were deleted from
GitHub; that file was the surviving record, and it was deleted on 2026-08-06 once every
entry had been transferred.

This is the index. The left column is an **old identifier** -- a name, not a link. The
right column is where that work lives now.

Cross-references in code comments, commit messages and issue bodies still use the old
numbers (`Refs #2989`, `split from #2960`, `the same class as #2814`). Use this table to
resolve them.

## rowauthz-enforcement -- epic #3171

| old | now | note |
|---|---|---|
| #3076 | #3172 | Original framing superseded: the tier is resolved from the declared binding, not caller filter text |
| #3077 | #3173 | Merged with #3135 |
| #3135 | #3173 | The map it re-types does not exist yet; #3077 never landed |
| #3079 | #3174 | |
| #3059 | #3175 | Option 1 (generic server-stamp) chosen; guard table is 9 concepts, not 3 |
| #3067 | #3176 | Deletion (option 2); the reviewed attempt never merged |
| #3129 | #3177 | Merged with #3138 |
| #3138 | #3177 | Same two mutations, same concept, one change |
| #3063 | #3178 | Narrowed: `workerTokensForUser` landed in PR #3072; two queries remain |
| #2802 | #3179 | Parked with one surviving blocker; the #2814 coupling is dissolved |

## secret-redaction -- epic #3180

| old | now | note |
|---|---|---|
| #3113 | #3181 | Decision already made -- annotating changes zero diagnostics |
| #3117 | #3182 | The worst of the four surfaces |
| #3111 | #3183 | |
| #3112 | #3184 | |
| #3143 | #3185 | Delete the clause; do not narrow a third time |
| #3108 | #3186 | |
| #3145 | #3187 | |
| #3052 | #3188 | **Reframed.** Its title names the wrong cause; the buildable decision is the ~492-alert family |

## scanner-correctness -- epic #3189

| old | now | note |
|---|---|---|
| #3120 | #3190 | Nine sites, not eleven |
| #3116 | #3191 | Decision made: match the lexer |
| #3099 | #3192 | |
| #3101 | #3193 | |
| #3105 | #3194 | |

## concept-schema-fidelity -- epic #3195

| old | now | note |
|---|---|---|
| #3038 | #3196 | Owner ruling carried; nested-vs-top-level decision still open |
| #3123 | #3197 | |
| #3124 | #3198 | |

## dsl-loader-integrity -- epic #3199

| old | now | note |
|---|---|---|
| #3084 | #3200 | Prior attempt gated the wrong site; refusal semantics still to decide |
| #3082 | #3201 | Option 3 chosen, plus a coverage gate; 19 live calls, not 14 |
| #3089 | #3202 | PR #3134 parked with two blocking defects |
| #3093 | #3203 | |

## auth-surface-hardening -- epic #3204

| old | now | note |
|---|---|---|
| #2876 | #3205 | PR #2891 is a draft and must not merge -- it is a net security regression |
| #3128 | #3206 | |
| #3114 | #3207 | |
| #3131 | #3209 | |

## db-test-lane-integrity -- epic #3167

| old | now | note |
|---|---|---|
| #3096 | #3168 | Merged with #3148 |
| #3148 | #3168 | The fix it refines never landed, so both paths are one change |
| #3095 | #3169 | Collides with #3160 on `scripts/cidb/dbgate_test.go` |
| #3149 | #3170 | |

## ci-pipeline-redesign -- epic #3156

| old | now | note |
|---|---|---|
| #3075 | **dropped**, residue #3210 | See below |

## Deliberately dropped

**#3075 -- "Two workflows both publish a required check named `scan`, so the required set
does not say what it means."**

Dropped because its premise no longer holds. The issue was filed when branch protection
required three checks by name -- `ci-required`, `scan`, `Analyze (go)` -- and its entire
argument rests on that: *"branch protection matches a required check by name, so it
cannot distinguish them"*, and its recommended remedy was to rename both jobs **and**
edit the ruleset in one operation, timed for when the review queue was drained.

Measured 2026-08-06:

```
$ gh api repos/znasllc-io/memql/rulesets/16630577 --jq '...required_status_checks[].context'
ci-required
```

`scan` is not a required context. So the gating hazard does not exist, its DoD item 4
("whether govulncheck gates PRs is a stated decision") is answered by the ruleset's
explicit content, and its cutover analysis -- both rename directions being unsafe -- was
entirely a consequence of `scan` being required.

What survived is the name collision itself (both `gitleaks.yml` and `govulncheck.yml`
still declare `name: scan`), which is cosmetic today and cheap to close. It is filed as
**#3210** under `ci-pipeline-redesign`, the epic that owns `.github/workflows`.

## Totals

39 entries -> 36 tasks across 7 epics, plus 1 deliberate drop.

| epic | issue | entries | tasks |
|---|---|---|---|
| rowauthz-enforcement | #3171 | 10 | 8 |
| secret-redaction | #3180 | 8 | 8 |
| scanner-correctness | #3189 | 5 | 5 |
| concept-schema-fidelity | #3195 | 3 | 3 |
| dsl-loader-integrity | #3199 | 4 | 4 |
| auth-surface-hardening | #3204 | 4 | 4 |
| db-test-lane-integrity | #3167 | 4 | 3 |
| ci-pipeline-redesign (existing) | #3156 | 1 | 1 (residue) |
| **total** | | **39** | **36** |
