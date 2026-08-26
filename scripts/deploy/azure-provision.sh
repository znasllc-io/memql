#!/usr/bin/env bash
#
# scripts/deploy/azure-provision.sh
# =================================
#
# Capability: deploy.azureProvision -- create (or converge) the Azure substrate
# ONE MemQL instance runs on: resource group, container registry, key vault,
# backup storage, AKS cluster, workload identities and their federated
# credentials.
#
# Backend for the `provisionAzureInfrastructure` deployment action (memql#4464,
# epic memql#4463).
#
# WHY THIS EXISTS. The first cloud instance was created by hand. Nothing in the
# tree could recreate it, so when its ArgoCD Application pinned a git revision
# that later vanished, the installation became unreproducible from source --
# the cluster WAS the source of truth. This script is the answer to "how do we
# get that cluster back", and it must stay runnable by a human and by an
# automation without behaving differently (capability-script contract, #2221).
#
# WHAT THIS SCRIPT KNOWS, AND WHAT IT DELIBERATELY DOES NOT. It knows how to
# turn (names, sizes, counts, location) into Azure resources. It does NOT know
# which environment it is building, what a "prod" cluster looks like, or which
# version to deploy -- those are decisions, and decisions live in the caller
# (CLAUDE.md forbids environment branching in engine code, and this script is
# held to the same rule). It provisions the SUBSTRATE only and deploys nothing:
# workloads arrive when ArgoCD reconciles the instance overlay.
#
# IDEMPOTENT BY CONSTRUCTION. Every step is an existence check followed by a
# create, so a re-run against converged infrastructure makes no calls that
# change anything and reports `changed: false`. That matters because the caller
# is an automation with at-least-once delivery: a redelivered provisioning event
# must not create a second cluster, and here it cannot create anything at all.
#
# ORDERING IS LOAD-BEARING, in one place. A federated identity credential names
# the AKS cluster's OIDC ISSUER URL, and that URL does not exist until the
# cluster does. So the cluster is created, its issuer is read back, and only
# then are the federated credentials written. Reordering these silently
# produces identities that authenticate to nothing -- which presents much later
# as pods that cannot read secrets.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4463 memql#4464 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

#=============================================================================
# CAPABILITY SPEC
#=============================================================================

cap_init "deploy.azureProvision" \
    "Create or converge the Azure substrate one MemQL instance runs on."

cap_spec_param_required "subscriptionId"  "Azure subscription the instance is billed to and lives in"
cap_spec_param_required "resourceGroup"   "resource group holding the cluster and its identities"
cap_spec_param_required "clusterName"     "AKS cluster name"
cap_spec_param_required "registryName"    "container registry name (GLOBALLY unique -- becomes <name>.azurecr.io)"
cap_spec_param_required "keyVaultName"    "key vault name (GLOBALLY unique, and soft-delete reserves it for 90 days after deletion)"
cap_spec_param "registryResourceGroup"    "resource group for registry + key vault (defaults to resourceGroup)"
cap_spec_param "backupStorageAccount"     "storage account for database backups (GLOBALLY unique; omit to skip)"
cap_spec_param "backupResourceGroup"      "resource group for the backup storage account (defaults to resourceGroup)"
cap_spec_param "location"                 "Azure region (default eastus)"
cap_spec_param "kubernetesVersion"        "AKS Kubernetes version (default: the region's default)"
cap_spec_param "zones"                    "availability zone(s) for both node pools, space-separated (default 1). EMPTY means non-zonal, which Premium SSD v2 cannot attach to -- see the note in ensure_cluster"
cap_spec_param "meshNodeCount"            "node count for the mesh (system) pool (default 2)"
cap_spec_param "meshNodeSize"             "VM size for the mesh pool (default Standard_D2as_v4)"
cap_spec_param "dbNodeCount"              "node count for the database (user) pool (default 1)"
cap_spec_param "dbNodeSize"               "VM size for the database pool (default Standard_D2as_v4)"
cap_spec_param "esoIdentityName"          "user-assigned identity External Secrets authenticates as (default id-eso-memql)"
cap_spec_param "dbIdentityName"           "user-assigned identity the database pod authenticates as (default id-memql-db)"
cap_spec_param "namespace"                "Kubernetes namespace the federated subjects are scoped to (default memql)"
cap_spec_param "dryRun"                   "plan only; make no Azure calls that change anything"

