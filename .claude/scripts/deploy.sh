#!/usr/bin/env bash
#
# .claude/scripts/deploy.sh
# =========================
#
# Build + push + deploy the memQL cluster to Azure Container Apps for
# the staging (and, parameterized, production) environment.
# See znasllc-io/memql#495 (epic #491).
#
# ARCHITECTURE (confirmed by the architect, epic #491 / COMPATIBILITY.md):
# the backend is a CLUSTER OF WORKER NODES sharing one Tiger Cloud DB,
# NOT a single binary. Two image families deploy into the one ACA
# environment (cae-memql-<env>):
#
#   1. Engine node-types from the `memql` image (build-tag-selected at
#      build time; MEMQL_NODE_TYPE-selected at run time):
#        cognition voice agent planner identity workbench
#      These are INTERNAL-ingress workers within the ACA environment
#      (port 8085, gRPC 50051). They never face the public internet.
#
#   2. The CoPresent BFF carrier from the `memql-bff-copresent` image
#      (MEMQL_NODE_TYPE=bff). This is the node the frontend hits, so it
#      gets EXTERNAL ingress (maps to api.<env>.copresent.ai later).
#      Its image is built from the SIBLING memql-bff-copresent repo,
#      whose `make release` build context spans both repos. CoPresent
#      pins this carrier at a specific VERSION; --carrier-version lets a
#      deploy target that pinned tag.
#
# This script does THREE things:
#
#   1. BUILD + PUSH the two images to the shared ACR (acrmemql):
#        - the memql engine image    (scripts/release/release.sh)
#        - the memql-bff-copresent    (../memql-bff-copresent `make release`)
#      Skippable with --skip-build (deploy already-pushed tags).
#
#   2. DEPLOY the cluster: create-or-update one Container App per engine
#      node-type (internal) + the BFF carrier (external). Idempotent --
#      `az containerapp create` vs `update` is chosen by an existence
#      check, so re-running converges with no duplicates.
#
#   3. Wire Key Vault secret refs (via the env's managed identity) and
#      env from service.yaml.
#
# A final STATE REPORT prints which apps were created/updated, with
# which image tags + ingress.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- show_help / parse_arguments /
# validate_arguments / check_prerequisites / main, one function per
# responsibility, main() at the bottom. Supports --help and a
# --dry-run that prints the full plan and mutates nothing.
#
# NOTE: there is no live Azure subscription yet. This script is written
# to be correct, shellcheck-clean, and dry-run-able; the operator runs
# it against their own `az login` once a subscription + the
# `make deploy-setup` resources exist. Until then, `--dry-run` exercises
# the full plan.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Naming + placement (locked epic architecture #491, matches
# deploy-setup.sh). Resource group + Key Vault + Container Apps env are
# per-environment; the ACR is SHARED across envs (Basic SKU).
# Region is inherited from the Container Apps environment (cae-memql-<env>,
# created in East US by deploy-setup.sh); kept here only for the report.
readonly REGION_DISPLAY="East US"
readonly ACR_NAME="acrmemql"          # global, alnum-only (no dashes)
readonly ACR_LOGIN_SERVER="${ACR_NAME}.azurecr.io"

# Engine node-types that run the `memql` image, selected at runtime by
# MEMQL_NODE_TYPE (the exact list lives in docker/docker-compose.cluster.yml).
# Each becomes one INTERNAL-ingress Container App: ca-memql-<type>-<env>.
readonly ENGINE_NODE_TYPES=(
    "cognition"
    "voice"
    "agent"
    "planner"
    "identity"
    "workbench"
)

# Image names in the shared ACR.
readonly ENGINE_IMAGE_NAME="memql"                 # built by scripts/release/release.sh
readonly CARRIER_IMAGE_NAME="memql-bff-copresent"  # built by ../memql-bff-copresent make release

# Cluster ports (docker-compose.cluster.yml): HTTP/WS on 8085, gRPC on
# 50051. Container Apps targets the HTTP/WS port for ingress.
readonly NODE_HTTP_PORT="8085"

# Where the sibling carrier repo lives, relative to this repo's root.
# The carrier's `make release` build context spans both repos (its
# go.mod has `replace ../memql`), so the workspace layout must hold
# memql/ + memql-bff-copresent/ as siblings.
readonly CARRIER_REPO_REL="../memql-bff-copresent"

