#!/usr/bin/env bash
#
# scripts/install/enrolment-link.sh
# =================================
#
# Capability: install.enrolmentLink -- mint a single-use enrolment link for the
# cluster owner, so a fresh install ends with a passkey rather than with an
# email round trip.
#
# WHY THIS EXISTS. The install wizard finishes with an owner account that has
# no credential: MEMQL_IDENTITY_BOOTSTRAP_* completed setup from env, so
# /setup will 404 from here on, and the only other way in is a magic link
# recovered from pod logs -- which is a mailbox-shaped answer to a
# no-mailbox-shaped problem. An enrolment link authorizes exactly one action,
# register a passkey as the named user, and the wizard opens it in the
# operator's own browser. Nothing is copied by hand.
#
# WHY IT EXECS THE POD. At this instant nothing can AUTHENTICATE to the
# cluster -- that is the problem being solved -- so the owner/admin issuer on
# IdentityAdminMsg is out of reach by construction. What is available is the
# authority `make voice-agent-token` already uses: somebody who can exec inside
# the identity pod holds the cluster's secrets already. Access to the process
# is the authorization.
#
# --local IS MANDATORY (exit 3 without it), for the same reason magic-link.sh
# demands it: kubectl points at whatever context was last used, possibly
# staging, possibly production. Minting a CREDENTIAL for an account must be an
# explicit local decision, and the affirmation is then backed mechanically --
# the script PINS --context rather than inheriting the ambient one.
#
# EXIT CODES:
#
#   0  a link was minted (it is on stdout, inside the result envelope)
#   2  bad param
#   3  REFUSED: --local was not passed
#   4  prerequisite missing (kubectl absent)
#   5  operation failed (kubectl errored, or the mint produced no link)
#
# NOTE: the minted link IS a credential, and a single-use one. It goes to
# stdout because that is the capability's entire product; treat the output like
# a password. Only its SHA-256 hash exists server-side.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/enrolment-link.sh --local --user-email=owner@example.com
#   scripts/install/enrolment-link.sh --local --user-email=me@example.com --base-url=https://identity.local.znas.io
#   scripts/install/enrolment-link.sh --print-spec
#
# Refs: #3408 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.enrolmentLink" \
    "Mint a single-use passkey-enrolment link for the cluster owner from the local identity pod."
cap_spec_param "local"      "REQUIRED affirmation that this targets the LOCAL cluster (flag)"
cap_spec_param_required "user-email" "primary email of the account to enrol (required)"
cap_spec_param "base-url"   "public identity base URL the link points at (default: the pod's MEMQL_IDENTITY_BASE_URL)"
cap_spec_param "ttl"        "link lifetime as a Go duration (default 15m, server ceiling 24h)"
cap_spec_param "context"    "kubectl context to pin (default k3d-memql)"
cap_spec_param "namespace"  "namespace holding the identity workload (default memql)"
cap_spec_param "target"     "workload to exec into (default deploy/identity)"
cap_spec_param "binary"     "path to the memql binary inside the pod (default /app/memql)"

# RESULT: enrolmentState is `minted` or `awaitingFirstSignIn`, and is the field
# the install graph verifies (memql#3591). `enrolUrl` cannot be it: a cluster
# nobody has signed into has no account to enrol, so no link exists, and a verify
# demanding one made every fresh install end in a failed step. The state is set on
# both paths and every genuine failure exits non-zero before reaching either -- so
# a state here means the question was answered, not that a link was produced.

# The minted link is <base>/enroll?code=mql_enr_<43>. Matched rather than
# trusted-verbatim so a stray log line on the pod's stdout cannot be mistaken
# for the product.
ENROL_LINK_RE='https?://[^[:space:]"\\]+/enroll\?code=mql_enr_[A-Za-z0-9_-]+'

#=============================================================================
# THE GATE
#=============================================================================

# require_local_affirmation -- refuses (3) unless --local was passed. Runs
# before any cluster contact, so a refusal is guaranteed side-effect-free.
function require_local_affirmation() {
    if [[ -z "$(cap_flag local)" ]]; then
        cap_fail 3 "refusing to mint an enrolment credential without --local: \
kubectl points at whatever context was last used, so this must be an explicit local decision"
    fi
}

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed; cannot exec the identity pod"
    fi
}

#=============================================================================
# MINT
#=============================================================================

