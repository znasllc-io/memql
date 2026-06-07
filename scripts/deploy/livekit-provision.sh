#!/usr/bin/env bash
#
# scripts/deploy/livekit-provision.sh
# ===================================
#
# Provision the self-hosted LiveKit shared-secret material in Azure Key Vault
# for a given environment (default: staging). See znasllc-io/memql#1043.
#
# LiveKit is self-hosted (open-source livekit-server, Apache-2.0) -- there is
# NO LiveKit Cloud and NO third-party credential. The "API key/secret" is a
# self-chosen shared-secret pair: the livekit-server validates tokens signed
# with it, and the BFF mints room tokens with the same pair. This script is the
# durable, repeatable home for seeding that pair (replacing the one-off
# `az keyvault secret set` an operator would otherwise run by hand).
#
# It seeds THREE Key Vault secrets, which the committed ExternalSecret
# (deploy/k8s/base/externalsecret-livekit.yaml) reconciles into the k8s Secret
# `livekit-secrets`:
#
#   livekit-keys                 = "<apiKey>: <secret>"   (livekit-server's keys)
#   polyphon-livekit-api-key     = "<apiKey>"             (BFF/voice token minter)
#   polyphon-livekit-api-secret  = "<secret>"             (BFF/voice token minter)
#
# Idempotent / convergent: a re-run REUSES the existing key/secret already in
# Key Vault (a second consecutive run is a no-op). Pass --rotate to generate a
# fresh pair (then re-roll the pods so livekit-server + BFF pick up the change).
# Provide your own pair with --api-key / --api-secret.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, main() at the
# very bottom. Supports --help and --dry-run (every mutating az call routes
# through run_or_plan, so --dry-run touches nothing).
#
# Requires `az login` before running (live mode). Run with --dry-run to print
# the full plan without making any Azure calls.
#
# DNS (the one manual step this can't do): add an A record
#   livekit.staging.copresent.ai -> <ingress-nginx LoadBalancer IP>
# so the wss signaling cert issues. The media plane rides the livekit-rtc
# LoadBalancer's own public IP (advertised via ICE -- no DNS needed).
# See docs/deploy/livekit-provision.md.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Key Vault name is kv-memql-<env> (matches tiger-provision.sh / blob-provision.sh).
readonly KV_PREFIX="kv-memql"

# Key Vault secret names (kebab-case) and the k8s Secret keys they map to via
# the ExternalSecret.
readonly KV_SECRET_KEYS="livekit-keys"
readonly KV_SECRET_API_KEY="polyphon-livekit-api-key"
readonly KV_SECRET_API_SECRET="polyphon-livekit-api-secret"

#=============================================================================
# STATE (for the end-of-run report)
#=============================================================================

STATE_EXISTS=()
STATE_CREATED=()
STATE_CHANGED=()
STATE_SKIPPED=()

function record_exists()  { STATE_EXISTS+=("$1"); }
function record_created() { STATE_CREATED+=("$1"); }
function record_changed() { STATE_CHANGED+=("$1"); }
function record_skipped() { STATE_SKIPPED+=("$1"); }

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function log()     { echo "  $*"; }
function info()    { echo "[livekit-provision] $*"; }
function warn()    { echo "  WARN: $*" >&2; }
function err()     { echo "  ERROR: $*" >&2; }
function section() { echo; echo "=== $* ==="; }

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: ENV=staging $0 [options]

Seeds the self-hosted LiveKit shared-secret pair into Azure Key Vault
(kv-memql-<env>) so the committed ExternalSecret can sync the livekit-secrets
k8s Secret. Idempotent: re-runs reuse the existing pair.

Options:
    --vault=NAME      Key Vault name (default: ${KV_PREFIX}-<env>).
    --api-key=KEY     Use this LiveKit API key instead of generating one.
    --api-secret=SEC  Use this LiveKit API secret instead of generating one.
    --rotate          Generate a fresh pair even if one already exists.
    --dry-run         Print the full plan; mutate nothing.
    --help, -h        Show this help and exit.

Environment:
    ENV               staging (default) | production

Examples:
    ENV=staging $0 --dry-run
    ENV=staging $0
    ENV=staging $0 --rotate
    ENV=production $0 --api-key=APxxxx --api-secret=yyyy

After a live run: the ExternalSecret reconciles livekit-secrets (verify with
'kubectl get externalsecret livekit-secrets -n memql'), then the livekit /
bff / voice manifests can roll. Add the DNS A record (see the header / runbook).
EOF
}

function parse_arguments() {
    ENV="${ENV:-staging}"
    DRY_RUN=false
    VAULT_ARG=""
    API_KEY_ARG=""
    API_SECRET_ARG=""
    ROTATE=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)         ENV="${1#*=}" ;;
            --env)           shift; ENV="${1:-}" ;;
            --vault=*)       VAULT_ARG="${1#*=}" ;;
            --vault)         shift; VAULT_ARG="${1:-}" ;;
            --api-key=*)     API_KEY_ARG="${1#*=}" ;;
            --api-key)       shift; API_KEY_ARG="${1:-}" ;;
            --api-secret=*)  API_SECRET_ARG="${1#*=}" ;;
            --api-secret)    shift; API_SECRET_ARG="${1:-}" ;;
            --rotate)        ROTATE=true ;;
            --dry-run)       DRY_RUN=true ;;
            --help|-h)       show_help; exit 0 ;;
            *) err "unknown option: $1"; show_help; exit 1 ;;
        esac
        shift
    done
}

