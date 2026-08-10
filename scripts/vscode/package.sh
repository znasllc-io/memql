#!/usr/bin/env bash
set -euo pipefail

# Script: package.sh
# Purpose: Build the memql-lsp binary for a target platform, stage it into the
#          VS Code extension, compile the extension, and produce a .vsix.
#
# The offline LSP embeds the engine, so the binary is bundled per platform and
# the extension resolves it at bin/<node-platform>-<node-arch>/memql-lsp -- where
# node-platform / node-arch are Node's `process.platform` / `process.arch` values
# (darwin|linux|win32 / x64|arm64|ia32), NOT Go's GOOS/GOARCH (darwin|linux|windows
# / amd64|arm64|386). We BUILD with Go but STAGE under the Node-named directory the
# extension actually looks in, so producer and consumer always agree. (They only
# happen to coincide for arm64-on-darwin, which is why a Mac-only default silently
# broke every amd64 / windows host.)
#
# Defaults to the HOST platform (via `go env`), so `make vscode-install` bundles a
# binary that runs on THIS machine. Cross-build with --goos / --goarch.
#
# It also stages scripts/ -- the install graph documents and the capability
# scripts the graph runs -- into editors/vscode/staged/ (memql#3487). Same reason
# as the binary: a .vsix contains only files from under the extension directory,
# so an installed extension has no copy of anything that lives at the repository
# root, and SessionOptions.root had no value that could be correct in one.
# src/install/root.ts is the consuming half.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"
# Default to the host platform. Fall back to the standardized dev target
# (darwin/arm64) if `go` is unavailable -- check_prerequisites reports that
# clearly a moment later.
DEFAULT_GOOS="$(go env GOHOSTOS 2>/dev/null || echo darwin)"
DEFAULT_GOARCH="$(go env GOHOSTARCH 2>/dev/null || echo arm64)"
VSCE_VERSION="@vscode/vsce@latest"
# Must match STAGED_ROOT_DIR in editors/vscode/src/install/root.ts, which is what
# reads this tree back at run time.
STAGED_DIR_NAME="staged"
# Everything under the staged tree lands here inside the archive; vsce roots a
# VSIX at extension/.
VSIX_PREFIX="extension"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--goos=OS] [--goarch=ARCH] [--out=FILE] [--skip-deps]

Options:
    --goos=OS      Target OS for the bundled binary (default: host -- $DEFAULT_GOOS)
    --goarch=ARCH  Target arch (default: host -- $DEFAULT_GOARCH)
    --out=FILE     VSIX output path (default: editors/vscode/<name>-<version>.vsix)
    --skip-deps    Skip rebuilding the file: workspace dependencies (inner-loop
                   reruns where sdk/ts and sdk/ts-viewkit have not changed)
    --help         Show this help

Produces a .vsix bundling the memql-lsp binary under the Node-named directory
(bin/<node-platform>-<node-arch>/) the extension resolves at runtime.
EOF
}

function parse_arguments() {
    GOOS_TARGET="$DEFAULT_GOOS"
    GOARCH_TARGET="$DEFAULT_GOARCH"
    OUT=""
    SKIP_DEPS=false
    while [[ $# -gt 0 ]]; do
        case $1 in
            --goos=*) GOOS_TARGET="${1#*=}"; shift ;;
            --goarch=*) GOARCH_TARGET="${1#*=}"; shift ;;
            --out=*) OUT="${1#*=}"; shift ;;
            --skip-deps) SKIP_DEPS=true; shift ;;
            --help) show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    command -v go >/dev/null || { echo "ERROR: go is not installed"; exit 1; }
    command -v npm >/dev/null || { echo "ERROR: npm is not installed"; exit 1; }
    # verify_vsix_executable_bits reads the produced archive back. Without unzip
    # that check would have to be skipped, and a packaging run that quietly drops
    # its only real assertion is worse than one that refuses to start.
    command -v unzip >/dev/null || { echo "ERROR: unzip is not installed (needed to verify the packaged scripts)"; exit 1; }
}

# node_platform maps a Go GOOS to Node's process.platform naming (what the
# extension's resolveServerPath uses to build the bundle directory name).
function node_platform() {
    case "$1" in
        windows) echo "win32" ;;
        *)       echo "$1"    ;; # darwin, linux pass through unchanged
    esac
}

