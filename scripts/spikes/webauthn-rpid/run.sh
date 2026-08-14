#!/usr/bin/env bash
#
# scripts/spikes/webauthn-rpid/run.sh
# ==================================
#
# The whole memql#3405 spike as one command (memql#3405, leg 2).
#
# WHY THIS EXISTS. The harness was already correct and the README already
# explained it, and the spike still sat open. What stood between an operator and
# a result was six prerequisite steps -- create a CA, remember that mkcert
# suffixes two-name output with `+1`, get the certificate OUT of the repository
# so a private key is not sitting untracked next to your work, find the right
# four flags, then do it again with a different --rp-id for the second case.
# Every one of those is a place to stop, and this issue is the label
# `needs-human`: the human's time is the scarce input, so everything that is not
# "hold a phone up to a screen" belongs in a script.
#
#   scripts/spikes/webauthn-rpid/run.sh              # serve; open the URL
#   scripts/spikes/webauthn-rpid/run.sh --probe-firefox
#   scripts/spikes/webauthn-rpid/run.sh --control    # the local.znas.io control
#
# WHAT IT DOES NOT DO. It does not answer leg 2. Nothing can, from here: the iOS
# and Android passkey providers are reached by a person scanning a QR code, and
# a script that claimed those rows would be manufacturing exactly the evidence
# the spike exists to obtain honestly. It gets you to the page with the table
# already half-filled, and stops.
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# OUTSIDE the repository, deliberately. `*.pem` is not gitignored here, and a
# private key written into the repo root sits untracked next to your work where
# a stray `git add` can sweep it up. memql#3518 was filed for this exact shape.
WORK_DIR="${MEMQL_SPIKE_3405_DIR:-/tmp/memql-spike-3405}"

# The default case: the parent RP ID the install wizard's D5 wants, so one
# passkey covers identity. / api. / bff.
DEFAULT_RP_ID="memql.localhost"
DEFAULT_PORT="8443"

# The control domain. A real domain that already resolves to 127.0.0.1 -- the
# shape the wizard's "Advanced" BYO-domain path offers. Running the identical
# sequence against it separates "`.localhost` specifically is the problem" from
# "local development origins are the problem", which are different findings with
# different consequences for the wizard's copy.
CONTROL_RP_ID="local.znas.io"

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

Brings up the memql#3405 spike harness: issues a local certificate, starts the
TLS server, and prints the URL to open. The browser's RP ID validator is
measured automatically the moment you load the page; the ceremonies that need a
phone are buttons on it.

Options:
    --probe-firefox   Load the page in headless Firefox, record leg 1, and exit.
                      Adds a Gecko data point without a human. Needs firefox +
                      certutil.
    --control         Run against ${CONTROL_RP_ID} instead of ${DEFAULT_RP_ID}.
    --rp-id=NAME      RP ID for the ceremony buttons (default: ${DEFAULT_RP_ID})
    --port=N          Listen port (default: ${DEFAULT_PORT})
    --work-dir=PATH   Certificates + results (default: ${WORK_DIR})
    --help            Show this help

Results land in <work-dir>/results.md and <work-dir>/results.jsonl.
EOF
}

function parse_arguments() {
    PROBE_FIREFOX=0
    RP_ID="$DEFAULT_RP_ID"
    PORT="$DEFAULT_PORT"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --probe-firefox) PROBE_FIREFOX=1; shift ;;
            --control)       RP_ID="$CONTROL_RP_ID"; shift ;;
            --rp-id=*)       RP_ID="${1#*=}"; shift ;;
            --port=*)        PORT="${1#*=}"; shift ;;
            --work-dir=*)    WORK_DIR="${1#*=}"; shift ;;
            --help)          show_help; exit 0 ;;
            *)               error "unknown option: $1"; show_help; exit 2 ;;
        esac
    done

    ORIGIN_HOST="identity.${RP_ID}"
    ORIGIN="https://${ORIGIN_HOST}:${PORT}"
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function check_tools() {
    section "Checking tools"
    local missing=() tool
    for tool in go mkcert; do
        command -v "$tool" >/dev/null || missing+=("$tool")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        error "missing: ${missing[*]}"
        error "mkcert installs pinned + digest-verified with:"
        error "    bash ${REPO_ROOT}/scripts/install/install-binary.sh --tool=mkcert"
        error "    export PATH=\"\$HOME/.memql/bin:\$PATH\""
        die 4 "missing tools: ${missing[*]}"
    fi
    info "go, mkcert: present"
}

