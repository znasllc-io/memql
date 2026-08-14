#!/usr/bin/env bash
#
# scripts/install/hosts-entries.sh
# ================================
#
# Capability: install.hostsEntries -- add or remove the memQL front-door
# hostnames in the system hosts file, inside a delimited managed block.
#
# The local stack is reached exactly as staging is: through the front door at
# https://api.memql.localhost and https://identity.memql.localhost (env parity
# -- see docs/public/operate/environment-parity.md). Those names have to
# resolve to the loopback address, which on a developer machine means a hosts
# file entry. This capability owns that edit, on install and on uninstall.
#
# THE MANAGED BLOCK
#
#   # BEGIN memql
#   127.0.0.1 api.memql.localhost
#   127.0.0.1 identity.memql.localhost
#   127.0.0.1 mcp.memql.localhost
#   127.0.0.1 portal.memql.localhost
#   127.0.0.1 memql.localhost
#   # END memql
#
# Everything between the markers is ours; everything outside them is the
# operator's and is never rewritten. `remove` restores the file BYTE FOR BYTE
# -- an uninstall that "helpfully" normalises a missing trailing newline or
# collapses a blank line has corrupted a file the whole machine depends on.
#
# Byte-exactness has one wrinkle worth naming: when the file does NOT end in a
# newline we must insert one before the block, and a plain removal would leave
# that inserted newline behind. So the block records how it attached itself --
# the BEGIN marker is written as `# BEGIN memql sep=nl` in that case, and
# removal drops the newline it added along with the block. In the ordinary case
# (a file that ends in a newline) the marker is the bare `# BEGIN memql`.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/hosts-entries.sh --action=add    --confirm=add-memql-hosts
#   scripts/install/hosts-entries.sh --action=remove --confirm=remove-memql-hosts
#   scripts/install/hosts-entries.sh --action=add --hosts-file=/tmp/hosts \
#       --hostnames=a.memql.localhost,b.memql.localhost --ip=127.0.0.1 --confirm=add-memql-hosts
#   scripts/install/hosts-entries.sh --print-spec
#
# The edit needs write access to the hosts file, so a real run is normally
# `sudo scripts/install/hosts-entries.sh ...`. There is no prompt: the
# confirmation is the --confirm phrase (contract rule 3).
#
# Exit codes:
#   0 ok | 2 bad param | 3 refused (missing/incorrect --confirm)
#   4 prerequisite missing (hosts file absent or not writable)
#   5 operation failed (malformed managed block, write failed)
#
# Refs: #3361 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The caller's own --action and the confirm phrase this run required, held for
# the remedy line the wizard offers when the write needs root (memql#3551).
_HOSTS_ACTION=""
_HOSTS_CONFIRM=""
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/elevate.sh
source "${SCRIPT_DIR}/../lib/elevate.sh"
# shellcheck source=../lib/resolve.sh
source "${SCRIPT_DIR}/../lib/resolve.sh"

cap_init "install.hostsEntries" "Add or remove the memQL front-door hostnames in the system hosts file."
cap_spec_param_required "action"     "add | remove (required)"
cap_spec_param "hosts-file" "hosts file to edit (default: /etc/hosts)"
cap_spec_param "hostnames"  "comma/space separated hostnames (default: the memQL front door)"
cap_spec_param "domain"     "front-door apex; derives api.<d>, identity.<d>, mcp.<d>, portal.<d>, <d> (mutually exclusive with --hostnames)"
cap_spec_param "ip"         "address the hostnames resolve to (default: 127.0.0.1)"
cap_spec_param "confirm"    "exact phrase: 'add-memql-hosts' or 'remove-memql-hosts'"

readonly DEFAULT_HOSTS_FILE="/etc/hosts"
# The local front door. Keep in step with deploy/k8s/overlays/local, whose
# Ingresses carry the same apex as their committed default (memql#3593).
readonly DEFAULT_DOMAIN="memql.localhost"
readonly DEFAULT_HOSTNAMES="api.${DEFAULT_DOMAIN},identity.${DEFAULT_DOMAIN},mcp.${DEFAULT_DOMAIN},portal.${DEFAULT_DOMAIN},${DEFAULT_DOMAIN}"
readonly DEFAULT_IP="127.0.0.1"

