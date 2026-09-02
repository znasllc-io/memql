---
title: Workers — Operations Runbook
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Workers — Operations Runbook

This document is the operator-side reference for the worker
subsystem (computer-use feature). It is the single source of
truth now that the implementation plan has shipped end-to-end.

---

## 1. What is a worker?

A worker is the **user's own machine** running
`memql worker run`. It connects to a MemQL cluster via
`WorkerService.Stream` (gRPC bidi), advertises its capabilities
(HEADLESS, optionally COMPUTERUSE), and accepts dispatched tool calls
(`workerHost.*`, `workerComputer.*`) from agents acting in
sessions owned by the same user.

Per-user routing means there is no shared pool — agents only ever
see workers owned by the user whose session they're acting in.

---

## 2. Install

Pick the installer for your OS. `install-mac.sh` and `install-linux.sh` ship
from the `memql-cockpit` repo (the worker is a run mode of the `memql`
command that repo builds), not from this engine repo's `scripts/install/`
-- that directory carries the cluster-bring-up installers (`mkcert-setup.sh`,
`hosts-entries.sh`, `install-binary.sh`, ...) and has no `install-mac.sh` /
`install-linux.sh` of its own.

Each command below is ONE physical line, deliberately: terminal paste handling
splits the multi-line backslash form, so `bash -s --` runs alone and every flag
after it then runs as a command of its own (`--token ...: command not found`).
The pairing panel on `/fleet/machines` (section 5.5) emits exactly this shape,
with the token and cluster URL filled in.

### macOS

```bash
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-mac.sh | bash -s -- --token mql_wkr_xxxxxxxxxxxx --cluster https://app.example.com --computeruse
```

The install script:

1. Downloads the appropriate binary
   (`memql-darwin-arm64`, or `memql-computeruse-darwin-arm64` with
   `--computeruse`) and installs it as **`memql`** -- one installed command
   for both build variants.
2. Writes `~/.memql/worker.yaml` with the token + cluster URL.
3. Drops a LaunchAgent at
   `~/Library/LaunchAgents/com.znasllc.memql-worker.plist`
   and `launchctl load`s it.

The first time you run the computer-use variant, macOS will prompt for
**Accessibility** and **Screen Recording** permissions (System
Settings → Privacy & Security). Approve both, then re-run the
service:

```bash
launchctl unload ~/Library/LaunchAgents/com.znasllc.memql-worker.plist
launchctl load   ~/Library/LaunchAgents/com.znasllc.memql-worker.plist
```

For an interactive walkthrough that probes both permissions,
opens System Settings to the right pane, and verifies per-binary
TCC status, run:

```bash
memql worker setup
```

The setup flow is plain sequential terminal output (the cockpit's TUI is
retired): it probes each permission, pauses for approval where a grant is
missing, and re-probes. Pass `--non-interactive` for scripted installs -- it
reports what is missing and never prompts, exiting **4** when a permission has
not been granted and **5** when a probe itself failed. An installer reading
`$?` can act on the difference; a prompt blocking in a pipe reads as a hung
install rather than as a missing grant.

### Linux

```bash
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-linux.sh | bash -s -- --token mql_wkr_xxxxxxxxxxxx --cluster https://app.example.com --computeruse
```

The install script writes a user-systemd unit at
`~/.config/systemd/user/memql-worker.service` and starts
it. On Wayland the worker registers HEADLESS only; X11 sessions
get COMPUTERUSE as well.

---

## 3. Configure

`~/.memql/worker.yaml`:

```yaml
cluster_url: https://app.example.com
token: mql_wkr_<your token>
name: jose-mac-mini
labels:
  os: darwin
  arch: arm64
  has-blender: true   # operator-defined for label-based routing
concurrency:
  HEADLESS: 8
  COMPUTERUSE: 1
state_dir: ~/.memql/state
log_level: info
capabilities:
  - HEADLESS
  - COMPUTERUSE
```

