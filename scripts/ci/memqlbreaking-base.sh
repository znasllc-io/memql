#!/usr/bin/env bash
#
# scripts/ci/memqlbreaking-base.sh
# ================================
#
# Hold this tree's authoring surface to the BASE COMMIT's baseline, not to its
# own (memql#5389, D21).
#
# WHY THIS EXISTS. `make memqlbreaking` runs
#
#     go run ./cmd/memqlbreaking -baseline component/language/surface/2026.json
#
# and that path -- the one CI ran -- reads the baseline out of the very tree it
# is judging. Both inputs are therefore editable by the change under test, so a
# change that deletes a name from the source AND deletes its entry from
# component/language/surface/2026.json sees no deletion at all. Measured on
# 0e70bc702, deleting `daysBetween` from the function catalog and from the
# committed baseline, with no entry in component/language/reserved.json:
#
#     SUCCESS: no breaks against component/language/surface/2026.json
#     exit=0
#
# The reservation the ledger exists to force into the open was never asked for.
# Editing both sides in lockstep is exactly the move the gate is against, and it
# was the one move that walked straight through it.
#
# WHAT THIS DOES INSTEAD. It extracts the base commit's copies of the two
# committed files and holds the tree's live capture to those. The head's ledger
# is still the tree's own -- a deliberate deletion lands by ADDING a reservation
# in the same change, so the ledger that accepts it has to be the one the change
# writes. The base's ledger is passed separately, as -base-reserved, for the one
# question the head's cannot answer: was this name already spent before?
#
# WHAT IT DELIBERATELY DOES NOT DO. It makes no decision about the comparison.
# It resolves two files and hands them to the tool; every classification, every
# refusal and the exit code are the tool's.
#
#     BASE_SHA        the commit to hold this tree to. Empty, unset or all
#                     zeros (a first push to a branch, a new ref) means "no base
#                     commit", and the run falls back to the committed baseline
#                     -- which is today's behaviour, announced on stderr rather
#                     than taken silently.
#
# Exit codes are the tool's, and its vocabulary is reused for this script's own
# failures so there is one set to read:
#
#     0  no breaks
#     1  breaks found
#     2  the comparison could not be made (bad usage, or the base commit could
#        not be fetched -- NEVER a silent fall back to the committed baseline,
#        which is the bypass)
set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# The two committed files, relative to the repo root. They are the tool's
# ReservedPath and BaselinePath; a disagreement would extract the wrong file and
# report a break against a baseline nobody committed, so scripts/ci's test pins
# these two literals to the Go constants.
readonly SURFACE_REL="component/language/surface/2026.json"
readonly LEDGER_REL="component/language/reserved.json"

# What a base commit with no ledger yet is held to. Empty rather than the
# committed one: an empty base ledger reserves nothing, so it reports nothing,
# while the committed one would be the same lockstep-editable file this script
# exists to stop reading.
readonly EMPTY_LEDGER='{"readme":["The base commit carried no reservation ledger."],"annotations":{},"constructs":{},"functions":{}}'

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
	cat >&2 <<'EOF'
Usage: BASE_SHA=<sha> scripts/ci/memqlbreaking-base.sh

Runs cmd/memqlbreaking against the BASE COMMIT's copies of
component/language/surface/2026.json and component/language/reserved.json, so a
change that edits the committed baseline in lockstep with the source hides no
removal.

Environment:
    BASE_SHA    the commit to hold this tree to. Empty, unset or all zeros runs
                against the committed baseline instead, and says so on stderr.

Exit codes:
    0  no breaks
    1  breaks found
    2  the comparison could not be made
EOF
}

function log() {
	printf 'memqlbreaking-base.sh: %s\n' "$*" >&2
}

# repo_root answers from this script's own location rather than from the working
# directory, so the answer does not depend on where it was invoked from.
function repo_root() {
	local here
	here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
	(cd "${here}/../.." && pwd)
}

# base_sha_is_absent reports whether BASE_SHA names no commit. All zeros is how
# `github.event.before` reads on the first push to a branch, and how a deleted
# ref reads; it is an absent base, not a commit that will fail to fetch.
function base_sha_is_absent() {
	local sha="$1"
	if [[ -z "${sha}" ]]; then
		return 0
	fi
	if [[ "${sha}" =~ ^0+$ ]]; then
		return 0
	fi
	return 1
}

# base_sha_is_well_formed refuses a value git would read as an OPTION rather
# than as a commit, and one carrying whitespace. It is deliberately looser than
# 40 hex -- a ref such as `origin/main` is the useful local spelling, and git is
# the right validator for whether a name resolves -- and deliberately not a
# no-op: every use below quotes the value, so nothing can be substituted, but
# `--upload-pack=...` in refspec position is an argument git would still read.
function base_sha_is_well_formed() {
	local sha="$1"
	[[ "${sha}" =~ ^[0-9A-Za-z][0-9A-Za-z._/^~-]*$ ]]
}

