#!/usr/bin/env bash
#
# scripts/fleet/tenant-provision.sh
# =================================
#
# Capability: fleet.tenantProvision -- render one MemQL tenant's overlay and its
# ArgoCD Application from the tenant component's templates, and (unless
# dry-running) ask ArgoCD to adopt it.
#
# Backend for the `provisionTenant` fleet action (epic memql#3852, task
# memql#3853).
#
# WHAT THIS SCRIPT KNOWS, AND WHAT IT DELIBERATELY DOES NOT. It knows how to
# turn (tenant, domain, profile, database preset, HA) into a kustomize overlay
# and an Application. It knows nothing about subscriptions, tiers, trials, or
# money -- those live in the fleet DSL bundle, which resolves a tier to these
# parameters before calling. That is the capability-script contract's "no
# decisions inside" rule (docs/internal/design/capability-script-contract.md)
# and it is what makes this script equally usable for a paying tenant, a trial,
# and an operator standing up an instance by hand.
#
# IDEMPOTENT BY CONSTRUCTION. Rendering is a pure function of the parameters, so
# a re-run against unchanged inputs rewrites byte-identical files and reports
# `changed: false`. That matters because the caller is an automation with
# at-least-once delivery: a redelivered provisioning event must not produce a
# second tenant, and here it cannot produce anything at all.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed
#
# Refs: memql#3852 memql#3853 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${REPO_ROOT}/.." && pwd)"
TEMPLATE_DIR="${REPO_ROOT}/deploy/k8s/components/tenant/template"

cap_init "fleet.tenantProvision" "Render a MemQL tenant overlay + ArgoCD Application and adopt it."
cap_spec_param_required "tenant"     "tenant slug -- the namespace, the Application name and the subdomain label (a DNS label)"
cap_spec_param_required "domain"     "the tenant's fully-qualified domain, e.g. acme.memql.cloud"
cap_spec_param_required "profile"    "instance profile: solo | standard | dedicated"
cap_spec_param_required "dbPreset"   "database preset: entry | mid | top"
cap_spec_param "ha"                  "compose the HA add-on component (true|false; only meaningful with profile=solo)"
cap_spec_param "engineTag"           "engine image tag every node of this tenant runs"
cap_spec_param "dbImageTag"          "database operand image tag (must begin with the PostgreSQL major)"
cap_spec_param "backupDestination"   "ObjectStore destinationPath for this tenant's WAL + base backups"
cap_spec_param_required "maxLlmCalls" "cumulative LLM-call ceiling this tenant boots with (the tier's structural backstop)"
cap_spec_param_required "maxLlmCostUsd" "cumulative estimated-USD ceiling this tenant boots with"
cap_spec_param "repoUrl"             "git repository ArgoCD reconciles the tenant from"
cap_spec_param "targetRevision"      "git revision ArgoCD reconciles the tenant at"
cap_spec_param "outputRoot"          "repository root to render into (defaults to this checkout)"
cap_spec_param "dryRun"              "render only; do not contact the cluster"

