#!/usr/bin/env bash
#
# scripts/deploy/aks-rollback.sh
# ==============================
#
# Roll the memQL mesh back to a previous good release.
#
# deployment-v2 Phase 1 (znasllc-io/memql#699) REPLACED the old `kubectl rollout
# undo` rollback. That reverted each Deployment to its previous ReplicaSet, whose
# pod template carried the MANIFEST tag rather than the actual pre-deploy image
# -- the #684 trap (a "rollback" could land you on the wrong version). The
# committed digest overlay deploy/k8s/overlays/<env> is now the single image
# authority, so a rollback is a GIT operation:
#
#     rollback = `git revert` the bad overlay commit  ->  reconcile.
#
# This tool guides + (optionally) performs the reconcile. It NEVER issues
# `kubectl rollout undo` and never mutates images out-of-band. Once Argo CD is
# live (Phase 2 #700), the `git revert` push reconciles automatically and the
# manual re-apply below is unnecessary.
#
# Function-based per the Skills+Scripts convention (CLAUDE.md). set -uo pipefail.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
K8S_DIR="$REPO_ROOT/deploy/k8s"
NAMESPACE="memql"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info() { echo "INFO: $*"; }
function warn() { echo "WARNING: $*"; }
function plan() { echo "  [plan] $*"; }

function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then plan "$*"; return 0; fi
    "$@"
}

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Roll the memQL mesh back to a previous good release. Rollback is a GIT REVERT of
the digest-pinned overlay (deploy/k8s/overlays/<env>), then a reconcile -- NOT
\`kubectl rollout undo\` (which would revert to the manifest tag, #684).

Options:
    --env=ENV     Environment overlay to roll back (default: staging).
    --to=REF      Git ref/commit whose overlay digests you want to restore.
                  The tool prints the exact \`git revert\` for it; with --apply
                  it also re-applies the overlay after you revert.
    --list        Show recent commits that changed the env overlay, then exit.
    --apply       After you have reverted in git, re-apply the overlay to the
                  cluster (re-converge). Omitted by default -- under Argo CD the
                  push reconciles automatically.
    --dry-run     Print what would happen and change nothing.
    --help        Show this help.

Inspect overlay history first with:
    git log --oneline -- deploy/k8s/overlays/$NAMESPACE  (or use --list)

Examples:
    $0 --list                       # recent overlay changes
    $0 --to=<commit>                # print the git revert + reconcile steps
    $0 --to=<commit> --apply        # revert in git yourself, then re-converge
EOF
}

function parse_arguments() {
    ENV="staging"
    TO_REF=""
    DO_APPLY=false
    DO_LIST=false
    DRY_RUN=false
    while [[ $# -gt 0 ]]; do
        case $1 in
            --env=*)  ENV="${1#*=}"; shift ;;
            --to=*)   TO_REF="${1#*=}"; shift ;;
            --list)   DO_LIST=true; shift ;;
            --apply)  DO_APPLY=true; shift ;;
            --dry-run) DRY_RUN=true; shift ;;
            --help)   show_help; exit 0 ;;
            *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
        esac
    done
    OVERLAY_DIR="$K8S_DIR/overlays/$ENV"
}

#=============================================================================
# CORE
#=============================================================================

function check_prerequisites() {
    if [ ! -f "$OVERLAY_DIR/kustomization.yaml" ]; then
        echo "ERROR: no overlay at $OVERLAY_DIR"; exit 1
    fi
    if [ "$DO_APPLY" = true ] && [ "$DRY_RUN" = false ] && ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: --apply needs a reachable cluster (kubectl context)."; exit 1
    fi
}

function show_history() {
    info "recent commits changing the $ENV overlay:"
    git -C "$REPO_ROOT" log --oneline -n 15 -- "$OVERLAY_DIR" 2>/dev/null || warn "git history unavailable"
}

# Print the canonical git-revert rollback procedure for the chosen ref.
function print_revert_procedure() {
    local ref="${TO_REF:-<bad-overlay-commit>}"
    echo "========================================="
    echo "memQL AKS rollback (git revert -- deployment-v2 #699)"
    echo "  Env:      $ENV"
    echo "  Overlay:  $OVERLAY_DIR"
    echo "  Revert:   $ref"
    echo "========================================="
    cat <<EOF

Rollback steps:
  1. Revert the bad overlay change in git (this restores the prior digests):
       git -C "$REPO_ROOT" revert --no-edit $ref
  2. Push:
       git -C "$REPO_ROOT" push
     Under Argo CD (Phase 2) the cluster reconciles automatically -- done.
  3. Until the reconciler is live, re-converge manually (or re-run with --apply):
       kubectl apply -k "$OVERLAY_DIR"

NOTE: no imperative ReplicaSet revert is issued -- that path lands on the
manifest tag, not the prior digest (#684). Git is the source of truth.
EOF
}

# Optional: re-apply the overlay to re-converge (operator reverts in git first).
function reapply_overlay() {
    [ "$DO_APPLY" = false ] && return 0
    warn "re-applying the CURRENT committed overlay to re-converge the cluster."
    warn "ensure you have already 'git revert'ed to the good digests, else this re-applies the bad set."
    run_or_plan kubectl apply -k "$OVERLAY_DIR"
    [ "$DRY_RUN" = false ] && info "verify: bash scripts/deploy/drift-check.sh --live --env=$ENV"
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    if [ "$DO_LIST" = true ]; then
        show_history
        exit 0
    fi
    show_history
    echo ""
    print_revert_procedure
    reapply_overlay
}

main "$@"