readonly BLOCK_BEGIN="# BEGIN memql"
readonly BLOCK_END="# END memql"
readonly SEP_MARK=" sep=nl"          # BEGIN-marker suffix: the block inserted a newline
readonly CONFIRM_ADD="add-memql-hosts"
readonly CONFIRM_REMOVE="remove-memql-hosts"

#=============================================================================
# FILE MODEL
#
# The file is read once into two parallel arrays: the line bodies and, per
# line, whether it was newline-terminated in the file. Rendering replays them,
# which reproduces the original bytes exactly -- including a final line with no
# newline. Nothing here goes through "$(...)", which strips trailing newlines
# and is precisely how this kind of script normally loses byte-exactness.
#=============================================================================

_FILE_LINES=()
_FILE_TERM=()
_FILE_COUNT=0

function read_hosts_file() {
    local path="$1" content="" rest line
    _FILE_LINES=(); _FILE_TERM=(); _FILE_COUNT=0

    # -d '' reads to NUL, i.e. the whole file, newlines and all; it returns
    # non-zero at EOF, which is the normal case here.
    IFS= read -r -d '' content < "$path" || true

    rest="$content"
    while [[ -n "$rest" ]]; do
        if [[ "$rest" == *$'\n'* ]]; then
            line="${rest%%$'\n'*}"
            rest="${rest#*$'\n'}"
            _FILE_LINES[$_FILE_COUNT]="$line"
            _FILE_TERM[$_FILE_COUNT]=1
        else
            _FILE_LINES[$_FILE_COUNT]="$rest"
            _FILE_TERM[$_FILE_COUNT]=0
            rest=""
        fi
        _FILE_COUNT=$((_FILE_COUNT + 1))
    done
}

#=============================================================================
# BLOCK SCAN
#
# Marks every line belonging to a managed block for removal, and remembers
# where the first one started so an update rewrites it IN PLACE rather than
# migrating it to the end of the file (content the operator put after our
# block must stay after it).
#=============================================================================

_DROP=()          # per line: 1 = belongs to a managed block
_NONL=()          # per line: 1 = its newline was inserted by us, drop it
_INSERT_AT=-1     # index of the first BEGIN marker, -1 when there is no block
_INSERT_SEP=0     # the first block's sep=nl state
_BLOCK_COUNT=0

# scan_blocks -- returns 1 when a BEGIN marker is never closed by an END.
function scan_blocks() {
    local i in_block=0
    _DROP=(); _NONL=(); _INSERT_AT=-1; _INSERT_SEP=0; _BLOCK_COUNT=0

    for ((i = 0; i < _FILE_COUNT; i++)); do
        _DROP[$i]=0
        _NONL[$i]=0
    done

    for ((i = 0; i < _FILE_COUNT; i++)); do
        if [[ "$in_block" == "0" ]]; then
            if [[ "${_FILE_LINES[$i]}" == "${BLOCK_BEGIN}"* ]]; then
                in_block=1
                _DROP[$i]=1
                _BLOCK_COUNT=$((_BLOCK_COUNT + 1))
                local sep=0
                if [[ "${_FILE_LINES[$i]}" == *"${SEP_MARK}" ]]; then
                    sep=1
                    if [[ "$i" -gt 0 ]]; then
                        _NONL[$((i - 1))]=1
                    fi
                fi
                if [[ "$_INSERT_AT" -lt 0 ]]; then
                    _INSERT_AT=$i
                    _INSERT_SEP=$sep
                fi
            fi
        else
            _DROP[$i]=1
            if [[ "${_FILE_LINES[$i]}" == "${BLOCK_END}"* ]]; then
                in_block=0
            fi
        fi
    done

    [[ "$in_block" == "0" ]]
}

#=============================================================================
# RENDERING
#=============================================================================

# emit_line <index> -- replays one line with its original termination, unless
# the newline was one we inserted (then it is dropped).
function emit_line() {
    local i="$1" term="${_FILE_TERM[$1]}"
    if [[ "${_NONL[$i]}" == "1" ]]; then
        term=0
    fi
    if [[ "$term" == "1" ]]; then
        printf '%s\n' "${_FILE_LINES[$i]}"
    else
        printf '%s' "${_FILE_LINES[$i]}"
    fi
}

# print_block <sep> -- the managed block itself, always newline-terminated.
function print_block() {
    local sep="$1" host
    if [[ "$sep" == "1" ]]; then
        printf '%s%s\n' "$BLOCK_BEGIN" "$SEP_MARK"
    else
        printf '%s\n' "$BLOCK_BEGIN"
    fi
    for host in "${HOSTNAMES[@]}"; do
        printf '%s %s\n' "$IP" "$host"
    done
    printf '%s\n' "$BLOCK_END"
}

