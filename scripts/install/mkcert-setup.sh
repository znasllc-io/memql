#!/usr/bin/env bash
#
# scripts/install/mkcert-setup.sh
# ===============================
#
# Capability: install.mkcert -- ensure a trusted local CA and issue the front
# door's wildcard certificate.
#
# The local cluster is reached exactly as staging is: over TLS, at
# https://cockpit.local.znas.io and https://identity.local.znas.io (env
# parity -- docs/public/operate/environment-parity.md). Traefik terminates
# that with the browser-trusted `*.local.znas.io` pair which
# scripts/k3d/seed-secrets.sh loads into the cluster as the local-znas-tls
# secret. This capability produces that pair.
#
# THE RESTRAINT THAT MATTERS
#
# mkcert's root CA is PER MACHINE, not per project. If one already exists it
# may be signing certificates for half a dozen other local stacks the operator
# depends on. So when $CAROOT/rootCA.pem is present this script reports
# caPreExisting=true / caInstalled=false and leaves it completely alone -- it
# does not regenerate it and does not re-run `mkcert -install` against it.
# Installing memQL must never be the reason someone's other local projects
# start failing TLS.
#
# When there is NO CA, creating one writes to the machine's trust store, so
# that step is gated on --confirm=install-memql-ca. There is no prompt
# (contract rule 3); mkcert's own sudo prompt is the OS-level gate, this is the
# capability-level one. A run that only issues a certificate against an
# existing CA needs no confirmation, because it changes nothing shared.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/mkcert-setup.sh --confirm=install-memql-ca
#   scripts/install/mkcert-setup.sh --hostnames='*.local.znas.io,local.znas.io' \
#       --cert-file=/path/dev.crt --key-file=/path/dev.key
#   scripts/install/mkcert-setup.sh --force        # reissue an existing pair
#   scripts/install/mkcert-setup.sh --print-spec
#
# Exit codes:
#   0 ok | 2 bad param | 3 refused (CA install without --confirm)
#   4 prerequisite missing (mkcert not installed)
#   5 operation failed (mkcert errored, or produced nothing)
#
# Refs: #3362 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "install.mkcert" "Ensure a trusted local CA and issue the front-door wildcard certificate."
cap_spec_param "hostnames" "comma/space separated names for the cert (default: the front-door wildcard + apex)"
cap_spec_param "cert-file" "where to write the certificate"
cap_spec_param "key-file"  "where to write the private key"
cap_spec_param "caroot"    "mkcert CAROOT to use (default: whatever mkcert reports)"
cap_spec_param "mkcert"    "path to the mkcert binary (default: resolved from PATH)"
cap_spec_param "force"     "reissue the certificate even when one exists (flag)"
cap_spec_param "confirm"   "exact phrase 'install-memql-ca'; required only when no CA exists yet"

# Hostnames and the on-disk pair location are declared once, in scripts/lib, so
# this issuer and the seeder that loads the pair into the cluster cannot drift
# apart -- a drifted default is exactly how memql#3384 happened.
readonly DEFAULT_HOSTNAMES="$MEMQL_LOCAL_TLS_HOSTNAMES"
readonly CONFIRM_INSTALL_CA="install-memql-ca"

#=============================================================================
# PARAMETER VALIDATION
#=============================================================================

HOSTNAMES=()

