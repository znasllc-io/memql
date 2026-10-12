#!/usr/bin/env bash
# Read the selected cluster's public installation facts; make no cloud changes.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/capability.sh"
source "${SCRIPT_DIR}/../lib/azure-email-installation.sh"
cap_init "deploy.inspectAzureEmail" "Read this cluster's email installation choices."
for field in subscriptionId resourceGroup clusterName; do
    cap_spec_param_required "$field" "Cluster selector: $field"
done
cap_spec_param "namespace" "Engine namespace (default memql)"
readonly UUID_RE='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
KUBE_FILE=""
function cleanup() {
    if [[ -n "$KUBE_FILE" ]]; then rm -f "$KUBE_FILE"; fi
    _cap_on_exit
}
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    SUB="$(cap_param subscriptionId '')"
    CLUSTER_RG="$(cap_param resourceGroup '')"
    CLUSTER="$(cap_param clusterName '')"
    NAMESPACE="$(cap_param namespace memql)"
    [[ "$SUB" =~ $UUID_RE && "$CLUSTER_RG" =~ ^[a-zA-Z0-9_-]{1,90}$ && "$CLUSTER" =~ ^[a-zA-Z0-9-]{1,63}$ && "$NAMESPACE" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || cap_fail 2 "valid cluster selectors and namespace are required"
    local tool geography previous installed client identities identity group region
    for tool in az kubectl jq openssl; do command -v "$tool" >/dev/null || cap_fail 4 "$tool is required"; done
    EXPECTED_CLUSTER_ID="/subscriptions/$SUB/resourceGroups/$CLUSTER_RG/providers/Microsoft.ContainerService/managedClusters/$CLUSTER"
    email_inspect_cluster
    geography="$(az account list-locations --subscription "$SUB" --query "[?name=='$CLUSTER_LOCATION'].metadata.geographyGroup | [0]" -o tsv --only-show-errors)" || cap_fail 5 "could not inspect the cluster's data geography"
    umask 077
    KUBE_FILE="$(mktemp)"
    trap cleanup EXIT
    az aks get-credentials --subscription "$SUB" --resource-group "$CLUSTER_RG" --name "$CLUSTER" --file "$KUBE_FILE" --overwrite-existing --only-show-errors >/dev/null || cap_fail 5 "could not connect to the selected cluster"
    previous="$(kubectl --kubeconfig "$KUBE_FILE" -n "$NAMESPACE" get configmap memql-email-installation --ignore-not-found -o json)" || cap_fail 5 "could not read this cluster's email installation choices"
    installed='{}'
    if [[ -n "$previous" ]]; then
        # Project only the public choice fields; never echo arbitrary ConfigMap data.
        installed="$(jq -ce '.data.MEMQL_EMAIL_INSTALLATION | fromjson | {resourceGroup, dataLocation, emailService, communicationService, replyTo}' <<<"$previous")" || cap_fail 3 "the saved email installation choices are invalid"
        group="$(jq -r '.resourceGroup' <<<"$installed")"
        client="$(jq -r '.data.MEMQL_EMAIL_INSTALLATION | fromjson | .clientId' <<<"$previous")"
        [[ "$group" =~ ^[a-zA-Z0-9_-]{1,90}$ && "$client" =~ $UUID_RE ]] || cap_fail 3 "the saved email identity is invalid"
        identities="$(az identity list --subscription "$SUB" --resource-group "$group" -o json --only-show-errors)" || cap_fail 5 "could not read the installed email identity"
        identity="$(jq -er --arg client "$client" '[.[] | select(.clientId == $client)] | if length == 1 then .[0].name else error("ambiguous identity") end' <<<"$identities")" || cap_fail 3 "the installed email identity is missing or ambiguous"
        region="$(az group show --subscription "$SUB" --name "$group" --query location -o tsv --only-show-errors)" || cap_fail 5 "could not read the installed email region"
        installed="$(jq -c --arg identity "$identity" --arg location "$region" '. + {identityName:$identity,location:$location}' <<<"$installed")"
    fi
    cap_result_set issuerHash "$ISSUER_HASH"
    cap_result_set serviceSuffix "${ISSUER_HASH:0:48}"
    cap_result_set location "$CLUSTER_LOCATION"
    cap_result_set geography "$geography"
    cap_result_set_raw installed "$installed"
    cap_ok
}
main "$@"
