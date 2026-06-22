# Platformization Program — Master Plan

Four-epic program to turn MemQL into a clean, plug-and-play platform and land
telephony on top of it. Built from the analysis in
[`05-current-state-map.md`](05-current-state-map.md).

**Epics**
1. [AI → AI rename](01-epic-si-to-ai-rename.md)
2. [Platform / plugin architecture](02-epic-platform-plugin.md)
3. [Decouple CoPresent from core](03-epic-decouple-copresent.md)
4. [Telephony into core](04-epic-telephony.md)

---

## Guiding decisions (locked)

- **AI → AI rename runs first**, as one coordinated sweep **including the
  wire/proto names** (regenerate `.pb.go` + frontend TS, roll all nodes + SPA
  together). Rationale: rename while everything is in one place, so the
  decoupling never chases renamed symbols.
- **`partition` is the canonical tenant/scope primitive** — it already exists
  (`v1:platform:partitionSecret`/`partitionVariable`,
  `ResolvePartitionFromContext`). We do **not** invent a new partition concept;
  we adopt the existing one. `space` is a CoPresent concept scoped *by* a
  partition.
- **Services stay core; packs are product.** cognition, voice, voice-agent,
  planner, agent are core services (the `mcp` node already builds engine-only
  with no CoPresent DSL — proof the split works). A "client" is a plugin repo:
  build-tag-gated Go (`RegisterPlugin`) + embedded `.memql` tree
  (`RegisterTree`) + routing rules.
- **Telephony attaches to a generic partition/room, never to a CoPresent
  `space`.**

---

## Dependency graph & gates

```
Epic 1 (rename)  ──► G1 ──►  Epic 2 (plugin)  ──► G2 ──►  Epic 3 (decouple)  ──► G3 ──►  Epic 4 (telephony)
                         └─►  Epic 3 PREP (inventory, pure-concept moves) ──────┘
```

**Gates**
- **G1 — rename landed everywhere.** Epic 1 complete across all repos; all
  builds + tests green. Unblocks Epic 2 (full) and Epic 3 (prep only).
- **G2 — partition foundation landed.** Epic 2 issues 2.1 (contract) + 2.2
  (partition adoption) merged. Unblocks Epic 3's core re-pointing.
- **G3 — core decoupled.** Epic 3 complete: core builds engine-only with zero
  CoPresent references; CoPresent pack loads in a CoPresent cluster. Unblocks
  Epic 4.

---

## Parallel session plan

Each epic is a separate working session. Within an epic, issues tagged
`[P]` can run in parallel; issues tagged `[G:x]` wait on gate/issue `x`.

| Session | Epic | Starts when | Can overlap with |
|---|---|---|---|
| **S1** | 1 — rename | now | (runs mostly solo to avoid merge churn) |
| **S2** | 2 — plugin | **G1** | S3-prep |
| **S3** | 3 — decouple | **G1** for prep; **G2** for core re-pointing | S2 |
| **S4** | 4 — telephony | **G3** | — |

Maximum concurrency: after **G1**, S2 (plugin) and S3-prep (inventory + moving
pure-CoPresent concepts that don't touch partition) run **together**. S3's
heavy `spaceId → partitionId` re-pointing waits on **G2** from S2. S4 waits on
**G3**.

---

## Repo ownership of the work

| Epic | Primary repo(s) |
|---|---|
| 1 — rename | `memql` (core), `memql-bff-copresent`, `memql-cockpit`, `copresent` (frontend gen) |
| 2 — plugin | `memql` |
| 3 — decouple | `memql` (extract from), `memql-bff-copresent` (move into) |
| 4 — telephony | `memql` |

---

## Verification posture (every epic)

Each epic ends with a verification issue: full build + test across affected
repos, plus the epic-specific proof (rename: zero stray AI identifiers outside
the denylist; plugin: example pack loads; decouple: engine-only core build is
green; telephony: a real inbound + outbound call). High-risk epics (1 wire
rename, 3 decouple) should run the verification via a fresh sub-session/agent.
