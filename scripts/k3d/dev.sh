#!/usr/bin/env bash
#
# scripts/k3d/dev.sh
# ==================
#
# Capability: k3d.dev -- inner-loop dev command: build one or more node images
# locally, import them into the k3d cluster, and restart the relevant
# Deployments.
#
# Design
# ------
# The local overlay pins images to a ':local' tag (not @sha256 digests)
# with imagePullPolicy: IfNotPresent. k3d pre-pulls images into its
# containerd runtime at import time, so pods always get the imported
# image rather than pulling from a registry.
#
# Because the manifest tag (':local') is stable, ArgoCD sees no diff
# after a rebuild -- the change is the image CONTENT, not the reference.
# We therefore trigger a rolling restart on the affected Deployment(s)
# after import, which causes kubelet to start fresh containers using the
# newly-imported image.
#
# This is NOT a manifest bypass: ArgoCD owns the Deployment spec;
# 'kubectl rollout restart' only touches the pod template's restart
# annotation. ArgoCD's selfHeal will not revert this because the restart
# annotation value is not part of the desired manifest. (selfHeal ignores
# pod restarts.)
#
# Node images and the carrier override
# ------------------------------------
# By default every app node type builds from THIS repo's Dockerfile
# (--build-arg BUILD_TAGS=<type>) and is tagged memql-<type>:local:
#     identity  voice  mcp  cognition  agent  planner  workbench
#
# A downstream product repo that ships its own DSL/integrations on top of
# the engine (a "carrier" image) reuses this script and overrides a subset
# of node types via the carrier hook:
#     --carrier-repo=PATH      the carrier repo (its Dockerfile is used)
#     --carrier-nodes=a,b,c    node types built from the carrier Dockerfile
#     --carrier-context=PATH   docker build context (default: the carrier
#                              repo's parent directory, i.e. the workspace
#                              root, so the carrier Dockerfile can mount
#                              both source trees at compile time)
# Carrier builds pass --build-arg BUILD_TAGS=<type> and tag the image
# memql-<type>:local -- the SAME name the overlay pins -- so overriding a
# node is transparent to the manifests. The engine repo itself never sets
# these; they exist for downstream repos' Makefiles.
#
#   LOCAL infra (pulled from public registries, not rebuilt here):
#     azurite     mcr.microsoft.com/azure-storage/azurite -- pull + import
#     redis       redis -- pull + import
#     livekit     livekit/livekit-server -- pull + import
#
#   LOCAL database (BUILT here, not pulled -- memql#3846):
#     memql-db    deploy/db-image -- build + import when absent from the cluster
#                 The local database is a CloudNativePG Cluster running this
#                 image; it exists in no registry, so an absent import is an
#                 ErrImagePull rather than a slow first start. Refresh it with
#                 `make dev PULL_INFRA=1` or `make db-image IMPORT=1`.
#
# Rebuilding a wizard-installed cluster
# ------------------------------------
# Two params exist for the rebuild the editor extension drives, and both name
# something this script used to assume:
#
#   --repo-root=DIR      the checkout the images are BUILT FROM. The packaged
#                        extension runs a STAGED copy of scripts/ with no Go
#                        source beside it, so "this script's own repository" is
#                        not a MemQL tree there; the build has to be pointed at
#                        the checkout the install cloned. Mirrors k3d.up's
#                        --repo-root, and defaults the same way.
#   --image-source=checkout
#                        a WIZARD install pins the Application's node images to
#                        a released registry tag
#                        (spec.source.kustomize.images, written by
#                        `k3d.up --image-registry/--image-tag`), so a bare
#                        rebuild imports images nothing references. Under this
#                        flag the node overrides are removed AFTER the images
#                        are imported, which lets the overlay's own ':local'
#                        references apply, and ArgoCD's resulting sync rolls
#                        the pods. The DATABASE OPERAND's override is KEPT: it
#                        is not a node, it is versioned on the PostgreSQL axis,
#                        and CNPG refuses an imageName whose tag it cannot
#                        parse (memql#4063). Nothing is patched without the
#                        flag, so `make dev` is unchanged.
#
# An install, upgrade or repair rewrites those overrides, so it returns the
# cluster to released images. Reaching --app-name is how a cluster whose
# Application is not the default `memql-local` is addressed -- and a name no
# Application answers to is REFUSED (exit 4) rather than read as "there was
# nothing to patch".
#
# The run then proves the patch reached the pods: `Synced` is ArgoCD's own
# bookkeeping and can be a stale read, so the gate that actually has to hold is
# every Deployment naming memql-<node>:local (exit 5 when it never does).
#
# Usage
# -----
#   make dev                          # rebuild + restart all app nodes
#   make dev NODE=cognition           # rebuild + restart one node
#   make dev NODE=mcp,cognition       # comma-separated list
#   make dev PULL_INFRA=1            # pull + re-import infra images
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Idempotent: each invocation rebuilds + re-imports the requested images and
# rolls the Deployments; safe to re-run.
#
# Exit codes: 0 ok | 2 bad param (unknown node type, unknown image-source) |
#             4 prerequisite missing (docker/k3d/kubectl absent, cluster not
#             running, carrier repo or repo-root missing, no such ArgoCD
#             Application) | 5 operation failed (an image build, an import, an
#             Application patch, a sync that never converged, or pods that never
#             came to name the locally built images)
#
# Refs: #2066 #2061 #2221 #4245

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/engine_build_args.sh
source "${SCRIPT_DIR}/../lib/engine_build_args.sh"