cap_handle_meta "$@"
cap_parse_flags "$@"

#=============================================================================
# CONFIGURATION
#=============================================================================

SUBSCRIPTION_ID="$(cap_param subscriptionId "")"
RESOURCE_GROUP="$(cap_param resourceGroup "")"
CLUSTER_NAME="$(cap_param clusterName "")"
REGISTRY_NAME="$(cap_param registryName "")"
KEY_VAULT_NAME="$(cap_param keyVaultName "")"
REGISTRY_RG="$(cap_param registryResourceGroup "")"
BACKUP_STORAGE="$(cap_param backupStorageAccount "")"
BACKUP_RG="$(cap_param backupResourceGroup "")"
LOCATION="$(cap_param location "eastus")"
K8S_VERSION="$(cap_param kubernetesVersion "")"
# Default ZONAL, because the overlay this script exists to serve
# (deploy/k8s/overlays/cloud-entry) pins storageClass managed-csi-premium-v2,
# and Premium SSD v2 attaches ONLY to a VM in an availability zone. A non-zonal
# default means the script's own substrate cannot run the overlay it is for.
ZONES="$(cap_param zones "1")"
MESH_NODE_COUNT="$(cap_param meshNodeCount "2")"
MESH_NODE_SIZE="$(cap_param meshNodeSize "Standard_D2as_v4")"
DB_NODE_COUNT="$(cap_param dbNodeCount "1")"
DB_NODE_SIZE="$(cap_param dbNodeSize "Standard_D2as_v4")"
ESO_IDENTITY="$(cap_param esoIdentityName "id-eso-memql")"
DB_IDENTITY="$(cap_param dbIdentityName "id-memql-db")"
NAMESPACE="$(cap_param namespace "memql")"
DRY_RUN="$(cap_bool_str dryRun false)"

: "${REGISTRY_RG:=$RESOURCE_GROUP}"
: "${BACKUP_RG:=$RESOURCE_GROUP}"

# The database pool carries this label/taint pair so the CNPG Cluster's
# nodeSelector + toleration (deploy/k8s/base) land the database on its own node
# rather than competing with the mesh for the same CPU.
readonly DB_POOL_NAME="db"
readonly MESH_POOL_NAME="mesh"
readonly DB_NODE_LABEL="memql.io/node-pool=database"
readonly DB_NODE_TAINT="memql.io/dedicated=database:NoSchedule"

OIDC_ISSUER=""

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    command -v az &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"
    command -v jq &>/dev/null || cap_fail 4 "jq is not installed or not on PATH"

    local active
    active="$(az account show --query id -o tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first"

    if [[ "$active" != "$SUBSCRIPTION_ID" ]]; then
        cap_info "active subscription ${active} is not the target; selecting ${SUBSCRIPTION_ID}"
        az account set --subscription "$SUBSCRIPTION_ID" 2>/dev/null \
            || cap_fail 3 "cannot select subscription ${SUBSCRIPTION_ID} -- the signed-in identity may not have access to it"
    fi
}

