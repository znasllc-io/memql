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
#   1. THE NEWEST RELEASE TAG CARRIES A DOCS BUNDLE. The newest vX.Y.Z tag
#      (numeric maximum, the same rule integrations/release/version.go uses to
#      compute the next cut) must have a GitHub Release with a
#      docs-<X.Y.Z>.tgz asset. publish-docs-bundle.yml attaches it on
#      `release: published`; a release with no asset is a docs publish that
#      failed or never ran. A tag with no Release at all fails too: it fired no
#      workflow, so it has no asset and no images either.
#   2. THE SITE SERVES THAT VERSION. The docs page (default
#      https://memql.io/docs/) must carry
#      <meta name="memql-docs-version" content="X.Y.Z"> equal to the newest
#      tag's bare version. An absent meta is a failure, not a skip: a site that
#      does not say which release it documents cannot be shown current.
#
# WHY A SCHEDULED CHECK (docs-current-check.yml, weekly). Every hop from a cut
# to the site is event-driven, and an event handler that silently does not fire
# looks exactly like one that has not fired yet. From v0.20.22 to v0.23.5 every
# release went out with no docs asset, and the site went on serving an old
# bundle, while every workflow stayed green. This check reads the two ends of
# the pipeline and says whether they agree; it fixes nothing.
#
# Both checks always run and both results are reported, so one failure never
# hides the other. The tag listing and the release's asset list are captured to
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
    "Fail when the newest release tag has no docs bundle asset, or the docs site serves a different memql-docs-version."
cap_spec_param "repo"    "owner/name of the repository whose releases are checked (default: \$GITHUB_REPOSITORY, else znasllc-io/memql)"
cap_spec_param "siteUrl" "the docs page whose memql-docs-version meta is compared (default: https://memql.io/docs/)"

REPO=""
SITE_URL=""
TAG=""
BARE=""
ASSET=""
ASSET_PRESENT="false"
SITE_VERSION=""
PROBLEMS=""
SCRATCH=""

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
    command -v gh &>/dev/null \
        || cap_fail 4 "gh is not installed or not on PATH; it reads the release tags and the release's assets"
    command -v curl &>/dev/null \
        || cap_fail 4 "curl is not installed or not on PATH; it fetches the docs page"
    return 0
}

function make_scratch() {
    SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/memql-docs-current.XXXXXX")" \
        || cap_fail 5 "could not create a scratch directory"
    trap _current_check_on_exit EXIT
    return 0
}

# resolve_newest_tag sets TAG and BARE to the numerically newest vX.Y.Z tag.
#
# NUMERIC, not lexicographic: v0.9.2 sorts after v0.17.0 as a string. And only
# tags that are exactly vX.Y.Z count -- memql-sdk-core-v*, component/*/gen/v*,
# pre-release suffixes and the bare 0.14.0-era tags are other things.
function resolve_newest_tag() {
    local tags_file="${SCRATCH}/tags"
    if ! gh api "repos/${REPO}/tags" --paginate --jq '.[].name' >"$tags_file"; then
        cap_fail 5 "could not list the tags of ${REPO} (gh's error is above), so there is no newest release to check"
    fi
    BARE="$(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' "$tags_file" \
        | sed 's/^v//' \
        | sort -t. -k1,1n -k2,2n -k3,3n \
        | tail -n 1 || true)"
    if [[ -z "$BARE" ]]; then
        cap_fail 5 "${REPO} has no vX.Y.Z tag, so there is no release whose docs could be current"
    fi
    TAG="v${BARE}"
    ASSET="docs-${BARE}.tgz"
    cap_result_set "tag" "$TAG"
    cap_result_set "version" "$BARE"
    cap_result_set "asset" "$ASSET"
    cap_info "newest release tag in ${REPO}: ${TAG}"
    return 0
}

function check_asset() {
    local assets_file="${SCRATCH}/assets"
    if ! gh release view "$TAG" --repo "$REPO" --json assets --jq '.assets[].name' >"$assets_file"; then
        problem "${TAG} has no readable GitHub Release (gh's error is above). A tag with no Release fired no publish workflow, so it has no ${ASSET}."
        return 0
    fi
    if grep -qxF "$ASSET" "$assets_file"; then
        ASSET_PRESENT="true"
        cap_info "${TAG} carries ${ASSET}"
        return 0
    fi
    problem "the ${TAG} release has no ${ASSET} asset. publish-docs-bundle.yml attaches it on release: published; read that workflow's run for ${TAG}, or dispatch it with version=${BARE}."
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
    [[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] \
        || cap_fail 2 "--repo must be owner/name, got '${REPO}'"
    [[ "$SITE_URL" =~ ^https?:// ]] \
        || cap_fail 2 "--siteUrl must be an http(s) URL, got '${SITE_URL}'"
    cap_result_set "repository" "$REPO"
    cap_result_set "siteUrl" "$SITE_URL"

    check_prerequisites
    make_scratch
    resolve_newest_tag
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
