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
# Refs: #4072 #3969 #3964 #3965 #2221

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

# RESULT: recoveryKeyState is `claimed`, `awaitingOwner` or `alreadyClaimed`,
# mirroring enrolment-link.sh's enrolmentState and for the same reason
# (memql#3591): a cluster with no owner has nothing to bind a key to, so there is
# no key, and a verify demanding one would turn that into a failed step. The
# state is set on all three paths and every genuine failure exits non-zero before
# reaching any of them -- so a state means the question was answered, not that a
# key was produced.
#
# `claimed` IS THE ORDINARY ANSWER ON A FRESH INSTALL. An install that seeded
# MEMQL_IDENTITY_BOOTSTRAP_* gets its owner row written on the identity node's
# own first boot (App.provisionBootstrapOwner), and ensureOwnerRecoveryKey runs
# immediately AFTER that on the same boot, deliberately -- so the key exists,
# unclaimed, before this ever runs, and this step is what reveals it. The
# ordering is argued at the call site in app/integrations_identity.go.
#
# THE THREE STATES ARE THREE DIFFERENT FACTS AND MUST STAY TELLABLE APART. Two
# of them emit no key, which is exactly why collapsing them would be tempting
# and wrong: an operator (or the record of a run) needs to distinguish "you were
# just handed a credential", "there is nobody to hold one yet" and "the
# credential exists and you already have it".
#
#   claimed        a key was revealed in THIS run. changed=true.
#   awaitingOwner  the cluster has no owner, so no key exists yet.
#   alreadyClaimed the key exists and was claimed on an earlier run.
#
# WHY `alreadyClaimed` IS A SUCCESS AND NOT A FAILURE (memql#4072). Running the
# install graph a SECOND time on the same cluster is precisely what the repair
# and upgrade verbs are, and this step used to fail on that second pass -- 15 of
# 16 steps green and then `exit 5: claiming the recovery key failed`. It failed
# BECAUSE the first run had worked.
#
# The subcommand's refusal is correct: only the key's SHA-256 hash was ever
# stored, so the plaintext genuinely cannot be shown again. But this step's job
# is not "reveal a key"; it is that the install ENDS WITH a break-glass
# credential the operator holds off-cluster. An already-claimed key satisfies
# that. Nothing is broken, and there is no action for the operator to take.
#
# The two alternatives are both worse, and it is worth naming why:
#
#   - FAILING (what it did) breaks repair and upgrade outright, and tells an
#     operator something is wrong at the exact moment nothing is.
#   - SILENTLY RE-CLAIMING (passing --reclaim here) would ROTATE the key on
#     every repair: retire the one the operator already wrote down and hand
#     them a replacement they may never notice, so the value in their password
#     manager silently stops working. Strictly worse than doing nothing.
#     `--reclaim` exists so that rotation is a DELIBERATE act, and this step is
#     not the place to make it accidental.

# The claimed key is a bare mql_rec_<43>. Matched rather than trusted-verbatim
# so a stray log line on the pod's stdout cannot be mistaken for the product.
RECOVERY_KEY_RE='mql_rec_[A-Za-z0-9_-]{43}'

# THE TWO NON-FAILURE EXIT-1 SHAPES, matched on the sentence the subcommand
# writes to stderr. `memql recovery-key claim` exits 1 for every refusal it
# makes -- no owner, no active key, several owners, already claimed -- so the
# exit code alone cannot separate "the goal already holds" from "the database is
# unreachable". The prose is the only signal there is.
#
# That coupling is invisible from both ends: nothing in subcommand_recovery_key.go
# says a shell script reads these sentences, and nothing here can notice when one
# is reworded. So they are named constants rather than inline greps, and
# TestRecoveryKeyStateDetectorsMatchTheSubcommandsOwnMessages
# (recovery_key_test.go) asserts each still appears verbatim in that file -- a
# rewording then breaks the build instead of quietly turning a success back into
# an exit 5. If the subcommand ever grows a machine-readable signal (a distinct
# exit code, a `status` subcommand), that is what should replace this pair.
CLAIM_NO_OWNER_MSG='no owner yet'
CLAIM_ALREADY_CLAIMED_MSG='was already claimed at'

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
# Set when the claim failed because the cluster has no owner yet -- not because
# nobody has signed in, which is a different question and no longer the same
# answer. Reported rather than raised; see the RESULT note above.
_RK_NO_OWNER=0
# Set when the claim was refused because the key has ALREADY been claimed, i.e.
# this install graph has run here before. Reported rather than raised, for the
# reason spelled out at length in the RESULT note above: the step's goal already
# holds, so there is nothing to do and nothing to report as broken.
_RK_ALREADY_CLAIMED=0

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
        # NO OWNER YET is not a failure of this step (memql#3591's shape), and is
        # no longer the expected answer on a fresh install: an install that
        # seeded the owner details has its owner row, and its key, before this
        # runs. The key is minted once an owner exists, so this branch is what
        # remains for a cluster that was never given those details.
        if printf '%s' "$_RK_STDERR" | grep -qF "$CLAIM_NO_OWNER_MSG"; then
            _RK_NO_OWNER=1
            return 0
        fi
        # ALREADY CLAIMED is not a failure of this step either (memql#4072).
        # This is the answer on the SECOND run of the install graph against the
        # same cluster -- which is what repair and upgrade are -- and it means
        # the credential exists and its owner holds it. See the RESULT note for
        # why that is a success and why re-claiming here would be worse.
        if printf '%s' "$_RK_STDERR" | grep -qF "$CLAIM_ALREADY_CLAIMED_MSG"; then
            _RK_ALREADY_CLAIMED=1
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
        cap_result_set     nextStep     "sign in with the owner magic link -- on a cluster with no owner account, that first sign-in is what creates one -- then run this again to claim the recovery key"
        cap_info "this cluster has no owner yet. An install that seeds the owner details names the owner as the cluster starts, and the key is minted for them then, so this cluster was started without them."
        cap_info "Sign in to create the owner, then claim the recovery key and store it somewhere the cluster is not."
        cap_ok
    fi

    # ALREADY CLAIMED (memql#4072). The goal of this step already holds, so it
    # reports and stops. NOTHING IS CHANGED HERE and `cap_changed` is
    # deliberately not called: a repair that reported a change it did not make
    # is exactly how a silent rotation would look, and this is the one step
    # where an operator must be able to trust that their stored key is still
    # the live one. `ownerClaimed` stays true because it answers a DIFFERENT
    # question -- has the cluster been claimed by an owner -- which a key that
    # has been claimed proves.
    if [[ "$_RK_ALREADY_CLAIMED" == "1" ]]; then
        cap_result_set     recoveryKey       ""
        cap_result_set_raw ownerClaimed      true
        cap_result_set     recoveryKeyState  "alreadyClaimed"
        cap_result_set     nextStep     "nothing -- the key claimed earlier is still the live one. If it was lost, rotate deliberately: kubectl --context=${ctx} -n ${ns} exec ${target} -- ${binary} recovery-key claim --reclaim"
        cap_info "the recovery key already exists and was claimed on an earlier run, so this install has nothing to do."
        cap_info "It is NOT re-revealed: only its SHA-256 hash was ever stored, so the original value cannot be shown again."
        cap_info "If you still hold it, keep it -- it is the live key and nothing here has changed it."
        cap_info "If you have LOST it, rotate deliberately (this retires the old key and reveals a replacement once):"
        cap_info "  kubectl --context=${ctx} -n ${ns} exec ${target} -- ${binary} recovery-key claim --reclaim"
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
