#!/usr/bin/env bash
#
# scripts/deploy/bind-account-front-door.sh
# =========================================
#
# Capability: frontdoor.bind -- apply the one Certificate and four Ingresses
# that serve a client's reserved MemQL name.
#
# Backend for the account front-door reconciler's script substrate (epic
# memql#5168, design D1/E), and the operator's manual path for the same job.
#
# WHAT IT APPLIES, AND WHY IT IS FOUR INGRESSES AND NOT THREE. A reserved name
# serves three hosts -- app. to the edge, api. to the engine, id. to identity --
# but api. needs TWO Ingress objects, for the same reason the cluster's own
# front door has two: an ingress controller's backend protocol is a
# per-SERVICE setting, so the bff's gRPC edge (h2c, :50051) and its HTTP edge
# (:8085) cannot share one object. The HTTP paths go in one, the `/` catch-all
# to the gRPC backend in the other.
#
# THE gRPC OBJECT CARRIES TWO RULES, AND THE FIRST IS NOT THE BFF'S (epic
# memql#5218, D10). WorkerService.Stream -- the cockpit's stream -- is served by
# the agent node and by nothing else, and gRPC puts the fully qualified service
# name in the request path. The cluster's own api host routes that prefix to
# the agent above its catch-all, and a door has to route it the same way, or a
# cockpit told to dial a client's api. host is answered `Unimplemented: unknown
# service` by the bff and never registers. --workerServicePath defaults to the
# prefix component/frontdoor.WorkerServicePath names, and
# account_front_door_test.go pins the two spellings against each other.
#
# THE HTTP PATH LIST IS PASSED IN, NOT KNOWN HERE. --apiPaths carries the
# comma-separated list that component/frontdoor/paths.generated.go holds, which
# cmd/frontdoorpaths writes from the SAME collect() that fills the cluster's own
# api Ingress. A list hardcoded here would be a second source, and its failure
# mode is the one that names nothing: a path the bff serves and no rule routes
# is an HTTP/1.1 request handed to an h2c backend, which fails without
# mentioning the path, the host or the generator.
#
# ONE CERTIFICATE WITH THREE SANs, AND THAT IS THE ACTIVATION RULE (design D8).
# An HTTP-01 order cannot go Ready unless every dnsName in it solves, so the
# certificate IS the all-or-nothing check rather than a rule the reconciler
# would have to police: three names, one order, one rate-limit unit against
# Let's Encrypt, one Ready condition to promote on. No wildcard appears -- these
# are exact hosts, which is precisely what HTTP-01 can issue (memql#4224).
#
# IDEMPOTENT BY CONSTRUCTION. Every object goes through `kubectl apply`, so a
# second run over unchanged objects is a no-op at the API server and creates no
# new ACME order: cert-manager's own backoff, not the caller's loop, remains
# what paces Let's Encrypt. `changed` reports whether anything actually moved.
#
# WHAT IT REFUSES. With no --issuer this cluster has no ACME issuer, and a
# Certificate with an empty issuerRef is ACCEPTED by the API server and then
# sits Pending forever with a condition nobody reads. That is a pretend
# success, and it is what the local k3d target would get every time. So it
# exits 3 naming `no_acme_issuer`, the reconciler writes that on the row, and
# the Accounts rail says so in as many words -- the same flow shape everywhere,
# honest about what the target can do (custom domains D7).
#
# Refs: memql#5168 memql#4805 memql#4224 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "frontdoor.bind" \
    "Apply the Certificate and four Ingresses that serve an account's reserved MemQL name, and report whether the certificate is Ready."

