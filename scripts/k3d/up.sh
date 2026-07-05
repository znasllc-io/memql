#!/usr/bin/env bash
#
# scripts/k3d/up.sh
# =================
#
# Capability: k3d.up -- bootstrap a local k3d cluster running ArgoCD pointed at
# the local overlay.
#
# Steps:
#   1. Verify prerequisites (docker, k3d, kubectl, git).
#   2. Create the k3d cluster (single-server, single-agent, with port-forwards).
#   3. Install ArgoCD (pinned to same version as staging: v2.13.3).
#   4. Apply the memql AppProject + Application (local overlay).
#   5. Wait for ArgoCD to come up.
#   6. Print status + next-step instructions.
#
# The ArgoCD Application points to the GitHub repo at the current branch so
# the pure-Argo inner loop is preserved (no direct-apply bypass).  For the
# first cluster boot while a feature branch is being developed, push the branch
# to GitHub first; then run `make up`.  Subsequent iteration uses
# `make dev` (E0.4): rebuild → k3d image import → argo sync.
#
# Port-forwards exposed by the cluster:
#   8085  -> identity
#   7880  -> livekit
#   5432  -> postgres (optional debug access)
#
# The engine repo runs the engine mesh only -- product frontends and carrier
# gRPC heads belong to their own repos, which reuse this script with their own
# --app-name/--overlay-path/--repo-url (#2204). The local engine gRPC head is
# the `mcp` node; reach it on demand with:
#   kubectl port-forward -n memql svc/mcp 50051:50051
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Idempotent: an existing cluster / ArgoCD install is detected and skipped
# (changed=false for that step). Re-running on a healthy cluster is a safe
# no-op.
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (docker/k3d/kubectl/git
#             absent or docker not running) | 5 operation failed (ArgoCD never
#             became ready)
#
# Refs: #2063 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.up" "Bootstrap a local k3d cluster running ArgoCD pointed at the local overlay."
cap_spec_param "cluster"        "k3d cluster name"
cap_spec_param "revision"       "git branch/tag/SHA for the ArgoCD Application"
cap_spec_param "namespace"      "k8s namespace for the memQL stack"
cap_spec_param "repo-url"       "git repo URL for the ArgoCD Application"
cap_spec_param "servers"        "number of k3d server nodes"
cap_spec_param "agents"         "number of k3d agent nodes"
cap_spec_param "argocd-timeout" "ArgoCD readiness timeout (seconds)"
cap_spec_param "app-name"       "ArgoCD Application name"
cap_spec_param "overlay-path"   "kustomize overlay path within the repo"
cap_spec_param "extra-ports"    "additional host:container loadbalancer port mappings, comma-separated"
cap_spec_param "app-project"    "ArgoCD AppProject the Application belongs to"
cap_spec_param "project-manifest" "path to the AppProject manifest to apply (downstream repos pass their own)"
cap_spec_param "no-secrets"     "skip secret seeding (flag)"                        ""

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
ARGOCD_VERSION="v2.13.3"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
ARGOCD_NAMESPACE="argocd"

# The Application's targetRevision: defaults to the current git branch.
TARGET_REVISION="${MEMQL_K3D_TARGET_REVISION:-$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "main")}"
REPO_URL="${MEMQL_K3D_REPO_URL:-https://github.com/znasllc-io/memql.git}"
OVERLAY_PATH="${MEMQL_K3D_OVERLAY_PATH:-deploy/k8s/overlays/local}"
APP_NAME="${MEMQL_K3D_APP_NAME:-memql-local}"
EXTRA_PORTS="${MEMQL_K3D_EXTRA_PORTS:-}"
APP_PROJECT="${MEMQL_K3D_APP_PROJECT:-memql}"
PROJECT_MANIFEST="${MEMQL_K3D_PROJECT_MANIFEST:-}"

# k3d cluster config
K3D_SERVERS="${MEMQL_K3D_SERVERS:-1}"
K3D_AGENTS="${MEMQL_K3D_AGENTS:-0}"

# Timeout for ArgoCD readiness check (seconds). On a fresh cluster all seven
# ArgoCD components pull their images concurrently, so the first boot routinely
# needs more than a couple of minutes on a cold image cache -- default high.
ARGOCD_TIMEOUT="${MEMQL_K3D_ARGOCD_TIMEOUT:-300}"

# Outcome tracking (result envelope + idempotency reporting).
CLUSTER_CREATED=false
ARGOCD_READY=false
SECRETS_SEEDED=false

#=============================================================================
# OUTPUT HELPERS -- delegate to the capability runtime (all logs to STDERR)
#=============================================================================

function info()  { cap_info  "$*"; }
function warn()  { cap_warn  "$*"; }
function error() { cap_error "$*"; }

function section() {
    {
        echo ""
        echo "============================================================"
        echo "  $*"
        echo "============================================================"
    } >&2
}

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    section "Checking prerequisites"

    local missing=()

    if ! command -v docker &>/dev/null; then
        missing+=("docker")
    else
        info "docker: $(docker version --format '{{.Client.Version}}' 2>/dev/null || echo 'found')"
    fi

    if ! command -v k3d &>/dev/null; then
        missing+=("k3d")
    else
        info "k3d: $(k3d version --output json 2>/dev/null | grep '"k3d"' | awk '{print $2}' | tr -d ',"' || k3d version | head -1)"
    fi

    if ! command -v kubectl &>/dev/null; then
        missing+=("kubectl")
    else
        info "kubectl: $(kubectl version --client --output=yaml 2>/dev/null | grep gitVersion | awk '{print $2}' || echo 'found')"
    fi

    if ! command -v git &>/dev/null; then
        missing+=("git")
    else
        info "git: $(git --version | awk '{print $3}')"
    fi

    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing required tools: ${missing[*]}"
        error "Install instructions:"
        error "  docker: https://docs.docker.com/get-docker/"
        error "  k3d:    brew install k3d  (or https://k3d.io)"
        error "  kubectl: brew install kubectl"
        cap_fail 4 "missing required tools: ${missing[*]}"
    fi

    if ! docker info &>/dev/null; then
        error "Docker daemon is not running. Start Docker Desktop first."
        cap_fail 4 "docker daemon is not running"
    fi

    info "All prerequisites satisfied."
}

