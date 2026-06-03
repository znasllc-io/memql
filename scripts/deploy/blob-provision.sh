#!/usr/bin/env bash
#
# scripts/deploy/blob-provision.sh
# =================================
#
# Provision a dedicated Azure Storage account + blob container for a given
# environment (default: staging) and print the connection string so the
# operator can add it to the genesis envelope.
# See znasllc-io/memql#807 (epic #805).
#
# This script does TWO things, both safe to re-run any number of times
# (convergent -- a second consecutive run is a no-op):
#
#   1. Storage account -- create-or-verify in the per-env resource group.
#      Existence is checked before create so the account is never duplicated.
#      Staging default: stmemqlstaging  in  rg-memql-staging  (East US)
#
#   2. Blob container -- create-or-verify inside that account.
#      Default container name: attachments
#
# After both steps succeed the connection string (storage account key + URL)
# is printed to stdout. The operator copies it into ~/Downloads/staging.genesis.env
# as MEMQL_AZURE_STORAGE_CONNECTION_STRING and runs `make genesis-seal` to
# reseal the envelope (see the runbook at docs/deploy/blob-provision.md).
#
# Connection-string auth is intentional -- the agent node uploads with the
# account key; it does NOT create containers at runtime (that's this script's
# job). Least-privilege at the app level; key rotation = rerun this script
# and re-seal the envelope.
#
# A final STATE REPORT prints what already existed, what was created, and
# what changed this run.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, main() at the
# very bottom calls them in order. Supports --help and --dry-run.
#
# NOTE: requires `az login` before running. The script verifies az + login
# status and exits with a clear message when either is missing. Run with
# --dry-run to print the full plan without making any Azure calls.
#
# Decision locked in #805: SEPARATE storage account per environment.
# Staging and production MUST NOT share the same account. See the runbook.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Region. Matches the AKS cluster / Key Vault region (East US).
readonly REGION="eastus"
readonly REGION_DISPLAY="East US"

# Azure Storage limits: account names are 3-24 chars, lowercase alnum only
# (no hyphens, no underscores). SKU Standard_LRS is General Purpose v2 --
# the lowest-cost durable tier, sufficient for staging attachments.
readonly ACCOUNT_SKU="Standard_LRS"
readonly ACCOUNT_KIND="StorageV2"

# Default environment. Overridable via --env / ENV= / first positional.
DEFAULT_ENV="staging"

# Per-environment defaults. Resolved in validate_arguments once ENV is known.
# Staging defaults displayed in --help; production is a parameterized stub.
readonly DEFAULT_RG_STAGING="rg-memql-staging"
readonly DEFAULT_RG_PRODUCTION="rg-memql-production"
readonly DEFAULT_ACCOUNT_STAGING="stmemqlstaging"
readonly DEFAULT_ACCOUNT_PRODUCTION="stmemqlproduction"
readonly DEFAULT_CONTAINER_STAGING="attachments"
readonly DEFAULT_CONTAINER_PRODUCTION="attachments"

# State accumulators for the final report.
STATE_EXISTS=()
STATE_CREATED=()
STATE_CHANGED=()
STATE_SKIPPED=()

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function log()     { echo "  $*"; }
function info()    { echo "[blob-provision] $*"; }
function warn()    { echo "  WARN: $*" >&2; }
function err()     { echo "  ERROR: $*" >&2; }

function section() {
    echo ""
    echo "=== $* ==="
}

function record_exists()  { STATE_EXISTS+=("$1"); }
function record_created() { STATE_CREATED+=("$1"); }
function record_changed() { STATE_CHANGED+=("$1"); }
function record_skipped() { STATE_SKIPPED+=("$1"); }

#=============================================================================
# HELP
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--env=staging|production] [options]

Provision (create-or-verify) an Azure Storage account and blob container
for a memQL environment, then print the connection string for inclusion in
the genesis envelope. Idempotent: re-running detects the existing account
and container and only prints the current connection string.