# render <mode> -- writes the whole desired file to stdout.
#   remove : every managed block dropped
#   upsert : every managed block dropped, a fresh one emitted where the first
#            one was (or appended at EOF when there was none)
function render() {
    local mode="$1" i sep

    if [[ "$mode" == "upsert" && "$_INSERT_AT" -lt 0 ]]; then
        # No block yet: append at EOF, inserting a separating newline (and
        # recording that we did) when the last line is unterminated.
        sep=0
        if [[ "$_FILE_COUNT" -gt 0 && "${_FILE_TERM[$((_FILE_COUNT - 1))]}" == "0" ]]; then
            sep=1
        fi
        for ((i = 0; i < _FILE_COUNT; i++)); do
            emit_line "$i"
        done
        if [[ "$sep" == "1" ]]; then
            printf '\n'
        fi
        print_block "$sep"
        return
    fi

    for ((i = 0; i < _FILE_COUNT; i++)); do
        if [[ "$mode" == "upsert" && "$i" -eq "$_INSERT_AT" ]]; then
            print_block "$_INSERT_SEP"
        fi
        if [[ "${_DROP[$i]}" == "1" ]]; then
            continue
        fi
        emit_line "$i"
    done
}

#=============================================================================
# PARAMETER VALIDATION
#=============================================================================

HOSTNAMES=()
IP=""

