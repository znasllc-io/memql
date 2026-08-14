#!/usr/bin/env bash
#
# scripts/ci/ruleset-drift.sh
# ===========================
#
# Assert that `main`'s protection ruleset still carries the rules this
# repository expects, and say so out loud when it does not (memql#3836).
#
# WHY THIS EXISTS. On 2026-08-06T19:29:07Z one edit to ruleset 16630577 dropped
# TWO rules at once. `docs/ci-audit.md` §2.3 caught the moment and recorded the
# rule list. Exactly one of the two came back:
#
#   dropped                  symptom                                outcome
#   required_status_checks   merges stop being gated -- immediate   restored same day
#   merge_queue              merges get SLOWER -- no red, no alert  gone 8 days
#
# (§2.3's recorded output also shows `copilot_code_review`, which was NOT part
# of this edit: it is the only rule in ruleset 19450314, and that endpoint --
# /rules/branches/main -- returns the UNION across active rulesets while naming
# no ruleset per rule. This script therefore reads PER-RULESET. An aggregate
# cannot tell "this rule was deleted" from "the ruleset contributing it was
# disabled", and treating one as the other is a false alarm nobody can act on,
# which is how a scheduled check gets muted.)
#
# The loss with a visible symptom was repaired within the day. The one whose only
# symptom was latency went unnoticed for over a week, while
# `docs/internal/ops/merge-queue.md` went on saying "Status -- enabled and
# verified" -- not through neglect, but because nobody CHOSE to turn the queue
# off, so nobody thought to falsify the claim. It was re-enabled on 2026-08-14
# once someone finally asked the API instead of the document.
#
# The mechanism is that `PATCH` on a ruleset REPLACES the rules array: an edit
# that does not re-send every existing rule silently deletes the ones it omits.
# So this is not a one-off mistake to be more careful about next time. It is the
# API's default behaviour, and the only reliable defence is to notice
# afterwards.
#
# WHAT IT ASSERTS, AND THE DECISION THAT MATTERS. The baseline below records the
# rules expected on the ruleset. One of them is recorded as KNOWN-ABSENT rather
# than as either present or forgotten:
#
#   - asserting only the four rules that exist today would go GREEN over the
#     very gap this check was created for;
#   - asserting all five as required would be permanently RED until someone with
#     ruleset write turns the queue back on, and a scheduled check that is always
#     red is a check people mute -- which is how the gap survives a second time.
#
# So an absent-but-expected rule is a WARNING naming it and its issue, on every
# run, while any OTHER drift is a hard failure. The gap is visible in the output
# rather than inferred from its absence, and re-enabling a rule is a one-line
# baseline edit. That is the same shape as envscan's ownedPreConvention list
# (memql#3831): an exemption declared in the artifact beats an exemption
# achieved by shrinking what the check looks at.
#
# IT REPORTS ITS OWN COVERAGE for the same reason. A bare "no drift" reads as a
# clean bill of health over a ruleset missing the queue; naming how many rules
# were asserted and how many are excused is a statement a reader can act on.
#
# Those numbers are COMPUTED FROM THE LISTS BELOW at run time, never written
# into this comment. A count spelled out in prose has no mechanism that can
# catch it going stale -- which is not hypothetical: correcting "three rules" to
# "two" elsewhere in this header left five other places saying three, six and
# two, and only re-reading found them.
#
# PERMISSIONS: READ only. `gh api repos/{owner}/{repo}/rulesets/{id}` needs no
# more than the default token, which is deliberate -- the check that notices a
# ruleset regression must not itself require the permission to cause one.

set -euo pipefail

REPO="${RULESET_DRIFT_REPO:-znasllc-io/memql}"
RULESET_ID="${RULESET_DRIFT_ID:-16630577}"

# EXPECTED is every rule that must be PRESENT right now. It and KNOWN_ABSENT
# below are DISJOINT: a rule intended but currently missing belongs in exactly
# one of them, the second. Listing a rule in both makes it fail and warn at once,
# which is neither.
#
# Keep it sorted; the comparison is set-based but a sorted list keeps diffs
# readable and stops two people adding the same rule twice.
EXPECTED=(
    deletion
    merge_queue
    non_fast_forward
    pull_request
    required_status_checks
)
# Overridable so the tests can drive the warn path with a synthetic baseline,
# and so this can be pointed at another ruleset (memql#3837) without a second
# copy of the script. Unset in CI, which is the case that matters.
[[ -n "${RULESET_DRIFT_EXPECTED:-}" ]] && IFS=$'\n' read -r -d '' -a EXPECTED < <(printf '%s\0' "${RULESET_DRIFT_EXPECTED}")

