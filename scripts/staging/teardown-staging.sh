#!/usr/bin/env bash
set -euo pipefail

# Script: teardown-staging.sh
# Purpose: Shut down the memQL STAGING environment to stop Azure + Tiger
#          Cloud charges. Tiered + dry-run-first + typed-confirmation.
#
# This is the recorded "full nuke" procedure (first authored 2026-06-24).
# Rebuild path: docs/internal/ops/staging-teardown-rebuild.md
#
# WHAT BILLS IN STAGING (eastus, sub Pay-As-You-Go 19f85801-...):
#   - AKS aks-memql-staging  node pool 4x Standard_B2s (autoscale 2-5)  <- dominant
#   - 5x Standard public IPs (in the MC_ node RG, deleted with the cluster)
#   - Tiger Cloud TimescaleDB service xahn9ru4v6 (memql-staging)        <- dominant
#   - minor: storage stmemqlstaging, Log Analytics workspace, empty CAE
#   - KEPT by default (rebuild-critical, low cost): ACR acrmemql, KV kv-memql-staging
#
# Tiers (compose with flags):
#   --compute    delete the AKS cluster (nodes, IPs, LBs, disks, ArgoCD, ESO)
#   --database   deprovision the Tiger Cloud DB service (PURGES all data)
#   --ancillary  delete storage account + Log Analytics workspace + empty CAE
#   --registry   ALSO delete the ACR (DESTROYS all release images) -- off by default
#   --keyvault   ALSO purge the Key Vault (DESTROYS sealed secrets) -- off by default
#   --all        == --compute --database --ancillary  (NOT registry/keyvault)
#
# Safety: prints a plan and requires --confirm-name='<env>' (or --yes) before
# executing. Always dry-runs unless --confirm is passed. Non-interactive by
# contract (#2221): no blocking prompt.

#=============================================================================
# CONFIGURATION
#=============================================================================

SUBSCRIPTION_ID="19f85801-7000-47ef-88d0-2a4ccf4482b8"
RESOURCE_GROUP="rg-memql-staging"
AKS_CLUSTER="aks-memql-staging"
NODE_RG="MC_rg-memql-staging_aks-memql-staging_eastus"
TIGER_SERVICE_ID="xahn9ru4v6"
STORAGE_ACCOUNT="stmemqlstaging"
LOG_ANALYTICS="workspace-rgmemqlstagingm904"
CAE_NAME="cae-memql-staging"
ACR_NAME="acrmemql"
KEY_VAULT="kv-memql-staging"
ENV_NAME="staging"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [tiers] [--confirm] [--confirm-name=$ENV_NAME | --yes]

Non-interactive by contract (capability-script contract, #2221): the
destructive confirmation is an explicit param, never a blocking prompt.

Tiers:
    --compute     Delete the AKS cluster ($AKS_CLUSTER): nodes, public IPs,
                  LoadBalancers, disks, ArgoCD, ESO. Stops the dominant compute charge.
    --database    Deprovision the Tiger Cloud DB ($TIGER_SERVICE_ID). PURGES ALL DATA.
    --ancillary   Delete storage ($STORAGE_ACCOUNT) + Log Analytics + empty CAE.
    --registry    ALSO delete ACR ($ACR_NAME) -- DESTROYS all release images.
    --keyvault    ALSO delete Key Vault ($KEY_VAULT) -- DESTROYS sealed secrets.
    --all         == --compute --database --ancillary (keeps ACR + Key Vault).

Modes:
    --confirm         Actually execute (default is DRY-RUN -- prints what it would do).
    --confirm-name=N  Must equal the env name ('$ENV_NAME') to proceed under --confirm
                      (or set CONFIRM_NAME=...). Replaces the old typed prompt.
    --yes             Skip the confirmation entirely (CI/non-interactive).
    --help            Show this help.

Examples:
    $0 --all                                       # dry-run the full nuke
    $0 --all --confirm --confirm-name=$ENV_NAME    # execute it (explicit confirm)
    $0 --compute --database --confirm --yes        # CI/non-interactive
EOF
}

function parse_arguments() {
    DO_COMPUTE=false; DO_DATABASE=false; DO_ANCILLARY=false
    DO_REGISTRY=false; DO_KEYVAULT=false
    CONFIRM=false; ASSUME_YES=false; CONFIRM_NAME="${CONFIRM_NAME:-}"
    [ $# -eq 0 ] && { show_help; exit 0; }
    while [[ $# -gt 0 ]]; do
        case $1 in
            --compute)        DO_COMPUTE=true; shift ;;
            --database)       DO_DATABASE=true; shift ;;
            --ancillary)      DO_ANCILLARY=true; shift ;;
            --registry)       DO_REGISTRY=true; shift ;;
            --keyvault)       DO_KEYVAULT=true; shift ;;
            --all)            DO_COMPUTE=true; DO_DATABASE=true; DO_ANCILLARY=true; shift ;;
            --confirm)        CONFIRM=true; shift ;;
            --confirm-name=*) CONFIRM_NAME="${1#*=}"; shift ;;
            --yes)            ASSUME_YES=true; shift ;;
            --help)           show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    command -v az >/dev/null    || { echo "ERROR: az CLI not found"; exit 1; }
    if [ "$DO_DATABASE" = true ]; then
        command -v tiger >/dev/null || { echo "ERROR: tiger CLI not found"; exit 1; }
    fi
    az account set --subscription "$SUBSCRIPTION_ID"
}

