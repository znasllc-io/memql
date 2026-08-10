#!/usr/bin/env bash
#
# scripts/vscode/verification-setup.sh
# ====================================
#
# Everything the VS Code runtime-panel verification checklist calls "Setup",
# done for you (memql#3337).
#
# The checklist -- docs/public/language/vscode-runtime-panel-verification.md --
# has five ordered setup steps before section 1, and memql#3386 was filed
# because a reader could not get through them. Four separate blockers (#3383,
# #3384, #3385, #3386) all landed BEFORE checkbox one. Every one of them was a
# setup fact that could not be guessed from outside, which is exactly the kind
# of thing a script should be holding rather than a human's memory.
#
# WHAT THIS DOES NOT DO, AND WILL NOT. It does not verify anything. It ends at
# the point the human's work begins: a built extension, a healthy cluster, a
# trusted CA, two credentialed cluster entries, and the F5 instruction. The
# checklist itself is a human pressing keys and looking at pixels, and a script
# that claimed any of those boxes would be manufacturing the exact false signal
# memql#3337 exists to prevent.
#
# WHY IT DOES NOT REIMPLEMENT THE INSTALLER. The install graph
# (scripts/install/graph/install.json) is the END-USER path: it checks the
# stack out at a release tag. Someone working this checklist already has the
# repo, so cluster bring-up here is `make up` -- the in-repo developer path --
# while the pinned tool downloads still go through install.binary
# (scripts/install/install-binary.sh), which digest-verifies every artifact.
# Nothing here pipes curl into a shell.
#
# THE TWO CLUSTER ENTRIES ARE WRITTEN BY THE EXTENSION'S OWN WRITER.
# src/clusters/file.ts is the code that has to preserve comments and unknown
# keys in a file shared with the Cockpit, so this script bundles and calls it
# rather than rewriting clusters.yaml with a YAML dump that would silently eat
# both.
#
# Usage:
#   scripts/vscode/verification-setup.sh                  # the whole path
#   scripts/vscode/verification-setup.sh --install-tools  # + fetch k3d/kubectl/mkcert
#   scripts/vscode/verification-setup.sh --skip-cluster   # cluster is already up
#   scripts/vscode/verification-setup.sh --skip-build     # extension already built
#   scripts/vscode/verification-setup.sh --skip-signin    # entries already credentialed
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"

# The two entries the checklist requires. Section 3 asks that running a
# mutation raise a modal confirmation, and a cluster marked `local: true`
# deliberately never prompts -- so verifying that item against one k3d cluster
# needs a second entry pointing at the same place WITHOUT the flag.
LOCAL_ENTRY="vscode-local"
NONLOCAL_ENTRY="vscode-nonlocal"

DEFAULT_DOMAIN="local.znas.io"

#=============================================================================
# LOGGING
#=============================================================================

function info()    { echo "INFO: $*" >&2; }
function warn()    { echo "WARNING: $*" >&2; }
function error()   { echo "ERROR: $*" >&2; }
function section() { echo >&2; echo "=== $* ===" >&2; }
function die()     { local code="$1"; shift; error "$*"; exit "$code"; }

#=============================================================================
# ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Brings this machine to the point where the VS Code runtime-panel verification
checklist can be started: built extension, healthy cluster, trusted CA, two
credentialed entries in ~/.memql/clusters.yaml.

Options:
    --install-tools     Install pinned k3d / kubectl / mkcert when missing
                        (digest-verified via scripts/install/install-binary.sh)
    --skip-cluster      Do not run 'make up' / 'make secrets'
    --skip-build        Do not build the extension or memql-lsp
    --skip-signin       Do not sign in; leave existing credentials alone
    --domain=NAME       Front-door domain (default: $DEFAULT_DOMAIN)
    --clusters-file=P   Registry to write (default: \$HOME/.memql/clusters.yaml)
    --help              Show this help
EOF
}

