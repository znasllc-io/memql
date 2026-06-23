#!/usr/bin/env bash
#
# .claude/scripts/deploy-setup.sh
# ===============================
#
# Idempotent Azure + toolchain bootstrap for the memQL / CoPresent
# staging (and, parameterized, production) deployment foundation.
# See znasllc-io/memql#492 (epic #491).
#
# This script does TWO things, both safe to re-run any number of
# times (convergent -- second consecutive run is a no-op):
#
#   1. Toolchain   -- install-if-missing-else-verify + authenticate:
#        az (+ containerapp extension), gh, tiger (Tiger Data CLI),
#        docker, jq, psql.
#      On macOS, installs go through Homebrew. On Linux we install
#      what we safely can and otherwise print an install hint rather
#      than running sudo from a make target.
#
#   2. Azure resources (create-or-converge -- existence-checked
#      before every create, so nothing is ever duplicated):
#        rg-memql-<env>        resource group        (East US)
#        acr-memql             container registry    (Basic, SHARED)
#        kv-memql-<env>        key vault
#        cae-memql-<env>       container apps env     (East US)
#        + load/refresh secrets into the key vault from a gitignored
#          local env file (or interactive prompt) -- never hardcoded.
#
# A final STATE REPORT prints what already existed, what was created,
# and what changed this run.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, main()
# at the very bottom calls them in order. Supports --help and a
# --dry-run that prints the full plan and mutates nothing.
#
# NOTE: there is no live Azure subscription yet. This script is
# written to be correct, syntactically clean, and dry-run-able; the
# operator runs it against their own `az login` once a subscription
# exists. Until then, `--dry-run` is the way to exercise it.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Naming + placement. Resource group + key vault + container-apps env
# are per-environment; the container registry is SHARED across envs
# (Basic SKU), matching the epic's locked architecture.
readonly REGION="eastus"
readonly REGION_DISPLAY="East US"
readonly ACR_NAME="acrmemql"          # ACR names are global + alnum-only (no dashes)
readonly ACR_SKU="Basic"

# Default environment. Overridable via --env / ENV / first positional.
DEFAULT_ENV="staging"

# A2 genesis-envelope model (issue #519, epic #491). The cloud cluster's
# ~150 config vars all live INSIDE the sealed genesis envelope, decrypted
# in-process at boot by component/genesis/autoload.go. So the Key Vault
# holds exactly THREE secrets -- the bootstrap layer the binary needs to
# unseal + reach the database -- not the old "6 individual secrets" that
# came from a stale GCP service.yaml:
#
#   memory-nodes-database-dsn  -- the Tiger Cloud DSN. KEPT. Often already
#                                 written by tiger-provision.sh; we only
#                                 rewrite it when it actually changed.
#   memql-master-key           -- the 32-byte master key (MEMQL_MASTER_KEY)
#                                 that decrypts the envelope. Operator-
#                                 supplied via env var only; NEVER read
#                                 from the repo. Warn + skip if unset.
#   memql-genesis-b64          -- base64 of the operator's SEALED envelope
#                                 file (default ~/.memql/genesis.znas). The
#                                 envelope is ENCRYPTED, so storing it in
#                                 Key Vault is safe. Warn + skip if missing.
#
# Everything that used to be its own secret (OpenAI keys, content-ID salt,
# Discord webhooks) is inside the envelope now.

# Source env var carrying the master key value. Operator-supplied only.
readonly MASTER_KEY_ENV_VAR="MEMQL_MASTER_KEY"

# Key Vault secret names (KV names allow only [A-Za-z0-9-]).
readonly KV_SECRET_DSN="memory-nodes-database-dsn"
readonly KV_SECRET_MASTER_KEY="memql-master-key"
readonly KV_SECRET_GENESIS_B64="memql-genesis-b64"

# Where the sealed envelope file lives. Overridable via MEMQL_GENESIS_PATH
# (the same env var the binary's autoload honors); defaults to
# ~/.memql/genesis.znas. The file is ENCRYPTED; we base64 it and store the
# ciphertext.
DEFAULT_GENESIS_PATH="${HOME}/.memql/genesis.znas"

# Where we look for the DSN value when not already in the environment.
# Gitignored (.env.* is ignored repo-wide; see .gitignore). Override
# with --secrets-file. Default is per-env so staging + prod don't
# collide.
DEFAULT_SECRETS_FILE=""   # resolved in validate_arguments once ENV is known

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
function info()  { echo "[deploy-setup] $*"; }
function warn()  { echo "  WARN: $*" >&2; }
function err()   { echo "  ERROR: $*" >&2; }

