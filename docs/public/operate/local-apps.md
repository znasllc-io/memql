---
title: Local apps as execution surfaces
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Local apps as execution surfaces

Delegating a planner Task to an app the user **already pays for**, running on
a machine they own, with MemQL's tools reachable from inside that app over
MCP. Epic [memql#4358](https://github.com/znasllc-io/memql/issues/4358);
design record
`docs/superpowers/specs/2026-08-22-local-apps-as-execution-surfaces-design.md`.

The engine drives exactly two apps today — **Claude Code** (`claude-code`) and
**Codex** (`codex`) — and that set is closed in the engine on purpose. See
[Why the app set is closed](#why-the-app-set-is-closed).

---

## The shape of it

```
   planner Task                                    the user's machine
   executionSurface=containerExecutor
   executorBackend=cockpit-app:claude-code
        │
        ▼
   cockpit-app backend  ──── consent gates ────►  refused, with a reason
   (agent node)                                    naming which gate
        │
        │  AppSessionStart {credential, mcpEndpoint, workspace, prompt}
        ▼
   WorkerService.Stream  ═══════════════════════►  memql-cockpit
        │                                              │
        │  ◄══ AppSessionChunk (stdout/stderr/event) ══ │ runs `claude -p`
        │  ═══ AppSessionControl (cancel / renew) ════► │
        │  ◄══ AppSessionEnd {exitCode, usage, ...} ═══ │
        ▼                                              │
   v1:worker:appSession                                │  MCP over HTTPS
   v1:router:call (billing=subscription)               ▼
                                          mcp.<domain>/mcp  ◄── the app,
                                                             acting as the user
```

**The engine cannot dial the machine.** It is behind NAT. The stream the
cockpit opened *outward* is the only channel, so it is the transport, and MCP
is the back-channel the app uses to reach back in.

---

## Why the app set is closed

`claude-code` and `codex` are the only ids the engine will open a session for.
A cockpit may report others — they are stored on
`v1:worker:registration.apps` and are never given a routing label, so they are
visible in the portal and unroutable.

This is the direction that fails safe. A cockpit ships on its own cadence; if
an unknown id could be driven, a newer cockpit reporting a third app would make
the engine attempt a protocol it does not have, at dispatch time, inside
somebody's plan. Growing the set is a value change in
`component/worker/apps.go` — not a wire change.

## Why a machine must be BOTH allowed and signed in

An `app:<id>` routing label is derived only from an entry that is:

| | |
|---|---|
| `allowed` | the machine's own `policy.yaml apps.allow` lists it |
| `signedIn` | the app's auth state, as the cockpit can observe it |

Either one false means no label, which means the router cannot select that
machine. The alternative — selecting on presence and discovering the refusal
on the far side — commits a plan to a machine that then rejects it, and the
resulting failure names the wrong thing.

Signing into an app takes effect on the **next heartbeat**, not the next
reconnect: an inventory change is applied to the live registry immediately and
persisted outside the 60-second `lastSeenAt` throttle, because it is a routing
change. Persisting it there is also what lets a **planner** node answer "is a
machine with this app online" — that node holds no worker streams at all, so
the row and its derived labels are the only thing it can read.

### Which machine, and on which replica

Selection is the **Fleet router**
([epic memql#4349](https://github.com/znasllc-io/memql/issues/4349)), asked for
the `app:<id>` label as a requirement. It applies the owner's routing policy,
orders by their chosen strategy, and knows which replica holds each machine's
stream. Nothing in this feature picks between machines itself — a second
selector would disagree with the first, and the plan would commit to a machine
that then refused.

**A session runs only on the replica holding that machine's stream.** The
tool-dispatch path can forward across nodes (`WorkerForward`, memql#4352); the
app-session envelope cannot yet. A machine attached to a different agent
replica is therefore skipped during selection rather than failing the run —
which makes it a routing outcome instead of an error, and is why the refusal
says "on a stream this replica holds".

> A cockpit that reports no apps at all is normal — that is what a build older
> than the app protocol does, and what a machine with neither binary on PATH
> does. It is not an error state.

---

## Sessions, not dispatches

A `ToolDispatch` carries one timeout and returns one result. A headless
`claude -p` can run for an hour and emits output the whole way, so an app run
is a **session**:

| Message | Direction | Carries |
|---|---|---|
| `AppSessionStart` | server → worker | app, kind, prompt, inputs, workspace, credential, mcpEndpoint, limits |
| `AppSessionChunk` | worker → server | stream (`stdout` / `stderr` / `event`), data, seq |
| `AppSessionControl` | server → worker | `cancel` or `renew_credential` |
| `AppSessionEnd` | worker → server | exitCode, usage, appSessionRef, producedArtifactIds |

Three kinds:

- **`run`** — headless and autonomous. The engine reads the output.
- **`open`** — launch the app for the HUMAN with the workspace and prompt
  loaded. Ends when the window closes, or immediately with a failure if the
  app cannot be opened.
- **`attach`** — stream a run the human started, named by `app_session_ref`.

> **`run` is the only kind anything initiates today.** The protocol carries all
> three and the runner accepts all three; `open` and `attach` have no
> engine-side caller yet, because a planner Task is autonomous by definition —
> they are for a portal hand-off and a resume, neither of which exists. Said
> plainly here rather than implied, because the same section of CLAUDE.md spent
> two years describing a coding agent nothing ran. The cockpit half implements
> all three (memql-cockpit#347 / #350); the engine-side initiator is the
> missing piece.

**Chunk `seq` is monotonic, and out-of-order or duplicate chunks are dropped.**
A transcript is a record; silently interleaving a replayed chunk corrupts it in
a way no later reader can detect.

**A caller context that dies cancels the run on the machine.** Otherwise a
plan that was cancelled leaves a headless agent working on somebody's laptop.
A worker disconnect ends every live session with a named error rather than
leaving callers parked until their own deadlines expire.

---

## The back-channel credential

At session start the engine mints a `class="service_account"` JWT and sends it
with `mcpEndpoint = https://mcp.<domain>/mcp`. The cockpit writes the app's MCP
configuration naming both, runs the app, and **deletes that file at end**.

**Its `sub` is the owning user's id.** That single choice is the security
story: the app reads over MCP *as that user*, so row authz applies to it
exactly as it does to their browser. The service-account interceptor pins the
surface to read/query and stamps `role=system` rather than `owner`, so the
credential reaches no credential mutation and no cluster-owner gate.

### It is not revocable, and this doc will not pretend otherwise

The verify path is JWKS-only and DB-free — that is what lets it work on every
node without a lookup — so there is no row to strike. Revoking one issued token
means rotating the cluster's signing key, which invalidates every other token
in the cluster. That is not a per-session operation.

Three things stand in for revocation:

1. The lifetime is short and **hard-capped at 8 hours** by the identity
   service, whatever the delegation policy asks for.
2. The cockpit deletes the MCP configuration file at session end, which is
   where the bearer actually lives.
3. A run that outlives its credential is handed a **replacement**
   (`AppSessionControl{renew_credential}`) rather than a longer-lived bearer up
   front, so no single bearer is ever long-lived.

**Residual exposure:** the unexpired remainder of one short-lived,
user-scoped, read-surface-pinned bearer. That is the trade the DB-free verify
path buys.

### The mint widens the bootstrap secret

`POST /node/bootstrap` with `tokenClass="app_session"` is the mint surface.
Before this, that secret bought only **machine** principals (`node`,
`voice_agent`); it now also buys a **user-scoped** one. Narrowing it:

- the named user must exist (a forged subject would verify and then act as a
  user nobody can point at in an audit);
- the TTL is clamped at the identity service;
- the surface stays read/query-pinned;
- the session id is the token label, so a leaked bearer traces back to the run
  it was handed to.

An operator who does not want this path at all leaves
`MEMQL_NODE_BOOTSTRAP_TOKEN` unset, which darkens the whole endpoint exactly
as it does today.

---

## Consent

An app run edits files and runs commands on somebody's own computer. It gets
**exactly** the gates `workerHost` gets — the backend calls the same
`preDispatchCheck` function, in the same package, rather than a copy:

1. **Per-task approval** — the Task must carry a `PlanId` from an approved
   scope-elevation Plan.
2. **The kill switch** — `v1:identity:user.preferences.computerUseEnabled`.
3. **Standing scope** — the agent's `agentAuthorization.computerUseScope` must
   be `full`; an app run does shell exec and file writes.
4. **The safety classifier**, fail-closed on this surface.
5. **Plus `apps.allow`** on the machine itself, which the cockpit enforces and
   the routing label reflects.

A refusal names which gate refused it.

---

## Delegation is a preference with a fallback

`v1:worker:delegationPolicy`, one row per user, edited at **/machines** in the
portal.

| Field | Meaning |
|---|---|
| `preferSubscriptionApps` | the master switch. False (the default, and the state of an absent row) means never delegate |
| `eligibleKinds` | task kinds that may be delegated. An **empty list allows nothing** — opting in does not opt every kind in with it |
| `appOrder` | which apps to try, in order. An app not listed is never selected even on a machine that has it |
| `maxConcurrentSessions` | live sessions across every machine. `0` reads as the default of 1, never as "none" |
| `workspaceRoot` | where per-run directories are created. The cockpit still gets to veto a path outside its own roots |
| `credentialLifetimeSeconds` | default 4h, clamped to 8h at the mint |

**If no machine with an allowed, signed-in app is online, the task runs
in-process.** A plan never waits for a laptop to wake up: a delegation design
that can block turns "my laptop was asleep" into "my plan hung", which is worse
than the cost it was trying to save.

`task.delegationReason` records the outcome on **both** branches:

| Reason | Means |
|---|---|
| `delegated` | a machine was found |
| `delegation_not_enabled` | the master switch is off |
| `kind_not_eligible` | the kind is not in `eligibleKinds` |
| `no_apps_configured` | `appOrder` is empty |
| `no_machine_with_app_online` | nothing online has an allowed, signed-in app from `appOrder` |
| `max_concurrent_sessions_reached` | the cap |
| `task_has_no_owner` | a machine-touching backend cannot run unattributed |

The reasons are checked cheapest-first so the recorded one points at the real
cause: somebody with delegation switched off is told *that*, rather than "no
machine online" — which would send them to check a laptop that was never going
to be consulted.

---

## Accounting

Every session writes one `v1:router:call` row, **including failures**: a run
that burned an hour of somebody's subscription and then crashed still spent it,
and a cost surface that records only successes understates what work cost.

- `billing` ∈ `metered | subscription | unknown`
- `executionSurface` = `cockpit-app:<appId>`
- `plan.tokenSpentSubscription` accumulates the covered spend separately from
  `plan.tokenSpent`

**The dollar ceiling excludes subscription tokens; the loop caps include the
call.** The two caps want opposite answers, so they are counted in different
places. Counting covered tokens against the ceiling would park a plan over
money nobody was charged — and the more a user leans on the subscription they
already pay for, the sooner their plans would stop, which is backwards. A cap
blind to those calls, meanwhile, is a hole the cheapest path walks straight
through.

**Cost is never synthesised.** MemQL knows its own providers' per-token prices,
not the prices inside somebody's subscription. `totalCost` carries what the app
reported (`claude --output-format json` gives `total_cost_usd`) and nothing
when it reported nothing. `billing` falls to `unknown` whenever the app's usage
report or the machine's subscription signal is silent — the number the owner
asked for is only worth having if silence stays visible as silence.

---

## Operating it

```bash
# Is the machine reporting apps at all?
psql "$MEMQL_DATABASE_DSN" -c \
  "select payload->>'name', payload->'apps' from memory_nodes
   where concept='v1:worker:registration' order by created_at desc limit 5;"

# Which sessions are live right now?
psql "$MEMQL_DATABASE_DSN" -c \
  "select id, payload->>'app', payload->>'status', payload->>'billing'
   from memory_nodes where concept='v1:worker:appSession'
     and payload->>'status' in ('starting','running');"

# Did the agent node install the executor?
kubectl logs -n memql deploy/agent | grep 'cockpit-app container executor installed'
```

### Symptoms

| What you see | What it means |
|---|---|
| `cockpit-app: the backend is registered but not wired on this node` | the Task reached a node with no WorkerService. Only an agent node serves one |
| `cockpit-app: no credential minter configured` | `MEMQL_IDENTITY_VERIFIER_BASE_URL` or `MEMQL_NODE_BOOTSTRAP_TOKEN` is unset on the agent. The run is REFUSED rather than started with a blank bearer — an app with no credential reaches nothing over MCP and reports that as "MemQL's tools are broken" |
| `no machine online with claude-code allowed and signed in` | check the portal's /machines page; the badge says which half is missing |
| `executorBackend "..." is not registered` at task creation | the name was validated against `RegisteredExecutors()`. With an empty registry the message says so |
| A session stuck in `starting` | the node holding it died. The row is the record; a live session cannot survive its node |

### Environment

No new variables. The path reuses `MEMQL_IDENTITY_VERIFIER_BASE_URL`,
`MEMQL_NODE_BOOTSTRAP_TOKEN` and `MEMQL_MCP_PUBLIC_URL`.

---

## The cockpit half

Filed in [`memql-cockpit`](https://github.com/znasllc-io/memql-cockpit) against
memql#4364: app detection (`claude` / `codex` on PATH, versions, auth state),
`apps.allow` in `policy.yaml`, the session runner (process supervision,
streaming, cancel, resume by app session id), the MCP config writer and its
cleanup, the Library pull/push with the session credential, and the `open` kind
per platform.

## Related

- [Workers runbook](workers-runbook.md) — the tool surface this shares its consent gates with
- [MCP connect](mcp-connect.md) — the endpoint the back-channel dials
- [LLM cost control](../ai/llm-cost-control.md) — the layered spend guardrails
- [Service-account JWTs](auth/service-account-jwt.md) — the credential class
