#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../scripts/lib/capability.sh"
source "$REPO_ROOT/scripts/macos/build-scratch.sh"
cap_init "cockpit.app.package" "Package the native MemQL worker app and installation capabilities."
cap_spec_param "worker" "Absolute built computer-use worker path"
cap_spec_param "arch" "arm64 or amd64"
cap_spec_param "output" "Absolute output directory"
cap_spec_param "version" "Version matching the worker"

function main() {
    cap_handle_meta "$@"; cap_parse_flags "$@"
    local worker arch output version stage expected asset
    worker="$(cap_param worker)"; arch="$(cap_param arch)"
    output="$(cap_param output "$REPO_ROOT/dist")"; version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")"
    [[ "$arch" == arm64 || "$arch" == amd64 ]] || cap_fail 2 "arch must be arm64 or amd64"
    [[ "$output" == /* ]] || cap_fail 2 "output must be absolute"
    create_build_scratch memql-app-package
    stage="$COCKPIT_BUILD_SCRATCH"
    "$REPO_ROOT/scripts/macos/build-app.sh" --worker="$worker" --output="$stage/MemQL.app" --version="$version" >&2 || cap_fail 5 "app build failed"
    expected="$arch"; [[ "$arch" != amd64 ]] || expected=x86_64
    [[ "$(lipo -archs "$stage/MemQL.app/Contents/MacOS/MemQL")" == "$expected" ]] || cap_fail 3 "worker architecture mismatch"
    [[ "$(lipo -archs "$stage/MemQL.app/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit")" == "$expected" ]] || cap_fail 3 "menu architecture mismatch"
    mkdir -p "$stage/scripts/macos" "$stage/scripts/lib" "$output"
    cp "$REPO_ROOT/scripts/macos/"{install-app-files,activate-app,install-menubar,launchagent}.sh "$stage/scripts/macos/"
    cp "$REPO_ROOT/../scripts/lib/capability.sh" "$stage/scripts/lib/"
    asset="memql-app-darwin-$arch.tar.gz"
    COPYFILE_DISABLE=1 tar -czf "$output/$asset" -C "$stage" MemQL.app scripts
    (cd "$output" && shasum -a 256 "$asset" > "$asset.sha256")
    cleanup_build_scratch || cap_fail 5 "build scratch cleanup failed"
    cap_changed; cap_result_set archive "$output/$asset"; cap_ok
}
main "$@"
