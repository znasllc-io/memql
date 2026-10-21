#!/usr/bin/env bash
# Library: requirements.sh -- the one-machine minimum requirements, in ONE place.
#
# Usage:   sourced, never executed. The caller owns `set -euo pipefail`.
#
# WHAT IT ANSWERS. A local install runs the whole of MemQL on one machine: the
# cluster in Docker and the local models that answer chat, embeddings, speech
# recognition and speech. docs/public/operate/minimum-requirements.md sets the
# figures and shows the memory budget behind each one; this file is where they
# are CHECKED, and the only place they are written in code.
#
# WHY ONE COPY. install.detect REPORTS the verdict, install.requirements
# REFUSES on it before anything is installed, scripts/k3d/up.sh refuses on it
# before `k3d cluster create`, and scripts/dev/install-deps.sh prints it. The
# VS Code wizard reads detect's report before it offers to start. Five readers
# of one answer: the shape ports.sh and docker.sh already follow, for the same
# reason -- two copies of a threshold are two opinions about one machine.
#
# UNKNOWN IS NOT A SHORTFALL. A probe that cannot answer (no sysctl, Docker not
# running) reports `unknown` and does not fail the minimum; Docker access has a
# gate of its own. The one exception is the Linux GPU: finding none IS the
# answer, because CPU-only inference is not supported.
#
# THE FIGURES ARE NOMINAL. A part sold as 32 GB can report a little less once
# firmware and the kernel take their share (a 16 GB card reports 16376 MiB), so
# memory and VRAM meet a figure at REQ_TOLERANCE_PERCENT of it. Free disk is
# not a sold size and gets no tolerance.
#
# Portability: bash 3.2 (stock macOS) -- no associative arrays, no `mapfile`,
#              no `${var,,}`. See scripts/lib/bash_portability_test.go.

if [[ -n "${_MEMQL_REQUIREMENTS_LOADED:-}" ]]; then
    return 0
fi
_MEMQL_REQUIREMENTS_LOADED=1

_REQ_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if ! declare -F platform_supported >/dev/null 2>&1; then
    # shellcheck source=platform.sh
    source "${_REQ_LIB_DIR}/platform.sh"
fi

readonly REQ_MAC_MEMORY_MIN_GB=32
readonly REQ_MAC_MEMORY_RECOMMENDED_GB=48
readonly REQ_LINUX_MEMORY_MIN_GB=32
readonly REQ_LINUX_MEMORY_RECOMMENDED_GB=64
readonly REQ_GPU_VRAM_MIN_GB=16
readonly REQ_GPU_VRAM_RECOMMENDED_GB=32
readonly REQ_CPU_CORES_MIN=8
readonly REQ_DISK_FREE_MIN_GB=100
readonly REQ_DISK_FREE_RECOMMENDED_GB=200
readonly REQ_MACOS_MIN_MAJOR=13
readonly REQ_DOCKER_MEMORY_MIN_GB=8
readonly REQ_DOCKER_MEMORY_RECOMMENDED_GB=12
readonly REQ_TOLERANCE_PERCENT=90
readonly REQ_DOCS_URL="https://github.com/znasllc-io/memql/blob/main/docs/public/operate/minimum-requirements.md#one-machine-the-entire-cluster"

# The last evaluation, for the caller to read.
REQ_MEETS_MINIMUM=true
REQ_MEETS_RECOMMENDED=true
REQ_CHECKS_JSON=""
REQ_SHORTFALLS_JSON=""
REQ_SHORTFALLS=""
REQ_SUMMARY=""

#=============================================================================
# PROBES -- each prints its answer, or nothing when it cannot tell
#=============================================================================

# _req_bounded <cmd...> -- run a probe with a time limit; never fail the caller.
function _req_bounded() {
    if command -v timeout &>/dev/null; then
        timeout 10 "$@" 2>/dev/null || true
    else
        "$@" 2>/dev/null || true
    fi
}

