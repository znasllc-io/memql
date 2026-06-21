---
audience: internal
status: current
area: ops
sinceVersion: "0.9.78"
owner: platform
---

# Spike: Postgres connection exhaustion (SQLSTATE 53300) on deploy/test

**Issue:** memql#1817 · **Status:** root cause identified · **Date:** 2026-06-20

Deliverable: understanding + recommendation. The robustness code/config
fixes that are unambiguously correct ship with this spike; the capacity
decision (right-size instance vs. pooler) is an owner call captured below.

## Problem

Repeated `FATAL: remaining connection slots are reserved for roles with
privileges of the "pg_use_reserved_connections" role (SQLSTATE=53300)` on
the staging Tiger Cloud (TimescaleDB) instance, during deploys **and**
during a forge validation test burst. It does **not** self-recover.

### Evidence (staging, 2026-06-20)

Forge validation run: early single reads/writes succeeded; a burst of 3
concurrent reads right after submitting a request returned `53300` +
"Session terminated"; still failing after a 20s idle wait; one write
returned "Session terminated" but the row committed (committed write,
lost response).

Captured live during this spike:

- **Instance is tiny.** `tiger service describe xahn9ru4v6` →
  `memql-staging`, **0.5 CPU / 2 GB RAM, DEV tier**. On Tiger Cloud
  `max_connections` is **configurable** (range **25–500** for 0.5 CPU/2 GB
  through 4 CPU/16 GB) with **17 connections reserved** (12 superuser + 5
  Tiger ops). The exact configured value couldn't be read live (every slot
  was taken), but the binding constraint is RAM: at ~10 MB/connection a 2 GB
  box can't safely run a high `max_connections`. That reserved-17 detail is
  also why even the admin role got `53300` — the app role had taken every
  non-reserved slot.
- **DB is hard-saturated hours after the test, at zero load.** Direct
  `psql` as `tsdbadmin` returns `53300` on every attempt; 40 retries
  over ~60s never caught a free slot. This is the "no self-recovery"
  signal: connections are held persistently, not a transient surge.
- **No connection pooler.** `tiger db connection-string --pooled`
  returns empty (not available on this DEV service) and the fleet
  connects direct.
- **All nodes run the hardcoded pool defaults** (confirmed via
  `kubectl`): `MAX_OPEN_CONNS` unset → **10**, `MAX_IDLE_CONNS` unset →
  **3**, across bff/cognition/agent/planner/identity/mcp/voice/workbench.