cap_spec_param_required "accountId"       "the v1:accounts:account row id -- every object is named after it"
cap_spec_param_required "reservedName"    "the account's reserved MemQL name, e.g. memql.acme.com; the three hosts are app./api./id. beneath it"
cap_spec_param          "doorId"          "the v1:platform:accountFrontDoor row id; recorded as a label"
cap_spec_param          "namespace"       "namespace the objects live in (default: memql)"
cap_spec_param          "issuer"          "cert-manager ClusterIssuer name; EMPTY refuses with no_acme_issuer rather than applying a Certificate nothing will fulfil"
cap_spec_param          "ingressClass"    "ingressClassName for every Ingress (default: nginx)"
cap_spec_param          "apiPaths"        "comma-separated HTTP paths routed to the bff's HTTP edge, from component/frontdoor/paths.generated.go; empty applies the gRPC rule only"
cap_spec_param          "edgeService"     "backend Service for app. (default: edge)"
cap_spec_param          "edgePort"        "backend Service port for app. (default: 8085)"
cap_spec_param          "bffHttpService"  "backend Service for api.'s HTTP paths (default: bff-http)"
cap_spec_param          "bffHttpPort"     "backend Service port for api.'s HTTP paths (default: 8085)"
cap_spec_param          "bffGrpcService"  "backend Service for api.'s h2c catch-all (default: bff)"
cap_spec_param          "bffGrpcPort"     "backend Service port for api.'s h2c catch-all (default: 50051)"
cap_spec_param          "workerServicePath" "the gRPC service prefix routed to the agent ahead of the catch-all -- the cockpit's WorkerService.Stream (default: component/frontdoor.WorkerServicePath)"
cap_spec_param          "agentGrpcService" "backend Service for api.'s worker-stream prefix (default: agent)"
cap_spec_param          "agentGrpcPort"   "backend Service port for api.'s worker-stream prefix (default: 50051)"
cap_spec_param          "identityService" "backend Service for id. (default: identity)"
cap_spec_param          "identityPort"    "backend Service port for id. (default: 8085)"
cap_spec_param          "waitSeconds"     "how long to wait for the certificate to become Ready before reporting not-ready (default: 15)"
cap_spec_param          "dryRun"          "render and validate the objects without applying them"
cap_spec_param          "renderTo"        "also write the rendered manifest to this file, for review with kubectl diff or for a test to parse"

cap_handle_meta "$@"
cap_parse_flags "$@"

ACCOUNT_ID="$(cap_param accountId "")"
RESERVED_NAME="$(cap_param reservedName "")"
DOOR_ID="$(cap_param doorId "")"
NAMESPACE="$(cap_param namespace "memql")"
ISSUER="$(cap_param issuer "")"
INGRESS_CLASS="$(cap_param ingressClass "nginx")"
API_PATHS="$(cap_param apiPaths "")"
EDGE_SERVICE="$(cap_param edgeService "edge")"
EDGE_PORT="$(cap_param edgePort "8085")"
BFF_HTTP_SERVICE="$(cap_param bffHttpService "bff-http")"
BFF_HTTP_PORT="$(cap_param bffHttpPort "8085")"
BFF_GRPC_SERVICE="$(cap_param bffGrpcService "bff")"
BFF_GRPC_PORT="$(cap_param bffGrpcPort "50051")"
# THE DEFAULT IS A SECOND SPELLING OF component/frontdoor.WorkerServicePath,
# and account_front_door_test.go reads it out of this file and holds the two
# equal -- the discipline the three host labels are already under
# (TestTheScriptsUseTheSameThreeLabelsAsFrontdoor). The engine passes the
# constant explicitly; the default exists for the operator's manual path, which
# this script's header says it also is.
WORKER_SERVICE_PATH="$(cap_param workerServicePath "/znasllc.memql.worker.v1.WorkerService/")"
AGENT_GRPC_SERVICE="$(cap_param agentGrpcService "agent")"
AGENT_GRPC_PORT="$(cap_param agentGrpcPort "50051")"
IDENTITY_SERVICE="$(cap_param identityService "identity")"
IDENTITY_PORT="$(cap_param identityPort "8085")"
WAIT_SECONDS="$(cap_param waitSeconds "15")"
DRY_RUN="$(cap_bool_str dryRun false)"
RENDER_TO="$(cap_param renderTo "")"

# FIELD_MANAGER is this feature's server-side-apply owner, and it is
# DELIBERATELY DISTINCT from the custom-domain reconciler's. Server-side apply
# tracks ownership per manager, so sharing one would make `kubectl get -o yaml`
# unable to answer the single question managed-fields exists for: which of the
# two reconcilers owns this field. It must equal doorFieldManager in
# integrations/customdomain/accountdoor_provision.go, and
# account_front_door_test.go pins the pair.
readonly FIELD_MANAGER="memql-account-front-door"

