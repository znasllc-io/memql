#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"

cap_init "ci.delivery-artifacts" "Qualify artifact receipts and publication against disposable local providers."
cap_spec_param_required "database-image" "Digest-pinned TimescaleDB and pgvector fixture image."
cap_spec_param_required "blob-image" "Digest-pinned official Azurite fixture image."
cap_spec_param_required "registry-image" "Digest-pinned official Distribution fixture image."
cap_spec_param_required "artifact-image" "Digest-pinned real image with a linux/arm64 manifest to publish."
cap_spec_param_required "output" "Directory for retained logs, OCI archive, and the qualification report."

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local database_image blob_image registry_image artifact_image output result status
    database_image="$(cap_param database-image)"
    blob_image="$(cap_param blob-image)"
    registry_image="$(cap_param registry-image)"
    artifact_image="$(cap_param artifact-image)"
    output="$(cap_param output)"
    cap_require "database-image" "$database_image"
    cap_require "blob-image" "$blob_image"
    cap_require "registry-image" "$registry_image"
    cap_require "artifact-image" "$artifact_image"
    cap_require "output" "$output"
    command -v python3 >/dev/null || cap_fail 4 "python3 is required"
    status=0
    result="$(python3 "$(dirname "${BASH_SOURCE[0]}")/delivery_artifacts.py" \
        --database-image="$database_image" --blob-image="$blob_image" \
        --registry-image="$registry_image" --artifact-image="$artifact_image" \
        --output="$output")" || status=$?
    cap_result_set_raw qualification "${result:-null}"
    # Successful prerequisites create an evidence directory even when a test fails.
    if [[ -n "$result" ]]; then cap_changed; fi
    if [[ "$status" -ne 0 ]]; then
        cap_fail "$status" "Artifact qualification did not pass; see retained evidence."
    fi
    cap_ok
}

main "$@"
