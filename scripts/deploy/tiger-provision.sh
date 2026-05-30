#!/usr/bin/env bash
#
# scripts/deploy/tiger-provision.sh
# =================================
#
# Provision the managed Tiger Cloud (Timescale Community + pgvector)
# database for the memQL / CoPresent Azure deployment foundation and
# wire its connection DSN into the per-env Key Vault.
# See znasllc-io/memql#494 (epic #491).
#
# This script does TWO things, both safe to re-run any number of times
# (convergent -- a second consecutive run is a no-op):
#
#   1. Tiger Cloud service -- create-or-verify (existence-checked
#      before create, so a service is never duplicated):
#        memql-<env>   service in Azure, region East US
#      then confirm the two extensions memQL needs are present:
#        timescaledb   (Timescale Community)
#        vector        (pgvector)
#
#   2. DSN -> Key Vault -- read the service's connection DSN from the
#      tiger CLI and store it as the Key Vault secret
#        memory-nodes-database-dsn   (in kv-memql-<env>)
#      ONLY when the value differs from what's already stored (so a
#      re-run that didn't rotate the DSN writes no new secret version).
#
# A final STATE REPORT prints what already existed, what was created,
# and what changed this run.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, main()
# at the very bottom calls them in order. Supports --help and a
# --dry-run that prints the full plan and mutates nothing.
#
# NOTE: there is no live Tiger Cloud account or Azure subscription yet.
# This script is written to be correct, syntactically clean, and
# dry-run-able; the operator runs it against their own `tiger auth
# login` + `az login` sessions once those accounts exist. On a
# non-macOS host with the `tiger` CLI missing the script verifies +
# instructs rather than auto-installing (matching deploy-setup.sh).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Placement. The DB lives in Tiger Cloud on Azure, East US -- the
# locked region from the epic architecture (#491). The Tiger CLI's
# Azure region slug for East US is "azure-eastus".
readonly REGION_DISPLAY="Azure East US"
readonly TIGER_REGION="azure-eastus"

# memQL needs TimescaleDB (Community) for hypertables + pgvector for
# embedding similarity. These are confirmed (not installed) -- Tiger
# Cloud ships TimescaleDB by default and offers pgvector; the script
# verifies they're enabled and, where the CLI allows, enables vector.
readonly REQUIRED_EXTENSIONS=(
    "timescaledb"
    "vector"
)

# The DSN is stored under the SAME Key Vault secret name deploy-setup.sh
# uses for it, so the two scripts converge on one secret. deploy-setup
# maps MEMORY_NODES_DATABASE_DSN -> kebab-case; we mirror that here
# rather than re-deriving so the names can never drift.
readonly DSN_ENV_VAR="MEMORY_NODES_DATABASE_DSN"
readonly DSN_KV_SECRET="memory-nodes-database-dsn"

# Default environment. Overridable via --env / ENV / first positional.
DEFAULT_ENV="staging"

# State accumulators for the final report. Each entry is a single
# "kind: name" line.
STATE_EXISTS=()
STATE_CREATED=()
STATE_CHANGED=()
STATE_SKIPPED=()

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function log()   { echo "  $*"; }
function info()  { echo "[tiger-provision] $*"; }
function warn()  { echo "  WARN: $*" >&2; }
function err()   { echo "  ERROR: $*" >&2; }

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

Provision the managed Tiger Cloud (Timescale Community + pgvector)
database for memQL and wire its DSN into the per-env Key Vault.
Idempotent: re-running detects the existing service and only rewrites
the DSN secret when it actually changed (rotation).

Options:
    --env=ENV            Target environment: staging (default) or production.
                         May also be passed positionally or via ENV=.
    --skip-dsn           Provision/verify the service but do not touch
                         the Key Vault DSN secret.
    --skip-extensions    Skip the timescaledb/vector extension check.
    --dry-run            Print the full plan; mutate nothing.
    --help               Show this help and exit.

Resources (region: ${REGION_DISPLAY}):
    memql-<env>                       Tiger Cloud service
    extensions: ${REQUIRED_EXTENSIONS[*]}
    ${DSN_KV_SECRET}     Key Vault secret in kv-memql-<env>

Examples:
    $0 --dry-run
    $0 --env=staging
    ENV=production $0 --dry-run

