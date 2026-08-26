#!/usr/bin/env bash
#
# scripts/deploy/verify-internal-tls.sh
# =====================================
#
# Capability: deploy.verifyInternalTls -- does the CA the mesh TRUSTS match the
# CA that SIGNED identity's serving certificate?
#
# WHY THIS EXISTS (memql#4599). When the trust bundle and the leaf disagree,
# every mesh node rejects identity with `remote error: tls: bad certificate` --
# and every pod stays Running and Ready, ArgoCD reports Healthy, and the only
# visible symptom is a 502 on "Continue to sign in". There is no CrashLoopBackOff
# to notice and no log line naming TLS. On a live install that shape cost an
# upgrade window: `v0.19.9 -> v0.20.0` adopted components/internal-tls onto a
# cluster whose `memql-ca` had been hand-seeded, cert-manager signed identity-tls
# against the outgoing CA one second before replacing it, and nobody could sign
# in.
#
# The component no longer has that race -- the CA lives in a Secret of its own,
# so a leaf cannot be signed by a CA about to be replaced. This check is the
# other half: a ROTATION still leaves running pods trusting the CA they booted
# with, and this is what says so in five seconds instead of leaving somebody to
# infer it from a 502.
#
# WHAT IT COMPARES. The SHA-256 fingerprint of the certificate in `memql-ca`'s
# `ca.crt` against the issuer of the certificate in `identity-tls`'s `tls.crt`.
# Equal means every mesh node that boots now will trust identity. Unequal means
# it will not, and the remedy is printed.
#
# IT REPORTS, IT DOES NOT DECIDE, and it changes nothing (contract rule 7).
# `matches` comes back in the envelope; exit 3 marks a mismatch so a lifecycle
# step can refuse to proceed, which is the whole point of running it BEFORE an
# upgrade rather than after.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok (bundle and signer agree) | 2 bad param | 3 they disagree | 4 prerequisite missing | 5 the check could not run
#
# Refs: memql#4599 memql#4484 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.verifyInternalTls" \
    "Compare the CA the mesh trusts against the CA that signed identity's serving certificate."

cap_spec_param "context"     "kubectl context to read (default: the current one)"
cap_spec_param "namespace"   "namespace the instance runs in (default memql)"
cap_spec_param "caSecret"    "Secret carrying the mounted trust bundle (default memql-ca)"
cap_spec_param "leafSecret"  "Secret carrying identity's serving certificate (default identity-tls)"

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    local tool
    for tool in kubectl openssl base64; do
        command -v "$tool" &>/dev/null || cap_fail 4 "${tool} is not installed"
    done
}

# kctl -- kubectl with the optional --context. A bash array is deliberately
# NOT used: bash 3.2 (which is /bin/bash on macOS, where an operator runs this)
# treats an EMPTY array as unbound under `set -u`, so "${KCTX[@]}" aborts the
# script when no context was passed -- the default case.
function kctl() {
    if [[ -n "$CAP_KUBE_CONTEXT" ]]; then
        kubectl --context="$CAP_KUBE_CONTEXT" "$@"
    else
        kubectl "$@"
    fi
}

