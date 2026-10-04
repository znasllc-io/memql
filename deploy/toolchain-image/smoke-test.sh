#!/usr/bin/env bash
#
# Smoke test for the pipeline toolchain image (epic memql#5478, issue
# memql#5496).
#
# Run against a BUILT image, before it is pushed anywhere. The release lane
# (.github/workflows/build-toolchain-image.yml) runs it between the local build
# and the push, so an image that fails it never reaches the registry; the same
# script checks a local build (see the Dockerfile header).
#
# WHAT IT ASSERTS, AND WHY EACH ONE EARNS ITS PLACE
#
#   1. Every tool a step reaches for runs.   go, node, npm, protoc, git, make,
#                                            psql, pg_isready, curl, tar, gzip,
#                                            base64, unzip. A miss exits
#                                            non-zero naming the tool.
#   2. Go and protoc are the versions this repository pins, read from WHERE it
#      pins them (go.work's toolchain line, scripts/dev/proto-gen.sh's
#      PROTOC_VERSION) rather than from a third copy here. A pinned image
#      carrying another Go would test the repository on a Go nothing else runs.
#   3. Go compiles and protoc resolves the well-known types this repository's
#      protos import. A version string proves the binary starts; these prove
#      the toolchain is whole (GOROOT, protoc's include directory).
#   4. It runs as uid 1000, not root, starts in /workspace, and /workspace,
#      /cache and $HOME are writable.
#   5. The artifact framing round-trips: the step wrapper ships declared
#      artifacts as `tar -czf - ... | base64`.
#   6. git reads a checkout that ANOTHER uid made at /workspace, which is what
#      a Job's clone container leaves behind. It first proves the probe really
#      reaches git's ownership check (with the trusted list cleared, git must
#      refuse), so the pass cannot be a probe that tested nothing.
#
# Usage:
#   deploy/toolchain-image/smoke-test.sh <image-ref>

set -euo pipefail

IMAGE="${1:?usage: smoke-test.sh <image-ref>}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SMOKE_VOLUME=""

function info() { printf 'INFO:  %s\n' "$*" >&2; }
function fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

function cleanup() {
	if [[ -n "$SMOKE_VOLUME" ]]; then
		docker volume rm -f "$SMOKE_VOLUME" >/dev/null 2>&1 || true
	fi
}

# in_image runs the bash script on stdin in a fresh container of the image,
# as the image's own user unless a flag says otherwise. Extra arguments are
# `docker run` flags.
function in_image() {
	docker run --rm -i "$@" --entrypoint bash "$IMAGE" -s
}

function pinned_go() {
	local v
	v="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "${REPO_ROOT}/go.work")"
	[[ -n "$v" ]] || fail "could not read the toolchain line from ${REPO_ROOT}/go.work"
	printf '%s' "$v"
}

