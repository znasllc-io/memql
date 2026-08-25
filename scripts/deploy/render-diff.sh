#!/usr/bin/env bash
#
# scripts/deploy/render-diff.sh
# =============================
#
# Capability: deploy.renderDiff -- render an instance overlay at two engine
# refs and diff the OBJECT SETS, before either one is applied.
#
# Backend for the `renderDiff` deployment action (memql#4483, epic memql#4490).
#
# A VERSION BUMP IS A MIGRATION. Moving one instance from ?ref=v0.19.6 to
# ?ref=v0.19.8 was ONE LINE in a kustomization. It turned up three breaks, none
# of which announce themselves:
#
#   1. A patch that becomes a DUPLICATE and fails. The engine's own overlay had
#      since adopted the exact workaround the instance carried locally, and a
#      JSON-patch `remove` against an already-absent path is an ERROR -- so the
#      instance's workaround had to be reverted in the SAME commit that bumped
#      the ref, or the render dies.
#   2. NEW OBJECTS WITH NEW INFRASTRUCTURE PREREQUISITES. The target added a
#      ClusterIssuer whose solver is azureDNS. An instance whose DNS is not in
#      Azure gets an issuer pointing at a zone that does not exist and a
#      wildcard order retrying against Let's Encrypt rate limits forever. A
#      manifest bump triggered SUBSTRATE work.
#   3. A CHANGED DELIVERY MECHANISM for a value that already existed, leaving
#      the instance's own patches redundant or conflicting.
#
# THE ASYMMETRY THAT MAKES THIS DANGEROUS, and the reason a render diff beats
# reading a changelog:
#
#   | change                          | failure                        |
#   |---------------------------------|--------------------------------|
#   | a patch whose target disappeared | render ERROR -- loud, safe     |
#   | a NEW OBJECT NOBODY PATCHES      | renders with the engine's      |
#   |                                  | PLACEHOLDER -- silent          |
#
# The target began generating a `portal-front-door` the instance had been
# shipping by hand. With no patch for it the render emitted the engine's
# placeholder host AND BUILT CLEANLY: a front door pointing at a domain the
# operator does not own, with a green build. Flagging NEW objects is the silent
# half, and it is the cheap half.
#
# THE MEASUREMENT THAT JUSTIFIES THE GATE. That one ?ref= line cost three
# retired workarounds (two of which would have failed the render), one new
# object needing a host patch, one new certificate regime needing real Azure
# substrate, one engine defect worked around, and nine digests repinned --
# about six iterations. NONE of it was discoverable from the release notes or
# the diff summary. ALL of it was discoverable in about fifteen minutes by
# rendering. That asymmetry is the whole argument.
#
# IT REPORTS, IT DOES NOT DECIDE. `passed`, `newKinds`, `newObjects`,
# `removedObjects` and `renderError` come back in the envelope; the automation's
# logic branches on them (contract rule 7). A caller that has reviewed a new
# kind and accepted it says so with --acceptKinds, which is a mechanical
# acknowledgement, not a decision taken inside the script.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok (the diff ran; read `passed`) | 2 bad param | 4 prerequisite missing | 5 the diff itself failed
#
# Refs: memql#4490 memql#4483 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.renderDiff" \
    "Render an instance overlay at two engine refs and report the object-set difference before applying either."

cap_spec_param_required "overlayPath" "path to the instance overlay whose kustomization pins the engine ref"
cap_spec_param_required "toRef"       "the engine ref being moved TO"
cap_spec_param "fromRef"     "the engine ref being moved FROM (default: the ref the overlay currently pins)"
cap_spec_param "acceptKinds" "comma-separated object kinds already reviewed and accepted for this instance -- a new kind not listed here sets passed=false"
cap_spec_param "outputDir"   "directory to write the two rendered manifests into, for a human to read"

cap_handle_meta "$@"
cap_parse_flags "$@"

OVERLAY_PATH="$(cap_param overlayPath "")"
TO_REF="$(cap_param toRef "")"
FROM_REF="$(cap_param fromRef "")"
ACCEPT_KINDS="$(cap_param acceptKinds "")"
OUTPUT_DIR="$(cap_param outputDir "")"

WORK=""
RENDER_PARENT=""
RENDER_ERROR=""
NEW_OBJECTS=""
REMOVED_OBJECTS=""
NEW_KINDS=""
UNACCEPTED_KINDS=""
RENDERER=""

