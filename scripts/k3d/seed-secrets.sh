#!/usr/bin/env bash
#
# scripts/k3d/seed-secrets.sh
# ============================
#
# Seed the k8s Secrets that the local k3d overlay requires.
# Replaces the cloud paths that staging/prod use (ESO + Azure Key Vault):
#
#   identity-tls           -- identity server TLS cert (self-signed cluster CA)
#   memql-ca               -- the cluster CA cert, mounted on every node
#   memql-secrets          -- main app envelope (MEMQL_MASTER_KEY,
#                             MEMQL_GENESIS_B64, DATABASE_DSN, ...)
#   livekit-secrets        -- LiveKit API key + secret for local livekit
#   memql-local-db-creds   -- Postgres credentials for the in-cluster DB
#
# Called by `make secrets` and by `make up` on first boot.
# Safe to re-run: uses `kubectl apply` (idempotent, creates or updates).
#
# PREREQUISITES
#   - kubectl context points at the k3d cluster (k3d-memql or equivalent).
#   - The memql namespace already exists (created by ArgoCD / `make up`).
#   - ~/.memql/genesis.znas exists (the sealed genesis envelope) -- OR
#     MEMQL_GENESIS_B64 is already in the environment.
#   - MEMQL_MASTER_KEY is in the environment (or falls back to a dev default).
#
# The Azurite connection string is always the well-known Azurite dev constant
# (account: devstoreaccount1, key: Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq
# /K1SZFPTOtr/KBHBeksoGMGw==).  It is not secret but lives in memql-secrets
# so the blob integration reads it via the existing genesis envelope path.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# Azurite well-known dev account + key (not secret; standard Azurite default).
AZURITE_ACCOUNT="devstoreaccount1"
AZURITE_KEY="Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
AZURITE_CONN="DefaultEndpointsProtocol=http;AccountName=${AZURITE_ACCOUNT};AccountKey=${AZURITE_KEY};BlobEndpoint=http://azurite:10000/${AZURITE_ACCOUNT};"

# Default dev DB credentials (override via env for security-conscious setups).
LOCAL_DB_USER="${MEMQL_LOCAL_DB_USER:-memql}"
LOCAL_DB_PASSWORD="${MEMQL_LOCAL_DB_PASSWORD:-memql_dev}"
LOCAL_DB_NAME="${MEMQL_LOCAL_DB_NAME:-memql}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }
function error() { echo "ERROR: $*" >&2; }

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        error "kubectl is required but not found. Install kubectl first."
        exit 1
    fi
    if ! kubectl cluster-info &> /dev/null; then
        error "kubectl cannot reach the cluster. Run 'make up' first."
        exit 1
    fi
    # Ensure the namespace exists before creating secrets.
    if ! kubectl get namespace "$NAMESPACE" &> /dev/null; then
        error "namespace $NAMESPACE does not exist. Run 'make up' first."
        exit 1
    fi
}

#=============================================================================
# RESOLVE GENESIS B64
#=============================================================================

function resolve_genesis_b64() {
    if [ -n "${MEMQL_GENESIS_B64:-}" ]; then
        echo "$MEMQL_GENESIS_B64"
        return
    fi
    local genesis_file="${MEMQL_GENESIS_FILE:-$HOME/.memql/genesis.znas}"
    if [ -f "$genesis_file" ]; then
        base64 < "$genesis_file"
        return
    fi
    warn "no genesis file at $genesis_file and MEMQL_GENESIS_B64 is unset"
    warn "memql-secrets will be seeded WITHOUT the genesis envelope."
    warn "identity will fail to start without MEMQL_GENESIS_B64."
    echo ""
}

#=============================================================================
# RESOLVE MASTER KEY
#=============================================================================

function resolve_master_key() {
    if [ -n "${MEMQL_MASTER_KEY:-}" ]; then
        echo "$MEMQL_MASTER_KEY"
        return
    fi
    warn "MEMQL_MASTER_KEY is unset; using insecure dev placeholder."
    warn "Override by setting MEMQL_MASTER_KEY in your environment."
    echo "local-dev-placeholder-not-for-production"
}

#=============================================================================
# INTERNAL TLS CA (identity-tls + memql-ca)
#=============================================================================