See docs/deploy/blob-provision.md for the full operator runbook (genesis
reseal, Key Vault storage, pod roll, and verification steps).

Options:
    --env=ENV              Target environment: staging (default) or production.
                           May also be passed positionally or via ENV=.
    --resource-group=RG    Resource group (default: rg-memql-<env>).
    --account-name=NAME    Storage account name (default: stmemql<env>).
                           Must be 3-24 lowercase alphanumeric characters.
    --container=NAME       Blob container name (default: attachments).
    --region=REGION        Azure region (default: ${REGION_DISPLAY} / ${REGION}).
    --dry-run              Print the full plan; mutate nothing.
    --help                 Show this help and exit.

Resources (env=staging, defaults):
    rg-memql-staging                  Resource group
    stmemqlstaging                    Storage account  (Standard_LRS, StorageV2)
    stmemqlstaging/attachments        Blob container   (private, per-env)

Environment variables (after provisioning):
    MEMQL_AZURE_STORAGE_CONNECTION_STRING  Connection string printed by this script.
    MEMQL_AZURE_BLOB_CONTAINER             Container name (default: attachments).

Decision: separate storage account per environment (#805). NEVER add the
staging connection string to local.genesis.env -- it must live ONLY in
staging.genesis.env (or the prod equivalent). Local dev uses Azurite.

Examples:
    $0 --dry-run
    $0 --env=staging
    $0 --env=staging --account-name=stmemqlstaging --container=attachments
    ENV=staging $0 --dry-run

Prerequisites (verified, never auto-installed):
    az      Azure CLI, logged in (az login).
            Install: brew install azure-cli  (macOS/Homebrew)
                or:  https://learn.microsoft.com/en-us/cli/azure/install-azure-cli
EOF
}

#=============================================================================
# ARGUMENT PARSING + VALIDATION
#=============================================================================

function parse_arguments() {
    ENV="${ENV:-${DEFAULT_ENV}}"
    DRY_RUN=false
    # Named with _ARG suffix so we can resolve defaults after ENV is known.
    RESOURCE_GROUP_ARG=""
    ACCOUNT_NAME_ARG=""
    CONTAINER_NAME_ARG=""
    REGION_ARG="${REGION}"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)            ENV="${1#*=}" ;;
            --env)              shift; ENV="${1:-}" ;;
            --resource-group=*) RESOURCE_GROUP_ARG="${1#*=}" ;;
            --resource-group)   shift; RESOURCE_GROUP_ARG="${1:-}" ;;
            --account-name=*)   ACCOUNT_NAME_ARG="${1#*=}" ;;
            --account-name)     shift; ACCOUNT_NAME_ARG="${1:-}" ;;
            --container=*)      CONTAINER_NAME_ARG="${1#*=}" ;;
            --container)        shift; CONTAINER_NAME_ARG="${1:-}" ;;
            --region=*)         REGION_ARG="${1#*=}" ;;
            --region)           shift; REGION_ARG="${1:-}" ;;
            --dry-run)          DRY_RUN=true ;;
            --help|-h)          show_help; exit 0 ;;
            staging|production) ENV="$1" ;;
            *)
                err "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
        shift
    done
}