#=============================================================================
# CLUSTER CREATION
#=============================================================================

function create_cluster() {
    section "Creating k3d cluster '${CLUSTER_NAME}'"

    if k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        info "Cluster '${CLUSTER_NAME}' already exists -- skipping creation."
        info "To recreate: make down && make up"
        return 0
    fi

    info "Creating cluster with ${K3D_SERVERS} server(s) and ${K3D_AGENTS} agent(s)..."

    # Port-forward table (engine mesh only; #2204):
    #   8085:8085  identity HTTP
    #   7880:7880  livekit WebSocket
    #   5432:5432  postgres (debug)
    # The mcp gRPC head (:50051) is reached on demand via
    # `kubectl port-forward -n memql svc/mcp 50051:50051`.
    # Downstream stacks add their own LoadBalancer mappings (e.g. a product
    # gRPC head or frontend) via --extra-ports=host:container,... -- k3d LB
    # ports are fixed at cluster-create time, so they must be declared here.
    local port_args=(
        # Front door: the in-cluster ingress (traefik, bundled with k3s)
        # terminates TLS on 443 and routes by hostname -- same shape as the
        # cloud ingress. 80 is kept for redirects.
        --port "443:443@loadbalancer"
        --port "80:80@loadbalancer"
        --port "8085:8085@loadbalancer"
        --port "7880:7880@loadbalancer"
        --port "5432:5432@loadbalancer"
    )
    if [ -n "${EXTRA_PORTS}" ]; then
        local extra
        IFS=',' read -ra extra <<< "${EXTRA_PORTS}"
        for p in "${extra[@]}"; do
            port_args+=(--port "${p}@loadbalancer")
        done
    fi

    k3d cluster create "${CLUSTER_NAME}" \
        --servers "${K3D_SERVERS}" \
        --agents "${K3D_AGENTS}" \
        "${port_args[@]}" \
        --wait \
        --timeout "120s" >&2

    info "Cluster '${CLUSTER_NAME}' created."
    CLUSTER_CREATED=true
    cap_changed

    # Merge kubeconfig for this cluster.
    k3d kubeconfig merge "${CLUSTER_NAME}" --kubeconfig-merge-default &>/dev/null
    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null
    info "kubectl context set to k3d-${CLUSTER_NAME}."
}

