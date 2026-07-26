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
# which also means it can never race a live session.
#
# HOW THAT IS CHECKED, and how far the check goes. stalled_prs_test.go RUNS
# this script against a stub `gh` that refuses any non-read subcommand and
# records every call, then asserts nothing forbidden was invoked. That is a
# behavioural check: it sees what bash actually executed, so it does not care
# how the command word was spelled.
#
# It replaced a static scan of this file, which three review rounds defeated in
# three different ways -- the last simply by putting quotes around one token
# (`"gh" pr merge`). Statically out-parsing shell quoting, expansion, eval,
# sub-shells and indirection is not a winnable game, so the check no longer
# tries. The trade is worth naming plainly: the behavioural check covers the
# code paths the tests drive, not unreachable ones. A cheap scan for blatant
# mutation verbs remains as defence in depth, NOT as a proof.
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
# Exit codes: 0 report produced (including "nothing stalled") | 2 bad parameter
#             | 4 gh unusable.
#
# Refs: #2833 #2834

# Reporter: no -e, so one failing gh call for a single PR does not abort the
# rest of the report. Every such failure resolves to UNKNOWN, never to a
# finding and never to silence.
set -uo pipefail

REPO="${REPO:-znasllc-io/memql}"
IDLE_MINUTES="${IDLE_MINUTES:-45}"
# One page of PRs. Not configurable on purpose -- raising it silently is how a
# cap stops being noticed. main() warns when the page comes back full.
PR_PAGE_LIMIT=100

require_gh() {
  # `gh --version` rather than `command -v gh`: it covers missing AND broken in
  # one probe, and it keeps every `gh` token in this file an actual invocation,
  # which is what lets the allowlist in stalled_prs_test.go demand that EVERY
  # occurrence be a known-read-only call instead of guessing which ones are.
  if ! gh --version >/dev/null 2>&1; then
    echo "ERROR: gh CLI not found or not working; this report reads the GitHub API." >&2
    exit 4
  fi
  if ! gh auth status >/dev/null 2>&1; then
    echo "ERROR: gh is not authenticated (run: gh auth login)." >&2
    exit 4
  fi
}

# IDLE_MINUTES is a documented knob, so a typo in it must not silently become a
# finding. `[ "$x" -lt "$y" ]` ERRORS on a non-integer rather than evaluating
# false, so an unvalidated `IDLE_MINUTES=45m` made every comparison fail and
# every PR -- including one-minute-old ones -- fall through to STALLED.
require_valid_threshold() {
  case "$IDLE_MINUTES" in
    '' | *[!0-9]*)
      echo "ERROR: IDLE_MINUTES must be a whole number of minutes; got '${IDLE_MINUTES}'." >&2
      exit 2
      ;;
  esac
  # All-digits is NOT sufficient. `[ "$a" -lt "$b" ]` errors on a value that
  # overflows int64 exactly as it does on "45m", and an errored test falls
  # through to the next line -- so IDLE_MINUTES=99999999999999999999, a
  # threshold meaning "show me nothing", reported EVERY pr as stalled. Cap
  # well below 2^63 and well above any real use (10^9 minutes ~ 1900 years).
  if [ "${#IDLE_MINUTES}" -gt 10 ]; then
    echo "ERROR: IDLE_MINUTES=${IDLE_MINUTES} is implausibly large; use a value under 10 digits." >&2
    exit 2
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
  gh pr list -R "$REPO" --state open --limit "$PR_PAGE_LIMIT" \
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
# collapse_runs reduces a newline-separated list of check-run conclusions to a
# single state. Split out of check_state so it is testable without the API --
# the previous "skipped counts as green" test only grepped the source for the
# word `skipped` and passed even when the logic was gutted, because the word
# survived in a comment.
collapse_runs() {
  local runs="$1"
  # Blank lines are stripped first. Without this a TRAILING newline made the
  # final `grep -qvE` match the empty line and return `unknown` for a
  # perfectly green PR:
  #
  #   collapse_runs $'success\nsuccess'    -> green
  #   collapse_runs $'success\nsuccess\n'  -> unknown
  #
  # Unreachable from check_state today only because $( ) strips trailing
  # newlines -- but the whole reason this function is split out is to be
  # callable without the API, and a newline-delimited list is exactly the
  # shape a caller would hand it. Any future change to how `runs` is captured
  # (mapfile, a file redirect, --slurp) would otherwise turn every PR UNKNOWN
  # forever, under a note telling the operator to re-run to resolve it.
  runs=$(printf '%s\n' "$runs" | grep -v '^[[:space:]]*$' || true)

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

check_state() {
  local num="$1" sha runs
  sha=$(gh pr view "$num" -R "$REPO" --json headRefOid --jq .headRefOid 2>/dev/null)
  [ -z "$sha" ] && { echo "unknown"; return; }

  runs=$(gh api --paginate "repos/$REPO/commits/$sha/check-runs" --jq '.check_runs[] | (.conclusion // .status)' 2>/dev/null)
  if [ $? -ne 0 ]; then
    echo "unknown"
    return
  fi
  collapse_runs "$runs"
}

idle_minutes() {
  local updated_epoch="$1" now
  case "$updated_epoch" in
    '' | *[!0-9]*) echo "unknown"; return ;;
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

  case "$draft" in
    true) echo "DRAFT"; return ;;
    false) ;;
    *) echo "UNKNOWN"; return ;;
  esac

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
    DIRTY) echo "CONFLICT"; return ;;
    BEHIND) echo "BEHIND"; return ;;
    # Not a conflict: branch protection is unsatisfied, usually an awaited
    # review. Reporting it as CONFLICT sends the operator to rebase a
    # mergeable branch; it is also the population this tool targets, so it
    # gets its own honest label rather than being folded into one.
    BLOCKED) echo "BLOCKED"; return ;;
    *) echo "UNKNOWN"; return ;; # UNKNOWN / UNSTABLE / anything new
  esac

  # Digits only. `[ "$idle" -lt N ]` errors (rather than being false) on a
  # non-integer, and an errored test falls through to the line below it -- so
  # without this guard an unreadable timestamp reached STALLED.
  case "$idle" in
    '' | *[!0-9]*) echo "UNKNOWN"; return ;;
  esac

  [ "$idle" -lt "$IDLE_MINUTES" ] && { echo "FRESH"; return; }
  echo "STALLED"
}