# mint_link <context> <namespace> <target> <binary> <email> <base-url> <ttl>
# Fills _EL_LINK. Assigns a global rather than echoing so a cap_fail here is
# never swallowed by a "$(...)" subshell.
_EL_LINK=""
_EL_OUTPUT=""
_EL_STDERR=""
# Set when the mint failed because no user with this email exists yet, i.e. the
# cluster has not been claimed. Not an error; see mint_link.
_EL_UNCLAIMED=0
function mint_link() {
    local ctx="$1" ns="$2" target="$3" binary="$4" email="$5" base="$6" ttl="$7" rc=0
    local -a argv=("$binary" enrolment-token mint "--user-email=${email}")
    [[ -n "$base" ]] && argv+=("--base-url=${base}")
    [[ -n "$ttl" ]] && argv+=("--ttl=${ttl}")

    cap_step "kubectl --context=${ctx} -n ${ns} exec ${target} -- ${binary} enrolment-token mint --user-email=${email}"

    # THE TWO CHANNELS ARE KEPT APART, AND BOTH ARE KEPT (memql#3591).
    #
    # stdout carries the link and nothing else; the subcommand sends every
    # component log to stderr, which is why stdout alone is what the regex below
    # runs over. But stderr was going to /dev/null -- so when the mint failed, the
    # ONE line explaining why ("no user with primary email ...") was discarded and
    # this step could only say "run the same exec by hand". The command it logged
    # omitted the flags, so doing that produced a DIFFERENT error than the one
    # being diagnosed, and sent the operator to look at the wrong thing.
    #
    # A capability's failure message is the entire product of a failed run. When
    # the process it drove has already said why, passing that sentence on is the
    # job.
    local errfile
    errfile="$(mktemp "${TMPDIR:-/tmp}/memql-enrol-stderr.XXXXXX")"
    _EL_OUTPUT="$(kubectl --context="$ctx" --namespace="$ns" exec "$target" -- "${argv[@]}" 2>"$errfile")" || rc=$?
    _EL_STDERR="$(cat "$errfile" 2>/dev/null || true)"
    rm -f "$errfile"

    if [[ "$rc" != "0" ]]; then
        # NOBODY HAS SIGNED IN YET, which is not a failure. The env bootstrap
        # writes the clusterSettings row and issues the owner's magic link; the
        # user itself is created by CreateUserOnFirstLogin when that link is
        # verified (component/identity/magiclink/verifier.go). So on every freshly
        # installed cluster there is no account to enrol, and reporting that as a
        # broken mint turns a complete install into a failed one.
        if printf '%s' "$_EL_STDERR" | grep -q 'no user with primary email'; then
            _EL_UNCLAIMED=1
            return 0
        fi
        cap_fail 5 "enrolment-token mint failed (exit ${rc}) in ${target} on ${ctx}: $(_el_first_reason)"
    fi
    _EL_LINK="$(printf '%s\n' "$_EL_OUTPUT" | grep -oE "$ENROL_LINK_RE" | tail -n 1 || true)"
}

# _el_first_reason -- the pod's own explanation, as one line.
#
# The subcommand's own messages are plain text; everything the ENGINE logs is
# JSON. Preferring the plain lines picks the sentence written for a human out of
# forty structured startup lines, and falls back to the last line rather than to
# nothing when there is no such sentence.
function _el_first_reason() {
    local plain
    plain="$(printf '%s\n' "$_EL_STDERR" | grep -v '^[[:space:]]*{' | grep -v '^[[:space:]]*$' | head -n 1)"
    if [[ -n "$plain" ]]; then
        printf '%s' "$plain"
        return 0
    fi
    plain="$(printf '%s\n' "$_EL_STDERR" | grep -v '^[[:space:]]*$' | tail -n 1)"
    if [[ -n "$plain" ]]; then
        printf '%s' "$plain"
        return 0
    fi
    printf 'the pod wrote nothing to stderr'
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ctx ns target binary email base ttl
    ctx="$(cap_param context k3d-memql)"
    ns="$(cap_param namespace memql)"
    target="$(cap_param target deploy/identity)"
    binary="$(cap_param binary /app/memql)"
    email="$(cap_param user-email)"
    base="$(cap_param base-url)"
    ttl="$(cap_param ttl)"

    cap_require context    "$ctx"
    cap_require namespace  "$ns"
    cap_require target     "$target"
    cap_require binary     "$binary"
    cap_require user-email "$email"

    require_local_affirmation
    check_prerequisites

    mint_link "$ctx" "$ns" "$target" "$binary" "$email" "$base" "$ttl"

    cap_result_set context   "$ctx"
    cap_result_set namespace "$ns"
    cap_result_set target    "$target"
    cap_result_set email     "$email"

    # NOTHING TO ENROL YET (memql#3591). Reported, not raised: `ownerClaimed`
    # is the field that separates it from a mint that produced no link, and the
    # next step belongs in the result because this is the last thing an install
    # says to the operator.
    if [[ "$_EL_UNCLAIMED" == "1" ]]; then
        cap_result_set     enrolUrl        ""
        cap_result_set_raw ownerClaimed    false
        cap_result_set     enrolmentState  "awaitingFirstSignIn"
        cap_result_set     nextStep     "sign in with the owner magic link -- that first sign-in is what creates the account -- then enrol a passkey from /me/devices"
        cap_info "no account for ${email} yet: a cluster is claimed by its first sign-in, and the account is created then."
        cap_info "Sign in with the magic link this install recovered, then enrol a passkey from /me/devices."
        cap_ok
    fi

    if [[ -z "$_EL_LINK" ]]; then
        cap_result_set enrolUrl ""
        cap_fail 5 "the mint reported success but emitted no enrolment link; \
check MEMQL_IDENTITY_BASE_URL on the identity workload, or pass --base-url"
    fi

    cap_changed
    cap_result_set     enrolUrl        "$_EL_LINK"
    cap_result_set_raw ownerClaimed    true
    cap_result_set     enrolmentState  "minted"
    cap_warn "The link below is a single-use CREDENTIAL -- treat it like a password."
    cap_info "Open it to set up a passkey for ${email}. It expires shortly and works once."
    cap_ok
}

main "$@"
