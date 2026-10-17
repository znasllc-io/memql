#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
source "$REPO_ROOT/scripts/macos/build-scratch.sh"
cap_init "cockpit.menubar.package" "Package the native menu app and standalone installer for this Mac architecture."
cap_spec_param "arch" "Native release architecture: arm64 or amd64"
cap_spec_param "output" "Output directory (default: dist)"
cap_spec_param "version" "Release version (default: repository VERSION)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local arch output version stage asset expected_arch
    arch="$(cap_param arch)"
    output="$(cap_param output "$REPO_ROOT/dist")"
    version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")"
    [[ "$arch" == arm64 || "$arch" == amd64 ]] || cap_fail 2 "arch must be arm64 or amd64"
    [[ "$output" == /* ]] || cap_fail 2 "output must be absolute"
    create_build_scratch memql-menu-package
    stage="$COCKPIT_BUILD_SCRATCH"
    "$REPO_ROOT/scripts/macos/build-menubar.sh" --output="$stage/MemQL Cockpit.app" --version="$version" --arch="$arch" >&2 || cap_fail 5 "menu build failed"
    expected_arch="$arch"
    [[ "$arch" != amd64 ]] || expected_arch=x86_64
    [[ "$(lipo -archs "$stage/MemQL Cockpit.app/Contents/MacOS/MemQLCockpit")" == "$expected_arch" ]] || cap_fail 3 "native binary architecture does not match requested asset"
    mkdir -p "$stage/scripts/macos" "$stage/scripts/lib" "$output"
    cp "$REPO_ROOT/scripts/macos/"{install-menubar,launchagent}.sh "$stage/scripts/macos/"
    cp "$REPO_ROOT/../memql/scripts/lib/capability.sh" "$stage/scripts/lib/"
    asset="memql-menubar-darwin-${arch}.tar.gz"
    COPYFILE_DISABLE=1 tar -czf "$output/$asset" -C "$stage" 'MemQL Cockpit.app' scripts
    (cd "$output" && shasum -a 256 "$asset" > "$asset.sha256")
    cleanup_build_scratch || cap_fail 5 "build scratch cleanup failed"
    cap_changed
    cap_result_set archive "$output/$asset"
    cap_ok
}
main "$@"
