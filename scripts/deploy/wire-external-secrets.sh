#!/usr/bin/env bash
#
# scripts/deploy/wire-external-secrets.sh
# =======================================
#
# Capability: deploy.wireExternalSecrets -- point this instance's External
# Secrets at THIS instance's Key Vault, authenticating as THIS instance's
# managed identity.
#
# Backend for the `wireExternalSecrets` deployment action (memql#4474, epic
# memql#4490). Step 10 of the eleven between substrate and argoSync.
#
# WHY IT RENDERS RATHER THAN APPLIES. deploy/external-secrets/secretstore.yaml
# and externalsecret-memql.yaml are committed with a placeholder vault URL, a
# literal tenant id and a literal client id -- values belonging to ONE install,
# in a file every install shares. Applying them verbatim on a second cluster
# produces a SecretStore that authenticates as another cluster's identity
# against another cluster's vault; ESO reports an auth failure that names a
# principal the operator has never heard of.
#
# So the two objects carrying per-install values (the SecretStore and the
# workload-identity ServiceAccount) are RENDERED HERE from parameters, and only
# the two ExternalSecrets -- which carry none -- are applied from the checkout.
# The parameters come from provisioning's own result envelope
# (`esoClientId`), which is the whole handoff between the two phases.
#
# THE TENANT ID IS NOT A SECRET and is required by the ESO Azure provider under
# WorkloadIdentity. It is a parameter because it varies per directory, not
# because it is sensitive.
#
# ORDER RELATIVE TO THE SEEDER. This must run AFTER seedInstanceSecrets: ESO
# resolves every remoteRef in an ExternalSecret and fails the object as a WHOLE
# when any one is unresolvable, so wiring it at an empty vault produces two
# permanently-failing objects rather than a partial sync.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4490 memql#4474 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.wireExternalSecrets" \
    "Render and apply this instance's SecretStore and ExternalSecrets against its own vault and identity."

cap_spec_param_required "keyVaultName" "key vault the SecretStore reads from"
cap_spec_param_required "tenantId"     "Azure AD tenant of the vault and the identity (not a secret; required by the ESO Azure provider under WorkloadIdentity)"
cap_spec_param_required "esoClientId"  "client id of the user-assigned identity External Secrets authenticates as -- provisioning reports it as esoClientId"
cap_spec_param "namespace"        "namespace the SecretStore and ExternalSecrets live in (default memql)"
cap_spec_param "serviceAccount"   "workload-identity ServiceAccount name (default external-secrets-kv)"
cap_spec_param "repoRoot"         "MemQL checkout holding the ExternalSecret definitions (default: the checkout this script is in)"
cap_spec_param "dryRun"           "plan only; apply nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

KEY_VAULT_NAME="$(cap_param keyVaultName "")"
TENANT_ID="$(cap_param tenantId "")"
ESO_CLIENT_ID="$(cap_param esoClientId "")"
NAMESPACE="$(cap_param namespace "memql")"
SERVICE_ACCOUNT="$(cap_param serviceAccount "external-secrets-kv")"
REPO_ROOT="$(cap_param repoRoot "$(cd "${SCRIPT_DIR}/../.." && pwd)")"
DRY_RUN="$(cap_param dryRun "false")"

readonly EXTERNALSECRETS_FILE="deploy/external-secrets/externalsecret-memql.yaml"
readonly UUID_RE='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'

APPLIED=""

function check_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed or not on PATH"
    kubectl get crd secretstores.external-secrets.io -o name &>/dev/null \
        || cap_fail 4 "the External Secrets CRDs are not installed on this cluster -- run installClusterOperators first, or a SecretStore apply is rejected with a schema error that names no operator"
}