# preflight_vm_sizes resolves each requested VM size against what the REGION
# actually offers, before anything is created.
#
# WHY THIS IS NOT OPTIONAL. --dryRun cannot catch an unavailable SKU: it reports
# "would create AKS cluster" and the real failure arrives about four minutes
# later, AFTER the resource group, registry, key vault and storage account are
# already real. So a plan-only run gave a false all-clear and the operator was
# left half-provisioned. Newer subscriptions frequently carry v5/v7 and ARM
# families while offering no v4 at all, which is exactly the case that hit.
#
# Checked: the size exists in this region, it carries no restrictions for this
# subscription, and it is x64 -- an ARM SKU schedules fine and then fails to run
# amd64 engine images, which surfaces as ImagePullBackOff naming a manifest
# rather than an architecture.
function preflight_vm_sizes() {
    local size seen=""
    for size in "$MESH_NODE_SIZE" "$DB_NODE_SIZE"; do
        case " $seen " in *" $size "*) continue ;; esac
        seen="$seen $size"

        local json
        json="$(az vm list-skus --location "$LOCATION" --size "$size" --resource-type virtualMachines -o json 2>/dev/null || true)"

        if [[ -z "$json" || "$json" == "[]" ]]; then
            cap_fail 3 "VM size ${size} is not offered in ${LOCATION} -- newer subscriptions often carry v5/v7 and ARM families with no v4 at all; run 'az vm list-skus --location ${LOCATION} --resource-type virtualMachines -o table' to see what is available"
        fi

        local restriction arch
        restriction="$(printf '%s' "$json" | jq -r '[.[] | select(.name=="'"$size"'")] | .[0].restrictions | length' 2>/dev/null || echo 0)"
        if [[ "${restriction:-0}" != "0" ]]; then
            local reason
            reason="$(printf '%s' "$json" | jq -r '[.[] | select(.name=="'"$size"'")] | .[0].restrictions[0].reasonCode // "unknown"' 2>/dev/null || echo unknown)"
            cap_fail 3 "VM size ${size} is restricted in ${LOCATION} for this subscription (${reason}) -- it cannot be created here even though it is listed"
        fi

        arch="$(printf '%s' "$json" | jq -r '[.[] | select(.name=="'"$size"'")] | .[0].capabilities[]? | select(.name=="CpuArchitectureType") | .value' 2>/dev/null | head -1)"
        if [[ -n "$arch" && "$arch" != "x64" ]]; then
            cap_fail 3 "VM size ${size} is ${arch}, not x64 -- the engine images are amd64, and an ARM node schedules pods that then fail to start with an error naming a manifest rather than an architecture"
        fi

        cap_info "VM size ${size} is available in ${LOCATION} (${arch:-x64}, unrestricted)"
    done
}

function validate_arguments() {
    [[ -n "$SUBSCRIPTION_ID" ]] || cap_fail 2 "--subscriptionId is required"
    [[ -n "$RESOURCE_GROUP"  ]] || cap_fail 2 "--resourceGroup is required"
    [[ -n "$CLUSTER_NAME"    ]] || cap_fail 2 "--clusterName is required"
    [[ -n "$REGISTRY_NAME"   ]] || cap_fail 2 "--registryName is required"
    [[ -n "$KEY_VAULT_NAME"  ]] || cap_fail 2 "--keyVaultName is required"

    # A registry name is alphanumeric only, 5-50 chars: Azure rejects anything
    # else, and it rejects it AFTER the resource group already exists.
    [[ "$REGISTRY_NAME" =~ ^[a-zA-Z0-9]{5,50}$ ]] \
        || cap_fail 2 "registryName ${REGISTRY_NAME} is invalid -- 5-50 alphanumeric characters, no hyphens"

    # Key vault: 3-24 alphanumeric-or-hyphen, must start with a letter.
    [[ "$KEY_VAULT_NAME" =~ ^[a-zA-Z][a-zA-Z0-9-]{2,23}$ ]] \
        || cap_fail 2 "keyVaultName ${KEY_VAULT_NAME} is invalid -- 3-24 chars, alphanumeric or hyphen, starting with a letter"

    if [[ -n "$BACKUP_STORAGE" ]]; then
        [[ "$BACKUP_STORAGE" =~ ^[a-z0-9]{3,24}$ ]] \
            || cap_fail 2 "backupStorageAccount ${BACKUP_STORAGE} is invalid -- 3-24 lowercase alphanumeric characters"
    fi

    [[ "$MESH_NODE_COUNT" =~ ^[0-9]+$ ]] || cap_fail 2 "meshNodeCount must be an integer, got ${MESH_NODE_COUNT}"
    [[ "$DB_NODE_COUNT"   =~ ^[0-9]+$ ]] || cap_fail 2 "dbNodeCount must be an integer, got ${DB_NODE_COUNT}"
}

