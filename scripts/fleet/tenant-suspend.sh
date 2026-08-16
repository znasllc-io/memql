#!/usr/bin/env bash
#
# scripts/fleet/tenant-suspend.sh
# ===============================
#
# Capability: fleet.tenantSuspend -- scale a tenant to zero, leaving its data
# entirely intact.
#
# Backend for the `suspendTenant` fleet action (epic memql#3852, task
# memql#3853). Reached three ways, all of which mean the same thing to this
# script: a trial that expired, a subscription whose grace period ran out, and
# an instance that has been idle long enough to hibernate (task memql#3856).
#
# SUSPEND IS NOT TEARDOWN, and the distinction is the whole reason this script
# exists separately. Everything that holds state survives: the PersistentVolume
# Claims, the CNPG Cluster's volumes, the object-store backups, the namespace,
# the Application. What stops is compute. A suspended tenant costs storage, and
# a resume is a scale-up rather than a restore.
#
# THE ORDER OF THE TWO STEPS IS LOAD-BEARING. Automated sync is turned OFF
# FIRST, then the workloads are scaled. Doing it the other way round against a
# selfHeal Application is a suspend that silently comes back up on the next
# reconcile -- and it comes back up looking exactly like a successful suspend
# until somebody reads a bill.
#
# THE DATABASE STAYS UP by default. Scaling the CNPG Cluster to zero saves more
# -- the database is the larger half of an idle tenant's cost -- but it adds
# tens of seconds of resume latency and takes the tenant's backups offline while
# it is down. `--includeDatabase=true` opts into the cheaper, slower shape; task
# memql#3856 measures which one hibernation should default to.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed
#
# Refs: memql#3852 memql#3853 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "fleet.tenantSuspend" "Scale a memQL tenant to zero, leaving its data intact."
cap_spec_param_required "tenant" "tenant slug -- its namespace and its ArgoCD Application name"
cap_spec_param "includeDatabase" "also scale the CNPG Cluster to zero (cheaper, slower to resume)"
cap_spec_param "argocdNamespace" "namespace the ArgoCD Application lives in"
cap_spec_param "dryRun"          "report the intended actions without performing them"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tenant include_db argons dry
    tenant="$(cap_param tenant "")"
    include_db="$(cap_param includeDatabase "false")"
    argons="$(cap_param argocdNamespace "argocd")"
    dry="$(cap_param dryRun "true")"
    cap_require tenant "$tenant"

    cap_result_set tenant "$tenant"
    cap_result_set_raw includeDatabase "$([[ "$include_db" == "true" ]] && echo true || echo false)"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would disable automated sync on Application ${tenant}, then scale namespace ${tenant} to zero"
        [[ "$include_db" == "true" ]] && cap_info "[dry-run] would also scale the CNPG Cluster memql-db to zero instances"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed on the runner"
    fi

    # Step 1: stop ArgoCD healing the scale-down back up. `--type=merge` with an
    # explicit null is how a field is REMOVED from an Application spec; setting
    # `automated: {}` would leave prune and selfHeal at their defaults, which is
    # not the same thing and reads as if it were.
    cap_step "Disabling automated sync on Application ${tenant}"
    kubectl patch application "$tenant" -n "$argons" --type=merge \
        -p '{"spec":{"syncPolicy":{"automated":null}}}' >&2 \
        || cap_fail 5 "failed to disable automated sync on Application ${tenant}"

    # Step 2: scale the workloads. `--all` rather than an enumerated list,
    # because the set of Deployments in a tenant namespace is a property of the
    # tier it was rendered at -- a list here would go stale the first time a
    # profile gained a node, and the failure would be one pod left running,
    # billed, and invisible.
    cap_step "Scaling every Deployment in namespace ${tenant} to zero"
    kubectl scale deployment --all -n "$tenant" --replicas=0 >&2 \
        || cap_fail 5 "failed to scale the workloads in namespace ${tenant}"

    if [[ "$include_db" == "true" ]]; then
        cap_step "Scaling the CNPG Cluster memql-db to zero instances"
        kubectl patch cluster memql-db -n "$tenant" --type=merge \
            -p '{"spec":{"instances":0}}' >&2 \
            || cap_fail 5 "failed to scale the database in namespace ${tenant}"
    fi

    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
