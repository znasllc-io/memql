#!/usr/bin/env bash
#
# scripts/deploy/staging-db-reset.sh
# ==================================
#
# DELIBERATE, MANUAL staging database reset (znasllc-io/memql#1500), made
# AUTH-COHERENT (znasllc-io/memql#1522).
#
# Wipes the STAGING database back to a fresh, empty state with the correct
# schema, for when many app iterations have left stale / crappy data behind and
# we want to start clean. This is DESTRUCTIVE: every row in the staging DB is
# gone. The owner re-registers via magic-link on next login.
#
# HARD RULE: this is NEVER part of a deploy. `make deploy` / aks-deploy.sh do
# NOT call it. It only runs when an operator invokes it directly and confirms.
#
# Auth coherence (#1522 -- depends on #1515 + #1521)
# --------------------------------------------------
# A DB wipe used to leave staging in a HALF-BROKEN auth state: every session
# row AND every node-token grant lives in the DB, so the wipe invalidates them
# all, and the cluster only recovered after a manual identity reseal + a manual
# mesh roll. Two facts make recovery automatic now, and this script LEANS ON
# BOTH:
#   * Identity's JWT signing key is a SHARED seed (MEMQL_IDENTITY_SIGNING_KEY_B64)
#     that rides the sealed genesis envelope (MEMQL_GENESIS_B64 in the
#     memql-secrets Secret), NOT the DB. The wipe never touches it, so every
#     identity replica derives the SAME key + kid + JWKS after the reset --
#     PROVIDED the seed is actually in place (#1515). If it is missing,
#     identity either fails fast (the #1515 guard) or mints per-pod ephemeral
#     keys that diverge across replicas, so ~50% of verifications fail. We
#     therefore PRE-FLIGHT the seed BEFORE wiping and refuse if it is absent.
#   * Mesh nodes re-mint their class="node" token on an auth-rejection (#1521),
#     so once identity is back with the stable key every leaf reconnects on its
#     own -- no manual pod roll.
#
# What it does:
#   1. Refuses unless --env=staging AND the current kube-context looks like the
#      staging cluster (a wrong-cluster guard).
#   2. PRE-FLIGHT (#1522): verifies the shared identity signing seed is in place
#      (memql-secrets carries the genesis envelope, and identity is NOT running
#      in the divergent ephemeral-key mode) so the post-reset JWKS stays
#      coherent. Refuses LOUDLY if it is not -- a wipe without the seed would
#      leave auth unrecoverable without a manual reseal.
#   3. Requires an explicit --confirm='reset staging' param (or --yes).
#   4. Captures + scales the namespace's app Deployments AND Argo Rollouts to 0
#      so nothing writes mid-reset, restoring the replica counts at the end
#      (even on failure).
#   5. Wipes: a one-shot in-cluster Job (postgres client) connects with
#      MEMQL_DATABASE_DSN from the memql-secrets Secret and runs
#      DROP SCHEMA public CASCADE; CREATE SCHEMA public; + re-grants. The Job is
#      generated INLINE (no destructive manifest left on disk to apply by
#      accident).
#   6. Rebuilds the schema by re-running the existing `memql migrate` Job
#      (deploy/k8s/base/migrate-job.yaml), pinned to the live identity image so
#      the schema matches the deployed version. Migrations are idempotent +
#      extension-aware.
#   7. Brings the app back up ORDERED (#1522): identity FIRST (waited ready) so
#      it is serving the stable JWKS before any mesh node bootstraps, then the
#      remaining workloads -- node tokens re-mint cleanly against a ready issuer
#      (#1521).
#   8. VERIFIES auth coherence (#1522): identity becomes Available and is
#      serving a well-formed JWKS (an in-cluster probe). Reports a clear PASS so
#      the operator knows login + mesh are healthy with NO manual recovery.
#
# Non-interactive by design (capability-script contract, #2221): the
# destructive confirmation is an explicit param, never a blocking prompt.
#
# Usage:
#   scripts/deploy/staging-db-reset.sh --env=staging [--namespace=memql]
#                                      (--confirm='reset staging' | --yes) [--dry-run]
#
#   --env=ENV         Must be "staging". Anything else (esp. production) is
#                     refused -- this tool is staging-only by design.
#   --namespace=NS    Kubernetes namespace (default: memql).
#   --confirm=PHRASE  Must equal 'reset staging' to proceed (or set CONFIRM=...).
#                     Replaces the old interactive typed confirmation.
#   --yes             Skip the confirmation entirely (for an operator who has
#                     already confirmed out of band). Still env- and
#                     context-guarded.
#   --dry-run         Print the full plan and touch NOTHING.
#   --help            Show this help.
#
set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_NS="memql"
ALLOWED_ENV="staging"
# The current kube-context must contain this substring, or we refuse: a
# wrong-cluster guard so a fat-fingered context can never wipe prod.
EXPECTED_CONTEXT_SUBSTR="staging"
CONFIRM_PHRASE="reset staging"
SECRET_NAME="memql-secrets"
DSN_KEY="MEMQL_DATABASE_DSN"
WIPE_JOB="memql-db-reset-wipe"
MIGRATE_JOB="memql-migrate"
PSQL_IMAGE="postgres:16-alpine"

