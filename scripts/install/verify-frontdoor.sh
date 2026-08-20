#!/usr/bin/env bash
#
# scripts/install/verify-frontdoor.sh
# ===================================
#
# Capability: install.verifyFrontDoor -- prove the local front door works.
#
# The front door is the ONE connection path clients use in every environment
# (env parity: ingress -> TLS -> gRPC -> bff, dialed as https://api.<domain>).
# Locally it stands on four independent properties, each of which can be broken
# while the others look perfect:
#
#   dns         the hostname resolves to 127.0.0.1 -- and to NOTHING ELSE. A
#               hostname pointing at some other address is a worse failure than
#               one that does not resolve: the installer would hand traffic to a
#               stranger's box while every symptom points at the cluster.
#   tls         https://<host>/ completes a handshake against a TRUSTED
#               certificate (locally the mkcert wildcard; in the cloud
#               cert-manager).
#   grpc        the front door negotiates HTTP/2. gRPC cannot exist over
#               HTTP/1.1, so a door that answers but will not speak h2 is a
#               broken door.
#   precedence  an exact hostname is not SWALLOWED by the wildcard rule beside
#               it. The cluster front door (memql#3700) serves `*.<domain>` and
#               the apex from the site edge alongside the exact api. /
#               identity. / mcp. hosts, and the wildcard MATCHES those three
#               exact names as well. The whole five-host design rests on an
#               exact host outranking a wildcard (decision D3); if it does not,
#               api. and identity. are answered by the site edge and every
#               symptom is a 404 from a server nobody meant to dial.
#
#               WHAT THIS ESTABLISHES, AND FOR WHICH HOSTS -- api. and
#               identity., NOT all three exact names, and NOT a
#               wildcard-served name. The check identifies the answering node
#               from its /healthz body, so it can only establish the property
#               for a host that answers /healthz through the front door.
#               `mcp.`'s Ingress routes :8090, the MCP protocol port, while
#               /healthz is on :8085 (deploy/k8s/base/mcp.yaml), so probing
#               `mcp.` reports INCONCLUSIVE, never passed. And `--hosts` means
#               the hosts that carry their OWN exact rule: `portal.` and the
#               apex are served by the edge BY DESIGN, so there is no
#               precedence to establish for them (PROBE 4 says what happens if
#               they are passed anyway). Anything citing this check should
#               claim those two hosts and no more.
#
# Each check reports ITSELF -- name, host, passed, status, detail -- because
# "the front door is broken" is not actionable and "dns for
# identity.memql.localhost resolves to 10.0.0.5" is. The rollup `allPassed` is
# the single boolean the graph verifies on, and it means NO CHECK FAILED.
#
# THREE STATES, NOT TWO. A check can also report `status: "inconclusive"` -- it
# ran and could not establish its property either way. That is a real outcome
# here rather than a hedge: the precedence check cannot mean anything until a
# wildcard router is actually loaded (see PROBE 4), and reporting it as a pass
# in the meantime would be a false assurance of exactly the thing the check
# exists to establish. An inconclusive check counts as NEITHER passed nor
# failed: `passed` is false (it did not pass), `allPassed` and the exit code are
# untouched (it did not fail), and `inconclusiveCount` surfaces it in the
# rollup.
#
# EXIT CODES:
#
#   0  every check passed or was inconclusive (or --report-only was set)
#   2  bad param (no hosts)
#   4  prerequisite missing (no curl, or no resolver at all)
#   5  STRICT DEFAULT: at least one check failed
#
# --report-only turns the failing exit into 0. It does NOT change `allPassed`
# or any check: it waives strictness, it does not launder the truth.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-frontdoor.sh
#   scripts/install/verify-frontdoor.sh --hosts=api.memql.localhost --report-only
#   scripts/install/verify-frontdoor.sh --print-spec
#
# Refs: #3365 #3357 #2221 #3700

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/resolve.sh
source "${SCRIPT_DIR}/../lib/resolve.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

# The label PROBE 4 dials to find out whether a wildcard router is loaded at
# all. Deliberately synthetic and deliberately fixed: it has to be a name no
# EXACT rule can ever match, or the probe stops measuring the wildcard. A real
# name would look friendlier and be wrong -- portal.<domain> is served by the
# wildcard today and gets its own exact Ingress rule the moment someone decides
# it should, at which point this probe would quietly start measuring that rule
# instead. Nothing will ever claim this one, and its name tells an operator who
# finds it in a log exactly what it is.
WILDCARD_PROBE_LABEL="frontdoor-precedence-probe"

cap_init "install.verifyFrontDoor" \
    "Check dns/tls per front-door hostname, a gRPC (h2) reachability probe, and that an exact host outranks the wildcard rule."
