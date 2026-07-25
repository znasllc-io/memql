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
# protoc is pinned too (memql#2774). The generated BODY is plugin-determined,
# but the `// protoc vX.Y.Z` header comment is not -- so an unpinned protoc
# rewrote that line in all 8 generated files, turning a one-message change into
# an 8-file diff the author had to hand-revert. Pinning makes every machine and
# CI produce byte-identical output.
readonly PROTOC_VERSION="33.4"

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

# protoc_platform echoes the asset suffix protobuf's releases use for this
# host, matching github.com/protocolbuffers/protobuf/releases.
protoc_platform() {
	local kernel arch
	kernel="$(uname -s | tr '[:upper:]' '[:lower:]')"
	arch="$(uname -m)"
	case "${kernel}/${arch}" in
		darwin/arm64)  echo "osx-aarch_64" ;;
		darwin/x86_64) echo "osx-x86_64" ;;
		linux/x86_64)  echo "linux-x86_64" ;;
		linux/aarch64|linux/arm64) echo "linux-aarch_64" ;;
		*)
			echo "ERROR: unsupported platform ${kernel}/${arch} for pinned protoc ${PROTOC_VERSION}." >&2
			exit 1
			;;
	esac
}

# resolve_protoc echoes the path to the PINNED protoc, downloading it into
# bin/tools/ on first use. Mirrors the Tailwind CLI provisioning in
# scripts/identity/build-css.sh: per-platform, cached, retried.
#
# The pinned binary is used even when a system protoc exists, because
# "whatever is on PATH" is exactly what produced the stamp churn.
resolve_protoc() {
	local platform tools_dir dir
	platform="$(protoc_platform)"
	tools_dir="$(repo_root)/bin/tools"
	dir="${tools_dir}/protoc-${PROTOC_VERSION}-${platform}"
	if [[ -x "${dir}/bin/protoc" ]]; then
		echo "${dir}/bin/protoc"
		return 0
	fi
	mkdir -p "${dir}"
	local url="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${platform}.zip"
	echo "  downloading pinned protoc ${PROTOC_VERSION} for ${platform}..." >&2
	local zip="${dir}/protoc.zip"
	if curl -sSL --fail --retry 5 --retry-all-errors --retry-delay 2 -o "${zip}" "${url}" &&
		unzip -oq "${zip}" -d "${dir}"; then
		rm -f "${zip}"
		chmod +x "${dir}/bin/protoc"
		echo "${dir}/bin/protoc"
		return 0
	fi
	rm -rf "${dir}"

	# Fall back to a system protoc rather than failing the lane outright. A
	# registry/release outage should not block a proto change -- and the
	# fallback is SAFE for the gate, because --check diffs with the version
	# stamp ignored, so a differing protoc still produces a correct verdict.
	# The only cost is cosmetic stamp churn on a plain regenerate, which is
	# exactly the annoyance the pin exists to avoid -- hence the loud warning
	# rather than silent degradation.
	if command -v protoc >/dev/null 2>&1; then
		echo "WARNING: could not fetch pinned protoc ${PROTOC_VERSION} (${url})." >&2
		echo "         Falling back to system protoc $(protoc --version 2>/dev/null | awk '{print $2}')." >&2
		echo "         Drift detection is unaffected (the version stamp is ignored when diffing)," >&2
		echo "         but a plain regenerate may rewrite the stamp in every generated file." >&2
		command -v protoc
		return 0
	fi

	echo "ERROR: could not fetch pinned protoc ${PROTOC_VERSION} from ${url}," >&2
	echo "       and no system protoc is available as a fallback." >&2
	echo "       Install one (e.g. 'apt-get install -y protobuf-compiler' or 'brew install protobuf') and retry." >&2
	exit 1
}

# ensure_tools installs the pinned plugins into a throwaway bin, provisions the
# pinned protoc, and prepends both to PATH.
ensure_tools() {
	local protoc_bin
	protoc_bin="$(resolve_protoc)"
	export PATH="$(dirname "${protoc_bin}"):$PATH"
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
		# "${grpc_args[@]+...}" guards the empty-array expansion under set -u
		# on bash 3.2 (macOS): a grpc=no target (bus) has an empty grpc_args,
		# and a bare "${grpc_args[@]}" aborts with "unbound variable" there.
		# shellcheck disable=SC2086 -- word-splitting the file list is intended.
		(cd "$dir" && protoc --proto_path=. \
			--go_out=gen --go_opt=paths=source_relative \
			${grpc_args[@]+"${grpc_args[@]}"} \
			$files)
	done
}

# run_check regenerates, diffs ignoring the stamp, restores the tree, and fails
# on real drift.
#
# The restore is from a BACKUP taken first, not from git (memql#2774).
# `git checkout --` restores to HEAD, which silently discarded an
# already-regenerated-but-uncommitted tree: run `make proto-gen` then
# `make proto-gen-check` before committing and the check ate the work, exiting
# 0 while the next build failed on undefined symbols. Restoring from the backup
# means the check leaves the tree exactly as it found it, committed or not --
# which is what the old comment claimed and never did.
run_check() {
	local backup
	backup="$(mktemp -d)"
	local p
	for p in "${GEN_PATHS[@]}"; do
		mkdir -p "${backup}/$(dirname "$p")"
		cp -R "$p" "${backup}/$(dirname "$p")/"
	done

	regenerate
	local rc=0
	git diff --exit-code -I"$STAMP_IGNORE" -- "${GEN_PATHS[@]}" || rc=$?

	for p in "${GEN_PATHS[@]}"; do
		rm -rf "$p"
		cp -R "${backup}/$p" "$p"
	done
	rm -rf "${backup}"
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
