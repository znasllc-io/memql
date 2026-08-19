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
#   memql-front-door-tls   -- front-door TLS (the mkcert *.memql.localhost pair)
#   memql-secrets          -- THE config delivery path (MEMQL_MASTER_KEY,
#                             MEMQL_IDENTITY_SIGNING_KEY_B64,
#                             MEMQL_NODE_BOOTSTRAP_TOKEN, DATABASE_DSN, ...)
#   livekit-secrets        -- LiveKit API key + secret for local livekit
#   memql-db-app-creds   -- Postgres credentials for the in-cluster DB
#
# Called by `make secrets` and by `make up` on first boot.
# Safe to re-run: uses `kubectl apply` (idempotent, creates or updates).
#
# PREREQUISITES
#   - kubectl context points at the k3d cluster (k3d-memql or equivalent).
#   - The memql namespace already exists (created by ArgoCD / `make up`).
#   - MEMQL_MASTER_KEY is in the environment. When unset, the key already in
#     memql-secrets is REUSED if valid; only a cluster with no usable key
#     falls back to the dev default. A re-run therefore never replaces a
#     working key with a placeholder (memql#2958).
#   - mkcert is installed AND has a root CA on this machine. The front-door
#     pair is ISSUED here when absent (memql#3384) rather than skipped; see
#     seed_front_door_tls below for why skipping was never survivable. An
#     existing pair is reused only when it COVERS the domain being served, and
#     reissued when it demonstrably does not (memql#3730) -- that decision
#     belongs to install.mkcert, and this script no longer pre-empts it.
#   - MEMQL_IDENTITY_SIGNING_KEY_B64 (the shared Ed25519 signing seed every
#     identity replica derives its key + kid + JWKS from) is GENERATED here
#     when the cluster has none (memql#3400), and REUSED verbatim thereafter --
#     a re-run must never rotate it, because rotation invalidates every live
#     session and every minted mesh node token. See resolve_signing_key.
#   - MEMQL_NODE_BOOTSTRAP_TOKEN (the shared secret identity mints class="node"
#     JWTs against, and every leaf node presents to get one) is likewise
#     GENERATED when the cluster has none and REUSED thereafter (memql#3784).
#     Without it the cross-node event mesh never forms at all. See
#     resolve_node_bootstrap_token.
#
# The Azurite connection string is always the well-known Azurite dev constant
# (account: devstoreaccount1, key: Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq
# /K1SZFPTOtr/KBHBeksoGMGw==).  It is not secret but lives in memql-secrets
# so the blob integration reads it through envFrom like every other value.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param
#             | 4 prerequisite missing (kubectl/cluster/ns; mkcert or its CA)
#             | 5 op failed (unreadable existing secret; cert issuance failed;
#                            an issuer envelope this script cannot read)
#
# Refs: #2061 #2221 #3384 #3730

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "k3d.seedSecrets" "Seed the k8s Secrets that the local k3d overlay requires."
cap_spec_param "gate-voice-lane-only" "only re-run the voice-lane gate (scale voice/voice-agent per LiveKit config)"
cap_spec_param "namespace" "k8s namespace to seed into"
cap_spec_param "domain"    "front-door apex; seeded as the memql-domain ConfigMap and derives the certificate SANs (default: the domain this cluster already serves, else memql.localhost)"
cap_spec_param "tls-cert"  "front-door TLS certificate path (issued with mkcert when absent)"
cap_spec_param "tls-key"   "front-door TLS private key path (issued with mkcert when absent)"
cap_spec_param "mkcert"    "path to the mkcert binary used to issue the front-door pair"
cap_spec_param "caroot"    "mkcert CAROOT the front-door pair is issued from, passed through to install.mkcert (default: whatever mkcert reports)"
cap_spec_param "allow-missing-certutil" "proceed without browser (NSS) trust on Linux, passed through to install.mkcert (flag)"
cap_spec_param "mkcert-setup" "path to the install.mkcert capability that decides reuse vs reissue (default: alongside this script)"
cap_spec_param "repo-root"      "the memQL checkout to read deploy/ from (default: this script's own repository)"
#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# Result accumulators.
SEEDED_COUNT=0

# Front-door TLS: resolved in main() from --tls-cert / --tls-key / --mkcert,
# whose defaults come from the shared local-TLS locations (scripts/lib) with an
# MEMQL_LOCAL_TLS_* environment override.
TLS_CERT=""
TLS_KEY=""
MKCERT_BIN=""
# WHICH CERTIFICATE AUTHORITY the front-door pair is signed by (memql#4069).
# Empty is the default and means "whatever install.mkcert resolves from
# `mkcert -CAROOT`" -- which is what `make up` / `make secrets` want and what
# every run did before this param existed. The INSTALLER passes a value, because
# it does not use mkcert's default root: it pins ~/.memql/mkcert so a
# snap-packaged editor cannot strand the CA in a revision-scoped XDG directory
# that moves on the next refresh (memql#3576). See ensure_front_door_pair for
# what happened while this did not travel.
MKCERT_CAROOT=""
ALLOW_MISSING_CERTUTIL=""
TLS_CERT_SOURCE="none"   # existing | issued | reissued
# Did anything actually READ the resulting certificate's names? Reported by the
# issuer, echoed in this envelope, and never assumed -- "we could not check" and
# "we checked and it covers" are different facts (memql#3730).
FRONT_DOOR_COVERAGE_VERIFIED="false"
# The SANs the front-door pair must carry, derived in main() from the RESOLVED
# domain (the same value seeded into the memql-domain ConfigMap). Seeded here
# with the environment-resolved names so the variable is never unset.
FRONT_DOOR_HOSTNAMES="$MEMQL_LOCAL_TLS_HOSTNAMES"
# The install.mkcert capability that owns the reuse-vs-reissue decision.
# Resolved in main(); a param purely so a test can hand this script a fabricated
# result envelope (see the --mkcert-setup declaration).
MKCERT_SETUP=""

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
# READING WHAT THE CLUSTER ALREADY HAS
#=============================================================================
#
# memql#2958: this script is documented idempotent and `make up` calls it, so
# every field it writes UNCONDITIONALLY is a field an ordinary re-run destroys.
# The incident was the master key; the identity signing seed and the node
# bootstrap token sit in the same write and have exactly the same shape. All
# three read the live value back and preserve it when the environment supplies
# nothing.
#
# (MEMQL_GENESIS_B64 was a fourth, and is gone with the envelope -- epic
# memql#3958. The guard's shape outlived it because the shape was never about
# that key.)
#
# The reads below are deliberately fail-CLOSED. A guard whose only job is "do
# not destroy something irreplaceable" must never map "I could not tell" onto
# "there is nothing there" -- that is the original bug reached by a different
# route, and it would print a false explanation while doing it.

# BSD/macOS base64 spells decode -D; GNU spells it -d/--decode. macOS is this
# project's stated dev platform (docs/public/overview/tech-stack.md), so
# probing rather than assuming is load-bearing: guessing wrong makes every read
# below look like "no value", which is the fail-open this section exists to
# prevent.
function b64_decode() {
    if printf '' | base64 --decode >/dev/null 2>&1; then
        base64 --decode
    else
        base64 -D
    fi
}

