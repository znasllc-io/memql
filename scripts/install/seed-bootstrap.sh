#!/usr/bin/env bash
#
# scripts/install/seed-bootstrap.sh
# =================================
#
# Capability: install.seedBootstrap -- write the k8s Secret that lets a freshly
# created cluster BOOTSTRAP ITSELF: the MEMQL_IDENTITY_BOOTSTRAP_* set that
# creates the cluster owner, plus the AI provider key the mesh needs to do
# anything once that owner signs in. With this seeded before the identity node
# first boots, the operator never visits /setup; without it they land on a
# wizard nobody told them was coming.
#
# AN INCOMPLETE SET IS EXIT 2, NOT A WARNING.
#
#   Identity auto-bootstraps only when ALL of domain / owner-email /
#   owner-first-name / owner-last-name / registration-mode are present
#   (component/identity/config.go: BootstrapConfig.HasAllRequired). Miss ONE and
#   the whole set is inert -- and nothing says so. The seed succeeds, the Secret
#   exists, kubectl shows four healthy keys, the cluster comes up green, and
#   what the operator gets is a login page for an account that was never
#   created. A partial seed looks MORE finished than no seed at all, which is
#   why it cannot be a warning: the warning scrolls past in a hundred lines of
#   cluster bring-up and the failure surfaces twenty minutes later as "I can't
#   sign in".
#
#   So the check happens before anything is written, names every missing field
#   at once (not one per re-run), and exits 2.
#
# THE API KEY GOES IN VIA --from-file, NEVER argv.
#
#   argv is world-readable: `ps`, /proc/<pid>/cmdline, the shell history of
#   whoever ran it, and the argv this script's own capability runner logs. A key
#   passed as --provider-key=sk-... is a key leaked to every process on the
#   machine for the lifetime of the call. The key therefore arrives as a FILE
#   PATH and reaches kubectl through `--from-file`, so the value itself never
#   appears in any command line -- ours or kubectl's.
#
# IDEMPOTENT. `kubectl apply` from a client-side dry-run, like every other
# secret seeder here; re-running with the same inputs changes nothing.
#
# EXIT CODES:
#
#   0  seeded (or already identical)
#   2  bad param -- an incomplete bootstrap set, an unknown registration mode
#   4  prerequisite missing (kubectl, cluster unreachable, namespace absent,
#      unreadable key file)
#   5  the write failed
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/seed-bootstrap.sh \
#       --domain=local.znas.io --owner-email=me@example.com \
#       --owner-first-name=Ada --owner-last-name=Lovelace \
#       --registration-mode=invite_only \
#       --provider=anthropic --provider-key-file=$HOME/.memql/anthropic.key
#   scripts/install/seed-bootstrap.sh --print-spec
#
# Refs: #3375 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.seedBootstrap" \
    "Seed the identity bootstrap values and AI provider key so the cluster self-bootstraps."
cap_spec_param "namespace"            "k8s namespace to seed into (default memql)"
cap_spec_param "context"              "kubectl context to pin (default: whatever is current)"
cap_spec_param "secret"               "Secret name to write (default memql-bootstrap)"
cap_spec_param "domain"               "REQUIRED cluster domain, e.g. local.znas.io"
cap_spec_param "owner-email"          "REQUIRED email the cluster owner receives the first magic link at"
cap_spec_param "owner-first-name"     "REQUIRED cluster owner's first name"
cap_spec_param "owner-last-name"      "REQUIRED cluster owner's last name"
cap_spec_param "registration-mode"    "REQUIRED: open | domain_restricted | invite_only | waitlist"
cap_spec_param "org-name"             "organization label shown in the admin surfaces"
cap_spec_param "registration-domains" "comma-separated allowlist; REQUIRED when mode is domain_restricted"
cap_spec_param "internal-domains"     "comma-separated domains whose users are flagged internal"
cap_spec_param "internal-default-role" "cluster role for internal users: owner | admin | writer | reader"
cap_spec_param "notify-emails"        "comma-separated waitlist-notification recipients"
cap_spec_param "provider"             "AI provider the key belongs to: anthropic | openai"
cap_spec_param "provider-key-file"    "path to a file holding the API key (never a flag -- argv is public)"
cap_spec_param "dry-run"              "report what would be written and write nothing (flag)"

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
DEFAULT_SECRET="memql-bootstrap"

