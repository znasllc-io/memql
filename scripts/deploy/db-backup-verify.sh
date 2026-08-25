#!/usr/bin/env bash
#
# scripts/deploy/db-backup-verify.sh
# ==================================
#
# Capability: deploy.dbBackupVerify -- answer "is there a recent backup?" by
# LOOKING IN THE CONTAINER, not by reading configuration.
#
# Backend for the backup-verification half of memql#4468, epic memql#4496.
#
# WHY THIS IS A SCRIPT AND NOT A PROMETHEUS ALERT. It is the obvious thing to
# want as an alert, and the metric that would carry it is dead here:
#
#   cnpg_collector_last_available_backup_timestamp
#   cnpg_collector_first_recoverability_point
#   cnpg_collector_last_failed_backup_timestamp
#
# CloudNativePG deprecated all three in v1.26 and its release note is explicit
# that they "will no longer update when using plugin-based backups (e.g. Barman
# Cloud via CNPG-I)". MemQL uses exactly that plugin, deliberately and
# everywhere (deploy/cnpg/install/kustomization.yaml). So those gauges sit at
# zero forever on this deployment, and an alert built on one of them either
# fires permanently or never -- which is how the ONE alert that should have
# caught memql#4460 came to be unsatisfiable for its whole life. Do not add a
# backup-age rule to prometheusrule-database.yaml on those metrics.
#
# WHAT THE CONTAINER PROVES THAT CONFIG CANNOT. An ObjectStore is a config
# object: it reports whether it parsed, never whether a byte was written. A
# real instance ran its entire life with a valid-looking ObjectStore, a Ready
# Cluster, healthy pods, and an EMPTY backup container (memql#4460 -> #4468).
# The only check that would have caught that is the one below: list the blobs
# and look at the newest one's age.
#
# NO DECISIONS INSIDE. This script does not know what a healthy backup age is
# for a given instance; it takes a threshold and reports against it
# (capability-script contract, #2221).
#
# TWO PREFIXES, ONE CONTAINER. barman writes base backups under
# <server>/base/ and WAL under <server>/wals/. They fail INDEPENDENTLY and the
# distinction matters: WAL archiving working while base backups do not means
# the replay window grows without bound, and base backups working while WAL
# does not means recovery can only reach the last base. Both are reported.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4468 memql#4460 memql#4496 memql#2221

set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"

cap_init "deploy.dbBackupVerify" "Verify a database backup actually landed in the object store, and report its age."
cap_spec_param "account"      "Azure storage account holding the backup container"
cap_spec_param "container"    "blob container name (default: memql-db-backups)"
cap_spec_param "server"       "barman server name / prefix inside the container (default: memql-db)"
cap_spec_param "max-age-hours" "fail if the newest base backup is older than this (default: 26)"
cap_spec_param "auth"         "azure auth mode: login | connection-string (default: login)"

# newest_blob_epoch <account> <container> <prefix> <auth>
# Echoes the newest blob's last-modified as a unix epoch, or empty when the
# prefix holds no blobs at all. Never exits non-zero on "no blobs" -- an empty
# prefix is a RESULT, and conflating it with a failed query is what would make
# this script report a broken credential as a missing backup.
function newest_blob_epoch() {
  local account="$1" container="$2" prefix="$3" auth="$4"
  local -a authflags=()
  [[ "$auth" == "login" ]] && authflags=(--auth-mode login)

  local newest
  newest="$(az storage blob list \
      --account-name "$account" \
      --container-name "$container" \
      --prefix "$prefix" \
      "${authflags[@]}" \
      --query 'max_by([], &properties.lastModified).properties.lastModified' \
      --output tsv 2>/dev/null || true)"

  [[ -z "$newest" || "$newest" == "None" ]] && return 0
  date -u -d "$newest" +%s 2>/dev/null || true
}

