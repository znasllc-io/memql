---
title: Workbench Runbook
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Workbench Runbook

Operational guide for the workbench capability -- the sandboxed
per-Plan Linux working environment that is the default first
choice for any HEADLESS work an agent needs to do (writing files,
running shell commands, fetching URLs).

Cluster mode is the deployed shape: a dedicated `workbench` node-type binary
hosts the workspaces and agent nodes route to it over `NodeService.Stream`. The
in-process path on the agent node survives as the local/no-mesh fallback (see
section 8). Operational detail lives in
[workbench-production.md](../../internal/ops/workbench-production.md).

## 1. Mental model

Three execution surfaces, in preference order:

1. **In-server MemQL tools** -- exhaust first when the work fits.
2. **Workbench** (this doc) -- default for any headless task that
   needs a shell or filesystem. Linux, sandboxed, per-Plan.
3. **Computer-use** -- the user's actual machine. Reserved for
   tasks the workbench cannot do: macOS-only tooling (Xcode,
   AppleScript), computer-use control / screenshots / mouse + keyboard,
   files already on the user's computer.

Computer-use has two slugs (`computer_use_headless` and
`computer_use_embodied`); the workbench has one (`workbench_use`),
universal across every role.

## 2. Lifetime model

Two distinct lifetimes inside the workbench:

- **Per-Task container (ephemeral compute).** A fresh process /
  namespace runs for each Task. Today the "container" is the agent
  node's own process; in cluster mode it will be a per-Task
  goroutine on the workbench node.
- **Per-Plan workspace (persistent filesystem).** One directory
  tree per Plan, mounted into every container that runs under it.
  Outlasts individual Tasks. Released when the parent Plan reaches
  a terminal status (succeeded / failed / cancelled).

Workspace root: `MEMQL_WORKBENCH_ROOT` env var, default
`/var/lib/memql/workbenches/`. Each Plan gets a subdirectory keyed
by `planId`.

## 3. Tool surface

One tool, `workbenchHost`, discriminated by `action`:

| Action       | Args                                                    | What it does |
|--------------|----------------------------------------------------------|--------------|
| `exec`       | `{cmd, cwd?, env?, stdin?, timeoutSec?}`                 | Run shell via `/bin/sh -c` inside the workspace. Default 60 s, max 600 s. Stdout + stderr each capped at 1 MiB. |
| `fs_read`    | `{path, maxBytes?}`                                      | Read file as text. Default + max 1 MiB. |
| `fs_write`   | `{path, content, mode?}`                                 | Write file; parent dirs auto-created. Max 16 MiB. |
| `fs_list`    | `{path}`                                                 | Non-recursive directory listing. Capped at 1000 entries. |
| `fs_stat`    | `{path}`                                                 | Size / mode / mtime / isDir / exists. Non-existent is exists=false, not an error. |
| `http_fetch` | `{url, method?, headers?, body?, timeoutSec?}`           | HTTP request from the workbench. Body capped at 5 MiB. |

All paths are RELATIVE to the workspace root; absolute paths and
`..` traversal are rejected.

The dispatch builtin behind the tool (`workbenchDispatchHost`) also takes an
optional `environment` hint alongside `action` / `args` / `planId` / `agentId` /
`taskId`. Section 10 is what it does.

### 3.1 The build entry, which is not a tool