cap_init "k3d.dev" "Build node image(s) locally, import into k3d, and restart Deployments."
cap_spec_param "node"            "node type(s) to rebuild, comma-separated (default: all app nodes)" ""
cap_spec_param "repo-root"       "the MemQL checkout to build from (default: this script's own repository)"
cap_spec_param "app-name"        "ArgoCD Application name (default: \$MEMQL_K3D_APP_NAME or memql-local)"
cap_spec_param "image-source"    "checkout: point the Application's node images at the locally built :local images, keeping the database operand override (default: leave the overrides as they are)" ""
cap_spec_param "pull-infra"      "pull + import infra images (flag)"                                 ""
cap_spec_param "cluster"         "k3d cluster name"
cap_spec_param "namespace"       "k8s namespace"
cap_spec_param "no-wait"         "skip rollout status wait (flag)"                                   ""
cap_spec_param "carrier-repo"    "downstream carrier repo whose Dockerfile builds the carrier nodes" ""
cap_spec_param "carrier-nodes"   "node types built from the carrier Dockerfile, comma-separated"     ""
cap_spec_param "carrier-context" "docker build context for carrier builds (default: carrier repo's parent)" ""

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
LOCAL_TAG="local"

# The ArgoCD Application this cluster is reconciled by, and the namespace ArgoCD
# itself runs in -- the literal "argocd", exactly as up.sh has it. Both are read
# only under --image-source=checkout; a plain `make dev` never touches the
# Application. Resolved from params in main().
ARGOCD_NAMESPACE="argocd"
APP_NAME=""
IMAGE_SOURCE=""

# App node types buildable from this repo's Dockerfile. The default `make dev`
# set matches the Deployments in deploy/k8s/overlays/local.
DEFAULT_APP_NODES=(identity bff voice mcp cognition agent planner workbench edge)

# Every node type this script can address (superset: node types a downstream
# carrier overlay may add, e.g. bff, are valid targets too).
VALID_NODES=(identity voice mcp bff cognition agent planner workbench edge)

# Carrier override (resolved from params in main; empty = engine-only).
CARRIER_REPO=""
CARRIER_CONTEXT=""
CARRIER_NODES=()

# Infra images (pull from upstream, import into k3d).
#
# The database is NOT here. It used to be `timescale/timescaledb:2.19.1-pg16`,
# pulled like the rest; since memql#3846 the local database is a CloudNativePG
# Cluster running `memql-db:16-dev`, which is BUILT here (deploy/db-image) and
# exists in no registry. ensure_db_image below handles it, and the difference
# matters: everything in this list can be pulled by k3s on its own if the
# import is skipped, and the operand image cannot -- skipping it leaves the
# database pod in ErrImagePull indefinitely.
INFRA_IMAGES=(
    "mcr.microsoft.com/azure-storage/azurite:3.34.0"
    "redis:7-alpine"
    "livekit/livekit-server:v1.8"
)

# The locally-built database operand image, and the tag the local overlay's
# Cluster CR names (deploy/k8s/overlays/local/database.yaml).
DB_IMAGE="memql-db:16-dev"

# Outcome tracking (result envelope + idempotency reporting).
REBUILT_COUNT=0
RESTARTED=false
INFRA_PULLED=false
DB_IMAGE_IMPORTED=false
OVERRIDES_PATCHED=false
# What was BUILT, for the envelope. A caller that asked for a rebuild of a
# checkout it named has no other way to learn which commit it got -- and a
# dirty tree is exactly the case where the ref alone is not the answer.
CHECKOUT_COMMIT=""
CHECKOUT_REF=""
CHECKOUT_DIRTY=0
# Set from the --pull-infra flag in main(); read by ensure_db_image, which runs
# unconditionally and so cannot take the flag from main's local.
PULL_INFRA=false

#=============================================================================
# OUTPUT HELPERS -- delegate to the capability runtime (all logs to STDERR)
#=============================================================================