function parse_arguments() {
    INSTALL_TOOLS=0
    SKIP_CLUSTER=0
    SKIP_BUILD=0
    SKIP_SIGNIN=0
    DOMAIN="$DEFAULT_DOMAIN"
    CLUSTERS_FILE="${HOME}/.memql/clusters.yaml"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --install-tools)  INSTALL_TOOLS=1; shift ;;
            --skip-cluster)   SKIP_CLUSTER=1; shift ;;
            --skip-build)     SKIP_BUILD=1; shift ;;
            --skip-signin)    SKIP_SIGNIN=1; shift ;;
            --domain=*)       DOMAIN="${1#*=}"; shift ;;
            --clusters-file=*) CLUSTERS_FILE="${1#*=}"; shift ;;
            --help)           show_help; exit 0 ;;
            *)                error "unknown option: $1"; show_help; exit 2 ;;
        esac
    done

    IDENTITY_HOST="identity.${DOMAIN}"
    COCKPIT_HOST="cockpit.${DOMAIN}"
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function check_repo_tools() {
    section "Checking repository tools"
    local missing=()
    local tool
    for tool in go npm curl jq openssl python3; do
        command -v "$tool" >/dev/null || missing+=("$tool")
    done
    [[ ${#missing[@]} -eq 0 ]] || die 4 "missing required tools: ${missing[*]}"
    info "go, npm, curl, jq, openssl, python3: present"
}

# check_container_access distinguishes the two ways `docker info` fails on
# Linux, because they have completely different remedies and the usual message
# names only one of them.
#
# "Cannot connect to the Docker daemon" means the daemon is down. "permission
# denied ... /var/run/docker.sock" means the daemon is running fine and THIS
# USER is not in the docker group -- starting Docker again, forever, will not
# fix it. Reporting the second as the first is what makes a five-second fix
# into an afternoon (memql#3337).
function check_container_access() {
    section "Checking container access"

    command -v docker >/dev/null || die 4 "docker is not installed -- see https://docs.docker.com/get-docker/"

    local probe
    if probe="$(docker info 2>&1)"; then
        info "docker: reachable"
        return 0
    fi

    if grep -qi "permission denied" <<<"$probe"; then
        error "the docker daemon is running, but this user cannot reach its socket."
        error "you are not in the 'docker' group:"
        error "    id -nG        -> $(id -nG)"
        error ""
        error "fix it, then start a NEW login session (the group is read at login):"
        error "    sudo usermod -aG docker \"\$USER\""
        error "    newgrp docker      # this shell only; log out and back in for the rest"
        die 4 "docker socket is not accessible to $(id -un)"
    fi

    error "docker is installed but the daemon is not answering:"
    error "    $probe"
    die 4 "docker daemon is not running"
}

function ensure_cluster_tools() {
    section "Checking cluster tools"
    local tool missing=()
    for tool in k3d kubectl mkcert; do
        if command -v "$tool" >/dev/null; then
            info "$tool: $(command -v "$tool")"
        else
            missing+=("$tool")
        fi
    done
    [[ ${#missing[@]} -eq 0 ]] && return 0

    if [[ "$INSTALL_TOOLS" -eq 0 ]]; then
        error "missing: ${missing[*]}"
        error "re-run with --install-tools to fetch the pinned, digest-verified builds:"
        for tool in "${missing[@]}"; do
            error "    scripts/install/install-binary.sh --tool=$tool"
        done
        die 4 "missing cluster tools: ${missing[*]}"
    fi

    for tool in "${missing[@]}"; do
        info "installing pinned $tool"
        bash "$REPO_ROOT/scripts/install/install-binary.sh" --tool="$tool" >/dev/null \
            || die 5 "install.binary failed for $tool"
    done
    # install.binary's default dest. Adding it here means the rest of this run
    # sees the tools it just installed; the operator still needs it on PATH for
    # their own shell, which print_next_steps says.
    export PATH="${HOME}/.memql/bin:${PATH}"
    for tool in "${missing[@]}"; do
        command -v "$tool" >/dev/null || die 5 "$tool still not on PATH after install"
        info "$tool: $(command -v "$tool")"
    done
}

# ensure_local_ca creates the mkcert CA only if none exists. Issuing the
# front-door wildcard is `make secrets`' job (memql#3384) -- this is only the
# one-time step that writes to the system trust store, which is why it is
# separate and why it takes a confirmation phrase.
function ensure_local_ca() {
    section "Checking the local CA"
    local caroot
    caroot="$(mkcert -CAROOT 2>/dev/null || true)"
    if [[ -n "$caroot" && -f "$caroot/rootCA.pem" ]]; then
        info "mkcert CA: $caroot/rootCA.pem"
        CAROOT="$caroot"
        return 0
    fi
    info "no mkcert CA found -- creating one (writes to the system trust store)"
    bash "$REPO_ROOT/scripts/install/mkcert-setup.sh" --confirm=install-memql-ca >/dev/null \
        || die 5 "mkcert CA setup failed"
    CAROOT="$(mkcert -CAROOT)"
    info "mkcert CA: $CAROOT/rootCA.pem"
}

#=============================================================================
# BUILD
#=============================================================================

# The staged binary goes under NODE's platform/arch spelling, which is what
# resolveServerPath() looks in -- not Go's GOOS/GOARCH. Same mapping
# package.sh and host-test.sh make, for the same reason.
function node_bin_dir() {
    local nodeos nodearch
    case "$(go env GOHOSTOS)" in
        windows) nodeos="win32" ;;
        *)       nodeos="$(go env GOHOSTOS)" ;;
    esac
    case "$(go env GOHOSTARCH)" in
        amd64) nodearch="x64" ;;
        386)   nodearch="ia32" ;;
        *)     nodearch="$(go env GOHOSTARCH)" ;;
    esac
    echo "$EXT_DIR/bin/${nodeos}-${nodearch}"
}

function build_extension() {
    [[ "$SKIP_BUILD" -eq 1 ]] && { info "skipping build (--skip-build)"; return 0; }
    section "Building the extension"

    # NOT optional on a clean checkout: the extension consumes sdk/ts and
    # sdk/ts-viewkit as file: dependencies whose main/types point into a dist/
    # that does not exist until they are built (memql#3340).
    bash "$REPO_ROOT/scripts/vscode/deps.sh" >&2 || die 5 "scripts/vscode/deps.sh failed"
    ( cd "$EXT_DIR" && npm ci --no-audit --no-fund >&2 ) || die 5 "npm ci failed"
    ( cd "$EXT_DIR" && npm run compile >&2 ) || die 5 "npm run compile failed"

    local bindir
    bindir="$(node_bin_dir)"
    info "building memql-lsp -> ${bindir#"$EXT_DIR"/}/memql-lsp"
    mkdir -p "$bindir"
    ( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$bindir/memql-lsp" ./cmd/memql-lsp ) \
        || die 5 "memql-lsp build failed"
}

#=============================================================================
# CLUSTER
#=============================================================================

function bring_up_cluster() {
    [[ "$SKIP_CLUSTER" -eq 1 ]] && { info "skipping cluster bring-up (--skip-cluster)"; return 0; }
    section "Bringing up the cluster"

    if k3d cluster list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "memql"; then
        info "k3d cluster 'memql' already exists -- re-seeding secrets only"
        ( cd "$REPO_ROOT" && make secrets >&2 ) || die 5 "make secrets failed"
    else
        info "running 'make up' (this takes several minutes)"
        ( cd "$REPO_ROOT" && make up >&2 ) || die 5 "make up failed"
    fi
}

# verify_tls is the check memql#3384 was filed over: the bring-up used to end
# with "All Deployments Available" while the front door served Traefik's
# default self-signed certificate, because the TLS secret nothing created was
# skipped with a WARN 140 lines into a 700-line log.
function verify_tls() {
    section "Verifying the front door's certificate"
    local issuer
    issuer="$(echo | openssl s_client -connect "${COCKPIT_HOST}:443" -servername "${COCKPIT_HOST}" 2>/dev/null \
        | openssl x509 -noout -issuer 2>/dev/null || true)"

    if [[ -z "$issuer" ]]; then
        die 5 "nothing answered TLS on ${COCKPIT_HOST}:443 -- is the cluster up, and does /etc/hosts point ${COCKPIT_HOST} at 127.0.0.1?"
    fi
    if grep -qi "TRAEFIK DEFAULT CERT" <<<"$issuer"; then
        error "the front door is serving Traefik's default certificate, not the mkcert wildcard."
        error "the 'local-znas-tls' secret is missing. Run: make secrets"
        die 5 "front door is on the default certificate"
    fi
    info "issuer: ${issuer#issuer=}"
}

# read_discovery takes every connection fact from the cluster's own document
# rather than from a guess. Until memql#3399 this field read
# "https://bff.local.znas.io" -- a URL form the extension refuses, at a host
# with no ingress -- so it is asserted here rather than trusted.
function read_discovery() {
    section "Reading the cluster's discovery document"
    local doc
    doc="$(curl -fsS "https://${IDENTITY_HOST}/.well-known/memql-config.json" 2>/dev/null)" \
        || die 5 "could not read https://${IDENTITY_HOST}/.well-known/memql-config.json"

    ISSUER_URL="$(jq -r '.identityUrl // empty' <<<"$doc")"
    ENDPOINT="$(jq -r '.grpcEndpoint // empty' <<<"$doc")"
    CLIENT_ID="$(jq -r '.clientId // empty' <<<"$doc")"

    [[ -n "$ISSUER_URL" ]] || die 5 "discovery document has no identityUrl"
    [[ -n "$ENDPOINT" ]]   || die 5 "discovery document has no grpcEndpoint"
    [[ -n "$CLIENT_ID" ]]  || die 5 "discovery document has no clientId"

    if [[ "$ENDPOINT" == *"://"* ]]; then
        error "the cluster advertises grpcEndpoint=\"$ENDPOINT\", which carries a scheme."
        error "the extension refuses that form -- it wants a bare host[:port] (memql#3399)."
        die 5 "grpcEndpoint is a URL, not a host:port"
    fi

    info "issuer:   $ISSUER_URL"
    info "endpoint: $ENDPOINT"
    info "clientId: $CLIENT_ID"
}

#=============================================================================
# SIGN-IN
#=============================================================================

# device_sign_in runs the RFC 8628 device grant and echoes "<access> <refresh>"
# on stdout. Everything a human reads goes to stderr.
#
# WHY THE DEVICE GRANT AND NOT LOOPBACK. The loopback flow needs a listener a
# browser can reach and a redirect back into a waiting process; from a script
# that means holding a socket open and parsing a callback. The device grant is
# the one sign-in shape designed for a process with no browser of its own:
# print a code, let the human approve it wherever they like, poll.
function device_sign_in() {
    local label="$1"
    local start poll now interval deadline expires_in
    section "Signing in ($label)" >&2

    start="$(curl -fsS -X POST "${ISSUER_URL}/device/code" \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-urlencode "client_id=${CLIENT_ID}" 2>/dev/null)" \
        || die 5 "device authorization request failed"

    local device_code user_code verify_uri verify_complete
    device_code="$(jq -r '.device_code // empty' <<<"$start")"
    user_code="$(jq -r '.user_code // empty' <<<"$start")"
    verify_uri="$(jq -r '.verification_uri // empty' <<<"$start")"
    verify_complete="$(jq -r '.verification_uri_complete // empty' <<<"$start")"
    interval="$(jq -r '.interval // 5' <<<"$start")"
    expires_in="$(jq -r '.expires_in // 600' <<<"$start")"
    [[ -n "$device_code" && -n "$user_code" ]] || die 5 "device authorization response was incomplete"

    echo >&2
    echo "  ------------------------------------------------------------" >&2
    echo "  APPROVE THIS SIGN-IN ($label)" >&2
    echo "  ------------------------------------------------------------" >&2
    echo "  code: $user_code" >&2
    echo "  open: ${verify_complete:-$verify_uri}" >&2
    echo "  ------------------------------------------------------------" >&2
    echo >&2

    deadline=$(( $(date +%s) + expires_in ))
    while :; do
        now="$(date +%s)"
        [[ "$now" -lt "$deadline" ]] || die 5 "device authorization expired before it was approved"
        sleep "$interval"

        poll="$(curl -sS -X POST "${ISSUER_URL}/oauth/token" \
            -H 'Content-Type: application/x-www-form-urlencoded' \
            --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:device_code" \
            --data-urlencode "device_code=${device_code}" \
            --data-urlencode "client_id=${CLIENT_ID}" 2>/dev/null || true)"

        local err access refresh
        err="$(jq -r '.error // empty' <<<"$poll" 2>/dev/null || true)"
        case "$err" in
            authorization_pending) continue ;;
            slow_down) interval=$(( interval + 5 )); continue ;;
            "") ;;
            *) die 5 "device authorization failed: $err $(jq -r '.error_description // empty' <<<"$poll")" ;;
        esac

        access="$(jq -r '.access_token // empty' <<<"$poll")"
        refresh="$(jq -r '.refresh_token // empty' <<<"$poll")"
        [[ -n "$access" ]] || die 5 "token response carried no access_token"
        [[ -n "$refresh" ]] || warn "token response carried no refresh_token -- the session will die at the access token's TTL"
        info "approved"
        echo "$access $refresh"
        return 0
    done
}

