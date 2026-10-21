#!/usr/bin/env bash
set -euo pipefail

# Development only: reuse a certificate already provisioned in this user's
# Keychain. Never generate a replacement or silently fall back to ad-hoc.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
cap_init "cockpit.app.build-local" "Build MemQL.app with the saved local signing identity."
cap_spec_param "worker" "Absolute path to the built macOS computer-use worker"
cap_spec_param "output" "Absolute output .app path"
cap_spec_param "version" "Version matching the worker"
cap_spec_param "signing-identity-file" "File containing the SHA-1 of the local Keychain signing certificate"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local identity_file signing_identity identities
    identity_file="$(cap_param signing-identity-file "$HOME/Library/Application Support/MemQL/Development/signing-identity")"
    [[ -f "$identity_file" && -r "$identity_file" ]] \
        || cap_fail 4 "Local signing identity is not configured; see docs/macos-menu.md."
    signing_identity="$(cat "$identity_file")"
    [[ "$signing_identity" =~ ^[[:xdigit:]]{40}$ ]] \
        || cap_fail 3 "Local signing identity must contain one certificate SHA-1, never an ad-hoc signature."
    identities="$(security find-identity -p codesigning)" \
        || cap_fail 4 "Could not read signing identities from the Keychain."
    # Self-signed development certificates can be usable without being trusted
    # distribution identities. codesign performs the actual signing validation.
    awk -v identity="$signing_identity" '$2 == toupper(identity) { found=1 } END { exit !found }' <<< "$identities" \
        || cap_fail 4 "The configured certificate and private key are not in the Keychain; restore that identity instead of replacing it."
    exec "$REPO_ROOT/scripts/macos/build-app.sh" \
        --worker="$(cap_param worker)" \
        --output="$(cap_param output "$REPO_ROOT/bin/MemQL.app")" \
        --version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")" \
        --signing-identity="$signing_identity"
}
main "$@"
