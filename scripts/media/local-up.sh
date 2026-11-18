#!/usr/bin/env bash
#
# scripts/media/local-up.sh
# =========================
#
# Capability: media.localUp -- start the local calls media server beside the k3d
# cluster: LiveKit, the Redis it keeps room state in, and Caddy terminating TLS in front
# of it (deploy/media/compose.yaml). This is `make media-up`, and `make up` runs it last
# unless MEDIA=0.
#
# WHY OUTSIDE THE CLUSTER. Calls media servers are an EXTERNAL media target in every
# environment: this container here, a rented server for a cloud installation. The engine
# reaches either through the same eight MEMQL_CALLS_MEDIA_* values
# (integrations/calls/config.go) -- a control API outside the cluster, a public address
# browsers reach over TLS, and webhooks back through the front door -- so the local
# cluster exercises the path a cloud one takes. Running it inside k3d would make the
# local path a different path.
#
# WHAT IT READS, AND FROM WHERE. Everything that must agree with the cluster is read from
# the cluster, so the server and the bff cannot disagree:
#   - the two key pairs and the public URL, from the memql-calls-media Secret that
#     `make secrets` seeds (scripts/k3d/seed-secrets.sh) and the bff reads;
#   - the domain, from the memql-domain ConfigMap, for the api host webhooks go to;
#   - the certificate authority that signed the front door, from memql-front-door-ca,
#     which LiveKit trusts for those webhooks (SSL_CERT_FILE in compose.yaml).
# The TLS pair Caddy serves is the front door's own mkcert pair, at the paths make secrets
# loads it from (scripts/lib/localtls.sh), so a browser that trusts the front door trusts
# the media server.
#
# ONLY EVER THE LOCAL CLUSTER. Every kubectl read names --context=k3d-<cluster> rather
# than following the current context: this script copies a credential Secret onto disk
# and starts a server with it, and a kubectl pointed at a cloud installation must never be
# where those keys come from.
#
# WHAT IT WRITES. livekit.yaml, the Caddyfile and a copy of the CA under the state
# directory (default ~/.memql/media: outside every checkout, beside the front door's pair
# in ~/.memql/certs), rendered from deploy/media/*.tmpl. livekit.yaml carries both key
# pairs, so the directory is 0700 and the files 0600, and nothing lands in the repository
# for `git add` to reach. No credential is passed in argv or the environment; the
# containers read them from the rendered file. make media-down removes the files.
#
# IDEMPOTENT. A second run renders the same files, Compose leaves the running containers
# alone, and the envelope says changed: false. A changed file (a re-seeded key, a reissued
# pair) changes the digest the containers carry (MEDIA_CONFIG_DIGEST), which makes Compose
# recreate them: the server reads its configuration only at start.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single JSON
# result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param
#             | 3 refused (the Secret holds values a media server cannot be configured
#                  with; re-seed it)
#             | 4 prerequisite missing (Docker, its daemon or Compose; kubectl or the
#                 cluster; the memql-calls-media Secret; the front door's CA or pair)
#             | 5 op failed (a cluster read, docker compose, or no answer through TLS)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "media.localUp" "Start the local calls media server (LiveKit, Redis, Caddy) beside the k3d cluster, configured from the memql-calls-media Secret."
cap_spec_param "cluster"      "k3d cluster name; every read uses its context, k3d-<cluster> (default: memql)"
cap_spec_param "namespace"    "k8s namespace holding memql-calls-media (default: memql)"
cap_spec_param "domain"       "front-door apex the webhooks go to, at api.<domain> (default: the domain this cluster serves, else memql.localhost)"
cap_spec_param "state-dir"    "where the rendered configuration is written (default: ~/.memql/media)"
cap_spec_param "tls-cert"     "the front door's TLS certificate, which Caddy serves (default: the pair make secrets loads)"
cap_spec_param "tls-key"      "the front door's TLS private key (default: the pair make secrets loads)"
cap_spec_param "docker"       "the docker CLI (default: docker on PATH)"
cap_spec_param "wait-seconds" "how long to wait for LiveKit to answer through TLS; 0 does not wait (default: 60)"
cap_spec_param "dry-run"      "render the configuration and report what would start, starting nothing (flag)"

