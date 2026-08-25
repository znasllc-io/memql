#!/usr/bin/env bash
#
# scripts/deploy/install-cluster-operators.sh
# ===========================================
#
# Capability: deploy.installClusterOperators -- put on a freshly provisioned
# cluster the operators and cluster-scoped objects that every committed
# manifest in this tree ASSUMES are already there.
#
# Backend for the `installClusterOperators` deployment action (memql#4472,
# memql#4473, epic memql#4490).
#
# WHY THIS EXISTS. `bringUpInstance` was `provisionInstance` then
# `installInstance`, which was substrate then `argoSync` -- and `argoSync`
# against a freshly provisioned cluster syncs nothing, because on that cluster
# there is no ArgoCD and none of the operators the manifests need. Eleven
# ordered steps live in that gap. This script owns steps 1 through 7.
#
# THE FAILURE MODE THESE SIX SHARE IS SILENCE, which is the whole reason they
# are worth a script rather than a runbook paragraph:
#
#   - ESO CRDs absent: the ESO CONTROLLER crashloops while the WEBHOOK stays
#     Running, so the namespace shows one healthy pod beside one restarting one
#     and nothing names a CRD.
#   - ingress-nginx absent: an Ingress naming an absent class is a VALID object
#     that nothing acts on. ADDRESS stays empty, which reads as "still
#     starting" indefinitely.
#   - the letsencrypt-prod ClusterIssuer absent: Certificates stay Pending and
#     ingress-nginx serves its own self-signed default, so the site loads with
#     a browser warning rather than failing.
#   - cert-manager or CNPG absent: an unrecognised CRD is REJECTED at apply, so
#     the sync fails as a whole and reports a schema error rather than a
#     missing operator.
#
# ORDERING IS LOAD-BEARING, in three places, and each is a real failure:
#
#   1. The kubeconfig comes first, because every later step is a kubectl.
#   2. cert-manager comes before External Secrets. ESO's install declares a
#      cert-manager Issuer for its webhook serving cert, so applying ESO onto a
#      cluster with no cert-manager CRDs is rejected at apply.
#   3. The ESO CRDs come before the ESO controller, per the crashloop above.
#   4. The ClusterIssuer comes LAST, after cert-manager is not merely applied
#      but ROLLED OUT -- a ClusterIssuer applied against a webhook that is not
#      serving yet is refused by the admission webhook, and the error names
#      a connection rather than a race.
#
# IDEMPOTENT BY CONSTRUCTION. Every step is `kubectl apply`, which converges,
# preceded by an existence check so a converged cluster makes no call that
# changes anything and the run reports `changed: false`. That matters because
# the caller is an automation with at-least-once delivery.
#
# NO DECISIONS INSIDE. It does not decide WHICH operators an install needs or
# what version they should be -- the versions are pinned in the repository, in
# each component's own install/kustomization.yaml, and gated by
# deploy/cnpg/operator_stack_test.go. This script applies what the checkout
# says.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4490 memql#4472 memql#4473 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

#=============================================================================
# CAPABILITY SPEC
#=============================================================================

cap_init "deploy.installClusterOperators" \
    "Install the cluster operators and cluster-scoped objects an instance's manifests assume exist."

cap_spec_param_required "subscriptionId" "Azure subscription holding the cluster"
cap_spec_param_required "resourceGroup"  "resource group holding the AKS cluster"
cap_spec_param_required "clusterName"    "AKS cluster name -- the kubeconfig is fetched for it"
cap_spec_param "repoRoot"       "MemQL checkout holding the pinned operator installs (default: the checkout this script is in)"
cap_spec_param "acmeEmail"      "ACME account email for the letsencrypt-prod ClusterIssuer. OMIT to skip the issuer -- there is no default, because a wrong address is where the expiry warnings go"
cap_spec_param "ingressClass"   "IngressClass name the manifests declare (default nginx)"
cap_spec_param "skipMonitoring" "do not install the prometheus-operator or apply deploy/k8s/monitoring. Set only when alerting is genuinely owned elsewhere: absent alerting produces a cluster that LOOKS QUIET, which is what a healthy one looks like"
cap_spec_param "timeoutSeconds" "per-operator rollout wait (default 300)"
cap_spec_param "dryRun"         "plan only; apply nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