# Auth coherence (#1522).
IDENTITY_DEPLOY="identity"            # the auth/JWKS issuer Deployment
GENESIS_KEY="MEMQL_GENESIS_B64"       # sealed envelope carrying MEMQL_IDENTITY_SIGNING_KEY_B64 (#1515/#550)
SIGNING_KEY="MEMQL_IDENTITY_SIGNING_KEY_B64" # the shared signing seed, if surfaced directly
EPHEMERAL_OPT_IN="MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY" # the #1515 per-pod ephemeral-key opt-in (divergent at >=2 replicas)
JWKS_VERIFY_JOB="memql-db-reset-jwks-verify"
JWKS_URL="https://identity:8085/.well-known/jwks.json"  # in-cluster identity TLS JWKS
IDENTITY_READY_TIMEOUT="180s"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATE_MANIFEST="$SCRIPT_DIR/../../deploy/k8s/base/migrate-job.yaml"

REPLICAS_FILE=""   # tmpfile holding "deploy<TAB>replicas" for restore

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    # Print the header comment block (line 2 up to, but not including, the
    # `set -euo pipefail` line) so the help text never drifts from the header.
    sed -n '2,/^set -euo pipefail/{/^set -euo pipefail/d;p;}' "$0" | sed 's/^# \{0,1\}//'
}

function err()  { echo "ERROR: $*" >&2; }
function info() { echo "INFO: $*" >&2; }
function warn() { echo "WARNING: $*" >&2; }

function parse_arguments() {
    ENV=""
    NS="$DEFAULT_NS"
    DRY_RUN=false
    ASSUME_YES=false
    CONFIRM="${CONFIRM:-}"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)       ENV="${1#*=}"; shift ;;
            --namespace=*) NS="${1#*=}"; shift ;;
            --confirm=*)   CONFIRM="${1#*=}"; shift ;;
            --yes)         ASSUME_YES=true; shift ;;
            --dry-run)     DRY_RUN=true; shift ;;
            --help|-h)     show_help; exit 0 ;;
            *) err "unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function validate_arguments() {
    # Staging-only by design. Refuse an empty, prod, or unknown env outright.
    if [[ "$ENV" != "$ALLOWED_ENV" ]]; then
        err "--env must be '$ALLOWED_ENV' (got '${ENV:-<empty>}'). This tool is staging-only and refuses prod."
        exit 1
    fi
    command -v kubectl >/dev/null 2>&1 || { err "kubectl is required"; exit 1; }
    # Wrong-cluster guard: the live kube-context must look like staging.
    local ctx
    ctx="$(kubectl config current-context 2>/dev/null || true)"
    if [[ -z "$ctx" ]]; then
        err "no current kube-context; refusing"
        exit 1
    fi
    if [[ "$ctx" != *"$EXPECTED_CONTEXT_SUBSTR"* ]]; then
        err "kube-context '$ctx' does not contain '$EXPECTED_CONTEXT_SUBSTR' -- refusing (wrong-cluster guard)."
        err "switch to the staging context before running a DB reset."
        exit 1
    fi
    info "env=$ENV namespace=$NS context=$ctx dry-run=$DRY_RUN"
}