#=============================================================================
# ARGOCD INSTALL
#=============================================================================

function install_argocd() {
    section "Installing ArgoCD ${ARGOCD_VERSION}"

    if kubectl get namespace "${ARGOCD_NAMESPACE}" &>/dev/null; then
        if kubectl get deployment argocd-server -n "${ARGOCD_NAMESPACE}" &>/dev/null; then
            info "ArgoCD already installed in namespace '${ARGOCD_NAMESPACE}' -- skipping."
            ARGOCD_READY=true
            return 0
        fi
    fi

    info "Applying ArgoCD bootstrap (namespace + install.yaml pinned to ${ARGOCD_VERSION})..."
    kubectl apply -k "${REPO_ROOT}/deploy/argocd/bootstrap" >&2

    wait_for_argocd
}

# Wait for ArgoCD to become ready. argocd-server is the gate, but it depends on
# argocd-repo-server + argocd-redis, and on a fresh cluster every component
# pulls its image concurrently -- so `rollout status` can legitimately need
# several minutes the first time. We guard the wait (set -e would otherwise
# abort on a timeout) and, if it does time out, re-check the Deployment's
# Available condition before giving up: the rollout frequently flips ready a
# beat after the status watch returns.
function wait_for_argocd() {
    info "Waiting for ArgoCD server to become ready (timeout: ${ARGOCD_TIMEOUT}s)..."

    if kubectl rollout status deployment/argocd-server \
        -n "${ARGOCD_NAMESPACE}" \
        --timeout="${ARGOCD_TIMEOUT}s" >&2; then
        info "ArgoCD ${ARGOCD_VERSION} is ready."
        ARGOCD_READY=true
        return 0
    fi

    warn "rollout status timed out; re-checking the Available condition directly..."
    if kubectl wait --for=condition=Available deployment/argocd-server \
        -n "${ARGOCD_NAMESPACE}" \
        --timeout=60s >&2; then
        info "ArgoCD ${ARGOCD_VERSION} is ready (became Available just after the rollout wait)."
        ARGOCD_READY=true
        return 0
    fi

    error "ArgoCD server did not become ready within ${ARGOCD_TIMEOUT}s."
    error "Image pulls may still be in flight on a slow connection. Inspect with:"
    error "  kubectl get pods -n ${ARGOCD_NAMESPACE}"
    error "  kubectl describe deployment/argocd-server -n ${ARGOCD_NAMESPACE}"
    error "Then re-run 'make up' (it is idempotent) or raise the timeout:"
    error "  MEMQL_K3D_ARGOCD_TIMEOUT=600 make up"
    cap_fail 5 "ArgoCD server did not become ready within ${ARGOCD_TIMEOUT}s"
}

#=============================================================================
# ARGOCD REPOSITORY CREDENTIAL (private downstream repos)
#=============================================================================

# The engine repo is public, so its Application syncs anonymously. A
# downstream stack whose Application tracks a PRIVATE repo must give ArgoCD
# a repository credential. To keep tokens out of argv (visible in ps), the
# credential is env-only:
#   MEMQL_K3D_REPO_TOKEN     -- token/password for REPO_URL (e.g. a GitHub
#                               fine-grained PAT, or `gh auth token`)
#   MEMQL_K3D_REPO_USERNAME  -- username (default x-access-token, correct
#                               for GitHub token auth)
# When the token env is present, an argocd repository Secret named
# <app-name>-repo is applied idempotently for REPO_URL.
function seed_repo_credential() {
    if [ -z "${MEMQL_K3D_REPO_TOKEN:-}" ]; then
        return 0
    fi

    section "Seeding ArgoCD repository credential for ${REPO_URL}"

    # The manifest travels via stdin (heredoc), never argv, so the token is
    # not visible in `ps`.
    kubectl apply -f - >&2 <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: ${APP_NAME}-repo
  namespace: ${ARGOCD_NAMESPACE}
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  type: git
  url: ${REPO_URL}
  username: ${MEMQL_K3D_REPO_USERNAME:-x-access-token}
  password: ${MEMQL_K3D_REPO_TOKEN}
YAML

    info "Repository credential '${APP_NAME}-repo' applied."
}