function section() {
    echo ""
    echo "=== $* ==="
}

# record_* push into the state report. We keep them distinct so the
# final report can show, per resource, whether it pre-existed, was
# created, or had drift reconciled this run.
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

Idempotent Azure + toolchain bootstrap for the memQL deployment
foundation. Installs/verifies the toolchain and creates-or-converges
the core Azure resources for the given environment. Safe to re-run;
the second consecutive run is a no-op.

Options:
    --env=ENV            Target environment: staging (default) or production.
                         May also be passed positionally or via ENV=.
    --secrets-file=PATH  Env file to read the DSN value from
                         (default: .env.deploy.<env>, gitignored).
    --skip-toolchain     Skip the toolchain install/verify phase.
    --skip-secrets       Skip loading secrets into Key Vault.
    --dry-run            Print the full plan; mutate nothing.
    --help               Show this help and exit.

Examples:
    $0 --dry-run
    $0 --env=staging
    ENV=production $0 --dry-run
    $0 --env=staging --secrets-file=~/.memql/deploy.staging.env

Toolchain bootstrapped (install-if-missing, else verify+auth):
    az (+ containerapp ext), gh, tiger, docker, jq, psql

Azure resources (create-or-converge, region: ${REGION_DISPLAY}):
    rg-memql-<env>   resource group
    ${ACR_NAME}      container registry (${ACR_SKU}, shared across envs)
    kv-memql-<env>   key vault
    cae-memql-<env>  container apps environment

Key Vault secrets (A2 genesis-envelope model -- issue #519):
    ${KV_SECRET_DSN}   Tiger Cloud DSN (kept; from MEMQL_DATABASE_DSN)
    ${KV_SECRET_MASTER_KEY}            32-byte key from \$${MASTER_KEY_ENV_VAR} (operator env only)
    ${KV_SECRET_GENESIS_B64}           base64 of the sealed envelope
                              (\$MEMQL_GENESIS_PATH, default ~/.memql/genesis.znas)
EOF
}

#=============================================================================
# ARGUMENT PARSING + VALIDATION
#=============================================================================

function parse_arguments() {
    ENV="${ENV:-$DEFAULT_ENV}"
    SECRETS_FILE=""
    DRY_RUN=false
    SKIP_TOOLCHAIN=false
    SKIP_SECRETS=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)          ENV="${1#*=}" ;;
            --env)            shift; ENV="${1:-}" ;;
            --secrets-file=*) SECRETS_FILE="${1#*=}" ;;
            --secrets-file)   shift; SECRETS_FILE="${1:-}" ;;
            --skip-toolchain) SKIP_TOOLCHAIN=true ;;
            --skip-secrets)   SKIP_SECRETS=true ;;
            --dry-run)        DRY_RUN=true ;;
            --help|-h)        show_help; exit 0 ;;
            staging|production)
                # Bare positional environment, e.g. `deploy-setup staging`.
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

    # Per-env resource names derived from the validated ENV.
    RG_NAME="rg-memql-${ENV}"
    KV_NAME="kv-memql-${ENV}"
    CAE_NAME="cae-memql-${ENV}"

    if [ -z "${SECRETS_FILE}" ]; then
        SECRETS_FILE="${DEFAULT_SECRETS_FILE:-.env.deploy.${ENV}}"
    fi

    if [ "$ENV" = "production" ]; then
        warn "ENV=production is a parameterized STUB. Resource names + steps"
        warn "are wired, but production has not been validated against a live"
        warn "subscription. Review carefully before a real production run."
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
# command and return success WITHOUT executing; otherwise we execute
# it. Every state-changing az/brew/tiger call routes through here so
# --dry-run is guaranteed side-effect-free.
function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] $*"
        return 0
    fi
    "$@"
}

function check_prerequisites() {
    OS="$(detect_os)"
    PKG=""
    if have brew; then PKG="brew"; fi

    if [ "$OS" = "unknown" ]; then
        warn "Unrecognized platform '$(uname -s)'. Toolchain auto-install is"
        warn "disabled; missing tools will be reported as install hints only."
    fi
}

#=============================================================================
# TOOLCHAIN -- install-if-missing-else-verify + authenticate
#=============================================================================

