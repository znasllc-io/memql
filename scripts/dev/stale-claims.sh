#!/usr/bin/env bash
#
# scripts/dev/stale-claims.sh
# ===========================
#
# Report `claimed:<session>` labels that no live session is holding, and -- on
# CLOSED issues whose claim has also gone cold, and only with --apply -- remove
# them (memql#2834).
#
# WHY IT MATTERS. The claim label is the concurrency primitive the ship-issue
# loop rests on: SELECT discards any issue carrying `claimed:*`. So a claim held
# by a dead session makes the issue PERMANENTLY INVISIBLE -- not parked, not
# assigned, not in anyone's queue, and with no diagnostic comment a human could
# find. It looks exactly like active work. Two dead sessions left claims behind
# in a single night (#2801 `claimed:s4b9e2`, #2785 `claimed:t8w4n1`), both found
# only because a human noticed unmerged PRs and asked.
#
# DISCOVERY IS BY LABEL, NOT BY SCANNING RECENT ISSUES.
#
# The first cut listed the newest 100 issues and filtered for claim labels. That
# is worse than useless on a repo with ~2900 issues: every claim on an older
# issue is invisible, which is the EXACT failure mode this script exists to
# detect. It was not hypothetical -- during review, two issues were filed
# between two runs, `#2707 claimed:s6f08e5` fell off the page, and the report
# dropped it silently while printing a confident summary line.
#
# So the enumeration starts from the LABEL side: list the repo's `claimed:*`
# labels (a bounded, small set -- 133 labels total here), then ask for every
# issue carrying each one. That is exact, and both calls warn loudly if they hit
# their page cap rather than silently truncating.
#
# CLAIM AGE COMES FROM THE `labeled` TIMELINE EVENT, NOT `updatedAt`.
#
# #2834 names this directly. `updatedAt` is the ISSUE's last activity, not the
# CLAIM's age: any comment, or a cross-reference from an unrelated PR, resets it
# to zero, so a genuinely dead claim on a busy issue reads FRESH forever. The
# timeline's `labeled` event for the specific label is the real answer and costs
# one call per issue.
#
# WHY CLOSED ISSUES ARE SWEPT AND OPEN ONES ARE ONLY REPORTED.
#
# This is the split the issue calls its "cheapest useful first step", and the
# reason is that the two cases have completely different risk:
#
#   - a `claimed:*` label on a CLOSED issue whose claim has gone cold is
#     abandoned. The work is done; SELECT only looks at open issues, so the
#     label affects nothing. Both incidents above were this shape.
#   - a claim on an OPEN issue may be live work. Distinguishing "working on it"
#     from "died two hours ago" is exactly what #2834 lists as unsettled --
#     what threshold, whether wall-clock age or PR-commit activity is the
#     liveness signal, and what to do when the stalled claim's PR is already
#     green. Guessing here reaps a claim mid-review; the review rounds on #2789
#     ran 10-20 minutes each and that PR legitimately took three of them over
#     roughly two hours.
#
# So open-issue claims are REPORTED with their age, and the decision stays with
# a human. That also mirrors the sibling reporter (stalled-prs.sh, #2833), which
# declined to auto-enqueue for a related reason.
#
# THE COLD-CLAIM GATE ON CLOSED ISSUES. "Closed implies abandoned" is NOT true,
# and an earlier draft of this file claimed it was. The normal happy path is
# claim -> work -> merge (which closes the issue) -> drop the claim, and there
# is a real window between those last two steps. Review caught the live case: a
# running session had closed #2869 three minutes before, and its still-held
# claim was classified ABANDONED. Nothing dangerous escapes (SELECT reads only
# open issues), but sweeping it destroys the report's audit value -- you cannot
# answer "which sessions died" if a live session's normal shutdown looks
# identical. So a closed claim must ALSO be older than IDLE_HOURS to be swept.
#
# MUTATION IS OPT-IN. The default run makes no GitHub write of any kind, so it
# is safe to put on a loop or in CI. `--apply` is required to remove anything,
# and even then only cold claims on closed issues. stale_claims_test.go enforces
# this behaviourally: it RUNS this script against a stub `gh` that refuses
# anything outside a DEFAULT-DENY read allowlist, and a companion meta-test
# fires a corpus of write spellings at that stub to prove the allowlist is not
# an enumerating blocklist (the #2873 lesson, which the first cut of this file
# re-broke).
#
# DEFAULT-DENY. A removal is a write, so it happens only when every input is
# positively known: the issue is CLOSED, the claim's own age is known and cold,
# and the label was read back from the API rather than inferred. Anything
# unresolved -- a failed call, an unparseable payload -- is reported as UNKNOWN
# and never swept.
#
# Usage:
#   bash scripts/dev/stale-claims.sh                    # report only, no writes
#   bash scripts/dev/stale-claims.sh --apply            # + sweep cold CLOSED claims
#   IDLE_HOURS=6 bash scripts/dev/stale-claims.sh       # claim age cutoff
#   REPO=owner/name bash scripts/dev/stale-claims.sh
#
# Backs 'make claims-stale'.
#
# Exit codes: 0 report produced | 2 bad param | 4 gh unusable | 5 a sweep failed.
#
# Refs: #2834 #2833 #2873

