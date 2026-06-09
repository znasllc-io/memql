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

The current MVP runs the workbench in-process on the agent node;
the cluster-mode + Cloud Run deployment is documented separately
in [production.md](../../internal/ops/workbench-production.md) and is deferred until you start
deploying to production.

## 1. Mental model

Three execution surfaces, in preference order:

1. **In-server MemQL tools** -- exhaust first when the work fits.
2. **Workbench** (this doc) -- default for any headless task that
   needs a shell or filesystem. Linux, sandboxed, per-Plan.
3. **Computer-use** -- the user's actual machine. Reserved for
   tasks the workbench cannot do: macOS-only tooling (Xcode,
   AppleScript), GUI control / screenshots / mouse + keyboard,
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

The agent's prompt template (`dsl/copresent/prompts/agentReply.tmpl`)
and the `workbench` knowledge domain (5 chunks in
`integrations/knowledge/seed.go`) instruct the agent to:

- Reach for the workbench FIRST for any headless task.
- Reach for computer-use ONLY when the workbench cannot do the
  job (macOS-only tools, GUI control, user-local files).
- Surface a "workbench can't do this -- needs computer use"
  message via `respondToUser` when it hits a Linux/macOS or
  sandbox/host limitation rather than silently retrying.

The planner can grant `computer_use_*` slugs per-Task when the
goal text indicates they're needed -- see the
`agentFactoryAnalyze` prompt rules.

## 6. Testing the MVP locally

```bash
docker compose -f docker/docker-compose.full.yml up --build
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
4. Verify the workspace inside the memql container:

   ```bash
   docker compose exec memql ls /var/lib/memql/workbenches/
   docker compose exec memql cat /var/lib/memql/workbenches/<planId>/birds.md
   ```

## 7. Teardown

When the parent Plan reaches a terminal status (succeeded /
failed / cancelled), the `releaseWorkspaceOnPlanTerminal`
automation fires:

1. The `releaseWorkspace` mutation flips the v1:workbench:workspace
   row to status=`released` (cluster mode -- the MVP doesn't write
   this row yet).
2. The `workbenchTeardownDirectory` builtin calls the integration's
   `teardownDirectory` capability which `rm -rf`s the per-Plan
   directory.

Idempotent: a Plan that never provisioned a workspace is a no-op.

## 8. Configuration

| Env var                    | Default                            | Effect |
|----------------------------|------------------------------------|--------|
| `MEMQL_WORKBENCH_ROOT`     | `/var/lib/memql/workbenches`       | Root directory for per-Plan workspaces. Override for dev (project-local path) or Docker volume mounts. |
| `MEMQL_WORKBENCH_REMOTE`   | unset (false)                      | When truthy AND a `ForwardRouter` is wired, the agent's dispatch delegates to a remote workbench node via NodeService.Stream. See [production.md](../../internal/ops/workbench-production.md). Leave unset for the MVP path. |

## 9. Files of interest

| Path                                                   | Purpose |
|--------------------------------------------------------|---------|
| `component/memql/operator_caps.go`                     | Slug expansion (`workbench_use` -> `workbenchHost` + `canvasPublish`) |
| `dsl/workbench/`                                       | Concept + mutations + queries + shapes + automation + logic + builtins |
| `dsl/copresent/tools.memql`                            | `tool workbenchHost { ... }` definition |
| `integrations/workbench/`                              | Go integration: Manager, dispatch handlers, forward router/handler |
| `integrations/knowledge/seed.go`                       | `workbench` knowledge domain + seed corpus |
| `dsl/copresent/prompts/agentReply.tmpl`                | `{{if .workbenchAvailable}}` capability block |
| `integrations/agent/replier.go`                        | `workbenchAvailable` data injection + domain auto-attach |
| `dsl/agents/roles/*.memql`                             | `workbench_use` in every role's `lockedToolSlugs` |
| `dsl/agents/prompts/agentFactoryAnalyze.tmpl`          | Factory rules for granting workbench / computer-use |

## 10. Workbench -> Computer-use fallback (verified path)

The "workbench first, computer-use fallback" ordering is real and the loop is
**closed**, but it is **agent-driven and user-gated** -- NOT an automatic
planner re-route. Verified path (memql#790):

1. The agent prefers the workbench (`workbench_use` is universal). Guidance:
   the `workbench:preferOverComputerUse` + `workbench:failureFallback` corpus
   chunks in `integrations/knowledge/seed.go`.
2. When the workbench genuinely can't do a job (macOS/Xcode, a GUI app, or a
   file already on the user's machine), the agent does NOT silently switch and
   does NOT dead-end. If it holds a computer-use slug it calls
   `requestComputerUseScope({intent, requestedScope, summary})`, naming the
   workbench limitation, and ends its turn with a short `respondToUser`.
3. That mints a scope-elevation Plan; the user sees an approval card on the
   canvas. On **Allow**, `handlePlanApprovedForExecution`
   (`integrations/planner/plan_execution.go`) dispatches a fresh turn back to
   the agent with `planApprovedTrigger=true`, where it runs the work on the
   user's machine via `workerHost` / `workerComputer`.
4. If the agent has no computer-use slug, it names the limitation and tells the
   user that enabling Computer Use would unblock it, so the user can grant the
   capability.

There is **no** planner "saw a workbench failure -> auto-granted computer-use
-> retried" path: the planner agent loop's task-completion re-invocation is
deferred (see the `HandlePlanUpdated` comment in
`integrations/planner/agent_loop.go`). The consent-gated escalation above is
the intended fallback and keeps the user in control of anything that touches
their machine. memql#790 hardened the `workbench:failureFallback` guidance so
the agent reliably escalates via `requestComputerUseScope` instead of relying
on a planner re-route that does not fire.
