#!/usr/bin/env bash
#
# scripts/release/release.sh
# ==========================
#
# Cut an IMMUTABLE memQL release image for the Azure deployment
# foundation (znasllc-io/memql#493, epic #491).
#
# The engine is the whole platform: this repo builds EVERY node type
# (identity/bff/cognition/agent/planner/voice/workbench/mcp) as a
# product-agnostic image from docker/memql.Dockerfile. Product DSL is
# NOT compiled in -- it is delivered at RUNTIME: a product ships its
# DSL as a tiny data-only bundle image, an init-container copies the
# tree into a shared volume, and dsl.MountRuntimeDomainsFromEnv mounts
# each domain via RegisterTree(os.DirFS(...)) at MEMQL_DSL_PATH on boot
# (see component/memql/dslfs + docs/internal/design/platform-consolidation.md).
# So a plain engine image runs any product with zero product code, and
# there is no <product>-carrier repo compile-linking the DSL.
#
# This script cuts the immutable engine image. Its tag is the
# engine-version leg of a release's {engine version, bundle digest,
# client digest} triple, pinned in one deploy overlay:
#
#     memql:X.Y.Z   <-- the engine-version leg of the release triple
#
# This script produces that image. Given VERSION (semver, e.g.
# 2.4.0) and the current short git SHA it builds, from the repo's
# docker/memql.Dockerfile, an image tagged:
#
#     <REGISTRY/>memql:X.Y.Z         (the pinnable, immutable tag)
#
# and stamps the build with the exact source revision as an OCI
# label (org.opencontainers.image.revision=<sha>) so the image is
# traceable back to a commit. The X.Y.Z tag is treated as
# write-once: pushing over an existing tag is refused unless
# --allow-overwrite is given, which is what makes the tag a
# trustworthy engine-version pin for a release.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, with
# main() at the bottom calling them in order. Supports --help and a
# --dry-run that prints the full plan and builds/pushes nothing.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Shared Basic ACR from the locked epic architecture (#491). ACR
# names are global + alphanumeric-only (no dashes); the login server
# is <name>.azurecr.io. Empty REGISTRY builds a local-only image
# (memql:X.Y.Z with no registry prefix), which is the default so the
# target works before a subscription exists.
readonly DEFAULT_ACR_NAME="acrmemql"
readonly IMAGE_NAME="memql"
readonly DOCKERFILE="docker/memql.Dockerfile"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: scripts/release/release.sh [options]

Cut an immutable memql:X.Y.Z engine image (VERSION + short git SHA) --
the engine-version leg of a release's {engine, bundle, client} triple.

Options:
    --version=X.Y.Z     Semver to tag the image with. Default: the
                        semver prefix of the VERSION file (the part
                        before the first '-').
    --registry=HOST     Container registry login server to prefix +
                        push to (e.g. acrmemql.azurecr.io). Empty =
                        build a local-only image, no push.
    --acr=NAME          Shorthand: derive --registry from an ACR name
                        (NAME.azurecr.io). Default ACR: $DEFAULT_ACR_NAME
                        (only used if --registry/--acr is given).
    --push              Push the built tag to the registry. Requires
                        a registry. Refused if the tag already exists
                        unless --allow-overwrite.
    --allow-overwrite   Permit pushing over an existing X.Y.Z tag.
                        Off by default: release tags are immutable.
    --dry-run           Print the full plan; build/push nothing.
    --help              Show this help.

Examples:
    # Local immutable image from the VERSION file's semver prefix:
    scripts/release/release.sh

    # Explicit version, build + push to the shared ACR:
    scripts/release/release.sh --version=2.4.0 --acr=$DEFAULT_ACR_NAME --push

    # Plan only:
    scripts/release/release.sh --version=2.4.0 --acr=$DEFAULT_ACR_NAME --push --dry-run
EOF
}

function parse_arguments() {
    VERSION=""
    REGISTRY=""
    ACR_NAME=""
    PUSH=false
    ALLOW_OVERWRITE=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version=*)         VERSION="${1#*=}" ;;
            --registry=*)        REGISTRY="${1#*=}" ;;
            --acr=*)             ACR_NAME="${1#*=}" ;;
            --push)              PUSH=true ;;
            --allow-overwrite)   ALLOW_OVERWRITE=true ;;
            --dry-run)           DRY_RUN=true ;;
            --help)              show_help; exit 0 ;;
            *)
                echo "ERROR: unknown option: $1" >&2
                show_help >&2
                exit 1
                ;;
        esac
        shift
    done
}

function resolve_repo_root() {
    # This script lives at scripts/release/; the repo root is two
    # directories up. Resolve it so the target works from anywhere.
    local here
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    REPO_ROOT="$(cd "${here}/../.." && pwd)"
}

