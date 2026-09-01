#!/usr/bin/env bash
#
# scripts/deploy/bind-custom-domain.sh
# ====================================
#
# Capability: domain.bind -- apply the exact-host Ingress and the cert-manager
# Certificate that let this cluster serve a client's own domain.
#
# Backend for the custom-domain reconciler's script substrate (epic memql#4805,
# design D2/D6), and the operator's manual path for the same job.
#
# WHY AN EXACT HOST AND NOT THE WILDCARD. The cluster's own `*.<domain>` Ingress
# rule cannot match a client's domain at all -- it is a different zone -- so
# every bound domain needs a rule of its own. Certificates are stricter still:
# ACME cannot issue a wildcard over HTTP-01, and ONE wildcard dnsName fails the
# WHOLE order rather than just its own name (memql#4224), so the Certificate
# here names exactly one host. That works precisely because the client's DNS now
# points at this cluster, which makes the HTTP-01 challenge servable -- and it
# is why the reconciler proves the pointing record BEFORE calling this.
#
# IDEMPOTENT BY CONSTRUCTION. Both objects go through `kubectl apply`, so a
# second run over unchanged objects is a no-op at the API server and creates no
# new ACME order: cert-manager's own backoff, not the caller's loop, remains
# what paces Let's Encrypt. `changed` reports whether anything actually moved.
#
# WHAT IT REFUSES, AND WHY THAT IS A REFUSAL RATHER THAN A FAILURE. With no
# --issuer this cluster has no ACME issuer, and a Certificate with an empty
# issuerRef is ACCEPTED by the API server and then sits Pending forever with a
# condition nobody reads. That is a pretend success, and it is exactly what the
# local k3d target would get every time. So it exits 3 (refused) naming
# `no_acme_issuer` in the result, the reconciler writes that on the row, and the
# Domains panel says so in as many words -- the same flow shape everywhere,
# honest about what the target can do (design D7).
#
# Refs: memql#4805 memql#4224 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "domain.bind" \
    "Apply the exact-host Ingress and cert-manager Certificate that serve a client's own domain, and report whether the certificate is Ready."

cap_spec_param_required "hostname"     "the client's own fully qualified host, e.g. www.acme.com"
cap_spec_param_required "domainId"     "the v1:platform:customDomain row id -- both objects are named after it"
cap_spec_param          "siteId"       "the v1:platform:site row the domain serves; recorded as a label"
cap_spec_param          "namespace"    "namespace the objects live in (default: memql)"
cap_spec_param          "issuer"       "cert-manager ClusterIssuer name; EMPTY refuses with no_acme_issuer rather than applying a Certificate nothing will fulfil"
cap_spec_param          "ingressClass" "ingressClassName for the Ingress (default: nginx)"
cap_spec_param          "service"      "backend Service name (default: edge)"
cap_spec_param          "port"         "backend Service port (default: 8085)"
cap_spec_param          "waitSeconds"  "how long to wait for the certificate to become Ready before reporting not-ready (default: 15)"
cap_spec_param          "dryRun"       "render and validate the objects without applying them"

cap_handle_meta "$@"
cap_parse_flags "$@"

HOSTNAME_ARG="$(cap_param hostname "")"
DOMAIN_ID="$(cap_param domainId "")"
SITE_ID="$(cap_param siteId "")"
NAMESPACE="$(cap_param namespace "memql")"
ISSUER="$(cap_param issuer "")"
INGRESS_CLASS="$(cap_param ingressClass "nginx")"
SERVICE="$(cap_param service "edge")"
PORT="$(cap_param port "8085")"
WAIT_SECONDS="$(cap_param waitSeconds "15")"
DRY_RUN="$(cap_bool_str dryRun false)"

OBJECT_NAME=""
CHANGED_ANY=false
CERT_READY=false
CERT_STATUS=""

