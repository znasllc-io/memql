#!/usr/bin/env bash
#
# scripts/install/remove-artifact.sh
# ==================================
#
# Capability: install.removeArtifact -- remove ONE thing the installer put on
# this machine. One script, six kinds:
#
#   binary        the installed CLI binary at --path
#   checkout      the pinned source checkout at --path (install.cloneStack's
#                 --dest), removed only when the directory really is a git
#                 checkout -- a recursive delete needs a reason, not a path
#   hostsEntries  the installer's marked block inside the hosts file at --path
#                 (required, never defaulted: an uninstall must not guess which
#                 hosts file to edit)
#   mkcertCA      the local mkcert CA (mkcert -uninstall + the rootCA files)
#   stack         the local k3d cluster named --cluster
#   images        locally built/imported images matching --image-prefix
#
# --pre-existing=true IS AN UNCONDITIONAL REFUSAL (exit 3).
#
#   The install receipt records, per artifact, whether the installer CREATED it
#   or merely FOUND it already there. A developer who already had mkcert, or a
#   hosts entry, or a k3d cluster before this installer ran must not lose it
#   because they uninstalled memQL. The guard therefore lives AT THE POINT OF
#   ACTION -- inside every removal path, immediately before the mutation --
#   not in the executor that reads the receipt and not once at the top of
#   argument parsing. An executor bug, a hand-edited receipt and a direct shell
#   invocation then all hit the same wall, and that wall is the last thing
#   between the flag and the damage.
#
#   Anything that is not PLAINLY false ("false"/"0"/"no"/absent) refuses. A
#   garbled receipt value is a reason to stop, not a reason to delete.
#
# Removing something already gone is a successful no-op with changed=false: an
# uninstall that fails on a missing artifact makes a half-finished install
# impossible to clean up.
#
# EXIT CODES:
#
#   0  removed, or already absent (no-op)
#   2  bad param (unknown kind, missing path)
#   3  REFUSED: --pre-existing said the installer did not create this
#   4  prerequisite missing (k3d / docker / mkcert absent)
#   5  operation failed
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/remove-artifact.sh --kind=binary --path=/usr/local/bin/memql-cockpit
#   scripts/install/remove-artifact.sh --kind=hostsEntries --path=/etc/hosts
#   scripts/install/remove-artifact.sh --kind=stack --cluster=memql
#   scripts/install/remove-artifact.sh --print-spec
#
# Refs: #3367 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.removeArtifact" \
    "Remove one installer-created artifact: binary | checkout | hostsEntries | mkcertCA | stack | images."
cap_spec_param "kind"         "what to remove: binary | checkout | hostsEntries | mkcertCA | stack | images"
cap_spec_param "pre-existing" "the installer did NOT create this -- refuses unconditionally when set"
cap_spec_param "path"         "filesystem target (binary path, or the hosts file)"
cap_spec_param "dest"         "alias for --path, for receipts that spell it that way"
cap_spec_param "marker"       "hosts block marker (default memql; matches '# BEGIN <marker>')"
cap_spec_param "caroot"       "mkcert CA root directory (default: whatever 'mkcert -CAROOT' reports)"
cap_spec_param "cluster"      "k3d cluster name for kind=stack (default memql)"
cap_spec_param "image-prefix" "repository prefix for kind=images (default memql)"

#=============================================================================
# THE GUARD -- called at the point of action in EVERY removal path
#=============================================================================

_RA_PRE_EXISTING=""

# refuse_if_pre_existing <kind> <target>
# The last thing between the flag and the damage. Deliberately fail-safe:
# only a plainly-false value proceeds, so a receipt that arrives garbled
# ("null", "maybe", a stray quote) stops instead of deleting.
function refuse_if_pre_existing() {
    local kind="$1" target="$2" v="$_RA_PRE_EXISTING"
    case "$v" in
        ""|false|FALSE|False|0|no|NO|No) return 0 ;;
    esac
    cap_fail 3 "refusing to remove ${kind} (${target}): --pre-existing=${v} says the installer \
did not create it, and uninstalling memQL must never take something that was already here"
}

#=============================================================================
# KIND: binary
#=============================================================================

