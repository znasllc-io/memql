#!/bin/bash
set -euo pipefail

# cut-release.sh -- recommend the next semver from the commits since the last
# tag (conventional-commit prefixes; NO AI), then optionally cut a tag-driven
# release. Cutting bumps the VERSION file + the main.go `version` var,
# commits, tags vX.Y.Z, pushes, and creates the GitHub Release as a DRAFT --
# which triggers .github/workflows/release.yml to build + upload the
# per-platform binaries -- then publishes it only once every asset the
# installers download is attached (memql-cockpit#525). `releases/latest`
# never points at a release whose binaries are still building.
#
# Usage:
#   make release                         recommend only (safe; shows next version + why)
#   make release ARGS="--cut"            cut the RECOMMENDED version (asks to confirm)
#   make release ARGS="--cut --yes"      cut it non-interactively
#   make release ARGS="--bump=minor --cut"      force a minor bump
#   make release ARGS="--version=1.2.3 --cut"   force an explicit version
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

function parse_arguments() {
    DO_CUT=false; ASSUME_YES=false; FORCE_BUMP=""; FORCE_VERSION=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cut)        DO_CUT=true; shift ;;
            --yes|-y)     ASSUME_YES=true; shift ;;
            --bump=*)     FORCE_BUMP="${1#*=}"; shift ;;
            --version=*)  FORCE_VERSION="${1#*=}"; shift ;;
            --dry-run)    DO_CUT=false; shift ;;
            -h|--help)    grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
        err "working tree is dirty -- commit or stash before cutting a release"; exit 3
    fi
    local br; br="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
    if [ "$br" != "main" ]; then
        err "on branch '$br', not 'main' -- releases are cut from main"; exit 3
    fi
    command -v gh >/dev/null 2>&1 || { err "gh CLI required to create the release"; exit 4; }
}

function perform_cut() {
    local next="$1" tag="v$1"
    if git -C "$REPO_ROOT" rev-parse "$tag" >/dev/null 2>&1; then
        err "tag $tag already exists"; exit 3
    fi
    log "==> bumping VERSION + main.go to $next"
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
    git -C "$REPO_ROOT" tag -a "$tag" -m "$tag"
    log "==> pushing main + $tag"
    git -C "$REPO_ROOT" push origin main
    git -C "$REPO_ROOT" push origin "$tag"
    local repo
    repo="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
    log "==> creating GitHub Release $tag as a draft (triggers release.yml build+upload)"
    gh release create "$tag" --repo "$repo" --draft \
        --title "$tag" --generate-notes --target main
    publish_when_complete "$repo" "$tag" "$next"
}

# The assets the installers download. A release missing any of them is not
# published: a `latest` without its binary is a 404 in the middle of
# somebody's install.
function expected_assets() {
    local ver="$1" platform
    for platform in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
        echo "memql-${platform}"
        echo "memql-computeruse-${platform}"
        echo "memql-${ver}-${platform}.tar.gz"
    done
    echo "memql-SHA256SUMS"
    echo "memql-computeruse-SHA256SUMS"
    echo "memql-${ver}-SHA256SUMS"
    for platform in darwin-arm64 darwin-amd64; do
        echo "memql-app-${platform}.tar.gz"
        echo "memql-app-${platform}.tar.gz.sha256"
        echo "memql-menubar-${platform}.tar.gz"
        echo "memql-menubar-${platform}.tar.gz.sha256"
    done
}

# missing_assets prints the expected assets the draft does not carry yet.
function missing_assets() {
    local repo="$1" tag="$2" ver="$3" have asset
    have="$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name')" || return 1
    while IFS= read -r asset; do
        grep -qxF "$asset" <<< "$have" || echo "$asset"
    done < <(expected_assets "$ver")
}

# publish_when_complete waits for release.yml on the draft, checks every
# expected asset is attached, and only then publishes. Anything short of
# that leaves the draft unpublished and says how to finish.
function publish_when_complete() {
    local repo="$1" tag="$2" ver="$3" run="" attempt missing
    log "==> waiting for release.yml to start for $tag"
    for ((attempt = 0; attempt < ${RELEASE_RUN_WAIT_ATTEMPTS:-60}; attempt++)); do
        run="$(gh run list --repo "$repo" --workflow release.yml --branch "$tag" --limit 1 --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null || true)"
        [[ -z "$run" ]] || break
        sleep "${RELEASE_POLL_SECONDS:-10}"
    done
    if [[ -z "$run" ]]; then
        err "release.yml never started for $tag; the draft is NOT published."
        err "Run it with: gh workflow run release.yml --repo $repo -f tag=$tag, then: gh release edit $tag --repo $repo --draft=false --latest"
        exit 5
    fi
    if ! gh run watch "$run" --repo "$repo" --exit-status >&2; then
        err "release.yml run $run failed for $tag; the draft is NOT published. Fix it, re-run the job, then publish with: gh release edit $tag --repo $repo --draft=false --latest"
        exit 5
    fi
    missing="$(missing_assets "$repo" "$tag" "$ver")" || { err "could not read the assets of $tag; the draft is NOT published."; exit 5; }
    if [[ -n "$missing" ]]; then
        err "the draft $tag is missing assets the installers download; it is NOT published:"
        local asset
        while IFS= read -r asset; do log "  $asset"; done <<< "$missing"
        exit 5
    fi
    gh release edit "$tag" --repo "$repo" --draft=false --latest
    log ""
    log "DONE: $tag published with every installer asset attached."
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

    show_range "$base"
    log ""
    log "Current:     v${CUR}"
    log "Recommended: v${next}   (${level} bump)"

    if [ "$DO_CUT" != true ]; then
        log ""
        log "(recommend-only) To cut it:  make release ARGS=\"--cut\""
        log "  override:  make release ARGS=\"--bump=minor --cut\"  |  ARGS=\"--version=X.Y.Z --cut\""
        exit 0
    fi

    check_cut_preconditions
    if [ "$ASSUME_YES" != true ]; then
        printf 'Cut release v%s now? [y/N] ' "$next" >&2
        read -r reply </dev/tty || reply=""
        case "$reply" in y|Y|yes) ;; *) log "aborted."; exit 0 ;; esac
    fi
    perform_cut "$next"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
