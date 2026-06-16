#!/usr/bin/env bash
#
# scripts/deploy/staging-db-reset.sh
# ==================================
#
# DELIBERATE, MANUAL staging database reset (znasllc-io/memql#1500).
#
# Wipes the STAGING database back to a fresh, empty state with the correct
# schema, for when many app iterations have left stale / crappy data behind and
# we want to start clean. This is DESTRUCTIVE: every row in the staging DB is
# gone. The owner re-registers via magic-link on next login.
#
# HARD RULE: this is NEVER part of a deploy. `make deploy` / aks-deploy.sh do
# NOT call it. It only runs when an operator invokes it directly and confirms.
#
# What it does:
#   1. Refuses unless --env=staging AND the current kube-context looks like the
#      staging cluster (a wrong-cluster guard).
#   2. Requires an interactive typed confirmation (unless --yes).
#   3. Captures + scales the namespace's app Deployments to 0 so nothing writes
#      mid-reset, restoring the replica counts at the end (even on failure).
#   4. Wipes: a one-shot in-cluster Job (postgres client) connects with
#      MEMORY_NODES_DATABASE_DSN from the memql-secrets Secret and runs
#      DROP SCHEMA public CASCADE; CREATE SCHEMA public; + re-grants. The Job is
#      generated INLINE (no destructive manifest left on disk to apply by
#      accident).
#   5. Rebuilds the schema by re-running the existing `memql migrate` Job
#      (deploy/k8s/base/migrate-job.yaml), pinned to the live identity image so
#      the schema matches the deployed version. Migrations are idempotent +
#      extension-aware.
#   6. Scales the app back up.
#
# Usage:
#   scripts/deploy/staging-db-reset.sh --env=staging [--namespace=memql]
#                                      [--yes] [--dry-run]
#
#   --env=ENV         Must be "staging". Anything else (esp. production) is
#                     refused -- this tool is staging-only by design.
#   --namespace=NS    Kubernetes namespace (default: memql).
#   --yes             Skip the interactive typed confirmation (for an operator
#                     who has already confirmed out of band). Still env- and
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
DSN_KEY="MEMORY_NODES_DATABASE_DSN"
WIPE_JOB="memql-db-reset-wipe"
MIGRATE_JOB="memql-migrate"
PSQL_IMAGE="postgres:16-alpine"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATE_MANIFEST="$SCRIPT_DIR/../../deploy/k8s/base/migrate-job.yaml"

REPLICAS_FILE=""   # tmpfile holding "deploy<TAB>replicas" for restore

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    sed -n '2,46p' "$0" | sed 's/^# \{0,1\}//'
}

function err()  { echo "ERROR: $*" >&2; }
function info() { echo "INFO: $*" >&2; }
function warn() { echo "WARNING: $*" >&2; }

function parse_arguments() {
    ENV=""
    NS="$DEFAULT_NS"
    DRY_RUN=false
    ASSUME_YES=false
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)       ENV="${1#*=}"; shift ;;
            --namespace=*) NS="${1#*=}"; shift ;;
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
        warn "--yes given; skipping the interactive prompt."
        return 0
    fi
    local answer=""
    read -r -p "Type '$CONFIRM_PHRASE' to proceed (anything else aborts): " answer
    if [[ "$answer" != "$CONFIRM_PHRASE" ]]; then
        err "confirmation did not match; aborting. Nothing was changed."
        exit 1
    fi
}

# Capture current Deployment replica counts and scale them to 0 so no app pod
# writes (or re-bootstraps rows) mid-reset. The restore runs from a trap.
function capture_and_scale_down() {
    REPLICAS_FILE="$(mktemp -t memql-dbreset-replicas.XXXXXX)"
    kubectl -n "$NS" get deploy \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\n"}{end}' \
        > "$REPLICAS_FILE" 2>/dev/null || true

    if [[ "$DRY_RUN" == true ]]; then
        info "[dry-run] would scale these Deployments to 0 then restore:"
        sed 's/^/    /' "$REPLICAS_FILE" >&2 || true
        return 0
    fi

    # Restore on ANY exit after this point (success or failure).
    trap restore_replicas EXIT

    local name replicas
    while IFS=$'\t' read -r name replicas; do
        [[ -z "$name" ]] && continue
        info "scaling deployment/$name ($replicas -> 0)"
        kubectl -n "$NS" scale "deployment/$name" --replicas=0 >/dev/null 2>&1 || warn "could not scale $name"
    done < "$REPLICAS_FILE"
    info "waiting for app pods to terminate..."
    kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/part-of=memql --timeout=120s >/dev/null 2>&1 || true
}

function restore_replicas() {
    [[ -n "$REPLICAS_FILE" && -f "$REPLICAS_FILE" ]] || return 0
    local name replicas
    while IFS=$'\t' read -r name replicas; do
        [[ -z "$name" || -z "$replicas" ]] && continue
        info "restoring deployment/$name (-> $replicas)"
        kubectl -n "$NS" scale "deployment/$name" --replicas="$replicas" >/dev/null 2>&1 \
            || warn "could not restore $name to $replicas replicas -- check manually"
    done < "$REPLICAS_FILE"
    rm -f "$REPLICAS_FILE" 2>/dev/null || true
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
    echo "RESULT: $ENV database reset to a fresh empty schema. App scaled back up; the owner re-registers via magic-link on next login." >&2
}

function main() {
    parse_arguments "$@"
    validate_arguments
    confirm
    capture_and_scale_down
    wipe_schema
    rebuild_schema
    # restore_replicas runs via the EXIT trap.
    report
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