# Reporter: no -e, so one failing gh call for a single issue does not abort the
# rest of the report.
set -uo pipefail

REPO="${REPO:-znasllc-io/memql}"
# Hours a claim must have sat before it counts as cold. Hours, not minutes, per
# #2834: a slow review gate must not be reaped mid-flight.
IDLE_HOURS="${IDLE_HOURS:-4}"
# Page caps. Not configurable -- raising a cap silently is how it stops being
# noticed. Both sites WARN when a page comes back full, and the warning is
# measured against the RAW page length, not a filtered subset (the first cut
# compared the filtered count and so could never fire).
LABEL_PAGE_LIMIT=300
ISSUE_PAGE_LIMIT=200
TIMELINE_PAGE_LIMIT=100

APPLY=0
SWEEP_FAILED=0

usage() {
  cat <<'USAGE'
stale-claims.sh -- report claimed:* labels no live session is holding.

Default is READ-ONLY: it makes no GitHub mutation. `--apply` removes the label
from CLOSED issues whose claim has also gone cold (older than IDLE_HOURS).
Claims on OPEN issues are always reported, never swept -- distinguishing a live
worker from a dead one is the unsettled half of #2834.

Usage:
  bash scripts/dev/stale-claims.sh
  bash scripts/dev/stale-claims.sh --apply

Environment:
  REPO         owner/name to scan (default: znasllc-io/memql)
  IDLE_HOURS   age before a claim counts as cold (default: 4)

Exit codes: 0 report produced, 2 bad parameter, 4 gh unusable, 5 a sweep failed.
USAGE
}

require_gh() {
  if ! gh --version >/dev/null 2>&1; then
    echo "ERROR: gh CLI not found or not working; this report reads the GitHub API." >&2
    exit 4
  fi
  if ! gh auth status >/dev/null 2>&1; then
    echo "ERROR: gh is not authenticated (run: gh auth login)." >&2
    exit 4
  fi
}

require_valid_threshold() {
  case "$IDLE_HOURS" in
    '' | *[!0-9]*)
      echo "ERROR: IDLE_HOURS must be a non-negative integer, got: $IDLE_HOURS" >&2
      exit 2
      ;;
  esac
}

# claim_labels prints every claimed:* label defined in the repo, one per line.
#
# Enumerating from the LABEL side is what makes the report exact: the claim
# labels are few and bounded, whereas the issues carrying them are spread over
# the repo's entire history.
claim_labels() {
  gh label list -R "$REPO" --limit "$LABEL_PAGE_LIMIT" --json name \
    --jq '.[].name | select(startswith("claimed:"))' </dev/null
}

# label_page_full reports whether the label list came back at its cap, which
# means labels beyond it were never examined.
label_page_full() {
  local n
  n=$(gh label list -R "$REPO" --limit "$LABEL_PAGE_LIMIT" --json name --jq 'length' </dev/null) || return 1
  [ -n "$n" ] && [ "$n" -ge "$LABEL_PAGE_LIMIT" ]
}

# issues_with_label prints one TSV row per issue carrying the given label:
# number, state, title. Both states are fetched -- the sweep needs CLOSED and
# the report needs OPEN.
#
# The label is passed to --label, so matching is exact and a label name
# containing a comma cannot be mis-split (the first cut joined names with
# commas and re-split, which mis-identified `claimed:a,b` as `claimed:a`).
issues_with_label() {
  gh issue list -R "$REPO" --label "$1" --state all --limit "$ISSUE_PAGE_LIMIT" \
    --json number,state,title \
    --jq '.[] | [.number, .state, .title] | @tsv' </dev/null
}

