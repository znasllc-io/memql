#!/usr/bin/env bash
#
# proto-gen.sh -- regenerate (or drift-check) the Go bindings for the proto
# sources whose committed output is pinned to protoc-gen-go v1.36.11 +
# protoc-gen-go-grpc v1.6.2: component/grpc and component/node.
#
#   scripts/dev/proto-gen.sh           # regenerate in place (the fix command)
#   scripts/dev/proto-gen.sh --check   # CI gate: fail on drift, leave tree clean
#
# WHY A DEDICATED GATE (memql#928): proto .pb.go is regenerated locally and
# committed by hand; nothing verified the committed bindings matched the .proto
# sources. A stale commit (edit the .proto, forget to regenerate) sails through
# CI today. This mirrors the existing `make sdk-gen-check` drift gate for the
# typed SDK.
#
# COVERAGE: component/grpc, component/node, component/bus -- every proto dir.
# (bus was normalized onto the pinned toolchain in the same change that added
# it here: its older-plugin field names, e.g. SIOpenaiApiKey, were regenerated
# to SiOpenaiApiKey and the handful of consumers updated -- memql#928.)
#
# WHY THE PROTOC VERSION STAMP IS IGNORED: the generated code BODY is determined
# by the protoc-gen-go / protoc-gen-go-grpc plugins (pinned here), not by the
# protoc binary -- protoc's version only appears in the `// protoc vX.Y.Z`
# header comment. Ignoring that line lets a patch-level protoc difference
# between a dev machine and CI coexist without tripping the gate.

set -euo pipefail

readonly PROTOC_GEN_GO_VERSION="v1.36.11"
readonly PROTOC_GEN_GO_GRPC_VERSION="v1.6.2"

# "<dir>|<grpc:yes|no>|<space-separated .proto files>" -- mirrors the
# //go:generate directives. grpc=no for service-less protos (bus has only
# messages, so running protoc-gen-go-grpc would emit an uncommitted file).
readonly PROTO_TARGETS=(
	"component/grpc|yes|memql.proto worker.proto deploy_control.proto"
	"component/node|yes|node.proto"
	"component/bus|no|bus.proto"
)
# Generated trees compared against the committed copies.
readonly GEN_PATHS=("component/grpc/gen" "component/node/gen" "component/bus/gen")
# Hunks whose only changed lines match this are ignored (the protoc stamp).
readonly STAMP_IGNORE='^//[[:space:]].*protoc'

repo_root() { git rev-parse --show-toplevel; }

# ensure_tools installs the pinned plugins into a throwaway bin and prepends it
# to PATH; verifies protoc itself is reachable (the body is plugin-determined,
# so any modern protoc works).
ensure_tools() {
	if ! command -v protoc >/dev/null 2>&1; then
		echo "ERROR: protoc not found. Install the protobuf compiler (e.g. 'apt-get install -y protobuf-compiler' or 'brew install protobuf')." >&2
		exit 1
	fi
	local bin
	bin="$(mktemp -d)/protobin"
	mkdir -p "$bin"
	echo "  installing protoc-gen-go@${PROTOC_GEN_GO_VERSION} + protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}..."
	GOBIN="$bin" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
	GOBIN="$bin" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
	export PATH="$bin:$PATH"
}

# regenerate runs protoc over every covered target, in place. Targets flagged
# grpc=no get --go_out only (service-less protos have no _grpc.pb.go).
regenerate() {
	local target dir grpc files grpc_args
	for target in "${PROTO_TARGETS[@]}"; do
		dir="${target%%|*}"
		grpc="${target#*|}"
		grpc="${grpc%%|*}"
		files="${target##*|}"
		grpc_args=()
		if [[ "$grpc" == "yes" ]]; then
			grpc_args=(--go-grpc_out=gen --go-grpc_opt=paths=source_relative)
		fi
		echo "  regenerating ${dir} (${files})..."
		# shellcheck disable=SC2086 -- word-splitting the file list is intended.
		(cd "$dir" && protoc --proto_path=. \
			--go_out=gen --go_opt=paths=source_relative \
			"${grpc_args[@]}" \
			$files)
	done
}

# run_check regenerates, diffs ignoring the stamp, restores the tree, and fails
# on real drift.
run_check() {
	regenerate
	local rc=0
	git diff --exit-code -I"$STAMP_IGNORE" -- "${GEN_PATHS[@]}" || rc=$?
	# Always restore so the check never leaves the working tree dirty.
	git checkout -- "${GEN_PATHS[@]}"
	if [[ $rc -ne 0 ]]; then
		echo "" >&2
		echo "ERROR: proto bindings are out of date (component/grpc, component/node)." >&2
		echo "       A .proto changed without its generated .pb.go. Run 'make proto-gen' and commit the result." >&2
		exit 1
	fi
	echo "proto-gen: no drift"
}

main() {
	cd "$(repo_root)"
	ensure_tools
	if [[ "${1:-}" == "--check" ]]; then
		run_check
	else
		regenerate
		echo "proto-gen: regenerated component/grpc, component/node (review + commit; the // protoc stamp line is cosmetic)"
	fi
}

main "$@"
