#!/usr/bin/env bash
#
# scripts/install/lib_test.sh
# ============================
#
# Smoke tests for lib.sh's pure-logic helpers: install_mode_dir,
# require_sudo's failure mode, binary_name_for (headless + computeruse
# flavours), and write_worker_yaml (fresh write, 0600 mode, capability
# list, and its --force clobber guard). The /usr/local/bin system path
# can't be exercised in CI without elevated privileges, so the
# system-mode path is checked via the require_sudo failure mode.
# Also exercises the installers' own lib.sh sourcing: the cloned-repo
# sibling path, and the curl|bash fallback that fetches lib.sh from
# MEMQL_INSTALL_RAW_BASE when no sibling exists.
# And the release-asset preflight: preflight_asset's verdicts over
# file:// fixtures (present passes, missing exits 4 naming the URL and
# the flavour/platform pair), plus both installers refusing BEFORE any
# mutation when the asset is missing and proceeding past the preflight
# to the download when it is present.
# And the --inference pass-through: setup_inference's reading of the
# setup command's exit code (0 quiet, 3 prints the interactive command
# on ONE physical line, anything else reported), that it never fails the
# install, and that both installers accept the flag and call it after
# worker.yaml and the service.
# And the uninstallers: --help and the unknown-flag exit, lib.sh
# sourcing (they join the installers' loop), and real runs against a
# throwaway HOME with launchctl / systemctl / sudo cut out of PATH --
# what --user-local removes and keeps (worker.yaml + workers.yaml),
# what --purge adds, the
# nothing-installed run, ~/.memql kept for the CLI's clusters.yaml, and
# the fence that keeps a state_dir outside ~/.memql from being deleted.
# And the no-flag contract MemQL OS relies on (both platforms, a stub
# memql standing in for the installed one): one enrollment is removed
# as --cluster=<its url>, none proceeds as --all-homes, several refuse
# with the command for each; --cluster=URL with no enrollment proceeds,
# with no MATCHING enrollment refuses naming the enrolled ones, and
# matches through trailing slashes and case; a binary too old for the
# unpair contract (or none at all) falls back to full removal only when
# the requested cluster is the only one; the install shape is detected
# without --user-local; --dry-run prints the plan and leaves HOME
# byte-identical; and the exact piped one-liner shapes all parse.
# And the installers' --version pin: the composed download base, the
# space and = spellings, --download-base winning, and the bad-value exit.
# And the second review pass: a registry of flow-mapping items refuses
# (the pre-fix scripts deleted every token over it); the installer's
# three registry readers accept the same block spellings as the
# uninstaller's; a sibling-remaining unpair whose worker will not
# reload is PARTIAL, never REFUSED; --dry-run makes the same pre-flight
# refusals the real run makes and its remedies keep --dry-run; a failing
# rm under --purge is a recorded leftover with the summary still
# printed; and the five low findings (HOME with a trailing slash, the
# lib.sh fetch failure's exit code, a user-local app rm that fails, a
# `#` inside a state_dir, a state_dir that names a regular file).
# And the third pass: every registry the binary decodes as two homes but
# the text readers saw no block `homes:` header in (a flow/JSON root, a
# quoted/spaced/tabbed/explicit/anchored/tagged/escaped key, an indented
# root, a BOM, UTF-16, CR-only / NEL / LS / PS line breaks) is refused
# (5) by all four readers -- the uninstaller's no-flag, --cluster and
# --dry-run runs on both drivers (and piped) leave HOME byte-identical,
# and the installer refuses before its download with workers.yaml
# unchanged -- while a file with provably no homes key still reads as
# zero; and a legacy service file that will not unlink after a
# sibling-remaining unpair ends in the PARTIAL summary, exit 5.
#
# Run: bash scripts/install/lib_test.sh
# Wired into native CI by memql-package.yaml.

set -uo pipefail

# Source lib.sh; suppress its `set -uo pipefail` from killing this
# driver if a helper returns non-zero (we assert that here).
# shellcheck disable=SC1091
source "$(dirname "$0")/lib.sh"

PASS=0
FAIL=0

function fail() {
    echo "FAIL: $*" >&2
    FAIL=$((FAIL + 1))
}

function pass() {
    echo "PASS: $*"
    PASS=$((PASS + 1))
}

function expect_eq() {
    local name="$1"
    local got="$2"
    local want="$3"
    if [[ "$got" == "$want" ]]; then
        pass "$name"
    else
        fail "$name: got=$got want=$want"
    fi
}

# ---------------------------------------------------------------
# install_mode_dir
# ---------------------------------------------------------------

expect_eq "install_mode_dir system" "$(install_mode_dir system)" "/usr/local/bin"
expect_eq "install_mode_dir user-local" "$(install_mode_dir user-local)" "${HOME}/.memql/bin"

# Unknown mode prints to stderr + non-zero.
if install_mode_dir bogus >/dev/null 2>&1; then
    fail "install_mode_dir bogus accepted; should have errored"
else
    pass "install_mode_dir bogus rejected"
fi

# ---------------------------------------------------------------
# require_sudo behaviour
# ---------------------------------------------------------------

# Running as root would short-circuit; in CI / dev we're not root.
if [[ $EUID -eq 0 ]]; then
    echo "INFO: running as root -- skipping require_sudo failure-mode tests"
else
    # Force sudo to be unreachable by clearing PATH inside a subshell
    # (so `command -v sudo` and sudo itself both fail). Running the
    # subshell as the `if` condition checks its status directly.
    if (
        # shellcheck disable=SC2123  # clearing PATH is the point of this probe
        PATH=""
        require_sudo
    ) >/dev/null 2>&1; then
        fail "require_sudo passed with PATH cleared (expected fail)"
    else
        pass "require_sudo fails when sudo unreachable"
    fi
fi

# ---------------------------------------------------------------
# binary_name_for
# ---------------------------------------------------------------

_os="$(detect_os)"
_arch="$(detect_arch)"

expect_eq "binary_name_for headless" \
    "$(binary_name_for headless)" "${INSTALLED_COMMAND}-${_os}-${_arch}"
expect_eq "binary_name_for computeruse" \
    "$(binary_name_for computeruse)" "${INSTALLED_COMMAND}-computeruse-${_os}-${_arch}"

# Unknown flavour prints to stderr + non-zero.
if binary_name_for bogus >/dev/null 2>&1; then
    fail "binary_name_for bogus accepted; should have errored"
else
    pass "binary_name_for bogus rejected"
fi

# ---------------------------------------------------------------
# write_worker_yaml
# ---------------------------------------------------------------

_tmp="$(mktemp -d)"
# Diagnostics stay outside fixture HOME so filesystem assertions still cover
# every worker/config/service path, including refusals and dry runs.
export MEMQL_INSTALL_LOG_DIR="${_tmp}/diagnostics"
trap 'rm -rf "$_tmp"' EXIT
_wy="${_tmp}/worker.yaml"

# file_mode prints an octal permission string portably (GNU + BSD stat).
function file_mode() {
    stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null
}

# Fresh write with default capabilities (HEADLESS only).
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "no" >/dev/null 2>&1; then
    pass "write_worker_yaml fresh write succeeds"
else
    fail "write_worker_yaml fresh write should succeed"
fi

expect_eq "write_worker_yaml renders mode 0600" "$(file_mode "$_wy")" "600"

if grep -qx '  - HEADLESS' "$_wy"; then
    pass "write_worker_yaml default lists HEADLESS capability"
else
    fail "write_worker_yaml default should list HEADLESS capability"
fi

# COMPUTERUSE appears in the concurrency block, so match the capability
# LIST line exactly (whole line) rather than a substring.
if grep -qx '  - COMPUTERUSE' "$_wy"; then
    fail "write_worker_yaml default must not list COMPUTERUSE capability"
else
    pass "write_worker_yaml default omits COMPUTERUSE capability"
fi

# Same-URL refresh without --force is allowed (token rotation).
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "no" >/dev/null 2>&1; then
    pass "write_worker_yaml refreshes the same home without --force"
else
    fail "write_worker_yaml should refresh the same cluster_url without --force"
fi

# Additive: a second cluster keeps the first home in workers.yaml.
_wy_workers="$(dirname "$_wy")/workers.yaml"
if write_worker_yaml "$_wy" "https://d.example" "mql_wkr_def" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'id: c.example' "$_wy_workers" && grep -q 'id: d.example' "$_wy_workers"; then
        pass "write_worker_yaml additive upsert keeps sibling homes"
    else
        fail "write_worker_yaml should keep both c.example and d.example in workers.yaml"
        echo "---- workers.yaml ----" >&2
        cat "$_wy_workers" >&2
    fi
else
    fail "write_worker_yaml should accept a second cluster without --force"
fi

# --force replaces THAT home only (computer-use caps) and keeps siblings.
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "yes" "HEADLESS,COMPUTERUSE" >/dev/null 2>&1; then
    pass "write_worker_yaml --force replaces matched home"
else
    fail "write_worker_yaml should accept --force for the matched home"
fi

if grep -qx '  - HEADLESS' "$_wy" && grep -qx '  - COMPUTERUSE' "$_wy"; then
    pass "write_worker_yaml computeruse lists HEADLESS + COMPUTERUSE"
else
    fail "write_worker_yaml computeruse should list HEADLESS + COMPUTERUSE"
fi

if grep -q 'id: d.example' "$_wy_workers"; then
    pass "write_worker_yaml --force preserves sibling homes"
else
    fail "write_worker_yaml --force must not drop sibling homes"
fi

# Same cluster_url under a different enrolled id (pair --home-id local
# vs install host-id) refreshes WITHOUT --force and keeps the enrolled id.
_url_mismatch="$(mktemp -d)/worker.yaml"
_url_workers="$(dirname "$_url_mismatch")/workers.yaml"
mkdir -p "$(dirname "$_url_mismatch")"
cat > "$_url_workers" << 'WY'
version: 1
worker_name: host1
labels:
  os: darwin
  arch: arm64
concurrency:
  HEADLESS: 8
  COMPUTERUSE: 1
state_dir: /tmp/x
log_level: info
capabilities:
  - HEADLESS
homes:
  - id: local
    cluster_url: https://api.memql.localhost
    token: mql_wkr_local_bbbbbbbbbbbb
    enabled: true
WY
if write_worker_yaml "$_url_mismatch" "https://api.memql.localhost" "mql_wkr_local_cccccccccccc" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'id: local' "$_url_workers" && grep -q 'mql_wkr_local_cccccccccccc' "$_url_workers"; then
        if grep -q 'id: api.memql.localhost' "$_url_workers"; then
            fail "write_worker_yaml must not invent a second home for the same cluster_url"
        else
            pass "write_worker_yaml same cluster_url different id refreshes without --force"
        fi
    else
        fail "write_worker_yaml should keep id local and refresh token"
        cat "$_url_workers" >&2
    fi
else
    fail "write_worker_yaml should refresh same cluster_url without --force when ids differ"
fi

# The same cluster spelled another way (an explicit :443, a trailing
# slash, capitals) is the SAME home: refreshed in place, never a second
# entry (memql-cockpit#433 -- two enabled homes on one cluster open one
# stream between them, and the token just issued must be the one that
# connects).
_hostmatch="$(mktemp -d)/worker.yaml"
_hostmatch_workers="$(dirname "$_hostmatch")/workers.yaml"
cat > "$_hostmatch_workers" << 'WY'
version: 1
worker_name: host1
state_dir: /tmp/x
log_level: info
capabilities:
  - HEADLESS
homes:
  - id: local
    cluster_url: https://api.example.com
    token: mql_wkr_old_bbbbbbbbbbbbbb
    enabled: true
  - id: other
    cluster_url: https://api.other.example
    token: mql_wkr_oth_bbbbbbbbbbbbbb
    enabled: true
WY
expect_eq "find_home_id_by_cluster_url matches the same host on another port" \
    "$(find_home_id_by_cluster_url "$_hostmatch_workers" "https://API.example.com:443/")" "local"
expect_eq "find_home_id_by_cluster_url prefers the exact URL" \
    "$(find_home_id_by_cluster_url "$_hostmatch_workers" "https://api.other.example")" "other"
expect_eq "find_home_id_by_cluster_url finds nothing for another cluster" \
    "$(find_home_id_by_cluster_url "$_hostmatch_workers" "https://api.third.example")" ""
if write_worker_yaml "$_hostmatch" "https://api.example.com:443" "mql_wkr_new_cccccccccccccc" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    if [[ "$(grep -c 'cluster_url: https://api.example.com' "$_hostmatch_workers")" == "1" ]] &&
        grep -q 'id: local' "$_hostmatch_workers" && grep -q 'mql_wkr_new_cccccccccccccc' "$_hostmatch_workers" &&
        ! grep -q 'mql_wkr_old_bbbbbbbbbbbbbb' "$_hostmatch_workers"; then
        pass "write_worker_yaml refreshes the same host on another port in place"
    else
        fail "write_worker_yaml appended a second home for the same host"
        cat "$_hostmatch_workers" >&2
    fi
else
    fail "write_worker_yaml should refresh the same host without --force"
fi

# Same id + different cluster_url still requires --force.
_remap="$(mktemp -d)/worker.yaml"
_remap_workers="$(dirname "$_remap")/workers.yaml"
mkdir -p "$(dirname "$_remap")"
cat > "$_remap_workers" << 'WY'
version: 1
worker_name: host1
capabilities:
  - HEADLESS
homes:
  - id: local
    cluster_url: https://api.memql.localhost
    token: mql_wkr_local_bbbbbbbbbbbb
    enabled: true
WY
if write_worker_yaml "$_remap" "https://api.other.example" "mql_wkr_other_dddddddddddd" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    # home_id from other URL is api.other.example — additive new home, OK.
    # Remap conflict is same *id* local onto a new URL.
    pass "write_worker_yaml additive different URL under new id does not need --force"
else
    fail "write_worker_yaml should append a new home for a new cluster_url"
fi
# Force the id-conflict path: write with a URL whose host_id is "local"
# is hard; instead pre-seed id matching host and try different URL via
# manually calling with cluster that home_id_from maps... Use explicit
# seed where id equals derived host of NEW url? Better: seed id c.example
# for url https://old.example and upsert https://c.example (home_id=c.example).
_idconflict="$(mktemp -d)/worker.yaml"
_idc_workers="$(dirname "$_idconflict")/workers.yaml"
mkdir -p "$(dirname "$_idconflict")"
cat > "$_idc_workers" << 'WY'
version: 1
worker_name: host1
capabilities:
  - HEADLESS
homes:
  - id: c.example
    cluster_url: https://old.example
    token: mql_wkr_old_eeeeeeeeeeeeee
    enabled: true
WY
if write_worker_yaml "$_idconflict" "https://c.example" "mql_wkr_new_ffffffffffffff" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    fail "write_worker_yaml must require --force to remap home id onto a different cluster_url"
else
    pass "write_worker_yaml requires --force for home id cluster remap"
fi
if write_worker_yaml "$_idconflict" "https://c.example" "mql_wkr_new_ffffffffffffff" "host1" "yes" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'cluster_url: https://c.example' "$_idc_workers" && grep -q 'id: c.example' "$_idc_workers"; then
        pass "write_worker_yaml --force remaps home id onto new cluster_url"
    else
        fail "write_worker_yaml --force should remap cluster_url"
        cat "$_idc_workers" >&2
    fi
else
    fail "write_worker_yaml --force should allow home id cluster remap"
fi

# ---------------------------------------------------------------
# Version helpers (compare / parse / resolve)
# ---------------------------------------------------------------

expect_eq "normalize_semver strips v" "$(normalize_semver 'v0.12.1')" "0.12.1"
expect_eq "parse_memql_version_line" "$(parse_memql_version_line 'memql 0.12.1 (headless)')" "0.12.1"
expect_eq "compare_semver equal" "$(compare_semver '0.12.1' '0.12.1')" "0"
expect_eq "compare_semver older" "$(compare_semver '0.11.0' '0.12.1')" "-1"
expect_eq "compare_semver newer" "$(compare_semver '0.13.0' '0.12.1')" "1"

# Fake installed binary via a shim that answers --version.
_verdir="$(mktemp -d)"
printf '%s\n' '#!/bin/sh' 'echo "memql 0.12.1 (headless)"' > "$_verdir/memql-same"
chmod +x "$_verdir/memql-same"
expect_eq "read_binary_version" "$(read_binary_version "$_verdir/memql-same")" "0.12.1"

expect_eq "stable designated requirement preserves grants" "$(macos_signing_transition 'anchor apple generic and identifier MemQL and TEAM' 'anchor apple generic and identifier MemQL and TEAM')" "unchanged"
expect_eq "different ad-hoc requirements require recovery guidance" "$(macos_signing_transition 'cdhash old' 'cdhash new')" "changed"
expect_eq "missing signing evidence never means stable" "$(macos_signing_transition '' 'cdhash new')" "unknown"

# resolve_target_version from download-base tag path. Run from a copy of
# lib.sh with no VERSION file beside it: in a repository checkout the
# sibling VERSION wins over the URL (as it should for a cloned-repo
# install), so asserting the URL path there only ever passed while
# VERSION happened to say 0.12.1.
_rtv="$(mktemp -d)"
mkdir -p "$_rtv/a/b"
cp "$(dirname "$0")/lib.sh" "$_rtv/a/b/lib.sh"
expect_eq "resolve_target_version from download-base" \
    "$(cd "$_rtv" && bash -c 'source ./a/b/lib.sh >/dev/null 2>&1; resolve_target_version "https://github.com/znasllc-io/memql/releases/download/v0.12.1"')" \
    "0.12.1"

# Second upsert must preserve Go-tuned shared header knobs (concurrency,
# labels, worker_name, state_dir, log_level), not rebuild them from
# install defaults.
_hdr="${_tmp}/header-preserve"
mkdir -p "$_hdr"
_hdr_legacy="${_hdr}/worker.yaml"
_hdr_workers="${_hdr}/workers.yaml"
cat > "$_hdr_workers" << 'HDR'
version: 1
worker_name: tuned-name
labels:
  os: darwin
  arch: arm64
  tier: gold
concurrency:
  HEADLESS: 3
  COMPUTERUSE: 2
state_dir: /custom/state
log_level: debug
capabilities:
  - HEADLESS
homes:
  - id: c.example
    cluster_url: https://c.example
    token: mql_wkr_oldtokennnnnnn
    enabled: true
HDR
if write_worker_yaml "$_hdr_legacy" "https://e.example" "mql_wkr_newtokennnnnnn" "should-not-replace-name" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'worker_name: tuned-name' "$_hdr_workers" \
        && grep -q 'tier: gold' "$_hdr_workers" \
        && grep -q 'HEADLESS: 3' "$_hdr_workers" \
        && grep -q 'COMPUTERUSE: 2' "$_hdr_workers" \
        && grep -q 'state_dir: /custom/state' "$_hdr_workers" \
        && grep -q 'log_level: debug' "$_hdr_workers" \
        && grep -q 'id: c.example' "$_hdr_workers" \
        && grep -q 'id: e.example' "$_hdr_workers"; then
        pass "write_worker_yaml upsert preserves tuned registry header fields"
    else
        fail "write_worker_yaml upsert clobbered tuned header or dropped homes"
        echo "---- workers.yaml ----" >&2
        cat "$_hdr_workers" >&2
    fi
else
    fail "write_worker_yaml should upsert beside a tuned registry"
fi

# ---------------------------------------------------------------
# install.sh restates lib.sh's names -- they must agree
# ---------------------------------------------------------------
#
# The root install.sh is fetched ALONE by `curl | sh` and has no lib.sh to
# source, so it carries its own copy of the migration names. A rename
# applied to one file and not the other produces an installer that retires
# a service nobody registered, or registers one nothing will find -- both
# silent. Reading the literals out of the script is what makes the
# duplication safe.

_install_sh="$(dirname "$0")/../../install.sh"

function install_sh_value() {
    local key="$1"
    grep -E "^${key}=" "$_install_sh" | head -1 | sed -E "s/^${key}=\"?([^\"]*)\"?.*/\\1/"
}

if [[ -f "$_install_sh" ]]; then
    expect_eq "install.sh installs the same command name" \
        "$(install_sh_value BINARY)" "$INSTALLED_COMMAND"
    expect_eq "install.sh SERVICE_LABEL_DARWIN" \
        "$(install_sh_value SERVICE_LABEL_DARWIN)" "com.znasllc.memql-worker"
    expect_eq "install.sh SERVICE_LABEL_LINUX" \
        "$(install_sh_value SERVICE_LABEL_LINUX)" "memql-worker"
    expect_eq "install.sh LEGACY_LABEL_DARWIN" \
        "$(install_sh_value LEGACY_LABEL_DARWIN)" "com.znasllc.memql-cockpit-worker"
    expect_eq "install.sh LEGACY_LABEL_LINUX" \
        "$(install_sh_value LEGACY_LABEL_LINUX)" "memql-cockpit-worker"
    expect_eq "install.sh LEGACY_BINARIES" \
        "$(install_sh_value LEGACY_BINARIES)" "$LEGACY_BINARIES"

    # lib.sh OWNS the four labels now (the uninstallers read them from
    # it), so its constants must agree with install.sh's copy too: the
    # literal expectations above pin the names, these pin the two files
    # to each other.
    expect_eq "lib.sh SERVICE_LABEL_DARWIN agrees with install.sh" \
        "$SERVICE_LABEL_DARWIN" "$(install_sh_value SERVICE_LABEL_DARWIN)"
    expect_eq "lib.sh SERVICE_LABEL_LINUX agrees with install.sh" \
        "$SERVICE_LABEL_LINUX" "$(install_sh_value SERVICE_LABEL_LINUX)"
    expect_eq "lib.sh LEGACY_LABEL_DARWIN agrees with install.sh" \
        "$LEGACY_LABEL_DARWIN" "$(install_sh_value LEGACY_LABEL_DARWIN)"
    expect_eq "lib.sh LEGACY_LABEL_LINUX agrees with install.sh" \
        "$LEGACY_LABEL_LINUX" "$(install_sh_value LEGACY_LABEL_LINUX)"

    # And the installers, which still spell the file names inline, must
    # write the service lib.sh names and retire the legacy one it
    # names -- or the uninstaller removes a file the installer never
    # wrote.
    if grep -qF "${SERVICE_LABEL_DARWIN}.plist" "$(dirname "$0")/install-mac.sh" \
        && grep -qF "${LEGACY_LABEL_DARWIN}.plist" "$(dirname "$0")/install-mac.sh"; then
        pass "install-mac.sh writes and retires the plists lib.sh names"
    else
        fail "install-mac.sh should name ${SERVICE_LABEL_DARWIN}.plist and ${LEGACY_LABEL_DARWIN}.plist"
    fi
    if grep -qF "${SERVICE_LABEL_LINUX}.service" "$(dirname "$0")/install-linux.sh" \
        && grep -qF "${LEGACY_LABEL_LINUX}.service" "$(dirname "$0")/install-linux.sh"; then
        pass "install-linux.sh writes and retires the units lib.sh names"
    else
        fail "install-linux.sh should name ${SERVICE_LABEL_LINUX}.service and ${LEGACY_LABEL_LINUX}.service"
    fi

    # The new label must not BE the old one -- a migration that renames
    # nothing would satisfy every other assertion here.
    if [[ "$(install_sh_value SERVICE_LABEL_DARWIN)" == "$(install_sh_value LEGACY_LABEL_DARWIN)" ]]; then
        fail "install.sh's darwin service label equals the legacy one; it migrates nothing"
    else
        pass "install.sh darwin service label differs from the legacy label"
    fi
