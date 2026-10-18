#!/usr/bin/env bash
set -euo pipefail

# Preserve the existing site's complete browser checks in an isolated native
# step. Browser downloads belong to this checkout; no host profile or service
# is used. Retry dependency transport only, never a build or test assertion.
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
site="$PWD/editors/vscode/site"
mkdir -p "$site/artifacts"
function preserve_result() {
    local status=$?
    trap - EXIT
    printf '%s\n' "$status" > "$site/artifacts/exit-code.txt"
    exit "$status"
}
trap preserve_result EXIT
export PLAYWRIGHT_BROWSERS_PATH="$PWD/.memql-site-browsers"
PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 bash scripts/ci/retry.sh -- \
    npm ci --foreground-scripts --no-audit --no-fund --prefix "$site"
npm --prefix "$site" run build
npm --prefix "$site" run check
(
    cd "$site"
    bash ../../../scripts/ci/retry.sh -- npx --no-install playwright install chromium
)
npm --prefix "$site" run test:browser
test -s "$site/dist/index.html"
