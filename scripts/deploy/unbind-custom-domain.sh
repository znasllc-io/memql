#!/usr/bin/env bash
#
# scripts/deploy/unbind-custom-domain.sh
# ======================================
#
# Capability: domain.unbind -- remove the exact-host Ingress and cert-manager
# Certificate that were serving a client's own domain.
#
# The mirror of bind-custom-domain.sh (epic memql#4805, design D6). Backend for
# the custom-domain reconciler's `removing` -> `removed` step, and the
# operator's manual path for the same job.
#
# THE HOSTNAME HAS ALREADY STOPPED RESOLVING BY THE TIME THIS RUNS, and that
# ordering is deliberate rather than incidental. `removeCustomDomain` writes
# status `removing`, and the edge's own read (`liveCustomDomainByHostname`)
# filters `status=="live"` -- so serving stops at the speed of a row write. An
# operator unbinding a domain because it is being abused does not have to wait
# for kubectl. This script is the cleanup behind that decision, which is why its
# failure is loud but not urgent.
#
# THE ROW SURVIVES. Nothing here deletes a graph row and nothing should: the
# history of what this cluster served, and when, is the audit trail, and a
# deleted row answers no question anybody asks after an incident.
#
# DELETION ORDER IS LOAD-BEARING. The Certificate goes first so cert-manager
# stops renewing before the route disappears; the reverse leaves a window where
# a renewal races the deletion and can recreate the Secret behind an Ingress
# that is already gone.
#
# ABSENT IS SUCCESS. Unbinding twice is a legitimate thing for a retry to do,
# and a NotFound that failed would make every redelivered removal look like a
# broken cluster.
#
# Refs: memql#4805 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "domain.unbind" \
    "Remove the exact-host Ingress and cert-manager Certificate for a client's own domain. Idempotent: absent is success."

cap_spec_param_required "domainId"  "the v1:platform:customDomain row id -- both objects are named after it"
cap_spec_param          "hostname"  "the client's host, recorded in the result so a log line names what came down"
cap_spec_param          "namespace" "namespace the objects live in (default: memql)"
cap_spec_param          "dryRun"    "report what would be removed without removing it"

cap_handle_meta "$@"
cap_parse_flags "$@"

DOMAIN_ID="$(cap_param domainId "")"
HOSTNAME_ARG="$(cap_param hostname "")"
NAMESPACE="$(cap_param namespace "memql")"
DRY_RUN="$(cap_bool_str dryRun false)"

OBJECT_NAME=""
REMOVED=()
ABSENT=()

function check_params() {
    [[ -n "$DOMAIN_ID" ]] || cap_fail 2 "--domainId is required: both objects are named after it, so there is nothing to remove without one"
    local slug
    slug="$(printf '%s' "$DOMAIN_ID" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed 's/^-*//; s/-*$//')"
    [[ -n "$slug" ]] || cap_fail 2 "--domainId ${DOMAIN_ID} contains no usable characters for an object name"
    # Mirrors bind-custom-domain.sh and objectName() in
    # integrations/customdomain/provision.go. Three spellings of one rule is
    # two too many, and custom_domain_test.go pins all three together.
    OBJECT_NAME="custom-domain-${slug:0:200}"
    return 0
}

function check_prereqs() {
    command -v kubectl &>/dev/null \
        || cap_fail 4 "kubectl is not installed or not on PATH"
    if [[ "$DRY_RUN" == "true" ]]; then
        return 0
    fi
    kubectl cluster-info &>/dev/null \
        || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first"
    return 0
}

# remove_object deletes one object, treating absent as success.
function remove_object() {
    local kind="$1"
    if [[ "$DRY_RUN" == "true" ]]; then
        if kubectl get "${kind}/${OBJECT_NAME}" -n "$NAMESPACE" &>/dev/null; then
            cap_info "dry run: would delete ${kind}/${OBJECT_NAME} in ${NAMESPACE}"
            REMOVED+=("${kind}/${OBJECT_NAME}")
        else
            ABSENT+=("${kind}/${OBJECT_NAME}")
        fi
        return 0
    fi
    local out
    if out="$(kubectl delete "${kind}/${OBJECT_NAME}" -n "$NAMESPACE" --ignore-not-found 2>&1)"; then
        if [[ -n "$out" ]]; then
            cap_info "$out"
            REMOVED+=("${kind}/${OBJECT_NAME}")
            cap_changed
        else
            # --ignore-not-found prints nothing when there was nothing to
            # delete, which is exactly the signal wanted here.
            ABSENT+=("${kind}/${OBJECT_NAME}")
        fi
        return 0
    fi
    cap_fail 5 "could not delete ${kind}/${OBJECT_NAME} in ${NAMESPACE}: ${out}"
}

function collect_result() {
    cap_result_set "domainId"   "$DOMAIN_ID"
    cap_result_set "hostname"   "$HOSTNAME_ARG"
    cap_result_set "namespace"  "$NAMESPACE"
    cap_result_set "objectName" "$OBJECT_NAME"
    cap_result_set "removed"    "$(IFS=,; printf '%s' "${REMOVED[*]:-}")"
    cap_result_set "absent"     "$(IFS=,; printf '%s' "${ABSENT[*]:-}")"
    return 0
}

function main() {
    check_params
    check_prereqs
    # Certificate first: cert-manager stops renewing before the route goes.
    remove_object "certificate"
    remove_object "ingress"
    collect_result
    cap_ok
}

main "$@"
