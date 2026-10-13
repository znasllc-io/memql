#!/usr/bin/env bash
# Copy one already-published engine image from ACR to its missing GHCR tag.
# This runs on the build server, never builds, and never replaces another digest.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
cap_init "release.recoverEngineMirror" "Recover a missing GHCR release image from its verified, immutable ACR digest."
cap_spec_param_required version "Bare release version X.Y.Z"
cap_spec_param_required sourceSha "Full commit named by the published release tag"
cap_spec_param_required node "Engine node: identity, mcp, bff, agent, planner, workbench, edge"
cap_spec_param_required digest "Exact sha256 digest already published in ACR"
cap_handle_meta "$@"
cap_parse_flags "$@"

VERSION="$(cap_param version '')"
SOURCE_SHA="$(cap_param sourceSha '')"
NODE="$(cap_param node '')"
DIGEST="$(cap_param digest '')"
REPO="znasllc-io/memql"
SCRATCH=""
CONTAINER=""
TARGET_PRESENT="false"

# Invoked by the EXIT trap below, including cap_fail paths.
# shellcheck disable=SC2329
function cleanup() {
    if [[ -n "$CONTAINER" ]]; then docker rm "$CONTAINER" >&2 || true; fi
    if [[ -n "$SCRATCH" ]]; then rm -rf "$SCRATCH"; fi
    return 0
}
# Preserve the capability's exactly-one-result guarantee before cleaning up.
trap '_cap_on_exit; cleanup' EXIT

