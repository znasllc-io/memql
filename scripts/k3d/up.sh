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
# Ports exposed by the cluster:
#   443   -> the front door (traefik; TLS terminated here, routed by hostname)
#   80    -> the front door (redirects)
#   5432  -> postgres (debug access only, never a connection path)
#
# The engine repo runs the engine mesh only -- product frontends belong to their
# own repos, which reuse this script with their own
# --app-name/--overlay-path/--repo-url (#2204). Clients (the Cockpit, SDKs)
# connect to the product-neutral `bff` node -- the client edge -- on demand with:
#   kubectl port-forward -n memql svc/bff 50051:50051
# (the `mcp` node is the MCP-protocol head; it also serves gRPC on :50051.)
#
# The memQL Portal -- the Cockpit's graphical sibling -- is served BY the bff
# and reached through the same front door, at
# https://api.memql.localhost/portal/ (memql#3314). Its bundle is baked into
# the bff image by the Dockerfile's portal stage, so `make dev NODE=bff`
# rebuilds it like any other change to that node.
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
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

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
cap_spec_param "image-registry" "registry to pull node images from (default: the overlay's own, i.e. locally built)"
cap_spec_param "image-tag"      "tag for those images; required with --image-registry"
cap_spec_param "domain"         "front-door apex the cluster is served at (default: memql.localhost)"
cap_spec_param "project-manifest" "path to the AppProject manifest to apply (downstream repos pass their own)"
cap_spec_param "repo-root"      "the memQL checkout to read deploy/ and the target revision from (default: this script's own repository)"
cap_spec_param "no-secrets"     "skip secret seeding (flag)"                        ""

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
ARGOCD_VERSION="v2.13.3"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
ARGOCD_NAMESPACE="argocd"

# The Application's targetRevision is resolved in main(), AFTER --repo-root is
# known -- it is read out of that checkout's git HEAD, so deriving it here would
# read the wrong tree whenever the param is supplied.
#
# It used to be derived here with `|| echo "main"` swallowing the git failure.
# That is what made a packaged run silently reconcile against whatever `main`
# happened to be instead of the intended revision: no path exists, git errors,
# the fallback fires, nothing is reported. require_checkout below now refuses
# that case outright rather than guessing (memql#3491).
REPO_URL="${MEMQL_K3D_REPO_URL:-https://github.com/znasllc-io/memql.git}"
OVERLAY_PATH="${MEMQL_K3D_OVERLAY_PATH:-deploy/k8s/overlays/local}"
APP_NAME="${MEMQL_K3D_APP_NAME:-memql-local}"
EXTRA_PORTS="${MEMQL_K3D_EXTRA_PORTS:-}"
APP_PROJECT="${MEMQL_K3D_APP_PROJECT:-memql}"
PROJECT_MANIFEST="${MEMQL_K3D_PROJECT_MANIFEST:-}"

