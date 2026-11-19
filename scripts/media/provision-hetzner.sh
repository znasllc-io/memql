#!/usr/bin/env bash
#
# scripts/media/provision-hetzner.sh
# ==================================
#
# Capability: media.provisionHetzner -- create one calls media server on Hetzner Cloud:
# the shared memql-media firewall, then a server whose first boot installs the media
# server (deploy/media/cloud-init.yaml.tmpl). It answers {serverId, ipv4, ipv6}: the
# addresses the server's DNS records name. The runbook, from this step to the first call,
# is docs/public/operate/calls.md ("Production media server").
#
# WHAT IT DOES NOT HAND THE SERVER: ITS KEYS. The cloud-init it renders carries no key,
# token or secret, because Hetzner keeps a server's user data and hands it out again
# (its API and console, and the metadata service on the server). This script never reads
# the media server's credentials at all. They arrive at the operator's step, over SSH,
# on the stdin of the command the first boot installs:
#     ssh root@<ipv4> memql-media configure --domain=<instance domain> < media.env
#
# THE API TOKEN. HCLOUD_TOKEN is read from the operator's environment (for now: the calls
# design moves it into MemQL, sealed under the master key, once MemQL creates servers
# itself), and then removed from this script's environment, so no command it runs
# inherits it. It is written only into curl's
# configuration, through a pipe from a shell builtin, on curl's stdin (--config -): not
# into an argv, which every process on the machine can read with ps; not to a file; and
# not to any output. A value that could not be a token is refused before it is used,
# because a quote or a newline in it would be a second curl option.
#
# WHAT A RUN CREATES, in order, each only when it is missing:
#   1. the firewall memql-media, labelled memql-media and applied by the label selector
#      memql-media, so every media server this script makes is behind it -- including
#      the one it is about to create, which carries the label. A firewall of that name
#      without the label is someone else's and is refused (exit 3), as is a server of the
#      requested name without its label. An existing firewall's rules are left as they
#      are: the operator may have narrowed SSH to their own address. Only the selector is
#      put back when it is missing, before the server is created.
#   2. the server <name>, labelled memql-media=<name>, from the rendered cloud-init and
#      the named SSH key, with an IPv4 and an IPv6 address. A server of that name that
#      this script made is answered as it is (changed: false) when its location and type
#      are the ones asked for, and refused (exit 3) when they are not: an existing server
#      is never changed to match.
# The create action is followed to its end, and so is the firewall's application to the
# new server (reported as firewallApplied; still pending after a minute is a warning).
#
# A REAL RUN IS GUARDED. It creates a server that is billed by the hour, so without
# --confirm=provision-<name> it is refused (exit 3) before the token is even looked at.
# A dry run calls nothing and needs no token: it prints the two requests a run against an
# empty project would send -- the firewall's and the server's, with the rendered cloud-init
# as its user_data -- in the envelope, and the cloud-init itself on stderr.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single JSON
# result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param (including a value that could not be an API token)
#             | 3 refused (no --confirm on a real run; a server or firewall of that name
#                 that this script did not make, or an existing server elsewhere)
#             | 4 prerequisite missing (HCLOUD_TOKEN, or one Hetzner refuses; curl or jq;
#                 the SSH key in the project)
#             | 5 op failed (Hetzner refused a request or failed its action; the
#                 templates do not render)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "media.provisionHetzner" "Create one calls media server on Hetzner Cloud: the memql-media firewall and a server whose cloud-init installs the media server and carries no secret."
cap_spec_param_required "name"        "the server's name, one lower-case DNS label (m1); its public host is <name>.media.<instance domain>"
cap_spec_param_required "location"    "ash (Ashburn) or hil (Hillsboro)"
cap_spec_param_required "server-type" "a Hetzner server type the location offers"
cap_spec_param          "image"       "the server's image (default: ubuntu-24.04, the only one its cloud-init is written for)"
cap_spec_param_required "ssh-key"     "the name of an SSH key in the Hetzner project; the operator's step reaches the server with it"
cap_spec_param          "confirm"     "required for a real run: the exact phrase provision-<name>"
cap_spec_param          "dry-run"     "print the API requests and the rendered cloud-init, calling nothing (flag)"

#=============================================================================
# CONFIGURATION
#=============================================================================

