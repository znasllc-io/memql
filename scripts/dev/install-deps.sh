#!/usr/bin/env bash
#
# scripts/dev/install-deps.sh
# ===========================
#
# Verifies + installs every build-time tool the memQL dev workflow
# expects. Idempotent: skips anything already present. Safe to run
# before every `dev-refresh`.
#
# Installed (or verified):
#   - protoc                (Protocol Buffer compiler -- regen .pb.go)
#   - protoc-gen-go         (Go bindings generator)
#   - protoc-gen-go-grpc    (Go gRPC stubs generator)
#
# Verified-only (printed install hint if missing -- we never auto-
# install platform tools like Docker or sudo-requiring packages from
# inside a make target):
#   - Go                    (>= 1.26.1; the repo's pinned version)
#   - Docker + docker compose
#   - mkcert                (local TLS; see setup-tls.sh for actual use)
#
# Per repo convention (CLAUDE.md): function-based structure. main()
# at the bottom calls them in order.

set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

readonly REQUIRED_GO_VERSION="1.26.1"
# Pinning protoc-plugin versions keeps the generated *.pb.go stable
# across machines + CI. Bump these together with the toolchain.
readonly PROTOC_GEN_GO_VERSION="v1.34.2"
readonly PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"

# -----------------------------------------------------------------
# Detection helpers
# -----------------------------------------------------------------

function detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        *)       echo "unknown" ;;
    esac
}

function detect_pkg_manager() {
    if command -v apt-get >/dev/null 2>&1; then echo "apt"; return; fi
    if command -v dnf     >/dev/null 2>&1; then echo "dnf"; return; fi
    if command -v pacman  >/dev/null 2>&1; then echo "pacman"; return; fi
    if command -v brew    >/dev/null 2>&1; then echo "brew"; return; fi
    echo "unknown"
}

# -----------------------------------------------------------------
# Steps
# -----------------------------------------------------------------

function check_go() {
    if ! command -v go >/dev/null 2>&1; then
        echo "  ERROR: go is not installed. Install Go ${REQUIRED_GO_VERSION}+ from https://go.dev/dl/ and re-run."
        return 1
    fi
    local version
    version=$(go version | awk '{print $3}' | sed 's/go//')
    echo "  [ok] go ${version}"
}

function check_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        echo "  ERROR: docker is not installed."
        echo "         Install from https://docs.docker.com/get-docker/ and re-run."
        return 1
    fi
    if ! docker compose version >/dev/null 2>&1; then
        echo "  ERROR: docker compose v2 plugin missing."
        echo "         Install via 'docker plugin install' or upgrade Docker Desktop."
        return 1
    fi
    echo "  [ok] docker + compose"
}

function check_mkcert() {
    if ! command -v mkcert >/dev/null 2>&1; then
        echo "  HINT: mkcert is not installed -- local TLS won't work until you install it."
        echo "        macOS:  brew install mkcert nss"
        echo "        Linux:  https://github.com/FiloSottile/mkcert#linux"
        echo "        (Not blocking; run 'make setup-tls' after installing.)"
        return 0
    fi
    echo "  [ok] mkcert"
}

# install_ngrok auto-installs the ngrok CLI (macOS + Linux) and, when an
# authtoken is available in the environment, configures it -- so a fresh
# `make dev-refresh` brings up the public LIVEKIT_PUBLIC_URL the Anam avatar
# cloud engine dials into without any manual steps. The avatar (direct/Guide
# AND voice-agent) needs Anam's cloud to reach the local LiveKit; on a dev box
# that means a public tunnel, which dev-refresh's lib_refresh_ngrok stands up
# from this binary. Without ngrok the voice loop still works in audio-only.
#
# Non-blocking throughout (matches mkcert): a failed install / missing
# authtoken prints a clear hint but never aborts the dev stack bring-up.
function install_ngrok() {
    if command -v ngrok >/dev/null 2>&1; then
        echo "  [ok] ngrok present ($(ngrok version 2>/dev/null | head -1))"
        ngrok_ensure_authed
        return 0
    fi

    local os
    os=$(detect_os)
    echo "  ngrok not found -- installing (needed for the public LIVEKIT_PUBLIC_URL the Anam avatar dials in on)..."
    case "${os}" in
        darwin)
            if command -v brew >/dev/null 2>&1; then
                brew install ngrok || { echo "  WARNING: 'brew install ngrok' failed; install manually from https://ngrok.com/download"; return 0; }
            else
                echo "  HINT: Homebrew not found -- install ngrok from https://ngrok.com/download, then re-run."
                return 0
            fi
            ;;
        linux)
            install_ngrok_linux || return 0
            ;;
        *)
            echo "  HINT: ngrok auto-install unsupported on this OS -- see https://ngrok.com/download"
            return 0
            ;;
    esac

    if ! command -v ngrok >/dev/null 2>&1; then
        echo "  WARNING: ngrok install reported success but the binary isn't on PATH."
        return 0
    fi
    echo "  [ok] ngrok installed ($(ngrok version 2>/dev/null | head -1))"
    ngrok_ensure_authed
}

