#!/usr/bin/env bash
#
# merge-as-owner.sh
# =================
#
# Merge a pull request you authored, on a repository whose ruleset requires a
# CODE OWNER review -- keeping that requirement in force for everyone else.
#
# THE CONSTRAINT THIS EXISTS TO WORK WITH, stated once because it is the whole
# reason the script is not a settings change:
#
#   GitHub does not allow a pull request's AUTHOR to approve it. There is no
#   toggle for this at repository, ruleset, organization or enterprise level --
#   on your own pull request the Approve control is simply not rendered.
#
# So "require the owner's approval, and let the owner approve their own work"
# cannot be expressed as two settings. It is expressed as ONE setting plus a
# bypass:
#
#   require_code_owner_review: true      <- the requirement, kept
#   bypass_actors: admin, mode=pull_request  <- the owner proceeding on their own
#
# That is not a loophole. It is GitHub's supported way to say "this rule binds
# the team; the owner may proceed on their own judgement" -- which is exactly
# the policy asked for. Every other actor still needs a real code-owner review.
#
# WHAT THIS SCRIPT WILL NOT DO. It never weakens the ruleset. It does not touch
# require_code_owner_review, the required status checks, or the merge queue. If
# the bypass is missing it says so and stops, rather than adding permissions on
# your behalf -- granting a bypass is a decision, not a step.
#
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 failed

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_REPO="znasllc-io/memql"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 --pr=<number> [options]

Merge a pull request you authored on a repo that requires code-owner review,
using the admin bypass the ruleset already grants you.

Options:
    --pr=N            pull request number (required)
    --repo=OWNER/NAME repository (default: $DEFAULT_REPO)
    --check           report policy and PR readiness, merge nothing
    --strategy=S      merge | squash | rebase (default: merge)
    --help            this message

Examples:
    $0 --pr=4476
    $0 --pr=4476 --check
    $0 --pr=4476 --repo=znasllc-io/memql-znas --strategy=squash

Why the bypass and not a settings change:
    GitHub never lets an author approve their own pull request, at any
    configuration level. The bypass is the supported way to express
    "the requirement stands, and the owner may proceed on their own work".
EOF
}

function log_info()  { printf 'INFO:  %s\n'  "$*" >&2; }
function log_warn()  { printf 'WARN:  %s\n'  "$*" >&2; }
function log_error() { printf 'ERROR: %s\n'  "$*" >&2; }
function log_step()  { printf '\n==> %s\n'   "$*" >&2; }

function check_prerequisites() {
    command -v gh >/dev/null 2>&1 || { log_error "gh CLI is not installed"; exit 4; }
    command -v jq >/dev/null 2>&1 || { log_error "jq is not installed"; exit 4; }
    gh auth status >/dev/null 2>&1 || { log_error "gh is not authenticated -- run 'gh auth login'"; exit 4; }
}

function parse_arguments() {
    PR=""
    REPO="$DEFAULT_REPO"
    CHECK_ONLY=false
    STRATEGY="merge"

    while [[ $# -gt 0 ]]; do
        case $1 in
            --pr=*)       PR="${1#*=}"; shift ;;
            --repo=*)     REPO="${1#*=}"; shift ;;
            --strategy=*) STRATEGY="${1#*=}"; shift ;;
            --check)      CHECK_ONLY=true; shift ;;
            --help|-h)    show_help; exit 0 ;;
            *)            log_error "unknown option: $1"; show_help; exit 2 ;;
        esac
    done

    [[ -n "$PR" ]] || { log_error "--pr is required"; show_help; exit 2; }
    [[ "$PR" =~ ^[0-9]+$ ]] || { log_error "--pr must be a number, got $PR"; exit 2; }
    case "$STRATEGY" in
        merge|squash|rebase) ;;
        *) log_error "--strategy must be merge, squash or rebase"; exit 2 ;;
    esac
}