#=============================================================================
# REGISTRY
#=============================================================================

# write_cluster_entries calls the EXTENSION'S OWN writer (src/clusters/file.ts)
# rather than dumping YAML.
#
# clusters.yaml is shared with the memQL Cockpit, and the checklist has an item
# for exactly this: "comments and unknown fields already in clusters.yaml
# survive a write". A pyyaml round-trip drops every comment in the file. Using
# the writer that has to satisfy that item is both safer and one more place it
# gets exercised.
function write_cluster_entries() {
    section "Writing the two cluster entries"

    local access_local="$1" refresh_local="$2" access_nonlocal="$3" refresh_nonlocal="$4"
    local tmp bundle
    tmp="$(mktemp -d)"
    bundle="$tmp/clusters-file.cjs"
    trap 'rm -rf "$tmp"' RETURN

    ( cd "$EXT_DIR" && npx --no-install esbuild --bundle --platform=node --format=cjs \
        --outfile="$bundle" src/clusters/file.ts >/dev/null 2>&1 ) \
        || die 5 "could not bundle src/clusters/file.ts (did the build step run?)"

    mkdir -p "$(dirname "$CLUSTERS_FILE")"
    [[ -f "$CLUSTERS_FILE" ]] || printf 'clusters: []\n' > "$CLUSTERS_FILE"
    cp "$CLUSTERS_FILE" "${CLUSTERS_FILE}.bak"
    info "backed up ${CLUSTERS_FILE} -> ${CLUSTERS_FILE}.bak"

    MEMQL_SETUP_FILE="$CLUSTERS_FILE" \
    MEMQL_SETUP_PAYLOAD="$(jq -n \
        --arg le "$LOCAL_ENTRY" --arg ne "$NONLOCAL_ENTRY" \
        --arg domain "$DOMAIN" --arg endpoint "$ENDPOINT" \
        --arg issuer "$ISSUER_URL" --arg clientId "$CLIENT_ID" \
        --arg la "$access_local" --arg lr "$refresh_local" \
        --arg na "$access_nonlocal" --arg nr "$refresh_nonlocal" \
        '[
           {name:$le, displayName:"\($domain) (local)", domain:$domain, endpoint:$endpoint,
            issuer:$issuer, clientId:$clientId, token:$la, refreshToken:$lr, local:true},
           {name:$ne, displayName:"\($domain) (not local)", domain:$domain, endpoint:$endpoint,
            issuer:$issuer, clientId:$clientId, token:$na, refreshToken:$nr}
         ]')" \
    node -e '
      const { upsertCluster, setSelectedCluster } = require(process.argv[1]);
      const file = process.env.MEMQL_SETUP_FILE;
      const entries = JSON.parse(process.env.MEMQL_SETUP_PAYLOAD);
      (async () => {
        for (const entry of entries) {
          // Drop empties rather than writing blank keys: an empty token: is a
          // different state from an absent one, and only one of them is true.
          for (const k of Object.keys(entry)) {
            if (entry[k] === "" || entry[k] === undefined) delete entry[k];
          }
          await upsertCluster(file, entry);
        }
        await setSelectedCluster(file, entries[0].name);
      })().catch((err) => { console.error(err?.stack ?? String(err)); process.exit(1); });
    ' "$bundle" || die 5 "writing $CLUSTERS_FILE failed"

    info "wrote '$LOCAL_ENTRY' (local: true) and '$NONLOCAL_ENTRY' (absent = not local)"
    info "selected: $LOCAL_ENTRY"
}

