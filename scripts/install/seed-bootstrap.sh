#!/usr/bin/env bash
#
# scripts/install/seed-bootstrap.sh
# =================================
#
# Capability: install.seedBootstrap -- make a freshly created cluster BOOTSTRAP
# ITSELF: write the MEMQL_IDENTITY_BOOTSTRAP_* set that creates the cluster owner,
# plus the AI provider key the mesh needs to do anything once that owner signs in,
# AND make sure the nodes that read them are running with them. The operator then
# never visits /setup; without this they land on a wizard nobody told them was
# coming.
#
# WRITING THE SECRET IS HALF THE JOB (memql#3588). Container environment is read
# once, at container start, and the install graph runs this step after the cluster
# is already up -- so a Secret written here reaches nothing that is already
# running. This step therefore restarts the nodes that read it and waits for them
# back; `bootstrapComplete` claims both halves. The whole argument, including why
# the restart happens EXACTLY ONCE, is at "GETTING THE VALUES INTO THE RUNNING
# MESH" below.
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
#   0  seeded, and every node that reads the values is running with them
#   2  bad param -- an incomplete bootstrap set, an unknown registration mode
#   4  prerequisite missing (kubectl, cluster unreachable, namespace absent,
#      unreadable key file)
#   5  the write failed, or a restarted node did not come back
#
# ENV:
#
#   MEMQL_BOOTSTRAP_ROLL_TIMEOUT  seconds allowed for the WHOLE restart, default
#                                 240. Kept under the wizard's 600s per-step
#                                 ceiling on purpose -- see _SB_ROLL_DEADLINE.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/seed-bootstrap.sh \
#       --domain=memql.localhost --owner-email=me@example.com \
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
cap_spec_param_required "domain"               "REQUIRED cluster domain, e.g. memql.localhost"
cap_spec_param_required "owner-email"          "REQUIRED email the cluster owner receives the first magic link at"
cap_spec_param_required "owner-first-name"     "REQUIRED cluster owner's first name"
cap_spec_param_required "owner-last-name"      "REQUIRED cluster owner's last name"
cap_spec_param_required "registration-mode"    "REQUIRED: open | domain_restricted | invite_only | waitlist"
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
# GETTING THE VALUES INTO THE RUNNING MESH (memql#3588)
#=============================================================================
#
# WRITING THE SECRET WAS NEVER THE JOB. Container environment is read ONCE, at
# container start. The install graph runs this step after `clusterUp` has waited
# for every workload to be Available, and the overlay mounts this Secret
# `optional: true` -- correct, because a plain `make up` never seeds it -- so on a
# fresh install the values landed in a Secret that no running process would ever
# read. `bootstrapComplete` said true, no cluster owner was created, and
# `magicLink` failed two steps later holding a failure it did not cause.
#
# So the step converges instead of writing and hoping: it restarts the nodes that
# read the Secret and waits for them back. `bootstrapComplete` now means the
# values are where something will read them.
#
# EXACTLY ONCE, AND NEVER AGAIN. Restarting identity is DESTRUCTIVE on a cluster
# nobody has claimed yet:
#
#   - identity emits the owner's magic link into its log at boot, and
#     magic-link.sh recovers it with `kubectl logs deploy/identity` -- from a LIVE
#     pod. Delete that pod and the only copy of the link goes with it.
#   - a restarted identity does NOT emit another. EvaluateAutoBootstrap returns
#     BootstrapActionSuppress once the clusterSettings row exists (memql#1829,
#     deliberately, so a restart cannot spam the owner).
#
# Together those make an unnecessary roll worse than no roll: it leaves a cluster
# that can never be claimed by this path. And re-running the graph IS the repair
# in this installer, so "run it twice" is ordinary rather than exotic. The
# decision is therefore RECORDED on the Secret -- `memql.io/rolled-for`, the
# digest of the values the mesh has actually been restarted for -- and compared,
# not merely checked for presence. Same values, no roll. Changed values, roll.
# Interrupted roll, roll again, because the marker is stamped only after every
# consumer is back.
#
# DELETE PODS RATHER THAN `rollout restart`. The Application syncs with
# `selfHeal: true` and ignores only `/spec/replicas`, so the pod-template
# annotation `rollout restart` writes is a diff ArgoCD reverts -- rolling the pods
# a SECOND time. The second roll would delete the very pod that emitted the magic
# link, which is the failure above arriving by another route. Deleting pods is not
# a spec change: ArgoCD is indifferent and the ReplicaSet brings up exactly one
# new generation.

