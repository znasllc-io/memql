---
title: Fleet Visual Composition
audience: internal
status: draft
area: design
sinceVersion: 0.22.0
owner: znas
---

# Fleet Visual Composition

## Interaction model

Fleet's Workspace consolidates Machines, Models, Routing, Workbenches and Apps.
The selected machine anchors reported hardware, attached local models, installed
apps, active work and sharing. A compact Machines/Policies switch opens cluster
routing composition. Selection and edits are separate: selecting equipment never
installs, probes, pairs or changes a route. Disappearing machines retain an
explicit missing-selection state rather than redirecting an edit to a peer.

The approved prototype in `work/visualizations/fleet-composition-preview.html`
is a design reference only. Production uses live collections and shared React
controls. It intentionally differs from the prototype's simplified task-scope
selector: task matching is a separate rule, and shipped matching rules retain
priority. All sources are ordered; more than three slots are supported. A source
being configured does not assert that it is available or compatible.

The guided install retains its existing state machine, credential-in-memory
handling, matching registration, install commands, recovery and post-connection
checks. The step trail reflects real lifecycle transitions. Info dialogs support
Escape, modal focus and return to their trigger. Critical installation, computer
use and metered-source consequences remain visible.

Fleet Settings and Logs retain their existing functionality. Their proposed
redesigns require separate user approval. This change does not redesign other
apps; the shared rule editor gains the backend integration needed by Fleet.

## Reachability and existing operations

| Operation | Entry |
| --- | --- |
| Connect, install, recover, verify machine | Connect machine / four-step install |
| Rename, hardware, labels, remove/uninstall | Selected machine → Machine details |
| Install/remove models, runtime controls | Attached models → Manage models |
| Installed app state and checks | App equipment → Inspect apps |
| Machine sharing / inference serving | Personal or shared inference |
| Calls and machine history | Recent work |
| Cross-machine model catalog / model ordering | Across your fleet → Model library |
| Machine routing and decisions | Across your fleet → Fleet routing |
| Delegation policy and app sessions | Across your fleet → App activity |
| Replica-hosted workbenches | Across your fleet → Cluster workspaces |
| Policy create/edit/reset, ordered sources | Policies |
| Rule matching, validate, save, remove | Policies → Task rules |
| Logs, preferences and revoked inventory | Existing Logs and Settings destinations |

Drilldowns preserve original section IDs, role and readiness gates, preferences
and deep links. Workspace stays mounted across ordinary Fleet navigation so
installation and policy drafts remain in memory. Closing a window or losing app
access ends those drafts; they are not written to browser storage.

## Durable routing configuration

Shipped DSL policies and rules remain immutable defaults. The additive
`router_policy_revision` PostgreSQL table stores the complete shared document:
policy overrides, custom rules, revision and author. Policy reset deletes an
override; it never reconstructs a default from the current edited chain.

Every routing decision reads policies and rules from the same committed revision.
There is no process-local customization or time-based cache. This adds a shared
DB read to routing; DB failure refuses resolution rather than silently restoring
old defaults. A future cache needs a consistency contract before replacing this.

Writes require an authenticated owner or developer. An advisory transaction lock
and expected-revision comparison allow one concurrent winner. Stale edits refuse
with their draft retained. Validation checks source grammar, duplicate sources,
references/cycles, rule vocabulary/precedence, shipped-rule protection and the
active embedding-space invariant. Rules keep absent conditions distinct from
explicit empty values. Creating a new name is distinct from editing an existing
name in the UI, while the backend save contract is an explicit revisioned upsert.

Migration must be applied before updated routing nodes serve. The local identity
node's supported startup migration performs the additive change; roll identity
first, verify the new table, then roll the remaining nodes. Never reset the DB.

## Ask and semantic activity

Ask recognizes policy-authoring requests or an explicit “Compose a routing
policy” mode. `routingPolicyDescribe` compiles a typed proposal against the actual
catalog, injects the observed revision and validates it through the same save/reset
path using a detached dry-run store. It cannot persist. Unsupported requests
refuse. The person reviews/corrects the same PolicyEditor used in Fleet and
explicitly saves. Rule description uses the existing local-only `compileRule`
prompt and the same rule validation path. Policy composition also has a shipped
local-only compiler rule. No inference is generated by browsing the editor.

