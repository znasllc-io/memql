#!/usr/bin/env bash
#
# scripts/install/detect.sh
# =========================
#
# Capability: install.detect -- the local-cluster dependency inventory.
#
# Answers, in one read-only pass: what OS/arch is this, which tools in the
# installer's graph already exist (and at what version), is the docker DAEMON
# actually answering, are the ingress ports free, and how much disk is left.
# Every later capability plans against this answer.
#
# READ-ONLY BY CONSTRUCTION. detect never installs, writes, starts or stops
# anything, so its envelope always reports changed=false. That is not a detail:
# the plan built from this inventory describes the machine as it was observed,
# and a probe that mutated on the way past would be describing a system it had
# already changed.
#
# A missing tool is DATA, not an error -- reporting present=false is the whole
# point, so a bare machine still exits 0. The one refusal is an unsupported
# platform: exit 3 (refused), NOT 4 (prerequisite missing). 4 would promise that
# installing something and retrying will help, and on macOS that is a lie --
# this epic is Linux/amd64 only.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/detect.sh
#   scripts/install/detect.sh --ports=80,443 --path="$HOME"
#   scripts/install/detect.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (unsupported platform)
#
# Refs: #3359 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.detect" "Inventory the local machine's installer dependencies (read-only)."
cap_spec_param "ports" "comma-separated TCP ports to probe (default: 80,443)"
cap_spec_param "path"  "filesystem path to measure free space on (default: \$HOME)"

# The tool graph the installer depends on. Every one gets a present flag.
readonly DETECT_TOOLS=(docker k3d kubectl git mkcert)

readonly SUPPORTED_OS="linux"
readonly SUPPORTED_ARCH="amd64"

#=============================================================================
# PLATFORM
#=============================================================================

# normalize_os <uname -s> -- lowercased kernel name.
function normalize_os() {
    printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# normalize_arch <uname -m> -- the Go/OCI spelling, so the answer lines up with
# the artifact names in tool-pins.env.
function normalize_arch() {
    case "$1" in
        x86_64|amd64)   printf 'amd64' ;;
        aarch64|arm64)  printf 'arm64' ;;
        armv7l|armv7)   printf 'arm' ;;
        *)              printf '%s' "$1" ;;
    esac
}

#=============================================================================
# TOOL PROBES
#=============================================================================

# tool_path <tool> -- resolved path on PATH, or empty when absent.
function tool_path() {
    command -v "$1" 2>/dev/null || true
}

# version_probe <tool> -- best-effort version string, or empty. Kept to pure
# bash matching so detect works on a stripped PATH.
function version_probe() {
    local tool="$1" out="" first=""
    case "$tool" in
        docker)  out="$(_run_bounded docker --version)" ;;
        k3d)     out="$(_run_bounded k3d version)" ;;
        kubectl) out="$(_run_bounded kubectl version --client)" ;;
        git)     out="$(_run_bounded git --version)" ;;
        mkcert)  out="$(_run_bounded mkcert -version)" ;;
    esac
    first="${out%%$'\n'*}"
    if [[ "$first" =~ ([0-9]+\.[0-9]+(\.[0-9]+)?) ]]; then
        printf '%s' "${BASH_REMATCH[1]}"
    fi
}

# _run_bounded <cmd...> -- run a probe, never let it hang the inventory, never
# let its failure abort the run.
function _run_bounded() {
    if command -v timeout &>/dev/null; then
        timeout 10 "$@" 2>/dev/null || true
    else
        "$@" 2>/dev/null || true
    fi
}

# docker_daemon_up -- 0 when the daemon ANSWERS. Deliberately a separate fact
# from "the docker binary exists": a present client with a dead daemon is the
# single most common local-cluster failure, and folding the two together hides
# it until the first `docker run`.
function docker_daemon_up() {
    command -v docker &>/dev/null || return 1
    if command -v timeout &>/dev/null; then
        timeout 15 docker info --format '{{.ServerVersion}}' &>/dev/null
    else
        docker info --format '{{.ServerVersion}}' &>/dev/null
    fi
}

#=============================================================================
# PORTS -- reported TRUE when FREE
#=============================================================================

