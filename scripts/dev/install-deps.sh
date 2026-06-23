#!/usr/bin/env bash
#
# scripts/dev/install-deps.sh
# ===========================
#
# Verifies + installs every build-time tool the memQL dev workflow
# expects. Idempotent: skips anything already present. Safe to run
# before `make generate` or `make up`.
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
#   - Docker                (k3d runs the cluster inside Docker)
#   - k3d + kubectl         (the local k3d + ArgoCD cluster; `make up`)
#
# Per repo convention (CLAUDE.md): function-based structure. main()
# at the bottom calls them in order.

set -euo pipefail

readonly REQUIRED_GO_VERSION="1.26.1"
# Pinning protoc-plugin versions keeps the generated *.pb.go stable
# across machines + CI. These MUST match the versions the committed
# bindings were generated with (and that scripts/dev/proto-gen.sh's drift
# gate pins) -- otherwise `make generate` reproduces nothing. Bump all
# three (here + proto-gen.sh) together with the toolchain.
readonly PROTOC_GEN_GO_VERSION="v1.36.11"
readonly PROTOC_GEN_GO_GRPC_VERSION="v1.6.2"

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
    echo "  [ok] docker (k3d runs the cluster inside Docker)"
}

# check_k3d + check_kubectl verify the local-cluster toolchain (Argo
# parity, #2061). Non-blocking hints: a fresh clone that only needs to
# build/generate doesn't strictly require them, but `make up` does.
function check_k3d() {
    if ! command -v k3d >/dev/null 2>&1; then
        echo "  HINT: k3d is not installed -- 'make up' (local k3d + ArgoCD cluster) needs it."
        echo "        macOS:  brew install k3d"
        echo "        Linux:  https://k3d.io/#installation"
        return 0
    fi
    echo "  [ok] k3d ($(k3d version 2>/dev/null | head -1))"
}

function check_kubectl() {
    if ! command -v kubectl >/dev/null 2>&1; then
        echo "  HINT: kubectl is not installed -- 'make up' / 'make dev' / 'make k3d-status' need it."
        echo "        macOS:  brew install kubectl"
        echo "        Linux:  https://kubernetes.io/docs/tasks/tools/#kubectl"
        return 0
    fi
    echo "  [ok] kubectl"
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
    # instead of silently failing during a non-tty run (CI, background
    # process, IDE task runner).
    if [ "$(id -u)" -ne 0 ] && [[ "${os}" == "linux" ]] && ! sudo -n true 2>/dev/null; then
        cat <<EOF
  ERROR: protoc install needs sudo, but sudo would prompt for a password
         and this script can't read one (no controlling terminal, or
         sudo timestamp expired).

         Install protoc once, manually, then re-run 'make install-deps':

           sudo apt-get install -y protobuf-compiler    # Debian/Ubuntu
           sudo dnf install -y protobuf-compiler        # Fedora
           sudo pacman -S protobuf                      # Arch

         (You only have to do this once. Subsequent 'make install-deps'
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
  Run 'make up' to bootstrap the local k3d + ArgoCD cluster.

EOF
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function main() {
    echo "[deps] Verifying memQL dev dependencies..."
    check_go
    check_docker
    check_k3d
    check_kubectl
    install_protoc
    install_protoc_go_plugins
    summary
}

main "$@"
