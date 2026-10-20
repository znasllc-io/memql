#!/bin/bash
set -euo pipefail

# cut-release.sh -- recommend the next semver from the commits since the last
# tag (conventional-commit prefixes; NO AI), then optionally prepare a version
# pull request. After it merges, tag the reviewed main commit and start its
# release build in MemQL. Publication uses the retained native candidate.
#
# Usage:
#   make release                         recommend only (safe; shows next version + why)
#   make release ARGS="--prepare"            prepare the recommended version PR
#   make release ARGS="--prepare --yes"      prepare it non-interactively
#   make release ARGS="--bump=minor --prepare"      force a minor bump
#   make release ARGS="--version=1.2.3 --prepare"   force an explicit version
#
# Recommendation rules (highest wins across the range):
#   major  <- a commit subject like `feat!:` / `fix(x)!:`, or `BREAKING CHANGE` in a body
#   minor  <- a `feat:` / `feat(scope):` subject
#   patch  <- fix/perf/refactor/docs/chore/... or anything else

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION_FILE="$REPO_ROOT/VERSION"
MAIN_GO="$REPO_ROOT/cmd/memql/main.go"
# The worker's FALLBACK version, for a build that never calls SetVersion.
# TestWorkerRegistersTheStampedVersion pins it to VERSION, so a cut that
# bumped only the two files above failed CI on the release PR (v0.11.0).
CONNECT_GO="$REPO_ROOT/internal/worker/connect.go"

#=============================================================================
# FUNCTIONS
#=============================================================================

function log() { echo "$*" >&2; }
function err() { echo "ERROR: $*" >&2; }
function show_help() {
    cat <<'HELP'
Prepare a MemQL Cockpit release version.

make release                              Show the recommended next version
make release ARGS="--prepare"             Open its version PR after confirmation
make release ARGS="--prepare --yes"       Open it without prompting

Options: --bump=major|minor|patch, --version=X.Y.Z, --dry-run, --help
After the PR merges, tag the reviewed commit and build it in MemQL Pipelines.
See VERSIONING.md for candidate publication and download verification.
HELP
}

function parse_arguments() {
    DO_PREPARE=false; ASSUME_YES=false; FORCE_BUMP=""; FORCE_VERSION=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --prepare)    DO_PREPARE=true; shift ;;
            --cut)        err "use --prepare to open a version PR, then build its merged tag in MemQL"; exit 2 ;;
            --yes|-y)     ASSUME_YES=true; shift ;;
            --bump=*)     FORCE_BUMP="${1#*=}"; shift ;;
            --version=*)  FORCE_VERSION="${1#*=}"; shift ;;
            --dry-run)    DO_PREPARE=false; shift ;;
            -h|--help)    show_help; exit 0 ;;
            *)            err "unknown arg: $1"; exit 2 ;;
        esac
    done
}

function last_version() {
    # Latest v-prefixed semver tag, else the VERSION file, else 0.0.0.
    local t
    t="$(git -C "$REPO_ROOT" tag --sort=-v:refname 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"
    if [ -n "$t" ]; then echo "${t#v}"; return; fi
    cat "$VERSION_FILE" 2>/dev/null || echo "0.0.0"
}

function classify_commits() {
    # Reads commits since the base ref; prints "major", "minor", or "patch".
    local base="$1" level="patch" subjects bodies
    subjects="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%s' 2>/dev/null || true)"
    bodies="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%b' 2>/dev/null || true)"
    if echo "$subjects" | grep -qE '^[a-z]+(\([^)]+\))?!:' || echo "$bodies" | grep -q 'BREAKING CHANGE'; then
        level="major"
    elif echo "$subjects" | grep -qE '^feat(\([^)]+\))?:'; then
        level="minor"
    fi
    echo "$level"
}

function bump() {
    # $1=current X.Y.Z  $2=level  ->  next X.Y.Z
    local IFS='.'; read -r ma mi pa <<< "$1"
    case "$2" in
        major) echo "$((ma+1)).0.0" ;;
        minor) echo "${ma}.$((mi+1)).0" ;;
        patch) echo "${ma}.${mi}.$((pa+1))" ;;
        *) err "bad bump level: $2"; exit 2 ;;
    esac
}

function show_range() {
    local base="$1" total drivers
    total="$(git -C "$REPO_ROOT" rev-list --count "${base}..HEAD" 2>/dev/null || echo 0)"
    log ""
    log "Commits since v${CUR}: ${total}"
    drivers="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%s' 2>/dev/null \
        | perl -ne 'chomp; if(/^[a-z]+(\([^)]+\))?!:/){print "  [MAJOR] $_\n"} elsif(/^feat(\([^)]+\))?:/){print "  [minor] $_\n"}' \
        | head -25 || true)"
    if [ -n "$drivers" ]; then
        log "Version-driving commits (feat / breaking):"; echo "$drivers" >&2
    else
        log "  (no feat/breaking commits since the last tag -> patch)"
    fi
}

