#!/usr/bin/env bash
#
# scripts/docs/current-check.sh
# =============================
#
# Capability: docs.currentCheck -- fail when the documentation site is not
# serving the newest release's docs.
#
# Two facts are checked, and either one failing fails the run:
#
#   1. THE NEWEST PUBLISHED STABLE RELEASE CARRIES A DOCS BUNDLE. Choose
#      the numeric maximum vX.Y.Z among non-draft, non-prerelease GitHub
#      Releases, then require its docs-<X.Y.Z>.tgz asset. Native release builds
#      reserve immutable tags before approval and publication; an unpublished
#      or retired candidate is not a release the docs site should serve.
#   2. THE SITE SERVES THAT VERSION. The docs page (default
#      https://memql.io/docs/) must carry
#      <meta name="memql-docs-version" content="X.Y.Z"> equal to the newest
#      release's bare version. An absent meta is a failure, not a skip: a site that
#      does not say which release it documents cannot be shown current.
#
# This weekly native documentation-audit step checks both ends of publication.
# From v0.20.22 to v0.23.5 releases had no docs asset while workflows stayed
# green. A published release with no bundle still fails; the check fixes nothing.
#
# Both checks always run and both results are reported, so one failure never
# hides the other. The release listing and the release's asset list are captured to
# files and searched there, never piped into `grep -q`: under pipefail an early
# grep exit can SIGPIPE the writer and fail a check that matched.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 current | 2 bad param | 4 gh or curl missing | 5 not current,
# or a read failed
#
# Refs: memql#5714

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "docs.currentCheck" \
    "Check that docs for the newest published release are available and served by the site."
cap_spec_param "repo"    "owner/name of the repository whose releases are checked (default: \$GITHUB_REPOSITORY, else znasllc-io/memql)"
cap_spec_param "siteUrl" "the docs page whose memql-docs-version meta is compared (default: https://memql.io/docs/)"
cap_spec_param "public-api" "read public GitHub data without credentials (flag)" ""

REPO=""
SITE_URL=""
TAG=""
BARE=""
ASSET=""
ASSET_PRESENT="false"
SITE_VERSION=""
PROBLEMS=""
SCRATCH=""
PUBLIC_API=false

# _current_check_on_exit removes the scratch directory, then hands off to the
# EXIT trap cap_init installed. Composed rather than replacing it: that trap is
# what emits a failure envelope on an unexpected abort, and a bare cleanup trap
# would silently remove the guarantee.
function _current_check_on_exit() {
    local rc=$?
    # errexit off: `(exit "$rc")` is a failing command whenever rc is non-zero,
    # and errexit would abandon the handler there, before the envelope.
    set +e
    if [[ -n "$SCRATCH" ]]; then
        rm -rf "$SCRATCH" 2>/dev/null
    fi
    (exit "$rc")
    _cap_on_exit
    return 0
}

function problem() {
    PROBLEMS="${PROBLEMS:+${PROBLEMS} }$1"
    cap_error "$1"
    return 0
}

function check_prerequisites() {
    if [[ "$PUBLIC_API" == true ]]; then
        command -v python3 &>/dev/null || cap_fail 4 "python3 is required for public GitHub reads"
    else
        command -v gh &>/dev/null \
            || cap_fail 4 "gh is not installed or not on PATH; it reads published releases and their assets"
    fi
    command -v curl &>/dev/null \
        || cap_fail 4 "curl is not installed or not on PATH; it fetches the docs page"
    return 0
}

function read_releases() {
    if [[ "$PUBLIC_API" == true ]]; then
        python3 "${SCRIPT_DIR}/../lib/github-public-read.py" published-releases --repo="$REPO"
    else
        gh api "repos/${REPO}/releases" --paginate --jq '.[] | select(.draft == false and .prerelease == false and .published_at != null) | .tag_name'
    fi
}

function read_assets() {
    if [[ "$PUBLIC_API" == true ]]; then
        python3 "${SCRIPT_DIR}/../lib/github-public-read.py" release-assets --repo="$REPO" --tag="$TAG"
    else
        gh release view "$TAG" --repo "$REPO" --json assets,isDraft,isPrerelease,publishedAt \
            --jq 'if .isDraft == false and .isPrerelease == false and .publishedAt != null then .assets[].name else error("Release is not published and stable") end'
    fi
}