**`cluster_url` states its transport, and a bare `host:port` means
PLAINTEXT.** The worker turns this value into a dial address plus a
TLS flag (`sdk/go/worker.ParseClusterURL`): `https://` and `wss://`
dial with TLS on 443 unless another port is given; `http://` and
`ws://` dial in the clear; a value with no scheme is dialled in the
clear whatever its port, because a port number is not evidence of a
transport -- `agent.example.com:443` and `agent.example.com:8443` are
equally silent about TLS. Pairing (`memql worker pair`) writes
this file for you and the server now supplies the scheme, so this only
bites a hand-written config. If you edit it, write the scheme
(memql#3437).

`~/.memql/policy.yaml` (optional) controls allow/deny for shell,
fs, and HTTP tools, plus per-call resource limits and the optional
setuid drop for `exec`:

```yaml
shell:
  allow: ["pytest", "pytest-watch"]   # extends the default allowlist
  deny: ["ssh"]                       # adds to the sticky deny list
  # Per-call rlimits applied to the parent process before fork+exec.
  # Zero / unset = inherit. Linux honours all three; macOS no-ops
  # max_memory_mb (no portable RLIMIT_AS).
  max_cpu_seconds: 60
  max_memory_mb: 512
  max_open_files: 256
  # Optional setuid drop for the child exec process. Requires the
  # cockpit-worker to be running as root (or with the appropriate
  # capability); silently inherits the worker's uid otherwise.
  run_as_user: memql-worker-exec
fs:
  workspace_root: ~/work/agent-sandbox
  allow:
    - ~/work/agent-sandbox
    - /tmp/agent-scratch
http:
  allow_urls:
    - https://api.openai.com/
  deny_urls:
    - https://internal.corp/
```

Reload after edits:

```bash
kill -HUP $(pgrep memql)
```

---

## 4. Permission model

Three independent gates run **before** every dispatch:

1. **Layer 1 — agent capability flag.** The agent must carry
   `computer_use_headless` and/or `computer_use_embodied` in
   `v1:agents:agent.capabilities` (legacy umbrella `computer_use`
   was split by mode on 2026-05-17 -- see CLAUDE.md, Workers
   section). The headless slug carries the `workerHost` family;
   the embodied slug carries `workerComputer`. Set on create,
   edit on the agent panel. Workbench (the sandboxed Linux
   default for headless work) is governed separately by
   `workbench_use` and is on by default for every role.
2. **Layer 2 — standing scope.** The user grants the agent a
   tier on the agentAuthorization row:
     - `observe` — read-only (screenshot, fs_read/list/stat,
       http_fetch GET, cursor + display + window-list).
     - `interact` — adds mouse + keyboard + window_focus.
     - `full` — adds shell exec + fs_write + full HTTP methods.
   Plans can declare a NARROWER scope at creation time; widening
   is rejected with `denied_by_scope`.
3. **Layer 3 — kill switch.**
   `User.preferences.computerUseEnabled` (default true).
   Floating widget in the frontend's space chrome flips it. When
   false, every dispatch is rejected with `kill_switch_engaged`
   and the `killSwitchSuspendsRunningPlans` automation
   transitions running plans to `awaitingFeedback`.

A single denial transitions the calling plan to
`awaitingFeedback` with `feedbackReason=scope_elevation_required`.
The user approves or denies on the canvas card.

---

## 4b. Local apps on a worker (epic memql#4358)

A worker is not only a tool surface. If the machine has **Claude Code** or
**Codex** installed and signed in, the planner can hand it a whole Task and let
that app do the work — on the user's own subscription, with MemQL's tools
reachable from inside the app over MCP. Full record:
[local-apps.md](local-apps.md). The parts an operator of *this* runbook needs:

**The cockpit reports its apps on `Register` and every `Heartbeat`**, and the
engine derives `app:<id>` routing labels from them. A label appears only when
the entry is BOTH `allowed` (this machine's `policy.yaml apps.allow`) and
`signedIn` — so a machine that merely has the binary is never selected, and
signing in takes effect on the next beat rather than the next reconnect.

```yaml
# policy.yaml on the machine
apps:
  allow:
    - claude-code
    - codex
```

**A run is a session, not a tool call.** `AppSessionStart / Chunk / Control /
End` on the same `WorkerService.Stream`, with kinds `run` (headless), `open`
(hand off to the human) and `attach` (stream a run they started). Sessions do
NOT take a slot from the per-capability tool concurrency pools: a session is
bounded by the delegation policy's `maxConcurrentSessions`, and blocking an
hour-long run behind a five-minute tool queue would deadlock the caller for
reasons nothing in the request states.

**Consent is this runbook's model, unchanged.** The app-session path calls the
same `preDispatchCheck` `workerHost` does — per-task approval, kill switch,
standing scope at `full`, the classifier — plus `apps.allow`. There is no
weaker consent tier for app runs, because an app run does exactly what a shell
command does: edits files and runs commands on the user's computer.

**Per-user delegation policy** (`v1:worker:delegationPolicy`, edited at
`/machines` in the portal) decides *when*. An absent row means never delegate.
If no machine with an allowed, signed-in app is online, the task runs
in-process — a plan never waits for a laptop to wake up.

## 5. Audit + observability

## 5. The Fleet: labels, routing, and where a call lands

Epic memql#4349. Everything here is **per-user**: a machine belongs to exactly
one `v1:identity:user`, and only agents acting in that user's sessions can
dispatch to it. The operator surface is `/fleet/machines` in the MemQL Portal
(see [portal.md](portal.md)).

### 5.1 Two label fields, and why they are not one

A registration carries **two** key=value maps:

| Field | Written by | Survives a reconnect? |
|---|---|---|
| `labels` | the cockpit, from the `Register` message | **No** -- the whole map is overwritten every time |
| `operatorLabels` | the owner, from `/fleet/machines` (`setWorkerOperatorLabels`) | Yes -- no register or heartbeat path writes it |

The split is design D3, and it exists because `labels` is **replaced wholesale**
on every reconnect. An operator tag written into it would survive until the
laptop's lid next closed and then vanish -- the worst failure mode a routing
input can have, because the rule still reads correctly and the machine silently
stops matching it.

`refreshWorkerRegistration` enforces the split by **not naming**
`operatorLabels`. `update{}` is a read-merge, so a field the body does not name
survives untouched: the prohibition is carried by the ABSENCE of a line, and a
well-meaning "complete the field list" edit is what would break it.
`displayName` is absent for the same reason -- the owner's rename must outlive
the hostname the cockpit re-reports on every connect.

**Routing matches the MERGE, operator side winning** (`MergeLabels`,
`integrations/agent/worker/router.go`). The machine gets the last word on facts
about itself (`os`, `arch`, `hostname`, which it auto-populates); the owner gets
the last word on anything they deliberately set. `/fleet/machines` renders the
merge and marks which side each value came from, because a reported value an
owner has overridden is a fact about their configuration, not about the machine.

### 5.2 The routing policy

`v1:worker:routingPolicy` -- one active row per user, edited from the Routing
panel on `/fleet/machines`. **Absent is a valid state and the common one:** a
user who never opened the page has no row, and the router applies
`firstFit` + `nextMatching`, which is exactly what the pre-router
`Registry.PickWorker` did.

| Field | What it does |
|---|---|
| `strategy` | how the surviving candidates are ORDERED |
| `requireLabels` | a machine must carry these to be a candidate at all. AND-ed with the agent's own requirement; narrows, never widens |
| `preferLabels` | ordering hint only, never a filter |
| `fallback` | `nextMatching` (try the next candidate when one refuses BEFORE starting) or `none` (report the refusal) |

**The four strategies, and what each actually orders by.** Every sort is
STABLE over registration order (`sort "row.createdAt", "asc"` in
`workersForOwnerWithStatus`), so ties are deterministic and every replica
agrees -- two replicas ordering one fleet differently is the bug class this
epic closes.

| Strategy | Orders by | Why it is expressed that way |
|---|---|---|
| `firstFit` | registration order, unchanged | The pre-policy behaviour. The input already arrives in this order, so the sort is a no-op. |
| `roundRobin` | oldest `lastSelectedAt` first | A TIMESTAMP on the row, not a counter: two agent replicas reading the same row reach the same answer with no shared state. A machine never selected carries the zero time and sorts first, so a newly paired machine gets work before the rotation settles. |
| `leastLoaded` | `activeCount` against that capability's `concurrency` cap, ties broken on absolute count | A machine that declared no cap reads as UNLOADED -- it asked for no ceiling, so the ceiling is not what to ration it by. The absolute-count tie-break is what stops "least loaded" pinning everything to the first uncapped machine and never moving. |
| `labelMatch` | most `preferLabels` hits first | Then registration order, from the stable sort. |

`activeCount` is what the machine reported on its most recent heartbeat, so it
is up to one interval stale by construction. It is a routing input, never a
correctness one -- the machine's own `Acquire` is the real valve.

An unrecognised `strategy` falls back to `firstFit` with a WARNING rather than
failing the call, and an unreadable policy row applies the default with a
WARNING. Refusing a user's work over a preference is the wrong trade; the log
line is what stops that being invisible.

**A conflicting requirement is left UNSATISFIABLE.** Owner policy says
`os=darwin`, the agent's call requires `os=linux`: `unionLabels` writes a
sentinel neither side can match and the call reports `no_worker_available`.
Silently preferring one side would run the work somewhere the other excluded.

### 5.3 There is no `workerId` argument, by design

`agentworkerDispatchHost` / `agentworkerDispatchComputer` take `requireLabels`
and `preferLabels` and **nothing that names a machine** (design D4). An agent
expresses what the work NEEDS; the owner's policy decides where it lands. **A
hallucinated machine id is therefore a failure mode this surface does not
have** -- the worst a wrong label can do is empty the candidate set.

`no_worker_available` then names every machine and why it was ruled out
(`revoked` / `offline` / `missing capability <X>` / `labels do not satisfy
<k=v>`), together with the requirement it filtered on and how many machines the
account has at all. "No worker available" on its own is the least useful true
sentence available to somebody looking at a laptop they can see is on.

The same reasoning shapes the consent card (design D6): the user's **Allow**
covers the task on ANY of their machines matching the requirement, so the card
names the requirement AND today's choice -- "on any of your machines matching
`os=darwin` -- currently Jose's MacBook". A card naming one machine while
authorizing a set describes something other than what it does.

### 5.4 `online` is DERIVED, and these are the numbers

There is no `online` field, and the DSL deliberately does not project one: a
shape body is a path list, and this is a predicate over two timestamps and a
clock. A machine is online when **all three** hold
(`component/worker/online.go`, `IsOnline`):

- `revokedAt` is empty -- a revoked machine is never online whatever its
  heartbeat says, and one still beating while revoked is the case that most
  needs to read as gone;
- `lastSeenAt` is non-empty -- a registration never heard from is offline, not
  online-since-the-epoch;
- `now - lastSeenAt <= OnlineWindow`.

`HeartbeatBatchInterval = 15s` (`component/worker/worker.go`) and
`OnlineWindow = 2 * HeartbeatBatchInterval = 30s`. The flush WAS 60s, throttled
on the reasoning that per-beat writes bought freshness nobody read -- which was
circular: nothing read `lastSeenAt` BECAUSE it was up to a minute stale. 15s is
the cockpit's own beat, so the flush is one write per machine per beat and the
flag decays within 30s. At 60s a closed laptop read as online for two more
minutes.

Two intervals rather than one because one is the boundary itself: a flush a few
milliseconds late would flap a machine offline and back while nothing was wrong.
A `lastSeenAt` in the FUTURE (clock skew between the writing replica and whoever
is asking) counts as online -- deliberately, a skewed clock should not make a
live machine disappear.

**There are exactly two implementations, and a test holds them together.**
`component/worker.IsOnline` and `clients/portal/src/fleet/online.ts`;
`TestFleetOnlineWindowMatchesPortal` reads the TypeScript and fails the build
when its window disagrees with the Go one. Deriving `online` from the in-memory
registry instead is what this design refuses: the registry is ONE replica's
stream table, so it answers "connected to me" rather than "connected to any
replica".

### 5.5 Pairing a machine from the portal

`/fleet/machines` -> **Add a machine**. It mints a worker token over
`CreateWorkerTokenMsg` on the connection's own credential, shows the plain
`mql_wkr_...` value **once**, and renders the install one-liner for macOS or
Linux with the token and cluster URL filled in. Only the SHA-256 hash persists,
so a lost token is replaced rather than looked up -- there is no lookup, and the
value is deliberately never written to browser storage or a URL.

The panel reports success by watching the machine POPULATION grow (a
`v1:worker:registration` the cluster wrote), not by the mint succeeding: a
minted token proves nothing about the machine, whose install can fail or whose
cluster URL can be wrong. It counts rather than matching by name, because the
token's name is what the operator typed here and the registration's is the
cockpit's hostname, so the two are routinely different and a name match would
report failure on a success.

The portal cannot drive identity's `POST /pair/codes` flow: that endpoint
authenticates with `Authorization: Bearer <access token>` and the portal
deliberately has no way to read the token it holds. `memql worker pair`
stays the right shape for a machine that can redeem a short code interactively.

### 5.6 The cross-node forward (memql#4352)

A machine's `WorkerService` stream terminates on **exactly one** agent replica.
The turn that wants it is served wherever the mesh routed the request -- at the
default two replicas, a coin flip. On the losing side the machine was simply not
there: the in-memory registry is per-node and is never rehydrated from the rows,
so the turn reported `no_worker_available` for a laptop the user could see was
online. This is the event/session bug class CLAUDE.md's multi-node section names
(#1448, #1412, #1388) arriving on the worker surface, and it gets the same fix
the workbench already had.

**`connectedNodeId` is what makes a machine reachable from a replica that is not
holding it.** The registration carries the `MEMQL_NODE_ID` of the replica that
holds the stream, stamped on register and on every heartbeat flush and CLEARED
on disconnect (`clearWorkerConnectedNode`). `lastSeenAt` is deliberately NOT
touched on the way out -- moving it would make a disconnected machine look fresh
for one whole online window. **Empty means no replica holds the stream**, i.e.
offline.

The dispatcher compares it with its own `MEMQL_NODE_ID`. Same, or self unset
(single-node): dispatch locally. Different: forward a `WorkerForwardRequest` to
that replica over `NodeService.Stream` and route the answer back by
`request_id` (`WorkerForwardResponse`), with `WorkerForwardStream` relaying
stdout / stderr / data chunks so streamed output crosses the hop, and
`WorkerForwardCancel` stopping in-flight work when the turn upstream is
cancelled. With no forward wired, a candidate held by another replica is
**skipped** with a logged reason and `worker_unreachable`, never run here --
running it locally would fail in a way that blames the machine.

**`refused_before_start` is the re-pick predicate**, and it is the one field on
the wire that must never be guessed. True means the receiver is CERTAIN nothing
executed -- it held no stream for the registration, or the machine was at its
concurrency cap -- so the sender may try the next candidate under
`fallback=nextMatching`. False means the dispatch reached the machine and the
sender must **not** re-pick even on failure: an exec that lost its stream
mid-run may have run, and running it elsewhere is a second side effect rather
than a retry (design D5).

**What the receiver re-checks, and what it deliberately does not.** The consent
gates -- per-task approval, the kill switch, standing scope, the classifier --
ran on the SENDER before its pick and are not re-run; answering one question in
two places is how the two answers drift. What only the receiver can establish is
that the registration is owned by the principal this envelope ASSERTS -- taken
from the verified `ForwardedAuthority`, never from the envelope's
`owner_user_id`, which is a hint used only to read the fleet -- and that it has
not been revoked in the window since the sender read the row.

Unlike every other forward there is **no enable flag**.
`MEMQL_WORKBENCH_REMOTE` exists because running a workbench call locally is a
legitimate alternative; here there is none, because this replica does not hold
the stream, so local dispatch is not a degraded path but a call that cannot
work.

INFO: the hop is gated by an **in-process** test
(`integrations/agent/worker/forward_hop_test.go`), which wires the real
`ForwardRouter` to the real `ForwardHandler` through a link carrying the same
envelopes `NodeService.Stream` carries. There is deliberately no
`test/clustere2e` lane for it: a live-cluster gate is skipped on every CI lane
and every developer machine, and a gate that is skipped by default cannot be the
thing standing between this feature and the bug it prevents.

### 5.7 Reading the routing record

Every `v1:worker:invocation` carries a `routing` object saying WHY the call
landed where it did. It is rendered per machine in the activity list on
`/fleet/machines`, and readable directly off the row:

| Key | Meaning |
|---|---|
| `policyId` | the `v1:worker:routingPolicy` row that decided. Empty when the owner has none and the default applied |
| `strategy` | `firstFit` / `roundRobin` / `leastLoaded` / `labelMatch` |
| `candidatesConsidered` | registration ids that survived the filter, IN THE ORDER the router would try them |
| `rejected` | per machine, why it was NOT a candidate. Present even -- especially -- when the candidate list is empty |
| `attempts` | 1 unless `fallback=nextMatching` moved past a refusal |
| `selectedBy` | `policy` / `reroute` / `only_candidate` |
| `reroutedFrom` | `workbench` (the workbench answered `environment_mismatch`) or `worker:<registrationId>` (a candidate refused before starting) |
| `requireLabels` / `preferLabels` | the MERGED agent+policy requirement the candidates were filtered and ordered by |

An **empty** `routing` object means "not recorded", never "chose nothing": rows
written before the router existed carry none, and so does a path denied before
anything was chosen. The `outcome` enum gained `rerouted` for a call that did
not run on the machine first chosen.

Superseded policy rows are deactivated rather than deleted, precisely because
`routing.policyId` points at whichever row made the choice.

Two reads back the activity list: `invocationsForWorker` (self-scoped, the
caller's own machines) and `invocationsForWorkerAsOperator` (cluster-owner).
The pair exists because `v1:worker:invocation` declares no row tier, so the
caller scope has to live in the FILTER and one filter cannot be both. **The
portal currently calls the self-scoped one only**, so a cluster owner inspecting
somebody else's machine sees an empty activity list on a machine it can
otherwise fully describe.

---

## 6. Audit + observability

| Where           | What lands                                         |
|-----------------|----------------------------------------------------|
| `v1:identity:auditEvent` | Security signals: `worker_registered`, `worker_revoked`, `scope_elevation_*`, `kill_switch_*`, `worker_call_denied_*`. Default 365-day retention (`MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS`). |
| `v1:worker:invocation` | Per-call telemetry: tool, action, args (redacted), duration, outcome, exit code, byte counts, output preview, plus the `routing` record saying why this machine (section 5.7). Default 90-day retention (`WORKER_INVOCATION_RETENTION_DAYS`). |
| Cockpit logs    | `~/.memql/state/worker.log` (LaunchAgent / systemd). |
| Slog stream     | The `audit` slog logger on the agent node. Operator log retention applies here. |

Worker actions audit as `actor=worker:<id>`, NOT the registering
user — the worker is its own principal for forensic blast-radius
clarity. The registering user is reachable via the
`v1:identity:identity.credentials.worker_token.registeredBy` field.

---

## 7. Common operations

### Revoke a worker

UI: `/fleet/machines` in the portal -> Revoke, per machine. The owner can
also rename it (`displayName`) and edit its `operatorLabels` from the same
card; see section 5.

SDK / VS Code extension -- run the mutation (the `memql` command does **not**
run mutations; it never carried a runner and the TUI that did is gone):

```memql fragment
revokeWorker(
  registrationId: "wkr-abc...",
  revokedAt: "2026-05-05T12:00:00Z",
  revokedBy: "user-jose-...",
  revokeReason: "decommissioned"
)
```

The agent node's registry checks `revokedAt` on every dispatch and
on a periodic sweep — a revoked worker's stream is closed out-of-
band so any in-flight calls fail with `worker_disconnected`.

### Disable computer-use for yourself (kill switch)

UI: Floating shield widget in any space's chrome.

CLI:

```memql fragment
toggleComputerUseEnabled(enabled: false)
```

The switch always targets the **caller's own** user row -- the id is
stamped from the auth context rather than passed in (memql#2840). There
is deliberately no operator path to flip it for someone else, and passing
a `userId` would be worse than an error: an undeclared argument is
dropped silently on the gRPC path, so the command would appear to work
while toggling your own switch. Walk the user through the shield widget
instead. If an administrative override is genuinely needed it belongs in
a separately-named mutation.

### Inspect invocations for a plan

```memql fragment
invocationsForPlan(planId: "plan-...")
```

Per machine rather than per plan, with the routing record rendered, use the
activity list on `/fleet/machines` (section 5.7).

### Force a token rotation

The worker emits a `RotationRequest` 7 days before
`worker_token.expiresAt`. Operators can also force one by
restarting the worker — the next reconnect refreshes
`lastSeenAt` and the next scheduled rotation fires from there.

---

## 8. Failure modes and remedies

| Symptom                                  | Diagnosis                                           | Remedy                                                  |
|------------------------------------------|-----------------------------------------------------|---------------------------------------------------------|
| Machine shows "offline" on `/fleet/machines` | gRPC stream lost, so `lastSeenAt` has aged past the 30s window | Check `worker.log`; `launchctl list` / `systemctl --user status` |
| `denied_by_policy: shell allow list: <cmd>` | Cmd not on policy allowlist                      | Add to `~/.memql/policy.yaml` shell.allow + SIGHUP      |
| `denied_by_scope`                        | Action exceeds the agent's standing or plan scope    | Either approve elevation on the plan card OR widen the agentAuthorization row |
| `kill_switch_engaged`                    | User flipped `computerUseEnabled` to false          | Re-enable from the floating widget; resume plans       |
| `computeruse_unavailable`                        | Worker is the headless build                         | Reinstall with `--computeruse`                          |
| `unsupported_on_platform`                | `window_list` / `window_focus` on a platform without WindowServer hooks | Use macOS or X11 Linux; tracked as known gap below |
| `no_worker_available`, and the message names each machine | Every machine was filtered out. The message says which and why: `revoked` / `offline` / `missing capability <X>` / `labels do not satisfy <k=v>` | Read the reason. `offline` at a machine you can see is on means `lastSeenAt` is older than 30s -- check the stream. `labels do not satisfy` means the merged agent+policy requirement excluded it (section 5.2) |
| `worker_unreachable`                     | The machine is held by another agent replica (`connectedNodeId`) and this node has no forward wired | Wire the mesh. A single-replica cluster never hits this; in a mesh, both halves of `WorkerForward*` are wired on every agent replica (section 5.6) |
| `worker_disconnected` on a forwarded call | The dispatch reached the far replica and the answer was lost. NOT re-picked, by design -- the call may have run | Check the far replica's logs for the call id before re-running anything with side effects |
| Process killed mid-exec on Linux         | `RLIMIT_AS` (memory) or `RLIMIT_CPU` cap reached     | Bump `policy.shell.max_memory_mb` / `max_cpu_seconds`   |

Note: registration rows persisted by pre-#1334 builds stay stale
(`lastSeenAt` frozen at register time) until the worker's next
reconnect; the in-memory registry is always fresh, and current builds
flush `lastSeenAt` once per **15s** heartbeat (memql#4350 -- it was 60s,
and `online` now derives from that value, so the flush cadence is the
flag's freshness budget; see section 5.4).

---

## 9. Worker observability

The cockpit-worker exposes a Prometheus text-format metrics
endpoint on `127.0.0.1:9100/metrics`:

- `worker_uptime_seconds` (gauge)
- `worker_calls_total` (counter)
- `worker_calls_by_outcome_total{outcome="..."}` (counter)
- `worker_call_duration_ms` (histogram, 50ms..60s buckets + Inf)
- `worker_reconnects_total` (counter)

Loopback-only by design — the worker is the user's machine and
the metrics endpoint is unauthenticated. Operators scrape from the
same box (`curl http://127.0.0.1:9100/metrics`) or via a
node-exporter-style sidecar that already runs on the host. Disable
with `--metrics-port 0` if the port collides.

`/healthz` returns `200 OK` for liveness probes.

---

## 10. Phase status

All seven phases shipped:

- [x] Phase 1 — Concepts + WorkerService gRPC foundation
- [x] Phase 2 — Cockpit `worker` subcommand
- [x] Phase 3 — Headless tool dispatch + policy engine
- [x] Phase 4 — computer-use build variant + RobotGo-backed `workerComputer.*`
- [x] Phase 5 — Frontend integration (WorkersListPanel + AddWorkerModal + kill-switch widget)
- [x] Phase 6 — Install scripts + service templates
- [x] Phase 7 — Hardening:
  - Drain + `RotationRequest` envelope
  - Server-side worker-token mint
    (`CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg` on
    `MemqlService.Stream`; the AddWorkerModal calls these directly,
    so the plain token never lives outside the gRPC reply).
  - macOS TCC + Linux X11 pre-flight wizard
    (`memql worker setup`, on the computer-use build).
  - Prometheus metrics on `127.0.0.1:9100/metrics`.
  - Per-call rlimits (`RLIMIT_CPU`, `RLIMIT_AS`, `RLIMIT_NOFILE`)
    and optional setuid drop via
    `policy.shell.{run_as_user,max_cpu_seconds,max_memory_mb,max_open_files}`.

## 11. Known polish gaps (out of initial ship)

- `window_list` / `window_focus` need platform-specific
  WindowServer / X11 wiring on top of RobotGo. They return
  `unsupported_on_platform` for now.
- macOS lacks a portable `RLIMIT_AS` equivalent; hard memory caps
  on darwin should ride launchd's `HardResourceLimits` stanza in
  the LaunchAgent rather than the per-call rlimit path.
- Red-team verification list (deny every operation explicitly,
  confirm both audit + invocation rows land correctly).