else
    fail "install.sh not found at $_install_sh"
fi

# ---------------------------------------------------------------
# Installer sourcing -- cloned repo vs curl|bash (piped stdin)
# ---------------------------------------------------------------
#
# The installers source lib.sh from $(dirname "$0"), which under
# `curl ... | bash` is the operator's cwd ($0 is `bash`), so they fall
# back to fetching lib.sh from MEMQL_INSTALL_RAW_BASE. That override is
# what keeps these tests offline: the real lib.sh in this directory IS
# the fixture, reached over file://, and the failure case points at a
# path that cannot resolve. `--help` is the probe because it exits 0
# before any download / sudo / service work. The uninstallers carry
# the same source_lib and travel the same way, so they are in the loop.

_script_dir="$(cd "$(dirname "$0")" && pwd)"
_piped_cwd="${_tmp}/piped-cwd"
mkdir -p "$_piped_cwd"

for _installer in install-mac.sh install-linux.sh uninstall-mac.sh uninstall-linux.sh; do
    # Piped from a cwd holding no lib.sh, with RAW_BASE at the real
    # lib.sh: must survive sourcing and reach flag handling.
    if _out="$(cd "$_piped_cwd" && MEMQL_INSTALL_RAW_BASE="file://${_script_dir}" \
        bash -s -- --help < "${_script_dir}/${_installer}" 2>&1)" \
        && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer piped --help sources lib.sh from RAW_BASE"
    else
        fail "$_installer piped --help should print usage and exit 0; got: $_out"
    fi

    # The cloned-repo path must NEVER fetch: with RAW_BASE poisoned,
    # running next to lib.sh still works because the sibling wins.
    if _out="$(cd "$_script_dir" && MEMQL_INSTALL_RAW_BASE="file:///nonexistent-raw-base" \
        "./${_installer}" --help 2>&1)" && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer cloned-repo --help never consults RAW_BASE"
    else
        fail "$_installer cloned-repo --help should use the sibling lib.sh; got: $_out"
    fi

    # Piped with a broken RAW_BASE: must die with an ERROR naming the
    # lib.sh URL it tried, not a cryptic bash sourcing error.
    _out="$(cd "$_piped_cwd" && MEMQL_INSTALL_RAW_BASE="file://${_tmp}/no-such-dir" \
        bash -s -- --help < "${_script_dir}/${_installer}" 2>&1)"
    _rc=$?
    # 4: a prerequisite (the helper library) is missing, in the table
    # the scripts document; it was a bare 1 (a review finding).
    expect_eq "$_installer piped fetch failure exits 4 (prerequisite missing)" "$_rc" "4"
    if [[ "$_out" == *"ERROR"* && "$_out" == *"file://${_tmp}/no-such-dir/lib.sh"* ]]; then
        pass "$_installer piped fetch failure names the lib.sh URL"
    else
        fail "$_installer piped fetch failure should name the URL it tried; got: $_out"
    fi
done

# ---------------------------------------------------------------
# preflight_asset -- refuse a missing release asset before mutating
# ---------------------------------------------------------------
#
# The helper alone first: verdicts over file:// (present -> 0, missing
# -> 4), which is what keeps these checks offline. file:// is also why
# the helper carries its ranged-GET fallback -- HEAD support for the
# FILE protocol varies by curl build -- so passing here proves the
# fallback chain, not just `curl -I`.

_pf_assets="${_tmp}/preflight-assets"
mkdir -p "$_pf_assets"
_pf_headless="$(binary_name_for headless)"
_pf_computeruse="$(binary_name_for computeruse)"
printf 'stub-binary' > "${_pf_assets}/${_pf_headless}"

if preflight_asset "file://${_pf_assets}/${_pf_headless}" headless >/dev/null 2>&1; then
    pass "preflight_asset passes a present asset"
else
    fail "preflight_asset should pass a present asset"
fi

_out="$(preflight_asset "file://${_pf_assets}/${_pf_computeruse}" computeruse 2>&1)"
_rc=$?
expect_eq "preflight_asset missing asset returns 4 (prerequisite missing)" "$_rc" "4"

if [[ "$_out" == *"file://${_pf_assets}/${_pf_computeruse}"* ]]; then
    pass "preflight_asset refusal names the exact asset URL"
else
    fail "preflight_asset refusal should name the asset URL; got: $_out"
fi

if [[ "$_out" == *"flavour: computeruse"* && "$_out" == *"${_os}/${_arch}"* ]]; then
    pass "preflight_asset refusal names flavour + platform"
else
    fail "preflight_asset refusal should name flavour + os/arch; got: $_out"
fi

# ---------------------------------------------------------------
# Installer preflight -- refusal mutates nothing; presence passes
# ---------------------------------------------------------------
#
# Now the call site: both installers must refuse AT the preflight --
# exit 4, nothing created under HOME, no download attempted -- and a
# present asset must sail through it. --user-local + --no-service keep
# the runs sudo-free; a fresh HOME per run is what makes "nothing was
# created" assertable. The success run still fails LATER, by design:
# download_binary pins --proto '=https' and so refuses the file://
# fixture. That is fine -- the assertion here is progress PAST the
# preflight (the download attempt on the same URL), not a completed
# install.

for _installer in install-mac.sh install-linux.sh; do
    # Missing asset (--computeruse against a base that publishes
    # nothing): a clean refusal before any mutation.
    _pf_home="${_tmp}/pf-404-home-${_installer}"
    mkdir -p "$_pf_home"
    _out="$(cd "$_script_dir" && HOME="$_pf_home" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --computeruse \
        --user-local --no-service --verbose \
        --download-base "file://${_pf_assets}-none" 2>&1)"
    _rc=$?
    expect_eq "$_installer missing asset exits 4" "$_rc" "4"

    if [[ "$_out" == *"file://${_pf_assets}-none/${_pf_computeruse}"* \
        && "$_out" == *"flavour: computeruse"* && "$_out" == *"${_os}/${_arch}"* ]]; then
        pass "$_installer refusal names the asset URL + flavour/platform"
    else
        fail "$_installer refusal should name URL + flavour/platform; got: $_out"
    fi

    if [[ ! -e "${_pf_home}/.memql" && ! -e "${_pf_home}/Library/LaunchAgents" \
        && ! -e "${_pf_home}/.config" && "$_out" != *"INFO: downloading"* ]]; then
        pass "$_installer refusal mutates nothing (no ~/.memql, no service dir, no download)"
    else
        fail "$_installer refusal left state behind or attempted a download"
    fi

    # Present asset (headless at the fixture base): the preflight
    # passes and the install proceeds to the download of the SAME URL.
    _pf_home_ok="${_tmp}/pf-ok-home-${_installer}"
    mkdir -p "$_pf_home_ok"
    _out="$(cd "$_script_dir" && HOME="$_pf_home_ok" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example \
        --user-local --no-service --verbose \
        --download-base "file://${_pf_assets}" 2>&1)"
    _rc=$?
    if [[ "$_rc" != "4" && "$_out" != *"release asset not found"* ]]; then
        pass "$_installer present asset passes preflight"
    else
        fail "$_installer present asset should pass preflight; rc=$_rc got: $_out"
    fi

    if [[ "$_out" == *"INFO: downloading file://${_pf_assets}/${_pf_headless}"* ]]; then
        pass "$_installer proceeds past preflight to the download"
    else
        fail "$_installer should reach the download after preflight; got: $_out"
    fi
done

# ---------------------------------------------------------------
# Linux registration capabilities follow the runtime display preflight.
# Load only the driver's config function, preserving its unconditional main.
# Capture its writer arguments, then render real YAML to a task-specific file.
# No HOME override, network, service call or user configuration write is needed.
_linux_write_config="$(sed -n '/^function write_config()/,/^}/p' "${_script_dir}/install-linux.sh")"

function check_linux_display_config() {
    local name="$1" wayland="$2" session_type="$3" display="$4" expected="$5" flavour="${6:-computeruse}"
    local output capabilities actual
    output="$(
        eval "$_linux_write_config"
        # Called by the evaluated driver function, beyond ShellCheck tracing.
        # shellcheck disable=SC2317
        # shellcheck disable=SC2329 # installer invokes this stub indirectly
        function write_worker_yaml() { printf 'CAPABILITIES=%s\n' "$6"; }
        # The evaluated write_config reads these installer variables.
        # shellcheck disable=SC2034
        FLAVOUR="$flavour" CLUSTER_URL=https://c.example TOKEN=mql_wkr_fixture NAME=fixture FORCE=no
        WAYLAND_DISPLAY="$wayland" XDG_SESSION_TYPE="$session_type" DISPLAY="$display" write_config
    )"
    capabilities="$(printf '%s\n' "$output" | sed -n 's/^CAPABILITIES=//p')"
    if [[ -z "$capabilities" ]]; then
        fail "Linux ${name} did not call the config writer"
        return
    fi
    if ! write_worker_yaml "${_tmp}/display-${name}.yaml" https://c.example mql_wkr_fixture fixture no "$capabilities" >/dev/null; then
        fail "Linux ${name} config rendering failed"
        return
    fi
    actual="$(sed -n '/^capabilities:/,$p' "${_tmp}/display-${name}.yaml" | tail -n +2 | sed 's/^  - //')"
    expect_eq "Linux ${name} rendered capabilities" "$actual" "$expected"
}

check_linux_display_config wayland-with-xwayland wayland-0 wayland :0 HEADLESS
check_linux_display_config wayland-env-wins wayland-0 x11 :0 HEADLESS
check_linux_display_config padded-session "" $' \tWaYlAnD\r\n' :0 HEADLESS
check_linux_display_config native-x11 "" x11 :0 $'HEADLESS\nCOMPUTERUSE'
check_linux_display_config display-only "" "" :0 $'HEADLESS\nCOMPUTERUSE'
check_linux_display_config no-display "" "" "" HEADLESS
check_linux_display_config session-without-display "" x11 "" HEADLESS
check_linux_display_config headless-build "" x11 :0 HEADLESS headless

# ---------------------------------------------------------------
# setup_inference -- the --inference pass-through
# ---------------------------------------------------------------
#
# A machine that paired fine and could not set up local models is still
# a working worker, so setup_inference REPORTS every failure and returns
# 0. Exit 3 is the one with its own answer -- "refused: required
# confirmation not provided", which here means a runtime install a
# scripted run may not approve -- and the answer is the interactive
# command, printed for the person who is standing at this terminal now.
# A fake binary stands in for memql: what is under test is the shell's
# reading of `$?`, not the Go program's.

_si_bin="${_tmp}/fake-memql"
_si_log="${_tmp}/fake-memql.argv"
cat > "$_si_bin" << STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${_si_log}"
exit "\${FAKE_MEMQL_EXIT:-0}"
STUB
chmod +x "$_si_bin"

_out="$(FAKE_MEMQL_EXIT=0 setup_inference "$_si_bin" 2>&1)"
_rc=$?
expect_eq "setup_inference success returns 0" "$_rc" "0"
expect_eq "setup_inference runs the non-interactive setup" \
    "$(tail -1 "$_si_log")" "worker setup --inference --non-interactive"

# Exit 3: print the interactive command, and do not fail the install.
_out="$(FAKE_MEMQL_EXIT=3 setup_inference "$_si_bin" 2>&1)"
_rc=$?
expect_eq "setup_inference exit 3 still returns 0 (the install succeeded)" "$_rc" "0"

# The command MUST BE ONE PHYSICAL LINE: it is meant to be copied out of
# the terminal, and a bracketed paste of a wrapped command has already
# cost this project once. grep -qx matches the WHOLE line.
if printf '%s\n' "$_out" | grep -qx "  ${_si_bin} worker setup --inference"; then
    pass "setup_inference exit 3 prints the interactive command on one line"
else
    fail "setup_inference exit 3 should print the interactive command; got: $_out"
fi

if [[ "$_out" == *"worker setup --inference --non-interactive"* ]]; then
    fail "setup_inference exit 3 must not tell the operator to re-run the flag that refused"
else
    pass "setup_inference exit 3 drops --non-interactive from the printed command"
fi

# Any other non-zero: reported, install not failed. A below-floor
# machine (4) or a failed pull (5) is not a reason to undo a working
# pairing.
for _si_code in 4 5 1; do
    _out="$(FAKE_MEMQL_EXIT="$_si_code" setup_inference "$_si_bin" 2>&1)"
    _rc=$?
    expect_eq "setup_inference exit ${_si_code} still returns 0" "$_rc" "0"
    if [[ "$_out" == *"exited ${_si_code}"* && "$_out" == *"paired and working"* ]]; then
        pass "setup_inference exit ${_si_code} reports without failing the install"
    else
        fail "setup_inference exit ${_si_code} should report the code and say the machine still works; got: $_out"
    fi
done

# ---------------------------------------------------------------
# setup_inference -- each exit code is its own outcome (memql-cockpit#524)
# ---------------------------------------------------------------
#
# The setup writes one line to MEMQL_SETUP_REASON_FILE when it does not
# finish, and the installer's summary prints it. 4 is permanent (this
# machine cannot run local models) and is never sent back to the same
# command; 5 and 2 are failures with a retry; 3 is the approval path.

_sr_bin="${_tmp}/fake-memql-reason"
cat > "$_sr_bin" << 'STUB'
#!/usr/bin/env bash
if [[ -n "${MEMQL_SETUP_REASON_FILE:-}" && -n "${FAKE_REASON:-}" ]]; then
    printf '%s\n' "$FAKE_REASON" > "$MEMQL_SETUP_REASON_FILE"
fi
exit "${FAKE_MEMQL_EXIT:-0}"
STUB
chmod +x "$_sr_bin"

FAKE_MEMQL_EXIT=4 FAKE_REASON="This machine has 8 GB of unified memory; the floor is 16 GB." \
    setup_inference "$_sr_bin" >/dev/null 2>&1
expect_eq "setup_inference exit 4 is unavailable, with the setup's reason" \
    "${INSTALL_MODELS_STATE}|${INSTALL_MODELS_REASON}" \
    "unavailable|This machine has 8 GB of unified memory; the floor is 16 GB."
FAKE_MEMQL_EXIT=5 FAKE_REASON="Pulling qwen3.5:9b failed: connection reset" \
    setup_inference "$_sr_bin" >/dev/null 2>&1
expect_eq "setup_inference exit 5 is failed, with the setup's reason" \
    "${INSTALL_MODELS_STATE}|${INSTALL_MODELS_REASON}" "failed|Pulling qwen3.5:9b failed: connection reset"
FAKE_MEMQL_EXIT=2 FAKE_REASON="" setup_inference "$_sr_bin" >/dev/null 2>&1
if [[ "$INSTALL_MODELS_STATE" == failed && "$INSTALL_MODELS_REASON" == *"did not accept its arguments (exit 2)"* ]]; then
    pass "setup_inference exit 2 is a usage failure even when the release records no reason"
else
    fail "setup_inference exit 2 should be a usage failure; got ${INSTALL_MODELS_STATE}|${INSTALL_MODELS_REASON}"
fi
FAKE_MEMQL_EXIT=3 FAKE_REASON="" setup_inference "$_sr_bin" >/dev/null 2>&1
expect_eq "setup_inference exit 3 is pending approval" "$INSTALL_MODELS_STATE" "pending"

# The summary: the reason once, no retry for a machine that cannot run
# models, the retry command for one that failed.
function _si_summary() {
    local out_file="${_tmp}/summary-$1.txt"
    (
        exec 3> "$out_file"
        INSTALL_UI_ACCENT=""; INSTALL_UI_RESET=""; INSTALL_UI_DIM=""
        INSTALL_SERVICE_STATE=started; INSTALL_LOG="/fixture/install.log"
        INSTALLED_BINARY="$_sr_bin"
        INSTALL_MODELS_STATE="$1"; INSTALL_MODELS_REASON="$2"
        INSTALL_WORKBENCH_STATE="$3"
        # shellcheck disable=SC2030  # the summary reads it in this subshell only
        INSTALL_WORKBENCH_REASON="$4"
        INSTALL_MENU_STATE="${5:-off}"; INSTALL_MENU_REASON="${6:-}"
        install_ui_finish
    )
    cat "$out_file"
}
_out="$(_si_summary unavailable "This machine has 8 GB of unified memory; the floor is 16 GB." off "")"
if [[ "$_out" == *"This machine can't run local models: This machine has 8 GB of unified memory; the floor is 16 GB."* \
    && "$_out" != *"worker setup --inference"* ]]; then
    pass "summary: a machine that cannot run local models is told once, with no retry"
else
    fail "summary for unavailable models is wrong: $_out"
fi
_out="$(_si_summary failed "Pulling qwen3.5:9b failed: connection reset" off "")"
if [[ "$_out" == *"Local models could not be set up: Pulling qwen3.5:9b failed: connection reset"* \
    && "$_out" == *"worker setup --inference"* ]]; then
    pass "summary: a failed model setup names the reason and the retry"
else
    fail "summary for failed models is wrong: $_out"
fi
_out="$(_si_summary ready "" failed "Docker needs at least 3 CPUs and 4 GB for two workbenches with headroom; it has 2 CPUs and 3.2 GB")"
if [[ "$_out" == *"Fleet workbenches need attention: Docker needs at least 3 CPUs"* \
    && "$_out" == *"worker setup --workbench"* && "$_out" != *"can't run local models"* ]]; then
    pass "summary: a failed workbench setup names the reason and the retry"
else
    fail "summary for failed workbenches is wrong: $_out"
fi
_out="$(_si_summary ready "" unsupported "")"
if [[ "$_out" == *"Fleet workbenches are not in Cockpit v"* && "$_out" != *"--workbench"* ]]; then
    pass "summary: workbenches missing from the installed release are named, with no command it cannot run"
else
    fail "summary for unsupported workbenches is wrong: $_out"
fi
_out="$(_si_summary ready "" off "" failed "it could not be downloaded from https://example.invalid")"
if [[ "$_out" == *"The menu companion was not installed: it could not be downloaded"* ]]; then
    pass "summary: a menu companion that did not install is reported, not fatal"
else
    fail "summary for the menu companion is wrong: $_out"
fi

# A person at a terminal approves in that terminal: no --non-interactive,
# stdin is the terminal, and the setup's words reach the screen and the log.
_tty_in="${_tmp}/fake-tty"
printf 'y\n' > "$_tty_in"
_tty_bin="${_tmp}/fake-memql-tty"
_tty_log="${_tmp}/fake-memql-tty.argv"
cat > "$_tty_bin" << STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${_tty_log}"
read -r answer
printf 'Run them now? [y/N] answered %s\n' "\$answer"
exit 0
STUB
chmod +x "$_tty_bin"
_screen="${_tmp}/fake-screen"
(
    exec 3> "$_screen"
    INSTALL_UI_SPINNER=""; INSTALL_UI_CURSOR_HIDDEN=no
    MEMQL_INSTALL_TTY="$_tty_in" setup_inference "$_tty_bin" > "${_tmp}/fake-tty-log" 2>&1
)
expect_eq "setup_inference at a terminal runs the interactive setup" "$(tail -1 "$_tty_log")" "worker setup --inference"
if grep -qF "answered y" "$_screen" && grep -qF "answered y" "${_tmp}/fake-tty-log"; then
    pass "setup_inference at a terminal reads the answer there and shows it on screen and in the log"
else
    fail "setup_inference at a terminal should read the answer from the terminal; screen: $(cat "$_screen")"
fi

# ---------------------------------------------------------------
# setup_workbench -- optional, detected, never fatal (memql-cockpit#524)
# ---------------------------------------------------------------

_wb_old="${_tmp}/fake-memql-0.19"
cat > "$_wb_old" << STUB
#!/usr/bin/env bash
if [[ "\$*" == "worker setup --help" ]]; then
    echo "Usage of worker setup:" >&2
    echo "  -inference" >&2
    exit 0
fi
if [[ "\$*" == "--version" ]]; then echo "memql 0.19.0 (computeruse)"; exit 0; fi
echo "flag provided but not defined: -workbench" >&2
exit 2
STUB
chmod +x "$_wb_old"
INSTALL_WORKBENCH_STATE=off
_out="$(setup_workbench "$_wb_old" 2>&1)"; _rc=$?
setup_workbench "$_wb_old" >/dev/null 2>&1
expect_eq "setup_workbench on a release without --workbench returns 0" "$_rc" "0"
expect_eq "setup_workbench on a release without --workbench is unsupported" "$INSTALL_WORKBENCH_STATE" "unsupported"
if [[ "$_out" != *"flag provided but not defined"* ]]; then
    pass "setup_workbench never runs a flag the installed release lacks"
else
    fail "setup_workbench ran --workbench on a release without it: $_out"
fi

_wb_new="${_tmp}/fake-memql-0.20"
cat > "$_wb_new" << 'STUB'
#!/usr/bin/env bash
if [[ "$*" == "worker setup --help" ]]; then
    echo "  -workbench  enable isolated Fleet workbenches" >&2
    exit 0
fi
if [[ "$*" == "worker setup --workbench" ]]; then
    [[ -z "${MEMQL_SETUP_REASON_FILE:-}" ]] || echo "Start Docker with Linux containers, then retry: Docker is not installed" > "$MEMQL_SETUP_REASON_FILE"
    exit "${FAKE_WORKBENCH_EXIT:-0}"
fi
exit 0
STUB
chmod +x "$_wb_new"
FAKE_WORKBENCH_EXIT=1 setup_workbench "$_wb_new" >/dev/null 2>&1; _rc=$?
expect_eq "setup_workbench failure returns 0" "$_rc" "0"
# shellcheck disable=SC2031  # set by setup_workbench in this shell, not the summary's subshell
expect_eq "setup_workbench failure records the setup's reason" \
    "${INSTALL_WORKBENCH_STATE}|${INSTALL_WORKBENCH_REASON}" \
    "failed|Start Docker with Linux containers, then retry: Docker is not installed"
FAKE_WORKBENCH_EXIT=0 setup_workbench "$_wb_new" >/dev/null 2>&1
expect_eq "setup_workbench success is ready" "$INSTALL_WORKBENCH_STATE" "ready"

# ---------------------------------------------------------------
# setup_permissions -- the person's turn, first, never fatal (memql-cockpit#548)
# ---------------------------------------------------------------

_pm_old="${_tmp}/fake-memql-perm-old"
cat > "$_pm_old" << 'STUB'
#!/usr/bin/env bash
if [[ "$*" == "worker setup --help" ]]; then echo "  -workbench" >&2; exit 0; fi
echo "flag provided but not defined: -permissions" >&2
exit 2
STUB
chmod +x "$_pm_old"
_out="$(setup_permissions "$_pm_old" 2>&1)"; _rc=$?
setup_permissions "$_pm_old" >/dev/null 2>&1
expect_eq "setup_permissions on a release without --permissions returns 0" "$_rc" "0"
expect_eq "setup_permissions on a release without --permissions is not set up" "$INSTALL_PERMISSIONS_STATE" "failed"
if [[ "$_out" != *"flag provided but not defined"* ]]; then
    pass "setup_permissions never runs a flag the installed release lacks"