# install_ngrok_linux downloads ngrok's official static binary for the host
# arch and drops it on PATH -- no sudo, works across distros (snap/apt aren't
# always present). Installs into the Go bin dir (already PATH-ensured for the
# protoc plugins) so refresh.sh's later lib_refresh_ngrok finds it.
function install_ngrok_linux() {
    local arch tgz_arch url dest tmp
    arch=$(uname -m)
    case "${arch}" in
        x86_64|amd64)  tgz_arch="amd64" ;;
        aarch64|arm64) tgz_arch="arm64" ;;
        *) echo "  HINT: unknown arch '${arch}' -- install ngrok from https://ngrok.com/download"; return 1 ;;
    esac
    url="https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-linux-${tgz_arch}.tgz"

    dest=$(go env GOBIN 2>/dev/null)
    if [ -z "${dest}" ]; then dest="$(go env GOPATH 2>/dev/null)/bin"; fi
    if [ -z "${dest}" ] || [ "${dest}" = "/bin" ]; then dest="${HOME}/.local/bin"; fi
    mkdir -p "${dest}"

    tmp=$(mktemp -d)
    echo "  downloading ngrok (linux-${tgz_arch}) -> ${dest}/ngrok ..."
    if ! curl -fsSL "${url}" -o "${tmp}/ngrok.tgz"; then
        echo "  WARNING: ngrok download failed (${url}); install manually from https://ngrok.com/download"
        rm -rf "${tmp}"; return 1
    fi
    if ! tar -xzf "${tmp}/ngrok.tgz" -C "${tmp}"; then
        echo "  WARNING: ngrok archive extract failed."
        rm -rf "${tmp}"; return 1
    fi
    mv "${tmp}/ngrok" "${dest}/ngrok" && chmod +x "${dest}/ngrok"
    rm -rf "${tmp}"

    case ":${PATH}:" in
        *":${dest}:"*) ;;
        *) export PATH="${PATH}:${dest}"
           echo "  HINT: add ${dest} to your shell PATH so ngrok persists across sessions." ;;
    esac
}

# ngrok_ensure_authed configures the authtoken from $NGROK_AUTHTOKEN (or
# $MEMQL_NGROK_AUTHTOKEN) when ngrok isn't already authed -- so the operator
# sets the token once in their environment and `make dev-refresh` is turnkey.
function ngrok_ensure_authed() {
    if ngrok config check >/dev/null 2>&1; then
        echo "  [ok] ngrok (authed)"
        return 0
    fi
    local token="${NGROK_AUTHTOKEN:-${MEMQL_NGROK_AUTHTOKEN:-}}"
    if [ -n "${token}" ]; then
        echo "  ngrok: configuring authtoken from \$NGROK_AUTHTOKEN ..."
        if ngrok config add-authtoken "${token}" >/dev/null 2>&1; then
            echo "  [ok] ngrok authed"
            return 0
        fi
        echo "  WARNING: 'ngrok config add-authtoken' failed -- check the token value."
        return 0
    fi
    cat <<'EOF'
  HINT: ngrok is installed but not authenticated. Do ONE of:
    - export NGROK_AUTHTOKEN=<your token>   (then `make dev-refresh` auto-configures it), or
    - run once:  ngrok config add-authtoken <your token>
  Without it the Anam avatar can't get a public LIVEKIT_PUBLIC_URL (the
  voice + avatar loop stays audio-only / orb-only on a local box).
EOF
}