cap_spec_param "hosts"       "comma-separated front-door hostnames (default: the local overlay's)"
cap_spec_param "domain"      "front-door apex; derives api.<d> and identity.<d> (mutually exclusive with --hosts)"
cap_spec_param "grpc-host"   "hostname for the gRPC reachability probe (default: the first host)"
cap_spec_param "port"        "TLS port (default 443)"
cap_spec_param "expect-addr" "the address every hostname must resolve to (default 127.0.0.1)"
cap_spec_param "timeout"     "per-probe timeout in seconds (default 8)"
cap_spec_param "report-only" "report failures without failing the run (flag)"
cap_spec_param "wildcard-probe-host" \
    "unclaimed hostname under the wildcard rule, proving exact-vs-wildcard precedence is testable (default: ${WILDCARD_PROBE_LABEL}.<apex of the first host>)"

# The local overlay's front door (deploy/k8s/overlays/local): traefik serves
# both on 443 with the mkcert *.memql.localhost wildcard.
# Derived from ONE apex (memql#3593), so the probe cannot name a different
# front door from the one the hosts block points at and the certificate covers.
# localtls.sh owns MEMQL_LOCAL_DOMAIN; sourcing it is what keeps the three in
# step. Overridden per run with --domain or --hosts.
DEFAULT_DOMAIN="$MEMQL_LOCAL_DOMAIN"
DEFAULT_HOSTS="api.${DEFAULT_DOMAIN},identity.${DEFAULT_DOMAIN}"

#=============================================================================
# CHECK LEDGER -- every probe records itself here: passed, failed, or
#                 inconclusive
#=============================================================================

_FD_CHECKS=()
_FD_PASSED=0
_FD_FAILED=0
_FD_INCONCLUSIVE=0

# record_check_status <name> <host> <passed|failed|inconclusive> <detail>
#
# WHY A THIRD STATE EXISTS AT ALL. A check that ran and could not establish its
# property is not a check that passed, and it is not a check that failed.
# Collapsing it into either is how a check stops meaning anything:
#
#   as a pass    it becomes a false assurance -- the worst possible outcome for
#                a verification, because the claim now reads as measured. PROBE
#                4 is exactly that case: until a wildcard router is loaded there
#                is nothing for an exact host to take precedence OVER, so "api.
#                reached the bff" proves nothing about precedence while looking
#                like proof.
#   as a failure it fails an install over a property the cluster is not yet in
#                a position to have, and the repair is to delete the check.
#
# THE WIRE SHAPE, AND WHY `passed` STAYS A PLAIN BOOLEAN. Consumers read
# `passed`; it keeps answering the question its name asks, and an inconclusive
# check did not pass, so it is false. It never claims a pass it did not
# measure. `status` is the field that separates "did not pass" from "failed",
# and an inconclusive check increments NEITHER counter -- so `allPassed` (no
# check failed) and the exit code are untouched, and `inconclusiveCount` carries
# the state up to the rollup. Additive for every consumer: the install graph
# verifies the `allPassed` rollup and nothing outside this script reads
# `checks[]` element-wise.
#
# MUST be called from the main shell (it appends to a global array), so no
# probe may run inside a "$(...)" substitution.
function record_check_status() {
    local name="$1" host="$2" status="$3" detail="$4" passed=false
    [[ "$status" == "passed" ]] && passed=true
    _FD_CHECKS+=("{\"name\":\"$(cap_json_escape "$name")\",\"host\":\"$(cap_json_escape "$host")\",\"passed\":${passed},\"status\":\"$(cap_json_escape "$status")\",\"detail\":\"$(cap_json_escape "$detail")\"}")
    case "$status" in
        passed)
            _FD_PASSED=$((_FD_PASSED + 1))
            cap_info "PASS ${name} ${host}: ${detail}"
            ;;
        failed)
            _FD_FAILED=$((_FD_FAILED + 1))
            cap_error "FAIL ${name} ${host}: ${detail}"
            ;;
        *)
            _FD_INCONCLUSIVE=$((_FD_INCONCLUSIVE + 1))
            # WARN, not INFO: an operator reading the log should see that
            # something went unproven rather than scroll past a green line.
            cap_warn "INCONCLUSIVE ${name} ${host}: ${detail}"
            ;;
    esac
}

# record_check <name> <host> <true|false> <detail>
# The boolean front door onto record_check_status, for every probe that has a
# yes/no answer. Unchanged for its callers.
function record_check() {
    local status=failed
    [[ "$3" == "true" ]] && status=passed
    record_check_status "$1" "$2" "$status" "$4"
}

function checks_json() {
    local IFS=,
    printf '[%s]' "${_FD_CHECKS[*]:-}"
}

#=============================================================================
# PREREQUISITES
#=============================================================================