Prerequisites (verified, never auto-installed from here):
    tiger   Tiger Data CLI, authenticated (tiger auth login)
    az      Azure CLI, logged in (az login) -- for the DSN -> Key Vault step
EOF
}

#=============================================================================
# ARGUMENT PARSING + VALIDATION
#=============================================================================

function parse_arguments() {
    ENV="${ENV:-$DEFAULT_ENV}"
    DRY_RUN=false
    SKIP_DSN=false
    SKIP_EXTENSIONS=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)            ENV="${1#*=}" ;;
            --env)              shift; ENV="${1:-}" ;;
            --skip-dsn)         SKIP_DSN=true ;;
            --skip-extensions)  SKIP_EXTENSIONS=true ;;
            --dry-run)          DRY_RUN=true ;;
            --help|-h)          show_help; exit 0 ;;
            staging|production)
                ENV="$1"
                ;;
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
    case "$ENV" in
        staging|production) ;;
        *)
            err "Invalid --env: '${ENV}'. Must be 'staging' or 'production'."
            exit 1
            ;;
    esac

    # Per-env names derived from the validated ENV. The Tiger service
    # name and the Key Vault both carry the env suffix; the DB is a
    # separate Tiger Cloud service per environment.
    SERVICE_NAME="memql-${ENV}"
    KV_NAME="kv-memql-${ENV}"

    if [ "$ENV" = "production" ]; then
        warn "ENV=production is a parameterized STUB. The service name +"
        warn "steps are wired, but production has not been validated against"
        warn "a live Tiger Cloud account. Review carefully before a real run."
    fi
}

#=============================================================================
# PREREQUISITES / PLATFORM DETECTION
#=============================================================================

function detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        *)       echo "unknown" ;;
    esac
}

function have() { command -v "$1" >/dev/null 2>&1; }

# run_or_plan: the single mutation gate. In --dry-run we print the
# command and return success WITHOUT executing; otherwise we execute it.
# Every state-changing tiger/az call routes through here so --dry-run is
# guaranteed side-effect-free.
function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] $*"
        return 0
    fi
    "$@"
}

function check_prerequisites() {
    section "Prerequisites"
    log "platform: $(detect_os)"

    if [ "$DRY_RUN" = true ]; then
        log "[plan] verify: tiger (authenticated) + az (logged in)"
        return 0
    fi

    if ! have tiger; then
        warn "tiger (Tiger Data CLI) not found. Install + authenticate first:"
        warn "  macOS/Linux (brew): brew install --cask timescale/tap/tiger-cli"
        warn "  universal script:   curl -fsSL https://cli.tigerdata.com | sh"
        warn "  then:               tiger auth login"
        warn "  (run 'make deploy-setup' to bootstrap the whole toolchain)"
    elif ! tiger service list >/dev/null 2>&1; then
        warn "tiger is installed but may not be authenticated. Run: tiger auth login"
    else
        log "[ok] tiger authenticated"
    fi

    if ! have az; then
        warn "az (Azure CLI) not found -- the DSN -> Key Vault step will be skipped."
        warn "  Install + log in (see 'make deploy-setup'), then re-run."
    elif ! az account show >/dev/null 2>&1; then
        warn "az is not logged in -- the DSN -> Key Vault step will be skipped."
        warn "  Run: az login"
    else
        log "[ok] az logged in"
    fi
}

# tiger_ready guards every Tiger Cloud step: in dry-run we always
# proceed (to print the plan); live, we require tiger present +
# authenticated.
function tiger_ready() {
    if [ "$DRY_RUN" = true ]; then
        return 0
    fi
    if ! have tiger; then
        return 1
    fi
    tiger service list >/dev/null 2>&1
}

# az_ready guards the Key Vault step: dry-run always proceeds; live
# requires az present + logged in.
function az_ready() {
    if [ "$DRY_RUN" = true ]; then
        return 0
    fi
    if ! have az; then
        return 1
    fi
    az account show >/dev/null 2>&1
}

#=============================================================================
# TIGER CLOUD SERVICE -- create-or-verify (idempotent)
#=============================================================================