# check_resolution catches the failure that otherwise shows up as an
# inexplicable connection refused AFTER the server says it is listening.
#
# macOS resolves *.localhost to 127.0.0.1 by itself, and Chrome maps it
# internally on every platform regardless of the resolver -- so the /etc/hosts
# entry the README documents is genuinely unnecessary on a Mac and genuinely
# necessary for Firefox on Linux. Rather than guess, ask the resolver.
function check_resolution() {
    section "Checking name resolution for ${ORIGIN_HOST}"
    if getent hosts "$ORIGIN_HOST" >/dev/null 2>&1 || ping -c1 -W1 "$ORIGIN_HOST" >/dev/null 2>&1; then
        info "${ORIGIN_HOST} resolves"
        return 0
    fi
    warn "${ORIGIN_HOST} does not resolve on this machine."
    warn "Chrome maps *.localhost internally and will work anyway; Firefox and curl will not."
    warn "If you need them, add (one sudo, once):"
    warn "    127.0.0.1  identity.${RP_ID} api.${RP_ID} bff.${RP_ID}"
}

# ensure_certificate issues the wildcard pair, creating the local CA if this
# machine has none.
#
# NOT `mkcert -install`: that writes to the SYSTEM trust store and wants a
# password, and the spike does not need it -- issuing a certificate creates the
# CA on its own. Browser trust is a separate question, handled per browser
# below, and saying so here is what keeps a sudo prompt out of an operator's
# way for a throwaway harness.
function ensure_certificate() {
    section "Issuing the local certificate"
    mkdir -p "$WORK_DIR"

    # mkcert names two-SAN output with a `+1` suffix. Deriving the name rather
    # than hardcoding it is what makes --rp-id work at all.
    CERT_FILE="${WORK_DIR}/_wildcard.${RP_ID}+1.pem"
    KEY_FILE="${WORK_DIR}/_wildcard.${RP_ID}+1-key.pem"

    if [[ -s "$CERT_FILE" && -s "$KEY_FILE" ]]; then
        info "certificate present; reusing ${CERT_FILE}"
        return 0
    fi

    ( cd "$WORK_DIR" && mkcert "*.${RP_ID}" "${RP_ID}" >&2 ) || die 5 "mkcert failed"
    [[ -s "$CERT_FILE" && -s "$KEY_FILE" ]] \
        || die 5 "mkcert reported success but ${CERT_FILE} is missing -- check its output for the names it actually wrote"
    info "certificate: ${CERT_FILE}"
}

#=============================================================================
# HEADLESS PROBE (leg 1, no human)
#=============================================================================

