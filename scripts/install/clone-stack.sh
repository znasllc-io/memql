#!/usr/bin/env bash
#
# scripts/install/clone-stack.sh
# ==============================
#
# Capability: install.cloneStack -- fetch the memQL stack at a release tag into
# ~/.memql/src, the checkout the rest of the install substrate then runs.
#
# WHY A TAG, AND ONLY A TAG
#
# scripts/k3d/up.sh defaults its ArgoCD targetRevision to whatever branch the
# operator happens to be sitting on. That is right for repo development and
# wrong for an install. A branch MOVES: two installs of "the same version" a
# week apart are not the same install, and afterwards nobody can say what a
# given machine is actually running. A tag is the only ref that makes "what is
# installed here" a durable answer, so a branch ref is rejected outright
# (exit 2) rather than quietly accepted -- an installer that silently pins to a
# moving target is worse than one that refuses.
#
# The check is against the REMOTE: the ref must resolve under refs/tags/. A
# name that is a branch gets an error saying so; a name that is neither is
# rejected as unknown.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/clone-stack.sh --tag=v1.4.0
#   scripts/install/clone-stack.sh --tag=v1.4.0 --dest=/opt/memql/src
#   scripts/install/clone-stack.sh --repo=https://github.com/acme/fork.git --tag=v2.0.0
#   scripts/install/clone-stack.sh --print-spec
#
# Idempotent: re-running at the tag already checked out is changed=false;
# naming a different tag fetches and checks it out in place.
#
# Exit codes:
#   0 ok | 2 bad param (missing tag, a branch, an unknown ref)
#   3 refused (the destination holds something that is not our checkout)
#   4 prerequisite missing (git not installed)
#   5 operation failed (remote unreachable, clone/fetch/checkout failed)
#
# Refs: #3363 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.cloneStack" "Fetch the memQL stack at a release tag into a pinned local checkout."
cap_spec_param "repo"  "git repository URL (default: the memQL engine)"
cap_spec_param "tag"   "release TAG to check out (required; a branch is rejected)"
cap_spec_param "dest"  "checkout directory (default: ~/.memql/src)"
cap_spec_param "depth" "clone/fetch depth, 0 for a full history (default: 1)"
cap_spec_param "git"   "path to the git binary (default: resolved from PATH)"

readonly DEFAULT_REPO="https://github.com/znasllc-io/memql.git"
readonly DEFAULT_DEST="${HOME}/.memql/src"
readonly DEFAULT_DEPTH="1"

# An installer must never sit waiting for a credential prompt -- a private repo
# or a typo'd URL has to fail, not hang (contract rule 3).
export GIT_TERMINAL_PROMPT=0

#=============================================================================
# PREREQUISITES
#=============================================================================

# NOTE: the resolve_* helpers set a global rather than printing their result.
# cap_fail inside a "$(...)" substitution would emit its envelope into the
# capture instead of onto stdout, and the caller would abort into the trap's
# generic "aborted without an explicit result" -- an honest exit code carrying
# a useless message. Anything that can fail runs in the parent shell.
GIT_BIN=""
WANT_COMMIT=""

function resolve_git() {
    local candidate="$1"
    if ! GIT_BIN="$(command -v "$candidate" 2>/dev/null)" || [[ -z "$GIT_BIN" ]]; then
        cap_fail 4 "git not found (looked for '${candidate}'); install git first"
    fi
}

#=============================================================================
# REF RESOLUTION -- the tag-not-branch gate
#=============================================================================

# resolve_tag_commit <git> <repo> <tag> -- sets WANT_COMMIT to the commit the
# tag points at. Fails 2 when the ref is a branch or does not exist, 5 when the
# remote cannot be reached at all (an unreachable remote is an operational
# problem, not a bad parameter, and the exit code has to tell them apart).
function resolve_tag_commit() {
    local git_bin="$1" repo="$2" tag="$3" refs sha ref found=""
    WANT_COMMIT=""

    # The pattern carries a trailing '*' so the PEELED entry
    # (refs/tags/<tag>^{}) comes back too -- an exact pattern filters it out,
    # and without it an annotated tag resolves to its tag OBJECT. The loop
    # below only accepts exact ref names, so the wildcard cannot drag in a
    # neighbouring tag like <tag>-rc1.
    if ! refs="$("$git_bin" ls-remote --tags "$repo" "refs/tags/${tag}*" 2>/dev/null)"; then
        cap_fail 5 "cannot reach the repository '${repo}'"
    fi

    while IFS=$'\t' read -r sha ref; do
        [[ -z "${sha:-}" ]] && continue
        # The peeled entry (^{}) is the commit an ANNOTATED tag points at;
        # prefer it, or comparing HEAD against the tag OBJECT would report a
        # difference on every run forever.
        if [[ "$ref" == "refs/tags/${tag}^{}" ]]; then
            found="$sha"
            break
        fi
        if [[ "$ref" == "refs/tags/${tag}" && -z "$found" ]]; then
            found="$sha"
        fi
    done <<< "$refs"

    if [[ -n "$found" ]]; then
        WANT_COMMIT="$found"
        return
    fi

    # Not a tag. Is it a branch? Say so explicitly -- "unknown ref" would send
    # the operator hunting for a typo when the real answer is "branches are not
    # versions".
    local heads
    heads="$("$git_bin" ls-remote --heads "$repo" "refs/heads/${tag}" 2>/dev/null || true)"
    if [[ -n "$heads" ]]; then
        cap_fail 2 "'${tag}' is a branch, not a tag; an install must pin to a tag (a branch moves, so two installs of the same 'version' would differ). Pass a release tag, e.g. --tag=v1.4.0"
    fi
    cap_fail 2 "'${tag}' is not a tag in ${repo}"
}