# Default environment + default carrier version. The carrier VERSION
# defaults to the sibling repo's VERSION file when present; copresent
# pins a specific tag later via --carrier-version.
DEFAULT_ENV="staging"

# State accumulators for the final report.
STATE_CREATED=()
STATE_CHANGED=()
STATE_SKIPPED=()

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function log()   { echo "  $*"; }
function info()  { echo "[deploy] $*"; }
function warn()  { echo "  WARN: $*" >&2; }
function err()   { echo "  ERROR: $*" >&2; }

function section() {
    echo ""
    echo "=== $* ==="
}

function record_created() { STATE_CREATED+=("$1"); }
function record_changed() { STATE_CHANGED+=("$1"); }
function record_skipped() { STATE_SKIPPED+=("$1"); }

#=============================================================================
# HELP
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--env=staging|production] [options]

Build + push the memQL engine + CoPresent BFF carrier images to the
shared ACR, then create-or-update the cluster of Container Apps in
cae-memql-<env>: one INTERNAL-ingress app per engine node-type
(${ENGINE_NODE_TYPES[*]}) from the memql image, plus the EXTERNAL-ingress
CoPresent BFF carrier. Idempotent: re-running converges each app.

Options:
    --env=ENV               Target environment: staging (default) or production.
                            May also be passed positionally or via ENV=.
    --version=X.Y.Z         Engine (memql) image tag to build/deploy.
                            Default: the repo VERSION file's semver.
    --carrier-version=X.Y.Z BFF carrier (memql-bff-copresent) image tag.
                            Default: the sibling repo's VERSION file, or
                            --version if that is unavailable. CoPresent
                            pins this tag.
    --skip-build            Don't build/push; deploy the already-pushed
                            tags (--version / --carrier-version).
    --skip-engine           Don't deploy the engine node-type apps.
    --skip-carrier          Don't deploy the BFF carrier app.
    --dry-run               Print the full plan (apps, images/tags,
                            ingress); mutate nothing.
    --help                  Show this help and exit.

Apps deployed into cae-memql-<env> (region: ${REGION_DISPLAY}):
    ca-memql-<type>-<env>   one per engine node-type, INTERNAL ingress,
                            image ${ACR_LOGIN_SERVER}/${ENGINE_IMAGE_NAME}:<version>,
                            MEMQL_NODE_TYPE=<type>, target port ${NODE_HTTP_PORT}
    ca-copresent-bff-<env>  CoPresent BFF carrier, EXTERNAL ingress,
                            image ${ACR_LOGIN_SERVER}/${CARRIER_IMAGE_NAME}:<carrier-version>,
                            MEMQL_NODE_TYPE=bff, target port ${NODE_HTTP_PORT}

Examples:
    $0 --dry-run
    $0 --env=staging
    $0 --env=staging --carrier-version=0.9.0
    $0 --env=staging --skip-build --version=0.9.0 --carrier-version=0.9.0
    ENV=production $0 --dry-run
EOF
}

#=============================================================================
# ARGUMENT PARSING + VALIDATION
#=============================================================================

function parse_arguments() {
    ENV="${ENV:-$DEFAULT_ENV}"
    VERSION=""
    CARRIER_VERSION=""
    SKIP_BUILD=false
    SKIP_ENGINE=false
    SKIP_CARRIER=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)              ENV="${1#*=}" ;;
            --env)                shift; ENV="${1:-}" ;;
            --version=*)          VERSION="${1#*=}" ;;
            --version)            shift; VERSION="${1:-}" ;;
            --carrier-version=*)  CARRIER_VERSION="${1#*=}" ;;
            --carrier-version)    shift; CARRIER_VERSION="${1:-}" ;;
            --skip-build)         SKIP_BUILD=true ;;
            --skip-engine)        SKIP_ENGINE=true ;;
            --skip-carrier)       SKIP_CARRIER=true ;;
            --dry-run)            DRY_RUN=true ;;
            --help|-h)            show_help; exit 0 ;;
            staging|production)
                ENV="$1"
                ;;
            *)
                err "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
        shift
    done
}

function resolve_repo_root() {
    # This script lives at .claude/scripts/; the repo root is two
    # directories up. Resolve it so the target works from anywhere.
    local here
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    REPO_ROOT="$(cd "${here}/../.." && pwd)"
    CARRIER_REPO="$(cd "${REPO_ROOT}/${CARRIER_REPO_REL}" 2>/dev/null && pwd || echo "${REPO_ROOT}/${CARRIER_REPO_REL}")"
}

