---
title: "Visual QA: the Build stop's readings"
audience: internal
status: stable
area: ops
sinceVersion: 0.9.37
owner: znas
---

# Visual QA: the Build stop's readings

- **Date:** 2026-09-03
- **Epic:** memql#4900 -- task memql#4905
- **Method:** a temporary Vite QA harness (`clients/os/qa.html` + `qa/main.tsx`
  + `vite.qa.config.ts`) mounting the REAL `DeployablesApp` over the REAL fake
  connection the vitest suite uses, driven in headless Chrome at 1240 wide in
  both themes. Deleted after the sweep.

jsdom performs no layout, resolves no custom properties, and never puts a
value beside its own label. Every defect below was found in a browser with the
suite green, and two of them are pure copy -- which is the part people assume
a test covers.

## Why the harness mounts the whole app

Three attempts at the module swap failed in ways worth recording, because the
next person will reach for the same shapes:

1. **Aliasing on the specifier tail.** `src/live/connection` is imported as
   `"../../../live/connection"` from the deployables tree and as
   `"./connection"` from inside `src/live`. Any tail match catches one and
   misses the other, and missing one means the app holds a SECOND, empty
   instance of the seam -- so the page renders "Not connected to the cluster"
   while every module under test holds the fake. **Decide the swap on the
   RESOLVED path**: call `this.resolve(id, importer, {skipSelf: true})` and
   compare the file.
2. **Bounding the swap by importer.** Narrowing it to importers under
   `apps/deployables` looks tidier and is wrong for the same reason: the live
   list reads its connection through `src/live/useLiveCollection.ts`, outside
   that tree. One seam, one instance, and the shim RE-EXPORTS the real module
   so unrelated importers still get the names they need.
3. **Forgetting the role ladder.** `src/system/roles.ts` starts with an EMPTY
   ladder that the shell fills from the cluster's `v1:rbac:role` rows, so a
   harness that skips `setRoleLadder(SEEDED_LADDER)` renders the app entirely
   READ-ONLY. That is correct product behaviour -- an unrankable role must not
   unlock a gated surface -- and a silent thing to debug: no write control
   appears anywhere and nothing says why. `test/setup.ts` does this for the
   suite; a harness has to do it too.

## What was checked

| Reading | Fixture | Result |
|---|---|---|
| A build that ran | `builtOn: {workbench, workbench-2}`, a log tail | Build stop shows `Plan`, `Output`, `Built / in this cluster's sandbox`, `On / workbench-2`, then the log in its own disclosure |
| A failed build | `deployable_build_failed` with a tail | rail `stopped here` at Build; the OS headline above the server's sentence; the same build facts; Publish never reached |
| A lost run | `status: abandoned`, `stoppedAt: building` | rail `stopped here` at **Build**, not Analyze; timeline word **lost**; **Retry** on the attempt; the Head reads Redeploy |
| A run nobody clicked | `automatic: true` | the `automatic` chip in the attempt header, before the controls; no Retry, which only `abandoned` gets |
| The auto-deploy switch | `autoDeploy` on and off | on the Source stop between the credential picker and Archive, with its promise beneath it in both states |
| Both themes | every scene above | legible in each; the dark palette resolves through `data-theme="dark"` |

## Findings, and what happened to each

### 1. `Built  built in this cluster's sandbox` -- FIXED

`buildSurfaceLabel` returned a value beginning with the verb its own label
supplies. The test asserted the value contains "sandbox", which passed either
way. Each value now COMPLETES the label: `in this cluster's sandbox`, `on your
own machine`, `before it got here -- the output was in the source`.

### 2. The abandoned copy contradicted its own button -- FIXED

`deployment_abandoned` ended "Retry starts a fresh run." The Retry beside it
does the opposite: it deploys the bytes THAT run had already fetched, which is
the whole point of the snapshot. Now: "Retry runs it again from the same
source it had already fetched." The test asserted the copy contains "Retry",
which passed either way.

### 3. The rail's note repeats the server's sentence -- NO CHANGE, NOT MINE

A stopped stop renders the run's own sentence as the rail note AND inside the
problem notice below it, so it appears twice. This is Compose's rail contract
(the note is what a collapsed rail shows) and it behaves identically for
`refused` and `failed`, which predate this epic. Recorded rather than changed:
altering it is a change to the rail's design, not to this epic's readings.

### 4. Fonts fall back under the harness -- HARNESS ONLY

`brand/fonts/*.woff2` sit outside Vite's serving allow list from
`clients/os`, so the harness renders in fallback faces. Layout and colour
were the subjects here; anything about type metrics needs `npm run dev` from
the repo root instead.