function resolve_version() {
    if [[ -z "$VERSION" ]]; then
        if [[ ! -f "${REPO_ROOT}/VERSION" ]]; then
            echo "ERROR: no --version given and no VERSION file found" >&2
            exit 1
        fi
        # The VERSION file is "X.Y.Z-<epoch>"; the release tag is the
        # clean semver prefix (everything before the first '-').
        VERSION="$(head -n1 "${REPO_ROOT}/VERSION" | cut -d- -f1)"
    fi

    # Validate strict semver X.Y.Z (the immutable image tag must be a
    # clean three-part version -- no pre-release/epoch suffix).
    if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "ERROR: --version must be clean semver X.Y.Z (got: '$VERSION')" >&2
        echo "       The VERSION file carries an epoch suffix for dev builds;" >&2
        echo "       a release tag drops it. Pass --version=X.Y.Z explicitly." >&2
        exit 1
    fi
}

function resolve_sha() {
    # Short SHA stamps the image's revision label so an immutable
    # X.Y.Z tag is traceable back to an exact commit. A dirty tree is
    # flagged so we never cut a "clean" release from uncommitted work.
    SHORT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "unknown")"
    if [[ -n "$(git -C "${REPO_ROOT}" status --porcelain 2>/dev/null)" ]]; then
        SHORT_SHA="${SHORT_SHA}-dirty"
    fi
}

function resolve_registry() {
    # --registry wins; else derive from --acr; else (neither given)
    # leave REGISTRY empty for a local-only build.
    if [[ -z "$REGISTRY" && -n "$ACR_NAME" ]]; then
        REGISTRY="${ACR_NAME}.azurecr.io"
    fi

    if [[ -n "$REGISTRY" ]]; then
        IMAGE_REF="${REGISTRY}/${IMAGE_NAME}:${VERSION}"
    else
        IMAGE_REF="${IMAGE_NAME}:${VERSION}"
    fi
}

function validate_push() {
    if [[ "$PUSH" == true && -z "$REGISTRY" ]]; then
        echo "ERROR: --push requires a registry (--registry=HOST or --acr=NAME)" >&2
        exit 1
    fi
}

function check_prerequisites() {
    if ! command -v docker >/dev/null 2>&1; then
        echo "ERROR: docker is not installed" >&2
        exit 1
    fi
}

function print_plan() {
    echo "========================================="
    echo "memQL release image"
    echo "========================================="
    echo "Version:    $VERSION"
    echo "Source SHA: $SHORT_SHA"
    echo "Image:      $IMAGE_REF"
    echo "Dockerfile: $DOCKERFILE"
    echo "Push:       $PUSH"
    echo "Overwrite:  $ALLOW_OVERWRITE"
    echo "Dry run:    $DRY_RUN"
    echo "========================================="
}

function ensure_tag_immutable() {
    # Only meaningful when pushing to a real registry. Refuse to clobber
    # an existing X.Y.Z tag unless explicitly allowed -- that is what
    # makes the tag a trustworthy engine-version pin for a release.
    if [[ "$PUSH" != true || -z "$REGISTRY" || "$ALLOW_OVERWRITE" == true ]]; then
        return 0
    fi
    if docker manifest inspect "$IMAGE_REF" >/dev/null 2>&1; then
        echo "ERROR: tag already exists in registry: $IMAGE_REF" >&2
        echo "       Release tags are immutable. Bump --version, or pass" >&2
        echo "       --allow-overwrite to deliberately re-cut this tag." >&2
        exit 1
    fi
}

function build_image() {
    local -a build_args=(
        docker build
        --platform "${BUILD_PLATFORM:-linux/amd64}"
        -f "${REPO_ROOT}/${DOCKERFILE}"
        -t "$IMAGE_REF"
        --label "org.opencontainers.image.version=${VERSION}"
        --label "org.opencontainers.image.revision=${SHORT_SHA}"
        "${REPO_ROOT}"
    )

    if [[ "$DRY_RUN" == true ]]; then
        echo "[plan] ${build_args[*]}"
        return 0
    fi
    "${build_args[@]}"
}

function push_image() {
    if [[ "$PUSH" != true ]]; then
        return 0
    fi
    if [[ "$DRY_RUN" == true ]]; then
        echo "[plan] docker push ${IMAGE_REF}"
        return 0
    fi
    docker push "$IMAGE_REF"
}

function print_result() {
    echo ""
    if [[ "$DRY_RUN" == true ]]; then
        echo "DRY RUN complete -- built/pushed nothing. Planned image: $IMAGE_REF"
    else
        echo "Release image ready: $IMAGE_REF (revision ${SHORT_SHA})"
        if [[ "$PUSH" == true ]]; then
            echo "Pushed. This is the engine-version leg of the {engine, bundle, client} triple, pinned as memql:${VERSION} in the deploy overlay."
        else
            echo "Local only. Re-run with --push (and a registry) to publish."
        fi
    fi
}

function main() {
    parse_arguments "$@"
    resolve_repo_root
    resolve_version
    resolve_sha
    resolve_registry
    validate_push
    check_prerequisites
    print_plan
    ensure_tag_immutable
    build_image
    push_image
    print_result
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