# KNOWN_ABSENT is expected-but-missing, each with the issue that will restore
# it. An entry here downgrades its absence from a failure to a named warning.
#
# REMOVING an entry from here is how a rule comes back: put it in EXPECTED, drop
# it here, and the check starts enforcing it. Leaving a stale entry means a rule
# that IS present gets reported as a known gap, which the "unexpectedly present"
# arm below catches.
# EMPTY, and that is the point of the arm that emptied it. merge_queue was
# recorded here from 2026-08-06 until the repository owner re-enabled it on
# 2026-08-14 -- at which moment this script FAILED, naming the rule and saying
# to move it to EXPECTED. A baseline that keeps excusing a gap after the gap
# closes is a baseline quietly becoming fiction, and it would never have started
# ENFORCING the rule, so a second removal would have been excused too.
KNOWN_ABSENT=()
# NEWLINE-separated, not whitespace: an entry carries a free-text reason, and
# splitting those on spaces turns one entry into five nonsense rules.
[[ -n "${RULESET_DRIFT_KNOWN_ABSENT:-}" ]] && IFS=$'\n' read -r -d '' -a KNOWN_ABSENT < <(printf '%s\0' "${RULESET_DRIFT_KNOWN_ABSENT}")

function have() {
    local needle="$1"
    shift
    local x
    for x in "$@"; do
        [[ "$x" == "$needle" ]] && return 0
    done
    return 1
}

function main() {
    local actual_raw
    if ! actual_raw="$(gh api "repos/${REPO}/rulesets/${RULESET_ID}" --jq '[.rules[].type] | sort | join(" ")' 2>&1)"; then
        echo "FAIL: could not read ruleset ${RULESET_ID} on ${REPO}: ${actual_raw}" >&2
        echo "  This check needs only READ on rulesets. A failure here is a token or" >&2
        echo "  network problem, NOT a drift result -- it must not be read as 'no drift'." >&2
        exit 2
    fi
    # shellcheck disable=SC2206 # deliberate word-splitting of a space-joined list
    local actual=(${actual_raw})

    local rc=0
    local missing=() unexpected=() warned=0

    local want
    for want in "${EXPECTED[@]}"; do
        if ! have "$want" "${actual[@]}"; then
            missing+=("$want")
            rc=1
        fi
    done

    local entry name why
    for entry in "${KNOWN_ABSENT[@]}"; do
        name="${entry%%|*}"
        why="${entry#*|}"
        if have "$name" "${actual[@]}"; then
            # A known-absent rule that is now PRESENT is drift too: the baseline
            # is describing a repository that no longer exists, and left alone
            # it would keep excusing an absence that has been fixed.
            unexpected+=("$name")
            rc=1
            continue
        fi
        echo "WARN: rule '${name}' is expected but ABSENT -- ${why}" >&2
        warned=$((warned + 1))
    done

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo "FAIL: ruleset ${RULESET_ID} is MISSING expected rule(s): ${missing[*]}" >&2
        echo "  A ruleset PATCH replaces the whole rules array, so an edit that did not" >&2
        echo "  re-send every rule deletes the ones it omitted. That is how two rules" >&2
        echo "  went at once on 2026-08-06 and only one came back (memql#3836)." >&2
        echo "  Restore by re-sending ALL rules, not just the one you meant to change." >&2
    fi
    if [[ ${#unexpected[@]} -gt 0 ]]; then
        echo "FAIL: rule(s) recorded as KNOWN_ABSENT are now present: ${unexpected[*]}" >&2
        echo "  Good news, and the baseline is now wrong. Move each to EXPECTED and drop" >&2
        echo "  it from KNOWN_ABSENT so this check starts enforcing it." >&2
    fi

    # The coverage line, always, pass or fail. "no drift" over a ruleset missing
    # the merge queue is exactly the reading this check exists to prevent.
    echo "ruleset ${RULESET_ID}: ${#EXPECTED[@]} rule(s) asserted, ${warned} known-absent, actual: ${actual_raw}" >&2
    if [[ "$rc" == "0" ]]; then
        echo "ruleset-drift: OK -- no drift from the recorded baseline" >&2
    fi
    exit "$rc"
}

main "$@"
