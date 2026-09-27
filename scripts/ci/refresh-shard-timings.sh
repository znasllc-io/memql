#!/usr/bin/env bash
# Rewrite scripts/ci/shard-timings.tsv from recent full runs on main
# (memql#5485, design record 2026-09-16 section 4, epic 1).
#
# The timing table is DATA the planner (scripts/ci/affected) balances the
# go-tests and db-tests shards with. The design record's pipeline model
# rewrites it after every full run; GitHub Actions cannot push to a protected
# main, so on the bridge it is refreshed by running this script and landing
# the diff in a pull request. A stale table never breaks anything -- a
# package it does not know lands in the lightest shard -- it only balances
# worse, and the plan job's summary shows the drift as "unmeasured" counts.
#
# It reads the logs of the SHARD jobs only: `go-tests (<shard>)` for the go
# lane and `db-tests (<shard>)` for the db lane. The node-tag passes
# (`go-tests -tags <tag>`) re-run app and component/node under a tag and would
# teach the table a second, different number for the same package, so their
# names are deliberately not matched.
#
# usage: scripts/ci/refresh-shard-timings.sh [--runs=N] [--repo=OWNER/NAME]
#   --runs  how many recent green push runs of ci.yml on main to read (default 5)
#   --repo  the repository (default znasllc-io/memql)
#
# Needs gh (authenticated, read access) and go. Writes the table in place;
# review and commit the diff like any other change.
set -euo pipefail

readonly TABLE="scripts/ci/shard-timings.tsv"

usage() {
	sed -n '2,/^set -euo pipefail/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
}

repo_root() {
	cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd
}

# fetch_logs writes one log file per green shard job of the last $runs green
# push runs, named <lane>-<run>-<job>.log, into $dir.
fetch_logs() {
	local repo="$1" runs="$2" dir="$3"
	local run_ids run id name lane
	run_ids="$(gh run list --repo "$repo" --workflow ci.yml --branch main --event push \
		--status success --limit "$runs" --json databaseId --jq '.[].databaseId')"
	if [[ -z "$run_ids" ]]; then
		echo "refresh-shard-timings: no green push run of ci.yml on main in $repo" >&2
		return 5
	fi
	for run in $run_ids; do
		while IFS=$'\t' read -r id name; do
			[[ -z "$id" ]] && continue
			lane="go"
			if [[ "$name" == db-tests* ]]; then
				lane="db"
			fi
			gh api "repos/$repo/actions/jobs/$id/logs" >"$dir/$lane-$run-$id.log"
		done < <(gh api "repos/$repo/actions/runs/$run/jobs?per_page=100" --jq \
			'.jobs[] | select(.conclusion == "success") | select((.name | startswith("go-tests (")) or (.name | startswith("db-tests ("))) | "\(.id)\t\(.name)"')
	done
}

# merge_lane feeds one lane's logs to the planner's timings command, which
# refuses (and leaves the table untouched) when they hold no result line.
merge_lane() {
	local lane="$1" dir="$2"
	local -a logs=()
	local f
	for f in "$dir/$lane"-*.log; do
		[[ -e "$f" ]] && logs+=("$f")
	done
	if ((${#logs[@]} == 0)); then
		echo "refresh-shard-timings: no $lane-lane shard logs found; the runs predate the shard jobs, or none was green" >&2
		return 5
	fi
	go run ./scripts/ci/affected timings --lane="$lane" --table="$TABLE" "${logs[@]}"
}

main() {
	local runs=5 repo="znasllc-io/memql" arg
	for arg in "$@"; do
		case "$arg" in
		--runs=*) runs="${arg#--runs=}" ;;
		--repo=*) repo="${arg#--repo=}" ;;
		-h | --help)
			usage
			return 0
			;;
		*)
			echo "refresh-shard-timings: unknown argument '$arg'" >&2
			usage
			return 2
			;;
		esac
	done
	if ! [[ "$runs" =~ ^[1-9][0-9]*$ ]]; then
		echo "refresh-shard-timings: --runs must be a positive whole number, got '$runs'" >&2
		return 2
	fi

	cd "$(repo_root)"
	local dir
	dir="$(mktemp -d)"
	trap 'rm -rf "$dir"' EXIT

	fetch_logs "$repo" "$runs" "$dir"
	merge_lane go "$dir"
	merge_lane db "$dir"
	echo "refresh-shard-timings: $TABLE rewritten from the last $runs green push run(s); review the diff and commit it" >&2
}

main "$@"