function check_prerequisites() {
    if command -v kustomize &>/dev/null; then
        RENDERER="kustomize build"
    elif command -v kubectl &>/dev/null; then
        RENDERER="kubectl kustomize"
    else
        cap_fail 4 "neither kustomize nor kubectl is installed -- this gate's entire value is that it renders, so it must not report a clean diff without one"
    fi
    command -v python3 &>/dev/null || cap_fail 4 "python3 is not installed or not on PATH (used to read the rendered YAML)"
}

function validate_arguments() {
    [[ -n "$OVERLAY_PATH" ]] || cap_fail 2 "--overlayPath is required"
    [[ -n "$TO_REF"       ]] || cap_fail 2 "--toRef is required"
    [[ -d "$OVERLAY_PATH" ]] || cap_fail 2 "--overlayPath ${OVERLAY_PATH} is not a directory"
    [[ -f "${OVERLAY_PATH}/kustomization.yaml" ]] \
        || cap_fail 2 "${OVERLAY_PATH}/kustomization.yaml does not exist"
}

# TWO scratch locations, and the split is load-bearing.
#
# WORK holds the rendered YAML and the key lists; anywhere is fine.
#
# The render copies go BESIDE THE OVERLAY, at exactly its own depth, because an
# overlay reaches its bases by RELATIVE path (`../../base`) as often as by a
# remote `?ref=` URL. Copy it to /tmp -- or even one directory deeper -- and
# every relative base stops resolving, so the diff reports a render error that
# is an artefact of this script rather than a fact about either ref. For a gate
# that is worse than not running, because it looks exactly like finding 1.
#
# Both are removed by the EXIT trap, which fires on cap_fail too, so an
# interrupted run leaves nothing beside the operator's overlay.
function make_workdir() {
    WORK="$(mktemp -d)"
    RENDER_PARENT="$(cd "$(dirname "$OVERLAY_PATH")" && pwd)"
    trap 'rm -rf "$WORK" "${RENDER_PARENT}"/.render-diff.$$.* 2>/dev/null || true' EXIT
}

# current_ref reads the ref the overlay pins today, so --fromRef is optional
# and the common case ("what does moving to X change") needs one argument.
function current_ref() {
    grep -oE '\?ref=[A-Za-z0-9._/-]+' "${OVERLAY_PATH}/kustomization.yaml" 2>/dev/null \
        | head -1 | sed 's/^?ref=//'
}

# render_at <ref> <outfile> -- copy the overlay, swap every ?ref= to <ref>, and
# build. Returns 1 and sets RENDER_ERROR when the build fails, because a build
# FAILURE at the target ref is itself one of the three findings (the retired
# workaround), not an error to abort on.
function render_at() {
    local ref="$1" out="$2"
    local dir="${RENDER_PARENT}/.render-diff.$$.${ref//\//_}"
    rm -rf "$dir"; mkdir -p "$dir"
    cp -R "${OVERLAY_PATH}/." "$dir/"
    # Every ?ref= in the kustomization, not just the first: an overlay may pull
    # several bases from the engine repo and a partial swap renders a mixture
    # of two versions, which is a state no release ever had.
    find "$dir" -name 'kustomization.yaml' -exec \
        sed -i.bak -E "s|\?ref=[A-Za-z0-9._/-]+|?ref=${ref}|g" {} +
    find "$dir" -name '*.bak' -delete

    if ! $RENDERER "$dir" > "$out" 2> "${out}.err"; then
        RENDER_ERROR="$(head -c 2000 "${out}.err" | tr '\n' ' ')"
        return 1
    fi
    return 0
}

# object_keys reads a rendered stream into `Kind/namespace/name` lines. python3
# rather than grep: a `name:` inside a ConfigMap's data is indistinguishable
# from a metadata name by text, and this gate's answer must not depend on that.
function object_keys() {
    python3 - "$1" <<'PY'
import sys, io
try:
    import yaml
except ImportError:
    sys.stderr.write("PyYAML-unavailable\n"); sys.exit(3)
keys = set()
with io.open(sys.argv[1], encoding="utf-8") as fh:
    for doc in yaml.safe_load_all(fh):
        if not isinstance(doc, dict):
            continue
        kind = doc.get("kind")
        if not kind:
            continue
        md = doc.get("metadata") or {}
        keys.add("%s/%s/%s" % (kind, md.get("namespace") or "-", md.get("name") or "-"))
for k in sorted(keys):
    print(k)
PY
}

