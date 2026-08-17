#!/usr/bin/env bash
#
# scripts/install/recovery-key.sh -- claim the cluster's owner recovery key
# (memql#3969).
#
# WHAT THIS IS FOR. Every other credential an install produces depends on the
# owner being able to sign in: a magic link goes to their mailbox, an enrolment
# link goes to their browser, a passkey lives on their device. This one is the
# answer to "all of that is gone". It authorizes exactly one action -- register
# a passkey as the cluster owner -- and only while that owner has NO working
# way in, so it is refused on any day it is not needed.
#
# THE KEY IS NEVER BROADCAST. It is minted by an invariant on the identity node
# which DISCARDS the plaintext: a break-glass credential printed at boot would
# land in the pod log and from there in whatever ships those logs off the
# cluster. This script is how a human obtains the value, once, on demand.
#
# It comes back on stdout because that is the capability's entire product;
# treat the output like a password, and store it somewhere the cluster is not.
# Only its SHA-256 hash exists server-side, so it cannot be shown again.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/recovery-key.sh --local
#   scripts/install/recovery-key.sh --local --user-id=v1:identity:user:abc123
#   scripts/install/recovery-key.sh --print-spec
#
# Refs: #3969 #3964 #3965 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.recoveryKey" \
    "Claim the cluster's owner recovery key from the local identity pod, revealing it once."
cap_spec_param "local"     "REQUIRED affirmation that this targets the LOCAL cluster (flag)"
cap_spec_param "user-id"   "owner whose key to claim (default: the cluster's owner when there is exactly one)"
cap_spec_param "context"   "kubectl context to pin (default k3d-memql)"
cap_spec_param "namespace" "namespace holding the identity workload (default memql)"
cap_spec_param "target"    "workload to exec into (default deploy/identity)"
cap_spec_param "binary"    "path to the memql binary inside the pod (default /app/memql)"

# RESULT: recoveryKeyState is `claimed` or `awaitingOwner`, mirroring
# enrolment-link.sh's enrolmentState and for the same reason (memql#3591): a
# cluster nobody has signed into has no owner, so there is no key, and a verify
# demanding one would make every fresh install end in a failed step. The state
# is set on both paths and every genuine failure exits non-zero before reaching
# either -- so a state means the question was answered, not that a key exists.

# The claimed key is a bare mql_rec_<43>. Matched rather than trusted-verbatim
# so a stray log line on the pod's stdout cannot be mistaken for the product.
RECOVERY_KEY_RE='mql_rec_[A-Za-z0-9_-]{43}'

#=============================================================================
# THE GATE
#=============================================================================

# require_local_affirmation -- refuses (3) unless --local was passed. Runs
# before any cluster contact, so a refusal is guaranteed side-effect-free.
function require_local_affirmation() {
    if [[ -z "$(cap_flag local)" ]]; then
        cap_fail 3 "refusing to claim a recovery credential without --local: \
kubectl points at whatever context was last used, so this must be an explicit local decision"
    fi
}

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed; cannot exec the identity pod"
    fi
}

#=============================================================================
# CLAIM
#=============================================================================

# claim_key <context> <namespace> <target> <binary> <user-id>
# Fills _RK_KEY. Assigns a global rather than echoing so a cap_fail here is
# never swallowed by a "$(...)" subshell.
_RK_KEY=""
_RK_OUTPUT=""
_RK_STDERR=""
# Set when the claim failed because the cluster has no owner yet, i.e. nobody
# has signed in. Reported rather than raised -- see the RESULT note above.
_RK_NO_OWNER=0

function claim_key() {
    local ctx="$1" ns="$2" target="$3" binary="$4" user_id="$5" rc=0
    local -a argv=("$binary" recovery-key claim)
    if [[ -n "$user_id" ]]; then
        argv+=("--user-id=${user_id}")
    fi

    cap_step "kubectl --context=${ctx} -n ${ns} exec ${target} -- ${argv[*]}"

    # stderr is captured separately rather than merged: the subcommand writes
    # the key to stdout ALONE precisely so a capture holds the key and nothing
    # else, and merging here would undo that on the way back out.
    local err_file
    err_file="$(mktemp)"
    _RK_OUTPUT="$(kubectl --context="$ctx" -n "$ns" exec "$target" -- "${argv[@]}" 2>"$err_file")" || rc=$?
    _RK_STDERR="$(cat "$err_file")"
    rm -f "$err_file"

    if [[ $rc -ne 0 ]]; then
        # NO OWNER YET is not a failure of this step (memql#3591's shape). A
        # cluster is claimed by its first sign-in and the key is minted once an
        # owner exists, so on a fresh install this is the expected answer.
        if printf '%s' "$_RK_STDERR" | grep -q 'no owner yet'; then
            _RK_NO_OWNER=1
            return 0
        fi
        printf '%s\n' "$_RK_STDERR" >&2
        cap_fail 5 "claiming the recovery key failed (exit ${rc}); see the pod output above"
    fi

    _RK_KEY="$(printf '%s' "$_RK_OUTPUT" | grep -oE "$RECOVERY_KEY_RE" | head -n1 || true)"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ctx ns target binary user_id
    ctx="$(cap_param context k3d-memql)"
    ns="$(cap_param namespace memql)"
    target="$(cap_param target deploy/identity)"
    binary="$(cap_param binary /app/memql)"
    user_id="$(cap_param user-id)"

    cap_require context   "$ctx"
    cap_require namespace "$ns"
    cap_require target    "$target"
    cap_require binary    "$binary"

    require_local_affirmation
    check_prerequisites

    claim_key "$ctx" "$ns" "$target" "$binary" "$user_id"

    cap_result_set context   "$ctx"
    cap_result_set namespace "$ns"
    cap_result_set target    "$target"

    if [[ "$_RK_NO_OWNER" == "1" ]]; then
        cap_result_set     recoveryKey       ""
        cap_result_set_raw ownerClaimed      false
        cap_result_set     recoveryKeyState  "awaitingOwner"
        cap_result_set     nextStep     "sign in with the owner magic link -- that first sign-in is what creates the account -- then run this again to claim the recovery key"
        cap_info "this cluster has no owner yet: a cluster is claimed by its first sign-in."
        cap_info "Sign in, then claim the recovery key and store it somewhere the cluster is not."
        cap_ok
    fi

    if [[ -z "$_RK_KEY" ]]; then
        cap_result_set recoveryKey ""
        cap_fail 5 "the claim reported success but emitted no recovery key; \
check the identity workload's logs for a mint failure"
    fi

    cap_changed
    cap_result_set     recoveryKey       "$_RK_KEY"
    cap_result_set_raw ownerClaimed      true
    cap_result_set     recoveryKeyState  "claimed"
    cap_warn "The key below is the cluster's BREAK-GLASS CREDENTIAL -- treat it like a password."
    cap_warn "It is shown once and cannot be shown again; only its hash is stored."
    cap_info "Store it somewhere the cluster is NOT -- a password manager, a safe."
    cap_info "It is refused while the owner can still sign in normally, and it works exactly once."
    cap_ok
}

main "$@"
