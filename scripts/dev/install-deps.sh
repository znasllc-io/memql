#!/usr/bin/env bash
#
# scripts/dev/install-deps.sh
# ===========================
#
# Verifies + installs every build-time tool the MemQL dev workflow
# expects. Idempotent: skips anything already present. Safe to run
# before `make generate`, `make test` or `make up`.
#
# Installed (or verified):
#   - protoc                (Protocol Buffer compiler -- regen .pb.go)
#   - protoc-gen-go         (Go bindings generator)
#   - protoc-gen-go-grpc    (Go gRPC stubs generator)
#   - PyYAML                (for the python3 first on PATH: tests that `make
#                            test` runs exec scripts/ci's Python, which
#                            imports it -- see install_pyyaml)
# On Linux protoc and the distribution's PyYAML package come from the package
# manager through sudo, refused up front when sudo would prompt for a password.
#
# Verified-only (printed install hint if missing -- we never auto-
# install platform tools like Docker from inside a make target):
#   - Go                    (>= 1.26.1; the repo's pinned version)
#   - Docker                (k3d runs the cluster inside Docker)
#   - k3d + kubectl         (the local k3d + ArgoCD cluster; `make up`)
#
# Reported last: how this machine measures against the minimum requirements
# for running the whole cluster (scripts/lib/requirements.sh). Reported, not
# enforced -- building and testing need far less, and `make up` is the step
# that refuses.
#
# Per repo convention (CLAUDE.md): function-based structure. main()
# at the bottom calls them in order.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/requirements.sh
source "${SCRIPT_DIR}/../lib/requirements.sh"

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
        echo "  HINT: kubectl is not installed -- 'make up' / 'make dev' / 'make status' need it."
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

# install_pyyaml makes `import yaml` work for the python3 first on PATH, the
# interpreter the tests reach: they exec "python3" by name. Go tests in the
# root package, scripts/ci and scripts/cidb run scripts/ci's own Python
# (pipeline-db-tests.py, codeql-scan.py and the unittest suites beside them),
# which imports PyYAML, so a machine without it fails tests that CI passes.
# CI's toolchain image installs python3-yaml for the same reason
# (deploy/toolchain-image/Dockerfile). Those tests fail rather than skip
# without it, naming this target (scripts/ci/pyyaml).
#
# THE ROUTE follows who owns the interpreter, which is also what decides
# whether pip may write to it (PEP 668):
#   - Linux's /usr/bin/python3 belongs to the distribution: install its own
#     package (python3-yaml, as CI does), never pip. It may not have pip.
#   - A virtualenv takes a plain `pip install`; pip refuses --user in one.
#   - Any other interpreter takes `pip install --user`: no sudo, and nothing
#     written under the interpreter's own prefix. That covers the Command Line
#     Tools' /usr/bin/python3 on macOS.
#   - An EXTERNALLY-MANAGED interpreter that is not the distribution's --
#     Homebrew's, on macOS -- refuses --user too, unless told
#     --break-system-packages. That override is PEP 668's explicit opt-in, so
#     it is the user's to give, never this script's: without pip's own switch
#     (PIP_BREAK_SYSTEM_PACKAGES) the step installs nothing and offers both
#     ways forward. The override with --user changes nothing under Homebrew's
#     prefix: the files go to ~/Library/Python/<version>, and Homebrew has no
#     PyYAML formula for them to shadow. A virtualenv, which Homebrew
#     recommends, needs no override but only helps while it is activated:
#     `make test` from a fresh shell, a bare `go test` and an editor's test
#     runner all reach the python3 on PATH.
# The one interpreter served is the first python3 on THIS shell's PATH, so run
# the target from the shell that runs the tests. A Homebrew Python of a new
# minor version has a new user site and needs the target again.
function install_pyyaml() {
    if ! command -v python3 >/dev/null 2>&1; then
        echo "  ERROR: python3 is not installed. 'make test' runs scripts/ci's Python with it."
        echo "         macOS:  xcode-select --install   (or: brew install python)"
        echo "         Linux:  your distribution's python3 package, then re-run 'make install-deps'."
        return 1
    fi
    local python
    python="$(command -v python3)"
    if python3 -c 'import yaml' >/dev/null 2>&1; then
        echo "  [ok] PyYAML $(python3 -c 'import yaml; print(yaml.__version__)') for ${python}"
        return 0
    fi

    echo "  ${python} cannot import yaml -- attempting to install PyYAML..."
    local pkg="" package=""
    if is_distro_python3 "${python}"; then
        pkg="$(detect_pkg_manager)"
        package="$(distro_pyyaml_package "${pkg}")"
    fi
    if [[ -n "${package}" ]]; then
        install_pyyaml_with_distro "${pkg}" "${package}" || return 1
    else
        install_pyyaml_with_pip || return 1
    fi

    # A reported success is not the proof: pip can install into a user site
    # this interpreter does not read (PYTHONNOUSERSITE, for one).
    if ! python3 -c 'import yaml' >/dev/null 2>&1; then
        echo "  ERROR: the install reported success, but ${python} still cannot import yaml:"
        echo "         $(python3 -c 'import yaml' 2>&1 | tail -n 1)"
        return 1
    fi
    echo "  [ok] PyYAML $(python3 -c 'import yaml; print(yaml.__version__)') installed for ${python}"
}