HCLOUD_API="https://api.hetzner.cloud/v1"
CLOUD_INIT_TEMPLATE="${REPO_ROOT}/deploy/media/cloud-init.yaml.tmpl"
LIVEKIT_TEMPLATE="${REPO_ROOT}/deploy/media/livekit.yaml.tmpl"
# Hetzner refuses user data over 32 KiB.
USER_DATA_LIMIT=32768

# One firewall for every media server, applied by this label (see the file note).
FIREWALL_NAME="memql-media"
MEDIA_LABEL="memql-media"

# The ports a media server needs, as protocol|port|why. ufw in the cloud-init opens the
# same ones (scripts/media/provision_hetzner_test.go holds the two lists together).
FIREWALL_RULES=(
    "tcp|22|SSH, for the operator's step that hands the server its keys"
    "tcp|80|certificate issuance (ACME HTTP-01)"
    "tcp|443|signalling, the control API and TURN over TLS, routed by server name"
    "tcp|7881|media over TCP (ICE)"
    "udp|7882|media over UDP (ICE)"
    "udp|3478|TURN over UDP, which also serves participants as STUN"
    "icmp||ping, and the ICMP path MTU discovery needs"
)

NAME=""
LOCATION=""
SERVER_TYPE=""
IMAGE=""
SSH_KEY=""
CONFIRM=""
DRY_RUN=""
# The token, unexported and out of the environment once main has read it.
API_TOKEN=""

USER_DATA=""
# USER_DATA as a JSON string, quotes included, escaped once.
USER_DATA_JSON=""
# The server find_server found, as Hetzner answered it.
SERVER_JSON=""
FIREWALL_ID=""
SERVER_ID=""
IPV4=""
IPV6=""
IPV6_NETWORK=""
CREATED=false
FIREWALL_CREATED=false
FIREWALL_APPLIED=false

#=============================================================================
# THE REQUEST
#=============================================================================

function validate_params() {
    cap_require name "$NAME"
    cap_require location "$LOCATION"
    cap_require server-type "$SERVER_TYPE"
    cap_require ssh-key "$SSH_KEY"
    # One DNS label: the server's public host is <name>.media.<domain>, and Hetzner holds
    # a server's name to a hostname (RFC 1123).
    [[ "$NAME" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] \
        || cap_fail 2 "--name must be one lower-case DNS label (letters, digits and inner hyphens), such as m1; got '${NAME}'"
    # The US locations the calls design names (Ashburn and Hillsboro).
    case "$LOCATION" in
        ash | hil) ;;
        *) cap_fail 2 "--location must be ash (Ashburn) or hil (Hillsboro); got '${LOCATION}'" ;;
    esac
    [[ "$SERVER_TYPE" =~ ^[a-z0-9][a-z0-9-]{0,31}$ ]] \
        || cap_fail 2 "--server-type must be a Hetzner server type's name, such as cpx31; got '${SERVER_TYPE}'"
    # The cloud-init installs Ubuntu's packages and asks Ubuntu's cloud-init for its
    # status, so another image would boot a server that never becomes a media server.
    [[ "$IMAGE" == "ubuntu-24.04" ]] \
        || cap_fail 2 "--image must be ubuntu-24.04, the image deploy/media/cloud-init.yaml.tmpl is written for; got '${IMAGE}'"
    [[ "$SSH_KEY" =~ ^[A-Za-z0-9._@-]{1,64}$ ]] \
        || cap_fail 2 "--ssh-key must name an SSH key of the Hetzner project in letters, digits and . _ @ - (rename it there if it has other characters); got '${SSH_KEY}'"
}

# embed_indented <file> -- the file as the content of a YAML literal block six spaces in.
function embed_indented() {
    local line
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ -z "$line" ]]; then
            printf '\n'
        else
            printf '      %s\n' "$line"
        fi
    done <"$1"
}

# render_cloud_init -- the template, with the LiveKit template embedded at its
# placeholder line and the comments that start a line dropped, except the
# #cloud-config the first line must be: those are the repository's explanation, and
# Hetzner takes at most 32 KiB. A comment inside an embedded file is indented, and kept.
# It runs inside a command substitution, so a failure is its status, never cap_fail.
function render_cloud_init() {
    local line first=1
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ -n "$first" ]]; then
            first=""
            printf '%s\n' "$line"
            continue
        fi
        case "$line" in
            '#'*) ;;
            '{{LIVEKIT_TEMPLATE}}') embed_indented "$LIVEKIT_TEMPLATE" || return 1 ;;
            *) printf '%s\n' "$line" ;;
        esac
    done <"$CLOUD_INIT_TEMPLATE"
}

