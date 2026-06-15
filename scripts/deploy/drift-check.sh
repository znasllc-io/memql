#!/usr/bin/env bash
#
# scripts/deploy/drift-check.sh
# =============================
#
# deployment-v2 Phase 1 (znasllc-io/memql#699). The committed kustomize overlay
# deploy/k8s/overlays/<env> is the SINGLE image authority -- every deployable
# image is pinned there by @sha256: DIGEST. This script is the drift detector
# that enforces it, in two modes:
#
#   --rendered  (default; CI-friendly, no cluster access)
#       Render the overlay and assert EVERY container image is pinned by
#       @sha256: digest -- no floating :tags. Catches a PR that re-introduces a
#       mutable tag (the #684 root cause). This is what CI gates on.
#
#   --live      (needs kubectl context to the env cluster)
#       Compare the digests the overlay pins against the digests actually
#       running in the cluster (pod imageID). Any divergence = out-of-band
#       mutation, which v2 forbids (the overlay, reconciled, is authoritative).
#       Used by aks-deploy.sh's post-apply guard and by a scheduled job; once
#       Argo CD is live (Phase 2) its OutOfSync status is the continuous
#       equivalent.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -uo pipefail.

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
function fail() { echo "DRIFT: $*"; }

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [--rendered|--live] [--env=ENV]

Drift detector for the digest-pinned deploy overlay (deployment-v2 #699).

Options:
    --rendered    Assert the overlay pins every image by \@sha256: digest
                  (no cluster access; CI gate). This is the default.
    --live        Assert live cluster digests == the committed overlay digests
                  (needs a kubectl context to the env cluster).
    --env=ENV     Overlay to check (default: staging) -> deploy/k8s/overlays/ENV
    --help        Show this help.

Exit: 0 = no drift; non-zero = drift / unpinned image / render failure.
EOF
}

function parse_arguments() {
    MODE="rendered"
    ENV="staging"
    NORMALIZE_ARG=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --rendered) MODE="rendered"; shift ;;
            --live)     MODE="live"; shift ;;
            --env=*)    ENV="${1#*=}"; shift ;;
            # Hidden: canonicalize a single image ref to stdout and exit.
            # Exposes normalize_imageref to the test harness (drift_check_test.go);
            # not part of the user-facing CLI, so it is absent from show_help.
            --normalize=*) MODE="normalize"; NORMALIZE_ARG="${1#*=}"; shift ;;
            --help)     show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 2 ;;
        esac
    done
    OVERLAY_DIR="$K8S_DIR/overlays/$ENV"
}

#=============================================================================
# RENDER
#=============================================================================

# Render the overlay; emit one "name@digest-or-tag" per container image line.
function rendered_images() {
    kubectl kustomize "$OVERLAY_DIR" 2>/dev/null \
        | awk '/^[[:space:]]+image:[[:space:]]/ { sub(/^[[:space:]]+image:[[:space:]]*/, ""); gsub(/"/, ""); print }' \
        | sort -u
}

#=============================================================================
# IMAGE-REF NORMALIZATION
#=============================================================================

# Canonicalize a "[registry/]repo@sha256:DIGEST" image ref so two refs that
# name the SAME image compare equal regardless of the implicit Docker Hub
# defaults. Docker reports a pod's imageID with the registry fully qualified
# (docker.io/livekit/livekit-server@sha256:...) while a kustomize overlay
# typically pins the bare form (livekit/livekit-server@sha256:...) -- same
# digest, different surface text. Without this they spuriously diverge (#1441).
#
# The defaults Docker fills in for a bare reference, and which we therefore
# strip back off:
#   - registry host: docker.io / index.docker.io / registry-1.docker.io
#     (the registry's own self-report) -> dropped, leaving the bare repo.
#   - official-image namespace: a leading "library/" on a single-segment repo
#     under Docker Hub (e.g. docker.io/library/redis -> redis) -> dropped.
#
# Refs pinned to any OTHER registry (an ACR host, ghcr.io, quay.io, ...) keep
# their host: the host carries meaning there and two different registries are
# legitimately different images. We only collapse the Docker Hub defaults.
function normalize_imageref() {
    local ref="$1"
    # Strip a leading Docker Hub registry host (with optional :port). These
    # are the only hosts for which the bare form is the canonical equivalent.
    ref="${ref#docker.io/}"
    ref="${ref#index.docker.io/}"
    ref="${ref#registry-1.docker.io/}"
    # Strip the implicit "library/" official-image namespace.
    ref="${ref#library/}"
    printf '%s\n' "$ref"
}

