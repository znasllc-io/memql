#!/usr/bin/env bash
#
# scripts/deploy/promote-overlay.sh
# =================================
#
# Capability: overlay.promote -- pin one kustomize overlay's image digests to
# the digests another overlay is running. ENGINE PROMOTION, in one tree.
#
# Backend for Executor.RunPromote (component/deploycontrol/executor.go,
# memql#3769). It replaces scripts/release/promote.sh, which copied a release
# between two SEPARATE ArgoCD estates and left this repository with the product
# deploy estate (992deb41). Staging and production are now two namespaces in one
# cluster reconciled by one ArgoCD from one base (epic memql#3748), so both
# overlays are files in this checkout and a promote is an edit to one of them.
#
# WHAT A PROMOTE IS, EXACTLY. Every image the target overlay pins and the source
# overlay also pins takes the source's digest. Nothing else moves: not the
# namespace, not the replica counts, not the environment ConfigMap -- those are
# the VALUES that make the two overlays two environments, and copying them would
# make production into a second staging.
#
# WHAT A PROMOTE IS NOT. It is not a rebuild -- the digests are already-built,
# already-exercised product-agnostic engine images. It carries no trained state:
# a promoted CONSTRUCT is a v1:authoring:construct row in the environment's own
# Postgres schema (memql#3746 + memql#3745), and this script writes YAML and
# touches no database, so a construct trained in staging is absent from
# production afterwards. That is the behaviour most likely to be reported as a
# bug and it is the design; training production is an explicit act against
# production.
#
# NO DECISIONS INSIDE (contract rule 5). --from and --to are supplied by the
# caller. This script does not know that production is promoted from staging and
# must not learn it: that mapping is one table in the one component whose
# subject is environments (component/deploycontrol/environment.go). Nor does it
# commit -- the caller owns that, because `git revert` of the promote commit is
# the rollback and the two halves of that contract belong together.
#
# THE DIFF IS A REVIEW SURFACE, and the reason this script REFUSES rather than
# guesses. An image production pins and staging does not (or the reverse) is a
# question with no mechanical answer -- prod deliberately pins memql-mcp closed
# with an all-zeros digest while staging runs a real one -- so an asymmetry is
# exit 3 with both names in the message, not a silent add or a silent skip.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused (asymmetric or unpinned image sets)
#             | 5 edit failed
#
# Refs: memql#3769 memql#3748 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "overlay.promote" "Pin one overlay's image digests to the digests another overlay is running."
cap_spec_param_required "from" "source overlay directory -- the environment whose digests are copied"
cap_spec_param_required "to"   "target overlay directory -- the environment being pinned"
cap_spec_param "version" "release version being promoted (provenance only; the digests come from --from)"
cap_spec_param "dryRun"  "report the intended pins without editing the target overlay"

#=============================================================================
# OVERLAY READING
#=============================================================================