# The exact set BootstrapConfig.HasAllRequired gates on. Kept as data so the
# "which ones are missing" message can name all of them in one pass.
readonly REQUIRED_PARAMS=(domain owner-email owner-first-name owner-last-name registration-mode)

# Env var each required param maps to.
function env_name_for() {
    case "$1" in
        domain)                 printf 'MEMQL_IDENTITY_BOOTSTRAP_DOMAIN' ;;
        owner-email)            printf 'MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL' ;;
        owner-first-name)       printf 'MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME' ;;
        owner-last-name)        printf 'MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME' ;;
        registration-mode)      printf 'MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE' ;;
        org-name)               printf 'MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME' ;;
        registration-domains)   printf 'MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_DOMAINS' ;;
        internal-domains)       printf 'MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS' ;;
        internal-default-role)  printf 'MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE' ;;
        notify-emails)          printf 'MEMQL_IDENTITY_BOOTSTRAP_NOTIFY_EMAILS' ;;
        *)                      printf '' ;;
    esac
}

# AI provider -> the env var the engine reads that provider's key from.
function provider_env_name() {
    case "$1" in
        anthropic) printf 'MEMQL_AI_ANTHROPIC_API_KEY' ;;
        openai)    printf 'MEMQL_AI_OPENAI_API_KEY' ;;
        *)         printf '' ;;
    esac
}

#=============================================================================
# SCRATCH CLEANUP
#=============================================================================
#
# The API key is copied (trimmed) into a 0600 temp file so kubectl can take it
# via --from-file. Removing it needs an EXIT trap, which would otherwise REPLACE
# the one cap_init installs -- and that trap is what guarantees a failure
# envelope on an unexpected abort. So we chain: capture the real status, clean
# up, restore the status, hand off.

_SB_SCRATCH=""

function _sb_on_exit() {
    local rc=$?
    # `set +e` is load-bearing: this handler runs under errexit, and
    # `(exit "$rc")` is by definition a failing command when rc is non-zero.
    # Without it errexit abandons the handler and _cap_on_exit never runs, so
    # the caller gets a non-zero exit and NO envelope.
    set +e
    if [[ -n "$_SB_SCRATCH" ]]; then
        rm -rf "$_SB_SCRATCH" 2>/dev/null
    fi
    (exit "$rc")
    _cap_on_exit
}
trap _sb_on_exit EXIT

#=============================================================================
# PREREQUISITES
#=============================================================================

KUBECTL=(kubectl)

function check_prerequisites() {
    local ns="$1" ctx="$2"
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is required but was not found on PATH"
    if [[ -n "$ctx" ]]; then
        KUBECTL=(kubectl --context "$ctx")
    fi
    "${KUBECTL[@]}" cluster-info &>/dev/null \
        || cap_fail 4 "kubectl cannot reach the cluster${ctx:+ (context ${ctx})} -- create it first"
    "${KUBECTL[@]}" get namespace "$ns" &>/dev/null \
        || cap_fail 4 "namespace ${ns} does not exist -- the cluster must be up before its bootstrap values are seeded"
}

#=============================================================================
# THE COMPLETENESS GATE
#=============================================================================
#
# Runs BEFORE the cluster is touched and before the key file is read: an
# operator who supplied four of five fields gets a clean exit 2 and an
# unchanged cluster, every time.

