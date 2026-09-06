---
title: "Visual QA: the four Settings sections the portal's retirement added"
audience: internal
status: stable
area: ops
sinceVersion: 0.20.0
owner: znas
---

# Visual QA: the four Settings sections the portal's retirement added

- **Date:** 2026-09-06
- **Epic:** memql#4984
- **Method:** a temporary Vite QA harness (`clients/os/qa.html` + `qa/main.tsx`
  + `qa/fakeConnection.tsx` + `vite.qa.config.ts`) mounting the REAL
  `SettingsApp` over a fake connection, driven in Chrome at 1120x1180 in both
  themes. Deleted after the sweep, as its predecessors were
  ([2026-09-04](2026-09-04-deployables-recomposed-visual-qa.md)).

DESIGN.md's closing note makes rendered screenshots the acceptance for any
surface change under its rules. **Every finding below was invisible to the 144
passing jsdom tests**, because jsdom performs no layout, resolves no custom
property, and has no reading order to get wrong.

## What the harness had to get right

Both traps recorded by the previous sweeps fired again, and a third is new:

1. **The module swap is decided on the RESOLVED path.** `src/live/connection`
   is spelled five ways; a specifier alias cannot cover `./connection` from
   inside `src/live` without also catching `chrome/connection`, a DIFFERENT
   module. The plugin resolves with `skipSelf` and compares absolute paths.
2. **`setRoleLadder(SEEDED_LADDER)`.** The ladder starts EMPTY and the shell
   fills it from `v1:rbac:role`; skipping it renders every gated section
   refused. Correct product behaviour, and silent to debug.
3. **NEW: the Keys section reads over `fetch`, not over the connection.** The
   JWKS feed is a PUBLIC document read the way a verifier reads it, so the
   harness has to intercept the TRANSPORT rather than a module. Without that
   stub the section rendered its honest "no read answered" state -- which was
   useful by accident, and would have hidden the agree and diverged states
   entirely.

A fourth, from the environment rather than the code: **the harness must bind
past loopback.** `host: "127.0.0.1"` served the shell fine and gave Chrome an
error page; `host: true` and the LAN address is what the browser reaches.

## Findings, and what happened to each

### 1. The token rows' trailing cluster was overloaded -- FIXED

Two chips plus a Revoke button in a 534 px measure wrapped "In use" onto two
lines and "Agents may use it" onto three, leaving three rows at three
different heights and breaking `bff-2` across a line as `bff-` / `2`.

**One state and one act in the cluster.** Everything that is a FACT rather
than a state -- whether agents may use it, the node type -- moved into the
row's quiet middle, where facts live. Rows are now uniform and no name breaks.

### 2. `Row` children were arriving as four flex items -- FIXED

`Row` renders `children` as direct siblings inside a flex row with a gap, so
a fragment of `<span>`s and strings became four columns with gutters between
them: `-- last used | 14h ago | -- agents may use it`. Every other caller in
the shell passes ONE element; that is why. Wrapped, and the sentence reads as
a sentence.

### 3. Raw ISO timestamps where the kit has a formatter -- FIXED

`2026-09-05T18:22:00Z` is the value the row carries and not the answer to the
question somebody opens Tokens with. Now `formatFreshness` -- "last used 14h
ago" -- with the exact instant on `title`.

### 4. The Keys "unknown" state rendered in the good-news voice -- FIXED

`tone={diverged ? "error" : "info"}` put "No read of the JWKS feed answered"
on a green bar. An outage is not coherence, and the reassuring colour on the
one reading that guarantees nothing is the worst tone this surface could pick.
Three tones for three answers now.

### 5. The destructive confirm read question -> buttons -> consequence -- FIXED

`Notice` renders `children` before `next`, so putting the acts in `children`
and the consequence in `next` offered the act before saying what it does.
Consequence first, acts last.

Also: `1 keys` -> `1 key`, and when the reads disagree the Published-keys
panel now says it is showing ONE READ'S ANSWER rather than implying a single
published truth.

## Scenes checked

| Section | Scene | Reading |
|---|---|---|
| AI providers | populated, dark | Head + one primary (Apply); "2 of 3 providers can be called" in the warn voice; federation above the key box in the Anthropic panel; each registry row names its credential SOURCE |
| AI providers | **empty**, light | "No AI provider is configured yet, which is how a cluster is installed." + "Configure one below, then Apply." -- an invitation, not a fault. The registry says "Nothing is registered on the node that answered" |
| Tokens | populated, both | Two populations kept apart; one chip + one act per row; a revoked row offers no Revoke at all |
| Tokens | **confirm**, both | Anchored to its own row, names the token and its holder, consequence above the acts, `Revoke it` / `Keep it` |
| Keys | **agree**, light | "4 reads all returned the same keyset. That is evidence the replicas agree, not proof" -- the bounded claim, rendered |
| Keys | **diverged**, dark | Error tone, "2 different keysets came back from the same hostname across 4 reads", the roll-the-Deployment next step, and both keysets listed |
| Keys | **down** | "No read of the JWKS feed answered, so nothing can be said about the keysets" + "4 of 4 reads did not answer" with the verbatim reason |
| Cluster -> Policy | populated, dark | The fact groups stay borderless and the form is a bordered Panel -- the read-only/editable boundary is visible without reading a word |
| Cluster -> Policy | bounds | A stored `1800` renders `30` minutes; a stored `0` renders BLANK with a `Cluster default` placeholder; `4000` minutes reds the form and disables Save |
| Both modes | all four | Legible in each; the danger tone holds in light, and the porcelain set resolves |

## What was NOT changed, having been looked at

- **The 560 px measure.** `os-settings` renders at 560 px inside a 980 px
  window and the registry list sits inside it. Rule 9 says lists take the
  window and forms take a readable measure -- but every sibling Settings
  section uses this measure, and a provider list at 980 px beside forms at
  560 px would be the drift DESIGN.md exists to remove.
- **A `Button` inside `Row`'s `state`.** Checked against rule 6; it is
  established practice (`InvitesSection`, `TemplatesSection`), and what was
  wrong was the COUNT of things in the cluster, not the kind.
- **Disabled primaries.** They render at `opacity: 0.5` with `cursor: default`
  from the kit. JPEG compression made them look pressable in a screenshot;
  `getComputedStyle` said otherwise.