function validate_inputs() {
    [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || cap_fail 2 "version must be bare X.Y.Z"
    [[ "$SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]] || cap_fail 2 "sourceSha must be a full lowercase commit SHA"
    [[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || cap_fail 2 "digest must be an exact lowercase sha256 digest"
    case "$NODE" in identity|mcp|bff|agent|planner|workbench|edge) ;; *) cap_fail 2 "unknown engine node" ;; esac
    [[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == "$REPO" && "${GITHUB_REF:-}" == refs/heads/main ]] \
        || cap_fail 3 "recovery runs only in the repository's GitHub Actions workflow on main"
    SOURCE_TAG="acrmemql.azurecr.io/memql-${NODE}:${VERSION}"
    SOURCE="acrmemql.azurecr.io/memql-${NODE}@${DIGEST}"
    TARGET_REPO="ghcr.io/znasllc-io/memql-${NODE}"
    TARGET="${TARGET_REPO}:${VERSION}"
    return 0
}

function check_prerequisites() {
    local tool
    for tool in gh jq python3 skopeo docker go; do
        command -v "$tool" >/dev/null || cap_fail 4 "required tool missing: ${tool}"
    done
    SCRATCH="$(mktemp -d)"
    return 0
}

function sha256_file() {
    python3 -c 'import hashlib,sys; print("sha256:" + hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$1"
}

function verify_release() {
    gh release view "v${VERSION}" --repo "$REPO" --json tagName,isDraft,publishedAt > "$SCRATCH/release.json" \
        || cap_fail 5 "could not read the published release"
    jq -e --arg tag "v${VERSION}" '.tagName == $tag and .isDraft == false and (.publishedAt | type == "string" and length > 0)' "$SCRATCH/release.json" >/dev/null \
        || cap_fail 3 "release must be published under the exact version tag"
    local sha version
    sha="$(gh api "repos/${REPO}/commits/v${VERSION}" --jq .sha)" || cap_fail 5 "could not resolve the release tag"
    [[ "$sha" == "$SOURCE_SHA" ]] || cap_fail 3 "release tag does not name sourceSha"
    version="$(gh api "repos/${REPO}/contents/VERSION?ref=${SOURCE_SHA}" -H 'Accept: application/vnd.github.raw+json')" \
        || cap_fail 5 "could not read VERSION at the release commit"
    [[ "$version" == "$VERSION" ]] || cap_fail 3 "VERSION at the release commit does not match version"
    return 0
}

function inspect_manifest() {
    skopeo inspect --raw "docker://$1" > "$2" || cap_fail 5 "could not read manifest: $1"
    return 0
}

function verify_source() {
    inspect_manifest "$SOURCE_TAG" "$SCRATCH/source-tag.json"
    [[ "$(sha256_file "$SCRATCH/source-tag.json")" == "$DIGEST" ]] || cap_fail 3 "ACR release tag does not match the requested digest"
    inspect_manifest "$SOURCE" "$SCRATCH/source.json"
    [[ "$(sha256_file "$SCRATCH/source.json")" == "$DIGEST" ]] || cap_fail 3 "ACR digest response does not match its raw manifest"
    # The release builder publishes one linux/amd64 image without attestations.
    # Refuse an index instead of selecting one child and claiming to verify all.
    jq -e '.schemaVersion == 2 and (.mediaType == "application/vnd.oci.image.manifest.v1+json" or .mediaType == "application/vnd.docker.distribution.manifest.v2+json") and (.config.digest | test("^sha256:[0-9a-f]{64}$"))' "$SCRATCH/source.json" >/dev/null \
        || cap_fail 3 "source must be a single release image manifest with a sha256 config"
    skopeo inspect --raw --config "docker://${SOURCE}" > "$SCRATCH/config.json" || cap_fail 5 "could not read source image configuration"
    [[ "$(sha256_file "$SCRATCH/config.json")" == "$(jq -r .config.digest "$SCRATCH/source.json")" ]] \
        || cap_fail 3 "source config does not match the manifest's digest"
    jq -e --arg version "$VERSION" --arg sha "$SOURCE_SHA" '
        .os == "linux" and .architecture == "amd64" and
        .config.Labels["org.opencontainers.image.source"] == "https://github.com/znasllc-io/memql" and
        .config.Labels["org.opencontainers.image.version"] == $version and
        ((.config.Labels["org.opencontainers.image.revision"] // $sha) == $sha)
    ' "$SCRATCH/config.json" >/dev/null || cap_fail 3 "source image platform or release labels do not match"

    # Existing release images have no revision label. Read Go's build metadata
    # from the binary without executing any image code; both linked buildinfo
    # values and the compiled node/platform must agree with the release.
    docker pull --platform linux/amd64 "$SOURCE" >&2 || cap_fail 5 "could not pull verified source for binary inspection"
    CONTAINER="$(docker create --platform linux/amd64 "$SOURCE")" || cap_fail 5 "could not create stopped inspection container"
    docker cp "${CONTAINER}:/app/memql" "$SCRATCH/memql" >&2 || cap_fail 5 "could not extract engine binary"
    docker rm "$CONTAINER" >&2 || cap_fail 5 "could not remove inspection container"
    CONTAINER=""
    go version -m "$SCRATCH/memql" > "$SCRATCH/build-info.txt" || cap_fail 3 "source does not contain a readable Go engine binary"
    python3 "$SCRIPT_DIR/verify-engine-build-info.py" --info "$SCRATCH/build-info.txt" --version "$VERSION" --source-sha "$SOURCE_SHA" --node "$NODE" \
        || cap_fail 3 "engine binary provenance does not match the release"
    return 0
}

function inspect_target() {
    # A failed lookup is not absence. Only a successful tag listing permits a
    # write; auth failures, missing repositories and outages all fail closed.
    skopeo list-tags "docker://${TARGET_REPO}" > "$SCRATCH/tags.json" || cap_fail 5 "could not establish which GHCR tags exist"
    jq -e '.Tags | type == "array" and all(.[]; type == "string")' "$SCRATCH/tags.json" >/dev/null || cap_fail 5 "invalid GHCR tag listing"
    TARGET_PRESENT="false"
    if jq -e --arg version "$VERSION" '.Tags | index($version) != null' "$SCRATCH/tags.json" >/dev/null; then
        inspect_manifest "$TARGET" "$SCRATCH/target.json"
        [[ "$(sha256_file "$SCRATCH/target.json")" == "$DIGEST" ]] || cap_fail 3 "GHCR release tag already exists with a different digest; refusing to replace it"
        TARGET_PRESENT="true"
    fi
    return 0
}

function copy_missing_target() {
    # Recheck immediately before the write. The workflow shares the builder's
    # per-version concurrency group, so repository-controlled writers serialize.
    inspect_target
    if [[ "$TARGET_PRESENT" == true ]]; then
        cap_result_set status already-present
        return 0
    fi
    # Mark the attempt before copy: an interrupted registry upload may have
    # side effects even if skopeo cannot report completion. Reruns re-inspect.
    cap_changed
    skopeo copy --all --preserve-digests "docker://${SOURCE}" "docker://${TARGET}" >&2 \
        || cap_fail 5 "digest-preserving copy failed; inspect the target before retrying"
    inspect_manifest "$TARGET" "$SCRATCH/copied.json"
    [[ "$(sha256_file "$SCRATCH/copied.json")" == "$DIGEST" ]] || cap_fail 5 "GHCR raw manifest digest differs after copy"
    cap_result_set status recovered
    return 0
}

function main() {
    validate_inputs
    check_prerequisites
    verify_release
    verify_source
    inspect_target
    copy_missing_target
    cap_result_set source "$SOURCE"
    cap_result_set target "$TARGET"
    cap_result_set digest "$DIGEST"
    cap_result_set sourceSha "$SOURCE_SHA"
    cap_result_set version "$VERSION"
    cap_result_set node "$NODE"
    # Emit the accumulated result, not the script's input arguments.
    # shellcheck disable=SC2119
    cap_ok
}

main "$@"
