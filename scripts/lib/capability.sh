#!/usr/bin/env bash
#
# scripts/lib/capability.sh
# =========================
#
# Shared runtime for memQL **capability scripts** -- the deterministic shell
# backends that the DSL `action` executor (and humans) invoke. It is the
# mechanism that makes the capability-script contract real and uniform:
#
#   - human-readable logs go to STDERR (info/warn/error/step),
#   - the machine-readable RESULT (a single JSON envelope) goes to STDOUT,
#   - failures always emit a structured envelope + an honest exit code,
#   - confirmations are explicit params, never an interactive `read` prompt.
#
# THE CONTRACT (authoritative): docs/internal/design/capability-script-contract.md
# This file is the hardened successor to the function-based-shell convention
# in CLAUDE.md ("Makefile + shell-script convention").
#
# Usage (a conformant capability script):
#
#   #!/usr/bin/env bash
#   set -euo pipefail
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"
#
#   cap_init "k3d.down" "Tear down the local k3d cluster."
#   cap_spec_param "cluster" "k3d cluster name" "MEMQL_K3D_CLUSTER"
#   cap_spec_param "purge"   "also purge the kubeconfig context (flag)" ""
#
#   function main() {
#     cap_handle_meta "$@"          # --help / --print-spec short-circuit
#     local cluster purge
#     cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
#     purge="$(cap_flag purge "$@")"
#     ...
#     cap_result_set    cluster "$cluster"
#     cap_result_set_raw deleted true
#     cap_changed
#     cap_ok
#   }
#   main "$@"
#
# Anything sourcing this file inherits a `trap` that guarantees exactly one
# JSON envelope is written to stdout, even on an unexpected `set -e` abort.

# Guard against double-sourcing.
if [[ -n "${_CAP_LIB_LOADED:-}" ]]; then
    return 0 2>/dev/null || exit 0
fi
_CAP_LIB_LOADED=1

#=============================================================================
# STATE
#=============================================================================

_CAP_NAME=""            # capability id, e.g. "k3d.down"
_CAP_SUMMARY=""         # one-line human summary
_CAP_EMITTED=0          # 1 once a result envelope has been written
_CAP_CHANGED=false      # idempotency signal: did this run mutate anything
_CAP_RESULT_FIELDS=()   # accumulated "key":<json-value> pairs for result{}
_CAP_SPEC_PARAMS=()     # "name|description|envvar" for --print-spec
_CAP_STDIN_JSON=""      # raw stdin JSON (captured lazily on first cap_param)
_CAP_STDIN_READ=0

#=============================================================================
# INITIALISATION
#=============================================================================

# cap_init <capability-id> <summary>
# Declares the capability and installs the result-guarantee EXIT trap.
function cap_init() {
    _CAP_NAME="${1:?cap_init requires a capability id}"
    _CAP_SUMMARY="${2:-}"
    trap '_cap_on_exit' EXIT
}

# cap_spec_param <name> <description> [envvar]
# Documents a parameter for --print-spec / --help. Purely declarative.
function cap_spec_param() {
    _CAP_SPEC_PARAMS+=("${1}|${2:-}|${3:-}")
}

#=============================================================================
# LOGGING -- always to STDERR so STDOUT stays pure JSON
#=============================================================================

function cap_log()   { printf '%s\n'        "$*" >&2; }
function cap_info()  { printf 'INFO:  %s\n' "$*" >&2; }
function cap_warn()  { printf 'WARN:  %s\n' "$*" >&2; }
function cap_error() { printf 'ERROR: %s\n' "$*" >&2; }
function cap_step()  { printf '==> %s\n'    "$*" >&2; }

#=============================================================================
# JSON HELPERS (pure bash -- no jq dependency for emission)
#=============================================================================

# cap_json_escape <string> -> escaped JSON string body (no surrounding quotes)
function cap_json_escape() {
    local s="$1"
    s="${s//\\/\\\\}"     # backslash first
    s="${s//\"/\\\"}"     # double quote
    s="${s//$'\n'/\\n}"   # newline
    s="${s//$'\r'/\\r}"   # carriage return
    s="${s//$'\t'/\\t}"   # tab
    printf '%s' "$s"
}

# cap_result_set <key> <string-value>   -- adds "key":"<escaped string>"
function cap_result_set() {
    local k="$1" v
    v="$(cap_json_escape "${2:-}")"
    _CAP_RESULT_FIELDS+=("\"${k}\":\"${v}\"")
}

# cap_result_set_raw <key> <raw-json>   -- adds "key":<raw> (numbers/bools/objects/arrays)
function cap_result_set_raw() {
    _CAP_RESULT_FIELDS+=("\"${1}\":${2}")
}

# cap_changed -- mark that this run mutated state (idempotency reporting)
function cap_changed() { _CAP_CHANGED=true; }

# _cap_result_object -> "{...}" assembled from accumulated fields
function _cap_result_object() {
    local IFS=,
    printf '{%s}' "${_CAP_RESULT_FIELDS[*]:-}"
}