# Normalize a newline-delimited list of image refs (one per line), then
# sort -u so set membership in check_live compares canonical forms.
function normalize_imageref_list() {
    local line
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        normalize_imageref "$line"
    done | sort -u
}

#=============================================================================
# MODES
#=============================================================================

# CI gate: every rendered image MUST be pinned by @sha256: digest.
function check_rendered() {
    info "rendering $OVERLAY_DIR and asserting every image is digest-pinned..."
    if [ ! -f "$OVERLAY_DIR/kustomization.yaml" ]; then
        fail "no overlay at $OVERLAY_DIR"; return 1
    fi
    local imgs unpinned=0 line
    imgs="$(rendered_images)"
    if [ -z "$imgs" ]; then
        fail "overlay rendered no container images (render failure?)"; return 1
    fi
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        if [[ "$line" == *"@sha256:"* ]]; then
            info "pinned: $line"
        else
            fail "NOT digest-pinned: $line"
            unpinned=1
        fi
    done <<< "$imgs"
    if [ "$unpinned" -ne 0 ]; then
        echo ""
        fail "overlay contains a floating tag -- the #684 root cause. Pin it by @sha256: digest."
        return 1
    fi
    echo ""
    info "OK -- every image in $ENV overlay is digest-pinned."
    return 0
}

# Live guard: committed overlay digests == digests running in the cluster.
function check_live() {
    if ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: --live needs a reachable cluster (kubectl context)."; return 2
    fi
    info "comparing committed $ENV overlay digests vs live cluster digests..."
    local committed live
    # Normalize the implicit docker.io/ + library/ defaults on BOTH sides so a
    # bare overlay pin (livekit/livekit-server@sha256:...) matches the live
    # pod's registry-qualified imageID (docker.io/livekit/...@sha256:...) for
    # the same image (#1441).
    committed="$(rendered_images | grep -oE '[^ ]+@sha256:[a-f0-9]+' | normalize_imageref_list)"
    live="$(kubectl get pods -n "$NAMESPACE" \
        -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}' 2>/dev/null \
        | grep -oE '[^ ]+@sha256:[a-f0-9]+' | normalize_imageref_list)"
    if [ -z "$committed" ]; then fail "overlay rendered no digests"; return 1; fi
    if [ -z "$live" ]; then fail "cluster reported no running image digests"; return 1; fi

    # A digest the overlay pins but the cluster is NOT running = un-converged
    # (or out-of-band drift). We only flag overlay-pinned images (the 8 we own);
    # incidental cluster images (ingress-nginx, etc.) are ignored.
    local d miss=0
    while IFS= read -r d; do
        [ -z "$d" ] && continue
        if grep -qxF "$d" <<< "$live"; then
            info "converged: $d"
        else
            fail "overlay pins $d but no live pod runs it (un-converged / drift)"
            miss=1
        fi
    done <<< "$committed"
    if [ "$miss" -ne 0 ]; then
        echo ""
        fail "live cluster does not match the committed overlay. Reconcile (apply the overlay / let Argo sync); do NOT 'kubectl set image'."
        return 1
    fi
    echo ""
    info "OK -- live $ENV cluster matches the committed overlay digests."
    return 0
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"
    case "$MODE" in
        rendered)  check_rendered ;;
        live)      check_live ;;
        normalize) normalize_imageref "$NORMALIZE_ARG" ;;
    esac
}

main "$@"
