#!/usr/bin/env bash
# The DB-gated package set, and its complement (znasllc-io/memql#3160).
#
# # The duplication being removed
#
# `go-checks` ran `go test ./...` over all 182 packages; `db-tests` then re-ran
# six trees carrying 543+ test files (component/memql 342, component/automations
# 100, component/grpc 42, integrations/planner 45, integrations/cognition 14,
# examples/referencepack). Both jobs therefore compiled and executed those files
# on every PR. In go-checks the DB-gated CASES skip (no MEMQL_DATABASE_DSN), but
# the packages still compile and their non-DB tests still run.
#
# So: db-tests owns those trees entirely, and go-checks runs the COMPLEMENT.
# The non-DB tests in those packages are not lost -- db-tests runs the whole
# package, not just the gated cases.
#
# # Why the list lives here and db-tests keeps its literal arguments
#
# `scripts/cidb/dbgate_test.go` is a 1100-line gate that derives the db-tests
# lane's package selector by parsing literal `./`-prefixed arguments out of that
# step's `run:` block. `source` and `.` are in its shellControlKeywords, so
# making db-tests source this script would either empty its derived package list
# or mark the block non-plain -- either way disarming the gate that exists
# because these suites once rotted unnoticed (memql#2342).
#
# Rather than rework that gate, db-tests keeps its literal arguments and this
# script holds the canonical set for go-checks to subtract. The two cannot drift
# apart because scripts/ci/db_gated_packages_test.go asserts they are identical;
# that test is the "single source of truth" property, enforced rather than
# asserted by convention.
set -euo pipefail

# MODULE_PATH is pinned, NOT read from `go list -m` (memql#3165).
#
# `go list -m` prints ONE line only while this repo is a single module with no
# go.work. Under a workspace it prints one line PER module, and the multi-line
# value was being interpolated straight into a `grep -E` pattern -- where a
# newline is grep's own pattern separator. The pattern
# `^mod-a/component/memql(/|$)` plus a stray `^mod-b` alternative matches every
# package in mod-b, `grep -Ev` emits nothing, and the complement collapses to
# empty. `go test` then ran with no package arguments at all: it tested the
# current directory and exited 0 over 169 unrun packages. That is the
# memql#2972 fail-open shape, reachable by adding a file this script never
# reads.
#
# Pinned here, the value cannot acquire a newline. verify_module_path below
# makes the pin falsifiable rather than a second place for the module path to
# rot: it fails loudly if the pinned path is not a module in scope.
readonly MODULE_PATH="github.com/znasllc-io/memql"

# DB_GATED_TREES is the canonical set. Keep in sync with the db-tests step in
# .github/workflows/ci.yml -- enforced by TestDBGatedTreesMatchTheDBTestsLane.
readonly DB_GATED_TREES=(
	"component/memql"
	"component/automations"
	"component/grpc"
	"integrations/cognition"
	"integrations/planner"
	"examples/referencepack"
)

# KNOWN_GO_MOD_DIRS is every directory this script knows carries a `go.mod`,
# repo-relative, `.` for the repo root (memql#3165 review).
#
# This is the ALLOW-LIST for verify_no_unknown_modules -- the PRIMARY
# detector for a nested module boundary. When memql#3165 genuinely creates
# modules, this array gets an explicit entry per module. That edit is the
# friction, and it is the correct friction: it is a one-line, reviewed,
# deliberate statement that the person drawing the boundary also thought about
# what the complement now means.
#
# memql#3240 added the first three: the `wire` tier. What the complement now
# means, having looked: `go list <module>/...` runs in workspace mode, so the
# wire packages are still enumerated and still tested -- the complement did not
# shrink. The count is unchanged at 181 untagged. Each wire module is
# ADDITIONALLY built and vetted with GOWORK=off by the `module-boundaries` lane,
# which is where the boundary itself is enforced.
readonly KNOWN_GO_MOD_DIRS=(
	"."
	"component/actions"
	"component/architecture"
	"component/auth"
	"component/bus"
	"component/bus/gen"
	"component/config"
	"component/database"
	"component/events"
	"component/fileprocessor"
	"component/genesis"
	"component/grpc/gen"
	"component/harness"
	"component/healing"
	"component/language"
	"component/language/annotations"
	"component/language/ast"
	"component/language/dslclause"
	"component/memql"
	"component/metadata"
	"component/metrics"
	"component/node/gen"
	"component/observe"
	"component/planner"
	"component/polyphon"
	"component/provenance"
	"component/safety"
	"component/secret"
	"core"
	"docs"
	"dsl"
)

