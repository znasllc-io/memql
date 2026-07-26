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
# that will review first. It is read-only: it makes no GitHub mutation at all,
# which also means it can never race a live session. stalled_prs_test.go
# enforces that by ALLOW-LISTING every `gh` invocation, rather than denying
# known-bad ones -- a denylist missed `gh pr comment`, `gh api -f` (which POSTs
# by default) and `gh api graphql -f query='mutation{...}'`, the last of which
# is a one-word edit from a call this script already makes.
#
# DEFAULT-DENY. STALLED is a finding that invites a human to enqueue, so it is
# reported ONLY when every input is positively known good. Anything unresolved
# -- a failed API call, a mergeability GitHub is still recomputing, a PR with
# no check runs -- lands in UNKNOWN instead. The first cut made STALLED the
# fallthrough, so one transient 502 manufactured a finding.
#
# Usage:
#   bash scripts/dev/stalled-prs.sh                  # human table on stdout
#   IDLE_MINUTES=15 bash scripts/dev/stalled-prs.sh  # tighter idle threshold
#   REPO=owner/name bash scripts/dev/stalled-prs.sh  # another repo
#
# Backs 'make prs-stalled'.
#
# Exit codes: 0 report produced (including "nothing stalled") | 4 gh unusable.
#
# Refs: #2833 #2834

# Reporter: no -e, so one failing gh call for a single PR does not abort the
# rest of the report. Every such failure resolves to UNKNOWN, never to a
# finding and never to silence.
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

# open_prs prints one TSV row per open PR: number, draft, mergeState,
# updatedEpoch, title.
#
# updatedAt is converted to epoch by jq (fromdateiso8601) rather than by
# `date -u -d`, which is GNU-only: on macOS -- the platform CLAUDE.md names as
# standard -- that flag fails, and the first cut swallowed the error into an
# idle of 0, so every PR reported FRESH and the tool said "no stalled PRs"
# forever without a word.
open_prs() {
  gh pr list -R "$REPO" --state open --limit 100 \
    --json number,isDraft,mergeStateStatus,updatedAt,title \
    --jq '.[] | [.number, .isDraft, .mergeStateStatus, (.updatedAt|fromdateiso8601), .title] | @tsv'
}

# in_merge_queue echoes true / false / unknown. The REST PR payload does not
# carry this, so it needs GraphQL. `unknown` is load-bearing: without it a
# failed call reported an already-queued PR as needing to be enqueued.
in_merge_queue() {
  local num="$1" out
  out=$(gh api graphql -f query="{repository(owner:\"${REPO%%/*}\",name:\"${REPO##*/}\"){pullRequest(number:${num}){isInMergeQueue}}}" \
    --jq '.data.repository.pullRequest.isInMergeQueue' 2>/dev/null)
  case "$out" in
    true | false) echo "$out" ;;
    *) echo "unknown" ;;
  esac
}

# check_state collapses a PR head's check runs to: green, failing, pending,
# none, unknown.
#
# `skipped` and `neutral` count as green -- this repo path-filters several
# lanes, so a healthy PR routinely carries them, and treating a skip as
# non-green would mark every PR red. A failure outranks a pending run: a build
# that has already failed is failing, whatever else is still going.
check_state() {
  local num="$1" sha runs
  sha=$(gh pr view "$num" -R "$REPO" --json headRefOid --jq .headRefOid 2>/dev/null)
  [ -z "$sha" ] && { echo "unknown"; return; }

  runs=$(gh api "repos/$REPO/commits/$sha/check-runs" --jq '.check_runs[] | (.conclusion // .status)' 2>/dev/null)
  if [ $? -ne 0 ]; then
    echo "unknown"
    return
  fi
  # No runs at all: CI never triggered. NOT green -- that is precisely the
  # state where "ready to merge" is least trustworthy.
  [ -z "$runs" ] && { echo "none"; return; }

  if grep -qE '^(failure|cancelled|timed_out|action_required|stale|startup_failure)$' <<<"$runs"; then
    echo "failing"
    return
  fi
  if grep -qE '^(queued|in_progress|pending|waiting|requested)$' <<<"$runs"; then
    echo "pending"
    return
  fi
  if grep -qvE '^(success|skipped|neutral)$' <<<"$runs"; then
    echo "unknown" # a conclusion this script has not been taught
    return
  fi
  echo "green"
}

