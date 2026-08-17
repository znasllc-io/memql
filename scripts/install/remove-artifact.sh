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
#   mkcertCA      EVERYTHING install.mkcert wrote: the local CA (mkcert
#                 -uninstall + the rootCA files + its marker) AND the front-door
#                 pair it issued at --cert-file / --key-file
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
#   THE ONE PATH THAT DOES NOT CONSULT IT is the front-door pair under
#   kind=mkcertCA, and the omission is deliberate rather than missed
#   (memql#4071). A receipt carries ONE verdict per step, and this step's is
#   `result.caPreExisting` -- a fact about the CA, in a different directory,
#   whose answer differs from the pair's in both directions. So the pair is
#   gated on something strictly stronger and artifact-local: the provenance
#   marker the issuing run wrote beside it. No marker, no removal. Do not
#   "restore" the flag check there; it would answer a question about the wrong
#   file.
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
# shellcheck source=../lib/elevate.sh
source "${SCRIPT_DIR}/../lib/elevate.sh"

# The provenance marker mkcert-setup.sh writes beside a CA it created.
readonly MEMQL_CA_MARKER=".memql-created"
# ...and the one it writes beside a front-door pair it issued (memql#4071).
# Different filename, same job; see the mkcert-setup.sh header for why the two
# artifacts may not share one.
readonly MEMQL_PAIR_MARKER=".memql-issued"

_CAP_PRUNED=()
cap_init "install.removeArtifact" \
    "Remove one installer-created artifact: binary | checkout | hostsEntries | mkcertCA | stack | images."
cap_spec_param_required "kind"         "what to remove: binary | checkout | hostsEntries | mkcertCA | stack | images"
cap_spec_param "pre-existing" "the installer did NOT create this -- refuses unconditionally when set"
cap_spec_param "path"         "filesystem target (binary path, or the hosts file)"
cap_spec_param "dest"         "alias for --path, for receipts that spell it that way"
cap_spec_param "marker"       "hosts block marker (default memql; matches '# BEGIN <marker>')"
cap_spec_param "caroot"       "mkcert CA root directory (default: whatever 'mkcert -CAROOT' reports)"
cap_spec_param "cert-file"    "front-door certificate install.mkcert issued (kind=mkcertCA; nothing is removed without it)"
cap_spec_param "key-file"     "its private key"
cap_spec_param "cluster"      "k3d cluster name for kind=stack (default memql)"
cap_spec_param "image-prefix" "repository prefix for kind=images (default memql)"
cap_spec_param "prune-empty-parents" "after removing, rmdir up to two now-empty parent dirs (flag)"

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

    # NOTHING THERE IS NOT SOMETHING TO REFUSE (memql#3583). The guard used to
    # run first, so an artifact that had ALREADY BEEN REMOVED still produced
    # "refusing to remove ... it was already on this machine before the
    # install" -- a refusal about a thing that does not exist, and a summary
    # line claiming something was left in place when nothing was. An operator
    # who had just cleaned up by hand was told their uninstall had not worked.
    #
    # Absence is checked first now. What is not there cannot be taken from
    # anyone, so it reports the honest no-op; the guard still stands between the
    # flag and the damage for everything that IS there.
    if [[ ! -e "$path" && ! -L "$path" ]]; then
        cap_info "No binary at ${path} -- nothing to remove."
        finish binary "$path" false "already absent"
    fi
    refuse_if_pre_existing binary "$path"
    rm -f "$path" || cap_fail 5 "could not remove ${path}"
    cap_changed
    cap_info "Removed ${path}."
    prune_empty_parents "$path"
    finish binary "$path" true "removed"
}

#=============================================================================
# EMPTY-PARENT PRUNING (opt-in, bounded)
#=============================================================================