function seed_internal_ca() {
    # The identity node serves its HTTP surface over TLS (the node-bootstrap
    # handler rejects plaintext) and every other node mounts the CA to trust
    # it -- see deploy/k8s/base/*.yaml (secretName: memql-ca) and the cloud
    # equivalent in scripts/deploy/aks-deploy.sh step 2a. Without these two
    # secrets every node that mounts memql-ca stalls in ContainerCreating with
    # a FailedMount, so the local bootstrap must generate them too. The
    # generator is idempotent (kubectl apply), so re-running make up / make
    # k3d-secrets is safe.
    local gen="${REPO_ROOT}/deploy/k8s/base/tls/gen-internal-ca.sh"
    if [ ! -f "$gen" ]; then
        warn "internal CA generator not found at $gen; skipping."
        warn "  identity TLS + memql-ca will be missing; nodes will FailedMount."
        return
    fi
    # Already present? Leave them be (preserves a manually-rotated CA).
    if kubectl get secret memql-ca identity-tls -n "$NAMESPACE" &>/dev/null; then
        info "internal CA already present (memql-ca + identity-tls); skipping."
        return
    fi
    info "seeding internal TLS CA (identity-tls + memql-ca)..."
    NAMESPACE="$NAMESPACE" bash "$gen"
}

#=============================================================================
# LOCAL DB CREDENTIALS SECRET
#=============================================================================

function seed_db_creds() {
    info "seeding memql-local-db-creds (Postgres credentials for in-cluster DB)..."
    kubectl create secret generic memql-local-db-creds \
        --namespace="$NAMESPACE" \
        --from-literal="POSTGRES_USER=$LOCAL_DB_USER" \
        --from-literal="POSTGRES_PASSWORD=$LOCAL_DB_PASSWORD" \
        --from-literal="POSTGRES_DB=$LOCAL_DB_NAME" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "memql-local-db-creds seeded."
}

#=============================================================================
# MAIN MEMQL SECRET
#=============================================================================

function seed_memql_secrets() {
    local genesis_b64 master_key db_dsn db_direct_dsn
    genesis_b64="$(resolve_genesis_b64)"
    master_key="$(resolve_master_key)"
    # Database DSN: local in-cluster Postgres.
    # The connection string uses 'disable' sslmode because the local Postgres
    # container does not have TLS configured (dev only).
    db_dsn="postgres://${LOCAL_DB_USER}:${LOCAL_DB_PASSWORD}@postgres:5432/${LOCAL_DB_NAME}?sslmode=disable"
    # For local, the direct DSN is the same as the pooler DSN (no PgBouncer).
    db_direct_dsn="$db_dsn"

    info "seeding memql-secrets (main app envelope)..."
    kubectl create secret generic memql-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="MEMQL_MASTER_KEY=$master_key" \
        --from-literal="MEMQL_GENESIS_B64=$genesis_b64" \
        --from-literal="MEMQL_DATABASE_DSN=$db_dsn" \
        --from-literal="MEMORY_NODES_DATABASE_DIRECT_DSN=$db_direct_dsn" \
        --from-literal="AZURE_BLOB_CONNECTION_STRING=$AZURITE_CONN" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "memql-secrets seeded."
}

#=============================================================================
# LIVEKIT SECRETS
#=============================================================================