# _req_number <text> -- the text when it is a whole number above zero, else
# nothing. No machine has zero bytes of memory or zero cores, so a probe that
# says 0 has not answered.
function _req_number() {
    local first="${1%%$'\n'*}"
    first="${first//[[:space:]]/}"
    if [[ "$first" =~ ^[0-9]+$ ]] && (( 10#$first > 0 )); then
        # Base 10 on the way out too: a leading zero would read as octal in
        # every comparison after this one.
        printf '%s' "$(( 10#$first ))"
    fi
    return 0
}

# req_memory_bytes <os> -- installed memory in bytes.
#
# PURE BASH past the probe itself, here and below: no awk, sed or cat, so the
# library answers on the minimal PATH an installer can be handed.
function req_memory_bytes() {
    local out="" label total kb _rest
    case "$1" in
        darwin) out="$(_req_bounded sysctl -n hw.memsize)" ;;
        linux)
            if command -v free &>/dev/null; then
                while read -r label total _rest; do
                    if [[ "$label" == "Mem:" ]]; then
                        out="$total"
                    fi
                done < <(_req_bounded free -b)
            elif [[ -r /proc/meminfo ]]; then
                while read -r label kb _rest; do
                    if [[ "$label" == "MemTotal:" && "$kb" =~ ^[0-9]+$ ]]; then
                        out=$(( kb * 1024 ))
                    fi
                done < /proc/meminfo
            fi
            ;;
    esac
    _req_number "$out"
}

# req_cpu_cores <os> -- logical CPUs.
function req_cpu_cores() {
    local out=""
    case "$1" in
        darwin) out="$(_req_bounded sysctl -n hw.ncpu)" ;;
        linux)
            if command -v nproc &>/dev/null; then
                out="$(_req_bounded nproc)"
            else
                out="$(_req_bounded getconf _NPROCESSORS_ONLN)"
            fi
            ;;
    esac
    _req_number "$out"
}

# req_macos_version -- the product version, such as 15.3.1.
function req_macos_version() {
    local out
    out="$(_req_bounded sw_vers -productVersion)"
    if [[ -z "$out" ]]; then
        out="$(_req_bounded sysctl -n kern.osproductversion)"
    fi
    out="${out%%$'\n'*}"
    out="${out//[[:space:]]/}"
    if [[ "$out" =~ ^[0-9]+(\.[0-9]+)*$ ]]; then
        printf '%s' "$out"
    fi
    return 0
}

# req_gpu -- the discrete GPU with the most memory, as "<bytes>|<name>".
#
# NVIDIA through nvidia-smi, AMD through the amdgpu driver's sysfs node: the
# two surfaces Cockpit's inference floor reads. Integrated graphics answer
# through neither, so a machine with only an iGPU has none.
function req_gpu() {
    local best_bytes=0 best_name="" line name mib bytes node
    if command -v nvidia-smi &>/dev/null; then
        while IFS= read -r line; do
            [[ "$line" == *,* ]] || continue
            name="${line%,*}"
            mib="${line##*,}"
            mib="${mib//[[:space:]]/}"
            [[ "$mib" =~ ^[0-9]+$ ]] || continue
            name="${name#"${name%%[![:space:]]*}"}"
            name="${name%"${name##*[![:space:]]}"}"
            bytes=$(( mib * 1048576 ))
            if (( bytes > best_bytes )); then
                best_bytes=$bytes
                best_name="$name"
            fi
        done < <(_req_bounded nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits)
    fi
    for node in /sys/class/drm/card[0-9]*/device/mem_info_vram_total; do
        [[ -r "$node" ]] || continue
        bytes=""
        read -r bytes < "$node" 2>/dev/null || true
        bytes="$(_req_number "$bytes")"
        [[ -n "$bytes" ]] || continue
        if (( bytes > best_bytes )); then
            best_bytes=$bytes
            best_name="AMD GPU"
        fi
    done
    if (( best_bytes > 0 )); then
        printf '%s|%s' "$best_bytes" "$best_name"
    fi
    return 0
}

# req_docker_memory_bytes -- the memory Docker Desktop's VM has. On Linux Docker
# shares the host's memory, which the memory check already covers.
function req_docker_memory_bytes() {
    if ! command -v docker &>/dev/null; then
        return 0
    fi
    _req_number "$(_req_bounded docker info --format '{{.MemTotal}}')"
}

# req_disk_free_bytes <path> -- free bytes on the filesystem holding <path>.
function req_disk_free_bytes() {
    local line avail="" fields
    if ! command -v df &>/dev/null; then
        return 0
    fi
    while IFS= read -r line; do
        # shellcheck disable=SC2206
        fields=($line)
        if [[ ${#fields[@]} -ge 4 && "${fields[3]}" =~ ^[0-9]+$ ]]; then
            avail="${fields[3]}"
        fi
    done < <(df -Pk "$1" 2>/dev/null || true)
    if [[ -n "$avail" ]]; then
        printf '%s' "$(( avail * 1024 ))"
    fi
    return 0
}

#=============================================================================
# EVALUATION
#=============================================================================

# _req_json_escape <text> -- a JSON string body. The text here is labels, sizes
# and GPU names, so backslash and quote are the characters that can occur.
function _req_json_escape() {
    local s="${1//\\/\\\\}"
    printf '%s' "${s//\"/\\\"}"
}

# _req_gb <bytes> -- whole gigabytes as machines are sold: GiB, rounded.
function _req_gb() {
    printf '%s' "$(( ($1 + 536870912) / 1073741824 ))"
}

# _req_reaches <bytes> <figure GB> -- 0 when bytes reach the figure, within the
# tolerance for nominal sizes.
function _req_reaches() {
    (( $1 * 100 >= $2 * 1073741824 * REQ_TOLERANCE_PERCENT ))
}

# _req_level <bytes> <minimum GB> <recommended GB> -- recommended | minimum | below.
function _req_level() {
    if _req_reaches "$1" "$3"; then
        printf 'recommended'
    elif _req_reaches "$1" "$2"; then
        printf 'minimum'
    else
        printf 'below'
    fi
}

# _req_record <id> <label> <observed> <minimum> <recommended> <status> [remedy]
#
# One check. status is recommended | minimum | below | unknown; recommended may
# be empty where the documentation names only a minimum.
function _req_record() {
    local id="$1" label="$2" observed="$3" minimum="$4" recommended="$5" status="$6" remedy="${7:-}" verdict sentence
    if [[ -n "$REQ_CHECKS_JSON" ]]; then
        REQ_CHECKS_JSON+=","
    fi
    REQ_CHECKS_JSON+="{\"id\":\"${id}\",\"label\":\"$(_req_json_escape "$label")\",\"observed\":\"$(_req_json_escape "$observed")\",\"minimum\":\"$(_req_json_escape "$minimum")\",\"recommended\":\"$(_req_json_escape "$recommended")\",\"status\":\"${status}\"}"
    case "$status" in
        below)
            REQ_MEETS_MINIMUM=false
            REQ_MEETS_RECOMMENDED=false
            verdict="below the minimum of ${minimum}"
            sentence="${label}: ${observed}. The minimum is ${minimum}.${remedy:+ ${remedy}}"
            REQ_SHORTFALLS+="${sentence}"$'\n'
            if [[ -n "$REQ_SHORTFALLS_JSON" ]]; then
                REQ_SHORTFALLS_JSON+=","
            fi
            REQ_SHORTFALLS_JSON+="\"$(_req_json_escape "$sentence")\""
            ;;
        minimum)
            REQ_MEETS_RECOMMENDED=false
            verdict="meets the minimum; ${recommended} is recommended"
            ;;
        recommended)
            verdict="meets the recommendation"
            if [[ -z "$recommended" ]]; then
                verdict="meets the requirement"
            fi
            ;;
        *)
            verdict="could not be measured"
            ;;
    esac
    REQ_SUMMARY+="$(printf '%-14s %-30s %s' "$label" "$observed" "$verdict")"$'\n'
}