# ROLL_ANNOTATION -- where the "what has the mesh been restarted for" fact lives.
readonly ROLL_ANNOTATION="memql.io/rolled-for"

# How many Deployments this run restarted. Set by roll_consumers.
_SB_ROLLED=0

# When the roll must be finished by, as a value of $SECONDS. ONE BUDGET FOR THE
# WHOLE ROLL, not one per node: the wizard kills a step at 600s (STEP_TIMEOUT_MS),
# and nine consumers each allowed their own 300s could only ever end in SIGKILL --
# exit 124, no envelope, no name of the node that hung. A shared deadline means the
# operator gets exit 5 saying which node did not come back, inside the time the
# step is allowed. `SECONDS` is a bash builtin (seconds since this shell started),
# so this needs no `date` and cannot differ between macOS and Linux.
_SB_ROLL_DEADLINE=0
# The budget that deadline came from, so the failure can name it.
_SB_ROLL_BUDGET=0

# sb_sha256 -- digest of stdin, portable across the two platforms this runs on.
function sb_sha256() {
    if command -v sha256sum &>/dev/null; then
        sha256sum | cut -d' ' -f1
    else
        shasum -a 256 | cut -d' ' -f1
    fi
}

# desired_digest <key=value...> -- the digest of the values being seeded.
#
# The API KEY IS HASHED, NEVER PASSED. Its content decides the digest (a rotated
# key has to roll the mesh) and the value itself must not reach a variable that
# could be logged, so the file is digested and the digest participates instead.
function desired_digest() {
    {
        local pair
        for pair in "$@"; do printf '%s\n' "$pair"; done
        if [[ -n "$_SB_KEY_FILE" ]]; then
            printf 'key-digest=%s\n' "$(sb_sha256 < "$_SB_KEY_FILE")"
        fi
    } | LC_ALL=C sort | sb_sha256
}

# recorded_digest <ns> <secret> -- what the mesh was last rolled for, or empty.
function recorded_digest() {
    local out
    out="$("${KUBECTL[@]}" get secret "$2" --namespace="$1" \
        -o go-template="{{index .metadata.annotations \"${ROLL_ANNOTATION}\"}}" 2>/dev/null || true)"
    # go-template prints `<no value>` for a key that is not there, and the
    # annotations map may be absent entirely.
    case "$out" in
        "<no value>"|"") printf '' ;;
        *) printf '%s' "$out" ;;
    esac
}

# secret_consumers <ns> <secret> -- "<deployment> <replicas>" for every
# Deployment that reads this Secret through envFrom.
#
# READ FROM THE CLUSTER, never a list here. The overlay names nine engine nodes
# today (deploy/k8s/overlays/local/kustomization.yaml); a hand-kept copy would
# silently stop covering the tenth on the day one is added -- and the symptom
# would be this exact bug, in one node, for one release.
function secret_consumers() {
    local ns="$1" secret="$2" name replicas refs
    while read -r name replicas refs; do
        [[ -n "$name" ]] || continue
        # Word-boundary match against the secretRef names, so `memql-secrets`
        # is never mistaken for `memql-secrets-old` or vice versa.
        case " $refs " in
            *" $secret "*) printf '%s %s\n' "$name" "${replicas:-0}" ;;
        esac
    done < <("${KUBECTL[@]}" get deployments --namespace="$ns" -o go-template='{{range .items}}{{.metadata.name}} {{if .spec.replicas}}{{.spec.replicas}}{{else}}0{{end}}{{range .spec.template.spec.containers}}{{range .envFrom}}{{if .secretRef}} {{.secretRef.name}}{{end}}{{end}}{{end}}{{"\n"}}{{end}}' 2>/dev/null || true)
}

# deployment_selector <ns> <deployment> -- its pod selector as a kubectl
# --selector string.
#
# Read off the Deployment rather than assuming the label convention, because a
# wrong selector here does not fail: it matches nothing, deletes nothing, waits
# for nothing and reports success.
function deployment_selector() {
    local out
    out="$("${KUBECTL[@]}" get deploy "$2" --namespace="$1" \
        -o go-template='{{range $k, $v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}' 2>/dev/null || true)"
    printf '%s' "${out%,}"
}