else
    fail "setup_permissions ran --permissions on a release without it: $_out"
fi

_pm_argv="${_tmp}/fake-memql-perm.argv"
_pm_new="${_tmp}/fake-memql-perm"
cat > "$_pm_new" << STUB
#!/usr/bin/env bash
if [[ "\$*" == "worker setup --help" ]]; then echo "  -permissions  open the Permissions window" >&2; exit 0; fi
printf '%s\n' "\$*" >> "${_pm_argv}"
[[ -z "\${MEMQL_SETUP_REASON_FILE:-}" || -z "\${FAKE_PERMISSIONS_REASON:-}" ]] || echo "\$FAKE_PERMISSIONS_REASON" > "\$MEMQL_SETUP_REASON_FILE"
exit "\${FAKE_PERMISSIONS_EXIT:-0}"
STUB
chmod +x "$_pm_new"
: > "$_pm_argv"
FAKE_PERMISSIONS_EXIT=0 setup_permissions "$_pm_new" >/dev/null 2>&1
expect_eq "setup_permissions with both allowed is ready" "$INSTALL_PERMISSIONS_STATE" "ready"
expect_eq "setup_permissions with nobody at a terminal asks nobody" "$(tail -1 "$_pm_argv")" "worker setup --permissions --non-interactive"
FAKE_PERMISSIONS_EXIT=4 FAKE_PERMISSIONS_REASON="Continued without Screen Recording." setup_permissions "$_pm_new" >/dev/null 2>&1; _rc=$?
expect_eq "setup_permissions skipped returns 0" "$_rc" "0"
expect_eq "setup_permissions skipped keeps what was left off" \
    "${INSTALL_PERMISSIONS_STATE}|${INSTALL_PERMISSIONS_REASON}|${INSTALL_UI_RESULT}" \
    "skipped|Continued without Screen Recording.|Permissions: Continued without Screen Recording."
FAKE_PERMISSIONS_EXIT=3 FAKE_PERMISSIONS_REASON="Not asked: this install ran without a terminal." setup_permissions "$_pm_new" >/dev/null 2>&1
expect_eq "setup_permissions nobody could answer is not set up" "${INSTALL_PERMISSIONS_STATE}|${INSTALL_PERMISSIONS_REASON}" \
    "failed|Not asked: this install ran without a terminal."
INSTALL_MENU=no FAKE_PERMISSIONS_EXIT=0 setup_permissions "$_pm_new" >/dev/null 2>&1
expect_eq "setup_permissions without the menu companion sends the person to System Settings" "$(tail -1 "$_pm_argv")" \
    "worker setup --permissions --no-window --non-interactive"
(
    exec 3> "${_tmp}/fake-perm-screen"
    INSTALL_UI_SPINNER=""; INSTALL_UI_CURSOR_HIDDEN=no
    MEMQL_INSTALL_TTY="$_tty_in" FAKE_PERMISSIONS_EXIT=0 setup_permissions "$_pm_new" > /dev/null 2>&1
)
expect_eq "setup_permissions at a terminal waits there for the person" "$(tail -1 "$_pm_argv")" "worker setup --permissions"
_out="$(
    exec 3>&1
    INSTALL_UI_ACCENT=""; INSTALL_UI_RESET=""; INSTALL_UI_DIM=""
    INSTALL_SERVICE_STATE=started; INSTALL_LOG="/fixture/install.log"; INSTALLED_BINARY="$_sr_bin"
    INSTALL_MODELS_STATE=off; INSTALL_WORKBENCH_STATE=off; INSTALL_MENU_STATE=off
    INSTALL_PERMISSIONS_STATE=skipped; INSTALL_PERMISSIONS_REASON="Continued without Screen Recording."
    install_ui_finish
)"
if [[ "$_out" == *"Computer use needs Accessibility and Screen Recording: Continued without Screen Recording."*"Set Up Permissions"* ]]; then
    pass "summary: a permission left off says where to turn it on"
else
    fail "summary for skipped permissions is wrong: $_out"
fi

# ---------------------------------------------------------------
# The setup report -- the plan and the end, best-effort (memql-cockpit#541)
# ---------------------------------------------------------------

expect_eq "setup_report_plan names every stage, in the installer's order" "$(setup_report_plan yes yes)" "runtime,models,workbench"
expect_eq "setup_report_plan without workbenches" "$(setup_report_plan yes no)" "runtime,models"
expect_eq "setup_report_plan with workbenches alone" "$(setup_report_plan no yes)" "workbench"
expect_eq "setup_report_plan with neither is empty" "$(setup_report_plan no no)" ""
expect_eq "setup_report_plan puts the person's turn first (memql-cockpit#548)" "$(setup_report_plan yes yes yes)" "permissions,runtime,models,workbench"
expect_eq "setup_report_plan with permissions alone" "$(setup_report_plan no no yes)" "permissions"

_report_log="${_tmp}/setup-report.log"
_report_bin="${_tmp}/fake-memql-report"
cat > "$_report_bin" << STUB
#!/usr/bin/env bash
echo "\$*" >> "$_report_log"
exit "\${FAKE_REPORT_EXIT:-0}"
STUB
chmod +x "$_report_bin"
: > "$_report_log"
report_setup_plan "$_report_bin" yes yes; _rc=$?
report_setup_finished "$_report_bin" yes yes
expect_eq "report_setup_plan and report_setup_finished send the plan, then the end" "$(cat "$_report_log")" \
    "$(printf '%s\n%s' "worker control --action=setup --plan=runtime,models,workbench --wait=20s" "worker control --action=setup --finished --wait=5s")"
expect_eq "report_setup_plan returns 0" "$_rc" "0"
: > "$_report_log"
report_setup_plan "$_report_bin" no no
report_setup_finished "$_report_bin" no no
expect_eq "an install with no stage forgets an earlier install's report, and reports no end" "$(cat "$_report_log")" \
    "worker control --action=setup --reset --wait=20s"
: > "$_report_log"
report_setup_plan "$_report_bin" yes no yes
report_setup_finished "$_report_bin" yes no yes
expect_eq "a computer-use Mac's plan starts with the permissions stage" "$(head -1 "$_report_log")" \
    "worker control --action=setup --plan=permissions,runtime,models --wait=20s"
FAKE_REPORT_EXIT=2 report_setup_plan "$_report_bin" yes no; _rc=$?
expect_eq "a worker that refuses the report never fails the install" "$_rc" "0"

# The installing shell's Ollama variables reach the worker service, so it
# serves from where the setup pulled (memql-cockpit#524).
_out="$(OLLAMA_HOST='127.0.0.1:11435' OLLAMA_MODELS='/Volumes/Big & Fast/<models>' ollama_plist_env)"
if [[ "$_out" == *"<key>OLLAMA_HOST</key>"*"<string>127.0.0.1:11435</string>"* \
    && "$_out" == *"<string>/Volumes/Big &amp; Fast/&lt;models&gt;</string>"* ]]; then
    pass "ollama_plist_env carries both variables, escaped for the plist"
else
    fail "ollama_plist_env is wrong: $_out"
fi
_out="$(unset OLLAMA_HOST OLLAMA_MODELS; ollama_plist_env)"
expect_eq "ollama_plist_env adds nothing when neither is set" "$_out" ""
_envf="${_tmp}/worker.env"
printf 'MEMQL_WORKER_TOKEN=keep\nOLLAMA_HOST=old\n' > "$_envf"
upsert_env_file_var "$_envf" OLLAMA_HOST '127.0.0.1:"11435"'
expect_eq "upsert_env_file_var replaces the key and keeps the rest" "$(cat "$_envf")" \
    $'MEMQL_WORKER_TOKEN=keep\nOLLAMA_HOST="127.0.0.1:\\"11435\\""'
expect_eq "upsert_env_file_var keeps the file owner-only" "$(file_mode "$_envf")" "600"

# The whole tail of an install under the installer's own shell options: a
# release without --workbench, models first, and the summary still printed.
_flow_home="${_tmp}/flow-home"
mkdir -p "$_flow_home"
_flow_out="$(HOME="$_flow_home" bash -c '
    set -euo pipefail
    source "$1/lib.sh"
    INSTALL_PLAIN=yes
    install_ui_init install
    INSTALLED_BINARY="$2"; INSTALL_SERVICE_STATE=started
    install_ui_stage "Preparing local models" setup_inference "$3"
    install_ui_stage "Preparing Fleet workbenches" setup_workbench "$2"
    install_ui_finish
' _ "$_script_dir" "$_wb_old" "$_si_bin" 2>&1)"
_flow_rc=$?
expect_eq "an install whose release lacks --workbench still finishes" "$_flow_rc" "0"
if [[ "$_flow_out" == *"Local models ready"* && "$_flow_out" == *"Fleet workbenches need a newer Cockpit"* \
    && "$_flow_out" == *"Fleet workbenches are not in Cockpit v0.19.0"* && "$_flow_out" == *"Install log:"* ]]; then
    pass "models are set up, the workbench stage is reported, and the summary is printed"
else
    fail "the install tail did not reach its summary: $_flow_out"
fi

# ---------------------------------------------------------------
# Both installers accept --inference and wire it after the service
# ---------------------------------------------------------------
#
# The flag itself is checked by running the real installer: --help exits
# 0 before any download, so `--inference --help` proves the flag reached
# a case arm rather than the unknown-flag branch. The CALL SITE is
# checked by reading main(), because the end-to-end path needs a
# release asset over https and these tests stay offline.

for _installer in install-mac.sh install-linux.sh; do
    if _out="$(cd "$_script_dir" && "./${_installer}" --inference --help 2>&1)" \
        && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer accepts --inference"
    else
        fail "$_installer should accept --inference; got: $_out"
    fi

    if [[ "$_out" == *"--inference"* ]]; then
        pass "$_installer documents --inference in its help"
    else
        fail "$_installer help should document --inference"
    fi

    # The order matters: worker.yaml first (nothing to configure
    # without it), the service next (so there is a running worker for
    # the setup's SIGHUP to reach), setup_inference last.
    _line_cfg="$(grep -nF 'install_ui_stage "Configuring this machine" write_config' "${_script_dir}/${_installer}" | head -1 | cut -d: -f1)"
    _line_inf="$(grep -nF 'install_ui_stage "Preparing local models" setup_inference' "${_script_dir}/${_installer}" | head -1 | cut -d: -f1)"
    if [[ -n "$_line_cfg" && -n "$_line_inf" && "$_line_inf" -gt "$_line_cfg" ]]; then
        pass "$_installer runs setup_inference after worker.yaml is written"
    else
        fail "$_installer should call setup_inference after write_config (cfg=$_line_cfg inf=$_line_inf)"
    fi

    # Local models before workbenches, and workbenches through the guarded
    # function, never a bare call that set -e can turn fatal
    # (memql-cockpit#524).
    _line_wb="$(grep -nF 'install_ui_stage "Preparing Fleet workbenches" setup_workbench' "${_script_dir}/${_installer}" | head -1 | cut -d: -f1)"
    if [[ -n "$_line_wb" && -n "$_line_inf" && "$_line_wb" -gt "$_line_inf" ]]; then
        pass "$_installer prepares workbenches after local models, through setup_workbench"
    else
        fail "$_installer should run setup_workbench after setup_inference (inf=$_line_inf wb=$_line_wb)"
    fi
    # shellcheck disable=SC2016  # the literal text of a bare call, not an expansion
    if grep -qE 'install_ui_stage "Preparing Fleet workbenches" "\$INSTALLED_BINARY"' "${_script_dir}/${_installer}"; then
        fail "$_installer still runs worker setup --workbench bare"
    else
        pass "$_installer never runs worker setup --workbench bare"
    fi

    # And only when asked: a plain install must not touch the runtime.
    if grep -qF 'INFERENCE" == "yes"' "${_script_dir}/${_installer}"; then
        pass "$_installer only sets up inference when --inference was given"
    else
        fail "$_installer should guard setup_inference behind --inference"
    fi
done

# Native activation may be an idempotent no-op for the same app. Enrollment
# still changed: the already-running worker must consume the new workers.yaml.
# Source the real installer without main, and fake only OS/archive boundaries.
_native_fixture="${_tmp}/native-enrollment"
mkdir -p "$_native_fixture/unpacked/scripts/macos"
cat > "$_native_fixture/unpacked/scripts/macos/activate-app.sh" <<'SH'
#!/usr/bin/env bash
function main() {
    printf '%s\n' activated >> "$MEMQL_TEST_ACTIVATION_LOG"
    return "${MEMQL_TEST_ACTIVATION_EXIT:-0}"
}
main "$@"
SH
sed '/^main "\$@"$/d' "$_script_dir/install-mac.sh" > "$_native_fixture/installer-functions.sh"
for _native_mode in restart no-service activation-failed restart-failed; do
    _native_log="$_native_fixture/$_native_mode.log"
    _out="$(MEMQL_TEST_ACTIVATION_LOG="$_native_log" bash -c '
        source "$1/installer-functions.sh"
        INSTALL_SERVICE=yes; INSTALL_MENU=yes
        NATIVE_APP=/fixture/MemQL.app; NATIVE_STAGE="$1"
        function launchctl() {
            [[ "$*" == "kickstart -k gui/$(id -u)/com.znasllc.memql-worker" ]] || return 99
            printf "%s\n" restarted >> "$MEMQL_TEST_ACTIVATION_LOG"
            [[ "$MODE" != restart-failed ]]
        }
        MODE="$2"
        case "$MODE" in
            no-service) INSTALL_SERVICE=no ;;
            activation-failed) export MEMQL_TEST_ACTIVATION_EXIT=5 ;;
        esac
        install_launch_agent
    ' "$_script_dir/install-mac.sh" "$_native_fixture" "$_native_mode" 2>&1)"
    _rc=$?
    case "$_native_mode" in
        restart)
            expect_eq "native re-enrollment restarts unchanged worker" "$(cat "$_native_log")" $'activated\nrestarted'
            expect_eq "native re-enrollment succeeds" "$_rc" 0 ;;
        no-service)
            expect_eq "native no-service succeeds" "$_rc" 0
            if [[ ! -e "$_native_log" ]]; then
                pass "native no-service does not activate or restart"
            else
                fail "native no-service touched services"
            fi ;;
        activation-failed)
            expect_eq "native activation failure stops install" "$_rc" 5
            expect_eq "native activation failure does not restart" "$(cat "$_native_log")" activated ;;
        restart-failed)
            if [[ "$_rc" != 0 ]]; then
                pass "native restart failure fails install"
            else
                fail "native restart failure reported success"
            fi ;;
    esac
done

# ---------------------------------------------------------------
# Uninstallers -- flags, the missing-tool path, and what is left on disk
# ---------------------------------------------------------------
#
# Driven as the installers are: real subprocesses against a throwaway
# HOME, never this machine's ~/.memql. PATH is cut down to a directory
# holding only the utilities the scripts need, which makes launchctl
# and systemctl ABSENT in every environment this runs in (a developer's
# Linux box has systemctl, CI has it too) and keeps sudo out of reach
# -- a --user-local run must never want it. Every run is checked on the
# ARTIFACTS, which files are gone and which are still there, not on
# what the script said it did.

_nobin="${_tmp}/nobin"
mkdir -p "$_nobin"
# awk reads the enrollment registry (list_enrolled_cluster_urls), id
# names the launchd domain, sleep paces the stop loops -- all three are
# on every macOS and Linux box the scripts run on.
for _tool in bash dirname basename uname rm rmdir sed head ls tr cat mktemp awk id sleep mkdir date mkfifo; do
    if _real="$(command -v "$_tool")"; then
        ln -s "$_real" "${_nobin}/${_tool}"
    else
        fail "uninstall fixture: ${_tool} not on PATH"
    fi
done
if (PATH="$_nobin"; command -v systemctl || command -v launchctl || command -v sudo) >/dev/null 2>&1; then
    fail "uninstall fixture: the reduced PATH still resolves systemctl, launchctl or sudo"
else
    pass "uninstall fixture: reduced PATH has no launchctl, systemctl or sudo"
fi

# write_memql_stub writes a test double for the installed memql at $1:
# it answers --version with $2 (empty: says nothing, like a binary whose
# version line cannot be read) and speaks the URL-scoped unpair contract
# of internal/worker/unpair_url.go over $HOME/.memql -- the dry-run
# counts, the real call rewrites workers.yaml (Go's `homes: []` when the
# last home goes) and deletes the legacy mirror with the last enabled
# home. Mode "old" ($3) exits 2 on `worker unpair --cluster-url`, as a
# pre-0.15.0 flag parser does; "real-fail" and "real-garbage" pass the
# preview and then fail the real call (exit 5, or an unreadable answer).
# Only tools on the reduced PATH are used.
function write_memql_stub() {
    local path="$1" version="$2" mode="${3:-current}"
    {
        echo '#!/usr/bin/env bash'
        echo 'set -uo pipefail'
        printf 'STUB_VERSION=%q\n' "$version"
        printf 'STUB_MODE=%q\n' "$mode"
        cat << 'STUB'
if [[ "${1:-}" == --version ]]; then
    [[ -z "$STUB_VERSION" ]] || echo "memql ${STUB_VERSION} (headless)"
    exit 0
fi
[[ "${1:-}" == worker && "${2:-}" == unpair ]] || exit 0
if [[ "$STUB_MODE" == old ]]; then
    echo "flag provided but not defined: -cluster-url" >&2
    exit 2
fi
shift 2
url=""; dry=no
# real-fail: the preview succeeds and the real call exits 5 having
# written nothing (Go's code for a read/write error of either file);
# real-garbage: the real call answers something the reader cannot parse.
case "$STUB_MODE:$*" in
    real-fail:*--dry-run*|real-garbage:*--dry-run*) ;;
    real-fail:*)    echo "ERROR: scoped unpair could not safely read or update enrollment files; files may need repair" >&2; exit 5 ;;
    real-garbage:*) echo "oops"; exit 0 ;;
esac
while [[ $# -gt 0 ]]; do
    case "$1" in
        --cluster-url) url="$2"; shift 2 ;;
        --dry-run)     dry=yes; shift ;;
        *)             shift ;;
    esac
done
reg="$HOME/.memql/workers.yaml"; legacy="$HOME/.memql/worker.yaml"
[[ -f "$reg" ]] || exit 5
want="$(printf '%s' "$url" | tr '[:upper:]' '[:lower:]' | sed -E 's#/+$##; s#:443$##')"
counts="$(awk -v want="$want" '
    function norm(s) { s = tolower(s); sub(/\/+$/, "", s); sub(/:443$/, "", s); return s }
    function flush() { if (url == "") return; if (norm(url) == want) removed++; else remaining++; url = "" }
    /^homes:[[:space:]]*$/ { in_homes = 1; next }
    !in_homes { next }
    /^[[:space:]]*-[[:space:]]*id:/ { flush(); next }
    /^[[:space:]]*cluster_url:/ { url = $0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", url) }
    END { flush(); printf "%d %d\n", removed, remaining }
' "$reg")"
removed="${counts%% *}"; remaining="${counts##* }"
if [[ "$dry" == yes ]]; then
    printf '{"removed":%s,"remaining":%s,"changed":false,"dry_run":true}\n' "$removed" "$remaining"
    exit 0
fi
changed=false
if [[ "$removed" -gt 0 ]]; then
    rewritten="$(awk -v want="$want" '
        function norm(s) { s = tolower(s); sub(/\/+$/, "", s); sub(/:443$/, "", s); return s }
        function flush() { if (buf == "") return; if (norm(url) != want) kept = kept buf; buf = ""; url = "" }
        /^homes:[[:space:]]*$/ { in_homes = 1; next }
        !in_homes { header = header $0 "\n"; next }
        /^[[:space:]]*-[[:space:]]*id:/ { flush(); buf = $0 "\n"; next }
        /^[[:space:]]*cluster_url:/ { url = $0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", url) }
        { buf = buf $0 "\n" }
        END { flush(); printf "%s", header; if (kept != "") printf "homes:\n%s", kept; else print "homes: []" }
    ' "$reg")"
    printf '%s\n' "$rewritten" > "$reg"
    [[ "$remaining" -gt 0 ]] || rm -f "$legacy"
    changed=true
fi
printf '{"removed":%s,"remaining":%s,"changed":%s,"dry_run":false}\n' "$removed" "$remaining" "$changed"
STUB
    } > "$path"
    chmod +x "$path"
}

# uninstall_fixture lays down what an install leaves behind, for the
# platform under test: workers.yaml + legacy worker.yaml with tokens,
# policy.yaml, a state dir with a log, a --user-local binary (the stub
# above, so a no-flag run can unpair through it as on a real machine)
# with its symlink, and the service file (plus worker.env on linux).
function uninstall_fixture() {
    local home="$1"
    local platform="$2"
    mkdir -p "${home}/.memql/state" "${home}/.memql/bin"
    # The legacy mirror the way write_worker_yaml writes it: state_dir PER
    # HOME (<root>/homes/<id>). A purge that read this first removed one
    # home's subdirectory and left the root and worker.log behind.
    printf 'cluster_url: https://c.example\ntoken: mql_wkr_fixture\nstate_dir: %s/.memql/state/homes/c.example\n' \
        "$home" > "${home}/.memql/worker.yaml"
    # Multi-home registry (install writes this first; legacy is the mirror).
    printf 'version: 1\nworker_name: fixture\nstate_dir: %s/.memql/state\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_fixture\n    enabled: true\n' \
        "$home" > "${home}/.memql/workers.yaml"
    printf 'apps:\n  allow: []\n' > "${home}/.memql/policy.yaml"
    printf 'log line\n' > "${home}/.memql/state/worker.log"
    write_memql_stub "${home}/.memql/bin/${_pf_headless}" 0.15.2
    ln -s "${home}/.memql/bin/${_pf_headless}" "${home}/.memql/bin/${INSTALLED_COMMAND}"
    case "$platform" in
        mac)
            mkdir -p "${home}/Library/LaunchAgents"
            printf '<plist/>\n' > "${home}/Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist"
            # A Mac without Homebrew that `worker setup --inference` set up
            # (memql-cockpit#524): the runtime's agent, its unpacked binary,
            # and a pulled model.
            printf '<plist/>\n' > "${home}/Library/LaunchAgents/com.znasllc.memql-ollama.plist"
            mkdir -p "${home}/.memql/ollama/runtime" "${home}/.memql/ollama/models/blobs"
            printf '#!/bin/sh\nexit 0\n' > "${home}/.memql/ollama/runtime/ollama"
            : > "${home}/.memql/ollama/models/blobs/sha256-fixture"
            ;;
        linux)
            mkdir -p "${home}/.config/systemd/user"
            printf '[Unit]\n' > "${home}/.config/systemd/user/${SERVICE_LABEL_LINUX}.service"
            : > "${home}/.memql/worker.env"
            # A machine `memql worker setup --inference` set up: the
            # runtime's unit, its unpacked binary, and a pulled model.
            printf '[Unit]\n' > "${home}/.config/systemd/user/${OLLAMA_LABEL_LINUX}.service"
            mkdir -p "${home}/.memql/ollama/runtime/bin" "${home}/.memql/ollama/models/blobs"
            printf '#!/bin/sh\nexit 0\n' > "${home}/.memql/ollama/runtime/bin/ollama"
            : > "${home}/.memql/ollama/models/blobs/sha256-fixture"
            ;;
    esac
}