#=============================================================================
# CONFIGURATION
#=============================================================================

SUBSCRIPTION_ID="$(cap_param subscriptionId "")"
RESOURCE_GROUP="$(cap_param resourceGroup "")"
CLUSTER_NAME="$(cap_param clusterName "")"
REPO_ROOT="$(cap_param repoRoot "$(cd "${SCRIPT_DIR}/../.." && pwd)")"
ACME_EMAIL="$(cap_param acmeEmail "")"
INGRESS_CLASS="$(cap_param ingressClass "nginx")"
SKIP_MONITORING="$(cap_param skipMonitoring "false")"
TIMEOUT_SECONDS="$(cap_param timeoutSeconds "300")"
DRY_RUN="$(cap_param dryRun "false")"

readonly CLUSTER_ISSUER_NAME="letsencrypt-prod"
readonly ACME_DIRECTORY="https://acme-v02.api.letsencrypt.org/directory"

INSTALLED=""
SKIPPED=""

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    command -v az      &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed or not on PATH"

    local active
    active="$(az account show --query id -o tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first"
    if [[ "$active" != "$SUBSCRIPTION_ID" ]]; then
        cap_info "active subscription ${active} is not the target; selecting ${SUBSCRIPTION_ID}"
        az account set --subscription "$SUBSCRIPTION_ID" 2>/dev/null \
            || cap_fail 3 "cannot select subscription ${SUBSCRIPTION_ID} -- the signed-in identity may not have access to it"
    fi
}