# Trim leading/trailing whitespace only, matching strings.TrimSpace -- which is
# what the runtime applies before validating (component/secret/encryption.go).
# A key stored with a stray newline IS usable by every node, so judging it
# garbage and replacing it would destroy a working key. Interior whitespace is
# left alone so it still fails validation rather than being silently repaired.
function trim_space() {
    local s="$1"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "$s"
}

# Prints: absent | present | error:<detail>
#
# "absent" (NotFound) is the ordinary first-boot case and is NOT an error.
# Anything else -- API timeout, RBAC denial, unreachable control plane -- is,
# and the caller must refuse to seed rather than assume an empty cluster.
function memql_secrets_state() {
    local err
    if err="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" -o name 2>&1 >/dev/null)"; then
        printf 'present'
        return 0
    fi
    case "$err" in
        *NotFound*|*"not found"*|*"NotFound"*) printf 'absent' ;;
        *) printf 'error:%s' "$(printf '%s' "$err" | tr '\n' ' ')" ;;
    esac
}

# Populated by load_cluster_secret_snapshot. Globals because the loader must
# run in the MAIN shell so cap_fail can reach the real stdout.
CLUSTER_SECRET_STATE=""
CLUSTER_MASTER_KEY=""
CLUSTER_SIGNING_KEY_B64=""
CLUSTER_SIGNING_KEY_CREATED_AT=""
CLUSTER_NODE_BOOTSTRAP_TOKEN=""

function load_cluster_secret_snapshot() {
    local state
    state="$(memql_secrets_state)"
    case "$state" in
        error:*)
            cap_fail 5 "cannot read the existing memql-secrets to check what this run would overwrite: ${state#error:}. Refusing to seed: if the cluster holds a working MEMQL_MASTER_KEY or identity signing seed, writing over it blind is how #2958 bricked a shared cluster. Fix cluster access and re-run, or export MEMQL_MASTER_KEY to seed deliberately."
            ;;
        absent)
            CLUSTER_SECRET_STATE="absent"
            return
            ;;
    esac
    CLUSTER_SECRET_STATE="present"

    local raw
    raw="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" \
              -o 'jsonpath={.data.MEMQL_MASTER_KEY}' 2>/dev/null)" \
        || cap_fail 5 "memql-secrets exists but its MEMQL_MASTER_KEY could not be read; refusing to overwrite it blind."
    if [ -n "$raw" ]; then
        CLUSTER_MASTER_KEY="$(trim_space "$(printf '%s' "$raw" | b64_decode 2>/dev/null || true)")"
        [ -n "$CLUSTER_MASTER_KEY" ] \
            || cap_fail 5 "memql-secrets holds a MEMQL_MASTER_KEY that could not be base64-decoded; refusing to overwrite it blind."
    fi

    # The identity signing seed is the third irreplaceable field in this
    # Secret (memql#3400) and gets the same fail-closed read: rotating it
    # invalidates every live session and every minted mesh node token, so
    # "I could not tell what is there" must never become "there is nothing
    # there".
    raw="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" \
              -o 'jsonpath={.data.MEMQL_IDENTITY_SIGNING_KEY_B64}' 2>/dev/null)" \
        || cap_fail 5 "memql-secrets exists but its MEMQL_IDENTITY_SIGNING_KEY_B64 could not be read; refusing to overwrite it blind."
    if [ -n "$raw" ]; then
        CLUSTER_SIGNING_KEY_B64="$(trim_space "$(printf '%s' "$raw" | b64_decode 2>/dev/null || true)")"
        [ -n "$CLUSTER_SIGNING_KEY_B64" ] \
            || cap_fail 5 "memql-secrets holds a MEMQL_IDENTITY_SIGNING_KEY_B64 that could not be base64-decoded; refusing to overwrite it blind."
    fi

    # The seed's mint date (memql#3381). Not irreplaceable -- it is a
    # timestamp, not key material -- so an unreadable value is not fatal; it
    # just means this run re-stamps. But a REUSED seed must keep its original
    # date, or every `make secrets` would silently reset the key's apparent
    # age to today, which is the exact false signal the metric exists to
    # avoid.
    raw="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" \
              -o 'jsonpath={.data.MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT}' 2>/dev/null || true)"
    if [ -n "$raw" ]; then
        CLUSTER_SIGNING_KEY_CREATED_AT="$(trim_space "$(printf '%s' "$raw" | b64_decode 2>/dev/null || true)")"
    fi

    # The mesh bootstrap secret (memql#3784) gets the same fail-closed read as
    # the three fields above, for a reason specific to how it is consumed:
    # container environment is read ONCE, at container start. So minting a new
    # token over a live one does not "rotate" anything gracefully -- it leaves
    # every running pod presenting a secret identity no longer accepts, and the
    # mesh stays down until something restarts all of them. `make secrets` does
    # not restart pods.
    #
    # "I could not read it" must therefore not become "there is nothing there":
    # that is memql#2958's lesson applied to the one field whose absence is
    # indistinguishable, from here, from a value we simply failed to fetch.
    raw="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" \
              -o 'jsonpath={.data.MEMQL_NODE_BOOTSTRAP_TOKEN}' 2>/dev/null)" \
        || cap_fail 5 "memql-secrets exists but its MEMQL_NODE_BOOTSTRAP_TOKEN could not be read; refusing to overwrite it blind. Minting a fresh token over the one the running mesh is using breaks every peer connection until all pods restart."
    if [ -n "$raw" ]; then
        CLUSTER_NODE_BOOTSTRAP_TOKEN="$(trim_space "$(printf '%s' "$raw" | b64_decode 2>/dev/null || true)")"
        [ -n "$CLUSTER_NODE_BOOTSTRAP_TOKEN" ] \
            || cap_fail 5 "memql-secrets holds a MEMQL_NODE_BOOTSTRAP_TOKEN that could not be base64-decoded; refusing to overwrite it blind."
    fi
}

#=============================================================================
# RESOLVE MASTER KEY
#=============================================================================

# The runtime requires EXACTLY 64 hex characters (32 bytes): see
# component/secret/encryption.go masterKey(). A value that misses it is
# not a weak key, it is an unusable one -- every node that reads it dies at
# boot with "MEMQL_MASTER_KEY is not valid hex".
readonly MASTER_KEY_HEX_CHARS=64

# Last-resort key for a cluster that has no usable one. Valid hex, so a
# from-nothing bring-up actually boots instead of crash-looping.
#
# FIXED rather than random, and the honest reason is reproducibility of the
# VALUE, not of any data: `make up-refresh` nukes the cluster and wipes the
# in-cluster Postgres by construction (scripts/k3d/bringup.sh), so there is no
# prior data to strand either way. A constant means two operators comparing a
# broken local cluster are looking at the same key, and a wrong-key symptom is
# recognisable on sight.
#
# TREAT ANY CLUSTER RUNNING THIS AS FULLY OPEN. It is not only an encryption
# key: component/grpc/operator_stream_interceptor.go admits it verbatim as an
# "Authorization: Operator" synthetic CLUSTER OWNER, bypassing per-row authz.
# It is printed in a public repository. Local dev only, never anywhere reachable.
#
# It cannot decrypt anything sealed under a real key -- stored secrets included.
# That is why the reuse branch below exists and runs FIRST.
readonly DEV_MASTER_KEY="deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