function seed_livekit_secrets() {
    # LOCAL DEV -> LIVEKIT CLOUD (Epic #2184 / #2186).
    #
    # The local dev loop uses a LiveKit Cloud project as the SIP + WebRTC
    # media plane (no self-hosted livekit-server / livekit/sip locally; the
    # local overlay removes those workloads). So the API key/secret AND the
    # URL must point at the operator's LiveKit Cloud project, sourced from the
    # environment -- NEVER hard-coded. Staging/prod stay self-hosted and pull
    # these from ESO/Key Vault instead (the no-cloud-leak guard,
    # scripts/deploy/livekit_cloud_guard_test.go, keeps cloud out of those
    # overlays).
    #
    # Both credential pairs must point at the SAME cloud project (verified on
    # main): the voice-agent reads the bare LIVEKIT_* names; telephony + the
    # voice/bff token-minters read the MEMQL_POLYPHON_LIVEKIT_* names.
    local lk_url="${LIVEKIT_URL:-${MEMQL_POLYPHON_LIVEKIT_URL:-}}"
    local lk_public_url="${MEMQL_POLYPHON_LIVEKIT_PUBLIC_URL:-$lk_url}"
    local lk_key="${LIVEKIT_API_KEY:-${MEMQL_POLYPHON_LIVEKIT_API_KEY:-}}"
    local lk_secret="${LIVEKIT_API_SECRET:-${MEMQL_POLYPHON_LIVEKIT_API_SECRET:-}}"

    if [ -z "$lk_url" ] || [ -z "$lk_key" ] || [ -z "$lk_secret" ]; then
        warn "LiveKit Cloud project not fully configured for local dev."
        warn "  voice + telephony need a LiveKit Cloud project. Set before 'make up':"
        warn "    export LIVEKIT_URL=wss://<your-project>.livekit.cloud"
        warn "    export LIVEKIT_API_KEY=<cloud-api-key>"
        warn "    export LIVEKIT_API_SECRET=<cloud-api-secret>"
        warn "  Seeding livekit-secrets with whatever is set; voice/telephony"
        warn "  pods stay degraded (LiveKit not configured) until provided."
    fi

    info "seeding livekit-secrets (LiveKit Cloud credentials for local dev)..."
    kubectl create secret generic livekit-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="MEMQL_POLYPHON_LIVEKIT_URL=$lk_url" \
        --from-literal="MEMQL_POLYPHON_LIVEKIT_PUBLIC_URL=$lk_public_url" \
        --from-literal="MEMQL_POLYPHON_LIVEKIT_API_KEY=$lk_key" \
        --from-literal="MEMQL_POLYPHON_LIVEKIT_API_SECRET=$lk_secret" \
        --from-literal="LIVEKIT_URL=$lk_url" \
        --from-literal="LIVEKIT_API_KEY=$lk_key" \
        --from-literal="LIVEKIT_API_SECRET=$lk_secret" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "livekit-secrets seeded."
}

#=============================================================================
# TELEPHONY SECRETS (local stub -- telephony disabled locally)
#=============================================================================

function seed_telephony_secrets() {
    # Telephony (Telnyx) is not used locally. Create a stub secret so pods
    # that mount telephony-secrets (livekit-sip via externalsecret ref) don't
    # crash on missing secret -- even though the ExternalSecret itself is
    # deleted in the local overlay (#2064), the livekit-sip Deployment
    # references the Secret directly.
    info "seeding telephony-secrets (stub -- telephony disabled locally)..."
    kubectl create secret generic telephony-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="TELNYX_API_KEY=disabled" \
        --from-literal="TELNYX_CONNECTION_ID=disabled" \
        --from-literal="TELNYX_OUTBOUND_PROFILE_ID=disabled" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "telephony-secrets seeded (stub)."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Seed k8s Secrets for the local k3d overlay. Safe to re-run (idempotent).

Options:
    --namespace=NS       k8s namespace to seed into (default: $NAMESPACE)
    --help               Show this help.

Environment:
    MEMQL_GENESIS_B64      Base64-encoded sealed genesis envelope (preferred).
    MEMQL_GENESIS_FILE     Path to genesis file (default: ~/.memql/genesis.znas).
    MEMQL_MASTER_KEY       Master key for genesis decryption.
    MEMQL_LOCAL_DB_USER    Postgres user (default: memql).
    MEMQL_LOCAL_DB_PASSWORD Postgres password (default: memql_dev).
    LIVEKIT_URL            LiveKit Cloud project URL for local dev
                           (wss://<project>.livekit.cloud). Required for
                           local voice/telephony (Epic #2184 / #2186).
    LIVEKIT_API_KEY        LiveKit Cloud API key.
    LIVEKIT_API_SECRET     LiveKit Cloud API secret.
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --namespace=*) NAMESPACE="${1#*=}"; shift ;;
            --help)        show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    seed_internal_ca
    seed_db_creds
    seed_memql_secrets
    seed_livekit_secrets
    seed_telephony_secrets
    echo ""
    info "All local secrets seeded. The k3d cluster can now start the memQL stack."
    info "ArgoCD reconciles automatically; check: kubectl get app memql-local -n argocd -w"
}

main "$@"
