#!/usr/bin/env bash
#
# scripts/deploy/seed-instance-secrets.sh
# =======================================
#
# Capability: deploy.seedInstanceSecrets -- GENERATE the credentials a fresh
# instance needs, into its own Key Vault, and create the in-cluster shell the
# ExternalSecrets merge into.
#
# Backend for the `seedInstanceSecrets` deployment action (memql#4474, epic
# memql#4490). Steps 8 and 9 of the eleven between substrate and argoSync.
#
# GENERATION IS THE OPERATION. NOT MIGRATION. The runbook step reads "seed the
# vault", and the instinct on a rebuild is to copy the old vault forward. That
# is wrong and the engine's own docs forbid it: a retired vault's entries are
# ANOTHER CLUSTER'S master key, operator key and DSN. A new instance is a new
# trust domain.
#
# It is also, concretely, not even possible. Of the seven entries the mesh
# reads, four did not exist in the retired vault at all -- it predates
# memql#3958/#3960 -- and it still carried memql-genesis-b64, which is retired.
# A migration would have imported a dead value and left four gaps.
#
# THIS SCRIPT THEREFORE HAS NO MIGRATE PATH, no --fromVault, and no way to
# express one. That absence is the feature.
#
# THREE PROPERTIES, EACH LEARNED THE HARD WAY:
#
#   1. CREATE-IF-ABSENT, NEVER OVERWRITE. Regenerating memql-master-key after
#      anything has been encrypted under it destroys the ability to read it
#      back. This is the single most destructive thing a redelivered
#      instance.installRequested event could do, and at-least-once IS the
#      delivery model. Rotation is a separate, explicit verb, not a re-run.
#
#   2. THE DSN AND THE CNPG BOOTSTRAP CREDENTIAL ARE ONE VALUE. CNPG creates
#      the database from memql-db-app-creds; the engine connects with
#      MEMQL_DATABASE_DSN. Generate them independently and the cluster comes up
#      HEALTHY and the engine cannot log in -- every pod Running, every probe
#      green, and an authentication failure in the logs. So on a re-run the
#      password is read back OUT of the existing DSN and never regenerated.
#
#   3. NO VALUE IN A LOG LINE, A RESULT ENVELOPE, OR ARGV. argv is
#      world-readable on a shared runner -- the argument seed-bootstrap.sh
#      already makes for --from-file. Every value here reaches `az` and
#      `kubectl` through a file in a mode-700 directory that is shredded on
#      exit, including on failure. The result envelope reports COUNTS and
#      NAMES; it never reports a value, and there is no verbosity flag that
#      changes that.
#
# THE MERGE SHELL (step 8) is the cheapest item here and the hardest to guess
# from its symptom. Both memql-secrets ExternalSecrets use
# `creationPolicy: Merge`, which merges into an existing Secret and DOES NOT
# CREATE ONE. Correct for the migration it was written for; wrong for a fresh
# install, where nothing has created it. The failure is a Secret that simply
# never appears -- and a missing Secret named by envFrom leaves pods erroring
# on a name with nothing anywhere saying "no target".
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4490 memql#4474 memql#3519 memql#3960 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

#=============================================================================
# CAPABILITY SPEC
#=============================================================================

cap_init "deploy.seedInstanceSecrets" \
    "Generate a fresh instance's credentials into its Key Vault, and create the memql-secrets merge shell."

cap_spec_param_required "keyVaultName" "key vault the instance's credentials live in"
cap_spec_param "subscriptionId" "Azure subscription holding the vault (default: the active one)"
cap_spec_param "namespace"      "Kubernetes namespace to create and seed the merge shell in (default memql)"
cap_spec_param "dbHost"         "database host the DSN points at (default memql-db-rw.<namespace>.svc.cluster.local -- the CNPG read-write service)"
cap_spec_param "dbName"         "database name (default memql; must match the CNPG Cluster's bootstrap.initdb.database)"
cap_spec_param "dbUser"         "database owner (default memql; must match bootstrap.initdb.owner)"
cap_spec_param "dbSslMode"      "sslmode for the DSN (default require)"
cap_spec_param "skipCluster"    "seed the vault only; create no namespace, merge shell or CNPG credential"
cap_spec_param "dryRun"         "plan only; generate and write nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

#=============================================================================
# CONFIGURATION
#=============================================================================

