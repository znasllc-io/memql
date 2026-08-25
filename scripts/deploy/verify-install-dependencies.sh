#!/usr/bin/env bash
#
# scripts/deploy/verify-install-dependencies.sh
# =============================================
#
# Capability: deploy.verifyInstallDependencies -- assert BY EXISTENCE that the
# objects every committed manifest assumes are present actually are, and report
# which are not.
#
# Backend for the `verifyInstallDependencies` deployment action (memql#4473,
# epic memql#4490).
#
# WHY EXISTENCE AND NOT HEALTH -- this is the whole point of the script.
# `repairInstance` is an argoSync, and that is RIGHT for drift: ArgoCD
# reconciling git IS the desired-state engine. But this entire class of failure
# is INVISIBLE to health, because the objects were never created. Nothing is
# unhealthy. Argo reports what it applied; it has no opinion at all about a CRD
# that was never installed. So a repair that only re-syncs declares success on
# a cluster that cannot work.
#
# The sharpest case: a missing Secret named by a volume does not error. The pod
# sits in ContainerCreating FOREVER --
#
#   MountVolume.SetUp failed for volume "memql-ca" : secret "memql-ca" not found
#
# -- with no log line at all, because the container never starts. Seven mesh
# Deployments hung together for seventeen minutes with nothing to read.
#
# IT REPORTS, IT DOES NOT DECIDE. Exit 0 whether or not everything is present;
# `passed` and `missing` are in the result envelope and the automation's logic
# branches on them. A non-zero exit here means the CHECK could not run --
# no kubectl, no cluster -- which is a different fact from "a dependency is
# absent" and must not be conflated with it. That is contract rule 7, and it is
# also what lets one call answer for a cluster with three problems instead of
# stopping at the first.
#
# THE LIST IS LISTED, NOT DISCOVERED. The failure worth catching is a NEW
# undeclared dependency arriving in the manifests and nobody extending this
# check -- and discovery, by construction, would wave exactly that through.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok (checks ran; read `passed`) | 2 bad param | 4 prerequisite missing | 5 the check itself failed
#
# Refs: memql#4490 memql#4473 memql#4484 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.verifyInstallDependencies" \
    "Check by existence that an instance's undeclared install dependencies are present, and report which are not."

cap_spec_param "namespace"    "namespace the instance runs in (default memql)"
cap_spec_param "ingressClass" "IngressClass the manifests declare (default nginx)"
cap_spec_param "clusterIssuer" "ClusterIssuer the front door annotates (default letsencrypt-prod)"
cap_spec_param "requireIssuer" "treat a missing ClusterIssuer as a failure rather than a warning (default true)"
cap_spec_param "requireMonitoring" "check that the alert rules are installed and evaluable (default true). Absent alerting is the one dependency whose absence looks exactly like health"

cap_handle_meta "$@"
cap_parse_flags "$@"

NAMESPACE="$(cap_param namespace "memql")"
INGRESS_CLASS="$(cap_param ingressClass "nginx")"
CLUSTER_ISSUER="$(cap_param clusterIssuer "letsencrypt-prod")"
REQUIRE_ISSUER="$(cap_param requireIssuer "true")"
REQUIRE_MONITORING="$(cap_param requireMonitoring "true")"

PRESENT=""
MISSING=""
DETAIL=""

function check_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed or not on PATH"
    kubectl cluster-info &>/dev/null \
        || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first. Reporting every dependency as 'missing' because the cluster is unreachable would be a confident wrong answer."
}

# check <label> <why-it-matters> <kubectl get args...>
function check() {
    local label="$1" why="$2"; shift 2
    if kubectl get "$@" -o name &>/dev/null; then
        PRESENT="${PRESENT:+${PRESENT},}${label}"
        cap_info "present: ${label}"
        return 0
    fi
    MISSING="${MISSING:+${MISSING},}${label}"
    DETAIL="${DETAIL:+${DETAIL}; }${label}: ${why}"
    cap_warn "MISSING: ${label} -- ${why}"
    return 0
}

