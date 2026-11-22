#!/usr/bin/env bash
#
# scripts/k3d/federation-keys.sh
# ==============================
#
# Capability: k3d.federationKeys -- print what a vendor needs in order to trust
# this cluster's identity tokens.
#
# Why this exists
# ---------------
# The cluster's account at Anthropic or OpenAI is workload identity federation:
# each engine pod presents a token the cluster signed, and the vendor checks the
# signature against the cluster's PUBLIC keys. A cloud cluster publishes those
# keys at its issuer address and the vendor fetches them. A local cluster's
# issuer is private, so the vendor is GIVEN the keys instead: both vendors
# accept a pasted set (Anthropic calls it inline keys, OpenAI uploaded keys),
# and after that neither contacts the cluster. This script prints exactly what
# is pasted: the issuer, the public keys, the subject each engine pod's token
# carries, and the two audiences.
#
# The repository used to say a local cluster could never federate. It can; what
# it cannot do is be DISCOVERED (design record
# docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md, A6).
#
# PUBLIC MATERIAL ONLY. It reads two public documents from the API server and a
# list of service account names. It refuses to print a key document that
# carries a private member, so a wrong endpoint or a hand-edited file cannot
# turn this into the command that leaked a signing key.
#
# It works on any cluster, local or cloud; it is not a second way to deploy.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make federation-keys
#   make federation-keys CONTEXT=k3d-memql NAMESPACE=memql
#   scripts/k3d/federation-keys.sh --context=k3d-memql
#   scripts/k3d/federation-keys.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (kubectl, python3)
#             | 5 the cluster could not be read, or its answer was not usable
#
# Runbooks: docs/public/operate/auth/anthropic-federation.md
#           docs/public/operate/auth/openai-federation.md

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/engine_build_args.sh
# ENGINE_NODE_TYPES: THE list of engine Deployments, in shell. Read here rather
# than restated, so a node type added or retired changes this script with it.
source "${SCRIPT_DIR}/../lib/engine_build_args.sh"

cap_init "k3d.federationKeys" \
    "Print what a vendor needs to trust this cluster's identity tokens: the issuer, the public keys, each engine pod's token subject and the two audiences. Public material only."
cap_spec_param "context"   "kubectl context to read (default: the current context)"
cap_spec_param "namespace" "namespace the engine runs in (default: memql, or MEMQL_K3D_NAMESPACE)"

#=============================================================================
# CONFIGURATION
#=============================================================================

CONTEXT=""
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"

# The audiences the manifests mint each pod's two vendor tokens for
# (deploy/k8s/base, deploy/k8s/components/engine-bff). A vendor's rule pins the
# same string.
ANTHROPIC_AUDIENCE="https://api.anthropic.com"
OPENAI_AUDIENCE="https://api.openai.com/v1"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }

#=============================================================================
# READING THE CLUSTER
#=============================================================================

# kc -- kubectl against the named context, or the current one.
function kc() {
    if [[ -n "${CONTEXT}" ]]; then
        kubectl --context "${CONTEXT}" "$@"
    else
        kubectl "$@"
    fi
}

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is required but not installed."
    fi
    if ! command -v python3 &>/dev/null; then
        cap_fail 4 "python3 is required but not installed."
    fi
}

# read_raw <path> <what> -- one public document from the API server.
function read_raw() {
    local path="$1" what="$2" out=""
    if ! out="$(kc get --raw "${path}" 2>&1)"; then
        cap_fail 5 "could not read ${what} (${path}) from the cluster: $(printf '%s' "${out}" | tr '\n' ' ' | cut -c1-300)"
    fi
    printf '%s' "${out}"
}

# issuer_of <openid-configuration json> -- the issuer string.
function issuer_of() {
    python3 -c '
import json, sys
doc = json.loads(sys.argv[1])
issuer = doc.get("issuer", "")
if not isinstance(issuer, str) or not issuer:
    sys.exit(1)
print(issuer)
' "$1" 2>/dev/null
}

# public_jwks <jwks json> -- the document, compact, after checking that it holds
# at least one key and that NO key carries a private member.
function public_jwks() {
    python3 -c '
import json, sys
doc = json.loads(sys.argv[1])
keys = doc.get("keys")
if not isinstance(keys, list) or not keys:
    sys.exit(2)
private = {"d", "p", "q", "dp", "dq", "qi", "oth", "k"}
for key in keys:
    if not isinstance(key, dict) or private & set(key):
        sys.exit(3)
print(json.dumps(doc, separators=(",", ":"), sort_keys=True))
' "$1" 2>/dev/null
}