# brew_install installs a formula (or --cask) on macOS, routed through
# run_or_plan so --dry-run stays clean. On non-brew platforms it prints
# a hint and returns non-zero so the caller can decide whether the tool
# is blocking.
function brew_install() {
    local spec="$1"   # e.g. "azure-cli" or "--cask timescale/tap/tiger-cli"
    if [ "$PKG" = "brew" ]; then
        # shellcheck disable=SC2086
        run_or_plan brew install $spec
        return $?
    fi
    return 1
}

function ensure_az() {
    if have az; then
        log "[ok] az $(az version --query '\"azure-cli\"' -o tsv 2>/dev/null || echo present)"
    else
        info "az (Azure CLI) not found -- installing..."
        if ! brew_install azure-cli; then
            err "Azure CLI missing. Install: https://learn.microsoft.com/cli/azure/install-azure-cli"
            err "  macOS: brew install azure-cli"
            err "  Linux: curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash"
            return 1
        fi
    fi

    # containerapp extension -- add if missing, else verify present.
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] az extension add --name containerapp --upgrade (if missing)"
    elif have az; then
        if az extension show --name containerapp >/dev/null 2>&1; then
            log "[ok] az containerapp extension"
        else
            info "adding az containerapp extension..."
            if az extension add --name containerapp --upgrade --only-show-errors; then
                log "[ok] az containerapp extension installed"
            else
                warn "could not add containerapp extension (will need it for deploy)"
            fi
        fi
    fi
}

function ensure_gh() {
    if have gh; then
        log "[ok] gh $(gh --version 2>/dev/null | head -1 | awk '{print $3}')"
    else
        info "gh (GitHub CLI) not found -- installing..."
        if ! brew_install gh; then
            err "GitHub CLI missing. Install: https://github.com/cli/cli#installation"
            return 1
        fi
    fi
}

# Tiger Data CLI (Timescale / Tiger Cloud). Authoritative install per
# https://github.com/timescale/tiger-cli :
#   macOS/Linux brew:  brew install --cask timescale/tap/tiger-cli
#   universal script:  curl -fsSL https://cli.tigerdata.com | sh
# We prefer brew on darwin; on other platforms we VERIFY + INSTRUCT
# rather than piping a remote script into a shell from a make target.
function ensure_tiger() {
    if have tiger; then
        log "[ok] tiger $(tiger version 2>/dev/null | head -1 || echo present)"
        return 0
    fi
    info "tiger (Tiger Data CLI) not found -- installing..."
    if [ "$PKG" = "brew" ]; then
        if brew_install "--cask timescale/tap/tiger-cli"; then
            return 0
        fi
    fi
    err "Tiger Data CLI missing. Install one of:"
    err "  macOS/Linux (brew): brew install --cask timescale/tap/tiger-cli"
    err "  universal script:   curl -fsSL https://cli.tigerdata.com | sh"
    err "  from source:        go install github.com/timescale/tiger-cli/cmd/tiger@latest"
    err "Docs: https://github.com/timescale/tiger-cli"
    return 1
}

function ensure_docker() {
    if have docker; then
        log "[ok] docker $(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',')"
    else
        err "docker missing. Install Docker Desktop (macOS) or your distro's docker."
        err "  https://docs.docker.com/get-docker/"
        return 1
    fi
}

function ensure_jq() {
    if have jq; then
        log "[ok] jq $(jq --version 2>/dev/null)"
    else
        info "jq not found -- installing..."
        if ! brew_install jq; then
            err "jq missing. Install: https://jqlang.github.io/jq/download/"
            return 1
        fi
    fi
}

function ensure_psql() {
    if have psql; then
        log "[ok] psql $(psql --version 2>/dev/null | awk '{print $3}')"
    else
        info "psql not found -- installing..."
        # libpq ships psql without a full server; keeps the footprint small.
        if ! brew_install libpq; then
            err "psql (libpq) missing. Install:"
            err "  macOS: brew install libpq && brew link --force libpq"
            err "  Linux: apt-get install postgresql-client (or your distro's package)"
            return 1
        fi
        warn "If 'psql' still isn't on PATH after install, run: brew link --force libpq"
    fi
}

