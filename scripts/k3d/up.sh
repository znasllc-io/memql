#!/usr/bin/env bash
#
# scripts/k3d/up.sh
# =================
#
# Bootstrap a local k3d cluster running ArgoCD pointed at the local overlay.
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
# to GitHub first; then run `make k3d-up`.  Subsequent iteration uses
# `make k3d-dev` (E0.4): rebuild → k3d image import → argo sync.
#
# Port-forwards exposed by the cluster:
#   8080  -> copresent (SPA)
#   8085  -> identity
#   7880  -> livekit
#   50051 -> bff gRPC
#   5432  -> postgres (optional debug access)
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.
#
# Refs: #2063 #2061

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
ARGOCD_VERSION="v2.13.3"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
ARGOCD_NAMESPACE="argocd"

# The Application's targetRevision: defaults to the current git branch.
TARGET_REVISION="${MEMQL_K3D_TARGET_REVISION:-$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "main")}"
REPO_URL="${MEMQL_K3D_REPO_URL:-https://github.com/znasllc-io/memql.git}"
OVERLAY_PATH="deploy/k8s/overlays/local"

# k3d cluster config
K3D_SERVERS="${MEMQL_K3D_SERVERS:-1}"
K3D_AGENTS="${MEMQL_K3D_AGENTS:-0}"

# Timeout for ArgoCD readiness check (seconds)
ARGOCD_TIMEOUT="${MEMQL_K3D_ARGOCD_TIMEOUT:-120}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }
function error() { echo "ERROR: $*" >&2; }

function section() {
    echo ""
    echo "============================================================"
    echo "  $*"
    echo "============================================================"
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
        exit 1
    fi

    if ! docker info &>/dev/null; then
        error "Docker daemon is not running. Start Docker Desktop first."
        exit 1
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
        info "To recreate: make k3d-down && make k3d-up"
        return 0
    fi

    info "Creating cluster with ${K3D_SERVERS} server(s) and ${K3D_AGENTS} agent(s)..."

    # Port-forward table:
    #   8080:80    copresent SPA (nginx / ingress)
    #   8085:8085  identity HTTP
    #   7880:7880  livekit WebSocket
    #   50051:50051 bff gRPC
    #   5432:5432  postgres (debug)
    k3d cluster create "${CLUSTER_NAME}" \
        --servers "${K3D_SERVERS}" \
        --agents "${K3D_AGENTS}" \
        --port "8080:80@loadbalancer" \
        --port "8085:8085@loadbalancer" \
        --port "7880:7880@loadbalancer" \
        --port "50051:50051@loadbalancer" \
        --port "5432:5432@loadbalancer" \
        --wait \
        --timeout "120s"

    info "Cluster '${CLUSTER_NAME}' created."

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
            return 0
        fi
    fi

    info "Applying ArgoCD bootstrap (namespace + install.yaml pinned to ${ARGOCD_VERSION})..."
    kubectl apply -k "${REPO_ROOT}/deploy/argocd/bootstrap"

    info "Waiting for ArgoCD server to become ready (timeout: ${ARGOCD_TIMEOUT}s)..."
    kubectl rollout status deployment/argocd-server \
        -n "${ARGOCD_NAMESPACE}" \
        --timeout="${ARGOCD_TIMEOUT}s"

    info "ArgoCD ${ARGOCD_VERSION} is ready."
}

#=============================================================================
# ARGOCD APPLICATION
#=============================================================================

function apply_argocd_app() {
    section "Registering memQL Application in ArgoCD"

    info "Target revision: ${TARGET_REVISION}"
    info "Repo URL:        ${REPO_URL}"
    info "Overlay path:    ${OVERLAY_PATH}"

    # Apply the memql AppProject first (required before the Application).
    kubectl apply -f "${REPO_ROOT}/deploy/argocd/apps/project.yaml"

    # Generate and apply the local Application manifest.
    # We template the targetRevision so it follows the current branch.
    kubectl apply -f - <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: memql-local
  namespace: ${ARGOCD_NAMESPACE}
  finalizers:
    - resources-finalizer.argocd.argoproj.io
  annotations:
    # Record which branch/revision this Application was bootstrapped against.
    memql.io/bootstrap-revision: "${TARGET_REVISION}"
spec:
  project: memql
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

    info "Application 'memql-local' registered."
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
        kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    fi

    info "Running scripts/k3d/seed-secrets.sh..."
    bash "${SCRIPT_DIR}/seed-secrets.sh" --namespace="${NAMESPACE}"
}

