#!/usr/bin/env bash
#
# scripts/release/publish-releases.sh
# ===================================
#
# Publish GitHub Releases for stack versions, idempotently, so the Releases
# pages never drift from the release lockfiles (znasllc-io/memql#1097).
#
# Two surfaces, because they have different data + credential needs:
#
#   memql      -- one Release per releases/<X.Y.Z>.yaml, tagged X.Y.Z at the
#                 commit that INTRODUCED that lockfile on main. Needs only
#                 git + gh (GITHUB_TOKEN), so CI runs this automatically on
#                 every push of a new lockfile (.github/workflows/publish-
#                 releases.yml).
#   components -- visionarys-io/memql-bff-copresent + visionarys-io/copresent.
#                 Deduplicated by image digest: a Release only for the stack
#                 version where that component's digest CHANGED, anchored to
#                 the build commit via ACR build-time -> that repo's main.
#                 Needs `az` (ACR build-time) + a cross-org gh token, so it is
#                 an operator step (run after a release) or a CI job wired with
#                 Azure OIDC + a RELEASE_PUBLISH_TOKEN PAT.
#
# memql-cockpit is intentionally excluded -- it is not a stack lockfile
# component (separate version line).
#
# Idempotent: an existing Release for a tag is skipped, so this is safe to
# re-run and safe as a backfill. Function-based + bash 3.2 compatible (no
# associative arrays) per the repo convention.
#
# Usage:
#   publish-releases.sh [--repo=memql|components|all] [--version=X.Y.Z] [--dry-run]
#
#   --repo      Which surface to publish. Default: all.
#   --version   Only this version (memql) / only groups touching it (components).
#               Default: every lockfile version missing a Release.
#   --dry-run   Print the plan; create nothing.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# The component repos (memql-bff-copresent, copresent) are siblings of the
# PRIMARY memql checkout, not of a linked worktree. Resolve the primary
# checkout's parent so --repo=components works from either location.
_MAIN_CHECKOUT="$(git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | awk '/^worktree /{print $2; exit}')"
WORKSPACE_ROOT="$(cd "${_MAIN_CHECKOUT:-$REPO_ROOT}/.." && pwd)"

readonly MEMQL_REPO="znasllc-io/memql"
readonly ACR_NAME="acrmemql"
# Dedup-by-digest component repos (lockfile component key == ACR repository).
readonly COMPONENT_KEYS="memql-bff-copresent copresent"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

function parse_arguments() {
    REPO="all"
    ONLY_VERSION=""
    DRY_RUN=false
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --repo=*)    REPO="${1#*=}"; shift ;;
            --version=*) ONLY_VERSION="${1#*=}"; shift ;;
            --dry-run)   DRY_RUN=true; shift ;;
            --help)      show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 2 ;;
        esac
    done
    case "$REPO" in
        memql|components|all) ;;
        *) echo "ERROR: --repo must be memql|components|all" >&2; exit 2 ;;
    esac
}

function info() { echo "INFO: $*"; }
function warn() { echo "WARNING: $*" >&2; }