# validate_tenant_slug -- the slug is a Kubernetes namespace, an ArgoCD
# Application name AND a DNS label all at once, so it is validated as the
# strictest of the three. Refusing here rather than at apply time matters: a
# slug that is a valid namespace but an invalid DNS label produces a tenant that
# reconciles green and is unreachable, with nothing in the cluster to say why.
function validate_tenant_slug() {
    local slug="$1"
    if [[ ! "$slug" =~ ^[a-z]([-a-z0-9]*[a-z0-9])?$ ]]; then
        cap_fail 2 "tenant '${slug}' is not a DNS label (lowercase alphanumeric and hyphens, starting with a letter)"
    fi
    if (( ${#slug} > 40 )); then
        cap_fail 2 "tenant '${slug}' is longer than 40 characters; it prefixes generated resource names that have their own limits"
    fi
    # Names the fleet must never hand out, because each one already means
    # something on the cluster this tenant would land in.
    case "$slug" in
        memql|memql-prod|memql-staging|argocd|kube-system|kube-public|kube-node-lease|default|cert-manager|cnpg-system)
            cap_fail 2 "tenant '${slug}' is a reserved namespace on this cluster" ;;
    esac
}

# validate_ceiling -- a spend ceiling must be a POSITIVE number.
#
# Zero is refused, and that refusal is the whole point of the parameter being
# required. The guard reads 0 as UNLIMITED (docs/public/ai/llm-cost-control.md),
# so a caller that computed a ceiling of zero -- a tier row with an unset field,
# an arithmetic slip, an empty string coerced -- would render a tenant with NO
# ceiling while every line of the configuration looked deliberate. Failing here
# costs a provisioning run; the alternative costs a bill.
function validate_ceiling() {
    local name="$1" value="$2"
    if [[ ! "$value" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
        cap_fail 2 "${name} must be a number (got '${value}')"
    fi
    # A bare numeric comparison, since the value may carry a decimal point.
    if ! awk -v v="$value" 'BEGIN { exit !(v > 0) }'; then
        cap_fail 2 "${name}=${value} is not positive; the LLM guard reads 0 as UNLIMITED, so this would provision a tenant with no spend ceiling at all"
    fi
}

function validate_enum() {
    local name="$1" value="$2" allowed="$3"
    case " ${allowed} " in
        *" ${value} "*) return 0 ;;
    esac
    cap_fail 2 "${name} must be one of: ${allowed} (got '${value}')"
}

# render_file <src> <dest> -- substitute the placeholders and write, reporting
# whether anything actually changed. Substitution is done with bash parameter
# expansion rather than sed so a value containing a slash or an ampersand (a
# backup destination URL does both) cannot corrupt the output.
function render_file() {
    local src="$1" dest="$2" content
    content="$(cat "$src")"
    content="${content//__TENANT__/$TENANT}"
    content="${content//__DOMAIN__/$DOMAIN}"
    content="${content//__PROFILE__/$PROFILE}"
    content="${content//__DB_PRESET__/$DB_PRESET}"
    content="${content//__ENGINE_TAG__/$ENGINE_TAG}"
    content="${content//__DB_IMAGE_TAG__/$DB_IMAGE_TAG}"
    content="${content//__BACKUP_DESTINATION__/$BACKUP_DESTINATION}"
    content="${content//__MAX_LLM_CALLS__/$MAX_LLM_CALLS}"
    content="${content//__MAX_LLM_COST_USD__/$MAX_LLM_COST_USD}"
    content="${content//__REPO_URL__/$REPO_URL}"
    content="${content//__TARGET_REVISION__/$TARGET_REVISION}"
    content="${content//__HA_COMPONENT_LINE__/$HA_LINE}"

    if [[ -f "$dest" ]] && [[ "$(cat "$dest")" == "$content" ]]; then
        return 1
    fi
    printf '%s\n' "$content" > "$dest"
    return 0
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    TENANT="$(cap_param tenant "")"
    DOMAIN="$(cap_param domain "")"
    PROFILE="$(cap_param profile "")"
    DB_PRESET="$(cap_param dbPreset "")"
    local ha dry out_root
    ha="$(cap_bool_str ha false)"
    ENGINE_TAG="$(cap_param engineTag "latest")"
    DB_IMAGE_TAG="$(cap_param dbImageTag "16")"
    BACKUP_DESTINATION="$(cap_param backupDestination "")"
    MAX_LLM_CALLS="$(cap_param maxLlmCalls "")"
    MAX_LLM_COST_USD="$(cap_param maxLlmCostUsd "")"
    REPO_URL="$(cap_param repoUrl "https://github.com/znasllc-io/memql.git")"
    TARGET_REVISION="$(cap_param targetRevision "main")"
    out_root="$(cap_param outputRoot "$REPO_ROOT")"
    dry="$(cap_bool_str dryRun true)"

    cap_require tenant   "$TENANT"
    cap_require domain   "$DOMAIN"
    cap_require profile  "$PROFILE"
    cap_require dbPreset "$DB_PRESET"
    cap_require maxLlmCalls   "$MAX_LLM_CALLS"
    cap_require maxLlmCostUsd "$MAX_LLM_COST_USD"

    validate_tenant_slug "$TENANT"
    validate_enum profile  "$PROFILE"   "solo standard dedicated"
    validate_enum dbPreset "$DB_PRESET" "entry mid top"
    validate_ceiling maxLlmCalls   "$MAX_LLM_CALLS"
    validate_ceiling maxLlmCostUsd "$MAX_LLM_COST_USD"

    # A tenant whose backups go nowhere looks perfectly healthy. The cnpg-db
    # component ships the PATCH-ME-IN-THE-OVERLAY placeholder precisely so this
    # cannot be forgotten silently, and defaulting it here would reintroduce the
    # silence -- so the default is derived from the tenant name, which is at
    # least per-tenant and therefore never another tenant's backups.
    if [[ -z "$BACKUP_DESTINATION" ]]; then
        BACKUP_DESTINATION="azure://memql-tenant-backups/${TENANT}/"
        cap_info "no --backupDestination given; defaulting to ${BACKUP_DESTINATION}"
    fi

    # The HA add-on is only ever composed on top of `solo`. From Graph up, HA is
    # in the price and the profile preset already carries it; composing it there
    # would be a no-op that implies the tier's replication were optional. This
    # is a MECHANICAL branch (does this line appear in the rendered file), not a
    # decision -- whether the tenant HAS high availability was decided by the
    # caller from the tier's haIncluded and the subscription's haAddOn.
    HA_LINE=""
    if [[ "$ha" == "true" ]]; then
        if [[ "$PROFILE" != "solo" ]]; then
            cap_info "profile ${PROFILE} already includes HA; the add-on component is not composed"
        else
            HA_LINE="  - ../../components/tenant/optional/ha"
        fi
    fi

    if [[ ! -d "$TEMPLATE_DIR" ]]; then
        cap_fail 4 "tenant template directory is missing: ${TEMPLATE_DIR}"
    fi

    local overlay_dir="${out_root}/deploy/k8s/tenants/${TENANT}"
    local app_file="${out_root}/deploy/argocd/tenants/${TENANT}.yaml"
    mkdir -p "$overlay_dir" "$(dirname "$app_file")"

    local changed=0 f
    for f in kustomization.yaml domain.yaml domain-envfrom.yaml allowance.yaml allowance-envfrom.yaml; do
        if render_file "${TEMPLATE_DIR}/${f}" "${overlay_dir}/${f}"; then
            changed=1
            cap_info "rendered ${overlay_dir}/${f}"
        fi
    done
    if render_file "${TEMPLATE_DIR}/application.yaml" "$app_file"; then
        changed=1
        cap_info "rendered ${app_file}"
    fi

    cap_result_set tenant       "$TENANT"
    cap_result_set domain       "$DOMAIN"
    cap_result_set profile      "$PROFILE"
    cap_result_set dbPreset     "$DB_PRESET"
    cap_result_set_raw maxLlmCalls   "$MAX_LLM_CALLS"
    cap_result_set_raw maxLlmCostUsd "$MAX_LLM_COST_USD"
    cap_result_set overlayPath  "deploy/k8s/tenants/${TENANT}"
    cap_result_set applicationPath "deploy/argocd/tenants/${TENANT}.yaml"
    cap_result_set_raw haComposed "$([[ -n "$HA_LINE" ]] && echo true || echo false)"
    [[ "$changed" == "1" ]] && cap_changed

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] rendered only; the cluster was not contacted"
        cap_result_set_raw dryRun true
        cap_result_set_raw applied false
        cap_ok
    fi

    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed on the runner"
    fi

    # Applying the Application is the whole of "provisioning" from this script's
    # point of view. ArgoCD does the rest, and the fleet learns the outcome from
    # the instance row's status rather than from this exit code -- a tenant is
    # `running` when its pods are, which is minutes after this returns.
    cap_info "Adopting tenant ${TENANT} into ArgoCD..."
    kubectl apply -f "$app_file" >&2 || cap_fail 5 "failed to apply the ArgoCD Application for ${TENANT}"
    cap_changed
    cap_result_set_raw dryRun false
    cap_result_set_raw applied true
    cap_ok
}

main "$@"