function info()  { cap_info  "$*"; }
function warn()  { cap_warn  "$*"; }
function error() { cap_error "$*"; }

function section() {
    {
        echo ""
        echo "------------------------------------------------------------"
        echo "  $*"
        echo "------------------------------------------------------------"
    } >&2
}

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    local missing=()

    for tool in docker k3d kubectl; do
        if ! command -v "$tool" &>/dev/null; then
            missing+=("$tool")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing required tools: ${missing[*]}"
        cap_fail 4 "missing required tools: ${missing[*]}"
    fi

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        error "Cluster '${CLUSTER_NAME}' is not running. Run 'make up' first."
        cap_fail 4 "cluster '${CLUSTER_NAME}' is not running"
    fi

    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null || true
}

#=============================================================================
# NODE TYPE CLASSIFICATION
#=============================================================================

function is_valid_node() {
    local node="$1"
    for n in "${VALID_NODES[@]}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function is_carrier_node() {
    local node="$1"
    for n in "${CARRIER_NODES[@]+"${CARRIER_NODES[@]}"}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function deployment_name_for_node() {
    # Node type == k8s Deployment name (matches base/ manifest names).
    local node="$1"
    if ! is_valid_node "$node"; then
        cap_fail 2 "unknown node type: $node"
    fi
    echo "$node"
}

function image_name_for_node() {
    # Map node type to the in-cluster image ref the local overlay's pods
    # pull. These MUST match the `newName`s in the local overlay's
    # `images:` block -- k3d imports under exactly this name so the
    # kubelet resolves it locally instead of trying to pull from a
    # registry. Uniform for engine and carrier builds, so a carrier
    # override is transparent to the manifests.
    local node="$1"
    echo "memql-${node}:${LOCAL_TAG}"
}

#=============================================================================
# PRE-WARM THE BUILDKIT FRONTEND
#=============================================================================

function build_frontend_ref() {
    # Read the `# syntax=` directive out of the Dockerfile rather than
    # hardcoding the image here. Two copies of that reference would drift, and
    # the drift is silent: this would pre-pull an image the build does not use,
    # leaving the exact failure it exists to prevent while looking like it ran.
    sed -n '1,5{s/^#[[:space:]]*syntax=[[:space:]]*//p;}' "${REPO_ROOT}/Dockerfile" | head -1
}

function prewarm_build_frontend() {
    # WHY. `Dockerfile:1` is `# syntax=docker/dockerfile:1`, which makes
    # BuildKit resolve an EXTERNAL frontend image before any of this
    # repository's code is compiled. On a cold content store -- a fresh
    # machine, after a prune, or a slow moment on registry-1.docker.io -- that
    # resolution can time out, and it does so PARTWAY THROUGH a multi-image
    # build: seven of eight images already built and imported, and a failure
    # naming Docker Hub rather than anything in this repository (memql#3873).
    #
    # MEASURED, because the fix turns on a fact worth checking rather than
    # assuming. Tagging the frontend to `unreachable-registry.invalid/...` --
    # a reserved TLD that cannot resolve -- and building against it succeeds
    # in 0.23s. So BuildKit's docker driver resolves the `# syntax=` reference
    # from the LOCAL IMAGE STORE and makes no round trip when the image is
    # already there. Pre-pulling it is therefore sufficient, not merely
    # hopeful.
    #
    # WHY NOT WRAP THE BUILD IN retry.sh. Because that retries the WHOLE
    # build, including a Go compile failure -- the anti-pattern retry.sh's own
    # header warns about. A broken build would burn three full compiles and
    # report the third failure. The network operation is the pull, so the
    # pull is what gets retried.
    #
    # NON-FATAL, and deliberately. If the pull fails but the frontend is
    # already present, the build works and refusing here would break it for no
    # reason. If it fails and the frontend is absent, the build fails anyway --
    # but now after a warning that named Docker Hub BEFORE seven images were
    # built, which is the whole complaint.
    local frontend
    frontend="$(build_frontend_ref)"
    if [ -z "${frontend}" ]; then
        # No syntax directive: nothing external to resolve, nothing to warm.
        # Not an error -- it is the state dropping the directive would leave.
        return 0
    fi

    if docker image inspect "${frontend}" >/dev/null 2>&1; then
        info "BuildKit frontend ${frontend} already in the local image store."
        return 0
    fi

    info "Pre-pulling BuildKit frontend ${frontend} (cold store -- see memql#3873)..."
    if "${REPO_ROOT}/scripts/ci/retry.sh" --attempts=3 --delay=5 -- \
        docker pull "${frontend}" >&2; then
        info "BuildKit frontend ${frontend} is warm."
        return 0
    fi

    warn "Could not pre-pull the BuildKit frontend ${frontend}."
    warn "Docker Hub looks unreachable. Every image build below resolves this"
    warn "frontend first, so they are likely to fail on a network timeout that"
    warn "names Docker Hub rather than this repository (memql#3873)."
    warn "Continuing anyway -- the build still succeeds if the frontend is cached."
    return 0
}

