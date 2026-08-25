#!/usr/bin/env bash
#
# scripts/deploy/report-instance-version.sh
# =========================================
#
# Capability: deploy.reportInstanceVersion -- report the THREE refs an instance
# carries, together, because they are allowed to differ and the difference is
# invisible when any one of them is quoted alone.
#
# Backend for the `reportInstanceVersion` deployment action (memql#4486, epic
# memql#4493).
#
# THE QUESTION THIS ANSWERS. Asked "what version is running?" on a live cloud
# instance, the honest answer required mapping the running image DIGEST back to
# a GHCR tag by querying the registry, because no node stated anything at boot.
# That half is fixed in the binary (core/buildinfo + app.newApp's boot line).
# This script fixes the other half, which is worse:
#
#   THE INTUITIVE ANSWER IS WRONG, NOT MERELY UNAVAILABLE.
#
# The instance declares ENGINE_REF=v0.19.6 and composes cloud-entry?ref=v0.19.6,
# and its BINARIES are 0.19.5 -- because a tag's image pins are written BEFORE
# that tag's own images exist, so v0.19.6's cloud-entry pins the 0.19.5 build and
# says so in a comment. Manifests and binaries legitimately differ by one
# release. So "we are on v0.19.6" is a statement about MANIFESTS that every
# reader hears as a statement about CODE, and during an incident that is the
# difference between reading the right diff and the wrong one.
#
# THE THREE REFS, and what each one actually is:
#
#   declared  -- ENGINE_REF in the instance's product.env. What an operator
#                SAYS the instance is on. Nothing enforces it.
#   rendered  -- the ?ref= the overlay's kustomization composes. What ArgoCD
#                will actually pull MANIFESTS from.
#   running   -- the image tag + digest the pods are executing. What the CODE
#                is.
#
# A fourth field, `reported`, is what the nodes say about THEMSELVES -- the boot
# line memql#4486 added. It is the only one of the four that cannot be stale in
# the direction that matters, because the binary is the thing answering. It is
# best-effort: pod logs rotate, and a cluster whose logs have aged out still
# deserves the other three.
#
# WHY THIS REPORTS RATHER THAN REFUSES. Divergence here is NORMAL -- the
# one-release skew above is structural, not a defect. A gate that failed on
# difference would fail on almost every correct instance, which is the same
# mistake memql#4475 names about the first Degraded. `agree` carries the answer
# and the run still succeeds; a human decides whether this particular
# divergence is the expected one.
#
# Refs: memql#4486 memql#4493 memql#4463 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.reportInstanceVersion" \
    "Report an instance's declared, rendered and running engine refs together, so a divergence between manifests and binaries stops being invisible."

cap_spec_param "namespace"   "namespace the mesh runs in (default: memql)"
cap_spec_param "overlayPath" "path to the instance overlay whose kustomization pins the engine ref -- the RENDERED ref"
cap_spec_param "productEnv"  "path to the instance's product.env, read for ENGINE_REF -- the DECLARED ref"
cap_spec_param "declaredRef" "the declared engine ref, passed directly instead of read from --productEnv"
cap_spec_param "dryRun"      "resolve what can be read from files and skip every cluster call"

cap_handle_meta "$@"
cap_parse_flags "$@"

NAMESPACE="$(cap_param namespace "memql")"
OVERLAY_PATH="$(cap_param overlayPath "")"
PRODUCT_ENV="$(cap_param productEnv "")"
DECLARED_REF="$(cap_param declaredRef "")"
DRY_RUN="$(cap_param dryRun "false")"

RENDERED_REF=""
RUNNING_IMAGES=""
RUNNING_DIGESTS=""
REPORTED=""
NOTES=""

function note() {
    NOTES="${NOTES:+${NOTES}; }$1"
    cap_info "$1"
    return 0
}