#=============================================================================
# RESULT EMISSION -- exactly one JSON envelope, on STDOUT
#=============================================================================

# _cap_emit <ok-bool> <exit-code> <error-message-or-empty>
function _cap_emit() {
    local ok="$1" code="$2" msg="${3:-}"
    local result errblock
    result="$(_cap_result_object)"
    if [[ "$ok" == "true" ]]; then
        errblock="null"
    else
        errblock="{\"code\":${code},\"message\":\"$(cap_json_escape "$msg")\"}"
    fi
    printf '{"ok":%s,"capability":"%s","changed":%s,"result":%s,"error":%s}\n' \
        "$ok" "$(cap_json_escape "$_CAP_NAME")" "$_CAP_CHANGED" "$result" "$errblock"
    _CAP_EMITTED=1
}

# cap_ok [raw-json-result]
# Emits the success envelope and exits 0. An optional argument replaces the
# accumulated result object wholesale (must be valid JSON).
function cap_ok() {
    if [[ $# -ge 1 ]]; then
        _cap_emit_raw_result "$1" "true" "0" ""
    else
        _cap_emit "true" "0" ""
    fi
    exit 0
}

# cap_fail <exit-code> <message>
# Emits the failure envelope and exits with <exit-code> (must be 1..255).
function cap_fail() {
    local code="${1:-1}" msg="${2:-unspecified failure}"
    [[ "$code" -ge 1 && "$code" -le 255 ]] || code=1
    _cap_emit "false" "$code" "$msg"
    exit "$code"
}

# _cap_emit_raw_result <raw-json> <ok> <code> <msg> -- internal, for cap_ok with arg
function _cap_emit_raw_result() {
    local raw="$1" ok="$2" code="$3" msg="$4" errblock
    if [[ "$ok" == "true" ]]; then errblock="null"; else
        errblock="{\"code\":${code},\"message\":\"$(cap_json_escape "$msg")\"}"; fi
    printf '{"ok":%s,"capability":"%s","changed":%s,"result":%s,"error":%s}\n' \
        "$ok" "$(cap_json_escape "$_CAP_NAME")" "$_CAP_CHANGED" "$raw" "$errblock"
    _CAP_EMITTED=1
}

# _cap_on_exit -- EXIT trap: guarantees a failure envelope if the script
# aborts (set -e / uncaught error) without explicitly emitting one. Success
# paths MUST call cap_ok; the conformance test enforces that.
function _cap_on_exit() {
    local code=$?
    [[ "$_CAP_EMITTED" == "1" ]] && return
    if [[ $code -ne 0 ]]; then
        _cap_emit "false" "$code" "capability '${_CAP_NAME}' aborted (exit ${code}) without an explicit result"
    fi
}

#=============================================================================
# PARAMETERS -- precedence: --flag=value  >  stdin JSON  >  environment
#=============================================================================

# _cap_load_stdin -- captures stdin JSON once. Reading stdin is OPT-IN (the
# `--params-stdin` flag or CAP_PARAMS_STDIN=1) so a capability never blocks on
# an inherited-but-idle stdin. The action executor passes --params-stdin when
# it pipes a JSON params object; humans normally use flags/env and never do.
function _cap_load_stdin() {
    [[ "$_CAP_STDIN_READ" == "1" ]] && return
    _CAP_STDIN_READ=1
    local want="${CAP_PARAMS_STDIN:-}"
    [[ -n "$(cap_flag params-stdin)" ]] && want=1
    if [[ -n "$want" ]]; then
        _CAP_STDIN_JSON="$(cat 2>/dev/null || true)"
    fi
}

# cap_param <name> [default]
# Resolves a parameter from (in precedence order) a --name=value flag already
# parsed into the global CAP_ARG_<NAME>, then stdin JSON, then the default.
# Flags are populated by cap_parse_flags; for direct use, prefer cap_flag.
function cap_param() {
    local name="$1" default="${2:-}" upper val
    upper="$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
    # 1. parsed flag
    local var="CAP_ARG_${upper}"
    if [[ -n "${!var:-}" ]]; then printf '%s' "${!var}"; return; fi
    # 2. stdin JSON (string field, shallow)
    _cap_load_stdin
    if [[ -n "$_CAP_STDIN_JSON" ]]; then
        val="$(_cap_json_field "$_CAP_STDIN_JSON" "$name")"
        if [[ -n "$val" ]]; then printf '%s' "$val"; return; fi
    fi
    # 3. default
    printf '%s' "$default"
}

# _cap_json_field <json> <key> -- shallow string/number/bool extractor. Uses
# jq when available (robust), else a best-effort grep fallback.
function _cap_json_field() {
    local json="$1" key="$2"
    if command -v jq &>/dev/null; then
        printf '%s' "$json" | jq -r --arg k "$key" \
            'if (type=="object") and (has($k)) and (.[$k] != null) then (.[$k]|tostring) else empty end' 2>/dev/null
        return
    fi
    printf '%s' "$json" | grep -oE "\"${key}\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[^,}[:space:]]+)" \
        | head -1 | sed -E "s/\"${key}\"[[:space:]]*:[[:space:]]*//; s/^\"//; s/\"$//"
}

# cap_parse_flags "$@" -- parses --name=value and --name (bare -> "1") into
# CAP_ARG_<NAME> globals. --help / --print-spec are handled by cap_handle_meta.
# Unknown flags are rejected (exit 2) to keep the param surface honest.
function cap_parse_flags() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --help|--print-spec) shift ;;  # handled by cap_handle_meta
            --*=*)
                local kv="${1#--}" name value upper
                name="${kv%%=*}"; value="${kv#*=}"
                upper="$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
                printf -v "CAP_ARG_${upper}" '%s' "$value"
                shift ;;
            --*)
                local name="${1#--}" upper
                upper="$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
                printf -v "CAP_ARG_${upper}" '%s' "1"
                shift ;;
            *) cap_fail 2 "unexpected positional argument: $1" ;;
        esac
    done
}

