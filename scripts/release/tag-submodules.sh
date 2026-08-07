#!/usr/bin/env bash
#
# scripts/release/tag-submodules.sh
# =================================
#
# Cut the Go module tags for memQL's nested modules (memql#3245, epic
# memql#3228).
#
# A nested module is only fetchable by an external consumer if a tag exists at
# `<module-dir>/vX.Y.Z`. The root `vX.Y.Z` tag does NOT publish them: Go
# resolves each module path against its own tag namespace, so
# `github.com/znasllc-io/memql/component/grpc/gen@v0.15.0` needs the tag
# `component/grpc/gen/v0.15.0` and nothing else will do.
#
# THE VERSION-LINE RULE, which this script implements:
#
#   wire      component/{grpc,node,bus}/gen        INDEPENDENT
#   engine    component/{language,database,harness,actions,memql}, dsl
#                                                   INDEPENDENT
#   lockstep  EVERY OTHER MODULE                    == the root release
#
# Two independent lines, not 48. `wire` versions independently because it is
# the wire contract and consumers pin it directly; `engine` because it is the
# platform. Everything else moves with the root, which means a lockstep tag is
# mechanical -- it carries the number the root release already chose, and there
# is nothing to decide. The complement is computed, not listed: a module added
# tomorrow is lockstep by default, which is the answer that needs no thought.
#
# WHY IT REFUSES A `v0.0.0` REQUIRE. Local development pins nested modules at
# the epoch-zero placeholder plus a relative `replace`. A dependency's replace
# directives are IGNORED by a consumer -- only the main module's apply -- so a
# published module whose go.mod says `require .../component/actions v0.0.0`
# sends every consumer looking for a version that does not exist. The tag would
# be immutable and broken. So this refuses, and says which module and which
# require.
#
# Per the repo's Makefile+shell-script convention (CLAUDE.md): function-based,
# one function per responsibility, main() at the bottom. Non-interactive,
# `--flag=value` only, and a `--dry-run` that prints the plan and creates
# nothing.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# The two independent lines, by module directory. Kept as literal lists
# because a version line is a DECISION -- deriving it would let a directory
# acquire an independent line by accident. scripts/release/submodule_lines_test.go
# asserts both lists exist on disk.
readonly WIRE_MODULES=(
	"component/grpc/gen"
	"component/node/gen"
	"component/bus/gen"
)

readonly ENGINE_MODULES=(
	"component/language"
	"component/database"
	"component/harness"
	"component/actions"
	"component/memql"
	"dsl"
)

#=============================================================================
# FUNCTIONS
#=============================================================================

usage() {
	cat <<'EOF'
Usage: tag-submodules.sh --version=X.Y.Z [--line=all|wire|engine|lockstep]
                         [--commit=<ref>] [--dry-run] [--no-push]

  --version=X.Y.Z   Semver WITHOUT the leading v (matches the VERSION file).
                    The tags created are <module-dir>/vX.Y.Z.
  --line=<name>     Which version line to cut. Default: all.
                      wire      the 3 wire modules
                      engine    the 6 engine modules
                      lockstep  every other module (the complement)
                      all       all three
  --commit=<ref>    Commit to tag. Default: the root tag vX.Y.Z, so submodule
                    tags land on exactly the commit the release was cut from.
  --dry-run         Print the plan; create and push nothing.
  --no-push         Create the tags locally but do not push them.

Exit codes: 0 ok, 2 bad argument, 3 refused (dirty tree, tag exists,
unresolvable require), 4 prerequisite missing.
EOF
}

repo_root() {
	git rev-parse --show-toplevel 2>/dev/null
}

# all_modules prints every module directory in the tree, repo-relative,
# excluding the root. Same discovery the module-boundaries CI lane uses.
all_modules() {
	find . -name go.mod -not -path './.git/*' -printf '%h\n' |
		sed 's|^\./||' | grep -v '^\.$' | sort
}

contains() {
	local needle="$1"
	shift
	local x
	for x in "$@"; do
		[[ "$x" == "$needle" ]] && return 0
	done
	return 1
}

# modules_for_line prints the module directories belonging to one line.
modules_for_line() {
	local line="$1" m
	case "$line" in
	wire) printf '%s\n' "${WIRE_MODULES[@]}" ;;
	engine) printf '%s\n' "${ENGINE_MODULES[@]}" ;;
	lockstep)
		while read -r m; do
			contains "$m" "${WIRE_MODULES[@]}" && continue
			contains "$m" "${ENGINE_MODULES[@]}" && continue
			printf '%s\n' "$m"
		done < <(all_modules)
		;;
	all)
		modules_for_line wire
		modules_for_line engine
		modules_for_line lockstep
		;;
	*)
		echo "ERROR: unknown --line=$line (want wire|engine|lockstep|all)" >&2
		return 2
		;;
	esac
}