# Ordinary Linux removal exercises a successful isolated service manager;
# explicit missing-command and failed-stop cases are tested separately below.
_uninstall_systemctl_dir="${_tmp}/uninstall-systemctl"
mkdir -p "$_uninstall_systemctl_dir"
printf '#!/bin/bash\nexit 0\n' > "${_uninstall_systemctl_dir}/systemctl"
chmod +x "${_uninstall_systemctl_dir}/systemctl"

# run_uninstaller runs one uninstaller against a HOME with the reduced
# PATH, from the script dir so the sibling lib.sh is what gets sourced.
# Output (both streams) on stdout; the caller reads $? for the code. No
# removal flag is added; --verbose exposes the diagnostic ledger. A run
# with neither --cluster nor --all-homes is the
# shape MemQL OS emitted, and the script decides from the fixture.
function run_uninstaller() {
    local script="$1"
    local home="$2"
    shift 2
    local tool_path="$_nobin"
    if [[ "$script" == uninstall-linux.sh ]]; then tool_path="${_uninstall_systemctl_dir}:$_nobin"; fi
    (cd "$_script_dir" && HOME="$home" PATH="$tool_path" bash "./${script}" --verbose "$@" 2>&1)
}

# run_uninstaller_piped is the same run as the one-liner makes it: the
# script on stdin, `bash -s -- <flags>`, from a cwd holding no lib.sh,
# with RAW_BASE at the real lib.sh over file:// and curl on the PATH to
# fetch it. What MemQL OS's line does, minus the network.
function run_uninstaller_piped() {
    local script="$1"
    local home="$2"
    shift 2
    local tool_path="${_pipebin}:$_nobin"
    if [[ "$script" == uninstall-linux.sh ]]; then tool_path="${_uninstall_systemctl_dir}:${tool_path}"; fi
    (cd "$_piped_cwd" && HOME="$home" PATH="$tool_path" MEMQL_INSTALL_RAW_BASE="file://${_script_dir}" \
        bash -s -- --verbose "$@" < "${_script_dir}/${script}" 2>&1)
}

# tree_fingerprint prints every path under $1 with its content hash or
# link target, so "the dry run changed nothing" is a string comparison.
function tree_fingerprint() {
    (cd "$1" && find . -print | LC_ALL=C sort | while IFS= read -r entry; do
        if [[ -L "$entry" ]]; then
            printf '%s -> %s\n' "$entry" "$(readlink "$entry")"
        elif [[ -f "$entry" ]]; then
            printf '%s %s\n' "$entry" "$(shasum -a 256 < "$entry" | cut -d' ' -f1)"
        else
            printf '%s/\n' "$entry"
        fi
    done)
}

_pipebin="${_tmp}/pipebin"
mkdir -p "$_pipebin"
if _real="$(command -v curl)"; then
    ln -s "$_real" "${_pipebin}/curl"
else
    fail "uninstall fixture: curl not on PATH"
fi

for _platform in mac linux; do
    _un="uninstall-${_platform}.sh"
    case "$_platform" in
        mac)
            _svc="Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist"
            _tool_line="INFO: launchctl not found"
            ;;
        linux)
            _svc=".config/systemd/user/${SERVICE_LABEL_LINUX}.service"
            _tool_line="INFO: stopped and disabled"
            ;;
    esac

    # --help exits 0 with usage; an unknown flag is exit 2 (bad
    # parameter), naming the flag.
    _out="$(cd "$_script_dir" && "./${_un}" --help 2>&1)"
    _rc=$?
    expect_eq "$_un --help exits 0" "$_rc" "0"
    if [[ "$_out" == *"Usage:"* && "$_out" == *"--purge"* && "$_out" == *"--user-local"* ]]; then
        pass "$_un --help documents --purge and --user-local"
    else
        fail "$_un --help should print usage with both flags; got: $_out"
    fi
    _out="$(cd "$_script_dir" && "./${_un}" --bogus 2>&1)"
    _rc=$?
    expect_eq "$_un unknown flag exits 2" "$_rc" "2"
    if [[ "$_out" == *"ERROR: unknown flag --bogus"* ]]; then
        pass "$_un unknown flag is named"
    else
        fail "$_un should name the unknown flag; got: $_out"
    fi

    # --user-local without --purge: the token, the binary + symlink, the
    # service file (and worker.env) go; policy.yaml and the state dir
    # stay, and the flag that removes them is named.
    _uh="${_tmp}/un-home-${_platform}"
    mkdir -p "$_uh"
    uninstall_fixture "$_uh" "$_platform"
    _out="$(run_uninstaller "$_un" "$_uh" --user-local)"
    _rc=$?
    expect_eq "$_un --user-local exits 0 after platform service cleanup" "$_rc" "0"
    if [[ "$_out" == *"$_tool_line"* ]]; then
        pass "$_un reports the platform service cleanup"
    else
        fail "$_un should print '$_tool_line'; got: $_out"
    fi
    if [[ ! -e "${_uh}/.memql/worker.yaml" ]]; then
        pass "$_un --user-local removes worker.yaml (the token)"
    else
        fail "$_un --user-local left worker.yaml behind"
    fi
    if [[ ! -e "${_uh}/.memql/workers.yaml" ]]; then
        pass "$_un --user-local removes workers.yaml (multi-home registry tokens)"
    else
        fail "$_un --user-local left workers.yaml behind"
    fi
    if [[ ! -e "${_uh}/.memql/bin/${INSTALLED_COMMAND}" && ! -L "${_uh}/.memql/bin/${INSTALLED_COMMAND}" \
        && ! -e "${_uh}/.memql/bin/${_pf_headless}" ]]; then
        pass "$_un --user-local removes the binary and its symlink"
    else
        fail "$_un --user-local left the binary or its symlink: $(ls -la "${_uh}/.memql/bin" 2>&1)"
    fi
    if [[ ! -e "${_uh}/${_svc}" ]]; then
        pass "$_un --user-local removes the service file"
    else
        fail "$_un --user-local left ${_svc} behind"
    fi
    if [[ "$_platform" == "linux" ]]; then
        if [[ ! -e "${_uh}/.memql/worker.env" ]]; then
            pass "$_un --user-local removes worker.env"
        else
            fail "$_un --user-local left worker.env behind"
        fi
        # The model runtime's unit goes with the worker's; the runtime and
        # its models stay without --purge, and the summary names them.
        if [[ ! -e "${_uh}/.config/systemd/user/${OLLAMA_LABEL_LINUX}.service" ]]; then
            pass "$_un removes ${OLLAMA_LABEL_LINUX}.service"
        else
            fail "$_un left ${OLLAMA_LABEL_LINUX}.service behind"
        fi
        if [[ -f "${_uh}/.memql/ollama/runtime/bin/ollama" && "$_out" == *"kept ${_uh}/.memql/ollama"* ]]; then
            pass "$_un keeps the model runtime and its models without --purge, and says so"
        else
            fail "$_un removed ~/.memql/ollama without --purge, or did not name it; got: $_out"
        fi
    else
        if [[ ! -e "${_uh}/Library/LaunchAgents/com.znasllc.memql-ollama.plist" ]]; then
            pass "$_un removes the native model runtime's LaunchAgent"
        else
            fail "$_un left com.znasllc.memql-ollama.plist behind"
        fi
        if [[ -f "${_uh}/.memql/ollama/runtime/ollama" && "$_out" == *"kept ${_uh}/.memql/ollama"* ]]; then
            pass "$_un keeps the model runtime and its models without --purge, and says so"
        else
            fail "$_un removed ~/.memql/ollama without --purge, or did not name it; got: $_out"
        fi
    fi
    if [[ -f "${_uh}/.memql/policy.yaml" && -f "${_uh}/.memql/state/worker.log" ]]; then
        pass "$_un keeps policy.yaml and the state dir without --purge"
    else
        fail "$_un removed policy.yaml or the state dir without --purge"
    fi
    if [[ "$_out" == *"--purge"* && "$_out" == *"Fleet -> Machines"* && "$_out" == *"SUCCESS:"* ]]; then
        pass "$_un names --purge, points at MemQL OS for the revoke, and closes with SUCCESS"
    else
        fail "$_un closing block is missing --purge, the revoke sentence or SUCCESS; got: $_out"
    fi

    # --user-local --purge: policy.yaml and the state dir go too, and an
    # emptied ~/.memql is removed with them.
    _ph="${_tmp}/un-purge-home-${_platform}"
    mkdir -p "$_ph"
    uninstall_fixture "$_ph" "$_platform"
    _out="$(run_uninstaller "$_un" "$_ph" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --user-local --purge exits 0" "$_rc" "0"
    if [[ ! -e "${_ph}/.memql/policy.yaml" && ! -e "${_ph}/.memql/state" ]]; then
        pass "$_un --purge removes policy.yaml and the state dir"
    else
        fail "$_un --purge left policy.yaml or the state dir: $(ls -laR "${_ph}/.memql" 2>&1)"
    fi
    if [[ ! -e "${_ph}/.memql/ollama" ]]; then
        pass "$_un --purge removes the model runtime and its models"
    else
        fail "$_un --purge left ~/.memql/ollama: $(ls -laR "${_ph}/.memql/ollama" 2>&1)"
    fi
    if [[ ! -e "${_ph}/.memql" ]]; then
        pass "$_un --purge removes an emptied ~/.memql"
    else
        fail "$_un --purge left ~/.memql behind: $(ls -laR "${_ph}/.memql" 2>&1)"
    fi

    # A ~/.memql that still holds the CLI's clusters.yaml is KEPT under
    # --purge, and the summary names what kept it: an uninstall of the
    # worker must not sign the person out of every cluster.
    _ch="${_tmp}/un-clusters-home-${_platform}"
    mkdir -p "$_ch"
    uninstall_fixture "$_ch" "$_platform"
    printf 'clusters: []\n' > "${_ch}/.memql/clusters.yaml"
    _out="$(run_uninstaller "$_un" "$_ch" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --purge beside clusters.yaml exits 0" "$_rc" "0"
    if [[ -f "${_ch}/.memql/clusters.yaml" && "$_out" == *"clusters.yaml"* ]]; then
        pass "$_un --purge keeps clusters.yaml and says it kept ~/.memql for it"
    else
        fail "$_un --purge should keep clusters.yaml and name it; got: $_out"
    fi

    # Multi-home registry only (no legacy worker.yaml): full uninstall
    # still removes workers.yaml so tokens are not left behind.
    _mh="${_tmp}/un-multi-home-${_platform}"
    mkdir -p "${_mh}/.memql"
    printf 'version: 1\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_only_registry\n    enabled: true\n' \
        > "${_mh}/.memql/workers.yaml"
    _out="$(run_uninstaller "$_un" "$_mh" --user-local)"
    _rc=$?
    expect_eq "$_un with workers.yaml-only exits 0" "$_rc" "0"
    if [[ ! -e "${_mh}/.memql/workers.yaml" ]]; then
        pass "$_un removes workers.yaml when legacy worker.yaml is absent"
    else
        fail "$_un left workers.yaml behind on a registry-only machine"
    fi

    # Nothing installed: every step reports nothing to remove, exit 0,
    # and no ~/.memql is created on the way.
    _eh="${_tmp}/un-empty-home-${_platform}"
    mkdir -p "$_eh"
    _out="$(run_uninstaller "$_un" "$_eh" --user-local)"
    _rc=$?
    expect_eq "$_un on a machine with nothing installed exits 0" "$_rc" "0"
    if [[ "$_out" == *"nothing to remove"* && ! -e "${_eh}/.memql" ]]; then
        pass "$_un with nothing installed reports it and creates nothing"
    else
        fail "$_un with nothing installed should say so and leave HOME untouched; got: $_out"
    fi

    # The fence: a state_dir worker.yaml points OUTSIDE ~/.memql is never
    # deleted, purge or not. That path is operator-authored text, and
    # rm -rf on it is the one thing this script must not do.
    _fh="${_tmp}/un-fence-home-${_platform}"
    _outside="${_tmp}/un-outside-${_platform}"
    mkdir -p "$_fh" "$_outside"
    uninstall_fixture "$_fh" "$_platform"
    printf 'keep me\n' > "${_outside}/precious"
    # In BOTH files: the registry's state_dir is read first, the mirror's second.
    printf 'cluster_url: https://c.example\ntoken: mql_wkr_fixture\nstate_dir: %s\n' \
        "$_outside" > "${_fh}/.memql/worker.yaml"
    printf 'version: 1\nworker_name: fixture\nstate_dir: %s\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_fixture\n    enabled: true\n' \
        "$_outside" > "${_fh}/.memql/workers.yaml"
    _out="$(run_uninstaller "$_un" "$_fh" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --purge with an outside state_dir exits 0" "$_rc" "0"
    if [[ -f "${_outside}/precious" && "$_out" == *"outside"* && "$_out" == *"$_outside"* ]]; then
        pass "$_un --purge leaves a state_dir outside ~/.memql alone and names it"
    else
        fail "$_un --purge touched or failed to name the outside state_dir; got: $_out"
    fi
    if [[ ! -e "${_fh}/.memql/policy.yaml" ]]; then
        pass "$_un --purge still removes policy.yaml when the state_dir was refused"
    else
        fail "$_un --purge should still remove policy.yaml"
    fi
done

# Default (system) mode with nothing at /usr/local/bin must not reach
# for sudo: the presence check runs first, and a machine with nothing
# there is told so rather than asked for a password to find out.
# Guarded, because a developer's machine may hold a real install at
# /usr/local/bin and this test must never remove one.
_sys_present="no"
for _n in "$INSTALLED_COMMAND" "$_pf_headless" "$_pf_computeruse" $LEGACY_BINARIES; do
    if [[ -e "/usr/local/bin/${_n}" || -L "/usr/local/bin/${_n}" ]]; then
        _sys_present="yes"
    fi
done
if [[ "$_sys_present" == "yes" ]]; then
    echo "INFO: /usr/local/bin holds a memql install; skipping the system-mode no-op check"
else
    for _platform in mac linux; do
        _un="uninstall-${_platform}.sh"
        _sh="${_tmp}/un-sys-home-${_platform}"
        mkdir -p "$_sh"
        _out="$(run_uninstaller "$_un" "$_sh")"
        _rc=$?
        expect_eq "$_un system mode with nothing installed exits 0 without sudo" "$_rc" "0"
        if [[ "$_out" == *"INFO: no ${INSTALLED_COMMAND} binary at /usr/local/bin; nothing to remove"* ]]; then
            pass "$_un system mode reports the empty prefix rather than asking for sudo"
        else
            fail "$_un system mode should report nothing at /usr/local/bin; got: $_out"
        fi
    done
fi

# Exercise the real service-stop branch without reaching this host's manager.
_native_systemctl_dir="${_tmp}/native-systemctl"
mkdir -p "$_native_systemctl_dir"
cat > "${_native_systemctl_dir}/systemctl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >> "$MEMQL_TEST_SYSTEMCTL_LOG"
if [[ "$*" == *"disable --now memql-ollama.service"* && "${MEMQL_TEST_STOP_FAIL:-}" == yes ]]; then
    exit 1
fi
exit 0
STUB
chmod +x "${_native_systemctl_dir}/systemctl"
for _stop_failure in no yes; do
    _native_home="${_tmp}/native-stop-${_stop_failure}"
    uninstall_fixture "$_native_home" linux
    _native_log="${_tmp}/native-stop-${_stop_failure}.log"
    _out="$(cd "$_script_dir" && HOME="$_native_home" PATH="${_native_systemctl_dir}:$_nobin" MEMQL_TEST_SYSTEMCTL_LOG="$_native_log" MEMQL_TEST_STOP_FAIL="$_stop_failure" bash ./uninstall-linux.sh --verbose --user-local --purge 2>&1)"
    _rc=$?
    if [[ "$_stop_failure" == no ]]; then
        expect_eq "native uninstall exits cleanly after systemd stops" "$_rc" "0"
        if grep -qF -- '--user disable --now memql-ollama.service' "$_native_log" && [[ ! -e "${_native_home}/.memql/ollama" ]]; then
            pass "native uninstall stops the runtime and purges its files"
        else
            fail "native uninstall did not stop and purge the runtime"
        fi
    else
        if [[ "$_rc" -eq 5 && "$_out" == *PARTIAL* && -e "${_native_home}/.memql/ollama/runtime/bin/ollama" && -e "${_native_home}/.config/systemd/user/memql-ollama.service" && ! -e "${_native_home}/.memql/worker.yaml" ]]; then
            pass "failed runtime stop preserves runtime and reports partial uninstall while removing token"
        else
            fail "failed runtime stop must not purge or claim success: $_out"
        fi
    fi
done

# Missing the command does not establish that an installed service stopped.
_native_missing_home="${_tmp}/native-stop-missing"
uninstall_fixture "$_native_missing_home" linux
_out="$(cd "$_script_dir" && HOME="$_native_missing_home" PATH="$_nobin" bash ./uninstall-linux.sh --verbose --user-local --purge 2>&1)"
_rc=$?
if [[ "$_rc" -eq 5 && "$_out" == *PARTIAL* && -e "${_native_missing_home}/.memql/ollama/runtime/bin/ollama" && -e "${_native_missing_home}/.config/systemd/user/memql-ollama.service" && ! -e "${_native_missing_home}/.memql/worker.yaml" ]]; then
    pass "missing systemctl preserves runtime for safe retry and removes token"
else
    fail "missing systemctl must not purge a potentially running runtime: $_out"
fi

# ---------------------------------------------------------------
# The no-flag contract -- what MemQL OS's one-liner relies on
# ---------------------------------------------------------------
#
# Real runs against fixture HOMEs, both platforms, with the stub memql
# standing in for the installed one. Every "nothing was changed" claim
# is a fingerprint comparison of the whole HOME, not a reading of what
# the script said. The auto-detect runs (no --user-local) are only
# meaningful on a machine with no system install to detect -- one
# would make the mac driver ask for sudo it cannot get -- so on such a
# machine they run with --user-local and the detection line is skipped.

_auto_ok="yes"
if [[ "$_sys_present" == yes || -e /Applications/MemQL.app ]]; then
    _auto_ok="no"
    echo "INFO: a system install is present on this machine; auto-detect runs use --user-local"
fi
_auto_flag=""
[[ "$_auto_ok" == yes ]] || _auto_flag="--user-local"

function add_second_home() {
    printf '  - id: d.example\n    cluster_url: https://d.example\n    token: mql_wkr_second\n    enabled: true\n' >> "$1/.memql/workers.yaml"
}

function fixture_home() {
    local name="$1" platform="$2"
    local home="${_tmp}/nf-${name}-${platform}"
    mkdir -p "$home"
    uninstall_fixture "$home" "$platform"
    printf '%s' "$home"
}

