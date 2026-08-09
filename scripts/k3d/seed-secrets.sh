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
#   local-znas-tls         -- front-door TLS (the mkcert *.local.znas.io pair)
#   memql-secrets          -- main app envelope (MEMQL_MASTER_KEY,
#                             MEMQL_GENESIS_B64,
#                             MEMQL_IDENTITY_SIGNING_KEY_B64, DATABASE_DSN, ...)
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
#   - MEMQL_MASTER_KEY is in the environment. When unset, the key already in
#     memql-secrets is REUSED if valid; only a cluster with no usable key
#     falls back to the dev default. A re-run therefore never replaces a
#     working key with a placeholder (memql#2958).
#   - mkcert is installed AND has a root CA on this machine. The front-door
#     pair is ISSUED here when absent (memql#3384) rather than skipped; see
#     seed_front_door_tls below for why skipping was never survivable.
#   - MEMQL_IDENTITY_SIGNING_KEY_B64 (the shared Ed25519 signing seed every
#     identity replica derives its key + kid + JWKS from) is GENERATED here
#     when the cluster has none (memql#3400), and REUSED verbatim thereafter --
#     a re-run must never rotate it, because rotation invalidates every live
#     session and every minted mesh node token. See resolve_signing_key.
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
# Exit codes: 0 ok | 2 bad param
#             | 4 prerequisite missing (kubectl/cluster/ns; mkcert or its CA)
#             | 5 op failed (unreadable existing secret; cert issuance failed)
#
# Refs: #2061 #2221 #3384

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "k3d.seedSecrets" "Seed the k8s Secrets that the local k3d overlay requires."
cap_spec_param "gate-voice-lane-only" "only re-run the voice-lane gate (scale voice/voice-agent per LiveKit config)"
cap_spec_param "namespace" "k8s namespace to seed into"
cap_spec_param "tls-cert"  "front-door TLS certificate path (issued with mkcert when absent)"
cap_spec_param "tls-key"   "front-door TLS private key path (issued with mkcert when absent)"
cap_spec_param "mkcert"    "path to the mkcert binary used to issue the front-door pair"
#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# Result accumulators.
SEEDED_COUNT=0
GENESIS_SOURCE="none"

# Front-door TLS: resolved in main() from --tls-cert / --tls-key / --mkcert,
# whose defaults come from the shared local-TLS locations (scripts/lib) with an
# MEMQL_LOCAL_TLS_* environment override.
TLS_CERT=""
TLS_KEY=""
MKCERT_BIN=""
TLS_CERT_SOURCE="none"   # existing | issued

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
# The incident was the master key; MEMQL_GENESIS_B64 sits three lines away in
# the same write and had exactly the same shape. Both now read the live value
# back and preserve it when the environment supplies nothing.
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
CLUSTER_GENESIS_B64=""
CLUSTER_SIGNING_KEY_B64=""

function load_cluster_secret_snapshot() {
    local state
    state="$(memql_secrets_state)"
    case "$state" in
        error:*)
            cap_fail 5 "cannot read the existing memql-secrets to check what this run would overwrite: ${state#error:}. Refusing to seed: if the cluster holds a working MEMQL_MASTER_KEY or genesis envelope, writing over it blind is how #2958 bricked a shared cluster. Fix cluster access and re-run, or export MEMQL_MASTER_KEY to seed deliberately."
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

    raw="$(kubectl get secret memql-secrets --namespace="$NAMESPACE" \
              -o 'jsonpath={.data.MEMQL_GENESIS_B64}' 2>/dev/null)" \
        || cap_fail 5 "memql-secrets exists but its MEMQL_GENESIS_B64 could not be read; refusing to overwrite it blind."
    if [ -n "$raw" ]; then
        CLUSTER_GENESIS_B64="$(trim_space "$(printf '%s' "$raw" | b64_decode 2>/dev/null || true)")"
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
}

#=============================================================================
# RESOLVE GENESIS B64
#=============================================================================

RESOLVED_GENESIS_B64=""

# Same precedence as the master key: environment, then the envelope already in
# the cluster, then nothing. The cluster branch is memql#2958's sibling -- a
# re-run with no genesis file on THIS machine used to blank the envelope every
# other node was running on.
function resolve_genesis_b64() {
    if [ -n "${MEMQL_GENESIS_B64:-}" ]; then
        RESOLVED_GENESIS_B64="$MEMQL_GENESIS_B64"
        GENESIS_SOURCE="envelope"
        return
    fi
    local genesis_file="${MEMQL_GENESIS_FILE:-$HOME/.memql/genesis.znas}"
    if [ -f "$genesis_file" ]; then
        RESOLVED_GENESIS_B64="$(base64 < "$genesis_file")"
        GENESIS_SOURCE="file"
        return
    fi
    if [ -n "$CLUSTER_GENESIS_B64" ]; then
        warn "no genesis file at $genesis_file and MEMQL_GENESIS_B64 is unset;"
        warn "  KEEPING the envelope already in memql-secrets. Nothing is overwritten."
        RESOLVED_GENESIS_B64="$CLUSTER_GENESIS_B64"
        GENESIS_SOURCE="cluster"
        return
    fi
    warn "no genesis file at $genesis_file and MEMQL_GENESIS_B64 is unset"
    warn "memql-secrets will be seeded WITHOUT the genesis envelope."
    warn "identity will fail to start without MEMQL_GENESIS_B64."
    RESOLVED_GENESIS_B64=""
    GENESIS_SOURCE="none"
}