function validate_arguments() {
    [[ -n "$SUBSCRIPTION_ID" ]] || cap_fail 2 "--subscriptionId is required"
    [[ -n "$RESOURCE_GROUP"  ]] || cap_fail 2 "--resourceGroup is required"
    [[ -n "$CLUSTER_NAME"    ]] || cap_fail 2 "--clusterName is required"

    [[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] \
        || cap_fail 2 "--timeoutSeconds must be an integer, got ${TIMEOUT_SECONDS}"

    # An email that is not one produces an ACME account registration failure at
    # the FIRST certificate order, which is minutes later and reads as a DNS or
    # rate-limit problem. Cheap to reject here.
    if [[ -n "$ACME_EMAIL" ]]; then
        [[ "$ACME_EMAIL" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] \
            || cap_fail 2 "--acmeEmail ${ACME_EMAIL} is not an email address"
    fi

    local d
    for d in deploy/cert-manager/install deploy/cnpg/install \
             deploy/external-secrets/crds deploy/external-secrets/install \
             deploy/ingress-nginx/install deploy/prometheus-operator/install \
             deploy/k8s/monitoring; do
        [[ -d "${REPO_ROOT}/${d}" ]] \
            || cap_fail 2 "--repoRoot ${REPO_ROOT} does not look like a MemQL checkout: ${d} is missing. The operator versions are pinned in the repository, so this script needs one."
    done
}

# would_change <description> -- true when the step is skipped because this is a
# plan-only run. Logs the intent either way, so a dry run reads as the plan it is.
function would_change() {
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would $1"
        return 0
    fi
    cap_step "$1"
    return 1
}

function note_installed() { INSTALLED="${INSTALLED:+${INSTALLED},}$1"; }
function note_skipped()   { SKIPPED="${SKIPPED:+${SKIPPED},}$1"; }

# ---- 1. kubeconfig -----------------------------------------------------------

function ensure_kubeconfig() {
    # Not conditional on an existence check: a kubeconfig entry can exist and
    # point at a cluster that has been rebuilt since, in which case every later
    # step authenticates to a cluster that is gone. --overwrite-existing makes
    # this converge on the CURRENT cluster, which is the only safe reading.
    if would_change "fetch kubeconfig for ${CLUSTER_NAME}"; then
        return 0
    fi
    az aks get-credentials --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --overwrite-existing --only-show-errors >/dev/null \
        || cap_fail 5 "failed to fetch kubeconfig for ${CLUSTER_NAME} in ${RESOURCE_GROUP}"

    kubectl cluster-info >/dev/null 2>&1 \
        || cap_fail 5 "kubeconfig fetched but the API server is unreachable -- the cluster may still be provisioning"
    note_installed "kubeconfig"
    cap_changed
}

# apply_pinned <label> <kustomize dir> <probe kind/name> [namespace]
#
# The probe is what makes this idempotent AND honest: an already-converged
# component is reported as kept, and one that is genuinely absent is applied.
# --server-side is not a style choice -- the CRD bundles here are large enough
# to trip kubectl's last-applied-configuration annotation size limit on a
# client-side apply, which is the documented reason ESO's chart render sets
# installCRDs: false in the first place.
function apply_pinned() {
    local label="$1" dir="$2" probe="$3" ns="${4:-}"
    local -a getargs=(get "$probe")
    [[ -n "$ns" ]] && getargs+=(-n "$ns")

    if kubectl "${getargs[@]}" -o name &>/dev/null; then
        cap_info "${label} already present; skipping"
        note_skipped "$label"
        return 0
    fi

    if would_change "install ${label} from ${dir}"; then
        return 0
    fi
    kubectl apply --server-side --force-conflicts -k "${REPO_ROOT}/${dir}" >/dev/null \
        || cap_fail 5 "failed to apply ${label} from ${dir}"
    note_installed "$label"
    cap_changed
}

# wait_rollout <label> <namespace> <deployment...> -- a component is not
# installed when its objects exist, it is installed when its webhook is
# serving. Skipping this is what makes the NEXT step fail with a connection
# error naming a service rather than a race.
function wait_rollout() {
    local label="$1" ns="$2"; shift 2
    [[ "$DRY_RUN" == "true" ]] && { cap_info "DRY RUN: would wait for ${label} to roll out"; return 0; }

    local d
    for d in "$@"; do
        kubectl -n "$ns" rollout status "deploy/${d}" --timeout="${TIMEOUT_SECONDS}s" >/dev/null 2>&1 \
            || cap_fail 5 "${label}: deployment ${d} in ${ns} did not become available within ${TIMEOUT_SECONDS}s"
    done
    cap_info "${label} is rolled out"
}

# ---- 7. the ACME ClusterIssuer ----------------------------------------------

function ensure_cluster_issuer() {
    if [[ -z "$ACME_EMAIL" ]]; then
        cap_info "no --acmeEmail given; skipping the ${CLUSTER_ISSUER_NAME} ClusterIssuer. Certificates will stay Pending and ingress-nginx will serve its self-signed default, which loads with a browser warning rather than failing."
        note_skipped "$CLUSTER_ISSUER_NAME"
        return 0
    fi
    if kubectl get clusterissuer "$CLUSTER_ISSUER_NAME" -o name &>/dev/null; then
        cap_info "ClusterIssuer ${CLUSTER_ISSUER_NAME} already present; skipping"
        note_skipped "$CLUSTER_ISSUER_NAME"
        return 0
    fi
    if would_change "create ClusterIssuer ${CLUSTER_ISSUER_NAME} (HTTP-01 via ${INGRESS_CLASS})"; then
        return 0
    fi

    # HTTP-01 only, deliberately. ACME cannot issue a WILDCARD over HTTP-01, and
    # one wildcard dnsName fails the WHOLE order -- which is why the front door's
    # certificate names exact hosts only. A DNS-01 issuer is a separate object
    # with real Azure substrate behind it (cloud-entry/dns01-wildcard-tls.yaml).
    #
    # The email is passed on stdin rather than argv for the same reason every
    # other value in this family is: argv is world-readable on a shared runner.
    kubectl apply -f - >/dev/null <<EOF || cap_fail 5 "failed to create ClusterIssuer ${CLUSTER_ISSUER_NAME}"
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${CLUSTER_ISSUER_NAME}
spec:
  acme:
    server: ${ACME_DIRECTORY}
    email: ${ACME_EMAIL}
    privateKeySecretRef:
      name: ${CLUSTER_ISSUER_NAME}-account-key
    solvers:
      - http01:
          ingress:
            class: ${INGRESS_CLASS}
EOF
    note_installed "$CLUSTER_ISSUER_NAME"
    cap_changed
}

# ---- 8. the alerting stack --------------------------------------------------
#
# THE WORST MEMBER OF THE SILENT-DEPENDENCY CLASS (memql#4499). A missing Secret
# leaves a pod in ContainerCreating -- visible, eventually investigated. A
# missing ALERTING STACK produces a cluster that looks QUIET, and silence is
# what a healthy system produces. Absent alerting is indistinguishable from
# nothing being wrong, for as long as it lasts.
#
# memql#4460 is the proof: an instance ran its entire life with WAL archiving
# broken and an empty backup container. Two of the five database alerts would
# have fired -- one within five minutes -- and neither could, because
# deploy/k8s/monitoring reached no cluster. Its only invocation in the whole
# tree was a manual kubectl in its own README.
function ensure_monitoring() {
    if [[ "$SKIP_MONITORING" == "true" ]]; then
        cap_warn "--skipMonitoring: neither the prometheus-operator nor deploy/k8s/monitoring will be installed. Every MemQL alert -- WAL archiving, volume fill, replication, auth -- will be ABSENT on this cluster, and absent alerting looks exactly like a healthy cluster."
        note_skipped "monitoring"
        return 0
    fi

    # The operator install is verify-first on its CRD, which is also what makes
    # BRING-YOUR-OWN work with no flag: a cluster already running a Prometheus
    # stack HAS this CRD, so the install is skipped and only the rules below are
    # applied -- against the operator that is already there.
    apply_pinned "prometheus-operator" "deploy/prometheus-operator/install" \
        "crd/prometheusrules.monitoring.coreos.com"

    # The rules are applied EVERY time, not probed for. They are the payload,
    # they change with the engine, and `kubectl apply` converges -- so skipping
    # them because an older copy exists is how a cluster ends up evaluating last
    # release's alerts.
    if would_change "apply deploy/k8s/monitoring (PodMonitors + PrometheusRules)"; then
        return 0
    fi
    kubectl apply --server-side --force-conflicts -k "${REPO_ROOT}/deploy/k8s/monitoring" >/dev/null \
        || cap_fail 5 "failed to apply deploy/k8s/monitoring -- the alert rules are not installed, and a cluster with no alerts looks exactly like a healthy one"
    note_installed "monitoring"
    cap_changed
}

function collect_result() {
    cap_result_set "clusterName" "$CLUSTER_NAME"
    cap_result_set "installed"   "$INSTALLED"
    cap_result_set "kept"        "$SKIPPED"
    cap_result_set "dryRun"      "$DRY_RUN"
    if [[ -n "$ACME_EMAIL" ]]; then
        cap_result_set "clusterIssuer" "$CLUSTER_ISSUER_NAME"
    fi
    # Deliberately unconditional: a function whose LAST statement is a
    # `[[ ]] && cmd` returns 1 when the test is false, and under `set -e` that
    # aborts the caller before cap_ok is ever reached. The envelope then reads
    # "aborted without an explicit result" on a run that did everything right.
    return 0
}

function main() {
    validate_arguments
    check_prerequisites

    cap_info "installing cluster operators on ${CLUSTER_NAME} from ${REPO_ROOT}"
    [[ "$DRY_RUN" == "true" ]] && cap_info "DRY RUN -- nothing will be applied"

    ensure_kubeconfig

    # cert-manager BEFORE External Secrets: ESO's install declares a
    # cert-manager Issuer for its webhook serving cert.
    apply_pinned "cert-manager" "deploy/cert-manager/install" "crd/certificates.cert-manager.io"
    wait_rollout "cert-manager" "cert-manager" cert-manager cert-manager-webhook cert-manager-cainjector

    apply_pinned "cnpg" "deploy/cnpg/install" "crd/clusters.postgresql.cnpg.io"
    wait_rollout "cnpg" "cnpg-system" cnpg-controller-manager

    # CRDs BEFORE the controller, per the crashloop in the header.
    apply_pinned "eso-crds" "deploy/external-secrets/crds" "crd/externalsecrets.external-secrets.io"
    apply_pinned "external-secrets" "deploy/external-secrets/install" "deploy/external-secrets" "external-secrets"
    wait_rollout "external-secrets" "external-secrets" external-secrets external-secrets-webhook

    apply_pinned "ingress-nginx" "deploy/ingress-nginx/install" "ingressclass/${INGRESS_CLASS}"
    wait_rollout "ingress-nginx" "ingress-nginx" ingress-nginx-controller

    ensure_monitoring

    # LAST: the issuer's admission webhook is cert-manager's, and it must be
    # serving before a ClusterIssuer can be admitted.
    ensure_cluster_issuer

    collect_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