for _platform in mac linux; do
    _un="uninstall-${_platform}.sh"
    case "$_platform" in
        mac)   _svc="Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist" ;;
        linux) _svc=".config/systemd/user/${SERVICE_LABEL_LINUX}.service" ;;
    esac

    # (a) No flags, one enrollment: removed as --cluster=<its url>, through
    # the binary, and the runtime goes with it (it was the last one).
    _h="$(fixture_home one "$_platform")"
    # shellcheck disable=SC2086  # _auto_flag is empty or one flag
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un no flags + one enrollment exits 0" "$_rc" "0"
    if [[ "$_out" == *"one worker enrollment on this machine, https://c.example"* ]]; then
        pass "$_un no flags names the one enrollment it removes"
    else
        fail "$_un no flags should name the enrollment; got: $_out"
    fi
    if [[ ! -e "${_h}/.memql/workers.yaml" && ! -e "${_h}/.memql/worker.yaml" && ! -e "${_h}/${_svc}" \
        && ! -e "${_h}/.memql/bin/${INSTALLED_COMMAND}" && ! -L "${_h}/.memql/bin/${INSTALLED_COMMAND}" ]]; then
        pass "$_un no flags + one enrollment removes tokens, service file and binary"
    else
        fail "$_un no flags + one enrollment left something: $(ls -laR "${_h}" 2>&1)"
    fi
    # (d) ...and found the per-user install without being told the mode.
    if [[ "$_auto_ok" == yes ]]; then
        if [[ "$_out" == *"user-local install found"* && "$_out" == *"no system install"* ]]; then
            pass "$_un detects the user-local install without --user-local"
        else
            fail "$_un should report the detected shapes; got: $_out"
        fi
    fi

    # (b) No flags, zero enrollments: the runtime files go, exit 0.
    _h="$(fixture_home zero "$_platform")"
    rm -f "${_h}/.memql/workers.yaml" "${_h}/.memql/worker.yaml"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un no flags + zero enrollments exits 0" "$_rc" "0"
    if [[ "$_out" == *"no worker enrollment on this machine"* && ! -e "${_h}/${_svc}" \
        && ! -e "${_h}/.memql/bin/${INSTALLED_COMMAND}" && ! -L "${_h}/.memql/bin/${INSTALLED_COMMAND}" ]]; then
        pass "$_un no flags + zero enrollments says so and removes the runtime"
    else
        fail "$_un no flags + zero enrollments; got: $_out"
    fi

    # (c) No flags, two enrollments: refused (2) with both URLs and the
    # command for each, and nothing touched.
    _h="$(fixture_home two "$_platform")"
    add_second_home "$_h"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un no flags + two enrollments exits 2" "$_rc" "2"
    if [[ "$_out" == *"--cluster=https://c.example"* && "$_out" == *"--cluster=https://d.example"* \
        && "$_out" == *"--all-homes"* && "$_out" == *"2 clusters are enrolled"* ]]; then
        pass "$_un two enrollments lists both URLs with the command for each"
    else
        fail "$_un two enrollments should list both commands; got: $_out"
    fi
    expect_eq "$_un two enrollments changes nothing" "$(tree_fingerprint "$_h")" "$_before"

    # --cluster=URL with zero enrollments: nothing to unpair, proceed as
    # --all-homes (a machine that never paired is still being uninstalled).
    _h="$(fixture_home cluster-zero "$_platform")"
    rm -f "${_h}/.memql/workers.yaml" "${_h}/.memql/worker.yaml"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster with zero enrollments exits 0" "$_rc" "0"
    if [[ "$_out" == *"nothing to unpair for https://c.example"* && ! -e "${_h}/${_svc}" ]]; then
        pass "$_un --cluster with zero enrollments says so and removes the runtime"
    else
        fail "$_un --cluster with zero enrollments; got: $_out"
    fi

    # --cluster=URL that matches no enrollment: refused (3) naming the
    # enrolled one and its command; nothing touched.
    _h="$(fixture_home cluster-nomatch "$_platform")"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://nomatch.example $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster with no matching enrollment exits 3" "$_rc" "3"
    if [[ "$_out" == *"no enrollment on this machine matches --cluster=https://nomatch.example"* \
        && "$_out" == *"--cluster=https://c.example"* && "$_out" == *"REFUSED"* ]]; then
        pass "$_un --cluster no-match lists the enrolled cluster and its command"
    else
        fail "$_un --cluster no-match should list the enrolled command; got: $_out"
    fi
    expect_eq "$_un --cluster no-match changes nothing" "$(tree_fingerprint "$_h")" "$_before"

    # --cluster=URL spelled another way (capitals, trailing slash) is the
    # same enrollment -- matched the way the worker matches.
    _h="$(fixture_home cluster-spelling "$_platform")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" "--cluster=https://C.EXAMPLE/" $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster with another spelling of the URL exits 0" "$_rc" "0"
    if [[ ! -e "${_h}/.memql/workers.yaml" && ! -e "${_h}/${_svc}" ]]; then
        pass "$_un --cluster matches through case and a trailing slash"
    else
        fail "$_un --cluster spelling did not match; got: $_out"
    fi

    # --cluster URL (space form) is accepted too.
    _h="$(fixture_home cluster-space "$_platform")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster https://c.example $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster URL (space form) exits 0" "$_rc" "0"

    # --cluster=URL with a sibling: that enrollment goes, the sibling and
    # the whole runtime stay, exit 0.
    _h="$(fixture_home cluster-sibling "$_platform")"
    add_second_home "$_h"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster with a sibling exits 0" "$_rc" "0"
    if grep -q 'id: d.example' "${_h}/.memql/workers.yaml" && ! grep -q 'id: c.example' "${_h}/.memql/workers.yaml" \
        && [[ -e "${_h}/${_svc}" && -x "${_h}/.memql/bin/${INSTALLED_COMMAND}" && "$_out" == *"1 other enrollment(s)"* ]]; then
        pass "$_un --cluster with a sibling removes one home and keeps the runtime"
    else
        fail "$_un --cluster with a sibling; got: $_out $(cat "${_h}/.memql/workers.yaml" 2>&1)"
    fi

    # An installed memql too old for the unpair contract (0.15.0): the
    # only enrollment goes with the runtime; with a sibling it refuses (4).
    _h="$(fixture_home old-one "$_platform")"
    write_memql_stub "${_h}/.memql/bin/${_pf_headless}" 0.14.0 old
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un old binary + one enrollment exits 0" "$_rc" "0"
    if [[ "$_out" == *"predates URL-scoped unpair"* && "$_out" == *"only enrollment"* && ! -e "${_h}/.memql/workers.yaml" ]]; then
        pass "$_un old binary + one enrollment falls back to full removal and says so"
    else
        fail "$_un old binary + one enrollment; got: $_out"
    fi
    _h="$(fixture_home old-two "$_platform")"
    add_second_home "$_h"
    write_memql_stub "${_h}/.memql/bin/${_pf_headless}" 0.14.0 old
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
    _rc=$?
    expect_eq "$_un old binary + sibling exits 4" "$_rc" "4"
    if [[ "$_out" == *"0.15.0 or newer"* && "$_out" == *"--all-homes"* ]]; then
        pass "$_un old binary + sibling names the upgrade and --all-homes"
    else
        fail "$_un old binary + sibling remedy; got: $_out"
    fi
    expect_eq "$_un old binary + sibling changes nothing" "$(tree_fingerprint "$_h")" "$_before"
    # A binary whose version line says nothing but whose flag parser
    # refuses --cluster-url (exit 2) is the same case, found at the call.
    _h="$(fixture_home old-mute "$_platform")"
    write_memql_stub "${_h}/.memql/bin/${_pf_headless}" "" old
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un mute old binary + one enrollment exits 0" "$_rc" "0"
    if [[ "$_out" == *"does not support URL-scoped unpair"* && ! -e "${_h}/.memql/workers.yaml" ]]; then
        pass "$_un mute old binary is detected at the call and falls back"
    else
        fail "$_un mute old binary; got: $_out"
    fi

    # No binary at all: the only enrollment goes with the runtime; with
    # a sibling and an explicit --cluster it refuses (4). Skipped on a
    # machine with a real system memql: the binary is looked for in both
    # shapes now, and that one would be found and asked.
    if [[ "$_sys_present" == yes ]]; then
        echo "INFO: /usr/local/bin holds a memql install; skipping the no-binary checks"
    fi
    _h="$(fixture_home nobin-one "$_platform")"
    rm -f "${_h}/.memql/bin/${_pf_headless}" "${_h}/.memql/bin/${INSTALLED_COMMAND}"
    if [[ "$_sys_present" != yes ]]; then
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un no binary + one enrollment exits 0" "$_rc" "0"
        if [[ "$_out" == *"only enrollment"* && ! -e "${_h}/.memql/workers.yaml" && ! -e "${_h}/.memql/worker.yaml" ]]; then
            pass "$_un no binary + one enrollment removes the worker files with the runtime"
        else
            fail "$_un no binary + one enrollment; got: $_out"
        fi
        _h="$(fixture_home nobin-two "$_platform")"
        add_second_home "$_h"
        rm -f "${_h}/.memql/bin/${_pf_headless}" "${_h}/.memql/bin/${INSTALLED_COMMAND}"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        _rc=$?
        expect_eq "$_un no binary + sibling + --cluster exits 4" "$_rc" "4"
        expect_eq "$_un no binary + sibling changes nothing" "$(tree_fingerprint "$_h")" "$_before"
    fi

    # A registry the reader cannot make sense of: refused (5) untouched;
    # --all-homes is the way past it.
    _h="$(fixture_home unreadable "$_platform")"
    printf 'homes: [invalid yaml' > "${_h}/.memql/workers.yaml"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
    _rc=$?
    expect_eq "$_un unreadable registry + no flags exits 5" "$_rc" "5"
    expect_eq "$_un unreadable registry changes nothing" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --all-homes $_auto_flag)"
    _rc=$?
    expect_eq "$_un unreadable registry + --all-homes exits 0" "$_rc" "0"
    if [[ ! -e "${_h}/.memql/workers.yaml" ]]; then
        pass "$_un --all-homes removes an unreadable registry"
    else
        fail "$_un --all-homes left the unreadable registry"
    fi

    # Bad parameters stay 2: an empty --cluster=, and both scope flags.
    _h="$(fixture_home badparam "$_platform")"
    _out="$(run_uninstaller "$_un" "$_h" --cluster=)"
    expect_eq "$_un --cluster= (empty) exits 2" "$?" "2"
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example --all-homes)"
    expect_eq "$_un --cluster + --all-homes exits 2" "$?" "2"

    # --dry-run: the plan, and HOME byte-identical afterwards -- with no
    # flags, with --purge, with --cluster piped exactly as the one-liner
    # runs it, and the two-enrollment refusal with the same code.
    _h="$(fixture_home dry "$_platform")"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --dry-run exits 0" "$_rc" "0"
    if [[ "$_out" == *"DRY RUN: nothing was changed."* && "$_out" == *"would unpair:  https://c.example"* \
        && "$_out" == *"would remove:  ${_h}/.memql/workers.yaml"* && "$_out" == *"sudo:          not needed"* \
        && "$_out" == *"would keep:    ${_h}/.memql/policy.yaml"* ]]; then
        pass "$_un --dry-run prints the plan (unpair, paths, sudo, kept state)"
    else
        fail "$_un --dry-run plan is incomplete; got: $_out"
    fi
    expect_eq "$_un --dry-run leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge --dry-run exits 0" "$_rc" "0"
    if [[ "$_out" == *"would remove:  ${_h}/.memql/policy.yaml"* && "$_out" == *"would remove:  ${_h}/.memql/state (recursively)"* ]]; then
        pass "$_un --purge --dry-run plans the purge targets"
    else
        fail "$_un --purge --dry-run plan; got: $_out"
    fi
    expect_eq "$_un --purge --dry-run leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller_piped "$_un" "$_h" --cluster=https://c.example --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un piped 'bash -s -- --cluster=URL --dry-run' exits 0" "$_rc" "0"
    if [[ "$_out" == *"DRY RUN: nothing was changed."* && "$_out" == *"would unpair:  https://c.example"* ]]; then
        pass "$_un piped --cluster --dry-run prints the plan"
    else
        fail "$_un piped --cluster --dry-run; got: $_out"
    fi
    expect_eq "$_un piped --cluster --dry-run leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    add_second_home "$_h"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --dry-run with two enrollments refuses with 2" "$_rc" "2"
    expect_eq "$_un --dry-run refusal leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # The remedies are the same run plus the missing piece: a copied one
    # must still be a dry run.
    if [[ "$_out" == *"--dry-run --cluster=https://c.example"* && "$_out" == *"--dry-run --all-homes"* ]]; then
        pass "$_un --dry-run refusal carries --dry-run into every printed remedy"
    else
        fail "$_un --dry-run refusal drops --dry-run from the remedy; got: $_out"
    fi
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://nomatch.example --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster no-match --dry-run refuses with 3" "$_rc" "3"
    if [[ "$_out" == *"--dry-run --cluster=https://c.example"* ]]; then
        pass "$_un --cluster no-match --dry-run carries --dry-run into the remedy"
    else
        fail "$_un --cluster no-match --dry-run remedy; got: $_out"
    fi
    expect_eq "$_un --cluster no-match --dry-run leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --cluster --dry-run with a sibling exits 0" "$_rc" "0"
    if [[ "$_out" == *"1 other enrollment(s) would remain"* && "$_out" == *"would stay for the remaining enrollment(s)"* ]]; then
        pass "$_un --cluster --dry-run with a sibling says the runtime would stay"
    else
        fail "$_un --cluster --dry-run with a sibling; got: $_out"
    fi
    expect_eq "$_un --cluster --dry-run with a sibling leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"

    # (f) The exact shapes MemQL OS emits, piped, on an empty HOME: all
    # parse, all exit 0, none creates ~/.memql.
    for _shape in "" "--purge" "--user-local" "--cluster=https://api.example.test" "--cluster=https://api.example.test --dry-run" "--cluster=https://api.example.test --purge --user-local"; do
        _h="${_tmp}/nf-shape-${_platform}-${_shape//[^a-z]/_}"
        mkdir -p "$_h"
        # shellcheck disable=SC2086  # the shape is a flag list on purpose
        _out="$(run_uninstaller_piped "$_un" "$_h" $_shape)"
        _rc=$?
        if [[ "$_rc" == 0 && ! -e "${_h}/.memql" && "$_out" != *"choose --cluster"* \
            && ( "$_out" == *"SUCCESS:"* || "$_out" == *"DRY RUN: nothing was changed."* ) ]]; then
            pass "$_un piped 'bash -s -- ${_shape}' parses and runs clean on an empty HOME"
        else
            fail "$_un piped 'bash -s -- ${_shape}' rc=$_rc; got: $_out"
        fi
    done
done

# A symlinked enrollment file is not decided from (3), on either driver.
_h="${_tmp}/nf-alias"
mkdir -p "${_h}/.memql" "${_tmp}/nf-alias-target"
printf 'version: 1\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_x\n' > "${_tmp}/nf-alias-target/workers.yaml"
ln -s "${_tmp}/nf-alias-target/workers.yaml" "${_h}/.memql/workers.yaml"
for _un in uninstall-mac.sh uninstall-linux.sh; do
    _out="$(run_uninstaller "$_un" "$_h" --user-local)"
    expect_eq "$_un symlinked workers.yaml + no flags exits 3" "$?" "3"
    if [[ -f "${_tmp}/nf-alias-target/workers.yaml" && -L "${_h}/.memql/workers.yaml" ]]; then
        pass "$_un symlinked workers.yaml is left alone"
    else
        fail "$_un touched the symlinked registry or its target"
    fi
done

# ---------------------------------------------------------------
# Review findings -- one regression per confirmed defect
# ---------------------------------------------------------------
#
# An adversarial review of the no-flag contract confirmed thirteen
# defects (two of them one defect seen through two lenses); each has a
# test here that failed before its fix. The destructive ones plant
# credentials, backups, certificates and clusters.yaml that must survive
# a --purge, and every destructive case has a --dry-run twin asserted on
# a byte-identical HOME. The system-shape cases run the REAL driver
# (main included) with its two path-answering functions redefined, so a
# fixture directory stands in for /usr/local/bin and /Applications and
# nothing on this machine is looked at.

# plant_protected writes what a --purge must never take; protected_intact
# answers whether every one of them still reads as written.
function plant_protected() {
    local home="$1"
    mkdir -p "${home}/.memql/credentials" "${home}/.memql/backups" "${home}/.memql/certs"
    printf 'secret\n' > "${home}/.memql/credentials/token"
    printf 'previous worker\n' > "${home}/.memql/backups/previous-worker"
    printf 'cert\n' > "${home}/.memql/certs/client.pem"
    printf 'clusters: []\n' > "${home}/.memql/clusters.yaml"
}

function protected_intact() {
    local home="$1"
    [[ "$(cat "${home}/.memql/credentials/token" 2>/dev/null)" == secret \
        && "$(cat "${home}/.memql/backups/previous-worker" 2>/dev/null)" == "previous worker" \
        && "$(cat "${home}/.memql/certs/client.pem" 2>/dev/null)" == cert \
        && "$(cat "${home}/.memql/clusters.yaml" 2>/dev/null)" == "clusters: []" ]]
}

# set_state_dir rewrites BOTH enrollment files of a fixture HOME with one
# state_dir spelling: the registry is read first, the mirror second.
function set_state_dir() {
    local home="$1" spelling="$2"
    printf 'cluster_url: https://c.example\ntoken: mql_wkr_fixture\nstate_dir: %s\n' "$spelling" > "${home}/.memql/worker.yaml"
    printf 'version: 1\nworker_name: fixture\nstate_dir: %s\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_fixture\n    enabled: true\n' \
        "$spelling" > "${home}/.memql/workers.yaml"
}

# A launchctl that answers from a state directory, as the macOS end-to-end
# test's does: print -> loaded iff the label's file exists, bootout removes
# it, bootstrap recreates it. Lets the mac driver take its "the worker was
# running" branches under the reduced PATH.
_review_launchctl_dir="${_tmp}/review-launchctl"
mkdir -p "$_review_launchctl_dir"
# MEMQL_TEST_STUBBORN names a label whose bootout does nothing (the
# agent stays loaded); MEMQL_TEST_BOOTSTRAP_FAIL=1 makes bootstrap fail.
# The state directory is required (:?), so an unset fixture path can
# never make the stub touch anything outside it.
cat > "${_review_launchctl_dir}/launchctl" <<'STUB'
#!/usr/bin/env bash
state="${MEMQL_TEST_AGENT_STATE:?}"
case "$1" in
    print)     test -f "${state}/${2##*/}" ;;
    bootout)   [[ "${2##*/}" == "${MEMQL_TEST_STUBBORN:-}" ]] || rm -f -- "${state:?}/${2##*/}" ;;
    bootstrap) [[ "${MEMQL_TEST_BOOTSTRAP_FAIL:-}" != 1 ]] || exit 5; : > "${state}/$(basename "$3" .plist)" ;;
    *)         exit 98 ;;
esac
STUB
chmod +x "${_review_launchctl_dir}/launchctl"

# A systemctl whose `start` fails when MEMQL_TEST_START_FAIL=1 and
# succeeds at everything else, for the linux sibling-remaining cases.
_review_systemctl_dir="${_tmp}/review-systemctl"
mkdir -p "$_review_systemctl_dir"
cat > "${_review_systemctl_dir}/systemctl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *" start "* && "${MEMQL_TEST_START_FAIL:-}" == 1 ]]; then exit 5; fi
exit 0
STUB
chmod +x "${_review_systemctl_dir}/systemctl"

# run_uninstaller_agent is run_uninstaller with the service stub for the
# platform on the PATH: launchctl above for mac (state in $1), the
# configurable systemctl above for linux (default: everything succeeds).
function run_uninstaller_agent() {
    local state="$1" script="$2" home="$3"
    shift 3
    local tool_path="${_review_launchctl_dir}:$_nobin"
    if [[ "$script" == uninstall-linux.sh ]]; then tool_path="${_review_systemctl_dir}:$_nobin"; fi
    (cd "$_script_dir" && HOME="$home" PATH="$tool_path" MEMQL_TEST_AGENT_STATE="$state" bash "./${script}" --verbose "$@" 2>&1)
}

# run_with_system_shape runs the REAL driver with install_mode_dir and
# worker_app_for_mode redefined, so $3 stands in for /usr/local/bin (and
# $3/MemQL.app for /Applications/MemQL.app), under the reduced PATH plus
# the platform's service stub -- and so with NO sudo. The driver is
# sourced with its `main "$@"` line removed and main called after the
# overrides; $0 is the script's real path, so its sibling lib.sh
# sourcing works as it does from a clone.
_review_scratch="${_tmp}/review-scratch"
mkdir -p "$_review_scratch"
function run_with_system_shape() {
    local state="$1" script="$2" home="$3" sysbin="$4"
    shift 4
    local tool_path="${_review_launchctl_dir}:$_nobin"
    if [[ "$script" == uninstall-linux.sh ]]; then tool_path="${_uninstall_systemctl_dir}:$_nobin"; fi
    (cd "$_script_dir" && HOME="$home" PATH="$tool_path" MEMQL_TEST_AGENT_STATE="$state" \
        MEMQL_TEST_SYSBIN="$sysbin" MEMQL_TEST_SCRATCH="$_review_scratch" bash -c '
        sed "/^main \"\$@\"\$/d" "$0" > "$MEMQL_TEST_SCRATCH/$(basename "$0")"
        source "$MEMQL_TEST_SCRATCH/$(basename "$0")"
        function install_mode_dir() {
            case "$1" in
                system)     echo "$MEMQL_TEST_SYSBIN" ;;
                user-local) echo "$INSTALL_PREFIX_USER" ;;
                *)          echo "ERROR: unknown install mode $1" >&2; return 1 ;;
            esac
        }
        function worker_app_for_mode() {
            case "$1" in system) echo "$MEMQL_TEST_SYSBIN/MemQL.app" ;; *) echo "$HOME/Applications/MemQL.app" ;; esac
        }
        main --verbose "$@"
    ' "$_script_dir/$script" "$@" 2>&1)
}

# [X3] worker_state_dir_from_yaml answers the machine's state ROOT.
_sd="${_tmp}/review-state-dir"
mkdir -p "$_sd/installer" "$_sd/legacy" "$_sd/slash" "$_sd/homes-name" "$_sd/root-homes"
printf 'state_dir: %s/.memql/state/homes/c.example\n' "$_sd" > "$_sd/installer/worker.yaml"
printf 'state_dir: %s/.memql/state\nhomes: []\n' "$_sd" > "$_sd/installer/workers.yaml"
expect_eq "state root: registry first, per-home mirror second" \
    "$(worker_state_dir_from_yaml "$_sd/installer/worker.yaml")" "$_sd/.memql/state"
printf 'state_dir: %s/.memql/state/homes/c.example\n' "$_sd" > "$_sd/legacy/worker.yaml"
expect_eq "state root: a legacy-only per-home dir answers its root" \
    "$(worker_state_dir_from_yaml "$_sd/legacy/worker.yaml")" "$_sd/.memql/state"
printf 'state_dir: ~/.memql/state/homes/x/\n' > "$_sd/slash/worker.yaml"
expect_eq "state root: ~/ and a trailing slash are expanded and dropped" \
    "$(worker_state_dir_from_yaml "$_sd/slash/worker.yaml")" "${HOME}/.memql/state"
printf 'state_dir: ~/.memql/homes\n' > "$_sd/homes-name/worker.yaml"
expect_eq "state root: a directory merely named homes is left alone" \
    "$(worker_state_dir_from_yaml "$_sd/homes-name/worker.yaml")" "${HOME}/.memql/homes"
printf 'state_dir: /homes/x\n' > "$_sd/root-homes/worker.yaml"
expect_eq "state root: /homes/x has no root above it" \
    "$(worker_state_dir_from_yaml "$_sd/root-homes/worker.yaml")" "/homes/x"

# [9] Every block-list spelling yaml.v3 accepts reads as the same enrollments.
_rs="${_tmp}/review-registry-shapes"
mkdir -p "$_rs"
printf 'version: 1\nhomes:\n- id: c.example\n  cluster_url: https://c.example\n  token: t\n- id: d.example\n  cluster_url: https://d.example\n  token: t\n' > "$_rs/column0.yaml"
printf 'version: 1\nhomes: # enrolled clusters\n  - id: c.example\n    cluster_url: https://c.example\n  - id: d.example\n    cluster_url: https://d.example\n' > "$_rs/comment.yaml"
printf 'version: 1\nhomes:\n  - cluster_url: https://c.example\n    id: c.example\n  - cluster_url: https://d.example\n    id: d.example\nother:\n  cluster_url: https://not-a-home.example\n' > "$_rs/urlfirst.yaml"
printf 'version: 1\nhomes: [] # none\n' > "$_rs/empty.yaml"
for _shape in column0 comment urlfirst; do
    expect_eq "registry shape ${_shape} lists both clusters" \
        "$(list_enrolled_cluster_urls "$_rs/${_shape}.yaml" /nonexistent | tr '\n' ' ')" "https://c.example https://d.example "
done
expect_eq "registry shape empty lists nothing" "$(list_enrolled_cluster_urls "$_rs/empty.yaml" /nonexistent)" ""

# Second pass: a block sequence of FLOW mappings is valid YAML the binary
# decodes, and the block reader cannot see inside it -- it read as zero
# enrollments and the pre-fix scripts removed every token. The gate now
# refuses (5) any item that opens a flow collection, on the dash line,
# on a bare-dash continuation, or in the header.
printf 'version: 1\nhomes:\n  - {id: c.example, cluster_url: https://c.example, token: mql_wkr_c, enabled: true}\n  - {id: d.example, cluster_url: https://d.example, token: mql_wkr_d}\n' > "$_rs/flow.yaml"
printf 'version: 1\nhomes:\n  - {id: c.example,\n     cluster_url: https://c.example}\n  - {id: d.example, cluster_url: https://d.example}\n' > "$_rs/flowmulti.yaml"
printf 'version: 1\nhomes:\n  -\n    {id: c.example, cluster_url: https://c.example}\n' > "$_rs/flowbare.yaml"
printf 'version: 1\nhomes:\n  - [https://c.example]\n' > "$_rs/flowseq.yaml"
printf 'version: 1\nhomes: [{id: c.example, cluster_url: https://c.example}]\n' > "$_rs/flowheader.yaml"
for _shape in flow flowmulti flowbare flowseq flowheader; do
    _out="$(list_enrolled_cluster_urls "$_rs/${_shape}.yaml" /nonexistent 2>/dev/null)"
    _rc=$?
    expect_eq "registry shape ${_shape} is refused with 5" "$_rc" "5"
    expect_eq "registry shape ${_shape} lists nothing" "$_out" ""
done
# A `{` inside a quoted scalar is a value, not a flow collection.
printf 'version: 1\nhomes:\n  - id: c.example\n    cluster_url: "https://c.example"\n    token: "{not a mapping}"\n' > "$_rs/quotedbrace.yaml"
expect_eq "registry shape quotedbrace still reads" "$(list_enrolled_cluster_urls "$_rs/quotedbrace.yaml" /nonexistent)" "https://c.example"