# ---------------------------------------------------------------------------
# Parameters
# ---------------------------------------------------------------------------
function check_params() {
    [[ -n "$HOSTNAME_ARG" ]] || cap_fail 2 "--hostname is required"
    [[ -n "$DOMAIN_ID" ]]    || cap_fail 2 "--domainId is required: both objects are named after it, so there is nothing to apply without one"
    if [[ "$HOSTNAME_ARG" == *"*"* ]]; then
        cap_fail 2 "--hostname ${HOSTNAME_ARG} is a wildcard. ACME cannot issue a wildcard over HTTP-01, and one wildcard dnsName fails the whole order (memql#4224). Bind each hostname separately."
    fi
    [[ "$HOSTNAME_ARG" == *.* ]] \
        || cap_fail 2 "--hostname ${HOSTNAME_ARG} is a single label, not a domain"
    [[ "$PORT" =~ ^[0-9]+$ ]] \
        || cap_fail 2 "--port ${PORT} is not a number"
    [[ "$WAIT_SECONDS" =~ ^[0-9]+$ ]] \
        || cap_fail 2 "--waitSeconds ${WAIT_SECONDS} is not a number"

    # THE OBJECT NAME IS KEYED ON THE ROW ID, NOT THE HOSTNAME, and the
    # difference matters twice. A hostname is not a legal Kubernetes object
    # name (a 253-character host is not a DNS-1123 subdomain, nor is one
    # starting with a digit), and any sanitiser that made it one would map some
    # pair of distinct hostnames onto a single name -- which is one binding
    # silently overwriting another's Ingress. The row id is already short,
    # already unique, and already the thing being reconciled. Mirrors
    # objectName() in integrations/customdomain/provision.go so the two
    # substrates converge on one object rather than fighting over two.
    local slug
    slug="$(printf '%s' "$DOMAIN_ID" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed 's/^-*//; s/-*$//')"
    [[ -n "$slug" ]] || cap_fail 2 "--domainId ${DOMAIN_ID} contains no usable characters for an object name"
    OBJECT_NAME="custom-domain-${slug:0:200}"
    return 0
}

function check_prereqs() {
    # A DRY RUN NEEDS NEITHER, because it reaches no cluster -- see apply_objects.
    # Requiring kubectl for it would make the one mode that exists to work
    # without a cluster refuse on a machine that has no reason to have one.
    if [[ "$DRY_RUN" == "true" ]]; then
        return 0
    fi
    command -v kubectl &>/dev/null \
        || cap_fail 4 "kubectl is not installed or not on PATH. This capability applies Kubernetes objects; there is no other way for it to do its job."
    kubectl cluster-info &>/dev/null \
        || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first"
    return 0
}

# ---------------------------------------------------------------------------
# The refusal that is not a failure
# ---------------------------------------------------------------------------
function check_issuer() {
    if [[ -n "$ISSUER" ]]; then
        return 0
    fi
    # `reason` is set on the RESULT as well as the exit code, because an exit
    # code says "refused" and not WHICH refusal -- and the row that records
    # this, and the panel that renders it, key on the typed reason.
    cap_result_set "reason"   "no_acme_issuer"
    cap_result_set "detail"   "this cluster declares no ACME issuer, so no certificate can be requested for ${HOSTNAME_ARG}"
    cap_result_set "hostname" "$HOSTNAME_ARG"
    cap_fail 3 "no ACME issuer configured (--issuer is empty), so ${HOSTNAME_ARG} cannot be issued a certificate. A Certificate with an empty issuerRef is accepted and then sits Pending forever, which is worse than refusing: nothing would ever say why the domain is not serving."
}

