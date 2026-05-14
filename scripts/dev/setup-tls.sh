#!/usr/bin/env bash
# Generate locally-trusted TLS certs for the dev cluster.
#
# Output: docker/nginx/certs/dev.{crt,key} -- one cert with a single
# wildcard SAN covering every subdomain of the dev cluster's domain
# (`*.${IDENTITY_BOOTSTRAP_DOMAIN}` -- e.g. `*.local.znas.io`).
# That single SAN covers app.<domain>, identity.<domain>,
# bff.<domain>, agent.<domain>, etc.
#
# Domain resolution order:
#   1. IDENTITY_BOOTSTRAP_DOMAIN env var (passed through from the
#      operator's shell)
#   2. ../.env.local (the standard dev secrets file)
#   3. fallback: local.znas.io (the maintainer default)
#
# Idempotent: safe to re-run after rotating mkcert's root CA or
# changing the domain; the existing cert file is overwritten in place.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CERT_DIR="$REPO_ROOT/docker/nginx/certs"
ENV_FILE="$REPO_ROOT/.env.local"

#=============================================================================
# FUNCTIONS
#=============================================================================

function require_mkcert() {
    if command -v mkcert >/dev/null 2>&1; then
        return 0
    fi
    cat <<'EOF' >&2
ERROR: mkcert is not installed.

mkcert generates locally-trusted TLS certificates and installs its
root CA into the system trust store, so browsers, Go clients, and
the cockpit-worker all verify the dev cluster's certs without
warnings.

Install:

  macOS (Homebrew):
    brew install mkcert nss

  Linux (Debian/Ubuntu):
    sudo apt install libnss3-tools
    # Then download the latest mkcert binary from
    # https://github.com/FiloSottile/mkcert/releases

  Linux (Arch):
    sudo pacman -S mkcert

After installation, re-run:  bash scripts/dev/setup-tls.sh
EOF
    exit 1
}

function resolve_domain() {
    if [[ -n "${IDENTITY_BOOTSTRAP_DOMAIN:-}" ]]; then
        DOMAIN="$IDENTITY_BOOTSTRAP_DOMAIN"
        return 0
    fi
    if [[ -f "$ENV_FILE" ]]; then
        local from_file
        from_file="$(grep -E '^IDENTITY_BOOTSTRAP_DOMAIN=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'" || true)"
        if [[ -n "$from_file" ]]; then
            DOMAIN="$from_file"
            return 0
        fi
    fi
    DOMAIN="local.znas.io"
}

function install_root_ca() {
    # Idempotent -- mkcert detects an already-installed CA and no-ops.
    mkcert -install
}

function generate_cert() {
    mkdir -p "$CERT_DIR"
    mkcert \
        -cert-file "$CERT_DIR/dev.crt" \
        -key-file  "$CERT_DIR/dev.key" \
        "*.${DOMAIN}"
}

function summarize() {
    echo ""
    echo "Wrote certs to $CERT_DIR (SAN: *.${DOMAIN}):"
    ls -lh "$CERT_DIR"/dev.crt "$CERT_DIR"/dev.key
    echo ""
    echo "Restart nginx for the new certs to take effect:"
    echo "  docker compose -f docker/docker-compose.full.yml restart nginx"
    echo ""
    echo "Verify (after DNS records propagate):"
    echo "  curl -v https://app.${DOMAIN}"
    echo "  curl -v https://identity.${DOMAIN}/.well-known/jwks.json"
    echo ""
    echo "Browsers should show a green padlock; Go clients (cockpit)"
    echo "verify against the system trust store (where mkcert -install"
    echo "registered its root CA) with no special config."
}

function main() {
    require_mkcert
    resolve_domain
    install_root_ca
    generate_cert
    summarize
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