# The installer's three readers accept the same block spellings, so a
# second install beside a commented header or a url-first item keeps the
# siblings it could not see before (a review finding).
for _shape in column0 comment urlfirst; do
    expect_eq "find_home_id_by_cluster_url reads the ${_shape} spelling" \
        "$(find_home_id_by_cluster_url "$_rs/${_shape}.yaml" https://C.EXAMPLE/)" "c.example"
    expect_eq "find_home_cluster_url_by_id reads the ${_shape} spelling" \
        "$(find_home_cluster_url_by_id "$_rs/${_shape}.yaml" d.example)" "https://d.example"
    _wd="${_tmp}/review-writer-${_shape}"
    mkdir -p "$_wd"
    cp "$_rs/${_shape}.yaml" "$_wd/workers.yaml"
    if write_worker_yaml "$_wd/worker.yaml" https://e.example mql_wkr_e host1 no HEADLESS >/dev/null 2>&1 \
        && grep -q 'id: c.example' "$_wd/workers.yaml" && grep -q 'id: d.example' "$_wd/workers.yaml" \
        && grep -q 'id: e.example' "$_wd/workers.yaml" && grep -q 'cluster_url: https://d.example' "$_wd/workers.yaml"; then
        pass "write_worker_yaml keeps both siblings of the ${_shape} spelling"
    else
        fail "write_worker_yaml dropped a sibling of the ${_shape} spelling: $(cat "$_wd/workers.yaml" 2>&1)"
    fi
done

# The registry writer ALSO keeps a `#` inside a header value: the scalar
# reader stops only at a comment (whitespace, then `#`).
_wd="${_tmp}/review-writer-hash"
mkdir -p "$_wd"
printf 'version: 1\nworker_name: h\nstate_dir: /custom/st#1\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: t\n' > "$_wd/workers.yaml"
write_worker_yaml "$_wd/worker.yaml" https://e.example mql_wkr_e host1 no HEADLESS >/dev/null 2>&1
if grep -qx 'state_dir: /custom/st#1' "$_wd/workers.yaml"; then
    pass "write_worker_yaml preserves a state_dir with a # in it"
else
    fail "write_worker_yaml cut the state_dir at #: $(grep state_dir "$_wd/workers.yaml")"
fi

# yaml_top_scalar: a `#` is a comment only after whitespace; quotes come off.
_ys="${_tmp}/review-yaml-scalar"
mkdir -p "$_ys"
printf 'state_dir: ~/.memql/state#1\n' > "$_ys/hash.yaml"
printf 'state_dir: ~/.memql/state # note\n' > "$_ys/comment.yaml"
printf 'state_dir: "~/.memql/st ate"\n' > "$_ys/quoted.yaml"
printf 'state_dir_other: /x\nlog_level: info\n' > "$_ys/absent.yaml"
printf 'state_dir: # only a comment\n' > "$_ys/onlycomment.yaml"
# shellcheck disable=SC2088  # the literal ~ IS the value under test; the reader does not expand it
expect_eq "yaml_top_scalar keeps an unspaced #" "$(yaml_top_scalar "$_ys/hash.yaml" state_dir)" "~/.memql/state#1"
# shellcheck disable=SC2088
expect_eq "yaml_top_scalar drops a spaced # comment" "$(yaml_top_scalar "$_ys/comment.yaml" state_dir)" "~/.memql/state"
# shellcheck disable=SC2088
expect_eq "yaml_top_scalar strips quotes" "$(yaml_top_scalar "$_ys/quoted.yaml" state_dir)" "~/.memql/st ate"
expect_eq "yaml_top_scalar ignores a longer key" "$(yaml_top_scalar "$_ys/absent.yaml" state_dir)" ""
expect_eq "yaml_top_scalar reads a comment-only value as empty" "$(yaml_top_scalar "$_ys/onlycomment.yaml" state_dir)" ""
printf 'state_dir: ~/.memql/state#1\n' > "$_ys/worker.yaml"
expect_eq "state root keeps a # in the directory name" "$(worker_state_dir_from_yaml "$_ys/worker.yaml")" "${HOME}/.memql/state#1"

# [8] The flag's shape is judged in the shell, once.
for _good in https://api.example.com HTTPS://api.example.com/ "https://api.example.com/path" " https://c.example "; do
    if require_cluster_url_flag "$_good" 2>/dev/null; then pass "require_cluster_url_flag accepts '${_good}'"; else fail "require_cluster_url_flag should accept '${_good}'"; fi
done
for _bad in api.memql.localhost "https://c.example?x=1" "https://u@c.example" "https://c.example#f" "ftp://c.example" ""; do
    require_cluster_url_flag "$_bad" >/dev/null 2>&1
    expect_eq "require_cluster_url_flag refuses '${_bad}' with 2" "$?" "2"
done

for _platform in mac linux; do
    _un="uninstall-${_platform}.sh"
    case "$_platform" in
        mac)   _svc="Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist" ;;
        linux) _svc=".config/systemd/user/${SERVICE_LABEL_LINUX}.service" ;;
    esac
    _state="${_tmp}/review-agent-state-${_platform}"
    mkdir -p "$_state"

    # [1] state_dir spelled as ~/.memql with a trailing slash (or a double
    # one, or the absolute form): --purge must judge it as ~/.memql and
    # keep it, credentials and backups included. Dry run first.
    # shellcheck disable=SC2088  # the literal ~ IS the spelling under test; the script expands it
    for _spelling in '~/.memql/' '~/.memql//' 'ABS/.memql/'; do
        _h="$(fixture_home "purge-root-${_spelling//[^a-z]/_}" "$_platform")"
        plant_protected "$_h"
        set_state_dir "$_h" "${_spelling/ABS/$_h}"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
        _rc=$?
        expect_eq "$_un --purge --dry-run with state_dir ${_spelling} exits 0" "$_rc" "0"
        if [[ "$_out" == *"would keep:    ${_h}/.memql (unsafe purge target)"* ]]; then
            pass "$_un --purge --dry-run with state_dir ${_spelling} plans to keep ~/.memql"
        else
            fail "$_un --purge --dry-run with state_dir ${_spelling}; got: $_out"
        fi
        expect_eq "$_un --purge --dry-run with state_dir ${_spelling} leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag)"
        _rc=$?
        expect_eq "$_un --purge with state_dir ${_spelling} exits 0" "$_rc" "0"
        if protected_intact "$_h" && [[ -d "${_h}/.memql" && "$_out" == *"unsafe purge target"* ]]; then
            pass "$_un --purge with state_dir ${_spelling} keeps ~/.memql, credentials, backups, certs and clusters.yaml"
        else
            fail "$_un --purge with state_dir ${_spelling} took protected data; got: $_out $(ls -laR "${_h}/.memql" 2>&1)"
        fi
    done

    # [2] A symlinked state_dir written with a trailing slash: the link
    # goes, what it points at is not touched. Dry run first.
    _h="$(fixture_home purge-link "$_platform")"
    plant_protected "$_h"
    _outside_state="${_tmp}/review-outside-state-${_platform}"
    mkdir -p "$_outside_state"
    printf 'keep me\n' > "${_outside_state}/keepme"
    rm -rf "${_h}/.memql/state"
    ln -s "$_outside_state" "${_h}/.memql/state"
    # shellcheck disable=SC2088  # the literal ~ IS the spelling under test
    set_state_dir "$_h" '~/.memql/state/'
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge --dry-run with a symlinked state_dir/ exits 0" "$_rc" "0"
    if [[ "$_out" == *"would remove:  ${_h}/.memql/state (the symlink only"* ]]; then
        pass "$_un --purge --dry-run with a symlinked state_dir/ plans the link only"
    else
        fail "$_un --purge --dry-run with a symlinked state_dir/; got: $_out"
    fi
    expect_eq "$_un --purge --dry-run with a symlinked state_dir/ leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge with a symlinked state_dir/ exits 0" "$_rc" "0"
    if [[ -f "${_outside_state}/keepme" && ! -L "${_h}/.memql/state" && ! -e "${_h}/.memql/state" ]] && protected_intact "$_h"; then
        pass "$_un --purge with a symlinked state_dir/ removes the link and leaves its target alone"
    else
        fail "$_un --purge with a symlinked state_dir/ followed the link or kept it; got: $_out $(ls -la "$_outside_state" 2>&1)"
    fi

    # [3] A case variant of a protected directory is protected (APFS is
    # case-insensitive by default; elsewhere this over-keeps, safely).
    _h="$(fixture_home purge-case "$_platform")"
    plant_protected "$_h"
    mkdir -p "${_h}/.memql/Credentials"
    printf 'marker\n' > "${_h}/.memql/Credentials/marker"
    # shellcheck disable=SC2088  # the literal ~ IS the spelling under test
    set_state_dir "$_h" '~/.memql/Credentials'
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge --dry-run with state_dir ~/.memql/Credentials exits 0" "$_rc" "0"
    if [[ "$_out" == *"would keep:    ${_h}/.memql/Credentials (protected data)"* ]]; then
        pass "$_un --purge --dry-run judges ~/.memql/Credentials as protected"
    else
        fail "$_un --purge --dry-run with ~/.memql/Credentials; got: $_out"
    fi
    expect_eq "$_un --purge --dry-run with ~/.memql/Credentials leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge with state_dir ~/.memql/Credentials exits 0" "$_rc" "0"
    if protected_intact "$_h" && [[ "$_out" == *"protected data"* ]]; then
        pass "$_un --purge with state_dir ~/.memql/Credentials keeps the credentials"
    else
        fail "$_un --purge with state_dir ~/.memql/Credentials took the credentials; got: $_out"
    fi

    # [X3] An installer-written machine: --purge removes the whole state
    # root (worker.log included) and an emptied ~/.memql, and the plan
    # names the root rather than one home's subdirectory.
    _h="$(fixture_home purge-root-of-home "$_platform")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
    if [[ "$_out" == *"would remove:  ${_h}/.memql/state (recursively)"* && "$_out" != *"homes/c.example"* ]]; then
        pass "$_un --purge --dry-run plans the state root, not the per-home dir"
    else
        fail "$_un --purge --dry-run should plan the state root; got: $_out"
    fi
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge on an installer-written machine exits 0" "$_rc" "0"
    if [[ ! -e "${_h}/.memql/state" && ! -e "${_h}/.memql" ]]; then
        pass "$_un --purge on an installer-written machine removes the state root and ~/.memql"
    else
        fail "$_un --purge left the state root or ~/.memql: $(ls -laR "${_h}/.memql" 2>&1)"
    fi

    # [4] The real unpair fails (or answers garbage) after a good preview:
    # the summary reads FAILED, names the enrollment files and the
    # stopped worker with its restart command, and never says nothing
    # was changed.
    for _mode in real-fail real-garbage; do
        _h="$(fixture_home "unpair-${_mode}" "$_platform")"
        write_memql_stub "${_h}/.memql/bin/${_pf_headless}" 0.15.2 "$_mode"
        : > "${_state}/${SERVICE_LABEL_DARWIN}"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        _rc=$?
        expect_eq "$_un ${_mode} after a good preview exits 5" "$_rc" "5"
        case "$_platform" in
            mac)   _restart="launchctl bootstrap gui/$(id -u) ${_h}/${_svc}" ;;
            linux) _restart="systemctl --user start ${SERVICE_LABEL_LINUX}.service" ;;
        esac
        if [[ "$_out" == *"FAILED:"* && "$_out" == *"did not complete"* && "$_out" == *"$_restart"* \
            && "$_out" != *"nothing was changed"* ]]; then
            pass "$_un ${_mode} reports FAILED with the files and the restart command"
        else
            fail "$_un ${_mode} summary; got: $_out"
        fi
        if [[ "$_platform" == mac && ! -e "${_state}/${SERVICE_LABEL_DARWIN}" ]]; then
            pass "$_un ${_mode}: the worker really was stopped, as the summary says"
        fi
    done

    # [5] / [X] A system shape, no sudo, one enrollment: refused BEFORE the
    # enrollment is unpaired and the agent booted out (mac); on linux the
    # unpair proceeds and the system binary is named as kept (PARTIAL).
    _sysbin="${_tmp}/review-sysbin-${_platform}"
    mkdir -p "$_sysbin"
    write_memql_stub "${_sysbin}/${_pf_headless}" 0.15.2
    ln -sf "${_sysbin}/${_pf_headless}" "${_sysbin}/${INSTALLED_COMMAND}"
    _h="$(fixture_home sys-nosudo "$_platform")"
    rm -f "${_h}/.memql/bin/${_pf_headless}" "${_h}/.memql/bin/${INSTALLED_COMMAND}"
    : > "${_state}/${SERVICE_LABEL_DARWIN}"
    _before="$(tree_fingerprint "$_h")"
    _out="$(run_with_system_shape "$_state" "$_un" "$_h" "$_sysbin")"
    _rc=$?
    expect_eq "$_un system shape + no sudo + one enrollment exits 4" "$_rc" "4"
    if [[ "$_out" == *"needs sudo"* ]]; then
        pass "$_un system shape says sudo is needed before asking"
    else
        fail "$_un system shape should announce sudo; got: $_out"
    fi
    case "$_platform" in
        mac)
            expect_eq "$_un system shape + no sudo leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
            if [[ "$_out" == *"REFUSED:"* && -e "${_state}/${SERVICE_LABEL_DARWIN}" ]]; then
                pass "$_un system shape + no sudo refuses with the agent still loaded"
            else
                fail "$_un system shape + no sudo unpaired or stopped before refusing; got: $_out"
            fi
            ;;
        linux)
            if [[ "$_out" == *"PARTIAL:"* && -x "${_sysbin}/${INSTALLED_COMMAND}" && "$_out" == *"Delete these by hand"* && ! -e "${_h}/.memql/workers.yaml" ]]; then
                pass "$_un system shape + no sudo keeps the system binary, names it, and still removes the tokens"
            else
                fail "$_un system shape + no sudo; got: $_out"
            fi
            ;;
    esac

    # [7] --user-local with a sibling and only a system memql: the binary
    # is found wherever it is; the user-local scope narrows what is
    # removed, not what may be asked.
    _h="$(fixture_home sys-userlocal "$_platform")"
    add_second_home "$_h"
    rm -f "${_h}/.memql/bin/${_pf_headless}" "${_h}/.memql/bin/${INSTALLED_COMMAND}"
    : > "${_state}/${SERVICE_LABEL_DARWIN}"
    _before="$(tree_fingerprint "$_h")"
    _out="$(run_with_system_shape "$_state" "$_un" "$_h" "$_sysbin" --user-local --cluster=https://c.example --dry-run)"
    _rc=$?
    expect_eq "$_un --user-local --cluster --dry-run with only a system memql exits 0" "$_rc" "0"
    if [[ "$_out" == *"would unpair:  https://c.example via ${_sysbin}/${INSTALLED_COMMAND}"* ]]; then
        pass "$_un --user-local finds the system memql for the unpair"
    else
        fail "$_un --user-local should use the system memql; got: $_out"
    fi
    expect_eq "$_un --user-local --cluster --dry-run leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    _out="$(run_with_system_shape "$_state" "$_un" "$_h" "$_sysbin" --user-local --cluster=https://c.example)"
    _rc=$?
    expect_eq "$_un --user-local --cluster with only a system memql exits 0" "$_rc" "0"
    if grep -q 'id: d.example' "${_h}/.memql/workers.yaml" && ! grep -q 'id: c.example' "${_h}/.memql/workers.yaml" \
        && [[ -x "${_sysbin}/${INSTALLED_COMMAND}" && -e "${_h}/${_svc}" ]]; then
        pass "$_un --user-local --cluster removes the home through the system memql and touches nothing system-owned"
    else
        fail "$_un --user-local --cluster with only a system memql; got: $_out $(cat "${_h}/.memql/workers.yaml" 2>&1)"
    fi

    # [6] A step that fails exits 5: an app at the standard path that is
    # not ours, or not a bundle at all (mac; the linux cases are the
    # tightened unit assertions above).
    if [[ "$_platform" == mac ]]; then
        _h="$(fixture_home foreign-app mac)"
        mkdir -p "${_h}/Applications/MemQL.app/Contents"
        printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.other</string></dict></plist>\n' \
            > "${_h}/Applications/MemQL.app/Contents/Info.plist"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un an app that is not ours at the standard path exits 5" "$_rc" "5"
        if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"leaving unrelated app"* && -d "${_h}/Applications/MemQL.app" ]]; then
            pass "$_un leaves the foreign app, names it, and says PARTIAL"
        else
            fail "$_un foreign app; got: $_out"
        fi
        _h="$(fixture_home file-app mac)"
        mkdir -p "${_h}/Applications"
        printf 'not a bundle\n' > "${_h}/Applications/MemQL.app"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un a plain file at the app path exits 5" "$_rc" "5"
    fi

    # [8] A --cluster value the binary would reject is a bad parameter here.
    _h="$(fixture_home bad-url "$_platform")"
    _before="$(tree_fingerprint "$_h")"
    for _bad in "--cluster=api.memql.localhost" "--cluster=https://c.example?x=1" "--cluster=https://u@c.example"; do
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" "$_bad" $_auto_flag)"
        _rc=$?
        expect_eq "$_un ${_bad} exits 2" "$_rc" "2"
        if [[ "$_out" == *"--cluster wants the cluster's URL"* ]]; then
            pass "$_un ${_bad} names the shape it wants"
        else
            fail "$_un ${_bad}; got: $_out"
        fi
    done
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --cluster api.memql.localhost $_auto_flag)"
    expect_eq "$_un --cluster api.memql.localhost (space form) exits 2" "$?" "2"
    expect_eq "$_un bad --cluster values change nothing" "$(tree_fingerprint "$_h")" "$_before"

    # [9] Two enrollments in each registry spelling: refused, nothing touched.
    for _shape in column0 comment urlfirst; do
        _h="$(fixture_home "shape-${_shape}" "$_platform")"
        cp "$_rs/${_shape}.yaml" "${_h}/.memql/workers.yaml"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un no flags + two enrollments (${_shape} spelling) exits 2" "$_rc" "2"
        if [[ "$_out" == *"--cluster=https://c.example"* && "$_out" == *"--cluster=https://d.example"* ]]; then
            pass "$_un ${_shape} spelling lists both clusters"
        else
            fail "$_un ${_shape} spelling; got: $_out"
        fi
        expect_eq "$_un ${_shape} spelling changes nothing" "$(tree_fingerprint "$_h")" "$_before"
    done

    # ---- second review pass ----

    # Flow-mapping registry, end to end: refused (5) with every token
    # still on disk, where the pre-fix scripts removed them all.
    for _shape in flow flowmulti; do
        _h="$(fixture_home "shape-${_shape}" "$_platform")"
        cp "$_rs/${_shape}.yaml" "${_h}/.memql/workers.yaml"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un no flags + a ${_shape} registry exits 5" "$_rc" "5"
        if [[ "$_out" == *"not an enrollment registry this script can read"* && -f "${_h}/.memql/workers.yaml" ]]; then
            pass "$_un ${_shape} registry is refused with the tokens still on disk"
        else
            fail "$_un ${_shape} registry; got: $_out"
        fi
        expect_eq "$_un ${_shape} registry changes nothing" "$(tree_fingerprint "$_h")" "$_before"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        expect_eq "$_un --cluster + a ${_shape} registry exits 5" "$?" "5"
        expect_eq "$_un --cluster + a ${_shape} registry changes nothing" "$(tree_fingerprint "$_h")" "$_before"
    done

    # A sibling-remaining unpair whose worker will not come back: the
    # enrollment is gone and the worker is down, so PARTIAL with the
    # restart command -- never REFUSED / "nothing was changed".
    _h="$(fixture_home restart-fail "$_platform")"
    add_second_home "$_h"
    : > "${_state}/${SERVICE_LABEL_DARWIN}"
    # shellcheck disable=SC2086  # _auto_flag is empty or one flag
    case "$_platform" in
        mac)   _out="$(MEMQL_TEST_BOOTSTRAP_FAIL=1 run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example $_auto_flag)" ;;
        linux) _out="$(MEMQL_TEST_START_FAIL=1 run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example $_auto_flag)" ;;
    esac
    _rc=$?
    expect_eq "$_un sibling unpair + failed worker restart exits 5" "$_rc" "5"
    case "$_platform" in
        mac)   _restart="launchctl bootstrap gui/$(id -u) ${_h}/${_svc}" ;;
        linux) _restart="systemctl --user start ${SERVICE_LABEL_LINUX}.service" ;;
    esac
    if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"the https://c.example enrollment"* && "$_out" == *"$_restart"* \
        && "$_out" != *"nothing was changed"* && "$_out" != *"REFUSED"* ]]; then
        pass "$_un failed restart reports PARTIAL with the removed enrollment and the restart command"
    else
        fail "$_un failed restart summary; got: $_out"
    fi
    if grep -q 'id: d.example' "${_h}/.memql/workers.yaml" && ! grep -q 'id: c.example' "${_h}/.memql/workers.yaml"; then
        pass "$_un failed restart: the registry really was rewritten, as the summary says"
    else
        fail "$_un failed restart registry: $(cat "${_h}/.memql/workers.yaml" 2>&1)"
    fi
    if [[ "$_platform" == mac ]]; then
        # ...and a legacy agent that will not unload after the unpair.
        _h="$(fixture_home legacy-stubborn mac)"
        add_second_home "$_h"
        : > "${_state}/${SERVICE_LABEL_DARWIN}"
        : > "${_state}/${LEGACY_LABEL_DARWIN}"
        # shellcheck disable=SC2086
        _out="$(MEMQL_TEST_STUBBORN="$LEGACY_LABEL_DARWIN" run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        _rc=$?
        expect_eq "$_un sibling unpair + stubborn legacy agent exits 5" "$_rc" "5"
        if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"the https://c.example enrollment"* \
            && "$_out" == *"launchctl bootout gui/$(id -u)/${LEGACY_LABEL_DARWIN}"* && "$_out" != *"nothing was changed"* ]]; then
            pass "$_un stubborn legacy agent reports PARTIAL naming the agent to stop by hand"
        else
            fail "$_un stubborn legacy agent summary; got: $_out"
        fi
        rm -f "${_state:?}/${LEGACY_LABEL_DARWIN}"

        # --dry-run makes the pre-flight refusal the real run makes: a
        # loaded worker with no regular plist and a sibling remaining is
        # 3 either way, with nothing touched.
        _h="$(fixture_home noplist-dry mac)"
        add_second_home "$_h"
        rm -f "${_h:?}/${_svc}"
        : > "${_state}/${SERVICE_LABEL_DARWIN}"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example --dry-run $_auto_flag)"
        _rc=$?
        expect_eq "$_un --dry-run with a loaded worker and no plist refuses with 3" "$_rc" "3"
        if [[ "$_out" == *"no regular service plist"* && "$_out" == *"REFUSED"* ]]; then
            pass "$_un --dry-run names the missing plist"
        else
            fail "$_un --dry-run with no plist; got: $_out"
        fi
        expect_eq "$_un --dry-run with no plist leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        expect_eq "$_un the real run with a loaded worker and no plist refuses with 3 too" "$?" "3"
        expect_eq "$_un the real run with no plist leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
        if [[ -e "${_state}/${SERVICE_LABEL_DARWIN}" ]]; then
            pass "$_un no-plist refusal leaves the worker loaded"
        else
            fail "$_un no-plist refusal stopped the worker"
        fi
        rm -f "${_state:?}/${SERVICE_LABEL_DARWIN}"
        # With the plist in place, the dry run says it would reload the worker.
        _h="$(fixture_home reload-dry mac)"
        add_second_home "$_h"
        : > "${_state}/${SERVICE_LABEL_DARWIN}"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller_agent "$_state" "$_un" "$_h" --cluster=https://c.example --dry-run $_auto_flag)"
        _rc=$?
        if [[ "$_rc" == 0 && "$_out" == *"would reload:  ${SERVICE_LABEL_DARWIN}"* ]]; then
            pass "$_un --dry-run with a loaded worker plans the reload"
        else
            fail "$_un --dry-run reload line (rc=$_rc); got: $_out"
        fi
        rm -f "${_state:?}/${SERVICE_LABEL_DARWIN}"
    fi

    # A failing rm under --purge is a leftover, not an abort: the rest of
    # the purge runs, the summary prints, the exit is 5.
    if [[ $EUID -eq 0 ]]; then
        echo "INFO: running as root; skipping the locked-directory purge check"
    else
        _h="$(fixture_home purge-locked "$_platform")"
        mkdir -p "${_h}/.memql/state/locked"
        : > "${_h}/.memql/state/locked/f"
        chmod 555 "${_h}/.memql/state/locked"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag 2>/dev/null)"
        _rc=$?
        chmod 755 "${_h}/.memql/state/locked"
        expect_eq "$_un --purge over an undeletable entry exits 5" "$_rc" "5"
        if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"could not be fully removed"* && "$_out" == *"Kept:"* \
            && ! -e "${_h}/.memql/policy.yaml" && ! -e "${_h}/.memql/workers.yaml" && -e "${_h}/.memql/state/locked/f" ]]; then
            pass "$_un --purge over an undeletable entry finishes, prints the summary and names the leftover"
        else
            fail "$_un --purge over an undeletable entry; got: $_out $(ls -laR "${_h}/.memql" 2>&1)"
        fi
    fi

    # HOME spelled with a trailing slash: the fence is the same directory.
    _h="$(fixture_home home-slash "$_platform")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "${_h}/" --all-homes --purge $_auto_flag)"
    _rc=$?
    expect_eq "$_un with HOME ending in a slash exits 0" "$_rc" "0"
    if [[ ! -e "${_h}/.memql/state" && "$_out" != *"outside"* ]]; then
        pass "$_un with HOME ending in a slash purges the state dir inside the fence"
    else
        fail "$_un with HOME ending in a slash; got: $_out"
    fi

    # A state_dir that names a regular FILE inside the fence is kept.
    _h="$(fixture_home purge-file "$_platform")"
    plant_protected "$_h"
    set_state_dir "$_h" "${_h}/.memql/clusters.yaml"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge --dry-run $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge --dry-run with a file as state_dir exits 0" "$_rc" "0"
    if [[ "$_out" == *"would keep:    ${_h}/.memql/clusters.yaml (not a directory)"* ]]; then
        pass "$_un --purge --dry-run judges a file state_dir as not a directory"
    else
        fail "$_un --purge --dry-run with a file state_dir; got: $_out"
    fi
    expect_eq "$_un --purge --dry-run with a file state_dir leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller "$_un" "$_h" --purge $_auto_flag)"
    _rc=$?
    expect_eq "$_un --purge with a file as state_dir exits 0" "$_rc" "0"
    if protected_intact "$_h" && [[ "$_out" == *"not a directory"* ]]; then
        pass "$_un --purge with a file as state_dir keeps the file"
    else
        fail "$_un --purge with a file as state_dir; got: $_out"
    fi

    # A user-local app whose rm -rf fails is kept and named, exit 5 (the
    # bundle identifier needs PlistBuddy, so macOS only, and only where
    # no real /Applications/MemQL.app would block the permission reset).
    if [[ "$_platform" == mac && "$(uname -s)" == Darwin && ! -e /Applications/MemQL.app && $EUID -ne 0 ]]; then
        _tcc="${_tmp}/review-tccutil"
        mkdir -p "$_tcc"
        printf '#!/usr/bin/env bash\nexit 0\n' > "${_tcc}/tccutil"
        chmod +x "${_tcc}/tccutil"
        _h="$(fixture_home app-locked mac)"
        mkdir -p "${_h}/Applications/MemQL.app/Contents/MacOS" "${_h}/Applications/MemQL.app/Contents/locked"
        printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.znasllc.memql-worker</string></dict></plist>\n' \
            > "${_h}/Applications/MemQL.app/Contents/Info.plist"
        : > "${_h}/Applications/MemQL.app/Contents/locked/f"
        chmod 555 "${_h}/Applications/MemQL.app/Contents/locked"
        _out="$(cd "$_script_dir" && HOME="$_h" PATH="${_tcc}:${_review_launchctl_dir}:$_nobin" MEMQL_TEST_AGENT_STATE="$_state" bash ./uninstall-mac.sh --verbose --all-homes --user-local 2>&1)"
        _rc=$?
        chmod 755 "${_h}/Applications/MemQL.app/Contents/locked"
        expect_eq "$_un a user-local app that cannot be removed exits 5" "$_rc" "5"
        if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"MemQL.app (could not be fully removed"* ]]; then
            pass "$_un a user-local app that cannot be removed is kept and named"
        else
            fail "$_un app rm failure; got: $_out"
        fi
    fi

    # [10] The space form never swallows the next flag or an empty word.
    _h="$(fixture_home cluster-swallow "$_platform")"
    rm -f "${_h}/.memql/workers.yaml" "${_h}/.memql/worker.yaml"
    _before="$(tree_fingerprint "$_h")"
    for _shape in "--cluster --dry-run" "--cluster --purge"; do
        # shellcheck disable=SC2086  # the shape is a flag list on purpose
        _out="$(run_uninstaller "$_un" "$_h" $_shape)"
        _rc=$?
        expect_eq "$_un '${_shape}' exits 2" "$_rc" "2"
        if [[ "$_out" == *"--cluster needs a URL"* ]]; then
            pass "$_un '${_shape}' says what --cluster needs"
        else
            fail "$_un '${_shape}'; got: $_out"
        fi
    done
    _out="$(run_uninstaller "$_un" "$_h" --cluster "")"
    expect_eq "$_un --cluster '' exits 2" "$?" "2"
    expect_eq "$_un a swallowed --cluster changes nothing" "$(tree_fingerprint "$_h")" "$_before"
