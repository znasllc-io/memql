#!/usr/bin/env bash
#
# scripts/fleet/tenant-resume.sh
# ==============================
#
# Capability: fleet.tenantResume -- bring a suspended tenant back up.
#
# Backend for the `resumeTenant` fleet action (epic memql#3852, task
# memql#3853). The exact inverse of tenant-suspend.sh, and the inverse in the
# opposite ORDER: suspend disables sync and then scales down, so resume scales
# the database up and then re-enables sync, letting ArgoCD restore the mesh to
# the counts the tenant's overlay declares.
#
# WHY RE-ENABLING SYNC IS THE WHOLE OF RESUMING THE MESH. The overlay is the
# authority on how many replicas a tier gets, and a resume that scaled
# deployments by hand would have to know those numbers -- which means knowing
# the tier, which means a decision inside a capability script. Handing the job
# back to ArgoCD means a resumed tenant comes back at exactly what it is paying
# for, including a tier change that happened while it was suspended.
#
# THE DATABASE IS SCALED FIRST, and unconditionally: a mesh that comes up
# against a database with zero instances crash-loops on connect, which is a
# noisy, alarming, and entirely self-inflicted way to resume. `--dbInstances`
# names the count to restore; the caller knows it from the tier's preset.
#
# RESUME IS SAFE AGAINST A TENANT THAT WAS NEVER SUSPENDED. Re-enabling sync
# that is already enabled and scaling a database to the count it already has are
# both no-ops. It is NOT safe against a torn-down tenant, and cannot be made so
# -- the volumes are gone. That case is refused by the caller, which reads the
# instance row's status first, because `torn_down` and `suspended` are different
# states for exactly this reason.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed
#
# Refs: memql#3852 memql#3853 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "fleet.tenantResume" "Bring a suspended memQL tenant back up at the counts its overlay declares."
cap_spec_param_required "tenant" "tenant slug -- its namespace and its ArgoCD Application name"
cap_spec_param "dbInstances"     "CNPG Cluster instance count to restore (from the tier's database preset)"
cap_spec_param "argocdNamespace" "namespace the ArgoCD Application lives in"
cap_spec_param "dryRun"          "report the intended actions without performing them"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tenant db_instances argons dry
    tenant="$(cap_param tenant "")"
    db_instances="$(cap_param dbInstances "1")"
    argons="$(cap_param argocdNamespace "argocd")"
    dry="$(cap_param dryRun "true")"
    cap_require tenant "$tenant"

    if [[ ! "$db_instances" =~ ^[1-9][0-9]*$ ]]; then
        cap_fail 2 "dbInstances must be a positive integer (got '${db_instances}'); resuming to zero is a suspend"
    fi

    cap_result_set tenant "$tenant"
    cap_result_set_raw dbInstances "$db_instances"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would scale the CNPG Cluster memql-db in ${tenant} to ${db_instances} instances, then re-enable automated sync on Application ${tenant}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed on the runner"
    fi

    # The database first. See the header: a mesh that comes up against a
    # zero-instance database crash-loops on connect.
    cap_step "Scaling the CNPG Cluster memql-db in ${tenant} to ${db_instances} instances"
    kubectl patch cluster memql-db -n "$tenant" --type=merge \
        -p "{\"spec\":{\"instances\":${db_instances}}}" >&2 \
        || cap_fail 5 "failed to scale the database in namespace ${tenant}"

    cap_step "Waiting for the database to report ready"
    if ! kubectl wait --for=condition=Ready cluster/memql-db -n "$tenant" --timeout=300s >&2; then
        cap_fail 5 "the database in namespace ${tenant} did not become ready within 300s"
    fi

    # Then hand the mesh back to ArgoCD, which restores the counts the tenant's
    # overlay declares -- including a tier change made while it was down.
    cap_step "Re-enabling automated sync on Application ${tenant}"
    kubectl patch application "$tenant" -n "$argons" --type=merge \
        -p '{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}' >&2 \
        || cap_fail 5 "failed to re-enable automated sync on Application ${tenant}"

    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
