#!/usr/bin/env bash
#
# scripts/lib/platform.sh
# =======================
#
# The installer's platform vocabulary, in ONE place (memql#4295).
#
# Three scripts have to agree on what platform this machine is and whether the
# installer supports it: detect.sh refuses an unsupported one, install-binary.sh
# composes a pin key from it, and refresh-tool-pins.sh generates a pin block per
# supported platform. They spelled it themselves when there was one platform and
# the answer was a constant. With two, three copies of the rule is three copies
# that can disagree -- and a disagreement here is an installer that refuses a
# platform it has pins for, or downloads a Linux binary onto a Mac.
#
# NORMALISATION IS THE GO/OCI SPELLING, deliberately, because that is what the
# release artifacts are named with: `uname -m` says `arm64` on an Apple Silicon
# Mac and `aarch64` on Linux for the same architecture, and `x86_64` where Go
# says `amd64`. Normalising to Go's names is what lets one pin key be composed
# from one uname pair.
#
# Sourced, never executed. No side effects at source time.

# platform_os -- the lowercased kernel name (linux | darwin | ...).
function platform_os() {
    printf '%s' "$(uname -s 2>/dev/null || printf 'unknown')" | tr '[:upper:]' '[:lower:]'
}

# platform_arch -- the Go/OCI architecture spelling, so the answer lines up with
# the artifact names in tool-pins.env.
function platform_arch() {
    local m
    m="$(uname -m 2>/dev/null || printf 'unknown')"
    case "$m" in
        x86_64|amd64)   printf 'amd64' ;;
        aarch64|arm64)  printf 'arm64' ;;
        armv7l|armv7)   printf 'arm' ;;
        *)              printf '%s' "$m" ;;
    esac
}

# platform_id [os] [arch] -- the canonical `os/arch` string. With no arguments
# it describes THIS machine; with arguments it normalises the pair given, which
# is what makes the supported-set check and the pin-key composition testable
# from a machine that is not the platform under test.
function platform_id() {
    local os="${1:-}" arch="${2:-}"
    [[ -n "$os"   ]] || os="$(platform_os)"
    [[ -n "$arch" ]] || arch="$(platform_arch)"
    printf '%s/%s' "$os" "$arch"
}

# SUPPORTED_PLATFORMS -- the closed set the local-cluster installer targets.
#
# A SET, not a pair of constants, and the difference is load-bearing: the thing
# that makes a platform supported is that tool-pins.env carries a verified
# digest for every tool on it. Adding a platform is therefore a pins
# regeneration plus one entry here, and pins_test.go asserts the two agree in
# both directions -- so a platform admitted here with no pins fails the build
# rather than failing at the first download.
#
# darwin/arm64 is Apple Silicon (memql#4295). darwin/amd64 (Intel Macs) is one
# `refresh-tool-pins.sh` run and one line away; it is absent because nothing has
# verified those digests, not because the mechanism excludes it.
readonly SUPPORTED_PLATFORMS=("linux/amd64" "darwin/arm64")

# platform_supported <os/arch> -- 0 when the installer targets it.
function platform_supported() {
    local want="$1" p
    for p in "${SUPPORTED_PLATFORMS[@]}"; do
        [[ "$p" == "$want" ]] && return 0
    done
    return 1
}

# platform_supported_csv -- the set, for a human-readable message.
function platform_supported_csv() {
    local IFS=", "
    printf '%s' "${SUPPORTED_PLATFORMS[*]}"
}

# platform_pin_suffix <os/arch> -- the tool-pins.env key fragment for a
# platform: `linux/amd64` -> `LINUX_AMD64`. One function so the generator and
# the consumer cannot spell the key differently.
function platform_pin_suffix() {
    printf '%s' "$1" | tr '[:lower:]/-' '[:upper:]__'
}