# report_policy prints what the ruleset actually requires, so the bypass is used
# with the requirement visible rather than assumed.
function report_policy() {
    log_step "Ruleset policy on ${REPO}"

    local rs
    rs="$(gh api "repos/${REPO}/rulesets" --jq '[.[]|select(.enforcement=="active")][0].id' 2>/dev/null || true)"
    if [[ -z "$rs" || "$rs" == "null" ]]; then
        log_warn "no active ruleset found -- nothing is being bypassed"
        HAS_BYPASS="unknown"
        # REQUIRED_KNOWN stays "no", so guard_readiness falls back to refusing
        # on ANY red check. See the note above the guard.
        return 0
    fi

    local full
    full="$(gh api "repos/${REPO}/rulesets/${rs}" 2>/dev/null)"

    printf '%s\n' "$full" | jq -r '
      "  code-owner review required : \((.rules[]|select(.type=="pull_request")|.parameters.require_code_owner_review) // false)",
      "  approvals required         : \((.rules[]|select(.type=="pull_request")|.parameters.required_approving_review_count) // 0)",
      "  required checks            : \([.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context]|join(", "))",
      "  merge queue                : \(if any(.rules[]; .type=="merge_queue") then "yes" else "no" end)"
    ' 2>/dev/null || log_warn "could not summarise the ruleset"

    # The contexts the ruleset actually REQUIRES, one per line. guard_readiness
    # intersects the failing rollup with this set; report_policy is the only
    # place that knows it, so it is captured here rather than re-fetched.
    #
    # REQUIRED_KNOWN is the fail-closed switch. An empty REQUIRED_CHECKS is
    # ambiguous -- it means EITHER "this ruleset requires no checks" OR "the
    # read failed / the shape changed" -- and an intersection against an
    # unknown set is empty either way, which would let the guard pass every
    # red build silently. So the guard only narrows to required checks when
    # the read demonstrably succeeded.
    if REQUIRED_CHECKS="$(printf '%s\n' "$full" | jq -er '
          [.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context] | .[]
        ' 2>/dev/null)"; then
        REQUIRED_KNOWN=yes
    else
        REQUIRED_CHECKS=""
        REQUIRED_KNOWN=no
        log_warn "could not read the ruleset's required checks -- every failing check will block"
    fi

    if printf '%s\n' "$full" | jq -e '[.bypass_actors[]?|select(.actor_type=="RepositoryRole")]|length > 0' >/dev/null 2>&1; then
        HAS_BYPASS=yes
        log_info "admin bypass IS configured -- the owner may proceed on their own pull request"
    else
        HAS_BYPASS=no
        log_warn "no admin bypass is configured on this ruleset"
    fi
}

