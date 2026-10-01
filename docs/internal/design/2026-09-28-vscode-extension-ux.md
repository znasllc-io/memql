---
title: VS Code Extension Interface
audience: internal
status: draft
area: design
sinceVersion: 0.23.5
owner: znas
---

# VS Code Extension Interface

The MemQL extension for Visual Studio Code and Cursor follows the MemQL OS
interface language ([DESIGN.md](../../../clients/os/DESIGN.md) and
[Supervised Visual Composition](../../../clients/os/SUPERVISED-VISUAL-COMPOSITION.md)),
adapted to what an editor extension can draw. This record states the adaptation.
When a screen and this record disagree, the screen is wrong.

The owner's brief (2026-09-28): the extension is cluttered and written for the
people who built it. It must be clean, minimal and production-worthy without
losing a single capability. Every label and element has a purpose. A user must
be able to reconnect to an existing local cluster, install one and uninstall one
from the extension, end to end.

## Audience and voice

The reader is a moderately technical MemQL user, not the extension's author.

- Plain words: cluster, local cluster, MemQL OS, sign in, install, uninstall,
  repair, update, deploy. Never "portal" or "console" for MemQL OS.
- Never in user copy: issue references (`memql#1234`), internal identifiers
  (`token`, `refresh_token`, `client_id`, receipt, graph, wave, capability,
  envelope, stderr, exit code, SecretStorage, `clusters.yaml` unless the person
  must edit that file), design rationale, or a description of what the
  extension does not do.
- A status line is sentence case and at most six words ("Creating the cluster").
- A lede is at most one sentence and exists only when it helps the decision on
  screen. Most screens have none.
- Say it once: a state shown by a badge, icon or bar is not repeated in prose.
- An error is one sentence of what happened plus the fix, as a button when the
  extension can do it ("Sign in", "Retry", "Run in terminal"). Detail goes to
  the log disclosure or the Output channel, never a paragraph on the page.
- Separator in descriptions: `·`. No `--` as punctuation in user copy.
- Toasts: one per event, short, with the fix as a button. Modal dialogs only for
  destructive confirmation.

## Page anatomy (webviews)

Every webview page is the same three parts, built from `src/webview/ui/kit.ts`:

1. **Head**: the title and a quiet meta (a version, a count). Record-management
   acts that do not change the thing's state (Edit, Remove from list) may sit
   as quiet text acts at the head's trailing edge. No brand strip, no lede by
   default.
2. **Body**: facts (`facts`), groups (`subhead`), forms (`field`, `switchRow`),
   notices (`notice`), lists. No boxes around groups; the subhead is the
   container language.
3. **Action bar**: sticky at the bottom. The state in words on the left, then
   the acts legal from that state on the right: at most three, primary last,
   one button, the rest text acts. **An act that is not legal is absent, never
   disabled.** Nothing that changes the thing's state lives anywhere else.

Loading is the shape of the content (`skeleton`), with the words only for
screen readers. A failed or disconnected read is shown as such, never as empty.

## Updating a page without repainting it

A page's document is assigned once per screen (`webview.html`). Everything that
changes while a screen is showing arrives by `postMessage` through
`src/webview/ui/liveView.ts` and the one page runtime
(`src/webview/ui/runtime.ts`): region patches, progress, log lines. Scroll,
focus, selection, an open disclosure and a half-typed field survive. The page
posts `ready` on load, and the host re-sends the current state then (a hidden
panel drops messages).

## Long operations: one progress screen

Install, repair, uninstall, deploy, rebuild and update use one screen
(`kit.progress`):

- The MemQL mark (inline SVG, accent colour), a title in the act's own words
  ("Installing MemQL", "Uninstalling MemQL").
- A determinate bar. Percent is weighted by each step's expected duration,
  moves forward only, and inside a long step follows the step's own reported
  phases (`cap_progress`).
- One status line: the running step's short label, or its current phase
  ("Starting services 5 of 9").
- A meta line: "Step 6 of 16 · 3:12".
- "Show logs": a text disclosure that opens the live, chronological log (each
  line prefixed with its step label), following the tail until the person
  scrolls up. Copy and Open in Output sit on the log's header.
