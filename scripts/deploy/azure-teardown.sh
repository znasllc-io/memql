#!/usr/bin/env bash
#
# scripts/deploy/azure-teardown.sh
# ================================
#
# Capability: deploy.azureTeardown -- delete the Azure substrate of ONE MemQL
# instance.
#
# Backend for the `deprovisionInstance` deployment action (memql#4469, epic
# memql#4463).
#
# THIS IS THE DESTRUCTIVE VERB, and it is the one place in the deploy pack where
# the capability-script contract's non-interactivity rule has real teeth. A
# script an automation drives cannot stop and ask, so the confirmation is a
# PARAMETER: --confirm must equal the resource group being deleted. A caller
# that cannot name the target has not decided to delete it, and a redelivered
# event carrying a stale name deletes nothing.
#
# DELETION IS ORDERED BY WHAT IS RECOVERABLE, not by the dependency graph. Azure
# will happily delete a resource group whole, and that is exactly what makes it
# dangerous here: the cluster is reproducible from source, but a key vault holds
# credentials that may exist NOWHERE ELSE, and a storage account may hold the
# only database backups. So the compute goes first and the stores go last,
# behind their own flags, and neither is included by --confirm alone.
#
# SOFT DELETE IS NOT DELETION, and the name is what matters. A deleted key vault
# RESERVES its name for 90 days; a create against that name then fails with a
# message that does not mention soft delete. Since a key vault name is globally
# unique, an operator rebuilding under the same name is blocked by their own
# previous instance. --purgeKeyVault is therefore separate from deleting it:
# purging is irreversible in a way deletion is not.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4463 memql#4469 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

#=============================================================================
# CAPABILITY SPEC
#=============================================================================

cap_init "deploy.azureTeardown" \
    "Delete the Azure substrate of one MemQL instance. Destructive; requires --confirm."

cap_spec_param_required "subscriptionId" "Azure subscription the instance lives in"
cap_spec_param_required "resourceGroup"  "resource group to delete"
cap_spec_param_required "confirm"        "must equal --resourceGroup exactly; anything else refuses"
cap_spec_param "clusterName"             "delete only this AKS cluster, leaving the resource group standing"
cap_spec_param "keyVaultName"            "key vault to delete (requires --deleteStores)"
cap_spec_param "backupStorageAccount"    "backup storage account to delete (requires --deleteStores)"
cap_spec_param "deleteStores"            "also delete the key vault and backup storage account -- these may hold the only copy of credentials and backups"
cap_spec_param "purgeKeyVault"           "after deleting the key vault, PURGE it so its globally-unique name is reusable immediately (irreversible)"
cap_spec_param "dryRun"                  "plan only; delete nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

#=============================================================================
# CONFIGURATION
#=============================================================================

SUBSCRIPTION_ID="$(cap_param subscriptionId "")"
RESOURCE_GROUP="$(cap_param resourceGroup "")"
CONFIRM="$(cap_param confirm "")"
CLUSTER_NAME="$(cap_param clusterName "")"
KEY_VAULT_NAME="$(cap_param keyVaultName "")"
BACKUP_STORAGE="$(cap_param backupStorageAccount "")"
DELETE_STORES="$(cap_param deleteStores "false")"
PURGE_KEY_VAULT="$(cap_param purgeKeyVault "false")"
DRY_RUN="$(cap_param dryRun "false")"

DELETED=""

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    command -v az &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"
    local active
    active="$(az account show --query id -o tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first"
    if [[ "$active" != "$SUBSCRIPTION_ID" ]]; then
        az account set --subscription "$SUBSCRIPTION_ID" 2>/dev/null \
            || cap_fail 3 "cannot select subscription ${SUBSCRIPTION_ID}"
    fi
}

function validate_arguments() {
    [[ -n "$SUBSCRIPTION_ID" ]] || cap_fail 2 "--subscriptionId is required"
    [[ -n "$RESOURCE_GROUP"  ]] || cap_fail 2 "--resourceGroup is required"

    # The confirmation is the whole safety interlock. Compare exactly: a
    # trimmed-or-lowercased match would accept a name the caller did not type.
    if [[ "$CONFIRM" != "$RESOURCE_GROUP" ]]; then
        cap_fail 3 "refusing to delete: --confirm must be exactly ${RESOURCE_GROUP}, got ${CONFIRM:-<empty>}"
    fi

    if [[ "$PURGE_KEY_VAULT" == "true" && "$DELETE_STORES" != "true" ]]; then
        cap_fail 2 "--purgeKeyVault requires --deleteStores -- purging a vault that was not deleted is not a thing"
    fi
    if [[ "$PURGE_KEY_VAULT" == "true" && -z "$KEY_VAULT_NAME" ]]; then
        cap_fail 2 "--purgeKeyVault requires --keyVaultName"
    fi
}

function note_deleted() {
    DELETED="${DELETED:+$DELETED,}$1"
}