done

# ---------------------------------------------------------------
# Installers -- the --version pin
# ---------------------------------------------------------------
#
# MEMQL_INSTALL_RELEASE_BASE points the composition at a file:// tree
# laid out like the releases page, so the pinned URL is asserted from
# the preflight's own "checking release asset" line and the download
# attempt that follows it -- offline, and exactly the URL a real run
# would fetch. The run still fails later, by design (download_binary
# refuses file://); what is under test is the URL.

_rel="${_tmp}/releases"
printf '%s\n' '[{"tag_name":"v99.0.0","draft":false,"prerelease":false},{"tag_name":"cockpit/v9.0.0","draft":false,"prerelease":true},{"tag_name":"cockpit/v0.16.0","draft":false,"prerelease":false}]' > "${_tmp}/releases.json"
export MEMQL_INSTALL_RELEASE_API="file://${_tmp}/releases.json"
mkdir -p "${_rel}/download/cockpit/v0.16.0" "${_rel}/latest/download"
printf 'stub-binary' > "${_rel}/download/cockpit/v0.16.0/${_pf_headless}"
printf 'stub-binary' > "${_rel}/latest/download/${_pf_headless}"

for _installer in install-mac.sh install-linux.sh; do
    for _spelling in "--version=v0.16.0" "--version=0.16.0" "--version 0.16.0"; do
        _vh="${_tmp}/ver-home-${_installer}-${_spelling//[^a-z0-9]/_}"
        mkdir -p "$_vh"
        # shellcheck disable=SC2086  # the spelling may be two words
        _out="$(cd "$_script_dir" && HOME="$_vh" MEMQL_INSTALL_RELEASE_BASE="file://${_rel}" "./${_installer}" \
            --token mql_wkr_test --cluster https://c.example --user-local --no-service --verbose $_spelling 2>&1)"
        _rc=$?
        if [[ "$_rc" != 4 && "$_out" == *"INFO: checking release asset file://${_rel}/download/cockpit/v0.16.0/${_pf_headless}"* \
            && "$_out" == *"INFO: downloading file://${_rel}/download/cockpit/v0.16.0/${_pf_headless}"* ]]; then
            pass "$_installer ${_spelling} composes the pinned download base"
        else
            fail "$_installer ${_spelling} should download from download/cockpit/v0.16.0; rc=$_rc got: $_out"
        fi
        if [[ "$_out" == *"v0.16.0"* && "$_out" == *"fresh install"* ]]; then
            pass "$_installer ${_spelling} names v0.16.0 as the target"
        else
            fail "$_installer ${_spelling} should name the pinned version; got: $_out"
        fi
    done

    # Without a pin the base is the latest release, composed from the same root.
    _vh="${_tmp}/ver-home-${_installer}-latest"
    mkdir -p "$_vh"
    _out="$(cd "$_script_dir" && HOME="$_vh" MEMQL_INSTALL_RELEASE_BASE="file://${_rel}" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --user-local --no-service --verbose 2>&1)"
    if [[ "$_out" == *"INFO: checking release asset file://${_rel}/download/cockpit/v0.16.0/${_pf_headless}"* ]]; then
        pass "$_installer without --version selects the latest published stable Cockpit release"
    else
        fail "$_installer default must select a stable Cockpit tag; got: $_out"
    fi

    # --download-base wins for the location when both are given.
    _vh="${_tmp}/ver-home-${_installer}-both"
    mkdir -p "$_vh"
    _out="$(cd "$_script_dir" && HOME="$_vh" MEMQL_INSTALL_RELEASE_BASE="file://${_rel}" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --user-local --no-service --verbose \
        --version=v0.16.0 --download-base "file://${_pf_assets}" 2>&1)"
    if [[ "$_out" == *"INFO: checking release asset file://${_pf_assets}/${_pf_headless}"* ]]; then
        pass "$_installer --download-base wins over --version for the location"
    else
        fail "$_installer --download-base should win; got: $_out"
    fi

    # A malformed pin is a bad parameter (2), before any preflight.
    _vh="${_tmp}/ver-home-${_installer}-bad"
    mkdir -p "$_vh"
    _out="$(cd "$_script_dir" && HOME="$_vh" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --user-local --no-service --verbose --version=banana 2>&1)"
    _rc=$?
    expect_eq "$_installer --version=banana exits 2" "$_rc" "2"
    if [[ "$_out" == *"ERROR: --version wants"* && "$_out" != *"checking release asset"* ]]; then
        pass "$_installer --version=banana is refused before the preflight"
    else
        fail "$_installer --version=banana; got: $_out"
    fi
    _out="$(cd "$_script_dir" && HOME="$_vh" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --user-local --no-service --verbose --version 2>&1)"
    expect_eq "$_installer --version without a value exits 2" "$?" "2"
    if [[ "$_out" == *"--version"* ]]; then
        pass "$_installer documents --version in its help"
    else
        fail "$_installer help should document --version"
    fi
done

# ---------------------------------------------------------------
# Third pass -- a registry the text readers cannot see into
# ---------------------------------------------------------------
#
# Every reader engaged only on a literal `^homes:` at column 0, so a
# workers.yaml the binary's yaml.v3 decodes as two homes but spelled
# any other way read as ZERO: the no-flag and --cluster uninstalls took
# the --all-homes path and deleted every token under exit 0, and the
# next install rewrote the registry holding only its own home. Each
# shape below was decoded with gopkg.in/yaml.v3 into the WorkersFile
# struct (internal/worker/homes.go) as the two homes c.example and
# d.example. All four readers must now refuse (5), and a file with
# provably no homes key must still read as zero.

_ur_items=$'- id: c.example\n  cluster_url: https://c.example\n  token: mql_wkr_c\n- id: d.example\n  cluster_url: https://d.example\n  token: mql_wkr_d\n'

# utf16be prints $1 (ASCII) as UTF-16BE behind its byte-order mark,
# which yaml.v3 detects and decodes.
function utf16be() {
    local text="$1" i
    printf '\376\377'
    for (( i = 0; i < ${#text}; i++ )); do
        printf '\000%s' "${text:i:1}"
    done
}

# join_lines prints stdin's lines each followed by the byte string $1
# instead of a newline; awk's -v turns its octal escapes into bytes.
function join_lines() {
    LC_ALL=C awk -v sep="$1" '{ printf "%s%s", $0, sep }'
}

# write_unreadable_registry writes registry shape $1 at $2.
function write_unreadable_registry() {
    local shape="$1" path="$2"
    case "$shape" in
        jsonline)
            printf '{"version": 1, "homes": [{"id": "c.example", "cluster_url": "https://c.example", "token": "mql_wkr_c"}, {"id": "d.example", "cluster_url": "https://d.example", "token": "mql_wkr_d"}]}\n' ;;
        jsonpretty)
            printf '{\n  "version": 1,\n  "homes": [\n    {\n      "id": "c.example",\n      "cluster_url": "https://c.example",\n      "token": "mql_wkr_c"\n    },\n    {\n      "id": "d.example",\n      "cluster_url": "https://d.example",\n      "token": "mql_wkr_d"\n    }\n  ]\n}\n' ;;
        flowroot)
            printf '{version: 1, homes: [{id: c.example, cluster_url: https://c.example, token: mql_wkr_c}, {id: d.example, cluster_url: https://d.example, token: mql_wkr_d}]}\n' ;;
        dquoted)     printf 'version: 1\n"homes":\n%s' "$_ur_items" ;;
        squoted)     printf "version: 1\n'homes':\n%s" "$_ur_items" ;;
        spacecolon)  printf 'version: 1\nhomes :\n%s' "$_ur_items" ;;
        tabcolon)    printf 'version: 1\nhomes\t:\n%s' "$_ur_items" ;;
        explicitkey) printf 'version: 1\n? homes\n:\n%s' "$_ur_items" ;;
        anchored)    printf 'version: 1\n&h homes:\n%s' "$_ur_items" ;;
        tagged)      printf 'version: 1\n!!str homes:\n%s' "$_ur_items" ;;
        escaped)     printf 'version: 1\n"hom\\x65s":\n%s' "$_ur_items" ;;
        contkey)
            printf 'version: 1\n? "hom\\\n  es"\n: - id: c.example\n    cluster_url: https://c.example\n  - id: d.example\n    cluster_url: https://d.example\n' ;;
        merge)
            printf 'version: 1\nbase: &b\n  "hom\\x65s":\n  - id: c.example\n    cluster_url: https://c.example\n  - id: d.example\n    cluster_url: https://d.example\n<<: *b\n' ;;
        docflow)
            printf -- '--- {"homes": [{"id": "c.example", "cluster_url": "https://c.example"}, {"id": "d.example", "cluster_url": "https://d.example"}]}\n' ;;
        indented)    printf 'version: 1\nhomes:\n%s' "$_ur_items" | sed 's/^/ /' ;;
        bom)         printf '\357\273\277homes:\n%s' "$_ur_items" ;;
        utf16)       utf16be "version: 1"$'\n'"homes:"$'\n'"$_ur_items" ;;
        cronly)      printf 'version: 1\nhomes:\n%s' "$_ur_items" | tr '\n' '\r' ;;
        nel)         printf 'version: 1\nhomes:\n%s' "$_ur_items" | join_lines '\302\205' ;;
        ls)          printf 'version: 1\nhomes:\n%s' "$_ur_items" | join_lines '\342\200\250' ;;
        ps)          printf 'version: 1\nhomes:\n%s' "$_ur_items" | join_lines '\342\200\251' ;;
        crinside)
            printf 'version: 1\nhomes:\n- id: c.example\n  cluster_url: https://c.example\r- id: d.example\r  cluster_url: https://d.example\n' ;;
        nelinside)
            printf 'version: 1\nhomes:\n- id: c.example\n  cluster_url: https://c.example\302\205- id: d.example\302\205  cluster_url: https://d.example\n' ;;
        # A block header the readers do understand, with VALUES they do
        # not: a tag the host dedup collapsed (both read as `!!str https:`,
        # one enrollment), and an alias the readers took literally.
        tagvalue)
            printf 'version: 1\nhomes:\n- id: c.example\n  cluster_url: !!str https://c.example\n- id: d.example\n  cluster_url: !!str https://d.example\n' ;;
        aliasvalue)
            printf 'version: 1\nurl: &u https://d.example\nhomes:\n- id: c.example\n  cluster_url: https://c.example\n- id: d.example\n  cluster_url: *u\n' ;;
        *) echo "unknown shape $shape" >&2; return 1 ;;
    esac > "$path"
}

_ur_shapes="jsonline jsonpretty flowroot dquoted squoted spacecolon tabcolon explicitkey anchored tagged escaped contkey merge docflow indented bom utf16 cronly nel ls ps crinside nelinside tagvalue aliasvalue"
_ur="${_tmp}/unreadable-registries"
mkdir -p "$_ur"
for _shape in $_ur_shapes; do
    write_unreadable_registry "$_shape" "$_ur/${_shape}.yaml"
    _sum="$(shasum -a 256 < "$_ur/${_shape}.yaml")"
    _out="$(list_enrolled_cluster_urls "$_ur/${_shape}.yaml" /nonexistent 2>&1 >/dev/null)"
    _rc=$?
    expect_eq "list_enrolled_cluster_urls refuses the ${_shape} registry with 5" "$_rc" "5"
    if [[ "$_out" == *"is not an enrollment registry this script can read; nothing was changed."* && "$_out" == *"--all-homes"* ]]; then
        pass "list_enrolled_cluster_urls ${_shape} refusal names --all-homes"
    else
        fail "list_enrolled_cluster_urls ${_shape} refusal; got: $_out"
    fi
    expect_eq "list_enrolled_cluster_urls prints no cluster for ${_shape}" \
        "$(list_enrolled_cluster_urls "$_ur/${_shape}.yaml" /nonexistent 2>/dev/null)" ""
    _out="$(find_home_id_by_cluster_url "$_ur/${_shape}.yaml" https://c.example 2>/dev/null)"
    _rc=$?
    expect_eq "find_home_id_by_cluster_url refuses the ${_shape} registry with 5" "$_rc:$_out" "5:"
    _out="$(find_home_cluster_url_by_id "$_ur/${_shape}.yaml" d.example 2>/dev/null)"
    _rc=$?
    expect_eq "find_home_cluster_url_by_id refuses the ${_shape} registry with 5" "$_rc:$_out" "5:"
    # The installer's writer: refuses, and neither file is written.
    _wd="${_tmp}/unreadable-writer-${_shape}"
    mkdir -p "$_wd"
    cp "$_ur/${_shape}.yaml" "$_wd/workers.yaml"
    _out="$(write_worker_yaml "$_wd/worker.yaml" https://e.example mql_wkr_e host1 no HEADLESS 2>&1)"
    _rc=$?
    expect_eq "write_worker_yaml refuses the ${_shape} registry with 5" "$_rc" "5"
    if [[ "$_out" == *"is not an enrollment registry this script can read"* && "$_out" == *"will not rewrite it"* ]]; then
        pass "write_worker_yaml ${_shape} refusal says it will not rewrite the registry"
    else
        fail "write_worker_yaml ${_shape} refusal; got: $_out"
    fi
    expect_eq "write_worker_yaml leaves the ${_shape} registry unchanged" \
        "$(shasum -a 256 < "$_wd/workers.yaml")" "$_sum"
    if [[ ! -e "$_wd/worker.yaml" ]]; then
        pass "write_worker_yaml writes no legacy mirror beside the ${_shape} registry"
    else
        fail "write_worker_yaml wrote worker.yaml beside the ${_shape} registry"
    fi
done
# yaml.v3 decodes the FIRST document only; a second one is refused
# rather than read as the registry.
printf -- '---\n---\nhomes:\n- id: c.example\n  cluster_url: https://c.example\n' > "$_ur/twodocs.yaml"
list_enrolled_cluster_urls "$_ur/twodocs.yaml" /nonexistent >/dev/null 2>&1
expect_eq "list_enrolled_cluster_urls refuses a second document with 5" "$?" "5"
# Deliberately conservative: with no `homes:` header, the word anywhere
# else is enough to refuse, even where yaml.v3 would find no homes key.
printf 'version: 1\nlabels:\n  homes: x\n' > "$_ur/nestedtoken.yaml"
list_enrolled_cluster_urls "$_ur/nestedtoken.yaml" /nonexistent >/dev/null 2>&1
expect_eq "list_enrolled_cluster_urls refuses the word homes outside a header with 5" "$?" "5"

# Controls: provably no homes key reads as zero (and the writer still
# writes); CRLF endings and a leading `---` still read their homes.
_urc="${_tmp}/readable-registries"
mkdir -p "$_urc"
: > "$_urc/emptyfile.yaml"
printf '# nothing here yet\n\n' > "$_urc/comments.yaml"
printf 'version: 1\nworker_name: host1\nlabels:\n  os: darwin\ncapabilities:\n- HEADLESS\n' > "$_urc/nohomeskey.yaml"
for _shape in emptyfile comments nohomeskey; do
    _out="$(list_enrolled_cluster_urls "$_urc/${_shape}.yaml" /nonexistent 2>&1)"
    _rc=$?
    expect_eq "list_enrolled_cluster_urls reads the ${_shape} registry as zero" "$_rc:$_out" "0:"
    _wd="${_tmp}/readable-writer-${_shape}"
    mkdir -p "$_wd"
    cp "$_urc/${_shape}.yaml" "$_wd/workers.yaml"
    if write_worker_yaml "$_wd/worker.yaml" https://e.example mql_wkr_e host1 no HEADLESS >/dev/null 2>&1 \
        && grep -q 'id: e.example' "$_wd/workers.yaml"; then
        pass "write_worker_yaml writes over the homes-free ${_shape} registry"
    else
        fail "write_worker_yaml over the ${_shape} registry: $(cat "$_wd/workers.yaml" 2>&1)"
    fi
done
printf 'version: 1\r\nhomes:\r\n- id: c.example\r\n  cluster_url: https://c.example\r\n- id: d.example\r\n  cluster_url: https://d.example\r\n' > "$_urc/crlf.yaml"
printf -- '---\nversion: 1\nhomes:\n%s' "$_ur_items" > "$_urc/docstart.yaml"
printf 'version: 1\nstate_dir: /x/homes/y\nhomes:\n%s' "$_ur_items" > "$_urc/tokenwithheader.yaml"
for _shape in crlf docstart tokenwithheader; do
    expect_eq "list_enrolled_cluster_urls still reads the ${_shape} registry" \
        "$(list_enrolled_cluster_urls "$_urc/${_shape}.yaml" /nonexistent 2>&1 | tr -d '\r' | tr '\n' ' ')" "https://c.example https://d.example "
    expect_eq "find_home_cluster_url_by_id still reads the ${_shape} registry" \
        "$(find_home_cluster_url_by_id "$_urc/${_shape}.yaml" d.example 2>&1 | tr -d '\r')" "https://d.example"
