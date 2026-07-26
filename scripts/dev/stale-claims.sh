#!/usr/bin/env bash
#
# scripts/dev/stale-claims.sh
# ===========================
#
# Report `claimed:<session>` labels that no live session is holding, and -- on
# CLOSED issues only, and only with --apply -- remove them (memql#2834).
#
# WHY IT MATTERS. The claim label is the concurrency primitive the ship-issue
# loop rests on: SELECT discards any issue carrying `claimed:*`. So a claim held
# by a dead session makes the issue PERMANENTLY INVISIBLE -- not parked, not
# assigned, not in anyone's queue, and with no diagnostic comment a human could
# find. It looks exactly like active work. Two dead sessions left claims behind
# in a single night (#2801 `claimed:s4b9e2`, #2785 `claimed:t8w4n1`), both found
# only because a human noticed unmerged PRs and asked.
#
# WHY CLOSED ISSUES ARE SWEPT AND OPEN ONES ARE ONLY REPORTED.
#
# This is the split the issue calls its "cheapest useful first step", and the
# reason is that the two cases have completely different risk:
#
#   - a `claimed:*` label on a CLOSED issue is unambiguously abandoned. The
#     work is done; SELECT only looks at open issues, so the label affects
#     nothing and removing it cannot race a live session. Both cases above were
#     this shape.
#   - a claim on an OPEN issue may be live work. Distinguishing "working on it"
#     from "died two hours ago" is exactly what #2834 lists as unsettled --
#     what threshold, whether wall-clock age or PR-commit activity is the
#     liveness signal, and what to do when the stalled claim's PR is already
#     green. Guessing here reaps a claim mid-review; the review rounds on #2789
#     ran 10-20 minutes each and that PR legitimately took three of them over
#     roughly two hours.
#
# So open-issue claims are REPORTED with their age and PR state, and the
# decision stays with a human. That also mirrors the sibling reporter
# (stalled-prs.sh, #2833), which declined to auto-enqueue for a related reason.
#
# MUTATION IS OPT-IN. The default run makes no GitHub write of any kind, so it
# is safe to put on a loop or in CI. `--apply` is required to remove anything,
# and even then it only touches closed issues. stale_claims_test.go enforces
# both halves behaviourally: it RUNS this script against a stub `gh` that
# refuses anything outside a read allowlist, and asserts the default run
# invokes no mutation while --apply invokes exactly the expected label removal.
#
# DEFAULT-DENY. A removal is a write, so it happens only when every input is
# positively known: the issue is CLOSED, the label matches `claimed:*`, and the
# label was read back from the API rather than inferred. Anything unresolved --
# a failed call, an unparseable payload -- is reported as UNKNOWN and never
# swept.
#
# Usage:
#   bash scripts/dev/stale-claims.sh                    # report only, no writes
#   bash scripts/dev/stale-claims.sh --apply            # + sweep CLOSED issues
#   IDLE_HOURS=6 bash scripts/dev/stale-claims.sh       # open-claim age cutoff
#   REPO=owner/name bash scripts/dev/stale-claims.sh
#
# Backs 'make claims-stale'.
#
# Exit codes: 0 report produced | 2 bad parameter | 4 gh unusable.
#
# Refs: #2834 #2833

# Reporter: no -e, so one failing gh call for a single issue does not abort the
# rest of the report.
set -uo pipefail

REPO="${REPO:-znasllc-io/memql}"
# Hours a claim on an OPEN issue must have sat before it is reported as
# suspicious. Hours, not minutes, per #2834: a slow review gate must not be
# reaped mid-flight.
IDLE_HOURS="${IDLE_HOURS:-4}"
# One page. Not configurable -- raising a cap silently is how it stops being
# noticed. main() warns when a page comes back full.
ISSUE_PAGE_LIMIT=100

APPLY=0

usage() {
  cat <<'USAGE'
stale-claims.sh -- report claimed:* labels no live session is holding.

Default is READ-ONLY: it makes no GitHub mutation. `--apply` removes the label
from CLOSED issues only, where an abandoned claim is unambiguous and cannot
race a live session. Claims on OPEN issues are always reported, never swept --
distinguishing a live worker from a dead one is the unsettled half of #2834.

Usage:
  bash scripts/dev/stale-claims.sh
  bash scripts/dev/stale-claims.sh --apply

Environment:
  REPO         owner/name to scan (default: znasllc-io/memql)
  IDLE_HOURS   age before an OPEN issue's claim is reported (default: 4)

Exit codes: 0 report produced, 2 bad parameter, 4 gh unusable.
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

# claimed_issues prints one TSV row per issue carrying a claimed:* label:
# number, state, labels(comma), updatedEpoch, title.
#
# Both states are fetched in one call -- the sweep needs CLOSED and the report
# needs OPEN, and a single page keeps the API budget flat.
claimed_issues() {
  gh issue list -R "$REPO" --state all --limit "$ISSUE_PAGE_LIMIT" \
    --json number,state,labels,updatedAt,title \
    --jq '.[] | select([.labels[].name] | any(startswith("claimed:")))
          | [.number, .state, ([.labels[].name] | join(",")), (.updatedAt|fromdateiso8601), .title]
          | @tsv'
}

# claim_labels_of echoes each claimed:* label in a comma-separated label list.
claim_labels_of() {
  printf '%s\n' "$1" | tr ',' '\n' | grep '^claimed:' || true
}

# has_label reports whether a comma-separated list contains an exact label.
has_label() {
  printf '%s\n' "$1" | tr ',' '\n' | grep -qx "$2"
}

# idle_hours converts an epoch to whole hours elapsed, or `unknown`.
idle_hours() {
  local then="$1" now
  case "$then" in
    '' | *[!0-9]*)
      echo "unknown"
      return
      ;;
  esac
  now=$(date -u +%s)
  if [ "$then" -gt "$now" ]; then
    echo "unknown" # clock skew -- never manufacture a finding from it
    return
  fi
  echo $(((now - then) / 3600))
}

