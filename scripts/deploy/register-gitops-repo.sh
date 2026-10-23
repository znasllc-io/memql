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
#     half the organizations that will run it.
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
cap_spec_param_required "transport" "https, ssh, or github-app. An ORG-POLICY question, not a preference: deploy keys are disabled org-wide at some organizations, which makes ssh unavailable rather than unattractive"
cap_spec_param "project"      "ArgoCD AppProject that must permit the repo (default memql)"
cap_spec_param "argoNamespace" "namespace ArgoCD runs in (default argocd)"
cap_spec_param "username"     "git username for https (default: a placeholder accepted by GitHub token auth)"
cap_spec_param "tokenFile"    "PATH to a file holding the https token. The value is never passed on argv"
cap_spec_param "sshKeyFile"   "PATH to a file holding the ssh private key. The value is never passed on argv"
cap_spec_param "githubAppId" "positive decimal GitHub App ID (github-app only)"
cap_spec_param "githubAppInstallationId" "positive decimal installation ID (github-app only)"
cap_spec_param "githubAppPrivateKeyFile" "PATH to the GitHub App RSA private key; never passed as a value on argv"
cap_spec_param "githubAppEnterpriseBaseUrl" "optional HTTPS GitHub Enterprise API base URL matching the repository host"
cap_spec_param "secretName"   "name of the repository Secret (default: derived from the repo)"
cap_spec_param "dryRun"       "plan only; write nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

REPO_URL="$(cap_param repoUrl "")"
TRANSPORT="$(cap_param transport "")"
PROJECT="$(cap_param project "memql")"
ARGO_NS="$(cap_param argoNamespace "argocd")"
GIT_USERNAME="$(cap_param username "")"
TOKEN_FILE="$(cap_param tokenFile "")"
SSH_KEY_FILE="$(cap_param sshKeyFile "")"
SECRET_NAME="$(cap_param secretName "")"
GITHUB_APP_ID="$(cap_param githubAppId "")"
GITHUB_INSTALLATION_ID="$(cap_param githubAppInstallationId "")"
GITHUB_KEY_FILE="$(cap_param githubAppPrivateKeyFile "")"
GITHUB_ENTERPRISE_URL="$(cap_param githubAppEnterpriseBaseUrl "")"
DRY_RUN="$(cap_bool_str dryRun false)"

WORK=""
WROTE=""

function make_workdir() {
    umask 077
    WORK="$(mktemp -d)"; chmod 700 "$WORK"
    trap '_cap_on_exit; cleanup_workdir' EXIT
}

