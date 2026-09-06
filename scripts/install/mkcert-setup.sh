#!/usr/bin/env bash
#
# scripts/install/mkcert-setup.sh
# ===============================
#
# Capability: install.mkcert -- ensure a trusted local CA and issue the front
# door's wildcard certificate.
#
# The local cluster is reached exactly as staging is: over TLS, at
# https://api.memql.localhost and https://identity.memql.localhost (env
# parity -- docs/public/operate/environment-parity.md). Traefik terminates
# that with the browser-trusted `*.memql.localhost` pair which
# scripts/k3d/seed-secrets.sh loads into the cluster as the memql-front-door-tls
# secret. This capability produces that pair.
#
# THE RESTRAINT THAT MATTERS
#
# mkcert's root CA is PER MACHINE, not per project. If one already exists it
# may be signing certificates for half a dozen other local stacks the operator
# depends on, so this script NEVER regenerates one: when $CAROOT/rootCA.pem is
# present it reports caPreExisting=true / caInstalled=false and the file's
# bytes are the same afterwards. Installing MemQL must never be the reason
# someone's other local projects start failing TLS.
#
# WHAT THAT RESTRAINT USED TO ALSO COVER, WRONGLY (memql#3560). It used to mean
# "and do not run `mkcert -install` either". That does not follow. `-install`
# never regenerates a CA -- it creates one only when none exists, and otherwise
# adds the existing one to trust stores that are missing it. It cannot make
# another project's certificates fail; the harm the restraint names comes from
# REGENERATION, which is guarded separately and still is.
#
# Meanwhile the cost of not running it was real. `mkcert -install` creates the
# CA and THEN trusts it, so an interrupted first run -- on Linux the trust half
# needs root, and a child process with no terminal cannot ask for a password --
# leaves rootCA.pem on disk and the trust store empty. The next run saw the
# file, said "pre-existing, leave it alone", issued a certificate against an
# UNTRUSTED CA and reported success. The install then completed and every
# browser refused the front door. So trust is ENSURED on every run, and
# result.caTrusted is what the graph verifies.
#
# BROWSER TRUST IS PART OF THE JOB, NOT A BONUS. The install ends by handing the
# operator a link to open. On Linux, Firefox and Chrome read their own NSS store
# rather than the system one, and mkcert can only write it through `certutil`
# (libnss3-tools / nss-tools / nss). Without that binary mkcert prints a warning
# and exits 0 -- a success that leaves the front door untrusted in exactly the
# application the operator is about to use. It is treated here as the missing
# prerequisite it is, with --allow-missing-certutil for a machine that genuinely
# has no browser.
#
# When there is NO CA, creating one writes to the machine's trust store, so
# that step is gated on --confirm=install-memql-ca. There is no prompt
# (contract rule 3); mkcert's own sudo prompt is the OS-level gate, this is the
# capability-level one. A run that only issues a certificate against an
# existing CA needs no confirmation, because it changes nothing shared.
#
# WHAT THIS LEAVES ON THE MACHINE, and why the list is written down
# (memql#4071). Six files, in two directories:
#
#   $CAROOT/rootCA.pem, rootCA-key.pem   the CA
#   $CAROOT/.memql-created               its provenance marker (memql#3576)
#   --cert-file, --key-file              the front-door certificate and key
#   $(dirname --cert-file)/.memql-issued the PAIR's provenance marker
#
# The uninstall's `kind=mkcertCA` removes exactly this set, and the equality is
# the whole point: the graph invariants (scripts/install/graph/graph_test.go)
# assert that this step HAS a reversal and cannot assert that the reversal covers
# everything it WROTE. It did not -- the pair was left behind, so an uninstall
# that reported OK on every step left a private key at ~/.memql/certs/dev.key.
# Adding a file here means adding it there.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/mkcert-setup.sh --confirm=install-memql-ca
#   scripts/install/mkcert-setup.sh --hostnames='*.memql.localhost,memql.localhost' \
#       --cert-file=/path/dev.crt --key-file=/path/dev.key
#   scripts/install/mkcert-setup.sh --force        # reissue an existing pair
#   scripts/install/mkcert-setup.sh --print-spec
#
# Exit codes:
#   0 ok | 2 bad param | 3 refused (CA install without --confirm)
#   4 prerequisite missing (mkcert absent; certutil absent; trust needs a
#     password and there is no terminal to ask on -- result.remedy carries the
#     command that finishes it)
#   5 operation failed (mkcert errored, or produced nothing)
#
# Refs: #3560 #3362 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/elevate.sh
source "${SCRIPT_DIR}/../lib/elevate.sh"
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "install.mkcert" "Ensure a trusted local CA and issue the front-door wildcard certificate."
cap_spec_param "hostnames" "comma/space separated names for the cert (default: the front-door wildcard + apex)"
cap_spec_param "domain"    "front-door apex; derives the wildcard + apex SANs (mutually exclusive with --hostnames)"
cap_spec_param "cert-file" "where to write the certificate"
cap_spec_param "key-file"  "where to write the private key"
cap_spec_param "caroot"    "mkcert CAROOT to use (default: whatever mkcert reports)"
cap_spec_param "mkcert"    "path to the mkcert binary (default: resolved from PATH)"
cap_spec_param "force"     "reissue the certificate even when one exists (flag)"
cap_spec_param "confirm"   "exact phrase 'install-memql-ca'; required only when no CA exists yet"
cap_spec_param "certutil"  "path to the NSS certutil binary (default: resolved from PATH)"
cap_spec_param "allow-missing-certutil" "proceed without browser (NSS) trust on Linux (flag)"