# pod_states <ns> <selector> -- "<pod> <ready>..." one line per pod.
function pod_states() {
    "${KUBECTL[@]}" get pods --namespace="$1" --selector="$2" \
        -o go-template='{{range .items}}{{.metadata.name}}{{range .status.containerStatuses}} {{.ready}}{{end}}{{"\n"}}{{end}}' 2>/dev/null || true
}

# wait_for_fresh_pods <ns> <deployment> <selector> <replicas> <stale-names>
#
# Waits until the Deployment is running <replicas> ready pods, none of them named
# in <stale-names>.
#
# ASSERTED ON THE PODS, NOT ON THE Available CONDITION. Immediately after a
# delete, the Deployment's status still describes the pods that are gone, so a
# `kubectl wait --for=condition=Available` returns instantly and truthfully about
# a moment that has passed. The question here is "is a NEW generation of pods
# serving", and only the pods can answer it.
#
# A Deployment at zero replicas is satisfied by having no pods -- the voice lane
# on any machine with no LiveKit credentials (memql#2416), which consumes this
# Secret and is deliberately switched off.
#
# WHY READY IS ENOUGH FOR IDENTITY, which is the assumption this whole fix rests
# on. `attemptAutoBootstrap` runs in the INTEGRATIONS phase (phase 4,
# app/integrations_identity.go), and /healthz is not served until TRANSPORT
# (phase 5). So a pod that reports ready has already attempted its bootstrap --
# the owner exists and its magic link is in that pod's log before the readiness
# probe can pass. Waiting on readiness is therefore waiting on the thing the next
# steps need, not merely on a process being up.
function wait_for_fresh_pods() {
    local ns="$1" deploy="$2" selector="$3" want="$4" stale="$5"
    local states name rest ready count ok

    while :; do
        states="$(pod_states "$ns" "$selector")"
        count=0; ok=1
        while read -r name rest; do
            [[ -n "$name" ]] || continue
            count=$((count + 1))
            case " $stale " in *" $name "*) ok=0 ;; esac
            for ready in $rest; do
                [[ "$ready" == "true" ]] || ok=0
            done
            # A pod with no container statuses at all is still starting.
            [[ -n "$rest" ]] || ok=0
        done <<< "$states"

        if [[ "$count" -eq "$want" && "$ok" -eq 1 ]]; then
            return 0
        fi
        if [[ "$SECONDS" -ge "$_SB_ROLL_DEADLINE" ]]; then
            cap_fail 5 "${deploy} did not come back after being restarted to pick up the bootstrap values (${count}/${want} pods ready when the ${_SB_ROLL_BUDGET}s budget ran out). The Secret is written but ${deploy} is not running with it, so the cluster has not bootstrapped. Inspect with: kubectl -n ${ns} get pods --selector=${selector}"
        fi
        sleep 3
    done
}