function cleanup_workdir() {
    if [[ -n "$WORK" && -d "$WORK" ]]; then
        if command -v shred >/dev/null; then find "$WORK" -type f -exec shred -u {} + 2>/dev/null || true; fi
        rm -rf "$WORK"
    fi
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
    [[ "$REPO_URL" != *[[:space:]\"\\]* ]] || cap_fail 2 "--repoUrl contains unsupported whitespace or quoting"
    if [[ "$TRANSPORT" != "github-app" && -n "${GITHUB_APP_ID}${GITHUB_INSTALLATION_ID}${GITHUB_KEY_FILE}${GITHUB_ENTERPRISE_URL}" ]]; then
        cap_fail 2 "GitHub App parameters require --transport=github-app"
    fi
    case "$TRANSPORT" in
        https)
            [[ -z "$SSH_KEY_FILE" ]] || cap_fail 2 "https and SSH credentials cannot be mixed"
            GIT_USERNAME="${GIT_USERNAME:-git}"
            [[ -n "$TOKEN_FILE" ]] || cap_fail 2 "--transport=https needs --tokenFile=<path>"
            [[ -f "$TOKEN_FILE" ]] || cap_fail 2 "--tokenFile ${TOKEN_FILE} does not exist"
            [[ "$REPO_URL" == https://* ]] \
                || cap_fail 2 "--transport=https but --repoUrl is ${REPO_URL}. The Secret's url and the Application's repoURL must match byte for byte, so the transport and the URL cannot disagree"
            ;;
        ssh)
            [[ -z "${TOKEN_FILE}${GIT_USERNAME}" ]] || cap_fail 2 "SSH and HTTPS credentials cannot be mixed"
            [[ -n "$SSH_KEY_FILE" ]] || cap_fail 2 "--transport=ssh needs --sshKeyFile=<path>"
            [[ -f "$SSH_KEY_FILE" ]] || cap_fail 2 "--sshKeyFile ${SSH_KEY_FILE} does not exist"
            [[ "$REPO_URL" == git@* || "$REPO_URL" == ssh://* ]] \
                || cap_fail 2 "--transport=ssh but --repoUrl is ${REPO_URL}"
            ;;
        github-app)
            [[ -z "${TOKEN_FILE}${SSH_KEY_FILE}${GIT_USERNAME}" ]] || cap_fail 2 "GitHub App, token and SSH credentials cannot be mixed"
            validate_github_app
            ;;
        "") cap_fail 2 "--transport is required (https, ssh or github-app)" ;;
        *)  cap_fail 2 "--transport is not one of: https, ssh, github-app" ;;
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
    [[ "$SECRET_NAME" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && ${#SECRET_NAME} -le 253 ]] || cap_fail 2 "--secretName must be a Kubernetes Secret name"
}

function validate_github_app() {
    command -v python3 >/dev/null || cap_fail 4 "python3 is required to validate GitHub App connection values"
    command -v openssl >/dev/null || cap_fail 4 "openssl is required to validate the GitHub App private key"
    [[ -f "$GITHUB_KEY_FILE" && -r "$GITHUB_KEY_FILE" && -s "$GITHUB_KEY_FILE" ]] || cap_fail 2 "--githubAppPrivateKeyFile must name a readable, nonempty file"
    python3 - "$REPO_URL" "$GITHUB_ENTERPRISE_URL" "$GITHUB_APP_ID" "$GITHUB_INSTALLATION_ID" <<'PYVALIDATE' || cap_fail 2 "GitHub App requires positive decimal IDs and an HTTPS repository with a matching GitHub API host"
import re, sys
from urllib.parse import urlsplit
repo, enterprise, app, installation = sys.argv[1:]
for value in (app, installation):
    if not re.fullmatch(r"[1-9][0-9]{0,18}", value) or int(value) > 9223372036854775807:
        sys.exit(1)
def url(value):
    try: parsed = urlsplit(value)
    except ValueError: sys.exit(1)
    if (parsed.scheme != "https" or not parsed.hostname or parsed.netloc != parsed.hostname
        or parsed.query or parsed.fragment or re.search(r"[%\\\s]", value)
        or not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", parsed.hostname)):
        sys.exit(1)
    return parsed
r = url(repo)
if not re.fullmatch(r"/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", r.path) or any(p in (".", "..") for p in r.path.split("/")[1:]):
    sys.exit(1)
if enterprise:
    e = url(enterprise)
    if e.hostname != r.hostname or e.path not in ("/api/v3", "/api/v3/") or r.hostname == "github.com":
        sys.exit(1)
elif r.hostname != "github.com":
    sys.exit(1)
PYVALIDATE
}

# Project only public identity and key names. Existing credential values never
# leave kubectl's internal object. The resourceVersion fences a concurrent edit.
function inspect_repo_secret() {
    kubectl get secret "$SECRET_NAME" -n "$ARGO_NS" --ignore-not-found -o 'go-template={{.metadata.uid}}{{"\n"}}{{.metadata.resourceVersion}}{{"\n"}}{{index .metadata.labels "argocd.argoproj.io/secret-type"}}{{"\n"}}{{index .data "url"}}{{"\n"}}{{index .data "type"}}{{"\n"}}{{index .data "githubAppID"}}{{"\n"}}{{index .data "githubAppInstallationID"}}{{"\n"}}{{index .data "githubAppEnterpriseBaseUrl"}}{{"\n"}}{{range $key, $value := .data}}{{$key}}{{"\n"}}{{end}}' > "$WORK/existing" 2> "$WORK/kubectl-error" \
        || cap_fail 5 "could not inspect the repository Secret"
    [[ -s "$WORK/existing" ]] || return 0
    python3 - "$WORK/existing" "$TRANSPORT" "$REPO_URL" "$GITHUB_APP_ID" "$GITHUB_INSTALLATION_ID" "$GITHUB_ENTERPRISE_URL" <<'PYEXISTING' || cap_fail 3 "existing Secret belongs to another repository or authentication mode; use a separate Secret or explicitly remove the old connection first"
import base64, pathlib, sys
path, transport, repo, app, installation, enterprise = sys.argv[1:]
rows = pathlib.Path(path).read_text().splitlines()
if len(rows) < 8 or not rows[0] or not rows[1] or rows[2] != "repository":
    sys.exit(1)
def decode(value):
    return "" if value == "<no value>" else base64.b64decode(value, validate=True).decode()
try:
    if decode(rows[3]) != repo or decode(rows[4]) != "git": sys.exit(1)
    allowed = {"type", "url"} | {
        "https": {"username", "password"}, "ssh": {"sshPrivateKey"},
        "github-app": {"githubAppID", "githubAppInstallationID", "githubAppPrivateKey", "githubAppEnterpriseBaseUrl"}
    }[transport]
    if set(rows[8:]) - allowed: sys.exit(1)
    if transport == "github-app" and [decode(v) for v in rows[5:8]] != [app, installation, enterprise]: sys.exit(1)
except (ValueError, UnicodeError):
    sys.exit(1)
PYEXISTING
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
    elif [[ "$TRANSPORT" == "github-app" ]]; then
        from+=("--from-literal=githubAppID=${GITHUB_APP_ID}" "--from-literal=githubAppInstallationID=${GITHUB_INSTALLATION_ID}" "--from-file=githubAppPrivateKey=${WORK}/githubAppPrivateKey")
        [[ -z "$GITHUB_ENTERPRISE_URL" ]] || from+=("--from-literal=githubAppEnterpriseBaseUrl=${GITHUB_ENTERPRISE_URL}")
    else
        cp "$SSH_KEY_FILE" "${WORK}/sshPrivateKey"; chmod 600 "${WORK}/sshPrivateKey"
        from+=("--from-file=sshPrivateKey=${WORK}/sshPrivateKey")
    fi

    # A reviewed public identity plus its resourceVersion prevents replacing a
    # different connection if another operator changes it between read and write.
    kubectl create secret generic "$SECRET_NAME" -n "$ARGO_NS" "${from[@]}" \
        --dry-run=client -o json > "$WORK/desired.json" 2> "$WORK/kubectl-error" \
        || cap_fail 5 "could not prepare repository Secret"
    python3 - "$WORK/desired.json" "$WORK/existing" <<'PYSECRET'
import json, pathlib, sys
path, existing = map(pathlib.Path, sys.argv[1:])
secret = json.loads(path.read_text())
secret["metadata"].setdefault("labels", {})["argocd.argoproj.io/secret-type"] = "repository"
if existing.stat().st_size:
    identity = existing.read_text().splitlines()
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": identity[0]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": identity[1]},
        {"op": "replace", "path": "/data", "value": secret["data"]},
    ]
    path.with_name("secret-patch.json").write_text(json.dumps(patch))
path.write_text(json.dumps(secret))
PYSECRET
    if [[ -s "$WORK/existing" ]]; then
        kubectl patch secret "$SECRET_NAME" -n "$ARGO_NS" --type=json --patch-file "$WORK/secret-patch.json" > "$WORK/write-result" 2> "$WORK/kubectl-error" \
            || cap_fail 5 "could not update repository Secret; read its current connection before retrying"
    else
        kubectl create -f "$WORK/desired.json" > "$WORK/write-result" 2> "$WORK/kubectl-error" \
            || cap_fail 5 "could not create repository Secret; read its current connection before retrying"
    fi
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
    command -v python3 >/dev/null || cap_fail 4 "python3 is required to validate repository connection identity"
    make_workdir
    if [[ "$TRANSPORT" == "github-app" ]]; then
        cp "$GITHUB_KEY_FILE" "$WORK/githubAppPrivateKey"
        openssl rsa -in "$WORK/githubAppPrivateKey" -passin pass: -check -noout </dev/null >/dev/null 2>&1 \
            || cap_fail 2 "GitHub App private key must be an unencrypted RSA PEM key"
    fi
    check_prerequisites
    refuse_engine_app_of_apps
    inspect_repo_secret
    # Check the project before any credential write.
    kubectl get appproject "$PROJECT" -n "$ARGO_NS" -o name &>/dev/null \
        || cap_fail 4 "AppProject ${PROJECT} does not exist in ${ARGO_NS}"

    ensure_repo_secret
    ensure_project_source_repo

    collect_result
    cap_ok
}

main "$@"
