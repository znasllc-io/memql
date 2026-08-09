#!/usr/bin/env bash
#
# scripts/install/e2e-baseline.sh
# ===============================
#
# install.e2eBaseline -- snapshot the surfaces the installer touches, and
# compare a later snapshot against an earlier one.
#
# WHY THIS EXISTS
#
# "The uninstall looks clean" is an opinion. A receipt is a promise that every
# artifact the installer put on the machine can be taken back, and the only
# thing that can hold that promise to account is a BEFORE/AFTER DIFF of the
# machine itself. So: snapshot, install, uninstall, snapshot, diff. A
# difference is a receipt that lied.
#
# THE FOUR SURFACES, and exactly what each one is
#
#   hosts         the sha256 of the hosts file, plus whether the installer's
#                 marked block is present. The block is the whole point of
#                 hosts-entries.sh: other people's lines are not ours to touch,
#                 so an uninstall must leave the file byte-identical.
#   memqlHome     every FILE under ~/.memql, as "sha256  relative/path". Files,
#                 not directories: an empty ~/.memql/bin left behind is not an
#                 artifact anybody installed, and treating it as drift would
#                 make the diff noisy about the one thing it does not care
#                 about. Directories that still CONTAIN something show up
#                 through their files.
#   dockerImages  every local image as "repo:tag id". The install path pulls
#                 and builds images into the local daemon; nothing should
#                 survive a teardown.
#   k3dClusters   every k3d cluster by name. Absent k3d is recorded as
#                 "<k3d not installed>" -- which is itself the right answer
#                 before an install and after an uninstall that removed it.
#
# WHAT A SNAPSHOT IS NOT: it is not a filesystem-wide diff. A trust store, a
# kubeconfig, apt state and $HOME outside ~/.memql are all outside these four
# surfaces, so this script proves nothing about them. Say so rather than imply
# otherwise -- the workflow that drives it repeats the caveat.
#
# Usage:
#   e2e-baseline.sh --mode=snapshot --out=/tmp/before.txt
#   e2e-baseline.sh --mode=compare  --baseline=/tmp/before.txt --out=/tmp/after.txt
#
# Refs: #3374 #3357

set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"

cap_init "install.e2eBaseline" \
    "Snapshot the four surfaces a local-cluster install touches, and diff a later snapshot against an earlier one."
cap_spec_param "mode"       "snapshot (default) or compare"
cap_spec_param "out"        "file to write the snapshot to (required)"
cap_spec_param "baseline"   "snapshot to compare against (required for --mode=compare)"
cap_spec_param "home"       "installer home to walk (default \$HOME/.memql)"
cap_spec_param "hosts-file" "hosts file to fingerprint (default /etc/hosts)"
cap_spec_param "marker"     "hosts block marker (default memql)"

readonly SNAPSHOT_FORMAT="memql-install-e2e-baseline v1"

#=============================================================================
# THE SURFACES
#=============================================================================

# _sha256_of <path> -- portable sha256, bare digest.
function _sha256_of() {
    if command -v sha256sum &>/dev/null; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum &>/dev/null; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        cap_fail 4 "no sha256 tool found (need sha256sum or shasum)"
    fi
}

function surface_hosts() {
    local hosts_file="$1" marker="$2"
    printf '[hosts]\n'
    if [[ -f "$hosts_file" ]]; then
        printf 'sha256 %s\n' "$(_sha256_of "$hosts_file")"
        if grep -qF "# BEGIN ${marker}" "$hosts_file" 2>/dev/null; then
            printf 'block present\n'
        else
            printf 'block absent\n'
        fi
    else
        printf 'absent\n'
    fi
}

# Files only, sorted, each with its digest. See the header for why directories
# are deliberately not part of this surface.
function surface_memql_home() {
    local home="$1" f rel
    printf '[memqlHome]\n'
    if [[ ! -d "$home" ]]; then
        printf 'absent\n'
        return 0
    fi
    while IFS= read -r f; do
        [[ -z "$f" ]] && continue
        rel="${f#"${home}"/}"
        printf '%s  %s\n' "$(_sha256_of "$f")" "$rel"
    done < <(find "$home" -type f 2>/dev/null | LC_ALL=C sort)
}