# _req_record_bytes <id> <label> <bytes> <minimum GB> <recommended GB> [remedy]
function _req_record_bytes() {
    local bytes="$3" min="$4" rec="$5"
    if [[ -z "$bytes" ]]; then
        _req_record "$1" "$2" "unknown" "${min} GB" "${rec} GB" unknown
        return 0
    fi
    _req_record "$1" "$2" "$(_req_gb "$bytes") GB" "${min} GB" "${rec} GB" "$(_req_level "$bytes" "$min" "$rec")" "${6:-}"
}

# _req_system_label <os> <arch> -- the machine kind in a person's words.
function _req_system_label() {
    case "$1/$2" in
        darwin/arm64) printf 'Apple silicon Mac' ;;
        darwin/amd64) printf 'Intel Mac' ;;
        linux/amd64)  printf 'Linux on x86_64' ;;
        linux/arm64)  printf 'Linux on arm64' ;;
        *)            printf '%s on %s' "$1" "$2" ;;
    esac
}

# requirements_evaluate [disk path] -- measure this machine and fill REQ_*.
function requirements_evaluate() {
    local path="${1:-${HOME:-/}}" os arch id bytes gpu cores version major disk
    REQ_MEETS_MINIMUM=true
    REQ_MEETS_RECOMMENDED=true
    REQ_CHECKS_JSON=""
    REQ_SHORTFALLS_JSON=""
    REQ_SHORTFALLS=""
    REQ_SUMMARY=""
    os="$(platform_os)"
    arch="$(platform_arch)"
    id="$(platform_id "$os" "$arch")"

    if ! platform_supported "$id"; then
        # Nothing else is worth measuring on a machine the installer does not
        # target: no figure below would change the answer.
        _req_record system "System" "$(_req_system_label "$os" "$arch")" "an Apple silicon Mac or Linux on x86_64" "" below \
            "Windows, Intel Macs and arm64 Linux are not supported yet."
        return 0
    fi
    _req_record system "System" "$(_req_system_label "$os" "$arch")" "an Apple silicon Mac or Linux on x86_64" "" recommended

    if [[ "$os" == "darwin" ]]; then
        _req_record_bytes memory "Memory" "$(req_memory_bytes "$os")" "$REQ_MAC_MEMORY_MIN_GB" "$REQ_MAC_MEMORY_RECOMMENDED_GB"
    else
        _req_record_bytes memory "Memory" "$(req_memory_bytes "$os")" "$REQ_LINUX_MEMORY_MIN_GB" "$REQ_LINUX_MEMORY_RECOMMENDED_GB"
        gpu="$(req_gpu)"
        if [[ -z "$gpu" ]]; then
            _req_record gpu "GPU" "none found" "${REQ_GPU_VRAM_MIN_GB} GB of VRAM" "${REQ_GPU_VRAM_RECOMMENDED_GB} GB of VRAM" below \
                "If this machine has an NVIDIA or AMD card, install its driver."
        else
            bytes="${gpu%%|*}"
            _req_record gpu "GPU" "${gpu#*|}, $(_req_gb "$bytes") GB" "${REQ_GPU_VRAM_MIN_GB} GB of VRAM" "${REQ_GPU_VRAM_RECOMMENDED_GB} GB of VRAM" \
                "$(_req_level "$bytes" "$REQ_GPU_VRAM_MIN_GB" "$REQ_GPU_VRAM_RECOMMENDED_GB")"
        fi
    fi

    cores="$(req_cpu_cores "$os")"
    if [[ -z "$cores" ]]; then
        _req_record cpu "Processor" "unknown" "${REQ_CPU_CORES_MIN} cores" "" unknown
    elif (( cores >= REQ_CPU_CORES_MIN )); then
        _req_record cpu "Processor" "${cores} cores" "${REQ_CPU_CORES_MIN} cores" "" recommended
    else
        _req_record cpu "Processor" "${cores} cores" "${REQ_CPU_CORES_MIN} cores" "" below
    fi

    disk="$(req_disk_free_bytes "$path")"
    if [[ -z "$disk" ]]; then
        _req_record disk "Free disk" "unknown" "${REQ_DISK_FREE_MIN_GB} GB" "${REQ_DISK_FREE_RECOMMENDED_GB} GB" unknown
    else
        local gb=$(( disk / 1000000000 )) status=below
        if (( gb >= REQ_DISK_FREE_RECOMMENDED_GB )); then
            status=recommended
        elif (( gb >= REQ_DISK_FREE_MIN_GB )); then
            status=minimum
        fi
        _req_record disk "Free disk" "${gb} GB" "${REQ_DISK_FREE_MIN_GB} GB" "${REQ_DISK_FREE_RECOMMENDED_GB} GB" "$status" \
            "Free some space, or install on a larger disk."
    fi

    if [[ "$os" == "darwin" ]]; then
        version="$(req_macos_version)"
        major="${version%%.*}"
        if [[ -z "$version" ]]; then
            _req_record macos "macOS" "unknown" "${REQ_MACOS_MIN_MAJOR}" "" unknown
        elif (( major >= REQ_MACOS_MIN_MAJOR )); then
            _req_record macos "macOS" "$version" "${REQ_MACOS_MIN_MAJOR}" "" recommended
        else
            _req_record macos "macOS" "$version" "${REQ_MACOS_MIN_MAJOR}" "" below "Update macOS."
        fi
        _req_record_bytes docker "Docker memory" "$(req_docker_memory_bytes)" "$REQ_DOCKER_MEMORY_MIN_GB" "$REQ_DOCKER_MEMORY_RECOMMENDED_GB" \
            "Raise it in Docker Desktop under Settings, Resources."
    fi
    return 0
}

# requirements_json -- the last evaluation as one JSON object.
function requirements_json() {
    printf '{"meetsMinimum":%s,"meetsRecommended":%s,"checks":[%s],"shortfalls":[%s],"docs":"%s"}' \
        "$REQ_MEETS_MINIMUM" "$REQ_MEETS_RECOMMENDED" "$REQ_CHECKS_JSON" "$REQ_SHORTFALLS_JSON" "$REQ_DOCS_URL"
}

# requirements_shortfalls_line -- every shortfall on one line, for a refusal.
function requirements_shortfalls_line() {
    local line out=""
    while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        out+="${out:+ }${line}"
    done <<< "$REQ_SHORTFALLS"
    printf '%s' "$out"
}
