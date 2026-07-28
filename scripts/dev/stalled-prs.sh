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
# IDLENESS IS HEAD-SHA STABILITY, NOT `updatedAt` (memql#2887). A PR is idle
# when no new CODE has arrived, not when nobody has typed. `updatedAt` bumps on
# every comment, label, review and re-run, so a dying session that posts its
# review summary before it stops resets its own clock -- and the tool then
# prints the one thing it must never print: an all-clear over abandoned work.
# Measured on a 49-PR sample of a live repository, `updatedAt` understated
# head-commit age by up to 128,356 minutes -- 89 days. The sibling
# stale-claims.sh reached the same conclusion about issues and paid an extra
# call per issue for it; this costs nothing.
#
# THE CLOCK IS THE HEAD COMMIT'S `committedDate`, read from the SAME per-PR
# GraphQL call that already asks `isInMergeQueue`. Zero extra requests.
#
# Adding `commits` to `gh pr list` instead is NOT an option, and the failure is
# total rather than gradual. gh's `commits` selection nests an
# `authors(first:100)` connection, so at PR_PAGE_LIMIT=100 GitHub refuses the
# whole query:
#
#   GraphQL: By the time this query traverses to the authors connection, it is
#   requesting up to 1,000,000 possible nodes which exceeds the maximum limit
#   of 500,000.
#
# `gh pr list` then exits non-zero and main() exits 4 -- the tool stops working
# entirely, on the first run. The ceiling is 49 PRs (49 x 10101 = 494,949
# nodes; 50 fails at 505,050), so taking that route would silently halve the
# page limit, which is exactly what PR_PAGE_LIMIT exists to prevent.
#
# WHAT `committedDate` DOES AND DOES NOT MEAN. It is the git COMMITTER date of
# the head commit. Git rewrites it on rebase, amend and cherry-pick, and GitHub
# sets it on an "Update branch" merge, so every way a head SHA moves forward
# moves this clock forward too. Nothing that is not a code change touches it.
#
# It is NOT push time, and two gaps follow. Both are named here rather than
# hidden:
#   - a commit made locally at T and pushed at T+30m reads 30 minutes idler
#     than it is, so a live PR can reach STALLED early;
#   - a force-push BACK to an older commit moves the SHA while moving this
#     clock backwards.
# The true "SHA last changed" instant lies somewhere in
# [committedDate, updatedAt]; narrowing it needs a timeline query per PR -- a
# third round-trip for a report whose only action is to ask a human to look.
# This end of the interval is chosen deliberately: it errs toward showing a
# human a PR that turns out to be live, never toward silence over one that is
# not. It does not weaken DEFAULT-DENY above, which exists to stop UNRESOLVED
# inputs manufacturing findings; this input is resolved, with a skew equal to
# the worker's commit-to-push lag, which in this repo's checkpoint-push
# workflow is seconds.
#
# `Commit.pushedDate` is not the answer either: GitHub deprecated it and it
# returns null for anything pushed after mid-2020, which would turn every PR
# UNKNOWN and leave the report a permanent no-op that still exits 0.
#
# `updatedAt` is still fetched and still shown, in the TOUCHED column, because
# a reader who sees IDLE 7d on a PR commented on yesterday will otherwise
# assume the tool is broken. It gates nothing.
#
# THE SECOND SWEEP: CLOSED WITHOUT MERGING (memql#2887, asked for in #2833).
# `--delete-branch` on a close, or a stray `gh pr close`, drops written work on
# the floor and nothing notices: the PR is closed with mergedAt null, its issue
# silently returns to the pool, and the work is rebuilt from scratch -- or not,
# if a claimed:* label was stranded too. That is the failure mode that loses
# work outright, so it is reported alongside the stalled-green one.
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
# Cap on the closed-unmerged sweep. Separate from PR_PAGE_LIMIT because the two
# populations grow differently: open PRs are bounded by how much is in flight,
# closed-unmerged ones accumulate forever. Newest-first ordering plus a loud
# warning is the same bargain the open sweep makes. There is deliberately no
# date bound: computing "14 days ago" portably needs `date -d` (GNU) or
# `date -r` (BSD), and this file rejects both by name -- see open_prs.
CLOSED_PAGE_LIMIT=100

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
# updatedEpoch, headSha, title.
#
# updatedAt is converted to epoch by jq (fromdateiso8601) rather than by
# `date -u -d`, which is GNU-only: on macOS -- the platform CLAUDE.md names as
# standard -- that flag fails, and the first cut swallowed the error into an
# idle of 0, so every PR reported FRESH and the tool said "no stalled PRs"
# forever without a word.
#
# headRefOid rides along here rather than being fetched per PR (memql#2887).
# check_state used to spend a whole `gh pr view` on it, which was a third of
# the tool's entire API budget for a value the list call already serves.
#
# `.title` MUST stay last. `IFS=$'\t' read -r ... title` gives the final
# variable everything remaining, which is what lets a tab-bearing title through
# without shifting columns. A new field always goes before it.
#
# The `// "-"` on headRefOid is not decoration: tab is an IFS WHITESPACE
# character, so an EMPTY middle field collapses with its neighbour and shifts
# every later column left. A placeholder keeps the row shape fixed, and "-"
# fails check_state's emptiness guard exactly as "" would have.
open_prs() {
  gh pr list -R "$REPO" --state open --limit "$PR_PAGE_LIMIT" \
    --json number,isDraft,mergeStateStatus,updatedAt,headRefOid,title \
    --jq '.[] | [.number, .isDraft, .mergeStateStatus, (.updatedAt|fromdateiso8601), (.headRefOid // "-"), .title] | @tsv'
}

