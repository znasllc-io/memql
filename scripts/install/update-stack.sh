#!/usr/bin/env bash
#
# scripts/install/update-stack.sh
# ===============================
#
# Capability: install.updateStack -- move an existing MemQL checkout forward to
# the tip of its branch, or refuse and say exactly which situation the operator
# is in. It builds nothing; the step after it does that.
#
# THE ONE PROPERTY EVERYTHING ELSE IS ARRANGED AROUND: a refusal changes
# nothing on disk. The person who wants this button is a developer with
# uncommitted work, and they are also the person a careless implementation
# hurts. There is exactly one exception, a merge that conflicts, and it is
# announced rather than discovered.
#
# WHAT IT DOES, and it is deliberately a small table:
#
#   already at the tip                      upToDate      nothing done
#   behind, edits do not overlap            fastForward   edits carried across
#   behind, edits overlap what is arriving  REFUSED       nothing done
#   diverged, --strategy=fastForward        REFUSED       nothing done
#   diverged, --strategy=merge, clean tree  merged        merge commit written
#   diverged, --strategy=merge, dirty tree  REFUSED       nothing done
#   merge conflicts                         REFUSED       conflict left in place
#   a merge or rebase already in progress   REFUSED       nothing done
#
# THE SAFETY IS GIT'S, NOT OURS. `git merge --ff-only` and `git checkout
# --detach` both carry uncommitted edits across a fast-forward, and both refuse
# atomically -- changing nothing -- when those edits overlap what is arriving.
# What this script adds is computing the overlap FIRST, so the refusal can name
# the files and the editor's checklist can PREDICT it instead of reporting it
# after the fact.
#
# A CONFLICT IS NOT ABORTED. VS Code has a merge editor and the developer is
# sitting in it; `git merge --abort` from under them throws away work they can
# see. The refusal names the conflicted paths and both ways out.
#
# A SHALLOW CHECKOUT IS DEEPENED, ONCE, AND IT IS SAID OUT LOUD. The wizard
# clones with `--depth 1 --single-branch`. At depth 1 the local HEAD and a
# freshly fetched tip share no ancestry in the object store, so git cannot
# prove a fast-forward and EVERY update would report a divergence that is not
# real. Deepening is not an optimisation here, it is what makes the answer
# correct.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/update-stack.sh
#   scripts/install/update-stack.sh --dest=/opt/memql/src --branch=main
#   scripts/install/update-stack.sh --strategy=merge
#   scripts/install/update-stack.sh --print-spec
#
# Exit codes:
#   0 ok | 2 bad param (unknown strategy, no branch to update, no such remote)
#   3 refused (local edits, divergence, conflicts, a merge already running)
#   4 prerequisite missing (git absent; no git identity for a merge commit)
#   5 operation failed (remote unreachable, fetch/merge failed unexpectedly)
#
# Refs: #4577 #4573 #4246 #3901 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.updateStack" "Bring an existing MemQL checkout up to date with its branch, or say why it cannot be."
cap_spec_param "dest"     "checkout directory (default: ~/.memql/src)"
cap_spec_param "remote"   "git remote to fetch from (default: origin)"
cap_spec_param "branch"   "branch to update to (default: the branch the checkout is on; required when it is on none)"
cap_spec_param "strategy" "fastForward (default) or merge -- merge is what a checkout with its own commits needs, and it needs a clean tree"
cap_spec_param "git"      "path to the git binary (default: resolved from PATH)"

readonly DEFAULT_DEST="${HOME}/.memql/src"
readonly DEFAULT_REMOTE="origin"
readonly STRATEGY_FF="fastForward"
readonly STRATEGY_MERGE="merge"

# An installer must never sit waiting for a credential prompt -- a private repo
# or an expired token has to fail, not hang (contract rule 3).
export GIT_TERMINAL_PROMPT=0

GIT_BIN=""

#=============================================================================
# PREREQUISITES AND STATE
#=============================================================================

# NOTE on the resolve_* shape, inherited from clone-stack.sh: these set a global
# rather than printing. cap_fail inside a "$(...)" would emit its envelope into
# the capture instead of onto stdout, and the caller would abort into the trap's
# generic "aborted without an explicit result" -- an honest exit code carrying a
# useless message.
function resolve_git() {
    local candidate="$1"
    if ! GIT_BIN="$(command -v "$candidate" 2>/dev/null)" || [[ -z "$GIT_BIN" ]]; then
        cap_fail 4 "git not found (looked for '${candidate}'); install git first"
    fi
}

function git_in() {
    "$GIT_BIN" -C "$DEST" "$@"
}

