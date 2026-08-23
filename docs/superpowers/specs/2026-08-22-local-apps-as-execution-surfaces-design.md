# Local apps as execution surfaces -- Claude Code and Codex on a cockpit machine, with MemQL's tools over MCP

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project H of nine)
**Owner:** `component/worker` (the protocol), `component/planner` (the executor seam), `integrations/agent/worker`, `dsl/worker`, `dsl/router`, `clients/portal`; the cockpit half in the `memql-cockpit` repository

Sub-project H of the 2026-08-22 backlog brief. Builds on G (epic #4349: the
Fleet router and the cross-node forward) and on F (epic #4339: the Library
routes). Lets the planner hand a task to an app the user already pays for
on a machine they own, with MemQL's tools reachable from inside that app
over MCP, and with the spend accounted as covered by a subscription.

---

## 1. Problem

The brief: "use MCP as a means of sending signals to different apps on a
machine running MemQL Cockpit -- Claude Code, Codex -- because if those are
set up with a subscription it helps with token usage coordination."

What the tree has, at `948de3de`:

- **The `mcp` node is a server.** `app/build_mcp.go`, `component/mcp/`:
  stdio for a local Claude Code / Claude Desktop, streamable HTTP at
  `mcp.<domain>` with identity-JWT verification (PATs rejected,
  `docs/public/operate/mcp-connect.md:50-52`; service-account JWTs are the
  intended machine credential). **No MCP client exists in the engine.**
- **The only channel into a cockpit machine is the stream it opened
  outward.** `WorkerService.Stream` (`component/grpc/worker.proto`): verbs
  fixed (`exec`, `fs_*`, `http_fetch`, mouse / keyboard / screenshot),
  one-shot `ToolDispatch` with a single timeout (`DispatchTimeoutDefault`
  5 m, `component/worker/worker.go:39`), output-only `ToolStream`. The
  de-facto "signal an app" is `workerHost.exec "claude -p ..."` under the
  cockpit's `policy.yaml` allowlist.
- **The seam a local app plugs into exists and is empty.**
  `component/planner/executor.go:83` `RegisterContainerExecutor(name, exec)`;
  `Task.executionSurface=containerExecutor` + `executorBackend`
  (`dsl/planner/concepts.memql:115-116`); `ExecutorRequest` has `TaskId,
  PlanId, AgentId, Kind, Input, Workspace` and **no machine identity**;
  `ExecutorResult.TokensSpent` rolls into `Plan.tokenSpent`;
  `ProgressCallback` events (`command | file | narration | screenshot`) were
  designed for a "watch live" view that has no page.
- **Cost is a ledger with no "covered" column.** `v1:router:call`
  (`dsl/router/concepts.memql:31-66`) records tokens, cost, vendor, model,
  policy, outcome; `Plan.tokenBudget / tokenSpent` gate the planner loop;
  nothing says "this ran on the user's subscription".

Two readings of "use MCP". The owner chose the first: MCP is the
**back-channel** through which a delegated app reaches MemQL's tools; the
worker stream is the **transport** that reaches the machine. The other
reading -- MemQL as an MCP client dialling the cockpit -- would rebuild the
transport (the machine is behind NAT; the stream already is the tunnel) to
change the wire format.

---

## 2. Decisions

### D1 -- Transport is the worker stream; MCP is the back-channel

A new app-session envelope on `WorkerService.Stream`; each run is handed a
per-run credential and the `mcp.<domain>` config so the app works with
MemQL's tools. Chosen over MemQL-as-MCP-client and over signals-only (which
moves no spend); the hand-off "open in app" survives as a session kind.

### D2 -- Sessions, not one-shot dispatches

A headless `claude -p` can run for an hour. The envelope has start, chunks,
control (cancel, credential renewal) and end, no ceiling but the policy's,
and a row per session whose transcript appends live.

### D3 -- The backend registers into the seam that was built for it

`cockpit-app` is the first `RegisterContainerExecutor` inhabitant.
`ExecutorRequest` gains the machine-routing facts it lacked
(`OwnerUserId`, `RequireLabels`); the Fleet router picks the machine; the
forward reaches it on any replica.

### D4 -- Consent is the headless class, reused

An app run edits files and runs commands on the user's machine. It needs
exactly what `workerHost` needs -- per-task approval, standing scope at
`interact` or above, the kill switch, the classifier -- plus the machine's
own `apps.allow`. No new consent class; the card names the app, the machine
and the workspace.

### D5 -- Subscription spend is counted, and it does not burn the budget

`v1:router:call.billing` (`metered | subscription | unknown`),
`Plan.tokenSpentSubscription`; the dollar ceiling excludes subscription
tokens; the loop caps still count the call. The number the owner asked for
-- what the subscription covered -- exists.