# warn_about_stores names what is about to become unrecoverable, every run,
# whether or not the flag was given. An operator who did not intend it sees the
# names; one who did sees confirmation of the right names.
function warn_about_stores() {
    if [[ "$DELETE_STORES" != "true" ]]; then
        [[ -n "$KEY_VAULT_NAME" ]] && cap_info "PRESERVING key vault ${KEY_VAULT_NAME} (pass --deleteStores to delete it)"
        [[ -n "$BACKUP_STORAGE" ]] && cap_info "PRESERVING storage account ${BACKUP_STORAGE} (pass --deleteStores to delete it)"
        return 0
    fi
    cap_warn "--deleteStores given: credentials and backups below will be DESTROYED"
    [[ -n "$KEY_VAULT_NAME" ]] && cap_warn "  key vault ${KEY_VAULT_NAME} -- may hold the only copy of provider credentials"
    [[ -n "$BACKUP_STORAGE" ]] && cap_warn "  storage account ${BACKUP_STORAGE} -- may hold the only database backups"
    return 0
}

function delete_cluster_only() {
    [[ -n "$CLUSTER_NAME" ]] || return 1

    if ! az aks show --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" -o none 2>/dev/null; then
        cap_info "AKS cluster ${CLUSTER_NAME} does not exist; nothing to delete"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would delete AKS cluster ${CLUSTER_NAME} (and its managed node resource group)"
        return 0
    fi
    cap_step "deleting AKS cluster ${CLUSTER_NAME}"
    az aks delete --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" --yes -o none \
        || cap_fail 5 "failed to delete AKS cluster ${CLUSTER_NAME}"
    note_deleted "aks:${CLUSTER_NAME}"
    cap_changed
}

function delete_stores() {
    [[ "$DELETE_STORES" == "true" ]] || return 0

    if [[ -n "$BACKUP_STORAGE" ]] && az storage account show --name "$BACKUP_STORAGE" -o none 2>/dev/null; then
        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would delete storage account ${BACKUP_STORAGE}"
        else
            cap_step "deleting storage account ${BACKUP_STORAGE}"
            az storage account delete --name "$BACKUP_STORAGE" --resource-group "$RESOURCE_GROUP" --yes -o none \
                || cap_fail 5 "failed to delete storage account ${BACKUP_STORAGE}"
            note_deleted "storage:${BACKUP_STORAGE}"
            cap_changed
        fi
    fi

    if [[ -n "$KEY_VAULT_NAME" ]] && az keyvault show --name "$KEY_VAULT_NAME" -o none 2>/dev/null; then
        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would delete key vault ${KEY_VAULT_NAME}"
        else
            cap_step "deleting key vault ${KEY_VAULT_NAME}"
            az keyvault delete --name "$KEY_VAULT_NAME" --resource-group "$RESOURCE_GROUP" -o none \
                || cap_fail 5 "failed to delete key vault ${KEY_VAULT_NAME}"
            note_deleted "keyvault:${KEY_VAULT_NAME}"
            cap_changed
        fi
    fi

    if [[ "$PURGE_KEY_VAULT" == "true" ]]; then
        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would PURGE key vault ${KEY_VAULT_NAME}, freeing its globally-unique name"
        else
            cap_step "purging key vault ${KEY_VAULT_NAME} (irreversible)"
            az keyvault purge --name "$KEY_VAULT_NAME" -o none \
                || cap_fail 5 "failed to purge key vault ${KEY_VAULT_NAME}"
            note_deleted "keyvault-purged:${KEY_VAULT_NAME}"
            cap_changed
        fi
    fi
}

function delete_resource_group() {
    if [[ "$(az group exists --name "$RESOURCE_GROUP" 2>/dev/null)" != "true" ]]; then
        cap_info "resource group ${RESOURCE_GROUP} does not exist; nothing to delete"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would delete resource group ${RESOURCE_GROUP} and everything in it"
        return 0
    fi
    cap_step "deleting resource group ${RESOURCE_GROUP}"
    az group delete --name "$RESOURCE_GROUP" --yes --no-wait -o none \
        || cap_fail 5 "failed to delete resource group ${RESOURCE_GROUP}"
    note_deleted "resourceGroup:${RESOURCE_GROUP}"
    cap_changed
}

function collect_result() {
    cap_result_set "subscriptionId" "$SUBSCRIPTION_ID"
    cap_result_set "resourceGroup"  "$RESOURCE_GROUP"
    cap_result_set "deleted"        "$DELETED"
    cap_result_set "dryRun"         "$DRY_RUN"
    cap_result_set_raw "storesDeleted" "$([[ "$DELETE_STORES" == "true" ]] && echo true || echo false)"
}

function main() {
    validate_arguments
    check_prerequisites
    warn_about_stores

    # --clusterName narrows the whole operation to the cluster: it is the
    # "stop paying for compute, keep everything else" case, which is what an
    # operator actually wants first and what makes the destructive path
    # avoidable most of the time.
    if delete_cluster_only; then
        collect_result
        cap_ok
    fi

    delete_stores
    delete_resource_group

    collect_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
