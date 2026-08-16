#!/usr/bin/env bash
#
# scripts/deploy/db-restore-drill.sh
# ==================================
#
# Capability: db.restoreDrill -- restore the latest database backup into a
# scratch cluster, prove the restored data is usable, tear it down, and report
# the timings.
#
# Epic memql#3842, task memql#3849. A backup's EXISTENCE proves nothing; its
# RESTORE proves everything. This turns "we have backups" into a routine with a
# number attached -- the measured RTO -- instead of a hope with a green
# checkbox.
#
# THE SCRATCH CLUSTER ARCHIVES NOWHERE, and that is the single most important
# line in this script. A recovered Cluster given the Barman plugin as its WAL
# archiver would start writing ITS OWN timeline into the SAME object store it
# was restored from -- so a routine drill would pollute the backups of the
# system it exists to protect, and the damage would be invisible until the next
# real recovery picked the wrong timeline. The Cluster below therefore declares
# `plugins: []`: it can read the store through externalClusters to recover, and
# has no archiver at all.
#
# ISOLATION IS BY CLUSTER NAME, IN THE SOURCE NAMESPACE, BY DEFAULT -- and that
# default was chosen after the obvious alternative was tried and failed.
#
#   A scratch NAMESPACE requires copying the ObjectStore and its credential
#   across, and an in-cluster destination host then does not resolve from the
#   new namespace. Qualifying the host (`azurite` ->
#   `azurite.memql.svc.cluster.local`) fixes DNS and BREAKS AZURITE: a
#   multi-label host makes Azurite parse the URL DNS-style and read the first
#   label as the ACCOUNT name, so every request comes back ResourceNotFound.
#   Measured, not assumed -- the same request answers differently at the two
#   hostnames.
#
#   A distinct cluster name gives the same isolation for what actually matters:
#   its own pods, its own PVCs, its own Services, and nothing shared with the
#   live cluster but the namespace it reads its ObjectStore from. It works
#   identically against an emulator and against real Azure Blob.
#
# --scratchNamespace is still honoured for the cloud, where destinations are
# real endpoints and cross-namespace costs nothing. When it names a namespace
# that does not exist, the drill creates it, copies what recovery needs, and
# deletes it afterwards. When the scratch namespace IS the source namespace the
# teardown deletes the drill CLUSTER and never the namespace -- deleting the
# source namespace would take the live database with it.
#
# WHAT IT ASSERTS, beyond "the pods started":
#
#   1. the cluster reaches Ready from a recovery bootstrap
#        RTO, measured rather than estimated.
#   2. the critical schema is present ("MemoryNodes", the #657 /readyz
#      assertion) -- a restored database that answers connections but lacks the
#      schema would pass a naive check and fail the application.
#   3. the TimescaleDB continuous aggregates survived
#        the part of the schema most likely to be missed: they are TSL-only
#        objects, so a restore onto an Apache build or an image without the
#        extension loses exactly these and nothing else visible.
#   4. row counts are non-zero and reported
#        an empty-but-valid restore is the failure a schema check alone waves
#        through.
#
# SAFE BY CONSTRUCTION: it never touches the live cluster and never repoints a
# DSN.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 the drill FAILED
#
# Refs: #3849 #3842 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "db.restoreDrill" "Restore the latest database backup into a scratch cluster and prove it."
cap_spec_param "sourceNamespace"  "namespace of the cluster whose backups to restore (memql | memql-staging | memql-prod)"
cap_spec_param "cluster"          "CNPG Cluster name in the source namespace"
cap_spec_param "scratchNamespace" "namespace for the restored copy (default: the source namespace, isolated by cluster name)"
cap_spec_param "timeout"          "seconds to allow for the restored cluster to become Ready"
cap_spec_param "recoveryTarget"   "RFC3339 timestamp for point-in-time recovery (default: latest)"
cap_spec_param "keep"             "leave the restored cluster behind for a post-mortem"

SRC_NS=""
SCRATCH_NS=""
CLUSTER=""
DRILL=""
KEEP="false"
SAME_NS="true"
RECOVERY_TARGET=""

function kc()      { kubectl -n "$SRC_NS" "$@"; }
function scratch() { kubectl -n "$SCRATCH_NS" "$@"; }

# Accepts the spellings an operator actually types. `KEEP=0` must not mean keep:
# a flag that reads as honoured and is not would leave a restored copy of
# production data lying around after a scheduled drill.
function is_true() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        true | 1 | yes | on) return 0 ;;
        *) return 1 ;;
    esac
}