function validate_arguments() {
    case "${ENV}" in
        staging|production) ;;
        *)
            err "Invalid --env: '${ENV}'. Must be 'staging' or 'production'."
            exit 1
            ;;
    esac

    # Resolve per-env defaults now that ENV is validated.
    local default_rg default_account default_container
    case "${ENV}" in
        staging)
            default_rg="${DEFAULT_RG_STAGING}"
            default_account="${DEFAULT_ACCOUNT_STAGING}"
            default_container="${DEFAULT_CONTAINER_STAGING}"
            ;;
        production)
            default_rg="${DEFAULT_RG_PRODUCTION}"
            default_account="${DEFAULT_ACCOUNT_PRODUCTION}"
            default_container="${DEFAULT_CONTAINER_PRODUCTION}"
            ;;
    esac
    RESOURCE_GROUP="${RESOURCE_GROUP_ARG:-${default_rg}}"
    ACCOUNT_NAME="${ACCOUNT_NAME_ARG:-${default_account}}"
    CONTAINER_NAME="${CONTAINER_NAME_ARG:-${default_container}}"
    REGION_EFFECTIVE="${REGION_ARG}"

    # Storage account name constraints: 3-24 chars, lowercase alnum only.
    if [[ ! "${ACCOUNT_NAME}" =~ ^[a-z0-9]{3,24}$ ]]; then
        err "Invalid storage account name: '${ACCOUNT_NAME}'"
        err "  Must be 3-24 lowercase alphanumeric characters (no hyphens)."
        exit 1
    fi

    if [[ "${ENV}" == "production" ]]; then
        warn "ENV=production is a parameterized stub. Resource names and steps"
        warn "  are parameterized but production has NOT been validated against"
        warn "  a live subscription. Review carefully before a real run."
    fi
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function have() { command -v "$1" >/dev/null 2>&1; }

# run_or_plan: the single mutation gate. In --dry-run we print the command
# and return success WITHOUT executing; in live mode we execute it.
# Every state-changing az call routes through here so --dry-run is
# guaranteed side-effect-free.
function run_or_plan() {
    if [[ "${DRY_RUN}" == true ]]; then
        echo "  [plan] $*"
        return 0
    fi
    "$@"
}

# az_ready: dry-run always passes; live requires az installed + logged in.
function az_ready() {
    if [[ "${DRY_RUN}" == true ]]; then return 0; fi
    if ! have az; then return 1; fi
    az account show >/dev/null 2>&1
}

function check_prerequisites() {
    section "Prerequisites"

    if [[ "${DRY_RUN}" == true ]]; then
        log "[plan] verify: az (Azure CLI, authenticated)"
        return 0
    fi

    if ! have az; then
        err "az (Azure CLI) is not installed."
        err "  Install: brew install azure-cli   (macOS)"
        err "        or: https://learn.microsoft.com/en-us/cli/azure/install-azure-cli"
        err "  Then:    az login"
        exit 1
    fi

    if ! az account show >/dev/null 2>&1; then
        err "az is installed but not logged in."
        err "  Run: az login"
        exit 1
    fi

    local subscription
    subscription="$(az account show --query name -o tsv 2>/dev/null || echo 'unknown')"
    log "[ok] az logged in (subscription: ${subscription})"
}

#=============================================================================
# RESOURCE GROUP -- verify existence (pre-existing; created by deploy-setup)
#=============================================================================

function check_resource_group() {
    section "Resource group: ${RESOURCE_GROUP}"

    if [[ "${DRY_RUN}" == true ]]; then
        log "[plan] verify resource group ${RESOURCE_GROUP} exists"
        return 0
    fi

    if ! az_ready; then
        warn "az not ready -- skipping resource group check."
        record_skipped "resource-group: ${RESOURCE_GROUP}"
        return 0
    fi

    if az group show --name "${RESOURCE_GROUP}" >/dev/null 2>&1; then
        log "[ok] resource group ${RESOURCE_GROUP} exists"
        record_exists "resource-group: ${RESOURCE_GROUP}"
    else
        err "Resource group ${RESOURCE_GROUP} does not exist."
        err "  Run 'make deploy-setup ENV=${ENV}' first to create it, then re-run this script."
        exit 1
    fi
}

#=============================================================================
# STORAGE ACCOUNT -- create-or-verify (idempotent)
#=============================================================================

function account_exists() {
    # Returns 0 when the account is present in the resource group.
    az storage account show \
        --name "${ACCOUNT_NAME}" \
        --resource-group "${RESOURCE_GROUP}" \
        >/dev/null 2>&1
}

