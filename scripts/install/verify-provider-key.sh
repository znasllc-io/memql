#!/usr/bin/env bash
#
# scripts/install/verify-provider-key.sh
# ======================================
#
# Capability: install.verifyProviderKey -- prove a cluster's AI-provider
# credential works, by asking the cluster.
#
# THERE IS NO KEY ANY MORE, AND THAT IS THE WHOLE SHAPE OF THIS SCRIPT
# (epic memql#5088). MemQL reaches both AI vendors by workload identity
# federation: the credential is a projected Kubernetes service-account token
# that exists only inside a pod, exchanged for a bearer that lives at most an
# hour. Nothing long-lived is at rest, so there is nothing an installer could
# hand this script to verify.
#
# The `--key-file` mode is therefore GONE rather than deprecated. Keeping it
# would have been worse than removing it: it verified a credential the cluster
# does not use, which is a green check on the wrong thing.
#
# What is left is the probe that asks the running pod what it is really using:
#
#   kubectl exec <deploy> -- memql provider-auth check --provider=<vendor>
#
# The pod holds the token, so the pod is the only thing that can answer. Run it
# against more than one node type: each pod projects its own token, so "it
# works on agent" is not "it works".
#
# THE CHECK REPORTS THE ENGINE'S OWN SIGN-IN AND MAKES NONE (design record
# docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
# A5). The engine in each pod signs in once at start, lists the vendor's models
# with the bearer, and records what happened; `provider-auth check` reads that
# record and waits up to its timeout for one to exist. It contacts no vendor.
# A vendor that accepts an identity token once would refuse whichever of two
# signers in a pod went second, which is why the check stopped being one.
#
# "VERIFIED" MEANS BOTH HALVES: the vendor accepted the sign-in AND its API
# took the bearer (the model list). The engine records every vendor, set up or
# not, so the check also has an answer for a pod that holds no federation ids.
#
# THE NAME IS OLDER THAN THE BEHAVIOUR. This file and its capability id still
# say "key"; both are referenced by the DSL action, the engine's capability
# allowlist, the install graph, the editor extension and its tests, so renaming
# them is a sweep of its own rather than a side effect of this one.
#
# EXIT CODES -- the distinction the installer branches on:
#
#   0  the vendor accepted the cluster's federated credential and its API
#      took the bearer
#   2  bad param (unknown provider, no --federation-deploy)
#   3  REFUSED: the check RAN and the credential is not usable. Either the
#      vendor did not accept it -- the provider or rule, the subject, the
#      audience, the service account: the federated analogue of a 401 on a
#      key, and what the operator re-does the console steps for -- or the pod
#      holds NO federation ids at all, so the ids did not reach it (the overlay
#      or Settings > Vendors, and whether the pods rolled). The message says
#      which.
#   4  prerequisite missing (kubectl absent)
#   5  the check could not be RUN (no such deployment, no cluster, exec
#      refused), or it ran and could not say yet: the engine has recorded
#      nothing, is waiting for its next identity token, got no answer from the
#      vendor, or nothing has used the bearer. Says nothing whatever about the
#      credential.
#
# The 3-versus-5 split is the reason this is not one exit code: "your mapping
# does not match" and "you are pointed at the wrong cluster" ask for opposite
# next actions, and `kubectl exec` collapses both into a non-zero exit unless
# they are told apart deliberately. They are told apart by whether the command
# RAN: kubectl exec returns the command's own status, and `provider-auth check`
# uses 1 for a refusal, 3 for "not set up in this pod" and 4 for "cannot say
# yet". So 1 and 3 are answers about the credential, 4 is an open question the
# caller asks again, and anything else is a failure to run.
#
# 0 USED TO COVER "NOT SET UP" AS WELL: the check exited 0 for a pod with no
# ids, and this script printed "Federation verified" for it.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-provider-key.sh --provider=anthropic --federation-deploy=agent
#   scripts/install/verify-provider-key.sh --provider=openai --federation-deploy=bff --namespace=memql
#   scripts/install/verify-provider-key.sh --print-spec
#
# Runbooks: docs/public/operate/auth/anthropic-federation.md
#           docs/public/operate/auth/openai-federation.md
#
# Refs: #3364 #3357 #2221 #4335 #5088

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.verifyProviderKey" \
    "Verify a cluster's AI-provider credential by running provider-auth check inside a pod: the cluster's account at a vendor is workload identity federation, so the pod that holds the projected token is the only thing that can answer. The check reports the sign-in the engine in that pod made at start."
