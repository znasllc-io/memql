#!/usr/bin/env bash
set -euo pipefail

# Run one real editor host in an isolated pipeline checkout. Dependencies and
# browser downloads stay in that checkout on native fleet machines; no sudo,
# machine-wide installation, host profile or signed-in editor is used.
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
    node -p 'JSON.stringify({platform:process.platform,arch:process.arch,node:process.version})' > "$report/runtime.json"

    bash scripts/ci/retry.sh -- bash scripts/vscode/deps.sh
    bash scripts/ci/retry.sh -- npm ci --no-audit --no-fund --prefix editors/vscode
    if [[ "$lane" == vscode-desktop ]]; then
        bash scripts/identity/build-css.sh
        node_platform="$(node -p 'process.platform + "-" + process.arch')"
        mkdir -p "editors/vscode/bin/$node_platform"
        CGO_ENABLED=0 go build -o "editors/vscode/bin/$node_platform/memql-lsp" ./cmd/memql-lsp
        bash scripts/vscode/host-test.sh --skip-deps
    else
        bash scripts/ci/retry.sh -- npm ci --no-audit --no-fund --prefix editors/productivity
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