# Hostnames and the on-disk pair location are declared once, in scripts/lib, so
# this issuer and the seeder that loads the pair into the cluster cannot drift
# apart -- a drifted default is exactly how memql#3384 happened.
readonly DEFAULT_HOSTNAMES="$MEMQL_LOCAL_TLS_HOSTNAMES"
readonly CONFIRM_INSTALL_CA="install-memql-ca"

# The provenance marker, beside the key material it describes. Shared with
# remove-artifact.sh, which reads it to decide whether the CA is MemQL's to
# take (memql#3576).
readonly MEMQL_CA_MARKER=".memql-created"

# THE SAME IDEA, FOR THE OTHER ARTIFACT THIS CAPABILITY WRITES (memql#4071).
#
# This script produces two things in two different directories: a CA in CAROOT,
# and the front-door pair in the cert directory. The uninstall took back the
# first and left the second, so a machine that had never had a ~/.memql before
# the install kept ~/.memql/certs/dev.crt and, worse, dev.key -- a private key,
# after an uninstall that reported OK on every step.
#
# Fixing that needs an answer to "may this pair be removed", and the step's
# receipt cannot give one: a receipt records ONE pre-existence verdict per step,
# and localCA's is `result.caPreExisting` -- a fact about the CA, in another
# directory, whose answer differs from the pair's in BOTH directions. An
# operator can hold a CA of their own while MemQL issues the pair, and can hold
# a pair of their own (a plain `make up` issues into the very same
# ~/.memql/certs) while MemQL creates the CA.
#
# So the pair gets its own marker, for the reasons memql#3576 gave the CA one:
# it is written by the run that issued, it is a fact about the artifact so it is
# stored WITH the artifact, it survives the receipt an uninstall deletes, and it
# disappears with what it describes. A DISTINCT filename from the CA's, because
# nothing stops an operator pointing --caroot and --cert-file at one directory,
# and a shared marker there would let either artifact's provenance answer for
# the other.
readonly MEMQL_PAIR_MARKER=".memql-issued"

# Flags this run was given that a remedy line has to repeat verbatim, so the
# command handed to the operator refuses or succeeds for the same reasons the
# run did. Filled in by main.
_MKCERT_REMEDY_FLAGS=""

#=============================================================================
# PARAMETER VALIDATION
#=============================================================================

HOSTNAMES=()