# would_change <human description> -- true when the step should be skipped
# because this is a plan-only run. Logs the intent either way, so a dry run
# reads as the plan it is.
function would_change() {
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would $1"
        return 0
    fi
    cap_step "$1"
    return 1
}

function ensure_resource_group() {
    local rg="$1"
    if [[ "$(az group exists --name "$rg" 2>/dev/null)" == "true" ]]; then
        cap_info "resource group ${rg} already exists"
        return 0
    fi
    would_change "create resource group ${rg} in ${LOCATION}" && return 0
    az group create --name "$rg" --location "$LOCATION" -o none \
        || cap_fail 5 "failed to create resource group ${rg}"
    cap_changed
}

function ensure_registry() {
    if az acr show --name "$REGISTRY_NAME" -o none 2>/dev/null; then
        cap_info "container registry ${REGISTRY_NAME} already exists"
        return 0
    fi
    would_change "create container registry ${REGISTRY_NAME} (Basic) in ${REGISTRY_RG}" && return 0
    # Basic is deliberate: it includes 10 GiB and costs ~$5/month. Premium buys
    # geo-replication and private link, neither of which a single-region
    # instance uses.
    az acr create --name "$REGISTRY_NAME" --resource-group "$REGISTRY_RG" \
        --sku Basic --location "$LOCATION" -o none \
        || cap_fail 5 "failed to create container registry ${REGISTRY_NAME} -- the name is GLOBALLY unique and may be taken"
    cap_changed
}

function ensure_key_vault() {
    if az keyvault show --name "$KEY_VAULT_NAME" -o none 2>/dev/null; then
        cap_info "key vault ${KEY_VAULT_NAME} already exists"
        return 0
    fi

    # A soft-deleted vault holds its name for 90 days and a create against that
    # name fails with a message that does not mention soft delete. Say so here.
    local deleted
    deleted="$(az keyvault list-deleted --query "[?name=='${KEY_VAULT_NAME}'].name" -o tsv 2>/dev/null || true)"
    if [[ -n "$deleted" ]]; then
        cap_fail 3 "key vault ${KEY_VAULT_NAME} is SOFT-DELETED, which reserves the name -- recover it with 'az keyvault recover --name ${KEY_VAULT_NAME}', or free the name with 'az keyvault purge --name ${KEY_VAULT_NAME}'"
    fi

    would_change "create key vault ${KEY_VAULT_NAME} in ${REGISTRY_RG}" && return 0
    # RBAC authorization rather than access policies: it is the model External
    # Secrets' workload identity binds to below, and the one Azure recommends.
    az keyvault create --name "$KEY_VAULT_NAME" --resource-group "$REGISTRY_RG" \
        --location "$LOCATION" --enable-rbac-authorization true -o none \
        || cap_fail 5 "failed to create key vault ${KEY_VAULT_NAME} -- the name is GLOBALLY unique and may be taken"
    cap_changed
}

function ensure_backup_storage() {
    [[ -n "$BACKUP_STORAGE" ]] || { cap_info "no backupStorageAccount given; skipping backup storage"; return 0; }

    if az storage account show --name "$BACKUP_STORAGE" -o none 2>/dev/null; then
        cap_info "storage account ${BACKUP_STORAGE} already exists"
    else
        would_change "create storage account ${BACKUP_STORAGE} (Standard_ZRS) in ${BACKUP_RG}" && return 0
        az storage account create --name "$BACKUP_STORAGE" --resource-group "$BACKUP_RG" \
            --location "$LOCATION" --sku Standard_ZRS --kind StorageV2 \
            --min-tls-version TLS1_2 --allow-blob-public-access false -o none \
            || cap_fail 5 "failed to create storage account ${BACKUP_STORAGE} -- the name is GLOBALLY unique and may be taken"
        cap_changed
    fi
}