# Outputs of resolve_master_key. Globals rather than stdout because these
# resolvers can cap_fail, and a failure must be attributable.
#
# Being precise about what a $(...) would actually cost, because an earlier
# version of this comment overstated it: capability.sh installs an exit trap,
# so even from inside a subshell an envelope DOES reach real stdout and the
# exit code is right -- and `set -e` aborts the assignment, so no empty key is
# ever seeded. What is lost is the specific cap_fail MESSAGE, replaced by the
# trap's generic "aborted without an explicit result". That is reason enough
# (a key rejected for being 63 chars should say so), but it is a diagnostics
# argument, not a correctness one.
RESOLVED_MASTER_KEY=""
RESOLVED_MASTER_KEY_SOURCE=""

function is_valid_master_key() {
    local candidate="$1"
    [ "${#candidate}" -eq "$MASTER_KEY_HEX_CHARS" ] || return 1
    [[ "$candidate" =~ ^[0-9a-fA-F]+$ ]] || return 1
    return 0
}

# Resolve the master key to seed, in strict precedence: environment, then the
# key already in the cluster, then the dev default.
#
# The middle branch is the one that matters (memql#2958). This script is
# documented idempotent and `make up` calls it, but it used to seed a
# non-hex placeholder whenever MEMQL_MASTER_KEY was unset -- so an ordinary
# re-run REPLACED a working key with one the runtime rejects, and took the
# whole mesh into CrashLoopBackOff, and every v1:platform:globalSecret row
# sealed under the replaced key stopped decrypting.
# Reading the live key first makes that re-run a genuine no-op.
#
# Requires load_cluster_secret_snapshot to have run.
function resolve_master_key() {
    if [ -n "${MEMQL_MASTER_KEY:-}" ]; then
        local from_env
        from_env="$(trim_space "$MEMQL_MASTER_KEY")"
        if ! is_valid_master_key "$from_env"; then
            cap_fail 2 "MEMQL_MASTER_KEY is set but is not ${MASTER_KEY_HEX_CHARS} hex characters (got ${#from_env} after trimming). The runtime rejects it at boot, so seeding it would take every node down. Generate one with: openssl rand -hex 32"
        fi
        # The only branch that can rotate a LIVE key. Shape validation cannot
        # tell a deliberate rotation from a stale value someone still has
        # exported, so say what is about to happen rather than doing it mutely.
        if [ -n "$CLUSTER_MASTER_KEY" ] && [ "$CLUSTER_MASTER_KEY" != "$from_env" ]; then
            warn "MEMQL_MASTER_KEY differs from the key currently in memql-secrets."
            warn "  This run ROTATES it. Anything sealed under the old key (stored"
            warn "  secrets) stops decrypting unless it is re-encrypted."
            warn "  Unset MEMQL_MASTER_KEY to keep the cluster's existing key instead."
        fi
        RESOLVED_MASTER_KEY="$from_env"
        RESOLVED_MASTER_KEY_SOURCE="env"
        return
    fi

    if is_valid_master_key "$CLUSTER_MASTER_KEY"; then
        warn "MEMQL_MASTER_KEY is unset; KEEPING the key already in memql-secrets."
        warn "  Nothing is overwritten -- this run does not change the master key."
        warn "  Export MEMQL_MASTER_KEY to rotate it deliberately."
        RESOLVED_MASTER_KEY="$CLUSTER_MASTER_KEY"
        RESOLVED_MASTER_KEY_SOURCE="cluster"
        return
    fi

    # Nothing usable anywhere. Replacing a stored key that is not 64 hex chars
    # loses nothing: the runtime rejects it, so it decrypts nothing either.
    # Note this is judged AFTER trim_space, so a well-formed key stored with a
    # stray newline reaches the branch above and is preserved.
    if [ -n "$CLUSTER_MASTER_KEY" ]; then
        warn "the MEMQL_MASTER_KEY already in memql-secrets is not valid hex; replacing it."
        warn "  (A key the runtime rejects cannot decrypt anything, so nothing is lost.)"
    fi
    warn "MEMQL_MASTER_KEY is unset and the cluster has no usable key; using the dev default."
    warn "  This key is PUBLIC and in the repository -- never use it outside local dev."
    warn "  It is also accepted as an 'Authorization: Operator' cluster-owner credential"
    warn "  (component/grpc/operator_stream_interceptor.go), so treat any cluster running"
    warn "  it as fully open to anyone who can reach it."
    RESOLVED_MASTER_KEY="$DEV_MASTER_KEY"
    RESOLVED_MASTER_KEY_SOURCE="dev-default"
}


#=============================================================================
# RESOLVE IDENTITY SIGNING SEED (memql#3400)
#=============================================================================

# WHY THIS EXISTS. deploy/k8s/base/identity.yaml runs identity at `replicas: 2`
# and says why it can: the seed arrives as a KEY on memql-secrets, so every pod
# derives the same key + kid + JWKS and there is NO single-writer key PVC.
# Nothing supplied that seed locally, so KeyManager.Load() fell through to
# generateAndWriteCurrent() and EVERY POD MINTED ITS OWN Ed25519 keypair. Two
# replicas behind one Service published two different `kid`s; a token minted by
# one is structurally unverifiable by any node that fetched JWKS from the other,
# so `make scale N=2` -- the documented multi-node command -- produced coin-flip
# auth failures. This is the local analogue of the ESO/Key Vault delivery the
# cloud uses, exactly as the master key and the front-door TLS pair already
# are: the SHAPE (one shared seed, delivered through memql-secrets, read by
# every replica via envFrom) is identical everywhere; only the VALUE is local.
#
# THE QUOTATION ABOVE WAS STALE AND IS NOW FIXED (memql#3960). It used to read
# "the signing key comes from the ENVELOPE (same seed on every pod -> identical
# JWKS)" -- accurate when written, and false since the envelope stopped being a
# delivery path for this seed in any environment. The cloud declares it in Key
# Vault (deploy/external-secrets/externalsecret-memql.yaml) exactly as this
# script writes it locally, which is the whole point: one shape, one path.
#
# The runtime requires base64-std of EXACTLY 32 bytes -- ed25519.SeedSize, see
# component/identity/keys.go NewKeyManagerFromSeed and the same rule in
# Config.Validate. 32 bytes encode to 43 base64 characters plus one '=' pad, so
# that shape check is complete: nothing else decodes to 32 bytes.
readonly SIGNING_KEY_B64_RE='^[A-Za-z0-9+/]{43}=$'

RESOLVED_SIGNING_KEY_B64=""
RESOLVED_SIGNING_KEY_SOURCE=""
RESOLVED_SIGNING_KEY_CREATED_AT=""

# resolve_signing_key_created_at stamps the seed with its mint date
# (memql#3381). A bare 32-byte seed carries no metadata, so this is the only
# way identity can report how old its signing key is -- and key age is the
# only automated pressure toward the manual rotation runbook that every
# deployed cluster depends on. Local seeds it too, so the "age is known" path
# is the one the parity cluster actually exercises.
#
# A REUSED seed keeps whatever date it already had; a new or replaced seed is
# stamped now. Getting that backwards would reset the key's apparent age on
# every `make secrets`.
function resolve_signing_key_created_at() {
    if [ "$RESOLVED_SIGNING_KEY_SOURCE" = "cluster" ] && [ -n "$CLUSTER_SIGNING_KEY_CREATED_AT" ]; then
        RESOLVED_SIGNING_KEY_CREATED_AT="$CLUSTER_SIGNING_KEY_CREATED_AT"
        return
    fi
    RESOLVED_SIGNING_KEY_CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}

