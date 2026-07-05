#!/usr/bin/env bash
set -eo pipefail

# gen-internal-ca.sh -- create the internal self-signed CA + identity
# server cert and load them into the cluster as k8s secrets, so the
# identity node can serve its HTTP surface (/.well-known/jwks.json,
# /node/bootstrap, /healthz) over TLS and every other node trusts it.
#
# Why internal TLS: the node-bootstrap handler rejects plaintext
# (403 insecure_transport). Rather than the insecure-bootstrap bypass,
# the cluster serves identity over TLS with a self-signed cluster CA --
# the same transport posture as production. See issue #527.
#
# Idempotent: re-running regenerates the material and re-applies the
# secrets (kubectl apply). Pods pick up rotated certs on next restart.
#
# Secrets produced (namespace: memql):
#   identity-tls  (kubernetes.io/tls: tls.crt + tls.key)  -- mounted on identity
#   memql-ca      (ca.crt)                                 -- mounted on every node
#
# These hold key material so they live OUTSIDE the kustomize tree
# (never committed); this script is the reproducible source.

#=============================================================================
# CONFIGURATION
#=============================================================================

NAMESPACE="${NAMESPACE:-memql}"
CA_DAYS="${CA_DAYS:-3650}"
CERT_DAYS="${CERT_DAYS:-825}"
# SANs the identity cert must answer to. The engine default is product-neutral
# (in-cluster DNS only). A downstream product stack that fronts identity at a
# public host MUST inject its own public SAN by exporting IDENTITY_SANS with the
# in-cluster names PLUS its host, e.g.:
#   IDENTITY_SANS="DNS:identity,DNS:identity.${NAMESPACE},DNS:identity.${NAMESPACE}.svc,DNS:identity.${NAMESPACE}.svc.cluster.local,DNS:identity.<product-host>"
IDENTITY_SANS="${IDENTITY_SANS:-DNS:identity,DNS:identity.${NAMESPACE},DNS:identity.${NAMESPACE}.svc,DNS:identity.${NAMESPACE}.svc.cluster.local}"

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    command -v openssl >/dev/null || { echo "ERROR: openssl not found"; exit 1; }
    command -v kubectl >/dev/null || { echo "ERROR: kubectl not found"; exit 1; }
}

function generate_ca() {
    echo "INFO: generating self-signed CA (${CA_DAYS}d)..."
    openssl genrsa -out "$WORK/ca.key" 4096 2>/dev/null
    openssl req -x509 -new -nodes -key "$WORK/ca.key" -sha256 -days "$CA_DAYS" \
        -subj "/CN=memql-internal-ca/O=ZNAS LLC" -out "$WORK/ca.crt" 2>/dev/null
}

function generate_identity_cert() {
    echo "INFO: generating identity server cert (${CERT_DAYS}d)..."
    echo "      SANs: ${IDENTITY_SANS}"
    openssl genrsa -out "$WORK/identity.key" 2048 2>/dev/null
    cat > "$WORK/san.cnf" <<EOF
[req]
distinguished_name = dn
req_extensions = v3_req
prompt = no
[dn]
CN = identity
[v3_req]
subjectAltName = ${IDENTITY_SANS}
extendedKeyUsage = serverAuth
EOF
    openssl req -new -key "$WORK/identity.key" -out "$WORK/identity.csr" -config "$WORK/san.cnf" 2>/dev/null
    openssl x509 -req -in "$WORK/identity.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" \
        -CAcreateserial -out "$WORK/identity.crt" -days "$CERT_DAYS" -sha256 \
        -extensions v3_req -extfile "$WORK/san.cnf" 2>/dev/null
    openssl verify -CAfile "$WORK/ca.crt" "$WORK/identity.crt"
}

function apply_secrets() {
    echo "INFO: applying secrets to namespace ${NAMESPACE}..."
    kubectl create secret tls identity-tls -n "$NAMESPACE" \
        --cert="$WORK/identity.crt" --key="$WORK/identity.key" \
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl create secret generic memql-ca -n "$NAMESPACE" \
        --from-file=ca.crt="$WORK/ca.crt" \
        --dry-run=client -o yaml | kubectl apply -f -
}

function main() {
    check_prerequisites
    WORK="$(mktemp -d)"
    trap 'rm -rf "$WORK"' EXIT
    generate_ca
    generate_identity_cert
    apply_secrets
    echo "INFO: done. Restart the identity + node pods to pick up the cert/CA."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