function run_checks() {
    check "cert-manager" \
        "no Certificate CRD, so every cert-manager object in the manifests is rejected at apply with a schema error naming no operator" \
        crd/certificates.cert-manager.io

    check "cnpg" \
        "no CloudNativePG Cluster CRD, so the database object is rejected at apply and the sync fails as a whole" \
        crd/clusters.postgresql.cnpg.io

    check "eso-crds" \
        "no ExternalSecret CRD. The ESO CONTROLLER crashloops while the WEBHOOK stays Running, so the namespace shows one healthy pod beside one restarting one and nothing names a CRD" \
        crd/externalsecrets.external-secrets.io

    check "external-secrets" \
        "no ESO controller, so every ExternalSecret is an inert object and memql-secrets never gains a key" \
        deploy/external-secrets -n external-secrets

    check "ingress-nginx" \
        "no ${INGRESS_CLASS} IngressClass. An Ingress naming an absent class is a VALID object nothing acts on: ADDRESS stays empty, which reads as 'still starting' forever" \
        "ingressclass/${INGRESS_CLASS}"

    check "memql-ca" \
        "the CA bundle every mesh Deployment mounts. Absent, pods sit in ContainerCreating FOREVER with no log line, because the container never starts" \
        "secret/memql-ca" -n "$NAMESPACE"

    check "identity-tls" \
        "identity's serving certificate. Absent, identity never starts and every node's JWKS fetch against https://identity:8085 fails" \
        "secret/identity-tls" -n "$NAMESPACE"

    check "memql-secrets" \
        "the merge shell. Both memql-secrets ExternalSecrets use creationPolicy: Merge, which merges into an existing Secret and does not create one -- so without it the Secret simply never appears" \
        "secret/memql-secrets" -n "$NAMESPACE"

    if [[ "$REQUIRE_MONITORING" == "true" ]]; then
        # Two checks, because either alone passes on a broken half: the CRD
        # without the rules is an operator evaluating nothing, and the rules
        # are not applicable without the CRD.
        check "prometheus-operator" \
            "no PrometheusRule CRD, so deploy/k8s/monitoring cannot be applied and no MemQL alert can be evaluated" \
            crd/prometheusrules.monitoring.coreos.com

        check "memql-alert-rules" \
            "the PrometheusRules are not on this cluster. This is the one absence that looks exactly like health -- a cluster with no alerts is QUIET, and quiet is what a working system sounds like. memql#4460 ran an instance's whole life with WAL archiving broken because two alerts that would have fired were never deployed" \
            prometheusrule/memql-database -n "$NAMESPACE"
    else
        cap_info "--requireMonitoring=false; not checking the alert rules"
    fi

    if [[ "$REQUIRE_ISSUER" == "true" ]]; then
        check "$CLUSTER_ISSUER" \
            "the ACME issuer the front-door annotations name. Absent, Certificates stay Pending and ingress-nginx serves its self-signed default -- the site LOADS, with a browser warning, which is why this one is easy to miss" \
            "clusterissuer/${CLUSTER_ISSUER}"
    else
        cap_info "--requireIssuer=false; not checking for ClusterIssuer ${CLUSTER_ISSUER}"
    fi
}

function collect_result() {
    local passed="true"
    [[ -n "$MISSING" ]] && passed="false"

    cap_result_set_raw "passed" "$passed"
    cap_result_set "present" "$PRESENT"
    cap_result_set "missing" "$MISSING"
    cap_result_set "detail"  "$DETAIL"
    cap_result_set "namespace" "$NAMESPACE"
    return 0
}

function main() {
    check_prerequisites
    cap_info "checking install dependencies by EXISTENCE in ${NAMESPACE} -- this whole class is invisible to a health check"
    run_checks
    collect_result
    # Deliberately cap_ok even when something is missing: the check RAN, and
    # that is what exit 0 reports here. `passed` carries the answer.
    cap_ok
}

main "$@"