function ensure_cluster() {
    if az aks show --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" -o none 2>/dev/null; then
        cap_info "AKS cluster ${CLUSTER_NAME} already exists"
        return 0
    fi
    would_change "create AKS cluster ${CLUSTER_NAME} (${MESH_NODE_COUNT} x ${MESH_NODE_SIZE} mesh)" && return 0

    local -a args=(
        --name "$CLUSTER_NAME"
        --resource-group "$RESOURCE_GROUP"
        --location "$LOCATION"
        --nodepool-name "$MESH_POOL_NAME"
        --node-count "$MESH_NODE_COUNT"
        --node-vm-size "$MESH_NODE_SIZE"
        --node-osdisk-size 32
        # Workload identity + OIDC issuer are what every federated credential
        # below binds to. Enabling them after the fact is possible but rotates
        # the issuer URL, which invalidates credentials already written.
        --enable-oidc-issuer
        --enable-workload-identity
        --enable-managed-identity
        # Free tier: the paid tier buys a financially-backed uptime SLA for the
        # control plane and costs ~$73/month. A single-instance install does not
        # buy an SLA it cannot honour in its own data path.
        --tier free
        --generate-ssh-keys
        -o none
    )
    [[ -n "$K8S_VERSION" ]] && args+=(--kubernetes-version "$K8S_VERSION")
    # shellcheck disable=SC2206
    [[ -n "$ZONES" ]] && args+=(--zones $ZONES)

    az aks create "${args[@]}" \
        || cap_fail 5 "failed to create AKS cluster ${CLUSTER_NAME}"
    cap_changed
}

function ensure_database_pool() {
    local existing
    existing="$(az aks nodepool show --cluster-name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --name "$DB_POOL_NAME" --query name -o tsv 2>/dev/null || true)"
    if [[ -n "$existing" ]]; then
        cap_info "database node pool ${DB_POOL_NAME} already exists"
        return 0
    fi
    would_change "add database node pool ${DB_POOL_NAME} (${DB_NODE_COUNT} x ${DB_NODE_SIZE}), labelled and tainted" && return 0

    az aks nodepool add --cluster-name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --name "$DB_POOL_NAME" --mode User \
        --node-count "$DB_NODE_COUNT" --node-vm-size "$DB_NODE_SIZE" --node-osdisk-size 32 \
        --labels "$DB_NODE_LABEL" --node-taints "$DB_NODE_TAINT" \
        ${ZONES:+--zones $ZONES} -o none \
        || cap_fail 5 "failed to add database node pool ${DB_POOL_NAME}"
    cap_changed
}

function read_oidc_issuer() {
    if [[ "$DRY_RUN" == "true" ]]; then
        OIDC_ISSUER="https://DRY-RUN-ISSUER-NOT-READ/"
        return 0
    fi
    OIDC_ISSUER="$(az aks show --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --query oidcIssuerProfile.issuerUrl -o tsv 2>/dev/null || true)"
    [[ -n "$OIDC_ISSUER" ]] \
        || cap_fail 5 "cluster ${CLUSTER_NAME} reports no OIDC issuer URL -- federated credentials cannot be written without it"
    cap_info "OIDC issuer: ${OIDC_ISSUER}"
}

function ensure_identity() {
    local name="$1" rg="$2"
    if az identity show --name "$name" --resource-group "$rg" -o none 2>/dev/null; then
        cap_info "managed identity ${name} already exists"
        return 0
    fi
    would_change "create user-assigned identity ${name} in ${rg}" && return 0
    az identity create --name "$name" --resource-group "$rg" --location "$LOCATION" -o none \
        || cap_fail 5 "failed to create managed identity ${name}"
    cap_changed
}