function parse_hostnames() {
    local raw="$1" host
    raw="${raw//,/ }"
    HOSTNAMES=()
    # Word splitting is the point here.
    # shellcheck disable=SC2206
    local parts=( $raw )
    for host in "${parts[@]:-}"; do
        [[ -z "$host" ]] && continue
        # No wildcards: a hosts file has no wildcard semantics, so '*.foo'
        # would silently resolve nothing. Reject it rather than pretend.
        if [[ ! "$host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
            cap_fail 2 "invalid hostname: '${host}' (letters, digits, '.' and '-' only; no wildcards)"
        fi
        HOSTNAMES+=("$host")
    done
    if [[ "${#HOSTNAMES[@]}" -eq 0 ]]; then
        cap_fail 2 "no hostnames given: --hostnames must name at least one host"
    fi
}

function validate_ip() {
    if [[ ! "$1" =~ ^[0-9A-Fa-f.:]+$ ]]; then
        cap_fail 2 "invalid ip: '${1}'"
    fi
}

function check_hosts_file() {
    local path="$1"
    if [[ ! -f "$path" ]]; then
        cap_fail 4 "hosts file not found: ${path}"
    fi
    if [[ ! -r "$path" ]]; then
        cap_fail 4 "hosts file is not readable: ${path}"
    fi
}

#=============================================================================
# APPLY
#=============================================================================

_TMP_FILE=""
function cleanup_tmp() {
    [[ -n "$_TMP_FILE" && -f "$_TMP_FILE" ]] && rm -f "$_TMP_FILE"
    return 0
}

# apply <mode> <path> -- renders the desired file, and writes it only when it
# actually differs. Returns 0 when it wrote, 1 when the file was already right.
function apply() {
    local mode="$1" path="$2"

    _TMP_FILE="$(mktemp "${TMPDIR:-/tmp}/memql-hosts.XXXXXX")"
    # COMPOSED with capability.sh's EXIT trap rather than replacing it. A bare
    # `trap cleanup_tmp EXIT` overwrote it, and with it the guarantee that an
    # unexpected abort still emits a failure envelope -- so from this line on,
    # a `set -e` death produced silence on stdout and the caller saw a bare
    # exit code with no message (memql#3562).
    trap 'cleanup_tmp; _cap_on_exit' EXIT
    render "$mode" > "$_TMP_FILE"

    if cmp -s "$_TMP_FILE" "$path"; then
        return 1
    fi
    # Write THROUGH the existing file so its inode, owner and mode survive --
    # a mv would hand /etc/hosts a fresh inode owned by whoever ran us. `tee`
    # is the elevated spelling of the same write, and takes the content on
    # stdin, which sudo leaves alone because the password comes from the
    # askpass helper rather than from there.
    if [[ -w "$path" ]]; then
        if ! cat "$_TMP_FILE" > "$path"; then
            cap_fail 5 "failed to write ${path}"
        fi
        return 0
    fi
    write_elevated "$path"
    return 0
}

# write_elevated <path> -- the same write, as root.
#
# The installer used to stop here and hand the operator a command (memql#3551),
# because a capability spawned by the wizard has no terminal and sudo cannot ask
# for a password without one. It can now ask through a desktop dialog
# (scripts/lib/elevate.sh), so the ordinary case is that this just works and the
# operator types their password once into a native prompt.
#
# The handoff remains for the case that is genuinely unautomatable -- over SSH,
# on a headless machine, in CI -- where there is no screen to put a dialog on.
function write_elevated() {
    local path="$1"

    if ! elevate_begin "update ${path}"; then
        offer_terminal_handoff "$path" \
            "hosts file is not writable: ${path}, and $(elevate_no_ask_reason)"
    fi

    cap_info "writing ${path} as root -- $(elevate_explain)"
    if ! elevate_run tee "$path" < "$_TMP_FILE" >/dev/null; then
        elevate_end
        # A cancelled dialog and a mistyped password land here together, and
        # both mean the same thing to the operator: nothing was written, and
        # the way forward is to try again or run it themselves.
        offer_terminal_handoff "$path" \
            "could not write ${path} as root -- the password prompt was cancelled, or the password was not accepted"
    fi
    elevate_end
}

# offer_terminal_handoff <path> <message> -- refuse, naming the exact command
# that would work, with THIS run's arguments in it. The wizard turns the remedy
# into a button that opens a terminal with the command already typed.
function offer_terminal_handoff() {
    local path="$1" message="$2"
    cap_result_set remedy "sudo $(printf '%q' "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")") --action=$(printf '%q' "$_HOSTS_ACTION") --hosts-file=$(printf '%q' "$path") --confirm=$(printf '%q' "$_HOSTS_CONFIRM")"
    cap_fail 4 "$message"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

# hostnames_for_domain <apex> -- the names a front door puts on a domain, apex
# last to match the block this script documents. The wildcard *.<apex> cannot
# go in a hosts file (no wildcard semantics), so each name is listed; sites
# added later need their own entry, which the site-hosting runbook covers.
function hostnames_for_domain() {
    local apex="$1"
    printf 'api.%s,identity.%s,mcp.%s,portal.%s,%s' "$apex" "$apex" "$apex" "$apex" "$apex"
}

# probe_hostnames -- decides whether the block is needed. Sets _PROBE_VERDICT to
# one of `absent`, `satisfied`, `conflict`, and _PROBE_DETAIL on a conflict.
#
# GLOBALS RATHER THAN STDOUT, deliberately: a `$(probe_hostnames)` call would
# run the whole function in a subshell, so the detail it set would be discarded
# and the refusal would name no address at all.
#
# THREE OUTCOMES, NOT TWO. An operator whose own DNS already answers 127.0.0.1
# has done this job; writing anyway costs a sudo prompt for no effect. A name
# answering somewhere ELSE is neither -- shadowing a record they may depend on
# is the wrong repair, so it is refused rather than overwritten. Partial
# resolution is refused for its own reason: half through DNS and half through
# the hosts file is two sources of truth for one front door.
_PROBE_DETAIL=""
_PROBE_VERDICT="absent"
function probe_hostnames() {
    local host addrs addr resolved=0 unresolved=0
    _PROBE_DETAIL=""
    _PROBE_VERDICT="absent"
    for host in "${HOSTNAMES[@]}"; do
        addrs="$(resolve_addresses "$host")"
        if [[ -z "$addrs" ]]; then
            unresolved=$((unresolved + 1))
            continue
        fi
        while IFS= read -r addr; do
            [[ -z "$addr" ]] && continue
            if [[ "$addr" != "$IP" ]]; then
                _PROBE_DETAIL="${host} resolves to ${addr}, not ${IP}"
                _PROBE_VERDICT="conflict"
                return 0
            fi
        done <<< "$addrs"
        resolved=$((resolved + 1))
    done

    if [[ "$resolved" -gt 0 && "$unresolved" -gt 0 ]]; then
        _PROBE_DETAIL="${resolved} of ${#HOSTNAMES[@]} front-door names already resolve to ${IP} and the rest do not"
        _PROBE_VERDICT="conflict"
        return 0
    fi
    if [[ "$unresolved" -eq 0 ]]; then
        _PROBE_VERDICT="satisfied"
        return 0
    fi
    _PROBE_VERDICT="absent"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local action hosts_file hostnames_raw domain confirm mode expected
    action="$(cap_param action "")"
    _HOSTS_ACTION="$action"
    hosts_file="$(cap_param hosts-file "$DEFAULT_HOSTS_FILE")"
    # --domain and --hostnames are two spellings of one answer (memql#3593).
    # Both is a contradiction, and picking a winner silently would mean an
    # install whose hosts block names something the operator did not ask for.
    domain="$(cap_param domain "")"
    hostnames_raw="$(cap_param hostnames "")"
    if [[ -n "$domain" && -n "$hostnames_raw" ]]; then
        cap_fail 2 "--domain and --hostnames are two spellings of one answer; pass one"
    fi
    if [[ -n "$domain" ]]; then
        hostnames_raw="$(hostnames_for_domain "$domain")"
    elif [[ -z "$hostnames_raw" ]]; then
        hostnames_raw="$DEFAULT_HOSTNAMES"
    fi
    IP="$(cap_param ip "$DEFAULT_IP")"
    confirm="$(cap_param confirm "")"

    cap_require action "$action"
    case "$action" in
        add)    mode="upsert"; expected="$CONFIRM_ADD" ;;
        remove) mode="remove"; expected="$CONFIRM_REMOVE" ;;
        *)      cap_fail 2 "invalid --action '${action}': expected 'add' or 'remove'" ;;
    esac

    parse_hostnames "$hostnames_raw"
    validate_ip "$IP"

    # Refuse BEFORE reading or touching the file: an operator who did not
    # confirm gets a clean exit 3 and an untouched hosts file, always.
    _HOSTS_CONFIRM="$expected"
    cap_confirm_or_die "$confirm" "$expected"

    # Probe BEFORE touching the file, and only on the add path: removing our own
    # block is not conditional on what DNS currently says.
    local probe="absent"
    if [[ "$mode" == "upsert" ]]; then
        probe_hostnames
        probe="$_PROBE_VERDICT"
        case "$probe" in
            conflict)
                cap_fail 3 "refusing to write hosts entries: ${_PROBE_DETAIL} -- fix the DNS record, or pass --hostnames naming only the names that need an entry"
                ;;
            satisfied)
                cap_info "every front-door hostname already resolves to ${IP} -- nothing to write"
                ;;
        esac
    fi

    local wrote=false skipped=false
    if [[ "$probe" == "satisfied" ]]; then
        skipped=true
        cap_info "skipping ${hosts_file} (no elevation needed)"
    else
        check_hosts_file "$hosts_file"

        read_hosts_file "$hosts_file"
        if ! scan_blocks; then
            cap_fail 5 "malformed managed block in ${hosts_file}: '${BLOCK_BEGIN}' with no '${BLOCK_END}' -- fix it by hand"
        fi

        cap_step "${action} memQL hosts entries in ${hosts_file}"
        cap_info "hostnames: ${HOSTNAMES[*]}"

        if apply "$mode" "$hosts_file"; then
            wrote=true
            cap_changed
            cap_info "${hosts_file} updated."
        else
            cap_info "${hosts_file} already correct -- no change."
        fi
    fi

    local block_present=false entries=0
    if [[ "$mode" == "upsert" ]]; then
        block_present=true
        entries="${#HOSTNAMES[@]}"
    fi

    local hostnames_json="" host first=1
    for host in "${HOSTNAMES[@]}"; do
        [[ "$first" == "1" ]] || hostnames_json+=","
        first=0
        hostnames_json+="\"$(cap_json_escape "$host")\""
    done

    cap_result_set     action       "$action"
    cap_result_set     hostsFile    "$hosts_file"
    cap_result_set     ip           "$IP"
    cap_result_set_raw hostnames    "[${hostnames_json}]"
    cap_result_set_raw blockPresent "$block_present"
    cap_result_set_raw entries      "$entries"
    cap_result_set_raw wrote        "$wrote"
    # What the probe found, and whether it made the write unnecessary. The
    # wizard reads `skipped` to render the step as done-without-elevation
    # rather than as a write it never saw happen (memql#3593).
    cap_result_set_raw skipped      "$skipped"
    cap_result_set     probe        "$probe"
    cap_result_set_raw blocksFound  "$_BLOCK_COUNT"
    cap_ok
}

main "$@"
