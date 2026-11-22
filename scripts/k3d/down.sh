#!/usr/bin/env bash
#
# scripts/k3d/down.sh
# ===================
#
# Capability: k3d.down -- tear down the local k3d cluster.
#
# Deletes the cluster and optionally purges what it leaves on this machine: the
# kubeconfig entry and the cluster's token signing key. Idempotent: deleting an
# absent cluster is a successful no-op (changed=false).
#
# THE LOCAL CALLS MEDIA SERVER GOES FIRST. `make up` starts it beside the
# cluster (scripts/media/local-up.sh), with `restart: unless-stopped` and its
# key pairs rendered into ~/.memql/media, so a teardown that deleted only the
# cluster left a server that Docker brought back on every start, holding
# credentials for a cluster that no longer existed. stop_media_server runs
# scripts/media/local-down.sh (make media-down) before the cluster is deleted:
#   - stopped: it ran, and the containers and rendered files are gone (or there
#     were none);
#   - unavailable: there is nothing to stop -- the tree has no
#     scripts/media/local-down.sh (this script is staged into the editor's
#     installer, and scripts/media is not), or Docker is missing or its daemon
#     is down (local-down.sh's exit 4), and no container can run then;
#   - failed: local-down.sh failed. The cluster is still deleted, because that is
#     the teardown asked for, and then the run fails (exit 5) naming
#     make media-down, rather than answering success over a server still running.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make down                       # delete cluster, keep kubeconfig context and signing key
#   make down PURGE=1               # delete cluster + purge kubeconfig context and signing key
#   scripts/k3d/down.sh --cluster=memql --purge
#   scripts/k3d/down.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (k3d absent)
#             | 5 op failed (the media server would not stop; the cluster was
#               deleted first)
#
# Refs: #2063 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.down" "Tear down the local k3d cluster."
cap_spec_param "cluster" "k3d cluster name"
cap_spec_param "purge"   "also remove the kubeconfig context and the cluster's token signing key (flag)"
cap_spec_param "keep-signing-key" "with purge: keep the token signing key, so a cluster recreated next is the same issuer (flag; the clean-slate bring-up passes it)"

# The media server's teardown, found from the repository root rather than as a
# sibling of this directory, and only when it is there: see the file note, and
# bringup.sh, which finds local-up.sh the same way for the same reason.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MEDIA_DOWN_SCRIPT="${REPO_ROOT}/scripts/media/local-down.sh"
# stopped | unavailable | failed, and the one-line reason for the last two.
MEDIA_STATE=""
MEDIA_DETAIL=""

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v k3d &>/dev/null; then
        cap_fail 4 "k3d is not installed; nothing to tear down"
    fi
}

#=============================================================================
# TEARDOWN
#=============================================================================

function cluster_exists() {
    k3d cluster list 2>/dev/null | grep "^${1}[[:space:]]" >/dev/null
}

# delete_cluster <cluster> -- returns 0 if it deleted, 1 if it was already absent.
function delete_cluster() {
    local cluster="$1"
    if ! cluster_exists "$cluster"; then
        cap_info "Cluster '${cluster}' does not exist -- nothing to delete."
        return 1
    fi
    cap_info "Deleting k3d cluster '${cluster}'..."
    k3d cluster delete "$cluster" >&2
    cap_info "Cluster '${cluster}' deleted."
    return 0
}

# stop_media_server -- make media-down (see the file note for the three outcomes).
# local-down.sh's envelope is captured, never forwarded: this script's stdout
# carries exactly one envelope, its own. Its `changed` is this run's too.
function stop_media_server() {
    if [[ ! -f "$MEDIA_DOWN_SCRIPT" ]]; then
        MEDIA_STATE="unavailable"
        MEDIA_DETAIL="${MEDIA_DOWN_SCRIPT} is not part of this tree"
        cap_info "No local calls media server to stop: ${MEDIA_DETAIL}."
        return 0
    fi
    cap_info "Stopping the local calls media server (make media-down)..."
    local out rc=0
    out="$(bash "$MEDIA_DOWN_SCRIPT")" || rc=$?
    case "$rc" in
        0)
            MEDIA_STATE="stopped"
            # capability.sh writes the envelope as "changed":<bool> and nothing else
            # spells it, so the match is exact.
            if [[ "$out" == *'"changed":true'* ]]; then
                cap_changed
            fi
            ;;
        4)
            MEDIA_STATE="unavailable"
            MEDIA_DETAIL="$(cap_envelope_error_message "$out" "make media-down gave no reason")"
            cap_info "No local calls media server can be running: ${MEDIA_DETAIL}"
            ;;
        *)
            MEDIA_STATE="failed"
            MEDIA_DETAIL="$(cap_envelope_error_message "$out" "make media-down gave no reason")"
            cap_error "the local calls media server did not stop (exit ${rc}): ${MEDIA_DETAIL}"
            ;;
    esac
}