# lockfile_versions -- every X.Y.Z with a releases/X.Y.Z.yaml, semver-sorted.
function lockfile_versions() {
    ls "$REPO_ROOT"/releases/*.yaml 2>/dev/null | xargs -n1 basename | sed 's/\.yaml$//' | sort -V
}

# repo_for_component KEY -> owner/name of the component repo.
function repo_for_component() {
    case "$1" in
        memql-bff-copresent) echo "visionarys-io/memql-bff-copresent" ;;
        copresent)           echo "visionarys-io/copresent" ;;
        *)                   echo "" ;;
    esac
}

# component_digest VERSION KEY -> the @sha256 digest that component shipped in
# that lockfile.
function component_digest() {
    awk -v c="$2:" '$1==c{f=1} f&&/digest:/{print $2; exit}' "$REPO_ROOT/releases/$1.yaml"
}

# gh_release_exists TAG REPO
function gh_release_exists() {
    gh release view "$1" --repo "$2" >/dev/null 2>&1
}

# create_release REPO TAG TARGET_SHA TITLE NOTES LATESTFLAG
function create_release() {
    local repo="$1" tag="$2" sha="$3" title="$4" notes="$5" latest="$6"
    if gh_release_exists "$tag" "$repo"; then
        info "skip ${repo} ${tag} (already exists)"
        return 0
    fi
    if [ "$DRY_RUN" = true ]; then
        echo "PLAN: create ${repo} ${tag} @ ${sha:0:9} (${latest})"
        return 0
    fi
    if gh release create "$tag" --repo "$repo" --target "$sha" --title "$title" --notes "$notes" $latest >/dev/null; then
        info "created ${repo} ${tag} @ ${sha:0:9}"
    else
        warn "FAILED to create ${repo} ${tag}"
        return 1
    fi
}

# attach_docs_bundle TAG -- build the per-version documentation bundle and
# upload it as a release asset (znasllc-io/memql#1172), so memql.io can consume
# a versioned docs snapshot per release. Best-effort + non-fatal: it needs `go`
# (to run cmd/docs-gen); if go is absent or the build fails it warns and skips,
# so a docs-tooling gap never blocks the Release itself. The bundle reflects the
# current checkout's docs/public -- on the CI push that introduces a lockfile
# that is the release commit, so the snapshot matches the release.
function attach_docs_bundle() {
    local tag="$1"
    [ -n "$tag" ] || return 0
    if [ "$DRY_RUN" = true ]; then
        echo "PLAN: build + upload docs-${tag}.tgz to ${MEMQL_REPO} ${tag}"
        return 0
    fi
    if ! command -v go >/dev/null 2>&1; then
        warn "go not found; skipping docs bundle for ${tag} (run scripts/docs/build-docs-bundle.sh + 'gh release upload' manually)"
        return 0
    fi
    if ! gh_release_exists "$tag" "$MEMQL_REPO"; then
        warn "release ${tag} not present yet; skipping docs bundle"
        return 0
    fi
    info "building docs bundle for ${tag}..."
    if ! bash "$REPO_ROOT/scripts/docs/build-docs-bundle.sh" --version="$tag"; then
        warn "docs bundle build failed for ${tag}; release left without a docs asset"
        return 0
    fi
    local tarball="$REPO_ROOT/docs-${tag}.tgz"
    [ -f "$tarball" ] || { warn "expected ${tarball} not produced; skipping upload"; return 0; }
    if gh release upload "$tag" "$tarball" --repo "$MEMQL_REPO" --clobber >/dev/null 2>&1; then
        info "uploaded docs-${tag}.tgz to ${MEMQL_REPO} ${tag}"
    else
        warn "failed to upload docs-${tag}.tgz to ${tag}"
    fi
}

# publish_memql -- one Release per lockfile, anchored at its introducing commit.
function publish_memql() {
    local versions; versions="$(lockfile_versions)"
    [ -z "$versions" ] && { warn "no release lockfiles found"; return 0; }
    local latest_ver; latest_ver="$(echo "$versions" | tail -1)"
    local v sha short subj engine gate digests notes latest
    for v in $versions; do
        [ -n "$ONLY_VERSION" ] && [ "$v" != "$ONLY_VERSION" ] && continue
        local f="$REPO_ROOT/releases/$v.yaml"
        sha="$(git -C "$REPO_ROOT" log --diff-filter=A --format=%H -1 -- "releases/$v.yaml")"
        if [ -z "$sha" ]; then warn "no introducing commit for releases/$v.yaml; skipping"; continue; fi
        short="$(git -C "$REPO_ROOT" log -1 --format=%h "$sha")"
        subj="$(git -C "$REPO_ROOT" log -1 --format=%s "$sha")"
        engine="$(grep -E '^engineVersion:' "$f" | sed -E 's/engineVersion: *"?([^"]*)"?/\1/')"
        gate="$(grep -E '^gate:' "$f" | sed -E 's/gate: *"?([^"]*)"?/\1/')"
        digests="$(awk '/^  [a-z]/{c=$1; sub(/:$/,"",c); next} /^    digest:/{print "- `" c "` " $2}' "$f")"
        [ "$v" = "$latest_ver" ] && latest="--latest" || latest="--latest=false"
        notes="$(printf '%s\n\n- **engineVersion:** %s\n- **gate:** `%s`\n- **source commit:** %s\n- **lockfile:** `releases/%s.yaml`\n\n## Component image digests (ACR `%s.azurecr.io`)\n%s\n\n---\nDeployment-v2 GitOps release: the immutable artifact is the digest-pinned image set above; the lockfile is the source of truth. Staging is rolled by ArgoCD from `deploy/k8s/overlays/staging`.' \
            "$subj" "$engine" "$gate" "$short" "$v" "$ACR_NAME" "$digests")"
        create_release "$MEMQL_REPO" "$v" "$sha" "$v" "$notes" "$latest"
    done
    # Attach the versioned docs bundle to the latest release (#1172). Only the
    # latest, since the bundle reflects the current tree -- backfilling old
    # versions with current docs would be misleading.
    [ -z "$ONLY_VERSION" ] && attach_docs_bundle "$latest_ver" || attach_docs_bundle "$ONLY_VERSION"
}

# publish_components -- dedup-by-digest, ACR-build-time anchored.
function publish_components() {
    if ! command -v az >/dev/null 2>&1; then
        warn "az CLI not found; component-repo Releases need ACR build-time. Skipping."
        warn "Run this locally (where az is authenticated) with --repo=components, or wire Azure OIDC into CI."
        return 0
    fi
    local versions; versions="$(lockfile_versions)"
    local comp repo repo_dir
    for comp in $COMPONENT_KEYS; do
        repo="$(repo_for_component "$comp")"
        repo_dir="$WORKSPACE_ROOT/$comp"
        if [ ! -d "$repo_dir/.git" ]; then warn "$repo_dir not checked out; skipping $comp"; continue; fi
        git -C "$repo_dir" fetch origin --quiet 2>/dev/null

        # Build the distinct-digest groups (first version + the run sharing it).
        local prev="" idx=-1
        local group_first=() group_digest=() group_versions=()
        local v dg
        for v in $versions; do
            dg="$(component_digest "$v" "$comp")"
            if [ "$dg" != "$prev" ]; then
                idx=$((idx+1)); group_first[$idx]="$v"; group_digest[$idx]="$dg"; group_versions[$idx]="$v"; prev="$dg"
            else
                group_versions[$idx]="${group_versions[$idx]} $v"
            fi
        done
        local last=$idx i fv vers t sha short subj latest vlist notes
        for i in $(seq 0 $last); do
            fv="${group_first[$i]}"; dg="${group_digest[$i]}"; vers="${group_versions[$i]}"
            if [ -n "$ONLY_VERSION" ]; then case " $vers " in *" $ONLY_VERSION "*) ;; *) continue ;; esac; fi
            t="$(az acr manifest show-metadata --registry "$ACR_NAME" --name "$comp@$dg" --query createdTime -o tsv 2>/dev/null)"
            if [ -z "$t" ]; then warn "no ACR build-time for $comp@$dg; skipping $fv"; continue; fi
            sha="$(git -C "$repo_dir" rev-list -1 --before="$t" origin/main 2>/dev/null)"
            if [ -z "$sha" ]; then warn "no commit before $t on $repo main; skipping $fv"; continue; fi
            short="$(git -C "$repo_dir" log -1 --format=%h "$sha")"
            subj="$(git -C "$repo_dir" log -1 --format=%s "$sha")"
            [ "$i" = "$last" ] && latest="--latest" || latest="--latest=false"
            vlist="$(echo $vers | tr ' ' ',' | sed 's/,/, /g')"
            notes="$(printf 'Component image shipped in the memQL stack.\n\n- **shipped in stack versions:** %s\n- **digest:** `%s`\n- **ACR built:** %s\n- **anchor commit (approx, by build-time):** %s — %s\n\n---\nThis repo ships as a digest-pinned component of the memQL stack (see znasllc-io/memql releases/*.yaml). Per-version build commits are not recorded in image metadata; the anchor is this repo main at the image build time.' \
                "$vlist" "$dg" "${t%.*}" "$short" "$subj")"
            create_release "$repo" "$fv" "$sha" "$fv" "$notes" "$latest"
        done
    done
}

function main() {
    parse_arguments "$@"
    if [ "$REPO" = "memql" ] || [ "$REPO" = "all" ]; then
        info "publishing memql Releases..."
        publish_memql
    fi
    if [ "$REPO" = "components" ] || [ "$REPO" = "all" ]; then
        info "publishing component Releases (bff-copresent, copresent)..."
        publish_components
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
