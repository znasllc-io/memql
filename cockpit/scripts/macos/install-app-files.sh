#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -f "$REPO_ROOT/scripts/lib/capability.sh" ]]; then
    source "$REPO_ROOT/scripts/lib/capability.sh"
else
    source "$REPO_ROOT/../scripts/lib/capability.sh"
fi
cap_init "cockpit.app.install-files" "Install a verified MemQL bundle and CLI link, retaining rollback copies."
cap_spec_param "app" "Absolute source MemQL.app path"
cap_spec_param "destination" "Absolute destination MemQL.app path"
cap_spec_param "cli-path" "Absolute installed memql CLI symlink path"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local app destination cli identifier stage backup link_stage
    app="$(cap_param app)"; destination="$(cap_param destination)"; cli="$(cap_param cli-path)"
    [[ "$app" == /* && -d "$app" ]] || cap_fail 2 "app must be an existing absolute bundle path"
    [[ "$destination" == /* && "$(basename "$destination")" == MemQL.app ]] || cap_fail 2 "destination must be an absolute MemQL.app path"
    [[ "$cli" == /* && "$(basename "$cli")" == memql ]] || cap_fail 2 "cli-path must be an absolute memql path"
    [[ ! -L "$destination" ]] || cap_fail 3 "destination must not be a symlink"
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")"
    [[ "$identifier" == com.znasllc.memql-worker ]] || cap_fail 3 "source is not the MemQL worker bundle"
    [[ -x "$app/Contents/MacOS/MemQL" && -x "$app/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit" ]] || cap_fail 3 "app lacks worker or menu helper"
    codesign --verify --deep --strict "$app" >&2 || cap_fail 3 "source bundle signature invalid"
    if [[ -e "$destination" ]]; then
        identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$destination/Contents/Info.plist" 2>/dev/null || true)"
        [[ "$identifier" == com.znasllc.memql-worker ]] || cap_fail 3 "refusing to replace an unrelated app"
    fi
    [[ ! -d "$cli" ]] || cap_fail 3 "CLI path is a directory"
    if [[ -d "$destination" && -L "$cli" && "$(readlink "$cli")" == "$destination/Contents/MacOS/MemQL" ]] && diff -qr "$app" "$destination" >/dev/null; then
        cap_result_set app "$destination"
        cap_result_set cli "$cli"
        cap_ok
        return
    fi
    mkdir -p "$(dirname "$destination")" "$(dirname "$cli")"
    backup="$(mktemp -d "$(dirname "$destination")/.memql-app-rollback.XXXXXX")"
    if [[ ! -d "$destination" ]] || ! diff -qr "$app" "$destination" >/dev/null; then
        stage="$backup/new.app"
        ditto "$app" "$stage" >&2 || cap_fail 5 "could not stage app"
        if [[ -d "$destination" ]]; then mv "$destination" "$backup/previous.app"; fi
        if ! mv "$stage" "$destination"; then
            [[ ! -d "$backup/previous.app" ]] || mv "$backup/previous.app" "$destination"
            cap_fail 5 "could not install app; previous bundle restored"
        fi
        cap_changed
    fi
    if [[ ! -L "$cli" || "$(readlink "$cli")" != "$destination/Contents/MacOS/MemQL" ]]; then
        if [[ -e "$cli" || -L "$cli" ]]; then cp -Pp "$cli" "$backup/previous-cli"; fi
        link_stage="$(mktemp -d "$(dirname "$cli")/.memql-link.XXXXXX")"
        ln -s "$destination/Contents/MacOS/MemQL" "$link_stage/memql"
        mv -f "$link_stage/memql" "$cli" || cap_fail 5 "could not replace CLI link"
        rmdir "$link_stage"
        cap_changed
    fi
    cap_result_set app "$destination"
    cap_result_set cli "$cli"
    cap_result_set rollback "$backup"
    cap_ok
}
main "$@"
