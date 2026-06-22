---
title: Database connection budget & graceful-deploy standard
audience: public
status: stable
area: operate
sinceVersion: 0.9.75
owner: znas
---

# Database connection budget & graceful-deploy standard

This is the standard for keeping the cluster's Postgres (Tiger Cloud in
staging/prod, local Postgres in dev) connection demand under the instance's
ceiling — across steady state **and** deploys — so a rollout can never trigger
`SQLSTATE 53300` ("remaining connection slots are reserved …" / "too many
clients already"). It is the deliverable of epic memql#1778.

**Environment-agnostic by design.** The connection *lifecycle* is identical in
every environment — a bounded per-pod pool, graceful pool close on shutdown,
leak-free reconnect, and a deploy that never exceeds the budget. Only the
*numbers* differ per environment as config (per-pod pool size, the DB's
`max_connections`, replica counts). Dev (docker-compose Postgres) and Azure
(AKS + Tiger Cloud) run the same code paths; they just plug in different limits.

## The budget formula

```
peak_connections  =  Σ_over_node_types( replicas × MAX_OPEN_CONNS )
                  +  rollout_surge            (RollingUpdate maxSurge pods × pool)
                  +  bluegreen_overlap        (preview color pods × pool, held for scaleDownDelaySeconds)

REQUIRE:  peak_connections  ≤  max_connections − reserved_connections
```

- `reserved_connections` = Postgres `superuser_reserved_connections` (+ any
  `reserved_connections`); on Tiger Cloud the platform also reserves a few.
- The dangerous term is **deploy-time overlap**, not steady state: a full-stack
  roll briefly runs old + new pods together. Size the budget for the *peak*, not
  the average.

## The knobs (per-pod pool — `component/database/database.go`)

Source of truth is the code; defaults are conservative and overridable per env:

| Env var | Default | Purpose |
|---|---|---|
| `MAX_OPEN_CONNS` | 10 | hard cap on concurrent connections per pod (the primary budget lever) |
| `MAX_IDLE_CONNS` | 3 | warm idle connections kept per pod |
| `CONN_MAX_LIFETIME_MS` | 3600000 (1h) | rotate connections hourly (bounds long-lived leaks) |
| `CONN_MAX_IDLE_TIME_MS` | 600000 (10m) | close idle connections after 10 min (reclaims slack) |

`MAX_OPEN_CONNS` is a **hard** per-pod cap (database/sql never exceeds it), so a
single pod cannot leak past it. Right-size it so `peak_connections` (above) fits
the instance ceiling. To shrink the budget, lower `MAX_OPEN_CONNS` before adding
replicas or a pooler.

## Defense in depth (already in code)

- **53300 retry** (`component/database/conn_retry.go`, memql#1076): `Connect()`
  retries transient slot-exhaustion with bounded jittered backoff instead of
  failing the query. Covers the residual spike; it deliberately does **not**
  retry forever (that just piles pressure on an exhausted server). It does *not*
  leak — a connection is only returned on success.
- **Graceful shutdown** (memql#1778 child D): `Database.Stop()` calls
  `db.Close()` on the pool, so a pod releases its connections on SIGTERM. Ensure
  the workload's `terminationGracePeriodSeconds` allows the pool to close before
  SIGKILL.
- **Idle reaping** (child G): `CONN_MAX_IDLE_TIME_MS` + `CONN_MAX_LIFETIME_MS`
  bound how long an *app* connection lingers, and the app stamps
  `idle_session_timeout` + `idle_in_transaction_session_timeout` as per-session
  params so a wedged app client can't hold a slot indefinitely (Tiger Cloud /
  local both support it).
- **Watch for non-app client leaks** (memql#1861): the app fleet reaps its own
  idle connections, but a **separate deploy/migrate/promote tool or exporter**
  that opens a pool and never closes it has no such reaper — it will pin idle
  slots for hours and eat the budget. On staging this showed up as ~28 idle
  backends stamped `application_name=deployer`, connecting as the `postgres`
  superuser, growing ~1 per 30 min. Detect them with
  `scripts/deploy/deployer-pool-reap.sh inspect` (read-only) and reclaim with
  `scripts/deploy/deployer-pool-reap.sh reap --confirm` and the postgres-superuser
  DSN (only a superuser may terminate superuser-owned sessions; the reaper is
  guarded — read-only without `--confirm`, and only ever targets idle,
  `deployer`-stamped, past-threshold backends). The durable fix is to **stop the
  leaking client** (identified by its `client_addr`) and make it close its pool /
  set an idle timeout — there is no shared role to put a server-side reaper on.
  The in-cluster deploy + migration cycle does not leak (the migrate Job closes
  its pool on exit and stamps `application_name=memql-migrate`, memql#1933).
- **Blue-green drain window** (child E, memql#1780): the bff Rollout's
  `scaleDownDelaySeconds` was cut 3600→300 so a promotion stops holding a full
  extra bff color (pods + pools) for an hour. See `deploy/rollouts/README.md`.

## Graceful deploy runbook (staging/prod)

Ordering and ArgoCD facts that a deploy MUST respect:

1. **ArgoCD ignores `/spec/replicas`** on Deployments (`ignoreDifferences`, no
   HPA). A manual scale-to-0 to drain connections **sticks** — ArgoCD will not
   restore it, and re-enabling auto-sync only reconciles the pod template. Any
   drain-based recovery must explicitly scale the fleet back up.
2. **Bring identity up first.** The deploy-gate's authenticated query, the
   voice-agent JWT bootstrap, and every JWT verifier need identity/JWKS. Draining
   identity aborts the bff blue-green gate and CrashLoops voice-agent.
3. **Don't roll the whole DB-heavy fleet at once** if the budget is tight —
   sequence it, or rely on the per-pod cap + scaleDownDelay so peak stays under
   the ceiling.

### 53300 recovery (when staging is already wedged)

The fleet has leaked/accumulated connections and every engine pod is `0/1`
(`/healthz` 503, logs spamming 53300):

1. Suspend ArgoCD auto-sync (so a scale-down isn't reverted).
2. Scale the engine fleet to 0 (drains every app connection).
3. Wait ~2 min for Postgres to reap the closed backends.
4. Re-enable auto-sync **and explicitly scale the fleet back up** (identity
   first) — ArgoCD will not restore replicas on its own.

## Connection pooling (hybrid endpoint split) — SHIPPED (memql#1925)

Right-sizing + `scaleDownDelay` kept the budget under the ceiling at steady
state, but a full-stack roll still tipped it: blue-green bff + rolling mesh
briefly doubles the pod count, and `pods × MAX_OPEN_CONNS` + the leaked
`deployer` pool blew past Tiger's ~88 usable direct slots → the 53300 storm
that wedged 0.9.87. The structural fix is **Tiger Cloud transaction-mode
pooling (PgBouncer)** via a **hybrid endpoint split**:

- **Bulk traffic** (the bun pool — all queries + mutations, every mesh pod) →
  `MEMORY_NODES_DATABASE_DSN` points at the **transaction pooler** (db
  `tsdb_transaction`, pooler port `39578`). Client connections decouple from
  Postgres backends, so a deploy surge no longer maps 1:1 to direct slots —
  the surge-killing multiplier.
- **Session-stateful traffic** (the 4 advisory-lock / leader components +
  migrations) → `MEMORY_NODES_DATABASE_DIRECT_DSN` points at the **direct**
  (non-pooled) endpoint (db `tsdb`). A transaction-mode pooler recycles a
  server backend between statements, which would drop a held session-scoped
  advisory lock; these few, bounded connections take a direct slot instead.

Code: `Database.DirectBunDB()` returns the direct pool when `DIRECT_DSN` is set,
else falls back to the main pool (env-agnostic — local/dev without a pooler is
unaffected). bun's `pgdriver` speaks the simple query protocol (no server-side
prepared statements), so transaction pooling is safe. Wiring + the
`kubectl create secret` recipe: `deploy/k8s/base/README.md`. Local parity:
the in-compose PgBouncer in `docker/docker-compose.cluster.yml`.

### Budget under the pooler

The dangerous deploy-overlap term now applies only to the **direct** budget,
which carries just the session-stateful set — a handful of leader-lock holders
(cron leader, topology reconciler, cognition dispatch/greet/feedback gates,
planner admission) plus the one-shot migrate Job — each a single held
connection, not `replicas × MAX_OPEN_CONNS`. Bulk pods multiplex through the
pooler, whose server-side pool to Postgres is capped (`default_pool_size`)
regardless of client count. So:

```
direct_backends  ≈  Σ(session-stateful holders)  +  migrate (1, transient)
pooler_backends  ≤  pooler default_pool_size                 (independent of pod/client surge)
REQUIRE:  direct_backends + pooler_backends  ≤  max_connections − reserved
```

### Verifying a deploy surge no longer storms (memql#1935)

Prove the storm is gone by re-running the 0.9.87 scenario under the pooler and
watching the instance backend count stay under budget throughout.

1. **Cut staging onto the pooler** (one-time): recreate `memql-secrets` with
   `DSN` → pooler and `DIRECT_DSN` → direct, then roll the pods (identity
   first). Exact command: `deploy/k8s/base/README.md`. With `DIRECT_DSN` unset
   this is a no-op (single-pool fallback), so the cutover is the trigger.
2. **Start the watcher** against the instance (read-only; queries the direct
   endpoint, which sees every backend on the instance):
   ```bash
   DIRECT_DSN="$(tiger db connection-string xahn9ru4v6 --with-password)" \
     scripts/deploy/conn-surge-watch.sh --interval=5
   ```
   It prints, each tick, total backends vs budget + the `application_name`
   breakdown (mesh pods stamp `memql-<type>`, the migrate Job `memql-migrate`),
   and tracks the peak.
3. **Trigger a full-stack roll** — the wedging scenario: blue-green bff + the
   rolling mesh (all node-types). e.g. bump the staging overlay digests / run
   the normal deploy so every Deployment rolls and the bff Rollout does its
   blue-green cutover.
4. **Watch for SQLSTATE 53300** in the pods throughout the roll (the
   authoritative signal):
   ```bash
   kubectl logs -n memql -l app.kubernetes.io/part-of=memql --tail=-1 --prefix \
     | grep -iE '53300|remaining connection slots' || echo "no 53300 — good"
   ```
5. **Stop the watcher** (Ctrl-C) and read its summary.

**Pass / fail (acceptance):**
- **PASS** — the full roll completes with **zero** SQLSTATE 53300 and **no**
  manual connection-reaping / scale-to-0 recovery; the watcher's peak backend
  count stayed under budget (capture peak vs budget in the epic).
- **FAIL** — any 53300, or the peak approached/exceeded the budget (the surge
  still pressures direct slots — re-check that bulk `DSN` actually points at the
  pooler and only the session-stateful set is on `DIRECT_DSN`).

Quick manual cross-check while a roll is in flight (run via the direct DSN):
```sql
-- total backends vs the instance ceiling
SELECT count(*) AS backends,
       (SELECT setting::int FROM pg_settings WHERE name='max_connections') AS max_conn
FROM pg_stat_activity WHERE datname IS NOT NULL;

-- who holds them (the #1817 application_name attribution)
SELECT coalesce(nullif(application_name,''),'(none)') AS app,
       count(*) FILTER (WHERE state='active') AS active,
       count(*) FILTER (WHERE state LIKE 'idle%') AS idle,
       count(*) AS total
FROM pg_stat_activity WHERE datname IS NOT NULL
GROUP BY 1 ORDER BY total DESC;
```

## Continuous monitoring + live deploy gate (memql#1958)

The 0.9.88 cutover stormed because nothing watched the *live* direct budget: an
external `deployer` leak (Tiger control plane, #1822) had silently consumed most
of the slots and the deploy cold-started into the remainder. Two layers now
guard against that:

**1. Continuous monitor** — `deploy/k8s/base/conn-monitor-cronjob.yaml` runs
every 5 min (read-only `postgres:16-alpine`, `DIRECT_DSN` from `memql-secrets`).
It logs total backends vs budget + the per-`application_name` breakdown and
emits greppable lines a log-based alert can key on:
- `CONN-MONITOR WARN: backends N/B (P%) >= 70% ...`
- `CONN-MONITOR CRIT: backends N/B (P%) >= 90% ...`
- `CONN-MONITOR WARN: foreign app 'deployer' holds N conns ... possible leak`
The operator-facing richer version is `scripts/ops/conn-monitor.sh` (same
thresholds; runnable ad-hoc with `DIRECT_DSN=... scripts/ops/conn-monitor.sh`).
A leak — *any* non-mesh `application_name` (not `memql%`) growing past the
threshold — is surfaced with its `client_addr` while it is still small.

**2. Live pre-deploy gate** — `scripts/deploy/conn-headroom-check.sh --live`
extends the projected-demand gate (#1820) to ALSO read the instance's real
`max_connections` and subtract the **live foreign (non-mesh) backends** from the
budget before the peak check. So a deploy into an already-near-full instance
fails fast (`CONN-HEADROOM-FAIL`) instead of cold-starting into a storm:

```bash
# in CI / pre-deploy, with a DSN available:
DIRECT_DSN="$(tiger db connection-string xahn9ru4v6 --with-password)" \
  scripts/deploy/conn-headroom-check.sh --live
# budget = live max_connections - reserved - live foreign backends
```

Without `--live` the gate is the pure manifest projection (unchanged). With it,
the gate would have BLOCKED the 0.9.88 deploy (budget `105-17-63=25` < projected
peak), preventing the storm.

## Future levers (tracked, not yet needed)

- **Connection pooler** — **SHIPPED** (epic memql#1925). Tiger Cloud
  transaction-mode pooling now fronts bulk traffic so pod count no longer maps
  1:1 to DB backends; see "Connection pooling (hybrid endpoint split)" below.
- **Deploy-gate connection-headroom check** (child F): have the gate assert DB
  connection headroom before promoting. Partly covered today by the gate's
  startup/auth resilience (memql#1782); a dedicated headroom metric is the next
  step if budget pressure returns.