# require_complete_bootstrap_set <domain> <email> <first> <last> <mode>
# Values are passed positionally in REQUIRED_PARAMS order.
function require_complete_bootstrap_set() {
    local -a values=("$@")
    local -a missing=()
    local i
    for i in "${!REQUIRED_PARAMS[@]}"; do
        if [[ -z "${values[$i]// }" ]]; then
            missing+=("--${REQUIRED_PARAMS[$i]} ($(env_name_for "${REQUIRED_PARAMS[$i]}"))")
        fi
    done
    [[ ${#missing[@]} -eq 0 ]] && return 0

    local list
    list="$(printf '%s, ' "${missing[@]}")"
    list="${list%, }"
    cap_fail 2 "incomplete bootstrap set -- missing: ${list}. Identity auto-bootstraps ONLY when \
all five of ${REQUIRED_PARAMS[*]} are present, so a partial seed writes a Secret that looks \
healthy, brings the cluster up green, and leaves the operator at a login page for an account that \
was never created. Supply the missing values and re-run."
}

function validate_registration_mode() {
    local mode="$1" domains="$2"
    case "$mode" in
        open|domain_restricted|invite_only|waitlist) ;;
        *) cap_fail 2 "unknown --registration-mode '${mode}' (expected: open, domain_restricted, invite_only, waitlist)" ;;
    esac
    # The engine rejects this combination at boot (identity Config.Validate), so
    # catching it here turns a crash-looping identity node into a clear message.
    if [[ "$mode" == "domain_restricted" && -z "${domains// }" ]]; then
        cap_fail 2 "--registration-mode=domain_restricted requires --registration-domains; identity refuses to start without an allowlist"
    fi
}

function validate_internal_role() {
    local role="$1"
    [[ -z "$role" ]] && return 0
    case "$role" in
        owner|admin|writer|reader) ;;
        *) cap_fail 2 "unknown --internal-default-role '${role}' (expected: owner, admin, writer, reader)" ;;
    esac
}

#=============================================================================
# THE PROVIDER KEY
#=============================================================================

# Staged path for the trimmed key, or "" when no key was supplied.
_SB_KEY_FILE=""
_SB_KEY_ENV=""

# stage_provider_key <provider> <key-file> -- copies the key into a 0600 temp
# file with surrounding whitespace stripped. An editor-added trailing newline is
# part of the file but NOT part of the key, and kubectl's --from-file would
# store it verbatim -- yielding a Secret that looks right and authenticates
# against nothing.
function stage_provider_key() {
    local provider="$1" src="$2"
    if [[ -z "$provider" && -z "$src" ]]; then
        return 0
    fi
    [[ -n "$provider" ]] || cap_fail 2 "--provider-key-file needs --provider so the key lands under the right env var"
    [[ -n "$src" ]] || cap_fail 2 "--provider=${provider} needs --provider-key-file (the key is passed as a FILE; argv is public)"

    _SB_KEY_ENV="$(provider_env_name "$provider")"
    [[ -n "$_SB_KEY_ENV" ]] || cap_fail 2 "unknown --provider '${provider}' (expected: anthropic, openai)"

    [[ -r "$src" ]] || cap_fail 4 "provider key file is not readable: ${src}"

    local key
    key="$(tr -d '[:space:]' < "$src")" || cap_fail 5 "could not read ${src}"
    [[ -n "$key" ]] || cap_fail 2 "provider key file is empty: ${src}"

    _SB_SCRATCH="$(mktemp -d)" || cap_fail 5 "could not create a staging directory"
    _SB_KEY_FILE="${_SB_SCRATCH}/${_SB_KEY_ENV}"
    ( umask 077; printf '%s' "$key" > "$_SB_KEY_FILE" ) \
        || cap_fail 5 "could not stage the provider key"
}

#=============================================================================
# THE WRITE
#=============================================================================

_SB_KEY_COUNT=0

# add_literal <array-name> <param> <value> -- appends --from-literal=ENV=value
# for a non-empty value, skipping empties so an unset optional never lands as an
# empty env var (which reads as "configured, to nothing").
function add_literal() {
    local -n out="$1"
    local param="$2" value="$3" env
    [[ -z "${value// }" ]] && return 0
    env="$(env_name_for "$param")"
    [[ -n "$env" ]] || cap_fail 2 "internal: no env var mapped for --${param}"
    out+=("--from-literal=${env}=${value}")
    _SB_KEY_COUNT=$((_SB_KEY_COUNT + 1))
}

