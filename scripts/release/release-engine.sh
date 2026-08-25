#!/usr/bin/env bash
#
# scripts/release/release-engine.sh
# =================================
#
# Capability: release.engine -- publish a GitHub RELEASE for an engine version
# and then prove the image build it is supposed to trigger actually started.
#
# Backend for the `releaseEngine` deployment action (memql#4485, epic
# memql#4493).
#
# THE MECHANISM THIS EXISTS FOR. build-engine-images.yml is
# workflow_dispatch-only. Its single automatic trigger is
# dispatch-engine-images-on-release.yml, which fires on `release: [published]`.
# Therefore:
#
#     git tag v0.19.8 && git push --tags     ->  NO IMAGES AT ALL
#     gh release create v0.19.8              ->  the bridge fires, images build
#
# THE EVIDENCE THAT THIS BITES, measured against the repository: thirteen tags
# between v0.16.0 and v0.19.7 carry no GitHub release. So every 0.19.x image in
# ACR and GHCR came from a manual dispatch somebody remembered to run, and a tag
# nobody dispatched for is a version that LOOKS cut and cannot be deployed. That
# is a regression of the exact gap memql#2519 was opened to close -- release
# 0.12.1 was cut, build-engine-images was never run for it, and the release
# silently had no images. The bridge works; the practice of tagging without
# releasing walks around it.
#
# WHY STEP 2 IS THE WHOLE POINT. Creating the Release and reporting success is
# what the existing release path already does, and it is not enough:
#
#     the bridge is an EVENT HANDLER, and an event handler that silently does
#     not fire is indistinguishable from one that has not fired YET.
#
# So this script waits for the dispatched run to APPEAR and fails when it does
# not. A release with no build is the failure being prevented, and the only
# moment it is cheap to notice is now -- afterwards it presents as
# ImagePullBackOff on somebody else's cluster, weeks later.
#
# TWO REGISTRIES, ONE BUILD (memql#4485 §16). build-engine-images pushes each
# node image to BOTH acrmemql.azurecr.io and ghcr.io/znasllc-io. The GHCR half
# is public and tenant-independent, and it is what made a full cloud bring-up
# possible without ever authenticating to the retired subscription. This script
# reports the GHCR refs so an instance lifecycle can pull from there by digest
# and `az acr import` into its own registry, rather than assuming access to
# whichever ACR the build workflow happens to target.
#
# DRAFTS ARE REFUSED, not offered. A draft release does not emit
# `release: [published]`, so it produces exactly the silent no-images state this
# script exists to detect -- while looking, in the GitHub UI, like a release.
#
# Refs: memql#4485 memql#4493 memql#2519 memql#4061 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "release.engine" \
    "Publish a GitHub release for an engine version and verify the image build it triggers actually started."

cap_spec_param_required "version" "the engine version to release, with or without a leading v (e.g. v0.19.8 or 0.19.8)"
cap_spec_param "repo"         "owner/name of the repository (default: znasllc-io/memql)"
cap_spec_param "notes"        "release notes body; when omitted GitHub generates them from the commits since the previous tag"
cap_spec_param "targetSha"    "commit the tag should point at when the tag does not already exist (default: the default branch head)"
cap_spec_param "pollSeconds"  "how long to wait for the dispatched image build to appear (default: 180)"
cap_spec_param "dryRun"       "report what would be released and verify the credential, without creating anything"

cap_handle_meta "$@"
cap_parse_flags "$@"

VERSION_IN="$(cap_param version "")"
REPO="$(cap_param repo "znasllc-io/memql")"
NOTES="$(cap_param notes "")"
TARGET_SHA="$(cap_param targetSha "")"
POLL_SECONDS="$(cap_param pollSeconds "180")"
DRY_RUN="$(cap_param dryRun "false")"

TAG=""
BARE=""
RELEASE_URL=""
RELEASE_CREATED="false"
RUN_ID=""
RUN_URL=""
RELEASE_STATE=""
PUBLISHED_AT=""
RUN_STATUS=""
RUN_CONCLUSION=""
NOTES_OUT=""

function note() {
    NOTES_OUT="${NOTES_OUT:+${NOTES_OUT}; }$1"
    cap_info "$1"
    return 0
}

function check_prerequisites() {
    command -v gh &>/dev/null \
        || cap_fail 4 "gh is not installed or not on PATH. Publishing a release is the ONLY sanctioned way to build engine images; a git tag pushed by hand builds nothing."
    gh auth status &>/dev/null \
        || cap_fail 4 "gh is not authenticated (run: gh auth login). A half-authenticated run would create a tag and fail to publish the release, which is the exact half-done state this capability exists to avoid."
    command -v python3 &>/dev/null \
        || cap_fail 4 "python3 is not installed or not on PATH (used to read gh's JSON)"
    return 0
}

