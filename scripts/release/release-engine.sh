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
# A STALE VERSION IS REFUSED TOO (memql#5714). The repo-root VERSION file equals
# the tag of the commit a release tags (VERSIONING.md), so before creating a
# release this reads VERSION at exactly the commit the tag points at -- the
# existing tag, or the commit it will be created on -- and refuses when the file
# names anything else. The same rule as the console cut
# (integrations/release/versionfile.go, refusal version_file_stale). When the
# tag does not exist yet and no targetSha is given, the default branch's head is
# read ONCE and passed as --target, so the commit whose VERSION was checked is
# the commit that gets tagged rather than whatever the branch points at a moment
# later. An already-PUBLISHED release is left alone: it is history, and this run
# only waits for its build. A VERSION refusal also reports
# result.reason=version_file_stale, the console cut's code for the same rule, so
# a caller can tell it from the draft refusal without reading the message.
#
# BARE X.Y.Z ONLY. VERSION is never suffixed (VERSIONING.md), the console cut
# and the docs bundle accept nothing else, and a pre-release accepted here could
# only be released by breaking that rule -- so a suffixed version is a bad
# parameter rather than a release this script half-supports.
#
# Exit codes: 0 ok | 2 bad param | 3 refused (a draft release, or a stale or
# missing VERSION) | 4 gh or python3 missing or unauthenticated | 5 GitHub call
# failed, or no build appeared
#
# Refs: memql#4485 memql#4493 memql#2519 memql#4061 memql#2221 memql#5714

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "release.engine" \
    "Publish a GitHub release for an engine version and verify the image build it triggers actually started."