# ensure_base_commit makes the base commit readable, fetching it when the
# checkout is shallow (actions/checkout takes one commit by default). It does
# not fall back: a base commit that cannot be read is the whole comparison, and
# quietly reverting to the committed baseline would restore the bypass while
# reporting success.
function ensure_base_commit() {
	local sha="$1"
	if git cat-file -e "${sha}^{commit}" 2>/dev/null; then
		log "the base commit ${sha} is already in this checkout"
		return 0
	fi
	log "fetching the base commit ${sha} (a CI checkout is shallow)"
	if git fetch --no-tags --depth=1 origin "${sha}" >&2; then
		return 0
	fi
	return 1
}

# extract_base_file writes one of the base commit's files into workdir, and
# answers whether the base commit had it. A first-time baseline is a base commit
# that predates the file: the caller falls back and says so, because there is
# nothing to hold the tree to and refusing would red every build until the
# baseline's own commit is the base.
function extract_base_file() {
	local sha="$1" rel="$2" dest="$3"
	git show "${sha}:${rel}" >"${dest}" 2>/dev/null
}

# resolve_base fills workdir with the base commit's two files, falling back per
# file, and writes the label the report will name the baseline by.
#
# The label is resolved here rather than composed by the caller because only
# this function knows which file the run actually got. A fallback run that still
# announced "the base commit <sha>" would put a claim in the report that is not
# true of the comparison it made -- the same defect, one level up, as the gate
# this script replaces.
function resolve_base() {
	local sha="$1" root="$2" workdir="$3"

	if extract_base_file "${sha}" "${SURFACE_REL}" "${workdir}/surface.json"; then
		log "holding this tree to ${SURFACE_REL} as the base commit ${sha} had it"
		printf 'the base commit %s (%s as it stood there)\n' "${sha}" "${SURFACE_REL}" >"${workdir}/label"
	else
		cp "${root}/${SURFACE_REL}" "${workdir}/surface.json"
		log "WARNING: the base commit ${sha} has no ${SURFACE_REL}, so this run falls back to the COMMITTED"
		log "WARNING: baseline -- the same file this change may edit. A removal edited into both sides would"
		log "WARNING: pass here. This is the first-baseline case and it stops once that commit is the base."
		printf '%s -- the COMMITTED baseline, because the base commit %s predates it\n' \
			"${SURFACE_REL}" "${sha}" >"${workdir}/label"
	fi

	if extract_base_file "${sha}" "${LEDGER_REL}" "${workdir}/reserved.json"; then
		log "reading ${LEDGER_REL} as the base commit ${sha} had it"
	else
		printf '%s\n' "${EMPTY_LEDGER}" >"${workdir}/reserved.json"
		log "the base commit ${sha} has no ${LEDGER_REL}; reading an EMPTY base ledger, which reserves nothing"
	fi
}

# run_against_committed is today's behaviour, kept for the no-base case and
# announced rather than assumed.
function run_against_committed() {
	local root="$1" status=0
	log "no base commit to hold this tree to: comparing against the COMMITTED baseline, ${SURFACE_REL}."
	log "A change that edits that file in lockstep with the source is NOT caught on this path."
	(cd "${root}" && go run ./cmd/memqlbreaking -baseline "${SURFACE_REL}") || status=$?
	return "${status}"
}

# run_against_base is the gate. -baseline and -base-reserved are the base
# commit's copies; -reserved is left at its default, the tree's own ledger,
# because a deliberate deletion is landed by adding a reservation in the change
# that makes it.
function run_against_base() {
	local root="$1" workdir="$2" status=0
	(cd "${root}" && go run ./cmd/memqlbreaking \
		-baseline "${workdir}/surface.json" \
		-base-reserved "${workdir}/reserved.json" \
		-base-label "$(cat "${workdir}/label")") || status=$?
	return "${status}"
}

function main() {
	if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
		show_help
		return 0
	fi
	if (($# > 0)); then
		log "unexpected argument '$1'; every input is an environment variable"
		show_help
		return 2
	fi

	local root
	root="$(repo_root)"

	local base_sha="${BASE_SHA:-}"
	if base_sha_is_absent "${base_sha}"; then
		run_against_committed "${root}"
		return $?
	fi
	if ! base_sha_is_well_formed "${base_sha}"; then
		log "ERROR: BASE_SHA='${base_sha}' is not a commit name. Pass a sha, or a ref such as origin/main."
		return 2
	fi

	if ! (cd "${root}" && ensure_base_commit "${base_sha}"); then
		log "ERROR: the base commit ${base_sha} could not be read or fetched from origin."
		log "ERROR: refusing to fall back to the committed baseline -- that is the comparison this step"
		log "ERROR: exists to avoid, and it would report success over a removal nothing checked."
		return 2
	fi

	local workdir
	workdir="$(mktemp -d)"
	# shellcheck disable=SC2064 # workdir is expanded now on purpose: the trap
	# must name the directory this run made, not whatever the variable holds later.
	trap "rm -rf '${workdir}'" EXIT

	(cd "${root}" && resolve_base "${base_sha}" "${root}" "${workdir}")

	run_against_base "${root}" "${workdir}"
	return $?
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
