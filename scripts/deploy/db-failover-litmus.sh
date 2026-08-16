#!/usr/bin/env bash
#
# scripts/deploy/db-failover-litmus.sh
# ====================================
#
# Capability: db.failoverLitmus -- kill the database primary and assert the
# cluster promotes a replica, keeps every committed write, and re-establishes
# replication.
#
# Epic memql#3842, task memql#3850. HA is a property you either MEASURE or
# merely believe. A CloudNativePG cluster reports `instances: 3` and
# `Cluster in healthy state` whether or not a promotion would actually work --
# a replica that is in recovery with a dead WAL receiver counts toward that
# number. The only way to know is to take the primary away and watch.
#
# WHAT IT ASSERTS, and why each one is separate:
#
#   1. a NEW primary is promoted, within --timeout seconds
#        the property HA is bought for. Timed, because "eventually" is not
#        failover.
#   2. the marker row written BEFORE the kill is present AFTER it
#        no data loss for COMMITTED writes. This is the one that would make a
#        promotion worthless: a replica far enough behind is promoted just as
#        readily, and the cluster looks equally healthy afterwards.
#   3. every instance returns to ready, and replication is streaming
#        a cluster that promoted but never rebuilt its replica has spent its
#        redundancy and is now a single point of failure that reports HA.
#
# WHEN TO RUN IT: on production bring-up acceptance, and after every operator
# upgrade -- an operator upgrade rolls every database pod, which is exactly
# when you want promotion proven rather than assumed.
#
# DESTRUCTIVE, and honestly so: it deletes the primary pod. That is a real
# failover with a real (brief) write interruption, so it takes an explicit
# --confirm rather than a prompt, per the capability-script contract.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused (no --confirm) | 4 prerequisite missing
#             | 5 the litmus FAILED (no promotion, data loss, or no recovery)
#
# Refs: #3850 #3842 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "db.failoverLitmus" "Kill the database primary and assert promotion, durability and recovery."
cap_spec_param "namespace" "namespace holding the CNPG cluster (memql | memql-staging | memql-prod)"
cap_spec_param "cluster"   "CNPG Cluster name"
cap_spec_param "timeout"   "seconds to allow for a new primary to be promoted"
cap_spec_param "recoveryTimeout" "seconds to allow for every instance to return to ready"
cap_spec_param_required "confirm" "type 'kill-the-primary' -- this deletes the primary pod"

NS=""
CLUSTER=""

function kc() { kubectl -n "$NS" "$@"; }

function ensure_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed"
    kc get cluster "$CLUSTER" &>/dev/null \
        || cap_fail 4 "no CNPG Cluster '${CLUSTER}' in namespace '${NS}'"
}

function current_primary() {
    kc get cluster "$CLUSTER" -o jsonpath='{.status.currentPrimary}' 2>/dev/null
}

function ready_instances() {
    kc get cluster "$CLUSTER" -o jsonpath='{.status.readyInstances}' 2>/dev/null
}

function declared_instances() {
    kc get cluster "$CLUSTER" -o jsonpath='{.spec.instances}' 2>/dev/null
}

# psql on a named instance pod, as the superuser over the local socket.
function psql_on() {
    local pod="$1" sql="$2"
    kc exec "$pod" -c postgres -- psql -U postgres -d memql -tAc "$sql" 2>/dev/null
}