# prune_empty_parents <removed-path>
#
# The installer does not only write files, it writes the directories that hold
# them: install.binary --dest creates ~/.memql/bin, install.cloneStack creates
# ~/.memql/src, and both create ~/.memql itself. Removing only the contents
# leaves that tree behind, so a machine whose baseline had NO ~/.memql does not
# get back to baseline -- which the round-trip E2E caught as a memqlHome drift
# after every artifact inside had been correctly removed.
#
# This is deliberately NOT automatic. It runs only with --prune-empty-parents,
# which the uninstall graph passes for the steps whose artifacts live under the
# memQL home, so an operator removing a binary from /usr/local/bin can never
# have a parent directory disappear as a side effect they did not ask for.
#
# Three bounds, because "walk up deleting empty directories" is a dangerous
# shape: at most TWO levels (bin -> home is the deepest real case), each level
# must be genuinely empty, and the walk stops at $HOME, a filesystem root, or
# any path shallower than two components. rmdir rather than rm -rf: it fails
# rather than recurses if the directory turns out not to be empty.
function prune_empty_parents() {
    [[ -n "$(cap_flag prune-empty-parents)" ]] || return 0

    local dir level=0
    dir="$(dirname "$1")"
    while [[ "$level" -lt 2 ]]; do
        [[ -d "$dir" ]] || break
        local resolved
        resolved="$(cd "$dir" 2>/dev/null && pwd -P)" || break
        case "$resolved" in
            /|"${HOME%/}"|/home|/Users|/root|/usr|/usr/local|/usr/local/bin|/bin|/etc|/opt|/var|/tmp)
                break ;;
        esac
        # Shallower than two components is a system location, not ours.
        [[ "$(printf '%s' "${resolved#/}" | tr -cd '/' | wc -c)" -ge 1 ]] || break
        rmdir "$resolved" 2>/dev/null || break
        cap_info "Pruned the now-empty ${resolved}."
        _CAP_PRUNED+=("$resolved")
        dir="$(dirname "$resolved")"
        level=$((level + 1))
    done
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

    # Absence before the guard -- see remove_binary (memql#3583).
    if [[ ! -e "$path" ]]; then
        cap_info "No checkout at ${path} -- nothing to remove."
        finish checkout "$path" false "already absent"
    fi
    refuse_if_pre_existing checkout "$path"
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
    prune_empty_parents "$resolved"
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

    local begin="# BEGIN ${marker}" end="# END ${marker}"
    # Absence before the guard -- see remove_binary (memql#3583).
    if [[ ! -f "$path" ]]; then
        cap_info "No hosts file at ${path} -- nothing to remove."
        finish hostsEntries "$path" false "already absent"
    fi
    if ! grep -qF "$begin" "$path"; then
        cap_info "No '${begin}' block in ${path} -- nothing to remove."
        finish hostsEntries "$path" false "no managed block present"
    fi
    refuse_if_pre_existing hostsEntries "$path"

    local tmp
    tmp="$(mktemp)" || cap_fail 5 "could not create a temporary file"
    # PREFIX matching, and the sep=nl rule -- both mirror hosts-entries.sh's
    # scan_blocks, which is the authority on this block's shape.
    #
    # Exact matching ($0 == b) was a silent reversal failure. When the hosts
    # file did not end in a newline, hosts-entries.sh had to insert one before
    # appending, and it records that by writing the marker as
    # "# BEGIN memql sep=nl". The guard above greps with -F, which matches that
    # line as a prefix and says "there is a block here"; exact-match awk then
    # never recognised it, so `skip` never turned on. Every entry survived, only
    # the "# END memql" line was dropped, and the envelope still reported
    # removed=true / changed=true. Uninstall claimed the machine was clean while
    # leaving the hostnames resolving -- the exact failure the receipt exists to
    # prevent. Worse, the block was left unterminated, so a second removal had
    # no END to find.
    #
    # sep=nl also means the newline itself was ours: restoring byte-for-byte
    # requires dropping the trailing newline from the line that preceded the
    # BEGIN marker, which is what nonl[] does.
    awk -v b="$begin" -v e="$end" '
        { line[NR] = $0; drop[NR] = 0; nonl[NR] = 0 }
        END {
            inblock = 0
            for (i = 1; i <= NR; i++) {
                if (inblock == 0) {
                    if (substr(line[i], 1, length(b)) == b) {
                        inblock = 1
                        drop[i] = 1
                        if (substr(line[i], length(line[i]) - 6) == " sep=nl" && i > 1) {
                            nonl[i - 1] = 1
                        }
                    }
                } else {
                    drop[i] = 1
                    if (substr(line[i], 1, length(e)) == e) { inblock = 0 }
                }
            }
            if (inblock != 0) { exit 3 }
            for (i = 1; i <= NR; i++) {
                if (drop[i]) { continue }
                if (nonl[i]) { printf "%s", line[i] } else { printf "%s\n", line[i] }
            }
        }
    ' "$path" > "$tmp" || {
        local rc=$?
        rm -f "$tmp"
        [[ "$rc" == "3" ]] && cap_fail 5 "unterminated '${begin}' block in ${path} -- refusing to guess where it ends"
        cap_fail 5 "could not rewrite ${path}"
    }

    # Redirect INTO the existing file rather than mv over it: preserves the
    # inode, its ownership and its mode (this is usually /etc/hosts) -- and
    # /etc/hosts needs root, which an uninstall run from the wizard has no
    # terminal to ask for. `tee` is the elevated spelling of the same write;
    # see scripts/lib/elevate.sh for how the asking happens (memql#3562).
    if [[ -w "$path" ]]; then
        cat "$tmp" > "$path" || { rm -f "$tmp"; cap_fail 5 "could not write ${path}"; }
    else
        if ! elevate_begin "remove the memQL entries from ${path}"; then
            rm -f "$tmp"
            cap_result_set remedy "sudo $(printf '%q' "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")") --kind=hostsEntries --path=$(printf '%q' "$path") --marker=$(printf '%q' "$marker")"
            cap_fail 4 "${path} is not writable and $(elevate_no_ask_reason)"
        fi
        cap_info "editing ${path} as root -- $(elevate_explain)"
        if ! elevate_run tee "$path" < "$tmp" >/dev/null; then
            elevate_end
            rm -f "$tmp"
            cap_result_set remedy "sudo $(printf '%q' "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")") --kind=hostsEntries --path=$(printf '%q' "$path") --marker=$(printf '%q' "$marker")"
            cap_fail 4 "could not edit ${path} as root -- the password prompt was cancelled, or the password was not accepted"
        fi
        elevate_end
    fi
    rm -f "$tmp"

    cap_changed
    cap_info "Removed the '${marker}' block from ${path}."
    finish hostsEntries "$path" true "managed block removed"
}

