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

function check_ngrok() {
    # dev-refresh's lib_refresh_ngrok publishes a public
    # LIVEKIT_PUBLIC_URL so the Anam avatar cloud engine (and any
    # other external service that has to reach the local LiveKit)
    # can hit it. Without ngrok the voice-agent still works in
    # audio-only via the internal "ws://livekit:7880" URL, but the
    # avatar plugin fails to negotiate -- so the full voice+video
    # loop on dev needs ngrok present + authed.
    #
    # Non-blocking (matches mkcert): the dev stack still comes up.
    # We surface the hint so a fresh contributor doesn't burn an
    # hour wondering why the avatar isn't rendering.
    if ! command -v ngrok >/dev/null 2>&1; then
        echo "  HINT: ngrok is not installed -- voice-agent runs audio-only without it."
        case "$(uname -s)" in
            Darwin) echo "        Install: brew install ngrok" ;;
            Linux)  echo "        Install: snap install ngrok  (or https://ngrok.com/download)" ;;
            *)      echo "        Install: see https://ngrok.com/download" ;;
        esac
        echo "        Then: ngrok config add-authtoken <your-token>"
        echo "        (Not blocking; the voice+video loop needs ngrok for the public LIVEKIT_PUBLIC_URL.)"
        return 0
    fi
    if ! ngrok config check >/dev/null 2>&1; then
        echo "  HINT: ngrok installed but unauthenticated -- run 'ngrok config add-authtoken <your-token>'."
        echo "        (Not blocking; voice-agent runs audio-only without the public LIVEKIT_PUBLIC_URL.)"
        return 0
    fi
    echo "  [ok] ngrok (authed)"
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
    check_ngrok
    install_protoc
    install_protoc_go_plugins
    summary
}

main "$@"
