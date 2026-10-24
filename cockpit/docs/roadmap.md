# Cockpit revival — the roadmap after the rename

**Status:** A shipped 2026-08-25. B–E are unstarted and each needs its own
design session first.

This is where the remaining sub-projects live now. They were tracked as
`memql#4549` and briefly as four issues under it; both are closed, because an
issue that represents undesigned work says less than a document that carries
the constraints somebody will need on the day they start.

**File an issue when you START a sub-project**, not before — with its design
record, and with sub-issues, the way A was run.

---

## A — slim + rename. DONE.

The TUI is removed outright (unused; removed, not deprecated) and the installed
command is `memql`. One installed command, two build variants (headless /
computeruse). The product name stays MemQL Cockpit.

Design: [`docs/superpowers/specs/2026-08-25-cockpit-slim-rename-design.md`](superpowers/specs/2026-08-25-cockpit-slim-rename-design.md).
Shipped across memql-cockpit#345–#351 and memql#4550–#4556, plus the engine-side
doc sweep in memql#4559 / memql#4561.

**The wire contract was untouched**, as planned: old and new binaries are
wire-identical, and no engine deploy was needed.

---

## B — connection security and recovery hardening

End-to-end audit and upgrade of the worker path: token lifecycle, TLS posture,
reconnect / backoff / resume, revocation latency, chaos-style disconnect tests.

**Two things landed after this was scoped, and B should start from them rather
than re-derive them:**

- **The engine has a live-data continuity contract now** (memql#4535). Every
  notification on a `MemqlService.Stream` carries `seq`, a dropped delivery
  surfaces as `gap_before` on the next one, and the answer to either is to
  re-seed. The Go SDK gained opt-in auto-reconnect: exponential backoff with
  full jitter, subscription replay on the new stream with original ids, and a
  `Final()` that distinguishes a recovered drop from the end of the connection.
  **The worker path got none of it** — `WorkerService.Stream` is a different
  service — so "what does the worker stream do when it drops" is an open
  question with a worked answer next door in `sdk/go/client`.
- **`online` is a 30-second derivation** (`component/worker.IsOnline`), two
  heartbeat flushes. Revocation latency has to be reasoned about against that
  window, not against the heartbeat alone.

---

## C — fleet management polish

Machines UX, routing strategies, failover semantics, OS-targeted dispatch,
verified end to end against the production portal.

**Settled already — do not re-open these in the design:**

- The **Fleet router** (`integrations/agent/worker/router.go`, memql#4349) and
  its four strategies, all stable sorts over REGISTRATION order so every
  replica agrees. Policy and agent requirements are AND-ed; a conflict is left
  unsatisfiable rather than resolved toward either side.
- **Two label maps that must not become one.** `labels` is overwritten from
  every `Register`; `operatorLabels` is never touched by a register or
  heartbeat path. An operator tag placed in the wrong one is erased roughly
  whenever the lid closes.
- **Labels match EXACTLY.** There is no "any value" form, and requiring an
  empty value matches nothing — which reads on screen as "machine offline".

**New ground to build on:** `/fleet/machines` in the portal is store-backed
since memql#4539 and reports `live | degraded | disconnected` honestly, so a
UX change there has a real liveness signal rather than a hardcoded chip.

---

## D — distribution

Per-OS installers (macOS DMG, Linux packages) plus a Fleet download section in
the portal with OS auto-detect.

Builds on A rather than replacing it: memql#4553 / memql#4554 already migrate
the old `memql-cockpit` LaunchAgent, keep credentials working, and renamed the
release artifacts and CI in lockstep with `install.sh`.

**D and E are coupled at the artifact** — E's menu bar app is bundled in D's
DMG — so D decides the bundle's shape.

---

## E — macOS menu bar app

The MemQL status item with cockpit controls, bundled in D's DMG, managing the
LaunchAgent.

Two constraints to design against rather than discover:

- **It ships inside D's DMG**, so it cannot land before D has decided the
  bundle's shape.
- **It manages the LaunchAgent A's installer already migrates.** There is
  exactly one agent, and two things writing it is the failure mode to design
  out.