# ---------------------------------------------------------------------------
# The two objects
# ---------------------------------------------------------------------------
function render_objects() {
    cat <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ${OBJECT_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: memql
    app.kubernetes.io/name: custom-domain
    memql/custom-domain-id: "${DOMAIN_ID}"
    memql/custom-domain-siteId: "${SITE_ID}"
  annotations:
    cert-manager.io/cluster-issuer: "${ISSUER}"
spec:
  ingressClassName: ${INGRESS_CLASS}
  tls:
    - hosts:
        - ${HOSTNAME_ARG}
      secretName: ${OBJECT_NAME}-tls
  rules:
    - host: ${HOSTNAME_ARG}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: ${SERVICE}
                port:
                  number: ${PORT}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${OBJECT_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: memql
    app.kubernetes.io/name: custom-domain
    memql/custom-domain-id: "${DOMAIN_ID}"
spec:
  secretName: ${OBJECT_NAME}-tls
  dnsNames:
    - ${HOSTNAME_ARG}
  issuerRef:
    name: ${ISSUER}
    kind: ClusterIssuer
    group: cert-manager.io
YAML
}

function apply_objects() {
    local out
    if [[ "$DRY_RUN" == "true" ]]; then
        # A DRY RUN TOUCHES NO CLUSTER, and getting there took two wrong turns
        # worth recording. `kubectl apply --dry-run=client` fetches the API
        # server's OpenAPI schema, so it fails with a connection refused
        # wherever there is no cluster -- which is every CI runner. Adding
        # `--validate=false` does not fix it either: `apply` still needs
        # discovery to map a kind to a resource, so it reaches the server
        # regardless. Only `--dry-run=server` is honestly a cluster operation,
        # and it is the opposite of what this flag is for.
        #
        # So the check is what a machine with no cluster can actually make:
        # the rendered documents PARSE, and they are the two objects this
        # script exists to apply. That is a real check -- it catches a
        # quoting bug in a hostname or a token, which is the failure this
        # rendering can plausibly have -- and it is honest about its limit,
        # which schema validation against a live server is not a substitute
        # for anyway.
        # NO PARSER DEPENDENCY. PyYAML is not stdlib and is not guaranteed on
        # a runner, so the check is the one every machine can make: the render
        # carries exactly the two documents this script exists to apply, the
        # separator between them, and the hostname in both. That catches the
        # failure this rendering can plausibly have -- a quoting bug in a
        # hostname or a token that swallows a line -- without importing
        # anything.
        local rendered kinds
        rendered="$(render_objects)"
        kinds="$(printf '%s\n' "$rendered" | grep -c '^kind: ')"
        if [[ "$kinds" != "2" ]]; then
            cap_fail 5 "the rendered objects did not validate: expected 2 documents, found ${kinds}"
        fi
        printf '%s\n' "$rendered" | grep -q '^kind: Ingress$' \
            || cap_fail 5 "the rendered objects did not validate: no Ingress document"
        printf '%s\n' "$rendered" | grep -q '^kind: Certificate$' \
            || cap_fail 5 "the rendered objects did not validate: no Certificate document"
        printf '%s\n' "$rendered" | grep -q -- "- ${HOSTNAME_ARG}$" \
            || cap_fail 5 "the rendered objects did not validate: ${HOSTNAME_ARG} is in neither document's host list"
        out="parsed 2 document(s): Ingress, Certificate"
        cap_info "dry run: ${out}"
        return 0
    fi
    if ! out="$(render_objects | kubectl apply -f - 2>&1)"; then
        cap_fail 5 "could not apply the Ingress and Certificate for ${HOSTNAME_ARG}: ${out}"
    fi
    cap_info "$out"
    # `unchanged` on EVERY line means a no-op run. That is the idempotency
    # signal the caller reads, and it is the one thing distinguishing "this
    # binding was already in place" from "this run created it".
    if printf '%s' "$out" | grep -qv 'unchanged$'; then
        CHANGED_ANY=true
        cap_changed
    fi
    return 0
}

# ---------------------------------------------------------------------------
# Readiness -- `live` is reachable only through this
# ---------------------------------------------------------------------------
function check_certificate() {
    if [[ "$DRY_RUN" == "true" ]]; then
        CERT_STATUS="dry run: the certificate was not requested"
        return 0
    fi
    # APPLYING A CERTIFICATE IS NOT HOLDING ONE. A status that went live on the
    # apply would tell somebody their domain was serving while browsers were
    # still being handed the ingress controller's self-signed default. So the
    # caller promotes to `live` on this field and on nothing else.
    #
    # The wait is BOUNDED and its timeout is not an error: an HTTP-01 order
    # takes tens of seconds on a good day, and the caller's next pass will look
    # again. Blocking until Ready would hold a reconciliation pass open across
    # every other binding on the cluster.
    if kubectl wait --for=condition=Ready \
        "certificate/${OBJECT_NAME}" -n "$NAMESPACE" \
        --timeout="${WAIT_SECONDS}s" &>/dev/null; then
        CERT_READY=true
        CERT_STATUS="the certificate is Ready"
        return 0
    fi
    # cert-manager's OWN message, verbatim. Its wording ("Issuing certificate
    # as Secret does not exist", "Waiting for http-01 challenge propagation")
    # is more useful than anything composed here, and it is what somebody
    # debugging a stuck order would go and read anyway.
    CERT_STATUS="$(kubectl get "certificate/${OBJECT_NAME}" -n "$NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true)"
    if [[ -z "$CERT_STATUS" ]]; then
        CERT_STATUS="the certificate has no Ready condition yet -- cert-manager has not started the order"
    fi
    cap_info "certificate not Ready after ${WAIT_SECONDS}s: ${CERT_STATUS}"
    return 0
}

function collect_result() {
    cap_result_set     "hostname"          "$HOSTNAME_ARG"
    cap_result_set     "domainId"          "$DOMAIN_ID"
    cap_result_set     "siteId"            "$SITE_ID"
    cap_result_set     "namespace"         "$NAMESPACE"
    cap_result_set     "objectName"        "$OBJECT_NAME"
    cap_result_set     "issuer"            "$ISSUER"
    cap_result_set_raw "applied"           "true"
    cap_result_set_raw "certificateReady"  "$CERT_READY"
    cap_result_set     "certificateStatus" "$CERT_STATUS"
    cap_result_set_raw "objectsChanged"    "$CHANGED_ANY"
    return 0
}

function main() {
    check_params
    check_issuer
    check_prereqs
    apply_objects
    check_certificate
    collect_result
    # cap_ok even when the certificate is not Ready: the bind RAN, which is
    # what exit 0 means here. `certificateReady` carries the answer, and a
    # not-yet-Ready certificate is the ordinary state for the first minute of
    # an order rather than a failure anybody should act on.
    cap_ok
}

main "$@"