`workbenchHost` is one of TWO entries. The other is the package build (epic
memql#4900): the deploy pipeline's, reached from Go and from nowhere else.

|  | `workbenchHost` | the build entry |
|---|---|---|
| Reached by | an agent's tool loop, through `workbenchDispatchHost` | `component/packages`, through `workbench.Integration.RunBuild` |
| A DSL construct? | yes, `tool workbenchHost` | **no**, deliberately -- there is nothing for a model to name |
| Keyed on | a Plan | a **deployment**, plus the deployable's name |
| Workspace row | `v1:workbench:workspace`, released on the Plan's terminal status | **none** -- the directory lives for one call and is torn down whatever happens |
| The command | allowlisted binaries only (section 4.1) | the manifest's own, whatever it is |
| The environment | the node's, plus what the call passes | **constructed**: PATH, a HOME inside the directory, the locale, `CI=true`, and nothing of the node's |
| Runs as | the engine's user | uid `MEMQL_PACKAGES_BUILD_UID` (10001), so `/proc/1/environ` is unreadable |
| On the wire | `WorkbenchForwardRequest`, the caller's own assertion re-asserted | the same message with `action: "build"` under a **system-class** assertion |

**The class is the gate.** Only this cluster's engine can mint a system-class
`ForwardedAuthority` (`auth.ForwardedAuthorityForSystem`, which refuses a user
subject); every tool-loop forward re-asserts the caller's own class. So the
workbench refuses `build` under anything else, and `handleDispatchHost` refuses
the action by name before it forwards -- a model naming it never leaves the
agent node.

**The image is different too.** `workbench-runtime` in the `Dockerfile` is the
only engine image carrying a Node toolchain and `git`, plus the build uid.
Building the workbench node with any other stage produces a node whose builds
fail with `npm: not found` on a cluster that looks correctly configured.

Note that **no CI lane builds this image**: `build-engine-images.yml` is
`workflow_dispatch`, so a mistake in the stage surfaces at an image build rather
than on a pull request. It was verified by hand at the time it landed --
`node --version`, `npm --version` and `git --version` inside the stage, and the
uid claim measured rather than assumed: with a secret in the container's
environment, root reads it out of `/proc/1/environ` and uid 10001 gets
`Permission denied`. Re-check the same way after touching the stage.

## 4. Authorization

Universal -- `workbench_use` is injected into every role's
`lockedToolSlugs` (see `dsl/agents/roles/*.memql`) so every agent
has it. No scope grants, no kill switch, no per-agent gating. The
blast radius is contained to the per-Plan directory tree.

### 4.1 Exec allowlist

`workbenchHost(action="exec")` runs commands via `/bin/sh -c`, so
a compromised agent (prompt injection, jailbroken base model)
could otherwise spawn arbitrary subprocesses. The dispatcher
enforces a **curated binary allowlist** (memql#110) before the
shell ever sees the string:

- Allowed: standard file inspection / mutation / text processing
  / archives / hashing / `curl` + `wget` for fetch / language
  toolchains (`python3`, `node`, `go`, `git`, etc.) / `jq` + `yq`.
  Full list in `integrations/workbench/exec_allowlist.go`.
- Rejected: `sudo`, `bash`, `sh`, `nc`, `ssh`, `iptables`, and
  every other binary not on the list. Pipelines are
  tokenized -- a single disallowed binary in any segment rejects
  the whole command with `command_not_allowed`.
- Path-bearing binaries (`/usr/bin/python3`, `./helper.sh`) match
  against their basename so PATH-independence is preserved.

**Known limitation:** subshell substitution (`echo $(curl ...)`)
isn't parsed; only the outer command's binary is checked. The
inner `curl` rides through to `/bin/sh` unchecked. This is a
documented gap with Option A; the architectural fix (Option B:
seccomp / AppArmor profile) is a follow-up tracked under #110.

Extending the allowlist: file a follow-up to memql#110 with the
binary name + the use case. Don't bypass the check by routing the
call through `bash -c` (the bash entry is itself off the list to
prevent this).

## 5. Routing preference

The agent's reply prompt template (`agentReply.tmpl`, shipped in the
product pack's prompts tree) and the `workbench` knowledge domain (5
chunks in `integrations/knowledge/seed.go`) instruct the agent to:

- Reach for the workbench FIRST for any headless task.
- Reach for computer-use ONLY when the workbench cannot do the
  job (macOS-only tools, computer-use control, user-local files).
- Surface a "workbench can't do this -- needs computer use"
  message via `respondToUser` when it hits a Linux/macOS or
  sandbox/host limitation rather than silently retrying.
- Declare an `environment` hint on calls that need something a workbench is
  not, so the refusal is typed and arrives before anything runs (section 10).

The planner can grant `computer_use_*` slugs per-Task when the
goal text indicates they're needed -- see the
`agentFactoryAnalyze` prompt rules.

## 6. Testing it locally

```bash
make up
```

Then:

1. Create an agent (or pick an existing one). All newly-created
   agents include `workbench_use` automatically; legacy agents
   need the slug added to their `capabilities.tools` once.
2. Open a Plan-anchored chat and ask the agent to do something
   file-y or shell-y. Example: "Write a markdown file listing the
   ten most beautiful birds on earth and save it as `birds.md`."
3. The agent calls `workbenchHost` with `action=fs_write` (and
   probably `action=exec` for any research it does).
4. Verify the workspace inside the **workbench** pod. `deploy/k8s/base`
   sets `MEMQL_WORKBENCH_REMOTE=1` +
   `MEMQL_WORKER_PEERS=workbench=workbench:50060` on the agent, so the
   directory is on a workbench replica rather than on the agent:

   ```bash
   kubectl exec -n memql deploy/workbench -- ls /var/lib/memql/workbenches/
   kubectl exec -n memql deploy/workbench -- cat /var/lib/memql/workbenches/<planId>/birds.md
   ```

   `deploy/k8s/base/workbench.yaml` runs **2 replicas**, so name the pod
   rather than the Deployment when you want a specific one -- and read
   `workspace.nodeId` to know which one holds the plan's tree (section 11).

## 7. Teardown

When the parent Plan reaches a terminal status (succeeded /
failed / cancelled), the `releaseWorkspaceOnPlanTerminal`
automation fires:

1. The `releaseWorkspace` mutation flips the `v1:workbench:workspace`
   row to `status=released`. Since memql#4354 that row is really
   written -- the concept was declared and written by nothing before
   then (section 11).
2. The `workbenchTeardownDirectory` builtin calls the integration's
   `teardownDirectory` capability which `rm -rf`s the per-Plan
   directory.

Idempotent: a Plan that never provisioned a workspace is a no-op.

## 8. Configuration

| Env var                    | Default                            | Effect |
|----------------------------|------------------------------------|--------|
| `MEMQL_WORKBENCH_ROOT`     | `/var/lib/memql/workbenches`       | Root directory for per-Plan workspaces. Override for dev (project-local path) or Docker volume mounts. |
| `MEMQL_WORKBENCH_REMOTE`   | unset (false)                      | When truthy, the agent's dispatch delegates to a remote workbench node via NodeService.Stream. **This is an assertion, not a preference** -- see below. See [production.md](../../internal/ops/workbench-production.md). Leave unset for the MVP path. |
| `MEMQL_WORKBENCH_LOCAL_FALLBACK` | unset (false)                | Opt-in escape valve: in remote mode, run the call on the agent node when no workbench peer is reachable, instead of refusing. Off by default. |

### Remote mode refuses rather than degrades (memql#3506)

`MEMQL_WORKBENCH_REMOTE=1` says **this work does not run on the agent**.
So when no healthy workbench peer is reachable, a workbench call now
**fails** with `no_workbench_peer` and a message naming the missing peer,
`MEMQL_WORKER_PEERS`, and this escape valve. It does not fall back to the
agent's own disk.

It used to. That fallback is why memql#3450 was invisible for its entire
life: the shipped `deploy/k8s/base/agent.yaml` sets both the remote flag
*and* the peer seed, the seed was being dropped at parse time, so the
router had no peer and **every** workbench tool call ran on the agent pod
-- no error, no warning, correct-looking results. The operator asked for
isolation and got none, and nothing anywhere said so.

Degrading to local execution does not honour the assertion, it inverts
it, and does so most readily in the case that matters: the workbench being
unavailable. A sandbox that silently becomes not-a-sandbox is worth less
than an error.

If you *do* want local execution when the workbench is unreachable -- a
reasonable thing to want in development -- set
`MEMQL_WORKBENCH_LOCAL_FALLBACK=1`. The point is that "run this remotely"
and "run it here if you must" are now spelled differently, so the second
cannot happen because nobody configured anything.

**Diagnosing a refusal.** `no_workbench_peer` means one of: no workbench
node is running; `MEMQL_WORKER_PEERS` does not name it (`MEMQL_WORKER_PEERS=workbench=workbench:50060`);
the workbench node is not reporting healthy. The agent also logs at ERROR
at boot if the remote flag is set but the router could not be wired at all
-- in that state every workbench call on that node refuses.

## 9. Files of interest

| Path                                                   | Purpose |
|--------------------------------------------------------|---------|
| `component/memql/worker_caps.go`                     | Slug expansion (`workbench_use` -> `workbenchHost` + `canvasPublish`) |
| `dsl/workbench/`                                       | Concept + mutations + queries + shapes + automation + logic + builtins |
| the product pack's `tools.memql`                       | `tool workbenchHost { ... }` definition (pack-owned) |
| `integrations/workbench/`                              | Go integration: Manager, dispatch handlers, forward router/handler |
| `integrations/workbench/build.go`                      | The package-build entry (epic memql#4900): the request, the constructed environment, the bounded tail, the teardown |
| `integrations/workbench/build_user_unix.go`            | The uid drop, and what it does and does not cover |
| `component/packages/build_workbench.go`                | The pipeline's side: the two translations, and the refusal mapping |
| `integrations/workbench/environment.go`                | The `environment` hint: the closed `needs` set, the typed mismatch, the two error codes |
| `integrations/workbench/workspace_store.go`            | The Go writer for `v1:workbench:workspace` -- provision / touch / release, all under the plan owner's actor |
| `integrations/agent/workbench_reroute_agent.go`        | The reroute: mismatch -> the fleet, or the consent card |
| `integrations/agent/worker/scope.go`                   | `needs` -> scope tier and -> routing labels |
| `integrations/knowledge/seed.go`                       | `workbench` knowledge domain + seed corpus |
| the product pack's `agentReply.tmpl`                   | `{{if .workbenchAvailable}}` capability block (pack-owned) |
| `integrations/agent/replier.go`                        | `workbenchAvailable` data injection + domain auto-attach |
| `dsl/agents/roles/*.memql`                             | `workbench_use` in every role's `lockedToolSlugs` |
| `dsl/agents/prompts/agentFactoryAnalyze.tmpl`          | Factory rules for granting workbench / computer-use |

## 10. The environment hint, and the reroute to the fleet

Shipped in memql#4353 (epic memql#4349). It does not weaken the rule below it:
the workbench stays first, and the fleet is where it cannot go.

### 10.1 The hint

`workbenchDispatchHost` takes an **optional** `environment` object -- the agent
saying what the action needs:

```json
{ "os": "darwin", "needs": ["macos_tooling"] }
```

- `os` is a GOOS string, compared against the evaluating node's own `runtime.GOOS`.
- `needs` is a **closed four-value set**: `display`, `gpu`, `macos_tooling`,
  `user_files`. They name exactly the four things a workbench is not -- a
  headless Linux sandbox in the cluster with an empty directory tree -- so
  declaring any of them is by construction a mismatch. It is written as a table
  (`workbenchProvides`, `integrations/workbench/environment.go`) rather than as
  `len(needs) > 0`, so the day a GPU-bearing workbench flavour exists one
  `false` becomes `true` and nothing else moves.

**Omitting `environment` means "no hint", and there is deliberately no
default.** Every caller predating the field, and every action that genuinely
does not care, passes nothing. A guessed default would fire the mismatch on
calls that would have worked.

### 10.2 The typed mismatch

`handleDispatchHost` evaluates the hint **before anything runs** -- above the
safety classifier, above workspace provisioning, on both the local and the
forwarded path. On a mismatch it returns `errorCode: environment_mismatch` with
a structured body on `dispatchResult.payload`:

| Field | Meaning |
|---|---|
| `unmetNeeds` | every reason the workbench cannot serve this call, drawn from the four need values plus `os`. Never empty in a mismatch |
| `requestedOs` | the hint's `os`, when supplied |
| `workbenchOs` | the GOOS of the node that evaluated the hint |

`os` is an **output-only** reason: it appears in `unmetNeeds` when the hint's
`os` names a platform this node is not, and it is NOT accepted as an input
`needs` value. It sits in the same list rather than beside it so a consumer
reading only `unmetNeeds` is never handed an empty list on a genuine mismatch.

**The action did not run.** No workspace was provisioned, no command executed,
nothing fetched. What this replaces is a failure that arrived three layers down
and named nothing -- a `defaults read` on Linux, an xdotool with no `DISPLAY`, a
path under `/Users` that is simply not there -- which reads to the model as "the
command is wrong", so it retries with variations.

Consumers read the payload through
`workbench.EnvironmentMismatchFromPayload`, never by regexing the message: an
error string a consumer has to parse is a contract that breaks the first time
somebody improves the wording.

### 10.3 An unknown need is a CALLER error, and never a reroute trigger

A malformed hint -- a non-object `environment`, a non-list `needs`, a
non-string element, or a need value outside the closed set -- returns
`errorCode: invalid_environment_hint`. **A separate code, deliberately.**

A mismatch is a fact about the workbench and the tool loop may act on it; an
invalid hint is the caller getting the contract wrong, and the only useful
response is to fix the call. Folding the two together would let a typo
(`macos-tooling`) read as "the workbench cannot do this" -- **a reroute to
somebody's laptop on the strength of a hyphen**. A test pins the split.

Silently dropping the unknown value would be worse still: the action would then
run having been told it needs something nobody checked, which is the exact
failure the hint exists to remove.

### 10.4 The reroute, and exactly when the card is raised instead

The workbench knowledge domain's ruling stands and is quoted in the Go that
implements the reroute (`integrations/agent/workbench_reroute_agent.go`):

> Never silently switch to the user's own machine. If the workbench cannot do
> the job, say so and request computer-use scope through
> `requestComputerUseScope` -- the user approves on the canvas card before any
> tool touches their machine.

What the automatic path removes is not the consent. It is the second **asking**
for consent already given.

On `environment_mismatch` the agent tool loop re-dispatches **the same call** --
same action, same inner arguments -- to `workerHost`, and **the dispatcher's
existing gate decides**. The gates run entirely before any wire traffic, so an
attempt that is refused touches nothing.

| The fleet dispatch answers | What the loop does |
|---|---|
| `denied_no_per_task_approval` | raises the consent card (`requestComputerUseScope`) and tells the model to end its turn |
| `denied_by_scope` | same |
| anything else -- including success, `kill_switch_engaged`, `no_worker_available`, an ordinary failure | that IS the answer; it is returned to the model as-is |

**A kill switch is not a missing card.** `kill_switch_engaged` means the user
deliberately turned computer use off, so it is surfaced rather than answered
with a card asking them to turn it back on. Answering a deliberate "no" with a
consent prompt is nagging, not consent.

**Asking the gate rather than re-deriving its ladder is the point.** A second
copy of "does this user's standing scope cover this" in the tool loop would
drift in the direction that reads as safe -- a loop asking for `observe` where
the ladder says `full` produces a card the user approves for less than what then
runs.

**Needs map to a scope and to routing labels**
(`integrations/agent/worker/scope.go`, beside the ladder it reads, importing the
need vocabulary from `integrations/workbench` rather than restating it):

| Unmet needs | Scope requested | Labels required of the machine |
|---|---|---|
| `user_files` alone | `observe` -- the workbench could not see the file and the machine is being asked to look at it | none. "The files are on the user's machine" is true of every machine they own |
| `display` | `full` | `display=true` |
| `gpu` | `full` | `gpu=true` |
| `macos_tooling` | `full` | `os=darwin` -- a need for macOS tooling IS a need for macOS, stated as the os label so a machine the cockpit already tagged `os=darwin` matches without hand-tagging |
| `os` (the hint named a different platform) | `full` | `os=<the requested goos>` |
| empty or unrecognised | `full` | -- |

An unrecognised set takes `full`, the conservative direction, for the same
reason: an unknown need asking for the narrower tier is how a card gets approved
for less than what runs.

**Both tool loops call it** -- streaming and non-streaming. One behaviour with
two transports; a divergence would present as "it works in chat but not in a
plan".

The routing record on the resulting `v1:worker:invocation` carries
`reroutedFrom: "workbench"`, which is what makes "why did this run on the
laptop" answerable after the fact (see the
[workers runbook](workers-runbook.md), section 5.7).

### 10.5 What has NOT changed

There is still **no** planner "saw a workbench failure -> auto-granted
computer-use -> retried" path, and the reroute is not one: it grants nothing.
Consent is either already held -- an approved task and standing scope at or
above the tier the unmet needs imply -- or the card goes up. If the agent holds
no computer-use slug at all, it names the limitation and tells the user that
enabling Computer Use would unblock it.

The user-gated escalation described in memql#790 is unchanged and is still what
runs for every workbench failure that is not an `environment_mismatch`: the
agent calls `requestComputerUseScope({intent, requestedScope, summary})`, ends
its turn with a short `respondToUser`, and on **Allow**
`handlePlanApprovedForExecution` (`integrations/planner/plan_execution.go`)
dispatches a fresh turn with `planApprovedTrigger=true`.

---

## 11. Replica affinity: a workspace lives on ONE replica

`deploy/k8s/base/workbench.yaml` runs **2 replicas**, and a workspace is a
**filesystem**. A filesystem does not follow the request, so which replica
serves a call is not load balancing -- it decides whether the plan's files are
there.

Until memql#4354 the agent's peer picker was **any-fit**. A plan's first call
made a directory on one replica; its second call landed on the other with even
odds and found an empty tree. Both calls reported `ok=true`, neither result
named a node, and the failure read as the agent having imagined the write. The
`v1:workbench:workspace` concept was declared in the DSL the whole time and
**written by nothing**, so there was no record to disagree with.

**How it works now.** The node that creates the directory writes the row and
stamps its own `MEMQL_NODE_ID` on `workspace.nodeId`. Before forwarding, the
agent reads the plan's live workspace row and passes that node id to the peer
picker, which **prefers that replica whenever it is healthy and connected** and
falls back to any-fit only when it is not. The fallback is still untuned for
load: with the pinned replica gone there is no better information available.

An affinity read that FAILS degrades to an unpinned pick with a WARNING rather
than refusing the call -- that is the pre-#4354 behaviour, so a transient read
problem cannot be worse than the status quo, and the receiving node still
records the substitution.

### 11.1 When a replica is lost, files are NOT migrated

A workbench replica leaving the mesh takes its directory tree with it. There is
nothing to copy them from. The design **accepts a fresh empty directory and
records why**:

1. The pinned replica is gone, so the picker returns a healthy substitute and
   the agent logs a WARNING naming **both** node ids and the plan -- the only
   vantage point that can see both at once.
2. The substitute creates the directory, finds a live row naming a different
   node, and flips that row to `status=released`,
   `releasedReason=node_lost`.
3. It inserts a successor row naming itself, at an id derived from
   `(planId, nodeId)` -- one row cannot be both released and provisioned.
4. The plan continues on an **empty** workspace, and the serving node logs the
   takeover too.

The re-provision happens **exactly once**: subsequent calls find a live row
naming the serving node and adopt it. A row naming NOBODY (written before
`nodeId` existed) is also adopted rather than replaced.

INFO: the operator answer to "where did my file go" is a released workspace row
carrying `releasedReason=node_lost`. `/fleet/workbenches` renders it with that
reason spelled out; without the row there is no record that anything moved.

[ ] Not implemented: **no notice reaches the user's canvas.** The log line and
    the row state ship; the card does not. `canvasState` is a pack-only
    construct the engine core does not load, and there is no canvas path
    reachable from `integrations/workbench` -- a node-loss card has to arrive
    the way the others do, from a product-bundle automation firing off the
    `node_lost` release. Full detail in
    [workbench-production.md](../../internal/ops/workbench-production.md),
    section 1a.

### 11.2 A plan whose owner cannot be resolved is REFUSED

`v1:workbench:workspace` now declares
`@rowAuthz(owner="ownerUserId", clusterOwner)`, and `ownerUserId` is stamped
from the parent plan's `requestedBy` at provision time -- never from a caller
argument.

WARNING: a workbench call whose `planId` does not resolve to a readable
`v1:planner:plan` row is now **refused** with
`errorCode: workspace_owner_unresolved` rather than run. This is a behaviour
change. Writing the row anyway would stamp `ownerUserId: ""`, and the row tier
then hides it from the person whose files it describes AND from the operator;
the next call would read no row and provision a second workspace, bringing the
split back wearing a bookkeeping layer. A workspace keyed on a plan that does
not exist also never reaches the `releaseWorkspaceOnPlanTerminal` automation, so
its directory is never reclaimed.

The same tier is why every workspace read and write runs under
`auth.ContextWithUserActor` for that owner: the read gate has no internal-origin
bypass, so an unactored read returns **zero rows and no error**, which is
indistinguishable from "this plan has no workspace".

### 11.3 Where to look

`/fleet/workbenches` in the portal lists the workbench replicas and the
workspaces living on each, live and released, with the release reason spelled
out. A cluster owner can widen it to every workspace in the cluster. See
[memql-os.md](memql-os.md).
