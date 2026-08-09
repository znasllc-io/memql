#!/usr/bin/env bash
#
# scripts/install/magic-link.sh
# =============================
#
# Capability: install.magicLink -- recover the cluster owner's magic link from
# the identity pod's logs.
#
# A freshly installed cluster has no way in until the owner claims ownership,
# and that claim arrives as a magic link. On a local cluster there is no
# mailbox, so identity's dev-mode escape hatch logs the link at INFO
# (component/identity/emailsender/sender.go). Reading it back out of the pod
# log is the installer's only route to a first login.
#
# --local IS MANDATORY (exit 3 without it). kubectl talks to whatever context
# was last used -- possibly staging, possibly production. Scraping an
# AUTHENTICATION CREDENTIAL out of pod logs must be a decision the operator
# states out loud, never something that happens because a kubeconfig happened
# to be pointing somewhere. The affirmation is then backed mechanically: the
# script PINS --context (default k3d-memql, overridable) instead of inheriting
# the ambient one, so "I meant the local cluster" is enforced, not trusted.
#
# THE LAST LINK WINS. Magic links are single-use. A pod that restarted, or an
# operator who already clicked once, leaves spent links earlier in the window.
# Handing back the first match hands back a link that is guaranteed to fail.
#
# EXIT CODES:
#
#   0  a link was recovered (it is on stdout, inside the result envelope)
#   2  bad param
#   3  REFUSED: --local was not passed
#   4  prerequisite missing (kubectl absent)
#   5  operation failed (kubectl errored, or the window holds no link)
#
# NOTE: the recovered link IS a credential. It goes to stdout because that is
# the capability's entire product; treat the output like a password.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/magic-link.sh --local
#   scripts/install/magic-link.sh --local --email=owner@example.com --since=1h
#   scripts/install/magic-link.sh --print-spec
#
# Refs: #3366 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.magicLink" \
    "Recover the cluster owner's magic link from the local identity pod's logs."
cap_spec_param "local"     "REQUIRED affirmation that this targets the LOCAL cluster (flag)"
cap_spec_param "context"   "kubectl context to pin (default k3d-memql)"
cap_spec_param "namespace" "namespace holding the identity workload (default memql)"
cap_spec_param "target"    "workload to read logs from (default deploy/identity)"
cap_spec_param "since"     "log window passed to kubectl --since (default 24h)"
cap_spec_param "tail"      "max log lines to scan (default 5000)"
cap_spec_param "email"     "only consider links issued to this address"

# A magic link is <base>/auth/complete?ml=<token> (see
# component/identity/magiclink/issuer.go buildMagicLinkURL). The log line is
# slog JSON, so the URL is delimited by double quotes -- excluding quote,
# whitespace and backslash from the tail is enough to bound the match.
MAGIC_LINK_RE='https?://[^[:space:]"\\]+/auth/complete\?[^[:space:]"\\]+'

#=============================================================================
# THE GATE
#=============================================================================

# require_local_affirmation -- refuses (3) unless --local was passed. Runs
# before any cluster contact, so a refusal is guaranteed side-effect-free.
function require_local_affirmation() {
    if [[ -z "$(cap_flag local)" ]]; then
        cap_fail 3 "refusing to scrape an auth credential from pod logs without --local: \
kubectl points at whatever context was last used, so this must be an explicit local decision"
    fi
}

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is not installed; cannot read the identity pod's logs"
    fi
}

#=============================================================================
# LOG FETCH + EXTRACTION
#=============================================================================

# fetch_logs <context> <namespace> <target> <since> <tail>
# Fills _ML_LOGS. Assigns a global rather than echoing so a cap_fail here is
# never swallowed by a "$(...)" subshell.
_ML_LOGS=""
function fetch_logs() {
    local ctx="$1" ns="$2" target="$3" since="$4" tail="$5" rc=0
    cap_step "kubectl --context=${ctx} -n ${ns} logs ${target} --since=${since}"
    _ML_LOGS="$(kubectl --context="$ctx" --namespace="$ns" logs "$target" \
                    --since="$since" --tail="$tail" 2>&1)" || rc=$?
    if [[ "$rc" != "0" ]]; then
        cap_fail 5 "kubectl logs failed (exit ${rc}) for ${target} in ${ns} on ${ctx}"
    fi
}

# extract_last_link <email-filter>
# Fills _ML_LINK and _ML_CANDIDATES from _ML_LOGS. The LAST match wins:
# earlier links in the window are spent (single-use), so returning the first
# match returns a link that cannot work.
_ML_LINK=""
_ML_CANDIDATES=0
function extract_last_link() {
    local email="$1" lines="$_ML_LOGS" matches=""

    if [[ -n "$email" ]]; then
        lines="$(printf '%s\n' "$lines" | grep -F "$email" || true)"
    fi
    matches="$(printf '%s\n' "$lines" | grep -oE "$MAGIC_LINK_RE" || true)"

    if [[ -z "$matches" ]]; then
        _ML_CANDIDATES=0
        _ML_LINK=""
        return
    fi
    _ML_CANDIDATES="$(printf '%s\n' "$matches" | grep -c . || true)"
    _ML_LINK="$(printf '%s\n' "$matches" | tail -n 1)"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ctx ns target since tail email
    ctx="$(cap_param context k3d-memql)"
    ns="$(cap_param namespace memql)"
    target="$(cap_param target deploy/identity)"
    since="$(cap_param since 24h)"
    tail="$(cap_param tail 5000)"
    email="$(cap_param email)"

    cap_require context   "$ctx"
    cap_require namespace "$ns"
    cap_require target    "$target"

    require_local_affirmation
    check_prerequisites

    fetch_logs "$ctx" "$ns" "$target" "$since" "$tail"
    extract_last_link "$email"

    cap_result_set     context    "$ctx"
    cap_result_set     namespace  "$ns"
    cap_result_set     target     "$target"
    cap_result_set     email      "$email"
    cap_result_set_raw candidates "$_ML_CANDIDATES"

    if [[ -z "$_ML_LINK" ]]; then
        cap_result_set link ""
        cap_fail 5 "no magic link found in the last ${since} of ${target} logs\
${email:+ for ${email}}; request one at the login page, then re-run"
    fi

    cap_result_set link "$_ML_LINK"
    cap_warn "The link below is a single-use CREDENTIAL -- treat it like a password."
    cap_info "Recovered the newest of ${_ML_CANDIDATES} link(s) in the window."
    cap_ok
}

main "$@"