#=============================================================================
# STATUS SUMMARY
#=============================================================================

function print_summary() {
    section "Bootstrap complete"

    echo ""
    echo "  Cluster:        k3d-${CLUSTER_NAME}"
    echo "  ArgoCD version: ${ARGOCD_VERSION}"
    echo "  Application:    memql-local (${TARGET_REVISION} -> ${OVERLAY_PATH})"
    echo "  Namespace:      ${NAMESPACE}"
    echo ""
    echo "  Port-forwards (via k3d LoadBalancer):"
    echo "    http://localhost:8080   copresent SPA"
    echo "    http://localhost:8085   identity"
    echo "    ws://localhost:7880     livekit"
    echo "    localhost:50051         bff gRPC"
    echo "    localhost:5432          postgres (debug)"
    echo ""
    echo "  Next steps:"
    echo "    1. Watch ArgoCD sync:  kubectl get apps -n argocd -w"
    echo "    2. Check pod status:   kubectl get pods -n ${NAMESPACE}"
    echo "    3. Inner-loop dev:     make k3d-dev (E0.4 -- build -> import -> sync)"
    echo ""
    echo "  Tear down:  make k3d-down"
    echo ""
}

#=============================================================================
# PARSE ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Bootstrap a local k3d cluster with ArgoCD pointing at the local overlay.

Options:
    --cluster=NAME        k3d cluster name (default: ${CLUSTER_NAME})
    --revision=REV        Git branch/tag/SHA for the ArgoCD Application
                          (default: current branch = ${TARGET_REVISION})
    --namespace=NS        k8s namespace for the memQL stack (default: ${NAMESPACE})
    --servers=N           Number of k3d server nodes (default: ${K3D_SERVERS})
    --agents=N            Number of k3d agent nodes (default: ${K3D_AGENTS})
    --no-secrets          Skip secret seeding (useful if already seeded)
    --help                Show this help message

Environment overrides:
    MEMQL_K3D_CLUSTER           cluster name
    MEMQL_K3D_TARGET_REVISION   git revision for ArgoCD
    MEMQL_K3D_NAMESPACE         k8s namespace
    MEMQL_K3D_REPO_URL          git repo URL
    MEMQL_K3D_SERVERS           server node count
    MEMQL_K3D_AGENTS            agent node count
    MEMQL_K3D_ARGOCD_TIMEOUT    ArgoCD readiness timeout (seconds)

Example:
    $0                         # bootstrap with current branch
    $0 --revision=main         # bootstrap against main
    $0 --servers=2 --agents=1  # multi-node (see E0.5 / #2067)
EOF
}

SKIP_SECRETS=false

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cluster=*)  CLUSTER_NAME="${1#*=}";        shift ;;
            --revision=*) TARGET_REVISION="${1#*=}";     shift ;;
            --namespace=*) NAMESPACE="${1#*=}";          shift ;;
            --servers=*)  K3D_SERVERS="${1#*=}";         shift ;;
            --agents=*)   K3D_AGENTS="${1#*=}";          shift ;;
            --no-secrets) SKIP_SECRETS=true;             shift ;;
            --help)       show_help; exit 0 ;;
            *) error "Unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    info "memQL k3d bootstrap"
    info "Cluster:   ${CLUSTER_NAME}"
    info "Revision:  ${TARGET_REVISION}"
    info "Namespace: ${NAMESPACE}"

    check_prerequisites
    create_cluster
    install_argocd

    if [ "${SKIP_SECRETS}" = false ]; then
        seed_secrets
    else
        info "Skipping secret seeding (--no-secrets)."
    fi

    apply_argocd_app
    print_summary
}

main "$@"
