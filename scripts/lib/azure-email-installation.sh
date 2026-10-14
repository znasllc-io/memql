#!/usr/bin/env bash
# Shared read-only identity binding for installation inspection and authorization.
# Callers supply validated subscription/cluster selectors. This computes resource
# identity, not installation policy; default choices live in deployment logic.
function email_inspect_cluster() {
    TENANT="$(az account show --subscription "$SUB" --query tenantId -o tsv --only-show-errors)" || cap_fail 4 "sign in to the operator's Azure account first"
    [[ "$TENANT" =~ $UUID_RE ]] || cap_fail 5 "Azure returned an invalid tenant identifier"
    local cluster
    cluster="$(az aks show --subscription "$SUB" --resource-group "$CLUSTER_RG" --name "$CLUSTER" -o json --only-show-errors)" || cap_fail 4 "the selected AKS cluster is unavailable"
    CLUSTER_ID="$(jq -r '.id // empty' <<<"$cluster")"
    ISSUER="$(jq -r '.oidcIssuerProfile.issuerUrl // empty' <<<"$cluster")"
    CLUSTER_LOCATION="$(jq -r '.location // empty' <<<"$cluster")"
    [[ "$(tr '[:upper:]' '[:lower:]' <<<"$CLUSTER_ID")" == "$(tr '[:upper:]' '[:lower:]' <<<"$EXPECTED_CLUSTER_ID")" && "$ISSUER" == https://* ]] || cap_fail 3 "the selected cluster must have its own OIDC issuer enabled"
    [[ ${#ISSUER} -le 256 && "$CLUSTER_LOCATION" =~ ^[a-z][a-z0-9]{1,39}$ ]] || cap_fail 3 "the cluster issuer or location is invalid"
    ISSUER_HASH="$(printf '%s' "$ISSUER" | openssl dgst -sha256 -r | cut -d ' ' -f 1)"
    [[ "$ISSUER_HASH" =~ ^[a-f0-9]{64}$ ]] || cap_fail 5 "could not bind the email resources to this cluster"
}
