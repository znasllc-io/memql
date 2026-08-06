#!/usr/bin/env bash
# Bounded retry for NETWORK operations in CI (znasllc-io/memql#3166).
#
# Design D4.3: retry `apt-get`, the protoc download, and `make sdk-ts-install`
# -- and NOTHING that runs tests. Retrying a test is how a flake becomes
# invisible, so this script is deliberately a general runner with a specific
# documented use: if you find yourself wrapping a `go test` in it, stop.
#
# Implemented as a bash loop rather than a marketplace retry action on purpose.
# Every `uses:` is a call to the action-download service that produced the
# `abandoned` job failures on 2026-08-06 (audit 4.4), and D4.2 asks to REDUCE
# action resolutions per job. Adding an action to improve network resilience
# would have been self-defeating.
#
# A retried-then-passed attempt is announced loudly on stderr rather than
# passing silently: a step that needed three attempts is a signal about the
# runner or the upstream service, and swallowing it turns a degrading
# dependency into a mystery later.
set -euo pipefail

readonly DEFAULT_ATTEMPTS=3
readonly DEFAULT_DELAY_SECONDS=5

usage() {
	cat >&2 <<'EOF'
usage: retry.sh [--attempts=N] [--delay=SECONDS] -- <command> [args...]

Runs <command>, retrying on non-zero exit with a fixed delay between attempts.
For network operations only -- never wrap a test command in this.
EOF
}

# run_with_retry executes "$@", retrying up to $attempts times.
run_with_retry() {
	local attempts="$1" delay="$2"
	shift 2

	local n=1 status
	while true; do
		# Capture the command's own status explicitly. `if "$@"; then ... fi`
		# followed by `$?` reads 0 when the condition is FALSE and there is no
		# else branch -- the `if` construct's own status, not the command's --
		# which silently turned every failure into a success here.
		status=0
		"$@" || status=$?

		if ((status == 0)); then
			if ((n > 1)); then
				echo "retry.sh: SUCCEEDED on attempt ${n}/${attempts}: $*" >&2
				echo "retry.sh: a network op that needed ${n} attempts is a signal, not noise." >&2
			fi
			return 0
		fi

		if ((n >= attempts)); then
			echo "retry.sh: FAILED after ${attempts} attempt(s) (exit ${status}): $*" >&2
			return "${status}"
		fi
		echo "retry.sh: attempt ${n}/${attempts} failed (exit ${status}); retrying in ${delay}s: $*" >&2
		sleep "${delay}"
		n=$((n + 1))
	done
}

main() {
	local attempts="${DEFAULT_ATTEMPTS}" delay="${DEFAULT_DELAY_SECONDS}"

	while (($# > 0)); do
		case "$1" in
		--attempts=*)
			attempts="${1#*=}"
			shift
			;;
		--delay=*)
			delay="${1#*=}"
			shift
			;;
		--)
			shift
			break
			;;
		-h | --help)
			usage
			return 0
			;;
		*)
			echo "retry.sh: unexpected argument '$1' (did you forget '--'?)" >&2
			usage
			return 2
			;;
		esac
	done

	if (($# == 0)); then
		echo "retry.sh: no command given" >&2
		usage
		return 2
	fi

	run_with_retry "${attempts}" "${delay}" "$@"
}

main "$@"