#=============================================================================
# BUILD ENGINE IMAGE (from this repo's Dockerfile)
#=============================================================================

function build_engine_node() {
    local node="$1"
    local image
    image="$(image_name_for_node "$node")"

    section "Building engine image: ${node} -> ${image}"

    # nodeType -> build-args mapping shared with the deploy.buildImage
    # capability backend (scripts/lib/engine_build_args.sh, memql#2379) so
    # the two local build paths cannot drift.
    engine_build_args_for_node "$node"
    if [[ "$node" == "voice" ]]; then
        warn "voice node requires libopus headers -- building from repo Dockerfile."
        warn "If the build fails with 'opus.h not found', see docs/public/build/build-tags.md."
    fi

    docker build \
        "${ENGINE_BUILD_ARGS[@]}" \
        --target "${ENGINE_BUILD_TARGET}" \
        --tag "${image}" \
        --file "${REPO_ROOT}/Dockerfile" \
        "${REPO_ROOT}" >&2

    info "Built ${image}."
}

#=============================================================================
# BUILD CARRIER IMAGE (from the downstream carrier repo's Dockerfile)
#=============================================================================

function build_carrier_node() {
    local node="$1"
    local image
    image="$(image_name_for_node "$node")"

    section "Building carrier image: ${node} -> ${image}"

    if [ ! -d "${CARRIER_REPO}" ]; then
        error "Carrier repo not found at ${CARRIER_REPO}."
        error "Carrier node builds (--carrier-nodes=${CARRIER_NODES[*]}) need the"
        error "carrier repo checked out. Pass --carrier-repo=<path> or set"
        error "MEMQL_CARRIER_REPO to its location."
        cap_fail 4 "carrier repo not found at ${CARRIER_REPO}"
    fi

    docker build \
        --build-arg BUILD_TAGS="${node}" \
        --build-arg CGO_ENABLED=0 \
        --tag "${image}" \
        --file "${CARRIER_REPO}/Dockerfile" \
        "${CARRIER_CONTEXT}" >&2

    info "Built ${image}."
}

#=============================================================================
# IMPORT IMAGE INTO K3D
#=============================================================================

function import_image() {
    local image="$1"

    info "Importing ${image} into k3d cluster '${CLUSTER_NAME}'..."
    k3d image import "${image}" --cluster "${CLUSTER_NAME}" >&2
    info "Imported ${image}."
}

#=============================================================================
# RESTART DEPLOYMENT
#=============================================================================

function restart_deployment() {
    local node="$1"
    local deployment
    deployment="$(deployment_name_for_node "$node")"

    if ! kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
        info "Deployment '${deployment}' not present in namespace '${NAMESPACE}' -- image imported; skipping restart."
        return 0
    fi

    info "Rolling restart of Deployment '${deployment}' in namespace '${NAMESPACE}'..."
    kubectl rollout restart deployment/"${deployment}" -n "${NAMESPACE}" >&2
    info "Restart initiated. Watch: kubectl rollout status deployment/${deployment} -n ${NAMESPACE}"
}

#=============================================================================
# PROCESS ONE NODE
#=============================================================================

function process_node() {
    local node="$1"

    if ! is_valid_node "$node"; then
        error "Unknown node type: '${node}'. Valid values: ${VALID_NODES[*]}"
        cap_fail 2 "unknown node type: ${node}"
    fi

    if is_carrier_node "$node"; then
        build_carrier_node "$node"
    else
        build_engine_node "$node"
    fi
    import_image "$(image_name_for_node "$node")"
    # Under --image-source=checkout the Application's node overrides are dropped
    # once every image is imported, and the sync that follows rolls the pods --
    # so restarting here would roll them onto the images they are still pinned
    # to. main() restarts explicitly when there was nothing to patch.
    if [[ "$IMAGE_SOURCE" != "checkout" ]]; then
        restart_deployment "$node"
    fi

    REBUILT_COUNT=$((REBUILT_COUNT + 1))
    RESTARTED=true
    cap_changed
}

#=============================================================================
# PULL + IMPORT INFRA IMAGES
#=============================================================================

