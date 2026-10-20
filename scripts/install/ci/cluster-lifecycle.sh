#!/usr/bin/env bash
# Installer lifecycle against a fresh, disposable Docker daemon. The assertions
# are carried from the installed-cluster lane; no install graph node is skipped.
# Invoke only through fleet-fixture.py: this script writes system trust/hosts
# inside its dedicated fixture and must never be run on the owner's OS.
# Evidence redirection intentionally belongs to the fixture user, and PATH
# modifications in each diagnostic subshell intentionally stay local.
# shellcheck disable=SC2024,SC2030,SC2031
set -euo pipefail
[[ -f /run/memql-install-fixture && $(id -u) == 1000 ]] || {
  echo 'installer lifecycle requires its disposable fixture' >&2; exit 2;
}
: "${MEMQL_SHA:?exact source revision required}"
: "${MEMQL_INSTALL_LEG:?lifecycle leg required}"
[[ "$MEMQL_SHA" =~ ^[a-f0-9]{40}$ ]] || exit 2
case "$MEMQL_INSTALL_LEG" in round-trip|repair|upgrade|from-source|pipelines) ;; *) exit 2;; esac
export REVISION="$MEMQL_SHA" LEG="$MEMQL_INSTALL_LEG" DOMAIN=memql.localhost
export MEMQL_INSTALL_ROOT=/workspace MEMQL_INSTALL_EVIDENCE_DIR=/evidence
export MEMQL_K3D_WORKLOAD_TIMEOUT=720s MEMQL_K3D_OPERATOR_TIMEOUT=480
cd "$MEMQL_INSTALL_ROOT"
export FROM_VERSION TO_VERSION
FROM_VERSION=$(sed -n 's/^export const DEFAULT_STACK_TAG = "\(.*\)";$/\1/p' editors/vscode/src/install/stackPin.ts)
[[ "$FROM_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'missing installer fallback version' >&2; exit 2; }
TO_VERSION=''

# Take preinstalled pinned tools out of the way
tools() (
set -euo pipefail
set -euo pipefail
for tool in k3d kubectl mkcert; do
  if command -v "$tool" >/dev/null 2>&1; then
    found="$(command -v "$tool")"
    sudo mv "$found" "$MEMQL_INSTALL_EVIDENCE_DIR/${tool}.preinstalled"
    echo "moved preinstalled $tool aside (was $found)"
  fi
done
)

# Build the install CLI
build_cli() (
set -euo pipefail
cd "${MEMQL_INSTALL_ROOT}/editors/vscode"
npm ci
npm run build:install-cli
)

# Give the runner the browser profile a desktop has
browser_profile() (
set -euo pipefail
set -euo pipefail
mkdir -p "$HOME/.pki/nssdb"
certutil -d "sql:$HOME/.pki/nssdb" -N --empty-password
)

# Snapshot the machine BEFORE
baseline() (
set -euo pipefail
set -euo pipefail
sudo -E env "HOME=$HOME" "PATH=$HOME/.memql/bin:$PATH" \
  ./scripts/install/e2e-baseline.sh --mode=snapshot --out="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt"
)

# Install
install_cluster() (
set -euo pipefail
set -euo pipefail
if [ "$LEG" = "upgrade" ]; then
  ref_flags=("--tag=${FROM_VERSION}")
elif [ "$LEG" = "from-source" ] || [ "$LEG" = "pipelines" ]; then
  ref_flags=("--commit=${REVISION}" "--from-source")
else
  ref_flags=("--commit=${REVISION}")
fi
echo "INFO: installing with ${ref_flags[*]}"
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js install \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    "${ref_flags[@]}" \
    --domain="$DOMAIN" \
    --owner-email=ci@example.com \
    --owner-first-name=Continuous \
    --owner-last-name=Integration \
    --param=clusterUp.argocd-timeout=480 \
    --timeout=1800 \
    --json > "$MEMQL_INSTALL_EVIDENCE_DIR/install-report.json"
sudo chown "$(id -u):$(id -g)" \
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" "$MEMQL_INSTALL_EVIDENCE_DIR/install-report.json"
)

# Show what the install recorded
show_receipt() (
set -euo pipefail
jq . "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"
)

# The install built a cluster, and the receipt says so
assert_cluster() (
set -euo pipefail
set -euo pipefail
receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"

field() {
  jq -r --arg id "$1" --arg path "$2" '
    ([.entries[] | select(.stepId == $id)] | last // {})
    | getpath($path | split("."))
    | if . == null then "MISSING" else tostring end' "$receipt"
}

fail=0
expect() {
  local what="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "[x] ${what} = ${got}"
  else
    echo "[ ] ERROR: ${what} = ${got} (expected ${want})" >&2
    fail=1
  fi
}

if [ "$LEG" = "from-source" ] || [ "$LEG" = "pipelines" ]; then
  expect "clusterUp.result.argocdReady" "$(field clusterUp result.argocdReady)" "true"
  expect "buildImages.result.imageSource" "$(field buildImages result.imageSource)" "checkout"
  expect "seedBootstrap.result.bootstrapComplete" \
    "$(field seedBootstrap result.bootstrapComplete)" "true"
else
  expect "clusterUp.result.workloadsReady" "$(field clusterUp result.workloadsReady)" "true"
  if [ "$(field buildImages result.imageSource)" != "MISSING" ]; then
    echo "[ ] ERROR: a release-graph install recorded a buildImages step; install.json is not supposed to have one" >&2
    fail=1
  else
    echo "[x] no buildImages on the release graph, as expected"
  fi
fi
expect "clusterUp.preExisting" "$(field clusterUp preExisting)" "false"
expect "seedBootstrap.result.bootstrapComplete" \
  "$(field seedBootstrap result.bootstrapComplete)" "true"
expect "frontDoor.result.allPassed" "$(field frontDoor result.allPassed)" "true"

cluster="$(field clusterUp result.cluster)"
if [ "$cluster" = "MISSING" ] || [ -z "$cluster" ]; then
  echo "[ ] ERROR: clusterUp recorded no cluster name, so the uninstall has nothing to remove" >&2
  fail=1
else
  echo "[x] clusterUp.result.cluster = ${cluster}"
fi

exit "$fail"
)

# The install actually changed the machine, on the surfaces that matter
assert_changed() (
set -euo pipefail
set -uo pipefail
set +e
sudo -E env "HOME=$HOME" "PATH=$HOME/.memql/bin:$PATH" \
  ./scripts/install/e2e-baseline.sh --mode=compare \
    --baseline="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt" --out="$MEMQL_INSTALL_EVIDENCE_DIR/during.txt" \
  > "$MEMQL_INSTALL_EVIDENCE_DIR/during.json"
rc=$?
set -e
sections="$(jq -r '.result.differingSections | join(" ")' "$MEMQL_INSTALL_EVIDENCE_DIR/during.json")"
echo "differing sections: ${sections:-<none>}"
if [ "$rc" -eq 0 ]; then
  echo "ERROR: the install left the machine identical -- the round trip below would prove nothing" >&2
  exit 1
fi
for required in dockerImages k3dClusters; do
  case " ${sections} " in
    *" ${required} "*) echo "[x] ${required} changed, as a cluster install must" ;;
    *) echo "[ ] ERROR: ${required} did not change -- no cluster was built" >&2; exit 1 ;;
  esac
done
)

# The owner claims the cluster with a passkey, and the editor signs in
owner_claim() (
set -euo pipefail
cd "${MEMQL_INSTALL_ROOT}/editors/vscode"
set -euo pipefail
scripts_retry="$MEMQL_INSTALL_ROOT/scripts/ci/retry.sh"
"$scripts_retry" -- npx playwright install --with-deps chromium
"$scripts_retry" -- bash "$MEMQL_INSTALL_ROOT/scripts/vscode/deps.sh"
node_modules/.bin/esbuild test-host/ownerClaim.ts --bundle --platform=node \
  --format=cjs --target=node20 --external:playwright \
  --outfile="$MEMQL_INSTALL_EVIDENCE_DIR/owner-claim.js"
sudo -E env "HOME=$HOME" "PATH=$PATH" "NODE_PATH=$PWD/node_modules" \
  "OWNER_EMAIL=ci@example.com" "DOMAIN=$DOMAIN" \
  node "$MEMQL_INSTALL_EVIDENCE_DIR/owner-claim.js"
)

# The pipelines substrate, end to end on the installed cluster
pipeline_substrate() (
set -euo pipefail
bash scripts/identity/build-css.sh
export GOWORK=off
set -euo pipefail
cluster="$(jq -r '([.entries[] | select(.stepId == "clusterUp")] | last // {}) | .result.cluster // ""' \
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json")"
if [ -z "$cluster" ]; then
  echo "ERROR: the install recorded no cluster name, so there is no context to name" >&2
  exit 1
fi
sudo -E env "HOME=$HOME" "PATH=$HOME/.memql/bin:$PATH" \
  k3d kubeconfig get "$cluster" > "$MEMQL_INSTALL_EVIDENCE_DIR/kubeconfig"
chmod 600 "$MEMQL_INSTALL_EVIDENCE_DIR/kubeconfig"
MEMQL_E2E_KUBECONFIG="$MEMQL_INSTALL_EVIDENCE_DIR/kubeconfig" MEMQL_E2E_KUBECONTEXT="k3d-${cluster}" \
  go test -tags clustere2e,agent -count=1 -timeout=30m -v \
    -run '^TestPipelinesSubstrate$' ./test/clustere2e/ 2>&1 | tee "$MEMQL_INSTALL_EVIDENCE_DIR/pipelines-e2e.txt"
if ! grep -q -- '^--- PASS: TestPipelinesSubstrate ' "$MEMQL_INSTALL_EVIDENCE_DIR/pipelines-e2e.txt"; then
  echo "ERROR: TestPipelinesSubstrate did not pass on a cluster this leg installed and named:" >&2
  echo "       it skipped, or the build left it out (it needs -tags clustere2e,agent)" >&2
  exit 1
fi
)