function normalise_version() {
    [[ -n "$VERSION_IN" ]] || cap_fail 2 "--version is required"
    # TWO CONVENTIONS, ONE RELEASE (memql#4061). Git tags carry the `v`; image
    # tags do not. Both spellings are derived here, once, so nothing downstream
    # has to remember which surface it is talking to -- forwarding the wrong one
    # burns an immutable image tag at the wrong name and puts every pod of the
    # release in ImagePullBackOff.
    BARE="${VERSION_IN#v}"
    TAG="v${BARE}"
    [[ "$BARE" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+.][0-9A-Za-z.-]+)?$ ]] \
        || cap_fail 2 "--version ${VERSION_IN} is not a semver version. Expected X.Y.Z (optionally with a pre-release or build suffix), with or without a leading v."
    note "releasing ${TAG} (image tag ${BARE}) in ${REPO}"
    return 0
}

# resolve_existing_release sets RELEASE_STATE (absent|draft|published) and, when
# a release exists, RELEASE_URL.
#
# IT ASSIGNS GLOBALS AND PRINTS NOTHING, deliberately. Written the obvious way
# -- `state="$(existing_release_state)"` -- the function body runs in a command-
# substitution SUBSHELL, so every variable it sets is discarded when that
# subshell exits. The state came back correctly and RELEASE_URL was silently
# empty, which is the shape of bug that survives review: the branch it feeds
# behaves correctly and only the reported value is wrong.
function resolve_existing_release() {
    # Three distinguishable states, and conflating any two of them is a bug:
    #   published  -> nothing to do; poll for the build
    #   draft      -> REFUSE; it emits no `release: [published]` event
    #   absent     -> create it
    RELEASE_STATE="absent"
    local json
    if ! json="$(gh release view "$TAG" --repo "$REPO" --json isDraft,url,publishedAt 2>/dev/null)"; then
        return 0
    fi
    local is_draft
    is_draft="$(printf '%s' "$json" | python3 -c 'import json,sys; print(str(json.load(sys.stdin).get("isDraft", False)).lower())' 2>/dev/null || echo "false")"
    RELEASE_URL="$(printf '%s' "$json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("url",""))' 2>/dev/null || echo "")"
    PUBLISHED_AT="$(printf '%s' "$json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("publishedAt","") or "")' 2>/dev/null || echo "")"
    if [[ "$is_draft" == "true" ]]; then
        RELEASE_STATE="draft"
        return 0
    fi
    RELEASE_STATE="published"
    return 0
}

function publish_release() {
    resolve_existing_release

    case "$RELEASE_STATE" in
        published)
            note "a published release already exists for ${TAG}: ${RELEASE_URL} -- not recreating it"
            return 0
            ;;
        draft)
            cap_fail 3 "a DRAFT release exists for ${TAG}. A draft emits no 'release: [published]' event, so it builds no images while looking like a release in the UI. Publish it in the GitHub UI (or delete it and re-run), then re-run this capability."
            ;;
    esac

    if [[ "$DRY_RUN" == "true" ]]; then
        note "--dryRun: would publish a release for ${TAG}; nothing created"
        return 0
    fi

    local args=(release create "$TAG" --repo "$REPO" --title "$TAG")
    if [[ -n "$NOTES" ]]; then
        args+=(--notes "$NOTES")
    else
        args+=(--generate-notes)
    fi
    if [[ -n "$TARGET_SHA" ]]; then
        args+=(--target "$TARGET_SHA")
    fi

    cap_step "publishing release ${TAG}"
    if ! RELEASE_URL="$(gh "${args[@]}" 2>&1 | tail -1)"; then
        cap_fail 5 "gh release create failed for ${TAG}: ${RELEASE_URL}"
    fi
    RELEASE_CREATED="true"
    cap_changed
    # Re-read, for publishedAt: the poll below uses it as the lower bound on
    # which runs count as evidence, and GitHub's timestamp is the one both
    # sides agree on. A locally-computed `date -u` would be this machine's
    # clock, compared against GitHub's.
    resolve_existing_release
    note "published ${TAG}: ${RELEASE_URL}"
    return 0
}