# ensure_federated_credential <identity> <rg> <credential-name> <service-account>
# Binds a Kubernetes service account to a managed identity through the
# cluster's OIDC issuer. The subject format is fixed by Azure.
function ensure_federated_credential() {
    local identity="$1" rg="$2" cred="$3" sa="$4"
    local subject="system:serviceaccount:${NAMESPACE}:${sa}"

    local existing
    existing="$(az identity federated-credential list --identity-name "$identity" --resource-group "$rg" \
        --query "[?name=='${cred}'].subject" -o tsv 2>/dev/null || true)"
    if [[ "$existing" == "$subject" ]]; then
        cap_info "federated credential ${cred} already binds ${sa}"
        return 0
    fi
    if [[ -n "$existing" ]]; then
        cap_fail 3 "federated credential ${cred} exists but binds ${existing}, not ${subject} -- refusing to silently repoint an identity; delete it first if the change is intended"
    fi

    would_change "federate ${identity} to service account ${sa}" && return 0
    az identity federated-credential create --name "$cred" \
        --identity-name "$identity" --resource-group "$rg" \
        --issuer "$OIDC_ISSUER" --subject "$subject" \
        --audiences "api://AzureADTokenExchange" -o none \
        || cap_fail 5 "failed to create federated credential ${cred}"
    cap_changed
}

# ensure_role_assignment <assignee-object-id> <role> <scope>
function ensure_role_assignment() {
    local assignee="$1" role="$2" scope="$3"
    [[ -n "$assignee" ]] || return 0

    local existing
    existing="$(az role assignment list --assignee "$assignee" --scope "$scope" \
        --query "[?roleDefinitionName=='${role}'].id" -o tsv 2>/dev/null || true)"
    if [[ -n "$existing" ]]; then
        cap_info "role ${role} already assigned at ${scope##*/}"
        return 0
    fi
    would_change "assign ${role} to ${assignee} at ${scope##*/}" && return 0
    az role assignment create --assignee-object-id "$assignee" --assignee-principal-type ServicePrincipal \
        --role "$role" --scope "$scope" -o none \
        || cap_fail 5 "failed to assign role ${role} at ${scope}"
    cap_changed
}

function wire_registry_pull() {
    [[ "$DRY_RUN" == "true" ]] && { cap_info "DRY RUN: would grant the kubelet identity AcrPull on ${REGISTRY_NAME}"; return 0; }

    local kubelet_id acr_id
    kubelet_id="$(az aks show --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --query identityProfile.kubeletidentity.objectId -o tsv 2>/dev/null || true)"
    acr_id="$(az acr show --name "$REGISTRY_NAME" --query id -o tsv 2>/dev/null || true)"
    [[ -n "$kubelet_id" && -n "$acr_id" ]] \
        || cap_fail 5 "cannot resolve the kubelet identity or registry id to grant AcrPull"
    ensure_role_assignment "$kubelet_id" "AcrPull" "$acr_id"
}

function wire_key_vault_access() {
    [[ "$DRY_RUN" == "true" ]] && { cap_info "DRY RUN: would grant the External Secrets identity Key Vault Secrets User"; return 0; }

    local eso_principal kv_id
    eso_principal="$(az identity show --name "$ESO_IDENTITY" --resource-group "$REGISTRY_RG" \
        --query principalId -o tsv 2>/dev/null || true)"
    kv_id="$(az keyvault show --name "$KEY_VAULT_NAME" --query id -o tsv 2>/dev/null || true)"
    [[ -n "$eso_principal" && -n "$kv_id" ]] \
        || cap_fail 5 "cannot resolve the External Secrets identity or key vault id"
    ensure_role_assignment "$eso_principal" "Key Vault Secrets User" "$kv_id"
}

function wire_backup_access() {
    [[ -n "$BACKUP_STORAGE" ]] || return 0
    [[ "$DRY_RUN" == "true" ]] && { cap_info "DRY RUN: would grant the database identity Storage Blob Data Contributor"; return 0; }

    local db_principal sa_id
    db_principal="$(az identity show --name "$DB_IDENTITY" --resource-group "$RESOURCE_GROUP" \
        --query principalId -o tsv 2>/dev/null || true)"
    sa_id="$(az storage account show --name "$BACKUP_STORAGE" --query id -o tsv 2>/dev/null || true)"
    [[ -n "$db_principal" && -n "$sa_id" ]] \
        || cap_fail 5 "cannot resolve the database identity or backup storage account id"
    ensure_role_assignment "$db_principal" "Storage Blob Data Contributor" "$sa_id"
}