# ---------------------------------------------------------------------------
# declared -- ENGINE_REF from product.env
# ---------------------------------------------------------------------------
function resolve_declared() {
    if [[ -n "$DECLARED_REF" ]]; then
        note "declared ref given directly: ${DECLARED_REF}"
        return 0
    fi
    if [[ -z "$PRODUCT_ENV" ]]; then
        note "no --declaredRef and no --productEnv, so the DECLARED ref is unknown -- reported empty rather than guessed"
        return 0
    fi
    if [[ ! -f "$PRODUCT_ENV" ]]; then
        cap_fail 2 "--productEnv ${PRODUCT_ENV} does not exist. Reporting an empty declared ref for a path that was given and is wrong would hide a typo behind a field that looks answered."
    fi
    # Deliberately grep rather than source: product.env is a file this script
    # READS, and sourcing it would execute whatever it contains.
    DECLARED_REF="$(grep -E '^[[:space:]]*ENGINE_REF[[:space:]]*=' "$PRODUCT_ENV" 2>/dev/null | tail -1 | sed -E 's/^[^=]*=[[:space:]]*//; s/^["'"'"']//; s/["'"'"']$//' | tr -d '[:space:]')" || true
    if [[ -z "$DECLARED_REF" ]]; then
        note "${PRODUCT_ENV} declares no ENGINE_REF"
    else
        note "declared ref from ${PRODUCT_ENV}: ${DECLARED_REF}"
    fi
    return 0
}

# ---------------------------------------------------------------------------
# rendered -- the ?ref= the overlay composes
# ---------------------------------------------------------------------------
function resolve_rendered() {
    if [[ -z "$OVERLAY_PATH" ]]; then
        note "no --overlayPath, so the RENDERED ref is unknown"
        return 0
    fi
    local kustomization="${OVERLAY_PATH}/kustomization.yaml"
    if [[ ! -f "$kustomization" ]]; then
        cap_fail 2 "${kustomization} does not exist -- --overlayPath must name an overlay directory"
    fi
    RENDERED_REF="$(grep -oE '\?ref=[A-Za-z0-9._/-]+' "$kustomization" 2>/dev/null | head -1 | sed 's/^?ref=//')" || true
    if [[ -z "$RENDERED_REF" ]]; then
        note "${kustomization} pins no ?ref= -- it composes a local path, so manifests come from this checkout"
    else
        note "rendered ref from ${kustomization}: ${RENDERED_REF}"
    fi
    return 0
}

# ---------------------------------------------------------------------------
# running -- what the pods are actually executing
# ---------------------------------------------------------------------------
function resolve_running() {
    if [[ "$DRY_RUN" == "true" ]]; then
        note "--dryRun: not reading the cluster, so the RUNNING and REPORTED refs are unknown"
        return 0
    fi
    command -v kubectl &>/dev/null \
        || cap_fail 4 "kubectl is not installed or not on PATH. The running ref is the only one of the three that cannot be read from a file, so reporting the other two as the answer would be exactly the manifests-for-code substitution this script exists to stop."
    kubectl cluster-info &>/dev/null \
        || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first. See the note above: two of three refs is not an answer to 'what is running'."

    command -v python3 &>/dev/null || cap_fail 4 "python3 is not installed or not on PATH (used to read the pod list)"

    local pods_json
    if ! pods_json="$(kubectl get pods -n "$NAMESPACE" -o json 2>/dev/null)"; then
        cap_fail 5 "could not list pods in namespace ${NAMESPACE}"
    fi

    local parsed
    parsed="$(printf '%s' "$pods_json" | python3 -c '
import json, sys
doc = json.load(sys.stdin)
images, digests = set(), set()
for pod in doc.get("items", []):
    if pod.get("metadata", {}).get("deletionTimestamp"):
        continue
    spec = pod.get("spec", {})
    statuses = pod.get("status", {}).get("containerStatuses") or []
    by_name = {c.get("name"): c for c in statuses}
    for c in spec.get("containers", []):
        image = c.get("image", "")
        if not image or "memql" not in image:
            continue
        images.add(image)
        st = by_name.get(c.get("name"), {})
        image_id = st.get("imageID", "")
        if "@" in image_id:
            digests.add(image_id.split("@", 1)[1])
print(",".join(sorted(images)))
print(",".join(sorted(digests)))
')" || cap_fail 5 "could not parse the pod list from namespace ${NAMESPACE}"

    RUNNING_IMAGES="$(printf '%s' "$parsed" | sed -n '1p')"
    RUNNING_DIGESTS="$(printf '%s' "$parsed" | sed -n '2p')"

    if [[ -z "$RUNNING_IMAGES" ]]; then
        note "no memql pods found in namespace ${NAMESPACE} -- the mesh may be scaled to zero"
    else
        note "running images: ${RUNNING_IMAGES}"
    fi
    return 0
}

