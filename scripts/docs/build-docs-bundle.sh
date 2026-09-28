#!/usr/bin/env bash
#
# scripts/docs/build-docs-bundle.sh
# =================================
#
# Capability: docs.buildBundle -- build the release documentation bundle,
# docs-<version>.tgz, or check docs/public against the public boundary.
#
# A thin wrapper. The work is `cmd/docs-gen bundle` (bundle contract v2,
# memql#5717; docs/DOCS_STANDARD.md section 5), the same code the root gates
# docs_public_boundary_test.go and docs_bundle_build_test.go run on every pull
# request, so the release lane cannot build a bundle the pull-request gate
# would have refused. It selects the pages git tracks under docs/public that
# are public, stable and exposure engine, renders the concept catalog as
# reference/concepts.md, rewrites every relative link to a published page into
# its /docs/<slug>/ route and refuses every other relative link unless
# docs/public_boundary_allowlist.toml names it, strips the HTML comments
# outside code, and writes manifest.json (schema 2), memql-docs-version,
# llms.txt, llms-full.txt, sitemap-docs.xml and the page tree into --out, packed
# reproducibly as <repo>/docs-<version>.tgz.
#
# The version is checked, not trusted: it must be X.Y.Z and equal the first
# line of the VERSION file, which equals the tag of the commit a release cut
# tags (VERSIONING.md, memql#5714). A mismatch means VERSION lagged the tag, and
# a bundle built anyway would publish docs labelled with one release from a
# tree that says it is another -- how bundle 0.21.25 went out naming engine
# 0.15.0. The check runs before anything is compiled or generated.
#
# A build dates every page from git history, so it refuses a shallow clone
# (exit 4): at depth 1 every page would date to the tip commit. --check needs
# no history and runs anywhere.
#
# Usage:
#   build-docs-bundle.sh --version=X.Y.Z [--out=DIR] [--check] [--dryRun]
#                        [--siteUrl=URL]
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param (unknown flag, a version that is not X.Y.Z,
# an --out directory holding something other than a previous bundle) |
# 3 refused: --version differs from VERSION | 4 precondition (no VERSION file,
# go or git missing, not a git checkout, a shallow clone, a page in no commit)
# | 5 the build failed (docs-gen did not compile, or could not render or
# write) | 6 the public boundary check failed (the violations are on stderr
# and in result.bundle.violations)
#
# Refs: memql#5717 memql#5714 memql#1171

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "docs.buildBundle" \
    "Build the release docs bundle docs-<version>.tgz from docs/public, or check docs/public against the public boundary."
cap_spec_param_required "version" "bare X.Y.Z the bundle is labelled with; must equal the first line of VERSION, the release tag"
cap_spec_param "out"     "directory the bundle tree is written to (default: <repo>/docs-bundle; a previous bundle there is replaced)"
cap_spec_param "check"   "run the selection and the boundary check only, writing nothing"
cap_spec_param "dryRun"  "report what would be built and run nothing"
cap_spec_param "siteUrl" "site the absolute URLs in llms.txt, llms-full.txt and the sitemap point at (default: https://memql.io)"

SCRATCH=""

# _bundle_on_exit removes the scratch directory, then hands off to the EXIT
# trap cap_init installed. Composed rather than replacing it: that trap is
# what emits a failure envelope on an unexpected abort.
function _bundle_on_exit() {
    local rc=$?
    set +e
    if [[ -n "$SCRATCH" ]]; then
        rm -rf "$SCRATCH" 2>/dev/null
    fi
    (exit "$rc")
    _cap_on_exit
    return 0
}

# check_version refuses a version that is not bare X.Y.Z (2), a tree with no
# VERSION file (4), and a version VERSION does not name (3).
function check_version() {
    local version="$1" file_version
    # `[[ =~ ]]` matches the whole string, newlines included; a grep would
    # anchor per line and pass a multi-line value whose first line is valid.
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        cap_fail 2 "--version '$version' is not X.Y.Z (bare, no leading v): it names docs-<version>.tgz, which the site fetches by the bare version"
    fi
    if [[ ! -f "$REPO_ROOT/VERSION" ]]; then
        cap_fail 4 "no VERSION file at $REPO_ROOT/VERSION; the bundle's version is checked against it"
    fi
    file_version="$(sed -n '1p' "$REPO_ROOT/VERSION" | tr -d '[:space:]')"
    cap_result_set "versionFile" "$file_version"
    if [[ "$version" != "$file_version" ]]; then
        cap_fail 3 "--version $version differs from VERSION ($file_version). VERSION equals the tag a release is cut at (VERSIONING.md), so a bundle whose version disagrees with it would label one release's docs with another's number. Build at the tag whose VERSION reads $version."
    fi
    return 0
}

# require_tools refuses to start without go and git (4).
function require_tools() {
    local tool
    for tool in go git; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            cap_fail 4 "$tool is not installed; the bundle is built by cmd/docs-gen from the git-tracked docs/public"
        fi
    done
    return 0
}

# compile_docs_gen builds cmd/docs-gen into the scratch directory. It is built
# and then run, rather than `go run`, because `go run` exits 1 whatever the
# program's own code was, and this wrapper passes docs-gen's codes through.
function compile_docs_gen() {
    SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/docs-bundle.XXXXXX")" \
        || cap_fail 5 "could not create a scratch directory"
    trap _bundle_on_exit EXIT
    cap_step "compiling cmd/docs-gen"
    if ! (cd "$REPO_ROOT" && GOWORK=off go build -o "$SCRATCH/docs-gen" ./cmd/docs-gen) >&2; then
        cap_fail 5 "cmd/docs-gen did not compile (the compiler's error is above)"
    fi
    return 0
}

# run_docs_gen runs `docs-gen bundle`, puts its one-line JSON summary in the
# result as `bundle`, and passes its exit code through.
function run_docs_gen() {
    local version="$1" out="$2" check="$3" site="$4" summary="" rc=0
    local -a args
    args=(bundle -version "$version" -root "$REPO_ROOT" -site-url "$site")
    if [[ "$check" == "true" ]]; then
        args+=(-check)
    else
        args+=(-out "$out" -tarball "$REPO_ROOT/docs-${version}.tgz")
    fi
    summary="$("$SCRATCH/docs-gen" "${args[@]}")" || rc=$?
    if [[ -n "$summary" ]]; then
        cap_result_set_raw "bundle" "$summary"
    fi
    case "$rc" in
        0) ;;
        2) cap_fail 2 "docs-gen refused its parameters (its message is above)" ;;
        4) cap_fail 4 "a precondition failed (docs-gen's message is above): a full-history git checkout with every page committed is required to build" ;;
        6) cap_fail 6 "the docs break the public boundary: every violation is listed above and in result.bundle.violations; nothing was written" ;;
        *) cap_fail 5 "docs-gen bundle failed with exit ${rc} (its message is above)" ;;
    esac
    return 0
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local version out site check dry tarball
    version="$(cap_param version "")"
    cap_require version "$version"
    out="$(cap_param out "$REPO_ROOT/docs-bundle")"
    site="$(cap_param siteUrl "https://memql.io")"
    check="$(cap_bool_str check false)"
    dry="$(cap_bool_str dryRun false)"
    tarball="$REPO_ROOT/docs-${version}.tgz"

    check_version "$version"
    cap_result_set "version" "$version"
    cap_result_set_raw "check" "$check"
    if [[ "$check" == "false" ]]; then
        cap_result_set "tarball" "$tarball"
        cap_result_set "out" "$out"
    fi

    if [[ "$dry" == "true" ]]; then
        if [[ "$check" == "true" ]]; then
            cap_info "[dry-run] would check docs/public against the public boundary for ${version}, writing nothing"
        else
            cap_info "[dry-run] would build docs-${version}.tgz at ${tarball} from docs/public, with the tree in ${out}"
        fi
        cap_result_set_raw "dryRun" true
        cap_ok
    fi
    cap_result_set_raw "dryRun" false

    require_tools
    compile_docs_gen
    run_docs_gen "$version" "$out" "$check" "$site"
    if [[ "$check" == "false" ]]; then
        cap_changed
        cap_info "wrote ${tarball}"
    fi
    cap_ok
}

main "$@"
