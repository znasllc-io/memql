#!/usr/bin/env bash
#
# scripts/install/verify-provider-key.sh
# ======================================
#
# Capability: install.verifyProviderKey -- prove an AI-provider API key works.
#
# The installer needs to know a key is GOOD before it seeds it into the
# cluster; a key that only fails later fails inside a pod, hours from the
# person who typed it. The check is a real authenticated call to
# `GET <base-url>/v1/models` -- authenticated, cheap, and it spends no tokens
# (no completion is requested, so there is nothing to bill).
#
# KEY HANDLING (the reason this script is not three lines of curl):
#
#   * The key is read from a FILE (--key-file), never a --key= flag. This
#     script's own argv is world-readable in `ps`, so a flag would leak the
#     key to every user on the box. There is deliberately no `key` param, so
#     cap_parse_flags rejects `--key=...` outright (exit 2).
#   * The key reaches curl through a 0600 CONFIG FILE (curl --config), never
#     through curl's argv -- same `ps` exposure, one process further along.
#     The file lives in a private mktemp dir and is removed on exit.
#
# EXIT CODES -- the distinction the installer branches on:
#
#   0  the provider accepted the key
#   2  bad param (unknown provider, missing/empty key file)
#   3  REFUSED: the provider rejected the key (HTTP 401/403). The key is bad;
#      re-prompt the operator. This is NOT 5 -- nothing malfunctioned.
#   4  prerequisite missing (curl absent)
#   5  operation failed: the provider is unreachable or faulted (5xx, timeout,
#      DNS). Says nothing about the key; retry or report an outage.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-provider-key.sh --provider=anthropic --key-file=/run/secrets/anthropic
#   scripts/install/verify-provider-key.sh --provider=openai --key-file=./k --base-url=http://127.0.0.1:8080
#   scripts/install/verify-provider-key.sh --print-spec
#
# Refs: #3364 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.verifyProviderKey" \
    "Verify an AI-provider API key with an authenticated, token-free GET /v1/models."
cap_spec_param "provider"          "provider to verify: anthropic | openai"
cap_spec_param "key-file"          "path to a file containing the API key (never a flag -- argv is public)"
cap_spec_param "base-url"          "API base URL override (default: the provider's public endpoint)"
cap_spec_param "timeout"           "per-request timeout in seconds (default 15)"
cap_spec_param "anthropic-version" "anthropic-version header value (default 2026-01-01)"

# Private working directory holding the 0600 curl config. Set by setup_workdir.
_VPK_WORKDIR=""

#=============================================================================
# WORKDIR -- the key lives here, briefly, at 0600
#=============================================================================

# _vpk_cleanup removes the workdir (and with it the file holding the key). It
# is chained AHEAD of capability.sh's EXIT trap so the result envelope is still
# guaranteed on every path.
function _vpk_cleanup() {
    [[ -n "$_VPK_WORKDIR" && -d "$_VPK_WORKDIR" ]] && rm -rf "$_VPK_WORKDIR" 2>/dev/null
    return 0
}

function setup_workdir() {
    _VPK_WORKDIR="$(mktemp -d 2>/dev/null)" || cap_fail 5 "could not create a private working directory"
    chmod 700 "$_VPK_WORKDIR" 2>/dev/null || true
    trap '_vpk_cleanup; _cap_on_exit' EXIT
}

#=============================================================================
# PARAMS
#=============================================================================

# default_base_url <provider>
function default_base_url() {
    case "$1" in
        anthropic) printf 'https://api.anthropic.com' ;;
        openai)    printf 'https://api.openai.com' ;;
    esac
}

# read_key <key-file> -- loads the key into _VPK_KEY with surrounding
# whitespace stripped. It assigns a global rather than echoing, because a
# cap_fail inside a "$(...)" substitution would exit only the subshell and its
# failure envelope would be captured as the value instead of reaching stdout.
# Fails (exit 2) when the file is unreadable or holds nothing usable: an empty
# key file is a typo, not a rejected credential.
_VPK_KEY=""
function read_key() {
    local file="$1"
    if [[ ! -f "$file" ]]; then
        cap_fail 2 "key file not found: ${file}"
    fi
    if [[ ! -r "$file" ]]; then
        cap_fail 2 "key file is not readable: ${file}"
    fi
    _VPK_KEY="$(tr -d '[:space:]' < "$file")"
    if [[ -z "$_VPK_KEY" ]]; then
        cap_fail 2 "key file is empty: ${file}"
    fi
}

#=============================================================================
# THE CURL CONFIG FILE -- where the key goes instead of argv
#=============================================================================