# The overlay's COMMITTED Ingress hostname (memql#3593). Patches are emitted
# only when the operator's domain differs, so the common case carries no
# generated YAML and `kubectl apply -k` on a clean checkout still works.
readonly OVERLAY_DEFAULT_DOMAIN="memql.localhost"
DOMAIN="$OVERLAY_DEFAULT_DOMAIN"

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
WORKLOADS_READY=false
IMAGE_REGISTRY=""
IMAGE_TAG=""
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

    # The two ways `docker info` fails have completely different remedies, and
    # reporting the second as the first turns a five-second fix into an
    # afternoon (memql#3337). "permission denied ... docker.sock" means the
    # daemon is running fine and THIS USER is not in the docker group --
    # starting Docker again, forever, will not fix it.
    local docker_probe
    if ! docker_probe="$(docker info 2>&1)"; then
        if grep -qi "permission denied" <<<"$docker_probe"; then
            error "The Docker daemon is running, but $(id -un) cannot reach its socket."
            error "You are not in the 'docker' group (groups: $(id -nG))."
            error "Fix it, then start a NEW login session -- the group is read at login:"
            error "    sudo usermod -aG docker \"\$USER\""
            error "    newgrp docker      # this shell only; log out and back in for the rest"
            cap_fail 4 "docker socket is not accessible to $(id -un)"
        fi
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

    # Host-port mappings -- NOT port-forwards, which is the distinction the
    # rest of this comment turns on. The FRONT DOOR is the connection path:
    # traefik terminates TLS on 443 and routes by hostname, exactly as the
    # cloud ingress does. 80 is kept for redirects.
    #
    # 5432 is DEBUG ONLY and is not a connection path -- see
    # docs/public/operate/environment-parity.md. The identity (8085) and
    # livekit (7880) mappings were deleted: 8085 was a second entrance to a
    # service the front door already serves, and 7880 pointed at a Deployment
    # this overlay removes (local voice uses LiveKit Cloud).
    #
    # Downstream stacks add their own LoadBalancer mappings (e.g. a product
    # gRPC head or frontend) via --extra-ports=host:container,... -- k3d LB
    # ports are fixed at cluster-create time, so they must be declared here.
    local port_args=(
        --port "443:443@loadbalancer"
        --port "80:80@loadbalancer"
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

    # Allow Ingress backends of type ExternalName in the k3s-bundled traefik
    # (off by default): the local front door routes a product's host-side
    # dev server (e.g. its SPA on host.k3d.internal) through the same
    # TLS-terminating ingress as the in-cluster services.
    kubectl apply -f - >&2 <<'HELMCFG'
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    providers:
      kubernetesIngress:
        allowExternalNameServices: true
HELMCFG

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

# all_deployments -- every Deployment in the namespace, as `deployment.apps/x`
# names. Empty means ArgoCD has not applied anything yet, which is a different
# problem from a workload that will not start.
function all_deployments() {
    kubectl get deployments -n "$NAMESPACE" -o name 2>/dev/null || true
}

# scaled_up_deployments -- the Deployments that are MEANT to be running, as
# `deployment.apps/x` names.
#
# A Deployment at zero replicas has been deliberately switched off -- the voice
# lane, on any machine with no LiveKit credentials (memql#2416) -- and waiting
# for one is waiting for something nobody asked to start.
#
# READ WITH custom-columns AND FILTERED HERE rather than with a jsonpath
# comparison. `?(@.spec.replicas>0)` would put the whole question in one line of
# a filter language whose numeric comparisons are easy to get subtly wrong and
# which fails silently when it does -- and failing silently in the excluding
# direction drops a node from the wait, which is the failure this function must
# not have.
#
# UNREADABLE MEANS INCLUDED, for the same reason. A replica count that is not a
# plain number is a Deployment we know nothing about, and the safe answer is to
# wait for it: waiting for something that is switched off costs a timeout an
# operator can see, while skipping something that should be running is the false
# green memql#3570 was about.
function scaled_up_deployments() {
    local name replicas
    while read -r name replicas; do
        [[ -n "$name" ]] || continue
        [[ "$replicas" == "0" ]] && continue
        printf 'deployment.apps/%s\n' "$name"
    done < <(kubectl get deployments -n "$NAMESPACE" \
        -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas \
        --no-headers 2>/dev/null || true)
}

# wait_for_workloads -- the memQL Deployments, not just the thing that applies
# them (memql#3570).
#
# WHY THIS EXISTS. ArgoCD being ready means the RECONCILER is up. It says
# nothing about whether the workloads it reconciled ever started, and the
# install trusted it: with the internal CA secrets missing, every node sat in
# ContainerCreating on a FailedMount while k3d.up reported the cluster up,
# `verify-frontdoor` passed (traefik terminates TLS at the ingress with or
# without a backend), and the first step that actually needed a running pod --
# `magicLink`, two steps later -- was left holding a failure it did not cause.
#
# A bring-up that reports success while nothing can start is worse than one
# that fails: it sends the operator looking in the wrong place. So the result
# now carries workloadsReady, and the install graph verifies it.
#
# NOT FATAL HERE. It records the verdict and lets the graph's verify decide, so
# `make up` on a developer machine still prints its summary and leaves them
# with a cluster to inspect rather than an abort in the middle of one.
#
# WHAT IT WAITS FOR IS WHAT IS MEANT TO BE RUNNING (memql#3585). It waited for
# every Deployment in the namespace, voice and voice-agent included -- and those
# two are scaled to 0 on every machine with no LiveKit credentials, because the
# voice binaries fail fast on the missing env by design (memql#2416). So the wait
# could not succeed on most machines, and clusterUp failed on a cluster where all
# nine other nodes were Available.
#
# The set therefore comes from scaled_up_deployments, which asks the cluster
# rather than trusting that the gate ran first. Ordering alone would have fixed
# the symptom and left it flaky: anything that scales the lane back up between
# the gate and the wait puts the failure back.
function wait_for_workloads() {
    local deadline="${MEMQL_K3D_WORKLOAD_TIMEOUT:-300s}" names
    info "Waiting up to ${deadline} for the memQL workloads to become Available..."

    if [[ -z "$(all_deployments)" ]]; then
        warn "no Deployments in ${NAMESPACE} yet -- ArgoCD may not have applied them."
        WORKLOADS_READY=false
        return 0
    fi

    names="$(scaled_up_deployments)"
    if [[ -z "$names" ]]; then
        # Nothing at all is running. No install path produces this, and reading
        # it as success would make an entirely stopped namespace indistinguishable
        # from a healthy one -- which is the class of false green workloadsReady
        # exists to end.
        warn "every Deployment in ${NAMESPACE} is scaled to 0 -- nothing is running."
        WORKLOADS_READY=false
        return 0
    fi

    # shellcheck disable=SC2086
    if kubectl wait --for=condition=Available --timeout="$deadline" \
        -n "$NAMESPACE" $names >&2; then
        info "every memQL workload is Available."
        WORKLOADS_READY=true
        return 0
    fi

    WORKLOADS_READY=false
    warn "not every workload that is scaled up became Available within ${deadline}."
    # The REASON, on stderr, where the operator is already looking. A pod stuck
    # on a missing Secret and a pod that cannot pull its image are different
    # problems with the same symptom, and the difference is in here.
    kubectl get pods -n "$NAMESPACE" >&2 || true
    kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp 2>/dev/null | tail -15 >&2 || true
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

# kustomize_image_overrides -- the `kustomize.images` block for the Application,
# or nothing.
#
# WHY THE APPLICATION AND NOT A SECOND OVERLAY (memql#3572). The local overlay
# renames every node image to `memql-<node>:local` -- images that exist only
# after a developer runs `make dev` to build and import them. That is right for
# the inner loop and impossible for an install: nobody installing a cluster has
# built anything, which is why every pod sat in ImagePullBackOff.
#
# The two need different image VALUES, not a different topology, so this is
# expressed where a value belongs -- `spec.source.kustomize.images` on the
# Application -- rather than as a second overlay, a component, or an
# `if installing` branch in the manifests. Same manifests, same overlay, same
# sync path; only the registry differs. That is the line
# docs/public/operate/environment-parity.md draws.
#
# THE KEYS ARE READ OUT OF THE OVERLAY, never listed here. kustomize matches an
# override on the name as it appears in the BASE manifests, and the overlay
# already names all eight. A hand-kept list in this script would be a second
# copy that silently stops covering a node the day one is added.
function kustomize_image_overrides() {
    [[ -n "$IMAGE_REGISTRY" ]] || return 0

    local kustomization="${REPO_ROOT}/${OVERLAY_PATH}/kustomization.yaml"
    if [[ ! -f "$kustomization" ]]; then
        cap_fail 4 "cannot read ${kustomization} to discover the node images; --image-registry needs the overlay to resolve them against"
    fi

    local names name out=""
    names="$(sed -n 's/^[[:space:]]*-[[:space:]]*name:[[:space:]]*\(.*memql-[a-z]*\)[[:space:]]*$/\1/p' "$kustomization")"
    if [[ -z "$names" ]]; then
        cap_fail 5 "no node images found in ${kustomization}; --image-registry has nothing to override"
    fi
    for name in $names; do
        out+="
      - ${name}=${IMAGE_REGISTRY}/$(basename "$name"):${IMAGE_TAG}"
    done
    printf '      images:%s\n' "$out"
}

# kustomize_host_overrides -- the `kustomize.patches` entries that repoint the
# two front-door Ingresses, or nothing.
#
# WHY THE APPLICATION AND NOT A SECOND OVERLAY. The same reasoning as the image
# overrides above (memql#3572): the two need different hostname VALUES, not a
# different topology, so it is expressed where a value belongs. One overlay, one
# sync path, no `if installing` branch in the manifests.
#
# WHY ONLY THESE TWO. Everything else the domain touches is process config and
# reaches the pods through the memql-domain ConfigMap, derived by
# component/genesis/domain.go. An Ingress host is a Kubernetes API object, so it
# has to be in the render.
#
# ONLY WHEN OVERRIDDEN. The overlay commits OVERLAY_DEFAULT_DOMAIN as its
# Ingress hostname, so the common case emits no generated YAML at all and a
# plain `kubectl apply -k` still produces a working front door.
function kustomize_host_overrides() {
    [[ "$DOMAIN" != "$OVERLAY_DEFAULT_DOMAIN" ]] || return 0
    cat <<EOF
      patches:
        - target:
            kind: Ingress
            name: api-front-door
          patch: |-
            - op: replace
              path: /spec/rules/0/host
              value: api.${DOMAIN}
            - op: replace
              path: /spec/tls/0/hosts/0
              value: api.${DOMAIN}
        - target:
            kind: Ingress
            name: identity-front-door
          patch: |-
            - op: replace
              path: /spec/rules/0/host
              value: identity.${DOMAIN}
            - op: replace
              path: /spec/tls/0/hosts/0
              value: identity.${DOMAIN}
EOF
}

# kustomize_source_block -- the whole `kustomize:` mapping, or nothing.
#
# ONE `kustomize:` KEY. Both emitters produce sub-blocks of the same mapping, so
# the key itself is written here exactly once; a second `kustomize:` line would
# be a duplicate mapping key and ArgoCD would silently keep only one of them.
function kustomize_source_block() {
    local images hosts
    images="$(kustomize_image_overrides)"
    hosts="$(kustomize_host_overrides)"
    [[ -n "$images" || -n "$hosts" ]] || return 0
    # `$(...)` strips trailing newlines, so each part gets one put back. Without
    # it the last image override and the `patches:` key end up on one line and
    # the Application's YAML silently loses the host patches entirely.
    printf '\n    kustomize:\n'
    [[ -n "$images" ]] && printf '%s\n' "$images"
    [[ -n "$hosts" ]] && printf '%s\n' "$hosts"
    return 0
}


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
    local source_overrides
    source_overrides="$(kustomize_source_block)"
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
    path: ${OVERLAY_PATH}${source_overrides}
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
    # --repo-root TRAVELS (memql#3570). seed-secrets.sh reads
    # deploy/k8s/base/tls/gen-internal-ca.sh out of the repository, and defaults
    # its own root to two levels above ITSELF. Run from a packaged extension
    # that is the STAGED tree, which carries scripts/ and nothing else -- so the
    # generator was not found, the internal CA was never seeded, and every node
    # that mounts memql-ca stalled in ContainerCreating forever while the
    # install reported success. memql#3491 threaded this value into k3d.up and
    # stopped there; it has to reach the script that actually reads deploy/.
    bash "${SCRIPT_DIR}/seed-secrets.sh" --namespace="${NAMESPACE}" --repo-root="${REPO_ROOT}" --domain="${DOMAIN}" >&2
    SECRETS_SEEDED=true
}

# refuse_domain_change -- a cluster's domain is chosen once (memql#3593).
#
# Changing it invalidates every passkey (the WebAuthn RP id is derived from the
# domain), every live session and node token (the issuer is
# https://identity.<domain>), the certificate SANs and the hosts block. A
# half-migrated cluster fails at sign-in with an error naming none of those
# things, so the honest move is to refuse and name the one command that does it
# properly. Local data is disposable, which is what makes that cheap.
function refuse_domain_change() {
    local existing
    existing="$(kubectl get configmap memql-domain -n "${NAMESPACE}" \
        -o jsonpath='{.data.MEMQL_DOMAIN}' 2>/dev/null || true)"
    [[ -n "$existing" ]] || return 0
    [[ "$existing" == "$DOMAIN" ]] && return 0
    cap_fail 3 "this cluster is serving ${existing} and --domain says ${DOMAIN}; a domain is chosen once -- it is baked into every passkey, session, certificate and hosts entry. Re-run with --domain=${existing}, or rebuild with: make up-refresh DOMAIN=${DOMAIN}"
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
        echo "  Entry points (front door on 443; *.memql.localhost resolves to 127.0.0.1):"
        echo "    https://identity.memql.localhost         identity (web UI + JWKS)"
        echo "    https://api.memql.localhost/portal/      memQL Portal (graphical ops console)"
        echo "    localhost:5432                           postgres (debug)"
        echo ""
        echo "  Client edge (Cockpit / SDKs), on demand:"
        echo "    kubectl port-forward -n ${NAMESPACE} svc/bff 50051:50051"
        echo "    (mcp is the MCP-protocol head; also serves gRPC on :50051)"
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
# its gate is a no-op on a fresh cluster).
#
# THE BUDGET IS ARGOCD_TIMEOUT, NOT A NUMBER OF ITS OWN (memql#3877).
#
# What this waits for is ArgoCD materialising the Deployments -- which is
# exactly what `--argocd-timeout` already governs, and the operator can already
# raise. It used to wait a private, hardcoded 120s instead, and on a machine
# with slow access to public registries that is simply too short: measured on a
# failing install, the gap from Application creation to the Deployments
# appearing was 124 SECONDS. The gate gave up four seconds early, voice and
# voice-agent started ungated with an empty livekit-secrets, crash-looped by
# design (memql#2416), and wait_for_workloads could not pass. Every install on
# that machine failed the same way -- deterministic, not flaky.
#
# Two budgets for one wait is the defect. A machine slow enough to need
# `MEMQL_K3D_ARGOCD_TIMEOUT=600` is exactly the machine whose Deployments take
# longer than 120s to appear, and raising the documented knob did nothing for
# the gate because the gate was not listening to it.
#
# THE EXPIRY IS LOUD, because it is no longer best-effort in any useful sense.
# With no LiveKit credentials, skipping the gate means the readiness wait two
# lines below CANNOT pass, and that wait is what the install graph verifies. The
# operator then sees `workloadsReady did not satisfy resultTrue` -- a
# consequence, from a step that is not the one that went wrong, naming neither
# voice nor LiveKit. Saying so here is the difference between a five-second fix
# and an afternoon.
function gate_voice_lane_post_sync() {
    local waited=0 interval=5
    while ! kubectl get deploy voice -n "$NAMESPACE" &>/dev/null; do
        sleep "$interval"; waited=$((waited + interval))
        if [ "$waited" -ge "${ARGOCD_TIMEOUT}" ]; then
            warn "voice deployment still absent after ${waited}s -- the voice lane is NOT gated."
            warn "  With no LiveKit credentials its pods fail fast by design, so the workload"
            warn "  wait below cannot pass and this bring-up will report workloadsReady=false."
            warn "  Fix: make secrets   (or raise MEMQL_K3D_ARGOCD_TIMEOUT and re-run)"
            return 0
        fi
    done
    bash "${SCRIPT_DIR}/seed-secrets.sh" --namespace="$NAMESPACE" --repo-root="${REPO_ROOT}" --gate-voice-lane-only >&2 || true
}

# require_checkout <dir> -- refuse a root that is not a real memQL checkout.
#
# A BEHAVIOUR CHANGE, stated plainly: this script used to proceed with whatever
# it derived. It now fails.
#
# The reason is that the two obvious failures are not the dangerous one. A
# missing deploy/argocd/bootstrap makes `kubectl apply -k` fail loudly and
# anyone testing once would see it. The silent one is the target revision: it is
# read from `git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD`, and a directory
# that is not a git tree used to make that fall through to "main" with nothing
# reported -- so a run would reconcile the cluster against whatever main
# happened to be, having been asked for something else.
#
# That cannot be fixed by shipping more files, because the problem is not which
# files are present: the root has to BE a checkout. So the honest move is to
# refuse, with exit 4 (prerequisite missing) naming what is wrong, rather than
# guess a revision.
# resolve_target_revision <repo-root> -- the revision ArgoCD should reconcile the
# MANIFESTS from, which must be the same release the IMAGES come from.
#
# `git rev-parse --abbrev-ref HEAD` answers the literal string "HEAD" for a
# DETACHED checkout, and an install is always detached: clone-stack.sh checks out
# a release TAG. ArgoCD then resolves "HEAD" to the repository's DEFAULT BRANCH,
# so an install that pinned its images to v0.16.1 reconciled its manifests from
# main -- and the tag, the one thing the operator pinned, was silently dropped
# (memql#3602).
#
# The skew is invisible until the two disagree, and then it is total: main's
# overlay stopped setting the identity env because the engine derives it now,
# and a v0.16.1 binary does not derive anything. The install completed green and
# could not be signed into.
#
# So: a detached HEAD sitting exactly on a tag resolves to that TAG; anything
# else keeps the branch name it already used.
function resolve_target_revision() {
    local root="$1" branch tag
    branch="$(git -C "${root}" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
    if [[ "$branch" != "HEAD" ]]; then
        printf '%s' "$branch"
        return 0
    fi
    tag="$(git -C "${root}" describe --tags --exact-match 2>/dev/null || true)"
    if [[ -n "$tag" ]]; then
        printf '%s' "$tag"
        return 0
    fi
    # Detached but not on a tag: the commit itself is still an honest, immutable
    # answer, and unlike "HEAD" it cannot silently mean a different tree later.
    git -C "${root}" rev-parse HEAD 2>/dev/null || printf 'HEAD'
}

function require_checkout() {
    local root="$1"
    if [[ ! -d "$root" ]]; then
        cap_fail 4 "repo-root ${root} does not exist"
    fi
    if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        cap_fail 4 "repo-root ${root} is not a git checkout, so the ArgoCD target revision cannot be read from it -- pass --repo-root=<a memQL checkout> (the install graph passes the directory install.cloneStack created)"
    fi
    if [[ ! -d "${root}/deploy/argocd/bootstrap" ]]; then
        cap_fail 4 "repo-root ${root} has no deploy/argocd/bootstrap -- it is a git checkout, but not of memQL"
    fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    NAMESPACE="$(cap_param namespace "${MEMQL_K3D_NAMESPACE:-memql}")"
    REPO_URL="$(cap_param repo-url "${MEMQL_K3D_REPO_URL:-https://github.com/znasllc-io/memql.git}")"
    K3D_SERVERS="$(cap_param servers "${MEMQL_K3D_SERVERS:-1}")"
    K3D_AGENTS="$(cap_param agents "${MEMQL_K3D_AGENTS:-0}")"
    ARGOCD_TIMEOUT="$(cap_param argocd-timeout "${MEMQL_K3D_ARGOCD_TIMEOUT:-300}")"
    APP_NAME="$(cap_param app-name "${APP_NAME}")"
    OVERLAY_PATH="$(cap_param overlay-path "${OVERLAY_PATH}")"
    EXTRA_PORTS="$(cap_param extra-ports "${EXTRA_PORTS}")"
    APP_PROJECT="$(cap_param app-project "${APP_PROJECT}")"
    # The checkout this run reads deploy/ and its target revision FROM.
    #
    # Defaults to the derived root, so `make up` and every CI lane that calls
    # this script from a checkout with no params behave exactly as before.
    #
    # A packaged extension has no checkout of its own -- the staged tree it runs
    # from carries scripts/ and nothing else -- so the install graph passes the
    # directory `install.cloneStack` put on disk (memql#3491).
    REPO_ROOT="$(cap_param repo-root "${REPO_ROOT}")"
    # The domain is a VALUE, not the shape of the system, so it arrives as a
    # parameter and lands in a ConfigMap -- exactly where environment-parity.md
    # puts a value. The env tier is the FLAG'S DEFAULT, per the capability
    # contract: cap_param has no env tier of its own.
    DOMAIN="$(cap_param domain "$MEMQL_LOCAL_DOMAIN")"
    IMAGE_REGISTRY="$(cap_param image-registry "")"
    IMAGE_TAG="$(cap_param image-tag "")"
    if [[ -n "$IMAGE_REGISTRY" && -z "$IMAGE_TAG" ]]; then
        cap_fail 2 "--image-registry needs --image-tag: an untagged override would follow a moving tag, which is the opposite of what pinning an install means"
    fi
    require_checkout "$REPO_ROOT"
    # Derived from REPO_ROOT, so it is resolved AFTER the param, not before.
    TARGET_REVISION="${MEMQL_K3D_TARGET_REVISION:-$(resolve_target_revision "${REPO_ROOT}")}"
    TARGET_REVISION="$(cap_param revision "${TARGET_REVISION}")"
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
        refuse_domain_change
        seed_secrets
    else
        info "Skipping secret seeding (--no-secrets)."
    fi

    seed_repo_credential
    apply_argocd_app
    # GATE, THEN WAIT (memql#3585). The gate has to come after the Application --
    # the Deployments it scales do not exist until ArgoCD has created them -- and
    # before the wait, so the voice lane is already at zero when the wait works
    # out what is meant to be running. The other order waits for two Deployments
    # the next line exists to switch off.
    gate_voice_lane_post_sync
    wait_for_workloads
    print_summary

    cap_result_set     cluster        "$CLUSTER_NAME"
    cap_result_set     revision       "$TARGET_REVISION"
    cap_result_set     namespace      "$NAMESPACE"
    cap_result_set_raw servers        "$K3D_SERVERS"
    cap_result_set_raw agents         "$K3D_AGENTS"
    cap_result_set_raw clusterCreated "$CLUSTER_CREATED"
    cap_result_set     domain         "$DOMAIN"
    cap_result_set_raw argocdReady    "$ARGOCD_READY"
    cap_result_set_raw workloadsReady "$WORKLOADS_READY"
    cap_result_set_raw secretsSeeded  "$SECRETS_SEEDED"
    cap_ok
}

# Run main only when EXECUTED, so the readiness logic can be sourced and
# exercised on its own -- the steps around it (cluster create, ArgoCD install,
# secret seeding) are far too heavy to drive from a test, which is how the wait
# came to be waiting for a Deployment the next line switches off (memql#3585).
# Executed directly this is identical to the bare `main "$@"` it replaces, and
# the same guard bringup.sh carries for the same reason.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
