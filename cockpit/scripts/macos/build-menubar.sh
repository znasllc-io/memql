#!/usr/bin/env bash
set -euo pipefail

# Native menu companion only. The worker stays a separate LaunchAgent.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../scripts/lib/capability.sh"
source "$REPO_ROOT/scripts/macos/build-scratch.sh"
cap_init "cockpit.menubar.build" "Build the native macOS menu companion."
cap_spec_param "output" "Output .app bundle (default: bin/MemQL Cockpit.app)"
cap_spec_param "version" "Display version (default: repository VERSION)"
cap_spec_param "arch" "Release architecture: arm64 or amd64 (default: this Mac)"
cap_spec_param "signing-identity" "Code-signing certificate name or SHA-1 (default: ad-hoc development signature)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local output version stage arch target_arch signing_identity
    output="$(cap_param output "$REPO_ROOT/bin/MemQL Cockpit.app")"
    version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")"
    arch="$(cap_param arch "$(uname -m)")"
    signing_identity="$(cap_param signing-identity -)"
    case "$arch" in
        arm64) target_arch=arm64 ;;
        amd64|x86_64) target_arch=x86_64 ;;
        *) cap_fail 2 "arch must be arm64 or amd64" ;;
    esac
    if [[ "$(uname -m)" == arm64 && "$target_arch" == x86_64 ]]; then
        pkgutil --pkg-info com.apple.pkg.RosettaUpdateAuto >/dev/null 2>&1 \
            || cap_fail 4 "Rosetta must already be installed to verify Intel Mac artifacts on Apple Silicon"
    fi
    [[ "$output" == /* && "$output" == *.app ]] || cap_fail 2 "output must be an absolute .app path"
    command -v swiftc >/dev/null || cap_fail 4 "swiftc is required (Xcode command line tools)"
    command -v codesign >/dev/null || cap_fail 4 "codesign is required on macOS"
    create_build_scratch memql-menubar
    stage="$COCKPIT_BUILD_SCRATCH/MemQL Cockpit.app"
    mkdir -p "$stage/Contents/MacOS" "$stage/Contents/Resources"
    swiftc -target "$(uname -m)-apple-macosx13.0" -framework AppKit "$REPO_ROOT/native/macos/Mark.swift" "$REPO_ROOT/native/macos/MemQLSwitch.swift" "$REPO_ROOT/native/macos/PermissionSetup.swift" "$REPO_ROOT/native/macos/tests/PermissionSetupTests.swift" -o "$(dirname "$stage")/permission-tests" >&2 || cap_fail 5 "permission tests failed to compile"
    "$(dirname "$stage")/permission-tests" >&2 || cap_fail 5 "permission behavior checks failed"
    swiftc -target "$(uname -m)-apple-macosx13.0" -framework AppKit "$REPO_ROOT/native/macos/Mark.swift" "$REPO_ROOT/native/macos/tests/AppIconTests.swift" -o "$(dirname "$stage")/icon-tests" >&2 || cap_fail 5 "app icon tests failed to compile"
    "$(dirname "$stage")/icon-tests" --resource="$REPO_ROOT/native/macos/mark.svg" >&2 || cap_fail 5 "app icon contrast checks failed"
    swiftc -O -target "$target_arch-apple-macosx13.0" -framework AppKit "$REPO_ROOT/native/macos/Mark.swift" "$REPO_ROOT/native/macos/MemQLSwitch.swift" "$REPO_ROOT/native/macos/PermissionSetup.swift" "$REPO_ROOT/native/macos/main.swift" -o "$stage/Contents/MacOS/MemQLCockpit" >&2 || cap_fail 5 "native menu build failed"
    cp "$REPO_ROOT/native/macos/Info.plist" "$stage/Contents/Info.plist"
    cp "$REPO_ROOT/native/macos/mark.svg" "$stage/Contents/Resources/mark.svg"
    "$stage/Contents/MacOS/MemQLCockpit" --write-iconset="$(dirname "$stage")/MemQL.iconset" >&2 || cap_fail 5 "app icon generation failed"
    iconutil -c icns "$(dirname "$stage")/MemQL.iconset" -o "$stage/Contents/Resources/MemQL.icns" >&2 || cap_fail 5 "app icon packaging failed"
    /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$stage/Contents/Info.plist" >&2
    /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$stage/Contents/Info.plist" >&2
    "$stage/Contents/MacOS/MemQLCockpit" --check-icon >&2 || cap_fail 5 "bundled MemQL menu icon failed rendering validation"
    codesign --force --sign "$signing_identity" "$stage" >&2 || cap_fail 5 "menu signing failed"
    codesign --verify --strict "$stage" >&2 || cap_fail 3 "menu signature verification failed"
    mkdir -p "$(dirname "$output")"
    if [[ -d "$output" ]] && diff -qr "$stage" "$output" >/dev/null; then
        cap_info "Menu bundle already current"
    else
        ditto "$stage" "$output" >&2 || cap_fail 5 "could not write menu bundle"
        cap_changed
    fi
    cleanup_build_scratch || cap_fail 5 "build scratch cleanup failed"
    cap_result_set app "$output"
    cap_ok
}
main "$@"