# escape_conf_value <string> -- escapes a value for a curl config file's
# double-quoted form (backslash and double quote are the only metacharacters).
function escape_conf_value() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    printf '%s' "$s"
}

# write_curl_config <cfg-path> <provider> <key> <anthropic-version>
# Creates the file at 0600 BEFORE the key is written into it, so the key is
# never momentarily world-readable.
function write_curl_config() {
    local cfg="$1" provider="$2" key="$3" anthropic_version="$4"
    : > "$cfg"
    chmod 600 "$cfg"
    {
        printf 'silent\n'
        printf 'show-error\n'
        case "$provider" in
            anthropic)
                printf 'header = "x-api-key: %s"\n' "$(escape_conf_value "$key")"
                printf 'header = "anthropic-version: %s"\n' "$(escape_conf_value "$anthropic_version")"
                ;;
            openai)
                printf 'header = "Authorization: Bearer %s"\n' "$(escape_conf_value "$key")"
                ;;
        esac
    } >> "$cfg"
}

#=============================================================================
# THE PROBE
#=============================================================================

function check_prerequisites() {
    if ! command -v curl &>/dev/null; then
        cap_fail 4 "curl is not installed; cannot verify a provider key"
    fi
}

# classify_and_finish <provider> <endpoint> <curl-rc> <http-code> <curl-stderr>
# Turns the transport + HTTP outcome into the envelope and the exit code.
function classify_and_finish() {
    local provider="$1" endpoint="$2" rc="$3" code="$4" transport="$5"
    [[ "$code" =~ ^[0-9]+$ ]] || code=0

    cap_result_set     provider   "$provider"
    cap_result_set     endpoint   "$endpoint"
    cap_result_set_raw httpStatus "$code"

    if [[ "$rc" != "0" ]]; then
        cap_result_set_raw valid false
        cap_result_set     detail "curl exit ${rc}: ${transport}"
        cap_fail 5 "provider unreachable (curl exit ${rc}); this says nothing about the key"
    fi

    case "$code" in
        2??)
            cap_result_set_raw valid true
            cap_result_set     detail "provider accepted the key (HTTP ${code})"
            cap_info "Key accepted by ${provider} (HTTP ${code})."
            cap_ok
            ;;
        401|403)
            cap_result_set_raw valid false
            cap_result_set     detail "provider rejected the key (HTTP ${code})"
            cap_error "${provider} rejected the key (HTTP ${code})."
            cap_fail 3 "provider rejected the key (HTTP ${code})"
            ;;
        *)
            cap_result_set_raw valid false
            cap_result_set     detail "unexpected response (HTTP ${code})"
            cap_fail 5 "provider returned HTTP ${code}; the key could not be verified"
            ;;
    esac
}

# probe <provider> <base-url> <key> <timeout> <anthropic-version>
function probe() {
    local provider="$1" base="$2" key="$3" timeout="$4" anthropic_version="$5"
    local endpoint="${base%/}/v1/models"
    local cfg="${_VPK_WORKDIR}/curl.conf"
    local body="${_VPK_WORKDIR}/body"
    local errf="${_VPK_WORKDIR}/stderr"

    write_curl_config "$cfg" "$provider" "$key" "$anthropic_version"

    cap_step "GET ${endpoint} (authenticated, no tokens spent)"

    local rc=0 code=""
    # The key is in --config, NOT here: curl's argv is visible in `ps`.
    code="$(curl --config "$cfg" \
                 --request GET \
                 --max-time "$timeout" \
                 --output "$body" \
                 --write-out '%{http_code}' \
                 "$endpoint" 2>"$errf")" || rc=$?

    local transport=""
    [[ -f "$errf" ]] && transport="$(tr '\n' ' ' < "$errf" | cut -c1-300)"

    classify_and_finish "$provider" "$endpoint" "$rc" "$code" "$transport"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local provider base_url timeout anthropic_version key_file
    provider="$(cap_param provider)"
    key_file="$(cap_param key-file)"
    timeout="$(cap_param timeout 15)"
    anthropic_version="$(cap_param anthropic-version 2026-01-01)"

    cap_require provider "$provider"
    case "$provider" in
        anthropic|openai) ;;
        *) cap_fail 2 "unknown provider: ${provider} (supported: anthropic, openai)" ;;
    esac
    cap_require key-file "$key_file"

    base_url="$(cap_param base-url "$(default_base_url "$provider")")"
    cap_require base-url "$base_url"

    check_prerequisites
    setup_workdir

    read_key "$key_file"

    probe "$provider" "$base_url" "$_VPK_KEY" "$timeout" "$anthropic_version"
}

main "$@"