# cap_flag <name> -- returns the parsed flag value (or "" if unset). Requires
# cap_parse_flags to have run.
function cap_flag() {
    local upper var
    upper="$(printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_')"
    var="CAP_ARG_${upper}"
    printf '%s' "${!var:-}"
}

# cap_require <name> <value> -- fails (exit 2) when a required param is empty.
function cap_require() {
    if [[ -z "${2:-}" ]]; then
        cap_fail 2 "missing required parameter: ${1}"
    fi
}

#=============================================================================
# NON-INTERACTIVE CONFIRMATION
#=============================================================================

# cap_confirm_or_die <provided> <expected-phrase>
# Replaces interactive `read -p "type X to confirm"` prompts. The caller (a
# human or the action executor) must pass the exact phrase as a param; there
# is never a blocking prompt. Mismatch -> exit 3 (refused).
function cap_confirm_or_die() {
    local provided="${1:-}" expected="${2:?expected phrase required}"
    if [[ "$provided" != "$expected" ]]; then
        cap_fail 3 "confirmation required: pass the exact phrase '${expected}' (got '${provided:-<empty>}')"
    fi
}

#=============================================================================
# META FLAGS -- --help / --print-spec
#=============================================================================

# cap_handle_meta "$@" -- if invoked with --help or --print-spec, print and
# exit 0 (marking the result as emitted so the EXIT trap stays quiet).
function cap_handle_meta() {
    local a
    for a in "$@"; do
        case "$a" in
            --print-spec) _cap_print_spec; _CAP_EMITTED=1; exit 0 ;;
            --help|-h)    _cap_print_help; _CAP_EMITTED=1; exit 0 ;;
        esac
    done
}

# _cap_print_spec -- machine-readable capability descriptor (stdout JSON).
function _cap_print_spec() {
    local params="" entry first=1
    for entry in "${_CAP_SPEC_PARAMS[@]:-}"; do
        [[ -z "$entry" ]] && continue
        local name="${entry%%|*}" rest="${entry#*|}"
        local desc="${rest%%|*}" env="${rest#*|}"
        [[ "$first" == "1" ]] || params+=","
        first=0
        params+="{\"name\":\"$(cap_json_escape "$name")\",\"description\":\"$(cap_json_escape "$desc")\",\"env\":\"$(cap_json_escape "$env")\"}"
    done
    printf '{"capability":"%s","summary":"%s","params":[%s]}\n' \
        "$(cap_json_escape "$_CAP_NAME")" "$(cap_json_escape "$_CAP_SUMMARY")" "$params"
}

# _cap_print_help -- human help (stderr-friendly text, printed to stdout for --help).
function _cap_print_help() {
    printf 'Capability: %s\n' "$_CAP_NAME"
    printf '  %s\n\n' "$_CAP_SUMMARY"
    printf 'Parameters (precedence: --flag=value > stdin JSON > env):\n'
    local entry
    for entry in "${_CAP_SPEC_PARAMS[@]:-}"; do
        [[ -z "$entry" ]] && continue
        local name="${entry%%|*}" rest="${entry#*|}"
        local desc="${rest%%|*}" env="${rest#*|}"
        if [[ -n "$env" ]]; then
            printf '  --%-22s %s (env: %s)\n' "$name" "$desc" "$env"
        else
            printf '  --%-22s %s\n' "$name" "$desc"
        fi
    done
    printf '\nMeta:\n'
    printf '  --print-spec           emit the JSON capability descriptor\n'
    printf '  --help                 show this help\n'
    printf '\nThis script is a capability backend: non-interactive, idempotent,\n'
    printf 'JSON result on stdout, human logs on stderr, honest exit codes.\n'
    printf 'Contract: docs/internal/design/capability-script-contract.md\n'
}
