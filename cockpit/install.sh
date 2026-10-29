#!/bin/sh
set -eu

# memQL Cockpit one-line installer (headless binary; macOS + Linux, no Windows).
#
#   curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/install.sh | sh
#
# Pin a version, or choose an install dir:
#   curl -fsSL .../install.sh | MEMQL_COCKPIT_VERSION=v0.9.0 sh
#   curl -fsSL .../install.sh | BIN_DIR=/usr/local/bin sh
#
# The computer-use variant (screenshot/mouse/keyboard) ships prebuilt as
# memql-computeruse-<os>-<arch> release assets; the token installers under
# scripts/install/ install it via their --computeruse flag (building from
# source stays the alternative -- see the README). It installs under the SAME
# name; this installer fetches the headless build.
#
# UPGRADING FROM A memql-cockpit INSTALL. This script deletes the pre-rename
# binaries AND retires the pre-rename service, in that order. Deleting the
# binary alone is not a migration: the LaunchAgent / systemd unit is
# KeepAlive/Restart=always, so it would go on trying to exec a path that no
# longer exists, and the operator sees a machine that simply went offline.
#
# The names below must agree with scripts/install/lib.sh, which owns the same
# set for the token-driven installers. They are DUPLICATED because this script
# is fetched alone by `curl | sh` and has no lib.sh to source; the duplication
# is held honest by scripts/install/lib_test.sh, which reads both files.

REPO="znasllc-io/memql"
BINARY="memql"

LEGACY_BINARIES="memql-cockpit memql-cockpit-computeruse"
LEGACY_LABEL_DARWIN="com.znasllc.memql-cockpit-worker"
LEGACY_LABEL_LINUX="memql-cockpit-worker"
SERVICE_LABEL_DARWIN="com.znasllc.memql-worker"
SERVICE_LABEL_LINUX="memql-worker"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

detect_os() {
    case "$(uname -s)" in
        Darwin) echo darwin ;;
        Linux)  echo linux ;;
        *) die "unsupported OS '$(uname -s)' (macOS + Linux only)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo amd64 ;;
        arm64|aarch64) echo arm64 ;;
        *) die "unsupported architecture '$(uname -m)'" ;;
    esac
}

resolve_version() {
    if [ -n "${MEMQL_COCKPIT_VERSION:-}" ]; then
        echo "cockpit/v${MEMQL_COCKPIT_VERSION#v}"; return
    fi
    command -v python3 >/dev/null 2>&1 || die "Python 3 is required to select a release; set MEMQL_COCKPIT_VERSION to pin one."
    # One selector for this POSIX entry point and Fleet's token installers.
    # A source checkout uses its own helper; curl | sh loads the matching
    # helper before discovery. A missing helper/catalog refuses before writes.
    helper=""
    # In a pipe $0 is sh, so a relative lookup would execute a helper from
    # the operator's current directory. Only a real entry-point file has a
    # checkout sibling; downloaded execution always fetches its own helper.
    if [ -f "$0" ] && [ "$(basename "$0")" = install.sh ]; then
        helper="$(dirname "$0")/scripts/install/release-version.py"
    fi
    temporary_helper=""
    if [ ! -f "$helper" ]; then
        helper="$(mktemp)" || return 4
        temporary_helper="$helper"
        raw_base="${MEMQL_INSTALL_RAW_BASE:-https://raw.githubusercontent.com/${REPO}/main/cockpit/scripts/install}"
        helper_protocol=https
        helper_redirects=10
        if [ "${MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP:-}" = 1 ] && python3 -c 'import sys; from urllib.parse import urlparse; u=urlparse(sys.argv[1]); sys.exit(not (u.scheme == "http" and u.hostname in ("127.0.0.1", "::1") and u.username is None and u.password is None and not u.fragment))' "$raw_base"; then
            helper_protocol=http
            helper_redirects=0
        fi
        if ! curl -fsSL --proto "=${helper_protocol}" --proto-redir "=${helper_protocol}" --max-redirs "$helper_redirects" --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 60 --max-filesize 65536 "${raw_base}/release-version.py" -o "$helper" || [ ! -s "$helper" ]; then
            rm -f "$temporary_helper"
            log "ERROR: Could not load the Cockpit release selector. Nothing was installed; retry later."
            return 4
        fi
    fi
    if selected_tag="$(python3 "$helper" --api-url "${MEMQL_INSTALL_RELEASE_API:-https://api.github.com/repos/${REPO}/releases?per_page=100}")"; then
        [ -z "$temporary_helper" ] || rm -f "$temporary_helper"
        printf '%s\n' "$selected_tag"
    else
        result=$?
        [ -z "$temporary_helper" ] || rm -f "$temporary_helper"
        return "$result"
    fi
}

verify_checksum() {
    # $1=dir $2=asset $3=sumsfile
    ( cd "$1" && grep " $2\$" "$3" | \
        if command -v sha256sum >/dev/null 2>&1; then sha256sum -c -; else shasum -a 256 -c -; fi ) >/dev/null 2>&1
}

# migrate_service moves a running worker from the pre-rename service name to
# the current one.
#
# It re-registers ONLY where a legacy service was actually installed. A machine
# that never ran the worker as a service must not acquire one by upgrading --
# an installer that starts a background service nobody asked for is a surprise
# it has no business springing.
migrate_service() {
    os="$1"; bin="$2"
    case "$os" in
        darwin) migrate_launch_agent "$bin" ;;
        linux)  migrate_systemd_unit "$bin" ;;
    esac
}