# refuse_placeholder_requires fails if any module about to be tagged still
# names an internal dependency at the epoch-zero placeholder. See the header.
refuse_placeholder_requires() {
	local ref="$1"
	shift
	local status=0 m bad
	for m in "$@"; do
		bad="$(git show "$ref:$m/go.mod" 2>/dev/null |
			grep -E '^[[:space:]]*github\.com/znasllc-io/memql(/[^[:space:]]+)? v0\.0\.0' || true)"
		if [[ -n "$bad" ]]; then
			echo "ERROR: $m/go.mod requires an internal module at the v0.0.0 placeholder:" >&2
			echo "$bad" | sed 's/^/         /' >&2
			status=3
		fi
	done
	if [[ "$status" -ne 0 ]]; then
		cat >&2 <<'EOF'

A consumer IGNORES a dependency's replace directives -- only the main module's
apply -- so these requires would send every consumer looking for a version that
does not exist, on an immutable tag.

Fix: the release commit rewrites internal requires from v0.0.0 to the release
version, keeping the relative `replace` alongside so local builds and the
GOWORK=off lane are unaffected.
EOF
	fi
	return "$status"
}

refuse_dirty_tree() {
	if [[ -n "$(git status --porcelain)" ]]; then
		echo "ERROR: working tree is dirty; a release tag must name a clean commit" >&2
		return 3
	fi
	return 0
}

refuse_existing_tags() {
	local version="$1"
	shift
	local status=0 m
	for m in "$@"; do
		if git rev-parse -q --verify "refs/tags/$m/v$version" >/dev/null; then
			echo "ERROR: tag $m/v$version already exists -- module tags are write-once" >&2
			status=3
		fi
	done
	return "$status"
}

create_tags() {
	local version="$1" commit="$2" push="$3" dry="$4"
	shift 4
	local m tag status=0 refs=()
	for m in "$@"; do
		tag="$m/v$version"
		if [[ "$dry" == "true" ]]; then
			echo "  would tag $tag -> $(git rev-parse --short "$commit")"
			continue
		fi
		# `^{}` PEELS to the commit. Without it, tagging the annotated root
		# tag creates a tag-of-a-tag: git warns, and the ref means something
		# subtly different from every other tag in the repo. Both git and the
		# Go proxy dereference it, so this is hygiene rather than a bug fix --
		# but a module tag is immutable, so hygiene is cheap here and a
		# correction is not.
		if ! git tag -a "$tag" -m "$m v$version" "$commit^{}"; then
			echo "ERROR: failed to create $tag" >&2
			status=5
			continue
		fi
		echo "  tagged $tag"
		refs+=("refs/tags/$tag")
	done
	if [[ "$dry" == "true" || "$push" != "true" || "${#refs[@]}" -eq 0 ]]; then
		return "$status"
	fi
	if ! git push origin "${refs[@]}"; then
		echo "ERROR: push failed; the tags exist locally" >&2
		return 5
	fi
	echo "pushed ${#refs[@]} tag(s)"
	return "$status"
}

main() {
	local version="" line="all" commit="" dry="false" push="true" arg
	for arg in "$@"; do
		case "$arg" in
		--version=*) version="${arg#*=}" ;;
		--line=*) line="${arg#*=}" ;;
		--commit=*) commit="${arg#*=}" ;;
		--dry-run) dry="true" ;;
		--no-push) push="false" ;;
		--help | -h)
			usage
			return 0
			;;
		*)
			echo "ERROR: unknown argument: $arg" >&2
			usage >&2
			return 2
			;;
		esac
	done

	if [[ -z "$version" ]]; then
		echo "ERROR: --version=X.Y.Z is required" >&2
		usage >&2
		return 2
	fi
	if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "ERROR: --version must be bare semver (X.Y.Z), no leading v" >&2
		return 2
	fi

	local root
	root="$(repo_root)" || {
		echo "ERROR: not inside a git repository" >&2
		return 4
	}
	cd "$root" || return 4

	# Default to the root release tag: submodule tags name the same commit the
	# release was cut from, or the module line and the release line disagree.
	if [[ -z "$commit" ]]; then
		commit="v$version"
	fi
	if ! git rev-parse -q --verify "$commit^{commit}" >/dev/null; then
		echo "ERROR: $commit does not resolve; cut the root tag v$version first" >&2
		return 4
	fi

	local mods=()
	mapfile -t mods < <(modules_for_line "$line") || return 2
	if [[ "${#mods[@]}" -eq 0 ]]; then
		echo "ERROR: --line=$line selected no modules" >&2
		return 2
	fi

	echo "memQL submodule tags"
	echo "  version : v$version"
	echo "  line    : $line (${#mods[@]} modules)"
	echo "  commit  : $commit ($(git rev-parse --short "$commit"))"
	echo "  push    : $push"
	echo

	refuse_dirty_tree || return 3
	refuse_placeholder_requires "$commit" "${mods[@]}" || return 3
	refuse_existing_tags "$version" "${mods[@]}" || return 3

	create_tags "$version" "$commit" "$push" "$dry" "${mods[@]}"
}

main "$@"