function pull_and_import_infra() {
    section "Pulling and importing infra images"

    for image in "${INFRA_IMAGES[@]}"; do
        info "Pulling ${image}..."
        docker pull "${image}" >&2
        info "Importing ${image} into k3d..."
        k3d image import "${image}" --cluster "${CLUSTER_NAME}" >&2
        info "Done: ${image}"
    done

    INFRA_PULLED=true
    cap_changed
}

#=============================================================================
# DATABASE OPERAND IMAGE (built here, not pulled)
#=============================================================================

# cluster_holds_db_image -- true when the k3d node's containerd already has the
# operand image under this reference.
#
# Presence, not freshness. A rebuilt image under the same tag is NOT detected,
# which is deliberate rather than a shortcut: the import is ~500MB and paying it
# on every inner-loop `make dev` is exactly the cost developers notice, while
# the database image changes about as often as an operator version does.
# Refreshing it is an explicit act -- `make dev PULL_INFRA=1` or
# `make db-image IMPORT=1`.
#
# A probe that cannot run returns false, so the fallback is to import.
function cluster_holds_db_image() {
    docker exec "k3d-${CLUSTER_NAME}-server-0" ctr -n k8s.io images ls -q 2>/dev/null \
        | grep -q "${DB_IMAGE}"
}

function ensure_db_image() {
    if [[ "$IMAGE_SOURCE" == "checkout" ]]; then
        info "Skipping the database operand image: --image-source=checkout leaves the operand override in place (the database is not a node)."
        return 0
    fi

    section "Ensuring the database operand image (${DB_IMAGE})"

    if [[ "$PULL_INFRA" != "true" ]] && cluster_holds_db_image; then
        info "${DB_IMAGE} already present in cluster '${CLUSTER_NAME}' -- skipping."
        info "  refresh it with: make dev PULL_INFRA=1   (or: make db-image IMPORT=1)"
        return 0
    fi

    # Smoke test skipped here: it takes ~40s and belongs to the build lane
    # (`make db-image`, and the CI push gate), not to an inner-loop rebuild.
    info "Building ${DB_IMAGE}..."
    bash "${SCRIPT_DIR}/../db-image/build.sh" --tag=16-dev --smokeTest=false >/dev/null \
        || cap_fail 5 "building ${DB_IMAGE} failed"

    info "Importing ${DB_IMAGE} into k3d..."
    k3d image import "${DB_IMAGE}" --cluster "${CLUSTER_NAME}" >&2 \
        || cap_fail 5 "k3d image import of ${DB_IMAGE} failed"

    DB_IMAGE_IMPORTED=true
    cap_changed
}

#=============================================================================
# WAIT FOR ROLLOUTS
#=============================================================================

function wait_for_rollouts() {
    local nodes=("$@")

    section "Waiting for rollouts to complete"

    for node in "${nodes[@]}"; do
        local deployment
        deployment="$(deployment_name_for_node "$node")"

        if kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
            info "Waiting for ${deployment}..."
            kubectl rollout status deployment/"${deployment}" \
                -n "${NAMESPACE}" \
                --timeout=120s >&2 || warn "${deployment} rollout did not complete in 120s -- check 'kubectl get pods -n ${NAMESPACE}'"
        fi
    done
}

#=============================================================================
# REBUILDING A WIZARD-INSTALLED CLUSTER (--repo-root, --image-source=checkout)
#=============================================================================

# require_build_checkout -- the directory the images are built FROM.
function require_build_checkout() {
    local root="$1"
    if [[ ! -d "$root" ]]; then
        cap_fail 4 "repo-root ${root} does not exist"
    fi
    if [[ ! -f "${root}/Dockerfile" ]]; then
        cap_fail 4 "repo-root ${root} has no Dockerfile -- it is not a MemQL checkout"
    fi
    if [[ ! -f "${root}/deploy/k8s/overlays/local/kustomization.yaml" ]]; then
        cap_fail 4 "repo-root ${root} has no deploy/k8s/overlays/local/kustomization.yaml -- it is not a MemQL checkout"
    fi
    if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        cap_fail 4 "repo-root ${root} is not a git checkout, so what was built cannot be recorded"
    fi
}

# checkout_facts -- commit, ref and dirtiness of the checkout, for the envelope.
function checkout_facts() {
    local root="$1"
    CHECKOUT_COMMIT="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
    local tag branch
    tag="$(git -C "$root" describe --exact-match --tags HEAD 2>/dev/null || true)"
    branch="$(git -C "$root" symbolic-ref --short -q HEAD 2>/dev/null || true)"
    if [[ -n "$tag" ]]; then CHECKOUT_REF="tag:${tag}"
    elif [[ -n "$branch" ]]; then CHECKOUT_REF="branch:${branch}"
    else CHECKOUT_REF="detached"; fi
    # `|| true` for the same reason the three reads above carry it, and it is
    # NOT decoration: under `set -euo pipefail` a failing git in a pipeline
    # aborts the script (measured: exit 128), and every caller reaches this on
    # a plain `make dev`. The abort would surface as the EXIT trap's
    # "aborted without an explicit result" envelope, naming nothing.
    CHECKOUT_DIRTY="$(git -C "$root" status --porcelain 2>/dev/null | wc -l | tr -d ' ' || true)"
}