done
# Quoted values read as what they say, in the installer's readers as in
# the uninstaller's: a quoted cluster_url once matched nothing, and the
# next install appended a second home for the same cluster.
printf 'version: 1\nhomes:\n  - id: "local"\n    cluster_url: "https://c.example"\n    token: mql_wkr_old\n' > "$_urc/quoted.yaml"
expect_eq "find_home_id_by_cluster_url reads a quoted cluster_url" \
    "$(find_home_id_by_cluster_url "$_urc/quoted.yaml" https://c.example)" "local"
expect_eq "find_home_cluster_url_by_id reads a quoted id and cluster_url" \
    "$(find_home_cluster_url_by_id "$_urc/quoted.yaml" local)" "https://c.example"
_wd="${_tmp}/readable-writer-quoted"
mkdir -p "$_wd"
cp "$_urc/quoted.yaml" "$_wd/workers.yaml"
write_worker_yaml "$_wd/worker.yaml" https://c.example mql_wkr_new host1 no HEADLESS >/dev/null 2>&1
if [[ "$(grep -c -- '- id:' "$_wd/workers.yaml")" == 1 ]] && grep -q 'id: local' "$_wd/workers.yaml" \
    && grep -q 'token: mql_wkr_new' "$_wd/workers.yaml" && ! grep -q 'mql_wkr_old' "$_wd/workers.yaml"; then
    pass "write_worker_yaml refreshes the quoted home in place instead of appending a second"
else
    fail "write_worker_yaml over a quoted home: $(cat "$_wd/workers.yaml" 2>&1)"
fi

# End to end: every shape, both drivers -- the no-flag line MemQL OS
# shows, --cluster=URL and the no-flag --dry-run all refuse with 5 and
# leave HOME byte-identical (the pre-fix scripts removed every token).
for _platform in mac linux; do
    _un="uninstall-${_platform}.sh"
    for _shape in $_ur_shapes; do
        _h="$(fixture_home "unreadable-${_shape}" "$_platform")"
        add_second_home "$_h"
        write_unreadable_registry "$_shape" "${_h}/.memql/workers.yaml"
        _before="$(tree_fingerprint "$_h")"
        # shellcheck disable=SC2086  # _auto_flag is empty or one flag
        _out="$(run_uninstaller "$_un" "$_h" $_auto_flag)"
        _rc=$?
        expect_eq "$_un no flags + a ${_shape} registry exits 5" "$_rc" "5"
        if [[ "$_out" == *"not an enrollment registry this script can read; nothing was changed."* \
            && "$_out" == *"--all-homes"* ]]; then
            pass "$_un no flags + a ${_shape} registry is refused naming --all-homes"
        else
            fail "$_un no flags + a ${_shape} registry; got: $_out"
        fi
        expect_eq "$_un no flags + a ${_shape} registry leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        _rc=$?
        expect_eq "$_un --cluster + a ${_shape} registry exits 5" "$_rc" "5"
        if [[ "$_out" == *"not an enrollment registry this script can read"* && "$_out" == *"REFUSED"* ]]; then
            pass "$_un --cluster + a ${_shape} registry ends REFUSED"
        else
            fail "$_un --cluster + a ${_shape} registry; got: $_out"
        fi
        expect_eq "$_un --cluster + a ${_shape} registry leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller "$_un" "$_h" --dry-run $_auto_flag)"
        expect_eq "$_un --dry-run + a ${_shape} registry exits 5" "$?" "5"
        if [[ "$_out" == *"not an enrollment registry this script can read"* && "$_out" != *"would remove"* ]]; then
            pass "$_un --dry-run + a ${_shape} registry plans nothing"
        else
            fail "$_un --dry-run + a ${_shape} registry; got: $_out"
        fi
        expect_eq "$_un --dry-run + a ${_shape} registry leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    done
    # The exact piped shape MemQL OS's line runs.
    _h="$(fixture_home unreadable-piped "$_platform")"
    write_unreadable_registry jsonpretty "${_h}/.memql/workers.yaml"
    _before="$(tree_fingerprint "$_h")"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller_piped "$_un" "$_h" $_auto_flag)"
    expect_eq "$_un piped + a JSON registry exits 5" "$?" "5"
    expect_eq "$_un piped + a JSON registry leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
    # shellcheck disable=SC2086
    _out="$(run_uninstaller_piped "$_un" "$_h" --dry-run $_auto_flag)"
    expect_eq "$_un piped --dry-run + a JSON registry exits 5" "$?" "5"
    expect_eq "$_un piped --dry-run + a JSON registry leaves HOME byte-identical" "$(tree_fingerprint "$_h")" "$_before"
done

# The installers refuse BEFORE the download, with worker state unchanged
# (a diagnostic log is still written): a registry they cannot read is one they would rewrite
# holding only the new home.
for _installer in install-mac.sh install-linux.sh; do
    _ih="${_tmp}/unreadable-install-${_installer}"
    mkdir -p "${_ih}/.memql"
    write_unreadable_registry jsonline "${_ih}/.memql/workers.yaml"
    chmod 600 "${_ih}/.memql/workers.yaml"
    _before="$(tree_fingerprint "$_ih/.memql")"
    _out="$(cd "$_script_dir" && HOME="$_ih" "./${_installer}" \
        --token mql_wkr_test --cluster https://e.example \
        --user-local --no-service --verbose \
        --download-base "file://${_pf_assets}" 2>&1)"
    _rc=$?
    expect_eq "$_installer over an unreadable registry exits 5" "$_rc" "5"
    if [[ "$_out" == *"not an enrollment registry this script can read; nothing was changed."* \
        && "$_out" != *"INFO: downloading"* ]]; then
        pass "$_installer refuses an unreadable registry before the download"
    else
        fail "$_installer over an unreadable registry; got: $_out"
    fi
    expect_eq "$_installer over an unreadable registry leaves worker state byte-identical" "$(tree_fingerprint "$_ih/.memql")" "$_before"
done

# A legacy service file that will not unlink after a sibling-remaining
# unpair: the enrollment is gone and the worker is back up for the
# sibling, but the file is a leftover -- Kept with its remedy, exit 5,
# the PARTIAL summary rather than the SUCCESS line.
if [[ $EUID -eq 0 ]]; then
    echo "INFO: running as root; skipping the locked legacy service file check"
else
    for _platform in mac linux; do
        _un="uninstall-${_platform}.sh"
        _lstate="${_tmp}/legacy-locked-state-${_platform}"
        mkdir -p "$_lstate"
        _h="$(fixture_home "legacy-locked" "$_platform")"
        add_second_home "$_h"
        case "$_platform" in
            mac)
                _ldir="${_h}/Library/LaunchAgents"
                _lfile="${_ldir}/${LEGACY_LABEL_DARWIN}.plist"
                : > "${_lstate}/${SERVICE_LABEL_DARWIN}"
                ;;
            linux)
                _ldir="${_h}/.config/systemd/user"
                _lfile="${_ldir}/${LEGACY_LABEL_LINUX}.service"
                ;;
        esac
        printf 'legacy\n' > "$_lfile"
        chmod 555 "$_ldir"
        # shellcheck disable=SC2086
        _out="$(run_uninstaller_agent "$_lstate" "$_un" "$_h" --cluster=https://c.example $_auto_flag)"
        _rc=$?
        chmod 755 "$_ldir"
        expect_eq "$_un sibling unpair + a legacy service file that will not unlink exits 5" "$_rc" "5"
        if [[ "$_out" == *"PARTIAL:"* && "$_out" == *"the https://c.example enrollment"* \
            && "$_out" == *"${_lfile} (could not be removed; delete it by hand)"* \
            && "$_out" != *"SUCCESS: selected cluster enrollment removed"* ]]; then
            pass "$_un a legacy service file that will not unlink is Kept with its remedy in the PARTIAL summary"
        else
            fail "$_un locked legacy service file; got: $_out"
        fi
        if [[ -e "$_lfile" ]] && grep -q 'id: d.example' "${_h}/.memql/workers.yaml" \
            && ! grep -q 'id: c.example' "${_h}/.memql/workers.yaml"; then
            pass "$_un locked legacy service file: the enrollment really was removed, the file really is still there"
        else
            fail "$_un locked legacy service file state: $(cat "${_h}/.memql/workers.yaml" 2>&1)"
        fi
        if [[ "$_platform" == mac ]]; then
            if [[ -e "${_lstate}/${SERVICE_LABEL_DARWIN}" ]]; then
                pass "$_un locked legacy plist: the worker for the sibling was reloaded"
            else
                fail "$_un locked legacy plist: the worker for the sibling was not reloaded"
            fi
        fi
    done
fi

# Build identity must not turn a selected release into a mismatch, or make
# two different prereleases (or explicitly selected commits) interchangeable.
for _version_case in '0.16.0+gabc|0.16.0|yes' '0.16.0-dev.ci+gabc|0.16.0-dev.ci|yes' '0.16.0-dev.old+gabc|0.16.0-dev.ci|no' '0.16.0+gabc|0.16.0+gdef|no' '0.16.0+gabc|0.16.0+gabc|yes' '0.15.0+gabc|0.16.0|no'; do
    IFS='|' read -r _actual _selected _expected <<< "$_version_case"
    _matched=no
    binary_version_matches_selection "$_actual" "$_selected" && _matched=yes
    expect_eq "build selection $_actual -> $_selected" "$_matched" "$_expected"
done

# Run the actual binary install against executable release fixtures. A release
# number alone must not preserve an older build before the native app check.
function binary_install_fixture() (
    set -e
    local mode="$1" before="$2" downloaded="$3" target="$4" selected="$5" expected="$6" action="$7" status="$8"
    local fixture rc=0
    fixture="$(mktemp -d)"
    trap 'rm -rf "$fixture"' EXIT
    function install_mode_dir() { printf '%s/bin\n' "$fixture"; }
    function resolve_target_version() { printf '%s\n' "$target"; }
    function require_sudo() { :; }
    function install_sudo() { "$@"; }
    function download_binary() {
        printf '#!/bin/bash\nprintf "memql %%s (computeruse)\\n" %s\n' "$downloaded" > "$2"
        chmod +x "$2"
    }
    # The release's checksum manifest is the integrity tests' subject
    # (memql-cockpit#525); these fixtures are about versions.
    function verify_release_binary() { :; }
    mkdir -p "$fixture/bin"
    printf '#!/bin/bash\nprintf "memql %%s (computeruse)\\n" %s\n' "$before" > "$fixture/bin/worker"
    chmod +x "$fixture/bin/worker"
    ln -s "$fixture/bin/worker" "$fixture/bin/memql"
    install_binary_with_mode "$mode" fixture://release worker memql "$target" "$selected" > "$fixture/install.log" 2>&1 || rc=$?
    if [[ "$rc" != "$status" || "$INSTALL_BINARY_ACTION" != "$action" || "$(read_binary_version_exact "$fixture/bin/memql")" != "$expected" ]]; then
        cat "$fixture/install.log" >&2
        printf 'got status=%s action=%s version=%s; wanted %s %s %s\n' "$rc" "$INSTALL_BINARY_ACTION" "$(read_binary_version_exact "$fixture/bin/memql")" "$status" "$action" "$expected" >&2
        return 1
    fi
)

for _mode in user-local system; do
    while IFS='|' read -r _case _before _downloaded _target _selected _expected _action _status; do
        if binary_install_fixture "$_mode" "$_before" "$_downloaded" "$_target" "$_selected" "$_expected" "$_action" "$_status"; then
            pass "$_mode binary install: $_case"
        else
            fail "$_mode binary install: $_case"
        fi
    done <<'CASES'
latest replaces another commit|0.19.0+gold|0.19.0+gnew|||0.19.0+gnew|upgrade|0
latest replaces an unstamped build|0.19.0|0.19.0+gnew|||0.19.0+gnew|upgrade|0
latest replaces a prerelease|0.19.0-dev.before+gold|0.19.0+gnew|||0.19.0+gnew|upgrade|0
pinned release replaces another commit|0.19.0+gold|0.19.0+gnew|0.19.0|0.19.0|0.19.0+gnew|upgrade|0
pinned prerelease replaces another commit|0.19.0-dev.ci+gold|0.19.0-dev.ci+gnew|0.19.0|0.19.0-dev.ci|0.19.0-dev.ci+gnew|upgrade|0
resolved release verifies the build|0.19.0+gold|0.19.0+gnew|0.19.0||0.19.0+gnew|upgrade|0
identical build stays installed|0.19.0+gnew|0.19.0+gnew|||0.19.0+gnew|skip|0
pinned identical build stays installed|0.19.0+gnew|0.19.0+gnew|0.19.0|0.19.0|0.19.0+gnew|skip|0
latest refuses a downgrade|0.20.0+gold|0.19.0+gnew|||0.20.0+gold|refuse|3
pinned release refuses a downgrade|0.20.0+gold|0.19.0+gnew|0.19.0|0.19.0|0.20.0+gold|refuse|3
CASES
done

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------

# ---------------------------------------------------------------
# Integrity and network (memql-cockpit#525)
# ---------------------------------------------------------------
#
# The downloaded binary is verified against its release's manifest BEFORE
# anything runs it; every fetch carries retries and bounds; and a script
# whose lib.sh lacks a function it calls refuses up front.

_iv="${_tmp}/integrity"
mkdir -p "${_iv}/release"
printf '#!/bin/sh\necho "memql 0.20.0 (computeruse)"\n' > "${_iv}/cu-binary"
printf '#!/bin/sh\necho "memql 0.19.0 (headless)"\n' > "${_iv}/hl-binary"
printf '%s  memql-computeruse-darwin-arm64\n' "$(sha256_of "${_iv}/cu-binary")" > "${_iv}/release/memql-computeruse-SHA256SUMS"
# The headless archive of a release published before memql-SHA256SUMS.
mkdir -p "${_iv}/archive-src"
cp "${_iv}/hl-binary" "${_iv}/archive-src/memql"
(cd "${_iv}/archive-src" && tar -czf "${_iv}/release/memql-0.19.0-darwin-arm64.tar.gz" ./memql)
printf '%s  memql-0.19.0-darwin-arm64.tar.gz\n' "$(sha256_of "${_iv}/release/memql-0.19.0-darwin-arm64.tar.gz")" > "${_iv}/release/memql-0.19.0-SHA256SUMS"

# The release served from disk: fetch_release_file and download_binary are
# the two network seams verify_release_binary uses.
function _iv_run() {
    IV_RELEASE="${_iv}/release" bash -c '
        source "$1/lib.sh"
        function fetch_release_file() { local name="${1##*/}"; [[ -f "$IV_RELEASE/$name" ]] || return 22; cp "$IV_RELEASE/$name" "$2"; }
        function download_binary() { cp "$IV_RELEASE/${1##*/}" "$2"; }
        function resolve_latest_version() { printf "0.19.0"; }
        shift
        verify_release_binary "$@"
    ' _ "$_script_dir" "$@"
}

_out="$(_iv_run "${_iv}/cu-binary" memql-computeruse-darwin-arm64 "https://example.invalid/releases/download/v0.20.0" 2>&1)"; _rc=$?
if [[ "$_rc" == 0 && "$_out" == *"verified memql-computeruse-darwin-arm64 against memql-computeruse-SHA256SUMS"* ]]; then
    pass "verify_release_binary accepts a binary its manifest lists"
else
    fail "verify_release_binary should accept a listed binary (rc=$_rc): $_out"
fi
printf '#!/bin/sh\necho tampered\n' > "${_iv}/tampered"
_out="$(_iv_run "${_iv}/tampered" memql-computeruse-darwin-arm64 "https://example.invalid/releases/download/v0.20.0" 2>&1)"; _rc=$?
if [[ "$_rc" == 5 && "$_out" == *"does not match its release checksum"* && "$_out" == *"Nothing was run or installed"* ]]; then
    pass "verify_release_binary refuses a binary that does not match, before running it"
else
    fail "verify_release_binary should refuse a mismatch with 5 (rc=$_rc): $_out"
fi
mv "${_iv}/release/memql-computeruse-SHA256SUMS" "${_iv}/release/hidden"
_out="$(_iv_run "${_iv}/cu-binary" memql-computeruse-darwin-arm64 "https://example.invalid/releases/download/v0.20.0" 2>&1)"; _rc=$?
mv "${_iv}/release/hidden" "${_iv}/release/memql-computeruse-SHA256SUMS"
if [[ "$_rc" == 5 && "$_out" == *"does not vouch for memql-computeruse-darwin-arm64"* ]]; then
    pass "verify_release_binary refuses a computer-use binary no manifest vouches for"
else
    fail "verify_release_binary should refuse an unvouched binary (rc=$_rc): $_out"
fi
_out="$(_iv_run "${_iv}/hl-binary" memql-darwin-arm64 "https://example.invalid/releases/latest/download" 2>&1)"; _rc=$?
if [[ "$_rc" == 0 && "$_out" == *"verified memql-darwin-arm64 against memql-0.19.0-darwin-arm64.tar.gz"* ]]; then
    pass "a headless binary from a release without memql-SHA256SUMS is verified through its release archive"
else
    fail "headless archive verification should pass (rc=$_rc): $_out"
fi
_out="$(_iv_run "${_iv}/tampered" memql-darwin-arm64 "https://example.invalid/releases/download/v0.19.0" 2>&1)"; _rc=$?
if [[ "$_rc" == 5 && "$_out" == *"is not the memql in its verified release archive"* ]]; then
    pass "a headless binary that differs from its verified archive is refused"
else
    fail "a tampered headless binary should be refused (rc=$_rc): $_out"
fi

# The order is the property: verified, then executed. In both install modes
# the verification sits between the download and the first version probe.
for _iv_mode in system user-local; do
    _iv_block="$(awk '/^function install_binary_with_mode\(\)/ { f = 1 } f && /^}/ { exit } f { print }' "${_script_dir}/lib.sh" \
        | awk -v mode="        ${_iv_mode})" 'index($0, mode) == 1 { on = 1 } on { print } on && /;;$/ { exit }')"
    # shellcheck disable=SC2016  # literal source text, not expansions
    _iv_dl="$(grep -n 'download_binary "\$url" "\$tmp"' <<< "$_iv_block" | head -1 | cut -d: -f1)"
    # shellcheck disable=SC2016
    _iv_vf="$(grep -n 'verify_release_binary "\$tmp"' <<< "$_iv_block" | head -1 | cut -d: -f1)"
    # shellcheck disable=SC2016
    _iv_ex="$(grep -n 'read_binary_version_exact "\$tmp"' <<< "$_iv_block" | head -1 | cut -d: -f1)"
    if [[ -n "$_iv_dl" && -n "$_iv_vf" && -n "$_iv_ex" && "$_iv_dl" -lt "$_iv_vf" && "$_iv_vf" -lt "$_iv_ex" ]]; then
        pass "install_binary_with_mode ${_iv_mode} verifies the download before running it"
    else
        fail "install_binary_with_mode ${_iv_mode} must verify between download and first run (dl=$_iv_dl verify=$_iv_vf run=$_iv_ex)"
    fi
done

# Every curl in the installers retries and is bounded.
_iv_bad="$(cd "$_script_dir" || exit 1
    grep -nE '(^|[^_[:alnum:]-])curl ("\$|-)' lib.sh install-mac.sh install-linux.sh uninstall-mac.sh uninstall-linux.sh \
        | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' | grep -vE '(printf|echo) ' | grep -vE 'CURL_NET|--retry')"
if [[ -z "$_iv_bad" ]]; then
    pass "every curl in the installers retries and is bounded"
else
    fail "curl calls without retries/bounds:\n$_iv_bad"
fi

# REQUIRED_LIB_FUNCTIONS is exactly the lib.sh functions each script calls.
_iv_libfns="$(sed -n -E 's/^function ([A-Za-z_][A-Za-z0-9_]*)\(\).*/\1/p' "${_script_dir}/lib.sh" | sort -u)"
[[ -n "$_iv_libfns" ]] || fail "could not read lib.sh's functions"
for _iv_name in install-mac.sh install-linux.sh uninstall-mac.sh uninstall-linux.sh; do
    _iv_script="${_script_dir}/${_iv_name}"
    _iv_own="$(sed -n -E 's/^function ([A-Za-z_][A-Za-z0-9_]*)\(\).*/\1/p' "$_iv_script" | sort -u)"
    _iv_body="$(sed -E 's/(^|[[:space:]])#.*$//' "$_iv_script" | grep -v '^readonly REQUIRED_LIB_FUNCTIONS=')"
    _iv_used=""
    while IFS= read -r _iv_fn; do
        [[ -n "$_iv_fn" ]] || continue
        grep -qxF "$_iv_fn" <<< "$_iv_own" && continue
        grep -qE "(^|[^[:alnum:]_-])${_iv_fn}([^[:alnum:]_-]|$)" <<< "$_iv_body" && _iv_used="${_iv_used} ${_iv_fn}"
    done <<< "$_iv_libfns"
    _iv_declared="$(sed -n -E 's/^readonly REQUIRED_LIB_FUNCTIONS="(.*)"$/\1/p' "$_iv_script")"
    if [[ "$(tr ' ' '\n' <<< "$_iv_used" | grep . | sort)" == "$(tr ' ' '\n' <<< "$_iv_declared" | grep . | sort)" ]]; then
        pass "$_iv_name declares exactly the lib.sh functions it calls"
    else
        fail "$_iv_name REQUIRED_LIB_FUNCTIONS is stale; it calls:${_iv_used}"
    fi
done

# A piped run whose lib.sh is older than the script refuses up front, and
# changes nothing, instead of stopping with exit 127 halfway through.
mkdir -p "${_iv}/oldraw" "${_iv}/cwd"
grep -v '^function setup_inference()' "${_script_dir}/lib.sh" > "${_iv}/oldraw/lib.sh"
_out="$(cd "${_iv}/cwd" && MEMQL_INSTALL_RAW_BASE="file://${_iv}/oldraw" bash -s -- --help < "${_script_dir}/install-mac.sh" 2>&1)"; _rc=$?
if [[ "$_rc" == 4 && "$_out" == *"lib.sh lacks: setup_inference"* && "$_out" == *"Nothing was changed"* ]]; then
    pass "a piped installer with a stale lib.sh refuses before running, naming what is missing"
else
    fail "a stale lib.sh should be refused with 4 (rc=$_rc): $_out"
fi
_out="$(cd "${_iv}/cwd" && MEMQL_INSTALL_RAW_BASE="file://${_script_dir}" bash -s -- --help < "${_script_dir}/install-mac.sh" 2>&1)"; _rc=$?
expect_eq "a piped installer with a matching lib.sh still works" "$_rc" "0"

echo ""
echo "===================="
echo "PASS: $PASS"
echo "FAIL: $FAIL"
echo "===================="

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
exit 0