# Detect an existing service named SERVICE_NAME. Tiger's CLI lists
# services as JSON; we match on the name field. Returns 0 if present.
function service_exists() {
    if ! have tiger; then
        return 1
    fi
    local listing
    listing="$(tiger service list -o json 2>/dev/null || tiger service list 2>/dev/null || echo '')"
    if [ -z "${listing}" ]; then
        return 1
    fi
    # Prefer a structured match when jq is available + the listing is
    # JSON; fall back to a plain grep on the service name otherwise.
    if have jq && echo "${listing}" | jq . >/dev/null 2>&1; then
        echo "${listing}" | jq -e --arg n "${SERVICE_NAME}" \
            'if type=="array" then any(.[]; (.name // .service_name) == $n) else (.name // .service_name) == $n end' \
            >/dev/null 2>&1
    else
        echo "${listing}" | grep -qw "${SERVICE_NAME}"
    fi
}

function ensure_service() {
    section "Tiger Cloud service: ${SERVICE_NAME} (${REGION_DISPLAY})"

    if ! tiger_ready; then
        warn "tiger not ready (not installed or not authenticated) -- skipping ${SERVICE_NAME}."
        record_skipped "tiger-service: ${SERVICE_NAME}"
        return 0
    fi

    if [ "$DRY_RUN" = false ] && service_exists; then
        log "[ok] tiger service ${SERVICE_NAME} already exists"
        record_exists "tiger-service: ${SERVICE_NAME}"
        return 0
    fi

    info "creating tiger service ${SERVICE_NAME} in ${REGION_DISPLAY}..."
    # --no-wait would return before the service is provisioned; we let
    # the CLI block so the DSN read below sees a ready service. The
    # exact flag surface is pinned to the tiger CLI's documented
    # `service create` options.
    run_or_plan tiger service create \
        --name "${SERVICE_NAME}" \
        --region "${TIGER_REGION}" \
        --addons time-series,ai
    record_created "tiger-service: ${SERVICE_NAME}"
}

#=============================================================================
# EXTENSIONS -- confirm timescaledb + pgvector
#=============================================================================

# Run a SQL probe against the service via the tiger CLI's psql-style
# bridge. We don't open a raw psql connection here -- `tiger service`
# brokers credentials, which keeps the DSN out of this script's
# environment until the explicit DSN-read step below.
function ensure_extensions() {
    section "Extensions on ${SERVICE_NAME}"

    if [ "$SKIP_EXTENSIONS" = true ]; then
        log "--skip-extensions set; not checking timescaledb/vector."
        return 0
    fi
    if ! tiger_ready; then
        warn "tiger not ready -- skipping extension check."
        record_skipped "extensions: ${SERVICE_NAME}"
        return 0
    fi

    local ext
    for ext in "${REQUIRED_EXTENSIONS[@]}"; do
        if [ "$DRY_RUN" = true ]; then
            echo "  [plan] tiger service exec ${SERVICE_NAME} -- CREATE EXTENSION IF NOT EXISTS ${ext}"
            continue
        fi

        # CREATE EXTENSION IF NOT EXISTS is itself idempotent -- it's a
        # no-op when the extension is already enabled, and enables it
        # when the platform allows. timescaledb is pre-enabled on Tiger
        # Cloud; vector may need this call on first provision.
        if tiger service exec "${SERVICE_NAME}" -- \
            "CREATE EXTENSION IF NOT EXISTS ${ext};" >/dev/null 2>&1; then
            log "[ok] extension ${ext} present/enabled"
            record_exists "extension: ${ext}"
        else
            warn "could not confirm extension ${ext} on ${SERVICE_NAME}"
            warn "  (verify in the Tiger Cloud console; vector may need a plan that offers pgvector)"
            record_skipped "extension: ${ext}"
        fi
    done
}

#=============================================================================
# DSN -> KEY VAULT (write only on change)
#=============================================================================

# Read the connection DSN for the service from the tiger CLI. Printed to
# stdout; empty + non-zero if it can't be resolved. An exported
# MEMORY_NODES_DATABASE_DSN overrides (lets an operator wire a manually-
# rotated DSN without re-reading from tiger).
function resolve_dsn() {
    local override="${!DSN_ENV_VAR:-}"
    if [ -n "${override}" ]; then
        printf '%s' "${override}"
        return 0
    fi

    if ! have tiger; then
        return 1
    fi

    local dsn
    # `tiger service get-connection-string` is the documented surface
    # for a service's libpq DSN. We request the pooler/primary URI.
    dsn="$(tiger service get-connection-string "${SERVICE_NAME}" --format uri 2>/dev/null || echo '')"
    if [ -z "${dsn}" ]; then
        return 1
    fi
    printf '%s' "${dsn}"
}

