#!/usr/bin/env bash
#
# scripts/fleet/tenant-teardown.sh
# ================================
#
# Capability: fleet.tenantTeardown -- destroy a tenant, after taking a final
# backup.
#
# Backend for the `tearDownTenant` fleet action (epic memql#3852, task
# memql#3853). The only irreversible operation in the fleet, and the only one
# that requires a confirmation phrase.
#
# THE CONFIRMATION IS A PARAMETER, NOT A PROMPT. The capability-script contract
# forbids a blocking `read -p` -- a script that prompts cannot be driven by an
# automation, and one that prompts only when a tty is attached behaves
# differently depending on how it was invoked, which is the worst of both. So
# the caller passes `--confirm=teardown <tenant>` and a mismatch is exit 3
# (refused). The phrase includes the tenant name deliberately: a copy-pasted
# confirmation from the last teardown does not authorise this one.
#
# THE FINAL BACKUP RUNS FIRST, and its failure ABORTS. That ordering is the
# whole point of this script existing rather than `kubectl delete application`:
# a teardown whose backup failed and proceeded anyway is indistinguishable, an
# hour later, from a teardown that worked. `--skipBackup=true` exists for the
# case where there is nothing to back up (a tenant that never provisioned), and
# it is deliberately a separate flag rather than a fallback on failure.
#
# WHAT THE FINALIZER DOES. Deleting the Application cascades through ArgoCD's
# `resources-finalizer.argocd.argoproj.io` to the namespace's workloads and
# volumes. Without it the Application would disappear while the PVCs stayed --
# a torn-down tenant we keep paying for, invisible to both the fleet and the
# bill until somebody audits the storage account.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 operation failed
#
# Refs: memql#3852 memql#3853 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "fleet.tenantTeardown" "Destroy a MemQL tenant after taking a final backup. Irreversible."
cap_spec_param_required "tenant"  "tenant slug -- its namespace and its ArgoCD Application name"
cap_spec_param_required "confirm" "the exact phrase 'teardown <tenant>'"
cap_spec_param "skipBackup"       "skip the final backup (only for a tenant that never provisioned)"
cap_spec_param "backupTimeout"    "seconds to wait for the final backup to complete"
cap_spec_param "argocdNamespace"  "namespace the ArgoCD Application lives in"
cap_spec_param "dryRun"           "report the intended actions without performing them"

# take_final_backup -- create a one-off CNPG Backup and wait for it. The name is
# derived from the tenant rather than from a timestamp so the object is
# predictable and a retried teardown reuses it instead of littering the
# namespace it is about to delete.
function take_final_backup() {
    local tenant="$1" timeout="$2" name="final-${tenant}"
    cap_step "Taking the final backup of ${tenant}"
    kubectl apply -n "$tenant" -f - >&2 <<EOF || cap_fail 5 "failed to create the final backup for ${tenant}"
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata:
  name: ${name}
spec:
  cluster:
    name: memql-db
  method: plugin
  pluginConfiguration:
    name: barman-cloud.cloudnative-pg.io
EOF
    if ! kubectl wait --for=jsonpath='{.status.phase}'=completed \
        "backup/${name}" -n "$tenant" --timeout="${timeout}s" >&2; then
        cap_fail 5 "the final backup of ${tenant} did not complete within ${timeout}s -- teardown ABORTED, nothing was deleted"
    fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tenant confirm skip_backup timeout argons dry
    tenant="$(cap_param tenant "")"
    confirm="$(cap_param confirm "")"
    skip_backup="$(cap_param skipBackup "false")"
    timeout="$(cap_param backupTimeout "900")"
    argons="$(cap_param argocdNamespace "argocd")"
    dry="$(cap_param dryRun "true")"
    cap_require tenant "$tenant"

    # Refused (exit 3) rather than bad-param (exit 2): the parameters are
    # well-formed, the operation was declined.
    cap_confirm_or_die "$confirm" "teardown ${tenant}"

    if [[ ! "$timeout" =~ ^[1-9][0-9]*$ ]]; then
        cap_fail 2 "backupTimeout must be a positive integer number of seconds (got '${timeout}')"
    fi

    cap_result_set tenant "$tenant"
    cap_result_set_raw skipBackup "$([[ "$skip_backup" == "true" ]] && echo true || echo false)"

    if [[ "$dry" != "false" ]]; then
        [[ "$skip_backup" == "true" ]] \
            && cap_info "[dry-run] would SKIP the final backup" \
            || cap_info "[dry-run] would take a final backup of ${tenant} and wait up to ${timeout}s"
        cap_info "[dry-run] would delete Application ${tenant} in ${argons}, cascading to its namespace"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed on the runner"
    fi

    if [[ "$skip_backup" == "true" ]]; then
        cap_warn "skipping the final backup of ${tenant} -- this is only correct for a tenant that never provisioned"
    else
        take_final_backup "$tenant" "$timeout"
        cap_result_set finalBackup "final-${tenant}"
    fi

    # The Application, not the namespace. Deleting the namespace directly would
    # leave the Application behind reconciling a namespace that no longer
    # exists, which ArgoCD dutifully recreates.
    cap_step "Deleting Application ${tenant} (cascades to the namespace)"
    kubectl delete application "$tenant" -n "$argons" --wait=true --timeout=600s >&2 \
        || cap_fail 5 "failed to delete Application ${tenant}"

    cap_changed
    cap_result_set_raw dryRun false
    cap_result_set_raw tornDown true
    cap_ok
}

main "$@"