# kustomization_path <overlay-dir> -- the file that is an overlay's single image
# authority. Resolved relative to the repository root so a caller may pass the
# repo-relative path the deploy console holds.
function kustomization_path() {
    local dir="$1"
    if [[ "$dir" != /* ]]; then
        dir="$(cd "${SCRIPT_DIR}/../.." && pwd)/${dir}"
    fi
    printf '%s/kustomization.yaml' "$dir"
}

# read_pins <kustomization.yaml> -- emit one "<name>\t<digest>" line per entry of
# the images: block, in file order.
#
# Parsed with awk rather than a YAML library because the edit below is a
# LINE-ORIENTED rewrite: the overlay is a heavily commented hand-maintained file
# and a round-trip through any YAML emitter would reformat it and drop every
# comment, turning a two-line promote diff into a whole-file one. The block is
# recognised by the `images:` key at column 0 and ends at the next column-0 key.
function read_pins() {
    awk '
        /^images:[[:space:]]*$/ { in_images = 1; next }
        in_images && /^[^[:space:]#]/ { in_images = 0 }
        in_images && $1 == "-" && $2 == "name:" { name = $3; next }
        in_images && $1 == "name:" { name = $2; next }
        in_images && $1 == "digest:" && name != "" { printf "%s\t%s\n", name, $2; name = "" }
    ' "$1"
}

#=============================================================================
# COMPARISON
#=============================================================================

# names_of <pins> -- the image names of a "<name>\t<digest>" listing, sorted.
function names_of() {
    printf '%s\n' "$1" | awk -F'\t' 'NF { print $1 }' | sort
}

# digest_of <pins> <name> -- the digest pinned for one image name, or empty.
function digest_of() {
    printf '%s\n' "$1" | awk -F'\t' -v want="$2" '$1 == want { print $2; exit }'
}

# require_pinned <pins> <label> -- exit 3 when any entry carries something that
# is not a sha256 digest. A promote whose source is a floating tag pins the
# target to a moving target, which is the one failure a digest-pinned GitOps
# estate exists to make impossible.
function require_pinned() {
    local pins="$1" label="$2" name digest
    while IFS=$'\t' read -r name digest; do
        [[ -z "$name" ]] && continue
        if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
            cap_fail 3 "${label} pins ${name} to ${digest:-<nothing>}, which is not a sha256 digest -- promoting it would pin the target to a moving reference"
        fi
    done <<< "$pins"
}

# require_same_image_set <from-pins> <to-pins> -- exit 3 when the two overlays do
# not name the same images. See the header: an asymmetry is a decision, and a
# decision does not live in a capability script.
function require_same_image_set() {
    local only_from only_to
    only_from="$(comm -23 <(names_of "$1") <(names_of "$2") | paste -sd, -)"
    only_to="$(comm -13 <(names_of "$1") <(names_of "$2") | paste -sd, -)"
    if [[ -n "$only_from" ]]; then
        cap_fail 3 "the source overlay pins images the target does not (${only_from}) -- whether the target should run them is a review decision, not a promote"
    fi
    if [[ -n "$only_to" ]]; then
        cap_fail 3 "the target overlay pins images the source does not (${only_to}) -- there is no digest to promote for them"
    fi
}

#=============================================================================
# EDIT
#=============================================================================

# apply_pins <target-file> <from-pins> -- rewrite the target's images: block so
# each entry carries the source's digest for the same name.
#
# The rewrite is in-place over the digest LINES only: same file, same comments,
# same order, same everything else. That is what makes the promote diff readable
# and what makes `git revert` of the resulting commit restore exactly the prior
# digests.
function apply_pins() {
    local target="$1" pins="$2" tmp
    tmp="$(mktemp)"
    awk -v pins="$pins" '
        BEGIN {
            n = split(pins, lines, "\n")
            for (i = 1; i <= n; i++) {
                if (split(lines[i], kv, "\t") == 2) want[kv[1]] = kv[2]
            }
        }
        /^images:[[:space:]]*$/ { in_images = 1; print; next }
        in_images && /^[^[:space:]#]/ { in_images = 0 }
        in_images && $1 == "-" && $2 == "name:" { name = $3 }
        in_images && $1 == "name:" && $2 != "" && $1 != "-" { name = $2 }
        in_images && $1 == "digest:" && name in want {
            sub(/digest:[[:space:]]*.*$/, "digest: " want[name])
            print
            name = ""
            next
        }
        { print }
    ' "$target" > "$tmp" || { rm -f "$tmp"; cap_fail 5 "rewriting ${target} failed"; }
    mv "$tmp" "$target" || cap_fail 5 "replacing ${target} failed"
}

#=============================================================================
# MAIN
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local from to version dry fromFile toFile fromPins toPins changed name fromDigest toDigest
    from="$(cap_param from "")"
    to="$(cap_param to "")"
    version="$(cap_param version "")"
    dry="$(cap_param dryRun "true")"
    cap_require from "$from"
    cap_require to "$to"

    fromFile="$(kustomization_path "$from")"
    toFile="$(kustomization_path "$to")"
    [[ -f "$fromFile" ]] || cap_fail 2 "source overlay has no kustomization.yaml at ${fromFile}"
    [[ -f "$toFile" ]]   || cap_fail 2 "target overlay has no kustomization.yaml at ${toFile}"
    [[ "$fromFile" != "$toFile" ]] || cap_fail 2 "--from and --to name the same overlay (${fromFile}); a promote moves digests between two environments"

    fromPins="$(read_pins "$fromFile")"
    toPins="$(read_pins "$toFile")"
    [[ -n "$fromPins" ]] || cap_fail 3 "source overlay ${fromFile} pins no images -- there is nothing to promote"
    [[ -n "$toPins" ]]   || cap_fail 3 "target overlay ${toFile} pins no images -- it is not a digest-pinned overlay"

    require_pinned "$fromPins" "source overlay ${fromFile}"
    require_same_image_set "$fromPins" "$toPins"

    # Count only what actually MOVES. Promoting a version that is already live is
    # an idempotent no-op, and the caller uses `changed` to decide whether there
    # is a commit to make -- so an honest count is what keeps an empty commit
    # from being manufactured.
    changed=0
    while IFS=$'\t' read -r name toDigest; do
        [[ -z "$name" ]] && continue
        fromDigest="$(digest_of "$fromPins" "$name")"
        [[ "$fromDigest" == "$toDigest" ]] || changed=$(( changed + 1 ))
    done <<< "$toPins"

    cap_result_set     from     "$from"
    cap_result_set     to       "$to"
    cap_result_set     version  "$version"
    cap_result_set_raw images   "$(printf '%s\n' "$toPins" | awk -F'\t' 'NF { print $1 }' | wc -l | tr -d ' ')"
    cap_result_set_raw repinned "$changed"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would re-pin ${changed} image digest(s) in ${toFile} from ${fromFile}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    cap_result_set_raw dryRun false
    if [[ "$changed" -eq 0 ]]; then
        cap_info "${toFile} already carries every digest ${fromFile} pins; nothing to promote"
        cap_ok
    fi

    cap_info "Promoting ${changed} image digest(s) from ${fromFile} into ${toFile}..."
    apply_pins "$toFile" "$fromPins"
    cap_changed
    cap_ok
}

main "$@"