# PRE-FLIGHT (#1522): the post-reset cluster's JWKS is only coherent if the
# shared identity signing seed is in place. The seed rides the sealed genesis
# envelope ($GENESIS_KEY) in the $SECRET_NAME Secret (#1515/#550); the wipe
# never touches the Secret, so the seed survives the reset -- but ONLY if it is
# actually there. We REFUSE to wipe if (a) the Secret/envelope is missing, or
# (b) identity is running in the divergent per-pod ephemeral-key mode with >=2
# replicas. A wipe in either state leaves auth unrecoverable without a manual
# reseal -- exactly the half-broken state #1522 exists to prevent.
function verify_auth_seed() {
    info "pre-flight: verifying the shared identity signing seed (#1515) is in place..."

    # (a) The Secret must exist and carry the sealed genesis envelope (which
    #     contains MEMQL_IDENTITY_SIGNING_KEY_B64), or the seed surfaced directly.
    if ! kubectl -n "$NS" get secret "$SECRET_NAME" >/dev/null 2>&1; then
        err "Secret '$SECRET_NAME' not found in namespace '$NS' -- cannot verify the identity signing seed."
        err "the reset would leave identity with no shared key. Seal + apply the genesis envelope first"
        err "(scripts/secrets/reseal-genesis.sh) so $GENESIS_KEY / $SIGNING_KEY is present."
        exit 1
    fi
    # List the Secret's data KEY NAMES (one per line) -- go-template, not
    # jsonpath, so we get plain key names instead of a Go map literal.
    # The go-template vars below are template syntax, not shell expansions.
    local secret_keys=""
    # shellcheck disable=SC2016
    secret_keys="$(kubectl -n "$NS" get secret "$SECRET_NAME" \
        -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}' 2>/dev/null || true)"
    if ! printf '%s\n' "$secret_keys" | grep -qxE "$GENESIS_KEY|$SIGNING_KEY"; then
        err "Secret '$SECRET_NAME' carries neither $GENESIS_KEY (sealed envelope) nor $SIGNING_KEY (direct seed)."
        err "identity would mint a per-pod key after the reset and JWKS would diverge across replicas (#1515)."
        err "reseal the genesis envelope (scripts/secrets/reseal-genesis.sh) before resetting."
        exit 1
    fi
    info "  seed source present in $SECRET_NAME (genesis envelope or direct seed)."

    # (b) Identity must NOT be in the divergent ephemeral-key mode at >=2 replicas.
    #     The #1515 guard only opts into per-pod ephemeral keys when
    #     $EPHEMERAL_OPT_IN=true; with multiple replicas that means divergent JWKS.
    local replicas eph
    replicas="$(kubectl -n "$NS" get deploy "$IDENTITY_DEPLOY" \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
    eph="$(kubectl -n "$NS" get deploy "$IDENTITY_DEPLOY" \
        -o jsonpath="{range .spec.template.spec.containers[*].env[?(@.name=='$EPHEMERAL_OPT_IN')]}{.value}{end}" \
        2>/dev/null || true)"
    if [[ -z "$replicas" ]]; then
        warn "could not read identity Deployment replicas -- skipping the ephemeral-key check."
    elif [[ "$eph" == "true" && "${replicas:-0}" -ge 2 ]]; then
        err "identity Deployment runs $replicas replicas with $EPHEMERAL_OPT_IN=true -- per-pod ephemeral keys"
        err "DIVERGE across replicas (#1515). Resetting now would leave ~50% of token verifications failing."
        err "remove $EPHEMERAL_OPT_IN (rely on the shared $SIGNING_KEY seed) before resetting."
        exit 1
    fi
    info "  identity (${replicas:-?} replicas) is on the shared-seed path -- JWKS will stay coherent after reset."

    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] pre-flight passed: the post-reset JWKS would be coherent."
    fi
}

function confirm() {
    cat >&2 <<BANNER

  ============================================================
   DESTRUCTIVE: this WIPES the entire $ENV database in
   namespace '$NS' on context '$(kubectl config current-context 2>/dev/null)'.
   Every row is permanently deleted; the schema is rebuilt empty.
  ============================================================

BANNER
    if [[ "$DRY_RUN" == true ]]; then
        info "--dry-run: no confirmation needed; nothing will be changed."
        return 0
    fi
    if [[ "$ASSUME_YES" == true ]]; then
        warn "--yes given; skipping confirmation."
        return 0
    fi
    # Non-interactive by contract (capability-script contract, #2221): the
    # confirmation is an explicit param, never a blocking prompt. The operator
    # (or an action executor) must pass --confirm='<phrase>' (or CONFIRM=...).
    if [[ "$CONFIRM" != "$CONFIRM_PHRASE" ]]; then
        err "confirmation required: pass --confirm='$CONFIRM_PHRASE' (or CONFIRM='$CONFIRM_PHRASE'), or --yes. Nothing was changed."
        exit 3
    fi
}