# remove_claim drops one claimed:* label from one issue. The ONLY mutation in
# this file, reached only for a CLOSED issue and only under --apply.
remove_claim() {
  local num="$1" label="$2"
  if gh issue edit "$num" -R "$REPO" --remove-label "$label" >/dev/null 2>&1; then
    echo "  swept  #$num  $label"
    return 0
  fi
  echo "  FAILED #$num  $label (label removal rejected; left in place)" >&2
  return 1
}

main() {
  require_valid_threshold
  require_gh

  local rows
  rows=$(claimed_issues)
  if [ $? -ne 0 ]; then
    echo "ERROR: could not list issues for $REPO; the report would be a false all-clear." >&2
    exit 4
  fi

  if [ "$(grep -c . <<<"$rows")" -ge "$ISSUE_PAGE_LIMIT" ]; then
    echo "WARNING: hit the ${ISSUE_PAGE_LIMIT}-issue page limit; issues beyond it were NOT examined." >&2
    echo "         This report is incomplete -- treat a clean result as unproven." >&2
  fi

  printf '%-7s %-7s %-7s %-22s  %s\n' "ISSUE" "STATE" "IDLE" "CLAIM" "TITLE"
  printf '%-7s %-7s %-7s %-22s  %s\n' "-------" "-------" "-------" "----------------------" "-----"

  local closed=0 open=0 total=0 unknown=0 swept=0
  while IFS=$'\t' read -r num state labels updated title; do
    [ -z "${num:-}" ] && continue
    local age
    age=$(idle_hours "$updated")

    while read -r label; do
      [ -z "$label" ] && continue
      total=$((total + 1))
      local age_col="${age}h"
      [ "$age" = "unknown" ] && age_col="?"

      case "$state" in
        CLOSED)
          closed=$((closed + 1))
          printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "ABANDONED" "$age_col" "${label:0:22}" "${title:0:44}"
          if [ "$APPLY" -eq 1 ]; then
            remove_claim "$num" "$label" && swept=$((swept + 1))
          fi
          ;;
        OPEN)
          # `parked` means a human already recorded why this is not moving; the
          # claim is not what is hiding it, so it is not a finding.
          if has_label "$labels" "parked"; then
            printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "PARKED" "$age_col" "${label:0:22}" "${title:0:44}"
          elif [ "$age" = "unknown" ]; then
            unknown=$((unknown + 1))
            printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "UNKNOWN" "$age_col" "${label:0:22}" "${title:0:44}"
          elif [ "$age" -ge "$IDLE_HOURS" ]; then
            open=$((open + 1))
            printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "SUSPECT" "$age_col" "${label:0:22}" "${title:0:44}"
          else
            printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "FRESH" "$age_col" "${label:0:22}" "${title:0:44}"
          fi
          ;;
        *)
          unknown=$((unknown + 1))
          printf '#%-6s %-7s %-7s %-22s  %s\n' "$num" "UNKNOWN" "$age_col" "${label:0:22}" "${title:0:44}"
          ;;
      esac
    done < <(claim_labels_of "$labels")
  done <<<"$rows"

  echo
  if [ "$total" -eq 0 ]; then
    echo "No claimed:* labels anywhere in $REPO."
    return 0
  fi

  echo "${total} claim label(s): ${closed} on CLOSED issues (abandoned), ${open} on OPEN issues idle >=${IDLE_HOURS}h."

  if [ "$closed" -gt 0 ]; then
    if [ "$APPLY" -eq 1 ]; then
      echo "Swept ${swept} of ${closed} abandoned claim(s)."
    else
      echo "Re-run with --apply to remove the ${closed} abandoned claim(s). A claim on a CLOSED"
      echo "issue affects nothing (SELECT only reads open issues) and cannot race a live session."
    fi
  fi

  if [ "$open" -gt 0 ]; then
    echo
    echo "The ${open} SUSPECT claim(s) are NOT swept, deliberately. A claim on an open issue may be"
    echo "live work, and distinguishing that from a dead session is the unsettled half of #2834:"
    echo "wall-clock age is a crude proxy (a review round can legitimately take hours), and the"
    echo "better signal is whether the linked PR's head has moved. Check each by hand, or park it."
  fi

  if [ "$unknown" -gt 0 ]; then
    echo
    echo "NOTE: ${unknown} claim(s) are UNKNOWN -- an unrecognised issue state or an unparseable"
    echo "timestamp (including clock skew). Never swept and never counted as a finding."
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