function ensure_storage_account() {
    section "Storage account: ${ACCOUNT_NAME} (${RESOURCE_GROUP} / ${REGION_EFFECTIVE})"

    if ! az_ready; then
        warn "az not ready -- skipping storage account step."
        record_skipped "storage-account: ${ACCOUNT_NAME}"
        return 0
    fi

    if [[ "${DRY_RUN}" == false ]] && account_exists; then
        log "[ok] storage account ${ACCOUNT_NAME} already exists"
        record_exists "storage-account: ${ACCOUNT_NAME}"
        return 0
    fi

    info "creating storage account ${ACCOUNT_NAME} ..."
    # allow-blob-public-access disabled -- attachments are private; the app
    # returns blob bytes via the /spaces/{id}/attachments download endpoint
    # rather than giving the browser a direct public URL.
    run_or_plan az storage account create \
        --name "${ACCOUNT_NAME}" \
        --resource-group "${RESOURCE_GROUP}" \
        --location "${REGION_EFFECTIVE}" \
        --sku "${ACCOUNT_SKU}" \
        --kind "${ACCOUNT_KIND}" \
        --allow-blob-public-access false \
        --min-tls-version TLS1_2 \
        --only-show-errors \
        --output none

    if [[ "${DRY_RUN}" == false ]]; then
        record_created "storage-account: ${ACCOUNT_NAME}"
    fi
}

#=============================================================================
# BLOB CONTAINER -- create-or-verify (idempotent)
#=============================================================================

function container_exists() {
    # Returns 0 when the container is present in the account.
    az storage container show \
        --name "${CONTAINER_NAME}" \
        --account-name "${ACCOUNT_NAME}" \
        --auth-mode login \
        >/dev/null 2>&1
}

function ensure_container() {
    section "Blob container: ${ACCOUNT_NAME}/${CONTAINER_NAME}"

    if ! az_ready; then
        warn "az not ready -- skipping blob container step."
        record_skipped "blob-container: ${CONTAINER_NAME}"
        return 0
    fi

    if [[ "${DRY_RUN}" == false ]] && container_exists; then
        log "[ok] blob container ${CONTAINER_NAME} already exists"
        record_exists "blob-container: ${CONTAINER_NAME}"
        return 0
    fi

    info "creating blob container ${CONTAINER_NAME} ..."
    # Private access level (no anonymous reads). The app uses connection-string
    # auth at upload time and serves downloads through its own HTTP endpoint.
    run_or_plan az storage container create \
        --name "${CONTAINER_NAME}" \
        --account-name "${ACCOUNT_NAME}" \
        --auth-mode login \
        --public-access off \
        --only-show-errors \
        --output none

    if [[ "${DRY_RUN}" == false ]]; then
        record_created "blob-container: ${CONTAINER_NAME}"
    fi
}

#=============================================================================
# CONNECTION STRING -- read + print instructions (no KV write)
#
# The blob connection string is NOT stored in Key Vault directly. It lives
# inside the sealed genesis envelope (MEMQL_AZURE_STORAGE_CONNECTION_STRING).
# The operator copies it from this script's output into
# ~/Downloads/staging.genesis.env, then re-seals with `make genesis-seal`.
# See the runbook at docs/deploy/blob-provision.md for the full flow.
#
# The connection string includes the account key -- treat the output of this
# section as SECRET. Never log it beyond what's printed here, never commit it.
#=============================================================================