`SemanticActivityProvider` accepts real semantic events and `ActivityTarget`
shows labelled proposed/running/completed/failed states. Ask's returned draft
produces a real proposal state. Ordinary Fleet has no synthetic AI activity.
Outlines differ from selection/focus, include text/icon cues and have no motion.
No cursor, focus or scrolling is driven by these hooks. General autonomous UI
control and voice orchestration are outside this change.

## Verification and limits

- Existing Fleet/Ask tests exercise install token lifecycle, matching registration,
  recovery, rename, labels, revoke, sharing, model/runtime controls, app delegation,
  workbenches and route controls through their actual UI hooks and mocked transport.
  They do not install software or revoke a user's real machine.
- Added UI tests cover missing-machine selection, exact ordered source/revision
  writes, retained refused drafts, no fake AI activity/focus movement, and the
  generated Ask compiler transport with no automatic save.
- Go tests cover peer/restart state, defaults/reset, policy graph/embedding/auth
  refusals, same-revision rules, stale edits, concurrent writers and DB failure.
- A separate PostgreSQL 16 test DB exercises the real store/migration/CAS,
  persistence across independent registries and reset with audit history.
- Headless Chrome with a fresh profile renders real components against explicitly
  labelled fixtures in light/dark at desktop and narrow sizes. It tests modal
  keyboard/focus, source movement, draft retention and overflow. Fixture code is
  excluded from production; the user's visible browser is reserved for delivery.

Actual machine installation/pairing/removal and live model-assisted compilation
require a connected machine. They are not exercised against the user's empty
cluster. No demo credentials, paid calls or invented machines are created.
Historical rule replay is unavailable: the UI reports definition validation only.
Codex image generation has not been verified by the app integration. Embedding
model changes require their existing migration/binding workflow.

## Follow-up: measured desktop permissions (2026-09-15)

The locally installed worker's original permission report was the MVP stub
(`permission probe not yet implemented (MVP)` plus false booleans). Terminal
`worker setup` success did not measure the detached LaunchAgent. Fleet now
separates granted, denied, and unknown evidence instead of treating the stub as
a measured denial or repeating the same setup command.

- Additive worker protobuf states and `checked_at`/`probe_context` preserve old
  clients. A missing heartbeat report preserves prior evidence; a measured
  unknown report clears earlier certainty. An empty report is unmeasured.
- Permission heartbeats update the live registry and owner-scoped registration
  through `updateWorkerPermissions`, independently of the last-seen throttle.
  Failed persistence retries on the next reported snapshot.
- The macOS setup check marks Accessibility and Screen Recording independently.
  The aggregate completes only when both are granted. Linux X11 needs measured
  display access; Wayland remains unsupported. Linux behavior is tested, not
  claimed as validated on a live Linux desktop.
- Local worker task installed `0.13.4-permission-dev`, preserving its symlink,
  enrollment, policy, and LaunchAgent configuration with a reversible backup.
  Its real native current-process preflights reported both macOS permissions
  **denied**. These results refresh every 15 seconds and appear in Fleet.
  The owner must grant the actual worker process in macOS; Terminal's grants
  do not establish the worker's grants. No grants, captures, cursor probes,
  inference calls, re-enrollment, or wizard advancement were performed here.
- The separate 60-second gRPC resets came from an ignored Traefik overlay:
  k3s's pinned helm-controller v0.16.17 only projects HelmChartConfig Secret
  values when inline ValuesContent is nonempty. The local ingress helper now
  adds an empty YAML map only when absent, with a resource-version fence,
  preserving existing operator content and Secret references. The real Helm
  rollout contains `readTimeout=0s`; a single worker stream remained connected
  for over four minutes, with 30 stored versions and no disconnected version.
- Fleet-local responsive rules stack details and wrap long labels. At the
  in-app browser's 361px viewport, its desktop rail leaves a 155px content
  panel; Fleet fits that panel without horizontal overflow. The global rail
  remains unchanged.

Validation: worker package tests, 359 Fleet tests, rendered grant/unknown
heartbeat transitions, OS typecheck/build, SDK generation drift and DSL lint
pass. Protobuf regeneration produces identical content (the git-based check
also reports the intentional uncommitted schema diff). Ingress-specific tests
pass. Four broader k3d TLS tests fail with macOS's LibreSSL; those same tests
pass with the installed OpenSSL 3 binary. Agent and edge are locally deployed;
edge was built from a temporary source snapshot excluding unfinished
Deployables changes, preserving the shared checkout. Chrome's active setup
wizard and the owner's account/session are retained.