function remove_kubeconfig() {
    local cluster="$1" purge="$2"
    if [[ -z "$purge" ]]; then
        cap_info "kubeconfig context 'k3d-${cluster}' kept (set --purge to remove)."
        return
    fi
    if command -v kubectl &>/dev/null; then
        cap_info "Removing kubeconfig context 'k3d-${cluster}'..."
        kubectl config delete-context "k3d-${cluster}" 2>/dev/null >&2 || true
        kubectl config delete-cluster "k3d-${cluster}"  2>/dev/null >&2 || true
        kubectl config unset "users.k3d-${cluster}"     2>/dev/null >&2 || true
        cap_info "kubeconfig context removed."
    fi
}

# remove_signing_key <cluster> <purge> <keep> -- delete the cluster's token
# signing key on an operator's purge, and only then.
#
# `make up` keeps a local cluster's signing key in a file so that a recreated
# cluster is the same issuer and the public keys given to a vendor stay good
# (up.sh, ensure_signing_key; design record
# 2026-11-21-vendor-accounts-and-api-keys, A7). TWO TEARDOWNS MUST KEEP IT:
#
#   - a plain one, which says nothing about the cluster being gone for good;
#   - the clean-slate bring-up's (`make up-refresh`, bringup.sh nuke). That one
#     IS a purge, because a clean slate must not inherit a stale kubeconfig
#     context, and it is also the exact case the key is kept for: the cluster
#     is recreated a moment later. It passes --keep-signing-key. Deleting the
#     key there made every refresh a new issuer, which the first version of
#     this function did.
#
# An operator's own purge (`make down PURGE=1`) is them saying the cluster is
# gone for good, and the key is a secret that should not outlive it.
#
# One file and its own directory, by exact path: never a recursive removal.
SIGNING_KEY_REMOVED=false
function remove_signing_key() {
    local cluster="$1" purge="$2" keep="$3" key
    key="${HOME}/.memql/k3d/${cluster}/service-account.key"
    if [[ ! -e "${key}" ]]; then
        return 0
    fi
    if [[ -z "$purge" ]]; then
        cap_info "Token signing key kept at ${key}: the next 'make up' is the same issuer (set --purge to delete it)."
        return 0
    fi
    if [[ -n "$keep" ]]; then
        cap_info "Token signing key kept at ${key}: the cluster created next is the same issuer."
        return 0
    fi
    rm -f "${key}"
    rmdir "$(dirname "${key}")" 2>/dev/null || true
    SIGNING_KEY_REMOVED=true
    cap_changed
    cap_info "Token signing key deleted. A vendor that was given this cluster's public keys must be given the next cluster's."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local cluster purge keep_signing_key
    cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    # "" or "1", read with cap_bool. PURGE in the environment is the default
    # and an explicit --purge=<bool> beats it. Both used to be emptiness tests,
    # so --purge=false and PURGE=0 removed the kubeconfig context.
    if cap_bool purge "${PURGE:-false}"; then purge=1; else purge=""; fi
    if cap_bool keep-signing-key; then keep_signing_key=1; else keep_signing_key=""; fi
    cap_require cluster "$cluster"

    check_prerequisites

    stop_media_server
    local deleted=false
    if delete_cluster "$cluster"; then
        deleted=true
        cap_changed
    fi
    remove_kubeconfig "$cluster" "$purge"
    remove_signing_key "$cluster" "$purge" "$keep_signing_key"

    cap_result_set     cluster "$cluster"
    cap_result_set_raw deleted "$deleted"
    cap_result_set_raw purged  "$( [[ -n "$purge" ]] && echo true || echo false )"
    cap_result_set_raw signingKeyRemoved "$SIGNING_KEY_REMOVED"
    cap_result_set     mediaServer "$MEDIA_STATE"
    cap_result_set     mediaServerDetail "$MEDIA_DETAIL"
    if [[ "$MEDIA_STATE" == "failed" ]]; then
        cap_fail 5 "the cluster was torn down, but the local calls media server did not stop: ${MEDIA_DETAIL}. Its containers may still be running, with their configuration in ~/.memql/media; once that is fixed, stop it with 'make media-down'."
    fi
    cap_info "Done. Run 'make up' to recreate the cluster."
    cap_ok
}

main "$@"
