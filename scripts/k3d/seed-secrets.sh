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
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (kubectl/cluster/ns)
#
# Refs: #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.seedSecrets" "Seed the k8s Secrets that the local k3d overlay requires."
cap_spec_param "gate-voice-lane-only" "only re-run the voice-lane gate (scale voice/voice-agent per LiveKit config)"
cap_spec_param "namespace" "k8s namespace to seed into"
#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# Result accumulators.
SEEDED_COUNT=0
GENESIS_SOURCE="none"

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

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }
function error() { cap_error "$*"; }

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        cap_fail 4 "kubectl is required but not found. Install kubectl first."
    fi
    if ! kubectl cluster-info &> /dev/null; then
        cap_fail 4 "kubectl cannot reach the cluster. Run 'make up' first."
    fi
    # Ensure the namespace exists before creating secrets.
    if ! kubectl get namespace "$NAMESPACE" &> /dev/null; then
        cap_fail 4 "namespace $NAMESPACE does not exist. Run 'make up' first."
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
    # it -- see deploy/k8s/base/*.yaml (secretName: memql-ca); the cloud
    # equivalent is seeded by the downstream product pack's deploy path.
    # Without these two
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
    NAMESPACE="$NAMESPACE" bash "$gen" >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
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
        | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
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
        | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
    info "memql-secrets seeded."
}

#=============================================================================
# LIVEKIT SECRETS
#=============================================================================

# gate_voice_lane scales the voice lane to match the LiveKit configuration
# (memql#2416): the local dev loop uses a LiveKit Cloud project (Epic #2184;
# no self-hosted livekit locally), and the voice / voice-agent binaries
# FAIL-FAST on the missing env (Epic 7 -- by design). Running them without
# credentials is therefore a guaranteed crash-loop, which read as a broken
# deploy at the D4 first live deploy. Without creds the lane is disabled
# LOUDLY (replicas=0 + a warn naming the re-enable path); with creds it is
# enabled. ArgoCD ignores /spec/replicas, so the scale sticks.
function gate_voice_lane() {
    local lk_url="${LIVEKIT_URL:-${MEMQL_POLYPHON_LIVEKIT_URL:-}}"
    local lk_key="${LIVEKIT_API_KEY:-${MEMQL_POLYPHON_LIVEKIT_API_KEY:-}}"
    local lk_secret="${LIVEKIT_API_SECRET:-${MEMQL_POLYPHON_LIVEKIT_API_SECRET:-}}"
    local replicas=1
    if [ -z "$lk_url" ] || [ -z "$lk_key" ] || [ -z "$lk_secret" ]; then
        replicas=0
    fi
    local scaled_any=""
    for d in voice voice-agent; do
        if kubectl get deploy "$d" -n "$NAMESPACE" &>/dev/null; then
            kubectl scale deploy "$d" -n "$NAMESPACE" --replicas="$replicas" >&2 || true
            scaled_any=1
        fi
    done
    if [ -z "$scaled_any" ]; then
        info "voice lane: deployments not present yet; gating happens on the next 'make secrets' (or scale manually)."
        return 0
    fi
    if [ "$replicas" -eq 0 ]; then
        warn "voice lane DISABLED (replicas=0): no LiveKit Cloud credentials in the environment."
        warn "  To enable: export LIVEKIT_URL/LIVEKIT_API_KEY/LIVEKIT_API_SECRET, then 'make secrets'."
    else
        info "voice lane enabled (LiveKit Cloud credentials present)."
    fi
}

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
        | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
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
        | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
    info "telephony-secrets seeded (stub)."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    NAMESPACE="$(cap_param namespace "$NAMESPACE")"
    cap_require namespace "$NAMESPACE"

    # --gate-voice-lane-only: re-run just the voice-lane gate (memql#2416).
    # up.sh calls this AFTER the ArgoCD app has created the Deployments,
    # since the full seeding pass runs before they exist.
    if [ -n "$(cap_flag gate-voice-lane-only)" ]; then
        gate_voice_lane
        cap_result_set_raw voiceLaneGated true
        cap_ok
    fi

    # Genesis source for the result envelope (computed here -- the resolver
    # runs in a $(...) subshell and cannot mutate this global).
    if [ -n "${MEMQL_GENESIS_B64:-}" ]; then
        GENESIS_SOURCE="envelope"
    elif [ -f "${MEMQL_GENESIS_FILE:-$HOME/.memql/genesis.znas}" ]; then
        GENESIS_SOURCE="file"
    else
        GENESIS_SOURCE="none"
    fi

    check_prerequisites
    seed_internal_ca
    seed_db_creds
    seed_memql_secrets
    seed_livekit_secrets
    gate_voice_lane
    seed_telephony_secrets

    info "All local secrets seeded. The k3d cluster can now start the memQL stack."
    info "ArgoCD reconciles automatically; check: kubectl get app memql-local -n argocd -w"

    cap_changed
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw secretsSeeded "$SEEDED_COUNT"
    cap_result_set     source "$GENESIS_SOURCE"
    cap_ok
}

main "$@"
