#!/usr/bin/env bash
#
# scripts/install/clone-stack.sh
# ==============================
#
# Capability: install.cloneStack -- fetch the MemQL stack at a release tag into
# ~/.memql/src, the checkout the rest of the install substrate then runs.
#
# WHY --tag REFUSES A BRANCH, AND WHY --branch EXISTS ANYWAY
#
# scripts/k3d/up.sh defaults its ArgoCD targetRevision to whatever branch the
# operator happens to be sitting on. That is right for repo development and
# wrong for an install. A branch MOVES: two installs of "the same version" a
# week apart are not the same install, and afterwards nobody can say what a
# given machine is actually running. So `--tag` rejects a branch outright
# (exit 2) -- an installer that SILENTLY pins to a moving target is worse than
# one that refuses, and that refusal is unchanged.
#
# The property being protected is not "never check out a branch". It is "what
# is installed on this machine must still be answerable a week later"
# (memql#3901). A branch checkout only makes that unanswerable when the ref is
# what gets recorded. Resolve the branch to a COMMIT, record the commit, and
# the property holds -- the cluster reports a specific SHA exactly as a tag
# install reports a specific tag.
#
# Hence three ways to say which code to check out, and exactly one may be given:
#
#   --tag=vX.Y.Z     a release tag. The ordinary install. A branch name here is
#                    still refused, with the message it always gave.
#   --branch=main    an EXPLICITLY LABELLED branch. Passing it here is the
#                    operator saying "I know this moves" -- which is the whole
#                    difference between this and the refusal above. Resolved to
#                    a commit before anything is written.
#   --commit=<sha>   a specific commit. This is what a REPAIR replays: replaying
#                    `--branch=main` would silently upgrade the cluster to
#                    wherever main is today, which is precisely the failure
#                    memql#3605 fixed for tags ("a repair returns a cluster to
#                    the state its receipt describes; it is not an upgrade").
#
# `result.commit` is set on every path and is the durable answer. `result.ref`
# and `result.refKind` say what was ASKED for, which is a different question and
# worth keeping separately.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/clone-stack.sh --tag=v1.4.0
#   scripts/install/clone-stack.sh --tag=v1.4.0 --dest=/opt/memql/src
#   scripts/install/clone-stack.sh --branch=main
#   scripts/install/clone-stack.sh --commit=8f2c1ab...
#   scripts/install/clone-stack.sh --repo=https://github.com/acme/fork.git --tag=v2.0.0
#   scripts/install/clone-stack.sh --print-spec
#
# Idempotent: re-running at the commit already checked out is changed=false;
# naming a different one fetches and checks it out in place.
#
# Exit codes:
#   0 ok | 2 bad param (no ref, more than one, a bare branch, an unknown ref)
#   3 refused (the destination holds something that is not our checkout)
#   4 prerequisite missing (git not installed)
#   5 operation failed (remote unreachable, clone/fetch/checkout failed)
#
# Refs: #4059 #3901 #3882 #3605 #3363 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.cloneStack" "Fetch the MemQL stack at a tag, a labelled branch, or an exact commit into a pinned local checkout."
cap_spec_param "repo"  "git repository URL (default: the MemQL engine)"
# DECLARED REQUIRED, and it stays that way even though --branch and --commit can
# stand in for it (memql#3568's property, memql#3901): the spec's contract is
# that a caller supplying everything marked required always succeeds, and for
# the ordinary install --tag IS the one. The two alternatives are optional
# because choosing one is a deliberate act, not something a caller discovers by
# reading the spec and finding a gap.
cap_spec_param_required "tag"   "release TAG to check out (a branch name here is rejected -- use --branch); replaced by --branch or --commit"
cap_spec_param "branch" "BRANCH to check out, explicitly labelled as one; resolved to a commit before anything is written. Mutually exclusive with --tag and --commit"
cap_spec_param "commit" "exact COMMIT sha to check out; what a repair replays so it cannot become an upgrade. Mutually exclusive with --tag and --branch"
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
REF_KIND=""
REF_NAME=""

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
        # The refusal stands; the guidance now names the deliberate way through
        # it (memql#3901). Telling an operator only "pass a release tag" was
        # complete when a branch install was impossible and is misleading now
        # that it is merely something they have to ask for by name.
        cap_fail 2 "'${tag}' is a branch, not a tag; an install must pin to a tag (a branch moves, so two installs of the same 'version' would differ). Pass a release tag, e.g. --tag=v1.4.0 -- or, if you mean it, --branch=${tag}, which resolves to a commit and records that instead of the ref"
    fi
    cap_fail 2 "'${tag}' is not a tag in ${repo}"
}