# Authenticate every CLI that needs a session. We never trigger an
# interactive login automatically -- we DETECT whether a session
# exists and, if not, print the exact command to run. This keeps the
# bootstrap non-interactive + CI-safe while still telling the operator
# precisely what's missing.
function authenticate_toolchain() {
    section "Authentication status"

    if [ "$DRY_RUN" = true ]; then
        log "[plan] verify: az account show / gh auth status / tiger auth (whoami)"
        return 0
    fi

    if have az; then
        if az account show >/dev/null 2>&1; then
            local sub
            sub="$(az account show --query name -o tsv 2>/dev/null)"
            log "[ok] az logged in (subscription: ${sub:-unknown})"
        else
            warn "az is NOT logged in. Run: az login   (then re-run deploy-setup)"
            AUTH_INCOMPLETE=true
        fi
    fi

    if have gh; then
        if gh auth status >/dev/null 2>&1; then
            log "[ok] gh authenticated"
        else
            warn "gh is NOT authenticated. Run: gh auth login"
            AUTH_INCOMPLETE=true
        fi
    fi

    if have tiger; then
        # tiger has no universal `auth status`; `service list` is the
        # cheapest authenticated probe. A non-zero exit => not logged in.
        if tiger service list >/dev/null 2>&1; then
            log "[ok] tiger authenticated"
        else
            warn "tiger may not be authenticated. Run: tiger auth login"
            AUTH_INCOMPLETE=true
        fi
    fi
}

function bootstrap_toolchain() {
    section "Toolchain (install-if-missing, else verify)"
    AUTH_INCOMPLETE=false
    local failed=0

    ensure_az     || failed=1
    ensure_gh     || failed=1
    ensure_tiger  || failed=1
    ensure_docker || failed=1
    ensure_jq     || failed=1
    ensure_psql   || failed=1

    authenticate_toolchain

    if [ "$failed" -ne 0 ]; then
        warn "One or more tools are missing. Install them (hints above) and re-run."
        warn "Continuing so the plan/report still prints, but Azure steps that"
        warn "need a missing tool will be skipped."
    fi
}

#=============================================================================
# AZURE RESOURCES -- create-or-converge (idempotent)
#=============================================================================

# az_ready guards every Azure step: in dry-run we always proceed (to
# print the plan); live, we require az present + logged in.
function az_ready() {
    if [ "$DRY_RUN" = true ]; then
        return 0
    fi
    if ! have az; then
        return 1
    fi
    az account show >/dev/null 2>&1
}

function ensure_resource_group() {
    section "Resource group: ${RG_NAME} (${REGION_DISPLAY})"
    if ! az_ready; then
        warn "az not ready (not installed or not logged in) -- skipping ${RG_NAME}."
        record_skipped "resource-group: ${RG_NAME}"
        return 0
    fi

    if [ "$DRY_RUN" = false ] && az group show --name "${RG_NAME}" >/dev/null 2>&1; then
        log "[ok] resource group ${RG_NAME} already exists"
        record_exists "resource-group: ${RG_NAME}"
        return 0
    fi

    info "creating resource group ${RG_NAME}..."
    run_or_plan az group create \
        --name "${RG_NAME}" \
        --location "${REGION}" \
        --only-show-errors
    record_created "resource-group: ${RG_NAME}"
}

function ensure_container_registry() {
    section "Container registry: ${ACR_NAME} (${ACR_SKU}, shared)"
    if ! az_ready; then
        warn "az not ready -- skipping ${ACR_NAME}."
        record_skipped "acr: ${ACR_NAME}"
        return 0
    fi

    # ACR is SHARED across envs but must live in a resource group. We
    # anchor it to the staging RG (created first / always present);
    # production reuses the same registry by name regardless of env.
    local acr_rg="rg-memql-staging"

    if [ "$DRY_RUN" = false ] && az acr show --name "${ACR_NAME}" >/dev/null 2>&1; then
        log "[ok] container registry ${ACR_NAME} already exists (shared)"
        # Converge SKU drift back to Basic if someone bumped it.
        local cur_sku
        cur_sku="$(az acr show --name "${ACR_NAME}" --query sku.name -o tsv 2>/dev/null || echo '')"
        if [ -n "${cur_sku}" ] && [ "${cur_sku}" != "${ACR_SKU}" ]; then
            info "ACR SKU drift: ${cur_sku} -> ${ACR_SKU}; reconciling..."
            run_or_plan az acr update --name "${ACR_NAME}" --sku "${ACR_SKU}" --only-show-errors
            record_changed "acr: ${ACR_NAME} (sku ${cur_sku}->${ACR_SKU})"
        else
            record_exists "acr: ${ACR_NAME}"
        fi
        return 0
    fi

    info "creating container registry ${ACR_NAME} (${ACR_SKU})..."
    run_or_plan az acr create \
        --resource-group "${acr_rg}" \
        --name "${ACR_NAME}" \
        --sku "${ACR_SKU}" \
        --location "${REGION}" \
        --only-show-errors
    record_created "acr: ${ACR_NAME}"
}

