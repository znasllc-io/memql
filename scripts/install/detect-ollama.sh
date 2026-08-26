#!/usr/bin/env bash
#
# scripts/install/detect-ollama.sh
# ================================
#
# Capability: install.detectOllama -- report whether this machine hosts a local
# model runtime, and which models it has.
#
# WHAT IT IS FOR. A local install can run the platform's own operations
# (planning, conductor, suggestions, embeddings) on models the machine already
# hosts, at no per-token cost (epic memql#4676). The installer needs to know
# whether that is possible here before it offers it.
#
# IT IS A PROBE, NOT A REQUIREMENT. "No Ollama" is a perfectly good answer and
# exits 0 with found=false -- NOT 3 (refused) and NOT 4 (prerequisite missing),
# because nothing was refused and nothing is missing that the install needs.
# Install, uninstall, repair and update never require inference (design D8),
# and an exit code that read as failure here would make an inference-free
# install look broken to any caller branching on status.
#
# WHAT IT DOES NOT DO:
#
#   * It does not INSTALL anything. Offering to install a model runtime is the
#     installer's decision to present and the operator's to make; a probe that
#     installed what it failed to find would be a probe nobody could run
#     safely to ask a question.
#   * It does not REGISTER models with the cluster. Advertising a machine's
#     models is the cockpit's job (memql-cockpit), on the worker stream it
#     already holds open. This script reports; the caller hands off.
#   * It does not judge whether a model is GOOD ENOUGH. The published floor is
#     a 7-8B instruct model with structured output plus a small embeddings
#     model, and what enforces it per call is the engine's catalog capability
#     gating -- not a string match on a model name here, which would go stale
#     the week a new model shipped.
#
# EXIT CODES:
#
#   0  probed successfully -- read `found` in the result to learn the answer
#   2  bad param
#   4  prerequisite missing (curl absent, so nothing can be probed at all)
#   5  the endpoint answered, but with something that is not an Ollama tag list
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/detect-ollama.sh
#   scripts/install/detect-ollama.sh --endpoint=http://127.0.0.1:11434 --timeout=3
#   scripts/install/detect-ollama.sh --print-spec
#
# Refs: #4676 #4685 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.detectOllama" \
    "Probe for a local Ollama runtime and report the models it hosts. A machine with no runtime is a successful probe reporting found=false, never a failure: install, uninstall, repair and update never require inference."
cap_spec_param "endpoint" "Ollama base URL (default http://127.0.0.1:11434)"
cap_spec_param "timeout"  "per-request timeout in seconds (default 3)"

# DEFAULT_ENDPOINT is loopback, and deliberately not a hostname. The probe asks
# "does THIS machine host models"; a machine answering for somebody else's
# runtime would advertise models it cannot serve, and the first call would land
# on a laptop that had nothing to do with it.
readonly DEFAULT_ENDPOINT="http://127.0.0.1:11434"
readonly DEFAULT_TIMEOUT="3"

#=============================================================================
# THE PROBE
#=============================================================================

# require_curl fails 4 when curl is absent. This is the ONE prerequisite: with
# no HTTP client there is no probe at all, which is different from probing and
# finding nothing.
function require_curl() {
    if ! command -v curl >/dev/null 2>&1; then
        cap_fail 4 "curl is required to probe for a local model runtime"
    fi
}

# probe <endpoint> <timeout> -- prints the raw /api/tags body on success,
# prints nothing and returns 1 when the endpoint is not there.
#
# `|| true` on the curl is deliberate: a connection refused is the ORDINARY
# answer on a machine with no runtime, and letting set -e turn it into an
# aborted script would make "no Ollama here" indistinguishable from a broken
# installer.
function probe() {
    local endpoint="$1" timeout="$2" body
    body="$(curl --silent --show-error --fail --max-time "$timeout" \
        "${endpoint%/}/api/tags" 2>/dev/null || true)"
    if [[ -z "$body" ]]; then
        return 1
    fi
    printf '%s' "$body"
}

# parse_models <body> -- prints one model name per line.
#
# Deliberately not jq: jq is not a documented prerequisite of the installer and
# adding one to a probe that must run everywhere would trade the answer for a
# dependency. The shape is Ollama's `{"models":[{"name":"llama3.1:8b",...}]}`,
# so the names are the "name" fields.
function parse_models() {
    local body="$1"
    printf '%s' "$body" \
        | tr ',' '\n' \
        | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | sed '/^$/d' \
        | sort -u
}

# json_string_array <lines...> -- renders newline-separated values as a JSON
# array. Values are model names from a local runtime, but they are still
# escaped: a name is a string somebody else chose.
function json_string_array() {
    local first=true out="[" line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        if [[ "$first" == true ]]; then first=false; else out+=","; fi
        out+="\"$(cap_json_escape "$line")\""
    done
    out+="]"
    printf '%s' "$out"
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local endpoint timeout
    endpoint="$(cap_param endpoint "$DEFAULT_ENDPOINT")"
    timeout="$(cap_param timeout "$DEFAULT_TIMEOUT")"

    if [[ ! "$timeout" =~ ^[0-9]+$ ]] || [[ "$timeout" -eq 0 ]]; then
        cap_fail 2 "timeout must be a positive whole number of seconds, got: ${timeout}"
    fi

    require_curl

    cap_step "probing for a local model runtime at ${endpoint}"
    local body=""
    if ! body="$(probe "$endpoint" "$timeout")"; then
        # THE ORDINARY ANSWER on a machine without Ollama. Success, found=false.
        cap_info "no local model runtime answered at ${endpoint}"
        cap_info "this changes nothing about the install: no step of install, uninstall, repair or update needs a model"
        cap_result_set_raw "found" "false"
        cap_result_set "endpoint" "$endpoint"
        cap_result_set_raw "models" "[]"
        cap_result_set_raw "modelCount" "0"
        # Bare cap_ok: its optional argument REPLACES the accumulated result
        # object and must be raw JSON, so passing a human message there emits
        # an envelope no parser can read.
        cap_ok
    fi

    if [[ "$body" != *'"models"'* ]]; then
        # Something IS listening and it is not Ollama. Reported as a failure
        # rather than as found=false, because "the port is taken by something
        # else" is a real condition an operator can act on, and silently
        # reporting it as absence would hide it.
        cap_fail 5 "something is listening at ${endpoint} but did not answer with an Ollama tag list"
    fi

    local models_json count
    models_json="$(parse_models "$body" | json_string_array)"
    count="$(parse_models "$body" | sed '/^$/d' | wc -l | tr -d '[:space:]')"

    cap_info "found a local model runtime hosting ${count} model(s)"
    # Booleans and counts go out as JSON booleans and numbers, not as the
    # strings "true"/"0". A consumer branching on a stringly-typed false is
    # branching on a truthy value in most languages.
    cap_result_set_raw "found" "true"
    cap_result_set "endpoint" "$endpoint"
    cap_result_set "runtime" "ollama"
    cap_result_set_raw "models" "$models_json"
    cap_result_set_raw "modelCount" "$count"
    cap_ok
}

main "$@"