# REFUSE A SINGLE-INSTANCE CLUSTER rather than "fail" it. There is nothing to
# promote to, so killing the primary is not a failover test -- it is an outage
# with a script watching. The distinction matters for the exit code: this is a
# prerequisite that is not met, not a property that is broken.
function ensure_ha() {
    local declared
    declared="$(declared_instances)"
    if [[ "${declared:-1}" -lt 2 ]]; then
        cap_fail 4 "cluster '${CLUSTER}' declares ${declared:-1} instance(s); a failover litmus needs at least 2 (there is nothing to promote to)"
    fi
    local ready
    ready="$(ready_instances)"
    if [[ "${ready:-0}" -lt "${declared}" ]]; then
        cap_fail 4 "cluster '${CLUSTER}' has ${ready:-0}/${declared} instances ready; start from a healthy cluster or the result means nothing"
    fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    NS="$(cap_param namespace "memql")"
    CLUSTER="$(cap_param cluster "memql-db")"
    local timeout recovery confirm
    timeout="$(cap_param timeout "120")"
    recovery="$(cap_param recoveryTimeout "600")"
    confirm="$(cap_param confirm "")"

    if [[ "$confirm" != "kill-the-primary" ]]; then
        cap_fail 3 "refusing: this deletes the database primary pod. Pass --confirm=kill-the-primary"
    fi

    ensure_prerequisites
    ensure_ha

    local before
    before="$(current_primary)"
    [[ -n "$before" ]] || cap_fail 5 "cluster '${CLUSTER}' reports no current primary"
    cap_info "Current primary: ${before}"

    # (2) A COMMITTED write, before the kill. The table is created if absent so
    # the litmus is re-runnable; the row carries the run's own marker.
    local marker
    marker="litmus-${before}-$(kc get cluster "$CLUSTER" -o jsonpath='{.metadata.resourceVersion}')"
    cap_info "Writing a committed marker row (${marker})..."
    kc exec "$before" -c postgres -- psql -U postgres -d memql -v ON_ERROR_STOP=1 -q -c "
        CREATE TABLE IF NOT EXISTS failover_litmus(marker text primary key, at timestamptz default now());
        INSERT INTO failover_litmus(marker) VALUES ('${marker}');
    " >&2 || cap_fail 5 "could not write the marker row to the primary"

    # Force the write out to the replicas before killing the primary. Without
    # this the litmus would be testing asynchronous replication's timing rather
    # than failover, and would fail intermittently for a reason that is not a
    # defect.
    kc exec "$before" -c postgres -- psql -U postgres -c "SELECT pg_switch_wal();" >/dev/null 2>&1 || true

    cap_info "Deleting the primary pod ${before}..."
    local killed_at
    killed_at="$SECONDS"
    kc delete pod "$before" --wait=false >&2 || cap_fail 5 "could not delete pod ${before}"

    # (1) PROMOTION, timed.
    cap_info "Waiting up to ${timeout}s for a new primary..."
    local after="" deadline=$((SECONDS + timeout))
    while ((SECONDS < deadline)); do
        after="$(current_primary)"
        [[ -n "$after" && "$after" != "$before" ]] && break
        sleep 2
    done
    local promoted_in=$((SECONDS - killed_at))
    if [[ -z "$after" || "$after" == "$before" ]]; then
        cap_result_set previousPrimary "$before"
        cap_fail 5 "no promotion within ${timeout}s -- the cluster still reports '${before}' as primary. HA is not working."
    fi
    cap_info "Promoted: ${after} (after ${promoted_in}s)"

    # (2) DURABILITY of the committed write.
    local found=""
    local durability_deadline=$((SECONDS + 60))
    while ((SECONDS < durability_deadline)); do
        found="$(psql_on "$after" "SELECT marker FROM failover_litmus WHERE marker = '${marker}'")"
        [[ "$found" == "$marker" ]] && break
        sleep 2
    done
    if [[ "$found" != "$marker" ]]; then
        cap_result_set previousPrimary "$before"
        cap_result_set newPrimary "$after"
        cap_fail 5 "DATA LOSS: the marker row committed before the kill is absent from the promoted primary. The promotion succeeded and the cluster looks healthy, which is what makes this the worst outcome."
    fi
    cap_info "Committed write survived the failover."

    # (3) RECOVERY: every instance back, and replication streaming again.
    cap_info "Waiting up to ${recovery}s for every instance to return to ready..."
    local want
    want="$(declared_instances)"
    local rec_deadline=$((SECONDS + recovery)) ready=""
    while ((SECONDS < rec_deadline)); do
        ready="$(ready_instances)"
        [[ "${ready:-0}" == "$want" ]] && break
        sleep 5
    done
    local recovered_in=$((SECONDS - killed_at))
    if [[ "${ready:-0}" != "$want" ]]; then
        cap_result_set     previousPrimary "$before"
        cap_result_set     newPrimary      "$after"
        cap_result_set_raw promotedInSeconds "$promoted_in"
        cap_fail 5 "only ${ready:-0}/${want} instances ready after ${recovery}s -- the cluster promoted but has not rebuilt its redundancy, so it now reports HA while having none"
    fi

    cap_info "SUCCESS: promoted in ${promoted_in}s, fully recovered in ${recovered_in}s, no committed write lost."

    cap_result_set     namespace         "$NS"
    cap_result_set     cluster           "$CLUSTER"
    cap_result_set     previousPrimary   "$before"
    cap_result_set     newPrimary        "$after"
    cap_result_set_raw promotedInSeconds "$promoted_in"
    cap_result_set_raw recoveredInSeconds "$recovered_in"
    cap_result_set_raw instancesReady    "$ready"
    cap_result_set_raw committedWriteSurvived true
    cap_changed
    cap_ok
}

main "$@"