# Repair
repair() (
set -euo pipefail
set -euo pipefail
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js repair \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    --param=clusterUp.argocd-timeout=480 \
    --timeout=1800 \
    --json > "$MEMQL_INSTALL_EVIDENCE_DIR/repair-report.json"
sudo chown "$(id -u):$(id -g)" \
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" "$MEMQL_INSTALL_EVIDENCE_DIR/repair-report.json"
)

# The repair replayed, and left the cluster removable
assert_repair() (
set -euo pipefail
set -euo pipefail
receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"
field() {
  jq -r --arg id "$1" --arg path "$2" '
    ([.entries[] | select(.stepId == $id)] | last // {})
    | getpath($path | split("."))
    | if . == null then "MISSING" else tostring end' "$receipt"
}
fail=0

commit="$(field stackCheckout params.commit)"
if [ "$commit" = "$REVISION" ]; then
  echo "[x] the repair replayed --commit=${commit}"
else
  echo "[ ] ERROR: the repair asked for commit '${commit}', not the installed ${REVISION}." >&2
  echo "    A repair that installs a different version is an upgrade wearing a repair's name." >&2
  fail=1
fi
tag="$(field stackCheckout params.tag)"
if [ "$tag" != "MISSING" ] && [ -n "$tag" ]; then
  echo "[ ] ERROR: the repair passed --tag=${tag}; the recorded install was a commit." >&2
  fail=1
fi

expect_pre="$(field clusterUp preExisting)"
if [ "$expect_pre" = "false" ]; then
  echo "[x] clusterUp.preExisting is still false after a repair"
else
  echo "[ ] ERROR: clusterUp.preExisting became '${expect_pre}' after the repair." >&2
  echo "    The uninstall will now REFUSE to delete a cluster MemQL created (memql#3605)." >&2
  fail=1
fi

if ! jq -e '.ok == true' "$MEMQL_INSTALL_EVIDENCE_DIR/repair-report.json" >/dev/null; then
  echo "[ ] ERROR: the repair run itself did not report ok" >&2
  fail=1
fi
exit "$fail"
)

# Upgrade
upgrade() (
set -euo pipefail
set -euo pipefail
if [ -n "$TO_VERSION" ]; then
  ref_flag="--tag=${TO_VERSION}"
else
  ref_flag="--commit=${REVISION}"
fi
echo "INFO: upgrading with ${ref_flag}"
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js install \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    "$ref_flag" \
    --domain="$DOMAIN" \
    --owner-email=ci@example.com \
    --owner-first-name=Continuous \
    --owner-last-name=Integration \
    --param=clusterUp.argocd-timeout=480 \
    --timeout=1800 \
    --json > "$MEMQL_INSTALL_EVIDENCE_DIR/upgrade-report.json"
sudo chown "$(id -u):$(id -g)" \
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" "$MEMQL_INSTALL_EVIDENCE_DIR/upgrade-report.json"
)

# The upgrade moved the checkout, and the cluster survived it
assert_upgrade() (
set -euo pipefail
set -euo pipefail
receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"
field() {
  jq -r --arg id "$1" --arg path "$2" '
    ([.entries[] | select(.stepId == $id)] | last // {})
    | getpath($path | split("."))
    | if . == null then "MISSING" else tostring end' "$receipt"
}
fail=0

want="${TO_VERSION:-$REVISION}"
got_tag="$(field stackCheckout params.tag)"
got_commit="$(field stackCheckout params.commit)"
if [ "$got_tag" = "$want" ] || [ "$got_commit" = "$want" ]; then
  echo "[x] the checkout now names ${want}"
else
  echo "[ ] ERROR: after the upgrade the checkout names tag='${got_tag}' commit='${got_commit}'," >&2
  echo "    not ${want}. The cluster was not moved." >&2
  fail=1
fi
if [ "$got_tag" = "$FROM_VERSION" ]; then
  echo "[ ] ERROR: the checkout is still at ${FROM_VERSION}" >&2
  fail=1
fi

expect_ready="$(field clusterUp result.workloadsReady)"
if [ "$expect_ready" = "true" ]; then
  echo "[x] clusterUp.result.workloadsReady still holds after the move"
else
  echo "[ ] ERROR: workloadsReady is '${expect_ready}' after the upgrade" >&2
  fail=1
fi
if [ "$(field clusterUp preExisting)" != "false" ]; then
  echo "[ ] ERROR: clusterUp.preExisting flipped during the upgrade (memql#3605)" >&2
  fail=1
fi
exit "$fail"
)

# Uninstall
uninstall() (
set -euo pipefail
set -euo pipefail
sudo -E env "HOME=$HOME" "PATH=$PATH" \
  node editors/vscode/dist-install/cli.js uninstall \
    --root="$MEMQL_INSTALL_ROOT" \
    --receipt="$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json" \
    --timeout=1800 \
    --json > "$MEMQL_INSTALL_EVIDENCE_DIR/uninstall-report.json"
sudo chown "$(id -u):$(id -g)" "$MEMQL_INSTALL_EVIDENCE_DIR/uninstall-report.json"
)

# Snapshot the machine AFTER, and diff
assert_restored() (
set -euo pipefail
set -uo pipefail
set +e
sudo -E env "HOME=$HOME" "PATH=$HOME/.memql/bin:$PATH" \
  ./scripts/install/e2e-baseline.sh --mode=compare \
    --baseline="$MEMQL_INSTALL_EVIDENCE_DIR/before.txt" --out="$MEMQL_INSTALL_EVIDENCE_DIR/after.txt" \
  > "$MEMQL_INSTALL_EVIDENCE_DIR/after.json"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  echo "SUCCESS: the machine is byte-identical to its baseline on all four surfaces."
  exit 0
fi

drifted="$(jq -r '.result.differingSections[]' "$MEMQL_INSTALL_EVIDENCE_DIR/after.json")"
echo "sections that drifted: ${drifted:-<none named>}"
if [ -z "$drifted" ]; then
  echo "ERROR: the snapshots differ but no section was named -- read the diff above." >&2
  exit 1
fi
for section in $drifted; do
  if [ "$section" != "dockerImages" ]; then
    echo "ERROR: ${section} did not return to its baseline -- the receipt did not take everything back." >&2
    exit 1
  fi
done

section_of() { sed -n '/^\[dockerImages\]$/,/^\[k3dClusters\]$/p' "$1"; }
removed="$(diff <(section_of "$MEMQL_INSTALL_EVIDENCE_DIR/before.txt") <(section_of "$MEMQL_INSTALL_EVIDENCE_DIR/after.txt") | sed -n 's/^< //p' || true)"
added="$(diff <(section_of "$MEMQL_INSTALL_EVIDENCE_DIR/before.txt") <(section_of "$MEMQL_INSTALL_EVIDENCE_DIR/after.txt") | sed -n 's/^> //p' || true)"

if [ -n "$removed" ]; then
  echo "ERROR: the install REMOVED images that were on this machine before it ran:" >&2
  printf '  %s\n' "$removed" >&2
  exit 1
fi

fail=0
while IFS= read -r line; do
  if [ -z "$line" ]; then continue; fi
  repo="${line%%:*}"
  if [ "$LEG" = "from-source" ] || [ "$LEG" = "pipelines" ]; then
    case "$repo" in
      memql-*|docker/dockerfile)
        echo "[x] built by this leg rather than pulled by the install, and k3d claims no images: ${line}"
        continue
        ;;
    esac
  fi
  case "$repo" in
    rancher/k3s|docker.io/rancher/k3s|ghcr.io/k3d-io/*)
      echo "[x] left behind by k3d's own teardown, which does not claim images: ${line}"
      ;;
    *)
      echo "[ ] ERROR: this image survived the uninstall and is not one of k3d's: ${line}" >&2
      fail=1
      ;;
  esac
done <<< "$added"
if [ "$fail" -ne 0 ]; then
  echo "ERROR: an image the installer pulled is still in the local daemon." >&2
  exit 1
fi
echo "SUCCESS: the three reversible surfaces are byte-identical; only k3d's own images remain."
)

# No cluster survived the uninstall
assert_no_cluster() (
set -euo pipefail
set -euo pipefail
left="$(docker ps -a --format '{{.Names}}' --filter 'name=^k3d-' || true)"
if [ -n "$left" ]; then
  echo "ERROR: the uninstall left a k3d cluster behind:" >&2
  printf '  %s\n' "$left" >&2
  echo "    A refused removal reads as success on the k3dClusters surface once the" >&2
  echo "    k3d binary is gone, which is why this check does not use it." >&2
  exit 1
fi
echo "SUCCESS: no k3d containers remain."
)

# Hand the evidence back to the runner
handoff() (
set -euo pipefail
set -euo pipefail
owner="$(id -u):$(id -g)"
evidence=(
  "$MEMQL_INSTALL_EVIDENCE_DIR/before.txt"
  "$MEMQL_INSTALL_EVIDENCE_DIR/during.txt"
  "$MEMQL_INSTALL_EVIDENCE_DIR/after.txt"
  "$MEMQL_INSTALL_EVIDENCE_DIR/during.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/after.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-receipt.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/install-report.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/repair-report.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/upgrade-report.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/uninstall-report.json"
  "$MEMQL_INSTALL_EVIDENCE_DIR/pipelines-e2e.txt"
  "$MEMQL_INSTALL_EVIDENCE_DIR/diagnostics.txt"
)
root_owned=()
for path in "${evidence[@]}"; do
  if [[ ! -f "$path" || -L "$path" ]]; then
    continue
  fi
  metadata="$(stat -c '%u %h' -- "$path" 2>/dev/null || true)"
  [[ -n "$metadata" ]] || continue
  read -r file_uid link_count <<<"$metadata" || continue
  if [[ "$file_uid" == 0 && "$link_count" == 1 ]]; then
    root_owned+=("$path")
  fi
done
if ((${#root_owned[@]} == 0)); then
  echo "no root-owned evidence files need handoff"
  exit 0
fi

if timeout --signal=TERM --kill-after=5s 30s \
    sudo chown --no-dereference "$owner" -- "${root_owned[@]}"; then
  exit 0
else
  status=$?
fi
echo "ERROR: bounded evidence ownership repair exited ${status}; continuing with diagnostics and teardown" >&2
exit "$status"
)

# Cluster diagnostics
diagnostics() (
set -euo pipefail
set +e
export PATH="$HOME/.memql/bin:$PATH"
function diagnostic_kubectl() {
  timeout --signal=TERM --kill-after=5s 15s \
    sudo -n -E env "HOME=$HOME" "PATH=$PATH" \
    kubectl --request-timeout=10s "$@"
}
{
  echo "=== k3d clusters ==="
  timeout --signal=TERM --kill-after=5s 15s \
    sudo -n -E env "HOME=$HOME" "PATH=$PATH" k3d cluster list
  echo "=== node capacity and allocation ==="
  diagnostic_kubectl describe nodes
  echo "=== pods ==="
  diagnostic_kubectl get pods -A -o wide
  echo "=== argocd applications ==="
  diagnostic_kubectl get applications -A -o wide
  echo "=== recent events ==="
  diagnostic_kubectl get events -A --sort-by=.lastTimestamp | tail -60
  echo "=== pods that are not Running ==="
  diagnostic_kubectl get pods -A \
    --field-selector=status.phase!=Running -o wide
  echo "=== the memql namespace, described ==="
  diagnostic_kubectl describe pods -n memql
  echo "=== the memql-pipelines namespace ==="
  diagnostic_kubectl get jobs,pods,secrets,pvc,resourcequota -n memql-pipelines -o wide
  diagnostic_kubectl describe pods -n memql-pipelines
  diagnostic_kubectl get events -n memql-pipelines --sort-by=.lastTimestamp
} > "$MEMQL_INSTALL_EVIDENCE_DIR/diagnostics.txt" 2>&1
cat "$MEMQL_INSTALL_EVIDENCE_DIR/diagnostics.txt"
exit 0
)

# Tear the cluster down whatever happened
teardown() (
set -euo pipefail
set +e
export PATH="$HOME/.memql/bin:$PATH"
if command -v k3d >/dev/null 2>&1; then
  timeout --signal=TERM --kill-after=5s 90s \
    sudo -n -E env "HOME=$HOME" "PATH=$PATH" k3d cluster delete memql
else
  echo "no k3d binary on PATH -- the uninstall took it, so there is nothing to tear down"
fi
exit 0
)

finish() {
  local status=$?
  trap - EXIT
  set +e
  if (( status != 0 )); then diagnostics; fi
  handoff
  local handoff_status=$?
  if (( handoff_status != 0 )); then status=$handoff_status; fi
  teardown
  printf '{"formatVersion":1,"leg":"%s","source":"%s","exitCode":%s}\n' \
    "$LEG" "$REVISION" "$status" > "$MEMQL_INSTALL_EVIDENCE_DIR/result.json"
  exit "$status"
}
trap finish EXIT

tools
build_cli
if [[ "$LEG" == from-source ]]; then browser_profile; fi
baseline
df -h /
install_cluster
show_receipt
assert_cluster
assert_changed
case "$LEG" in
  from-source) owner_claim ;;
  pipelines) pipeline_substrate ;;
  repair) repair; assert_repair ;;
  upgrade) upgrade; assert_upgrade ;;
esac
uninstall
assert_restored
assert_no_cluster