# filter_node_image_overrides <json-string-array> -- the same list with every
# node override removed and the database operand's kept. The argument is the
# jsonpath rendering of spec.source.kustomize.images (a JSON array of
# "name=image:tag" strings); the output is a JSON array, "[]" for nothing.
function filter_node_image_overrides() {
    local raw="${1:-}"
    raw="${raw#[}"; raw="${raw%]}"
    local out="" entry name base
    local IFS=','
    for entry in $raw; do
        entry="${entry#\"}"; entry="${entry%\"}"
        [[ -z "$entry" ]] && continue
        name="${entry%%=*}"
        base="$(basename "$name")"
        if [[ "$base" == "memql-db" ]]; then
            out+="${out:+,}\"${entry}\""
        fi
    done
    printf '[%s]\n' "$out"
}

# point_application_at_local_images -- drop the node overrides a wizard install
# wrote onto the Application, so the overlay's own :local references apply.
# Call AFTER the images are imported: the sync this triggers rolls the pods.
function point_application_at_local_images() {
    section "Pointing Application '${APP_NAME}' at the locally built images"
    # EXISTENCE FIRST, and it is not a formality. The read below is
    # `|| true`-guarded, so "no such Application" and "an Application with no
    # image overrides" both arrive as the empty string -- and the second is a
    # legitimate pass-through. Without this probe a typo'd --app-name logs
    # "the overlay's :local images already apply", returns 0, and the run
    # reports imageSource=checkout while the pods stay on the released images
    # this exists to replace. That satisfies the graph's verify, which is the
    # worst shape a failure can take.
    if ! kubectl -n "${ARGOCD_NAMESPACE}" get application "${APP_NAME}" -o name >/dev/null 2>&1; then
        cap_fail 4 "ArgoCD Application ${APP_NAME} not found in namespace ${ARGOCD_NAMESPACE} -- pass --app-name=<name> (k3d.up registers it as \${MEMQL_K3D_APP_NAME:-memql-local})"
    fi
    local current filtered
    current="$(kubectl -n "${ARGOCD_NAMESPACE}" get application "${APP_NAME}" -o 'jsonpath={.spec.source.kustomize.images}' 2>/dev/null || true)"
    if [[ -z "$current" || "$current" == "[]" ]]; then
        info "No image overrides on ${APP_NAME} -- the overlay's :local images already apply."
        return 0
    fi
    filtered="$(filter_node_image_overrides "$current")"
    if [[ "$filtered" == "$current" ]]; then
        info "Only the database operand is overridden on ${APP_NAME} -- nothing to patch."
        return 0
    fi
    info "Removing the node image overrides (keeping the database operand's)..."
    kubectl -n "${ARGOCD_NAMESPACE}" patch application "${APP_NAME}" --type=merge \
        -p "{\"spec\":{\"source\":{\"kustomize\":{\"images\":${filtered}}}}}" >&2 \
        || cap_fail 5 "patching ${APP_NAME} image overrides failed"
    kubectl -n "${ARGOCD_NAMESPACE}" annotate application "${APP_NAME}" argocd.argoproj.io/refresh=normal --overwrite >&2 || true
    OVERRIDES_PATCHED=true
    cap_changed
    wait_for_application_synced
}

# wait_for_application_synced -- ArgoCD has reconciled the patched Application.
function wait_for_application_synced() {
    local timeout="${MEMQL_K3D_SYNC_TIMEOUT:-300}"
    local deadline=$((SECONDS + timeout)) sync=""
    info "Waiting for ${APP_NAME} to sync (up to ${timeout}s)..."
    while ((SECONDS < deadline)); do
        sync="$(kubectl -n "${ARGOCD_NAMESPACE}" get application "${APP_NAME}" -o 'jsonpath={.status.sync.status}' 2>/dev/null || true)"
        if [[ "$sync" == "Synced" ]]; then
            info "${APP_NAME} is Synced."
            return 0
        fi
        sleep 5
        (( (deadline - SECONDS) % 15 == 0 )) && info "  still ${sync:-unknown} ..."
    done
    cap_fail 5 "${APP_NAME} did not reach Synced within ${timeout}s (last: ${sync:-unknown}); inspect: kubectl -n ${ARGOCD_NAMESPACE} get application ${APP_NAME}"
}

