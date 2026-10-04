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
#                                            base64, unzip, jq, python3 and its
#                                            yaml module, kubectl, the docker
#                                            client. A miss exits non-zero
#                                            naming the tool.
#   2. Go and protoc are the versions this repository pins, read from WHERE it
#      pins them (go.work's toolchain line, scripts/dev/proto-gen.sh's
#      PROTOC_VERSION) rather than from a third copy here. A pinned image
#      carrying another Go would test the repository on a Go nothing else runs.
#      Node's MAJOR is the one this image's Dockerfile declares
#      (`FROM node:<major>-...`): no file pins Node repository-wide (the
#      packages accept >=20, ci.yml uses 20 and 22), so the declaration is the
#      source, and the check proves the image runs what it says, not Debian's
#      Node 18 or a copy that went wrong.
#   3. Go compiles, protoc resolves the well-known types this repository's
#      protos import, and kubectl renders a kustomization that uses a
#      component, as this repository's overlays do. A version string proves
#      the binary starts; these prove the toolchain is whole (GOROOT, protoc's
#      include directory, a kustomize new enough for `components:`).
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

function declared_node_major() {
	local v
	v="$(awk '/^FROM node:[0-9]/ { sub(/^FROM node:/, ""); sub(/[^0-9].*$/, ""); print; exit }' "${REPO_ROOT}/deploy/toolchain-image/Dockerfile")"
	[[ -n "$v" ]] || fail "could not read a FROM node:<major> line from ${REPO_ROOT}/deploy/toolchain-image/Dockerfile"
	printf '%s' "$v"
}

function assert_tools_run() {
	info "(1) every tool a step reaches for runs..."
	in_image <<'SCRIPT' || fail "a tool a pipeline step needs is missing or broken (see above)"
set -euo pipefail
for probe in "go version" "node --version" "npm --version" "protoc --version" \
	"git --version" "make --version" "psql --version" "pg_isready --version" \
	"curl --version" "tar --version" "gzip --version" "base64 --version" "unzip -v" \
	"jq --version" "python3 --version" "kubectl version --client" "docker --version"; do
	tool="${probe%% *}"
	command -v "$tool" >/dev/null || { echo "ERROR: ${tool} is not on PATH" >&2; exit 1; }
	# Word-split on purpose: the probe is a command and its flag.
	out="$($probe 2>&1)" || { echo "ERROR: '${probe}' failed: ${out}" >&2; exit 1; }
	printf '  %-10s %s\n' "$tool" "${out%%$'\n'*}"
done
# A module, not a binary, so not a probe above: scripts/deploy's
# conn-headroom-check.sh imports it.
python3 -c 'import yaml' || { echo "ERROR: python3 cannot import yaml (python3-yaml)" >&2; exit 1; }
printf '  %-10s %s\n' "yaml" "$(python3 -c 'import yaml; print(yaml.__version__)')"
SCRIPT
}

function assert_pinned_versions() {
	local want_go want_protoc want_node
	want_go="$(pinned_go)"
	want_protoc="$(pinned_protoc)"
	want_node="$(declared_node_major)"
	info "(2) Go is ${want_go} (go.work), protoc ${want_protoc} (scripts/dev/proto-gen.sh), Node ${want_node}.x (its Dockerfile)..."
	in_image -e "WANT_GO=${want_go}" -e "WANT_PROTOC=${want_protoc}" -e "WANT_NODE_MAJOR=${want_node}" <<'SCRIPT' || fail "the image does not carry the toolchain this repository pins"
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
got_node="$(node --version)"
got_node_major="${got_node#v}"
got_node_major="${got_node_major%%.*}"
if [[ "$got_node_major" != "$WANT_NODE_MAJOR" ]]; then
	echo "ERROR: the image's Node is ${got_node}; its Dockerfile declares node ${WANT_NODE_MAJOR}" >&2
	exit 1
fi
echo "  go ${got_go#go}, protoc ${got_protoc#libprotoc }, node ${got_node#v}"
SCRIPT
}

function assert_toolchain_builds() {
	info "(3) Go compiles, protoc resolves the well-known types, kubectl renders a component..."
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
# The overlays compose `components:` (kustomize v3.7+); Debian's own kubectl,
# 1.20.2, refuses the field, so this is the check that the copied one does not.
mkdir -p base component overlay
cat > base/kustomization.yaml <<'YAML'
resources: [configmap.yaml]
YAML
cat > base/configmap.yaml <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata: {name: smoke}
data: {probe: "1"}
YAML
cat > component/kustomization.yaml <<'YAML'
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
labels:
  - pairs: {example.com/smoke: component}
YAML
cat > overlay/kustomization.yaml <<'YAML'
resources: [../base]
components: [../component]
YAML
rendered="$(kubectl kustomize overlay)"
grep -q 'example.com/smoke: component' <<<"$rendered" || {
	echo "ERROR: kubectl kustomize did not apply the component:" >&2
	echo "$rendered" >&2
	exit 1
}
echo "  kubectl kustomize with a component: OK"
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