#=============================================================================
# KIND: mkcertCA
#=============================================================================

# _RA_PAIR_REMOVED records whether the front-door pair went, so the envelope can
# say so even when the CA half finds nothing to do (or refuses).
_RA_PAIR_REMOVED=false

# remove_issued_pair <cert> <key>
#
# The other half of what install.mkcert wrote (memql#4071): the front-door
# certificate and its PRIVATE KEY. `kind=mkcertCA` used to mean the CA alone, so
# an uninstall reported OK on all seven steps and left ~/.memql/certs/dev.key on
# a machine that had had no ~/.memql at all before the install.
#
# THREE DECISIONS, each of which could reasonably have gone the other way:
#
# 1. IT RIDES kind=mkcertCA RATHER THAN BECOMING ITS OWN KIND. A receipt kind
#    needs an install step to write it, and a step carries exactly one receipt
#    (graph.go) while graph_test.go refuses a second uninstall step reversing the
#    same install step -- so its own kind would mean SPLITTING install.mkcert in
#    two. That split is not available: the pair is issued by the CA this same
#    process has just trusted, in one mkcert invocation, and is worthless signed
#    by any other. It would also put two checkboxes in front of the operator for
#    one decision.
#
# 2. THE PATHS ARE PARAMETERS WITH NO DEFAULT. scripts/lib/localtls.sh knows
#    where the pair lives and this script could source it -- but a removal that
#    defaults to $HOME/.memql/certs/dev.crt reaches into the real home of anyone
#    who runs it with no flags, `go test` included. The uninstall passes what the
#    install RECORDED (result.certFile / result.keyFile via REMOVAL_TARGETS in
#    editors/vscode/src/install/receipt.ts), which is both safer and more
#    accurate: an operator who issued to a custom path gets that path back.
#    Nothing recorded means nothing to remove -- the same conclusion
#    hostsEntries reaches, for the same reason.
#
# 3. THE MARKER IS THE ONLY AUTHORITY, and `--pre-existing` is not consulted
#    here. That flag carries this step's single verdict, `result.caPreExisting`,
#    which is a fact about the CA in another directory; the two answers differ in
#    both directions (see the mkcert-setup.sh header). So key material is removed
#    only where memQL can PROVE it wrote it, and the proof is the marker the
#    issuing run left beside it. No marker, no removal, and the run says so
#    rather than deciding quietly.
function remove_issued_pair() {
    local cert="$1" key="$2"

    # No certificate path is no marker path either, so there is nothing that
    # could authorise a deletion. Reported, not assumed: this is also the state a
    # hand-run `--kind=mkcertCA` with no flags is in.
    if [[ -z "$cert" ]]; then
        cap_info "No --cert-file given -- leaving any front-door pair alone."
        return 0
    fi

    local marker
    marker="$(dirname "$cert")/${MEMQL_PAIR_MARKER}"

    # Absence before the guard -- see remove_binary (memql#3583).
    if [[ ! -e "$cert" && ! -e "$key" && ! -e "$marker" ]]; then
        cap_info "No front-door pair at ${cert} -- nothing to remove."
        return 0
    fi

    if [[ ! -f "$marker" ]]; then
        cap_info "No ${MEMQL_PAIR_MARKER} beside ${cert}: memQL did not issue this pair, so it stays."
        return 0
    fi

    local targets=("$cert" "$marker")
    [[ -n "$key" ]] && targets+=("$key")
    rm -f "${targets[@]}" || cap_fail 5 "could not remove the front-door pair at ${cert}"
    _RA_PAIR_REMOVED=true
    cap_changed
    cap_info "Removed the front-door certificate and key memQL issued (${cert})."
    # The certs directory is ours and nothing else lives in it, so it goes the
    # way ~/.memql/bin does -- under the same opt-in flag, with the same bounds.
    # The CAROOT deliberately does NOT get this treatment: it can be a machine
    # location (~/.local/share/mkcert) whose shape memQL does not own.
    prune_empty_parents "$cert"
}