# port_free <port> -- 0 when NOTHING is listening. The report follows this
# polarity: true means "available to bind", which is what the caller needs to
# decide whether the cluster's front door can come up.
function port_free() {
    local port="$1"
    if command -v ss &>/dev/null; then
        if ss -ltnH 2>/dev/null | grep -qE "[:.]${port}[[:space:]]"; then
            return 1
        fi
    fi
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
        return 1
    fi
    return 0
}

#=============================================================================
# DISK
#=============================================================================

# free_mb <path> -- available MiB, or -1 when it cannot be determined.
function free_mb() {
    local path="$1" line avail="" fields
    command -v df &>/dev/null || { printf '%s' "-1"; return; }
    while IFS= read -r line; do
        # shellcheck disable=SC2206
        fields=($line)
        if [[ ${#fields[@]} -ge 4 && "${fields[3]}" =~ ^[0-9]+$ ]]; then
            avail="${fields[3]}"
        fi
    done < <(df -Pk "$path" 2>/dev/null || true)
    if [[ -z "$avail" ]]; then printf '%s' "-1"; return; fi
    printf '%s' "$(( avail / 1024 ))"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ports_csv path
    ports_csv="$(cap_param ports "80,443")"
    path="$(cap_param path "${HOME:-/}")"
    cap_require ports "$ports_csv"
    cap_require path  "$path"

    # --- platform -------------------------------------------------------
    local os arch supported=false
    os="$(normalize_os  "$(uname -s 2>/dev/null || printf 'unknown')")"
    arch="$(normalize_arch "$(uname -m 2>/dev/null || printf 'unknown')")"
    cap_info "Platform: ${os}/${arch}"
    if [[ "$os" == "$SUPPORTED_OS" && "$arch" == "$SUPPORTED_ARCH" ]]; then
        supported=true
    fi
    if [[ "$supported" != "true" ]]; then
        cap_result_set     os        "$os"
        cap_result_set     arch      "$arch"
        cap_result_set_raw supported false
        cap_fail 3 "unsupported platform ${os}/${arch}: the local cluster installer targets ${SUPPORTED_OS}/${SUPPORTED_ARCH} only"
    fi

    # --- tools ----------------------------------------------------------
    local tool p v present tools_json="" first=1
    for tool in "${DETECT_TOOLS[@]}"; do
        p="$(tool_path "$tool")"
        if [[ -n "$p" ]]; then present=true; v="$(version_probe "$tool")"; else present=false; v=""; fi
        cap_info "$(printf '%-8s present=%-5s %s' "$tool" "$present" "${v:+v$v}")"
        [[ "$first" == 1 ]] || tools_json+=","
        first=0
        tools_json+="\"${tool}\":{\"present\":${present},\"path\":\"$(cap_json_escape "$p")\",\"version\":\"$(cap_json_escape "$v")\"}"
    done

    # --- docker daemon (a fact of its own) ------------------------------
    local daemon=false
    if docker_daemon_up; then daemon=true; fi
    cap_info "docker daemon: ${daemon}"

    # --- ports ----------------------------------------------------------
    local ports_json="" port free
    first=1
    IFS=',' read -r -a _ports <<< "$ports_csv"
    for port in "${_ports[@]}"; do
        port="${port//[[:space:]]/}"
        [[ -z "$port" ]] && continue
        [[ "$port" =~ ^[0-9]+$ ]] || cap_fail 2 "not a TCP port number: '${port}'"
        if port_free "$port"; then free=true; else free=false; fi
        cap_info "port ${port} free=${free}"
        [[ "$first" == 1 ]] || ports_json+=","
        first=0
        ports_json+="\"${port}\":${free}"
    done

    # --- disk -----------------------------------------------------------
    local mb
    mb="$(free_mb "$path")"
    cap_info "free disk at ${path}: ${mb} MiB"

    cap_result_set     os           "$os"
    cap_result_set     arch         "$arch"
    cap_result_set_raw supported    true
    cap_result_set_raw tools        "{${tools_json}}"
    cap_result_set_raw dockerDaemon "$daemon"
    cap_result_set_raw ports        "{${ports_json}}"
    cap_result_set_raw disk         "{\"path\":\"$(cap_json_escape "$path")\",\"freeMb\":${mb}}"
    # No cap_changed: detection is read-only, so changed stays false. Always.
    cap_ok
}

main "$@"
