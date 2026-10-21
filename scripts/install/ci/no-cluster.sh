#!/usr/bin/env bash
# Fast installer round trip inside the disposable fixture. The full lifecycle
# suite separately covers the cluster and local CA, which this case skips.
# Evidence is intentionally written by the fixture user after sudo exits.
# shellcheck disable=SC2024
set -euo pipefail
[[ -f /run/memql-install-fixture && $(id -u) == 1000 ]] || exit 2
: "${MEMQL_SHA:?exact source revision required}"
[[ "$MEMQL_SHA" =~ ^[a-f0-9]{40}$ ]] || exit 2
export MEMQL_INSTALL_ROOT=/workspace MEMQL_INSTALL_EVIDENCE_DIR=/evidence
export MEMQL_INSTALL_TAG="e2e-${MEMQL_SHA:0:12}"
cd "$MEMQL_INSTALL_ROOT"
git -c user.email=ci@example.com -c user.name=ci tag "$MEMQL_INSTALL_TAG"
finish() {
  local status=$?
  trap - EXIT
  printf '{"formatVersion":1,"leg":"no-cluster","source":"%s","exitCode":%s}\n' \
    "$MEMQL_SHA" "$status" > "$MEMQL_INSTALL_EVIDENCE_DIR/result.json"
  exit "$status"
}
trap finish EXIT

# Take preinstalled pinned tools out of the way
(
set -euo pipefail
set -euo pipefail
for tool in k3d kubectl mkcert; do
  if command -v "$tool" >/dev/null 2>&1; then
    found="$(command -v "$tool")"
    # These fixture tools are not test evidence. Keep them in disposable
    # scratch space so artifact collection does not upload their binaries.
    scratch="$(mktemp -d /tmp/memql-installer-tool.XXXXXX)"
    sudo mv "$found" "$scratch/$tool"
    echo "moved preinstalled $tool aside (was $found)"
  fi
done
)

# Build the install CLI
(
set -euo pipefail
cd "$MEMQL_INSTALL_ROOT/editors/vscode"
npm ci
npm run build:install-cli
)

# Snapshot the machine BEFORE
(
set -euo pipefail
./scripts/install/e2e-baseline.sh --mode=snapshot --out="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt"
)

# Install
(
set -euo pipefail
set -euo pipefail
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js install \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    --repo="$MEMQL_INSTALL_ROOT" \
    --tag="$MEMQL_INSTALL_TAG" \
    --skip=localCA \
    --timeout=600 \
    --json > "$MEMQL_INSTALL_EVIDENCE_DIR/report.json"
sudo chown "$(id -u):$(id -g)" "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"
sudo chown "$(id -u):$(id -g)" "$MEMQL_INSTALL_EVIDENCE_DIR/report.json" 2>/dev/null || true
)

# Show what the run reported
(
set -euo pipefail
cat "$MEMQL_INSTALL_EVIDENCE_DIR/report.json" || true
)

# providerFederation was SKIPPED, with a reason
(
set -euo pipefail
set -euo pipefail
status="$(jq -r '.outcomes[] | select(.id=="providerFederation") | .status' "$MEMQL_INSTALL_EVIDENCE_DIR/report.json")"
reason="$(jq -r '.outcomes[] | select(.id=="providerFederation") | .reason // ""' "$MEMQL_INSTALL_EVIDENCE_DIR/report.json")"
echo "providerFederation: status=$status reason=$reason"
[ -n "$status" ] && [ "$status" != "null" ] || {
  echo "ERROR: the report carries no providerFederation outcome at all. " \
       "Either the step was renamed again or the graph no longer has it." >&2
  exit 1
}
[ "$status" = "skipped" ] || {
  echo "ERROR: providerFederation status is '$status', expected 'skipped'. " \
       "'failed' means the step RAN -- which on a lane with no cluster is the " \
       "regression this job exists to catch." >&2
  exit 1
}
case "$reason" in
  *"installing spends no AI credit"*) ;;
  *) echo "ERROR: the skip does not state why: '$reason'" >&2; exit 1 ;;
esac
satisfied="$(jq -r '.outcomes[] | select(.id=="providerFederation") | .satisfied' "$MEMQL_INSTALL_EVIDENCE_DIR/report.json")"
[ "$satisfied" = "true" ] || {
  echo "ERROR: the providerFederation skip is not marked satisfied (got '$satisfied'). " \
       "Dependents block on an unsatisfied skip, so the whole install is about to be skipped." >&2
  exit 1
}
)

# Every other step actually RAN
(
set -euo pipefail
set -euo pipefail
for id in detect dockerAccess toolK3d toolKubectl toolMkcert hostsBlock \
          browserTrust stackCheckout; do
  status="$(jq -r --arg id "$id" '.outcomes[] | select(.id==$id) | .status' "$MEMQL_INSTALL_EVIDENCE_DIR/report.json")"
  [ "$status" = "ok" ] || {
    echo "ERROR: $id is '$status'. The providerFederation skip cascaded " \
         "through the dependency graph and the install did nothing." >&2
    exit 1
  }
done
echo "every mutating step ran"
)

# The install actually changed the machine
(
set -euo pipefail
set -euo pipefail
if ./scripts/install/e2e-baseline.sh --mode=compare \
    --baseline="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt" --out="$MEMQL_INSTALL_EVIDENCE_DIR/during.txt"; then
  echo "ERROR: the install left the machine identical" >&2
  exit 1
fi
echo "the install changed the machine, as it must"
)

# Uninstall
(
set -euo pipefail
set -euo pipefail
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js uninstall \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    --timeout=600
)

# Snapshot the machine AFTER, and diff
(
set -euo pipefail
./scripts/install/e2e-baseline.sh --mode=compare \
  --baseline="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt" --out="$MEMQL_INSTALL_EVIDENCE_DIR/after.txt"
)