function build_user_data() {
    [[ -r "$CLOUD_INIT_TEMPLATE" && -r "$LIVEKIT_TEMPLATE" ]] \
        || cap_fail 5 "could not read ${CLOUD_INIT_TEMPLATE} and ${LIVEKIT_TEMPLATE}"
    USER_DATA="$(render_cloud_init)" || cap_fail 5 "could not render ${CLOUD_INIT_TEMPLATE}"
    [[ "$USER_DATA" == "#cloud-config"$'\n'* ]] \
        || cap_fail 5 "${CLOUD_INIT_TEMPLATE} does not start with #cloud-config, so cloud-init would not read it"
    [[ "$USER_DATA" != *"{{LIVEKIT_TEMPLATE}}"* ]] \
        || cap_fail 5 "${CLOUD_INIT_TEMPLATE} holds its LiveKit placeholder somewhere other than a line of its own"
    local bytes
    bytes="$(printf '%s\n' "$USER_DATA" | wc -c | tr -d ' ')"
    [[ "$bytes" -le "$USER_DATA_LIMIT" ]] \
        || cap_fail 5 "the rendered cloud-init is ${bytes} bytes, over Hetzner's ${USER_DATA_LIMIT}-byte limit for user data; shorten the comments inside deploy/media/cloud-init.yaml.tmpl's embedded files"
    USER_DATA_JSON="\"$(json_escape_lines "$USER_DATA")\""
}

# json_escape_lines <text> -- cap_json_escape's escaping, a line at a time. The same
# characters are escaped, but on short strings: bash 3.2 (macOS's /bin/bash) makes a
# global substitution over the whole cloud-init quadratic, about two seconds a run.
function json_escape_lines() {
    local line out=""
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line//\\/\\\\}"
        line="${line//\"/\\\"}"
        line="${line//$'\r'/\\r}"
        line="${line//$'\t'/\\t}"
        out+="${line}\\n"
    done <<<"$1"
    printf '%s' "${out%\\n}"
}

function json_string() { printf '"%s"' "$(cap_json_escape "$1")"; }

function firewall_rules_json() {
    local entry proto rest port why rules=""
    for entry in "${FIREWALL_RULES[@]}"; do
        proto="${entry%%|*}"
        rest="${entry#*|}"
        port="${rest%%|*}"
        why="${rest#*|}"
        rules+="${rules:+,}{\"direction\":\"in\",\"protocol\":$(json_string "$proto")"
        if [[ -n "$port" ]]; then
            rules+=",\"port\":$(json_string "$port")"
        fi
        rules+=",\"source_ips\":[\"0.0.0.0/0\",\"::/0\"],\"description\":$(json_string "$why")}"
    done
    printf '[%s]' "$rules"
}

function selector_json() {
    printf '[{"type":"label_selector","label_selector":{"selector":%s}}]' "$(json_string "$MEDIA_LABEL")"
}

function firewall_body() {
    printf '{"name":%s,"labels":{%s:""},"rules":%s,"apply_to":%s}' \
        "$(json_string "$FIREWALL_NAME")" "$(json_string "$MEDIA_LABEL")" "$(firewall_rules_json)" "$(selector_json)"
}

# The server's request carries no firewall id: the label puts it behind memql-media,
# which a dry run can show without knowing an id.
function server_body() {
    printf '{"name":%s,"server_type":%s,"image":%s,"location":%s,"ssh_keys":[%s],"labels":{%s:%s},"public_net":{"enable_ipv4":true,"enable_ipv6":true},"start_after_create":true,"user_data":%s}' \
        "$(json_string "$NAME")" "$(json_string "$SERVER_TYPE")" "$(json_string "$IMAGE")" \
        "$(json_string "$LOCATION")" "$(json_string "$SSH_KEY")" \
        "$(json_string "$MEDIA_LABEL")" "$(json_string "$NAME")" "$USER_DATA_JSON"
}

#=============================================================================
# HETZNER'S API (a real run only)
#=============================================================================

function require_tools() {
    command -v curl >/dev/null 2>&1 || cap_fail 4 "curl is required to call Hetzner's API. Install it first."
    command -v jq >/dev/null 2>&1 || cap_fail 4 "jq is required to read Hetzner's answers. Install it first."
}