function validate_arguments() {
    [[ -n "$KEY_VAULT_NAME" ]] || cap_fail 2 "--keyVaultName is required"
    [[ -n "$TENANT_ID"      ]] || cap_fail 2 "--tenantId is required"
    [[ -n "$ESO_CLIENT_ID"  ]] || cap_fail 2 "--esoClientId is required -- provisioning reports it as esoClientId; without it the SecretStore has no identity to authenticate as"

    # Both are GUIDs. Getting one wrong produces an ESO auth failure naming a
    # principal that does not exist, minutes later, on an object that looks
    # correct -- so the shape is worth refusing here.
    [[ "$TENANT_ID"     =~ $UUID_RE ]] || cap_fail 2 "--tenantId ${TENANT_ID} is not a GUID"
    [[ "$ESO_CLIENT_ID" =~ $UUID_RE ]] || cap_fail 2 "--esoClientId ${ESO_CLIENT_ID} is not a GUID"

    [[ -f "${REPO_ROOT}/${EXTERNALSECRETS_FILE}" ]] \
        || cap_fail 2 "--repoRoot ${REPO_ROOT} does not look like a MemQL checkout: ${EXTERNALSECRETS_FILE} is missing"
}

# apply_stdin <label> -- apply a rendered document, or describe it under dry run.
function apply_stdin() {
    local label="$1" doc
    doc="$(cat)"
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would apply ${label}"
        return 0
    fi
    cap_step "applying ${label}"
    printf '%s\n' "$doc" | kubectl apply -f - >/dev/null \
        || cap_fail 5 "failed to apply ${label}"
    APPLIED="${APPLIED:+${APPLIED},}${label}"
    cap_changed
}

function wire_service_account() {
    # The label is what makes the Azure workload-identity webhook mutate the
    # pod; the annotation is what tells it WHICH identity. Either one alone
    # produces a pod that starts and cannot authenticate.
    apply_stdin "serviceaccount/${SERVICE_ACCOUNT}" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SERVICE_ACCOUNT}
  namespace: ${NAMESPACE}
  annotations:
    azure.workload.identity/client-id: "${ESO_CLIENT_ID}"
  labels:
    azure.workload.identity/use: "true"
EOF
}

function wire_secret_store() {
    apply_stdin "secretstore/keyvault" <<EOF
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: keyvault
  namespace: ${NAMESPACE}
spec:
  provider:
    azurekv:
      authType: WorkloadIdentity
      vaultUrl: https://${KEY_VAULT_NAME}.vault.azure.net
      tenantId: ${TENANT_ID}
      serviceAccountRef:
        name: ${SERVICE_ACCOUNT}
EOF
}

function wire_external_secrets() {
    # These two carry no per-install value, so they come from the checkout --
    # which also means a change to WHICH keys the mesh reads is a repository
    # change, reviewed, rather than a string in a shell script.
    #
    # They stay two objects on purpose: ESO fails a whole ExternalSecret when
    # any one remoteRef is unresolvable, so splitting them is what stops a
    # single missing key taking every credential down with it.
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would apply ${EXTERNALSECRETS_FILE}"
        return 0
    fi
    cap_step "applying ${EXTERNALSECRETS_FILE}"
    # The ServiceAccount in that file carries another install's client id, so
    # it is filtered out here and the one rendered above is authoritative.
    kubectl apply -n "$NAMESPACE" \
        -f <(awk '/^kind: ServiceAccount$/{skip=1} /^---$/{skip=0} !skip' "${REPO_ROOT}/${EXTERNALSECRETS_FILE}") >/dev/null \
        || cap_fail 5 "failed to apply the ExternalSecrets from ${EXTERNALSECRETS_FILE}"
    APPLIED="${APPLIED:+${APPLIED},}externalsecrets"
    cap_changed
}

function collect_result() {
    cap_result_set "namespace"      "$NAMESPACE"
    cap_result_set "keyVault"       "$KEY_VAULT_NAME"
    cap_result_set "serviceAccount" "$SERVICE_ACCOUNT"
    cap_result_set "applied"        "$APPLIED"
    cap_result_set "dryRun"         "$DRY_RUN"
    return 0
}

function main() {
    validate_arguments
    check_prerequisites

    cap_info "wiring External Secrets in ${NAMESPACE} to ${KEY_VAULT_NAME} as ${ESO_CLIENT_ID}"
    wire_service_account
    wire_secret_store
    wire_external_secrets

    collect_result
    cap_ok
}

main "$@"
