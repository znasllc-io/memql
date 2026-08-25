#!/usr/bin/env bash
#
# scripts/deploy/register-gitops-repo.sh
# ======================================
#
# Capability: deploy.registerGitOpsRepo -- make an instance's own git
# repository reachable and PERMITTED for ArgoCD on that instance's cluster.
#
# Backend for the `registerGitOpsRepo` deployment action (epic memql#4490,
# memql#4472). Step 11 of the eleven, and the last thing that must be true
# before `argoSync` means anything.
#
# WHAT `installInstance` USED TO ASSUME. It called argoSync(app: "memql") as
# though the Application were already registered. Registering it needs three
# cluster writes and one credential, none of which were modelled -- and each
# fails in a way that reads like a different problem:
#
#   - The AppProject must exist AND list the repo in sourceRepos, or the
#     Application is refused as "repo not permitted in project". That stops
#     reconciliation while looking like a permissions note.
#   - The repository Secret's `url` must match the Application's `repoURL`
#     BYTE FOR BYTE. A trailing slash, or .git present on one side and absent
#     on the other, produces a ComparisonError that reads like a manifest
#     problem. This script normalises both ends from one input, which is the
#     only way to make them agree by construction.
#   - The credential TRANSPORT is an ORG-POLICY question, not a preference.
#     Deploy keys were disabled org-wide on the install this came from, so the
#     ssh path was simply unavailable and the repoURL had to become https with
#     a fine-grained token. A lifecycle step must therefore treat the transport
#     as an INPUT; a script that hardcodes one is a script that cannot run at
#     half the organisations that will run it.
#
# THE APP-OF-APPS IS REFUSED HERE, DELIBERATELY. deploy/argocd/apps/root.yaml
# carries `automated: {prune: true, selfHeal: true}` and an include glob
# containing memql.yaml -- the ENGINE's own Application, named `memql`,
# pointing at the ENGINE's repo. Applying root on an instance cluster would
# continuously revert the instance's Application to the engine's. That is
# memql#4463's failure with the polarity reversed, and it is armed by default,
# so this script checks for it and refuses rather than registering a repo into
# a cluster that will overwrite the result.
#
# THE CREDENTIAL NEVER TRAVELS ON ARGV. --tokenFile and --sshKeyFile take a
# PATH; the value is read from the file and handed to kubectl through another
# file. argv is world-readable on a shared runner.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4490 memql#4472 memql#4463 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.registerGitOpsRepo" \
    "Register an instance's git repository with ArgoCD: credential Secret plus the AppProject sourceRepos entry."

cap_spec_param_required "repoUrl"   "the instance repository, exactly as the Application's repoURL names it"
cap_spec_param_required "transport" "https or ssh. An ORG-POLICY question, not a preference: deploy keys are disabled org-wide at some organisations, which makes ssh unavailable rather than unattractive"
cap_spec_param "project"      "ArgoCD AppProject that must permit the repo (default memql)"
cap_spec_param "argoNamespace" "namespace ArgoCD runs in (default argocd)"
cap_spec_param "username"     "git username for https (default: a placeholder accepted by GitHub token auth)"
cap_spec_param "tokenFile"    "PATH to a file holding the https token. The value is never passed on argv"
cap_spec_param "sshKeyFile"   "PATH to a file holding the ssh private key. The value is never passed on argv"
cap_spec_param "secretName"   "name of the repository Secret (default: derived from the repo)"
cap_spec_param "dryRun"       "plan only; write nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

REPO_URL="$(cap_param repoUrl "")"
TRANSPORT="$(cap_param transport "")"
PROJECT="$(cap_param project "memql")"
ARGO_NS="$(cap_param argoNamespace "argocd")"
GIT_USERNAME="$(cap_param username "git")"
TOKEN_FILE="$(cap_param tokenFile "")"
SSH_KEY_FILE="$(cap_param sshKeyFile "")"
SECRET_NAME="$(cap_param secretName "")"
DRY_RUN="$(cap_param dryRun "false")"