# read_token -- HCLOUD_TOKEN from the environment, then out of it (see the file note).
# Hetzner's tokens are 64 letters and digits; the shape checked here is wider than that
# and narrow enough that the token cannot be read as more than one curl option.
function read_token() {
    API_TOKEN="${HCLOUD_TOKEN:-}"
    unset HCLOUD_TOKEN
    [[ -n "$API_TOKEN" ]] \
        || cap_fail 4 "HCLOUD_TOKEN is not set: export a read-write API token of the Hetzner project (Security > API tokens) in this shell, and run again"
    # The length is compared apart from the pattern: macOS's regex library refuses a
    # bounded repeat over 255, and bash 3.2 there reports that as no match.
    [[ "$API_TOKEN" =~ ^[A-Za-z0-9_-]+$ && ${#API_TOKEN} -ge 16 && ${#API_TOKEN} -le 256 ]] \
        || cap_fail 2 "HCLOUD_TOKEN does not look like a Hetzner API token (16 to 256 letters, digits, '_' and '-'); it is not repeated here"
}

# auth_config -- curl's configuration for one request: the bearer header. printf is a
# builtin, so the token goes into the pipe without an argv of its own.
function auth_config() {
    printf 'header = "Authorization: Bearer %s"\n' "$API_TOKEN"
}

# hcloud <method> <path> [<body>] -- one request; sets HC_STATUS and HC_BODY. A body goes
# through a pipe too (process substitution), so nothing is written to disk. A transport
# failure (no answer at all) stops the run here.
HC_STATUS=""
HC_BODY=""
function hcloud() {
    local method="$1" path="$2" body="${3:-}" out rc=0
    local -a args=(--silent --show-error --proto '=https' --connect-timeout 15 --max-time 60
        --config - --request "$method" --header 'Content-Type: application/json'
        --write-out '\n%{http_code}')
    if [[ -n "$body" ]]; then
        out="$(auth_config | curl "${args[@]}" --data-binary @<(printf '%s' "$body") "${HCLOUD_API}${path}")" || rc=$?
    else
        out="$(auth_config | curl "${args[@]}" "${HCLOUD_API}${path}")" || rc=$?
    fi
    if [[ "$rc" -ne 0 ]]; then
        cap_fail 5 "could not reach Hetzner's API (${method} ${path}): curl exited ${rc}; its message is above"
    fi
    HC_STATUS="${out##*$'\n'}"
    HC_BODY="${out%$'\n'*}"
    if [[ "$HC_STATUS" == "401" || "$HC_STATUS" == "403" ]]; then
        cap_fail 4 "Hetzner refused HCLOUD_TOKEN (${method} ${path}: $(hc_error)): it must be a read-write API token of the project the server goes in"
    fi
}

# hc_error -- Hetzner's own reason for a refusal ({"error":{"code","message"}}).
function hc_error() {
    local message
    message="$(printf '%s' "$HC_BODY" | jq -r '.error | "\(.message) (\(.code))"' 2>/dev/null || true)"
    printf '%s' "${message:-HTTP ${HC_STATUS}}"
}

# hcloud_ok <method> <path> <want-status> [<body>] -- hcloud, refusing any other status.
function hcloud_ok() {
    local method="$1" path="$2" want="$3"
    hcloud "$method" "$path" "${4:-}"
    [[ "$HC_STATUS" == "$want" ]] || cap_fail 5 "Hetzner refused ${method} ${path} (HTTP ${HC_STATUS}): $(hc_error)"
}

function hc_field() { printf '%s' "$HC_BODY" | jq -r "$1"; }

# record_addresses <server-json-path> -- the server's IPv4, and its IPv6 address: Hetzner
# answers the /64 it routes to the server, and the server takes the first address in it
# (::1), which is what its AAAA records name.
function record_addresses() {
    local base="$1"
    SERVER_ID="$(hc_field "${base}.id")"
    [[ "$SERVER_ID" =~ ^[0-9]+$ ]] || cap_fail 5 "Hetzner's answer about ${NAME} carries no server id"
    IPV4="$(hc_field "${base}.public_net.ipv4.ip // empty")"
    IPV6_NETWORK="$(hc_field "${base}.public_net.ipv6.ip // empty")"
    IPV6=""
    if [[ "$IPV6_NETWORK" == *"::/64" ]]; then
        IPV6="${IPV6_NETWORK%/64}1"
    elif [[ -n "$IPV6_NETWORK" ]]; then
        cap_warn "Hetzner gave the server the IPv6 network ${IPV6_NETWORK}, whose first address this script cannot write out; its AAAA records name the address the server holds (ip -6 addr on the server)"
    fi
}

# find_server -- an existing server of the requested name: answered when this script
# made it for this request, refused otherwise (see the file note).
function find_server() {
    hcloud_ok GET "/servers?name=${NAME}" 200
    [[ "$(hc_field '.servers | length')" -gt 0 ]] || return 1
    local label location type
    label="$(hc_field ".servers[0].labels[\"${MEDIA_LABEL}\"] // empty")"
    [[ "$label" == "$NAME" ]] \
        || cap_fail 3 "the project already has a server named ${NAME} that this script did not make (it has no ${MEDIA_LABEL}=${NAME} label); choose another --name"
    location="$(hc_field '.servers[0].location.name // empty')"
    type="$(hc_field '.servers[0].server_type.name // empty')"
    if [[ "$location" != "$LOCATION" || "$type" != "$SERVER_TYPE" ]]; then
        cap_fail 3 "the media server ${NAME} already exists in ${location} as a ${type}, not in ${LOCATION} as a ${SERVER_TYPE}; an existing server is not changed. Ask for what it is, or remove it first (docs/public/operate/calls.md)"
    fi
    record_addresses '.servers[0]'
    SERVER_JSON="$(hc_field '.servers[0]')"
    cap_info "the media server ${NAME} already exists (id ${SERVER_ID}); nothing to create"
}

# existing_server_firewall_applied -- whether memql-media (FIREWALL_ID, once
# ensure_firewall has found it) is applied to the server find_server read.
function existing_server_firewall_applied() {
    FIREWALL_APPLIED="$(printf '%s' "$SERVER_JSON" \
        | jq "[.public_net.firewalls[]? | select(.id == ${FIREWALL_ID} and .status == \"applied\")] | length > 0")"
}

function require_ssh_key() {
    hcloud_ok GET "/ssh_keys?name=${SSH_KEY}" 200
    [[ "$(hc_field '.ssh_keys | length')" -gt 0 ]] \
        || cap_fail 4 "the Hetzner project has no SSH key named ${SSH_KEY}; add your public key there (Security > SSH keys) first, and name it with --ssh-key"
}

# ensure_firewall -- memql-media, created when missing, its selector put back when gone.
function ensure_firewall() {
    hcloud_ok GET "/firewalls?name=${FIREWALL_NAME}" 200
    if [[ "$(hc_field '.firewalls | length')" -eq 0 ]]; then
        cap_info "creating the firewall ${FIREWALL_NAME}..."
        hcloud_ok POST "/firewalls" 201 "$(firewall_body)"
        FIREWALL_ID="$(hc_field '.firewall.id')"
        [[ "$FIREWALL_ID" =~ ^[0-9]+$ ]] || cap_fail 5 "Hetzner's answer about the new firewall carries no id"
        FIREWALL_CREATED=true
        cap_changed
        return 0
    fi
    FIREWALL_ID="$(hc_field '.firewalls[0].id')"
    [[ "$FIREWALL_ID" =~ ^[0-9]+$ ]] || cap_fail 5 "Hetzner's answer about the firewall ${FIREWALL_NAME} carries no id"
    [[ "$(hc_field ".firewalls[0].labels | has(\"${MEDIA_LABEL}\")")" == "true" ]] \
        || cap_fail 3 "the project already has a firewall named ${FIREWALL_NAME} that this script did not make (it has no ${MEDIA_LABEL} label); rename it, or label it ${MEDIA_LABEL} if it is the one media servers should use"
    local applied
    applied="$(hc_field "[.firewalls[0].applied_to[]? | select(.type == \"label_selector\" and .label_selector.selector == \"${MEDIA_LABEL}\")] | length")"
    if [[ "$applied" -eq 0 ]]; then
        cap_info "applying the firewall ${FIREWALL_NAME} to every server labelled ${MEDIA_LABEL} again..."
        hcloud_ok POST "/firewalls/${FIREWALL_ID}/actions/apply_to_resources" 201 "{\"apply_to\":$(selector_json)}"
        cap_changed
    fi
}

# wait_for_action <id> -- a Hetzner action followed to success or error, for up to five
# minutes.
function wait_for_action() {
    local id="$1" status deadline=$((SECONDS + 300))
    while :; do
        hcloud_ok GET "/actions/${id}" 200
        status="$(hc_field '.action.status')"
        case "$status" in
            success) return 0 ;;
            error) cap_fail 5 "Hetzner could not create the server: $(hc_field '.action.error | "\(.message) (\(.code))"')" ;;
        esac
        [[ "$SECONDS" -lt "$deadline" ]] \
            || cap_fail 5 "Hetzner's create action ${id} had not finished after five minutes; the server ${NAME} (id ${SERVER_ID}) may still come up: run again to answer it"
        sleep 3
    done
}

# wait_for_firewall -- the label selector applies the firewall shortly after the server
# exists; a minute without it is reported, not fatal: ufw on the server holds the same
# rules from its first boot.
function wait_for_firewall() {
    local deadline=$((SECONDS + 60))
    while :; do
        hcloud_ok GET "/servers/${SERVER_ID}" 200
        if [[ "$(hc_field "[.server.public_net.firewalls[]? | select(.id == ${FIREWALL_ID} and .status == \"applied\")] | length")" -gt 0 ]]; then
            FIREWALL_APPLIED=true
            return 0
        fi
        if [[ "$SECONDS" -ge "$deadline" ]]; then
            cap_warn "the firewall ${FIREWALL_NAME} is not yet applied to ${NAME} after a minute; check it in the Hetzner console (the server's own firewall, ufw, holds the same rules)"
            return 0
        fi
        sleep 3
    done
}

function create_server() {
    require_ssh_key
    ensure_firewall
    cap_info "creating the media server ${NAME} (${SERVER_TYPE} in ${LOCATION})..."
    hcloud_ok POST "/servers" 201 "$(server_body)"
    record_addresses '.server'
    CREATED=true
    cap_changed
    wait_for_action "$(hc_field '.action.id')"
    wait_for_firewall
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function report_answer() {
    local dry=false
    if [[ -n "$DRY_RUN" ]]; then dry=true; fi
    cap_result_set_raw dryRun "$dry"
    cap_result_set     name "$NAME"
    cap_result_set     location "$LOCATION"
    cap_result_set     serverType "$SERVER_TYPE"
    cap_result_set     image "$IMAGE"
    cap_result_set     sshKey "$SSH_KEY"
    cap_result_set     firewall "$FIREWALL_NAME"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    NAME="$(cap_param name "")"
    LOCATION="$(cap_param location "")"
    SERVER_TYPE="$(cap_param server-type "")"
    IMAGE="$(cap_param image "ubuntu-24.04")"
    SSH_KEY="$(cap_param ssh-key "")"
    CONFIRM="$(cap_param confirm "")"
    if cap_bool dry-run; then DRY_RUN=1; else DRY_RUN=""; fi
    validate_params
    build_user_data

    if [[ -n "$DRY_RUN" ]]; then
        # The token is not needed, and is dropped from the environment all the same.
        unset HCLOUD_TOKEN
        cap_info "dry run: nothing is called. The cloud-init the server would boot with:"
        printf '%s\n' "$USER_DATA" >&2
        report_answer
        cap_result_set_raw requests "[{\"method\":\"POST\",\"url\":\"${HCLOUD_API}/firewalls\",\"body\":$(firewall_body)},{\"method\":\"POST\",\"url\":\"${HCLOUD_API}/servers\",\"body\":$(server_body)}]"
        cap_result_set_raw serverId null
        cap_result_set_raw ipv4 null
        cap_result_set_raw ipv6 null
        cap_ok
    fi

    cap_confirm_or_die "$CONFIRM" "provision-${NAME}"
    read_token
    require_tools

    if find_server; then
        ensure_firewall
        existing_server_firewall_applied
    else
        create_server
    fi

    report_answer
    cap_result_set_raw serverId "$SERVER_ID"
    cap_result_set     ipv4 "$IPV4"
    cap_result_set     ipv6 "$IPV6"
    cap_result_set     ipv6Network "$IPV6_NETWORK"
    cap_result_set_raw firewallId "$FIREWALL_ID"
    cap_result_set_raw firewallCreated "$FIREWALL_CREATED"
    cap_result_set_raw firewallApplied "$FIREWALL_APPLIED"
    cap_result_set_raw created "$CREATED"
    cap_info "media server ${NAME}: id ${SERVER_ID}, ${IPV4}, ${IPV6:-no IPv6}. Next: its DNS records (docs/public/operate/calls.md)."
    cap_ok
}

main "$@"