function install_protoc() {
    if command -v protoc >/dev/null 2>&1; then
        local version
        version=$(protoc --version | awk '{print $2}')
        echo "  [ok] protoc ${version} already installed"
        return 0
    fi

    local os pkg
    os=$(detect_os)
    pkg=$(detect_pkg_manager)
    echo "  protoc not found -- attempting install..."

    # Refuse non-interactive sudo upfront so we give a clear hint
    # instead of silently failing during 'make dev-refresh' run from
    # a non-tty context (CI, background process, IDE task runner).
    if [ "$(id -u)" -ne 0 ] && [[ "${os}" == "linux" ]] && ! sudo -n true 2>/dev/null; then
        cat <<EOF
  ERROR: protoc install needs sudo, but sudo would prompt for a password
         and this script can't read one (no controlling terminal, or
         sudo timestamp expired).

         Install protoc once, manually, then re-run 'make install-deps':

           sudo apt-get install -y protobuf-compiler    # Debian/Ubuntu
           sudo dnf install -y protobuf-compiler        # Fedora
           sudo pacman -S protobuf                      # Arch

         (You only have to do this once. Subsequent 'make dev-refresh'
         runs will see protoc and skip the install.)
EOF
        return 1
    fi

    case "${os}:${pkg}" in
        linux:apt)
            if [ "$(id -u)" -eq 0 ]; then
                apt-get update -qq && apt-get install -y protobuf-compiler
            else
                sudo apt-get update -qq && sudo apt-get install -y protobuf-compiler
            fi
            ;;
        linux:dnf)
            sudo dnf install -y protobuf-compiler
            ;;
        linux:pacman)
            sudo pacman -S --noconfirm protobuf
            ;;
        darwin:brew)
            brew install protobuf
            ;;
        *)
            cat <<EOF
  ERROR: don't know how to install protoc on this platform automatically.
         Detected os='${os}' pkg='${pkg}'. Install manually:
           macOS:  brew install protobuf
           Linux:  sudo apt-get install protobuf-compiler   (or your distro's package)
         Then re-run 'make install-deps'.
EOF
            return 1
            ;;
    esac

    # Verify the install actually placed protoc on PATH. Catches the
    # case where the package manager returned 0 but didn't end up
    # putting the binary somewhere we can find.
    if ! command -v protoc >/dev/null 2>&1; then
        echo "  ERROR: package manager reported success but 'protoc' is still missing from PATH."
        return 1
    fi
    echo "  [ok] protoc $(protoc --version | awk '{print $2}') installed"
}

function install_protoc_go_plugins() {
    local gobin
    gobin=$(go env GOBIN)
    if [ -z "${gobin}" ]; then
        gobin="$(go env GOPATH)/bin"
    fi
    # Make sure the install dir is in PATH for the rest of the session,
    # otherwise `make generate` (which shells out to protoc) won't find
    # the plugins even though they just got installed.
    case ":${PATH}:" in
        *":${gobin}:"*) ;;
        *) export PATH="${PATH}:${gobin}" ;;
    esac

    if command -v protoc-gen-go >/dev/null 2>&1; then
        echo "  [ok] protoc-gen-go already installed ($(which protoc-gen-go))"
    else
        echo "  installing protoc-gen-go@${PROTOC_GEN_GO_VERSION}..."
        go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
        echo "  [ok] protoc-gen-go installed to ${gobin}"
    fi

    if command -v protoc-gen-go-grpc >/dev/null 2>&1; then
        echo "  [ok] protoc-gen-go-grpc already installed ($(which protoc-gen-go-grpc))"
    else
        echo "  installing protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}..."
        go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
        echo "  [ok] protoc-gen-go-grpc installed to ${gobin}"
    fi

    if [[ ":${PATH}:" != *":${gobin}:"* ]]; then
        cat <<EOF

  HINT: ${gobin} is not on your PATH. Add this to ~/.bashrc or ~/.zshrc
  so 'make generate' can find the protoc plugins:

      export PATH="\$PATH:${gobin}"

EOF
    fi
}

function summary() {
    cat <<EOF

  Dev dependencies verified.
  Run 'make generate' to regenerate *.pb.go from edited .proto files.

EOF
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function main() {
    echo "[deps] Verifying memQL dev dependencies..."
    check_go
    check_docker
    check_mkcert
    install_protoc
    install_protoc_go_plugins
    install_ngrok
    summary
}

main "$@"
