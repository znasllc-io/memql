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

# MIN_COMPLEMENT is the floor the complement must clear (memql#3165 review).
#
# Measured at 170 packages (169 cacheable, i.e. minus the root package) on
# 2026-08-06. Re-measure with:
#
#	scripts/ci/db-gated-packages.sh --complement | wc -l
#
# BE PRECISE ABOUT WHAT THIS CATCHES, because a floor is a coarse instrument
# and pretending otherwise is how a guard gets trusted past its range.
#
# It catches COLLAPSE and gross TRUNCATION -- the shapes where the selector
# stops covering the repo and `go test` silently runs a fraction of the suite.
# A `go.mod` under component/language, component/database, integrations/ or
# app/ removes 15-34 packages from the module pattern with exit 0 and no
# diagnostic; measured, those land at 154/146/135/152 and the three largest
# trip this floor. It does NOT catch a boundary that removes one or two
# packages -- nothing countable can, which is why it is not the only guard:
# TestAreaGraphIsADAG pins the area SET (any missing directory is red) and
# TestEmbeddedFileCountsAreStable pins per-package embed counts.
#
# Headroom is ~12%. Deliberately not tighter: a floor that fires on ordinary
# package deletion gets lowered by whoever is unlucky enough to hit it under
# time pressure, and a guard that gets edited away is worth less than a looser
# one that never cries wolf.
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
	verify_module_path
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
		echo "  Usual cause: a 'go.mod' added beneath the repo root. Every '...'-shaped" >&2
		echo "  selector then silently drops everything inside the new module, with exit 0" >&2
		echo "  and no diagnostic. Check with:" >&2
		echo "      git ls-files | grep '/go.mod\$'" >&2
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
	full="$(print_complement)" # fails closed on its own; `set -e` propagates

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