WORK=""
WROTE=""

function make_workdir() {
    WORK="$(mktemp -d)"; chmod 700 "$WORK"
    trap 'if [[ -n "$WORK" && -d "$WORK" ]]; then command -v shred >/dev/null && find "$WORK" -type f -exec shred -u {} + 2>/dev/null; rm -rf "$WORK"; fi' EXIT
}

function check_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed or not on PATH"
    kubectl get namespace "$ARGO_NS" -o name &>/dev/null \
        || cap_fail 4 "namespace ${ARGO_NS} does not exist -- ArgoCD is not installed on this cluster, and argoSync against it would sync nothing"
    kubectl get crd appprojects.argoproj.io -o name &>/dev/null \
        || cap_fail 4 "the ArgoCD CRDs are not installed on this cluster"
}

# The engine's app-of-apps on an instance cluster is armed to revert this work.
function refuse_engine_app_of_apps() {
    local root
    root="$(kubectl get application root -n "$ARGO_NS" \
        -o jsonpath='{.spec.source.repoURL}' 2>/dev/null || true)"
    [[ -n "$root" ]] || return 0
    if [[ "$root" == "$REPO_URL" ]]; then
        cap_info "an app-of-apps Application named root is present and points at THIS repo; that is the instance's own, not the engine's"
        return 0
    fi
    cap_fail 3 "the ArgoCD app-of-apps Application 'root' on this cluster points at ${root}, not at ${REPO_URL}. If that is the ENGINE's root.yaml it carries prune + selfHeal and an include glob containing the engine's own Application named 'memql', so it will continuously revert this instance's Application to the engine's. Delete it before registering this repo; the engine's app-of-apps is not applicable to an instance cluster."
}

function validate_arguments() {
    [[ -n "$REPO_URL" ]] || cap_fail 2 "--repoUrl is required"
    case "$TRANSPORT" in
        https)
            [[ -n "$TOKEN_FILE" ]] || cap_fail 2 "--transport=https needs --tokenFile=<path>"
            [[ -f "$TOKEN_FILE" ]] || cap_fail 2 "--tokenFile ${TOKEN_FILE} does not exist"
            [[ "$REPO_URL" == https://* ]] \
                || cap_fail 2 "--transport=https but --repoUrl is ${REPO_URL}. The Secret's url and the Application's repoURL must match byte for byte, so the transport and the URL cannot disagree"
            ;;
        ssh)
            [[ -n "$SSH_KEY_FILE" ]] || cap_fail 2 "--transport=ssh needs --sshKeyFile=<path>"
            [[ -f "$SSH_KEY_FILE" ]] || cap_fail 2 "--sshKeyFile ${SSH_KEY_FILE} does not exist"
            [[ "$REPO_URL" == git@* || "$REPO_URL" == ssh://* ]] \
                || cap_fail 2 "--transport=ssh but --repoUrl is ${REPO_URL}"
            ;;
        "") cap_fail 2 "--transport is required (https or ssh)" ;;
        *)  cap_fail 2 "--transport ${TRANSPORT} is not one of: https, ssh" ;;
    esac

    # A trailing slash is the classic byte-for-byte mismatch, and the resulting
    # ComparisonError names a manifest rather than a URL.
    [[ "$REPO_URL" != */ ]] \
        || cap_fail 2 "--repoUrl ends in a slash. ArgoCD matches a repository Secret's url against the Application's repoURL EXACTLY, so a trailing slash produces a ComparisonError that reads like a manifest problem"

    if [[ -z "$SECRET_NAME" ]]; then
        # Derived, so the same repo always lands on the same Secret and a
        # re-run converges rather than accumulating.
        SECRET_NAME="repo-$(printf '%s' "$REPO_URL" | tr -c 'a-zA-Z0-9' '-' | tr -s '-' | sed 's/^-//;s/-$//' | tr '[:upper:]' '[:lower:]' | cut -c1-53)"
    fi
}