# resolver_tool and resolve_addresses come from scripts/lib/resolve.sh, which
# the hosts-entries probe sources too (memql#3593). One copy, because the probe
# decides whether to write the entry this script then checks -- two spellings of
# "resolves to 127.0.0.1" is the memql#3384 shape.

function check_prerequisites() {
    if ! command -v curl &>/dev/null; then
        cap_fail 4 "curl is not installed; cannot probe the front door"
    fi
    if [[ -z "$(resolver_tool)" ]]; then
        cap_fail 4 "no resolver available (need one of: getent, dig, host)"
    fi
}

#=============================================================================
# PROBE 1 -- DNS
#=============================================================================

# check_dns <host> <expected-addr>
function check_dns() {
    local host="$1" expect="$2" addrs joined
    addrs="$(resolve_addresses "$host" || true)"
    joined="$(printf '%s' "$addrs" | tr '\n' ' ' | sed -e 's/  */ /g' -e 's/ $//')"

    if [[ -z "$joined" ]]; then
        record_check dns "$host" false "does not resolve (no A record and no hosts entry)"
        return
    fi
    # Strict on purpose: resolving ANYWHERE other than the expected address
    # sends installer traffic to a machine nobody vetted.
    local addr bad=""
    for addr in $joined; do
        [[ "$addr" == "$expect" ]] || bad+="${addr} "
    done
    if [[ -n "$bad" ]]; then
        record_check dns "$host" false "resolves to ${joined} -- must resolve to ${expect} only (offending: ${bad% })"
        return
    fi
    record_check dns "$host" true "resolves to ${expect}"
}

#=============================================================================
# PROBE 2 + 3 -- TLS and gRPC (h2), over the real HTTPS front door
#=============================================================================

# https_probe <host> <port> <timeout> -- sets _FD_RC / _FD_VERSION / _FD_CODE.
# Globals rather than a captured "$(...)" so a later cap_fail is never trapped
# in a subshell.
_FD_RC=0
_FD_VERSION=""
_FD_CODE=""
function https_probe() {
    local host="$1" port="$2" timeout="$3" out=""
    _FD_RC=0
    out="$(curl --silent --show-error \
                --http2 \
                --max-time "$timeout" \
                --output /dev/null \
                --write-out '%{http_version} %{http_code}' \
                "https://${host}:${port}/" 2>/dev/null)" || _FD_RC=$?
    _FD_VERSION="${out%% *}"
    _FD_CODE="${out##* }"
}

# curl_tls_hint <rc> -- turns curl's exit code into something an operator can act on.
function curl_tls_hint() {
    case "$1" in
        6)  printf 'could not resolve host' ;;
        7)  printf 'connection refused -- nothing is listening on the port' ;;
        28) printf 'timed out' ;;
        35) printf 'TLS handshake failed' ;;
        51) printf 'certificate hostname mismatch' ;;
        60) printf 'certificate not trusted -- run `mkcert -install` and re-issue the wildcard' ;;
        *)  printf 'transport failure' ;;
    esac
}

# check_tls <host> <port> <timeout>
function check_tls() {
    local host="$1" port="$2" timeout="$3"
    https_probe "$host" "$port" "$timeout"
    if [[ "$_FD_RC" != "0" ]]; then
        record_check tls "$host" false "curl exit ${_FD_RC}: $(curl_tls_hint "$_FD_RC")"
        return
    fi
    record_check tls "$host" true "TLS handshake ok against a trusted certificate (HTTP ${_FD_CODE})"
}

# check_grpc <host> <port> <timeout> -- gRPC requires HTTP/2 end to end, so the
# reachability signal is "the front door negotiated h2 and answered".
function check_grpc() {
    local host="$1" port="$2" timeout="$3"
    https_probe "$host" "$port" "$timeout"
    if [[ "$_FD_RC" != "0" ]]; then
        record_check grpc "$host" false "curl exit ${_FD_RC}: $(curl_tls_hint "$_FD_RC")"
        return
    fi
    case "$_FD_VERSION" in
        2|2.0)
            record_check grpc "$host" true "negotiated HTTP/2 (HTTP ${_FD_CODE}) -- gRPC can ride this door"
            ;;
        *)
            record_check grpc "$host" false "negotiated HTTP/${_FD_VERSION:-unknown}, not h2 -- gRPC cannot run over HTTP/1.1"
            ;;
    esac
}

