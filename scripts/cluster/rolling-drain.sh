#!/usr/bin/env bash
#
# rolling-drain.sh -- coordinated/ordered rollout driver (memql#1270,
# epic memql#1259 Phase 3).
#
# Drains a set of nodes ONE AT A TIME via the per-node operator maintenance
# trigger, so a rollout never takes all replicas of a type down at once. For
# each node, in the order given, it:
#
#   1. Sends the operator drain trigger (scripts/cluster/rolling-drain Go
#      tool) -- the node runs the SAME graceful-drain sequence the deploy
#      SIGTERM path runs (memql#1269): Draining advertised in gossip +
#      readiness 503, in-flight drained within MEMQL_SHUTDOWN_GRACE_PERIOD,
#      then Stopped + the Stop sweep.
#   2. Waits for that node to finish draining (its readiness probe goes 503,
#      then the pod is replaced / restarted by the orchestrator) AND for a
#      healthy Ready replacement to come back, before moving to the next
#      node.
#
# This keeps the rollout deterministic and gap-free: with N replicas, at
# most one is ever out of rotation. The actual pod replacement is the
# orchestrator's job (k8s rolling the Deployment/Rollout, or an operator
# restarting the process); this driver only sequences the drains and gates
# on readiness between them.
#
#   scripts/cluster/rolling-drain.sh \
#       --reason "manual roll 0.9.40" \
#       --readiness-url-template 'http://%s:8080/readyz' \
#       bff-1:50051 bff-2:50051
#
# The trailing positional args are the per-node gRPC endpoints, IN THE ORDER
# they should be drained (e.g. one replica of a type, wait for its
# replacement, then the next replica; or hub-after-dependents ordering).
#
# Env:
#   MEMQL_MASTER_KEY   required -- the cluster operator credential, passed
#                      through to the per-node trigger.
#
# Flags:
#   --reason TEXT                  operator note recorded in node logs/audit.
#   --readiness-url-template TPL   printf template taking the node host (the
#                                  endpoint with the :port stripped) and
#                                  yielding a readiness URL polled until 200.
#                                  When omitted, the driver drains + waits a
#                                  fixed settle delay instead of polling.
#   --ready-timeout SECONDS        how long to wait for a Ready replacement
#                                  per node (default 180).
#   --settle-seconds SECONDS       fixed wait when no readiness URL is given
#                                  (default 30).
#   --dry-run                      print the plan without draining anything.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REASON="operator rolling drain"
READINESS_URL_TEMPLATE=""
READY_TIMEOUT=180
SETTLE_SECONDS=30
DRY_RUN=false
ENDPOINTS=()

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
	sed -n '2,60p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

function parse_arguments() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--reason=*) REASON="${1#*=}"; shift ;;
			--reason) REASON="$2"; shift 2 ;;
			--readiness-url-template=*) READINESS_URL_TEMPLATE="${1#*=}"; shift ;;
			--readiness-url-template) READINESS_URL_TEMPLATE="$2"; shift 2 ;;
			--ready-timeout=*) READY_TIMEOUT="${1#*=}"; shift ;;
			--ready-timeout) READY_TIMEOUT="$2"; shift 2 ;;
			--settle-seconds=*) SETTLE_SECONDS="${1#*=}"; shift ;;
			--settle-seconds) SETTLE_SECONDS="$2"; shift 2 ;;
			--dry-run) DRY_RUN=true; shift ;;
			--help|-h) show_help; exit 0 ;;
			--*) echo "ERROR: unknown flag: $1" >&2; show_help; exit 1 ;;
			*) ENDPOINTS+=("$1"); shift ;;
		esac
	done
}

function validate_arguments() {
	if [[ ${#ENDPOINTS[@]} -eq 0 ]]; then
		echo "ERROR: at least one node endpoint is required" >&2
		show_help
		exit 1
	fi
	if [[ -z "${MEMQL_MASTER_KEY:-}" ]]; then
		echo "ERROR: MEMQL_MASTER_KEY must be set (the cluster operator credential)" >&2
		exit 1
	fi
}

# host_of strips the :port from an endpoint so the readiness template can
# target the node's HTTP probe (a different port than gRPC).
function host_of() {
	local ep="$1"
	ep="${ep#http://}"
	ep="${ep#https://}"
	echo "${ep%%:*}"
}

# drain_one fires the per-node operator trigger via the Go tool.
function drain_one() {
	local endpoint="$1"
	echo "  -> draining ${endpoint} ..."
	if [[ "${DRY_RUN}" == true ]]; then
		echo "     [dry-run] would: go run ./scripts/cluster/rolling-drain --endpoint=${endpoint} --reason=\"${REASON}\""
		return 0
	fi
	( cd "${REPO_ROOT}" && \
		GOWORK=off go run ./scripts/cluster/rolling-drain \
			--endpoint="${endpoint}" \
			--reason="${REASON}" )
}

# wait_for_ready blocks until the node's readiness probe returns 200 again
# (a healthy replacement is up) or the timeout elapses. When no readiness
# URL template is configured, it waits a fixed settle delay instead.
function wait_for_ready() {
	local endpoint="$1"
	if [[ -z "${READINESS_URL_TEMPLATE}" ]]; then
		echo "  -> no --readiness-url-template; settling ${SETTLE_SECONDS}s before next node"
		[[ "${DRY_RUN}" == true ]] || sleep "${SETTLE_SECONDS}"
		return 0
	fi

	local host url deadline
	host="$(host_of "${endpoint}")"
	# shellcheck disable=SC2059
	url="$(printf "${READINESS_URL_TEMPLATE}" "${host}")"
	echo "  -> waiting up to ${READY_TIMEOUT}s for a Ready replacement at ${url}"
	if [[ "${DRY_RUN}" == true ]]; then
		echo "     [dry-run] would poll ${url} for HTTP 200"
		return 0
	fi

	deadline=$(( $(date +%s) + READY_TIMEOUT ))
	while [[ $(date +%s) -lt ${deadline} ]]; do
		if curl -fsS -o /dev/null --max-time 5 "${url}" 2>/dev/null; then
			echo "  -> ${url} is Ready"
			return 0
		fi
		sleep 3
	done
	echo "ERROR: timed out waiting for a Ready replacement at ${url}" >&2
	return 1
}

function execute_rollout() {
	echo "========================================="
	echo "Coordinated rolling drain (memql#1270)"
	echo "========================================="
	echo "Nodes (in order): ${ENDPOINTS[*]}"
	echo "Reason:           ${REASON}"
	echo "Dry run:          ${DRY_RUN}"
	echo "========================================="

	local ep
	for ep in "${ENDPOINTS[@]}"; do
		echo ""
		echo "Node: ${ep}"
		drain_one "${ep}"
		wait_for_ready "${ep}"
		echo "  -> ${ep} done; proceeding to next node"
	done

	echo ""
	echo "SUCCESS: all ${#ENDPOINTS[@]} node(s) drained in order, one at a time."
}

function main() {
	parse_arguments "$@"
	validate_arguments
	execute_rollout
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