function ensure_repo_secret() {
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would write repository Secret ${SECRET_NAME} in ${ARGO_NS} for ${REPO_URL} over ${TRANSPORT}"
        return 0
    fi
    cap_step "writing repository Secret ${SECRET_NAME} for ${REPO_URL}"

    local -a from=(--from-literal=type=git "--from-literal=url=${REPO_URL}")
    if [[ "$TRANSPORT" == "https" ]]; then
        cp "$TOKEN_FILE" "${WORK}/password"; chmod 600 "${WORK}/password"
        # Trailing whitespace in a pasted token is invisible and authenticates
        # as nothing.
        printf '%s' "$(tr -d '\r\n' < "${WORK}/password")" > "${WORK}/password.clean"
        from+=("--from-literal=username=${GIT_USERNAME}" "--from-file=password=${WORK}/password.clean")
    else
        cp "$SSH_KEY_FILE" "${WORK}/sshPrivateKey"; chmod 600 "${WORK}/sshPrivateKey"
        from+=("--from-file=sshPrivateKey=${WORK}/sshPrivateKey")
    fi

    # create --dry-run=client | apply is the converging form: it writes the
    # same Secret on a re-run rather than failing AlreadyExists.
    kubectl create secret generic "$SECRET_NAME" -n "$ARGO_NS" "${from[@]}" \
        --dry-run=client -o yaml \
        | kubectl label --local -f - "argocd.argoproj.io/secret-type=repository" -o yaml \
        | kubectl apply -f - >/dev/null \
        || cap_fail 5 "failed to write repository Secret ${SECRET_NAME}"
    WROTE="${WROTE:+${WROTE},}secret/${SECRET_NAME}"
    cap_changed
}

function ensure_project_source_repo() {
    kubectl get appproject "$PROJECT" -n "$ARGO_NS" -o name &>/dev/null \
        || cap_fail 4 "AppProject ${PROJECT} does not exist in ${ARGO_NS}. An Application in a project that does not exist is refused, and the message names the project rather than saying it is absent."

    local existing
    existing="$(kubectl get appproject "$PROJECT" -n "$ARGO_NS" \
        -o jsonpath='{.spec.sourceRepos[*]}' 2>/dev/null || true)"
    case " $existing " in
        *" $REPO_URL "*|*" * "*)
            cap_info "AppProject ${PROJECT} already permits ${REPO_URL}"
            return 0 ;;
    esac

    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would add ${REPO_URL} to AppProject ${PROJECT} sourceRepos"
        return 0
    fi
    cap_step "adding ${REPO_URL} to AppProject ${PROJECT} sourceRepos"
    # A JSON patch `add` to `/-`, not a replace of the list: an instance project
    # may legitimately permit more than one repo, and replacing the list would
    # silently revoke the others.
    kubectl patch appproject "$PROJECT" -n "$ARGO_NS" --type=json \
        -p "[{\"op\":\"add\",\"path\":\"/spec/sourceRepos/-\",\"value\":\"${REPO_URL}\"}]" >/dev/null \
        || cap_fail 5 "failed to add ${REPO_URL} to AppProject ${PROJECT}"
    WROTE="${WROTE:+${WROTE},}appproject/${PROJECT}"
    cap_changed
}

function collect_result() {
    cap_result_set "repoUrl"    "$REPO_URL"
    cap_result_set "transport"  "$TRANSPORT"
    cap_result_set "project"    "$PROJECT"
    cap_result_set "secretName" "$SECRET_NAME"
    cap_result_set "wrote"      "$WROTE"
    cap_result_set "dryRun"     "$DRY_RUN"
    return 0
}

function main() {
    validate_arguments
    check_prerequisites
    make_workdir
    refuse_engine_app_of_apps

    ensure_repo_secret
    ensure_project_source_repo

    collect_result
    cap_ok
}

main "$@"
