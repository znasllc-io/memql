# MemQL OS -- the Settings App -- Design

- **Date:** 2026-08-31
- **Status:** approved (epic memql#4741; written alongside the implementation)
- **Scope:** the client half only -- `clients/os/src/apps/settings/`, the
  theme registry, one contract change in `src/system/registry.ts`, and one
  narrowing in the shell's open-by-id path. **No DSL, no Go, no proto, no
  wire-contract change**, so no frontend-team ping: this IS the frontend, and
  every read it makes already existed.
- **Depends on:** epic memql#4710 (the foundation), fully merged.
- **Issues closed:** #4741 (epic), #4742 (cluster facts), #4743 (per-app
  settings registry), #4744 (diagnostics).

## Why

The foundation shipped Settings as the app that PROVES the sections pattern:
About and Appearance real, a Cluster stub gated admin+. This epic makes it the
cluster's control room -- and does it entirely out of reads that already
exist.

## What was built

Five sections on one manifest: `about`, `appearance`, `apps`, `cluster`
(admin+), `diagnostics`.

### `settingsSection` became REQUIRED (#4743)

The type now demands it, and `settingsSectionProblem` checks that it names a
section the manifest actually declares -- a gear pointing at an id
`sectionsForRole` never returns navigates the window nowhere, and that failure
is silent. The check is a FUNCTION rather than a bare test assertion so the
apps index can report the same defect it gates on.

It checks DECLARED sections, never role-admitted ones. A gear target gated
above the viewer is a legitimate manifest -- the window simply shows no gear
for them -- while a target that exists for nobody is a bug in every session.

### The apps index is a DIRECTORY, not a host

Settings could embed each app's settings UI, and then every app epic would
have to keep a second surface working inside a window it does not own: role
gating, live reads, Ask context and all. Instead an entry OPENS the app on its
own settings section.

**That deep-link exposed a real defect, and fixing it took two changes.** An
app applies its own default-section preference on mount -- Fleet does, because
the shell opens every app on `sections[0]`. A window created on the shell
default and navigated a tick later gets dragged straight back, so the
deep-link silently did not work. So:

- `OsActions.openApp` gained an optional `sectionId`, and a NEW window is
  created on the requested section rather than navigated afterwards;
- Fleet's preference now applies only when the window opened on the shell's
  default, because a window opened on a NAMED section was opened by somebody
  who said where they wanted to be.

Both halves are load-bearing; `test/settings/appsIndex.test.tsx` fails with
either one reverted.

### Two kinds of fact, read two ways (#4742)

This split is the design, not an optimization:

- **Graph rows** -- the cluster singleton, the deployment history -- are
  LIVE. Rows somebody writes, events the mesh broadcasts, and a deployment
  landing while an operator watches is what a console should show unasked.
- **Registry projections** -- `integrationStatus`, `providerAuthStatus` --
  are request/reply with an explicit Refresh and a fetched-at stamp. They
  project ONE NODE's in-memory state, so there is no graph event to subscribe
  to, and a panel that LOOKED live while showing a registry that stopped
  moving would invite an operator to trust a minutes-old reading. Which
  replica answered is not knowable from here and the panel says so -- the
  same deliberate downgrade `clients/portal/src/admin/useProviders.ts`
  documents.

The per-node specs are a FETCH keyed off the resolved deployment, never a
collection keyed on it: the deployment id resolves asynchronously, and a
collection key carrying async-resolved state tears its subscription down and
re-seeds the moment the value lands.

Engine-as-spine is resolved on the READ side, because the engine never does
it: an empty `deploymentNodeSpec.version` means the deployment's engine
version, and rendering the raw empty string shows "no version" for the normal
case -- most node types are unpinned -- which reads as a broken deployment.

**A server refusal renders where the panel would be, in the engine's own
words.** `providerAuthStatus` is owner-only while the section admits admin, so
an admin seeing the refusal there is the system working. The role floor for
the section and the floor for one read inside it are genuinely different, and
flattening them would either hide the section from admins or promise them a
panel that always fails.