function remove_binary() {
    local path="$1"
    cap_require path "$path"
    refuse_if_pre_existing binary "$path"

    if [[ ! -e "$path" && ! -L "$path" ]]; then
        cap_info "No binary at ${path} -- nothing to remove."
        finish binary "$path" false "already absent"
    fi
    rm -f "$path" || cap_fail 5 "could not remove ${path}"
    cap_changed
    cap_info "Removed ${path}."
    finish binary "$path" true "removed"
}

#=============================================================================
# KIND: checkout
#=============================================================================

# The pinned source checkout install.cloneStack wrote (default ~/.memql/src).
# This is the one removal path that recurses, so it is the one that has to
# justify itself before it deletes: a recursive rm driven by a path out of a
# receipt is exactly the shape of an uninstall that eats someone's home
# directory. Three things must hold -- the path is a directory, it contains a
# .git, and it is not the invoking user's home or a filesystem root. A checkout
# that fails any of them is not ours to delete, and saying so beats guessing.
function remove_checkout() {
    local path="$1"
    cap_require path "$path"
    refuse_if_pre_existing checkout "$path"

    if [[ ! -e "$path" ]]; then
        cap_info "No checkout at ${path} -- nothing to remove."
        finish checkout "$path" false "already absent"
    fi
    if [[ ! -d "$path" ]]; then
        cap_fail 5 "${path} is not a directory -- refusing to treat it as a source checkout"
    fi

    # Resolve before comparing: a receipt carrying "~/.memql/src/.." must not
    # get past a string comparison against $HOME.
    local resolved
    resolved="$(cd "$path" 2>/dev/null && pwd -P)" || cap_fail 5 "could not resolve ${path}"
    case "$resolved" in
        /|"${HOME%/}"|/home|/Users|/root)
            cap_fail 3 "refusing to remove ${resolved}: that is a home or filesystem root, not a checkout"
            ;;
    esac
    if [[ ! -e "${resolved}/.git" ]]; then
        cap_fail 3 "refusing to remove ${resolved}: it holds no .git, so it is not the checkout we made"
    fi

    rm -rf "$resolved" || cap_fail 5 "could not remove ${resolved}"
    cap_changed
    cap_info "Removed the checkout at ${resolved}."
    finish checkout "$resolved" true "checkout removed"
}

#=============================================================================
# KIND: hostsEntries
#=============================================================================

# The installer writes its hostnames inside a marked block so uninstall can
# take back exactly what it wrote. /etc/hosts is a shared file; other people's
# lines are not ours to delete.
function remove_hosts_entries() {
    local path="$1" marker="$2"
    cap_require path "$path"
    refuse_if_pre_existing hostsEntries "$path"

    local begin="# BEGIN ${marker}" end="# END ${marker}"
    if [[ ! -f "$path" ]]; then
        cap_info "No hosts file at ${path} -- nothing to remove."
        finish hostsEntries "$path" false "already absent"
    fi
    if ! grep -qF "$begin" "$path"; then
        cap_info "No '${begin}' block in ${path} -- nothing to remove."
        finish hostsEntries "$path" false "no managed block present"
    fi

    local tmp
    tmp="$(mktemp)" || cap_fail 5 "could not create a temporary file"
    awk -v b="$begin" -v e="$end" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        skip == 0 { print }
    ' "$path" > "$tmp" || { rm -f "$tmp"; cap_fail 5 "could not rewrite ${path}"; }

    # Redirect INTO the existing file rather than mv over it: preserves the
    # inode, its ownership and its mode (this is usually /etc/hosts).
    cat "$tmp" > "$path" || { rm -f "$tmp"; cap_fail 5 "could not write ${path}"; }
    rm -f "$tmp"

    cap_changed
    cap_info "Removed the '${marker}' block from ${path}."
    finish hostsEntries "$path" true "managed block removed"
}

#=============================================================================
# KIND: mkcertCA
#=============================================================================