function is_valid_signing_key() {
    [[ "$1" =~ $SIGNING_KEY_B64_RE ]]
}

# NEVER echoed. A seed printed to a terminal or a CI log is a seed that must be
# rotated, and rotation is the operation the reuse branch below exists to avoid.
function generate_signing_key() {
    head -c 32 /dev/urandom | base64 | tr -d '\n'
}

# Strict precedence: environment, then the seed already in the cluster, then a
# freshly generated one.
#
# The middle branch is the load-bearing one. `make secrets` runs on every
# `make up`, so a seed regenerated on each run would silently rotate the
# cluster's signing key -- invalidating every browser session and every minted
# class="node" mesh token, which reads to the operator as the very auth
# breakage this change fixes. Reuse makes a re-run a genuine no-op.
#
# Requires load_cluster_secret_snapshot to have run.
function resolve_signing_key() {
    if [ -n "${MEMQL_IDENTITY_SIGNING_KEY_B64:-}" ]; then
        local from_env
        from_env="$(trim_space "$MEMQL_IDENTITY_SIGNING_KEY_B64")"
        if ! is_valid_signing_key "$from_env"; then
            cap_fail 2 "MEMQL_IDENTITY_SIGNING_KEY_B64 is set but is not base64-std of 32 bytes (an Ed25519 seed; got ${#from_env} characters after trimming). identity REFUSES TO BOOT on a seed it cannot decode, so seeding it would take auth down cluster-wide. Generate one with: make identity-signing-key"
        fi
        if [ -n "$CLUSTER_SIGNING_KEY_B64" ] && [ "$CLUSTER_SIGNING_KEY_B64" != "$from_env" ]; then
            warn "MEMQL_IDENTITY_SIGNING_KEY_B64 differs from the seed currently in memql-secrets."
            warn "  This run ROTATES the identity signing key. Every live browser session and"
            warn "  every minted class=\"node\" mesh token stops verifying; sign-in again after"
            warn "  identity rolls. Unset MEMQL_IDENTITY_SIGNING_KEY_B64 to keep the existing seed."
        fi
        RESOLVED_SIGNING_KEY_B64="$from_env"
        RESOLVED_SIGNING_KEY_SOURCE="env"
        return
    fi

    if is_valid_signing_key "$CLUSTER_SIGNING_KEY_B64"; then
        RESOLVED_SIGNING_KEY_B64="$CLUSTER_SIGNING_KEY_B64"
        RESOLVED_SIGNING_KEY_SOURCE="cluster"
        return
    fi

    # Nothing usable. Replacing a stored value that is not a 32-byte seed
    # loses nothing: identity refuses to boot on it, so it has signed nothing.
    # Judged AFTER trim_space, so a good seed stored with a stray newline
    # reaches the reuse branch above instead.
    if [ -n "$CLUSTER_SIGNING_KEY_B64" ]; then
        warn "the MEMQL_IDENTITY_SIGNING_KEY_B64 already in memql-secrets is not a 32-byte Ed25519 seed; replacing it."
        warn "  (identity refuses to boot on it, so it has signed nothing and nothing is lost.)"
    fi
    RESOLVED_SIGNING_KEY_B64="$(generate_signing_key)"
    RESOLVED_SIGNING_KEY_SOURCE="generated"
    if ! is_valid_signing_key "$RESOLVED_SIGNING_KEY_B64"; then
        cap_fail 5 "generating an Ed25519 signing seed produced a value of the wrong shape; /dev/urandom or base64 is not behaving as expected."
    fi
    info "generated a shared identity signing seed (every identity replica will derive the same key + kid + JWKS)."
}

#=============================================================================
# RESOLVE NODE BOOTSTRAP TOKEN (memql#3784)
#=============================================================================

# WHY THIS EXISTS. Every leaf node authenticates its outbound NodeService.Stream
# with a class="node" JWT it mints from identity's /node/bootstrap, presenting
# this shared secret to prove it may. Nothing supplied that secret locally, so:
#
#   - identity read an empty MEMQL_NODE_BOOTSTRAP_TOKEN into
#     Cfg.NodeBootstrapToken and answered every mint request
#     `503 bootstrap_disabled`;
#   - every leaf node found nothing to mint WITH, so it dialled its peers with
#     no Authorization header and was refused `Unauthenticated` -- permanently,
#     on a 30s reconnect loop.
#
# The whole cross-node event mesh therefore never formed on a local cluster:
# component/node/routing.go's forward rules delivered nothing, and the
# cross-node invariant test/clustere2e exists to protect could not be exercised
# at all. A green single-node test was the only kind available, which is the
# false signal CLAUDE.md's "multi-node is the DEFAULT" section is about.
#
# This is the same shape as the identity signing seed (memql#3400) and gets the
# same treatment, because it is the same class of thing: a cluster-internal
# shared secret that staging and prod receive from ESO/Key Vault and local
# received from nothing. The SHAPE is identical everywhere -- one value,
# delivered through memql-secrets, read by every node via envFrom; only the
# VALUE is local. That is env parity, not a local special case.

# NEVER echoed. Anyone holding this value can mint a class="node" JWT for any
# node type in the mesh.
#
# Hex rather than base64 because it travels in an `Authorization: Bootstrap
# <token>` header: 64 hex characters raise no question about '+', '/' or '='
# surviving the trip, and the runtime imposes no format of its own.
function generate_node_bootstrap_token() {
    od -An -tx1 -N32 /dev/urandom | tr -d ' \n'
}

RESOLVED_NODE_BOOTSTRAP_TOKEN=""
RESOLVED_NODE_BOOTSTRAP_TOKEN_SOURCE=""

# Strict precedence: environment, then the token already in the cluster, then a
# freshly generated one -- the same order as the master key and the signing seed.
#
# NOTHING VALIDATES THE VALUE'S SHAPE, deliberately. identity compares the
# presented token against the configured one with subtle.ConstantTimeCompare
# over raw bytes (component/identity/http/node_bootstrap.go), so there is no
# format to conform to. A shape check here would invent a constraint the mesh
# does not have and would reject a perfectly good secret handed to an operator
# by Key Vault. Only emptiness is judged.
#
# Requires load_cluster_secret_snapshot to have run.
function resolve_node_bootstrap_token() {
    if [ -n "${MEMQL_NODE_BOOTSTRAP_TOKEN:-}" ]; then
        local from_env
        from_env="$(trim_space "$MEMQL_NODE_BOOTSTRAP_TOKEN")"
        if [ -n "$CLUSTER_NODE_BOOTSTRAP_TOKEN" ] && [ "$CLUSTER_NODE_BOOTSTRAP_TOKEN" != "$from_env" ]; then
            warn "MEMQL_NODE_BOOTSTRAP_TOKEN differs from the token currently in memql-secrets."
            warn "  This run ROTATES the mesh bootstrap secret. Pods read their environment at"
            warn "  start, so every peer connection stays refused until all nodes restart"
            warn "  ('make dev' rolls them). Unset MEMQL_NODE_BOOTSTRAP_TOKEN to keep the existing one."
        fi
        RESOLVED_NODE_BOOTSTRAP_TOKEN="$from_env"
        RESOLVED_NODE_BOOTSTRAP_TOKEN_SOURCE="env"
        return
    fi

    # The load-bearing branch, exactly as it is for the signing seed: `make
    # secrets` runs on every `make up`, so a token regenerated per run would
    # silently break a working mesh on a routine re-run.
    if [ -n "$CLUSTER_NODE_BOOTSTRAP_TOKEN" ]; then
        RESOLVED_NODE_BOOTSTRAP_TOKEN="$CLUSTER_NODE_BOOTSTRAP_TOKEN"
        RESOLVED_NODE_BOOTSTRAP_TOKEN_SOURCE="cluster"
        return
    fi

    RESOLVED_NODE_BOOTSTRAP_TOKEN="$(generate_node_bootstrap_token)"
    RESOLVED_NODE_BOOTSTRAP_TOKEN_SOURCE="generated"
    if [ -z "$RESOLVED_NODE_BOOTSTRAP_TOKEN" ]; then
        cap_fail 5 "generating a node bootstrap token produced an empty value; /dev/urandom or od is not behaving as expected."
    fi
    info "generated a mesh bootstrap secret (identity mints node tokens against it; every node presents it)."
}