function ensure_key_vault() {
    section "Key vault: ${KV_NAME}"
    if ! az_ready; then
        warn "az not ready -- skipping ${KV_NAME}."
        record_skipped "key-vault: ${KV_NAME}"
        return 0
    fi

    if [ "$DRY_RUN" = false ] && az keyvault show --name "${KV_NAME}" >/dev/null 2>&1; then
        log "[ok] key vault ${KV_NAME} already exists"
        record_exists "key-vault: ${KV_NAME}"
        grant_kv_secrets_officer
        return 0
    fi

    info "creating key vault ${KV_NAME}..."
    run_or_plan az keyvault create \
        --name "${KV_NAME}" \
        --resource-group "${RG_NAME}" \
        --location "${REGION}" \
        --only-show-errors
    record_created "key-vault: ${KV_NAME}"
    grant_kv_secrets_officer
}

# Grant the signed-in operator the "Key Vault Secrets Officer" role at
# the vault scope. The vault is RBAC-mode, so without this the secret
# writes in load_secrets fail (Forbidden) -- validated live. Idempotent:
# az role assignment create is a no-op when the assignment already
# exists. The secrets phase depends on this, so it runs before
# load_secrets. Routed through run_or_plan so --dry-run stays clean.
#
# Runtime READ access for the Container Apps is granted by `make deploy`
# (.claude/scripts/deploy.sh): each app's system-assigned managed
# identity gets the "Key Vault Secrets User" role at this vault scope so
# it can resolve the three Key Vault secret references at boot. That
# can't happen here -- the Container Apps don't exist yet at bootstrap
# time -- so it lives in the deploy step that creates them.
function grant_kv_secrets_officer() {
    local oid vault_id
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] az ad signed-in-user show --query id -o tsv"
        echo "  [plan] az keyvault show --name ${KV_NAME} --query id -o tsv"
        echo "  [plan] az role assignment create --role \"Key Vault Secrets Officer\" --assignee <oid> --scope <vaultId>"
        return 0
    fi

    oid="$(az ad signed-in-user show --query id -o tsv 2>/dev/null || echo '')"
    if [ -z "${oid}" ]; then
        warn "could not resolve signed-in user object id -- skipping ${KV_NAME} RBAC grant."
        warn "  Secret writes may fail (Forbidden) until the operator has 'Key Vault Secrets Officer'."
        record_skipped "kv-role: Key Vault Secrets Officer (${KV_NAME})"
        return 0
    fi

    vault_id="$(az keyvault show --name "${KV_NAME}" --query id -o tsv 2>/dev/null || echo '')"
    if [ -z "${vault_id}" ]; then
        warn "could not resolve vault id for ${KV_NAME} -- skipping RBAC grant."
        record_skipped "kv-role: Key Vault Secrets Officer (${KV_NAME})"
        return 0
    fi

    info "granting 'Key Vault Secrets Officer' on ${KV_NAME} to the signed-in operator..."
    if az role assignment create \
        --role "Key Vault Secrets Officer" \
        --assignee "${oid}" \
        --scope "${vault_id}" \
        --only-show-errors >/dev/null 2>&1; then
        record_changed "kv-role: Key Vault Secrets Officer (${KV_NAME})"
    else
        # Already-assigned is the common idempotent case; az returns
        # non-zero. Treat as already-correct rather than a hard failure.
        log "[ok] 'Key Vault Secrets Officer' already granted on ${KV_NAME} (or grant unchanged)"
        record_exists "kv-role: Key Vault Secrets Officer (${KV_NAME})"
    fi
}

