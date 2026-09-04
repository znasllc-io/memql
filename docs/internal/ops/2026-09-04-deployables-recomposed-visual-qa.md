---
title: "Visual QA: the Deployables recomposition"
audience: internal
status: stable
area: ops
sinceVersion: 0.9.38
owner: znas
---

# Visual QA: the Deployables recomposition

- **Date:** 2026-09-04
- **Epic:** memql#4937
- **Method:** a temporary Vite QA harness (`clients/os/qa.html` + `qa/main.tsx`
  + `vite.qa.config.ts`) mounting the REAL `DeployablesApp` over the REAL fake
  connection the vitest suite uses, driven in headless Chrome at 1600x1100 in
  both themes. Deleted after the sweep, as its predecessor was.

DESIGN.md's closing note makes rendered screenshots the acceptance for any
surface change under its rules, and this epic's whole case is a set of
measurements jsdom cannot take: it performs no layout, resolves no custom
properties, and never puts a value beside its own label. **Every figure below
is a `getBoundingClientRect` offset or an element count taken in a browser.**

## What the harness had to get right

Both traps were already recorded from the Build epic's sweep, and both fired
again anyway -- which is the argument for writing them down a second time.

1. **The module swap is decided on the RESOLVED path.** `src/live/connection`
   is spelled five ways across the tree: `../../live/connection` (30 files),
   `../../../live/connection` (10), `../live/connection` (6),
   `../../../../live/connection` (3), and `./connection` from the three files
   inside `src/live` itself. A specifier alias cannot cover the last without
   also catching `chrome/connection`, which is a DIFFERENT module. The first
   cut used a tail match, missed one spelling, and the app rendered **"Not
   connected to the cluster"** while every component under test held the fake.
   The plugin resolves with `skipSelf` and compares the absolute path.
2. **`setRoleLadder(SEEDED_LADDER)`.** The ladder starts EMPTY and the shell
   fills it from `v1:rbac:role`; a harness that skips it renders the app
   entirely read-only. Correct product behaviour, and silent to debug.

A third, new one: **the harness must not import real vitest.** The shared
fixtures build their fake with `vi.fn`, and importing vitest outside a test run
throws "Vitest failed to access its internal state". A four-line stub of `vi.fn`
lets the browser and the suite drive the SAME fixtures -- a harness with
fixtures of its own would be a second description of the rows, free to disagree
with the one the tests assert.

## The measurements

Taken on one published deployable, before and after, at the same width:

| | before | after |
|---|---:|---:|
| One scroll column | **5,069 px / 5.9 viewports** | **741 px / 1.00** |
| `os-head` elements in it | 2 | **1** |
| Rails drawn | 13 | **1** |
| Controls reading "Retry" | 6 (two different promises) | **0** in content, 1 on the bar |
| "Archive" controls | 2, 1,614 px apart | **0** in content |
| Pause / Resume in content | 1, at y=2412 | **0** |
| The act you came for | y=2412, scrolled to | on the bar, always visible |

## Scenes checked

| Scene | Reading |
|---|---|
| List | One list language: the source is a REAL row with its app count, its apps indented beneath it, and the five-dot rail landing at the same x on every row |
| Published | `Published / serving now`, bar offers `Unpublish` + `Redeploy` |
| **Draft** | `Draft / not served to anyone yet`, bar offers `Discard` + `Deploy`, and **zero Archive buttons anywhere on the page** |
| Archived | `Archived / answers nothing, and the name is still held`, bar offers `Delete` + `Restore` |
| Delete confirm | Takes the bar over, names the hostname and the domains, says the record stays and the deployable cannot be brought back, `Delete` disabled until the hostname is typed |
| **A build that broke** | Open stop is **Build**, not Live; marks read `done, ahead, done, stopped, done`; bar says **Retry the deploy** rather than "Deploy the update", because there is no update -- there is an attempt that did not land |
| Source view | The credential, the auto-deploy switch, the app list and the cascade archive, on the page whose subject is the source |
| Both themes | Legible in each; the porcelain light set resolves and `Delete` keeps the error tone |

## Findings, and what happened to each

### 1. The Live stop still carried Pause and Archive -- FIXED

The acts moved to the bar and the stop's own copies were **left behind**, so
Pause existed twice (as `Unpublish` on the bar and `Pause` in the stop) and
Archive was still buried under five sections at y=2499. That is precisely what
rule 12 forbids -- "nothing that changes the thing's state lives anywhere else
on the page" -- and **the whole test suite was green with it**: 1,693 cases,
none of which can see that two controls do one thing in two places.

The stop keeps the READINGS (traffic, versions, settings), which are things to
look at rather than things to press, and which a reader who cannot write still
sees.

### 2. The rail printed its own answer twice -- FIXED

A collapsed stop shows its ANSWER on the line; an open one also showed the
NOTE, and for a settled stop those are the same string. Found as
`Found multiple elements with the text: /cannot be paused/` in the suite once
the assertion existed, but it renders as a visible duplicate. The note now
renders only when it says something the line above does not.

### 3. `Redeploy` did nothing -- FIXED

The act name existed and the switch that dispatches it had no case for it, so
the primary button on every live source-backed deployable with nothing newer
upstream was inert. Caught by a test asserting the CALL rather than the label.

### 4. The Map rendered the whole page under the picture -- FIXED

Not found in this sweep so much as confronted by it: the Map section rendered
the same `DeployablePage` beneath the drawing, so the 5,069px lived there too.
A map answers "which host, which site, which bundle" at a glance; it points at
a deployable and links to its page now.

## What was NOT changed

A live deployable with **no run history** draws `What it is` and `Build` as
unreachable, because `railFor` reads those stops off the newest run's report
and there is none. It is the pre-existing standing reading and this epic did
not touch it -- noted here so the next sweep does not read it as new.