# read_secret_key <ns> <secret> <key> -- the decoded PEM, or empty.
function read_secret_key() {
    local ns="$1" secret="$2" key="$3" b64
    b64="$(kctl -n "$ns" get secret "$secret" \
        -o "jsonpath={.data.${key//./\\.}}" 2>/dev/null || true)"
    [[ -z "$b64" ]] && return 0
    printf '%s' "$b64" | base64 -d 2>/dev/null || true
}

# subject_fingerprint <pem> -- "<sha256 fingerprint>|<subject>" for a cert PEM.
function subject_fingerprint() {
    local pem="$1" fp subj
    fp="$(printf '%s' "$pem" | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/.*=//')"
    subj="$(printf '%s' "$pem" | openssl x509 -noout -subject 2>/dev/null | sed 's/^subject=//; s/^ *//')"
    printf '%s|%s' "$fp" "$subj"
}

# issuer_of <pem> -- the issuer DN of a leaf, normalised for comparison.
function issuer_of() {
    printf '%s' "$1" | openssl x509 -noout -issuer 2>/dev/null | sed 's/^issuer=//; s/^ *//'
}

function verify() {
    local ns="$1" ca_secret="$2" leaf_secret="$3"

    local bundle_pem leaf_pem
    bundle_pem="$(read_secret_key "$ns" "$ca_secret" "ca.crt")"
    if [[ -z "$bundle_pem" ]]; then
        cap_fail 5 "secret ${ca_secret} in ${ns} has no ca.crt -- the mesh mounts that key, so there is no trust bundle to compare"
    fi
    leaf_pem="$(read_secret_key "$ns" "$leaf_secret" "tls.crt")"
    if [[ -z "$leaf_pem" ]]; then
        cap_fail 5 "secret ${leaf_secret} in ${ns} has no tls.crt -- identity has no serving certificate to compare"
    fi

    local bundle_fp bundle_subj leaf_issuer
    IFS='|' read -r bundle_fp bundle_subj <<< "$(subject_fingerprint "$bundle_pem")"
    leaf_issuer="$(issuer_of "$leaf_pem")"
    [[ -z "$bundle_fp" ]] && cap_fail 5 "could not parse ${ca_secret}/ca.crt as a certificate"
    [[ -z "$leaf_issuer" ]] && cap_fail 5 "could not parse ${leaf_secret}/tls.crt as a certificate"

    # THE COMPARISON IS BY SUBJECT, NOT BY FINGERPRINT, and the distinction is
    # the whole bug: the two CAs in memql#4599 shared a common name
    # (memql-internal-ca) and differed only in fingerprint. So a name match is
    # NOT sufficient -- it is a necessary first check that gives a readable
    # message, and the fingerprints are then compared by verifying the leaf
    # against the bundle, which is the only test that answers the question a
    # mesh node actually asks.
    #
    # NO -partial_chain: the bundle CA is a self-signed ROOT, so a plain chain
    # verify is the exact test, and -partial_chain is absent from the LibreSSL
    # that ships as /usr/bin/openssl on macOS -- where this runs from an
    # operator's laptop. Temp files rather than process substitution for the
    # same portability reason.
    local ca_file leaf_file verify_out rc=0
    ca_file="$(mktemp)"; leaf_file="$(mktemp)"
    printf '%s' "$bundle_pem" > "$ca_file"
    printf '%s' "$leaf_pem"   > "$leaf_file"
    verify_out="$(openssl verify -CAfile "$ca_file" "$leaf_file" 2>&1)" || rc=$?
    rm -f "$ca_file" "$leaf_file"
    # Strip the temp path so the message names the certificate, not a tmpfile.
    verify_out="${verify_out//${leaf_file}: /}"

    cap_result_set     namespace       "$ns"
    cap_result_set     caSecret        "$ca_secret"
    cap_result_set     leafSecret      "$leaf_secret"
    cap_result_set     trustBundleCa   "$bundle_subj"
    cap_result_set     trustBundleFingerprint "$bundle_fp"
    cap_result_set     leafIssuer      "$leaf_issuer"

    if [[ $rc -eq 0 ]]; then
        cap_result_set_raw matches true
        cap_info "the mesh's trust bundle signed identity's certificate; a node booting now will trust identity."
        cap_info "  trust bundle: ${bundle_subj} (${bundle_fp})"
        cap_ok
    fi

    cap_result_set_raw matches false
    cap_warn "THE MESH DOES NOT TRUST IDENTITY. The CA in ${ca_secret} did not sign ${leaf_secret}."
    cap_warn "  trust bundle: ${bundle_subj} (${bundle_fp})"
    cap_warn "  leaf issuer:  ${leaf_issuer}"
    cap_warn "  openssl:      ${verify_out}"
    cap_info "Every mesh node will reject identity with 'remote error: tls: bad certificate'. Pods stay"
    cap_info "Running and Ready and ArgoCD reports Healthy; the only symptom is a 502 on sign-in."
    cap_info "A RESTART ALONE DOES NOT FIX A LEAF SIGNED BY THE WRONG CA. Reissue it, then roll the mesh:"
    cap_info "  kubectl -n ${ns} delete secret ${leaf_secret}"
    cap_info "  kubectl -n ${ns} rollout restart deploy/identity"
    cap_info "  kubectl -n ${ns} rollout restart deploy/bff deploy/cognition deploy/agent deploy/planner deploy/workbench deploy/edge"
    cap_result_set nextStep "delete secret ${leaf_secret} so cert-manager remints it from the current CA, then roll identity and the mesh"
    cap_fail 3 "the trust bundle in ${ca_secret} did not sign ${leaf_secret}; sign-in is broken until the leaf is reissued"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ctx ns ca_secret leaf_secret
    ctx="$(cap_param context "")"
    ns="$(cap_param namespace memql)"
    ca_secret="$(cap_param caSecret memql-ca)"
    leaf_secret="$(cap_param leafSecret identity-tls)"

    cap_require namespace  "$ns"
    cap_require caSecret   "$ca_secret"
    cap_require leafSecret "$leaf_secret"

    CAP_KUBE_CONTEXT="$ctx"

    check_prerequisites
    cap_step "kubectl${ctx:+ --context=$ctx} -n ${ns} get secret ${ca_secret} ${leaf_secret}"
    verify "$ns" "$ca_secret" "$leaf_secret"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