function parse_hostnames() {
    local raw="$1" host
    raw="${raw//,/ }"
    HOSTNAMES=()
    # Word splitting is the point here.
    # shellcheck disable=SC2206
    local parts=( $raw )
    for host in "${parts[@]:-}"; do
        [[ -z "$host" ]] && continue
        # A leading '*.' label is the only wildcard form mkcert (and TLS) can
        # do; anything else with a '*' in it would produce a certificate that
        # silently matches nothing.
        if [[ ! "$host" =~ ^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
            cap_fail 2 "invalid hostname: '${host}' (a leading '*.' label, then letters, digits, '.' and '-')"
        fi
        HOSTNAMES+=("$host")
    done
    if [[ "${#HOSTNAMES[@]}" -eq 0 ]]; then
        cap_fail 2 "no hostnames given: --hostnames must name at least one host"
    fi
}

#=============================================================================
# PREREQUISITES
#=============================================================================

# NOTE: the resolve_* helpers set a global rather than printing their result.
# cap_fail inside a "$(...)" substitution would emit its envelope into the
# capture instead of onto stdout, and the caller would abort into the trap's
# generic "aborted without an explicit result" -- an honest exit code carrying
# a useless message, which for a missing prerequisite is exactly the message
# that had to be useful. Anything that can fail runs in the parent shell.
MKCERT_BIN=""
CAROOT_DIR=""

# resolve_mkcert <candidate> -- sets MKCERT_BIN, or fails 4. mkcert is
# deliberately NOT auto-installed: it writes to the system trust store, so
# which package manager touches this machine is the operator's call.
function resolve_mkcert() {
    local candidate="$1"
    if ! MKCERT_BIN="$(command -v "$candidate" 2>/dev/null)" || [[ -z "$MKCERT_BIN" ]]; then
        cap_fail 4 "mkcert not found (looked for '${candidate}'); install it first: brew install mkcert  |  https://github.com/FiloSottile/mkcert"
    fi
}

# resolve_caroot <mkcert-bin> <override> -- sets CAROOT_DIR.
function resolve_caroot() {
    local bin="$1" override="$2"
    if [[ -n "$override" ]]; then
        CAROOT_DIR="$override"
        return
    fi
    if ! CAROOT_DIR="$("$bin" -CAROOT 2>/dev/null)" || [[ -z "$CAROOT_DIR" ]]; then
        cap_fail 5 "could not determine the mkcert CAROOT ('${bin} -CAROOT' produced nothing)"
    fi
}

#=============================================================================
# THE CA
#=============================================================================

# install_ca <mkcert-bin> <caroot> -- creates and trusts a new root CA. Only
# ever called when there is no CA to preserve.
function install_ca() {
    local bin="$1" caroot="$2"
    cap_step "installing a local CA into the system trust store (CAROOT=${caroot})"
    cap_info "mkcert may ask for your password -- it is writing to the trust store."
    if ! CAROOT="$caroot" "$bin" -install >&2; then
        cap_fail 5 "mkcert -install failed"
    fi
    if [[ ! -f "${caroot}/rootCA.pem" ]]; then
        cap_fail 5 "mkcert -install reported success but ${caroot}/rootCA.pem is missing"
    fi
}

#=============================================================================
# THE CERTIFICATE
#=============================================================================

function issue_cert() {
    local bin="$1" caroot="$2" cert="$3" key="$4"
    mkdir -p "$(dirname "$cert")" "$(dirname "$key")"
    cap_step "issuing ${cert} for: ${HOSTNAMES[*]}"
    if ! CAROOT="$caroot" "$bin" -cert-file "$cert" -key-file "$key" "${HOSTNAMES[@]}" >&2; then
        cap_fail 5 "mkcert failed to issue the certificate"
    fi
    if [[ ! -f "$cert" || ! -f "$key" ]]; then
        cap_fail 5 "mkcert reported success but ${cert} / ${key} are missing"
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local hostnames_raw cert key caroot_override mkcert_bin force confirm
    hostnames_raw="$(cap_param hostnames "$DEFAULT_HOSTNAMES")"
    cert="$(cap_param cert-file "${MEMQL_LOCAL_TLS_CERT:-$MEMQL_LOCAL_TLS_DEFAULT_CERT}")"
    key="$(cap_param key-file  "${MEMQL_LOCAL_TLS_KEY:-$MEMQL_LOCAL_TLS_DEFAULT_KEY}")"
    caroot_override="$(cap_param caroot "")"
    mkcert_bin="$(cap_param mkcert "mkcert")"
    force="$(cap_flag force)"
    confirm="$(cap_param confirm "")"

    parse_hostnames "$hostnames_raw"
    cap_require cert-file "$cert"
    cap_require key-file  "$key"

    resolve_mkcert "$mkcert_bin"
    local bin="$MKCERT_BIN"
    resolve_caroot "$bin" "$caroot_override"
    local caroot="$CAROOT_DIR"

    # The central question, asked before anything is touched: is there already
    # a CA on this machine?
    local ca_pre_existing=false ca_installed=false
    if [[ -f "${caroot}/rootCA.pem" ]]; then
        ca_pre_existing=true
        cap_info "root CA already present at ${caroot}/rootCA.pem -- leaving it untouched."
        cap_info "  (it may be signing certificates for other local stacks; not ours to re-install)"
    else
        cap_confirm_or_die "$confirm" "$CONFIRM_INSTALL_CA"
        install_ca "$bin" "$caroot"
        ca_installed=true
        cap_changed
    fi

    local cert_issued=false
    if [[ -f "$cert" && -f "$key" && -z "$force" ]]; then
        cap_info "certificate already present at ${cert} -- pass --force to reissue."
    else
        issue_cert "$bin" "$caroot" "$cert" "$key"
        cert_issued=true
        cap_changed
    fi

    local hostnames_json="" host first=1
    for host in "${HOSTNAMES[@]}"; do
        [[ "$first" == "1" ]] || hostnames_json+=","
        first=0
        hostnames_json+="\"$(cap_json_escape "$host")\""
    done

    cap_info "Done. seed-secrets.sh loads this pair into the cluster as local-znas-tls."
    cap_result_set     caroot        "$caroot"
    cap_result_set     certFile      "$cert"
    cap_result_set     keyFile       "$key"
    cap_result_set     mkcertBin     "$bin"
    cap_result_set_raw hostnames     "[${hostnames_json}]"
    cap_result_set_raw caPreExisting "$ca_pre_existing"
    cap_result_set_raw caInstalled   "$ca_installed"
    cap_result_set_raw certIssued    "$cert_issued"
    cap_ok
}

main "$@"