migrate_launch_agent() {
    bin="$1"
    plist="${HOME}/Library/LaunchAgents/${LEGACY_LABEL_DARWIN}.plist"
    [ -f "$plist" ] || return 0

    log "==> migrating the ${LEGACY_LABEL_DARWIN} LaunchAgent to ${SERVICE_LABEL_DARWIN}"
    launchctl unload "$plist" >/dev/null 2>&1 || true
    rm -f "$plist"

    new_plist="${HOME}/Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist"
    # PATH as install-mac.sh writes it: the system directories first, then
    # where the app installers put claude and codex.
    cat > "$new_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${SERVICE_LABEL_DARWIN}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${bin}</string>
        <string>worker</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>StandardErrorPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
        <key>PATH</key>
        <string>/usr/bin:/bin:/usr/sbin:/sbin:${HOME}/.local/bin:${HOME}/.claude/local:/opt/homebrew/bin:/usr/local/bin</string>
    </dict>
</dict>
</plist>
PLIST
    chmod 644 "$new_plist"
    launchctl load "$new_plist" >/dev/null 2>&1 || \
        log "    (could not load ${SERVICE_LABEL_DARWIN}; run: launchctl load ${new_plist})"
    log "    worker service is now ${SERVICE_LABEL_DARWIN}"
}

migrate_systemd_unit() {
    bin="$1"
    unit_dir="${HOME}/.config/systemd/user"
    legacy_unit="${unit_dir}/${LEGACY_LABEL_LINUX}.service"
    [ -f "$legacy_unit" ] || return 0

    log "==> migrating the ${LEGACY_LABEL_LINUX} systemd unit to ${SERVICE_LABEL_LINUX}"
    systemctl --user disable --now "${LEGACY_LABEL_LINUX}.service" >/dev/null 2>&1 || true
    rm -f "$legacy_unit"

    new_unit="${unit_dir}/${SERVICE_LABEL_LINUX}.service"
    mkdir -p "$unit_dir"
    cat > "$new_unit" <<UNIT
[Unit]
Description=MemQL Cockpit Worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${bin} worker run
Restart=on-failure
RestartSec=5
StandardOutput=append:${HOME}/.memql/state/worker.log
StandardError=append:${HOME}/.memql/state/worker.log

[Install]
WantedBy=default.target
UNIT
    systemctl --user daemon-reload >/dev/null 2>&1 || true
    systemctl --user enable --now "${SERVICE_LABEL_LINUX}.service" >/dev/null 2>&1 || \
        log "    (could not start ${SERVICE_LABEL_LINUX}; run: systemctl --user enable --now ${SERVICE_LABEL_LINUX}.service)"
    log "    worker service is now ${SERVICE_LABEL_LINUX}"
}