# in_progress_operation -- prints the name of an unfinished git operation, or
# nothing.
#
# CHECKED BEFORE ANYTHING ELSE, because every other read in this script is a
# lie during one: HEAD points at the pre-merge commit, `status --porcelain`
# reports conflict markers as ordinary modifications, and a fast-forward
# attempted on top would fail with a message about the wrong thing entirely.
function in_progress_operation() {
    local dir
    dir="$(git_in rev-parse --git-dir 2>/dev/null || true)"
    if [[ -z "$dir" ]]; then
        printf ''
        return 0
    fi
    # rev-parse returns a path relative to the checkout unless it is elsewhere.
    case "$dir" in
        /*) ;;
        *) dir="${DEST}/${dir}" ;;
    esac
    if [[ -e "${dir}/MERGE_HEAD" ]]; then
        printf 'a merge'
    elif [[ -d "${dir}/rebase-merge" || -d "${dir}/rebase-apply" ]]; then
        printf 'a rebase'
    elif [[ -e "${dir}/CHERRY_PICK_HEAD" ]]; then
        printf 'a cherry-pick'
    elif [[ -e "${dir}/REVERT_HEAD" ]]; then
        printf 'a revert'
    else
        printf ''
    fi
}

# resolve_branch -- sets BRANCH and DETACHED from --branch and the checkout.
#
# A DETACHED HEAD IS ORDINARY HERE, not a fault. A release install checks out a
# tag detached, and a repair of a branch install reconciles onto an exact commit
# and detaches too (clone-stack.sh's fetch_and_detach). So the branch is asked
# for rather than assumed -- and when neither the caller nor the checkout can
# name one, that is a missing parameter and says so.
BRANCH=""
DETACHED=false
function resolve_branch() {
    local requested="$1" current
    current="$(git_in symbolic-ref --short -q HEAD 2>/dev/null || true)"
    if [[ -z "$current" ]]; then
        DETACHED=true
    fi
    if [[ -n "$requested" ]]; then
        BRANCH="$requested"
        return 0
    fi
    if [[ -n "$current" ]]; then
        BRANCH="$current"
        return 0
    fi
    cap_fail 2 "${DEST} is not on a branch (its HEAD is at a specific commit, which is what a release install and a repair both leave behind), so there is no branch to bring it up to date with. Pass --branch=<name>, e.g. --branch=main"
}

#=============================================================================
# FETCH
#=============================================================================

# deepen_if_shallow -- turn a `--depth 1` clone into one with history.
#
# See the header: at depth 1 the local commit and the fetched tip have no common
# ancestor in the object store, so `merge-base --is-ancestor` answers no and a
# plain fast-forward looks like a divergence. Announced rather than silent
# because it is the slowest thing this script ever does and it happens exactly
# once per checkout.
UNSHALLOWED=false
function deepen_if_shallow() {
    local remote="$1" shallow
    shallow="$(git_in rev-parse --is-shallow-repository 2>/dev/null || echo false)"
    if [[ "$shallow" != "true" ]]; then
        return 0
    fi
    cap_step "this checkout was cloned without its history; fetching the rest so an update can be applied"
    if ! git_in fetch --unshallow "$remote" >&2; then
        cap_fail 5 "could not fetch the history of ${DEST} from ${remote}"
    fi
    UNSHALLOWED=true
    cap_changed
}

# fetch_tip <remote> <branch> -- sets TARGET to the commit the branch names now.
#
# FETCH_HEAD rather than the remote-tracking ref, which is what `git pull` reads
# for the same reason: a checkout cloned `--single-branch` tracks one branch,
# and asking for a different one still has to work.
TARGET=""
function fetch_tip() {
    local remote="$1" branch="$2"
    cap_step "fetching ${branch} from ${remote}"
    if ! git_in fetch "$remote" "$branch" >&2; then
        cap_fail 5 "could not fetch ${branch} from ${remote}; check the remote is reachable and the branch exists"
    fi
    TARGET="$(git_in rev-parse FETCH_HEAD 2>/dev/null || true)"
    if [[ -z "$TARGET" ]]; then
        cap_fail 5 "fetched ${branch} from ${remote} but could not resolve what it points at"
    fi
}

#=============================================================================
# THE OVERLAP -- what makes the refusal name files instead of quoting git
#=============================================================================

# overlapping_paths <target> -- prints the paths that both this checkout has
# changed and the update would change, one per line.
#
# Tracked modifications AND untracked files, because they block for different
# reasons and both change nothing when they do: an incoming commit that MODIFIES
# a file the operator has edited, and one that ADDS a file the operator already
# has sitting there untracked.
#
# `comm` needs sorted input and both sides are already line-per-path, so this is
# a set intersection and not a loop.
function overlapping_paths() {
    local target="$1" mine theirs
    mine="$( { git_in diff --name-only HEAD 2>/dev/null || true; \
               git_in ls-files --others --exclude-standard 2>/dev/null || true; } | sort -u )"
    theirs="$(git_in diff --name-only "HEAD..${target}" 2>/dev/null | sort -u || true)"
    if [[ -z "$mine" || -z "$theirs" ]]; then
        printf ''
        return 0
    fi
    # `|| true` because this runs inside a command substitution under `set -e`:
    # a non-zero here would abort the script BEFORE any envelope is written, and
    # the contract's EXIT trap can only report the catch-all -- an honest exit
    # code carrying a useless message (the memql#4458 failure, in a new place).
    comm -12 <(printf '%s\n' "$mine") <(printf '%s\n' "$theirs") || true
}

# join_paths -- a space-separated list for the result envelope, capped so a
# refusal naming four hundred files is still a sentence.
#
# The CAP IS REPORTED (`... and N more`) rather than applied silently: a list
# that stops without saying so reads as a complete list, and an operator who
# fixes the eight files named would run again and be refused by a ninth.
function join_paths() {
    local -a all=()
    local line
    while IFS= read -r line; do
        [[ -n "$line" ]] && all+=("$line")
    done
    local n=${#all[@]}
    if (( n == 0 )); then
        printf ''
    elif (( n <= 8 )); then
        printf '%s' "${all[*]}"
    else
        printf '%s and %d more' "${all[*]:0:8}" "$(( n - 8 ))"
    fi
}

# count_commits <range> -- how many commits are in a range, as a NUMBER.
#
# The digit check is not defensiveness about git, which always prints one. It is
# about what these values are used for: they go onto the envelope through
# `cap_result_set_raw`, which writes them UNQUOTED, so anything that is not a
# number produces malformed JSON -- and a malformed envelope fails at the
# runner's parser with a message about JSON rather than about the checkout.
function count_commits() {
    local n
    n="$(git_in rev-list --count "$1" 2>/dev/null || true)"
    if [[ "$n" =~ ^[0-9]+$ ]]; then
        printf '%s' "$n"
    else
        printf '0'
    fi
}

#=============================================================================
# THE THREE OUTCOMES
#=============================================================================

function apply_fast_forward() {
    local target="$1" overlap
    overlap="$(overlapping_paths "$target" | join_paths)"
    if [[ -n "$overlap" ]]; then
        cap_result_set outcome "blockedByLocalEdits"
        cap_result_set files   "$overlap"
        cap_fail 3 "your changes to ${overlap} are to files this update also changes, so nothing was touched. Commit or set aside those changes and update again -- or build what you have now, without updating."
    fi

    if [[ "$DETACHED" == true ]]; then
        # The same fast-forward, spelled for a checkout that is not on a
        # branch. `checkout --detach` carries uncommitted edits across and
        # refuses atomically for the same overlaps `merge --ff-only` does, so
        # the two paths have identical safety and differ only in where HEAD
        # ends up.
        if ! git_in checkout --detach "$target" >&2; then
            cap_fail 5 "could not move ${DEST} onto ${target}; nothing was changed"
        fi
    else
        if ! git_in merge --ff-only "$target" >&2; then
            cap_fail 5 "could not bring ${DEST} up to date; nothing was changed"
        fi
    fi
    cap_result_set outcome "fastForward"
    cap_changed
}

# require_git_identity -- a merge writes a COMMIT, and git refuses to author one
# without a name and address. Checked before the merge rather than after,
# because the failure it produces otherwise arrives in the middle of a merge
# that has already touched the tree.
function require_git_identity() {
    local name email
    name="$(git_in config user.name 2>/dev/null || true)"
    email="$(git_in config user.email 2>/dev/null || true)"
    if [[ -z "$name" || -z "$email" ]]; then
        cap_fail 4 "combining these changes writes a commit, and git has no name or address to write it under. Set one first: git config --global user.name \"Your Name\" and git config --global user.email \"you@example.com\""
    fi
}

function apply_merge() {
    local target="$1" branch="$2" remote="$3" dirty conflicts

    dirty="$(git_in status --porcelain 2>/dev/null || true)"
    if [[ -n "$dirty" ]]; then
        # STRICTER THAN A FAST-FORWARD, deliberately. A fast-forward either
        # applies cleanly or does nothing; a merge can stop half-way, and a
        # half-finished merge sitting on top of uncommitted work is a state
        # nobody -- including this script on its next run -- can take apart.
        cap_result_set outcome "blockedByLocalEdits"
        cap_result_set files   "$(printf '%s\n' "$dirty" | cut -c4- | join_paths)"
        cap_fail 3 "combining ${remote}/${branch} with your own commits needs a settled starting point, and ${DEST} has uncommitted changes. Commit or set them aside and update again -- or build what you have now, without updating."
    fi

    require_git_identity

    cap_step "combining ${remote}/${branch} with the commits in ${DEST}"
    if git_in merge --no-edit "$target" >&2; then
        cap_result_set outcome "merged"
        cap_changed
        return 0
    fi

    conflicts="$(git_in diff --name-only --diff-filter=U 2>/dev/null | join_paths || true)"
    if [[ -z "$conflicts" ]]; then
        # It stopped for a reason that is not a conflict. Take the tree back --
        # here there is nothing to preserve, which is precisely the case the
        # conflict path is NOT.
        git_in merge --abort >&2 2>/dev/null || true
        cap_fail 5 "could not combine ${remote}/${branch} with ${DEST}; the tree was put back as it was"
    fi

    # LEFT IN PLACE, ON PURPOSE. See the header.
    cap_changed
    cap_result_set outcome   "blockedByConflict"
    cap_result_set conflicts "$conflicts"
    cap_fail 3 "${remote}/${branch} and your own changes both edited ${conflicts}, and only you can decide how they combine. Resolve them in your editor and commit, then update again. To undo the attempt instead, run: git -C ${DEST} merge --abort"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

DEST=""

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local remote requested_branch strategy git_candidate
    DEST="$(cap_param dest "$DEFAULT_DEST")"
    remote="$(cap_param remote "$DEFAULT_REMOTE")"
    requested_branch="$(cap_param branch "")"
    strategy="$(cap_param strategy "$STRATEGY_FF")"
    git_candidate="$(cap_param git "git")"

    cap_require dest "$DEST"
    cap_require remote "$remote"
    if [[ "$strategy" != "$STRATEGY_FF" && "$strategy" != "$STRATEGY_MERGE" ]]; then
        cap_fail 2 "unknown --strategy '${strategy}': expected ${STRATEGY_FF} or ${STRATEGY_MERGE}"
    fi

    resolve_git "$git_candidate"

    if [[ ! -d "$DEST" ]] || ! git_in rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        cap_fail 3 "${DEST} is not a checkout, so there is nothing to bring up to date"
    fi

    local running
    running="$(in_progress_operation)"
    if [[ -n "$running" ]]; then
        cap_result_set outcome "blockedByInProgress"
        cap_fail 3 "${running} is already under way in ${DEST}, and nothing was read or changed. Finish it or undo it first."
    fi

    resolve_branch "$requested_branch"
    if ! git_in remote get-url "$remote" >/dev/null 2>&1; then
        cap_fail 2 "${DEST} has no remote called '${remote}'; pass --remote=<name>"
    fi

    local before
    before="$(git_in rev-parse HEAD 2>/dev/null || true)"
    if [[ -z "$before" ]]; then
        cap_fail 3 "${DEST} has no commits yet, so there is nothing to bring up to date"
    fi

    # Everything the envelope carries about WHERE this ran is set before the
    # first decision, so a refusal reports it too. A failure envelope that names
    # only the reason leaves the operator guessing which checkout it was about.
    cap_result_set dest         "$DEST"
    cap_result_set remote       "$remote"
    cap_result_set branch       "$BRANCH"
    cap_result_set strategy     "$strategy"
    cap_result_set commitBefore "$before"
    cap_result_set_raw detached "$DETACHED"

    deepen_if_shallow "$remote"
    fetch_tip "$remote" "$BRANCH"
    cap_result_set_raw unshallowed "$UNSHALLOWED"
    cap_result_set target "$TARGET"

    local behind ahead
    behind="$(count_commits "HEAD..${TARGET}")"
    ahead="$(count_commits "${TARGET}..HEAD")"
    cap_result_set_raw behind "$behind"
    cap_result_set_raw ahead  "$ahead"

    if [[ "$TARGET" == "$before" ]]; then
        cap_info "${DEST} is already at ${remote}/${BRANCH} (${before})."
        cap_result_set outcome "upToDate"
    elif [[ "$ahead" == "0" ]]; then
        cap_info "${remote}/${BRANCH} is ${behind} commit(s) ahead; bringing ${DEST} up to date."
        apply_fast_forward "$TARGET"
    elif [[ "$strategy" == "$STRATEGY_MERGE" ]]; then
        cap_info "${DEST} has ${ahead} commit(s) of its own and ${remote}/${BRANCH} has ${behind}; combining them."
        apply_merge "$TARGET" "$BRANCH" "$remote"
    else
        cap_result_set outcome "blockedByDivergence"
        cap_fail 3 "${DEST} has ${ahead} commit(s) that ${remote}/${BRANCH} does not, and ${remote}/${BRANCH} has ${behind} that it does not, so it cannot simply be moved forward -- nothing was changed. Combine them instead, or build what you have now, without updating."
    fi

    local after
    after="$(git_in rev-parse HEAD 2>/dev/null || true)"
    if [[ -z "$after" ]]; then
        cap_fail 5 "${DEST} is in an unreadable state after the update"
    fi
    cap_result_set commitAfter "$after"
    cap_info "Done. ${DEST} is at ${after}."
    cap_ok
}

main "$@"