#=============================================================================
# DESTINATION
#=============================================================================

function dir_is_empty() {
    [[ -z "$(ls -A "$1" 2>/dev/null)" ]]
}

# classify_dest <dest> -- prints: absent | empty | checkout | occupied
function classify_dest() {
    local dest="$1"
    if [[ ! -e "$dest" ]]; then
        printf 'absent'
    elif [[ ! -d "$dest" ]]; then
        printf 'occupied'
    elif [[ -e "${dest}/.git" ]]; then
        printf 'checkout'
    elif dir_is_empty "$dest"; then
        printf 'empty'
    else
        printf 'occupied'
    fi
}

#=============================================================================
# CLONE / UPDATE
#=============================================================================

function clone_at_tag() {
    local git_bin="$1" repo="$2" tag="$3" dest="$4" depth="$5"
    local -a args=(clone --branch "$tag")
    if [[ "$depth" != "0" ]]; then
        args+=(--depth "$depth" --single-branch)
    fi
    args+=("$repo" "$dest")

    cap_step "cloning ${repo} at ${tag} into ${dest}"
    mkdir -p "$(dirname "$dest")"
    if ! "$git_bin" "${args[@]}" >&2; then
        cap_fail 5 "git clone of ${repo} at ${tag} failed"
    fi
}

function update_to_tag() {
    local git_bin="$1" repo="$2" tag="$3" dest="$4" depth="$5" current_origin

    current_origin="$("$git_bin" -C "$dest" remote get-url origin 2>/dev/null || true)"
    if [[ "$current_origin" != "$repo" ]]; then
        cap_info "origin was '${current_origin:-<none>}'; pointing it at ${repo}"
        if ! "$git_bin" -C "$dest" remote set-url origin "$repo" >&2; then
            cap_fail 5 "could not point origin at ${repo}"
        fi
    fi

    cap_step "fetching ${tag} into the existing checkout at ${dest}"
    local -a args=(-C "$dest" fetch)
    if [[ "$depth" != "0" ]]; then
        args+=(--depth "$depth")
    fi
    # Explicit refspec, no leading '+': a tag that has MOVED upstream fails
    # here instead of being silently overwritten. Release tags are immutable;
    # one that is not is worth stopping for.
    args+=(origin "refs/tags/${tag}:refs/tags/${tag}")
    if ! "$git_bin" "${args[@]}" >&2; then
        cap_fail 5 "could not fetch tag ${tag} from ${repo}"
    fi
    if ! "$git_bin" -C "$dest" checkout --detach "refs/tags/${tag}" >&2; then
        cap_fail 5 "could not check out tag ${tag} in ${dest}"
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local repo tag dest depth git_candidate
    repo="$(cap_param repo  "$DEFAULT_REPO")"
    tag="$(cap_param tag    "")"
    dest="$(cap_param dest  "$DEFAULT_DEST")"
    depth="$(cap_param depth "$DEFAULT_DEPTH")"
    git_candidate="$(cap_param git "git")"

    cap_require repo "$repo"
    cap_require tag  "$tag"
    cap_require dest "$dest"
    if [[ ! "$depth" =~ ^[0-9]+$ ]]; then
        cap_fail 2 "invalid --depth '${depth}': expected a non-negative integer (0 = full history)"
    fi
    if [[ ! "$tag" =~ ^[A-Za-z0-9][A-Za-z0-9._/+-]*$ ]]; then
        cap_fail 2 "invalid --tag '${tag}'"
    fi

    resolve_git "$git_candidate"
    local git_bin="$GIT_BIN"

    # Resolve (and gate) the ref BEFORE touching the destination: a rejected
    # ref must leave no checkout behind.
    resolve_tag_commit "$git_bin" "$repo" "$tag"
    local want_commit="$WANT_COMMIT"
    cap_info "${tag} resolves to ${want_commit}"

    local state cloned=false updated=false
    state="$(classify_dest "$dest")"
    case "$state" in
        occupied)
            cap_fail 3 "refusing to overwrite ${dest}: it exists and is not a memQL checkout; move it aside or pass --dest"
            ;;
        absent|empty)
            clone_at_tag "$git_bin" "$repo" "$tag" "$dest" "$depth"
            cloned=true
            cap_changed
            ;;
        checkout)
            local head
            head="$("$git_bin" -C "$dest" rev-parse HEAD 2>/dev/null || true)"
            if [[ "$head" == "$want_commit" ]]; then
                cap_info "${dest} is already at ${tag} (${want_commit}) -- no change."
            else
                update_to_tag "$git_bin" "$repo" "$tag" "$dest" "$depth"
                updated=true
                cap_changed
            fi
            ;;
    esac

    local commit
    commit="$("$git_bin" -C "$dest" rev-parse HEAD 2>/dev/null || true)"
    if [[ "$commit" != "$want_commit" ]]; then
        cap_fail 5 "checkout landed on ${commit:-<nothing>}, expected ${tag} (${want_commit})"
    fi

    cap_info "Done. ${dest} is pinned at ${tag}."
    cap_result_set     repo    "$repo"
    cap_result_set     tag     "$tag"
    cap_result_set     dest    "$dest"
    cap_result_set     commit  "$commit"
    cap_result_set_raw cloned  "$cloned"
    cap_result_set_raw updated "$updated"
    cap_ok
}

main "$@"
