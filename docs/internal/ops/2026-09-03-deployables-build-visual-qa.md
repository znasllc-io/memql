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
  + `vite.qa.config.ts`) mounting the real `PackageDetail` over a stubbed feed,
  driven in headless Chrome at 1180x1400 in both themes. Deleted after the
  sweep.

jsdom performs no layout, resolves no custom properties, and asserts nothing
about which SECTION a row appears under. The 1297 green vitest cases were
therefore silent about both defects below.

## What was checked

| Reading | Fixture | Result |
|---|---|---|
| A running build | `status: building`, no `builtOn` yet | rail at Build with the moving mark, under "Deploying now"; no build facts, because nothing has built |
| A failed build | `deployable_build_failed` with a tail | rail stopped at Build; the OS headline above the server's sentence; `Built / in this cluster's sandbox`, `On / workbench-2`; the tail in its own disclosure |
| A build past its timeout | `deployable_build_timeout` | the same shape, with the copy naming `MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS` |
| An abandoned run | `status: abandoned`, `stoppedAt: building` | "lost" as the status word, **Retry** as the action, and the sentence "Nothing was published and nothing failed" |
| The auto-deploy switch | `autoDeploy` on and off | the checkbox with its promise beneath it, in both states |
| Both themes | every scene above | legible in each; the dark palette resolves through `data-theme="dark"` |

## Findings, and what happened to each

### 1. A lost run rendered under "Deploying now" -- FIXED

`PackageDetail`'s own terminal set was `{succeeded, refused, failed}`, so a run
the sweep had closed still matched "not terminal" and appeared under the
heading **Deploying now** -- with a rail that had stopped. That is precisely
the sentence the `abandoned` status exists to stop the surface saying.

It is invisible to jsdom because the DOM is identical either way: what was
wrong was which heading the run appeared beneath, and no assertion named one.

`abandoned` is in the set now, with a comment recording that a browser is what
caught it.

### 2. The rail understated how far a lost run got -- FIXED, in the engine

A terminal run's furthest stage is read from its leftovers: deployables mean
publishing ran, a staged version means it rolled, a build log means it built, a
report means it analyzed. A run that died mid-build has **none of those**, so
the rail drew it as having stopped at **Analyze** -- understating what it
achieved and sending somebody to look in the wrong place.

The cause is that closing the row is what destroys the fact: `status` held
`building`, and the sweep overwrites it with `abandoned`.

Fixed at the source rather than guessed at in the client: the sweep now keeps
the stage in `stoppedAt` before it writes, and the rail prefers it. Runs closed
before this field existed carry an empty one and fall back to the evidence
rule, which has its own test.

### 3. A contrast defect that was the harness, not the product -- NO CHANGE

In the first dark-mode pass every `Facts` VALUE was nearly illegible while its
label was fine. The harness was painting `body { color: var(--os-text, #191d1a) }`
inline -- and the token is `--os-ink`, so the fallback put the light theme's
near-black ink on the dark ground, inline, where it beat the stylesheet.

Recorded because the failure was convincing: it looked exactly like a token
that had not been given a dark value, and "fixing" it would have put a
hardcoded colour into a themed surface. The harness was corrected to set
nothing and let `index.css` paint.

## The one thing this sweep could not do

The Chrome extension was not connected in this environment, so the pass ran
through headless Chrome screenshots rather than a live driven session. That
covers layout, theme resolution and copy at real size; it does not cover
interaction (hover states, the disclosure opening, focus rings under keyboard
navigation). Those remain unmeasured for these readings.