# Matches the cluster's own api Ingress and doorProxyBodySize in
# integrations/customdomain/accountdoor_provision.go. A door whose upload cap
# differs from the cluster's own api host is a client discovering that the same
# call works at one address and 413s at another.
readonly PROXY_BODY_SIZE="48m"

OBJECT_NAME=""
APPLIED=false
APP_HOST=""
API_HOST=""
ID_HOST=""
API_PATH_COUNT=0
DOCUMENT_COUNT=0
CHANGED_ANY=false
CERT_READY=false
CERT_STATUS=""

# ---------------------------------------------------------------------------
# Parameters
# ---------------------------------------------------------------------------
function check_params() {
    [[ -n "$ACCOUNT_ID" ]] \
        || cap_fail 2 "--accountId is required: every object is named after it, so there is nothing to apply without one"
    [[ -n "$RESERVED_NAME" ]] \
        || cap_fail 2 "--reservedName is required: it is the name the three hosts are composed beneath"
    if [[ "$RESERVED_NAME" == *"*"* ]]; then
        cap_fail 2 "--reservedName ${RESERVED_NAME} is a wildcard. ACME cannot issue a wildcard over HTTP-01, and one wildcard dnsName fails the whole order (memql#4224)."
    fi
    [[ "$RESERVED_NAME" == *.* ]] \
        || cap_fail 2 "--reservedName ${RESERVED_NAME} is a single label, not a domain"
    for pair in "edgePort:${EDGE_PORT}" "bffHttpPort:${BFF_HTTP_PORT}" \
                "bffGrpcPort:${BFF_GRPC_PORT}" "agentGrpcPort:${AGENT_GRPC_PORT}" \
                "identityPort:${IDENTITY_PORT}" "waitSeconds:${WAIT_SECONDS}"; do
        [[ "${pair#*:}" =~ ^[0-9]+$ ]] || cap_fail 2 "--${pair%%:*} ${pair#*:} is not a number"
    done
    [[ "$WAIT_SECONDS" -gt 0 ]] \
        || cap_fail 2 "--waitSeconds must be at least 1; kubectl reads --timeout=0s as 'check once', which reports every certificate not Ready"

    # EVERY VALUE THAT REACHES THE RENDER IS SHAPE-CHECKED HERE, and this is
    # not defence in depth -- it is the ONLY check on the apply path.
    #
    # render_objects interpolates these into YAML. A value carrying a newline
    # injects whatever follows it as sibling YAML, and the highest-value target
    # is an Ingress annotation: an `--issuer` containing a newline and
    # `nginx.ingress.kubernetes.io/server-snippet: "return 302 ..."` renders
    # arbitrary nginx configuration on that server block, which is total
    # request interception for the host.
    #
    # The engine path constrains reservedName through createAccountFrontDoor's
    # @pattern, but this script's own header says it is ALSO the operator's
    # manual path -- and an operator pastes values. So the check lives here,
    # where both paths pass.
    #
    # Deliberately allow-list shapes rather than reject characters: a deny list
    # is a list of the injections somebody thought of.
    local label value
    for pair in "reservedName:${RESERVED_NAME}"; do
        label="${pair%%:*}"; value="${pair#*:}"
        [[ "$value" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] \
            || cap_fail 2 "--${label} ${value} is not a hostname: lowercase letters, digits, dots and hyphens only, starting and ending alphanumeric"
    done
    for pair in "accountId:${ACCOUNT_ID}" "doorId:${DOOR_ID}"; do
        label="${pair%%:*}"; value="${pair#*:}"
        [[ -z "$value" || "$value" =~ ^[A-Za-z0-9:_.-]+$ ]] \
            || cap_fail 2 "--${label} carries a character that is not legal in a row id (letters, digits, and : _ . -)"
    done
    for pair in "namespace:${NAMESPACE}" "ingressClass:${INGRESS_CLASS}" \
                "issuer:${ISSUER}" "edgeService:${EDGE_SERVICE}" \
                "bffHttpService:${BFF_HTTP_SERVICE}" "bffGrpcService:${BFF_GRPC_SERVICE}" \
                "agentGrpcService:${AGENT_GRPC_SERVICE}" "identityService:${IDENTITY_SERVICE}"; do
        label="${pair%%:*}"; value="${pair#*:}"
        [[ -z "$value" || "$value" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] \
            || cap_fail 2 "--${label} ${value} is not a Kubernetes name: lowercase letters, digits and hyphens only, starting and ending alphanumeric"
    done

    # A gRPC service prefix is `/<package>.<Service>/`: an absolute path of
    # proto identifier characters, ending in the slash that stops a prefix rule
    # matching a service whose name merely begins with this one. Refused rather
    # than defaulted when it is anything else -- an empty value would render a
    # rule with no path, and a bare `/` would shadow the bff's catch-all with
    # the agent.
    [[ "$WORKER_SERVICE_PATH" =~ ^/[A-Za-z0-9_.]+/$ ]] \
        || cap_fail 2 "--workerServicePath ${WORKER_SERVICE_PATH} is not a gRPC service prefix: /<package>.<Service>/, proto identifier characters only, with the trailing slash"

    # Every api path must be ABSOLUTE. A relative entry is rendered into the
    # Ingress by render_api_http_ingress but NOT counted by count_paths, so it
    # would make the whole api. HTTP Ingress vanish while the door still
    # promoted to `live` -- every HTTP route dropped, and the failure is the
    # h2c protocol error naming nothing that this script exists to prevent.
    local path
    while IFS= read -r path || [[ -n "$path" ]]; do
        [[ -n "$path" ]] || continue
        [[ "$path" == /* ]] \
            || cap_fail 2 "--apiPaths entry ${path} is not absolute. A relative entry renders into the Ingress and is not counted, which drops the whole api. HTTP rule set while the door still goes live"
    done < <(printf '%s' "$API_PATHS" | tr ',' '\n')

    # THE THREE HOSTS ARE COMPOSED HERE AND IN component/frontdoor's
    # AccountHosts, and the two must agree. They are pinned by
    # scripts/deploy/account_front_door_test.go, which reads the labels out of
    # this file rather than restating them -- a second spelling would be a
    # certificate for a host nothing routes.
    APP_HOST="app.${RESERVED_NAME}"
    API_HOST="api.${RESERVED_NAME}"
    ID_HOST="id.${RESERVED_NAME}"

    # THE OBJECT NAME IS KEYED ON THE ACCOUNT ID, NOT THE RESERVED NAME, for
    # bind-custom-domain.sh's reason: a hostname is not a legal Kubernetes
    # object name, and any sanitiser that made it one would map some pair of
    # distinct names onto a single object -- one account silently overwriting
    # another's Ingresses. Keyed on the account rather than the door row so a
    # torn-down-and-reopened door converges on the same objects instead of
    # leaving orphans behind it.
    local slug
    slug="$(printf '%s' "$ACCOUNT_ID" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed 's/^-*//; s/-*$//')"
    [[ -n "$slug" ]] || cap_fail 2 "--accountId ${ACCOUNT_ID} contains no usable characters for an object name"
    OBJECT_NAME="account-front-door-${slug:0:200}"
    return 0
}

function check_prereqs() {
    # A DRY RUN NEEDS NEITHER, because it reaches no cluster -- see apply_objects.
    if [[ "$DRY_RUN" == "true" ]]; then
        return 0
    fi
    command -v kubectl &>/dev/null \
        || cap_fail 4 "kubectl is not installed or not on PATH. This capability applies Kubernetes objects; there is no other way for it to do its job."
    kubectl cluster-info &>/dev/null \
        || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first"
    return 0
}

# ---------------------------------------------------------------------------
# The refusal that is not a failure
# ---------------------------------------------------------------------------
function check_issuer() {
    if [[ -n "$ISSUER" ]]; then
        return 0
    fi
    cap_result_set "reason"       "no_acme_issuer"
    cap_result_set "detail"       "this cluster declares no ACME issuer, so no certificate can be requested for ${APP_HOST}, ${API_HOST} and ${ID_HOST}"
    cap_result_set "reservedName" "$RESERVED_NAME"
    cap_fail 3 "no ACME issuer configured (--issuer is empty), so ${RESERVED_NAME} cannot be issued a certificate. A Certificate with an empty issuerRef is accepted and then sits Pending forever, which is worse than refusing: nothing would ever say why the front door is not serving."
}

# ---------------------------------------------------------------------------
# The five objects
# ---------------------------------------------------------------------------

# render_labels emits the label block every object carries, indented to sit
# under metadata.
function render_labels() {
    cat <<YAML
    app.kubernetes.io/part-of: memql
    app.kubernetes.io/name: account-front-door
    memql/account-id: "${ACCOUNT_ID}"
    memql/account-front-door-id: "${DOOR_ID}"
YAML
    return 0
}

# render_simple_ingress emits one host -> one service Ingress with a `/` Prefix
# rule. Used for app. and id.; api. needs the two-object treatment below.
# render_simple_ingress emits one host -> one service Ingress with a `/` Prefix
# rule. $5 onward are extra `key: "value"` annotation lines, emitted verbatim.
#
# THE EXTRA ANNOTATIONS ARE NOT DECORATION -- see doorBackend in
# integrations/customdomain/accountdoor_provision.go for the two that matter
# and why omitting them breaks the feature rather than degrading it: identity
# serves TLS in-cluster, so `id.` without backend-protocol: HTTPS answers 502
# on every request while the row says live; and ingress-nginx defaults
# proxy-body-size to 1m, so `api.` without it 413s every upload past a
# megabyte with a message naming no knob.
function render_simple_ingress() {
    local name="$1" host="$2" service="$3" port="$4"
    shift 4
    cat <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
YAML
    render_labels
    cat <<YAML
  annotations:
YAML
    local extra
    for extra in "$@"; do
        printf '    %s\n' "$extra"
    done
    cat <<YAML
spec:
  ingressClassName: ${INGRESS_CLASS}
  tls:
    - hosts:
        - ${host}
      secretName: ${OBJECT_NAME}-tls
  rules:
    - host: ${host}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: ${service}
                port:
                  number: ${port}
YAML
    return 0
}

# render_api_http_ingress emits the api. host's HTTP rule set: one path entry
# per --apiPaths member, all to the bff's HTTP Service.
#
# EVERY ENTRY IS pathType: Prefix, matching cmd/frontdoorpaths' render(). That
# generator's comment carries the reasoning, and repeating the choice here
# rather than the reasoning is deliberate: the two must agree, and the place
# the decision is argued is the one that produces the list.
function render_api_http_ingress() {
    cat <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ${OBJECT_NAME}-api
  namespace: ${NAMESPACE}
  labels:
YAML
    render_labels
    cat <<YAML
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "${PROXY_BODY_SIZE}"
spec:
  ingressClassName: ${INGRESS_CLASS}
  tls:
    - hosts:
        - ${API_HOST}
      secretName: ${OBJECT_NAME}-tls
  rules:
    - host: ${API_HOST}
      http:
        paths:
YAML
    local path
    # `|| [[ -n "$path" ]]` IS LOAD-BEARING. `tr` leaves the final field
    # without a trailing newline, so `read` returns non-zero on it and a bare
    # `while read` drops it -- routing every path but the last, silently. The
    # failure that produces is the one this whole feature exists to avoid: an
    # HTTP/1.1 request handed to an h2c backend, naming nothing.
    while IFS= read -r path || [[ -n "$path" ]]; do
        [[ -n "$path" ]] || continue
        cat <<YAML
          - path: ${path}
            pathType: Prefix
            backend:
              service:
                name: ${BFF_HTTP_SERVICE}
                port:
                  number: ${BFF_HTTP_PORT}
YAML
    done < <(printf '%s' "$API_PATHS" | tr ',' '\n')
    return 0
}

# render_api_grpc_ingress emits the api. host's gRPC rules: the worker stream's
# service prefix to the agent, then the `/` catch-all to the bff's h2c Service.
#
# A SECOND OBJECT OVER THE SAME HOST, and it has to be: backend-protocol is a
# per-Service annotation, so the h2c backend and the HTTP backend cannot live
# in one Ingress. This mirrors api-front-door / api-front-door-grpc in the
# cluster's own generated manifests, rule for rule -- including the one rule
# in it that is not the bff's (the header says why), emitted first so this
# file and cmd/frontdoorhosts show one shape. nginx orders locations by prefix
# length, so the order is for the reader rather than the router.
function render_api_grpc_ingress() {
    cat <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ${OBJECT_NAME}-api-grpc
  namespace: ${NAMESPACE}
  labels:
YAML
    render_labels
    cat <<YAML
  annotations:
    nginx.ingress.kubernetes.io/backend-protocol: "GRPC"
spec:
  ingressClassName: ${INGRESS_CLASS}
  tls:
    - hosts:
        - ${API_HOST}
      secretName: ${OBJECT_NAME}-tls
  rules:
    - host: ${API_HOST}
      http:
        paths:
          - path: ${WORKER_SERVICE_PATH}
            pathType: Prefix
            backend:
              service:
                name: ${AGENT_GRPC_SERVICE}
                port:
                  number: ${AGENT_GRPC_PORT}
          - path: /
            pathType: Prefix
            backend:
              service:
                name: ${BFF_GRPC_SERVICE}
                port:
                  number: ${BFF_GRPC_PORT}
YAML
    return 0
}

function render_certificate() {
    cat <<YAML
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${OBJECT_NAME}
  namespace: ${NAMESPACE}
  labels:
YAML
    render_labels
    cat <<YAML
spec:
  secretName: ${OBJECT_NAME}-tls
  dnsNames:
    - ${APP_HOST}
    - ${API_HOST}
    - ${ID_HOST}
  issuerRef:
    name: ${ISSUER}
    kind: ClusterIssuer
    group: cert-manager.io
YAML
    return 0
}

# render_objects emits every document, INGRESSES FIRST AND THE CERTIFICATE
# LAST.
#
# `kubectl apply` honours stream order, and the order matters for the reason
# the Go substrate's BindDoor states: the HTTP-01 challenge is served THROUGH
# these Ingresses, so requesting the certificate before they exist starts an
# order whose first attempt is guaranteed to fail -- and cert-manager backs off
# after a failure, which makes every door slower to come up for no reason.
#
# The first version of this script emitted the Certificate first while the Go
# path emitted it last, so one substrate documented why the other was wrong.
function render_objects() {
    render_simple_ingress "${OBJECT_NAME}-app" "$APP_HOST" "$EDGE_SERVICE" "$EDGE_PORT"
    printf -- '---\n'
    render_api_grpc_ingress
    printf -- '---\n'
    render_simple_ingress "${OBJECT_NAME}-id" "$ID_HOST" "$IDENTITY_SERVICE" "$IDENTITY_PORT" \
        'nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"' \
        'nginx.ingress.kubernetes.io/proxy-ssl-verify: "off"' 
    # THE HTTP RULE SET IS OMITTED WHEN THERE ARE NO PATHS, rather than emitted
    # empty. An Ingress whose rule carries a zero-length `paths` list is
    # rejected by the API server, so an empty --apiPaths would take the whole
    # apply down -- including the three hosts that have nothing to do with it.
    # In practice the provisioner always passes the generated list; this is
    # what happens if it ever passes nothing, and it is a front door missing
    # its HTTP routes rather than a front door that failed to come up.
    if [[ "$API_PATH_COUNT" -gt 0 ]]; then
        printf -- '---\n'
        render_api_http_ingress
    fi
    printf -- '---\n'
    render_certificate
    return 0
}

function count_paths() {
    if [[ -n "$API_PATHS" ]]; then
        API_PATH_COUNT="$(printf '%s' "$API_PATHS" | tr ',' '\n' | grep -c '^/' || true)"
    fi
    DOCUMENT_COUNT=4
    if [[ "$API_PATH_COUNT" -gt 0 ]]; then
        DOCUMENT_COUNT=5
    fi
    return 0
}

# check_render asserts the rendered documents are the ones this script exists
# to apply.
#
# IT RUNS ON EVERY PATH, NOT ONLY THE DRY RUN, and that placement is the whole
# point. The first version put all of this inside the `--dryRun` branch, so the
# path that actually touches the cluster was checked by nothing -- the mode
# whose job is to be safe was the only mode that looked.
#
# What it can catch is a RENDERING fault: a value that swallowed a line, a
# document that did not come out, a host with no rule or no SAN. That is a real
# class and it is the one this rendering can plausibly have. It is not schema
# validation, and it is not a substitute for check_params, which is what stops
# a hostile value reaching the render at all.
function check_render() {
    local rendered="$1"
    local kinds
    kinds="$(printf '%s\n' "$rendered" | grep -c '^kind: ' || true)"
    if [[ "$kinds" != "$DOCUMENT_COUNT" ]]; then
        cap_fail 5 "the rendered objects did not validate: expected ${DOCUMENT_COUNT} documents, found ${kinds}"
    fi
    # Quiet grep may close a pipe before printf finishes a large manifest.
    # Here-strings keep pipefail from turning a valid match into a refusal.
    grep -q '^kind: Certificate$' <<< "$rendered" \
        || cap_fail 5 "the rendered objects did not validate: no Certificate document"

    # EVERY PATH WE WERE GIVEN MUST HAVE A RULE. This exists because its
    # absence hid a real defect: a `while read` loop dropped the last path,
    # apiPathCount still reported the count it was HANDED, and the document
    # count was unchanged -- so the check passed while one route went nowhere.
    # A count taken only from the input cannot notice the render disagreeing.
    local rendered_paths
    rendered_paths="$(printf '%s\n' "$rendered" | grep -c '^          - path: /' || true)"
    # +4: the `/` rules on app., id. and api.-grpc, plus the worker-stream
    # prefix on api.-grpc, which is an absolute path like every other.
    if [[ "$rendered_paths" != "$((API_PATH_COUNT + 4))" ]]; then
        cap_fail 5 "the rendered objects did not validate: ${API_PATH_COUNT} api path(s) plus 3 catch-alls and the worker-stream rule were expected, but the render carries ${rendered_paths} rule(s)"
    fi

    # THE SAN CHECK IS ANCHORED, and the first version was not. Certificate
    # dnsNames sit at four spaces and Ingress tls.hosts at eight, and an
    # unanchored `    - ${host}$` matches BOTH -- so a Certificate naming one
    # host instead of three satisfied it, because the other two matched their
    # own Ingresses' tls entries. The host is also grep-escaped: an unescaped
    # dot matches any character, which would let `app.acme.com` be satisfied by
    # `appxacme.com`.
    local host escaped
    for host in "$APP_HOST" "$API_HOST" "$ID_HOST"; do
        escaped="$(printf '%s' "$host" | sed 's/[.[\*^$]/\\&/g')"
        grep -qE "^    - host: ${escaped}\$" <<< "$rendered" \
            || cap_fail 5 "the rendered objects did not validate: ${host} has no Ingress rule"
        grep -qE "^    - ${escaped}\$" <<< "$rendered" \
            || cap_fail 5 "the rendered objects did not validate: ${host} is not a certificate dnsName"
    done
    return 0
}

function apply_objects() {
    local rendered out
    rendered="$(render_objects)"
    check_render "$rendered"

    # --renderTo EXISTS BECAUSE THE ENVELOPE IS NOT THE OBJECTS.
    #
    # A review of this script mutation-tested its test suite and found that
    # swapping the app./id. backend Services, renaming all four Ingress
    # suffixes so bind and unbind disagree, and reducing the Certificate to a
    # single SAN each produced a BYTE-IDENTICAL envelope -- so every assertion
    # passed against three broken renders. That is what a suite asserting only
    # `ok`, `apiPathCount` and `objectName` can do: it re-checks the input.
    #
    # So the render is made inspectable. account_front_door_test.go parses this
    # file and asserts hosts, backends, ports, protocols and SANs, and an
    # operator gets the same thing for `kubectl diff -f`.
    if [[ -n "$RENDER_TO" ]]; then
        printf '%s\n' "$rendered" > "$RENDER_TO" \
            || cap_fail 5 "could not write the rendered manifest to ${RENDER_TO}"
        cap_info "rendered manifest written to ${RENDER_TO}"
    fi

    if [[ "$DRY_RUN" == "true" ]]; then
        # A DRY RUN TOUCHES NO CLUSTER. `kubectl apply --dry-run=client`
        # fetches the API server's OpenAPI schema and needs discovery to map a
        # kind to a resource, so it reaches the server regardless of
        # --validate=false -- which fails on every CI runner. check_render
        # above is what a machine with no cluster can honestly make, and it is
        # now the same check the real apply gets.
        cap_info "dry run: ${DOCUMENT_COUNT} document(s) covering ${APP_HOST}, ${API_HOST}, ${ID_HOST}"
        return 0
    fi

    # SERVER-SIDE, UNDER THIS FEATURE'S OWN FIELD MANAGER, matching
    # integrations/customdomain's apiProvisioner exactly. A client-side apply
    # here and a server-side apply from the engine over the same object is the
    # managed-fields conflict that leaves an operator's kubectl and the sweep
    # arguing about who owns a field. --force-conflicts is the script's half of
    # the engine's `force=true`, and it is right for the same reason: THIS is
    # the owner, and a field somebody edited by hand must not make every
    # subsequent reconciliation fail with a conflict nobody can see.
    if ! out="$(printf '%s\n' "$rendered" | kubectl apply --server-side \
        --field-manager="${FIELD_MANAGER}" --force-conflicts -f - 2>&1)"; then
        cap_fail 5 "could not apply the front door for ${RESERVED_NAME}: ${out}"
    fi
    cap_info "$out"
    if grep -qv 'unchanged$' <<< "$out"; then
        CHANGED_ANY=true
        cap_changed
    fi
    APPLIED=true
    return 0
}

# ---------------------------------------------------------------------------
# Readiness -- `live` is reachable only through this
# ---------------------------------------------------------------------------
function check_certificate() {
    if [[ "$DRY_RUN" == "true" ]]; then
        CERT_STATUS="dry run: the certificate was not requested"
        return 0
    fi
    # APPLYING A CERTIFICATE IS NOT HOLDING ONE, and here it is also the
    # activation rule: this ONE certificate names all three hosts, so it cannot
    # go Ready until all three solve HTTP-01 -- which is what makes the front
    # door all-or-nothing without the reconciler policing anything.
    if kubectl wait --for=condition=Ready \
        "certificate/${OBJECT_NAME}" -n "$NAMESPACE" \
        --timeout="${WAIT_SECONDS}s" &>/dev/null; then
        CERT_READY=true
        CERT_STATUS="the certificate is Ready for all three hosts"
        return 0
    fi
    # cert-manager's OWN message, verbatim. Its wording names which challenge
    # is outstanding, which is exactly what an operator with one CNAME still
    # missing needs to read.
    CERT_STATUS="$(kubectl get "certificate/${OBJECT_NAME}" -n "$NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true)"
    if [[ -z "$CERT_STATUS" ]]; then
        CERT_STATUS="the certificate has no Ready condition yet -- cert-manager has not started the order"
    fi
    cap_info "certificate not Ready after ${WAIT_SECONDS}s: ${CERT_STATUS}"
    return 0
}

function collect_result() {
    cap_result_set     "accountId"         "$ACCOUNT_ID"
    cap_result_set     "doorId"            "$DOOR_ID"
    cap_result_set     "reservedName"      "$RESERVED_NAME"
    cap_result_set     "appHost"           "$APP_HOST"
    cap_result_set     "apiHost"           "$API_HOST"
    cap_result_set     "idHost"            "$ID_HOST"
    cap_result_set     "namespace"         "$NAMESPACE"
    cap_result_set     "objectName"        "$OBJECT_NAME"
    cap_result_set     "issuer"            "$ISSUER"
    cap_result_set_raw "apiPathCount"      "$API_PATH_COUNT"
    # HONEST ON A DRY RUN. The first version set this true unconditionally,
    # including for a run that reached no cluster and requested nothing -- so
    # a caller reading `applied` could not tell a real bind from a rehearsal.
    cap_result_set_raw "applied"           "$APPLIED"
    cap_result_set_raw "certificateReady"  "$CERT_READY"
    cap_result_set     "certificateStatus" "$CERT_STATUS"
    cap_result_set_raw "objectsChanged"    "$CHANGED_ANY"
    return 0
}

function main() {
    check_params
    check_issuer
    check_prereqs
    count_paths
    apply_objects
    check_certificate
    collect_result
    # cap_ok even when the certificate is not Ready: the bind RAN, which is
    # what exit 0 means here. `certificateReady` carries the answer, and a
    # not-yet-Ready certificate is the ordinary state for the first minutes of
    # a three-name order rather than a failure anybody should act on.
    cap_ok
}

main "$@"
