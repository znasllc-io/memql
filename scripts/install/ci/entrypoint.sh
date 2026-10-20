#!/usr/bin/env bash
# Runs only inside the installer fixture; no host sockets or paths are mounted.
set -euo pipefail
[[ -f /run/memql-install-fixture && $(id -u) == 0 ]] || exit 2
: "${MEMQL_SHA:?exact source revision required}"
[[ "$MEMQL_SHA" =~ ^[a-f0-9]{40}$ ]] || exit 2
case "${MEMQL_INSTALL_LEG:-}" in no-cluster|round-trip|repair|upgrade|from-source|pipelines) ;; *) exit 2;; esac
mkdir -p /etc/docker
printf '{"features":{"containerd-snapshotter":false}}\n' > /etc/docker/daemon.json
dind dockerd --host=unix:///var/run/docker.sock --group=docker \
  --exec-opt native.cgroupdriver=cgroupfs > /evidence/docker.log 2>&1 &
daemon_pid=$!
trap 'kill "$daemon_pid" 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then break; fi
  kill -0 "$daemon_pid" || { cat /evidence/docker.log; exit 1; }
  sleep 1
done
docker info >/dev/null
runuser -u memql --preserve-environment -- bash -euo pipefail <<'MEMQL_FIXTURE'
cd /workspace
git init -q .
git remote add origin https://github.com/znasllc-io/memql.git
git -c credential.helper= fetch --depth=1 origin "$MEMQL_SHA"
git checkout --detach -q FETCH_HEAD
[[ "$(git rev-parse HEAD)" == "$MEMQL_SHA" ]]
if [[ "$MEMQL_INSTALL_LEG" == no-cluster ]]; then
  exec bash scripts/install/ci/no-cluster.sh
fi
exec bash scripts/install/ci/cluster-lifecycle.sh
MEMQL_FIXTURE