# every_image_is <space-separated-images> <want> -- true when the list is
# non-empty and every entry is exactly <want>.
#
# The emptiness check is the load-bearing half. The jsonpath read that feeds
# this is `|| true`-guarded, so a kubectl that failed hands back "", and a
# vacuous "every element matches" would end the wait below on the strength of a
# read that never happened.
function every_image_is() {
    local images="$1" want="$2" image
    [[ -n "$images" ]] || return 1
    for image in $images; do
        [[ "$image" == "$want" ]] || return 1
    done
    return 0
}

# wait_for_local_images <node...> -- the patch has actually reached the pods:
# every container of each node's Deployment names memql-<node>:local.
#
# WHY THIS EXISTS ON TOP OF `Synced`. `.status.sync.status` is ArgoCD's
# bookkeeping about a comparison it has ALREADY made, so a Synced read taken
# moments after the patch can be the previous answer -- the refresh has not
# landed, nothing has been re-compared, and the wait returns on a status that
# predates the change it is meant to prove. The Deployment's image refs are the
# thing the patch exists to change, so they are what is waited on. Synced stays
# the first gate; this is the one that cannot be satisfied by a stale read.
function wait_for_local_images() {
    local nodes=("$@")
    local timeout="${MEMQL_K3D_SYNC_TIMEOUT:-300}"

    section "Waiting for the Deployments to name the locally built images"

    local node deployment want deadline images matched
    for node in "${nodes[@]}"; do
        deployment="$(deployment_name_for_node "$node")"
        if ! kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
            info "Deployment '${deployment}' not present in namespace '${NAMESPACE}' -- nothing to wait for."
            continue
        fi
        want="$(image_name_for_node "$node")"
        deadline=$((SECONDS + timeout))
        matched=false
        images=""
        info "Waiting for ${deployment} to name ${want} (up to ${timeout}s)..."
        while ((SECONDS < deadline)); do
            images="$(kubectl -n "${NAMESPACE}" get deployment "${deployment}" -o 'jsonpath={.spec.template.spec.containers[*].image}' 2>/dev/null || true)"
            if every_image_is "$images" "$want"; then
                matched=true
                break
            fi
            sleep 5
            (( (deadline - SECONDS) % 15 == 0 )) && info "  still ${images:-unknown} ..."
        done
        if [[ "$matched" != true ]]; then
            cap_fail 5 "${deployment} still names '${images:-unknown}' rather than ${want} after ${timeout}s; inspect: kubectl -n ${NAMESPACE} get deployment ${deployment} -o jsonpath='{.spec.template.spec.containers[*].image}'"
        fi
        info "${deployment} names ${want}."
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    NAMESPACE="$(cap_param namespace "${MEMQL_K3D_NAMESPACE:-memql}")"
    local nodes_arg pull_infra wait_flag carrier_nodes_arg
    nodes_arg="$(cap_param node "")"
    pull_infra="$(cap_flag pull-infra)"
    wait_flag="$(cap_flag no-wait)"
    CARRIER_REPO="$(cap_param carrier-repo "${MEMQL_CARRIER_REPO:-}")"
    carrier_nodes_arg="$(cap_param carrier-nodes "${MEMQL_CARRIER_NODES:-}")"
    CARRIER_CONTEXT="$(cap_param carrier-context "${MEMQL_CARRIER_CONTEXT:-}")"
    REPO_ROOT="$(cap_param repo-root "${REPO_ROOT}")"
    APP_NAME="$(cap_param app-name "${MEMQL_K3D_APP_NAME:-memql-local}")"
    IMAGE_SOURCE="$(cap_param image-source "")"

    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    # A closed set, refused up front. An unrecognised value must not read as
    # "leave the overrides alone" -- that is the outcome the caller was trying
    # to avoid, reported as success.
    case "$IMAGE_SOURCE" in
        ""|checkout) ;;
        *) cap_fail 2 "image-source must be empty or 'checkout' (got '${IMAGE_SOURCE}')" ;;
    esac

    # Before anything is built: the root is what every build below reads, so a
    # root that is not a checkout must fail here rather than as a Dockerfile
    # that cannot be found, eight images into a rebuild.
    require_build_checkout "$REPO_ROOT"
    checkout_facts "$REPO_ROOT"

    if [ -n "${carrier_nodes_arg}" ]; then
        IFS=',' read -ra CARRIER_NODES <<< "${carrier_nodes_arg}"
        if [ -z "${CARRIER_REPO}" ]; then
            cap_fail 2 "--carrier-nodes requires --carrier-repo (or MEMQL_CARRIER_REPO)"
        fi
        # Validate the carrier repo up front (before the default-context cd
        # below, which would otherwise abort raw under set -e) so a typo'd or
        # not-yet-cloned path fails honestly as a missing prerequisite.
        if [ ! -d "${CARRIER_REPO}" ]; then
            error "Carrier repo not found at ${CARRIER_REPO}."
            error "Pass --carrier-repo=<path> or set MEMQL_CARRIER_REPO to its location."
            cap_fail 4 "carrier repo not found at ${CARRIER_REPO}"
        fi
        # Default carrier build context: the carrier repo's parent (workspace
        # root), so its Dockerfile can mount both source trees.
        if [ -z "${CARRIER_CONTEXT}" ]; then
            CARRIER_CONTEXT="$(cd "${CARRIER_REPO}/.." && pwd)"
        fi
    fi

    check_prerequisites

    # Resolve node list: explicit --node wins; otherwise all default app
    # nodes plus any carrier-only node types the override adds (e.g. bff).
    local nodes_to_build=()
    if [ -n "${nodes_arg}" ]; then
        IFS=',' read -ra nodes_to_build <<< "${nodes_arg}"
    else
        nodes_to_build=("${DEFAULT_APP_NODES[@]}")
        for cn in "${CARRIER_NODES[@]+"${CARRIER_NODES[@]}"}"; do
            local seen=false
            for n in "${nodes_to_build[@]}"; do
                [[ "$n" == "$cn" ]] && seen=true && break
            done
            [[ "$seen" == false ]] && nodes_to_build+=("$cn")
        done
    fi

    if [ -n "${pull_infra}" ]; then
        PULL_INFRA=true
        pull_and_import_infra
    fi

    # UNCONDITIONAL, unlike the infra images above: memql-db:16-dev exists in no
    # registry, so a cluster without it cannot fall back to pulling. The
    # function itself is a no-op once the image is in the cluster, so the inner
    # loop does not pay for it.
    ensure_db_image

    if [ ${#nodes_to_build[@]} -gt 0 ]; then
        info "Nodes to build: ${nodes_to_build[*]}"
        # ONCE, before the loop. The frontend is resolved by every `docker
        # build` below, so warming it after the first build has already failed
        # would be too late for the case this exists for.
        prewarm_build_frontend
        if [ ${#CARRIER_NODES[@]} -gt 0 ]; then
            info "Carrier override: ${CARRIER_NODES[*]} (from ${CARRIER_REPO})"
        fi
        for node in "${nodes_to_build[@]}"; do
            process_node "$node"
        done

        if [[ "$IMAGE_SOURCE" == "checkout" ]]; then
            point_application_at_local_images
            if [[ "$OVERRIDES_PATCHED" == true ]]; then
                # Synced is ArgoCD's own bookkeeping and can be a stale read;
                # the pods' image refs are the fact the patch was for.
                wait_for_local_images "${nodes_to_build[@]}"
            else
                # Nothing was patched: the Application already pointed at the
                # overlay's own :local references, so the image REFS are
                # unchanged and only their CONTENT moved -- which ArgoCD cannot
                # see and a restart is what rolls.
                for node in "${nodes_to_build[@]}"; do
                    restart_deployment "$node"
                done
            fi
        fi

        if [ -z "${wait_flag}" ]; then
            wait_for_rollouts "${nodes_to_build[@]}"
        fi
    fi

    section "Done"
    info "Cluster '${CLUSTER_NAME}' is running the latest local build."
    info "Pod status: kubectl get pods -n ${NAMESPACE}"

    cap_result_set     cluster     "$CLUSTER_NAME"
    cap_result_set     namespace   "$NAMESPACE"
    cap_result_set     nodes       "${nodes_to_build[*]}"
    cap_result_set_raw rebuilt     "$REBUILT_COUNT"
    cap_result_set_raw restarted   "$RESTARTED"
    cap_result_set_raw infraPulled "$INFRA_PULLED"
    cap_result_set_raw dbImageImported "$DB_IMAGE_IMPORTED"
    cap_result_set     imageSource "${IMAGE_SOURCE:-unchanged}"
    cap_result_set     repoRoot    "$REPO_ROOT"
    cap_result_set     commit      "$CHECKOUT_COMMIT"
    cap_result_set     ref         "$CHECKOUT_REF"
    cap_result_set_raw dirtyCount  "${CHECKOUT_DIRTY:-0}"
    cap_result_set_raw overridesPatched "$OVERRIDES_PATCHED"
    cap_result_set     appName     "$APP_NAME"
    cap_ok
}

# Run main only when EXECUTED, so the build helpers can be sourced and
# exercised directly by scripts/k3d/*_test.go. Executed directly this is
# identical to the bare `main "$@"` it replaces, and it is the same guard
# up.sh and bringup.sh already carry for the same reason.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
