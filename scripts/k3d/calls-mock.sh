#!/usr/bin/env bash
# Configure only the local, server-owned calls simulator. The existing
# memql-secrets envFrom path survives ArgoCD reconciliation and preserves every
# unrelated operator key. This never changes a Deployment, site policy or real
# provider credential, and never renews the observation without an operator run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/capability.sh"
source "${SCRIPT_DIR}/../lib/argocd-preservation.sh"

cap_init "k3d.callsMock" "Preview or install a finite calls simulation envelope in an existing local k3d cluster."
cap_spec_param "cluster" "Existing local k3d cluster name (default: memql)"
cap_spec_param "namespace" "Existing engine namespace (default: memql)"
cap_spec_param "valid-days" "Observation validity, 1 to 30 days, simulation only (default: 30)"
cap_spec_param "family" "Only change the family simulator: mock or disabled; preserves the existing commerce envelope"
cap_spec_param "apply" "Apply the previewed mock values to memql-secrets (flag); default is read-only"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local cluster namespace days family current listing mode patch values applied=false
    cluster="$(cap_param cluster memql)"
    namespace="$(cap_param namespace memql)"
    days="$(cap_param valid-days 30)"
    family="$(cap_param family '')"
    [[ -z "$family" || "$family" == mock || "$family" == disabled ]] \
        || cap_fail 2 "family must be mock or disabled"
    [[ -z "$family" || -z "$(cap_param valid-days '')" ]] \
        || cap_fail 2 "family changes preserve the envelope; do not combine family with valid-days"
    [[ "$cluster" =~ ^[a-z0-9][a-z0-9-]*$ && "$namespace" =~ ^[a-z0-9][a-z0-9-]*$ ]] \
        || cap_fail 2 "cluster and namespace must be local Kubernetes names"
    [[ "$days" =~ ^([1-9]|[12][0-9]|30)$ ]] || cap_fail 2 "valid-days must be between 1 and 30"
    if cap_bool apply; then applied=true; fi
    if ! command -v kubectl >/dev/null || ! command -v k3d >/dev/null || ! command -v python3 >/dev/null; then
        cap_fail 4 "kubectl, k3d and python3 are required"
    fi

    # Require the selected local context, then pin every subsequent request to
    # that same context. Another process switching kubectl cannot redirect the
    # patch to a cloud installation after this check.
    current="$(command kubectl config current-context)" || cap_fail 4 "cannot read current Kubernetes context"
    [[ "$current" == "k3d-${cluster}" ]] || cap_fail 4 "select the existing k3d-${cluster} context first; other contexts are refused"
    listing="$(k3d cluster list -o json)" || cap_fail 4 "cannot inspect existing k3d clusters"
    python3 -c 'import json,sys; clusters=json.load(sys.stdin); sys.exit(0 if any(c.get("name")==sys.argv[1] and c.get("serversRunning",0)>0 for c in clusters) else 1)' "$cluster" <<<"$listing" \
        || cap_fail 4 "the named local k3d cluster is not running"
    KUBE_CONTEXT="$current"
    function kubectl() { command kubectl --context="$KUBE_CONTEXT" "$@"; }

    # Snapshot preservation metadata and its optimistic write version before
    # reading mode. The merge touches only listed simulation values; it never
    # fetches or reconstructs unrelated Secret data.
    argocd_preservation_load "$namespace" memql-secrets \
        || cap_fail 4 "cannot read safely preserved memql-secrets metadata"
    mode="$(kubectl get secret memql-secrets --namespace="$namespace" -o 'jsonpath={.data.MEMQL_CALLS_COMMERCE_MODE}')" \
        || cap_fail 4 "cannot read existing commerce mode"
    mode="$(python3 -c 'import base64,sys; print(base64.b64decode(sys.argv[1],validate=True).decode())' "$mode")" \
        || cap_fail 4 "existing commerce mode is invalid"
    [[ -z "$mode" || "$mode" == disabled || "$mode" == mock ]] \
        || cap_fail 4 "refusing to replace an existing provider mode with mock"
    [[ "$family" != mock || "$mode" == mock ]] \
        || cap_fail 4 "family simulation requires an existing mock commerce envelope"

    if [[ -n "$family" ]]; then
        # This independent switch must never renew a stale observation, reset
        # current capacity/budgets or install real consent-provider credentials.
        values="$(python3 - "$family" <<'PY'
import json, sys
print(json.dumps({"MEMQL_CALLS_FAMILY_MODE": sys.argv[1]}))
PY
)" || cap_fail 5 "could not construct the family simulation switch"
    else
    # These are deliberately simulated cents. They are neither measured cash
    # nor a claim of profit/hardware capacity. Limits include pre-existing
    # account and room commitments through the server's global durable gate.
    values="$(python3 - "$days" <<'PY'
import datetime, json, sys
values = {
    "MEMQL_CALLS_COMMERCE_MODE": "mock",
    "MEMQL_CALLS_COMMERCE_VALID_UNTIL": (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=int(sys.argv[1]))).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "MEMQL_CALLS_COMMERCE_CASH_CENTS": "100000",
    "MEMQL_CALLS_COMMERCE_PROTECTED_CENTS": "10000",
    "MEMQL_CALLS_COMMERCE_FIXED_CENTS": "10000",
    "MEMQL_CALLS_COMMERCE_PROVIDER_CREDIT_CENTS": "10000",
    "MEMQL_CALLS_COMMERCE_VERIFICATION_UNIT_CENTS": "100",
    "MEMQL_CALLS_COMMERCE_VERIFICATION_BUDGET_CENTS": "10000",
    "MEMQL_CALLS_COMMERCE_CALL_BUDGET_CENTS": "50000",
    "MEMQL_CALLS_COMMERCE_FULFILLMENT_RESERVE_BPS": "5000",
    "MEMQL_CALLS_COMMERCE_MAX_ACCOUNTS": "200",
    "MEMQL_CALLS_COMMERCE_MAX_PENDING_VERIFICATIONS": "10",
    "MEMQL_CALLS_COMMERCE_MAX_VERIFICATION_ATTEMPTS": "3",
    "MEMQL_CALLS_COMMERCE_MAX_OPEN_CALLS": "5",
    "MEMQL_CALLS_COMMERCE_MAX_PARTICIPANTS": "40",
}
print(json.dumps(values))
PY
)" || cap_fail 5 "could not construct the finite mock envelope"
    fi

    if [[ "$applied" == true ]]; then
        patch="$(python3 - "$ARGO_PRESERVATION_RV" "$ARGO_PRESERVATION_SYNC" "$ARGO_PRESERVATION_COMPARE" "$values" <<'PY'
import json, sys
print(json.dumps({"metadata": {"resourceVersion": sys.argv[1], "annotations": {
    "argocd.argoproj.io/sync-options": sys.argv[2],
    "argocd.argoproj.io/compare-options": sys.argv[3]
}}, "stringData": json.loads(sys.argv[4])}))
PY
)" || cap_fail 5 "could not construct the guarded configuration patch"
        printf '%s' "$patch" | argocd_preservation_write_secret "$namespace" memql-secrets >&2 \
            || cap_fail 5 "memql-secrets changed concurrently or could not be written; inspect before retrying"
        cap_changed
        cap_info "Mock configuration saved. Existing pods retain their environment until the normal make dev rollout or a bff restart. No site policy changed."
    fi
    cap_result_set context "$current"
    cap_result_set namespace "$namespace"
    cap_result_set_raw applied "$applied"
    cap_result_set_raw values "$values"
    # No positional result argument: command flags have already been parsed.
    # shellcheck disable=SC2119
    cap_ok
}

main "$@"