# has_label reports whether an issue carries an exact label. Asked per issue via
# the API rather than parsed out of a joined string.
has_label() {
  local num="$1" want="$2" hit
  # jq's $ENV, not --arg: `gh issue view` takes exactly one positional and
  # rejects `--arg w parked` outright ("accepts 1 arg(s), received 4"). The
  # first cut used --arg, so this lookup ALWAYS failed and every issue read as
  # not-parked. The stub accepted --arg, so the suite never noticed -- a stub
  # more permissive than the real tool tests nothing. It now refuses --arg too.
  hit=$(STALE_CLAIMS_WANT="$want" gh issue view "$num" -R "$REPO" --json labels \
    --jq '[.labels[].name] | index($ENV.STALE_CLAIMS_WANT) // empty' </dev/null 2>/dev/null) || return 1
  [ -n "$hit" ]
}

# claim_age_hours prints whole hours since the label was last applied to the
# issue, from the timeline's `labeled` event, or `unknown`.
#
# The LAST matching event wins: a claim removed and re-applied is as old as the
# re-application, not the original.
claim_age_hours() {
  local num="$1" label="$2" at then now
  # $ENV again -- `gh api` has no --arg either. Interpolating the label into
  # the filter text would also work but needs jq-string escaping; $ENV needs
  # none.
  at=$(STALE_CLAIMS_LABEL="$label" gh api "repos/$REPO/issues/$num/timeline?per_page=$TIMELINE_PAGE_LIMIT" \
    --jq '[.[] | select(.event=="labeled" and .label.name==$ENV.STALE_CLAIMS_LABEL) | .created_at] | last // empty' \
    </dev/null 2>/dev/null) || {
    echo "unknown"
    return
  }
  if [ -z "$at" ]; then
    echo "unknown"
    return
  fi
  then=$(date -u -d "$at" +%s 2>/dev/null) || {
    echo "unknown"
    return
  }
  now=$(date -u +%s)
  if [ "$then" -gt "$now" ]; then
    echo "unknown" # clock skew -- never manufacture a finding from it
    return
  fi
  echo $(((now - then) / 3600))
}

# remove_claim drops one claimed:* label from one issue. The ONLY mutation in
# this file, reached only for a cold claim on a CLOSED issue, and only under
# --apply.
#
# stdin is closed so the call cannot consume the caller's loop input.
remove_claim() {
  local num="$1" label="$2"
  if gh issue edit "$num" -R "$REPO" --remove-label "$label" </dev/null >/dev/null 2>&1; then
    echo "  swept  #$num  $label"
    return 0
  fi
  echo "  FAILED #$num  $label (label removal rejected; left in place)" >&2
  SWEEP_FAILED=1
  return 1
}

row() {
  printf '#%-6s %-9s %-8s %-22s  %s\n' "$1" "$2" "$3" "${4:0:22}" "${5:0:42}"
}