# Capture the namespace's scalable workloads (Deployments AND Argo Rollouts)
# as "kind<TAB>name<TAB>replicas". bff is an Argo Rollout that owns its pods
# directly -- its backing Deployment sits at 0 via workloadRef -- so scaling
# only Deployments would leave bff (a DB writer) running mid-reset. We must
# scale the Rollout too.
function capture_workloads() {
    REPLICAS_FILE="$(mktemp -t memql-dbreset-replicas.XXXXXX)"
    kubectl -n "$NS" get deploy \
        -o jsonpath='{range .items[*]}deployment{"\t"}{.metadata.name}{"\t"}{.spec.replicas}{"\n"}{end}' \
        >> "$REPLICAS_FILE" 2>/dev/null || true
    # Rollout CRD may be absent on a non-Argo cluster -- tolerate the failure.
    kubectl -n "$NS" get rollout \
        -o jsonpath='{range .items[*]}rollout{"\t"}{.metadata.name}{"\t"}{.spec.replicas}{"\n"}{end}' \
        >> "$REPLICAS_FILE" 2>/dev/null || true
}

# scale_one <kind> <name> <replicas>: scale a single captured workload.
function scale_one() {
    local kind="$1" name="$2" to="$3"
    [[ -z "$kind" || -z "$name" || -z "$to" ]] && return 0
    info "scaling $kind/$name -> $to"
    kubectl -n "$NS" scale "$kind/$name" --replicas="$to" >/dev/null 2>&1 \
        || warn "could not scale $kind/$name to $to -- check manually"
}

# scale_workloads <0|restore> [skip_identity]: 0 scales everything down;
# restore puts each back to its captured count. skip_identity=true leaves the
# identity Deployment untouched (the ordered bring-up handles it first --
# see restore_replicas).
function scale_workloads() {
    local target="$1" skip_identity="${2:-false}" kind name replicas to
    while IFS=$'\t' read -r kind name replicas; do
        [[ -z "$kind" || -z "$name" ]] && continue
        [[ "$skip_identity" == true && "$name" == "$IDENTITY_DEPLOY" ]] && continue
        to="$target"
        [[ "$target" == "restore" ]] && to="$replicas"
        scale_one "$kind" "$name" "$to"
    done < "$REPLICAS_FILE"
}

function capture_and_scale_down() {
    capture_workloads

    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would scale these workloads (kind name replicas) to 0 then restore:"
        sed 's/^/    /' "$REPLICAS_FILE" >&2 || true
        return 0
    fi

    # Restore on ANY exit after this point (success or failure).
    trap restore_replicas EXIT

    scale_workloads 0
    info "waiting for app pods to terminate..."
    kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/part-of=memql --timeout=120s >/dev/null 2>&1 || true
}

# Ordered bring-up (#1522): scale identity back FIRST and wait for it to report
# Available, so it is serving the stable JWKS before any mesh node bootstraps;
# THEN restore the remaining workloads. The mesh re-mints its node tokens on the
# first auth-rejection (#1521), so once identity is up every leaf reconnects on
# its own with no manual roll. Used by BOTH the EXIT trap (failure-path safety
# net) and the explicit success-path bring-up, so it must be idempotent.
function restore_replicas() {
    [[ -n "$REPLICAS_FILE" && -f "$REPLICAS_FILE" ]] || return 0

    local irep
    irep="$(awk -F '\t' -v d="$IDENTITY_DEPLOY" '$2==d{print $3; exit}' "$REPLICAS_FILE")"
    if [[ -n "$irep" ]]; then
        scale_one deployment "$IDENTITY_DEPLOY" "$irep"
        info "waiting for identity to become Available before the mesh comes back (so node tokens re-mint cleanly)..."
        kubectl -n "$NS" rollout status "deploy/$IDENTITY_DEPLOY" --timeout="$IDENTITY_READY_TIMEOUT" >/dev/null 2>&1 \
            || warn "identity not Available within $IDENTITY_READY_TIMEOUT -- the mesh re-mints on auth failure (#1521), but check identity manually."
    fi

    scale_workloads restore true   # everyone except identity (already restored)
    rm -f "$REPLICAS_FILE" 2>/dev/null || true
}