function print_connection_string() {
    section "Connection string"

    if [[ "${DRY_RUN}" == true ]]; then
        echo "  [plan] az storage account show-connection-string --name ${ACCOUNT_NAME} --resource-group ${RESOURCE_GROUP} (printed to terminal only)"
        echo ""
        echo "  After provisioning, copy the printed connection string into:"
        echo "    ~/Downloads/${ENV}.genesis.env"
        echo "    MEMQL_AZURE_STORAGE_CONNECTION_STRING=<value>"
        echo "    MEMQL_AZURE_BLOB_CONTAINER=${CONTAINER_NAME}"
        echo "  Then run: make genesis-seal ENV_FILE=~/Downloads/${ENV}.genesis.env"
        echo "  (See docs/deploy/blob-provision.md for the full runbook.)"
        return 0
    fi

    if ! az_ready; then
        warn "az not ready -- cannot read connection string."
        record_skipped "connection-string: ${ACCOUNT_NAME}"
        return 0
    fi

    local conn_str
    conn_str="$(az storage account show-connection-string \
        --name "${ACCOUNT_NAME}" \
        --resource-group "${RESOURCE_GROUP}" \
        --query connectionString \
        -o tsv 2>/dev/null || echo '')"

    if [[ -z "${conn_str}" ]]; then
        warn "could not read connection string for ${ACCOUNT_NAME}."
        warn "  (az may lack 'Microsoft.Storage/storageAccounts/listKeys/action' on the resource group)"
        record_skipped "connection-string: ${ACCOUNT_NAME}"
        return 0
    fi

    echo ""
    echo "  ================================================================="
    echo "  STAGING BLOB CONNECTION STRING (treat as SECRET -- do not share)"
    echo "  ================================================================="
    echo "  ${conn_str}"
    echo "  ================================================================="
    echo ""
    echo "  NEXT STEPS (see docs/deploy/blob-provision.md for full runbook):"
    echo ""
    echo "  1. Append to ~/Downloads/${ENV}.genesis.env:"
    echo "       MEMQL_AZURE_STORAGE_CONNECTION_STRING=<paste connection string>"
    echo "       MEMQL_AZURE_BLOB_CONTAINER=${CONTAINER_NAME}"
    echo ""
    echo "  2. Reseal the genesis envelope:"
    echo "       make genesis-seal ENV_FILE=~/Downloads/${ENV}.genesis.env"
    echo ""
    echo "  3. Store the new genesis-b64 in Key Vault + k8s secret, then roll"
    echo "     agent pods. Steps in docs/deploy/blob-provision.md."
    echo ""
    record_exists "connection-string: ${ACCOUNT_NAME} (printed to terminal)"
}

#=============================================================================
# STATE REPORT
#=============================================================================

function print_state_report() {
    section "State report (env=${ENV}, account=${ACCOUNT_NAME}, container=${CONTAINER_NAME}, dry-run=${DRY_RUN})"

    local plan_note=""
    if [[ "${DRY_RUN}" == true ]]; then
        plan_note=" (planned, not executed)"
    fi

    echo "  Already correct:${plan_note}"
    if [[ "${#STATE_EXISTS[@]}" -eq 0 ]]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_EXISTS[@]}"; fi

    echo "  Created:${plan_note}"
    if [[ "${#STATE_CREATED[@]}" -eq 0 ]]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CREATED[@]}"; fi

    echo "  Changed / rotated:${plan_note}"
    if [[ "${#STATE_CHANGED[@]}" -eq 0 ]]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CHANGED[@]}"; fi

    echo "  Skipped (tool / auth missing):"
    if [[ "${#STATE_SKIPPED[@]}" -eq 0 ]]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_SKIPPED[@]}"; fi

    echo ""
    echo "  IMPORTANT (env-specificity): MEMQL_AZURE_STORAGE_CONNECTION_STRING belongs"
    echo "  in ~/Downloads/${ENV}.genesis.env ONLY -- each environment seals its own"
    echo "  connection string. Do not share it across environments or with dev."

    echo ""
    if [[ "${DRY_RUN}" == true ]]; then
        info "DRY RUN complete -- nothing was mutated. Re-run without --dry-run"
        info "against an 'az login' session to apply."
    elif [[ "${#STATE_SKIPPED[@]}" -ne 0 ]]; then
        info "Provisioning ran with skips. Resolve the items above and re-run;"
        info "re-running is safe (idempotent) and converges remaining state."
    else
        info "Provisioning complete. A second consecutive run will be a no-op."
    fi
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    parse_arguments "$@"
    validate_arguments

    info "memQL blob-provision -- env=${ENV} account=${ACCOUNT_NAME} container=${CONTAINER_NAME:-<pending>} dry-run=${DRY_RUN}"

    check_prerequisites
    check_resource_group
    ensure_storage_account
    ensure_container
    print_connection_string
    print_state_report
}

main "$@"