main() {
  require_valid_threshold
  require_gh

  local labels
  labels=$(claim_labels)
  if [ $? -ne 0 ]; then
    echo "ERROR: could not list labels for $REPO; the report would be a false all-clear." >&2
    exit 4
  fi

  if label_page_full; then
    echo "WARNING: hit the ${LABEL_PAGE_LIMIT}-label page cap; labels beyond it were NOT examined." >&2
    echo "         This report is incomplete -- treat a clean result as unproven." >&2
  fi

  printf '%-7s %-9s %-8s %-22s  %s\n' "ISSUE" "STATE" "CLAIMED" "CLAIM" "TITLE"
  printf '%-7s %-9s %-8s %-22s  %s\n' "-------" "---------" "--------" "----------------------" "-----"

  local cold=0 open=0 total=0 unknown=0 swept=0 warm=0
  while read -r label; do
    [ -z "$label" ] && continue

    local rows
    rows=$(issues_with_label "$label")
    if [ $? -ne 0 ]; then
      echo "WARNING: could not list issues for label $label; it is omitted from the report." >&2
      unknown=$((unknown + 1))
      continue
    fi
    if [ "$(grep -c . <<<"$rows")" -ge "$ISSUE_PAGE_LIMIT" ]; then
      echo "WARNING: label $label hit the ${ISSUE_PAGE_LIMIT}-issue page cap; some were NOT examined." >&2
    fi

    while IFS=$'\t' read -r num state title; do
      [ -z "${num:-}" ] && continue
      total=$((total + 1))

      local age age_col
      age=$(claim_age_hours "$num" "$label")
      age_col="${age}h"
      [ "$age" = "unknown" ] && age_col="?"

      case "$state" in
        CLOSED)
          if [ "$age" = "unknown" ]; then
            unknown=$((unknown + 1))
            row "$num" "UNKNOWN" "$age_col" "$label" "$title"
          elif [ "$age" -ge "$IDLE_HOURS" ]; then
            cold=$((cold + 1))
            row "$num" "ABANDONED" "$age_col" "$label" "$title"
            if [ "$APPLY" -eq 1 ]; then
              remove_claim "$num" "$label" && swept=$((swept + 1))
            fi
          else
            # Closed moments ago: almost certainly a live session between
            # "merge closed the issue" and "drop the claim". Not a finding.
            warm=$((warm + 1))
            row "$num" "CLOSING" "$age_col" "$label" "$title"
          fi
          ;;
        OPEN)
          # `parked` means a human already recorded why this is not moving; the
          # claim is not what is hiding it, so it is not a finding.
          if has_label "$num" "parked"; then
            row "$num" "PARKED" "$age_col" "$label" "$title"
          elif [ "$age" = "unknown" ]; then
            unknown=$((unknown + 1))
            row "$num" "UNKNOWN" "$age_col" "$label" "$title"
          elif [ "$age" -ge "$IDLE_HOURS" ]; then
            open=$((open + 1))
            row "$num" "SUSPECT" "$age_col" "$label" "$title"
          else
            row "$num" "FRESH" "$age_col" "$label" "$title"
          fi
          ;;
        *)
          # An unrecognised state is never swept. This arm is exercised by the
          # test fixture precisely because "writes only to CLOSED" is the
          # load-bearing property and this is the branch a future edit could
          # quietly break.
          unknown=$((unknown + 1))
          row "$num" "UNKNOWN" "$age_col" "$label" "$title"
          ;;
      esac
    done <<<"$rows"
  done <<<"$labels"

  echo
  if [ "$total" -eq 0 ]; then
    echo "No claimed:* labels anywhere in $REPO."
    return 0
  fi

  echo "${total} claim label(s): ${cold} cold on CLOSED issues (abandoned), ${open} on OPEN issues idle >=${IDLE_HOURS}h."

  if [ "$cold" -gt 0 ]; then
    if [ "$APPLY" -eq 1 ]; then
      echo "Swept ${swept} of ${cold} abandoned claim(s)."
    else
      echo "Re-run with --apply to remove the ${cold} abandoned claim(s). A claim on a CLOSED"
      echo "issue affects nothing -- SELECT only reads open issues."
    fi
  fi

  if [ "$warm" -gt 0 ]; then
    echo
    echo "${warm} claim(s) are on issues closed less than ${IDLE_HOURS}h ago (CLOSING). Not swept: that"
    echo "is the normal window between a merge closing the issue and the session dropping its claim."
  fi

  if [ "$open" -gt 0 ]; then
    echo
    echo "The ${open} SUSPECT claim(s) are NOT swept, deliberately. A claim on an open issue may be"
    echo "live work, and distinguishing that from a dead session is the unsettled half of #2834:"
    echo "age alone is a crude proxy (a review round can legitimately take hours), and the better"
    echo "signal is whether the linked PR's head has moved. Check each by hand, or park it."
  fi

  if [ "$unknown" -gt 0 ]; then
    echo
    echo "NOTE: ${unknown} claim(s) are UNKNOWN -- an unrecognised issue state, a failed lookup, or"
    echo "no readable 'labeled' timeline event (including clock skew). Never swept, never a finding."
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    -h | --help)
      usage
      exit 0
      ;;
    --apply)
      APPLY=1
      shift
      ;;
    *)
      echo "ERROR: unrecognised argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

main
status=$?
if [ "$status" -ne 0 ]; then
  exit "$status"
fi
# A sweep that failed must not look like a successful one. The capability
# contract reserves 5 for "op failed"; without this an automated caller cannot
# tell "swept everything" from "every removal was rejected".
if [ "$SWEEP_FAILED" -ne 0 ]; then
  exit 5
fi