#=============================================================================
# FRONT-DOOR TLS (memql-front-door-tls -- browser-trusted wildcard for the ingress)
#=============================================================================

# The local front door (traefik ingress on 443, see the local overlay's
# front-door manifests) terminates TLS with a browser-trusted wildcard cert for
# the operator's local domain -- exactly as the cloud ingress does, which is
# what makes the local connection model env-parity rather than a local special
# case (docs/public/operate/environment-parity.md).
#
# WHY THIS ISSUES RATHER THAN SKIPS (memql#3384). This step used to warn and
# return when the pair was absent. That was never a survivable degradation:
# both front-door ingresses NAME memql-front-door-tls, and traefik answers a missing
# referenced secret by silently serving its own "TRAEFIK DEFAULT CERT" for both
# hosts. Browsers show "Not secure" on the very link a first-time operator
# clicks (the setup magic link), and Node clients -- including the VS Code
# extension host -- fail outright with "unable to verify the first
# certificate". Meanwhile every Deployment is Available, so the bring-up ends
# in a green summary and the WARN scrolls past ~140 lines into a ~700-line run.
# A certificate this script can create for itself is not an operator task.
#
# Issuance is delegated to the install.mkcert capability
# (scripts/install/mkcert-setup.sh) rather than re-implemented here: it already
# owns the restraint that matters (it never touches a pre-existing per-machine
# CA) and there must not be a second way to mint this pair.
#
# AND NOT A SECOND WAY TO DECIDE WHETHER TO MINT, EITHER (memql#3730). The line
# above was written about minting and this function then short-circuited on
# `[ -s "$TLS_CERT" ]` -- existence alone -- and returned before install.mkcert
# was ever invoked. That is the same defect one word over: the decision moved up
# here while the knowledge stayed down there. install.mkcert's cert_mismatches()
# already asked the question that matters (does this pair cover the names about
# to be served?), already cited the domain rename that creates stale pairs, and
# was UNREACHABLE from the only caller that runs on every `make up`. A machine
# that ran the local stack before memql#3593 therefore served a valid mkcert
# certificate for a domain that no longer exists, traefik fell back to its own
# TRAEFIK DEFAULT CERT for a name it had no certificate for, and this script
# reported ok:true / frontDoorTlsSource:existing over it.
#
# So the whole decision is delegated: install.mkcert without --force reuses a
# pair that covers the hostnames and reissues one that demonstrably does not.
# There is nothing to re-implement here -- re-implementing the SAN check in this
# file is precisely the second way to decide that the paragraph above forbids.
#
# Idempotent: a pair that covers the hostnames is REUSED verbatim, never
# reissued -- `make secrets` runs on every `make up`, and rotating the
# front-door certificate as a side effect of a routine re-run would invalidate
# whatever already trusts it. install.mkcert keeps that promise on the
# can't-tell case too (no openssl, an unparseable file): it reuses, because a
# capability that reissued whenever it was unsure would stop being idempotent.
# Deliberate reissue of a MATCHING pair is `mkcert-setup.sh --force`.
function ensure_front_door_pair() {
    local gen="$MKCERT_SETUP"
    if [ ! -f "$gen" ]; then
        cap_fail 4 "the front-door TLS issuer is not at ${gen}, so this run can neither check that ${TLS_CERT} covers ${FRONT_DOOR_HOSTNAMES} nor issue a pair that does. Issue it by hand with: mkcert -cert-file '${TLS_CERT}' -key-file '${TLS_KEY}' ${FRONT_DOOR_HOSTNAMES//,/ }"
    fi

    info "resolving the front-door TLS pair for ${FRONT_DOOR_HOSTNAMES} (install.mkcert decides reuse vs reissue)..."
    # stdin is closed and the stdin opt-in cleared so the child cannot block;
    # its human logs flow to our stderr, its JSON envelope is captured here so
    # exactly one envelope (ours) ever reaches stdout.
    # The child's envelope is CAPTURED rather than discarded: cap_fail writes
    # its message only there, so a `>/dev/null` threw away the one sentence
    # saying what went wrong -- and left this caller guessing from an exit code.
    # Exit 4 from install.mkcert now covers three different prerequisites
    # (mkcert absent, certutil absent, a password with no terminal to ask on),
    # so a hardcoded "mkcert is not installed" here would be wrong two times in
    # three (memql#3560).
    #
    # AND THE CAROOT TRAVELS WITH IT (memql#4069). install.mkcert takes
    # --caroot, and with none it resolves `mkcert -CAROOT` -- the machine
    # default. That is right for `make up` (nothing here pins a root, so the
    # operator's own mkcert answers) and WRONG for the installer, which
    # deliberately does not use the machine default: its localCA step creates
    # the CA under ~/.memql/mkcert. While this value did not travel, the two
    # halves of one install disagreed about which CA exists, and disagreed
    # differently depending on the machine:
    #
    #   a clean machine -- nothing in the default root, so install.mkcert
    #     refuses (exit 3 -> our exit 4 below) and the whole install dies at
    #     clusterUp, naming a prerequisite localCA had satisfied one step
    #     earlier under a root this call never looked in. Every leg of the CI
    #     cluster lane failed exactly here.
    #   a machine that has run `make up` -- a CA is already in the default root,
    #     so this succeeds and the cluster's front door is served by a
    #     certificate from a CA THE INSTALLER DID NOT CREATE and will not remove
    #     on uninstall (removal is gated on the `.memql-created` marker beside
    #     the key material). Nothing warns. That silence is the worse half.
    #
    # Passed only when non-empty, so absence stays absence: an empty --caroot=
    # would resolve identically today, but spelling "no override" as a flag with
    # an empty value invites a future reader to treat it as a value. The inner
    # quotes are load-bearing -- a CAROOT is a PATH and may contain a space;
    # `${VAR:+--caroot=$VAR}` would hand install.mkcert two arguments and a
    # truncated root, which fails as "no CA there" and reads like this very bug.
    local rc=0 child=""
    child="$(CAP_PARAMS_STDIN= bash "$gen" \
        --hostnames="$FRONT_DOOR_HOSTNAMES" \
        --cert-file="$TLS_CERT" \
        --key-file="$TLS_KEY" \
        --mkcert="$MKCERT_BIN" \
        ${MKCERT_CAROOT:+--caroot="${MKCERT_CAROOT}"} \
        ${ALLOW_MISSING_CERTUTIL:+--allow-missing-certutil} \
        </dev/null)" || rc=$?

    if [ "$rc" -ne 0 ] && [ -n "$child" ]; then
        printf 'install.mkcert said: %s\n' "$child" >&2
    fi

    case "$rc" in
        0) ;;
        # NAMES THE ESCAPE, because this refusal now reaches EVERY run rather
        # than only the runs that had no pair (memql#3730) -- and telling the
        # operator to run install.mkcert directly is advice that refuses again
        # for the same reason and unblocks nothing. That is the shape of defect
        # this issue was filed about ("run make secrets", which reused the stale
        # cert); shipping a second one would be worse than the first.
        4) cap_fail 4 "the front-door certificate could not be resolved: install.mkcert is missing something it needs (its own message is above -- mkcert itself, certutil for browser trust, or a password it had no terminal to ask for). Fix what it named, then re-run 'make secrets'. If it was certutil and this machine has no browser, waive browser trust and continue:  MEMQL_LOCAL_TLS_ALLOW_MISSING_CERTUTIL=1 make secrets  (the cluster front door then works for curl and the SDKs, and stays untrusted in Firefox/Chrome)." ;;
        # Reached whether or not a pair is on disk, and that is right: a
        # certificate signed by a CA this machine no longer has is one nothing
        # trusts, so seeding it would put an untrusted edge in front of the
        # cluster exactly as a missing secret does.
        # THE REMEDY CARRIES THE CAROOT when one was given (memql#4069), because
        # the root is exactly what this refusal is about. Without it the advice
        # says "create a CA" and creates it in the machine default -- a
        # different root from the one this run was told to use, so the next run
        # refuses identically and the operator has been handed a command that
        # cannot work twice in a row.
        3) cap_fail 4 "no mkcert root CA exists in ${MKCERT_CAROOT:-the default mkcert CAROOT} yet. Creating one writes to the system trust store, so it is a deliberate one-time step -- run it, then re-run 'make secrets':  bash scripts/install/mkcert-setup.sh${MKCERT_CAROOT:+ --caroot=$MKCERT_CAROOT} --confirm=install-memql-ca" ;;
        *) cap_fail 5 "resolving the front-door certificate failed (install.mkcert exit ${rc}); see the log above." ;;
    esac

    if [ ! -s "$TLS_CERT" ] || [ ! -s "$TLS_KEY" ]; then
        cap_fail 5 "install.mkcert reported success but ${TLS_CERT} / ${TLS_KEY} are missing or empty."
    fi

    # WHAT HAPPENED IS THE CHILD'S TO REPORT, and this envelope must not guess
    # it (memql#3730). The entire bug was frontDoorTlsSource saying `existing`
    # while the front door served garbage, so an unreadable envelope is a
    # FAILURE here rather than a default: "I could not tell" reported as
    # "nothing changed" is the original defect wearing a new hat.
    case "$child" in
        *'"certIssued"'*) ;;
        *) cap_fail 5 "install.mkcert exited 0 but its result envelope does not report certIssued, so this run cannot tell whether the front-door pair was reused or replaced -- and reporting 'existing' on a guess is the memql#3730 defect itself. Envelope: ${child:-<empty>}" ;;
    esac

    # Whether the names were CHECKED comes from the child's measurement and is
    # never inferred here (memql#3730). install.mkcert deliberately reuses a pair
    # whose names it could not read -- no openssl, a file that does not parse --
    # which is right for idempotency and is NOT the same fact as "it covers the
    # domain". Saying "already covers" on that branch would assert exactly the
    # unverified coverage claim this change exists to stamp out, in a line this
    # change added; and it was demonstrably false, since the reuse test's own
    # fixture is not parseable X.509.
    if child_reports_true "$child" coverageVerified; then
        FRONT_DOOR_COVERAGE_VERIFIED="true"
    fi

    if ! child_reports_true "$child" certIssued; then
        TLS_CERT_SOURCE="existing"
        if [ "$FRONT_DOOR_COVERAGE_VERIFIED" = "true" ]; then
            info "front-door TLS pair at ${TLS_CERT} covers ${FRONT_DOOR_HOSTNAMES}; reusing it."
        else
            info "front-door TLS pair at ${TLS_CERT} kept as-is; reusing it."
            warn "  its names could NOT be read, so coverage of ${FRONT_DOOR_HOSTNAMES} is UNVERIFIED"
            warn "  (openssl absent, or ${TLS_CERT} does not parse as a certificate). This is the one"
            warn "  case a run cannot rule out serving the wrong certificate; install openssl to"
            warn "  have it checked."
        fi
        return 0
    fi

    if child_reports_true "$child" reissued; then
        # A THIRD source value, not folded into `issued`, because the two mean
        # different things to whoever reads this envelope: `issued` created
        # something that was absent, `reissued` REPLACED something that was
        # present and wrong. An operator whose front-door certificate has just
        # been overwritten is entitled to see that in the JSON.
        TLS_CERT_SOURCE="reissued"
        warn "the front-door TLS pair at ${TLS_CERT} did NOT cover ${FRONT_DOOR_HOSTNAMES} and was REISSUED."
        warn "  A pair issued for a different domain leaves traefik serving a certificate for a"
        warn "  name nobody dialed, which fails TLS against a hostname you typed yourself."
        warn "  The old file is gone -- mkcert wrote the new pair over it in place."
    else
        TLS_CERT_SOURCE="issued"
    fi
    cap_changed
}

