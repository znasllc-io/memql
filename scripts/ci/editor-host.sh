#!/usr/bin/env bash
set -euo pipefail

# Run one real editor host in an isolated pipeline checkout. Dependencies and
# browser downloads stay in that checkout on native fleet machines; no sudo,
# machine-wide installation, host profile or signed-in editor is used.
function prepare_mac_node() {
    [[ "$(uname -m)" == arm64 ]] || { echo "The native editor host requires darwin/arm64" >&2; return 2; }
    # Match the toolchain's Node 22 major without relying on Homebrew or
    # replacing the owner's Node. The official archive's checksum is pinned.
    local archive=node-v22.23.3-darwin-arm64.tar.gz
    local checksum=23b25245dcfb9af7262f8ff142e9e2e0af025368117329e7a7458a51e5922f53
    local destination="$PWD/.memql-host-cache/node"
    mkdir -p "$destination"
    (
        cd "$destination"
        curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
            --connect-timeout 15 --max-time 300 --retry 3 \
            "https://nodejs.org/dist/v22.23.3/$archive" --output "$archive"
        printf '%s  %s\n' "$checksum" "$archive" | shasum -a 256 -c -
        tar -xzf "$archive" --strip-components=1
        rm -- "$archive"
    )
    export PATH="$destination/bin:$PATH"
}

function preserve_result() {
    local status=$?
    trap - EXIT
    printf '%s\n' "$status" > "$report/exit-code.txt"
    if [[ -d editors/productivity/dist-test/host-logs ]]; then
        cp -R editors/productivity/dist-test/host-logs "$report/productivity-logs" || true
    fi
    exit "$status"
}

function main() {
    lane="${1:?expected vscode-desktop, productivity-desktop or productivity-web}"
    version="${2:?expected stable or 1.91.0}"
    case "$lane:$version" in
        vscode-desktop:stable|vscode-desktop:1.91.0|productivity-desktop:stable|productivity-desktop:1.91.0|productivity-web:stable) ;;
        *) echo "Unsupported editor host selection" >&2; exit 2 ;;
    esac

    cd "$(dirname "${BASH_SOURCE[0]}")/../.."
    export GOTOOLCHAIN=go1.27.2 GOFLAGS=-p=2 GOMAXPROCS=2 GOMEMLIMIT=1GiB
    export MEMQL_VSCODE_VERSION="$version"
    export PLAYWRIGHT_BROWSERS_PATH="$PWD/.memql-host-cache/browsers"
    if [[ "$(uname -s)" == Darwin ]]; then
        export GOPATH="$PWD/.memql-host-cache/go"
        export GOCACHE="$PWD/.memql-host-cache/go-build"
        export npm_config_cache="$PWD/.memql-host-cache/npm"
        mkdir -p "$GOPATH" "$GOCACHE" "$npm_config_cache"
    fi

    report="$PWD/.memql-host-reports"
    mkdir -p "$report"
    printf '%s\n' "$lane" "$version" > "$report/selection.txt"
    trap preserve_result EXIT
    if [[ "$(uname -s)" == Darwin ]]; then
        prepare_mac_node
    fi
    node -p 'JSON.stringify({platform:process.platform,arch:process.arch,node:process.version})' > "$report/runtime.json"

    bash scripts/ci/retry.sh -- bash scripts/vscode/deps.sh
    # @playwright/browser-chromium downloads a browser during npm install.
    # Desktop cases use Electron; the web case installs Chromium explicitly
    # below. Keep downloads out of npm's hidden lifecycle scripts, and keep
    # this setting scoped to installation so the web host still gets tested.
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 bash scripts/ci/retry.sh -- npm ci --foreground-scripts --no-audit --no-fund --prefix editors/vscode
    if [[ "$lane" == vscode-desktop ]]; then
        bash scripts/identity/build-css.sh
        node_platform="$(node -p 'process.platform + "-" + process.arch')"
        mkdir -p "editors/vscode/bin/$node_platform"
        CGO_ENABLED=0 go build -o "editors/vscode/bin/$node_platform/memql-lsp" ./cmd/memql-lsp
        bash scripts/vscode/host-test.sh --skip-deps
    else
        PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 bash scripts/ci/retry.sh -- npm ci --foreground-scripts --no-audit --no-fund --prefix editors/productivity
        npm --prefix editors/vscode run compile
        npm --prefix editors/productivity run compile
        npm --prefix editors/productivity test
        if [[ "$lane" == productivity-web ]]; then
            (cd editors/vscode && bash ../../scripts/ci/retry.sh -- npx --no-install playwright install chromium)
            node editors/productivity/test/runner.cjs
            if [[ "$(uname -s)" == Linux ]]; then
                npm --prefix editors/productivity run package
            fi
        elif [[ "$(uname -s)" == Linux ]]; then
            xvfb-run -a node editors/productivity/test/runner.cjs --desktop
        else
            node editors/productivity/test/runner.cjs --desktop
        fi
    fi
}

main "$@"