# Success-path wrapper around the ordered bring-up. Dry-run aware: in a dry run
# capture_and_scale_down never scaled anything down, so we must NOT scale up.
function bring_up_ordered() {
    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would bring workloads back up ORDERED: identity first (waited Available), then the mesh -- node tokens re-mint against a ready JWKS issuer (#1521/#1522)."
        rm -f "$REPLICAS_FILE" 2>/dev/null || true
        return 0
    fi
    info "bringing workloads back up (identity first, then the mesh)..."
    restore_replicas
}

# VERIFY auth coherence (#1522): the load-bearing acceptance check. After the
# ordered bring-up, identity MUST be Available (fatal if not -- JWKS would be
# down and both login and mesh tokens would fail). As a functional signal we
# also probe the JWKS document in-cluster; a failed probe is a loud WARNING
# (network/tooling flake must not fail an otherwise-good reset) that points the
# operator at the end-to-end smoke test.
function verify_auth_coherence() {
    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would verify post-reset auth coherence: identity Available + JWKS served at $JWKS_URL (in-cluster probe)."
        return 0
    fi
    info "verifying post-reset auth coherence (#1522)..."
    if ! kubectl -n "$NS" rollout status "deploy/$IDENTITY_DEPLOY" --timeout="$IDENTITY_READY_TIMEOUT" >/dev/null 2>&1; then
        err "identity did not become Available within $IDENTITY_READY_TIMEOUT after the reset."
        err "JWKS is not being served -- login + mesh tokens will fail. Investigate identity:"
        kubectl -n "$NS" get pods -l app.kubernetes.io/name="$IDENTITY_DEPLOY" >&2 2>/dev/null || true
        exit 1
    fi
    info "  identity is Available -- JWKS issuer is up."
    if ! probe_jwks; then
        warn "could not confirm a well-formed JWKS via the in-cluster probe."
        warn "identity is Available, so this is most likely a probe-tooling flake; confirm login end-to-end with: make smoke-staging"
    fi
}

# probe_jwks: one-shot in-cluster Job that fetches identity's JWKS over its
# internal TLS surface and asserts a "keys" array. Uses the same postgres:alpine
# image as the wipe Job (already proven to have working TLS libs, since it
# speaks SSL to Tiger Cloud). --no-check-certificate: the cluster CA is
# self-signed; we only need to prove identity SERVES a well-formed document.
function probe_jwks() {
    kubectl -n "$NS" delete job "$JWKS_VERIFY_JOB" --ignore-not-found >/dev/null 2>&1 || true
    kubectl apply -f - <<JOB >/dev/null 2>&1 || return 1
apiVersion: batch/v1
kind: Job
metadata:
  name: $JWKS_VERIFY_JOB
  namespace: $NS
  labels:
    app.kubernetes.io/name: $JWKS_VERIFY_JOB
    app.kubernetes.io/part-of: memql
spec:
  backoffLimit: 2
  ttlSecondsAfterFinished: 120
  template:
    metadata:
      labels:
        app.kubernetes.io/name: $JWKS_VERIFY_JOB
        app.kubernetes.io/part-of: memql
    spec:
      restartPolicy: Never
      containers:
        - name: probe
          image: $PSQL_IMAGE
          command: ["sh", "-c"]
          args:
            - |
              set -e
              body="\$(wget -q -O - --no-check-certificate "$JWKS_URL")"
              echo "\$body" | grep -q '"keys"' || { echo "JWKS missing keys array"; exit 1; }
              echo "JWKS OK (contains keys)"
      resources: {}
JOB
    local rc=1
    if kubectl -n "$NS" wait --for=condition=complete --timeout=90s "job/$JWKS_VERIFY_JOB" >/dev/null 2>&1; then
        info "  JWKS served + well-formed at $JWKS_URL (in-cluster probe)."
        rc=0
    fi
    kubectl -n "$NS" logs "job/$JWKS_VERIFY_JOB" --tail=3 >&2 2>/dev/null || true
    kubectl -n "$NS" delete job "$JWKS_VERIFY_JOB" --ignore-not-found >/dev/null 2>&1 || true
    return $rc
}

# Wipe via a one-shot in-cluster Job: psql connects with the DSN from the
# Secret and drops + recreates the public schema. Generated inline so no
# destructive manifest sits on disk.
function wipe_schema() {
    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would run a one-shot Job ($WIPE_JOB, image $PSQL_IMAGE) executing:"
        echo "    DROP SCHEMA public CASCADE; CREATE SCHEMA public; + re-grants" >&2
        return 0
    fi
    info "wiping schema (DROP SCHEMA public CASCADE) via one-shot Job..."
    kubectl -n "$NS" delete job "$WIPE_JOB" --ignore-not-found >/dev/null 2>&1 || true
    kubectl apply -f - <<JOB >/dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: $WIPE_JOB
  namespace: $NS
  labels:
    app.kubernetes.io/name: $WIPE_JOB
    app.kubernetes.io/part-of: memql
spec:
  backoffLimit: 1
  ttlSecondsAfterFinished: 300
  template:
    metadata:
      labels:
        app.kubernetes.io/name: $WIPE_JOB
        app.kubernetes.io/part-of: memql
    spec:
      restartPolicy: Never
      containers:
        - name: wipe
          image: $PSQL_IMAGE
          envFrom:
            - secretRef:
                name: $SECRET_NAME
          command: ["sh", "-c"]
          args:
            - |
              set -e
              if [ -z "\${$DSN_KEY:-}" ]; then echo "missing $DSN_KEY in secret"; exit 1; fi
              psql "\$$DSN_KEY" -v ON_ERROR_STOP=1 <<'SQL'
              DROP SCHEMA IF EXISTS public CASCADE;
              CREATE SCHEMA public;
              GRANT ALL ON SCHEMA public TO CURRENT_USER;
              GRANT ALL ON SCHEMA public TO public;
              SQL
              echo "schema wiped + recreated empty"
      resources: {}
JOB
    if ! kubectl -n "$NS" wait --for=condition=complete --timeout=180s "job/$WIPE_JOB" >/dev/null 2>&1; then
        err "wipe Job did not complete; logs:"
        kubectl -n "$NS" logs "job/$WIPE_JOB" --tail=50 >&2 2>/dev/null || true
        exit 1
    fi
    kubectl -n "$NS" logs "job/$WIPE_JOB" --tail=5 >&2 2>/dev/null || true
    kubectl -n "$NS" delete job "$WIPE_JOB" --ignore-not-found >/dev/null 2>&1 || true
    info "wipe complete."
}

# Rebuild the schema by re-running the existing migrate Job, pinned to the live
# identity image so it matches the deployed version. Mirrors aks-deploy.sh's
# run_migration_gate.
function rebuild_schema() {
    if [[ ! -f "$MIGRATE_MANIFEST" ]]; then
        err "migrate Job manifest not found at $MIGRATE_MANIFEST"
        exit 1
    fi
    local img
    img="$(kubectl -n "$NS" get deploy identity -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"

    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would re-run the migrate Job to rebuild the schema (image: ${img:-<manifest-pinned>})"
        return 0
    fi
    info "rebuilding schema via the migrate Job (image: ${img:-<manifest-pinned>})..."
    kubectl -n "$NS" delete job "$MIGRATE_JOB" --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "$img" ]]; then
        sed -E "s#image: .*/memql-identity:.*#image: ${img}#" "$MIGRATE_MANIFEST" | kubectl apply -f - >/dev/null
    else
        kubectl apply -f "$MIGRATE_MANIFEST" >/dev/null
    fi
    if ! kubectl -n "$NS" wait --for=condition=complete --timeout=300s "job/$MIGRATE_JOB" >/dev/null 2>&1; then
        err "migrate Job did not complete; logs:"
        kubectl -n "$NS" logs "job/$MIGRATE_JOB" --tail=50 >&2 2>/dev/null || true
        exit 1
    fi
    info "schema rebuilt (migrations applied to an empty DB)."
}

function report() {
    if [[ "$DRY_RUN" == true ]]; then
        echo "RESULT: dry-run only -- nothing changed." >&2
        return 0
    fi
    echo "RESULT: $ENV database reset to a fresh empty schema. Identity brought up first on the shared signing seed (JWKS coherent), then the mesh (node tokens re-mint via #1521) -- auth is coherent with NO manual recovery. The owner re-registers via magic-link on next login. Confirm end-to-end with: make smoke-staging" >&2
}

function main() {
    parse_arguments "$@"
    validate_arguments
    verify_auth_seed        # PRE-FLIGHT (#1522): refuse if the shared signing seed is absent.
    confirm
    capture_and_scale_down
    wipe_schema
    rebuild_schema
    bring_up_ordered        # ORDERED bring-up (#1522): identity first, then the mesh.
    verify_auth_coherence   # VERIFY (#1522): identity Available + JWKS served.
    # The EXIT trap (restore_replicas) is now a no-op -- bring_up_ordered already
    # restored every workload and removed the replicas file. It remains as the
    # safety net for the FAILURE path (an exit before bring_up_ordered).
    report
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