# is_distro_python3 -- is this interpreter the Linux distribution's own?
function is_distro_python3() {
    [[ "$(detect_os)" == "linux" && "$1" == "/usr/bin/python3" ]]
}

# distro_pyyaml_package names the package that carries PyYAML for a Linux
# distribution's python3, or prints nothing for a package manager not listed.
function distro_pyyaml_package() {
    case "$1" in
        apt)    echo "python3-yaml" ;;
        dnf)    echo "python3-pyyaml" ;;
        pacman) echo "python-yaml" ;;
    esac
}

function install_pyyaml_with_distro() {
    local pkg="$1" package="$2"
    # The same up-front refusal as install_protoc: a sudo that would prompt
    # with nobody to answer fails late and says less.
    if [ "$(id -u)" -ne 0 ] && ! sudo -n true 2>/dev/null; then
        cat <<EOF
  ERROR: installing ${package} needs sudo, but sudo would prompt for a password
         and this script can't read one (no controlling terminal, or sudo
         timestamp expired). Install it once, manually, then re-run
         'make install-deps':

           sudo apt-get install -y python3-yaml    # Debian/Ubuntu
           sudo dnf install -y python3-pyyaml      # Fedora
           sudo pacman -S python-yaml              # Arch
EOF
        return 1
    fi
    case "${pkg}" in
        apt)    as_root apt-get update -qq && as_root apt-get install -y "${package}" ;;
        dnf)    as_root dnf install -y "${package}" ;;
        pacman) as_root pacman -S --noconfirm "${package}" ;;
    esac
}

function install_pyyaml_with_pip() {
    if ! python3 -m pip --version >/dev/null 2>&1; then
        echo "  ERROR: $(command -v python3) has no pip to install PyYAML with. Install pip"
        echo "         for it (python3 -m ensurepip --upgrade), or PyYAML another way,"
        echo "         then re-run 'make install-deps'."
        return 1
    fi
    local kind
    kind="$(python_install_kind)"
    local -a cmd=(python3 -m pip install --disable-pip-version-check)
    case "${kind}" in
        venv)
            cmd+=(pyyaml)
            ;;
        externally-managed)
            if ! pip_override_given; then
                cat <<EOF
  ERROR: $(command -v python3) is EXTERNALLY-MANAGED (PEP 668), as Homebrew's
         Python is: pip refuses to install into it, --user included, unless
         the user overrides that. Choose one, then re-run:

         - PyYAML in this user's site directory, outside the interpreter's
           own prefix (pip install --user --break-system-packages pyyaml):
               PIP_BREAK_SYSTEM_PACKAGES=1 make install-deps
         - a virtualenv, activated in every shell that runs the tests:
               python3 -m venv ~/.venvs/memql && . ~/.venvs/memql/bin/activate
               make install-deps
EOF
                return 1
            fi
            cmd+=(--user --break-system-packages pyyaml)
            ;;
        *)
            cmd+=(--user pyyaml)
            ;;
    esac
    echo "  running: ${cmd[*]}"
    "${cmd[@]}"
}

# pip_override_given -- has the user set pip's own PEP 668 override? Read with
# the spellings pip itself accepts as true, so "0" or "no" stays a refusal.
function pip_override_given() {
    case "${PIP_BREAK_SYSTEM_PACKAGES:-}" in
        1|[Yy]|[Yy][Ee][Ss]|[Tt]|[Tt][Rr][Uu][Ee]|[Oo][Nn]) return 0 ;;
    esac
    return 1
}

# python_install_kind prints how pip may install into the python3 on PATH, by
# the test pip applies itself: "venv" inside a virtualenv; otherwise
# "externally-managed" when the stdlib directory carries the PEP 668 marker,
# else "base".
function python_install_kind() {
    python3 - <<'PY'
import os, sys, sysconfig
if sys.prefix != getattr(sys, "base_prefix", sys.prefix):
    print("venv")
elif os.path.isfile(os.path.join(sysconfig.get_path("stdlib"), "EXTERNALLY-MANAGED")):
    print("externally-managed")
else:
    print("base")
PY
}

# as_root runs a command as root: directly when this already is root (a
# container), otherwise through sudo.
function as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# check_requirements -- print this machine against the minimum requirements
# `make up` holds it to. Never fails the dependency check.
function check_requirements() {
    local line
    requirements_evaluate "${HOME:-/}"
    echo "[deps] This machine against the minimum requirements for running the cluster:"
    while IFS= read -r line; do
        [[ -n "$line" ]] && echo "  ${line}"
    done <<< "$REQ_SUMMARY"
    if [[ "$REQ_MEETS_MINIMUM" != "true" ]]; then
        echo "[deps] WARNING: below the minimum. 'make up' will refuse to create a cluster here"
        echo "       (override: make up ALLOW_BELOW_MINIMUM=1). See ${REQ_DOCS_URL}"
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
    echo "[deps] Verifying MemQL dev dependencies..."
    check_go
    check_docker
    check_k3d
    check_kubectl
    install_protoc
    install_protoc_go_plugins
    install_pyyaml
    check_requirements
    summary
}

# Run main only when EXECUTED, so a test can source this file and drive one
# step against stub tools (scripts/dev/install_deps_pyyaml_test.go). Executed
# directly this is identical to the bare `main "$@"` it replaces.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