# engine_subjects -- one line per distinct subject of the engine pods that call
# models, sorted. A cluster that has not deployed one of them yet simply
# contributes nothing for it.
#
# NOT THE IDENTITY NODE. It runs under a service account of its own, never
# calls a model, and does not sign in to a vendor (app/build_identity.go). A
# vendor's rule must not admit it, so its subject is not offered for pasting.
function engine_subjects() {
    local accounts="" node
    local -a model_nodes=()
    for node in "${ENGINE_NODE_TYPES[@]}"; do
        [[ "${node}" == "identity" ]] || model_nodes+=("${node}")
    done
    if ! accounts="$(kc get deployments "${model_nodes[@]}" -n "${NAMESPACE}" --ignore-not-found \
            -o jsonpath='{range .items[*]}{.spec.template.spec.serviceAccountName}{"\n"}{end}' 2>&1)"; then
        cap_fail 5 "could not read the engine Deployments in namespace '${NAMESPACE}': $(printf '%s' "${accounts}" | tr '\n' ' ' | cut -c1-300)"
    fi
    printf '%s\n' "${accounts}" | grep -E '^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$' | sort -u | while read -r account; do
        printf 'system:serviceaccount:%s:%s\n' "${NAMESPACE}" "${account}"
    done
}

# json_string_array -- stdin lines as a JSON array of strings.
function json_string_array() {
    python3 -c '
import json, sys
print(json.dumps([line.strip() for line in sys.stdin if line.strip()]))
'
}

#=============================================================================
# WHAT A PERSON READS
#=============================================================================

function explain() {
    local issuer="$1" subjects="$2"
    info "Issuer:    ${issuer}"
    info "Subjects:"
    printf '%s\n' "${subjects}" | while read -r subject; do
        [[ -n "${subject}" ]] && info "  ${subject}"
    done
    info "Audiences: Anthropic ${ANTHROPIC_AUDIENCE}   OpenAI ${OPENAI_AUDIENCE}"
    info ""
    info "The public keys are the 'jwks' field of the result on stdout. They are public: they"
    info "verify the cluster's tokens and cannot sign one."
    info "  Anthropic: Console > Settings > Workload identity. Register the issuer above with"
    info "             INLINE keys and paste the jwks. One rule: subject as above, audience as above."
    info "  OpenAI:    register the identity provider for the issuer above, choose UPLOADED keys"
    info "             for token verification and upload the jwks. Map each subject above."
    info "A cluster that is recreated signs with a new key unless it keeps its signing key;"
    info "'make up' keeps a local cluster's. Run this again after anything that changed it."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CONTEXT="$(cap_param context "${CONTEXT}")"
    NAMESPACE="$(cap_param namespace "${NAMESPACE}")"
    if ! [[ "${NAMESPACE}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
        cap_fail 2 "namespace must be a Kubernetes namespace name, got: '${NAMESPACE}'"
    fi

    check_prerequisites

    local configuration issuer jwks_raw jwks subjects subjects_json
    configuration="$(read_raw /.well-known/openid-configuration "the issuer document")"
    if ! issuer="$(issuer_of "${configuration}")"; then
        cap_fail 5 "the cluster's issuer document names no issuer"
    fi

    jwks_raw="$(read_raw /openid/v1/jwks "the public keys")"
    if ! jwks="$(public_jwks "${jwks_raw}")"; then
        cap_fail 5 "the cluster's key document is not a set of PUBLIC keys (it is empty, malformed, or carries a private member); nothing was printed"
    fi

    subjects="$(engine_subjects)"
    if [[ -z "${subjects}" ]]; then
        warn "No engine Deployment was found in namespace '${NAMESPACE}', so no subject is listed. The keys and the issuer are still right."
    fi
    subjects_json="$(printf '%s\n' "${subjects}" | json_string_array)"

    explain "${issuer}" "${subjects}"

    cap_result_set     issuer    "${issuer}"
    cap_result_set     namespace "${NAMESPACE}"
    cap_result_set_raw jwks      "${jwks}"
    cap_result_set_raw subjects  "${subjects_json}"
    cap_result_set_raw audiences "{\"anthropic\":\"${ANTHROPIC_AUDIENCE}\",\"openai\":\"${OPENAI_AUDIENCE}\"}"
    cap_ok
}

main "$@"