### D6 -- Delegation is a preference with a fallback

A per-user delegation policy says which task kinds may go to which apps and
in what order; the planner's triage routes there when a machine with a
signed-in, allowed app is online and otherwise runs the in-process path. A
plan never waits for a laptop to wake up.

### D7 -- The cockpit half lives in its repository

App detection, the session runner, the MCP config writer and `apps.allow`
are cockpit code; this record fixes the protocol and the engine side and
names what the cockpit must do, and its issues are filed there.

---

## 3. The protocol

### 3.1 Apps on the registration

`Register` and `Heartbeat` carry `apps[]`:

| Field | Meaning |
|---|---|
| `id` | `claude-code`, `codex` (closed list in the engine; the cockpit may report others, which are stored and not runnable) |
| `version` | the CLI's version string |
| `signedIn` | the app's own auth state as the cockpit can detect it |
| `subscription` | `unknown | none | present` -- what the app reports, never inferred |
| `allowed` | whether `policy.yaml apps.allow` lists it |

Stored on `v1:worker:registration.apps` and derived into labels
(`app:claude-code=<major.minor>`, `app:codex=...`) that the Fleet router
matches; only `allowed && signedIn` apps produce labels.

### 3.2 The session envelope

Server -> worker: `AppSessionStart {sessionId, app, kind, prompt, inputs[],
workspace, credential, mcpEndpoint, limits}`; `AppSessionControl
{sessionId, action: cancel | renew_credential, credential?}`.
Worker -> server: `AppSessionChunk {sessionId, stream: stdout | stderr |
event, data, seq}`; `AppSessionEnd {sessionId, exitCode, usage {inputTokens,
outputTokens, costUSD?, known}, appSessionRef, producedArtifactIds[],
error?}`.