KEY_VAULT_NAME="$(cap_param keyVaultName "")"
SUBSCRIPTION_ID="$(cap_param subscriptionId "")"
NAMESPACE="$(cap_param namespace "memql")"
DB_HOST="$(cap_param dbHost "")"
DB_NAME="$(cap_param dbName "memql")"
DB_USER="$(cap_param dbUser "memql")"
DB_SSLMODE="$(cap_param dbSslMode "require")"
SKIP_CLUSTER="$(cap_param skipCluster "false")"
DRY_RUN="$(cap_param dryRun "false")"

: "${DB_HOST:=memql-db-rw.${NAMESPACE}.svc.cluster.local}"

readonly MERGE_SHELL_SECRET="memql-secrets"
readonly CNPG_BOOTSTRAP_SECRET="memql-db-app-creds"

# The seven vault entries the mesh reads. Listed rather than discovered: the
# failure worth catching is a new required credential arriving in the engine
# and nobody extending this list, and discovery would wave that through.
readonly VAULT_ENTRIES="memql-master-key memql-operator-key memql-node-bootstrap-token memql-identity-signing-key-b64 memql-identity-signing-key-created-at memory-nodes-database-dsn memory-nodes-database-direct-dsn"

WORK=""
CREATED=0
KEPT=0

#=============================================================================
# FUNCTIONS
#=============================================================================

# A mode-700 scratch directory is the ONLY place a plaintext value is ever
# written, and the trap fires on failure and on the cap_fail exits too, because
# cap_fail exits rather than returning.
function make_workdir() {
    WORK="$(mktemp -d)"
    chmod 700 "$WORK"
    trap 'shred_workdir' EXIT
}

function shred_workdir() {
    [[ -n "$WORK" && -d "$WORK" ]] || return 0
    if command -v shred &>/dev/null; then
        find "$WORK" -type f -exec shred -u {} + 2>/dev/null || true
    fi
    rm -rf "$WORK"
}

function check_prerequisites() {
    command -v az      &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"
    command -v openssl &>/dev/null || cap_fail 4 "openssl is not installed or not on PATH"
    if [[ "$SKIP_CLUSTER" != "true" ]]; then
        command -v kubectl &>/dev/null || cap_fail 4 "kubectl is required to create the merge shell but is not on PATH (pass --skipCluster=true to seed the vault only)"
    fi

    local active
    active="$(az account show --query id -o tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first"
    if [[ -n "$SUBSCRIPTION_ID" && "$active" != "$SUBSCRIPTION_ID" ]]; then
        az account set --subscription "$SUBSCRIPTION_ID" 2>/dev/null \
            || cap_fail 3 "cannot select subscription ${SUBSCRIPTION_ID}"
    fi

    az keyvault show --name "$KEY_VAULT_NAME" -o none 2>/dev/null \
        || cap_fail 4 "key vault ${KEY_VAULT_NAME} does not exist or is not readable by the signed-in identity -- provisioning creates it, so run provisionInstance first"
}