main() {
  require_valid_threshold
  require_gh

  local rows
  rows=$(open_prs)
  if [ $? -ne 0 ]; then
    echo "ERROR: could not list PRs for $REPO; the report would be a false all-clear." >&2
    exit 4
  fi

  # A full page means the list was almost certainly truncated. Say so: this
  # tool's entire job is to notice work nobody is advancing, so dropping the
  # remainder silently -- and possibly still printing "No STALLED PRs" -- is
  # the one failure mode it must not have. (CLAUDE.md: no silent caps.)
  if [ "$(grep -c . <<<"$rows")" -ge "$PR_PAGE_LIMIT" ]; then
    echo "WARNING: hit the ${PR_PAGE_LIMIT}-PR page limit; PRs beyond it were NOT examined." >&2
    echo "         This report is incomplete -- treat a clean result as unproven." >&2
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
    [ "$idle" = "unknown" ] && idle_col="?"
    printf '#%-6s %-9s %-7s %-7s  %s\n' "$num" "$bucket" "$idle_col" "$checks" "${title:0:56}"
  done <<<"$rows"

  echo
  if [ "$total" -eq 0 ]; then
    echo "No open PRs."
  elif [ "$stalled" -eq 0 ]; then
    # Deliberately does NOT enumerate the other buckets. An earlier version
    # said "every open PR is queued, draft, red, or still within the idle
    # window", which is false: CONFLICT, BEHIND, BLOCKED, NO-CI and UNKNOWN
    # are all reachable and none of them is any of those four. A summary line
    # that lists the wrong set reads as an all-clear over PRs that need work.
    echo "No stalled PRs. ${total} open PR(s) are in some other state -- see the table above."
  else
    echo "${stalled} of ${total} open PR(s) are STALLED -- green, mergeable, and nobody is advancing them."
    echo "Review before enqueuing: green is not reviewed (memql#2833). A dead session may also"
    echo "have left a claimed:* label behind on its issue (memql#2834)."
  fi
  if [ "$unknown" -gt 0 ]; then
    echo
    # UNKNOWN has three causes and only one of them clears on a re-run, so the
    # note names all three. Saying "re-run to resolve them" alone sent the
    # operator into a loop on the two that never resolve.
    echo "NOTE: ${unknown} PR(s) are UNKNOWN. Three causes:"
    echo "  - an API call failed, or GitHub is still computing mergeability -- a re-run resolves this one;"
    echo "  - mergeStateStatus is UNSTABLE (a non-required check is red) -- a re-run will not change it;"
    echo "  - a check-run conclusion this script has not been taught -- needs a code change."
    echo "They are deliberately NOT reported as stalled either way."
  fi
}

usage() {
  cat <<'USAGE'
stalled-prs.sh -- report open PRs that are green, mergeable, and un-enqueued.

READ-ONLY. Makes no GitHub mutation of any kind; that is enforced by
TestStalledPRs_MakesNoMutatingCall, which runs the script against a stub `gh`
that refuses anything outside a read allowlist.

Usage:
  bash scripts/dev/stalled-prs.sh          # or: make prs-stalled
  IDLE_MINUTES=15 bash scripts/dev/stalled-prs.sh

Environment:
  REPO           owner/name to report on (default: znasllc-io/memql)
  IDLE_MINUTES   how long a green PR must sit before it counts as STALLED
                 (default: 45)

Exit codes: 0 report produced, 2 bad parameter, 4 gh unusable.
USAGE
}

# `main` never read $@, so `--help` printed the full report -- a live API sweep
# for someone who just wanted the usage text. Usage lived only in a comment.
case "${1:-}" in
  -h | --help) usage; exit 0 ;;
  "") ;;
  *) echo "ERROR: unrecognised argument: $1" >&2; usage >&2; exit 2 ;;
esac

main "$@"