function diff_object_sets() {
    local a="${WORK}/from.yaml" b="${WORK}/to.yaml"

    if ! render_at "$FROM_REF" "$a"; then
        cap_warn "the overlay does not render at the CURRENT ref ${FROM_REF}: ${RENDER_ERROR}"
        RENDER_ERROR="from(${FROM_REF}): ${RENDER_ERROR}"
        return 0
    fi
    if ! render_at "$TO_REF" "$b"; then
        # This is finding 1: a patch whose target the target ref no longer has,
        # or that the target ref has already adopted. Loud and safe -- and the
        # repair is to retire the instance's own workaround IN THE SAME COMMIT
        # as the bump.
        cap_warn "the overlay does not render at the TARGET ref ${TO_REF}. This is usually a RETIRED WORKAROUND: a JSON-patch 'remove' against a path the target ref no longer has, or has already removed itself. The repair is to drop that patch in the same commit as the ref bump."
        RENDER_ERROR="to(${TO_REF}): ${RENDER_ERROR}"
        return 0
    fi

    if ! object_keys "$a" > "${WORK}/from.keys" 2>"${WORK}/from.keys.err"; then
        cap_fail 4 "$(cat "${WORK}/from.keys.err"): PyYAML is required to read the rendered manifests"
    fi
    object_keys "$b" > "${WORK}/to.keys"

    NEW_OBJECTS="$(comm -13 "${WORK}/from.keys" "${WORK}/to.keys" | paste -sd',' -)"
    REMOVED_OBJECTS="$(comm -23 "${WORK}/from.keys" "${WORK}/to.keys" | paste -sd',' -)"

    cut -d/ -f1 "${WORK}/from.keys" | sort -u > "${WORK}/from.kinds"
    cut -d/ -f1 "${WORK}/to.keys"   | sort -u > "${WORK}/to.kinds"
    NEW_KINDS="$(comm -13 "${WORK}/from.kinds" "${WORK}/to.kinds" | paste -sd',' -)"

    local k
    for k in ${NEW_KINDS//,/ }; do
        case ",${ACCEPT_KINDS}," in
            *",${k},"*) cap_info "new kind ${k} is in --acceptKinds; accepted" ;;
            *) UNACCEPTED_KINDS="${UNACCEPTED_KINDS:+${UNACCEPTED_KINDS},}${k}" ;;
        esac
    done

    if [[ -n "$NEW_OBJECTS" ]]; then
        cap_warn "NEW OBJECTS at ${TO_REF} that ${FROM_REF} did not render. This is the SILENT half: an object nobody patches renders with the ENGINE'S PLACEHOLDER and builds cleanly. Check each one for a value this instance must own: ${NEW_OBJECTS}"
    fi
    if [[ -n "$REMOVED_OBJECTS" ]]; then
        cap_info "objects present at ${FROM_REF} and absent at ${TO_REF}: ${REMOVED_OBJECTS}"
    fi
    if [[ -n "$UNACCEPTED_KINDS" ]]; then
        cap_warn "NEW OBJECT KINDS not in --acceptKinds: ${UNACCEPTED_KINDS}. A new kind can carry an infrastructure prerequisite this instance has not declared -- a DNS-01 ClusterIssuer needs a hosted zone, an identity and a role assignment that no manifest mentions."
    fi

    if [[ -n "$OUTPUT_DIR" ]]; then
        mkdir -p "$OUTPUT_DIR"
        cp "$a" "${OUTPUT_DIR}/rendered-${FROM_REF//\//_}.yaml"
        cp "$b" "${OUTPUT_DIR}/rendered-${TO_REF//\//_}.yaml"
        cap_info "wrote both rendered manifests to ${OUTPUT_DIR}"
    fi
}

function collect_result() {
    local passed="true"
    [[ -n "$RENDER_ERROR"      ]] && passed="false"
    [[ -n "$UNACCEPTED_KINDS"  ]] && passed="false"

    cap_result_set_raw "passed"  "$passed"
    cap_result_set "fromRef"     "$FROM_REF"
    cap_result_set "toRef"       "$TO_REF"
    cap_result_set "newObjects"     "$NEW_OBJECTS"
    cap_result_set "removedObjects" "$REMOVED_OBJECTS"
    cap_result_set "newKinds"       "$NEW_KINDS"
    cap_result_set "unacceptedKinds" "$UNACCEPTED_KINDS"
    cap_result_set "renderError"    "$RENDER_ERROR"
    return 0
}

function main() {
    validate_arguments
    check_prerequisites
    make_workdir

    : "${FROM_REF:=$(current_ref)}"
    [[ -n "$FROM_REF" ]] \
        || cap_fail 2 "no --fromRef given and ${OVERLAY_PATH}/kustomization.yaml pins no ?ref= to read one from"

    cap_info "rendering ${OVERLAY_PATH} at ${FROM_REF} and ${TO_REF} -- a ref is not a scalar to swap"
    diff_object_sets

    collect_result
    cap_ok
}

main "$@"