function parse_hostnames() {
    local raw="$1" host
    raw="${raw//,/ }"
    HOSTNAMES=()
    # Word splitting is the point here.
    # shellcheck disable=SC2206
    local parts=( $raw )
    for host in "${parts[@]:-}"; do
        [[ -z "$host" ]] && continue
        # A leading '*.' label is the only wildcard form mkcert (and TLS) can
        # do; anything else with a '*' in it would produce a certificate that
        # silently matches nothing.
        if [[ ! "$host" =~ ^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
            cap_fail 2 "invalid hostname: '${host}' (a leading '*.' label, then letters, digits, '.' and '-')"
        fi
        HOSTNAMES+=("$host")
    done
    if [[ "${#HOSTNAMES[@]}" -eq 0 ]]; then
        cap_fail 2 "no hostnames given: --hostnames must name at least one host"
    fi
}

#=============================================================================
# PREREQUISITES
#=============================================================================

# NOTE: the resolve_* helpers set a global rather than printing their result.
# cap_fail inside a "$(...)" substitution would emit its envelope into the
# capture instead of onto stdout, and the caller would abort into the trap's
# generic "aborted without an explicit result" -- an honest exit code carrying
# a useless message, which for a missing prerequisite is exactly the message
# that had to be useful. Anything that can fail runs in the parent shell.
MKCERT_BIN=""
CAROOT_DIR=""

# resolve_mkcert <candidate> -- sets MKCERT_BIN, or fails 4. mkcert is
# deliberately NOT auto-installed: it writes to the system trust store, so
# which package manager touches this machine is the operator's call.
function resolve_mkcert() {
    local candidate="$1"
    if ! MKCERT_BIN="$(command -v "$candidate" 2>/dev/null)" || [[ -z "$MKCERT_BIN" ]]; then
        cap_fail 4 "mkcert not found (looked for '${candidate}'); install it first: brew install mkcert  |  https://github.com/FiloSottile/mkcert"
    fi
}

# resolve_caroot <mkcert-bin> <override> -- sets CAROOT_DIR.
function resolve_caroot() {
    local bin="$1" override="$2"
    if [[ -n "$override" ]]; then
        CAROOT_DIR="$override"
        return
    fi
    if ! CAROOT_DIR="$("$bin" -CAROOT 2>/dev/null)" || [[ -z "$CAROOT_DIR" ]]; then
        cap_fail 5 "could not determine the mkcert CAROOT ('${bin} -CAROOT' produced nothing)"
    fi
}

#=============================================================================
# BROWSER TRUST (NSS)
#=============================================================================

# nss_package_hint -- the install command for certutil on THIS machine, or an
# empty string when no package manager we know about is present. The remedy
# names one command rather than listing five, because the operator is reading
# it to type it.
function nss_package_hint() {
    if command -v apt-get &>/dev/null; then
        printf 'sudo apt-get install -y libnss3-tools\n'
    elif command -v dnf &>/dev/null; then
        printf 'sudo dnf install -y nss-tools\n'
    elif command -v yum &>/dev/null; then
        printf 'sudo yum install -y nss-tools\n'
    elif command -v pacman &>/dev/null; then
        printf 'sudo pacman -S --noconfirm nss\n'
    elif command -v zypper &>/dev/null; then
        printf 'sudo zypper install -y mozilla-nss-tools\n'
    fi
}

# require_browser_trust_tooling <certutil-candidate> <allow-missing>
#
# On Linux, Firefox and Chrome do not read the system trust store -- they read
# their own NSS database, and mkcert can only write it through certutil. Without
# it `mkcert -install` still exits 0, so this is checked HERE or not at all.
#
# Checked BEFORE anything is created: a run that is going to refuse must not
# leave a half-made CA behind, which is the exact shape of the bug this file's
# header describes.
function require_browser_trust_tooling() {
    local candidate="$1" allow_missing="$2" hint

    if command -v "$candidate" &>/dev/null; then
        return 0
    fi
    if [[ "$(uname -s 2>/dev/null || true)" != "Linux" ]]; then
        # macOS trusts through the system keychain, which Safari and Chrome
        # both read; only Firefox needs NSS there. Not worth refusing over.
        cap_info "certutil not found -- Firefox will not trust the local CA (Safari/Chrome will)."
        return 0
    fi
    if [[ -n "$allow_missing" ]]; then
        cap_info "certutil not found and --allow-missing-certutil was passed -- no browser will trust the local CA."
        return 0
    fi

    hint="$(nss_package_hint)"
    if [[ -n "$hint" ]]; then
        cap_result_set remedy "$hint"
    else
        # No package manager we recognise. Name the packages instead of a
        # command, rather than printing a command that will not work here.
        hint="the NSS tools package for this system (libnss3-tools / nss-tools / nss)"
    fi
    cap_fail 4 "certutil not found: Firefox and Chrome read their own NSS trust store on Linux, and mkcert needs certutil to write it -- without it the cluster front door is untrusted in every browser. Install it with: ${hint} -- or pass --allow-missing-certutil on a machine with no browser."
}

#=============================================================================
# THE CA
#=============================================================================

# self_command -- this script's own absolute path, for a remedy line the
# operator can paste. Resolved from BASH_SOURCE so it is right whether the
# script was invoked from a checkout or from the staged copy inside a packaged
# extension.
function self_command() {
    printf '%s' "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
}

# looks_like_an_elevation_failure <output> -- did mkcert fail because it needed
# a password and had nowhere to ask?
#
# Matched on mkcert's own words plus sudo's. The classification decides between
# exit 4 ("here is the command that finishes it") and exit 5 ("this one may be
# transient, retry"), and getting it wrong is how an operator ends up retrying
# a thing that will never succeed unattended.
function looks_like_an_elevation_failure() {
    local lower="${1,,}"
    [[ "$lower" == *"a terminal is required"* ]] && return 0
    [[ "$lower" == *"a password is required"* ]] && return 0
    [[ "$lower" == *"askpass"* ]] && return 0
    [[ "$lower" == *"no tty present"* ]] && return 0
    [[ "$lower" == *"sudo"*"password"* ]] && return 0
    return 1
}

# replay_mkcert_output <text> -- mkcert's own output, with ONE line demoted.
#
# mkcert writes `ERROR: no Firefox and/or Chrome/Chromium security databases
# found` to stderr when it finds no NSS profile to install the CA into, and
# then EXITS 0 -- because on a machine with no browser profile there is nothing
# to install into and nothing has gone wrong. We replayed it verbatim, so the
# word ERROR appeared under `[localCA]` and again under `[clusterUp]` on every
# headless run, green ones included (memql#5029). When a later step then failed
# for an unrelated reason, that adjacency is what sent the next investigation
# at the certificate authority -- twice, across two sessions.
#
# NOT SUPPRESSED, and not conditioned on "is a browser present": the message is
# true and occasionally useful, and the "no certutil at all" decision is
# already owned by require_browser_trust_tooling above. Only its SEVERITY was
# wrong. Everything else mkcert says is replayed unchanged, because this
# function must not become a filter on output nobody has read.
function replay_mkcert_output() {
    local line
    while IFS= read -r line; do
        if [[ "$line" == *"security databases found"* ]]; then
            cap_info "mkcert: ${line#ERROR: } (no browser profile on this machine -- mkcert exits 0 and the CA is still installed for command-line clients)"
            continue
        fi
        printf '%s\n' "$line" >&2
    done <<<"$1"
}

# ensure_ca_trusted <mkcert-bin> <caroot> <cert> <key> <confirm>
#
# Runs `mkcert -install`, which is idempotent: it creates a CA only when none
# exists, adds it to any trust store missing it, and prints "already installed"
# otherwise. Running it unconditionally is what makes a created-but-untrusted CA
# (the memql#3560 failure) repair itself on the next run instead of being
# mistaken for a healthy one.
function ensure_ca_trusted() {
    local bin="$1" caroot="$2" cert="$3" key="$4" confirm="$5" out=""

    cap_step "ensuring the local CA is trusted (CAROOT=${caroot})"

    # mkcert SHELLS OUT TO SUDO for the trust-store write, and sudo with no
    # terminal reads the password from $SUDO_ASKPASS. Exporting that variable is
    # the entire integration -- MemQL does not wrap, re-implement or elevate
    # mkcert, it gives mkcert's own sudo a way to ask (memql#3562).
    #
    # NOT `sudo mkcert -install`, which would be the obvious thing and is wrong:
    # mkcert resolves CAROOT from the running user's home, so as root it would
    # build a SECOND CA under /root, trusted for a user who is not the one
    # running a browser. mkcert stays the operator; only the write becomes root.
    if elevate_begin "trust the local certificate authority"; then
        cap_info "mkcert needs your password to write the trust store -- $(elevate_explain)."
    else
        cap_info "mkcert may ask for your password -- it is writing to the trust store."
    fi

    if out="$(CAROOT="$caroot" "$bin" -install 2>&1)"; then
        elevate_end
        [[ -n "$out" ]] && replay_mkcert_output "$out"
        if [[ ! -f "${caroot}/rootCA.pem" ]]; then
            cap_fail 5 "mkcert -install reported success but ${caroot}/rootCA.pem is missing"
        fi
        return 0
    fi
    elevate_end
    replay_mkcert_output "$out"

    if looks_like_an_elevation_failure "$out"; then
        # NOT `sudo <this script>`: run as root, mkcert would resolve CAROOT
        # under root's home and build a second CA there, trusted for a user who
        # is not the one browsing. The script runs AS THE OPERATOR, in a
        # terminal, where mkcert's own sudo can prompt.
        #
        # It carries the flags THIS run was given, including the waiver: a
        # remedy that drops one refuses for a different reason than the run it
        # is meant to finish.
        cap_result_set remedy "$(printf '%q' "$(self_command)") --confirm=$(printf '%q' "$confirm") --caroot=$(printf '%q' "$caroot") --cert-file=$(printf '%q' "$cert") --key-file=$(printf '%q' "$key")${_MKCERT_REMEDY_FLAGS}"
        if [[ "$(elevate_method)" == "none" ]]; then
            cap_fail 4 "trusting the local CA needs your password, and $(elevate_no_ask_reason). Run the command below in a terminal -- it is the same step, where mkcert can prompt -- then retry."
        fi
        cap_fail 4 "trusting the local CA needs your password, and the prompt was cancelled or the password was not accepted. Retry, or run the command below in a terminal."
    fi
    cap_fail 5 "mkcert -install failed"
}

#=============================================================================
# THE CERTIFICATE
#=============================================================================

# cert_sans_oneline <cert-path> -- the certificate's subjectAltName names on one
# line, or empty when they cannot be read. Empty means "cannot tell", never
# "carries nothing".
function cert_sans_oneline() {
    local cert="$1" sans
    [[ -f "$cert" ]] || return 0
    command -v openssl &>/dev/null || return 0
    sans="$(openssl x509 -in "$cert" -noout -ext subjectAltName 2>/dev/null || true)"
    [[ -n "$sans" ]] || return 0
    printf '%s' "$sans" | tr '\n' ' ' | tr -s ' '
}

# cert_coverage <cert-path> -- THREE states, printed:
#
#   covers   the SANs were read and carry every name in HOSTNAMES
#   missing  the SANs were read and at least one required name is absent
#   unknown  they could not be read at all (no file, no openssl, not a
#            certificate, no subjectAltName extension)
#
# WHY THREE AND NOT TWO. Existence alone is not enough to reuse a pair
# (memql#3593): one on disk was issued for whatever domain the last run used, and
# reusing it for a different one leaves traefik serving a certificate for a name
# nobody dialed -- a TLS failure against a hostname the operator typed
# themselves. But "cannot tell" is not "does not cover", and it is not "covers"
# either. Collapsing it into either one produces a false statement: reissuing
# whenever unsure destroys idempotency, and REPORTING coverage we never
# established is the memql#3730 defect itself. So the third state is named and
# travels into the result envelope as coverageVerified, where every caller can
# see the difference instead of guessing at it.
function cert_coverage() {
    local cert="$1" sans host
    sans="$(cert_sans_oneline "$cert")"
    if [[ -z "$sans" ]]; then
        printf 'unknown'
        return 0
    fi
    for host in "${HOSTNAMES[@]}"; do
        if ! printf '%s' "$sans" | grep -Fq "DNS:${host}"; then
            printf 'missing'
            return 0
        fi
    done
    printf 'covers'
}

# cert_mismatches <cert-path> -- true ONLY when the certificate demonstrably
# does not cover every name in HOSTNAMES. The reuse decision, phrased
# negatively: "unknown" keeps the pair, because a capability that reissued
# whenever it was unsure would stop being idempotent -- and a second identical
# run reporting changed=true is its own contract violation. Only a certificate
# we could read, and whose SANs are missing a name we need, is replaced.
function cert_mismatches() {
    [[ "$(cert_coverage "$1")" == "missing" ]]
}

function issue_cert() {
    local bin="$1" caroot="$2" cert="$3" key="$4"
    mkdir -p "$(dirname "$cert")" "$(dirname "$key")"
    cap_step "issuing ${cert} for: ${HOSTNAMES[*]}"
    if ! CAROOT="$caroot" "$bin" -cert-file "$cert" -key-file "$key" "${HOSTNAMES[@]}" >&2; then
        cap_fail 5 "mkcert failed to issue the certificate"
    fi
    if [[ ! -f "$cert" || ! -f "$key" ]]; then
        cap_fail 5 "mkcert reported success but ${cert} / ${key} are missing"
    fi
}

# mark_pair_as_ours <cert> -- claim the issued pair, so an uninstall may take it
# back (memql#4071).
#
# Stamped AFTER the pair exists, like the CA's marker and for the same reason: a
# run that died issuing must leave no claim on key material it did not finish
# writing.
#
# The caller decides WHETHER to claim; this only writes. That split matters,
# because the claim is only honest when the pair was created where there was
# nothing -- see the read of `pair_on_disk` in main.
function mark_pair_as_ours() {
    local marker
    marker="$(dirname "$1")/${MEMQL_PAIR_MARKER}"
    [[ -f "$marker" ]] && return 0
    printf 'MemQL issued the front-door certificate and key beside this file.\nRemoving this file makes an uninstall leave them in place.\n' > "$marker" || true
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local hostnames_raw domain cert key caroot_override mkcert_bin force confirm
    local certutil_bin allow_missing_certutil
    # --domain and --hostnames are two spellings of one answer (memql#3593), the
    # same rule hosts-entries.sh applies. Both is a contradiction, and the two
    # scripts must not resolve it differently: the certificate has to cover what
    # the hosts block points at.
    domain="$(cap_param domain "")"
    hostnames_raw="$(cap_param hostnames "")"
    if [[ -n "$domain" && -n "$hostnames_raw" ]]; then
        cap_fail 2 "--domain and --hostnames are two spellings of one answer; pass one"
    fi
    if [[ -n "$domain" ]]; then
        # Through localtls.sh's one derivation, so --domain here and --domain in
        # the seeder cannot produce different SAN lists (memql#3730).
        hostnames_raw="$(localtls_hostnames_for "$domain")"
    elif [[ -z "$hostnames_raw" ]]; then
        hostnames_raw="$DEFAULT_HOSTNAMES"
    fi
    cert="$(cap_param cert-file "${MEMQL_LOCAL_TLS_CERT:-$MEMQL_LOCAL_TLS_DEFAULT_CERT}")"
    key="$(cap_param key-file  "${MEMQL_LOCAL_TLS_KEY:-$MEMQL_LOCAL_TLS_DEFAULT_KEY}")"
    caroot_override="$(cap_param caroot "")"
    mkcert_bin="$(cap_param mkcert "mkcert")"
    force="$(cap_flag force)"
    confirm="$(cap_param confirm "")"
    certutil_bin="$(cap_param certutil "certutil")"
    allow_missing_certutil="$(cap_flag allow-missing-certutil)"

    parse_hostnames "$hostnames_raw"
    cap_require cert-file "$cert"
    cap_require key-file  "$key"

    resolve_mkcert "$mkcert_bin"
    local bin="$MKCERT_BIN"
    resolve_caroot "$bin" "$caroot_override"
    local caroot="$CAROOT_DIR"

    # The central question, asked before anything is touched: is there already
    # a CA on this machine? The answer decides whether this run CREATES shared
    # machine state (gated on the phrase) or merely completes trust for one that
    # is already there.
    # WHO CREATED THIS CA -- asked of the CA itself, not of a receipt
    # (memql#3576).
    #
    # The verdict used to be "is rootCA.pem here", recorded once into the
    # install receipt. That receipt is DELETED by an uninstall, so anything the
    # uninstall refused to remove -- which is exactly the CA it could not prove
    # was ours -- came back on the next install as "already here", truthfully,
    # forever. A ratchet: one run that could not tell, and the answer is locked
    # in because the uninstall that would have cleared it is the thing being
    # blocked.
    #
    # A marker beside the key material has none of that. It is written by the
    # run that creates the CA, it survives every receipt, and it disappears with
    # the CA it describes. The question "did MemQL create this" is a fact about
    # the artifact, so it is stored with the artifact.
    # TWO DIFFERENT QUESTIONS, and conflating them made a re-run claim it had
    # installed a CA that was already there:
    #
    #   ca_on_disk      -- was there a CA before THIS run? decides `changed`
    #                      and caInstalled.
    #   ca_pre_existing -- was it here before MEMQL ever touched this machine?
    #                      decides whether an uninstall may take it.
    #
    # A CA MemQL created on an earlier run is on disk (so this run installs
    # nothing) and NOT pre-existing (so it is still ours to remove).
    local ca_on_disk=false ca_pre_existing=false ca_installed=false
    if [[ -f "${caroot}/rootCA.pem" ]]; then
        ca_on_disk=true
        if [[ -f "${caroot}/${MEMQL_CA_MARKER}" ]]; then
            cap_info "root CA at ${caroot} was created by MemQL -- reusing it."
        else
            ca_pre_existing=true
            cap_info "root CA already present at ${caroot}/rootCA.pem -- it will not be regenerated."
            cap_info "  (no ${MEMQL_CA_MARKER}: MemQL did not create it, so an uninstall will leave it)"
        fi
    else
        cap_confirm_or_die "$confirm" "$CONFIRM_INSTALL_CA"
    fi

    # Refuse BEFORE creating anything: a machine that cannot give browsers the
    # CA must not be left holding a half-finished one.
    require_browser_trust_tooling "$certutil_bin" "$allow_missing_certutil"
    if [[ -n "$allow_missing_certutil" ]]; then
        _MKCERT_REMEDY_FLAGS+=" --allow-missing-certutil"
    fi

    ensure_ca_trusted "$bin" "$caroot" "$cert" "$key" "$CONFIRM_INSTALL_CA"
    if [[ "$ca_on_disk" != "true" ]]; then
        ca_installed=true
        cap_changed
        # Stamped AFTER the CA exists, so a run that died creating one leaves no
        # claim on a CA it did not finish making.
        if [[ -f "${caroot}/rootCA.pem" && ! -f "${caroot}/${MEMQL_CA_MARKER}" ]]; then
            printf 'MemQL created this certificate authority. Removing this file makes an\nuninstall leave the CA in place.\n' > "${caroot}/${MEMQL_CA_MARKER}" || true
        fi
    fi

    local cert_issued=false reissued=false
    # WAS THERE A PAIR HERE BEFORE THIS RUN -- read now, for the same reason
    # coverage_before is: after issue_cert the answer is yes either way
    # (memql#4071).
    #
    # EITHER file counts, not both. This decides whether MemQL may later delete
    # key material, so the conservative reading is the right one: a directory
    # holding half a pair is a directory somebody else was using, and issuing
    # over it does not make what was there ours.
    #
    # A LATER RUN MUST NOT RE-ASK THE QUESTION, which is why the answer becomes a
    # marker on disk rather than a result field. `install.mkcert` runs at least
    # twice per install (localCA, then again inside k3d.up's seed-secrets), and
    # every repair runs it again; each of those finds the pair present and would
    # report "not ours". That is the exact ratchet memql#3576 described for the
    # CA -- a fact about the past, re-derived from the present, and wrong ever
    # after.
    local pair_on_disk=false
    if [[ -f "$cert" || -f "$key" ]]; then
        pair_on_disk=true
    fi
    # Read ONCE, before anything is written: after issue_cert the old names are
    # gone (mkcert overwrites in place), and they are the useful half of the
    # reissue message -- they name WHICH stale domain the operator had.
    local coverage_before old_sans
    coverage_before="$(cert_coverage "$cert")"
    old_sans="$(cert_sans_oneline "$cert")"
    if [[ -f "$cert" && -f "$key" && -z "$force" && "$coverage_before" != "missing" ]]; then
        if [[ "$coverage_before" == "covers" ]]; then
            cap_info "certificate already present at ${cert} and covers ${HOSTNAMES[*]} -- pass --force to reissue."
        else
            # Said, not implied. This is the branch that keeps a pair nothing
            # checked, and a caller echoing "already covers" here would be
            # asserting what it never established (memql#3730).
            cap_info "certificate already present at ${cert} -- pass --force to reissue."
            cap_warn "its names could NOT be read, so coverage of ${HOSTNAMES[*]} is unverified"
            cap_warn "  (openssl absent, or ${cert} does not parse as a certificate)."
        fi
    else
        if [[ -f "$cert" && -z "$force" && "$coverage_before" == "missing" ]]; then
            reissued=true
            # BOTH NAME SETS. "Does not cover X" alone leaves the operator
            # guessing which domain they were serving; the old SANs are what
            # identify the stale pair, and this is the last moment they exist.
            cap_warn "certificate at ${cert} carries ${old_sans}"
            cap_warn "  which does not cover ${HOSTNAMES[*]} -- reissuing."
        fi
        issue_cert "$bin" "$caroot" "$cert" "$key"
        cert_issued=true
        cap_changed
        if [[ "$pair_on_disk" != "true" ]]; then
            mark_pair_as_ours "$cert"
        fi
    fi

    # coverageVerified describes THE PAIR THIS RUN LEAVES IN PLACE: true only
    # when its SANs were read and carry every requested name. Re-read after
    # issuance rather than assumed from it, so the field is a measurement in
    # every branch -- which is the whole point of having it.
    local coverage_verified=false
    [[ "$(cert_coverage "$cert")" == "covers" ]] && coverage_verified=true

    local hostnames_json="" host first=1
    for host in "${HOSTNAMES[@]}"; do
        [[ "$first" == "1" ]] || hostnames_json+=","
        first=0
        hostnames_json+="\"$(cap_json_escape "$host")\""
    done

    cap_info "Done. seed-secrets.sh loads this pair into the cluster as ${MEMQL_LOCAL_TLS_SECRET}."
    cap_result_set     caroot        "$caroot"
    cap_result_set     certFile      "$cert"
    cap_result_set     keyFile       "$key"
    cap_result_set     mkcertBin     "$bin"
    cap_result_set_raw hostnames     "[${hostnames_json}]"
    cap_result_set_raw reissued      "$reissued"
    # Whether the names on the resulting certificate were actually CHECKED, as
    # opposed to hoped for. A caller reporting "covers" off certIssued=false
    # alone would be asserting an unverified claim (memql#3730), so the
    # distinction is published rather than left for each caller to invent.
    cap_result_set_raw coverageVerified "$coverage_verified"
    cap_result_set_raw caPreExisting "$ca_pre_existing"
    cap_result_set_raw caInstalled   "$ca_installed"
    # The field the graph verifies. Reaching here means `mkcert -install`
    # returned 0, which is the only evidence there is that the trust stores
    # actually hold this CA -- and the fact whose absence used to be reported as
    # a successful install (memql#3560).
    cap_result_set_raw caTrusted     true
    cap_result_set_raw certIssued    "$cert_issued"
    cap_ok
}

main "$@"
