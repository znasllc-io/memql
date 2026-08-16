#!/usr/bin/env bash
#
# scripts/k3d/status.sh
# =====================
#
# Capability: k3d.status -- print a parity litmus for the local k3d cluster:
# pod status, per-replica MEMQL_NODE_ID values (cross-node mesh test),
# identity signing-keyset parity, and ArgoCD sync status.
#
# Backs 'make status'. Two properties are asserted, both of which only EXIST
# once there is more than one replica -- which is why they live in the command
# CLAUDE.md sends operators to right after 'make scale N=2':
#
#   1. every pod has a UNIQUE MEMQL_NODE_ID (cross-node event routing);
#   2. every identity replica publishes the SAME JWKS keyset (memql#3400) --
#      divergent keysets make ~half of all token verifications fail.
#
# This is a status REPORTER: the human-readable table goes to STDERR (so
# 'make status' still shows it inline), and the machine-readable litmus RESULT
# (pods / uniqueNodeIds / meshHealthy / identityReplicas / identityKeysets /
# identityKeysShared) is the single JSON envelope on STDOUT.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param | 4 cluster unreachable
#
# Refs: #2067 #2061 #2221

# Reporter: no -e so an individual kubectl failure doesn't abort the report.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.status" "Print local k3d cluster status and mesh litmus."
cap_spec_param "cluster"   "k3d cluster name"
cap_spec_param "namespace" "k8s namespace"
cap_spec_param "app-name"  "ArgoCD Application name to report on"
#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
APP_NAME="${MEMQL_K3D_APP_NAME:-memql-local}"

# Litmus accumulators (populated by check_node_ids).
PODS_TOTAL=0
UNIQUE_NODE_IDS=0
MESH_HEALTHY=false

# Identity-keyset litmus accumulators (populated by check_identity_keysets).
# IDENTITY_KEYS_SHARED is raw JSON: true | false | null. `null` means the
# property was NOT MEASURED (no identity pods, or a replica could not be read)
# and must never be conflated with true -- see check_identity_keysets.
IDENTITY_REPLICAS=0
IDENTITY_KEYSETS=0
IDENTITY_KEYS_SHARED=null

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }

function section() {
    {
        echo ""
        echo "============================================================"
        echo "  $*"
        echo "============================================================"
    } >&2
}

#=============================================================================
# CLUSTER STATUS
#=============================================================================

function check_cluster() {
    section "k3d cluster '${CLUSTER_NAME}'"

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        {
            echo "  STATUS: cluster '${CLUSTER_NAME}' is NOT running."
            echo "  Run:    make up"
        } >&2
        cap_result_set     cluster "$CLUSTER_NAME"
        cap_result_set     namespace "$NAMESPACE"
        cap_fail 4 "k3d cluster '${CLUSTER_NAME}' is not running"
    fi

    k3d cluster list 2>/dev/null | grep -E "^NAME|^${CLUSTER_NAME}" >&2
    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null || true
}

#=============================================================================
# ARGOCD STATUS
#=============================================================================