- Action bar: busy state ("Installing") with Cancel as a text act. After Cancel
  the state reads "Stopping after the current step" until the run settles.
- Failure: the bar turns red, the status names the step in the negative
  ("Couldn't create the cluster": the label's verb in its base form), one notice
  gives the reason and the remedy command with Run in terminal, and the log
  opens at the failed step. Acts: Cancel, Retry (only when retryable).
- Done: the mark, "MemQL is installed", and the one next act as primary.

Short step labels live in the graph documents (`label`) so the CLI shares them.

| Step | Label | Expected seconds |
|---|---|---|
| detect | Checking this computer | 5 |
| dockerAccess | Checking Docker | 3 |
| providerFederation | Checking AI access | 1 |
| toolK3d / toolKubectl / toolMkcert | Installing tools | 15 each |
| hostsBlock | Adding local addresses | 5 |
| browserTrust | Setting up browser trust | 30 |
| stackCheckout | Downloading MemQL | 30 |
| localCA | Creating certificates | 10 |
| clusterUp | Creating the cluster | 480 |
| buildImages | Building MemQL | 900 |
| seedBootstrap | Creating your account | 90 |
| frontDoor | Checking secure access | 20 |
| magicLink / enrolmentLink | Preparing sign-in | 5 |
| recoveryKey | Creating your recovery key | 5 |
| removeCluster | Removing the cluster | 30 |
| removeCheckout | Removing downloaded files | 3 |
| removeHostsBlock | Removing local addresses | 3 |
| removeLocalCA | Removing certificates | 3 |
| removeToolK3d / Kubectl / Mkcert | Removing tools | 3 each |

A step's weight is its most recent successful duration from the run history
when one exists, otherwise the table's value.

## Choices are switches

Every on/off choice on a page is a switch (`kit.switchRow`: a native checkbox
with `role="switch"`, so Space, label click and assistive tech work; the thumb
position carries the state, not colour alone). A destructive switch uses the
danger tone. A typed confirmation appears beneath its switch only while the
switch is on, and the danger act appears in the action bar only once the
phrase matches.

## Signing in and reconnecting

- The editor signs in as its own client, `memql-vscode`, always. A `client_id`
  in the shared cluster registry belongs to whichever tool wrote it and is never
  used for the editor's sign-in or refresh. The client a refresh token was
  issued to is stored beside it and presented on refresh.
- Sign in is one click from every place that says sign-in is needed: the
  cluster's row (inline), the cluster page (primary act), the toast. No dialog
  in front of it. When MemQL OS is signed in in the browser, sign-in completes
  without typing.
- The browser flow is checked before the browser opens: a refused client or
  redirect fails in seconds with a sentence and a fix, not a ten-minute wait.
  After 30 seconds without a callback, "Use a code instead" is offered without
  abandoning the browser flow. The browser's own page says "Signed in" only
  after the code exchange succeeds.
- The selected cluster reconnects when the window opens and after a dropped
  connection, without a click, whenever a stored session can do it.
- A claimed cluster (the owner already exists) always routes to Sign in, never
  to owner set-up.

## Tree views and menus

- One inline act per row: the next thing to do in the row's state (Sign in,
  Open MemQL OS). Everything else is in the context menu, gated by state, with
  destructive items in their own last group.
- The cluster in use is marked on its row.
- Title bars keep Add and navigation. No Refresh on a view that updates itself
  (Clusters, Deployments, Runs); Constructs and Data keep Refresh because
  nothing pushes their changes. Every command stays in the palette.
- Commands carry the category "MemQL" and titles without the prefix.
- Welcome views are one short line and their buttons.

## Acceptance

Rendered screens at real size in light and dark, wide and narrow, empty and
populated (the gallery: `npm run gallery` in `editors/vscode`), keyboard reach,
and the same scenarios end to end in a real editor against a real local cluster:
reconnect, uninstall, install. Every command and every page act that existed
before still exists after (the action inventory test).