function check_cut_preconditions() {
    if [ -n "$(git -C "$REPO_ROOT" status --porcelain)" ]; then
        err "working tree is dirty; commit your changes before preparing a release"; exit 3
    fi
    local br; br="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
    if [ "$br" != "main" ]; then
        err "on branch '$br'; prepare the version PR from an up-to-date main checkout"; exit 3
    fi
    command -v gh >/dev/null 2>&1 || { err "gh CLI is required to open the version PR"; exit 4; }
    local remote_head local_head
    remote_head="$(git -C "$REPO_ROOT" ls-remote --exit-code origin refs/heads/main)"
    remote_head="${remote_head%%[[:space:]]*}"
    local_head="$(git -C "$REPO_ROOT" rev-parse HEAD)"
    if [ "$local_head" != "$remote_head" ]; then
        err "main differs from origin; update the checkout before preparing the version PR"; exit 3
    fi
    REPOSITORY="$(cd "$REPO_ROOT" && gh repo view --json nameWithOwner -q .nameWithOwner)"
    [[ "$REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { err "could not resolve the GitHub repository"; exit 4; }
}

function perform_cut() {
    local next="$1" tag="v$1" branch="release/v$1" tag_status
    if git -C "$REPO_ROOT" show-ref --verify --quiet "refs/tags/$tag"; then
        err "tag $tag already exists"; exit 3
    fi
    if git -C "$REPO_ROOT" ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null; then
        err "remote tag $tag already exists"; exit 3
    else
        tag_status=$?
        [ "$tag_status" -eq 2 ] || { err "could not check the remote tag"; exit 4; }
    fi
    if git -C "$REPO_ROOT" ls-remote --exit-code origin "refs/heads/$branch" >/dev/null; then
        err "release branch $branch already exists; resume its PR"; exit 3
    else
        tag_status=$?
        [ "$tag_status" -eq 2 ] || { err "could not check the remote release branch"; exit 4; }
    fi
    # Check both anchors before modifying any file or creating a branch.
    grep -q '^var version = "[^"]*"$' "$MAIN_GO" || { err "main.go version declaration is missing"; exit 3; }
    grep -q '^var cockpitVersionValue = "[^"]*"$' "$CONNECT_GO" || { err "worker version declaration is missing"; exit 3; }
    git -C "$REPO_ROOT" switch -c "$branch"
    log "Preparing version $next"
    echo "$next" > "$VERSION_FILE"
    # `var`, not `const`: -ldflags -X can only set a var, so that is what
    # main.go declares. Matching the wrong keyword substitutes NOTHING and
    # the cut proceeds with a stale version compiled in -- which is why
    # the result is VERIFIED below rather than assumed.
    perl -pi -e "s/^var version = \".*\"/var version = \"$next\"/" "$MAIN_GO"
    if ! grep -q "^var version = \"$next\"$" "$MAIN_GO"; then
        err "failed to bump the version in $MAIN_GO (expected: var version = \"$next\")"
        exit 1
    fi
    perl -pi -e "s/^var cockpitVersionValue = \".*\"/var cockpitVersionValue = \"$next\"/" "$CONNECT_GO"
    if ! grep -q "^var cockpitVersionValue = \"$next\"$" "$CONNECT_GO"; then
        err "failed to bump the fallback version in $CONNECT_GO (expected: var cockpitVersionValue = \"$next\")"
        exit 1
    fi
    git -C "$REPO_ROOT" add "$VERSION_FILE" "$MAIN_GO" "$CONNECT_GO"
    git -C "$REPO_ROOT" commit -q -m "release: $tag"
    git -C "$REPO_ROOT" push -u origin "HEAD:refs/heads/$branch"
    release_pr_body="$(mktemp)"
    trap 'rm -f -- "$release_pr_body"' EXIT
    printf 'Stamp Cockpit %s in VERSION and both binary version fallbacks.\n\nAfter native checks pass and this PR merges, tag the reviewed main commit and run the tag through MemQL Pipelines. Publish the verified retained candidate through MemQL.\n' "$next" > "$release_pr_body"
    gh pr create --repo "$REPOSITORY" --base main --head "$branch" \
        --title "release: $tag" --body-file "$release_pr_body"
    rm -f -- "$release_pr_body"
    trap - EXIT
    log "Version PR prepared. After merge, follow VERSIONING.md to build and publish $tag in MemQL."
}

function main() {
    parse_arguments "$@"
    CUR="$(last_version)"
    local base="v${CUR}"
    git -C "$REPO_ROOT" rev-parse "$base" >/dev/null 2>&1 || base="$(git -C "$REPO_ROOT" rev-list --max-parents=0 HEAD | tail -1)"  # no tag yet -> from root

    local level next
    if [ -n "$FORCE_VERSION" ]; then
        next="$FORCE_VERSION"; level="explicit"
    else
        level="${FORCE_BUMP:-$(classify_commits "$base")}"
        next="$(bump "$CUR" "$level")"
    fi
    [[ "$next" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { err "version must be MAJOR.MINOR.PATCH"; exit 2; }

    show_range "$base"
    log ""
    log "Current:     v${CUR}"
    log "Recommended: v${next}   (${level} bump)"

    if [ "$DO_PREPARE" != true ]; then
        log ""
        log "To prepare its PR: make release ARGS=\"--prepare\""
        log "Override with --bump=minor or --version=X.Y.Z."
        exit 0
    fi

    check_cut_preconditions
    if [ "$ASSUME_YES" != true ]; then
        printf 'Prepare the version PR for v%s? [y/N] ' "$next" >&2
        read -r reply </dev/tty || reply=""
        case "$reply" in y|Y|yes) ;; *) log "aborted."; exit 0 ;; esac
    fi
    perform_cut "$next"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