function validate_arguments() {
    case "${ENV}" in
        staging|production) ;;
        *) err "invalid ENV: ${ENV} (must be staging or production)"; exit 1 ;;
    esac
    KV_NAME="${VAULT_ARG:-${KV_PREFIX}-${ENV}}"
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function have() { command -v "$1" >/dev/null 2>&1; }

# Every state-changing az call routes through here so --dry-run is side-effect
# free.
function run_or_plan() {
    if [[ "${DRY_RUN}" == true ]]; then
        echo "  [plan] $*"
        return 0
    fi
    "$@"
}

function check_prerequisites() {
    have az || { err "az CLI not found. Install the Azure CLI (see 'make deploy-setup')."; exit 1; }
    have openssl || { err "openssl not found (needed to generate the key/secret)."; exit 1; }
    if [[ "${DRY_RUN}" != true ]]; then
        if ! az account show >/dev/null 2>&1; then
            err "not logged in to Azure. Run 'az login' first."; exit 1
        fi
        if ! az keyvault show --name "${KV_NAME}" >/dev/null 2>&1; then
            err "Key Vault ${KV_NAME} not found / no access. Run 'make deploy-setup ENV=${ENV}' first."; exit 1
        fi
    fi
}

#=============================================================================
# KEY VAULT HELPERS
#=============================================================================

# Echo the current value of a KV secret, or empty string if absent.
function kv_get() {
    local name="$1"
    [[ "${DRY_RUN}" == true ]] && { echo ""; return 0; }
    az keyvault secret show --vault-name "${KV_NAME}" --name "${name}" \
        --query value -o tsv 2>/dev/null || echo ""
}

# Set a KV secret only if its value differs from what's already stored.
function kv_set_if_changed() {
    local name="$1" value="$2"
    local current
    current="$(kv_get "${name}")"
    if [[ "${current}" == "${value}" ]]; then
        record_exists "${name}"
        log "${name}: unchanged"
        return 0
    fi
    if run_or_plan az keyvault secret set --vault-name "${KV_NAME}" \
        --name "${name}" --value "${value}" --only-show-errors >/dev/null 2>&1; then
        if [[ -n "${current}" ]]; then record_changed "${name}"; log "${name}: updated";
        else record_created "${name}"; log "${name}: created"; fi
    else
        err "failed to set ${name} in ${KV_NAME}"; exit 1
    fi
}

#=============================================================================
# KEY/SECRET RESOLUTION
#=============================================================================

# Resolve the api key + secret to seed:
#   --api-key/--api-secret      -> use as given
#   existing in KV + no --rotate -> reuse (idempotent no-op)
#   otherwise                    -> generate a fresh pair
function resolve_pair() {
    local existing_key existing_secret
    existing_key="$(kv_get "${KV_SECRET_API_KEY}")"
    existing_secret="$(kv_get "${KV_SECRET_API_SECRET}")"

    if [[ -n "${API_KEY_ARG}" && -n "${API_SECRET_ARG}" ]]; then
        LK_KEY="${API_KEY_ARG}"; LK_SECRET="${API_SECRET_ARG}"
        info "using operator-supplied key/secret"
        return 0
    fi

    if [[ "${ROTATE}" != true && -n "${existing_key}" && -n "${existing_secret}" ]]; then
        LK_KEY="${existing_key}"; LK_SECRET="${existing_secret}"
        info "reusing existing key/secret already in ${KV_NAME}"
        return 0
    fi

    if [[ "${DRY_RUN}" == true ]]; then
        LK_KEY="AP<generated>"; LK_SECRET="<generated>"
        info "would generate a fresh key/secret pair"
        return 0
    fi

    LK_KEY="AP$(openssl rand -hex 6)"
    LK_SECRET="$(openssl rand -base64 36 | tr -d '/+=' | head -c 43)"
    info "generated a fresh key/secret pair"
}

#=============================================================================
# REPORT
#=============================================================================

function state_report() {
    section "STATE REPORT (${ENV} / ${KV_NAME})"
    printf '  exists/unchanged : %s\n' "${STATE_EXISTS[*]:-(none)}"
    printf '  created          : %s\n' "${STATE_CREATED[*]:-(none)}"
    printf '  changed          : %s\n' "${STATE_CHANGED[*]:-(none)}"
    printf '  skipped          : %s\n' "${STATE_SKIPPED[*]:-(none)}"
    cat <<EOF

Next steps:
  1. ExternalSecret reconciles the k8s Secret (declarative, in-repo):
       kubectl get externalsecret livekit-secrets -n memql
  2. Roll/merge the LiveKit manifests (deploy/k8s/base/livekit.yaml + the
     bff/voice env). On a key ROTATE, restart livekit + bff + voice pods.
  3. DNS (manual, registrar-side): A record
       livekit.${ENV}.copresent.ai -> <ingress-nginx LoadBalancer IP>
     The media plane uses the livekit-rtc LoadBalancer IP (ICE; no DNS).
EOF
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    parse_arguments "$@"
    validate_arguments
    check_prerequisites

    section "LiveKit secret provisioning (${ENV} / ${KV_NAME})"
    [[ "${DRY_RUN}" == true ]] && info "DRY RUN -- no Azure calls will mutate state"

    resolve_pair

    kv_set_if_changed "${KV_SECRET_KEYS}"       "${LK_KEY}: ${LK_SECRET}"
    kv_set_if_changed "${KV_SECRET_API_KEY}"    "${LK_KEY}"
    kv_set_if_changed "${KV_SECRET_API_SECRET}" "${LK_SECRET}"

    state_report
    info "done."
}

main "$@"