function surface_docker_images() {
    printf '[dockerImages]\n'
    if ! command -v docker &>/dev/null; then
        printf '<docker not installed>\n'
        return 0
    fi
    if ! docker info &>/dev/null; then
        printf '<docker daemon unreachable>\n'
        return 0
    fi
    docker image ls --all --format '{{.Repository}}:{{.Tag}} {{.ID}}' 2>/dev/null | LC_ALL=C sort
}

function surface_k3d_clusters() {
    printf '[k3dClusters]\n'
    if ! command -v k3d &>/dev/null; then
        printf '<k3d not installed>\n'
        return 0
    fi
    k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | LC_ALL=C sort
}

#=============================================================================
# SNAPSHOT / COMPARE
#=============================================================================

# write_snapshot <out> <home> <hosts-file> <marker>
function write_snapshot() {
    local out="$1" home="$2" hosts_file="$3" marker="$4" dir
    dir="$(dirname "$out")"
    mkdir -p "$dir" || cap_fail 5 "could not create ${dir}"
    {
        printf '# %s\n' "$SNAPSHOT_FORMAT"
        surface_hosts "$hosts_file" "$marker"
        surface_memql_home "$home"
        surface_docker_images
        surface_k3d_clusters
    } > "$out" || cap_fail 5 "could not write ${out}"
}

# compare_snapshots <baseline> <current> -- prints the unified diff to STDERR
# (stdout stays reserved for the one result envelope) and returns 1 on drift.
function compare_snapshots() {
    local baseline="$1" current="$2"
    if diff -u "$baseline" "$current" >&2; then
        return 0
    fi
    return 1
}

# differing_sections <baseline> <current> -- JSON array of section names whose
# content is not identical, so the result says WHERE the machine drifted and
# not only that it did.
function differing_sections() {
    local baseline="$1" current="$2" section out="" first=1
    for section in hosts memqlHome dockerImages k3dClusters; do
        local a b
        a="$(awk -v s="[${section}]" '$0==s{f=1;next} /^\[/{f=0} f' "$baseline")"
        b="$(awk -v s="[${section}]" '$0==s{f=1;next} /^\[/{f=0} f' "$current")"
        if [[ "$a" != "$b" ]]; then
            [[ "$first" == "1" ]] || out+=","
            first=0
            out+="\"${section}\""
        fi
    done
    printf '[%s]' "$out"
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local mode out baseline home hosts_file marker
    mode="$(cap_param mode snapshot)"
    out="$(cap_param out "")"
    baseline="$(cap_param baseline "")"
    home="$(cap_param home "${HOME}/.memql")"
    hosts_file="$(cap_param hosts-file /etc/hosts)"
    marker="$(cap_param marker memql)"

    cap_require out "$out"

    case "$mode" in
        snapshot)
            cap_step "Snapshotting the four install surfaces -> ${out}"
            write_snapshot "$out" "$home" "$hosts_file" "$marker"
            cap_result_set     mode     "snapshot"
            cap_result_set     snapshot "$out"
            cap_result_set     digest   "$(_sha256_of "$out")"
            cap_result_set_raw taken    true
            cap_ok
            ;;
        compare)
            cap_require baseline "$baseline"
            [[ -f "$baseline" ]] || cap_fail 2 "baseline snapshot not found: ${baseline}"
            cap_step "Snapshotting again and diffing against ${baseline}"
            write_snapshot "$out" "$home" "$hosts_file" "$marker"

            local sections
            sections="$(differing_sections "$baseline" "$out")"
            cap_result_set     mode     "compare"
            cap_result_set     snapshot "$out"
            cap_result_set     baseline "$baseline"
            cap_result_set_raw differingSections "$sections"

            if compare_snapshots "$baseline" "$out"; then
                cap_result_set_raw identical true
                cap_info "The machine is byte-identical to its baseline on all four surfaces."
                cap_ok
            fi
            cap_result_set_raw identical false
            cap_fail 5 "the machine did not return to its baseline; sections that drifted: ${sections}"
            ;;
        *)
            cap_fail 2 "unknown --mode '${mode}' (expected: snapshot, compare)"
            ;;
    esac
}

main "$@"