#=============================================================================
# ARGOCD APPLICATION
#=============================================================================

function apply_argocd_app() {
    section "Registering Application '${APP_NAME}' in ArgoCD"

    info "Target revision: ${TARGET_REVISION}"
    info "Repo URL:        ${REPO_URL}"
    info "Overlay path:    ${OVERLAY_PATH}"

    # Apply the AppProject first (required before the Application). The
    # engine's memql AppProject restricts sourceRepos to the engine repo, so
    # a downstream stack tracking its own repo supplies its own AppProject
    # manifest via --project-manifest and names it via --app-project.
    kubectl apply -f "${PROJECT_MANIFEST:-${REPO_ROOT}/deploy/argocd/apps/project.yaml}" >&2

    # Generate and apply the local Application manifest.
    # We template the targetRevision so it follows the current branch.
    kubectl apply -f - >&2 <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ${APP_NAME}
  namespace: ${ARGOCD_NAMESPACE}
  finalizers:
    - resources-finalizer.argocd.argoproj.io
  annotations:
    # Record which branch/revision this Application was bootstrapped against.
    memql.io/bootstrap-revision: "${TARGET_REVISION}"
spec:
  project: ${APP_PROJECT}
  source:
    repoURL: ${REPO_URL}
    targetRevision: ${TARGET_REVISION}
    path: ${OVERLAY_PATH}
  destination:
    server: https://kubernetes.default.svc
    namespace: ${NAMESPACE}
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      # The namespace is created by base/namespace.yaml.
      - CreateNamespace=false
      - RespectIgnoreDifferences=true
      - ApplyOutOfSyncOnly=true
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers:
        - /spec/replicas
YAML

    info "Application '${APP_NAME}' registered."
    info "ArgoCD will begin syncing from ${REPO_URL}@${TARGET_REVISION}."
    info "Note: if this is a new branch, ensure it is pushed to GitHub before"
    info "      ArgoCD fetches it (ArgoCD cannot access local filesystem branches)."
}

#=============================================================================
# SEED SECRETS
#=============================================================================

function seed_secrets() {
    section "Seeding local k8s Secrets"

    # Ensure the memql namespace exists before seeding.
    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        info "Namespace '${NAMESPACE}' not yet created by ArgoCD -- creating for secrets..."
        kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >&2
    fi

    info "Running scripts/k3d/seed-secrets.sh..."
    bash "${SCRIPT_DIR}/seed-secrets.sh" --namespace="${NAMESPACE}" >&2
    SECRETS_SEEDED=true
}

#=============================================================================
# STATUS SUMMARY
#=============================================================================

function print_summary() {
    section "Bootstrap complete"

    {
        echo ""
        echo "  Cluster:        k3d-${CLUSTER_NAME}"
        echo "  ArgoCD version: ${ARGOCD_VERSION}"
        echo "  Application:    ${APP_NAME} (${TARGET_REVISION} -> ${OVERLAY_PATH})"
        echo "  Namespace:      ${NAMESPACE}"
        echo ""
        echo "  Entry points (front door on 443; *.local.znas.io resolves to 127.0.0.1):"
        echo "    https://identity.local.znas.io   identity (web UI + JWKS)"
        echo "    ws://localhost:7880              livekit"
        echo "    localhost:5432                   postgres (debug)"
        echo ""
        echo "  Engine gRPC head (mcp), on demand:"
        echo "    kubectl port-forward -n ${NAMESPACE} svc/mcp 50051:50051"
        echo ""
        echo "  Next steps:"
        echo "    1. Watch ArgoCD sync:  kubectl get apps -n argocd -w"
        echo "    2. Check pod status:   kubectl get pods -n ${NAMESPACE}"
        echo "    3. Inner-loop dev:     make dev (E0.4 -- build -> import -> sync)"
        echo ""
        echo "  Tear down:  make down"
        echo ""
    } >&2
}

#=============================================================================
# ENTRY POINT
#=============================================================================


