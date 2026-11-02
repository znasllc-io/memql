#!/usr/bin/env bash
# Dedicated per-cluster email authority. Operator authorization stays in az;
# pods exchange their own short-lived projected assertions. No CLI tokens,
# client secrets, sending keys, or another installation's credentials are copied.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/capability.sh"
source "${SCRIPT_DIR}/../lib/azure-email-installation.sh"
cap_init "deploy.configureAzureEmail" "Authorize this cluster's email setup in its own Azure resource group."
for field in subscriptionId resourceGroup clusterName emailResourceGroup emailIdentityName emailService communicationService emailLocation emailDataLocation; do
    cap_spec_param_required "$field" "Installation choice: $field"
done
cap_spec_param "namespace" "Engine namespace (default memql)"
cap_spec_param "replyTo" "Optional operator reply-to mailbox"
cap_spec_param "dryRun" "Validate and describe choices without contacting Azure or Kubernetes"
readonly UUID_RE='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
readonly CONTRIBUTOR='b24988ac-6180-42a0-ab88-20f7382dd24c'
KUBE_FILE=""

function cleanup() {
    local rc=$?
    # Preserve the capability's failure while cleaning up its kubeconfig.
    set +e
    if [[ -n "$KUBE_FILE" ]]; then rm -f "$KUBE_FILE"; fi
    (exit "$rc")
    _cap_on_exit
}
function validate_arguments() {
    SUB="$(cap_param subscriptionId '')"
    CLUSTER_RG="$(cap_param resourceGroup '')"
    CLUSTER="$(cap_param clusterName '')"
    GROUP="$(cap_param emailResourceGroup '')"
    IDENTITY="$(cap_param emailIdentityName '')"
    EMAIL="$(cap_param emailService '')"
    COMM="$(cap_param communicationService '')"
    LOCATION="$(cap_param emailLocation '')"
    DATA_LOCATION="$(cap_param emailDataLocation '')"
    NAMESPACE="$(cap_param namespace memql)"
    REPLY_TO="$(cap_param replyTo '')"
    DRY_RUN="$(cap_bool_str dryRun false)"
    [[ "$SUB" =~ $UUID_RE ]] || cap_fail 2 "subscriptionId must be an Azure subscription identifier"
    local value
    for value in "$CLUSTER_RG" "$GROUP"; do
        [[ "$value" =~ ^[a-zA-Z0-9_-]{1,90}$ ]] || cap_fail 2 "resource group names must use letters, digits, underscores or hyphens"
    done
    [[ "$GROUP" != "$CLUSTER_RG" ]] || cap_fail 2 "emailResourceGroup must be dedicated to email, separate from the cluster group"
    for value in "$CLUSTER" "$IDENTITY" "$EMAIL" "$COMM"; do
        [[ "$value" =~ ^[a-zA-Z0-9-]{1,63}$ ]] || cap_fail 2 "cluster, identity and service names must use 1–63 letters, digits or hyphens"
    done
    [[ "$LOCATION" =~ ^[a-z][a-z0-9]{1,39}$ ]] || cap_fail 2 "emailLocation must be an Azure region"
    [[ "$NAMESPACE" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || cap_fail 2 "namespace must be a Kubernetes namespace"
    case "$DATA_LOCATION" in
        'United States'|Europe|'United Kingdom'|Australia|'Asia Pacific'|Brazil|Canada|France|Germany|India|Japan|Korea|Norway|Switzerland|'United Arab Emirates'|Africa) ;;
        *) cap_fail 2 "emailDataLocation must be a supported Azure email data location" ;;
    esac
    [[ -z "$REPLY_TO" || "$REPLY_TO" =~ ^[^@[:space:]\<\>\,\;]+@[^@[:space:]\<\>\,\;]+\.[^@[:space:]\<\>\,\;]+$ ]] || cap_fail 2 "replyTo must be one mailbox"
}
function require_tools() {
    local tool
    for tool in az kubectl jq openssl; do command -v "$tool" >/dev/null || cap_fail 4 "$tool is required"; done
}
function inspect_cluster() {
    EXPECTED_CLUSTER_ID="/subscriptions/$SUB/resourceGroups/$CLUSTER_RG/providers/Microsoft.ContainerService/managedClusters/$CLUSTER"
    email_inspect_cluster
    # Enforce physical isolation before any write. Azure group create is an
    # upsert, so check-absent/tag checks alone cannot fence simultaneous installs.
    [[ "$GROUP" == *"-$ISSUER_HASH" ]] || cap_fail 3 "emailResourceGroup must end with this cluster's issuer fingerprint; use the installation's inspected choices"
}
function ensure_group() {
    local exists group provider registration
    exists="$(az group exists --subscription "$SUB" --name "$GROUP" -o tsv --only-show-errors)" || cap_fail 5 "could not inspect the email resource group"
    if [[ "$exists" == true ]]; then
        group="$(az group show --subscription "$SUB" --name "$GROUP" -o json --only-show-errors)" || cap_fail 5 "could not read the email resource group"
        jq -e --arg issuer "$ISSUER" --arg location "$LOCATION" '.tags["memql-email-issuer"] == $issuer and .location == $location' <<<"$group" >/dev/null || cap_fail 3 "the email resource group belongs to another installation or location; choose a dedicated group"
    elif [[ "$exists" == false ]]; then
        az group create --subscription "$SUB" --name "$GROUP" --location "$LOCATION" --tags "memql-email-issuer=$ISSUER" -o none --only-show-errors || cap_fail 5 "could not create the email resource group"
        cap_changed
    else cap_fail 5 "Azure did not confirm resource-group existence"; fi
    for provider in Microsoft.Communication Microsoft.EventGrid; do
        registration="$(az provider show --subscription "$SUB" --namespace "$provider" --query registrationState -o tsv --only-show-errors)" || cap_fail 5 "could not inspect email provider registration"
        if [[ "$registration" != Registered ]]; then
            az provider register --subscription "$SUB" --namespace "$provider" --wait -o none --only-show-errors || cap_fail 5 "could not register the email provider using operator authorization"
            cap_changed
        fi
    done
}
function ensure_identity() {
    local identities count identity subject name federations found
    identities="$(az identity list --subscription "$SUB" --resource-group "$GROUP" -o json --only-show-errors)" || cap_fail 5 "could not list email identities"
    count="$(jq --arg name "$IDENTITY" '[.[] | select(.name == $name)] | length' <<<"$identities")"
    if [[ "$count" == 0 ]]; then
        identity="$(az identity create --subscription "$SUB" --resource-group "$GROUP" --name "$IDENTITY" --location "$LOCATION" --tags "memql-email-issuer=$ISSUER" -o json --only-show-errors)" || cap_fail 5 "could not create this cluster's email identity"
        cap_changed
    elif [[ "$count" == 1 ]]; then
        identity="$(jq --arg name "$IDENTITY" '.[] | select(.name == $name)' <<<"$identities")"
        jq -e --arg issuer "$ISSUER" '.tags["memql-email-issuer"] == $issuer' <<<"$identity" >/dev/null || cap_fail 3 "the email identity belongs to another installation"
    else cap_fail 3 "the email identity is ambiguous"; fi
    CLIENT_ID="$(jq -r '.clientId // empty' <<<"$identity")"
    PRINCIPAL_ID="$(jq -r '.principalId // empty' <<<"$identity")"
    [[ "$CLIENT_ID" =~ $UUID_RE && "$PRINCIPAL_ID" =~ $UUID_RE ]] || cap_fail 5 "Azure has not prepared the email identity; run this step again"
    federations="$(az identity federated-credential list --subscription "$SUB" --resource-group "$GROUP" --identity-name "$IDENTITY" -o json --only-show-errors)" || cap_fail 5 "could not inspect email identity federation"
    # Reject extra bindings too: this identity must never authorize a sibling
    # cluster, even if it also has the three correct bindings for this one.
    jq -e --arg issuer "$ISSUER" --arg prefix "system:serviceaccount:$NAMESPACE:" 'all(.[]; .issuer == $issuer and .audiences == ["api://AzureADTokenExchange"] and (.subject == ($prefix+"memql-engine") or .subject == ($prefix+"memql-deploy") or .subject == ($prefix+"memql-engine-workbench")))' <<<"$federations" >/dev/null || cap_fail 3 "the email identity has a federation outside this installation"
    for name in memql-engine memql-deploy memql-engine-workbench; do
        subject="system:serviceaccount:$NAMESPACE:$name"
        found="$(jq --arg subject "$subject" '[.[] | select(.subject == $subject)] | length' <<<"$federations")"
        if [[ "$found" == 0 ]]; then
            az identity federated-credential create --subscription "$SUB" --resource-group "$GROUP" --identity-name "$IDENTITY" --name "email-$name" --issuer "$ISSUER" --subject "$subject" --audiences api://AzureADTokenExchange -o none --only-show-errors || cap_fail 5 "could not authorize an engine service account"
            cap_changed
        fi
    done
}
function ensure_role() {
    local scope assignments
    scope="/subscriptions/$SUB/resourceGroups/$GROUP"
    assignments="$(az role assignment list --subscription "$SUB" --scope "$scope" --all -o json --only-show-errors)" || cap_fail 5 "could not inspect the email identity's role assignment"
    if ! jq -e --arg principal "$PRINCIPAL_ID" --arg scope "$scope" --arg role "$CONTRIBUTOR" 'any(.[]; .principalId == $principal and (.scope | ascii_downcase) == ($scope | ascii_downcase) and (.roleDefinitionId | ascii_downcase | endswith("/"+$role)))' <<<"$assignments" >/dev/null; then
        az role assignment create --subscription "$SUB" --assignee-object-id "$PRINCIPAL_ID" --assignee-principal-type ServicePrincipal --role "$CONTRIBUTOR" --scope "$scope" -o none --only-show-errors || cap_fail 5 "could not grant Contributor on the dedicated email group; the operator needs role-assignment authority"
        cap_changed
    fi
}
function install_public_configuration() {
    local config previous manifest
    config="$(jq -cn --arg tenant "$TENANT" --arg client "$CLIENT_ID" --arg sub "$SUB" --arg group "$GROUP" --arg location "$DATA_LOCATION" --arg email "$EMAIL" --arg comm "$COMM" --arg reply "$REPLY_TO" '{tenantId:$tenant,clientId:$client,subscriptionId:$sub,resourceGroup:$group,dataLocation:$location,emailService:$email,communicationService:$comm,replyTo:$reply}')"
    umask 077
    KUBE_FILE="$(mktemp)"
    trap cleanup EXIT
    az aks get-credentials --subscription "$SUB" --resource-group "$CLUSTER_RG" --name "$CLUSTER" --file "$KUBE_FILE" --overwrite-existing --only-show-errors >/dev/null || cap_fail 5 "could not connect to the selected cluster"
    previous="$(kubectl --kubeconfig "$KUBE_FILE" -n "$NAMESPACE" get configmap memql-email-installation --ignore-not-found -o json)" || cap_fail 5 "could not read this cluster's email installation choices"
    if [[ -n "$previous" ]]; then
        jq -e --arg config "$config" '.data.MEMQL_EMAIL_INSTALLATION == $config' <<<"$previous" >/dev/null || cap_fail 3 "the installed email choices differ; review the existing connection before replacing them"
        return 0
    fi
    manifest="$(jq -cn --arg namespace "$NAMESPACE" --arg config "$config" '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"memql-email-installation",namespace:$namespace},data:{MEMQL_EMAIL_INSTALLATION:$config}}')"
    kubectl --kubeconfig "$KUBE_FILE" -n "$NAMESPACE" create -f - <<<"$manifest" >/dev/null || cap_fail 5 "could not install public email choices; ensure the engine namespace exists, then retry"
    cap_changed
}
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    validate_arguments
    cap_result_set resourceGroup "$GROUP"
    cap_result_set emailService "$EMAIL"
    cap_result_set communicationService "$COMM"
    cap_result_set_raw dryRun "$DRY_RUN"
    if [[ "$DRY_RUN" == true ]]; then
        cap_info "Would authorize a dedicated cluster identity on the email group and install public sender-setup choices."
        cap_ok
        return 0
    fi
    require_tools
    inspect_cluster
    ensure_group
    ensure_identity
    ensure_role
    install_public_configuration
    cap_result_set clientId "$CLIENT_ID"
    cap_result_set status authorized
    cap_info "Email authorization is installed. Engine startup will prepare the sender; repeat runs preserve its progress."
    cap_ok
}
main "$@"
