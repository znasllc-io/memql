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
- **Platform-side idle holders** (memql#1822): some idle backends are not the
  app at all. On Tiger Cloud, the managed **control plane** holds idle sessions
  stamped `application_name=deployer` as the `postgres` superuser (TimescaleDB
  extension management) — un-killable by the customer `tsdbadmin` role and not a
  memql leak. Diagnose with `scripts/ops/conn-recover.sh deployer-inspect`
  (read-only). They're cleared by cycling the service
  (`tiger service stop … && tiger service start …`) or a database-level reaper
  (`ALTER DATABASE … SET idle_session_timeout`); a recurring platform holder is a
  Tiger Cloud support ticket. Budget for them as part of `reserved_connections`.
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

## Future levers (tracked, not yet needed)

- **Connection pooler** (pgbouncer sidecar or Tiger Cloud pooling, transaction
  mode) to decouple pod count from DB connections — epic memql#1778 child C.
  Deferred while right-sizing + scaleDownDelay keep the budget under the ceiling;
  revisit if replica counts grow.
- **Deploy-gate connection-headroom check** (child F): have the gate assert DB
  connection headroom before promoting. Partly covered today by the gate's
  startup/auth resilience (memql#1782); a dedicated headroom metric is the next
  step if budget pressure returns.