# ---------------------------------------------------------------------------
# reported -- what the binaries say about themselves (memql#4486's boot line)
# ---------------------------------------------------------------------------
function resolve_reported() {
    if [[ "$DRY_RUN" == "true" ]]; then
        return 0
    fi
    local line
    # Best-effort by construction: the boot line is written once per process, so
    # a pod whose logs have rotated legitimately no longer carries it. An absent
    # `reported` is reported absent, never inferred from the image tag -- an
    # inferred value would agree with `running` by construction and so could
    # never disagree with it, which is the one thing it is here to be able to do.
    # --limit-bytes BOUNDS THE FETCH, and the bound is what makes this safe in
    # the case the note below describes. The boot line is written once, at
    # process start, so it is at the very TOP of a pod's log -- a small prefix
    # always contains it when it exists.
    #
    # Without the bound, `grep -m1` terminates the stream early on a HIT and
    # not at all on a MISS: a cluster whose nodes predate memql#4486 has no
    # boot line anywhere, so every byte of every pod's log would be pulled
    # across the API server to discover that. That is exactly the cluster most
    # likely to be running this, and the slowest possible way to learn nothing.
    local limit="--limit-bytes=262144"
    line="$(kubectl logs -n "$NAMESPACE" -l app.kubernetes.io/part-of=memql $limit --prefix=false 2>/dev/null \
        | grep -m1 'build identity' || true)"
    if [[ -z "$line" ]]; then
        line="$(kubectl logs -n "$NAMESPACE" "deploy/identity" $limit 2>/dev/null | grep -m1 'build identity' || true)"
    fi
    if [[ -z "$line" ]]; then
        note "no build-identity boot line found in pod logs -- either the logs have rotated, or these nodes predate memql#4486 and cannot state their own version"
        return 0
    fi
    REPORTED="$(printf '%s' "$line" | python3 -c '
import json, re, sys
raw = sys.stdin.read().strip()
m = re.search(r"\{.*\}", raw)
if not m:
    sys.exit(0)
try:
    doc = json.loads(m.group(0))
except Exception:
    sys.exit(0)
parts = [str(doc.get(k)) for k in ("version", "commit") if doc.get(k)]
print(" ".join(parts))
' 2>/dev/null)" || true
    if [[ -n "$REPORTED" ]]; then
        note "nodes report: ${REPORTED}"
    fi
    return 0
}

# ---------------------------------------------------------------------------
# the verdict
# ---------------------------------------------------------------------------
function collect_result() {
    local agree="true" detail=""

    # Compare only what was actually resolved. An unknown ref is not a
    # disagreement -- calling it one would make --dryRun always report a
    # divergence, and a signal that is always on carries no information.
    if [[ -n "$DECLARED_REF" && -n "$RENDERED_REF" && "$DECLARED_REF" != "$RENDERED_REF" ]]; then
        agree="false"
        detail="${detail:+${detail}; }declared ${DECLARED_REF} != rendered ${RENDERED_REF} -- the overlay composes manifests from a ref the instance does not claim to be on"
    fi
    if [[ -n "$RENDERED_REF" && -n "$RUNNING_IMAGES" ]]; then
        # The bare version, both sides, because git tags carry the `v` and image
        # tags do not (memql#4061). Comparing them unnormalised reports every
        # correct instance as divergent.
        local bare_rendered="${RENDERED_REF#v}"
        if [[ "$RUNNING_IMAGES" != *":${bare_rendered}"* && "$RUNNING_IMAGES" != *":${RENDERED_REF}"* ]]; then
            agree="false"
            detail="${detail:+${detail}; }rendered ${RENDERED_REF} is not the tag any running image carries (${RUNNING_IMAGES}) -- EXPECTED when a tag's image pins predate its own images, and the reason this script reports all three rather than one"
        fi
    fi

    cap_result_set     "declared"       "$DECLARED_REF"
    cap_result_set     "rendered"       "$RENDERED_REF"
    cap_result_set     "runningImages"  "$RUNNING_IMAGES"
    cap_result_set     "runningDigests" "$RUNNING_DIGESTS"
    cap_result_set     "reported"       "$REPORTED"
    cap_result_set_raw "agree"          "$agree"
    cap_result_set     "detail"         "$detail"
    cap_result_set     "namespace"      "$NAMESPACE"
    cap_result_set     "notes"          "$NOTES"
    return 0
}

function main() {
    resolve_declared
    resolve_rendered
    resolve_running
    resolve_reported
    collect_result
    # cap_ok even on disagreement: the report RAN, which is what exit 0 means
    # here. `agree` carries the answer, and divergence is frequently correct.
    cap_ok
}

main "$@"
