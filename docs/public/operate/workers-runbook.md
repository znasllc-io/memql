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
`memql-cockpit worker run`. It connects to a memQL cluster via
`WorkerService.Stream` (gRPC bidi), advertises its capabilities
(HEADLESS, optionally GUI), and accepts dispatched tool calls
(`workerHost.*`, `workerComputer.*`) from agents acting in
sessions owned by the same user.

Per-user routing means there is no shared pool — agents only ever
see workers owned by the user whose session they're acting in.

---

## 2. Install

Pick the installer for your OS. Both ship under `scripts/install/`.

### macOS

```bash
curl -fsSL https://app.copresent.ai/admin/workers/install/install-mac.sh | \
  bash -s -- \
    --token mql_wkr_xxxxxxxxxxxx \
    --cluster https://app.copresent.ai \
    --gui
```

The install script:

1. Downloads the appropriate binary
   (`memql-cockpit-darwin-arm64` or `memql-cockpit-gui-darwin-arm64`).
2. Writes `~/.memql/worker.yaml` with the token + cluster URL.
3. Drops a LaunchAgent at
   `~/Library/LaunchAgents/com.znasllc.memql-cockpit-worker.plist`
   and `launchctl load`s it.

The first time you run the GUI variant, macOS will prompt for
**Accessibility** and **Screen Recording** permissions (System
Settings → Privacy & Security). Approve both, then re-run the
service:

```bash
launchctl unload ~/Library/LaunchAgents/com.znasllc.memql-cockpit-worker.plist
launchctl load   ~/Library/LaunchAgents/com.znasllc.memql-cockpit-worker.plist
```

For an interactive walkthrough that probes both permissions,
opens System Settings to the right pane, and verifies per-binary
TCC status, run:

```bash
./bin/memql-cockpit-gui worker setup
```

The setup flow is a single-panel TUI built on the same
`cli/ui` + `cli/canvas` primitives as the operations console
(see [cli/CLAUDE.md](../../cli/CLAUDE.md) — every interactive
surface in `memql-cockpit` uses the TUI). Detects non-TTY
callers and falls back to a plain printf path so install
scripts piping the wizard's output don't break.

### Linux

```bash
curl -fsSL https://app.copresent.ai/admin/workers/install/install-linux.sh | \
  bash -s -- \
    --token mql_wkr_xxxxxxxxxxxx \
    --cluster https://app.copresent.ai \
    --gui
```

The install script writes a user-systemd unit at
`~/.config/systemd/user/memql-cockpit-worker.service` and starts
it. On Wayland the worker registers HEADLESS only; X11 sessions
get GUI as well.

---

## 3. Configure

`~/.memql/worker.yaml`:

```yaml
cluster_url: https://app.copresent.ai
token: mql_wkr_<your token>
name: jose-mac-mini
labels:
  os: darwin
  arch: arm64
  has-blender: true   # operator-defined for label-based routing
concurrency:
  HEADLESS: 8
  GUI: 1
state_dir: ~/.memql/state
log_level: info
capabilities:
  - HEADLESS
  - GUI
```

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
kill -HUP $(pgrep memql-cockpit)
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
   Floating widget in CoPresent's space chrome flips it. When
   false, every dispatch is rejected with `kill_switch_engaged`
   and the `killSwitchSuspendsRunningPlans` automation
   transitions running plans to `awaitingFeedback`.

A single denial transitions the calling plan to
`awaitingFeedback` with `feedbackReason=scope_elevation_required`.
The user approves or denies on the canvas card.

---

## 5. Audit + observability

| Where           | What lands                                         |
|-----------------|----------------------------------------------------|
| `v1:identity:auditEvent` | Security signals: `worker_registered`, `worker_revoked`, `scope_elevation_*`, `kill_switch_*`, `worker_call_denied_*`. Default 365-day retention (`IDENTITY_AUDIT_LOG_RETENTION_DAYS`). |
| `v1:worker:invocation` | Per-call telemetry: tool, action, args (redacted), duration, outcome, exit code, byte counts, output preview. Default 90-day retention (`WORKER_INVOCATION_RETENTION_DAYS`). |
| Cockpit logs    | `~/.memql/state/worker.log` (LaunchAgent / systemd). |
| Slog stream     | The `audit` slog logger on the agent node. Operator log retention applies here. |