# gate_voice_lane_post_sync re-runs the voice-lane gate once the ArgoCD app
# has created the Deployments (seed-secrets runs BEFORE the app applies, so
# its gate is a no-op on a fresh cluster). Bounded wait; best-effort.
function gate_voice_lane_post_sync() {
    local waited=0
    while ! kubectl get deploy voice -n "$NAMESPACE" &>/dev/null; do
        sleep 5; waited=$((waited + 5))
        if [ "$waited" -ge 120 ]; then
            info "voice deployment not present after ${waited}s; voice-lane gating deferred to 'make secrets'."
            return 0
        fi
    done
    bash "${SCRIPT_DIR}/seed-secrets.sh" --namespace="$NAMESPACE" --gate-voice-lane-only >&2 || true
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    TARGET_REVISION="$(cap_param revision "${TARGET_REVISION}")"
    NAMESPACE="$(cap_param namespace "${MEMQL_K3D_NAMESPACE:-memql}")"
    REPO_URL="$(cap_param repo-url "${MEMQL_K3D_REPO_URL:-https://github.com/znasllc-io/memql.git}")"
    K3D_SERVERS="$(cap_param servers "${MEMQL_K3D_SERVERS:-1}")"
    K3D_AGENTS="$(cap_param agents "${MEMQL_K3D_AGENTS:-0}")"
    ARGOCD_TIMEOUT="$(cap_param argocd-timeout "${MEMQL_K3D_ARGOCD_TIMEOUT:-300}")"
    APP_NAME="$(cap_param app-name "${APP_NAME}")"
    OVERLAY_PATH="$(cap_param overlay-path "${OVERLAY_PATH}")"
    EXTRA_PORTS="$(cap_param extra-ports "${EXTRA_PORTS}")"
    APP_PROJECT="$(cap_param app-project "${APP_PROJECT}")"
    PROJECT_MANIFEST="$(cap_param project-manifest "${PROJECT_MANIFEST}")"
    local skip_secrets
    skip_secrets="$(cap_flag no-secrets)"

    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    # Downstream-stack guards: a non-default repo URL means a product
    # Application, whose revision cannot default to THIS checkout's branch
    # (the engine repo's branch is meaningless in the product repo), and a
    # non-default AppProject needs its manifest supplied.
    local default_repo_url="https://github.com/znasllc-io/memql.git"
    if [ "${REPO_URL}" != "${default_repo_url}" ] && [ -z "$(cap_flag revision)" ] && [ -z "${MEMQL_K3D_TARGET_REVISION:-}" ]; then
        cap_fail 2 "--repo-url targets a non-engine repo; pass --revision (the branch of THAT repo) explicitly"
    fi
    if [ "${APP_PROJECT}" != "memql" ] && [ -z "${PROJECT_MANIFEST}" ]; then
        cap_fail 2 "--app-project=${APP_PROJECT} requires --project-manifest (the engine's AppProject only allowlists the engine repo)"
    fi
    if [ -n "${PROJECT_MANIFEST}" ] && [ ! -f "${PROJECT_MANIFEST}" ]; then
        cap_fail 4 "project manifest not found at ${PROJECT_MANIFEST}"
    fi

    info "memQL k3d bootstrap"
    info "Cluster:   ${CLUSTER_NAME}"
    info "Revision:  ${TARGET_REVISION}"
    info "Namespace: ${NAMESPACE}"

    check_prerequisites
    create_cluster
    install_argocd

    if [[ -z "$skip_secrets" ]]; then
        seed_secrets
    else
        info "Skipping secret seeding (--no-secrets)."
    fi

    seed_repo_credential
    apply_argocd_app
    gate_voice_lane_post_sync
    print_summary

    cap_result_set     cluster        "$CLUSTER_NAME"
    cap_result_set     revision       "$TARGET_REVISION"
    cap_result_set     namespace      "$NAMESPACE"
    cap_result_set_raw servers        "$K3D_SERVERS"
    cap_result_set_raw agents         "$K3D_AGENTS"
    cap_result_set_raw clusterCreated "$CLUSTER_CREATED"
    cap_result_set_raw argocdReady    "$ARGOCD_READY"
    cap_result_set_raw secretsSeeded  "$SECRETS_SEEDED"
    cap_ok
}

main "$@"