function pinned_protoc() {
	local v
	v="$(sed -n 's/^readonly PROTOC_VERSION="\([^"]*\)"$/\1/p' "${REPO_ROOT}/scripts/dev/proto-gen.sh")"
	[[ -n "$v" ]] || fail "could not read PROTOC_VERSION from ${REPO_ROOT}/scripts/dev/proto-gen.sh"
	printf '%s' "$v"
}

function assert_tools_run() {
	info "(1) every tool a step reaches for runs..."
	in_image <<'SCRIPT' || fail "a tool a pipeline step needs is missing or broken (see above)"
set -euo pipefail
for probe in "go version" "node --version" "npm --version" "protoc --version" \
	"git --version" "make --version" "psql --version" "pg_isready --version" \
	"curl --version" "tar --version" "gzip --version" "base64 --version" "unzip -v"; do
	tool="${probe%% *}"
	command -v "$tool" >/dev/null || { echo "ERROR: ${tool} is not on PATH" >&2; exit 1; }
	# Word-split on purpose: the probe is a command and its flag.
	out="$($probe 2>&1)" || { echo "ERROR: '${probe}' failed: ${out}" >&2; exit 1; }
	printf '  %-10s %s\n' "$tool" "${out%%$'\n'*}"
done
SCRIPT
}

function assert_pinned_versions() {
	local want_go want_protoc
	want_go="$(pinned_go)"
	want_protoc="$(pinned_protoc)"
	info "(2) Go is ${want_go} (go.work) and protoc is ${want_protoc} (scripts/dev/proto-gen.sh)..."
	in_image -e "WANT_GO=${want_go}" -e "WANT_PROTOC=${want_protoc}" <<'SCRIPT' || fail "the image does not carry the toolchain this repository pins"
set -euo pipefail
# GOTOOLCHAIN=local: report the Go INSTALLED here, never one fetched on demand.
got_go="$(GOTOOLCHAIN=local go env GOVERSION)"
if [[ "$got_go" != "go${WANT_GO}" ]]; then
	echo "ERROR: the image's Go is ${got_go}; go.work pins go${WANT_GO}" >&2
	exit 1
fi
got_protoc="$(protoc --version)"
if [[ "$got_protoc" != "libprotoc ${WANT_PROTOC}" ]]; then
	echo "ERROR: the image's protoc says '${got_protoc}'; scripts/dev/proto-gen.sh pins ${WANT_PROTOC}" >&2
	exit 1
fi
echo "  go ${got_go#go}, protoc ${got_protoc#libprotoc }"
SCRIPT
}

function assert_toolchain_builds() {
	info "(3) Go compiles, and protoc resolves the well-known types..."
	in_image <<'SCRIPT' || fail "the tools start but the toolchain cannot build"
set -euo pipefail
cd "$(mktemp -d)"
cat > main.go <<'GO'
package main

import "fmt"

func main() { fmt.Println("  go run: OK") }
GO
GOTOOLCHAIN=local go run main.go
cat > probe.proto <<'PROTO'
syntax = "proto3";
package probe;
import "google/protobuf/timestamp.proto";
message Probe { google.protobuf.Timestamp at = 1; }
PROTO
protoc --proto_path=. --descriptor_set_out=/dev/null probe.proto
echo "  protoc with google/protobuf/timestamp.proto: OK"
SCRIPT
}

function assert_runtime_contract() {
	info "(4, 5) uid 1000 in /workspace, writable directories, artifact framing..."
	in_image <<'SCRIPT' || fail "the image breaks the step container's contract"
set -euo pipefail
if [[ "$(id -u):$(id -g)" != "1000:1000" ]]; then
	echo "ERROR: the image runs as $(id -u):$(id -g); steps must run as 1000:1000, never root" >&2
	exit 1
fi
if [[ "$PWD" != "/workspace" ]]; then
	echo "ERROR: the image starts in ${PWD}; a step's command runs in /workspace" >&2
	exit 1
fi
for dir in /workspace /cache "$HOME"; do
	probe="${dir}/.memql-smoke-probe"
	if ! { : > "$probe" && rm -f "$probe"; }; then
		echo "ERROR: ${dir} is not writable by uid 1000" >&2
		exit 1
	fi
done
echo "  uid 1000:1000 in /workspace; /workspace, /cache and ${HOME} writable: OK"

cd "$(mktemp -d)"
mkdir -p out back
printf 'artifact\n' > out/report.txt
tar -czf - -- out | base64 > framed.b64
base64 -d framed.b64 | tar -xzf - -C back
cmp out/report.txt back/out/report.txt
echo "  tar | gzip | base64 round trip: OK"
SCRIPT
}

function assert_git_reads_a_foreign_checkout() {
	info "(6) git reads a checkout another uid made at /workspace..."
	SMOKE_VOLUME="memql-toolchain-smoke-$$"
	docker volume create "$SMOKE_VOLUME" >/dev/null

	# What a Job's clone container leaves: a repository written as root into a
	# volume whose root is root-owned and world-writable, like an emptyDir.
	in_image --user 0:0 -v "${SMOKE_VOLUME}:/workspace" <<'SCRIPT' || fail "could not prepare a root-owned checkout"
set -euo pipefail
chown 0:0 /workspace
chmod 0777 /workspace
git init -q /workspace
git -C /workspace -c user.name=smoke -c user.email=smoke@example.invalid commit -q --allow-empty -m "foreign checkout"
SCRIPT

	in_image -v "${SMOKE_VOLUME}:/workspace" <<'SCRIPT' || fail "git, as uid 1000, cannot use a checkout another uid made at /workspace"
set -euo pipefail
if [[ "$(stat -c %u /workspace/.git)" != "0" ]]; then
	echo "ERROR: the probe's checkout is not root-owned, so it cannot exercise git's ownership check" >&2
	exit 1
fi
# The control: with the trusted list cleared, git must refuse. If it does not,
# the probe never reached the ownership check and the pass below proves nothing.
if git -c safe.directory= -C /workspace rev-parse --show-toplevel >/dev/null 2>&1; then
	echo "ERROR: git accepted the foreign checkout with safe.directory cleared; the probe tests nothing" >&2
	exit 1
fi
git -C /workspace rev-parse --show-toplevel >/dev/null
git -C /workspace ls-files >/dev/null
echo "  git log on a root-made checkout: $(git -C /workspace log -1 --format=%s)"
SCRIPT
}

function main() {
	trap cleanup EXIT
	command -v docker >/dev/null || fail "docker is required"
	docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "no local image ${IMAGE}; build it first"
	info "smoke-testing ${IMAGE}"
	assert_tools_run
	assert_pinned_versions
	assert_toolchain_builds
	assert_runtime_contract
	assert_git_reads_a_foreign_checkout
	info "SUCCESS: ${IMAGE} passed every smoke check"
}

main "$@"