#=============================================================================
# RESOLVE MASTER KEY
#=============================================================================

# The runtime requires EXACTLY 64 hex characters (32 bytes): see
# component/secret/encryption.go masterKey(), and genesis.ValidateMasterKeyHex
# which encodes the same rule on the sealing side. A value that misses it is
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
# It cannot decrypt a genesis envelope sealed under a real key. That is why the
# reuse branch below exists and runs FIRST.
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
# whole mesh into CrashLoopBackOff. If the genesis envelope was sealed under
# the replaced key and no pod still held it in memory, it was unrecoverable.
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
            warn "  This run ROTATES it. Anything sealed under the old key (the genesis"
            warn "  envelope, stored secrets) stops decrypting unless it is re-sealed."
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
    if [ -n "$CLUSTER_GENESIS_B64" ] || [ -n "${MEMQL_GENESIS_B64:-}" ] || [ -f "${MEMQL_GENESIS_FILE:-$HOME/.memql/genesis.znas}" ]; then
        warn "  A genesis envelope IS present. If it was sealed under a real key, the dev"
        warn "  default will NOT decrypt it -- export the real MEMQL_MASTER_KEY and re-run."
    fi
    RESOLVED_MASTER_KEY="$DEV_MASTER_KEY"
    RESOLVED_MASTER_KEY_SOURCE="dev-default"
}


#=============================================================================
# RESOLVE IDENTITY SIGNING SEED (memql#3400)
#=============================================================================

# WHY THIS EXISTS. deploy/k8s/base/identity.yaml runs identity at `replicas: 2`
# and says why it can: "the signing key comes from the envelope (same seed on
# every pod -> identical JWKS), so there is NO single-writer key PVC". Nothing
# supplied that seed locally, so KeyManager.Load() fell through to
# generateAndWriteCurrent() and EVERY POD MINTED ITS OWN Ed25519 keypair. Two
# replicas behind one Service published two different `kid`s; a token minted by
# one is structurally unverifiable by any node that fetched JWKS from the other,
# so `make scale N=2` -- the documented multi-node command -- produced coin-flip
# auth failures. This is the local analogue of the ESO/Key Vault delivery
# staging uses, exactly as the master key and the front-door TLS pair already
# are: the SHAPE (one shared seed, delivered through memql-secrets, read by
# every replica via envFrom) is identical everywhere; only the VALUE is local.
#
# The runtime requires base64-std of EXACTLY 32 bytes -- ed25519.SeedSize, see
# component/identity/keys.go NewKeyManagerFromSeed and the same rule in
# Config.Validate. 32 bytes encode to 43 base64 characters plus one '=' pad, so
# that shape check is complete: nothing else decodes to 32 bytes.
readonly SIGNING_KEY_B64_RE='^[A-Za-z0-9+/]{43}=$'

RESOLVED_SIGNING_KEY_B64=""
RESOLVED_SIGNING_KEY_SOURCE=""

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
# FRONT-DOOR TLS (local-znas-tls -- browser-trusted wildcard for the ingress)
#=============================================================================

