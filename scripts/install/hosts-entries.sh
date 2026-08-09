#!/usr/bin/env bash
#
# scripts/install/hosts-entries.sh
# ================================
#
# Capability: install.hostsEntries -- add or remove the memQL front-door
# hostnames in the system hosts file, inside a delimited managed block.
#
# The local stack is reached exactly as staging is: through the front door at
# https://cockpit.local.znas.io and https://identity.local.znas.io (env parity
# -- see docs/public/operate/environment-parity.md). Those names have to
# resolve to the loopback address, which on a developer machine means a hosts
# file entry. This capability owns that edit, on install and on uninstall.
#
# THE MANAGED BLOCK
#
#   # BEGIN memql
#   127.0.0.1 cockpit.local.znas.io
#   127.0.0.1 identity.local.znas.io
#   127.0.0.1 local.znas.io
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
#       --hostnames=a.local.znas.io,b.local.znas.io --ip=127.0.0.1 --confirm=add-memql-hosts
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
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.hostsEntries" "Add or remove the memQL front-door hostnames in the system hosts file."
cap_spec_param "action"     "add | remove (required)"
cap_spec_param "hosts-file" "hosts file to edit (default: /etc/hosts)"
cap_spec_param "hostnames"  "comma/space separated hostnames (default: the memQL front door)"
cap_spec_param "ip"         "address the hostnames resolve to (default: 127.0.0.1)"
cap_spec_param "confirm"    "exact phrase: 'add-memql-hosts' or 'remove-memql-hosts'"

readonly DEFAULT_HOSTS_FILE="/etc/hosts"
# The local front door. Keep in step with deploy/k8s/overlays/local.
readonly DEFAULT_HOSTNAMES="cockpit.local.znas.io,identity.local.znas.io,local.znas.io"
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
    trap cleanup_tmp EXIT
    render "$mode" > "$_TMP_FILE"

    if cmp -s "$_TMP_FILE" "$path"; then
        return 1
    fi
    if [[ ! -w "$path" ]]; then
        cap_fail 4 "hosts file is not writable: ${path} (re-run with sudo)"
    fi
    # Write THROUGH the existing file so its inode, owner and mode survive --
    # a mv would hand /etc/hosts a fresh inode owned by whoever ran us.
    if ! cat "$_TMP_FILE" > "$path"; then
        cap_fail 5 "failed to write ${path}"
    fi
    return 0
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local action hosts_file hostnames_raw confirm mode expected
    action="$(cap_param action "")"
    hosts_file="$(cap_param hosts-file "$DEFAULT_HOSTS_FILE")"
    hostnames_raw="$(cap_param hostnames "$DEFAULT_HOSTNAMES")"
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
    cap_confirm_or_die "$confirm" "$expected"

    check_hosts_file "$hosts_file"

    read_hosts_file "$hosts_file"
    if ! scan_blocks; then
        cap_fail 5 "malformed managed block in ${hosts_file}: '${BLOCK_BEGIN}' with no '${BLOCK_END}' -- fix it by hand"
    fi

    cap_step "${action} memQL hosts entries in ${hosts_file}"
    cap_info "hostnames: ${HOSTNAMES[*]}"

    local wrote=false
    if apply "$mode" "$hosts_file"; then
        wrote=true
        cap_changed
        cap_info "${hosts_file} updated."
    else
        cap_info "${hosts_file} already correct -- no change."
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
    cap_result_set_raw blocksFound  "$_BLOCK_COUNT"
    cap_ok
}

main "$@"