function remove_mkcert_ca() {
    local caroot="$1"

    # Guarded before the CAROOT lookup as well as before the mutation: a
    # refusal should never even ask the machine questions.
    refuse_if_pre_existing mkcertCA "${caroot:-<mkcert -CAROOT>}"

    if [[ -z "$caroot" ]]; then
        command -v mkcert &>/dev/null || cap_fail 4 "mkcert is not installed and no --caroot was given"
        caroot="$(mkcert -CAROOT 2>/dev/null | head -n 1 || true)"
        [[ -n "$caroot" ]] || cap_fail 5 "could not determine the mkcert CA root"
    fi
    refuse_if_pre_existing mkcertCA "$caroot"

    if [[ ! -f "${caroot}/rootCA.pem" ]]; then
        cap_info "No CA at ${caroot} -- nothing to remove."
        finish mkcertCA "$caroot" false "already absent"
    fi

    command -v mkcert &>/dev/null || cap_fail 4 "mkcert is not installed; cannot uninstall the local CA"
    cap_info "Uninstalling the mkcert CA from the system trust stores..."
    mkcert -uninstall >&2 || cap_fail 5 "mkcert -uninstall failed"
    rm -f "${caroot}/rootCA.pem" "${caroot}/rootCA-key.pem" || cap_fail 5 "could not remove the CA files in ${caroot}"

    cap_changed
    cap_info "Removed the mkcert CA at ${caroot}."
    finish mkcertCA "$caroot" true "uninstalled and CA files removed"
}

#=============================================================================
# KIND: stack
#=============================================================================

function remove_stack() {
    local cluster="$1"
    cap_require cluster "$cluster"
    refuse_if_pre_existing stack "$cluster"

    command -v k3d &>/dev/null || cap_fail 4 "k3d is not installed; cannot remove the local cluster"

    if ! k3d cluster list 2>/dev/null | grep -q "^${cluster}[[:space:]]"; then
        cap_info "No k3d cluster named '${cluster}' -- nothing to remove."
        finish stack "$cluster" false "already absent"
    fi
    cap_info "Deleting k3d cluster '${cluster}'..."
    k3d cluster delete "$cluster" >&2 || cap_fail 5 "k3d cluster delete ${cluster} failed"

    cap_changed
    finish stack "$cluster" true "cluster deleted"
}

#=============================================================================
# KIND: images
#=============================================================================

function remove_images() {
    local prefix="$1"
    cap_require image-prefix "$prefix"
    refuse_if_pre_existing images "$prefix"

    command -v docker &>/dev/null || cap_fail 4 "docker is not installed; cannot remove local images"

    local matches count=0
    matches="$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null \
                | grep -E "^${prefix}" | grep -v '<none>' | sort -u || true)"
    if [[ -z "$matches" ]]; then
        cap_info "No local images matching '${prefix}' -- nothing to remove."
        cap_result_set_raw count 0
        finish images "$prefix" false "already absent"
    fi

    local image
    while IFS= read -r image; do
        [[ -z "$image" ]] && continue
        cap_info "Removing image ${image}..."
        docker image rm -f "$image" >&2 || cap_fail 5 "docker image rm ${image} failed"
        count=$((count + 1))
    done <<< "$matches"

    cap_changed
    cap_result_set_raw count "$count"
    finish images "$prefix" true "removed ${count} image(s)"
}

#=============================================================================
# RESULT
#=============================================================================

# finish <kind> <target> <removed-bool> <detail> -- emits the envelope and exits 0.
function finish() {
    cap_result_set     kind    "$1"
    cap_result_set     target  "$2"
    cap_result_set_raw removed "$3"
    cap_result_set     detail  "$4"
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local kind path marker caroot cluster prefix
    kind="$(cap_param kind)"
    path="$(cap_param path "$(cap_param dest)")"
    marker="$(cap_param marker memql)"
    caroot="$(cap_param caroot)"
    cluster="$(cap_param cluster memql)"
    prefix="$(cap_param image-prefix memql)"
    _RA_PRE_EXISTING="$(cap_param pre-existing)"

    cap_require kind "$kind"

    case "$kind" in
        binary)       remove_binary "$path" ;;
        checkout)     remove_checkout "$path" ;;
        hostsEntries) remove_hosts_entries "$path" "$marker" ;;
        mkcertCA)     remove_mkcert_ca "$caroot" ;;
        stack)        remove_stack "$cluster" ;;
        images)       remove_images "$prefix" ;;
        *) cap_fail 2 "unknown kind: ${kind} (supported: binary, checkout, hostsEntries, mkcertCA, stack, images)" ;;
    esac
}

main "$@"
