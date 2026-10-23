#!/usr/bin/env bash
# Offline archive boundary tests; no installed apps or services are touched.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

function show_help() { echo "Usage: $0 [--help] — exercise menu archive verification offline"; }
function main() {
    [[ "${1:-}" != --help ]] || { show_help; return; }
    local root asset fixture stage
    root="$(mktemp -d)"
    trap 'rm -rf "$root"' EXIT
    fixture="$root/fixture"
    asset=memql-menubar-darwin-arm64.tar.gz
    mkdir -p "$fixture/MemQL Cockpit.app/Contents/MacOS" "$fixture/scripts/macos" "$fixture/scripts/lib" "$root/assets"
    printf '#!/bin/sh\nexit 0\n' > "$fixture/MemQL Cockpit.app/Contents/MacOS/MemQLCockpit"
    chmod +x "$fixture/MemQL Cockpit.app/Contents/MacOS/MemQLCockpit"
    touch "$fixture/scripts/macos/install-menubar.sh" "$fixture/scripts/lib/capability.sh"
    COPYFILE_DISABLE=1 tar -czf "$root/assets/$asset" -C "$fixture" 'MemQL Cockpit.app' scripts
    (cd "$root/assets" && shasum -a 256 "$asset" > "$asset.sha256")
    stage="$root/valid"; mkdir "$stage"
    fetch_macos_menu "file://$root/assets" arm64 "$stage"
    [[ -x "$stage/unpacked/MemQL Cockpit.app/Contents/MacOS/MemQLCockpit" ]]
    echo 'PASS: valid menu archive verifies and extracts'
    printf tampered >> "$root/assets/$asset"
    stage="$root/corrupt"; mkdir "$stage"
    if fetch_macos_menu "file://$root/assets" arm64 "$stage"; then echo 'FAIL: corrupt archive accepted' >&2; exit 1; fi
    [[ ! -e "$stage/unpacked" ]]
    echo 'PASS: corrupt archive rejected before extraction'
    touch "$fixture/unexpected"
    COPYFILE_DISABLE=1 tar -czf "$root/assets/$asset" -C "$fixture" 'MemQL Cockpit.app' scripts unexpected
    (cd "$root/assets" && shasum -a 256 "$asset" > "$asset.sha256")
    stage="$root/extra"; mkdir "$stage"
    if fetch_macos_menu "file://$root/assets" arm64 "$stage"; then echo 'FAIL: extra path accepted' >&2; exit 1; fi
    [[ ! -e "$stage/unpacked" ]]
    echo 'PASS: unrecognized archive path rejected'
    rm "$fixture/scripts/lib/capability.sh"
    ln -s /tmp "$fixture/scripts/lib/capability.sh"
    COPYFILE_DISABLE=1 tar -czf "$root/assets/$asset" -C "$fixture" 'MemQL Cockpit.app' scripts
    (cd "$root/assets" && shasum -a 256 "$asset" > "$asset.sha256")
    stage="$root/link"; mkdir "$stage"
    if fetch_macos_menu "file://$root/assets" arm64 "$stage"; then echo 'FAIL: link accepted' >&2; exit 1; fi
    [[ ! -e "$stage/unpacked" ]]
    echo 'PASS: archive links rejected before extraction'
    rm -rf "$root"
    trap - EXIT
}
main "$@"