#=============================================================================
# NEXT STEPS
#=============================================================================

function print_next_steps() {
    local caroot
    caroot="${CAROOT:-$(mkcert -CAROOT 2>/dev/null || echo '<mkcert -CAROOT>')}"

    section "Setup complete -- the rest is yours"
    cat >&2 <<EOF

Node does NOT read the OS trust store, so the extension host needs the CA
named explicitly, in the environment VS Code is LAUNCHED from. An export in a
shell after launch does not reach an already-running window.

    export NODE_EXTRA_CA_CERTS="${caroot}/rootCA.pem"
    code ${EXT_DIR}

Then press F5, open a folder with .memql files (${REPO_ROOT}/dsl is the obvious
one) in the Extension Development Host, and work through:

    docs/public/language/vscode-runtime-panel-verification.md

Two cluster defects worth knowing before you start:

  * Keep identity at ONE replica (memql#3400). Each replica self-generates a
    signing key, so scaling identity breaks roughly half of all auth and
    reports it as "invalid or expired token".
  * If sign-in suddenly redirects to /setup, restart the identity pod
    (memql#3415) -- it repairs itself on boot.

EOF
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    check_repo_tools
    check_container_access
    ensure_cluster_tools
    ensure_local_ca
    build_extension
    bring_up_cluster
    verify_tls
    read_discovery

    if [[ "$SKIP_SIGNIN" -eq 1 ]]; then
        info "skipping sign-in (--skip-signin); leaving $CLUSTERS_FILE alone"
    else
        # Each entry gets its OWN token pair. The refresh exchange ROTATES the
        # refresh token -- the presented one is consumed, with a 30-second
        # grace on the previous value -- so a pair shared between two entries
        # survives the first exchange and then stops working on the other.
        local pair_local pair_nonlocal
        pair_local="$(device_sign_in "$LOCAL_ENTRY")"
        pair_nonlocal="$(device_sign_in "$NONLOCAL_ENTRY")"
        # shellcheck disable=SC2086
        write_cluster_entries ${pair_local} ${pair_nonlocal}
    fi

    print_next_steps
}

main "$@"