# check_argocd ASSERTS that the cluster is still reconciling (memql#3817).
#
# IT USED TO ONLY PRINT. The old body ran `kubectl get application` with a
# custom-columns format including SYNC, wrote the row to stderr, and returned 0
# unconditionally -- no result field, no assertion. So when this machine's
# Application spent four hours unable to resolve its targetRevision, `SYNC:
# Unknown` was on screen for every one of those runs and the envelope still said
# ok:true. A display mistaken for a check is worse than an absent one: it
# occupies the space where the check would go and looks like coverage.
#
# WHAT MAKES IT INVISIBLE OTHERWISE. `health.status` stays `Healthy`, because
# health describes the workloads that ARE running and they are fine. It says
# nothing about whether they are what the repository asks for. Every pod
# Running, every probe green, and a merged fix that never arrives -- from
# outside, indistinguishable from a fix that did not work.
#
# THE FAILURE IS RESERVED FOR "CANNOT RECONCILE", not for "not synced".
# OutOfSync means the comparison SUCCEEDED and found drift; auto-sync resolves
# it in seconds, and failing on it would make this litmus flaky. A
# ComparisonError means nothing will happen at all, ever, without a human. Only
# the second is an outage in slow motion.
function check_argocd() {
    section "ArgoCD Application"

    ARGOCD_SYNC=""
    ARGOCD_HEALTH=""
    ARGOCD_TARGET=""
    ARGOCD_RECONCILING=""

    if ! kubectl get namespace argocd &>/dev/null; then
        echo "  STATUS: ArgoCD namespace not found -- run 'make up' first." >&2
        return 0
    fi

    if ! kubectl get application "${APP_NAME}" -n argocd &>/dev/null; then
        {
            echo "  STATUS: ${APP_NAME} Application not found."
            echo "  Run:    make up"
        } >&2
        return 0
    fi

    ARGOCD_SYNC="$(kubectl get application "${APP_NAME}" -n argocd \
        -o 'jsonpath={.status.sync.status}' 2>/dev/null || true)"
    ARGOCD_HEALTH="$(kubectl get application "${APP_NAME}" -n argocd \
        -o 'jsonpath={.status.health.status}' 2>/dev/null || true)"
    ARGOCD_TARGET="$(kubectl get application "${APP_NAME}" -n argocd \
        -o 'jsonpath={.spec.source.targetRevision}' 2>/dev/null || true)"
    local conditions
    conditions="$(kubectl get application "${APP_NAME}" -n argocd \
        -o 'jsonpath={.status.conditions[*].type}' 2>/dev/null || true)"

    info "  sync=${ARGOCD_SYNC:-unknown} health=${ARGOCD_HEALTH:-unknown} targetRevision=${ARGOCD_TARGET:-unknown}"

    # A ComparisonError means ArgoCD cannot even work out what the desired
    # state IS -- most commonly a targetRevision that no longer resolves,
    # which is what a merged-and-deleted feature branch leaves behind.
    if [[ "$conditions" == *ComparisonError* ]]; then
        ARGOCD_RECONCILING="false"
        cap_result_set     argocdSync   "$ARGOCD_SYNC"
        cap_result_set     argocdHealth "$ARGOCD_HEALTH"
        cap_result_set     argocdTargetRevision "$ARGOCD_TARGET"
        cap_result_set_raw argocdReconciling false
        cap_fail 5 "ArgoCD is NOT RECONCILING this cluster: the '${APP_NAME}' Application reports a ComparisonError against targetRevision '${ARGOCD_TARGET}'. Nothing merged reaches this cluster until it is fixed -- and every pod stays Running and Healthy meanwhile, so nothing else will tell you. If that revision is a branch that has since merged and been deleted, point it back at main:  kubectl -n argocd patch app ${APP_NAME} --type=merge -p '{\"spec\":{\"source\":{\"targetRevision\":\"main\"}}}'"
    fi

    ARGOCD_RECONCILING="true"

    # Tracking a branch is LEGITIMATE -- testing an overlay change from one is
    # how this is normally done, and it is also how memql#3817 happened. So it
    # is reported rather than refused: the defect was never that someone
    # pointed it at a branch, it was that the fact then outlived the branch
    # with nothing surfacing it.
    if [[ -n "$ARGOCD_TARGET" && "$ARGOCD_TARGET" != "main" ]]; then
        warn "  this cluster tracks '${ARGOCD_TARGET}', NOT main -- merges to main will not reach it."
        warn "  That is fine while you mean it. If that branch merges and is deleted, ArgoCD"
        warn "  stops reconciling entirely and every other signal here stays green (memql#3817)."
    fi

    if [[ -n "$ARGOCD_SYNC" && "$ARGOCD_SYNC" != "Synced" ]]; then
        warn "  sync status is '${ARGOCD_SYNC}' -- ArgoCD has found drift and auto-sync should close it."
    fi
}

#=============================================================================
# POD STATUS
#=============================================================================

function check_pods() {
    section "Pods in namespace '${NAMESPACE}'"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  STATUS: namespace '${NAMESPACE}' not found -- ArgoCD may not have synced yet." >&2
        return 0
    fi

    kubectl get pods -n "${NAMESPACE}" \
        -o wide \
        --sort-by=.metadata.name >&2 2>/dev/null || echo "  (no pods found)" >&2
}

#=============================================================================
# MESH LITMUS: UNIQUE NODE IDS
#=============================================================================