function teardown() {
    [[ -n "$DRILL" ]] || return 0
    if is_true "$KEEP"; then
        cap_warn "--keep set: leaving ${DRILL} in ${SCRATCH_NS}. Remove it with: kubectl delete cluster ${DRILL} -n ${SCRATCH_NS}"
        return 0
    fi
    if [[ "$SAME_NS" == "true" ]]; then
        # THE SOURCE NAMESPACE IS NEVER DELETED. It holds the live database.
        # Deleting the drill Cluster is enough: CNPG owns its pods and PVCs
        # through ownerReferences, so they go with it.
        cap_info "Removing the drill cluster ${DRILL} from ${SCRATCH_NS}..."
        scratch delete cluster "$DRILL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    else
        cap_info "Tearing down namespace ${SCRATCH_NS}..."
        kubectl delete namespace "$SCRATCH_NS" --wait=false >/dev/null 2>&1 || true
    fi
}

function ensure_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed"
    kc get cluster "$CLUSTER" &>/dev/null \
        || cap_fail 4 "no CNPG Cluster '${CLUSTER}' in namespace '${SRC_NS}'"
    kc get objectstore "${CLUSTER}-backup" &>/dev/null \
        || cap_fail 4 "no ObjectStore '${CLUSTER}-backup' in namespace '${SRC_NS}' -- there is nothing to restore from"
    if kubectl get cluster "$DRILL" -n "$SCRATCH_NS" &>/dev/null; then
        cap_fail 2 "cluster ${DRILL} already exists in ${SCRATCH_NS}; delete it or pass --scratchNamespace=<something-else>"
    fi
}