function ensure_container_apps_env() {
    section "Container Apps environment: ${CAE_NAME} (${REGION_DISPLAY})"
    if ! az_ready; then
        warn "az not ready -- skipping ${CAE_NAME}."
        record_skipped "container-apps-env: ${CAE_NAME}"
        return 0
    fi

    if [ "$DRY_RUN" = false ] \
        && az containerapp env show --name "${CAE_NAME}" --resource-group "${RG_NAME}" >/dev/null 2>&1; then
        log "[ok] container apps environment ${CAE_NAME} already exists"
        record_exists "container-apps-env: ${CAE_NAME}"
        return 0
    fi

    info "creating container apps environment ${CAE_NAME}..."
    run_or_plan az containerapp env create \
        --name "${CAE_NAME}" \
        --resource-group "${RG_NAME}" \
        --location "${REGION}" \
        --only-show-errors
    record_created "container-apps-env: ${CAE_NAME}"
}

#=============================================================================
# SECRETS -- the A2 genesis-envelope model: exactly THREE Key Vault secrets
#=============================================================================

# Resolve the DSN value: prefer an already-exported MEMQL_DATABASE_DSN;
# else read it from the gitignored secrets file; else (interactive only)
# prompt. Returns empty + non-zero if we can't resolve one. The DSN may
# already have been written by tiger-provision.sh, so a missing value here
# is a non-fatal skip rather than an error.
function resolve_dsn_value() {
    local var="MEMQL_DATABASE_DSN"
    local val="${!var:-}"

    if [ -n "${val}" ]; then
        printf '%s' "${val}"
        return 0
    fi

    if [ -f "${SECRETS_FILE}" ]; then
        val="$(grep -E "^${var}=" "${SECRETS_FILE}" 2>/dev/null | tail -1 | cut -d= -f2- | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//")"
        if [ -n "${val}" ]; then
            printf '%s' "${val}"
            return 0
        fi
    fi

    # Interactive fallback only when attached to a TTY and not dry-run.
    if [ "$DRY_RUN" = false ] && [ -t 0 ]; then
        local entered
        read -r -s -p "  value for ${var} (input hidden, blank to skip): " entered
        echo ""
        if [ -n "${entered}" ]; then
            printf '%s' "${entered}"
            return 0
        fi
    fi

    return 1
}

# set_kv_secret writes one Key Vault secret idempotently: it only rewrites
# when the stored value differs from the new one, so re-runs are a true
# no-op and we don't spam new secret versions. Records exists/created/
# changed/skipped into the state report. Routed past run_or_plan manually
# because we need the read-compare-write logic.
#   $1 KV secret name, $2 new value
function set_kv_secret() {
    local kvname="$1" value="$2"

    local current
    current="$(az keyvault secret show --vault-name "${KV_NAME}" --name "${kvname}" --query value -o tsv 2>/dev/null || echo '')"
    if [ -n "${current}" ] && [ "${current}" = "${value}" ]; then
        log "[ok] ${kvname} already up to date"
        record_exists "secret: ${kvname}"
        return 0
    fi

    info "setting ${kvname}..."
    if az keyvault secret set --vault-name "${KV_NAME}" --name "${kvname}" --value "${value}" --only-show-errors >/dev/null 2>&1; then
        if [ -n "${current}" ]; then
            record_changed "secret: ${kvname} (updated)"
        else
            record_created "secret: ${kvname}"
        fi
    else
        warn "failed to set ${kvname}"
        record_skipped "secret: ${kvname}"
    fi
}

# memory-nodes-database-dsn -- the Tiger Cloud DSN. KEPT from the old model.
# Often already written by tiger-provision.sh; we only rewrite on change so
# we never clobber an unchanged value.
function load_dsn_secret() {
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] az keyvault secret set --vault-name ${KV_NAME} --name ${KV_SECRET_DSN} --value <MEMQL_DATABASE_DSN>"
        return 0
    fi

    local value
    if ! value="$(resolve_dsn_value)"; then
        warn "no value for MEMQL_DATABASE_DSN -- leaving ${KV_SECRET_DSN} unchanged."
        warn "  (tiger-provision.sh may already have stored it; that's fine.)"
        record_skipped "secret: ${KV_SECRET_DSN}"
        return 0
    fi
    set_kv_secret "${KV_SECRET_DSN}" "${value}"
    unset value
}

