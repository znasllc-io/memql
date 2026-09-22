---
title: "Visual QA: the machine detail's connection health"
audience: internal
status: stable
area: ops
sinceVersion: 0.21.0
owner: znas
---

# Visual QA: the machine detail's connection health

- **Date:** 2026-09-22
- **Epic:** memql#5327 (`fleet-connection-hardening`), designs D2, D4 and D6
- **Method:** the standing QA harness (`clients/os/qa/`), extended with five
  `fleet-*` views, in `/usr/bin/google-chrome --headless=new` one-shot
  captures at 1400px and 820px, dark and light.

`clients/os/DESIGN.md` makes rendered screenshots the acceptance for any
surface change under its rules, and this sweep earned it twice: **the surface
shipped 3,495 passing assertions with two defects that only pixels could
show.**

## What changed on the surface

Three things, all of them sentences rather than figures -- the class jsdom
cannot judge, because it performs no layout and never puts a value beside its
own label.

- **`CredentialHealth`**, above the facts: a warning when the machine's
  credential is about to expire or has (D4), and one when its clock is far
  enough out to have decided `online` under the old rule (D6). Absent
  otherwise, which is most of the time.
- **Two facts**, always: `Clock offset` and `Credential expires`.
- **The removal flow**, whose copy is now true: one act revokes the
  registration and the credential together (D2), and a partial result says so.

## What the harness gained

Two additions, both reusable and both in the committed harness rather than in
a throwaway:

- five `fleet-*` views (`fleet-healthy`, `fleet-expiring`, `fleet-expired`,
  `fleet-skewed`, `fleet-silent`) mounting the real `MachineDetail` over
  `test/fleet/harness.tsx`'s own fixture connection;
- **`?open=1`**, which opens every `<details>` on the page after the virtual
  clock settles. A one-shot capture cannot click, and the facts list -- the
  densest thing on this page, and the place a long value runs past its own
  label -- is behind a shut `<details>`. Without it the two new facts could
  not have been judged at all, which is how the first defect below would have
  survived.

## The two defects, neither visible to the suite

**1. Every advisory ran to about a hundred and sixty characters on one line.**
An `.os-notice` fills its container, and on a machine detail at 1400px that is
most of the window. The second line of each warning is a full sentence, so it
rendered unbroken across the whole width -- at which point the eye returns to
the line it has just read rather than finding the start of the next.

Fixed twice over: the copy was cut (the clock warning lost a clause it did not
need), and `.os-fleet-credential-health` caps its measure at `74ch`. That cap
is the only rule in this change that is about reading rather than about state.

**2. `Clock offset` read `-212000 ms`.** A raw millisecond figure, directly
under a round trip written as `34 ms, checked 30s ago`. Nobody reads 212,000
milliseconds as three and a half minutes.

Fixed with `clockOffsetParts` in `rows.ts`: ONE reading of the number, two
renderings of it. The facts list gets `3m 32s behind` and the advisory above
gets `3 minutes 32 seconds behind the cluster's` -- genuinely different jobs,
but a second implementation of "how long is 212000 ms" would be a second
answer waiting to disagree with the first. A measured zero reads `in step with
the cluster` rather than `0 ms ahead`, because the clocks agreeing is the
answer somebody is looking for and a signed zero reads as a measurement that
came out oddly.

Both are pinned by `test/fleet/credentialHealth.test.tsx` now, so neither can
come back quietly.

## What the captures confirmed

- **`fleet-healthy` is the control, and it is the one to read first.** The
  whole design is that these warnings are ABSENT almost always, so a capture
  of an unbroken machine showing nothing at all above the facts is what says
  the page has not become a wall of advisories. `:empty { display: none }` on
  the wrapper does its job.
- **Two advisories at once stack and stay legible**, at 1400px and at 820px,
  in both modes. The warn tone reads as an amber left rule against both
  backgrounds and does not compete with the red Remove button at the foot of
  the panel.
- **`fleet-silent` is the pre-D4 fleet** -- a cockpit that stamps no
  timestamps and a token with no expiry -- and both facts read as answers:
  `not reported -- this cockpit does not timestamp its heartbeats` and `does
  not expire`. Neither is a blank that looks like a missing value, and neither
  is dressed as health.
- **The error tone is reserved for the expired case**, which is the one where
  nothing can be done from this page any more. Before the date the machine
  renews itself; after it, the only route back is pairing again, and the two
  must not read alike.

## What this sweep did NOT judge

It renders `MachineDetail` directly, in the wrapper the page gives it, and it
does not click. So these captures judge LAYOUT, COLOUR, COPY and DENSITY, and
say nothing about the navigation that reaches them or about the removal
confirmation's own flow -- which is what `test/fleet/machines.test.tsx` and
`test/fleet/workspace.test.tsx` are for.

Reproduce:

```bash
cd clients/os
npx vite --config qa/vite.config.ts --port 5207 --strictPort
google-chrome --headless=new --disable-gpu --no-sandbox --hide-scrollbars \
  --window-size=1400,900 --virtual-time-budget=14000 \
  --screenshot=facts.png "http://localhost:5207/?view=fleet-skewed&mode=dark&open=1"
```