# firefox_profile builds a throwaway profile that trusts the mkcert CA and has
# a software authenticator.
#
# TRUST. Firefox keeps its own NSS trust store, so a CA in the system store
# means nothing to it -- which is the whole reason `mkcert -install` shells out
# to certutil for Firefox separately. Writing the CA into a PROFILE WE CREATED
# needs no sudo and touches nothing the operator uses, and a throwaway profile
# also means no saved passkeys, no extensions, and no prior state to explain a
# result away with.
#
# THE SOFT TOKEN, AND WHY THE PROBE IS USELESS WITHOUT IT. Measured on Firefox
# 153: with no authenticator present, Gecko rejects `create()` with
# NotAllowedError for EVERY rp.id -- including `example.com`, which must be a
# SecurityError. The negative control failing is the whole signal: it says the
# RP ID validator is not what answered, so nothing that run produced was a
# measurement of it. (This is the same reason the Chrome leg used an abort:
# reach the validator without needing an authenticator to exist. Chrome's
# validator runs early enough for that; Gecko's ordering does not.)
#
# `webauthn_enable_softtoken` is Gecko's built-in software authenticator, used
# by its own test suite. It consents without a human, which lets the ceremony
# proceed far enough for the RP ID verdict to be the thing that comes back --
# a SecurityError where the RP ID is refused, a credential where it is not.
# The USB token is turned off so a security key the operator happens to have
# plugged in cannot answer instead and make the run unreproducible.
function firefox_profile() {
    local profile="$1" caroot
    caroot="$(mkcert -CAROOT)"
    [[ -f "${caroot}/rootCA.pem" ]] || die 4 "no mkcert CA at ${caroot}/rootCA.pem"

    rm -rf "$profile"
    mkdir -p "$profile"
    certutil -N -d "sql:${profile}" --empty-password >/dev/null 2>&1 \
        || die 5 "could not create an NSS database in ${profile}"
    certutil -A -n "mkcert-memql-spike" -t "C,," -i "${caroot}/rootCA.pem" -d "sql:${profile}" \
        || die 5 "could not add the mkcert CA to the throwaway profile"

    cat > "${profile}/user.js" <<'PREFS'
user_pref("security.webauth.webauthn_enable_softtoken", true);
user_pref("security.webauth.webauthn_enable_usbtoken", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
PREFS

    info "throwaway firefox profile trusts ${caroot}/rootCA.pem, soft token enabled"
}

# probe_firefox loads the page once and waits for it to finish recording.
#
# It waits on the RESULTS FILE, not on a timer. The page marks itself complete
# only after every probe's POST has been awaited, so the file growing to four
# gecko rows is the honest completion signal; a sleep would either be too short
# (a truncated table nobody notices) or padded to cover the worst case.
function probe_firefox() {
    section "Probing leg 1 in headless Firefox"
    command -v firefox >/dev/null || die 4 "firefox is not installed"
    command -v certutil >/dev/null || die 4 "certutil is not installed (apt: libnss3-tools)"

    local profile="${WORK_DIR}/firefox-profile"
    firefox_profile "$profile"

    local before after deadline
    before="$(count_gecko_rows)"

    info "loading ${ORIGIN} ..."
    firefox --headless --profile "$profile" --new-instance "${ORIGIN}/" >/dev/null 2>&1 &
    local ff_pid=$!

    deadline=$(( $(date +%s) + 90 ))
    while :; do
        after="$(count_gecko_rows)"
        [[ "$after" -ge $(( before + 4 )) ]] && break
        if [[ "$(date +%s)" -ge "$deadline" ]]; then
            kill "$ff_pid" 2>/dev/null || true
            die 5 "firefox recorded ${after} Gecko rows (wanted $(( before + 4 ))) before the 90s deadline -- see ${WORK_DIR}/results.md"
        fi
        sleep 2
    done

    kill "$ff_pid" 2>/dev/null || true
    wait "$ff_pid" 2>/dev/null || true
    info "recorded $(( after - before )) Gecko rows"
    assert_controls_discriminated "Gecko"
}

function count_gecko_rows() {
    local file="${WORK_DIR}/results.jsonl"
    [[ -f "$file" ]] || { echo 0; return 0; }
    # `grep -c` PRINTS "0" and EXITS 1 when nothing matches, so the obvious
    # `|| echo 0` emits a SECOND zero -- the caller then does arithmetic on a
    # two-line value, which fails, and under `set -e` the whole run ends
    # silently at that point with no message. Which is exactly what happened
    # the first time, and cost a debugging pass to see.
    local n
    n="$(grep -c 'Gecko' "$file" 2>/dev/null || true)"
    echo "${n:-0}"
}

# assert_controls_discriminated refuses to call a run a measurement when the
# controls did not fire.
#
# THIS IS THE MOST IMPORTANT FUNCTION IN THE FILE, and it exists because the
# first Gecko run produced four tidy rows that meant nothing. Firefox 153 with
# no authenticator answers `create()` with NotAllowedError for EVERY rp.id --
# `example.com` included. A NEGATIVE CONTROL THAT IS NOT REJECTED PROVES THE RP
# ID VALIDATOR IS NOT WHAT ANSWERED, so every other row in that run is a
# reading off an instrument that was not connected.
#
# Without this check the script exits 0, the table fills with plausible rows,
# and "we measured Gecko" enters the record. The page already refuses to guess
# (it reports `inconclusive` rather than folding an unexpected error name into
# accepted or rejected); this makes the SCRIPT refuse too, because an exit code
# is what a person actually reads.
#
# Exit 5, not 0: the run failed to measure. That is a different thing from
# measuring a negative, and the spike needs to be able to tell them apart.
function assert_controls_discriminated() {
    local engine="$1" file="${WORK_DIR}/results.jsonl"
    [[ -f "$file" ]] || die 5 "no results were recorded at all"

    # The negative control must have been REJECTED. grep over the JSONL rather
    # than parsing: one line, two fields, and jq is not a prerequisite anywhere
    # else in this script.
    if grep '"rpId":"example.com"' "$file" | grep -q '"outcome":"rejected"'; then
        info "controls discriminated: example.com was rejected, so the validator answered"
        return 0
    fi

    error "NOT A MEASUREMENT. The negative control (example.com) was not rejected."
    error ""
    error "  A browser's RP ID validator MUST refuse example.com at this origin. If it"
    error "  did not, then whatever answered these probes was not the validator, and"
    error "  every row this run recorded for ${engine} is a reading off a disconnected"
    error "  instrument -- including any that happen to look like the expected answer."
    error ""
    error "  Known cause: a browser with no authenticator available rejects every"
    error "  ceremony before the RP ID is ever considered. Firefox 153 headless does"
    error "  this even with security.webauth.webauthn_enable_softtoken set."
    error ""
    error "  The rows are still in ${WORK_DIR}/results.md, marked 'inconclusive'."
    error "  Do not report them as a browser-leg result."
    die 5 "controls did not discriminate for ${engine}"
}

#=============================================================================
# SERVE
#=============================================================================

function start_server() {
    ( cd "$REPO_ROOT" && go run ./scripts/spikes/webauthn-rpid \
        --rp-id="$RP_ID" \
        --addr="127.0.0.1:${PORT}" \
        --cert="$CERT_FILE" \
        --key="$KEY_FILE" \
        --origin="$ORIGIN" \
        --results="$WORK_DIR" ) &
    SERVER_PID=$!
    trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT

    # Wait for the port rather than sleeping: `go run` compiles first, and how
    # long that takes depends on a build cache we know nothing about.
    local deadline=$(( $(date +%s) + 60 ))
    # The probe runs in a SUBSHELL, so the fd it opens dies with it and there is
    # nothing to close afterwards.
    #
    # The obvious cleanup line -- `exec 3<&- 2>/dev/null` -- is a trap worth
    # naming, because it cost a debugging pass here: `exec` with no command
    # applies its redirections to the CURRENT SHELL, permanently. That line
    # closes fd 3 and then points this script's stderr at /dev/null for the rest
    # of the run, so every info() and error() after it vanishes. The symptom is
    # a script that exits 5 having printed nothing about why.
    while ! (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null; do
        [[ "$(date +%s)" -lt "$deadline" ]] || die 5 "the harness did not start listening on ${PORT} within 60s"
        kill -0 "$SERVER_PID" 2>/dev/null || die 5 "the harness exited during startup -- see its output above"
        sleep 1
    done
    info "harness listening on 127.0.0.1:${PORT}"
}

function print_next_steps() {
    cat >&2 <<EOF

=== Open this ===

    ${ORIGIN}/

Leg 1 (the browser's RP ID validator) measures itself on load and is recorded
before you touch anything. Read the two control rows first: example.com must be
REJECTED, and so must localhost.

Leg 2 needs you:

    1. platform authenticator      -- Touch ID / Windows Hello, no phone
    2. hybrid -> iOS               -- scan the QR with an iPhone
    3. hybrid -> Android           -- scan the QR with an Android phone
    5. usernameless assertion      -- proves discoverable login works

Hybrid transport needs BLUETOOTH between the phone and this machine. Same Wi-Fi
is not a substitute and not a fallback.

Results, written as they happen:

    ${WORK_DIR}/results.md

Then run the control, which tells apart "\`.localhost\` is the problem" from
"local origins are the problem":

    $0 --control

Ctrl-C when you are done.
EOF
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    parse_arguments "$@"
    check_tools
    check_resolution
    ensure_certificate

    start_server

    if [[ "$PROBE_FIREFOX" -eq 1 ]]; then
        probe_firefox
        section "Leg 1, Gecko"
        sed -n '/Gecko/,/^$/p' "${WORK_DIR}/results.md" >&2 || true
        info "full table: ${WORK_DIR}/results.md"
        return 0
    fi

    print_next_steps
    wait "$SERVER_PID"
}

main "$@"