cap_spec_param "provider"          "vendor to verify: anthropic | openai"
# NOT cap_spec_param_required, and deliberately so: an installer that supplied
# no federation ids has nothing to check and SKIPS this step rather than
# failing it -- installing spends no AI credit. Requiredness is enforced below,
# where a caller that did ask for a check gets exit 2 for a missing target.
cap_spec_param "federation-deploy" "the Deployment to exec provider-auth check in (e.g. agent). The credential lives inside the pod, so the check runs there"
cap_spec_param "namespace"         "Kubernetes namespace holding --federation-deploy (default memql)"
cap_spec_param "memql-binary"      "path to the memql binary inside the pod (default /app/memql)"
cap_spec_param "timeout"           "how long, in seconds, to wait for the engine's sign-in to be recorded (default 15)"

#=============================================================================
# THE FEDERATION PROBE -- ask the pod, because the credential is the pod's
#=============================================================================

# probe_federation <provider> <deployment> <namespace> <memql-binary> <timeout>
function probe_federation() {
    local provider="$1" deployment="$2" namespace="$3" binary="$4" timeout="$5"

    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed; cannot verify federation from outside the cluster"
    fi

    cap_step "kubectl exec -n ${namespace} deploy/${deployment} -- ${binary} provider-auth check --provider=${provider}"

    local out="" rc=0
    out="$(kubectl exec -n "$namespace" "deploy/${deployment}" -- \
              "$binary" provider-auth check --provider="$provider" --timeout="${timeout}s" 2>&1)" || rc=$?

    local note
    note="$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-600)"

    cap_result_set     provider    "$provider"
    cap_result_set     credential  "federation"
    cap_result_set     deployment  "${namespace}/${deployment}"
    cap_result_set     detail      "$note"

    case "$rc" in
        0)
            cap_result_set_raw valid true
            cap_info "Federation verified for ${provider} in ${namespace}/${deployment}."
            cap_ok
            ;;
        1)
            cap_result_set_raw valid false
            cap_error "The pod ran the check and ${provider} did not accept the federated credential."
            cap_fail 3 "federation was refused: ${note}"
            ;;
        3)
            # The check ran and the engine in the pod holds no account at this
            # vendor. This step runs only after an installer has written
            # federation ids, so they did not reach this pod.
            cap_result_set_raw valid false
            cap_error "The pod ran the check and holds no federation ids for ${provider}."
            cap_fail 3 "federation is not set up in ${namespace}/${deployment}: the pod holds no ${provider} ids, so the ones written for this cluster did not reach it. Check the overlay or Settings > Vendors, and that the pods rolled: ${note}"
            ;;
        4)
            # The check ran and could not say. Not a refusal: the engine has
            # recorded nothing yet, is waiting for its next identity token, or
            # nothing has used the bearer. The caller asks again.
            cap_result_set_raw valid false
            cap_fail 5 "the sign-in in ${namespace}/${deployment} is not verified yet; this says nothing about the credential, so run the check again: ${note}"
            ;;
        *)
            cap_result_set_raw valid false
            cap_fail 5 "could not run provider-auth check in ${namespace}/${deployment} (kubectl exit ${rc}); this says nothing about the credential: ${note}"
            ;;
    esac
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local provider federation_deploy namespace memql_binary timeout
    provider="$(cap_param provider)"
    federation_deploy="$(cap_param federation-deploy)"
    namespace="$(cap_param namespace memql)"
    memql_binary="$(cap_param memql-binary /app/memql)"
    timeout="$(cap_param timeout 15)"

    cap_require provider "$provider"
    case "$provider" in
        anthropic|openai) ;;
        *) cap_fail 2 "unknown provider: ${provider} (supported: anthropic, openai)" ;;
    esac

    # NAMED SEPARATELY FROM cap_require's generic message, because the thing a
    # caller most often gets wrong here is assuming there is a credential to
    # point at. There is not: the only way to verify is to ask a pod.
    if [[ -z "$federation_deploy" ]]; then
        cap_fail 2 "--federation-deploy is required: the credential is a projected token inside a pod, so the check has to run there. There is no key to verify from outside."
    fi

    probe_federation "$provider" "$federation_deploy" "$namespace" "$memql_binary" "$timeout"
}

main "$@"