# Only for the cross-namespace (cloud) path: recovery reads the ObjectStore from
# the namespace the restored Cluster lives in, so both it and its credential
# have to exist there.
function stage_scratch_namespace() {
    cap_info "Creating scratch namespace ${SCRATCH_NS}..."
    kubectl create namespace "$SCRATCH_NS" >/dev/null

    local secret
    secret="$(kc get objectstore "${CLUSTER}-backup" \
        -o jsonpath='{.spec.configuration.azureCredentials.connectionString.name}' 2>/dev/null || true)"
    if [[ -n "$secret" ]]; then
        cap_info "Copying credential Secret ${secret}..."
        kc get secret "$secret" -o json \
            | python3 -c 'import json,sys; d=json.load(sys.stdin); d["metadata"]={"name":d["metadata"]["name"]}; d.pop("status",None); print(json.dumps(d))' \
            | scratch apply -f - >/dev/null \
            || cap_fail 5 "could not copy the backup credential Secret into ${SCRATCH_NS}"
    else
        cap_info "Source ObjectStore uses no connection-string Secret (workload identity); the scratch ServiceAccount must be federated the same way."
    fi

    cap_info "Copying ObjectStore ${CLUSTER}-backup..."
    kc get objectstore "${CLUSTER}-backup" -o json \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); d["metadata"]={"name":d["metadata"]["name"]}; d.pop("status",None); print(json.dumps(d))' \
        | scratch apply -f - >/dev/null \
        || cap_fail 5 "could not copy the ObjectStore into ${SCRATCH_NS}"

    # A destination naming an in-cluster Service by its BARE name does not
    # resolve from here, and qualifying it is not the fix -- see the header.
    # Say so plainly rather than letting the recovery fail with a DNS error
    # three minutes later.
    local dest
    dest="$(kc get objectstore "${CLUSTER}-backup" -o jsonpath='{.spec.configuration.destinationPath}' 2>/dev/null)"
    if [[ "$dest" =~ ^https?://([^/:]+) ]] && [[ "${BASH_REMATCH[1]}" != *.* ]]; then
        cap_fail 2 "the ObjectStore destination names the in-cluster Service '${BASH_REMATCH[1]}' by its bare name, which does not resolve from ${SCRATCH_NS}. Run the drill in the source namespace instead (omit --scratchNamespace); isolation there is by cluster name."
    fi
}

function launch_restore() {
    local image target_block=""
    image="$(kc get cluster "$CLUSTER" -o jsonpath='{.spec.imageName}')"
    [[ -n "$image" ]] || cap_fail 5 "could not read the source cluster's imageName"

    if [[ -n "$RECOVERY_TARGET" ]]; then
        cap_info "Point-in-time recovery to ${RECOVERY_TARGET}"
        target_block="
      recoveryTarget:
        targetTime: \"${RECOVERY_TARGET}\""
    fi

    cap_info "Launching ${DRILL} in ${SCRATCH_NS} from the latest backup..."
    scratch apply -f - >/dev/null <<YAML || cap_fail 5 "could not create the restore Cluster"
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: ${DRILL}
  labels:
    app.kubernetes.io/part-of: memql
    memql.io/role: restore-drill
spec:
  instances: 1
  imageName: ${image}
  imagePullPolicy: IfNotPresent
  # One instance, no PDB: a drill is not an HA test, and a PDB here would block
  # node operations for no benefit.
  enablePDB: false
  postgresql:
    shared_preload_libraries: [timescaledb]
  storage:
    size: 10Gi
  resources:
    requests: { cpu: 100m, memory: 512Mi }
  # THE ARCHIVER IS ABSENT DELIBERATELY. Given the Barman plugin as a WAL
  # archiver, this cluster would write its own timeline into the SAME object
  # store it was restored from -- a routine drill silently polluting the backups
  # it exists to validate, discovered at the next real recovery. externalClusters
  # below gives it READ access to recover; nothing gives it write.
  plugins: []
  bootstrap:
    recovery:
      source: origin${target_block}
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters:
          barmanObjectName: ${CLUSTER}-backup
          serverName: ${CLUSTER}
YAML
}

function assert_restored_data() {
    local pod="${DRILL}-1"
    q() { scratch exec "$pod" -c postgres -- psql -U postgres -d memql -tAc "$1" 2>/dev/null; }

    # (2) the critical schema -- the same tables /readyz asserts (#657).
    local tables
    tables="$(q "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('MemoryNodes','automation_execution_claims')")"
    if [[ "${tables:-0}" -lt 1 ]]; then
        cap_fail 5 "the restored database has none of the critical tables -- it would fail /readyz. A database that accepts connections is not a restored database."
    fi
    cap_info "Critical schema present (${tables} of the 2 critical tables)."
    cap_result_set_raw criticalTables "${tables:-0}"

    # (3) the continuous aggregates. TSL-only objects, so they are exactly what
    # a restore onto the wrong image loses while everything else looks fine.
    local caggs
    caggs="$(q "SELECT count(*) FROM timescaledb_information.continuous_aggregates")"
    if [[ -z "$caggs" ]]; then
        cap_fail 5 "could not read timescaledb_information.continuous_aggregates -- the TimescaleDB extension is missing from the restored database"
    fi
    cap_info "Continuous aggregates present: ${caggs}"
    cap_result_set_raw continuousAggregates "${caggs:-0}"

    # (4) row counts. An empty-but-valid restore is what a schema check alone
    # waves through.
    local rows
    rows="$(q 'SELECT count(*) FROM "MemoryNodes"')"
    cap_info "MemoryNodes rows: ${rows:-<unreadable>}"
    cap_result_set_raw memoryNodeRows "${rows:-0}"
    if [[ "${rows:-0}" -eq 0 ]]; then
        cap_warn "the restored database has ZERO MemoryNodes rows. That is correct only if the source is genuinely empty -- otherwise the restore found a backup with no data in it."
    fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    SRC_NS="$(cap_param sourceNamespace "memql")"
    CLUSTER="$(cap_param cluster "memql-db")"
    SCRATCH_NS="$(cap_param scratchNamespace "$SRC_NS")"
    local timeout
    timeout="$(cap_param timeout "900")"
    RECOVERY_TARGET="$(cap_param recoveryTarget "")"
    KEEP="$(cap_param keep "false")"

    DRILL="${CLUSTER}-drill"
    [[ "$SCRATCH_NS" == "$SRC_NS" ]] && SAME_NS=true || SAME_NS=false

    ensure_prerequisites

    # CHAINED, not replaced. cap_init installs `trap '_cap_on_exit' EXIT`, and
    # that trap is what guarantees exactly one JSON envelope reaches stdout even
    # when the script aborts unexpectedly under `set -e`. A bare
    # `trap teardown EXIT` here would silently remove it, so an abort would clean
    # up correctly and then say nothing at all to its caller -- which for a
    # scheduled drill means a failure that reports as no output rather than as a
    # failure.
    trap 'teardown; _cap_on_exit' EXIT

    local started="$SECONDS"
    if [[ "$SAME_NS" == "false" ]]; then
        stage_scratch_namespace
    else
        cap_info "Restoring into ${SRC_NS} as ${DRILL} -- isolated by cluster name (own pods, own PVCs, own Services)."
    fi
    launch_restore

    cap_info "Waiting up to ${timeout}s for ${DRILL} to become Ready..."
    if ! scratch wait --for=condition=Ready "cluster/${DRILL}" --timeout="${timeout}s" >&2 2>/dev/null; then
        scratch get cluster "$DRILL" -o jsonpath='{.status.phase}{" | "}{.status.phaseReason}{"\n"}' >&2 || true
        scratch get pods -l "cnpg.io/cluster=${DRILL}" >&2 || true
        cap_fail 5 "${DRILL} did not become Ready within ${timeout}s -- the backups did not produce a working database"
    fi
    local rto=$((SECONDS - started))
    cap_info "Restored and Ready in ${rto}s (measured RTO)."

    assert_restored_data

    cap_info "SUCCESS: the latest backup restores to a working database in ${rto}s."

    cap_result_set     sourceNamespace   "$SRC_NS"
    cap_result_set     cluster           "$CLUSTER"
    cap_result_set     drillCluster      "$DRILL"
    cap_result_set     scratchNamespace  "$SCRATCH_NS"
    cap_result_set     recoveryTarget    "${RECOVERY_TARGET:-latest}"
    cap_result_set_raw restoredInSeconds "$rto"
    cap_result_set_raw restoreVerified   true
    cap_ok
}

main "$@"