cap_spec_param_required "version" "the engine version to release, X.Y.Z with no suffix, with or without a leading v (e.g. v0.19.8 or 0.19.8)"
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
DRY_RUN="$(cap_bool_str dryRun false)"

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
VERSION_REF=""
VERSION_FILE=""

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
    [[ "$BARE" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        || cap_fail 2 "--version ${VERSION_IN} is not a release version. Expected X.Y.Z, with or without a leading v; a pre-release or build suffix is refused because VERSION, which must equal the tag, is never suffixed (VERSIONING.md)."
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

# resolve_version_ref sets VERSION_REF to the commit the release's tag points
# at, or will point at once created.
#
#   the tag exists       -> the tag itself; gh release create uses it as is
#   targetSha was given  -> that sha, which is already passed as --target
#   neither              -> the default branch's head, read once here and then
#                           assigned to TARGET_SHA so it is ALSO passed as
#                           --target. Without that, gh release create would tag
#                           whatever the branch points at when it runs, which
#                           need not be the commit VERSION was read at.
#
# Tag existence is asked through matching-refs, which answers an empty list for
# an absent tag -- so "absent" and "could not ask" are different outcomes here
# rather than one failed call read two ways. matching-refs is a PREFIX match
# (v1.2.3 also matches v1.2.30), hence the exact select.
function resolve_version_ref() {
    local existing branch
    if ! existing="$(gh api "repos/${REPO}/git/matching-refs/tags/${TAG}" \
            --jq ".[] | select(.ref == \"refs/tags/${TAG}\") | .ref")"; then
        cap_fail 5 "could not ask GitHub whether the tag ${TAG} exists in ${REPO} (gh's error is above), so there is no commit to check VERSION at"
    fi
    if [[ -n "$existing" ]]; then
        VERSION_REF="$TAG"
        return 0
    fi
    if [[ -z "$TARGET_SHA" ]]; then
        if ! branch="$(gh api "repos/${REPO}" --jq '.default_branch')" || [[ -z "$branch" ]]; then
            cap_fail 5 "could not read the default branch of ${REPO} (gh's error is above), so there is no commit to check VERSION at"
        fi
        if ! TARGET_SHA="$(gh api "repos/${REPO}/commits/${branch}" --jq '.sha')" || [[ -z "$TARGET_SHA" ]]; then
            cap_fail 5 "could not read the head of ${branch} in ${REPO} (gh's error is above), so there is no commit to check VERSION at"
        fi
        note "the tag ${TAG} will be created at ${branch}'s head ${TARGET_SHA}"
    fi
    VERSION_REF="$TARGET_SHA"
    return 0
}

# version_remedy prints what resolves a VERSION refusal at VERSION_REF.
#
# THE REMEDY DEPENDS ON WHETHER THE TAG EXISTS. For a tag this run would create,
# a prepare pull request moves the default branch's head to a commit whose
# VERSION is right, and a re-run tags that. For a tag that ALREADY exists --
# this capability's main case, a tag pushed by hand with no Release -- VERSION
# is read at the tag, and no pull request can change the commit a tag points
# at: telling the operator to land one would send every re-run back to the same
# refusal.
function version_remedy() {
    if [[ "$VERSION_REF" == "$TAG" ]]; then
        printf '%s' "The tag ${TAG} already exists, and no pull request can change the commit a tag points at. Delete the tag, then re-run at a commit whose VERSION reads ${BARE}: the default branch's head once a prepare pull request setting VERSION to ${BARE} has merged, or the commit you pass as targetSha. A tag cut before VERSION was required to equal the tag cannot pass this check where it stands."
    else
        printf '%s' "Land a pull request setting VERSION to ${BARE}, then re-run this capability."
    fi
    return 0
}

# verify_version_file refuses (exit 3) unless VERSION at VERSION_REF reads BARE.
#
# The root listing is read first so a MISSING file is told apart from a failed
# call without parsing gh's error text: a listing that succeeds and does not
# name VERSION means the file is absent at that commit. The file itself is then
# fetched RAW, which needs no base64 decoding (GNU `base64 -d` and BSD
# `base64 -D` disagree) and no JSON parsing. The listing is captured and
# searched with a here-string, never piped into `grep -q`: under pipefail an
# early grep exit can SIGPIPE the writer and fail a check that matched.
function verify_version_file() {
    resolve_version_ref
    cap_result_set "versionRef" "$VERSION_REF"

    local listing raw
    if ! listing="$(gh api "repos/${REPO}/contents?ref=${VERSION_REF}" --jq '.[].name')"; then
        cap_fail 5 "could not list the root of ${REPO} at ${VERSION_REF} (gh's error is above), so VERSION could not be checked"
    fi
    if ! grep -qx 'VERSION' <<<"$listing"; then
        cap_result_set "reason" "version_file_stale"
        cap_fail 3 "there is no VERSION file at ${VERSION_REF} in ${REPO}. VERSION must equal the tag a release is cut at (VERSIONING.md), and at ${TAG} it must read ${BARE}. $(version_remedy)"
    fi
    if ! raw="$(gh api -H 'Accept: application/vnd.github.raw+json' "repos/${REPO}/contents/VERSION?ref=${VERSION_REF}")"; then
        cap_fail 5 "could not read VERSION at ${VERSION_REF} in ${REPO} (gh's error is above)"
    fi
    # First line, all whitespace removed -- the same reading the Makefile and
    # scripts/docs/build-docs-bundle.sh take.
    VERSION_FILE="$(printf '%s\n' "$raw" | sed -n '1p' | tr -d '[:space:]')"
    cap_result_set "versionFile" "$VERSION_FILE"
    if [[ "$VERSION_FILE" != "$BARE" ]]; then
        cap_result_set "reason" "version_file_stale"
        cap_fail 3 "VERSION at ${VERSION_REF} reads '${VERSION_FILE}', and this would release ${TAG}. VERSION must equal the tag a release is cut at, so every reader of the file -- the docs bundle among them -- names the release it belongs to (VERSIONING.md). $(version_remedy)"
    fi
    note "VERSION at ${VERSION_REF} reads ${VERSION_FILE}, matching ${TAG}"
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

    # Before the dry-run return: a dry run that passed here and a real run that
    # then refused would make the dry run a promise it cannot keep.
    verify_version_file

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