#=============================================================================
# PROBE 4 -- WILDCARD VERSUS EXACT HOST PRECEDENCE
#=============================================================================
#
# THE PROPERTY. The cluster front door (memql#3700) is five hosts: the exact
# api. / identity. / mcp. names, plus `*.<domain>` and the apex, which both
# reach the site edge. The wildcard MATCHES the three exact names too, so every
# one of those doors depends on the Ingress rule that an exact host outranks a
# wildcard (decision D3). Neither this repository nor anything else was checking
# it, which is what this probe is for -- the manifest header claimed it was
# probed before it was.
#
# AND IT IS NOT OBVIOUSLY TRUE, WHICH IS THE POINT. traefik orders routers by
# RULE LENGTH unless a priority is set explicitly, and the wildcard's rule
# string -- `HostRegexp(...) && PathPrefix("/")` -- is LONGER than
# `Host("api.<domain>") && PathPrefix("/healthz")`. So the heuristic that
# actually decides this does not obviously favour the design the five-host
# table depends on. An assurance that "both controllers implement it" is what
# stood in for a measurement here before, and it is why the measurement was
# missing.
#
# WHY A NAIVE PROBE IS WORSE THAN NO PROBE. `svc/edge` need not exist yet, and
# an ingress controller DROPS A ROUTER WHOSE BACKEND SERVICE IS ABSENT rather
# than serving a 503 through it -- traefik says so in its own log:
#
#   ERR Cannot create service error="service not found" \
#       ingress=edge-front-door serviceName=edge servicePort=8085
#
# With the wildcard router not loaded, "api. reached the bff" is true and proves
# NOTHING: there was no competing route. A check that reported that as a pass
# could never fail, which is the false assurance this file exists to remove. So
# the probe ESTABLISHES A COMPETING ROUTE FIRST and reports inconclusive when it
# cannot (see record_check_status for what that costs and does not cost).
#
# THE DISCRIMINATOR: /healthz names the node that answered. The check DOES read
# a response body -- the health body, in both steps. What it cannot read is a
# 404, and the LIVENESS GATE is where that bites, because the two answers it has
# to tell apart are the ingress controller's 404 for a name no router matches and
# THE EDGE'S OWN 404 for a hostname it has no site row for
# (component/edge/handler.go, `site == nil` -> http.NotFound):
#
#   - not the status code. Both are 404.
#   - not the body of those two. Both are Go's http.NotFound, so both are the
#     same 19 bytes ("404 page not found"); traefik is itself a Go server. (A
#     404 against a 200 health body on a DIFFERENT host does discriminate --
#     that is the by-hand measurement -- but it is not the comparison the gate
#     has to make.)
#   - not TLS. The wildcard certificate is loaded from an Ingress's `tls` block
#     independently of whether the router referencing it survives backend
#     resolution, and the other front-door Ingresses load the same secret, so
#     the handshake succeeds against a trusted cert either way.
#
# So the gate cannot ask "did something answer" or "what did it say" -- only
# "did a MemQL node name itself".
#
# GET /healthz is positive identification instead of inference: the node that
# served the request NAMES ITSELF (`{"status":"ok","nodeId":...,"nodeType":...}`),
# which is the same evidence an operator uses by hand. It is public on every
# node type (server.HealthzPaths in PublicPaths) and registered as an exact
# `GET /healthz` mux pattern, so it wins over the edge's root site mount
# (app/transport_edge.go mounts the site handler at "/"). The path and the
# discriminator are ONE decision, which is why the path is not a parameter: any
# other path returns a body that names nobody.
#
# COMPARED, NOT HARDCODED. Nothing here looks for the string "edge". The probe
# asks the wildcard host WHICH node type serves the wildcard, then asks each
# exact host which node type serves IT, and fails when they are the same. A
# nodeId comparison would be wrong -- the edge may run more than one replica, so
# two different pod ids prove nothing -- while nodeType is replica-invariant and
# survives the node type being renamed.
#
# EVERY UNKNOWN LANDS ON INCONCLUSIVE, never on a pass. If /healthz stops being
# reachable through a host, or a node stops naming itself, this check goes
# quiet rather than confident.
#
# WHAT IS MEASURED, EXACTLY -- one request per host, to /healthz. An ingress
# controller resolves precedence per (host, path) RULE, not per host, so a
# cluster could in principle yield /healthz to the exact rule while `/` went to
# the wildcard. The reported details therefore name the route they measured
# rather than claiming more than one request can support. The whole-host case is
# what the five-host design turns on and is the one this catches; a per-path
# split cannot be caught by a second probe of `/`, because the only response
# that names its author on this front door is the health body.
#
# WHICH HOSTS BELONG IN `--hosts`: THE ONES THAT CARRY THEIR OWN EXACT RULE.
# This check's question -- "is a node other than the wildcard's answering?" --
# is only meaningful for such a host. `portal.<domain>` and the apex are served
# by the edge BY DESIGN (the apex has its own rule pointing at the same Service
# the wildcard does), so "the edge answered" is the correct outcome for them,
# not a defect. The apex is detected here and reported inconclusive with that
# reason, because it is cheap and exact -- a host equal to the wildcard's apex
# cannot be anything else.
#
# `portal.` and any future wildcard-served label are NOT detected, because
# deciding that from a name would mean keeping a list of which labels are exact,
# and that list would be wrong in one direction or the other on the day it
# changed -- memql#3711 gives `portal.` a rule THROUGH THE WILDCARD, which is
# precisely the case a name list would misread. So it is stated rather than
# guessed: a wildcard-served name passed in `--hosts` will be reported FAILED
# with a detail that is backwards, and the fix is to not pass it.
# editors/vscode/src/install/session.ts's PROBE_SUBDOMAINS is the live example
# of the right set (api. + identity.); its sibling HOSTS_BLOCK_SUBDOMAINS
# carries `portal.` and the apex because a hosts file has no wildcard, and those
# two lists are deliberately not the same list.