# Read a clean semver X.Y.Z from a repo's VERSION file (stripping any
# legacy -<epoch> suffix). Prints empty if the file is absent.
function semver_from_version_file() {
    local file="$1"
    if [ -f "${file}" ]; then
        head -n1 "${file}" | cut -d- -f1
    fi
}

function validate_arguments() {
    case "$ENV" in
        staging|production) ;;
        *)
            err "Invalid --env: '${ENV}'. Must be 'staging' or 'production'."
            exit 1
            ;;
    esac

    # Per-env resource names derived from the validated ENV.
    RG_NAME="rg-memql-${ENV}"
    KV_NAME="kv-memql-${ENV}"
    CAE_NAME="cae-memql-${ENV}"

    # Engine version: explicit --version wins, else the repo VERSION file.
    if [ -z "${VERSION}" ]; then
        VERSION="$(semver_from_version_file "${REPO_ROOT}/VERSION")"
    fi
    if [ -z "${VERSION}" ]; then
        err "could not resolve an engine --version (no VERSION file at ${REPO_ROOT}/VERSION)"
        exit 1
    fi
    if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        err "--version must be clean semver X.Y.Z (got: '${VERSION}')"
        exit 1
    fi

    # Carrier version: explicit --carrier-version wins, else the sibling
    # repo's VERSION file, else fall back to the engine VERSION.
    if [ -z "${CARRIER_VERSION}" ]; then
        CARRIER_VERSION="$(semver_from_version_file "${CARRIER_REPO}/VERSION")"
    fi
    if [ -z "${CARRIER_VERSION}" ]; then
        CARRIER_VERSION="${VERSION}"
    fi
    if [[ ! "$CARRIER_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        err "--carrier-version must be clean semver X.Y.Z (got: '${CARRIER_VERSION}')"
        exit 1
    fi

    # Fully-qualified image refs in the shared ACR.
    ENGINE_IMAGE_REF="${ACR_LOGIN_SERVER}/${ENGINE_IMAGE_NAME}:${VERSION}"
    CARRIER_IMAGE_REF="${ACR_LOGIN_SERVER}/${CARRIER_IMAGE_NAME}:${CARRIER_VERSION}"

    if [ "$ENV" = "production" ]; then
        warn "ENV=production is a parameterized STUB. App names + steps are"
        warn "wired, but production has not been validated against a live"
        warn "subscription. Review carefully before a real production run."
    fi
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function have() { command -v "$1" >/dev/null 2>&1; }

# run_or_plan: the single mutation gate. In --dry-run we print the
# command and return success WITHOUT executing; otherwise we execute it.
# Every state-changing az/make/release call routes through here so
# --dry-run is guaranteed side-effect-free.
function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then
        echo "  [plan] $*"
        return 0
    fi
    "$@"
}

function check_prerequisites() {
    section "Prerequisites"

    if [ "$DRY_RUN" = true ]; then
        log "[plan] verify: az (logged in) + containerapp ext + docker; sibling carrier repo present"
        return 0
    fi

    if ! have az; then
        warn "az (Azure CLI) not found. Run 'make deploy-setup' to install the toolchain,"
        warn "  then 'az login'. Deploy steps that need az will be skipped."
    elif ! az account show >/dev/null 2>&1; then
        warn "az is not logged in. Run: az login   (then re-run deploy)"
    else
        log "[ok] az logged in"
        if ! az extension show --name containerapp >/dev/null 2>&1; then
            warn "az containerapp extension missing. Run: az extension add --name containerapp"
        else
            log "[ok] az containerapp extension"
        fi
    fi

    if [ "$SKIP_BUILD" = false ]; then
        if ! have docker; then
            warn "docker not found -- image build/push will fail. Use --skip-build to"
            warn "  deploy already-pushed tags, or install Docker."
        else
            log "[ok] docker present"
        fi
        if [ ! -d "${CARRIER_REPO}" ]; then
            warn "sibling carrier repo not found at ${CARRIER_REPO}"
            warn "  The BFF carrier build context spans both repos. Use --skip-carrier"
            warn "  or --skip-build, or check out memql-bff-copresent as a sibling."
        else
            log "[ok] carrier repo present: ${CARRIER_REPO}"
        fi
    fi
}

# az_ready guards every Azure step: dry-run always proceeds (to print
# the plan); live requires az present + logged in.
function az_ready() {
    if [ "$DRY_RUN" = true ]; then
        return 0
    fi
    if ! have az; then
        return 1
    fi
    az account show >/dev/null 2>&1
}

#=============================================================================
# BUILD + PUSH IMAGES
#=============================================================================

function build_and_push_images() {
    section "Build + push images -> ${ACR_LOGIN_SERVER}"

    if [ "$SKIP_BUILD" = true ]; then
        log "--skip-build set; deploying already-pushed tags:"
        log "  engine:  ${ENGINE_IMAGE_REF}"
        log "  carrier: ${CARRIER_IMAGE_REF}"
        return 0
    fi

    # Authenticate the local docker to the ACR so the release scripts'
    # `docker push` can publish. `az acr login` is a no-op-safe re-run.
    if az_ready; then
        info "logging docker in to ${ACR_NAME}..."
        run_or_plan az acr login --name "${ACR_NAME}"
    else
        warn "az not ready -- skipping 'az acr login'. Push will fail without it."
    fi

    # 1. Engine image via this repo's release tooling. The release
    #    script tags + (with --push) publishes acrmemql.azurecr.io/memql:X.Y.Z.
    if [ "$SKIP_ENGINE" = true ]; then
        log "--skip-engine set; not building the engine image."
    else
        info "building + pushing engine image ${ENGINE_IMAGE_REF}..."
        run_or_plan bash "${REPO_ROOT}/scripts/release/release.sh" \
            --version="${VERSION}" --acr="${ACR_NAME}" --push
    fi

    # 2. Carrier image via the sibling repo's `make release`. Its build
    #    context spans both repos (replace ../memql), so we invoke its
    #    own release tooling rather than re-implement the cross-repo build.
    if [ "$SKIP_CARRIER" = true ]; then
        log "--skip-carrier set; not building the carrier image."
    elif [ ! -d "${CARRIER_REPO}" ] && [ "$DRY_RUN" = false ]; then
        warn "carrier repo absent at ${CARRIER_REPO}; skipping carrier build."
        record_skipped "carrier-build: ${CARRIER_IMAGE_REF}"
    else
        info "building + pushing carrier image ${CARRIER_IMAGE_REF}..."
        run_or_plan make -C "${CARRIER_REPO}" release \
            ARGS="--version=${CARRIER_VERSION} --acr=${ACR_NAME} --push"
    fi
}

#=============================================================================
# DEPLOY -- create-or-update Container Apps (idempotent)
#=============================================================================

# containerapp_exists: true if a Container App of the given name exists
# in the env's resource group. In dry-run we always return false so the
# plan shows the create path (and never probes Azure).
function containerapp_exists() {
    local app="$1"
    if [ "$DRY_RUN" = true ]; then
        return 1
    fi
    az containerapp show --name "${app}" --resource-group "${RG_NAME}" >/dev/null 2>&1
}

# Deploy (create-or-update) one Container App. Args:
#   $1 app name, $2 image ref, $3 node-type (MEMQL_NODE_TYPE),
#   $4 ingress ("internal"|"external").
#
# Secrets ride Key Vault references via the env's managed identity; the
# DSN secret name matches what tiger-provision.sh / deploy-setup.sh store
# (memory-nodes-database-dsn). Non-secret env mirrors service.yaml
# (SERVER_ADDRESS, MEMQL_GRPC_ADDRESS, auto-migrate flags).
function deploy_container_app() {
    local app="$1" image="$2" node_type="$3" ingress="$4"
    local kv_uri="https://${KV_NAME}.vault.azure.net/secrets/memory-nodes-database-dsn"

    if ! az_ready; then
        warn "az not ready -- skipping ${app}."
        record_skipped "containerapp: ${app}"
        return 0
    fi

    # Common env + secret wiring shared by create + update. Secret refs
    # are resolved at runtime by the app's managed identity against the
    # per-env Key Vault.
    local -a common_args=(
        --resource-group "${RG_NAME}"
        --image "${image}"
        --secrets "memory-nodes-database-dsn=keyvaultref:${kv_uri},identityref:system"
        --env-vars
            "MEMQL_NODE_TYPE=${node_type}"
            "MEMORY_NODES_DATABASE_DSN=secretref:memory-nodes-database-dsn"
            "SERVER_ADDRESS=0.0.0.0:${NODE_HTTP_PORT}"
            "MEMQL_GRPC_ADDRESS=:50051"
            "MEMORY_NODES_DATABASE_AUTO_MIGRATE=true"
            "MEMORY_NODES_DATABASE_MIGRATE_ON_START=true"
    )

    if containerapp_exists "${app}"; then
        info "updating ${app} (${node_type}, ${ingress}) -> ${image}..."
        run_or_plan az containerapp update \
            --name "${app}" \
            "${common_args[@]}" \
            --only-show-errors
        # Ingress is converged separately so a flip (internal<->external)
        # is reconciled on re-run too.
        run_or_plan az containerapp ingress enable \
            --name "${app}" --resource-group "${RG_NAME}" \
            --type "${ingress}" --target-port "${NODE_HTTP_PORT}" \
            --transport auto --only-show-errors
        record_changed "containerapp: ${app} (${node_type}, ${ingress}) -> ${image}"
    else
        info "creating ${app} (${node_type}, ${ingress}) -> ${image}..."
        run_or_plan az containerapp create \
            --name "${app}" \
            --environment "${CAE_NAME}" \
            "${common_args[@]}" \
            --registry-server "${ACR_LOGIN_SERVER}" \
            --system-assigned \
            --ingress "${ingress}" \
            --target-port "${NODE_HTTP_PORT}" \
            --transport auto \
            --min-replicas 1 \
            --only-show-errors
        record_created "containerapp: ${app} (${node_type}, ${ingress}) -> ${image}"
    fi
}

function deploy_engine_nodes() {
    section "Engine node-type apps (internal ingress)"

    if [ "$SKIP_ENGINE" = true ]; then
        log "--skip-engine set; not deploying engine node-type apps."
        return 0
    fi

    local node_type app
    for node_type in "${ENGINE_NODE_TYPES[@]}"; do
        app="ca-memql-${node_type}-${ENV}"
        deploy_container_app "${app}" "${ENGINE_IMAGE_REF}" "${node_type}" "internal"
    done
}

function deploy_carrier_node() {
    section "CoPresent BFF carrier app (external ingress)"

    if [ "$SKIP_CARRIER" = true ]; then
        log "--skip-carrier set; not deploying the BFF carrier app."
        return 0
    fi

    local app="ca-copresent-bff-${ENV}"
    deploy_container_app "${app}" "${CARRIER_IMAGE_REF}" "bff" "external"
}

#=============================================================================
# STATE REPORT
#=============================================================================

function print_state_report() {
    section "State report (env=${ENV}, region=${REGION_DISPLAY}, dry-run=${DRY_RUN})"

    log "Engine image:  ${ENGINE_IMAGE_REF}"
    log "Carrier image: ${CARRIER_IMAGE_REF}  (CoPresent pins this tag)"
    log "ACA env:       ${CAE_NAME}  (rg ${RG_NAME})"
    echo ""

    local plan_note=""
    if [ "$DRY_RUN" = true ]; then
        plan_note=" (planned, not executed)"
    fi

    echo "  Created:${plan_note}"
    if [ "${#STATE_CREATED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CREATED[@]}"; fi

    echo "  Updated:${plan_note}"
    if [ "${#STATE_CHANGED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_CHANGED[@]}"; fi

    echo "  Skipped (tool/auth/repo missing):"
    if [ "${#STATE_SKIPPED[@]}" -eq 0 ]; then echo "    (none)"; else
        printf '    %s\n' "${STATE_SKIPPED[@]}"; fi

    echo ""
    if [ "$DRY_RUN" = true ]; then
        info "DRY RUN complete -- nothing was built, pushed, or deployed."
        info "Re-run without --dry-run against an 'az login' session + the"
        info "'make deploy-setup' resources to apply."
    elif [ "${#STATE_SKIPPED[@]}" -ne 0 ]; then
        info "Deploy ran with skips. Resolve the items above and re-run;"
        info "re-running is safe (idempotent) and converges remaining apps."
    else
        info "Deploy complete. A second consecutive run converges (no churn)."
    fi
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    parse_arguments "$@"
    resolve_repo_root
    validate_arguments

    info "memQL deploy -- env=${ENV} region=${REGION_DISPLAY} dry-run=${DRY_RUN}"
    info "  engine ${ENGINE_IMAGE_REF} / carrier ${CARRIER_IMAGE_REF}"

    check_prerequisites
    build_and_push_images
    deploy_engine_nodes
    deploy_carrier_node
    print_state_report
}

main "$@"