#=============================================================================
# CONFIGURATION
#=============================================================================

PROJECT="memql-media"
COMPOSE_FILE="${REPO_ROOT}/deploy/media/compose.yaml"
TEMPLATE_DIR="${REPO_ROOT}/deploy/media"
MEDIA_SECRET="memql-calls-media"

# The rendered files, by name. local-down.sh removes exactly these.
RENDERED_LIVEKIT="livekit.yaml"
RENDERED_CADDY="Caddyfile"
RENDERED_CA="front-door-ca.pem"

# A DNS label, lower case: letters, digits and inner hyphens.
LABEL='[a-z0-9]([a-z0-9-]*[a-z0-9])?'
DNS_NAME="^${LABEL}(\.${LABEL})+$"
# What a key and a secret may hold. Both are written into YAML double quotes and through
# a bash substitution (substitute below), so neither may carry a quote, a backslash, an
# ampersand or a newline. The seeded values are hex with a short prefix.
KEY_PATTERN='^[A-Za-z0-9._-]+$'
SECRET_PATTERN='^[A-Za-z0-9._+/=-]{32,}$'
# The public URL: a scheme browsers reach over TLS, a host, an optional port, no path.
PUBLIC_URL_PATTERN="^(wss|https)://(${LABEL}(\.${LABEL})+)(:([0-9]{1,5}))?$"

CLUSTER_NAME=""
CONTEXT=""
NAMESPACE=""
DOMAIN=""
STATE_DIR=""
TLS_CERT=""
TLS_KEY=""
DOCKER=""
WAIT_SECONDS=""
DRY_RUN=""

API_KEY=""
API_SECRET=""
WEBHOOK_KEY=""
WEBHOOK_SECRET=""
PUBLIC_URL=""
MEDIA_HOST=""
HTTPS_PORT=""
API_HOST=""
WEBHOOK_URL=""
FRONT_DOOR_CA=""

STARTED=false
ANSWERED=false

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info() { cap_info "$*"; }
function warn() { cap_warn "$*"; }

# BSD/macOS base64 spells decode -D and GNU spells it --decode, so probe rather than
# assume (the same probe scripts/k3d/seed-secrets.sh makes).
function b64_decode() {
    if printf '' | base64 --decode >/dev/null 2>&1; then
        base64 --decode
    else
        base64 -D
    fi
}