# closed_unmerged_prs prints one TSV row per PR CLOSED WITHOUT MERGING:
# number, headRefName, closedEpoch, title.
#
# `is:unmerged` is pushed SERVER-SIDE deliberately. `gh pr list --state closed`
# returns merged PRs too -- gh maps that state to {CLOSED, MERGED} -- so on a
# repo with thousands of merges an unfiltered page of 100 is almost entirely
# merged PRs, and the sweep would find nothing while looking exhaustive. The jq
# `select` stays as well: two independent filters, so neither one silently
# becoming a no-op turns a merged PR into a finding.
#
# NOT `linked:issue`, which would also be a valid server-side filter and would
# halve the candidate set. It drops exactly the PRs whose "Closes #N" link
# never formed -- the ones most worth seeing. Narrowing a watchdog's input by
# the thing it is watching for is how this repo has lost rows before.
closed_unmerged_prs() {
  gh pr list -R "$REPO" --state closed --search "is:unmerged sort:updated-desc" \
    --limit "$CLOSED_PAGE_LIMIT" \
    --json number,state,mergedAt,headRefName,closedAt,title \
    --jq '.[] | select(.state == "CLOSED" and .mergedAt == null)
              | [.number, (.headRefName // "-"), ((.closedAt // empty | fromdateiso8601) // "-"), .title] | @tsv'
}

# pr_liveness makes the ONE per-PR GraphQL call and prints two TSV fields:
#
#   <queued>     true | false | unknown
#   <headEpoch>  epoch seconds of the head commit's committedDate, or unknown
#
# They ride together because they come from one request. The REST PR payload
# carries neither, and asking for the commit date separately would be a third
# round-trip per PR -- see the header for why `gh pr list --json commits` is
# not an option at all.
#
# `unknown` is load-bearing on both. Without it a failed call reported an
# already-queued PR as needing to be enqueued, and a missing date fell straight
# through to a finding.
#
# The returned oid is cross-checked against the head SHA `gh pr list` gave us.
# commits(last:1) IS the head commit -- but being wrong here yields a wrong
# CLOCK rather than an error, which is invisible in the output, so it is
# asserted rather than assumed. The check doubles as a mid-sweep race detector:
# a mismatch means the head moved between the two calls, so the PR is
# demonstrably being worked AND the check-runs already fetched describe a SHA
# that is no longer the head. Both point at UNKNOWN, never STALLED.
pr_liveness() {
  local num="$1" head_sha="$2" out queued oid epoch
  # Assignment and the status check on separate lines: `local out=$(...)` would
  # report local's own status, which is always 0.
  out=$(gh api graphql -f query="{repository(owner:\"${REPO%%/*}\",name:\"${REPO##*/}\"){pullRequest(number:${num}){isInMergeQueue commits(last:1){nodes{commit{oid committedDate}}}}}}" \
    --jq '.data.repository.pullRequest
          | [ (.isInMergeQueue | tostring)
            , (.commits.nodes[0].commit.oid // "")
            , ((.commits.nodes[0].commit.committedDate // empty | fromdateiso8601 | tostring) // "")
            ] | @tsv' 2>/dev/null)

  # Herestring, not a bare `read`: reading from stdin here would eat rows out
  # of main()'s `while ... <<<"$rows"` loop and silently halve the report.
  IFS=$'\t' read -r queued oid epoch <<<"$out"

  case "${queued:-}" in
    true | false) ;;
    *) queued="unknown" ;;
  esac
  if [ -z "${oid:-}" ] || [ "$oid" != "$head_sha" ]; then
    epoch="unknown"
  fi
  case "${epoch:-}" in
    '' | *[!0-9]*) epoch="unknown" ;;
  esac
  printf '%s\t%s\n' "$queued" "$epoch"
}

# closed_pr_facts prints one TSV line for a closed-unmerged PR:
#
#   <branch>       present | gone | unknown
#   <openIssues>   comma-separated issue numbers still OPEN, or "-"
#
# ONE GraphQL call answers both questions. `headRef` is null exactly when the
# branch has been deleted, and closingIssuesReferences carries each linked
# issue's state -- so this replaces what would otherwise be a REST branch probe
# plus an issue lookup per PR. Neither field exists on `gh pr list --json` or
# `gh pr view --json` in gh 2.45.0, which is why this is GraphQL and not
# another --json column.
#
# Empty lists are emitted as "-" and never as "", for the IFS-whitespace reason
# open_prs documents: an empty middle field shifts every later column left.
closed_pr_facts() {
  local num="$1" out branch open_issues
  out=$(gh api graphql -f query="{repository(owner:\"${REPO%%/*}\",name:\"${REPO##*/}\"){pullRequest(number:${num}){headRef{name} closingIssuesReferences(first:20){nodes{number state}}}}}" \
    --jq '.data.repository.pullRequest
          | [ (if .headRef == null then "gone" else "present" end)
            , ([.closingIssuesReferences.nodes[]? | select(.state == "OPEN") | .number | tostring] | join(","))
            ] | @tsv' 2>/dev/null)

  IFS=$'\t' read -r branch open_issues <<<"$out"

  case "${branch:-}" in
    present | gone) ;;
    *) branch="unknown" ;;
  esac
  [ -z "${open_issues:-}" ] && open_issues="-"
  printf '%s\t%s\n' "$branch" "$open_issues"
}

# classify_closed returns the bucket for one closed-unmerged PR.
#
# LOST-WORK is the finding, and like STALLED it is reached only when every
# input is positively known: the branch definitely still exists AND at least
# one linked issue is definitely still open. A failed lookup lands in UNKNOWN,
# never in a finding and never in silence.
classify_closed() {
  local branch="$1" open_issues="$2"

  case "$branch" in
    present) ;;
    gone) echo "BRANCH-GONE"; return ;;
    *) echo "UNKNOWN"; return ;;
  esac

  # No open linked issue: either the issue was closed anyway (the work landed
  # some other way, or was abandoned deliberately), or the PR never carried a
  # "Closes #N" link. Neither is the loss this sweep exists to catch, and
  # neither is a failure -- "-" is what closed_pr_facts emits for both.
  [ "$open_issues" = "-" ] && { echo "NO-OPEN-ISSUE"; return; }

  echo "LOST-WORK"
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