# remove_legacy_binaries deletes the pre-rename binaries from ONE directory --
# the one just installed into. Scoped there on purpose: a $PATH-wide sweep
# would delete binaries this installer never placed.
remove_legacy_binaries() {
    dir="$1"
    for name in $LEGACY_BINARIES; do
        if [ -e "${dir}/${name}" ] || [ -L "${dir}/${name}" ]; then
            # if/else rather than `A && B || C`: with the && || form, a
            # failing log() would run the NOTE branch after a successful
            # removal (SC2015).
            if rm -f "${dir}/${name}" 2>/dev/null; then
                log "    removed ${dir}/${name}"
            else
                log "    NOTE: could not remove ${dir}/${name} -- delete it by hand"
            fi
        fi
    done
}

main() {
    need curl; need tar
    os="$(detect_os)"; arch="$(detect_arch)"
    tag="$(resolve_version)"
    [ -n "$tag" ] || die "could not resolve a release version (set MEMQL_COCKPIT_VERSION)"
    ver="${tag#cockpit/v}"

    asset="${BINARY}-${ver}-${os}-${arch}.tar.gz"
    sums="${BINARY}-${ver}-SHA256SUMS"
    base="https://github.com/${REPO}/releases/download/${tag}"

    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT INT TERM

    log "==> memQL Cockpit ${tag} (${os}/${arch})"
    log "==> downloading ${asset}"
    # Consolidated releases publish a complete versioned archive and manifest
    # for every supported platform. Missing evidence is a failed publication,
    # never permission to install an unchecked archive or legacy raw binary.
    curl -fsSL --proto '=https' --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 1800 \
        "${base}/${asset}" -o "${tmp}/${asset}" || die "no release asset for ${os}/${arch} in ${tag}"
    curl -fsSL --proto '=https' --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 60 \
        "${base}/${sums}" -o "${tmp}/${sums}" || die "release checksum manifest is missing; nothing was installed"
    log "==> verifying checksum"
    verify_checksum "$tmp" "$asset" "$sums" || die "checksum verification failed; nothing was installed"
    # make dist archives the staging directory as '.', yielding ./memql;
    # older complete archives may name memql directly. Extract exactly one
    # accepted member instead of trusting other archive paths.
    member="$(tar -tzf "${tmp}/${asset}" | awk -v name="$BINARY" '$0 == name || $0 == "./" name { member=$0; count++ } END { if (count != 1) exit 1; print member }')" \
        || die "archive must contain exactly one ${BINARY} file"
    tar -xzf "${tmp}/${asset}" -C "$tmp" "$member"
    [ -f "${tmp}/${BINARY}" ] && [ ! -L "${tmp}/${BINARY}" ] || die "archive did not contain a regular ${BINARY} file"

    bindir="${BIN_DIR:-${PREFIX:-$HOME/.local}/bin}"
    mkdir -p "$bindir"
    if ! install -m 0755 "${tmp}/${BINARY}" "${bindir}/${BINARY}" 2>/dev/null; then
        cp "${tmp}/${BINARY}" "${bindir}/${BINARY}" && chmod 0755 "${bindir}/${BINARY}"
    fi

    # Migration, AFTER a successful install: an interrupted download must not
    # leave a machine with neither the old binary nor the new one. The service
    # is re-registered BEFORE the old binary is deleted, so a KeepAlive restart
    # in the gap finds something to exec.
    migrate_service "$os" "${bindir}/${BINARY}"
    remove_legacy_binaries "$bindir"
    if [ "$os" = linux ] && "${bindir}/${BINARY}" menu --help >/dev/null 2>&1; then
        "${bindir}/${BINARY}" menu --install || die "Cockpit installed, but the desktop menu could not start. Retry: ${bindir}/${BINARY} menu --install"
    fi

    log ""
    log "installed ${BINARY} ${tag} -> ${bindir}/${BINARY}"
    case ":${PATH}:" in
        *":${bindir}:"*) : ;;
        *) log "NOTE: ${bindir} is not on your PATH -- add it, e.g. export PATH=\"${bindir}:\$PATH\"" ;;
    esac
    log "next: ${BINARY} --version"
}

main "$@"