function make_scratch() {
    SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/memql-docs-current.XXXXXX")" \
        || cap_fail 5 "could not create a scratch directory"
    trap _current_check_on_exit EXIT
    return 0
}

# resolve_newest_release selects the numerically newest published stable release.
#
# NUMERIC, not lexicographic: v0.9.2 sorts after v0.17.0 as a string. And only
# tags that are exactly vX.Y.Z count -- memql-sdk-core-v*, component/*/gen/v*,
# pre-release suffixes and the bare 0.14.0-era tags are other things.
function resolve_newest_release() {
    local releases_file="${SCRATCH}/releases"
    if ! read_releases >"$releases_file"; then
        cap_fail 5 "could not list the published releases of ${REPO}, so there is no newest release to check"
    fi
    BARE="$(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' "$releases_file" \
        | sed 's/^v//' \
        | sort -t. -k1,1n -k2,2n -k3,3n \
        | tail -n 1 || true)"
    if [[ -z "$BARE" ]]; then
        cap_fail 5 "${REPO} has no published stable vX.Y.Z release, so there are no release docs to check"
    fi
    TAG="v${BARE}"
    ASSET="docs-${BARE}.tgz"
    cap_result_set "tag" "$TAG"
    cap_result_set "version" "$BARE"
    cap_result_set "asset" "$ASSET"
    cap_info "newest published release in ${REPO}: ${TAG}"
    return 0
}

function check_asset() {
    local assets_file="${SCRATCH}/assets"
    if ! read_assets >"$assets_file"; then
        problem "${TAG} has no readable GitHub Release containing ${ASSET}."
        return 0
    fi
    if grep -qxF "$ASSET" "$assets_file"; then
        ASSET_PRESENT="true"
        cap_info "${TAG} carries ${ASSET}"
        return 0
    fi
    problem "the ${TAG} release has no ${ASSET} asset. Check its release publication receipts."
    return 0
}

# read_site_version sets SITE_VERSION from the page's memql-docs-version meta.
# Attribute order and quoting vary with the renderer, so the whole tag is
# matched first and its content attribute read from it.
function read_site_version() {
    local page_file="${SCRATCH}/page.html"
    if ! curl -fsSL --max-time 30 -o "$page_file" "$SITE_URL"; then
        problem "could not fetch ${SITE_URL}, so which release it documents is unknown"
        return 0
    fi
    SITE_VERSION="$(grep -oE "<meta[^>]*name=[\"']memql-docs-version[\"'][^>]*>" "$page_file" \
        | sed -n '1p' \
        | sed -nE "s/.*content=[\"']([^\"']*)[\"'].*/\\1/p" || true)"
    if [[ -z "$SITE_VERSION" ]]; then
        problem "${SITE_URL} carries no memql-docs-version meta, so it cannot be shown to serve ${BARE}. The docs layout renders it from the bundle's manifest version."
        return 0
    fi
    if [[ "$SITE_VERSION" != "$BARE" ]]; then
        problem "${SITE_URL} serves the docs of ${SITE_VERSION}, and the newest release is ${BARE}. The site's docs sync has not picked up ${ASSET}."
        return 0
    fi
    cap_info "${SITE_URL} serves ${SITE_VERSION}"
    return 0
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    REPO="$(cap_param repo "${GITHUB_REPOSITORY:-znasllc-io/memql}")"
    SITE_URL="$(cap_param siteUrl "https://memql.io/docs/")"
    if cap_bool public-api; then PUBLIC_API=true; fi
    [[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] \
        || cap_fail 2 "--repo must be owner/name, got '${REPO}'"
    [[ "$SITE_URL" =~ ^https?:// ]] \
        || cap_fail 2 "--siteUrl must be an http(s) URL, got '${SITE_URL}'"
    cap_result_set "repository" "$REPO"
    cap_result_set "siteUrl" "$SITE_URL"

    check_prerequisites
    make_scratch
    resolve_newest_release
    check_asset
    read_site_version

    cap_result_set_raw "assetPresent" "$ASSET_PRESENT"
    cap_result_set "siteVersion" "$SITE_VERSION"
    if [[ -n "$PROBLEMS" ]]; then
        cap_fail 5 "the docs are not current: ${PROBLEMS}"
    fi
    cap_ok
}

main "$@"