# check_state takes the head SHA rather than a PR number: open_prs already
# carries it, so the `gh pr view` this used to spend is gone (memql#2887).
# That is a third of the tool's per-PR budget -- measured at ~2.3s/PR, so ~4
# minutes per run at the 100-PR page ceiling, for a tool #2833 wants on a loop.
check_state() {
  local sha="$1" runs
  # "-" is open_prs' placeholder for a missing headRefOid, and it must fail
  # this guard exactly as "" does -- otherwise it reaches the API as a literal
  # ref name and the 404 becomes UNKNOWN by a longer route.
  { [ -z "$sha" ] || [ "$sha" = "-" ]; } && { echo "unknown"; return; }

  runs=$(gh api --paginate "repos/$REPO/commits/$sha/check-runs" --jq '.check_runs[] | (.conclusion // .status)' 2>/dev/null)
  if [ $? -ne 0 ]; then
    echo "unknown"
    return
  fi
  collapse_runs "$runs"
}

# idle_minutes prints whole minutes between an epoch and now, or `unknown` for
# an empty, non-numeric or FUTURE value.
#
# The future guard is not theoretical now that the clock is a commit date this
# script does not control: GIT_COMMITTER_DATE, a skewed runner, or a hand-set
# date puts it ahead of now and `(now - then)/60` goes negative. A negative
# value already reached UNKNOWN via classify's digit check -- the leading `-`
# is not a digit -- but only by accident, and only as long as that check keeps
# its exact shape. Refusing it here says so on purpose.
idle_minutes() {
  local epoch="$1" now
  case "$epoch" in
    '' | *[!0-9]*) echo "unknown"; return ;;
  esac
  now=$(date -u +%s)
  [ "$epoch" -gt "$now" ] && { echo "unknown"; return; }
  echo $(( (now - epoch) / 60 ))
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

  printf '%-7s %-9s %-8s %-8s %-7s  %s\n' "PR" "STATE" "IDLE" "TOUCHED" "CHECKS" "TITLE"
  printf '%-7s %-9s %-8s %-8s %-7s  %s\n' "-------" "---------" "--------" "--------" "-------" "-----"

  local stalled=0 total=0 unknown=0
  while IFS=$'\t' read -r num draft merge_state updated_epoch head_sha title; do
    [ -z "${num:-}" ] && continue
    total=$((total + 1))
    local facts queued head_epoch checks idle touched bucket
    facts=$(pr_liveness "$num" "$head_sha")
    IFS=$'\t' read -r queued head_epoch <<<"$facts"
    checks=$(check_state "$head_sha")
    idle=$(idle_minutes "$head_epoch")       # the gate: head-SHA stability
    touched=$(idle_minutes "$updated_epoch") # display only: last activity of any kind
    bucket=$(classify "$draft" "$merge_state" "$queued" "$checks" "$idle")
    [ "$bucket" = "STALLED" ] && stalled=$((stalled + 1))
    [ "$bucket" = "UNKNOWN" ] && unknown=$((unknown + 1))
    local idle_col="${idle}m" touched_col="${touched}m"
    [ "$idle" = "unknown" ] && idle_col="?"
    [ "$touched" = "unknown" ] && touched_col="?"
    printf '#%-6s %-9s %-8s %-8s %-7s  %s\n' "$num" "$bucket" "$idle_col" "$touched_col" "$checks" "${title:0:48}"
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
    echo "${stalled} of ${total} open PR(s) are STALLED -- green, mergeable, and no new commit for ${IDLE_MINUTES}+ minutes."
    echo "IDLE is head-commit age; TOUCHED is last activity of any kind. A comment or review moves"
    echo "TOUCHED and not IDLE, which is the point (memql#2887)."
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

  report_closed_unmerged
}

