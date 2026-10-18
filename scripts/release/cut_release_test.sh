#!/usr/bin/env bash
#
# scripts/release/cut_release_test.sh
#
# A release is created as a DRAFT and published only once release.yml has
# attached every asset the installers download (memql-cockpit#525), so
# `releases/latest` never points at a release whose binaries are still
# building. publish_when_complete is driven here against a fake `gh`: no
# tag, no push, no release is touched.
#
# Run: bash scripts/release/cut_release_test.sh

set -uo pipefail

PASS=0
FAIL=0
function pass() { echo "PASS: $*"; PASS=$((PASS + 1)); }
function fail() { echo "FAIL: $*" >&2; FAIL=$((FAIL + 1)); }

_dir="$(cd "$(dirname "$0")" && pwd)"
_tmp="$(mktemp -d)"
trap 'rm -rf "$_tmp"' EXIT
# The script without its entry point, so its functions can be called alone.
sed '/^main "\$@"$/d' "$_dir/cut-release.sh" > "$_tmp/cut-release-functions.sh"

# run_publish SCENARIO: publish_when_complete under a fake gh that records
# every call. SCENARIO picks what the fake reports.
function run_publish() {
    local scenario="$1"
    : > "$_tmp/gh.log"
    (
        # shellcheck source=/dev/null
        source "$_tmp/cut-release-functions.sh"
        export RELEASE_RUN_WAIT_ATTEMPTS=2 RELEASE_POLL_SECONDS=0
        # shellcheck disable=SC2317  # called by the functions under test
        function gh() {
            printf '%s\n' "$*" >> "$_tmp/gh.log"
            case "$1 $2" in
                "run list")
                    [[ "$scenario" == never-started ]] || echo 4242 ;;
                "run watch")
                    [[ "$scenario" != run-failed ]] ;;
                "release view")
                    expected_assets 0.20.0 | if [[ "$scenario" == missing-asset ]]; then grep -v '^memql-SHA256SUMS$'; else cat; fi ;;
                "release edit") return 0 ;;
            esac
        }
        publish_when_complete owner/repo v0.20.0 0.20.0
    ) > "$_tmp/out.log" 2>&1
}

run_publish complete; rc=$?
if [[ "$rc" == 0 ]] && grep -qx 'release edit v0.20.0 --repo owner/repo --draft=false --latest' "$_tmp/gh.log"; then
    pass "a draft with every installer asset is published"
else
    fail "a complete draft should be published (rc=$rc): $(cat "$_tmp/gh.log" "$_tmp/out.log")"
fi
if grep -n 'run watch 4242 --repo owner/repo --exit-status' "$_tmp/gh.log" | cut -d: -f1 | head -1 | grep -q . \
    && [[ "$(grep -n 'run watch' "$_tmp/gh.log" | cut -d: -f1)" -lt "$(grep -n 'release edit' "$_tmp/gh.log" | cut -d: -f1)" ]]; then
    pass "publishing waits for release.yml to finish"
else
    fail "publish must come after the release.yml run: $(cat "$_tmp/gh.log")"
fi

for scenario in missing-asset run-failed never-started; do
    run_publish "$scenario"; rc=$?
    if [[ "$rc" == 5 ]] && ! grep -q 'release edit' "$_tmp/gh.log" && grep -q 'NOT published' "$_tmp/out.log"; then
        pass "$scenario: the draft stays unpublished, exit 5, with the way to finish"
    else
        fail "$scenario: must not publish (rc=$rc): $(cat "$_tmp/gh.log" "$_tmp/out.log")"
    fi
done
if run_publish missing-asset; grep -q 'memql-SHA256SUMS' "$_tmp/out.log"; then
    pass "a missing asset is named"
else
    fail "the missing asset must be named: $(cat "$_tmp/out.log")"
fi

# The release is created as a draft, never published directly.
# shellcheck disable=SC2016  # the literal source line
if grep -q 'gh release create "$tag" --repo "$repo" --draft' "$_dir/cut-release.sh"; then
    pass "cut-release.sh creates the release as a draft"
else
    fail "cut-release.sh must create the release with --draft"
fi
# And release.yml builds on draft creation, not on publish.
if grep -q 'types: \[created\]' "$_dir/../../.github/workflows/release.yml"; then
    pass "release.yml builds when the draft is created"
else
    fail "release.yml must trigger on release: created"
fi

echo ""
echo "PASS: $PASS"
echo "FAIL: $FAIL"
[[ "$FAIL" == 0 ]]