# resolve_branch_commit <git> <repo> <branch> -- sets WANT_COMMIT to the commit
# the branch head points at RIGHT NOW.
#
# The resolution is the entire point of allowing this at all. From here on the
# run behaves exactly like a commit install: the SHA is what is checked out,
# what is verified, and what lands on the result envelope for the receipt to
# record. Nothing downstream ever sees a moving ref.
function resolve_branch_commit() {
    local git_bin="$1" repo="$2" branch="$3" heads
    WANT_COMMIT=""

    if ! heads="$("$git_bin" ls-remote --heads "$repo" "refs/heads/${branch}" 2>/dev/null)"; then
        cap_fail 5 "cannot reach the repository '${repo}'"
    fi
    if [[ -z "$heads" ]]; then
        # Say which mistake it is. A tag passed to --branch is a swapped flag,
        # not a typo, and telling them to check the spelling sends them hunting
        # for something that is not wrong.
        local tags
        tags="$("$git_bin" ls-remote --tags "$repo" "refs/tags/${branch}" 2>/dev/null || true)"
        if [[ -n "$tags" ]]; then
            cap_fail 2 "'${branch}' is a tag, not a branch; pass it as --tag=${branch}"
        fi
        cap_fail 2 "'${branch}' is not a branch in ${repo}"
    fi

    WANT_COMMIT="$(printf '%s\n' "$heads" | head -n 1 | cut -f1)"
    if [[ -z "$WANT_COMMIT" ]]; then
        cap_fail 5 "could not resolve refs/heads/${branch} in ${repo}"
    fi
}

# resolve_commit <git> <repo> <sha> -- validates the sha and sets WANT_COMMIT.
#
# NOT verified against the remote first, deliberately. `ls-remote` lists refs,
# and the commit a repair replays is typically NOT at any ref any more -- that
# is the whole reason it was recorded as a SHA. The fetch below is the real
# existence check, and it fails 5 with git's own message, which names the commit.
function resolve_commit() {
    local sha="$3"
    WANT_COMMIT=""
    if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
        cap_fail 2 "invalid --commit '${sha}': expected a full 40-character lowercase hex sha (an abbreviated one cannot be fetched from a remote)"
    fi
    WANT_COMMIT="$sha"
}