usage() {
	cat >&2 <<'EOF'
usage: db-gated-packages.sh (--trees | --patterns | --complement | --complement-cacheable)

  --trees                 one repo-relative tree per line (the canonical set)
  --patterns              the `./<tree>/...` patterns db-tests passes to `go test`
  --complement            every package NOT under a db-gated tree
  --complement-cacheable  the complement minus the root package, which must
                          always run uncached (see print_complement_cacheable)
EOF
}

print_trees() {
	printf '%s\n' "${DB_GATED_TREES[@]}"
}

print_patterns() {
	local t
	for t in "${DB_GATED_TREES[@]}"; do
		printf './%s/...\n' "$t"
	done
}

# verify_module_path fails if MODULE_PATH is not a module in scope.
#
# The pin above trades "always current" for "never malformed". This is the
# other half of that trade: if the module is ever renamed, this aborts with a
# named fix instead of quietly producing a complement that excludes nothing
# (every db-gated pattern would stop matching) or everything.
verify_module_path() {
	if go list -m 2>/dev/null | grep -Fxq "${MODULE_PATH}"; then
		return 0
	fi
	echo "db-gated-packages.sh: '${MODULE_PATH}' is not a module in scope here." >&2
	echo "  Modules found: $(go list -m 2>&1 | tr '\n' ' ')" >&2
	echo "  If the module was renamed or moved, update MODULE_PATH in this script." >&2
	return 4
}

# script_repo_root prints the repo root, derived from THIS FILE's own location.
#
# Not `$PWD` and not `git rev-parse`: the meaning of "the repo" must not depend
# on where the caller happened to be standing, and the walk below has to cover
# the same tree the toolchain reads whether or not this is a git checkout.
script_repo_root() {
	cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd
}

# find_go_mod_dirs prints every directory beneath the repo root that contains a
# `go.mod`, repo-relative, `.` for the root itself.
#
# The FILESYSTEM, not `git ls-files`: an untracked or gitignored `go.mod`
# truncates `go list` exactly as a committed one does, because the toolchain
# reads the disk.
#
# `.git` and `node_modules` are pruned, for DIFFERENT reasons -- an earlier
# revision of this comment claimed one reason for both and was wrong about
# node_modules:
#
#   .git          cannot hold a package the module pattern covers. True.
#   node_modules  CAN. Measured: planting `sdk/ts/node_modules/gopkg/g.go`
#                 takes `go list <mod>/...` from 183 to 184, so a `go.mod`
#                 beside it would truncate and this walk would not see it.
#
# It stays pruned anyway, deliberately. npm packages do ship Go source, and a
# vendored `go.mod` under node_modules is a FALSE REFUSAL that breaks CI for a
# tree that is perfectly fine -- a worse failure than the blind spot, which
# requires Go source in an npm dependency to be reachable by the main module's
# pattern. Accepted residual, recorded rather than hidden.
#
# `find`'s exit status is CAPTURED, not consumed through a process
# substitution. `while read < <(find ...)` discards it, and a walk that hit an
# unreadable directory and returned a PARTIAL list would then read as "no
# nested modules" -- the same swallow-the-status shape this whole file exists
# to refuse, one level down.
find_go_mod_dirs() {
	local root="$1" raw status=0 f rel
	raw="$(find "${root}" \( -name .git -o -name node_modules \) -prune -o -name go.mod -print)" || status=$?
	if ((status != 0)); then
		echo "db-gated-packages.sh: walking '${root}' for nested 'go.mod' files FAILED" >&2
		echo "  (find exit ${status}). A partial walk cannot be distinguished from a clean" >&2
		echo "  tree, so this refuses instead of guessing." >&2
		return 5
	fi
	if [[ -z "${raw}" ]]; then
		return 0
	fi
	while IFS= read -r f; do
		rel="${f#"${root}/"}"
		if [[ "${rel}" == "go.mod" ]]; then
			printf '.\n'
		else
			printf '%s\n' "${rel%/go.mod}"
		fi
	done <<<"${raw}" | sort
}