# json_string_field <json> <key> -- shallow read of a top-level string field.
#
# ONE implementation on purpose. capability.sh's _cap_json_field has a jq tier
# and a grep tier; that is right for a params object an executor hands in, and
# wrong here -- a check whose verdict could differ between a machine with jq and
# a machine without it is a check nobody can reason about. The body being read
# is a flat object emitted by component/server/health.go, which grep handles
# exactly. (It is also the params reader, and a probe response is not params;
# borrowing a private helper across that line is how the two would drift.)
function json_string_field() {
    local json="$1" key="$2"
    printf '%s' "$json" \
        | grep -oE "\"${key}\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
        | head -1 \
        | sed -E "s/\"${key}\"[[:space:]]*:[[:space:]]*\"//; s/\"$//"
}

# healthz_probe <host> <port> <timeout> <pin-addr-or-empty>
# Sets _FD_HZ_RC / _FD_HZ_CODE / _FD_HZ_NODETYPE / _FD_HZ_NODEID. Globals rather
# than a captured "$(...)" for the same reason https_probe uses them: a later
# cap_fail must not be trapped in a subshell.
_FD_HZ_RC=0
_FD_HZ_CODE=""
_FD_HZ_NODETYPE=""
_FD_HZ_NODEID=""
_FD_HZ_CONTENT_TYPE=""
function healthz_probe() {
    local host="$1" port="$2" timeout="$3" addr="$4" out="" body="" tail=""
    _FD_HZ_RC=0
    _FD_HZ_CODE=""
    _FD_HZ_NODETYPE=""
    _FD_HZ_NODEID=""
    _FD_HZ_CONTENT_TYPE=""

    # --resolve pins the address WITHOUT going through the resolver, and only
    # the caller that asks for it gets it. The wildcard probe host needs it: a
    # hosts file has no wildcard semantics, so a synthetic label under
    # `*.<domain>` has no entry and never will -- requiring it to resolve would
    # make this probe permanently inconclusive for a reason that has nothing to
    # do with routing, which is what it is measuring. The real hosts are pinned
    # to nothing, deliberately: their resolution is PROBE 1's assertion and must
    # not be bypassed by the probe that follows it.
    local -a pin=()
    [[ -n "$addr" ]] && pin=(--resolve "${host}:${port}:${addr}")

    # The body is needed, so no --output /dev/null here; the trailing newline in
    # --write-out separates it from the status code (a body may itself contain
    # newlines, so the code is read back from the LAST one).
    # ${pin[@]+...} guards the empty-array expansion under `set -u` on bash 3.2.
    #
    # CONTENT TYPE IS READ TOO (memql#3814), on the same trailing line so the
    # last-newline parse above is untouched. It is what lets this probe tell
    # "the response identified nobody" from "the response identified the WRONG
    # backend": an h2c gRPC server answering an HTTP/1.1 request emits
    # `application/grpc` and nothing else does. See check_precedence.
    out="$(curl --silent --show-error \
                --http2 \
                --max-time "$timeout" \
                ${pin[@]+"${pin[@]}"} \
                --write-out '\n%{http_code} %{content_type}' \
                "https://${host}:${port}/healthz" 2>/dev/null)" || _FD_HZ_RC=$?
    [[ "$_FD_HZ_RC" == "0" ]] || return 0

    tail="${out##*$'\n'}"
    _FD_HZ_CODE="${tail%% *}"
    # No space means no content_type was written (an older curl, or a response
    # that carried no Content-Type header); empty is the honest answer and the
    # caller treats it as "not established" rather than as "not grpc".
    [[ "$tail" == *" "* ]] && _FD_HZ_CONTENT_TYPE="${tail#* }"
    body="${out%$'\n'*}"
    # `|| true`: no match is a legitimate answer (the response named no node),
    # and grep's exit 1 under `set -e` would abort the run instead.
    _FD_HZ_NODETYPE="$(json_string_field "$body" nodeType || true)"
    _FD_HZ_NODEID="$(json_string_field "$body" nodeId || true)"
}