# node_arch maps a Go GOARCH to Node's process.arch naming.
function node_arch() {
    case "$1" in
        amd64) echo "x64"  ;;
        386)   echo "ia32" ;;
        *)     echo "$1"   ;; # arm64 passes through unchanged
    esac
}

function build_binary() {
    local nodeos nodearch bindir binname
    nodeos="$(node_platform "$GOOS_TARGET")"
    nodearch="$(node_arch "$GOARCH_TARGET")"
    bindir="$EXT_DIR/bin/${nodeos}-${nodearch}"
    binname="memql-lsp"
    [[ "$GOOS_TARGET" == "windows" ]] && binname="memql-lsp.exe"
    echo "INFO: building memql-lsp (GOOS=${GOOS_TARGET} GOARCH=${GOARCH_TARGET}) -> bin/${nodeos}-${nodearch}/${binname}"
    mkdir -p "$bindir"
    ( cd "$REPO_ROOT" && GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" CGO_ENABLED=0 \
        go build -o "$bindir/$binname" ./cmd/memql-lsp )
}

# capability_script_relpaths prints the repo-relative path of every script the
# graph runner is allowed to spawn, READ OUT OF runner.ts.
#
# Derived rather than listed here on purpose. CAPABILITY_SCRIPTS is already the
# allowlist -- capabilityScriptPath refuses any id that is not in it -- so a
# second list in this file would be a second source of truth, and the way it
# would fail is the worst kind: a capability added to the allowlist, working from
# a checkout, and ENOENT-ing only for someone who installed the .vsix. The map's
# keys are ids like "k3d.up", so anchoring on the "scripts/..." VALUES cannot
# match one by accident.
function capability_script_relpaths() {
    grep -oE '"scripts/[^"]+\.sh"' "$EXT_DIR/src/install/runner.ts" | tr -d '"' | sort -u
}

# support_file_relpaths prints the repo-relative path of everything a capability
# script needs that is not itself a capability script.
#
# scripts/lib/*.sh: every one of those scripts SOURCES a shared library, and a
# staged script whose `source` line points at a file nobody copied dies at run
# time with a missing-file error that reads as a broken machine. The whole
# directory is taken rather than the sourced names, so there is no list to keep
# in step; verify_staged_sources then proves the result actually covers them.
#
# scripts/install/tool-pins.env: install-binary.sh defaults its `pins` parameter
# to the committed manifest beside it and cap_fails 4 when it is absent, so
# without this the very first tool step of an install refuses to run.
#
# scripts/install/graph/*.json: the documents graph.ts loads. The .go files
# beside them are the Go loader and its tests, and belong nowhere near a VSIX.
function support_file_relpaths() {
    ( cd "$REPO_ROOT" && printf '%s\n' \
        scripts/lib/*.sh \
        scripts/install/graph/*.json \
        scripts/install/tool-pins.env )
}

# stage_one copies one repo-relative file into the staged tree at the SAME
# relative path.
#
# The layout is reproduced exactly, never flattened: graph.ts and runner.ts both
# hard-code paths beneath the root they are handed, so `<staged>/scripts/install/
# graph/install.json` is not a convention here, it is the contract. -p carries the
# mode across, which is what keeps an executable script executable.
function stage_one() {
    local rel="$1" staged="$2" src dest
    src="$REPO_ROOT/$rel"
    dest="$staged/$rel"
    [[ -f "$src" ]] || { echo "ERROR: $rel is part of the staged set but does not exist at $src"; exit 1; }
    mkdir -p "$(dirname "$dest")"
    cp -p "$src" "$dest"
}

function stage_install_tree() {
    local staged="$EXT_DIR/$STAGED_DIR_NAME" rel count=0
    echo "INFO: staging the install graph + capability scripts -> $STAGED_DIR_NAME/scripts/"
    # Rebuilt from scratch every run. The staged tree is a BUILD ARTIFACT (it is
    # gitignored); a leftover file from a capability that was renamed or removed
    # would otherwise ship forever.
    rm -rf "$staged"
    while IFS= read -r rel; do
        stage_one "$rel" "$staged"
        # The runner spawns these DIRECTLY (spawn(scriptPath, argv, {shell:false}),
        # runner.ts) so that a param value containing `;` or `$(...)` stays an
        # inert string. Direct execution needs the executable bit, so set it
        # explicitly rather than inheriting whatever the checkout happened to
        # have -- the sourced libraries below are deliberately left alone.
        chmod +x "$staged/$rel"
        count=$((count + 1))
    done < <(capability_script_relpaths)
    [[ "$count" -gt 0 ]] || { echo "ERROR: no capability scripts found in src/install/runner.ts -- has CAPABILITY_SCRIPTS moved?"; exit 1; }
    while IFS= read -r rel; do
        stage_one "$rel" "$staged"
    done < <(support_file_relpaths)
    echo "INFO: staged $count capability scripts plus their libraries and graph documents"
}

# verify_staged_sources proves every shared library a staged script reaches for
# was actually staged.
#
# A missing `source` target is the failure this whole staging step is most likely
# to produce and the least likely to be noticed: it does not break packaging, it
# breaks the operator's first install with "No such file or directory" naming a
# path inside an extension directory. Matching on `lib/<name>.sh` catches both
# spellings in the tree -- the `${SCRIPT_DIR}/../lib/...` idiom and
# e2e-baseline.sh's `$(cd "$(dirname "${BASH_SOURCE[0]}")/..")` form -- so there
# is no shape of the convention it silently skips.
function verify_staged_sources() {
    local staged="$EXT_DIR/$STAGED_DIR_NAME" lib missing=0
    while IFS= read -r lib; do
        [[ -f "$staged/scripts/$lib" ]] && continue
        echo "ERROR: a staged script sources $lib, which was not staged"
        missing=$((missing + 1))
    done < <(grep -rhoE 'lib/[A-Za-z0-9_.-]+\.sh' "$staged/scripts" | sort -u)
    [[ "$missing" -eq 0 ]] || { echo "ERROR: $missing sourced library file(s) missing from the staged tree"; exit 1; }
    echo "INFO: every library the staged scripts source is present"
}

# THE EXTENSION CANNOT COMPILE WITHOUT THIS (memql#3340). Both
# @znasllc-io/memql-sdk-core and @znasllc-io/memql-view-kit are `file:`
# dependencies whose types come from a gitignored dist/. The `npm ci` below
# only creates the symlinks -- it does not build what they point at -- so
# without this step `tsc` cannot resolve either package and the compile dies
# with TS2307 plus a shower of downstream implicit-`any` errors.
#
# This lane shipped without it. `make vscode-install` and `make vscode-package`
# both run this script, so the entire developer-facing install path failed on
# any checkout that had not already built the SDKs for some other reason. CI hid
# it: the packaging step runs after a step that builds them, so it passed on the
# residue of an unrelated step rather than on its own merits.
function build_workspace_deps() {
    if [[ "$SKIP_DEPS" == true ]]; then
        echo "INFO: --skip-deps; not rebuilding the file: workspace dependencies"
        return
    fi
    bash "$REPO_ROOT/scripts/vscode/deps.sh"
}

function build_extension() {
    echo "INFO: compiling the extension"
    ( cd "$EXT_DIR" && npm ci --no-audit --no-fund && npm run compile )
}

function package_vsix() {
    echo "INFO: packaging the VSIX"
    # --no-dependencies: the extension is bundled (esbuild.js, run by `npm run
    # compile` above), so out/extension.js is already self-contained. Without
    # this flag vsce's own dependency-detection walks the `file:` workspace
    # dependencies (@znasllc-io/memql-sdk-core, @znasllc-io/memql-view-kit) by
    # following their node_modules symlinks OUT of editors/vscode entirely,
    # and absorbs sdk/ts's and sdk/ts-viewkit's own devDependencies
    # (typescript, @types/*) into the VSIX -- which isn't just bloat, it
    # actually fails packaging outright ("invalid relative path:
    # extension/../../sdk/ts/node_modules/..."), because vsce cannot express
    # a path that walks above the extension root inside the VSIX archive.
    local args=(package --no-dependencies)
    [[ -n "$OUT" ]] && args+=(--out "$OUT")
    ( cd "$EXT_DIR" && npx --yes "$VSCE_VERSION" "${args[@]}" )
}

# vsix_output_path prints where package_vsix put the archive. vsce resolves a
# relative --out against its cwd, which is EXT_DIR, and names the default after
# the manifest.
function vsix_output_path() {
    if [[ -n "$OUT" ]]; then
        case "$OUT" in
            /*) echo "$OUT" ;;
            *)  echo "$EXT_DIR/$OUT" ;;
        esac
        return
    fi
    local name version
    name="$(node -p "require('$EXT_DIR/package.json').name")"
    version="$(node -p "require('$EXT_DIR/package.json').version")"
    echo "$EXT_DIR/${name}-${version}.vsix"
}

# verify_vsix_executable_bits proves the staged scripts come back out of the
# ARCHIVE executable.
#
# THIS IS THE ONLY CHECK THAT TESTS THE PACKAGE RATHER THAN THE BUILD MACHINE.
# stage_install_tree chmods the staged files, so a stat on the working tree
# always passes and says nothing: what matters is whether the mode survives the
# zip round trip into somebody else's extensions directory. It might not have --
# a zip entry carries its unix mode in an optional external-attributes field that
# a writer is free to leave at zero -- and if it does not, every step of an
# install dies with EACCES, which reads as a broken machine rather than a broken
# package. The runner spawns these DIRECTLY and must keep doing so (shell:false
# is what makes a `;` in a param value inert, and installExecutor.test.ts asserts
# it), so the bit is load-bearing and gets asserted, not assumed.
#
# Extracting and testing -x is deliberate: it reproduces exactly what happens on
# an operator's machine. The zipinfo listing beside it is evidence for a human
# reading the log.
#
# SCOPED TO THE CAPABILITY SCRIPTS, not to every staged .sh. The shared libraries
# under scripts/lib/ are SOURCED -- bash reads them, nothing execs them -- and
# two of them are committed 0644, correctly. Asserting +x on those would be
# asserting a property that is not required and not true. Driving the loop off
# capability_script_relpaths also makes this check prove PRESENCE: a script in
# the allowlist that never reached the archive fails here rather than at an
# operator's first install.
function verify_vsix_executable_bits() {
    local vsix staged_prefix tmp listing rel count=0 failures=0
    vsix="$(vsix_output_path)"
    staged_prefix="$VSIX_PREFIX/$STAGED_DIR_NAME"
    [[ -f "$vsix" ]] || { echo "ERROR: expected a VSIX at $vsix, found none"; exit 1; }

    echo "INFO: verifying the staged capability scripts are executable inside $(basename "$vsix")"
    # Captured rather than piped straight into head: head closing the pipe early
    # would SIGPIPE unzip, and pipefail turns that into an aborted packaging run.
    listing="$(unzip -Z "$vsix" "$staged_prefix/scripts/install/*.sh" 2>/dev/null || true)"
    printf '%s\n' "$listing" | head -n 5
    echo "INFO: (first archive entries above; the leading field is the stored mode)"

    tmp="$(mktemp -d)"
    unzip -q "$vsix" "$staged_prefix/*" -d "$tmp"

    while IFS= read -r rel; do
        count=$((count + 1))
        local extracted="$tmp/$staged_prefix/$rel"
        if [[ ! -f "$extracted" ]]; then
            echo "ERROR: $rel is in CAPABILITY_SCRIPTS but absent from the VSIX"
            failures=$((failures + 1))
            continue
        fi
        [[ -x "$extracted" ]] && continue
        echo "ERROR: $rel is NOT executable after extraction (mode $(file_mode "$extracted"))"
        failures=$((failures + 1))
    done < <(capability_script_relpaths)

    rm -rf "$tmp"

    # A check that passes because it found nothing to check is worthless.
    [[ "$count" -gt 0 ]] || { echo "ERROR: no capability scripts to verify -- has CAPABILITY_SCRIPTS moved?"; exit 1; }
    [[ "$failures" -eq 0 ]] || { echo "ERROR: $failures capability script(s) missing or non-executable in the VSIX"; exit 1; }
    echo "SUCCESS: all $count capability scripts extract executable from the VSIX"
}

# file_mode prints a file's permission bits, on either stat dialect (GNU on the
# Linux build hosts, BSD on a developer's Mac). Used only to make a failure
# message specific.
function file_mode() {
    stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null || echo '?'
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    build_binary
    stage_install_tree
    verify_staged_sources
    build_workspace_deps
    build_extension
    package_vsix
    verify_vsix_executable_bits
    echo "SUCCESS: VSIX packaged"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