# memql-master-key -- the 32-byte master key that decrypts the genesis
# envelope. Operator-supplied via $MEMQL_MASTER_KEY ONLY; never read from
# the repo or the secrets file. Warn + skip if unset.
function load_master_key_secret() {
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] az keyvault secret set --vault-name ${KV_NAME} --name ${KV_SECRET_MASTER_KEY} --value <\$${MASTER_KEY_ENV_VAR}>"
        return 0
    fi

    local value="${!MASTER_KEY_ENV_VAR:-}"
    if [ -z "${value}" ]; then
        warn "\$${MASTER_KEY_ENV_VAR} is not set -- leaving ${KV_SECRET_MASTER_KEY} unchanged."
        warn "  Export it (32-byte / 64 hex) before running so the cluster can unseal the envelope."
        record_skipped "secret: ${KV_SECRET_MASTER_KEY}"
        return 0
    fi
    set_kv_secret "${KV_SECRET_MASTER_KEY}" "${value}"
}

# memql-genesis-b64 -- base64 of the operator's SEALED envelope file. The
# envelope is ENCRYPTED, so the ciphertext is safe to store in Key Vault.
# The binary's autoload decodes + decrypts it in-process at boot. Warn +
# skip if the envelope file is missing.
function load_genesis_b64_secret() {
    local path="${MEMQL_GENESIS_PATH:-$DEFAULT_GENESIS_PATH}"

    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] az keyvault secret set --vault-name ${KV_NAME} --name ${KV_SECRET_GENESIS_B64} --value <base64 of ${path}>"
        return 0
    fi

    if [ ! -f "${path}" ]; then
        warn "sealed envelope not found at ${path} -- leaving ${KV_SECRET_GENESIS_B64} unchanged."
        warn "  Set \$MEMQL_GENESIS_PATH or place the envelope at ${DEFAULT_GENESIS_PATH}."
        record_skipped "secret: ${KV_SECRET_GENESIS_B64}"
        return 0
    fi

    local value
    if ! value="$(base64 < "${path}" | tr -d '\n')"; then
        warn "could not base64 the envelope at ${path}."
        record_skipped "secret: ${KV_SECRET_GENESIS_B64}"
        return 0
    fi
    set_kv_secret "${KV_SECRET_GENESIS_B64}" "${value}"
    unset value
}

function load_secrets() {
    section "Secrets -> Key Vault (${KV_NAME}) -- A2 genesis-envelope model"

    if [ "$SKIP_SECRETS" = true ]; then
        log "--skip-secrets set; not touching key vault secrets."
        return 0
    fi
    if ! az_ready; then
        warn "az not ready -- skipping secret load."
        record_skipped "secrets: ${KV_NAME}"
        return 0
    fi

    log "storing 3 bootstrap secrets (envelope holds the rest, decrypted at boot):"
    log "  ${KV_SECRET_DSN}, ${KV_SECRET_MASTER_KEY}, ${KV_SECRET_GENESIS_B64}"
    if [ "$DRY_RUN" = false ] && [ -f "${SECRETS_FILE}" ]; then
        log "reading the DSN value from ${SECRETS_FILE} (gitignored) if not already exported"
    fi

    load_dsn_secret
    load_master_key_secret
    load_genesis_b64_secret
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

    echo "  Changed / reconciled:${plan_note}"
    if [ "${#STATE_CHANGED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CHANGED[@]}"; fi

    echo "  Skipped (tool/auth/value missing):"
    if [ "${#STATE_SKIPPED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_SKIPPED[@]}"; fi

    echo ""
    if [ "$DRY_RUN" = true ]; then
        info "DRY RUN complete -- nothing was mutated. Re-run without --dry-run"
        info "against an 'az login' session to apply."
    elif [ "${#STATE_SKIPPED[@]}" -ne 0 ] || [ "${AUTH_INCOMPLETE:-false}" = true ]; then
        info "Bootstrap ran with skips. Resolve the items above and re-run;"
        info "re-running is safe (idempotent) and converges remaining state."
    else
        info "Bootstrap complete. A second consecutive run will be a no-op."
    fi
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    parse_arguments "$@"
    validate_arguments
    check_prerequisites

    info "memQL deploy-setup -- env=${ENV} region=${REGION_DISPLAY} dry-run=${DRY_RUN}"

    if [ "$SKIP_TOOLCHAIN" = true ]; then
        info "--skip-toolchain set; skipping toolchain phase."
        AUTH_INCOMPLETE=false
    else
        bootstrap_toolchain
    fi

    # Order matters: RG first (everything lives in it / anchors to it),
    # then the shared ACR, then per-env KV + Container Apps env, then
    # secrets into the KV.
    ensure_resource_group
    ensure_container_registry
    ensure_key_vault
    ensure_container_apps_env
    load_secrets

    print_state_report
}

main "$@"