function absolute_path() {
    case "$1" in
        /*) printf '%s' "$1" ;;
        *)  printf '%s/%s' "$PWD" "$1" ;;
    esac
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function require_docker() {
    if ! command -v "$DOCKER" >/dev/null 2>&1; then
        cap_fail 4 "Docker is required but '${DOCKER}' was not found. Install Docker Desktop or Docker Engine first."
    fi
    if ! "$DOCKER" info >/dev/null 2>&1; then
        cap_fail 4 "the Docker daemon is not running, or '${DOCKER}' cannot reach it. Start Docker first."
    fi
    if ! "$DOCKER" compose version >/dev/null 2>&1; then
        cap_fail 4 "the Docker Compose plugin is required ('${DOCKER} compose version' failed). Install it first."
    fi
}

function require_cluster() {
    if ! command -v kubectl >/dev/null 2>&1; then
        cap_fail 4 "kubectl is required but not found. Install kubectl first."
    fi
    if ! kubectl get namespace "$NAMESPACE" --context="$CONTEXT" -o name >/dev/null 2>&1; then
        cap_fail 4 "kubectl cannot reach namespace '${NAMESPACE}' through context ${CONTEXT}. Bring the local cluster up first: make up"
    fi
}

#=============================================================================
# READING THE CLUSTER
#=============================================================================

# The domain this cluster says it serves, else the local default: the same order
# scripts/k3d/seed-secrets.sh resolves it in, so the webhook URL names the front door the
# Secret's public URL was derived beside.
function resolve_domain_default() {
    local existing
    existing="$(kubectl get configmap memql-domain --namespace="$NAMESPACE" --context="$CONTEXT" \
        -o 'jsonpath={.data.MEMQL_DOMAIN}' 2>/dev/null || true)"
    if [[ -n "$existing" ]]; then
        printf '%s' "$existing"
        return 0
    fi
    printf '%s' "$MEMQL_LOCAL_DOMAIN"
}

# A missing Secret is a prerequisite (make secrets seeds it); a Secret that cannot be
# read is a failure, never a reason to tell the operator to re-seed.
function read_media_secret() {
    local probe raw api_key api_secret webhook_key webhook_secret public_url
    if ! probe="$(kubectl get secret "$MEDIA_SECRET" --namespace="$NAMESPACE" --context="$CONTEXT" -o name 2>&1)"; then
        case "$probe" in
            *NotFound*)
                cap_fail 4 "the ${MEDIA_SECRET} Secret does not exist in namespace '${NAMESPACE}', so there is no media server to configure. Seed it with: make secrets" ;;
        esac
        cap_fail 5 "could not read the ${MEDIA_SECRET} Secret in namespace '${NAMESPACE}'"
    fi
    # One read for all five, so they come from one version of the Secret. Base64 has no
    # '|', and read keeps the empty field of a missing key in its place.
    raw="$(kubectl get secret "$MEDIA_SECRET" --namespace="$NAMESPACE" --context="$CONTEXT" \
        -o 'jsonpath={.data.MEMQL_CALLS_MEDIA_API_KEY}|{.data.MEMQL_CALLS_MEDIA_API_SECRET}|{.data.MEMQL_CALLS_MEDIA_WEBHOOK_KEY}|{.data.MEMQL_CALLS_MEDIA_WEBHOOK_SECRET}|{.data.MEMQL_CALLS_MEDIA_PUBLIC_URL}' \
        2>/dev/null)" || cap_fail 5 "could not read the ${MEDIA_SECRET} Secret's values in namespace '${NAMESPACE}'"
    IFS='|' read -r api_key api_secret webhook_key webhook_secret public_url <<<"$raw"
    API_KEY="$(printf '%s' "$api_key" | b64_decode)" || cap_fail 5 "${MEDIA_SECRET}: MEMQL_CALLS_MEDIA_API_KEY is not base64"
    API_SECRET="$(printf '%s' "$api_secret" | b64_decode)" || cap_fail 5 "${MEDIA_SECRET}: MEMQL_CALLS_MEDIA_API_SECRET is not base64"
    WEBHOOK_KEY="$(printf '%s' "$webhook_key" | b64_decode)" || cap_fail 5 "${MEDIA_SECRET}: MEMQL_CALLS_MEDIA_WEBHOOK_KEY is not base64"
    WEBHOOK_SECRET="$(printf '%s' "$webhook_secret" | b64_decode)" || cap_fail 5 "${MEDIA_SECRET}: MEMQL_CALLS_MEDIA_WEBHOOK_SECRET is not base64"
    PUBLIC_URL="$(printf '%s' "$public_url" | b64_decode)" || cap_fail 5 "${MEDIA_SECRET}: MEMQL_CALLS_MEDIA_PUBLIC_URL is not base64"
}

# Refusals name the value at fault and never repeat it.
function refuse_media_secret() {
    cap_fail 3 "${MEDIA_SECRET}: $1. Re-seed it: delete the Secret and run make secrets, then make media-up."
}

function validate_media_secret() {
    [[ "$API_KEY" =~ $KEY_PATTERN ]] \
        || refuse_media_secret "MEMQL_CALLS_MEDIA_API_KEY is empty or holds a character other than letters, digits, '.', '_' and '-'"
    [[ "$WEBHOOK_KEY" =~ $KEY_PATTERN ]] \
        || refuse_media_secret "MEMQL_CALLS_MEDIA_WEBHOOK_KEY is empty or holds a character other than letters, digits, '.', '_' and '-'"
    [[ "$API_SECRET" =~ $SECRET_PATTERN ]] \
        || refuse_media_secret "MEMQL_CALLS_MEDIA_API_SECRET must be at least 32 characters of letters, digits and . _ + / = -"
    [[ "$WEBHOOK_SECRET" =~ $SECRET_PATTERN ]] \
        || refuse_media_secret "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET must be at least 32 characters of letters, digits and . _ + / = -"
    # LiveKit holds one secret per key, so one key for both pairs would leave one of them
    # unusable.
    [[ "$API_KEY" != "$WEBHOOK_KEY" ]] \
        || refuse_media_secret "MEMQL_CALLS_MEDIA_API_KEY and MEMQL_CALLS_MEDIA_WEBHOOK_KEY are the same key, and the server holds one secret per key"
    if [[ "$PUBLIC_URL" =~ $PUBLIC_URL_PATTERN ]]; then
        MEDIA_HOST="${BASH_REMATCH[2]}"
        HTTPS_PORT="${BASH_REMATCH[7]:-443}"
    else
        refuse_media_secret "MEMQL_CALLS_MEDIA_PUBLIC_URL must be wss:// or https:// with a lower-case host, an optional port and no path"
    fi
    # 10#: a port written with a leading zero is decimal, not octal.
    HTTPS_PORT=$((10#$HTTPS_PORT))
    if [[ "$HTTPS_PORT" -lt 1 || "$HTTPS_PORT" -gt 65535 ]]; then
        refuse_media_secret "MEMQL_CALLS_MEDIA_PUBLIC_URL names port ${HTTPS_PORT}, which is not a port"
    fi
}

function read_front_door_ca() {
    FRONT_DOOR_CA="$(kubectl get configmap memql-front-door-ca --namespace="$NAMESPACE" --context="$CONTEXT" \
        -o 'jsonpath={.data.ca\.crt}' 2>/dev/null || true)"
    if [[ -z "$FRONT_DOOR_CA" ]]; then
        cap_fail 4 "the memql-front-door-ca ConfigMap (the certificate authority that signed the front door) is missing in namespace '${NAMESPACE}', so LiveKit could not verify the front door its webhooks go to. Seed it with: make secrets"
    fi
}

function require_tls_pair() {
    if [[ ! -s "$TLS_CERT" || ! -s "$TLS_KEY" ]]; then
        cap_fail 4 "the front door's TLS pair (${TLS_CERT}, ${TLS_KEY}) is missing, and Caddy serves it for the media server too. Issue and seed it with: make secrets"
    fi
}

#=============================================================================
# RENDERING
#=============================================================================

# substitute <text> <name> <value> [<name> <value>]... -- every {{name}} replaced by its
# value. Every value was validated first and holds no character bash's replacement treats
# specially (& or \), which is what makes a plain parameter substitution safe here.
function substitute() {
    local text="$1"
    shift
    while [[ $# -ge 2 ]]; do
        text="${text//"{{$1}}"/$2}"
        shift 2
    done
    printf '%s\n' "$text"
}

# write_private <path> <content> -- the content and one newline, written 0600 through a
# temporary file and a rename, so a reader never sees half a file and the file is never
# readable by anyone else.
function write_private() {
    local target="$1" tmp="$1.tmp.$$"
    if ! (umask 077 && printf '%s\n' "$2" >"$tmp"); then
        rm -f "$tmp"
        cap_fail 5 "could not write ${target}"
    fi
    mv -f "$tmp" "$target" || cap_fail 5 "could not write ${target}"
}

function render_template() {
    local template="$1" target="$2" text
    shift 2
    text="$(substitute "$(cat "$template")" "$@")"
    # A placeholder left over is a template this script does not know how to fill, and a
    # server started on it would carry the literal braces.
    if [[ "$text" == *"{{"* ]]; then
        cap_fail 5 "${template} has a value this script does not fill; the template and scripts/media/local-up.sh disagree"
    fi
    write_private "$target" "$text"
}

function render_configuration() {
    mkdir -p "$STATE_DIR" || cap_fail 5 "could not create ${STATE_DIR}"
    chmod 700 "$STATE_DIR" || cap_fail 5 "could not make ${STATE_DIR} private"
    render_template "${TEMPLATE_DIR}/livekit.yaml.tmpl" "${STATE_DIR}/${RENDERED_LIVEKIT}" \
        API_KEY "$API_KEY" API_SECRET "$API_SECRET" \
        WEBHOOK_KEY "$WEBHOOK_KEY" WEBHOOK_SECRET "$WEBHOOK_SECRET" \
        WEBHOOK_URL "$WEBHOOK_URL"
    render_template "${TEMPLATE_DIR}/Caddyfile.tmpl" "${STATE_DIR}/${RENDERED_CADDY}" \
        MEDIA_HOST "$MEDIA_HOST" HTTPS_PORT "$HTTPS_PORT"
    write_private "${STATE_DIR}/${RENDERED_CA}" "$FRONT_DOOR_CA"
    info "rendered the media server's configuration into ${STATE_DIR}"
}

#=============================================================================
# STARTING
#=============================================================================

# config_digest -- one SHA-256 over every file the containers read, which is what makes
# Compose recreate them when one changes (see the file note). A digest of a file carrying
# 256-bit secrets says nothing about them. It runs inside a command substitution, so it
# reports a failure through its status and never with cap_fail, whose envelope would be
# captured instead of printed.
function config_digest() {
    cat "${STATE_DIR}/${RENDERED_LIVEKIT}" "${STATE_DIR}/${RENDERED_CADDY}" "${STATE_DIR}/${RENDERED_CA}" \
        "$TLS_CERT" "$TLS_KEY" | openssl dgst -sha256 | awk '{print $NF}'
}

# already_running <digest> -- succeeds when every service of the project is running and the
# two that read the rendered files carry this digest, so the compose up that follows
# changes nothing. Read through Compose's own labels, which need no compose file.
function already_running() {
    local service id carried
    for service in livekit redis caddy; do
        id="$("$DOCKER" ps --quiet --filter "label=com.docker.compose.project=${PROJECT}" \
            --filter "label=com.docker.compose.service=${service}" --filter status=running 2>/dev/null | head -n 1)"
        [[ -n "$id" ]] || return 1
        if [[ "$service" == "redis" ]]; then
            continue
        fi
        carried="$("$DOCKER" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$id" 2>/dev/null \
            | sed -n 's/^MEDIA_CONFIG_DIGEST=//p' | head -n 1)"
        [[ "$carried" == "$1" ]] || return 1
    done
}

function start_media_server() {
    local digest unchanged=""
    command -v openssl >/dev/null 2>&1 \
        || cap_fail 4 "openssl is required to digest the media server's configuration. Install it first."
    digest="$(config_digest)" || cap_fail 5 "could not digest the rendered configuration"
    [[ -n "$digest" ]] || cap_fail 5 "could not digest the rendered configuration"
    if already_running "$digest"; then
        unchanged=1
    fi
    info "starting ${PROJECT} (LiveKit, Redis, Caddy) from deploy/media/compose.yaml..."
    # Paths and host names only: the credentials reach the containers inside the rendered
    # file, never through argv or the environment.
    if ! MEDIA_CONFIG_DIR="$STATE_DIR" MEDIA_TLS_CERT="$TLS_CERT" MEDIA_TLS_KEY="$TLS_KEY" \
        MEDIA_API_HOST="$API_HOST" MEDIA_HTTPS_PORT="$HTTPS_PORT" MEDIA_CONFIG_DIGEST="$digest" \
        "$DOCKER" compose --project-name "$PROJECT" --file "$COMPOSE_FILE" up --detach --wait --remove-orphans >&2; then
        cap_fail 5 "docker compose could not start ${PROJECT}; its output is above, and the containers' logs are at: ${DOCKER} compose --project-name ${PROJECT} logs"
    fi
    STARTED=true
    if [[ -z "$unchanged" ]]; then
        cap_changed
    fi
}

# wait_for_answer -- LiveKit answers "OK" at its root, so a request through Caddy that gets
# it back proves the TLS pair, the proxy and the server together. The name is pinned to
# loopback with --resolve, because what is being checked is the server, not how this
# machine resolves *.localhost.
function wait_for_answer() {
    local url="https://${MEDIA_HOST}:${HTTPS_PORT}/" deadline body
    if [[ "$WAIT_SECONDS" -eq 0 ]]; then
        info "not waiting for the media server to answer (--wait-seconds=0)"
        return 0
    fi
    if ! command -v curl >/dev/null 2>&1; then
        warn "curl is not installed, so whether LiveKit answers at ${url} was not checked"
        return 0
    fi
    deadline=$((SECONDS + WAIT_SECONDS))
    while [[ "$SECONDS" -lt "$deadline" ]]; do
        body="$(curl --silent --max-time 5 --cacert "${STATE_DIR}/${RENDERED_CA}" \
            --resolve "${MEDIA_HOST}:${HTTPS_PORT}:127.0.0.1" "$url" 2>/dev/null)" || body=""
        if [[ "$body" == "OK" ]]; then
            ANSWERED=true
            info "LiveKit answers at ${url}"
            return 0
        fi
        sleep 2
    done
    cap_fail 5 "the media server did not answer at ${url} within ${WAIT_SECONDS}s; see: ${DOCKER} compose --project-name ${PROJECT} logs"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function report() {
    local dry=false
    if [[ -n "$DRY_RUN" ]]; then dry=true; fi
    cap_result_set     project "$PROJECT"
    cap_result_set     stateDir "$STATE_DIR"
    cap_result_set     publicUrl "$PUBLIC_URL"
    cap_result_set     webhookUrl "$WEBHOOK_URL"
    cap_result_set_raw dryRun "$dry"
    cap_result_set_raw started "$STARTED"
    cap_result_set_raw answered "$ANSWERED"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    NAMESPACE="$(cap_param namespace "${MEMQL_K3D_NAMESPACE:-memql}")"
    STATE_DIR="$(cap_param state-dir "${HOME:-}/.memql/media")"
    TLS_CERT="$(cap_param tls-cert "${MEMQL_LOCAL_TLS_CERT:-$MEMQL_LOCAL_TLS_DEFAULT_CERT}")"
    TLS_KEY="$(cap_param tls-key "${MEMQL_LOCAL_TLS_KEY:-$MEMQL_LOCAL_TLS_DEFAULT_KEY}")"
    DOCKER="$(cap_param docker "docker")"
    WAIT_SECONDS="$(cap_param wait-seconds "60")"
    if cap_bool dry-run; then DRY_RUN=1; else DRY_RUN=""; fi
    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"
    [[ "$WAIT_SECONDS" =~ ^[0-9]{1,6}$ ]] || cap_fail 2 "--wait-seconds takes a whole number of seconds; got '${WAIT_SECONDS}'"
    WAIT_SECONDS=$((10#$WAIT_SECONDS))
    CONTEXT="k3d-${CLUSTER_NAME}"
    # Compose resolves a relative bind source against the compose file's directory, not
    # this shell's, so every path it is handed is made absolute here.
    STATE_DIR="$(absolute_path "$STATE_DIR")"
    TLS_CERT="$(absolute_path "$TLS_CERT")"
    TLS_KEY="$(absolute_path "$TLS_KEY")"

    # Docker first: a run that could start nothing reads no credential and renders none.
    # A dry run starts nothing, so it needs no Docker at all.
    if [[ -z "$DRY_RUN" ]]; then
        require_docker
    fi
    require_cluster
    DOMAIN="$(cap_param domain "$(resolve_domain_default)")"
    [[ "$DOMAIN" =~ $DNS_NAME ]] || cap_fail 2 "--domain must be a lower-case DNS name of two or more labels; got '${DOMAIN}'"
    API_HOST="api.${DOMAIN}"
    WEBHOOK_URL="https://${API_HOST}/inbound/livekit"

    read_media_secret
    validate_media_secret
    require_tls_pair
    read_front_door_ca
    render_configuration

    if [[ -n "$DRY_RUN" ]]; then
        info "dry run: would start ${PROJECT} from ${COMPOSE_FILE}, serving ${PUBLIC_URL} with webhooks to ${WEBHOOK_URL}"
        report
        cap_ok
    fi

    start_media_server
    wait_for_answer
    info "media server up: browsers connect to ${PUBLIC_URL}; the bff's control API is LiveKit's port 7880; webhooks go to ${WEBHOOK_URL}"
    report
    cap_ok
}

main "$@"
