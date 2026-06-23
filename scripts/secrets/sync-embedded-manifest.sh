#!/usr/bin/env bash
# Sync the embedded genesis manifest snapshot from the authored registry.
#
# scripts/secrets/manifest.yaml is the source of truth (Epic 7 / memql#2104).
# component/genesis/manifest.yaml is a //go:embed snapshot baked into the
# binary as the last-resort fallback (loader priority 4). The two must carry
# identical secrets/variables; this script regenerates the snapshot so they
# can't drift. Run it after any edit to the authored manifest.
# TestEmbeddedManifestInSync (component/genesis) fails CI if they diverge.
set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SOURCE="$REPO_ROOT/scripts/secrets/manifest.yaml"
SNAPSHOT="$REPO_ROOT/component/genesis/manifest.yaml"

#=============================================================================
# FUNCTIONS
#=============================================================================

function write_banner() {
    cat > "$SNAPSHOT" <<'BANNER'
# memQL env-var registry -- EMBEDDED SNAPSHOT (generated, do not edit)
# ============================================================================
# This is a //go:embed snapshot of scripts/secrets/manifest.yaml baked into
# the binary as the last-resort fallback (loader priority 4), so `genesis
# init` works on operator machines without a memql checkout.
#
# DO NOT EDIT BY HAND. Regenerate with:
#     bash scripts/secrets/sync-embedded-manifest.sh
# TestEmbeddedManifestInSync fails CI if this drifts from the authored file.
# ============================================================================

BANNER
}

function append_registry() {
    cat "$SOURCE" >> "$SNAPSHOT"
}

function main() {
    if [[ ! -f "$SOURCE" ]]; then
        echo "ERROR: authored manifest not found: $SOURCE" >&2
        exit 1
    fi
    write_banner
    append_registry
    echo "INFO: synced embedded snapshot <- $SOURCE"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