Worker actions audit as `actor=worker:<id>`, NOT the registering
user — the worker is its own principal for forensic blast-radius
clarity. The registering user is reachable via the
`v1:identity:identity.credentials.worker_token.registeredBy` field.

---

## 6. Common operations

### Revoke a worker

UI: Workers panel (`?panel=workers`) → Revoke per row.

CLI: `memql-cockpit` → connect → run mutation:

```memql
mutationRevokeWorker({
  registrationId: "wkr-abc...",
  revokedAt: "2026-05-05T12:00:00Z",
  revokedBy: "user-jose-...",
  revokeReason: "decommissioned"
})
```

The agent node's registry checks `revokedAt` on every dispatch and
on a periodic sweep — a revoked worker's stream is closed out-of-
band so any in-flight calls fail with `worker_disconnected`.

### Disable computer-use for a user (kill switch)

UI: Floating shield widget in any space's chrome.

CLI:

```memql
mutationToggleComputerUseEnabled({
  userId: "user-jose-...",
  enabled: false
})
```

### Inspect invocations for a plan

```memql
queryInvocationsForPlan({ planId: "plan-..." })
```

### Force a token rotation

The worker emits a `RotationRequest` 7 days before
`worker_token.expiresAt`. Operators can also force one by
restarting the worker — the next reconnect refreshes
`lastSeenAt` and the next scheduled rotation fires from there.

---

## 7. Failure modes and remedies

| Symptom                                  | Diagnosis                                           | Remedy                                                  |
|------------------------------------------|-----------------------------------------------------|---------------------------------------------------------|
| Worker shows "offline" in /workers       | gRPC stream lost                                    | Check `worker.log`; `launchctl list` / `systemctl --user status` |
| `denied_by_policy: shell allow list: <cmd>` | Cmd not on policy allowlist                      | Add to `~/.memql/policy.yaml` shell.allow + SIGHUP      |
| `denied_by_scope`                        | Action exceeds the agent's standing or plan scope    | Either approve elevation on the plan card OR widen the agentAuthorization row |
| `kill_switch_engaged`                    | User flipped `computerUseEnabled` to false          | Re-enable from the floating widget; resume plans       |
| `gui_unavailable`                        | Worker is the headless build                         | Reinstall with `--gui`                                  |
| `unsupported_on_platform`                | `window_list` / `window_focus` on a platform without WindowServer hooks | Use macOS or X11 Linux; tracked as known gap below |
| Process killed mid-exec on Linux         | `RLIMIT_AS` (memory) or `RLIMIT_CPU` cap reached     | Bump `policy.shell.max_memory_mb` / `max_cpu_seconds`   |

---

## 8. Worker observability

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

## 9. Phase status

All seven phases shipped:

- [x] Phase 1 — Concepts + WorkerService gRPC foundation
- [x] Phase 2 — Cockpit `worker` subcommand
- [x] Phase 3 — Headless tool dispatch + policy engine
- [x] Phase 4 — GUI build variant + RobotGo-backed `workerComputer.*`
- [x] Phase 5 — Copresent integration (WorkersListPanel + AddWorkerModal + kill-switch widget)
- [x] Phase 6 — Install scripts + service templates
- [x] Phase 7 — Hardening:
  - Drain + `RotationRequest` envelope
  - Server-side worker-token mint
    (`CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg` on
    `MemqlService.Stream`; the AddWorkerModal calls these directly,
    so the plain token never lives outside the gRPC reply).
  - macOS TCC + Linux X11 pre-flight wizard
    (`memql-cockpit-gui worker setup`).
  - Prometheus metrics on `127.0.0.1:9100/metrics`.
  - Per-call rlimits (`RLIMIT_CPU`, `RLIMIT_AS`, `RLIMIT_NOFILE`)
    and optional setuid drop via
    `policy.shell.{run_as_user,max_cpu_seconds,max_memory_mb,max_open_files}`.

## 10. Known polish gaps (out of initial ship)

- `window_list` / `window_focus` need platform-specific
  WindowServer / X11 wiring on top of RobotGo. They return
  `unsupported_on_platform` for now.
- macOS lacks a portable `RLIMIT_AS` equivalent; hard memory caps
  on darwin should ride launchd's `HardResourceLimits` stanza in
  the LaunchAgent rather than the per-call rlimit path.
- Red-team verification list (deny every operation explicitly,
  confirm both audit + invocation rows land correctly).