function remove_mkcert_ca() {
    local caroot="$1" cert="$2" key="$3"

    # THE PAIR GOES FIRST, and the order is load-bearing. The CA half can end in
    # `cap_fail 3` -- an operator's own CA, which memQL must not take -- and
    # cap_fail exits. Removing the pair afterwards would mean never removing it
    # on exactly the machines where the two provenance answers differ, which is
    # the case this whole marker scheme exists to get right.
    remove_issued_pair "$cert" "$key"
    # Published as its own field rather than folded into `removed`, which keeps
    # meaning THE CA: two artifacts under one kind cannot share one boolean, and
    # a caller reading "removed" would otherwise learn nothing about the half it
    # cares about. Set here so every exit below carries it, refusals included.
    cap_result_set_raw pairRemoved "$_RA_PAIR_REMOVED"

    local pair_note=""
    if [[ "$_RA_PAIR_REMOVED" == "true" ]]; then
        pair_note="; front-door pair removed"
    fi

    if [[ -z "$caroot" ]]; then
        # No receipt value to go on. Resolving CAROOT is a read, and the guard
        # below still runs before anything is touched.
        command -v mkcert &>/dev/null || cap_fail 4 "mkcert is not installed and no --caroot was given"
        caroot="$(mkcert -CAROOT 2>/dev/null | head -n 1 || true)"
        [[ -n "$caroot" ]] || cap_fail 5 "could not determine the mkcert CA root"
    fi

    # THE MARKER OUTRANKS THE RECEIPT, and only in the direction of doing less
    # harm (memql#3576).
    #
    # mkcert-setup.sh writes .memql-created beside the key material when it
    # creates a CA. That is a fact about the artifact, written by the run that
    # made it, and it survives the receipt -- which an uninstall deletes, taking
    # with it the only record of who created anything the uninstall refused to
    # remove. One run that could not tell then locked the answer in forever,
    # because the uninstall that would have cleared it is the thing being
    # blocked.
    #
    # So: a marker present means memQL created this CA and may take it. NO
    # marker falls back to the receipt's verdict, which is the conservative
    # half -- an unmarked CA on a machine whose receipt says "already here" is
    # still refused, exactly as before.
    # Absence before the guard -- see remove_binary (memql#3583).
    # This is the one that was actually reported: an operator removed the CA by
    # hand, ran the uninstall, and was told memQL was refusing to take a CA that
    # had not been there for an hour.
    if [[ ! -f "${caroot}/rootCA.pem" ]]; then
        cap_info "No CA at ${caroot} -- nothing to remove."
        finish mkcertCA "$caroot" false "already absent${pair_note}"
    fi

    if [[ -f "${caroot}/${MEMQL_CA_MARKER}" ]]; then
        cap_info "${caroot}/${MEMQL_CA_MARKER} says memQL created this CA."
    else
        refuse_if_pre_existing mkcertCA "$caroot"
    fi

    command -v mkcert &>/dev/null || cap_fail 4 "mkcert is not installed; cannot uninstall the local CA"
    cap_info "Uninstalling the mkcert CA from the system trust stores..."
    mkcert -uninstall >&2 || cap_fail 5 "mkcert -uninstall failed"
    # The marker goes with what it describes.
    rm -f "${caroot}/rootCA.pem" "${caroot}/rootCA-key.pem" "${caroot}/${MEMQL_CA_MARKER}" \
        || cap_fail 5 "could not remove the CA files in ${caroot}"

    cap_changed
    cap_info "Removed the mkcert CA at ${caroot}."
    finish mkcertCA "$caroot" true "uninstalled and CA files removed${pair_note}"
}