# report_closed_unmerged is the second sweep (memql#2887): PRs closed WITHOUT
# merging whose branch still exists and whose linked issue is still open.
#
# It prints its own section rather than folding into the table above, because
# the populations answer different questions -- the first is "what is ready and
# waiting", this is "what was thrown away". A reader scanning for one must not
# have to filter out the other.
#
# A failure here degrades to a loud note, not to an exit: the open-PR report
# above is already printed and is still true. Exiting non-zero would discard a
# good report because a secondary sweep failed.
report_closed_unmerged() {
  local rows
  rows=$(closed_unmerged_prs)
  if [ $? -ne 0 ]; then
    echo
    echo "WARNING: could not list closed-unmerged PRs for $REPO; that half of this report is MISSING," >&2
    echo "         not clean. The open-PR table above is unaffected." >&2
    return
  fi

  # No candidates at all is the common case and deserves no section.
  local candidates
  candidates=$(grep -c . <<<"$rows")
  [ "$candidates" -eq 0 ] && return

  if [ "$candidates" -ge "$CLOSED_PAGE_LIMIT" ]; then
    echo
    echo "WARNING: hit the ${CLOSED_PAGE_LIMIT}-PR cap on the closed-unmerged sweep; older ones were NOT examined." >&2
  fi

  local lost=0 examined=0 unknown=0 printed=0
  while IFS=$'\t' read -r num branch_name closed_epoch title; do
    [ -z "${num:-}" ] && continue
    examined=$((examined + 1))
    local facts branch open_issues bucket age
    facts=$(closed_pr_facts "$num")
    IFS=$'\t' read -r branch open_issues <<<"$facts"
    bucket=$(classify_closed "$branch" "$open_issues")
    [ "$bucket" = "UNKNOWN" ] && unknown=$((unknown + 1))

    # Only the finding and the unresolved are worth a reader's attention. A
    # closed PR whose branch is gone, or whose issue is closed too, is the
    # ordinary end of a PR's life and printing it would bury the two rows that
    # matter under every abandoned experiment in the repo's history.
    case "$bucket" in
      LOST-WORK | UNKNOWN) ;;
      *) continue ;;
    esac

    if [ "$printed" -eq 0 ]; then
      echo
      echo "CLOSED WITHOUT MERGING -- branch still exists, issue still open:"
      printf '%-7s %-11s %-9s %-14s  %s\n' "PR" "STATE" "CLOSED" "OPEN ISSUES" "BRANCH"
      printf '%-7s %-11s %-9s %-14s  %s\n' "-------" "-----------" "---------" "--------------" "------"
      printed=1
    fi
    [ "$bucket" = "LOST-WORK" ] && lost=$((lost + 1))
    age=$(idle_minutes "$closed_epoch")
    local age_col="${age}m"
    [ "$age" = "unknown" ] && age_col="?"
    printf '#%-6s %-11s %-9s %-14s  %s\n' "$num" "$bucket" "$age_col" "$open_issues" "${branch_name:0:40}"
  done <<<"$rows"

  [ "$printed" -eq 0 ] && return

  echo
  if [ "$lost" -gt 0 ]; then
    echo "${lost} of ${examined} closed-unmerged PR(s) may have LOST WORK: closed without merging, branch still"
    echo "on the remote, linked issue still open. The issue has silently returned to the pool, so the next"
    echo "session will rebuild what is already written on that branch. Check the branch before it is pruned."
  fi
  if [ "$unknown" -gt 0 ]; then
    echo "${unknown} closed-unmerged PR(s) are UNKNOWN -- a lookup failed. Deliberately not reported as"
    echo "findings; re-run to resolve."
  fi
}

usage() {
  cat <<'USAGE'
stalled-prs.sh -- report PRs nobody is advancing.

Two sweeps:
  STALLED    open PRs that are green, mergeable and un-enqueued
  LOST-WORK  PRs closed WITHOUT merging whose branch still exists and whose
             linked issue is still open -- work that was written and dropped

READ-ONLY. Makes no GitHub mutation of any kind; that is enforced by
TestStalledPRs_MakesNoMutatingCall, which runs the script against a stub `gh`
that refuses anything outside a read allowlist.

Usage:
  bash scripts/dev/stalled-prs.sh          # or: make prs-stalled
  IDLE_MINUTES=15 bash scripts/dev/stalled-prs.sh

Environment:
  REPO           owner/name to report on (default: znasllc-io/memql)
  IDLE_MINUTES   how long a green PR's HEAD COMMIT must sit unchanged before it
                 counts as STALLED (default: 45). Comments, labels and reviews
                 do not reset it; only a new head SHA does.

Columns:
  IDLE     head-commit age -- the gate
  TOUCHED  last activity of any kind -- display only, gates nothing

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