function check_node_ids() {
    section "Mesh litmus: MEMQL_NODE_ID per running pod (must be unique)"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)" >&2
        return 0
    fi

    local pods
    pods=$(kubectl get pods -n "${NAMESPACE}" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)

    if [ -z "$pods" ]; then
        echo "  (no running pods found)" >&2
        return 0
    fi

    # Node ids are read from the POD SPEC, not `kubectl exec` (#2380): the
    # pure-engine images are FROM scratch (no shell), so exec-based reads
    # reported every engine pod as unset and the litmus false-failed the
    # first live deploy. A fieldRef metadata.name env resolves to the pod
    # name BY CONSTRUCTION; an explicit value is read verbatim; pods that
    # do not declare the var at all (infra: postgres/azurite) are exempt
    # from the mesh litmus.
    local node_ids=()
    for pod in $pods; do
        local node_id
        node_id=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{range .spec.containers[0].env[?(@.name=="MEMQL_NODE_ID")]}{.value}{.valueFrom.fieldRef.fieldPath}{end}' 2>/dev/null)
        case "$node_id" in
            "")            echo "  ${pod}: (no MEMQL_NODE_ID declared -- infra pod, exempt)" >&2; continue ;;
            metadata.name) node_id="$pod" ;;
        esac
        echo "  ${pod}: MEMQL_NODE_ID=${node_id}" >&2
        node_ids+=("${node_id}")
    done

    if [ ${#node_ids[@]} -eq 0 ]; then
        echo "  (no mesh pods declare MEMQL_NODE_ID)" >&2
        PODS_TOTAL=0
        UNIQUE_NODE_IDS=0
        MESH_HEALTHY=false
        return 0
    fi

    # Check uniqueness.
    local unique_count
    unique_count=$(printf '%s\n' "${node_ids[@]}" | sort -u | wc -l | tr -d ' ')
    local total_count=${#node_ids[@]}

    PODS_TOTAL="${total_count}"
    UNIQUE_NODE_IDS="${unique_count}"

    echo "" >&2
    if [ "${unique_count}" -eq "${total_count}" ]; then
        echo "  PASS: all ${total_count} pod(s) have unique MEMQL_NODE_ID values." >&2
        MESH_HEALTHY=true
    else
        warn "FAIL: ${total_count} pods but only ${unique_count} unique MEMQL_NODE_ID values."
        warn "Shared node ids cause silent event-routing failures (mesh #1388 class of bugs)."
        MESH_HEALTHY=false
    fi
}

#=============================================================================
# IDENTITY LITMUS: ONE SIGNING KEYSET ACROSS EVERY REPLICA
#=============================================================================

# WHY THIS IS IN THE LITMUS (memql#3400). identity mints the JWTs the whole
# mesh verifies. When each replica generates its OWN Ed25519 key -- which is
# what happens with no shared MEMQL_IDENTITY_SIGNING_KEY_B64 -- the Service
# round-robins two different keysets, and a token minted by one replica is
# structurally unverifiable by any node that fetched JWKS from the other.
# Failure is intermittent, roughly coin-flip, and surfaces on the far side of
# the mesh as an authentication error that says nothing about signing keys.
#
# Config.Validate has a boot-time guard for the misconfiguration, but a config
# check cannot see replica count. `make status` can, and it is the command
# CLAUDE.md already tells operators to run straight after `make scale N=2` --
# the step that creates the second replica. So the property is asserted here,
# next to the node-id litmus it sits beside.
#
# The read goes through the apiserver's POD PROXY, not a port-forward and not
# `kubectl exec`: engine images are FROM scratch, so there is no shell in the
# pod (the same constraint that moved the node-id read to the pod spec,
# memql#2380), and a port-forward would put a background process and a port
# allocation inside a reporter. `https:` selects a TLS backend; the apiserver
# does not verify the pod's internal-CA certificate, which is what makes this
# work against identity's in-cluster TLS.
function identity_keyset_signature() {
    local pod="$1" body
    body="$(kubectl get --raw \
        "/api/v1/namespaces/${NAMESPACE}/pods/https:${pod}:8085/proxy/.well-known/jwks.json" \
        2>/dev/null)" || return 1
    [ -n "$body" ] || return 1
    # The SET of kids, sorted and joined -- not just the first one. A JWKS
    # legitimately carries two keys during a rotation overlap, and comparing
    # only the first would call two replicas mid-rotation "divergent" while
    # missing a genuine divergence hidden behind a shared first key.
    local kids
    kids="$(printf '%s' "$body" \
        | grep -oE '"kid"[[:space:]]*:[[:space:]]*"[^"]+"' \
        | sed -E 's/.*"([^"]+)"$/\1/' \
        | sort -u | tr '\n' ',')"
    [ -n "$kids" ] || return 1
    printf '%s' "$kids"
}

function check_identity_keysets() {
    section "Identity litmus: JWKS keyset per identity replica (must be identical)"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)" >&2
        return 0
    fi

    local pods
    pods=$(kubectl get pods -n "${NAMESPACE}" \
        -l app.kubernetes.io/name=identity \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)

    if [ -z "$pods" ]; then
        echo "  UNKNOWN: no running identity pods -- keyset parity not measured." >&2
        return 0
    fi

    local signatures=() unread=0 total=0
    for pod in $pods; do
        total=$((total + 1))
        local sig
        if ! sig="$(identity_keyset_signature "$pod")"; then
            warn "  ${pod}: UNKNOWN -- could not read /.well-known/jwks.json through the apiserver pod proxy."
            unread=$((unread + 1))
            continue
        fi
        echo "  ${pod}: kid(s) ${sig%,}" >&2
        signatures+=("$sig")
    done

    IDENTITY_REPLICAS="$total"

    local unique=0
    if [ ${#signatures[@]} -gt 0 ]; then
        unique=$(printf '%s\n' "${signatures[@]}" | sort -u | wc -l | tr -d ' ')
    fi
    IDENTITY_KEYSETS="$unique"

    echo "" >&2
    # Fail-open guard: a replica that could not be read means the property was
    # not measured. Reporting "shared" off the replicas that DID answer would
    # certify exactly what the check failed to observe -- the shape of defect
    # this litmus exists to catch, one level up.
    if [ "$unread" -gt 0 ]; then
        warn "UNKNOWN: ${unread} of ${total} identity replica(s) could not be read; keyset parity not measured."
        IDENTITY_KEYS_SHARED=null
        return 0
    fi

    if [ "$unique" -eq 1 ]; then
        echo "  PASS: all ${total} identity replica(s) publish the same signing keyset." >&2
        IDENTITY_KEYS_SHARED=true
    else
        warn "FAIL: ${total} identity replicas publish ${unique} DIFFERENT signing keysets (kid mismatch)."
        warn "Each replica has minted its own key, so a token issued by one is unverifiable"
        warn "by any node that fetched JWKS from the other -- roughly half of all auth fails,"
        warn "and the rejection reads as an authentication error, not a key problem (memql#3400)."
        warn "Fix: seed a shared MEMQL_IDENTITY_SIGNING_KEY_B64 with 'make secrets', then"
        warn "     'kubectl rollout restart deploy/identity -n ${NAMESPACE}'."
        IDENTITY_KEYS_SHARED=false
    fi
}

#=============================================================================
# SECRET STATUS
#=============================================================================

function check_secrets() {
    section "k8s Secrets"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)" >&2
        return 0
    fi

    for secret in memql-secrets memql-local-db-creds livekit-secrets telephony-secrets; do
        if kubectl get secret "${secret}" -n "${NAMESPACE}" &>/dev/null; then
            echo "  PRESENT: ${secret}" >&2
        else
            echo "  MISSING: ${secret}  (run: make secrets)" >&2
        fi
    done
}

#=============================================================================
# DATABASE (CloudNativePG)
#=============================================================================

# check_database -- the database litmus (memql#3846).
#
# Reports the three things that can be independently wrong on a CNPG cluster and
# that a pod listing does NOT distinguish:
#
#   1. the Cluster's own phase -- is Postgres serving, and which pod is primary;
#   2. whether the declared EXTENSIONS applied. A Cluster reports Ready as soon
#      as Postgres accepts connections, which happens BEFORE the Database CR
#      reconciles timescaledb and vector. A cluster that is Ready with no
#      timescaledb looks entirely healthy and fails the first create_hypertable;
#   3. whether WAL archiving is working. This is the one that fails SILENTLY --
#      the database serves traffic perfectly with no backups behind it, and the
#      only place it shows is this condition.
function check_database() {
    section "Database litmus: CloudNativePG cluster health"

    if ! kubectl get crd clusters.postgresql.cnpg.io &>/dev/null; then
        echo "  (CNPG CRDs absent -- the operator stack is not installed)" >&2
        return 0
    fi
    if ! kubectl get cluster memql-db -n "${NAMESPACE}" &>/dev/null; then
        echo "  (no memql-db Cluster in namespace '${NAMESPACE}')" >&2
        return 0
    fi

    local phase primary instances ready
    phase="$(kubectl get cluster memql-db -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null)"
    primary="$(kubectl get cluster memql-db -n "${NAMESPACE}" -o jsonpath='{.status.currentPrimary}' 2>/dev/null)"
    instances="$(kubectl get cluster memql-db -n "${NAMESPACE}" -o jsonpath='{.status.instances}' 2>/dev/null)"
    ready="$(kubectl get cluster memql-db -n "${NAMESPACE}" -o jsonpath='{.status.readyInstances}' 2>/dev/null)"
    echo "  cluster:    memql-db -- ${ready:-0}/${instances:-0} ready, primary=${primary:-<none>}" >&2
    echo "  phase:      ${phase:-<unknown>}" >&2

    local applied
    applied="$(kubectl get database memql-db-memql -n "${NAMESPACE}" -o jsonpath='{.status.applied}' 2>/dev/null)"
    if [[ "$applied" == "true" ]]; then
        local exts
        exts="$(kubectl get database memql-db-memql -n "${NAMESPACE}" \
            -o jsonpath='{range .status.extensions[*]}{.name}={.applied} {end}' 2>/dev/null)"
        echo "  extensions: applied -- ${exts}" >&2
    else
        echo "  extensions: NOT APPLIED (a migration now would fail on a missing timescaledb)" >&2
    fi

    local archiving
    archiving="$(kubectl get cluster memql-db -n "${NAMESPACE}" \
        -o jsonpath='{range .status.conditions[?(@.type=="ContinuousArchiving")]}{.status}{end}' 2>/dev/null)"
    case "$archiving" in
        True)  echo "  archiving:  OK (WAL reaching the object store)" >&2 ;;
        False) echo "  archiving:  FAILING -- the database is serving with no backups behind it" >&2 ;;
        *)     echo "  archiving:  <no condition yet>" >&2 ;;
    esac
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "$CLUSTER_NAME")"
    NAMESPACE="$(cap_param namespace "$NAMESPACE")"
    APP_NAME="$(cap_param app-name "$APP_NAME")"

    check_cluster
    check_argocd
    check_pods
    check_secrets
    check_database
    check_node_ids
    check_identity_keysets

    {
        echo ""
        echo "  Tear down:  make down"
        echo "  Inner loop: make dev [NODE=<type>]"
        echo ""
    } >&2

    cap_result_set     cluster "$CLUSTER_NAME"
    cap_result_set     namespace "$NAMESPACE"
    # ArgoCD reconciliation (memql#3817). argocdReconciling is true | false |
    # null on the same contract as identityKeysShared below: null means NOT
    # MEASURED -- no argocd namespace, or no Application -- and never "fine".
    # The false case never reaches here; check_argocd cap_fails on it.
    cap_result_set     argocdSync   "$ARGOCD_SYNC"
    cap_result_set     argocdHealth "$ARGOCD_HEALTH"
    cap_result_set     argocdTargetRevision "$ARGOCD_TARGET"
    cap_result_set_raw argocdReconciling "${ARGOCD_RECONCILING:-null}"
    cap_result_set_raw pods "$PODS_TOTAL"
    cap_result_set_raw uniqueNodeIds "$UNIQUE_NODE_IDS"
    cap_result_set_raw meshHealthy "$MESH_HEALTHY"
    cap_result_set_raw identityReplicas "$IDENTITY_REPLICAS"
    cap_result_set_raw identityKeysets "$IDENTITY_KEYSETS"
    # true | false | null -- null means NOT MEASURED, never "fine".
    cap_result_set_raw identityKeysShared "$IDENTITY_KEYS_SHARED"
    cap_ok
}

main "$@"