`kind`: **run** (headless, autonomous), **open** (launch the app with the
workspace and prompt loaded for the human; ends when the window closes or
immediately if the app cannot be opened), **attach** (stream a run the
human started, by the app's session id). `inputs[]` are Library artifact
ids the cockpit pulls through `GET /artifacts/{id}/content` with the
session credential; produced files are pushed with `POST /artifacts`
(sub-project F) and returned as ids.

### 3.3 The session row

`v1:worker:appSession`: `ownerUserId`, `workerId`, `app`, `kind`, `planId`,
`taskId`, `status` (`starting | running | ended | failed | cancelled`),
`workspace`, `transcript` (appended from chunks, bounded; the full
transcript is also a produced artifact at end), `usage`, `billing`,
`producedArtifactIds`, `appSessionRef`, `startedAt`, `endedAt`. Composite
tier. Live in the portal through CDC (sub-project B's admission applies).

---

## 4. The backend, the router, the consent

- `integrations/agent/worker/cockpitapp`: `RegisterContainerExecutor("cockpit-app", ...)`
  from `init()` under the agent build tag. `Run(ctx, req, progress)`: resolve
  the delegation policy; `RequireLabels = {app:<id>}` merged with the task's;
  `Router.Pick` (G); the consent gates (D4) with the card text "run
  `<app>` on `<machine>` in `<workspace>`"; open the session, locally or
  through `WorkerForward` (G); map chunks to `ProgressCallback` events
  (`command`, `file`, `narration`); map `AppSessionEnd` to `ExecutorResult`
  (`TokensSpent`, `Billing`, `DurationMs`, `Output.artifactIds`).
- `ExecutorRequest` gains `OwnerUserId`, `RequireLabels`, `Inputs []string`;
  `ExecutorResult` gains `Billing` and `ArtifactIds`. `ExecutorBackend` names
  are validated at task creation against `RegisteredExecutors()`, so a task
  cannot name a backend nobody registered (today it silently has nowhere to
  land).
- `Task.executorBackend` for this path is `cockpit-app:<appId>`.

---

## 5. The MCP back-channel

At `run` start the engine mints a `class="service_account"` JWT for the
owning user (the identity service's existing issuance, scoped to the
read/query surface and the `@mcp`-tagged tools, lifetime from the
delegation policy, default 4 h) and sends it with `mcpEndpoint =
https://mcp.<domain>/mcp`. The cockpit writes the app's MCP configuration
(Claude Code: a project `.mcp.json` in the workspace; Codex: its equivalent)
naming the endpoint and the bearer, runs the app, and deletes the file at
end. `renew_credential` replaces the bearer for a run that outlives it. The
MCP node's `MEMQL_MCP_MODE` and role apply unchanged; the credential is
audited `app_session_started` / `app_session_ended` with machine, app, plan
and task, and is revoked at end through the service-account revocation that
exists.

---

## 6. Accounting and the delegation policy

- `v1:router:call` gains `billing enum(metered, subscription, unknown)` and
  `executionSurface string`; an app session writes one row from the app's
  reported usage (Claude Code's `--output-format json` carries tokens and
  `total_cost_usd`; an app that reports nothing writes `unknown` and zero).
- `v1:planner:plan` gains `tokenSpentSubscription`; `component/planner/budget.go`
  excludes it from the dollar ceiling and includes the call in the loop
  caps.
- `v1:worker:delegationPolicy` (per user): `preferSubscriptionApps`,
  `eligibleKinds []string` (starting with the planner's coding and
  file-production kinds), `appOrder []string`, `maxConcurrentSessions`,
  `workspaceRoot`, `credentialLifetimeSeconds`. The planner's triage, for an
  eligible task, asks the router whether a machine with an allowed,
  signed-in app in `appOrder` is online; if so the task is created with
  `containerExecutor / cockpit-app:<id>`, else in-process, with the reason
  recorded on the task.

---

## 7. The portal, and the cockpit

- Fleet / Machines (G): each machine's apps (version, signed in, allowed,
  last used); the delegation policy editor beside the routing policy.
- A task with an app session shows the live transcript, the usage and
  billing, and the produced artifacts (links into the Library).
- Cockpit (`memql-cockpit`, filed there): app detection (`claude`, `codex`
  on PATH; versions; auth state), `apps.allow` in `policy.yaml`, the session
  runner (process supervision, streaming, cancel, resume by app session id),
  the MCP config writer and its cleanup, the Library pull/push with the
  session credential, the `open` kind per platform.

---

## 8. Security posture

| Concern | Handling |
|---|---|
| Running an agent on the user's machine | D4: the headless consent class, plus `apps.allow` on the machine |
| The back-channel credential leaking | per-run, short-lived, user-scoped, MCP-surface-pinned, revoked at end, written to a file the cockpit deletes |
| A delegated app reaching rows the user cannot | it acts as the user over MCP; row authz applies as to any stream |
| Prompt injection from Library inputs | the app runs in the workspace with the inputs it was given; the card named them; the transcript is kept |
| Unbounded runs | policy limits (max sessions, credential lifetime); cancel from the portal; the kill switch ends every session |
| Misreported usage | recorded as reported with `known=false` when absent; never inferred |

---

## 9. Testing

1. Protocol: a fake cockpit round-trips start / chunk / control / end;
   renewal replaces the bearer; cancel ends the session; `open` and `attach`
   kinds.
2. Backend: a fixture session maps to an `ExecutorResult` with
   `subscription` billing and artifact ids; each missing gate refuses with
   its reason; an unknown backend name is refused at task creation.
3. Routing: the `app:` label selects the right machine; the cross-node
   forward carries a session (G's e2e extended); a machine whose app is not
   allowed is never selected.
4. Credential: scoped and short-lived; the MCP node accepts it during the
   session and rejects it after end.
5. Accounting: the ledger row and the plan rollup; the dollar ceiling
   ignores subscription tokens; the loop cap counts the call.
6. Triage: an eligible task goes to the backend when a machine is online and
   in-process otherwise, with the reason on the task.
7. Portal: apps on the machine card, the policy editor, the live transcript.

---

## 10. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- protocol and rows | `apps[]` on registration, the session envelope, `appSession`, the credential mint, audit | G's PR 1 (#4350-#4352) |
| 2 -- the backend | seam changes, `cockpit-app`, router + consent, triage, the ledger and plan fields, the delegation policy | PR 1; G's router |
| 3 -- the portal and docs | machines' apps, policy editor, task transcript, docs | PR 1 |

One `Closes #N` line per issue. Cockpit issues are filed in `memql-cockpit`
against this record.

---

## 11. Out of scope

- MemQL as an MCP client (D1).
- Apps beyond Claude Code and Codex (the list is closed in the engine and
  grows by a value).
- Charging or metering the subscription itself (MemQL records what the app
  reports).
- Running apps on the workbench node (a different surface; the workbench
  has no subscription to prefer).
- The product SPA.

---

## 12. References

- Code: `component/grpc/worker.proto`, `component/worker/{server,registry}.go`,
  `integrations/agent/worker/{dispatch,integration}.go`, `component/planner/{executor,budget}.go`,
  `integrations/planner/agent_loop*.go`, `component/mcp/{http,tool_surface}.go`,
  `dsl/{worker,planner,router}/concepts.memql`, `component/identity` (service-account issuance).
- Docs: `docs/public/operate/mcp-connect.md`, `docs/public/operate/workers-runbook.md`,
  `docs/public/ai/llm-cost-control.md`, CLAUDE.md "Coding Agent -- a SEAM, not a
  running deployment".
- Related: epic #4349 (G), epic #4339 (F), memql#1556 (OAuth for MCP
  connectors), memql#4120 (the empty executor registry).
