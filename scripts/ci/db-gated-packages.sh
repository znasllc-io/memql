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

# print_complement lists every package in the module except those under a
# db-gated tree.
#
# Anchored with a leading `/` and matched to a `/` or end-of-string boundary so
# a future package named e.g. `component/memqlx` is not swallowed by the
# `component/memql` entry.
print_complement() {
	local module pattern=""
	module="$(go list -m)"
	local t
	for t in "${DB_GATED_TREES[@]}"; do
		pattern+="${pattern:+|}^${module}/${t}(/|\$)"
	done
	go list ./... | grep -Ev "${pattern}"
}

# print_complement_cacheable is the complement MINUS the module's root package.
#
# The root package hosts seven repo-sweeping gates
# (docs_construct_names_test.go, product_neutrality_test.go, ...) that shell out
# to `git ls-files` and then read whatever it returns. Go's test cache records
# the files a test OPENS, but it cannot record the SET a subprocess returned --
# so adding a new `.md` leaves the cache key unchanged and the gate reports a
# stale green over a file it never looked at.
#
# That is precisely the fail-open memql#2972 was filed to close, so the root
# package is always run with -count=1 in its own step rather than being made
# cacheable. Everything else keys correctly on its own sources.
print_complement_cacheable() {
	local module
	module="$(go list -m)"
	print_complement | grep -Fxv "${module}"
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