# wildcard_apex <host> -- the domain a `*.<domain>` rule covers for <host>: the
# host with its first label removed. Empty when the result could not be a domain
# (`example.com` would yield a bare TLD), because dialing a synthetic label
# under a guessed apex is worse than admitting the apex is unknown.
#
# It answers for a SUBDOMAIN. Handed the apex itself it returns nothing, which is
# correct for one host and wrong for a host SET -- see resolve_wildcard_apex.
function wildcard_apex() {
    local host="$1" apex="${1#*.}"
    [[ "$apex" != "$host" ]] || return 0   # no label to strip
    [[ "$apex" == *.* ]]     || return 0   # would be a bare TLD
    printf '%s' "$apex"
}

# resolve_wildcard_apex <host>... -- the domain a `*.<domain>` rule covers for a
# whole host set.
#
# THE APEX IS A PROPERTY OF THE SET, NOT OF host_list[0]. This existed as a bare
# `wildcard_apex "${host_list[0]}"` and made the answer depend on ARGUMENT ORDER:
# `--hosts=<apex>,api.<apex>` derived nothing (the apex has no label to strip), so
# no probe host was built and EVERY host fell into the "cannot derive the wildcard
# apex" branch -- including `api.` and `identity.`, which are perfectly testable.
# Latent (no default host set names the apex, and frontDoorFor() lists it last on
# purpose) and invisible from the call site, which is the argument for the apex
# being resolved here rather than at one index.
#
# So every member gets a say: the first host that yields an apex wins. A set where
# NO host yields one is a set of bare apexes -- `memql.localhost` alone is the
# apex, not a subdomain of `localhost`, because no `*.localhost` front door exists
# -- so the first host that could BE a domain is itself the answer.
#
# "Could be a domain" means it has a dot, and that condition is load-bearing
# rather than defensive. A single-label `localhost` yields nothing here on
# purpose: calling it the apex would assert a `*.localhost` wildcard the operator
# never named, and the honest answer for a name with no parent domain is the
# "cannot derive the wildcard apex" report plus the --wildcard-probe-host way out.
# Recognising the apex must not become guessing at one.
function resolve_wildcard_apex() {
    local host derived
    for host in "$@"; do
        [[ -z "$host" ]] && continue
        derived="$(wildcard_apex "$host")"
        if [[ -n "$derived" ]]; then
            printf '%s' "$derived"
            return
        fi
    done
    for host in "$@"; do
        [[ -z "$host" || "$host" != *.* ]] && continue
        printf '%s' "$host"
        return
    done
}