function collect_result() {
    cap_result_set "subscriptionId"  "$SUBSCRIPTION_ID"
    cap_result_set "resourceGroup"   "$RESOURCE_GROUP"
    cap_result_set "location"        "$LOCATION"
    cap_result_set "clusterName"     "$CLUSTER_NAME"
    cap_result_set "registry"        "${REGISTRY_NAME}.azurecr.io"
    cap_result_set "keyVault"        "$KEY_VAULT_NAME"
    cap_result_set "oidcIssuer"      "$OIDC_ISSUER"
    cap_result_set "zones"           "$ZONES"
    cap_result_set "dryRun"          "$DRY_RUN"

    [[ -n "$BACKUP_STORAGE" ]] && cap_result_set "backupStorageAccount" "$BACKUP_STORAGE"

    if [[ "$DRY_RUN" != "true" ]]; then
        local eso_client db_client
        eso_client="$(az identity show --name "$ESO_IDENTITY" --resource-group "$REGISTRY_RG" \
            --query clientId -o tsv 2>/dev/null || true)"
        db_client="$(az identity show --name "$DB_IDENTITY" --resource-group "$RESOURCE_GROUP" \
            --query clientId -o tsv 2>/dev/null || true)"
        # These two client ids are what the instance overlay binds its service
        # accounts to. Reporting them is the whole handoff to the overlay step.
        [[ -n "$eso_client" ]] && cap_result_set "esoClientId" "$eso_client"
        [[ -n "$db_client"  ]] && cap_result_set "dbClientId"  "$db_client"
    fi
    # UNCONDITIONAL, and not decoration. A function whose LAST statement is a
    # `[[ ... ]] && cmd` returns 1 when the test is false, and under `set -e`
    # that aborts the caller BEFORE cap_ok is ever reached -- so the envelope
    # reads "aborted (exit 1) without an explicit result" on a run that did
    # everything right, with changed:true beside it. Measured on both scripts
    # in this directory (memql#4490).
    return 0
}

function main() {
    validate_arguments
    check_prerequisites
    # After check_prerequisites: this needs an authenticated az. It runs on a
    # dry run too -- catching an unavailable SKU is most of what makes --dryRun
    # worth running at all.
    preflight_vm_sizes

    cap_info "provisioning MemQL substrate in ${SUBSCRIPTION_ID} (${LOCATION})"
    [[ "$DRY_RUN" == "true" ]] && cap_info "DRY RUN -- no Azure resource will be created or modified"

    if [[ -z "$ZONES" ]]; then
        cap_warn "--zones is empty, so both node pools will be NON-ZONAL. Premium SSD v2 (managed-csi-premium-v2, which deploy/k8s/overlays/cloud-entry pins) attaches only to a VM in an availability zone, so a database pod will stay Pending with an attach error naming the disk type. A PVC-bind probe does NOT detect this -- the PV provisions fine and only the attach fails."
    fi

    ensure_resource_group "$RESOURCE_GROUP"
    [[ "$REGISTRY_RG" != "$RESOURCE_GROUP" ]] && ensure_resource_group "$REGISTRY_RG"
    [[ "$BACKUP_RG"   != "$RESOURCE_GROUP" && -n "$BACKUP_STORAGE" ]] && ensure_resource_group "$BACKUP_RG"

    ensure_registry
    ensure_key_vault
    ensure_backup_storage

    ensure_cluster
    ensure_database_pool

    # Ordering: the issuer must be read from the LIVE cluster before any
    # federated credential can name it. See the header note.
    read_oidc_issuer

    ensure_identity "$ESO_IDENTITY" "$REGISTRY_RG"
    ensure_identity "$DB_IDENTITY"  "$RESOURCE_GROUP"

    ensure_federated_credential "$ESO_IDENTITY" "$REGISTRY_RG"  "eso-memql"  "external-secrets-kv"
    ensure_federated_credential "$DB_IDENTITY"  "$RESOURCE_GROUP" "fc-memql-db" "memql-db"

    wire_registry_pull
    wire_key_vault_access
    wire_backup_access

    collect_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