# child_reports_true <envelope> <key> -- true when install.mkcert's result
# envelope carries "<key>":true.
#
# NOT capability.sh's _cap_json_field, for two reasons. It reads a TOP-LEVEL
# field (its jq tier answers `has($k)` against the outer object) and these keys
# live one level down in `result`, so it would return empty for both. And it has
# a jq tier and a grep tier: a verdict that could differ between a machine with
# jq and a machine without it is not a verdict -- the same reasoning
# verify-frontdoor.sh gives for keeping one reader of its own.
#
# A substring match is sound here because cap_result_set_raw emits a raw boolean
# and both keys are unique within the envelope; nothing else in it can produce
# `"certIssued":true`. The caller has already refused an envelope that does not
# carry the key at all, so a false from this function means false, not absent.
function child_reports_true() {
    printf '%s' "$1" | grep -qE "\"$2\"[[:space:]]*:[[:space:]]*true"
}

# resolve_domain_default -- the default for --domain: what this cluster is
# ALREADY serving, or the environment-resolved domain when it is serving nothing
# yet (a first bring-up).
#
# Read from the memql-domain ConfigMap this script itself seeds, which is the
# cluster's own statement of its domain (component/envregistry/domain.go derives
# every issuer, CORS origin and redirect URI from it). Unreadable or absent is
# the ordinary first-boot case and falls through quietly -- this is a default,
# not a guard, and nothing irreplaceable rests on it.
function resolve_domain_default() {
    local existing
    existing="$(trim_space "$(kubectl get configmap memql-domain --namespace="$NAMESPACE" \
        -o 'jsonpath={.data.MEMQL_DOMAIN}' 2>/dev/null || true)")"
    if [ -n "$existing" ]; then
        printf '%s' "$existing"
        return 0
    fi
    printf '%s' "$MEMQL_LOCAL_DOMAIN"
}