#=============================================================================
# KIND: stack
#=============================================================================

function remove_stack() {
    local cluster="$1"
    cap_require cluster "$cluster"

    command -v k3d &>/dev/null || cap_fail 4 "k3d is not installed; cannot remove the local cluster"

    # Absence before the guard -- see remove_binary (memql#3583).
    if ! k3d cluster list 2>/dev/null | grep -q "^${cluster}[[:space:]]"; then
        cap_info "No k3d cluster named '${cluster}' -- nothing to remove."
        finish stack "$cluster" false "already absent"
    fi
    refuse_if_pre_existing stack "$cluster"
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
    # Pruned parents are reported, never silent: an uninstall that removes a
    # directory says which one.
    local pruned="[" first=1 d
    for d in "${_CAP_PRUNED[@]:-}"; do
        [[ -z "$d" ]] && continue
        [[ "$first" == "1" ]] || pruned+=","
        first=0
        pruned+="\"$(cap_json_escape "$d")\""
    done
    pruned+="]"
    cap_result_set_raw prunedDirs "$pruned"
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local kind path marker caroot cert key cluster prefix
    kind="$(cap_param kind)"
    path="$(cap_param path "$(cap_param dest)")"
    marker="$(cap_param marker memql)"
    caroot="$(cap_param caroot)"
    # NO DEFAULT, deliberately -- see remove_issued_pair. Every other path here
    # takes its target from the receipt too; this is the only one whose default
    # would be a file in the invoking user's home.
    cert="$(cap_param cert-file)"
    key="$(cap_param key-file)"
    cluster="$(cap_param cluster memql)"
    prefix="$(cap_param image-prefix memql)"
    _RA_PRE_EXISTING="$(cap_param pre-existing)"

    cap_require kind "$kind"

    case "$kind" in
        binary)       remove_binary "$path" ;;
        checkout)     remove_checkout "$path" ;;
        hostsEntries) remove_hosts_entries "$path" "$marker" ;;
        mkcertCA)     remove_mkcert_ca "$caroot" "$cert" "$key" ;;
        stack)        remove_stack "$cluster" ;;
        images)       remove_images "$prefix" ;;
        *) cap_fail 2 "unknown kind: ${kind} (supported: binary, checkout, hostsEntries, mkcertCA, stack, images)" ;;
    esac
}

main "$@"