# check_precedence <probe-host-or-empty> <apex-or-empty> <port> <timeout> <pin-addr> <host>...
#
# <apex> is the domain the wildcard covers, and it is passed for ONE reason: a
# host equal to it is the apex, which is edge-served by design and therefore has
# no precedence to establish. See "WHICH HOSTS BELONG IN --hosts" above for why
# that is the only such name detected here rather than a list of labels.
function check_precedence() {
    local probe_host="$1" apex="$2" port="$3" timeout="$4" addr="$5"
    shift 5
    local host

    # The apex is answered by the edge BY DESIGN, so it is settled before
    # anything is dialled: no liveness gate can make "the edge answered" wrong
    # for this host, and the reason it carries is permanent rather than a fact
    # about today's cluster.
    local -a testable=()
    for host in "$@"; do
        [[ -z "$host" ]] && continue
        if [[ -n "$apex" && "$host" == "$apex" ]]; then
            record_check_status precedence "$host" inconclusive \
                "this is the front door's apex, which the edge serves BY DESIGN (its own rule pointing at the same Service the wildcard rule does), so there is no exact-versus-wildcard precedence to establish for it -- --hosts is for hosts that carry their own exact rule to a backend of their own"
            continue
        fi
        testable+=("$host")
    done
    if [[ ${#testable[@]} -eq 0 ]]; then
        # Nothing left to establish precedence FOR, so nothing is dialled.
        return
    fi

    if [[ -z "$probe_host" ]]; then
        for host in "${testable[@]}"; do
            record_check_status precedence "$host" inconclusive \
                "cannot derive the wildcard apex from the probed hostnames; pass --wildcard-probe-host=<a name no exact rule matches> to make exact-versus-wildcard precedence testable"
        done
        return
    fi

    # STEP 1 -- is a wildcard router loaded at all? Without one there is no
    # competing route, so precedence is not a thing this cluster can get wrong
    # yet, and saying so is the honest report.
    cap_step "checking wildcard-versus-exact precedence via ${probe_host}"
    healthz_probe "$probe_host" "$port" "$timeout" "$addr"

    local why=""
    if [[ "$_FD_HZ_RC" != "0" ]]; then
        why="the unclaimed wildcard name ${probe_host} did not answer (curl exit ${_FD_HZ_RC}: $(curl_tls_hint "$_FD_HZ_RC")), so the wildcard router is not loaded and there is nothing for an exact host to take precedence over"
    elif [[ -z "$_FD_HZ_NODETYPE" ]]; then
        why="the unclaimed wildcard name ${probe_host} answered HTTP ${_FD_HZ_CODE} without naming a MemQL node -- which is what the ingress controller's own default backend returns when no router matches -- so the wildcard router is not loaded (an absent backend Service drops the whole router) and there is nothing for an exact host to take precedence over"
    fi
    if [[ -n "$why" ]]; then
        for host in "${testable[@]}"; do
            record_check_status precedence "$host" inconclusive "$why"
        done
        return
    fi

    local wildcard_type="$_FD_HZ_NODETYPE"
    cap_info "wildcard router is live: ${probe_host} is served by nodeType=${wildcard_type}${_FD_HZ_NODEID:+ (${_FD_HZ_NODEID})} -- precedence is genuinely testable"

    # STEP 2 -- with a competing route proven live, every exact host must be
    # answered by something OTHER than whatever serves the wildcard.
    for host in "${testable[@]}"; do
        healthz_probe "$host" "$port" "$timeout" ""
        local who=""
        [[ -n "$_FD_HZ_NODEID" ]] && who=" (${_FD_HZ_NODEID})"
        if [[ "$_FD_HZ_RC" != "0" ]]; then
            record_check_status precedence "$host" inconclusive \
                "the wildcard router is live (nodeType=${wildcard_type}) but ${host} did not answer (curl exit ${_FD_HZ_RC}: $(curl_tls_hint "$_FD_HZ_RC")), so which backend serves it cannot be established"
        elif [[ -z "$_FD_HZ_NODETYPE" && "$_FD_HZ_CONTENT_TYPE" == application/grpc* ]]; then
            # INDIRECT IS NOT INSUFFICIENT (memql#3814).
            #
            # This response names no node, so the branch below would have
            # called it inconclusive -- and did, for the whole life of
            # memql#3810, while every HTTP path on api. was being answered by
            # the gRPC backend. The check was looking straight at the defect
            # and declining to use what it could see.
            #
            # `application/grpc` is a FINGERPRINT, not an inference. /healthz
            # is declared to the HTTP Service (bff-http:8085); only an h2c gRPC
            # server produces this content type, and it produces it precisely
            # because it was handed an HTTP/1.1 request it cannot parse. No
            # correct configuration yields this response at this path, so the
            # responder has identified itself as the wrong backend without
            # naming itself.
            #
            # The rule this encodes: report inconclusive when the evidence is
            # INSUFFICIENT, never when it is merely INDIRECT. An
            # honest-uncertainty verdict that fires on sufficient-but-indirect
            # evidence becomes its own way of not looking.
            record_check_status precedence "$host" failed \
                "${host} is answered by a gRPC backend on a path declared to the HTTP one: HTTP ${_FD_HZ_CODE} with Content-Type ${_FD_HZ_CONTENT_TYPE}. /healthz routes to the bff's HTTP Service, and only an h2c gRPC server answers an HTTP/1.1 request this way -- so an HTTP path is falling through to the gRPC catch-all rule. This is memql#3703's failure mode (a protocol error naming nothing, not a 404) and memql#3810 is the worked example: an Ingress-level router.priority flattened the path ordering so \`/\` outranked the 21 specific paths. Check that no multi-path Ingress carries a uniform traefik.ingress.kubernetes.io/router.priority (deploy/k8s/overlays/local/render_priority_test.go gates this)"
        elif [[ -z "$_FD_HZ_NODETYPE" ]]; then
            record_check_status precedence "$host" inconclusive \
                "the wildcard router is live (nodeType=${wildcard_type}) but ${host} answered HTTP ${_FD_HZ_CODE} without naming a MemQL node, so which backend serves it cannot be established"
        elif [[ "$_FD_HZ_NODETYPE" == "$wildcard_type" ]]; then
            # The remedy belongs in the detail, the way curl_tls_hint puts
            # `mkcert -install` there. BOTH branches are named because this
            # check cannot tell them apart: "the edge answered" is a defect for
            # a host with its own backend and the intended behaviour for a
            # wildcard-served name, and the response is identical either way.
            # Naming only the first would send an operator to add a priority
            # annotation for `portal.` -- a repair for a problem they do not
            # have.
            record_check_status precedence "$host" failed \
                "/healthz answered by nodeType=${_FD_HZ_NODETYPE}${who} -- the same node type that serves the wildcard name ${probe_host}, so this host is not getting a backend of its own. If it is SUPPOSED to have one, the wildcard rule outranked its exact rule: pin the exact router above the wildcard with traefik.ingress.kubernetes.io/router.priority (traefik orders routers by rule length unless told otherwise). If it is a wildcard-served name (portal., the apex), it does not belong in --hosts"
        else
            record_check_status precedence "$host" passed \
                "/healthz answered by nodeType=${_FD_HZ_NODETYPE}${who}, not by the wildcard's nodeType=${wildcard_type} -- the exact host rule takes precedence"
        fi
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local hosts grpc_host port expect timeout report_only
    local domain
    # --domain and --hosts are two spellings of one answer, the same rule
    # hosts-entries.sh and mkcert-setup.sh apply.
    domain="$(cap_param domain "")"
    hosts="$(cap_param hosts "")"
    if [[ -n "$domain" && -n "$hosts" ]]; then
        cap_fail 2 "--domain and --hosts are two spellings of one answer; pass one"
    fi
    if [[ -n "$domain" ]]; then
        hosts="api.${domain},identity.${domain}"
    elif [[ -z "$hosts" ]]; then
        hosts="$DEFAULT_HOSTS"
    fi
    port="$(cap_param port 443)"
    expect="$(cap_param expect-addr 127.0.0.1)"
    timeout="$(cap_param timeout 8)"
    report_only="$(cap_flag report-only)"
    cap_require hosts "$hosts"

    local -a host_list=()
    IFS=',' read -ra host_list <<< "$hosts"
    if [[ ${#host_list[@]} -eq 0 || -z "${host_list[0]}" ]]; then
        cap_fail 2 "no hostnames to check"
    fi
    grpc_host="$(cap_param grpc-host "${host_list[0]}")"

    # The wildcard probe host (PROBE 4). --domain IS the apex when it was given;
    # otherwise it is resolved from the probed host SET -- not from one index, so
    # the answer cannot depend on which host was typed first (see
    # resolve_wildcard_apex). The relation is the one the wildcard rule itself
    # expresses: `*.<apex>` covers api.<apex>, so stripping api. off gets back to
    # the apex the wildcard claims. An explicit --wildcard-probe-host wins over
    # both.
    local wildcard_probe_host apex
    apex="$domain"
    [[ -n "$apex" ]] || apex="$(resolve_wildcard_apex "${host_list[@]}")"
    wildcard_probe_host="$(cap_param wildcard-probe-host "")"
    if [[ -z "$wildcard_probe_host" && -n "$apex" ]]; then
        wildcard_probe_host="${WILDCARD_PROBE_LABEL}.${apex}"
    fi

    check_prerequisites

    local host
    for host in "${host_list[@]}"; do
        [[ -z "$host" ]] && continue
        cap_step "checking ${host}"
        check_dns "$host" "$expect"
        check_tls "$host" "$port" "$timeout"
    done
    cap_step "checking gRPC reachability on ${grpc_host}"
    check_grpc "$grpc_host" "$port" "$timeout"
    check_precedence "$wildcard_probe_host" "$apex" "$port" "$timeout" "$expect" "${host_list[@]}"

    # allPassed is "no check FAILED". An inconclusive check is deliberately not
    # in either counter -- see record_check_status.
    local all_passed=true
    [[ "$_FD_FAILED" -eq 0 ]] || all_passed=false

    cap_result_set     hosts             "$hosts"
    cap_result_set     grpcHost          "$grpc_host"
    cap_result_set     wildcardProbeHost "$wildcard_probe_host"
    cap_result_set_raw allPassed         "$all_passed"
    cap_result_set_raw passedCount       "$_FD_PASSED"
    cap_result_set_raw failedCount       "$_FD_FAILED"
    cap_result_set_raw inconclusiveCount "$_FD_INCONCLUSIVE"
    cap_result_set_raw reportOnly        "$( [[ -n "$report_only" ]] && echo true || echo false )"
    cap_result_set_raw checks            "$(checks_json)"

    if [[ "$all_passed" == "true" ]]; then
        # Never "all checks passed" when some did not: the count of what was
        # actually established is the honest headline.
        if [[ "$_FD_INCONCLUSIVE" -gt 0 ]]; then
            cap_warn "Front door healthy: ${_FD_PASSED} check(s) passed, ${_FD_INCONCLUSIVE} inconclusive (nothing failed) -- see the details for what went unproven."
        else
            cap_info "Front door healthy: ${_FD_PASSED}/${_FD_PASSED} checks passed."
        fi
        cap_ok
    fi
    if [[ -n "$report_only" ]]; then
        cap_warn "${_FD_FAILED} check(s) failed; --report-only set, exiting 0 anyway."
        cap_ok
    fi
    cap_fail 5 "${_FD_FAILED} of $((_FD_PASSED + _FD_FAILED)) front-door checks failed"
}

main "$@"