# seed_domain_configmap -- the cluster's domain, as ONE key.
#
# Everything domain-shaped is derived from it at boot by
# component/envregistry/domain.go: identity's base URL, the issuer every node
# verifies against, the discovery endpoint, the CORS origins and the OAuth
# redirect URIs. So this ConfigMap is the whole of what the cluster is told
# about its own hostname (memql#3593).
#
# The local overlay mounts it via envFrom on all nine node Deployments, NOT
# optional: a node with no domain would fall back to the base manifests'
# staging placeholder issuer and reject every token while looking healthy.
function seed_domain_configmap() {
    info "seeding memql-domain (MEMQL_DOMAIN=${DOMAIN})..."
    kubectl create configmap memql-domain \
        --namespace "${NAMESPACE}" \
        --from-literal=MEMQL_DOMAIN="${DOMAIN}" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
    info "memql-domain seeded."
}

function seed_front_door_tls() {
    ensure_front_door_pair
    info "seeding ${MEMQL_LOCAL_TLS_SECRET} (front-door TLS for the local ingress)..."
    kubectl create secret tls "${MEMQL_LOCAL_TLS_SECRET}" \
        --namespace "${NAMESPACE}" \
        --cert="$TLS_CERT" --key="$TLS_KEY" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
    info "${MEMQL_LOCAL_TLS_SECRET} seeded."
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
        # A HARD FAILURE, not a warning (memql#3570). The comment above already
        # says what happens without these two secrets -- every node that mounts
        # memql-ca stalls in ContainerCreating -- and the code then warned and
        # carried on. So `make secrets` succeeded, k3d.up reported the cluster
        # up, the front door answered (traefik terminates TLS at the ingress,
        # with or without a backend), and the install ran three more steps
        # before anything noticed. A skip whose documented consequence is a
        # cluster that can never start is not a skip.
        cap_fail 4 "the internal CA generator is not at ${gen}, so identity-tls and memql-ca cannot be created -- and every node that mounts memql-ca would stall in ContainerCreating forever. Point --repo-root at a memQL checkout (the installer's is ~/.memql/src)."
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
    info "seeding memql-db-app-creds (Postgres credentials for in-cluster DB)..."
    # A kubernetes.io/basic-auth Secret, because that is the shape CNPG's
    # `bootstrap.initdb.secret` reads (memql#3846). It replaced the
    # POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB triple the retired
    # `postgres` Deployment consumed as container env; nothing else read those
    # keys, and the database name now lives in the Cluster CR's
    # bootstrap.initdb.database rather than in a credential.
    #
    # This Secret and the DSN in memql-secrets are written from the SAME two
    # variables a few lines below, which is what stops the database being
    # created with one password and connected to with another -- a failure that
    # presents as an authentication error against a database that just came up
    # clean.
    kubectl create secret generic memql-db-app-creds \
        --namespace="$NAMESPACE" \
        --type=kubernetes.io/basic-auth \
        --from-literal="username=$LOCAL_DB_USER" \
        --from-literal="password=$LOCAL_DB_PASSWORD" \
        --dry-run=client -o yaml \
        | kubectl apply -f - >&2
    SEEDED_COUNT=$((SEEDED_COUNT + 1))
    info "memql-db-app-creds seeded."
}

#=============================================================================
# MAIN MEMQL SECRET
#=============================================================================

