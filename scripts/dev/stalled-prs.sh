#!/usr/bin/env bash
#
# scripts/dev/stalled-prs.sh
# ==========================
#
# Report open PRs that are READY TO MERGE but that nobody is advancing --
# green checks, mergeable, not draft, not in the merge queue, and idle past a
# threshold (memql#2833).
#
# WHY THIS REPORTS AND DOES NOT ENQUEUE. Enqueuing is an explicit step a worker
# takes after its own review gate. When a session dies, is interrupted, or
# stalls before that point, its PR sits green and un-enqueued indefinitely --
# twice in one night the repo owner had to notice by hand. The obvious fix is a
# sweeper that enqueues them, and that fix is wrong here: adversarial review of
# every PR merged that night found a real defect in EVERY one, so "green" is
# demonstrably not "reviewed" in this repo. A sweeper that auto-enqueued would
# land unreviewed work at exactly the moment nobody is watching.
#
# So this surfaces the queue and leaves the decision to a human or to a worker
# that will review first. It is deliberately read-only: it makes no GitHub
# mutations at all, which also means it can never race a live session.
#
# Usage:
#   bash scripts/dev/stalled-prs.sh                  # human table
#   IDLE_MINUTES=15 bash scripts/dev/stalled-prs.sh  # tighter idle threshold
#   REPO=owner/name bash scripts/dev/stalled-prs.sh  # another repo
#
# Backs 'make prs-stalled'.
#
# Exit codes: 0 report produced (including "nothing stalled") | 4 gh unusable.
#
# Refs: #2833 #2834

# Reporter: no -e, so one failing gh call for a single PR does not abort the
# rest of the report.
set -uo pipefail

REPO="${REPO:-znasllc-io/memql}"
IDLE_MINUTES="${IDLE_MINUTES:-45}"

require_gh() {
  if ! command -v gh >/dev/null 2>&1; then
    echo "ERROR: gh CLI not found; this report reads the GitHub API." >&2
    exit 4
  fi
  if ! gh auth status >/dev/null 2>&1; then
    echo "ERROR: gh is not authenticated (gh auth login)." >&2
    exit 4
  fi
}

# open_prs prints one TSV row per open PR: number, draft, mergeState, updatedAt, title.
open_prs() {
  gh pr list -R "$REPO" --state open --limit 100 \
    --json number,isDraft,mergeStateStatus,updatedAt,title \
    --jq '.[] | [.number, .isDraft, .mergeStateStatus, .updatedAt, .title] | @tsv'
}

# in_merge_queue reports whether a PR is already queued. The REST PR payload
# does not carry this, so it needs GraphQL.
in_merge_queue() {
  local num="$1"
  gh api graphql -f query="{repository(owner:\"${REPO%%/*}\",name:\"${REPO##*/}\"){pullRequest(number:${num}){isInMergeQueue}}}" \
    --jq '.data.repository.pullRequest.isInMergeQueue' 2>/dev/null || echo "unknown"
}

# check_state collapses a PR head's check runs to one of: green, failing,
# pending, none. `skipped` counts as green -- a path-filtered lane that did not
# run is not a failure.
check_state() {
  local num="$1" sha
  sha=$(gh pr view "$num" -R "$REPO" --json headRefOid --jq .headRefOid 2>/dev/null) || { echo "unknown"; return; }
  [ -z "$sha" ] && { echo "unknown"; return; }

  local runs
  runs=$(gh api "repos/$REPO/commits/$sha/check-runs" --jq '.check_runs[] | (.conclusion // .status)' 2>/dev/null)
  [ -z "$runs" ] && { echo "none"; return; }

  if grep -qvE '^(success|skipped|neutral|completed)$' <<<"$runs"; then
    if grep -qE '^(queued|in_progress|pending)$' <<<"$runs"; then
      echo "pending"
    else
      echo "failing"
    fi
    return
  fi
  echo "green"
}

idle_minutes() {
  local updated="$1" then now
  then=$(date -u -d "$updated" +%s 2>/dev/null) || { echo 0; return; }
  now=$(date -u +%s)
  echo $(( (now - then) / 60 ))
}

# classify returns the bucket for one PR. Only STALLED is a finding; the rest
# are printed for context so the report explains what it did NOT flag.
classify() {
  local draft="$1" merge_state="$2" queued="$3" checks="$4" idle="$5"
  [ "$draft" = "true" ] && { echo "DRAFT"; return; }
  [ "$queued" = "true" ] && { echo "QUEUED"; return; }
  case "$checks" in
    failing) echo "RED"; return ;;
    pending) echo "PENDING"; return ;;
  esac
  case "$merge_state" in
    DIRTY|BLOCKED|behind) echo "CONFLICT"; return ;;
  esac
  if [ "$idle" -lt "$IDLE_MINUTES" ]; then
    echo "FRESH"
    return
  fi
  echo "STALLED"
}

main() {
  require_gh

  printf '%-7s %-9s %-7s %-6s  %s\n' "PR" "STATE" "IDLE" "CHECKS" "TITLE" >&2
  printf '%-7s %-9s %-7s %-6s  %s\n' "-------" "---------" "-------" "------" "-----" >&2

  local stalled=0 total=0
  while IFS=$'\t' read -r num draft merge_state updated title; do
    [ -z "${num:-}" ] && continue
    total=$((total + 1))
    local queued checks idle bucket
    queued=$(in_merge_queue "$num")
    checks=$(check_state "$num")
    idle=$(idle_minutes "$updated")
    bucket=$(classify "$draft" "$merge_state" "$queued" "$checks" "$idle")
    [ "$bucket" = "STALLED" ] && stalled=$((stalled + 1))
    printf '#%-6s %-9s %-7s %-6s  %s\n' "$num" "$bucket" "${idle}m" "$checks" "${title:0:56}" >&2
  done < <(open_prs)

  echo >&2
  if [ "$stalled" -eq 0 ]; then
    echo "No stalled PRs: every open PR is queued, draft, red, or still within the ${IDLE_MINUTES}m idle window." >&2
  else
    echo "${stalled} of ${total} open PR(s) are STALLED -- green, mergeable, and nobody is advancing them." >&2
    echo "Review before enqueuing: green is not reviewed (memql#2833). A dead session may also" >&2
    echo "have left a claimed:* label behind on its issue (memql#2834)." >&2
  fi
}

main "$@"
