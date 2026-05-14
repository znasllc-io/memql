#!/usr/bin/env bash
# scripts/identity/build-assets.sh
# Identity-binary asset pipeline.
#
# Phase 3 ships hand-written CSS + a vendored Alpine.js CSP build +
# inline heroicons SVG, all committed under
# component/identity/web/static/. This script is a no-op pass-through
# in that regime — the convention is in place so a future Tailwind
# pipeline can plug in here without restructuring the Makefile.
#
# When a Tailwind upgrade lands:
#  1. Download the standalone Tailwind CLI into bin/tools/.
#  2. Run it against component/identity/web/templates/*.html plus a
#     scripts/identity/tailwind.config.js to produce
#     component/identity/web/static/app.css.
#  3. Add app.css to .gitignore.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

main() {
  log_info "asset pipeline (no-op pass-through; assets are committed verbatim under component/identity/web/static/)"
  ls component/identity/web/static/ 2>/dev/null | sed 's/^/  /' >&2 || true
}

main "$@"