### What the Cluster panel deliberately does NOT claim

The epic's brief named database engine facts and a "JWKS reachable" line off
`v1:cluster:identityProvider.status` + `lastVerifiedAt`. **Those fields have
no writer.** `createDatabase` stamps `engine` / `port` / `status` and never
`engineVersion` / `extensions`; `createIdentityProvider` never writes
`jwksUrl` or `lastVerifiedAt`; `status` is stamped `"connected"` at bootstrap
and nothing ever refreshes it. The `databaseId` / `identityProviderId` links
on the cluster row are never populated either, so there is no path from the
cluster singleton to those rows at all.

Rendering a frozen literal as a health check is worse than omitting it: an
operator would read "JWKS reachable" off a constant. So the panel shows the
LIVE engine version, commit and node id the connected node reports on the
ServerHello, the deployment pins from rows something actually writes, and the
domain and issuer the cluster served this client. Making the rest real is a
bootstrap-path change -- `createDatabase` / `createIdentityProvider`, the
`bootstrapCluster` steps that call them, and `createCluster`'s link args --
and bootstrap runs once per cluster, so existing clusters would need a rebuild
to backfill. Filed separately rather than smuggled into a client epic.

### Diagnostics (#4744)

- **Connection.** sdk-core exposes EVENTS, not history, so the app keeps a
  bounded in-memory ring buffer (50) of transitions. The event
  `onStatusChange` fires synchronously on subscribe is recorded as a
  BASELINE, not a transition, and never counts as a reconnect -- nothing is
  known about what preceded it, and counting it would tell an operator the
  connection had just recovered at the moment they opened the window. The
  provider wraps the whole Settings app so the buffer covers the WINDOW's
  lifetime: a person who saw the dock dot change and then navigated to
  Diagnostics is exactly who it is for.
- **The endpoint is resolved, not composed.** The OS dials a relative
  `/_memql/ws` because component/edge serves the bundle. Composing
  `wss://api.<domain>/memql/ws` from the runtime-config domain would print an
  endpoint this client does not use -- a second derivation that disagrees
  with the first. Displayed, never re-dialed.
- **Permissions self-view** over the one role predicate, with the spec's own
  presentation-gating caveat on the panel. A hidden app's sections are not
  enumerated under it: that pads the table with rows all saying the same
  thing and buries the informative case, a section gated above an app the
  person can otherwise open.
- **Copy diagnostics** produces plain text with an in-surface fallback (a
  selectable textarea) when the clipboard refuses -- never a toast. The
  report carries no bearer or token material, no credential presence map
  (which slots are filled is reconnaissance even without values, the same
  argument that makes `providerAuthStatus` owner-only), and no address but
  the session's own. AI provider status is deliberately absent for that
  reason. Cluster facts appear only when the session was ADMITTED and the
  reads returned; "not admitted" is a LINE in the report rather than a silent
  omission, because a reader must be able to tell a fact that is absent from
  one that was never asked for.

### The OS bundle learned its own build identifier

Nothing in the bundle knew what it was: `package.json` is private and never
imported, `runtime-config.json` carries no version, and the only build
identity reaching the browser was the server's. `__OS_BUILD__` is a Vite
`define`, defaulting to the package version and overridable by
`MEMQL_OS_BUILD`.

It is deliberately NOT a git sha: the release path builds this bundle inside a
Docker stage that does `COPY clients/os ./clients/os`, with no `.git` present
-- so a sha derived at build time would resolve to nothing in exactly the
build whose identity matters most, and would do it silently. No timestamp and
no host either, so the bundle stays reproducible.

## Not built

Any cluster WRITE (cluster settings editing stays in the portal's admin
console; deploy control stays with the portal and the cockpit); the theme
marketplace and installable packs (#4745 -- the selector lists the built-in
and says so); per-window theme mix; notification settings.
