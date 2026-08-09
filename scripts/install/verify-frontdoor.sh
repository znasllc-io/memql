#!/usr/bin/env bash
#
# scripts/install/verify-frontdoor.sh
# ===================================
#
# Capability: install.verifyFrontDoor -- prove the local front door works.
#
# The front door is the ONE connection path clients use in every environment
# (env parity: ingress -> TLS -> gRPC -> bff, dialed as https://cockpit.<domain>).
# Locally it stands on three independent legs, each of which can be broken
# while the other two look perfect:
#
#   dns   the hostname resolves to 127.0.0.1 -- and to NOTHING ELSE. A
#         hostname pointing at some other address is a worse failure than one
#         that does not resolve: the installer would hand traffic to a
#         stranger's box while every symptom points at the cluster.
#   tls   https://<host>/ completes a handshake against a TRUSTED certificate
#         (locally, the mkcert wildcard; in the cloud, cert-manager).
#   grpc  the front door negotiates HTTP/2. gRPC cannot exist over HTTP/1.1,
#         so a door that answers but will not speak h2 is a broken door.
#
# Each check reports ITSELF -- name, host, passed, detail -- because "the front
# door is broken" is not actionable and "dns for identity.local.znas.io
# resolves to 10.0.0.5" is. The rollup `allPassed` is the single boolean the
# graph verifies on.
#
# EXIT CODES:
#
#   0  every check passed (or --report-only was set)
#   2  bad param (no hosts)
#   4  prerequisite missing (no curl, or no resolver at all)
#   5  STRICT DEFAULT: at least one check failed
#
# --report-only turns the failing exit into 0. It does NOT change `allPassed`
# or any check: it waives strictness, it does not launder the truth.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-frontdoor.sh
#   scripts/install/verify-frontdoor.sh --hosts=cockpit.local.znas.io --report-only
#   scripts/install/verify-frontdoor.sh --print-spec
#
# Refs: #3365 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.verifyFrontDoor" \
    "Check dns/tls per front-door hostname plus a gRPC (h2) reachability probe."
cap_spec_param "hosts"       "comma-separated front-door hostnames (default: the local overlay's)"
cap_spec_param "grpc-host"   "hostname for the gRPC reachability probe (default: the first host)"
cap_spec_param "port"        "TLS port (default 443)"
cap_spec_param "expect-addr" "the address every hostname must resolve to (default 127.0.0.1)"
cap_spec_param "timeout"     "per-probe timeout in seconds (default 8)"
cap_spec_param "report-only" "report failures without failing the run (flag)"

# The local overlay's front door (deploy/k8s/overlays/local): traefik serves
# both on 443 with the mkcert *.local.znas.io wildcard.
DEFAULT_HOSTS="cockpit.local.znas.io,identity.local.znas.io"

#=============================================================================
# CHECK LEDGER -- every probe records itself here, pass or fail
#=============================================================================

_FD_CHECKS=()
_FD_PASSED=0
_FD_FAILED=0

# record_check <name> <host> <true|false> <detail>
# MUST be called from the main shell (it appends to a global array), so no
# probe may run inside a "$(...)" substitution.
function record_check() {
    local name="$1" host="$2" passed="$3" detail="$4"
    _FD_CHECKS+=("{\"name\":\"$(cap_json_escape "$name")\",\"host\":\"$(cap_json_escape "$host")\",\"passed\":${passed},\"detail\":\"$(cap_json_escape "$detail")\"}")
    if [[ "$passed" == "true" ]]; then
        _FD_PASSED=$((_FD_PASSED + 1))
        cap_info "PASS ${name} ${host}: ${detail}"
    else
        _FD_FAILED=$((_FD_FAILED + 1))
        cap_error "FAIL ${name} ${host}: ${detail}"
    fi
}

function checks_json() {
    local IFS=,
    printf '[%s]' "${_FD_CHECKS[*]:-}"
}

#=============================================================================
# PREREQUISITES
#=============================================================================

# resolver_tool -- names the resolver this machine actually has. getent is the
# Linux/glibc path; dig and host cover macOS, where getent does not exist.
function resolver_tool() {
    if command -v getent &>/dev/null; then printf 'getent'; return; fi
    if command -v dig    &>/dev/null; then printf 'dig';    return; fi
    if command -v host   &>/dev/null; then printf 'host';   return; fi
    printf ''
}

function check_prerequisites() {
    if ! command -v curl &>/dev/null; then
        cap_fail 4 "curl is not installed; cannot probe the front door"
    fi
    if [[ -z "$(resolver_tool)" ]]; then
        cap_fail 4 "no resolver available (need one of: getent, dig, host)"
    fi
}

#=============================================================================
# PROBE 1 -- DNS
#=============================================================================

# resolve_addresses <host> -- prints each resolved IPv4 address on its own
# line. Empty output means the name did not resolve.
function resolve_addresses() {
    local host="$1"
    case "$(resolver_tool)" in
        getent) getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u ;;
        dig)    dig +short A "$host" 2>/dev/null | grep -E '^[0-9]+(\.[0-9]+){3}$' | sort -u ;;
        host)   host -t A "$host" 2>/dev/null | awk '/has address/ {print $NF}' | sort -u ;;
    esac
}

