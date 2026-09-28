#!/usr/bin/env bash
set -uo pipefail
#
# scripts/docs/build-docs-bundle.sh
# =================================
#
# Build the per-release documentation bundle that memql.io consumes
# (znasllc-io/memql#1171). The bundle is:
#
#   docs/public/**  (files whose front-matter is `audience: public`)
#   + machine-generated reference (cmd/docs-gen concept catalog, and a
#     best-effort architecture model from cmd/memql-arch)
#   + manifest.json (version, page count, areas, and a nav tree by area)
#
# packaged as docs-<version>.tgz at the repo root. The release pipeline
# (#1172) calls this and attaches the tarball to the GitHub Release; the
# site unpacks it into a per-version content dir. Docs version == engine
# release (see VERSIONING.md).
#
# The version is checked, not trusted: it must be X.Y.Z and equal the first
# line of the VERSION file, which equals the tag of the commit a release cut
# tags (VERSIONING.md, memql#5714). The bundle is built at the tag, so the two
# agree on every release; a mismatch means VERSION lagged the tag, and a bundle
# built anyway would publish docs labelled with one release from a tree that
# says it is another -- how bundle 0.21.25 went out naming engine 0.15.0. The
# check runs before anything is generated.
#
# Usage:
#   build-docs-bundle.sh --version=X.Y.Z [--out=DIR] [--dry-run]
#
#   --version   Version label stamped into the manifest + tarball name.
#               Must be X.Y.Z and equal VERSION's first line.
#               Default: the VERSION file.
#   --out       Staging dir for the bundle tree. Default: <root>/docs-bundle.
#   --dry-run   Print the selection plan; generate + package nothing.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PUBLIC_DIR="$REPO_ROOT/docs/public"
GENERATED_DIR="$PUBLIC_DIR/reference/_generated"

function show_help() { sed -n '2,35p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
function info() { echo "INFO: $*"; }
function warn() { echo "WARNING: $*" >&2; }
function die()  { echo "ERROR: $*" >&2; exit 1; }

function parse_arguments() {
    VERSION=""
    OUT="$REPO_ROOT/docs-bundle"
    DRY_RUN=false
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version=*) VERSION="${1#*=}"; shift ;;
            --out=*)     OUT="${1#*=}"; shift ;;
            --dry-run)   DRY_RUN=true; shift ;;
            --help)      show_help; exit 0 ;;
            *) die "unknown option: $1" ;;
        esac
    done
    local file_version
    file_version="$(sed -n '1p' "$REPO_ROOT/VERSION" 2>/dev/null | tr -d '[:space:]')"
    [ -n "$file_version" ] || die "no VERSION file at $REPO_ROOT/VERSION; the bundle's version is checked against it"
    if [ -z "$VERSION" ]; then
        VERSION="$file_version"
    fi
    [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        || die "--version '$VERSION' is not X.Y.Z (bare, no leading v): it names docs-<version>.tgz, which the site fetches by the bare version"
    [ "$VERSION" = "$file_version" ] \
        || die "--version $VERSION differs from VERSION ($file_version). VERSION equals the tag a release is cut at (VERSIONING.md), so a bundle whose version disagrees with it would label one release's docs with another's number. Build at the tag whose VERSION reads $VERSION."
    [ -d "$PUBLIC_DIR" ] || die "docs/public not found at $PUBLIC_DIR"
}

# generate_reference runs the DB-free generators into docs/public/reference/_generated/.
# v1 ships the concept catalog (cmd/docs-gen). The architecture model
# (cmd/memql-arch) is a heavier workspace walk already produced for the
# cockpit; wiring it into the bundle is a follow-up.
function generate_reference() {
    info "generating concept catalog (cmd/docs-gen)..."
    ( cd "$REPO_ROOT" && GOWORK=off go run ./cmd/docs-gen -out "docs/public/reference/_generated" ) \
        || die "docs-gen failed"
}

# build_bundle selects audience:public docs + generated assets into $OUT and
# writes manifest.json. Python does the front-matter parsing + nav assembly.
function build_bundle() {
    rm -rf "$OUT"
    mkdir -p "$OUT"
    PUBLIC_DIR="$PUBLIC_DIR" OUT="$OUT" VERSION="$VERSION" \
        python3 "$SCRIPT_DIR/_bundle.py"
}

function package() {
    local tarball="$REPO_ROOT/docs-${VERSION}.tgz"
    tar -czf "$tarball" -C "$OUT" .
    info "wrote $tarball ($(du -h "$tarball" | cut -f1 | tr -d ' '))"
}

function main() {
    parse_arguments "$@"
    info "docs bundle: version=$VERSION"
    if [ "$DRY_RUN" = true ]; then
        local n
        n="$(grep -rl '^audience: public' "$PUBLIC_DIR" --include='*.md' 2>/dev/null | wc -l | tr -d ' ')"
        echo "PLAN: would generate reference, select ~${n} public docs, write manifest.json, and package docs-${VERSION}.tgz"
        echo "PLAN: (dry-run) nothing generated or written"
        exit 0
    fi
    generate_reference
    build_bundle
    package
    info "done"
}

main "$@"