function main() {
  cap_handle_meta "$@"
  cap_parse_flags "$@"

  local account container server max_age auth
  account="$(cap_param account "")"
  container="$(cap_param container "memql-db-backups")"
  server="$(cap_param server "memql-db")"
  max_age="$(cap_param max-age-hours "26")"
  auth="$(cap_param auth "login")"

  [[ -n "$account" ]] || cap_fail 2 "--account is required: the storage account holding the backup container"
  [[ "$max_age" =~ ^[0-9]+$ ]] || cap_fail 2 "--max-age-hours must be a whole number of hours, got '${max_age}'"
  case "$auth" in
    login|connection-string) ;;
    *) cap_fail 2 "--auth must be 'login' or 'connection-string', got '${auth}'" ;;
  esac

  # cap_require checks a PARAMETER, not a binary -- the idiom for a missing
  # prerequisite is `command -v`, and exit 4 rather than 2.
  command -v az &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"
  if [[ "$auth" == "login" ]]; then
    local active
    active="$(az account show --query id --output tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first, or pass --auth=connection-string"
  fi

  cap_step "Listing backups in ${account}/${container} (server ${server})"

  # CAN WE READ AT ALL? This must be settled BEFORE any statement about
  # backups, because "the listing came back empty" and "the listing was
  # refused" look identical downstream and only one of them means backups are
  # not running. Reporting a denied read as a missing backup would send an
  # operator to fix the database when the database is fine.
  cap_result_set account   "$account"
  cap_result_set container "$container"

  if ! az storage blob list --account-name "$account" --container-name "$container" \
        $([[ "$auth" == "login" ]] && echo "--auth-mode login") \
        --num-results 1 --output tsv >/dev/null 2>&1; then

    # Separate the two causes using the CONTROL plane, which a subscription
    # Owner can always read. THE DISTINCTION IS NOT PEDANTRY: Azure splits
    # storage RBAC across two planes, and being Owner of the subscription
    # grants NOTHING on the data plane. Listing blobs needs an explicit
    # "Storage Blob Data Reader" (or Contributor/Owner) assignment, so the
    # overwhelmingly likely cause of a refused read is a missing role rather
    # than a missing container -- and the error Azure returns names five roles
    # without saying which plane it means.
    if az storage container list --account-name "$account" \
         $([[ "$auth" == "login" ]] && echo "--auth-mode login") \
         --query "[?name=='${container}'] | [0].name" --output tsv 2>/dev/null | grep -qx "$container"; then
      cap_result_set_raw containerExists true
      cap_fail 3 "the container ${account}/${container} EXISTS but this identity cannot read its blobs. Azure storage RBAC is data-plane: subscription Owner does not grant it. Assign 'Storage Blob Data Reader' on the account, then re-run. NOTHING HERE IS EVIDENCE ABOUT BACKUPS."
    fi

    cap_result_set_raw containerExists false
    cap_fail 5 "the container ${account}/${container} does not exist (or the account is unreachable). Backups have nowhere to land."
  fi
  cap_result_set_raw containerExists true

  local base_epoch wal_epoch now
  base_epoch="$(newest_blob_epoch "$account" "$container" "${server}/base/" "$auth")"
  wal_epoch="$(newest_blob_epoch  "$account" "$container" "${server}/wals/" "$auth")"
  now="$(date -u +%s)"

  cap_result_set     account       "$account"
  cap_result_set     container     "$container"
  cap_result_set     server        "$server"
  cap_result_set_raw maxAgeHours   "$max_age"

  # NEVER BACKED UP is its own outcome, deliberately distinct from STALE. An
  # empty container is the memql#4468 finding verbatim, and it means the backup
  # path has never worked -- a different investigation from one that worked and
  # stopped.
  if [[ -z "$base_epoch" ]]; then
    cap_result_set_raw baseBackupFound false
    cap_result_set_raw walArchiveFound "$([[ -n "$wal_epoch" ]] && echo true || echo false)"
    cap_fail 5 "NO BASE BACKUP HAS EVER BEEN WRITTEN to ${container}/${server}/base/. The ObjectStore can be valid and the Cluster Ready with this true (memql#4468); check the barman sidecar's log and the ObjectStore destinationPath host (memql#4460)."
  fi

  local base_age_h=$(( (now - base_epoch) / 3600 ))
  cap_result_set_raw baseBackupFound true
  cap_result_set_raw baseBackupAgeHours "$base_age_h"
  cap_result_set     baseBackupAt "$(date -u -d "@${base_epoch}" +%Y-%m-%dT%H:%M:%SZ)"

  if [[ -n "$wal_epoch" ]]; then
    cap_result_set_raw walArchiveFound true
    cap_result_set_raw walArchiveAgeHours "$(( (now - wal_epoch) / 3600 ))"
    cap_result_set     walArchiveAt "$(date -u -d "@${wal_epoch}" +%Y-%m-%dT%H:%M:%SZ)"
  else
    # A base backup with no archived WAL is recoverable only to that base.
    cap_result_set_raw walArchiveFound false
    cap_warn "a base backup exists but NO WAL has been archived -- point-in-time recovery is not available, only recovery to the base itself"
  fi

  cap_info "newest base backup is ${base_age_h}h old (threshold ${max_age}h)"

  if (( base_age_h > max_age )); then
    cap_fail 5 "the newest base backup is ${base_age_h}h old, past the ${max_age}h threshold. Backups have STOPPED -- they ran once and no longer do."
  fi

  cap_ok
}

main "$@"