- **Fleet is bigger than the budgeted 17 pods:**
  - **bff = 4 pods** — two ReplicaSets (`699fcb6477` ×2 + `9c5b87cc5`
    ×2). The blue-green **old color never scaled down** (still up 13h
    later, not the 300s `scaleDownDelaySeconds` from #1780).
  - **cognition** has an `Error` pod; **planner** has a
    `ContainerStatusUnknown` pod (1 restart); **workbench** has a
    `0/1 Running` pod. Ungracefully-killed pods skip graceful shutdown,
    so their Postgres backends become **orphaned zombies**.

## Capacity math (H1 — PRIMARY cause)

~20 DB-connecting pods.

| | per-pod | × ~20 pods |
|---|---|---|
| Idle floor (`MaxIdleConns`) | 3 | **~60** |
| Max under load (`MaxOpenConns`) | 10 | **~200** |

Against a 2 GB instance cap (~75–150) minus superuser-reserved slots,
**steady state already sits at/over the ceiling**, with **no pooler** to
decouple pod count from DB connections. A burst of 3 concurrent reads
then tips it over → `53300`.

`SetConnMaxIdleTime(10m)` only reaps *idle* connections in the pool — it
never reaps a connection that is checked out, nor a backend orphaned by a
dead pod. With no DB-side idle reaping, those persist → no recovery.

## Shutdown / leak audit (H3, H4 — REFUTED as the cause)

Two independent code sweeps found **no connection leak**:

- Single shared `*sql.DB`/`*bun.DB` pool per process; no second pool
  anywhere (`app/database.go`).
- `Database.Stop()` → `cleanupAfterRun()` **closes `bunDB` + `db`** on
  SIGTERM (`component/database/database.go:848-870`); `app/run.go` calls
  it in `Order()` sequence with a shutdown budget.
- Every advisory-lock `db.DB.Conn()` borrower releases on all paths:
  `component/automations/cron_leader.go` (cleanup lifecycle hook),
  `integrations/cognition/dispatch_gate.go` (callers `defer release`),
  `integrations/planner/admission.go` (deferred unlock + Close).
- Generic reads go through **bun** (manages rows); raw `QueryContext`
  sites (`integrations/planner/fairness.go`,
  `integrations/similarity/capabilities.go`) all `defer rows.Close()`.
- **Forge has no self-trigger loop:** only `routeRequest` (`node.created`
  on `v1:forge:request`); its routing writes emit `node.updated` but
  nothing is wired to fire on those → no cascade (task 5 refuted).

## Deploy choreography (H2 — CONTRIBUTING)

- preStop drain only on **bff** (`sleep 5`); the other 7 node types have
  **no preStop** → SIGTERM (then SIGKILL after grace) without draining
  the pool, which both spikes the peak and creates orphans.
- blue-green old color not scaling down (4 bff pods) persistently doubles
  bff's connection footprint.

## No DB-side safety net (epic #1778 child G — never shipped — CONTRIBUTING)

`idle_in_transaction_session_timeout` is unset and there are no
aggressive TCP keepalives, so **orphaned backends from dead/killed pods
are never reaped** and the cap stays pinned for hours.

## Root cause (ranked)

1. **Capacity vs pool-math (H1):** ~20 pods × default pool, no pooler, on
   a 2 GB instance whose `max_connections` is below even the idle floor
   under load.
2. **No DB-side reaping (child G):** orphaned backends from ungraceful
   pod deaths persist → no recovery.
3. **Deploy choreography (H2):** blue-green old color not draining + no
   preStop on most nodes amplifies the peak and creates orphans.
4. **REFUTED:** code connection leak (H3/H4); forge self-trigger loop.

## Recommendation

### Shipped in code/config with this spike (env-agnostic)

1. **Cut the idle floor.** `MaxIdleConns` default 3 → 1 and
   `ConnMaxIdleTime` 10m → 2m (`component/database/database.go`). Idle
   floor across ~20 pods drops from ~60 to ~20, reaping to near-zero
   within 2 min of inactivity.
2. **DB session safety net on every connection** (`sessionConnParams()` via
   `pgdriver.WithConnParams`, applied to every backend incl. reconnects —
   the missing child G, in code so it holds in every env):
   - `idle_in_transaction_session_timeout` = 60s — reap a session wedged
     mid-transaction.
   - `idle_session_timeout` = 5m — reap orphaned backends left by an
     ungracefully-killed pod (the "no self-recovery" fix). Set ABOVE the
     client's 2m idle reaping so it only bites connections the client can
     no longer close; long-lived holders (cron leader polls every 10s)
     never trip it. Knobs: `MEMQL_DB_IDLE_IN_TX_TIMEOUT_MS`,
     `MEMQL_DB_IDLE_SESSION_TIMEOUT_MS` (0 disables).
3. **`application_name` stamped per backend** (`MEMQL_NODE_TYPE`/hostname)
   so `pg_stat_activity` attributes connections to a node type — future
   53300 triage becomes a `GROUP BY`. Override: `MEMQL_DB_APP_NAME`.
4. **preStop drain on every DB-connecting node**, not just bff
   (`deploy/k8s/base/*.yaml`), so the pool closes gracefully before SIGKILL
   instead of orphaning backends.

### Operational (no code)

- The bff blue-green Rollout was sitting **`Paused`/`BlueGreenPause`** for
  13h (preview + active = 4 bff pods). Promote or abort stale rollouts
  promptly; `scripts/ops/conn-recover.sh` aborts it as part of recovery.

### Owner decision — LOCKED: right-size the instance

Tiger's managed pooler isn't available on this DEV tier, and shrinking
`MAX_OPEN_CONNS` low enough to fit a 2 GB cap (~2–3) would starve per-pod
concurrency. Decision (2026-06-20): **resize the staging Tiger service to
2 CPU / 8 GB and set `max_connections` ≈ 300** — comfortably above
Σ(pods × MaxOpen=10) ≈ 200 + reserved 17, with headroom for a full-stack
roll surge. ~$140–220/mo est. (region `az-eastus2`; exact figure in the
Tiger console compute calculator). The code fixes reduce the floor and
stop the bleed; the resize makes capacity durable. Tracked as the resize
follow-up off #1817.

## Leaked `application_name=deployer` pool (memql#1861)

While root-causing the deploy connection storm (#1858) we found a separate,
durable leak: **~28 idle connections, up to 12h old** on the staging instance,
all stamped **`application_name='deployer'`**, owned by the **`postgres`
superuser**, from a single client (Tailscale `100.126.218.80`), each last-running
`pg_advisory_unlock(...)` and growing ~1 connection per ~30 min. With
`max_connections=105` and ~17 reserved, that leak ate roughly **a third** of the
usable mesh budget — a major reason #1858 had to pin `MAX_OPEN_CONNS=4`.

**It is NOT a `deployer` role.** There is no Postgres role named `deployer`
(`ALTER ROLE deployer` → "role does not exist"). The leak is a *client* — a
deploy/migration-type process connecting as the `postgres` superuser and
stamping `application_name=deployer` — that opens a connection and never closes
it. (The app fleet itself connects as the Tiger master role `tsdbadmin` and
reaps its own idle connections client-side via `CONN_MAX_IDLE_TIME_MS` + the
per-session `idle_session_timeout` stamped in `component/database/database.go`,
#1817 — so the app is not the leaker.)

**Reclaim (owner-run, needs SUPERUSER).** The leaked sessions are owned by
`postgres`, and *only a superuser may terminate a superuser's sessions* — so
`tsdbadmin` (the recovery DSN) CANNOT kill them. Reclaim with the
postgres-superuser DSN (the same credential the leaking tool uses):

```bash
SUPERUSER_DSN='postgresql://postgres:…@<host>:<port>/tsdb?sslmode=require' \
  scripts/ops/conn-recover.sh ... # or the staged /tmp reclaim helper
# terminates idle sessions WHERE application_name='deployer'
```

`conn-recover.sh deployer-inspect` (read-only, runs as `tsdbadmin`) still finds
the leak and prints the source `client_addr`. `deployer-reclaim` attempts the
terminate but needs a superuser DSN to succeed against `postgres`-owned sessions.

**Durable fix = stop the source.** There is no role to put a server-side reaper
on, so terminating only reclaims the *current* leak — the source process keeps
re-leaking until it is stopped or made to close its pool / set an idle timeout.
Identify it by the `client_addr` from `deployer-inspect`.

After the reclaim sticks, the ~28 slots are back and the per-pod
`MAX_OPEN_CONNS` (cut to 4 under #1858) can be raised back toward the default 10
— see [the budget standard](../../public/operate/db-connection-budget.md).

## bff is Rollout-managed — never scale its Deployment up (memql#1868)

`bff` is an argo **Rollout** that adopts the `bff` Deployment via `workloadRef`
(`scaleDown: onsuccess`). The Deployment is **only the pod template and must stay
scaled to 0** — the Rollout owns the serving pods, and ArgoCD ignores the
Deployment's `/spec/replicas` (see `deploy/argocd/apps/rollouts.yaml`). Scaling
`deployment/bff` **up** spawns a SECOND bff ReplicaSet that double-draws DB
connections and is itself a 53300 driver. A recovery drain that scales
`deployment/bff` back to 2 on restore recreates exactly that overlap — so
`conn-recover.sh` keeps bff out of the Deployment scale set and pins
`deployment/bff` to 0 (`enforce_bff_rollout_invariant`, memql#1868). To drain
bff's own connections, scale the Rollout via `kubectl argo rollouts`, not the
Deployment.

## Reproduction / verification

- Reproduce: roll a full-stack deploy while issuing a small concurrent
  read load; watch `pg_stat_activity` cross the cap (requires a free
  admin slot — see recovery).
- Recovery of the currently-wedged DB requires freeing connections
  (owner-gated cluster action): `scripts/ops/conn-recover.sh` scales the
  fleet down to release pools, captures the `pg_stat_activity`
  breakdown + `max_connections`, then scales back up.

## Follow-ups

Scoped off this spike (see memql#1817):

- **Resize staging Tiger to 2 CPU / 8 GB, `max_connections` ≈ 300**
  (owner-run; decision locked 2026-06-20). Recovery + the resize use
  `scripts/ops/conn-recover.sh`.
- **Deploy-gate connection-headroom check** (epic #1778 child F): block a
  promotion whose projected Σ(pods × MaxOpen) + surge would exceed
  `max_connections − reserved`.
- **Reclaim the leaked `application_name=deployer` pool + stop the source**
  (memql#1861, owner-run, needs SUPERUSER): terminate the idle sessions, then
  stop the leaking client (identified by `client_addr` from
  `conn-recover.sh deployer-inspect`). Then re-evaluate whether `MAX_OPEN_CONNS`
  can be raised off the #1858 floor of 4.
- **Confirm the `pg_stat_activity` breakdown** once a slot is free (run
  `scripts/ops/conn-recover.sh capture`) and append the table here — now
  attributable by `application_name` thanks to the stamping above.