# The local front door (traefik ingress on 443, see the local overlay's
# front-door manifests) terminates TLS with a browser-trusted wildcard cert for
# the operator's local domain -- exactly as the cloud ingress does, which is
# what makes the local connection model env-parity rather than a local special
# case (docs/public/operate/environment-parity.md).
#
# WHY THIS ISSUES RATHER THAN SKIPS (memql#3384). This step used to warn and
# return when the pair was absent. That was never a survivable degradation:
# both front-door ingresses NAME local-znas-tls, and traefik answers a missing
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
# Idempotent: an existing pair is REUSED verbatim, never reissued -- `make
# secrets` runs on every `make up`, and rotating the front-door certificate as
# a side effect of a routine re-run would invalidate whatever already trusts
# it. Deliberate reissue is `mkcert-setup.sh --force`.
function ensure_front_door_pair() {
    if [ -s "$TLS_CERT" ] && [ -s "$TLS_KEY" ]; then
        info "front-door TLS pair present (${TLS_CERT}); reusing it."
        TLS_CERT_SOURCE="existing"
        return 0
    fi

    local gen="${REPO_ROOT}/scripts/install/mkcert-setup.sh"
    if [ ! -f "$gen" ]; then
        cap_fail 4 "front-door TLS pair is missing (${TLS_CERT}) and the issuer script is not at ${gen}. Issue the pair by hand with: mkcert -cert-file '${TLS_CERT}' -key-file '${TLS_KEY}' ${MEMQL_LOCAL_TLS_HOSTNAMES//,/ }"
    fi

    info "front-door TLS pair missing; issuing it with mkcert (install.mkcert)..."
    # stdin is closed and the stdin opt-in cleared so the child cannot block;
    # its human logs flow to our stderr, its JSON envelope is captured here so
    # exactly one envelope (ours) ever reaches stdout.
    local rc=0
    CAP_PARAMS_STDIN= bash "$gen" \
        --hostnames="$MEMQL_LOCAL_TLS_HOSTNAMES" \
        --cert-file="$TLS_CERT" \
        --key-file="$TLS_KEY" \
        --mkcert="$MKCERT_BIN" \
        </dev/null >/dev/null || rc=$?

    case "$rc" in
        0) ;;
        4) cap_fail 4 "mkcert is not installed, so the front-door certificate cannot be issued. Install it and re-run 'make secrets':  brew install mkcert  (macOS)  |  https://github.com/FiloSottile/mkcert" ;;
        3) cap_fail 4 "no mkcert root CA exists on this machine yet. Creating one writes to the system trust store, so it is a deliberate one-time step -- run it, then re-run 'make secrets':  bash scripts/install/mkcert-setup.sh --confirm=install-memql-ca" ;;
        *) cap_fail 5 "issuing the front-door certificate failed (install.mkcert exit ${rc}); see the log above." ;;
    esac

    if [ ! -s "$TLS_CERT" ] || [ ! -s "$TLS_KEY" ]; then
        cap_fail 5 "install.mkcert reported success but ${TLS_CERT} / ${TLS_KEY} are missing or empty."
    fi
    TLS_CERT_SOURCE="issued"
    cap_changed
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
    # Both values were resolved in main BEFORE any mutation -- see the note
    # there. This function only writes.
    local genesis_b64 master_key signing_key db_dsn db_direct_dsn
    genesis_b64="$RESOLVED_GENESIS_B64"
    master_key="$RESOLVED_MASTER_KEY"
    signing_key="$RESOLVED_SIGNING_KEY_B64"
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
        --from-literal="MEMQL_IDENTITY_SIGNING_KEY_B64=$signing_key" \
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

    # Env feeds the DEFAULT slot; cap_param has no environment tier of its own.
    TLS_CERT="$(cap_param tls-cert "${MEMQL_LOCAL_TLS_CERT:-$MEMQL_LOCAL_TLS_DEFAULT_CERT}")"
    TLS_KEY="$(cap_param tls-key   "${MEMQL_LOCAL_TLS_KEY:-$MEMQL_LOCAL_TLS_DEFAULT_KEY}")"
    MKCERT_BIN="$(cap_param mkcert "${MEMQL_MKCERT_BIN:-mkcert}")"
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

    # Resolve EVERYTHING that can be rejected before the first mutation.
    #
    # These three run in the main shell, not a $(...), for two reasons. The
    # cheap one: they set globals. The load-bearing one: they can cap_fail, and
    # a bad MEMQL_MASTER_KEY must abort while the cluster is still untouched.
    # Previously resolution happened inside seed_memql_secrets -- the fourth
    # seeding step -- so a rejected key aborted a run that had already applied
    # the CA, the front-door TLS and the DB credentials, and then reported
    # changed:false. GENESIS_SOURCE is set by resolve_genesis_b64 itself now,
    # rather than being recomputed here to dodge the old subshell.
    load_cluster_secret_snapshot
    resolve_master_key
    resolve_signing_key
    resolve_genesis_b64

    seed_internal_ca
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
    cap_result_set     source "$GENESIS_SOURCE"
    # env | cluster | dev-default -- so a caller can tell a deliberate rotation
    # from a run that preserved the cluster's existing key (memql#2958).
    cap_result_set     masterKeySource "$RESOLVED_MASTER_KEY_SOURCE"
    # env | cluster | generated -- so a caller can tell a run that PRESERVED the
    # identity signing seed from one that minted or rotated it (memql#3400).
    # The seed itself is never emitted; only where it came from.
    cap_result_set     signingKeySource "$RESOLVED_SIGNING_KEY_SOURCE"
    # existing | issued -- so a caller can tell a routine re-run from the run
    # that minted the front-door pair (memql#3384).
    cap_result_set     frontDoorTlsSource "$TLS_CERT_SOURCE"
    cap_result_set     frontDoorTlsCert   "$TLS_CERT"
    cap_ok
}

main "$@"