function seed_memql_secrets() {
    # Both values were resolved in main BEFORE any mutation -- see the note
    # there. This function only writes.
    # WHY THE LOCAL CLUSTER TURNS DCR ON (memql#3719 / memql#3793).
    #
    # MEMQL_IDENTITY_OAUTH_DCR_ENABLED now defaults to FALSE: RFC 7591 dynamic
    # client registration is an unauthenticated write endpoint, and most
    # clusters route no mcp.<domain> and have no consumer for it.
    #
    # This cluster HAS one, and it is not a deployment -- it is in this
    # repository. editors/vscode/src/auth/register.ts's ensureClientId() POSTs
    # <issuer>/register the first time the extension signs in against a cluster
    # it has no stored client_id for. With DCR off that is a 403 and the
    # extension cannot complete a first sign-in at all.
    #
    # That consumer is invisible to both places memql#3719's prerequisite check
    # says to look: there is no v1:identity:oauthClient row until the extension
    # registers, and no overlay routes an MCP host. A consumer living in the
    # source tree rather than in a deployment is a third place, and it is the
    # one that caught this.
    #
    # Seeded into memql-secrets rather than onto identity's Deployment because
    # every node envFroms this Secret -- which is also what makes the mcp node's
    # boot warning (app/transport_mcp.go) correct rather than permanently
    # noisy: it reads this same variable from its own environment.
    local master_key signing_key signing_key_created_at db_dsn db_direct_dsn
    local node_bootstrap_token
    master_key="$RESOLVED_MASTER_KEY"
    signing_key="$RESOLVED_SIGNING_KEY_B64"
    signing_key_created_at="$RESOLVED_SIGNING_KEY_CREATED_AT"
    node_bootstrap_token="$RESOLVED_NODE_BOOTSTRAP_TOKEN"
    # Database DSN: the local CloudNativePG cluster (memql#3846).
    #
    # `memql-db-rw` is the Service CNPG maintains pointing at the CURRENT
    # PRIMARY -- it follows a failover, which is the whole reason to address the
    # cluster by it rather than by a pod. There is a `-ro` sibling for replicas
    # and a `-r` for any instance; the app uses `-rw` because it writes, and
    # read-replica routing is deliberately out of scope (epic memql#3842).
    #
    # sslmode=disable: intra-cluster traffic to a database that terminates no
    # TLS of its own. Unchanged from the Deployment this replaced.
    db_dsn="postgres://${LOCAL_DB_USER}:${LOCAL_DB_PASSWORD}@memql-db-rw:5432/${LOCAL_DB_NAME}?sslmode=disable"
    # For local, the direct DSN is the same as the pooler DSN (no PgBouncer).
    db_direct_dsn="$db_dsn"

    info "seeding memql-secrets (the config delivery path)..."
    kubectl create secret generic memql-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="MEMQL_MASTER_KEY=$master_key" \
        --from-literal="MEMQL_IDENTITY_SIGNING_KEY_B64=$signing_key" \
        --from-literal="MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT=$signing_key_created_at" \
        --from-literal="MEMQL_NODE_BOOTSTRAP_TOKEN=$node_bootstrap_token" \
        --from-literal="MEMQL_IDENTITY_OAUTH_DCR_ENABLED=true" \
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
    # environment -- NEVER hard-coded. A cloud install stays self-hosted and
    # pulls these from ESO/Key Vault instead; the no-cloud-leak guard,
    # deploy/k8s/overlays/livekit_cloud_guard_test.go, keeps *.livekit.cloud
    # out of deploy/k8s/overlays/cloud and deploy/k8s/base.
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
    # The checkout deploy/k8s/base/tls/gen-internal-ca.sh is read from. Defaults
    # to the derived root so a run from a checkout is unchanged; the install
    # graph passes the cloned stack (memql#3491).
    #
    # NOTE: only the deploy/ read moves. The install.mkcert path below is
    # resolved from THIS SCRIPT'S OWN location, because scripts/ IS staged at
    # that relative position -- repointing it would break the packaged path.
    REPO_ROOT="$(cap_param repo-root "$REPO_ROOT")"
    # A PARAM ONLY SO THE SEAM IS TESTABLE (memql#3730). The fail-closed
    # certIssued guard below is the single thing standing between a future
    # refactor and a silent default back to reporting `existing`, and with the
    # path hardcoded no test could hand this script a fabricated envelope to
    # exercise it. This does NOT create a second decision point: whatever sits
    # at this path owns the reuse-vs-reissue decision entirely, which is the
    # property the whole fix rests on.
    MKCERT_SETUP="$(cap_param mkcert-setup "${SCRIPT_DIR}/../install/mkcert-setup.sh")"
    cap_require namespace "$NAMESPACE"

    # Env feeds the DEFAULT slot; cap_param has no environment tier of its own.
    TLS_CERT="$(cap_param tls-cert "${MEMQL_LOCAL_TLS_CERT:-$MEMQL_LOCAL_TLS_DEFAULT_CERT}")"
    TLS_KEY="$(cap_param tls-key   "${MEMQL_LOCAL_TLS_KEY:-$MEMQL_LOCAL_TLS_DEFAULT_KEY}")"
    MKCERT_BIN="$(cap_param mkcert "${MEMQL_MKCERT_BIN:-mkcert}")"
    # NO ENVIRONMENT DEFAULT, unlike its neighbours -- deliberately. The caller
    # that needs a non-default CAROOT is the install graph, which passes the
    # value it already resolved for its own localCA step; anything else wants
    # mkcert's own answer. An env tier here would be a second place for the root
    # to be decided, which is the shape of the defect this param exists to close
    # (memql#4069).
    MKCERT_CAROOT="$(cap_param caroot "")"
    # Passed straight through to install.mkcert, which refuses (exit 4) on Linux
    # without certutil because browsers read the NSS store and a front door no
    # browser trusts is not a front door. That refusal now reaches every `make
    # secrets` rather than only the runs that had no pair yet (memql#3730), so
    # the waiver install.mkcert already documents has to be reachable from here
    # too -- otherwise this change newly bricks `make secrets` on a headless
    # machine with no way out short of editing a script.
    ALLOW_MISSING_CERTUTIL="$(cap_param allow-missing-certutil "${MEMQL_LOCAL_TLS_ALLOW_MISSING_CERTUTIL:-}")"
    cap_require tls-cert "$TLS_CERT"
    cap_require tls-key  "$TLS_KEY"

    # --gate-voice-lane-only: re-run just the voice-lane gate (memql#2416).
    # up.sh calls this AFTER the ArgoCD app has created the Deployments,
    # since the full seeding pass runs before they exist.
    if [ -n "$(cap_flag gate-voice-lane-only)" ]; then
        gate_voice_lane
        cap_result_set_raw voiceLaneGated true
        cap_ok
    fi

    check_prerequisites

    # THE DOMAIN IS RESOLVED HERE, AFTER the cluster is reachable, because the
    # cluster's own answer is the best default (memql#3730). Flag first, then
    # what this cluster is ALREADY serving, then the environment default.
    #
    # WHY THE CLUSTER TIER EXISTS. Without it, `make secrets` with no DOMAIN on a
    # cluster serving lab.example.com fell back to memql.localhost -- and now that
    # the pair is checked against the resolved domain, that would REISSUE over the
    # operator's lab.example.com certificate in place. The ConfigMap clobber was
    # already happening before this change (the domain-change refusal lives in
    # up.sh, not here); this makes the common case correct rather than merely
    # refused. An explicit --domain that disagrees with the cluster is still
    # up.sh's refuse_domain_change to reject -- one gate, where it already is.
    DOMAIN="$(cap_param domain "$(resolve_domain_default)")"
    # NOW TRUE: the certificate SANs and the memql-domain ConfigMap cannot name
    # different domains. This comment used to claim so on the strength of both
    # resolving MEMQL_LOCAL_DOMAIN -- but only the ConfigMap read the FLAG, and
    # `make up DOMAIN=lab.example.com` passes the flag without exporting the
    # variable. So the cluster was told one domain while the pair was issued for
    # another: the same "certificate for a name nobody dialed" this issue is
    # about, reached without any stale file at all. Both derive from the RESOLVED
    # domain now, through the one derivation in scripts/lib/localtls.sh.
    FRONT_DOOR_HOSTNAMES="$(localtls_hostnames_for "$DOMAIN")"

    # Resolve EVERYTHING that can be rejected before the first mutation.
    #
    # These run in the main shell, not a $(...), for two reasons. The
    # cheap one: they set globals. The load-bearing one: they can cap_fail, and
    # a bad MEMQL_MASTER_KEY must abort while the cluster is still untouched.
    # Previously resolution happened inside seed_memql_secrets -- the fourth
    # seeding step -- so a rejected key aborted a run that had already applied
    # the CA, the front-door TLS and the DB credentials, and then reported
    # changed:false.
    load_cluster_secret_snapshot
    resolve_master_key
    resolve_signing_key
    resolve_signing_key_created_at
    resolve_node_bootstrap_token

    seed_internal_ca
    seed_domain_configmap
    seed_front_door_tls
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
    # env | cluster | dev-default -- so a caller can tell a deliberate rotation
    # from a run that preserved the cluster's existing key (memql#2958).
    cap_result_set     masterKeySource "$RESOLVED_MASTER_KEY_SOURCE"
    # env | cluster | generated -- so a caller can tell a run that PRESERVED the
    # identity signing seed from one that minted or rotated it (memql#3400).
    # The seed itself is never emitted; only where it came from.
    cap_result_set     signingKeySource "$RESOLVED_SIGNING_KEY_SOURCE"
    # env | cluster | generated -- same contract as the signing seed, and read
    # for the same reason: "generated" on a cluster that was already running
    # means the mesh bootstrap secret just changed under live pods, and they
    # need a restart before peer connections recover (memql#3784). The token
    # itself is never emitted; only where it came from.
    cap_result_set     nodeBootstrapTokenSource "$RESOLVED_NODE_BOOTSTRAP_TOKEN_SOURCE"
    # existing | issued | reissued -- so a caller can tell a routine re-run from
    # the run that minted the front-door pair (memql#3384), and either of those
    # from the run that REPLACED a pair which did not cover the domain being
    # served (memql#3730). This field claimed `existing` over a certificate for
    # a domain that no longer existed anywhere in the project, so what it
    # distinguishes is the whole point of it: `reissued` means something the
    # operator had is gone.
    cap_result_set     frontDoorTlsSource "$TLS_CERT_SOURCE"
    cap_result_set     frontDoorTlsCert   "$TLS_CERT"
    # The names the seeded certificate had to cover. Absent from this envelope
    # until memql#3730, which is why nothing reading it could see that the cert
    # at frontDoorTlsCert was for somebody else's domain.
    cap_result_set     frontDoorTlsHostnames "$FRONT_DOOR_HOSTNAMES"
    # Whether those names were CHECKED on the seeded certificate, as measured by
    # install.mkcert -- not inferred from frontDoorTlsSource. `existing` with
    # coverageVerified false is the one outcome that leaves a pair nobody could
    # read, and a reader has to be able to see that rather than assume coverage.
    cap_result_set_raw frontDoorTlsCoverageVerified "$FRONT_DOOR_COVERAGE_VERIFIED"
    cap_ok
}

main "$@"