# check_dns <host> <expected-addr>
function check_dns() {
    local host="$1" expect="$2" addrs joined
    addrs="$(resolve_addresses "$host" || true)"
    joined="$(printf '%s' "$addrs" | tr '\n' ' ' | sed -e 's/  */ /g' -e 's/ $//')"

    if [[ -z "$joined" ]]; then
        record_check dns "$host" false "does not resolve (no A record and no hosts entry)"
        return
    fi
    # Strict on purpose: resolving ANYWHERE other than the expected address
    # sends installer traffic to a machine nobody vetted.
    local addr bad=""
    for addr in $joined; do
        [[ "$addr" == "$expect" ]] || bad+="${addr} "
    done
    if [[ -n "$bad" ]]; then
        record_check dns "$host" false "resolves to ${joined} -- must resolve to ${expect} only (offending: ${bad% })"
        return
    fi
    record_check dns "$host" true "resolves to ${expect}"
}

#=============================================================================
# PROBE 2 + 3 -- TLS and gRPC (h2), over the real HTTPS front door
#=============================================================================

# https_probe <host> <port> <timeout> -- sets _FD_RC / _FD_VERSION / _FD_CODE.
# Globals rather than a captured "$(...)" so a later cap_fail is never trapped
# in a subshell.
_FD_RC=0
_FD_VERSION=""
_FD_CODE=""
function https_probe() {
    local host="$1" port="$2" timeout="$3" out=""
    _FD_RC=0
    out="$(curl --silent --show-error \
                --http2 \
                --max-time "$timeout" \
                --output /dev/null \
                --write-out '%{http_version} %{http_code}' \
                "https://${host}:${port}/" 2>/dev/null)" || _FD_RC=$?
    _FD_VERSION="${out%% *}"
    _FD_CODE="${out##* }"
}

# curl_tls_hint <rc> -- turns curl's exit code into something an operator can act on.
function curl_tls_hint() {
    case "$1" in
        6)  printf 'could not resolve host' ;;
        7)  printf 'connection refused -- nothing is listening on the port' ;;
        28) printf 'timed out' ;;
        35) printf 'TLS handshake failed' ;;
        51) printf 'certificate hostname mismatch' ;;
        60) printf 'certificate not trusted -- run `mkcert -install` and re-issue the wildcard' ;;
        *)  printf 'transport failure' ;;
    esac
}

# check_tls <host> <port> <timeout>
function check_tls() {
    local host="$1" port="$2" timeout="$3"
    https_probe "$host" "$port" "$timeout"
    if [[ "$_FD_RC" != "0" ]]; then
        record_check tls "$host" false "curl exit ${_FD_RC}: $(curl_tls_hint "$_FD_RC")"
        return
    fi
    record_check tls "$host" true "TLS handshake ok against a trusted certificate (HTTP ${_FD_CODE})"
}

# check_grpc <host> <port> <timeout> -- gRPC requires HTTP/2 end to end, so the
# reachability signal is "the front door negotiated h2 and answered".
function check_grpc() {
    local host="$1" port="$2" timeout="$3"
    https_probe "$host" "$port" "$timeout"
    if [[ "$_FD_RC" != "0" ]]; then
        record_check grpc "$host" false "curl exit ${_FD_RC}: $(curl_tls_hint "$_FD_RC")"
        return
    fi
    case "$_FD_VERSION" in
        2|2.0)
            record_check grpc "$host" true "negotiated HTTP/2 (HTTP ${_FD_CODE}) -- gRPC can ride this door"
            ;;
        *)
            record_check grpc "$host" false "negotiated HTTP/${_FD_VERSION:-unknown}, not h2 -- gRPC cannot run over HTTP/1.1"
            ;;
    esac
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local hosts grpc_host port expect timeout report_only
    hosts="$(cap_param hosts "$DEFAULT_HOSTS")"
    port="$(cap_param port 443)"
    expect="$(cap_param expect-addr 127.0.0.1)"
    timeout="$(cap_param timeout 8)"
    report_only="$(cap_flag report-only)"
    cap_require hosts "$hosts"

    local -a host_list=()
    IFS=',' read -ra host_list <<< "$hosts"
    if [[ ${#host_list[@]} -eq 0 || -z "${host_list[0]}" ]]; then
        cap_fail 2 "no hostnames to check"
    fi
    grpc_host="$(cap_param grpc-host "${host_list[0]}")"

    check_prerequisites

    local host
    for host in "${host_list[@]}"; do
        [[ -z "$host" ]] && continue
        cap_step "checking ${host}"
        check_dns "$host" "$expect"
        check_tls "$host" "$port" "$timeout"
    done
    cap_step "checking gRPC reachability on ${grpc_host}"
    check_grpc "$grpc_host" "$port" "$timeout"

    local all_passed=true
    [[ "$_FD_FAILED" -eq 0 ]] || all_passed=false

    cap_result_set     hosts       "$hosts"
    cap_result_set     grpcHost    "$grpc_host"
    cap_result_set_raw allPassed   "$all_passed"
    cap_result_set_raw passedCount "$_FD_PASSED"
    cap_result_set_raw failedCount "$_FD_FAILED"
    cap_result_set_raw reportOnly  "$( [[ -n "$report_only" ]] && echo true || echo false )"
    cap_result_set_raw checks      "$(checks_json)"

    if [[ "$all_passed" == "true" ]]; then
        cap_info "Front door healthy: ${_FD_PASSED}/${_FD_PASSED} checks passed."
        cap_ok
    fi
    if [[ -n "$report_only" ]]; then
        cap_warn "${_FD_FAILED} check(s) failed; --report-only set, exiting 0 anyway."
        cap_ok
    fi
    cap_fail 5 "${_FD_FAILED} of $((_FD_PASSED + _FD_FAILED)) front-door checks failed"
}

main "$@"