# roll_consumers <ns> <secret> -- restart every node that reads the Secret, and
# wait for each back before returning. The count lands in _SB_ROLLED.
#
# NOT VIA STDOUT, and this is not a style choice: cap_fail writes the result
# ENVELOPE to stdout, so a `$(roll_consumers ...)` would capture the envelope of
# its own failure into a variable and the operator would get
# "aborted without an explicit result" instead of the reason.
function roll_consumers() {
    local ns="$1" secret="$2" deploy replicas selector stale rolled=0 entry
    _SB_ROLL_BUDGET="${MEMQL_BOOTSTRAP_ROLL_TIMEOUT:-240}"
    _SB_ROLL_DEADLINE=$((SECONDS + _SB_ROLL_BUDGET))
    # NOT `mapfile`: it is bash 4, and this installer runs on stock macOS bash
    # 3.2 (the constraint scripts/lib/agents.sh states). A read loop is the
    # portable form and the only one that cannot fail on an operator's machine
    # while passing every test on a Linux CI runner.
    local -a consumers=()
    local line
    while IFS= read -r line; do
        [[ -n "$line" ]] && consumers+=("$line")
    done < <(secret_consumers "$ns" "$secret")

    if [[ "${#consumers[@]}" -eq 0 ]]; then
        # Nothing reads it YET. This is what seeding before the workloads exist
        # looks like, and it is the ordering this step would prefer to have: the
        # values are simply read at first boot.
        cap_info "no Deployment reads ${secret} yet -- the values will be read when the workloads first start."
        _SB_ROLLED=0
        return 0
    fi

    for entry in "${consumers[@]}"; do
        deploy="${entry%% *}"
        replicas="${entry##* }"
        selector="$(deployment_selector "$ns" "$deploy")"
        if [[ -z "$selector" ]]; then
            cap_fail 5 "could not read the pod selector for deployment ${deploy}; refusing to guess which pods to restart"
        fi

        stale="$(pod_states "$ns" "$selector" | awk '{print $1}' | tr '\n' ' ')"
        if [[ -n "${stale// /}" ]]; then
            cap_step "restarting ${deploy} so it reads ${secret}"
            "${KUBECTL[@]}" delete pods --namespace="$ns" --selector="$selector" >&2 \
                || cap_fail 5 "could not restart ${deploy} to pick up the bootstrap values"
            rolled=$((rolled + 1))
        fi
        wait_for_fresh_pods "$ns" "$deploy" "$selector" "$replicas" "$stale"
    done

    cap_info "${rolled} of ${#consumers[@]} node(s) restarted; every consumer of ${secret} is running with the seeded values."
    _SB_ROLLED="$rolled"
}

# record_roll <ns> <secret> <digest> -- stamp what the mesh is now running.
#
# AFTER the roll, never before: a marker written first and interrupted second
# would tell the next run that the mesh has values it does not have, and that run
# would skip the roll it needed to do.
function record_roll() {
    "${KUBECTL[@]}" annotate secret "$2" --namespace="$1" \
        "${ROLL_ANNOTATION}=$3" --overwrite >&2 \
        || cap_fail 5 "could not record the roll marker on Secret $2; a later run cannot tell whether the mesh has these values"
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

    # THE HALF THAT WAS MISSING (memql#3588). A Secret nothing has read is not a
    # bootstrapped cluster.
    local digest="" recorded="" rolled=0 converged=false
    if [[ -z "$dry_run" ]]; then
        digest="$(desired_digest "${from_args[@]}")"
        recorded="$(recorded_digest "$ns" "$name")"
        if [[ "$recorded" == "$digest" ]]; then
            # Already restarted for exactly these values. Rolling again would
            # delete the identity pod whose log holds the owner's only magic
            # link, and a restarted identity does not emit another.
            cap_info "the mesh is already running with these values -- nothing restarted."
        else
            roll_consumers "$ns" "$name"
            rolled="$_SB_ROLLED"
            record_roll "$ns" "$name" "$digest"
        fi
        converged=true
    fi

    [[ -z "$dry_run" ]] && cap_changed
    cap_result_set_raw meshConverged   "$converged"
    cap_result_set_raw nodesRestarted  "${rolled:-0}"
    cap_result_set     namespace       "$ns"
    cap_result_set     secret          "$name"
    cap_result_set     domain          "$domain"
    cap_result_set     ownerEmail      "$owner_email"
    cap_result_set     registrationMode "$mode"
    cap_result_set     providerKeyEnv  "$_SB_KEY_ENV"
    # WHAT THIS NOW CLAIMS (memql#3588). Not "a Secret was written" -- that was
    # true of the runs that left the cluster unbootstrapped -- but "the values are
    # written AND every node that reads them is running with them". Everything
    # that could make the second half false has already exited non-zero above, so
    # reaching here is the claim.
    #
    # On a dry run it means neither, which is why it reports the dry run's own
    # answer rather than a bare true.
    cap_result_set_raw bootstrapComplete "$( [[ -n "$dry_run" ]] && echo false || echo true )"
    cap_result_set_raw providerKeySeeded "$( [[ -n "$_SB_KEY_FILE" ]] && echo true || echo false )"
    cap_result_set_raw keyCount        "$_SB_KEY_COUNT"
    cap_result_set_raw dryRun          "$( [[ -n "$dry_run" ]] && echo true || echo false )"
    cap_ok
}

main "$@"