# verify_no_unknown_modules is the PRIMARY detector for the failure this script
# exists to refuse (memql#3165 review).
#
# The thing that silently shrinks `go list <module>/...` is a nested `go.mod`.
# Every earlier guard inferred that from a symptom -- an empty list, a short
# list, a count below a floor -- and every one of those inferences has a range
# it cannot see past: a leaf tree like scripts/ or cmd/ leaves with exit 0 and
# takes only 16-19 packages with it, which no floor loose enough to live with
# will ever notice (see MIN_COMPLEMENT).
#
# So assert the CAUSE. A `go.mod` this script does not know about is
# unambiguous, needs no calibration, and cannot drift with package churn. When
# memql#3165 draws a real boundary, KNOWN_GO_MOD_DIRS gets an explicit entry
# and whoever adds it has to look at what the complement now covers.
verify_no_unknown_modules() {
	local root dirs dir known ok status=0 found=0
	local -a unknown=()

	# Every substitution below states its own `|| return`. This function runs
	# inside print_complement, which runs inside a command substitution in
	# `--complement-cacheable` mode -- where `set -e` is suspended and an
	# implicit abort simply does not happen (see print_complement_cacheable).
	root="$(script_repo_root)" || return 5
	if [[ -z "${root}" || ! -d "${root}" ]]; then
		echo "db-gated-packages.sh: cannot resolve the repo root from this script's own" >&2
		echo "  location; the nested-module check cannot run, so it refuses." >&2
		return 5
	fi

	# The walk and `go list` must agree on WHICH tree they are talking about.
	# script_repo_root is derived from ${BASH_SOURCE[0]}; `go list` resolves
	# against $PWD. When those differ -- this script invoked from inside a
	# different checkout -- the walk clears a clean tree while `go list`
	# enumerates a truncated one, and the complement is silently short.
	# Demonstrated: cwd on a copy carrying `scripts/go.mod`, guard silent,
	# 150 packages printed. PWD-independence is the WRONG invariant here;
	# agreement is the right one.
	#
	# Name the main module EXPLICITLY. A bare `go list -m` prints one line per
	# module in the build list, and under a go.work that is every workspace
	# module -- four lines since memql#3240, which this comparison then treated
	# as a single directory name and refused on. Naming the module pins it to
	# the one root both sides mean.
	local golist_root
	golist_root="$(go list -m -f '{{.Dir}}' "${MODULE_PATH}" 2>/dev/null)" || golist_root=""
	if [[ -n "${golist_root}" ]] && [[ "$(cd -- "${golist_root}" && pwd -P)" != "$(cd -- "${root}" && pwd -P)" ]]; then
		echo "db-gated-packages.sh: this script and the Go toolchain disagree about the repo:" >&2
		echo "    this script walks : ${root}" >&2
		echo "    go list resolves  : ${golist_root}" >&2
		echo "  The nested-module walk would clear one tree while the package list came from" >&2
		echo "  another, so the complement could be silently truncated. Run this from the" >&2
		echo "  repo it lives in (ci.yml invokes it relatively from the repo root)." >&2
		return 5
	fi
	dirs="$(find_go_mod_dirs "${root}")" || status=$?
	if ((status != 0)); then
		echo "db-gated-packages.sh: the nested-module check could not enumerate 'go.mod'" >&2
		echo "  files (exit ${status}); refusing rather than reporting a tree it did not walk." >&2
		return 5
	fi

	while IFS= read -r dir; do
		if [[ -z "${dir}" ]]; then
			continue
		fi
		found=$((found + 1))
		ok=0
		for known in "${KNOWN_GO_MOD_DIRS[@]}"; do
			if [[ "${dir}" == "${known}" ]]; then
				ok=1
				break
			fi
		done
		if ((ok == 0)); then
			if [[ "${dir}" == "." ]]; then
				unknown+=("go.mod")
			else
				unknown+=("${dir}/go.mod")
			fi
		fi
	done <<<"${dirs}"

	# Zero `go.mod` files anywhere means the walk did not see the repo. That is
	# not "no nested modules", it is "this check verified nothing", and it fails
	# closed for the same reason everything else here does.
	if ((found == 0)); then
		echo "db-gated-packages.sh: found NO 'go.mod' anywhere under '${root}'." >&2
		echo "  The nested-module check cannot verify what it was written to verify, so it" >&2
		echo "  refuses rather than report a clean tree it never actually walked." >&2
		return 5
	fi

	if ((${#unknown[@]} == 0)); then
		return 0
	fi

	echo "db-gated-packages.sh: NESTED MODULE this script does not know about:" >&2
	local u
	for u in "${unknown[@]}"; do
		echo "      ${u}" >&2
	done
	echo "  A 'go.mod' beneath the repo root removes every package under it from" >&2
	echo "  'go list ${MODULE_PATH}/...'. When that tree has no external importers the" >&2
	echo "  removal is SILENT -- exit 0, no diagnostic, a shorter complement -- and" >&2
	echo "  'go test' then runs a smaller suite than this script reports. Measured, a" >&2
	echo "  'go.mod' at scripts/ costs 19 of 170 packages and trips no count floor loose" >&2
	echo "  enough to live with, which is why this check asserts the CAUSE and not a" >&2
	echo "  symptom." >&2
	echo "  If the module boundary is DELIBERATE (memql#3165), add its directory to" >&2
	echo "  KNOWN_GO_MOD_DIRS in this script and decide, in the same edit, what the" >&2
	echo "  complement should now cover -- the packages inside a new module are NOT in" >&2
	echo "  '${MODULE_PATH}/...' and need a lane of their own." >&2
	echo "  If it is NOT deliberate, delete it." >&2
	return 5
}

# MIN_COMPLEMENT is a SECONDARY, coarse backstop -- not the detector.
#
# Measured at 170 packages (169 cacheable, i.e. minus the root package) on
# 2026-08-06. Re-measure with:
#
#	scripts/ci/db-gated-packages.sh --complement | wc -l
#
# HONEST STATEMENT OF ITS RANGE (memql#3165 review; the previous version of
# this comment was measured wrong, and the correction is the point).
#
# It used to claim a `go.mod` under component/language, component/database,
# integrations/ or app/ truncates "with exit 0 and no diagnostic" at
# 154/146/135/152, three of which trip the floor. Re-measured against real
# nested `go.mod` files and the real toolchain, ALL FOUR exit 1 (`go list`
# prints 175/179/147/182 lines and still fails) because every one of those
# trees has EXTERNAL IMPORTERS -- the root module still imports packages that
# have just left it, so the load fails loudly. They are caught by the
# exit-status check in print_complement, NOT by this floor. The floor's stated
# justification described no case it actually catches.
#
# A tree can truncate with exit 0 only when NOTHING OUTSIDE IT IMPORTS IT.
# Measured, those are the leaf trees, and this is what they cost:
#
#	go.mod at scripts/        exit 0, complement 151   (-19)
#	go.mod at cmd/            exit 0, complement 154   (-16)
#	go.mod at sdk/go          exit 0, complement 163   (-7)
#	go.mod at cmd/memql-lsp   exit 0, complement 167   (-3)
#	go.mod at component/mcp   exit 0, complement 169   (-1)
#
# 21 of 170 packages must vanish before a floor of 150 fires. The LARGEST
# silent truncation this repo can produce is 19. So the floor catches NONE of
# the realistic silent cases -- which is why verify_no_unknown_modules exists
# and asserts the root cause directly instead of inferring it from a count.
#
# The floor is kept anyway, as a coarse net under everything the direct check
# cannot see: a `go list` that returns a short list for some reason nobody has
# thought of yet, with exit 0 and no new `go.mod` anywhere. It is not tightened
# to catch a 3-package truncation, because a floor that tight would fire on
# ordinary package deletion and get lowered by whoever hits it under time
# pressure -- a guard that gets edited away is worth less than a looser one
# that never cries wolf. The 12% headroom buys "never cries wolf"; it does not
# buy detection, and this comment no longer claims it does.
readonly MIN_COMPLEMENT=150

# count_lines counts the lines in a variable, treating empty as 0.
#
# `printf '%s\n' "" | wc -l` is 1, not 0, which is exactly the kind of
# off-by-one that would let a zero-package list clear a floor comparison.
count_lines() {
	if [[ -z "$1" ]]; then
		echo 0
	else
		printf '%s\n' "$1" | wc -l
	fi
}

# print_complement lists every package in the module except those under a
# db-gated tree.
#
# Anchored with a leading `/` and matched to a `/` or end-of-string boundary so
# a future package named e.g. `component/memqlx` is not swallowed by the
# `component/memql` entry.
#
# The `go list` selector is the module pattern rather than `./...` for the same
# reason MODULE_PATH is pinned: `./...` silently narrows to whichever module
# owns the working directory, so a `go.mod` added beneath the root would drop
# packages out of the complement with no diagnostic and CI would run a smaller
# suite than it reports.
print_complement() {
	# `|| return $?`, NOT a bare call (memql#3165 review). `set -e` is
	# SUSPENDED inside a command substitution subshell, and
	# print_complement_cacheable calls this function inside one -- so a bare
	# `verify_module_path` aborts when `--complement` is invoked directly and
	# is SILENTLY IGNORED when `--complement-cacheable` is. Measured against a
	# `go` stub whose `go list -m` reports a renamed module:
	#
	#	--complement            exit=4  printed=0     <-- guard fires
	#	--complement-cacheable  exit=0  printed=169   <-- guard bypassed
	#
	# Only an EXPLICIT return propagates out of a substitution, so every guard
	# in this function states one. The two modes must refuse identically or
	# the one CI actually calls is the one that does not.
	verify_module_path || return $?
	verify_no_unknown_modules || return $?

	local pattern="" t
	for t in "${DB_GATED_TREES[@]}"; do
		pattern+="${pattern:+|}^${MODULE_PATH}/${t}(/|\$)"
	done

	# NO `|| true` on this pipeline, and that is load-bearing (memql#3165
	# review). `go list` prints every package it COULD load on stdout and
	# still exits non-zero when any package fails to load, so a swallowed
	# exit status leaves `out` non-empty and TRUNCATED: the empty check below
	# passes, the script exits 0, and CI runs a smaller suite than it reports
	# -- the memql#2972 fail-open, reintroduced by the one construct that
	# looks like defensive coding. Measured A/B against a `go` stub that
	# prints 180 of 183 packages and exits 1, as the real toolchain does on a
	# partial load failure:
	#
	#	714ccbb4 (bare pipeline)  exit=1  167 packages printed
	#	1d13eef6 (`|| true`)      exit=0  167 packages printed  <-- fail-OPEN
	#	here     (status kept)    exit=5    0 packages printed
	#
	# `set -o pipefail` makes the pipeline's status non-zero if EITHER stage
	# fails, and every non-zero status is fatal here: grep's "no lines
	# selected" (1) means an empty complement, grep's usage error (2) means a
	# malformed pattern, and go list's (1) means an incomplete answer. There
	# is nothing to distinguish, only to refuse.
	local out status=0
	out="$(go list "${MODULE_PATH}/..." | grep -Ev "${pattern}")" || status=$?
	if ((status != 0)); then
		echo "db-gated-packages.sh: enumerating packages FAILED (exit ${status})." >&2
		echo "  'go list ${MODULE_PATH}/... | grep -Ev <db-gated>' did not succeed, so the" >&2
		echo "  complement cannot be trusted -- and note that 'go list' prints what it COULD" >&2
		echo "  load on stdout while still exiting non-zero, so the output being non-empty is" >&2
		echo "  NOT evidence that it is complete." >&2
		echo "  Refusing to print it: the caller would run a smaller suite than it reports." >&2
		echo "  Fix the load error 'go list ${MODULE_PATH}/...' reports; do not add '|| true'." >&2
		return 5
	fi

	local count
	count="$(count_lines "${out}")"
	if ((count < MIN_COMPLEMENT)); then
		echo "db-gated-packages.sh: the complement is ${count} packages, below the floor of" >&2
		echo "  ${MIN_COMPLEMENT} -- so it is TRUNCATED, not merely small, and this script fails" >&2
		echo "  closed rather than hand the caller a short list that tests green." >&2
		echo "  This is the SECONDARY backstop, and reaching it is informative: the direct" >&2
		echo "  nested-module check above already passed, so the usual cause -- a 'go.mod'" >&2
		echo "  beneath the repo root -- has been RULED OUT and something else shrank the" >&2
		echo "  list. Look at what 'go list' actually returned:" >&2
		echo "      go list ${MODULE_PATH}/... | wc -l" >&2
		echo "  If the repo genuinely got smaller, re-measure and lower MIN_COMPLEMENT in this" >&2
		echo "  script, recording the new number:" >&2
		echo "      scripts/ci/db-gated-packages.sh --complement | wc -l" >&2
		echo "  Do NOT silence this by lowering the floor to whatever today's count happens" >&2
		echo "  to be without establishing why the count moved." >&2
		return 5
	fi
	printf '%s\n' "${out}"
}

# print_complement_cacheable is the complement MINUS the module's root package.
#
# The root package hosts the repo-sweeping gates (docs_construct_names_test.go,
# product_neutrality_test.go, embed_inventory_test.go, area_graph_dag_test.go,
# ...) that shell out to `git ls-files` or `go list` and then read whatever
# they return. Go's test cache records the files a test OPENS, but it cannot
# record the SET a subprocess returned -- so adding a new `.md` leaves the
# cache key unchanged and the gate reports a stale green over a file it never
# looked at.
#
# That is precisely the fail-open memql#2972 was filed to close, so the root
# package is always run with -count=1 in its own step rather than being made
# cacheable. Everything else keys correctly on its own sources.
print_complement_cacheable() {
	local full
	# `|| return $?` is load-bearing, and the comment it replaces was FALSE
	# (memql#3165 review). It claimed print_complement "fails closed on its
	# own; `set -e` propagates". It does not: bash SUSPENDS `set -e` inside the
	# command-substitution subshell, so only an explicit `return N` crosses the
	# boundary. Every implicit `set -e` abort inside print_complement was
	# swallowed here, and this mode -- the one ci.yml actually calls -- printed
	# a full-looking list at exit 0 while `--complement` refused. Reproduced
	# with a `go` stub whose `go list -m` reports a renamed module:
	#
	#	before  --complement exit=4 / --complement-cacheable exit=0, 169 lines
	#	after   both exit 4, nothing printed
	#
	# print_complement now states an explicit return for each of its guards
	# too, so the propagation is correct on both sides of this boundary rather
	# than only at the seam.
	full="$(print_complement)" || return $?

	local out status=0
	out="$(printf '%s\n' "${full}" | grep -Fxv "${MODULE_PATH}")" || status=$?
	if ((status != 0)); then
		echo "db-gated-packages.sh: subtracting the root package FAILED (exit ${status})." >&2
		echo "  See print_complement." >&2
		return 5
	fi

	# Assert the subtraction removed EXACTLY the root package. `grep -Fxv`
	# removing nothing is silent, so an absent root package -- the tell for a
	# truncated `go list` -- would otherwise pass through as a list the caller
	# is told is "the complement minus the root package" when it is not.
	local full_count out_count
	full_count="$(count_lines "${full}")"
	out_count="$(count_lines "${out}")"
	if ((out_count != full_count - 1)); then
		echo "db-gated-packages.sh: expected the cacheable complement to be the complement" >&2
		echo "  minus exactly the root package '${MODULE_PATH}' (${full_count} -> $((full_count - 1)))," >&2
		echo "  got ${out_count}. Either the root package is missing from the complement (a" >&2
		echo "  truncated 'go list') or it appears more than once. Refusing to print a list" >&2
		echo "  whose composition is not what the caller is told it is." >&2
		return 5
	fi
	printf '%s\n' "${out}"
}

main() {
	if (($# != 1)); then
		usage
		return 2
	fi
	case "$1" in
	--trees) print_trees ;;
	--patterns) print_patterns ;;
	--complement) print_complement ;;
	--complement-cacheable) print_complement_cacheable ;;
	-h | --help) usage ;;
	*)
		echo "db-gated-packages.sh: unknown mode '$1'" >&2
		usage
		return 2
		;;
	esac
}

main "$@"