function seed_secret() {
    local ns="$1" name="$2" dry_run="$3"
    shift 3
    local -a from_args=("$@")

    if [[ -n "$dry_run" ]]; then
        cap_info "DRY RUN: would write Secret ${name} in ${ns} with ${_SB_KEY_COUNT} key(s)"
        return 0
    fi

    cap_step "seeding Secret ${name} in namespace ${ns}"
    # Client-side dry-run piped into apply: create-or-update, idempotent, and
    # the same idiom every other seeder here uses.
    "${KUBECTL[@]}" create secret generic "$name" \
        --namespace="$ns" \
        "${from_args[@]}" \
        --dry-run=client -o yaml \
        | "${KUBECTL[@]}" apply -f - >&2 \
        || cap_fail 5 "could not write Secret ${name} in namespace ${ns}"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ns ctx name dry_run
    ns="$(cap_param namespace "$DEFAULT_NAMESPACE")"
    ctx="$(cap_param context "")"
    name="$(cap_param secret "$DEFAULT_SECRET")"
    dry_run="$(cap_flag dry-run)"

    local domain owner_email owner_first owner_last mode
    domain="$(cap_param domain "")"
    owner_email="$(cap_param owner-email "")"
    owner_first="$(cap_param owner-first-name "")"
    owner_last="$(cap_param owner-last-name "")"
    mode="$(cap_param registration-mode "")"

    local org_name reg_domains internal_domains internal_role notify_emails
    org_name="$(cap_param org-name "")"
    reg_domains="$(cap_param registration-domains "")"
    internal_domains="$(cap_param internal-domains "")"
    internal_role="$(cap_param internal-default-role "")"
    notify_emails="$(cap_param notify-emails "")"

    local provider key_file
    provider="$(cap_param provider "")"
    key_file="$(cap_param provider-key-file "")"

    # Everything that can be REFUSED is refused before anything is read from
    # disk or written to the cluster.
    require_complete_bootstrap_set "$domain" "$owner_email" "$owner_first" "$owner_last" "$mode"
    validate_registration_mode "$mode" "$reg_domains"
    validate_internal_role "$internal_role"
    cap_require namespace "$ns"
    cap_require secret "$name"

    stage_provider_key "$provider" "$key_file"

    local -a from_args=()
    add_literal from_args domain                "$domain"
    add_literal from_args owner-email           "$owner_email"
    add_literal from_args owner-first-name      "$owner_first"
    add_literal from_args owner-last-name       "$owner_last"
    add_literal from_args registration-mode     "$mode"
    add_literal from_args org-name              "$org_name"
    add_literal from_args registration-domains  "$reg_domains"
    add_literal from_args internal-domains      "$internal_domains"
    add_literal from_args internal-default-role "$internal_role"
    add_literal from_args notify-emails         "$notify_emails"

    if [[ -n "$_SB_KEY_FILE" ]]; then
        # --from-file, so the key value never appears in any argv: not ours, not
        # kubectl's, not the runner's log of either.
        from_args+=("--from-file=${_SB_KEY_ENV}=${_SB_KEY_FILE}")
        _SB_KEY_COUNT=$((_SB_KEY_COUNT + 1))
    else
        cap_warn "no AI provider key supplied -- the cluster will bootstrap its owner but every model call fails."
        cap_warn "  Re-run with --provider=<anthropic|openai> --provider-key-file=<path> to add one."
    fi

    if [[ -z "$dry_run" ]]; then
        check_prerequisites "$ns" "$ctx"
    fi

    seed_secret "$ns" "$name" "$dry_run" "${from_args[@]}"

    [[ -z "$dry_run" ]] && cap_changed
    cap_result_set     namespace       "$ns"
    cap_result_set     secret          "$name"
    cap_result_set     domain          "$domain"
    cap_result_set     ownerEmail      "$owner_email"
    cap_result_set     registrationMode "$mode"
    cap_result_set     providerKeyEnv  "$_SB_KEY_ENV"
    cap_result_set_raw bootstrapComplete true
    cap_result_set_raw providerKeySeeded "$( [[ -n "$_SB_KEY_FILE" ]] && echo true || echo false )"
    cap_result_set_raw keyCount        "$_SB_KEY_COUNT"
    cap_result_set_raw dryRun          "$( [[ -n "$dry_run" ]] && echo true || echo false )"
    cap_ok
}

main "$@"