function store_dsn() {
    section "DSN -> Key Vault (${KV_NAME} / ${DSN_KV_SECRET})"

    if [ "$SKIP_DSN" = true ]; then
        log "--skip-dsn set; not touching the Key Vault DSN secret."
        return 0
    fi

    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] read DSN: tiger service get-connection-string ${SERVICE_NAME} --format uri"
        echo "  [plan] az keyvault secret set --vault-name ${KV_NAME} --name ${DSN_KV_SECRET} --value <DSN> (only if changed)"
        return 0
    fi

    if ! az_ready; then
        warn "az not ready -- skipping DSN -> Key Vault."
        record_skipped "secret: ${DSN_KV_SECRET}"
        return 0
    fi

    local dsn
    if ! dsn="$(resolve_dsn)"; then
        warn "could not resolve the DSN for ${SERVICE_NAME}"
        warn "  (provision the service first, or export ${DSN_ENV_VAR} to set it manually)"
        record_skipped "secret: ${DSN_KV_SECRET}"
        return 0
    fi

    # Idempotent convergence: only write when the stored value differs.
    # Keeps a re-run that didn't rotate the DSN a true no-op and avoids
    # spamming new secret versions.
    local current
    current="$(az keyvault secret show --vault-name "${KV_NAME}" --name "${DSN_KV_SECRET}" --query value -o tsv 2>/dev/null || echo '')"
    if [ -n "${current}" ] && [ "${current}" = "${dsn}" ]; then
        log "[ok] ${DSN_KV_SECRET} already up to date"
        record_exists "secret: ${DSN_KV_SECRET}"
        unset dsn current
        return 0
    fi

    info "setting ${DSN_KV_SECRET} in ${KV_NAME}..."
    if az keyvault secret set --vault-name "${KV_NAME}" --name "${DSN_KV_SECRET}" --value "${dsn}" --only-show-errors >/dev/null 2>&1; then
        if [ -n "${current}" ]; then
            record_changed "secret: ${DSN_KV_SECRET} (rotated)"
        else
            record_created "secret: ${DSN_KV_SECRET}"
        fi
    else
        warn "failed to set ${DSN_KV_SECRET}"
        record_skipped "secret: ${DSN_KV_SECRET}"
    fi
    unset dsn current
}

#=============================================================================
# STATE REPORT
#=============================================================================

function print_state_report() {
    section "State report (env=${ENV}, region=${REGION_DISPLAY}, dry-run=${DRY_RUN})"

    local plan_note=""
    if [ "$DRY_RUN" = true ]; then
        plan_note=" (planned, not executed)"
    fi

    echo "  Already correct:${plan_note}"
    if [ "${#STATE_EXISTS[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_EXISTS[@]}"; fi

    echo "  Created:${plan_note}"
    if [ "${#STATE_CREATED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CREATED[@]}"; fi

    echo "  Changed / rotated:${plan_note}"
    if [ "${#STATE_CHANGED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CHANGED[@]}"; fi

    echo "  Skipped (tool/auth/value missing):"
    if [ "${#STATE_SKIPPED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_SKIPPED[@]}"; fi

    echo ""
    echo "  Note: memQL runs auto-migrations on backend start"
    echo "  (MEMORY_NODES_DATABASE_AUTO_MIGRATE=true,"
    echo "  MEMORY_NODES_DATABASE_MIGRATE_ON_START=true in service.yaml),"
    echo "  so the schema converges against this DB on first deploy."

    echo ""
    if [ "$DRY_RUN" = true ]; then
        info "DRY RUN complete -- nothing was mutated. Re-run without --dry-run"
        info "against authenticated tiger + az sessions to apply."
    elif [ "${#STATE_SKIPPED[@]}" -ne 0 ]; then
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

    info "memQL tiger-provision -- env=${ENV} region=${REGION_DISPLAY} dry-run=${DRY_RUN}"

    check_prerequisites
    ensure_service
    ensure_extensions
    store_dsn
    print_state_report
}

main "$@"