# await_dispatched_build waits for a build-engine-images run that POSTDATES this
# release, and reports what it finds.
#
# WHAT THIS CAN AND CANNOT PROVE, stated because the repository already reasoned
# about it: release-cutting.md §9 records "the Actions API does not expose a
# run's dispatch inputs, so matching a run to a version is a guess". That is
# correct and still true -- `gh run list` gives displayTitle
# "build-engine-images" for every run, with no version anywhere.
#
# So this does NOT claim the run it finds is building this exact version. It
# claims something weaker and checkable: A BUILD WAS DISPATCHED AFTER THIS
# RELEASE WAS PUBLISHED. The release bridge fires within seconds of the publish
# event, so on the failure being prevented -- the bridge not firing at all --
# that window stays empty and this fails. An older run cannot satisfy it,
# which is the part a "newest workflow_dispatch run" check gets wrong: with no
# lower bound, a manual dispatch from yesterday reads as today's success.
#
# The registry remains the authority on whether a version is actually
# deployable (§6's Check images, and §9's reasoning). This is the dispatch
# half, which is the half that fails silently.
#
# THE RUN'S CONCLUSION IS REPORTED, NOT ASSERTED. A build can be dispatched and
# then fail -- the most recent 0.19.x run on this repository did exactly that --
# and that is a different problem from the one here, with a different fix. It is
# surfaced rather than swallowed, and rather than being turned into this
# capability's failure.
function await_dispatched_build() {
    if [[ "$DRY_RUN" == "true" ]]; then
        note "--dryRun: not waiting for a build that was never triggered"
        return 0
    fi
    if [[ -z "$PUBLISHED_AT" ]]; then
        cap_fail 5 "the release exists but GitHub reported no publishedAt for ${TAG}, so there is no lower bound to judge a build run against. Without one, any older manual dispatch would read as this release's build."
    fi

    local deadline=$(( SECONDS + POLL_SECONDS ))
    cap_step "waiting up to ${POLL_SECONDS}s for a build-engine-images run dispatched after ${PUBLISHED_AT}"

    while :; do
        local json hit
        json="$(gh run list --repo "$REPO" --workflow=build-engine-images.yml \
                    --limit 30 --json databaseId,url,event,status,conclusion,createdAt 2>/dev/null || echo '[]')"
        hit="$(printf '%s' "$json" | PUBLISHED_AT="$PUBLISHED_AT" python3 -c '
import json, os, sys
since = os.environ.get("PUBLISHED_AT", "")
try:
    runs = json.load(sys.stdin)
except Exception:
    runs = []
# The bridge dispatches on the default branch, so the run arrives as a
# workflow_dispatch. createdAt is the lower bound: an ISO-8601 Z timestamp
# compares correctly as a string, both sides coming from GitHub.
for r in runs:
    if r.get("event") != "workflow_dispatch":
        continue
    created = str(r.get("createdAt", ""))
    if not created or not since or created < since:
        continue
    print("\t".join([str(r.get("databaseId","")), str(r.get("url","")),
                     str(r.get("status","")), str(r.get("conclusion") or "")]))
    break
' 2>/dev/null || true)"
        if [[ -n "$hit" ]]; then
            RUN_ID="$(printf '%s' "$hit" | cut -f1)"
            RUN_URL="$(printf '%s' "$hit" | cut -f2)"
            RUN_STATUS="$(printf '%s' "$hit" | cut -f3)"
            RUN_CONCLUSION="$(printf '%s' "$hit" | cut -f4)"
            note "build-engine-images dispatched after the release: run ${RUN_ID} (${RUN_URL}), status=${RUN_STATUS}${RUN_CONCLUSION:+ conclusion=${RUN_CONCLUSION}}"
            if [[ "$RUN_CONCLUSION" == "failure" || "$RUN_CONCLUSION" == "cancelled" ]]; then
                cap_warn "that run finished ${RUN_CONCLUSION}. The bridge fired, so THIS capability's check passed -- but the images are not published. Read the run, fix it, and re-dispatch build-engine-images with version=${BARE}."
            fi
            return 0
        fi
        (( SECONDS < deadline )) || break
        sleep 10
    done

    cap_fail 5 "no build-engine-images run dispatched after ${PUBLISHED_AT} appeared within ${POLL_SECONDS}s of publishing ${TAG}. The release exists and its images do not. THIS IS THE FAILURE THIS CAPABILITY EXISTS TO CATCH: dispatch-engine-images-on-release.yml is an event handler, and one that silently does not fire looks exactly like one that has not fired yet -- until the version is deployed and every pod lands in ImagePullBackOff. Check the workflow's run history and Actions permissions, then dispatch build-engine-images manually with version=${BARE}."
}

function collect_result() {
    cap_result_set     "tag"            "$TAG"
    cap_result_set     "imageTag"       "$BARE"
    cap_result_set     "repository"     "$REPO"
    cap_result_set     "releaseUrl"     "$RELEASE_URL"
    cap_result_set_raw "releaseCreated" "$RELEASE_CREATED"
    cap_result_set     "buildRunId"     "$RUN_ID"
    cap_result_set     "buildRunUrl"    "$RUN_URL"
    cap_result_set     "buildRunStatus" "$RUN_STATUS"
    # Reported, never asserted: a dispatched build that FAILED is a different
    # problem from a bridge that did not fire, and swallowing it here would hide
    # the one this capability cannot fix behind the one it can.
    cap_result_set     "buildRunConclusion" "$RUN_CONCLUSION"
    cap_result_set     "releasePublishedAt" "$PUBLISHED_AT"
    # The tenant-independent half of the build's output. An instance lifecycle
    # pulls from here by digest and imports into its own registry, rather than
    # assuming access to whichever ACR the build workflow targets.
    cap_result_set     "ghcrPrefix"     "ghcr.io/znasllc-io/memql-"
    cap_result_set     "notes"          "$NOTES_OUT"
    return 0
}

function main() {
    check_prerequisites
    normalise_version
    publish_release
    await_dispatched_build
    collect_result
    cap_ok
}

main "$@"