# resolve_requested_ref <git> <repo> <tag> <branch> <commit>
#
# Enforces exactly-one-of and dispatches. Sets REF_KIND, REF_NAME and (via the
# resolvers) WANT_COMMIT. Runs BEFORE the destination is touched, so a rejected
# ref leaves no checkout behind -- the property the tag path already had.
function resolve_requested_ref() {
    local git_bin="$1" repo="$2" tag="$3" branch="$4" commit="$5"
    local -a given=()
    [[ -n "$tag"    ]] && given+=("--tag")
    [[ -n "$branch" ]] && given+=("--branch")
    [[ -n "$commit" ]] && given+=("--commit")

    if [[ ${#given[@]} -eq 0 ]]; then
        cap_fail 2 "missing required parameter: one of --tag=<release tag>, --branch=<branch>, or --commit=<sha>"
    fi
    if [[ ${#given[@]} -gt 1 ]]; then
        cap_fail 2 "pass exactly one of --tag / --branch / --commit; got ${given[*]}"
    fi

    if [[ -n "$tag" ]]; then
        if [[ ! "$tag" =~ ^[A-Za-z0-9][A-Za-z0-9._/+-]*$ ]]; then
            cap_fail 2 "invalid --tag '${tag}'"
        fi
        REF_KIND="tag"
        REF_NAME="$tag"
        resolve_tag_commit "$git_bin" "$repo" "$tag"
    elif [[ -n "$branch" ]]; then
        if [[ ! "$branch" =~ ^[A-Za-z0-9][A-Za-z0-9._/+-]*$ ]]; then
            cap_fail 2 "invalid --branch '${branch}'"
        fi
        REF_KIND="branch"
        REF_NAME="$branch"
        resolve_branch_commit "$git_bin" "$repo" "$branch"
    else
        REF_KIND="commit"
        REF_NAME="$commit"
        resolve_commit "$git_bin" "$repo" "$commit"
    fi
}

#=============================================================================
# DESTINATION
#=============================================================================

function dir_is_empty() {
    [[ -z "$(ls -A "$1" 2>/dev/null)" ]]
}

# worktree_is_materialized <git> <dest> -- is a tree actually checked out here,
# or is there only a .git directory?
#
# THE CHECK THIS SCRIPT WAS MISSING, and the reason it could report success over
# an empty directory (memql#4059). `git clone` writes objects, refs and HEAD
# BEFORE it materializes the working tree. The clone is the longest step in an
# install, so it is the step an interruption lands in, and what it leaves behind
# passes every ref-shaped check: .git exists, so classify_dest calls it a
# checkout; `rev-parse HEAD` is the wanted commit, so the idempotency branch
# calls it up to date; and there is not one file on disk. The run then reports
# ok/changed=false with the correct commit sha, and the install fails two steps
# later complaining that the directory "is a git checkout, but not of MemQL".
#
# A ref is not a working tree. This asks about the tree.
#
# Two reads, because the damage arrives in two shapes. `ls-files` reads the
# INDEX, which an interrupted clone never wrote at all; checking that the first
# tracked path is also PRESENT catches the other shape, where the index survived
# and the files were removed from under it. Neither can misfire on a healthy
# checkout, which always has both.
#
# Deliberately not `git ls-files | head -1`: this tree carries thousands of
# files, so head closes the pipe first and git dies of SIGPIPE, which under
# `set -o pipefail` would report a healthy checkout as broken. Reading one
# NUL-delimited record through a redirection keeps git out of a pipeline
# entirely.
function worktree_is_materialized() {
    local git_bin="$1" dest="$2" first=""
    IFS= read -r -d '' first \
        < <("$git_bin" -C "$dest" ls-files -z 2>/dev/null) || true
    [[ -n "$first" ]] || return 1
    [[ -e "${dest}/${first}" ]]
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

# init_empty <git> <repo> <dest> -- an empty repository pointed at origin.
#
# The --commit path cannot `git clone --branch <sha>`: clone takes a ref name
# and a sha is not one. Init + fetch-the-sha is the only shape that reaches an
# arbitrary commit, and it is also the shape that works when that commit is no
# longer at any ref -- which is the normal case for a repair replaying one.
function init_empty() {
    local git_bin="$1" repo="$2" dest="$3"
    cap_step "initialising ${dest} against ${repo}"
    mkdir -p "$dest"
    if ! "$git_bin" -C "$dest" init --quiet >&2; then
        cap_fail 5 "could not initialise a git repository at ${dest}"
    fi
    if ! "$git_bin" -C "$dest" remote add origin "$repo" >&2; then
        cap_fail 5 "could not point ${dest} at ${repo}"
    fi
}

# fetch_and_detach <git> <repo> <dest> <depth> <commit>
#
# Lands the checkout on an exact commit, whatever it was before.
#
# WHY THIS FETCHES THE SHA RATHER THAN THE REF. For a branch the resolved head
# can move between the ls-remote above and this fetch, and fetching
# `refs/heads/main` would then land on a DIFFERENT commit than the one this run
# resolved, reported and is about to record. Fetching the SHA makes the run
# atomic with respect to its own resolution: the commit reported on the envelope
# is the commit on disk, always. GitHub serves an arbitrary reachable sha to a
# fetch, which is what makes this possible at depth 1.
function fetch_and_detach() {
    local git_bin="$1" repo="$2" dest="$3" depth="$4" commit="$5" current_origin

    current_origin="$("$git_bin" -C "$dest" remote get-url origin 2>/dev/null || true)"
    if [[ "$current_origin" != "$repo" ]]; then
        cap_info "origin was '${current_origin:-<none>}'; pointing it at ${repo}"
        if ! "$git_bin" -C "$dest" remote set-url origin "$repo" >&2; then
            cap_fail 5 "could not point origin at ${repo}"
        fi
    fi

    cap_step "fetching ${commit} into ${dest}"
    local -a args=(-C "$dest" fetch)
    if [[ "$depth" != "0" ]]; then
        args+=(--depth "$depth")
    fi
    args+=(origin "$commit")
    if ! "$git_bin" "${args[@]}" >&2; then
        # FETCHING A BARE SHA IS A SERVER OPTION, not a git guarantee. GitHub
        # enables it (uploadpack.allowAnySHA1InWant), which is why the default
        # path works, but --repo accepts a fork and a self-hosted host may not.
        # Falling back to fetching every ref is slower and always works, because
        # the commit is reachable from one of them; only a commit that is
        # reachable from NOTHING fails both, and that one genuinely is gone.
        cap_info "the remote refused a fetch by sha; retrying with a full fetch"
        if ! "$git_bin" -C "$dest" fetch --tags origin >&2; then
            cap_fail 5 "could not fetch from ${repo}"
        fi
        if ! "$git_bin" -C "$dest" cat-file -e "${commit}^{commit}" 2>/dev/null; then
            cap_fail 5 "commit ${commit} is not in ${repo} (it may have been force-pushed away)"
        fi
    fi
    if ! "$git_bin" -C "$dest" checkout --detach "$commit" >&2; then
        cap_fail 5 "could not check out commit ${commit} in ${dest}"
    fi
}

# restore_worktree <git> <dest> <commit> -- force the working tree back to
# <commit>, whatever is or is not on disk.
#
# WHY --force, AND WHY ONLY HERE. A plain `git checkout --detach` is enough for
# only ONE of the shapes this repairs. Measured, all three at the same commit:
#
#   damage                          checkout --detach   checkout --force --detach
#   no index, no files (clone -9)   restored            restored
#   index intact, files removed     STILL BROKEN (rc 0) restored
#
# With the index intact git reads a missing file as a local DELETION to carry
# across the checkout, prints "D <path>", and exits 0 having restored nothing --
# a silent no-op that looks exactly like a repair. --force is what makes this a
# restore instead of a merge.
#
# It is confined to this path deliberately. Here the tree is already known to be
# unusable, so there is by definition nothing to clobber; an ordinary upgrade
# must keep refusing to overwrite an operator's modifications, so it does not
# come through here.
function restore_worktree() {
    local git_bin="$1" dest="$2" commit="$3"
    cap_step "restoring the working tree at ${dest}"
    if ! "$git_bin" -C "$dest" checkout --force --detach "$commit" >&2; then
        cap_fail 5 "could not restore the working tree in ${dest}"
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local repo tag branch want_ref dest depth git_candidate
    repo="$(cap_param repo   "$DEFAULT_REPO")"
    tag="$(cap_param tag     "")"
    branch="$(cap_param branch "")"
    want_ref="$(cap_param commit "")"
    dest="$(cap_param dest   "$DEFAULT_DEST")"
    depth="$(cap_param depth "$DEFAULT_DEPTH")"
    git_candidate="$(cap_param git "git")"

    cap_require repo "$repo"
    cap_require dest "$dest"
    if [[ ! "$depth" =~ ^[0-9]+$ ]]; then
        cap_fail 2 "invalid --depth '${depth}': expected a non-negative integer (0 = full history)"
    fi

    resolve_git "$git_candidate"
    local git_bin="$GIT_BIN"

    # Resolve (and gate) the ref BEFORE touching the destination: a rejected
    # ref must leave no checkout behind.
    resolve_requested_ref "$git_bin" "$repo" "$tag" "$branch" "$want_ref"
    local ref_kind="$REF_KIND" ref_name="$REF_NAME" want_commit="$WANT_COMMIT"
    cap_info "${ref_kind} ${ref_name} resolves to ${want_commit}"

    local state cloned=false updated=false
    state="$(classify_dest "$dest")"
    case "$state" in
        occupied)
            cap_fail 3 "refusing to overwrite ${dest}: it exists and is not a MemQL checkout; move it aside or pass --dest"
            ;;
        absent|empty)
            case "$ref_kind" in
                tag)
                    clone_at_tag "$git_bin" "$repo" "$ref_name" "$dest" "$depth"
                    ;;
                branch)
                    # Cloned at the branch and then reconciled to the resolved
                    # sha below, rather than fetched blind: a shallow clone of
                    # the branch is one round trip and lands on the right commit
                    # in every case except the narrow race the reconcile covers.
                    clone_at_tag "$git_bin" "$repo" "$ref_name" "$dest" "$depth"
                    ;;
                commit)
                    init_empty "$git_bin" "$repo" "$dest"
                    fetch_and_detach "$git_bin" "$repo" "$dest" "$depth" "$want_commit"
                    ;;
            esac
            cloned=true
            cap_changed
            ;;
        checkout)
            local head materialized=true
            head="$("$git_bin" -C "$dest" rev-parse HEAD 2>/dev/null || true)"
            worktree_is_materialized "$git_bin" "$dest" || materialized=false
            if [[ "$head" == "$want_commit" && "$materialized" == true ]]; then
                cap_info "${dest} is already at ${ref_name} (${want_commit}) -- no change."
            else
                if [[ "$materialized" == false ]]; then
                    # Name the repair. "fetching v1.4.0 into the existing
                    # checkout" logged over a directory already sitting on
                    # v1.4.0 reads like a no-op that somehow changed something;
                    # the operator is owed the actual reason, which is that
                    # their previous run did not finish.
                    cap_info "${dest} has a .git directory but no checked-out tree -- an interrupted clone leaves exactly this; restoring the working tree"
                fi
                if [[ "$ref_kind" == "tag" ]]; then
                    update_to_tag "$git_bin" "$repo" "$ref_name" "$dest" "$depth"
                else
                    fetch_and_detach "$git_bin" "$repo" "$dest" "$depth" "$want_commit"
                fi
                if [[ "$materialized" == false ]]; then
                    # The fetch above guaranteed the objects; this guarantees
                    # the FILES. Ordered this way round so the repair works
                    # whether the broken tree was already on the wanted commit
                    # or on a different one.
                    restore_worktree "$git_bin" "$dest" "$want_commit"
                fi
                updated=true
                cap_changed
            fi
            ;;
    esac

    local commit
    commit="$("$git_bin" -C "$dest" rev-parse HEAD 2>/dev/null || true)"
    if [[ "$commit" != "$want_commit" && "$ref_kind" == "branch" ]]; then
        # The branch moved between resolution and clone. Reconcile onto the
        # commit this run resolved rather than accepting the newer one: the
        # envelope has already committed to a SHA and the receipt is about to
        # record it, so landing anywhere else would make the record a lie.
        cap_info "branch ${ref_name} moved during the clone; reconciling onto ${want_commit}"
        fetch_and_detach "$git_bin" "$repo" "$dest" "$depth" "$want_commit"
        updated=true
        cap_changed
        commit="$("$git_bin" -C "$dest" rev-parse HEAD 2>/dev/null || true)"
    fi
    if [[ "$commit" != "$want_commit" ]]; then
        cap_fail 5 "checkout landed on ${commit:-<nothing>}, expected ${ref_kind} ${ref_name} (${want_commit})"
    fi
    # THE POST-CONDITION, and it is deliberately separate from the branch that
    # repairs this above. That branch fixes the shape we know about; this one
    # states the property every caller depends on -- there are FILES here -- so
    # no future path can exit 0 having left a directory the next step cannot
    # read. The commit check immediately above is a ref check, and a ref check
    # is what let this through in the first place.
    if ! worktree_is_materialized "$git_bin" "$dest"; then
        cap_fail 5 "${dest} is at ${commit} but has no checked-out working tree; the checkout did not complete"
    fi

    cap_info "Done. ${dest} is pinned at ${commit} (${ref_kind} ${ref_name})."
    cap_result_set     repo    "$repo"
    # `tag` stays on the envelope and stays EMPTY for a non-tag install rather
    # than being repurposed to carry a branch name. Readers that mean "which
    # release is this" must not silently start receiving "main"; `ref` and
    # `refKind` are where the non-tag answer lives, and `commit` is the durable
    # one every path sets.
    cap_result_set     tag     "$tag"
    cap_result_set     ref     "$ref_name"
    cap_result_set     refKind "$ref_kind"
    cap_result_set     dest    "$dest"
    cap_result_set     commit  "$commit"
    cap_result_set_raw cloned  "$cloned"
    cap_result_set_raw updated "$updated"
    cap_ok
}

main "$@"