function report_pr() {
    log_step "Pull request ${REPO}#${PR}"

    local j
    j="$(gh pr view "$PR" --repo "$REPO" --json state,title,author,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup 2>/dev/null)" \
        || { log_error "cannot read ${REPO}#${PR}"; exit 5; }

    PR_STATE="$(printf '%s' "$j" | jq -r .state)"
    MERGE_STATE="$(printf '%s' "$j" | jq -r .mergeStateStatus)"

    printf '%s\n' "$j" | jq -r '
      "  title    : \(.title)",
      "  author   : \(.author.login)",
      "  state    : \(.state)",
      "  mergeable: \(.mergeable)  (\(.mergeStateStatus))",
      "  review   : \(.reviewDecision // "none")",
      "  checks   : \([.statusCheckRollup[]?|select(.conclusion=="SUCCESS")]|length) passed, \([.statusCheckRollup[]?|select(.conclusion=="FAILURE" or .conclusion=="TIMED_OUT")]|length) failed, \([.statusCheckRollup[]?|select((.status//.state)=="QUEUED" or (.status//.state)=="IN_PROGRESS")]|length) pending"
    '

    FAILED="$(printf '%s' "$j" | jq '[.statusCheckRollup[]?|select(.conclusion=="FAILURE" or .conclusion=="TIMED_OUT")]|length')"
    PENDING="$(printf '%s' "$j" | jq '[.statusCheckRollup[]?|select((.status//.state)=="QUEUED" or (.status//.state)=="IN_PROGRESS")]|length')"

    # The same two counts, narrowed to the contexts the ruleset REQUIRES. A
    # check's rollup name is `.name` for a check-run and `.context` for a
    # commit status; the ruleset names it either way, so match on both.
    local req_json="[]"
    if [[ "${REQUIRED_KNOWN:-no}" == "yes" ]]; then
        req_json="$(printf '%s\n' "${REQUIRED_CHECKS}" | jq -R . | jq -sc 'map(select(length > 0))')"
    fi
    FAILED_REQUIRED="$(printf '%s' "$j" | jq --argjson req "$req_json" '
        [.statusCheckRollup[]?
         | select(.conclusion=="FAILURE" or .conclusion=="TIMED_OUT")
         | select(((.name//.context) as $n | $req | index($n)) != null)] | length')"
    PENDING_REQUIRED="$(printf '%s' "$j" | jq --argjson req "$req_json" '
        [.statusCheckRollup[]?
         | select((.status//.state)=="QUEUED" or (.status//.state)=="IN_PROGRESS")
         | select(((.name//.context) as $n | $req | index($n)) != null)] | length')"

    # Print every red check, marking which of them the ruleset requires -- the
    # distinction the guard now turns on, so it must be visible in the report
    # and not only in the refusal.
    if [[ "${FAILED:-0}" -gt 0 ]]; then
        printf '%s\n' "$j" | jq -r --argjson req "$req_json" '
            [.statusCheckRollup[]?
             | select(.conclusion=="FAILURE" or .conclusion=="TIMED_OUT")
             | (.name//.context) as $n
             | if ($req | index($n)) != null
               then "    FAILED (REQUIRED): \($n)"
               else "    failed (not required): \($n)" end] | .[]'
    fi
}

# guard_readiness refuses on the two conditions a bypass must never paper over.
# The bypass exists to skip a review that cannot be given; it is not a way past
# a red build.
#
# RED WHERE IT COUNTS (memql#5016). This counted EVERY failing check, including
# lanes the ruleset does not require -- and this repository has two that are
# red for reasons no pull request can fix: CodeQL's `Analyze (go)`, which
# crashes on a 2GiB query result above roughly 300 changed files and is red on
# pristine `main`, and `install-cluster-e2e`, which is documented as flaky and
# installs a PINNED RELEASED STACK rather than the branch under test.
#
# So the guard was strictest on exactly the pull requests it was written for --
# large refactors, removal epics, regenerations -- and it named no way out. A
# guard that cannot be satisfied is not a safety measure; it is an invitation
# to reach for `gh pr merge --admin`, which skips this script and its reporting
# entirely. Refusing on a red REQUIRED check and REPORTING the rest is what
# "never a red build" meant: the ruleset already decides which lanes gate a
# merge, and `ci-required` is an `if: always()` aggregate over all of them, so
# its own green is the statement that everything required has settled.
#
# When the required-check set could not be read, REQUIRED_KNOWN is "no" and
# this falls back to refusing on any red check. An intersection against an
# unknown set is empty, and an empty intersection would pass every red build.
function guard_readiness() {
    [[ "$PR_STATE" == "OPEN" ]] || { log_error "pull request is ${PR_STATE}, not OPEN"; exit 3; }

    local blocking_failed="${FAILED:-0}" blocking_pending="${PENDING:-0}" scope="check(s)"
    if [[ "${REQUIRED_KNOWN:-no}" == "yes" ]]; then
        blocking_failed="${FAILED_REQUIRED:-0}"
        blocking_pending="${PENDING_REQUIRED:-0}"
        scope="REQUIRED check(s)"
    fi

    if [[ "${blocking_failed}" -gt 0 ]]; then
        log_error "refusing: ${blocking_failed} ${scope} FAILED. The bypass skips a review, never a red build."
        exit 3
    fi
    if [[ "${blocking_pending}" -gt 0 ]]; then
        log_error "refusing: ${blocking_pending} ${scope} still running. Re-run when CI has settled."
        exit 3
    fi
    # Red lanes the ruleset does not require do not block, but they are never
    # silent: a merge that proceeded over one should say so in its own output.
    if [[ "${FAILED:-0}" -gt 0 ]]; then
        log_warn "proceeding over ${FAILED} failing check(s) the ruleset does not require -- listed above"
    fi
    if [[ "${PENDING:-0}" -gt 0 ]]; then
        log_warn "proceeding with ${PENDING} non-required check(s) still running"
    fi
    # A bypass is only NEEDED when something is actually blocking. A CLEAN pull
    # request merges through the ordinary path, and demanding a bypass for it
    # would refuse a merge that requires no special powers at all -- which is
    # how a safety check turns into an obstacle.
    if [[ "$MERGE_STATE" == "BLOCKED" && "${HAS_BYPASS:-no}" == "no" ]]; then
        log_error "refusing: this pull request is BLOCKED and the ruleset grants no admin bypass, so --admin would fail. Add one deliberately, or have a second code owner review."
        exit 3
    fi
}

function merge_pr() {
    log_step "Merging ${REPO}#${PR} (strategy: ${STRATEGY})"

    # Reach for the bypass only when the pull request is blocked. Passing
    # --admin to a mergeable PR would work, but it would also record every
    # ordinary merge as an override, which makes the genuine overrides
    # unfindable later.
    local -a extra=()
    if [[ "$MERGE_STATE" == "BLOCKED" ]]; then
        extra+=(--admin)
        log_info "pull request is BLOCKED; using the admin bypass. Code-owner review remains required for everyone else."
    else
        log_info "pull request is ${MERGE_STATE}; merging through the ordinary path, no bypass needed"
    fi

    if gh pr merge "$PR" --repo "$REPO" "${extra[@]+"${extra[@]}"}" "--${STRATEGY}" 2>&1; then
        log_info "merge command accepted"
    else
        log_error "merge failed -- see the output above"
        exit 5
    fi

    local state merged
    state="$(gh pr view "$PR" --repo "$REPO" --json state -q .state 2>/dev/null || echo unknown)"
    merged="$(gh pr view "$PR" --repo "$REPO" --json mergedAt -q '.mergedAt // "not merged"' 2>/dev/null || echo unknown)"
    log_step "Result"
    printf '  state : %s\n  merged: %s\n' "$state" "$merged"
    [[ "$state" == "MERGED" ]] || { log_warn "not merged yet -- if a merge queue is enabled it may still be enqueued"; exit 0; }
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    report_policy
    report_pr

    if [[ "$CHECK_ONLY" == true ]]; then
        log_step "--check given; nothing merged"
        exit 0
    fi

    guard_readiness
    merge_pr
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