function validate_arguments() {
    [[ -n "$KEY_VAULT_NAME" ]] || cap_fail 2 "--keyVaultName is required"
    [[ "$DB_SSLMODE" =~ ^(disable|allow|prefer|require|verify-ca|verify-full)$ ]] \
        || cap_fail 2 "--dbSslMode ${DB_SSLMODE} is not a libpq sslmode"
    [[ "$DB_NAME" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || cap_fail 2 "--dbName ${DB_NAME} is not a valid identifier"
    [[ "$DB_USER" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || cap_fail 2 "--dbUser ${DB_USER} is not a valid identifier"
}

# vault_has <name> -- an entry exists AND is enabled. A soft-deleted entry
# reports absent here and then REFUSES the set with "already exists in a
# deleted state", which is why the create path reports that specifically.
function vault_has() {
    az keyvault secret show --vault-name "$KEY_VAULT_NAME" --name "$1" -o none 2>/dev/null
}

# vault_get <name> -- read one value into stdout. Used for exactly one thing:
# recovering the database password from an existing DSN so a re-run does not
# generate a second one. It is never logged and never leaves this process.
function vault_get() {
    az keyvault secret show --vault-name "$KEY_VAULT_NAME" --name "$1" \
        --query value -o tsv 2>/dev/null || true
}

# vault_put_file <name> <file> -- write a value that is already on disk.
# --file, never --value: a value on argv is readable by every other process on
# the runner for as long as the call takes.
function vault_put_file() {
    local name="$1" file="$2"
    az keyvault secret set --vault-name "$KEY_VAULT_NAME" --name "$name" \
        --file "$file" -o none 2>/dev/null \
        || cap_fail 5 "failed to write ${name} to ${KEY_VAULT_NAME} -- if the name exists in a soft-deleted state, recover or purge it first ('az keyvault secret recover'); the vault will not overwrite one"
}

# ensure_generated <name> <generator-command...> -- the create-if-absent core.
function ensure_generated() {
    local name="$1"; shift
    if vault_has "$name"; then
        cap_info "${name} already present; kept"
        KEPT=$((KEPT + 1))
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would generate ${name}"
        return 0
    fi
    cap_step "generating ${name}"
    local f="${WORK}/${name}"
    ( umask 077; "$@" > "$f" ) || cap_fail 5 "failed to generate a value for ${name}"
    vault_put_file "$name" "$f"
    CREATED=$((CREATED + 1))
    cap_changed
}

# ---- generators. Each prints ONE value to stdout with no trailing newline
#      concerns: az --file uploads the file verbatim, and a stray newline in a
#      key is a value that does not match the one the engine derived.

function gen_hex32()  { openssl rand -hex 32 | tr -d '\n'; }
function gen_hex24()  { openssl rand -hex 24 | tr -d '\n'; }
function gen_key_b64() { openssl rand 32 | base64 | tr -d '\n'; }
function gen_now_rfc3339() { date -u +"%Y-%m-%dT%H:%M:%SZ" | tr -d '\n'; }

# ---- the database password: ONE value, two consumers -------------------------

DB_PASSWORD=""

# resolve_db_password establishes the single password the DSN and the CNPG
# bootstrap credential both carry.
#
# Read-back-first is not an optimisation. If an instance has been running, its
# database already HAS a password, and generating a new one here produces a
# vault that disagrees with the live database -- a cluster that comes up
# healthy and cannot log in.
function resolve_db_password() {
    if vault_has "memory-nodes-database-dsn"; then
        local dsn creds
        dsn="$(vault_get "memory-nodes-database-dsn")"
        # postgres://user:pass@host:port/db?params -- the password is between
        # the first ':' after the scheme and the LAST '@' before the host.
        creds="${dsn#*://}"
        creds="${creds%%@*}"
        DB_PASSWORD="${creds#*:}"
        if [[ -z "$DB_PASSWORD" || "$DB_PASSWORD" == "$creds" ]]; then
            cap_fail 5 "memory-nodes-database-dsn exists but carries no password, so the CNPG credential cannot be made to agree with it. Resolve by hand rather than letting this script generate a second password: the running database already has one."
        fi
        cap_info "recovered the database password from the existing DSN; it will NOT be regenerated"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        DB_PASSWORD="DRY-RUN-PASSWORD-NOT-GENERATED"
        return 0
    fi
    # Hex, deliberately: it needs no percent-encoding in a URL, so the DSN it
    # lands in can be parsed back apart by the branch above without a decoder.
    DB_PASSWORD="$(gen_hex24)"
}

function seed_dsns() {
    local dsn="postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:5432/${DB_NAME}?sslmode=${DB_SSLMODE}"
    local name
    # Both entries name the SAME endpoint: there is no PgBouncer in this shape,
    # and the direct DSN exists so a pooled deployment can differ later without
    # every consumer changing.
    for name in memory-nodes-database-dsn memory-nodes-database-direct-dsn; do
        if vault_has "$name"; then
            cap_info "${name} already present; kept"
            KEPT=$((KEPT + 1))
            continue
        fi
        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would compose ${name} for ${DB_USER}@${DB_HOST}/${DB_NAME}"
            continue
        fi
        cap_step "composing ${name}"
        local f="${WORK}/${name}"
        ( umask 077; printf '%s' "$dsn" > "$f" )
        vault_put_file "$name" "$f"
        CREATED=$((CREATED + 1))
        cap_changed
    done
}

# ---- step 8: the namespace and the merge shell ------------------------------

function ensure_namespace() {
    if kubectl get namespace "$NAMESPACE" -o name &>/dev/null; then
        cap_info "namespace ${NAMESPACE} already exists"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would create namespace ${NAMESPACE}"
        return 0
    fi
    cap_step "creating namespace ${NAMESPACE}"
    kubectl create namespace "$NAMESPACE" >/dev/null \
        || cap_fail 5 "failed to create namespace ${NAMESPACE}"
    cap_changed
}

function ensure_merge_shell() {
    if kubectl get secret "$MERGE_SHELL_SECRET" -n "$NAMESPACE" -o name &>/dev/null; then
        cap_info "${MERGE_SHELL_SECRET} already exists; ExternalSecrets have a target to merge into"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would create the empty ${MERGE_SHELL_SECRET} shell in ${NAMESPACE}"
        return 0
    fi
    cap_step "creating the empty ${MERGE_SHELL_SECRET} shell in ${NAMESPACE}"
    # EMPTY on purpose. Its only job is to exist, so that two ExternalSecrets
    # with creationPolicy: Merge have a target. ESO fills it; nothing here puts
    # a value in it, which is also why this step is safe to re-run.
    kubectl create secret generic "$MERGE_SHELL_SECRET" -n "$NAMESPACE" >/dev/null \
        || cap_fail 5 "failed to create ${MERGE_SHELL_SECRET} in ${NAMESPACE}"
    cap_changed
}

function ensure_cnpg_credential() {
    if kubectl get secret "$CNPG_BOOTSTRAP_SECRET" -n "$NAMESPACE" -o name &>/dev/null; then
        cap_info "${CNPG_BOOTSTRAP_SECRET} already exists; kept (it is the password the live database was created with)"
        return 0
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would create ${CNPG_BOOTSTRAP_SECRET} carrying the same password as the DSN"
        return 0
    fi
    cap_step "creating ${CNPG_BOOTSTRAP_SECRET} from the SAME password as the DSN"
    local uf="${WORK}/cnpg-username" pf="${WORK}/cnpg-password"
    ( umask 077; printf '%s' "$DB_USER" > "$uf"; printf '%s' "$DB_PASSWORD" > "$pf" )
    # --from-file, not --from-literal: a literal is argv.
    kubectl create secret generic "$CNPG_BOOTSTRAP_SECRET" -n "$NAMESPACE" \
        --type=kubernetes.io/basic-auth \
        --from-file=username="$uf" --from-file=password="$pf" >/dev/null \
        || cap_fail 5 "failed to create ${CNPG_BOOTSTRAP_SECRET} in ${NAMESPACE}"
    cap_changed
}

function collect_result() {
    cap_result_set "keyVault"  "$KEY_VAULT_NAME"
    cap_result_set "namespace" "$NAMESPACE"
    cap_result_set "dryRun"    "$DRY_RUN"
    cap_result_set_raw "created" "$CREATED"
    cap_result_set_raw "kept"    "$KEPT"
    # NAMES, never values. There is no flag that changes this.
    cap_result_set "entries"   "$(printf '%s' "$VAULT_ENTRIES" | tr ' ' ',')"
    return 0
}

function main() {
    validate_arguments
    check_prerequisites
    make_workdir

    cap_info "seeding instance credentials into ${KEY_VAULT_NAME} (generate-if-absent; nothing is ever overwritten)"
    [[ "$DRY_RUN" == "true" ]] && cap_info "DRY RUN -- no value will be generated or written"

    ensure_generated "memql-master-key"                      gen_hex32
    # A SEPARATE value from the master key, deliberately (memql#3519): one
    # DECRYPTS, the other AUTHENTICATES a cluster-owner bearer over the network.
    ensure_generated "memql-operator-key"                    gen_hex32
    ensure_generated "memql-node-bootstrap-token"            gen_hex32
    # Every identity replica must derive the SAME Ed25519 key, kid and JWKS
    # from this one seed; divergent keysets fail roughly half of all auth.
    ensure_generated "memql-identity-signing-key-b64"        gen_key_b64
    ensure_generated "memql-identity-signing-key-created-at" gen_now_rfc3339

    resolve_db_password
    seed_dsns

    if [[ "$SKIP_CLUSTER" == "true" ]]; then
        cap_info "--skipCluster given; leaving the namespace, merge shell and CNPG credential alone"
    else
        ensure_namespace
        ensure_merge_shell
        ensure_cnpg_credential
    fi

    collect_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