idle_minutes() {
  local updated_epoch="$1" now
  case "$updated_epoch" in
    '' | *[!0-9]*) echo "-1"; return ;; # unparseable -> caller treats as unknown
  esac
  now=$(date -u +%s)
  echo $(( (now - updated_epoch) / 60 ))
}

# classify returns the bucket for one PR.
#
# STALLED is the only finding, and it is reached only when every input is
# positively known: not draft, definitely not queued, definitely green,
# definitely CLEAN, and a readable idle time past the threshold. Every other
# path -- including every "could not determine" -- lands somewhere else.
classify() {
  local draft="$1" merge_state="$2" queued="$3" checks="$4" idle="$5"

  [ "$draft" = "true" ] && { echo "DRAFT"; return; }

  case "$queued" in
    true) echo "QUEUED"; return ;;
    false) ;;
    *) echo "UNKNOWN"; return ;;
  esac

  case "$checks" in
    green) ;;
    failing) echo "RED"; return ;;
    pending) echo "PENDING"; return ;;
    none) echo "NO-CI"; return ;;
    *) echo "UNKNOWN"; return ;;
  esac

  case "$merge_state" in
    CLEAN) ;;
    DIRTY | BLOCKED | BEHIND) echo "CONFLICT"; return ;;
    *) echo "UNKNOWN"; return ;; # UNKNOWN / UNSTABLE / anything new
  esac

  [ "$idle" -lt 0 ] && { echo "UNKNOWN"; return; }
  [ "$idle" -lt "$IDLE_MINUTES" ] && { echo "FRESH"; return; }
  echo "STALLED"
}

main() {
  require_gh

  local rows
  rows=$(open_prs)
  if [ $? -ne 0 ]; then
    echo "ERROR: could not list PRs for $REPO; the report would be a false all-clear." >&2
    exit 4
  fi

  printf '%-7s %-9s %-7s %-7s  %s\n' "PR" "STATE" "IDLE" "CHECKS" "TITLE"
  printf '%-7s %-9s %-7s %-7s  %s\n' "-------" "---------" "-------" "-------" "-----"

  local stalled=0 total=0 unknown=0
  while IFS=$'\t' read -r num draft merge_state updated_epoch title; do
    [ -z "${num:-}" ] && continue
    total=$((total + 1))
    local queued checks idle bucket
    queued=$(in_merge_queue "$num")
    checks=$(check_state "$num")
    idle=$(idle_minutes "$updated_epoch")
    bucket=$(classify "$draft" "$merge_state" "$queued" "$checks" "$idle")
    [ "$bucket" = "STALLED" ] && stalled=$((stalled + 1))
    [ "$bucket" = "UNKNOWN" ] && unknown=$((unknown + 1))
    local idle_col="${idle}m"
    [ "$idle" -lt 0 ] && idle_col="?"
    printf '#%-6s %-9s %-7s %-7s  %s\n' "$num" "$bucket" "$idle_col" "$checks" "${title:0:56}"
  done <<<"$rows"

  echo
  if [ "$total" -eq 0 ]; then
    echo "No open PRs."
  elif [ "$stalled" -eq 0 ]; then
    echo "No stalled PRs: every open PR is queued, draft, red, or still within the ${IDLE_MINUTES}m idle window."
  else
    echo "${stalled} of ${total} open PR(s) are STALLED -- green, mergeable, and nobody is advancing them."
    echo "Review before enqueuing: green is not reviewed (memql#2833). A dead session may also"
    echo "have left a claimed:* label behind on its issue (memql#2834)."
  fi
  if [ "$unknown" -gt 0 ]; then
    echo
    echo "NOTE: ${unknown} PR(s) are UNKNOWN -- an API call failed or GitHub is still computing"
    echo "mergeability. They are deliberately NOT reported as stalled; re-run to resolve them."
  fi
}

main "$@"
