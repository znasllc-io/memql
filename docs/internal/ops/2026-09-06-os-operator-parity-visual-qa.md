---
title: "Visual QA: the operator surfaces the portal was the only home for"
audience: internal
status: stable
area: ops
sinceVersion: 0.20.28
owner: znas
---

# Visual QA: the operator surfaces the portal was the only home for

- **Date:** 2026-09-06
- **Epic:** memql#5009 (memql#5010 Concepts, memql#5011 Cluster, memql#5012
  Stores, memql#5013 the in-place gaps)
- **Method:** a temporary Vite QA harness (`clients/os/qa.html` + `qa/` +
  `vite.qa.config.ts`) mounting the REAL apps over the REAL fakes the vitest
  suites use, driven in headless Chrome at 1600x1100 in both modes. Deleted
  after the sweep, as its two predecessors were.

DESIGN.md's closing note makes rendered screenshots the acceptance for any
surface change under its rules. This sweep earned that: **it found a defect
2,344 passing tests did not, and the fix needed a different test from the one
the defect suggested.**

## The three traps, and a fourth

The three recorded by the previous sweeps all fired again, which is the
argument for writing them down a third time.

1. **The module swap is decided on the RESOLVED path.** A specifier alias
   cannot separate `src/live/connection` from `chrome/connection`. The plugin
   resolves with `skipSelf` and compares absolute paths.
2. **`setRoleLadder(SEEDED_LADDER)`.** The ladder starts empty and the shell
   fills it from `v1:rbac:role`; a harness that skips it renders every gated
   surface hidden. Correct product behaviour, and silent to debug.
3. **The harness must not import real vitest.** The shared fixtures build
   their fakes with `vi.fn`, and importing vitest outside a test run throws
   "Vitest failed to access its internal state".

The fourth is recorded in the memory notes and cost the most time here, so it
is repeated:

4. **The stubbed `useOsConnection` must return a MODULE-LEVEL SINGLETON.**
   Returning `qaConnection()` per render gives every `useMemo` keyed on the
   connection a new identity, so effects re-run forever. **The page then hangs
   with no console output and the capture returns zero bytes** -- which reads
   as a browser or extension problem rather than as the stub. One page in this
   sweep was diagnosed as "chrome cannot render this route" for several
   minutes before the singleton fixed it.

Two more, new to this sweep:

5. **Concurrent headless Chrome invocations block on the default profile
   lock.** A batch of captures produced exactly one file and then hung with 34
   live chrome processes. Each capture needs its own `--user-data-dir`.
6. **A wrong FIXTURE reads as a broken SURFACE.** The Stores list rendered its
   empty state over two seeded stores, because this harness answered
   `shopifyStoreHealth` with a bundle envelope and a builtin answers ONE
   id-keyed map of nodes. The suite's own harness had it right. The fixtures
   are meant to be the same description of the rows the tests assert against;
   where they diverge, the browser is judging something nothing else does.

## The defect the browser found

**A concept's rows were rendered twice.** Three fixture rows appeared as six,
under a footer reporting "All 6 readable rows loaded".

`useConceptRows` restarted the walk from an effect keyed on
`[conceptId, pageSize]`, which also ran on MOUNT. Under StrictMode the restart
effect and the walk effect both ran, the restart bumped `attempt`, and the
walk ran a second time and APPENDED its page to the first one's.

A confident wrong total is the worst shape that surface can take: the whole
point of its four footer states is that "that is all of them" can be trusted.

### The first test written for it was worthless, and the negative control said so

The obvious test renders under `<StrictMode>` and asserts two `.os-rows-row`
elements. **It passed with the fix reverted.** jsdom's effect timing lets the
duplicate page land in a way the browser's does not, so the SYMPTOM is not
reproducible here.

What is reproducible is the MECHANISM: one mount of one concept is one browse.
The test counts `executeNamed` calls named `conceptBrowse` and asserts exactly
one, and reverting the guard fails it. **Running the negative control is what
turned a test that pinned nothing into one that catches this.**

## Scenes checked

| Scene | Reading |
|---|---|
| Concepts -- registry | Grouped by domain, search behind `Refine` with no standing domain rail (rule 2), and a badge only where a mirror or origin declaration earns one -- native concepts carry none, which is most of the registry |
| Concepts -- one concept | One `Head`, two columns with their own scrollers (rule 11). The join reads: `retiredAt` marked `not seen`, `lens` and `sneakedIn` marked `undeclared`, and under them "1 declared field is not carried by any of the 3 rows loaded. 2 keys are carried by rows and declared by nothing" |
| Cluster -- Readiness | Two calm lines for a healthy cluster and the meta "nothing here blocks anything". No wall of green ticks, and the closing sentence says nothing here stops you using the product |
| Cluster -- Modules | Fixed kind order (packs, integrations, node types, components), "answered by bff-7c9f4 (bff)", and a credential-gated integration explaining itself in words |
| **Cluster -- Data origins** | **The through-line, visible.** The never-run connector draws `—` for lag, drift, outbox and dead; the healthy one draws `0`. The caption states the difference. Rule 12 holds: the paused row offers Resume and only Resume, the running rows offer Pause and only Pause, and no act is ever drawn disabled |
| Cluster -- Audit trail | Owner-floored, so an admin never reaches the empty list that would read as "nothing happened" |
| Stores -- list | Status, missing scopes, and the pinned-vs-mirror version mismatch in the danger tone. The never-synced store reads "drift —", not "drift 0" |
| Stores -- empty | "No store is configured. Create the custom-distribution app in the Shopify Dev Dashboard, seal its three credentials as secrets, then add the store above" -- an invitation to act rather than a blank |
| Both modes | Every surface resolves in the light set; no hardcoded dark value leaked, and the error tone survives |

## What was NOT changed

The Data origins table renders lag as raw seconds (`4210 s` beside `12 s`).
`formatDuration` exists in the kit and "70m" is more actionable in isolation,
but this is a COLUMN whose job is comparison, and tabular seconds compare at a
glance where mixed units do not. Left as it is, noted here so the next sweep
does not read it as an oversight.