function print_plan() {
    echo "========================================="
    echo "memQL STAGING teardown plan"
    echo "  subscription: $SUBSCRIPTION_ID"
    echo "  mode:         $([ "$CONFIRM" = true ] && echo EXECUTE || echo DRY-RUN)"
    echo "-----------------------------------------"
    echo "  [compute]    AKS $AKS_CLUSTER + node RG ...... $DO_COMPUTE"
    echo "  [database]   Tiger DB $TIGER_SERVICE_ID (PURGE)  $DO_DATABASE"
    echo "  [ancillary]  storage + log-analytics + CAE .. $DO_ANCILLARY"
    echo "  [registry]   ACR $ACR_NAME (DESTROY images) .. $DO_REGISTRY"
    echo "  [keyvault]   KV $KEY_VAULT (DESTROY secrets).. $DO_KEYVAULT"
    echo "========================================="
}

function confirm_or_die() {
    [ "$CONFIRM" = false ] && return 0
    [ "$ASSUME_YES" = true ] && return 0
    # Non-interactive by contract (capability-script contract, #2221): the
    # destructive confirmation is an explicit param, never a blocking prompt.
    # Pass --confirm-name='<env>' (or CONFIRM_NAME=...) matching the env name.
    echo "This DESTROYS the resources above and is hard to reverse." >&2
    if [ "$CONFIRM_NAME" != "$ENV_NAME" ]; then
        echo "ERROR: confirmation required: pass --confirm-name='$ENV_NAME' (or CONFIRM_NAME='$ENV_NAME'), or --yes. Aborting." >&2
        exit 3
    fi
}

# run CMD... -- echoes it; only executes when --confirm is set.
function run() {
    echo "+ $*"
    [ "$CONFIRM" = true ] && "$@"
    return 0
}

function teardown_compute() {
    [ "$DO_COMPUTE" = true ] || return 0
    echo "INFO: deleting AKS cluster $AKS_CLUSTER (removes node RG $NODE_RG: VMs, IPs, LBs, disks, ArgoCD, ESO)..."
    run az aks delete --name "$AKS_CLUSTER" --resource-group "$RESOURCE_GROUP" --yes
}

function teardown_database() {
    [ "$DO_DATABASE" = true ] || return 0
    echo "INFO: deprovisioning Tiger Cloud DB service $TIGER_SERVICE_ID (PURGES all data)..."
    run tiger service delete "$TIGER_SERVICE_ID" --confirm
}

function teardown_ancillary() {
    [ "$DO_ANCILLARY" = true ] || return 0
    echo "INFO: deleting storage account $STORAGE_ACCOUNT..."
    run az storage account delete --name "$STORAGE_ACCOUNT" --resource-group "$RESOURCE_GROUP" --yes
    echo "INFO: deleting Container Apps environment $CAE_NAME..."
    run az containerapp env delete --name "$CAE_NAME" --resource-group "$RESOURCE_GROUP" --yes
    echo "INFO: deleting Log Analytics workspace $LOG_ANALYTICS..."
    run az monitor log-analytics workspace delete --workspace-name "$LOG_ANALYTICS" --resource-group "$RESOURCE_GROUP" --yes --force true
}

function teardown_registry() {
    [ "$DO_REGISTRY" = true ] || return 0
    echo "WARNING: deleting ACR $ACR_NAME -- ALL release images are destroyed."
    run az acr delete --name "$ACR_NAME" --resource-group "$RESOURCE_GROUP" --yes
}

function teardown_keyvault() {
    [ "$DO_KEYVAULT" = true ] || return 0
    echo "WARNING: deleting + purging Key Vault $KEY_VAULT -- sealed secrets destroyed."
    run az keyvault delete --name "$KEY_VAULT" --resource-group "$RESOURCE_GROUP"
    run az keyvault purge --name "$KEY_VAULT"
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    print_plan
    confirm_or_die
    teardown_compute
    teardown_database
    teardown_ancillary
    teardown_registry
    teardown_keyvault
    echo ""
    if [ "$CONFIRM" = true ]; then
        echo "DONE: teardown executed. Verify with:"
        echo "  az aks list -o table ; tiger service list ; az resource list -g $RESOURCE_GROUP -o table"
    else
        echo "DRY-RUN complete. Re-run with --confirm to execute."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
