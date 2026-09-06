---
title: Retiring the MemQL Portal
audience: internal
status: stable
area: ops
sinceVersion: 0.20.0
owner: znas
---

# Retiring the MemQL Portal

Epic [memql#4984](https://github.com/znasllc-io/memql/issues/4984), sub-project
E of the program [memql#4961](https://github.com/znasllc-io/memql/issues/4961).
The portal SPA (`clients/portal`) is deleted; MemQL OS is the console.

This record is the inventory issue #4985 asked for: **every portal capability,
against its OS equivalent or its retirement.** It is written to be read after
the fact, by somebody asking "where did X go".

---

## What the portal was, structurally

Three separate things wore the name, and they died at different rates. Keeping
them apart is what made the deletion tractable.

| | What it was | What happened |
|---|---|---|
| The source tree | `clients/portal/`, 352 tracked files | deleted |
| The DSL domain | `dsl/portalviews/` + `v1:portalviews:view` | deleted |
| The deployed site | `portal.<domain>`, a `v1:platform:site` row, a front-door host, an OAuth client, a seed | retired; the OS shell inherits the slot |

The third is the one that reached furthest. `portal` was **not** a front-door
ROLE — `frontdoor.Roles()` has always been `{api, identity, mcp}`. It was the
one SITE whose name existed before an operator created a row, which is why it
had an exact Ingress rule, a certificate SAN and a reserved hostname. The OS
shell had acquired the identical exception in memql#4705, so retiring the
portal did not remove the exception; it left the OS holding it alone.

---

## The inventory

### Covered: the OS already did this

| Portal surface | Where it is now |
|---|---|
| `/views/users`, `/admin/people` | Users app -> People |
| `/views/users/rows/:id` | Users app -> People -> person detail |
| `/views/accounts` + detail | Accounts app |
| `/integrations` | Settings -> Integrations |
| `/integrations/campaigns` + editor | Campaigns app (which also adds senders and rules) |
| `/deployables` + detail | Deployables app (which also adds GitHub sources, custom domains, traffic) |
| `/fleet/machines` | Fleet -> Machines, Fleet -> Routing |
| `/fleet/workbenches` | Fleet -> Workbenches |
| `/artifacts` + detail | Files app -> Browse, plus the Bin app for archived items |
| Invite / pending invitations | Users app -> Invites |
| Theme toggle | Settings -> Appearance, and the Launcher's Themes drawer |
| Rail status / connection | Settings -> Diagnostics, Settings -> Cluster |
| Live band | `clients/os/src/live/` |
| Nav rail, area frames | The dock plus per-app section nav |
| `auth/callback`, the sign-in page | `clients/os/src/auth/`, `chrome/SignIn.tsx` |

### Built for this epic: no OS equivalent, and no other home

These four are the reason this epic ships code rather than only deletions.
Each is live operator capability whose engine half already existed; only the
UI lived in the portal.

| Portal surface | Built as | Engine half it drives |
|---|---|---|
| `/admin/providers` | Settings -> AI providers | `providerAuthStatus`, `providerKeySet`, `providerFederationSet`, `providerVerify`, `providersReload` |
| `/admin/tokens` | Settings -> Tokens | `patIdentitiesForUser`, `nodeTokenIdentitiesAdmin`, `IdentityAdminMsg` revokes |
| `/admin/settings` (editing) | Settings -> Cluster -> Policy | `clusterSettingsCurrent`, `IdentityAdminMsg.updateClusterSettings` |
| `/admin/keys` | Settings -> Keys | the public JWKS feed, `recentAuditEvents` |

**Why these four and not others.** The test is not "was it useful" but "does
deleting it remove a capability with no other route". A cluster with no
provider configured cannot call a model, and the engine's own keyless-boot
message used to say *"configure in the portal"*; a leaked personal access token
had no revoke path outside the portal at all. The rest of the gaps below have
either another home or no live claim on them.

One design decision inside that work is worth recording on its own. **The Keys
section leads with whether the identity replicas AGREE on their keyset, not
with a list of keys.** A key list is something `curl` gives you. Divergent
keysets across replicas fail roughly half of all auth (memql#3400) and present
as "sign-in is broken" with every manifest looking correct; `make status`
checked it from a terminal and nothing checked it from a browser. The section
reads the feed four times and states exactly what that is worth: disagreement
is proof, agreement is evidence, because the front door chooses which replica
answers each read.

### Retired with the legacy they served

This is the largest group by file count and the smallest by capability. It is
`dsl/portalviews` and everything the portal built on top of it — the
arrangement system that WAS the portal's page system.

| Portal surface | Why it is not replaced |
|---|---|
| `/views` gallery, `/views/:id` | The arrangement engine is the thing being retired. The OS has hand-built sections. |
| `/compose`, `/compose/new`, `/compose/:id` | Composing a screen from a concept selection. Same. |
| Page regeneration, the version strip, page overrides | Same. `v1:portalviews:view` `kind="override"` rows go with the concept. |
| Synapse (the `uiAssist` AI form fill) | A portal-only affordance over a portal-only prompt. The OS's Ask makes a goal from a prompt, which is a different capability, and duplicating Synapse into it was not asked for. |
| The widget registry's 13 widgets | Their underlying capabilities are in the apps above; the WIDGET form was the arrangement system's. |
| Page guides (`src/guides/`) | Per-page explanatory text keyed by nav id. The OS names its own sections. |
| The command palette | The OS Launcher covers app launching. Concept and view entries had nothing left to point at. |
| The Constellation 3-D concept-graph scene | The OS carries no WebGL (epic memql#4785, owner requirement). |
| The Console dashboard `/` | A desk of windows is the OS's answer to a landing page. |
| `/me`, `/me/settings`, `/me/sessions`, `/me/security` | **Identity already owns these**, at `identity.<domain>/me/{settings,devices,tokens,export}` — passkeys, sessions, revoke, revoke-all, the sign-in-policy switch. The portal's Me tabs were partly a wrapper and partly a link list pointing at exactly those pages. |
| `/views/deployments` deploy + rollback, `/cluster-ops` | **The Cockpit owns deploy control**, and the OS Cluster section said so in surface before this epic. `DeployControlService` is unchanged. |

### Deferred: filed, not silently dropped

These have no OS equivalent and no other home, and they are NOT operator-
critical in the sense above: nothing about running a cluster stops working
without them. They are filed against a follow-up epic rather than left as an
undocumented hole.

- The concept browser: `/concepts`, `/concepts/:id`, its rows pane and schema
  pane. The VS Code extension's Constructs view covers the developer case.
- `/modules` and `/modules/:kind/:name` — the module registry and the per-node
  env surface.
- `/data-origins` — declared data state per concept, plus connector health.
- `/stores` and `/stores/:id` — the Shopify connector's operator surface.
- `/views/agents` — the agent registry and standing authorizations.
- `/views/audit` — the `v1:identity:auditEvent` trail. The Logs app is the
  `v1:observability:logLine` store, which is a different population.
- Artifact LABELS — the filter and the editor. OS Files is folder-based.
- `/fleet/apps` delegation policy and delegated-run transcripts. Fleet ->
  Machines already lists each machine's local apps.
- Account credential issue / revoke and the one-time secret reveal.
- The first-run gate. Identity's `/setup` and the Accounts app's first-run card
  each cover part of it.

---

## What removing the site touched

Beyond the two trees, in the order the work had to happen:

1. **`component/frontdoor`** — `PortalSite` and `PortalHost` deleted; the
   `Hosts()` derivation drops one entry, so the SAN set and every gate that
   computes from it follow automatically. Six hosts now, not seven.
2. **`component/envregistry/domain.go`** — the portal origin, its CORS entry,
   and its redirect URI. **The OAuth client id changed from `portal` to `os`**,
   which is safe precisely because no client hardcodes it: a bundle reads its
   own client id out of the edge's runtime-config document, which resolves the
   request hostname against the registered redirect URIs.
3. **`component/identity`** — `PortalHomeURL` became `ShellHomeURL` and the
   post-login landing moved from `portal.<d>` to `os.<d>`. The file moved with
   it (`portal.go` -> `shellhome.go`): a helper called `PortalHomeURL` that
   returns an `os.<d>` URL is a name that lies to the next reader.
4. **`component/memql`** — the seed-materializer's portal hostname hook, and
   the reserved-label set. **`portal` stays reserved**, moved from the derived
   front-door labels into `squatReservedSiteLabels` beside `www` / `admin` /
   `mail`: it stopped being a front-door host and did not stop reading as the
   platform's, and un-reserving a label is a one-way door.
5. **`component/node/routing.go`** — the three `v1:portalviews:view` broadcast
   rules. The cross-replica reach test they anchored was re-pointed at
   `v1:worker:registration` rather than deleted; the mechanism it proves is
   live, only its subject was not.
6. **`cmd/frontdoorhosts`**, the two generated cloud overlays and the local
   overlay's `portal-front-door.yaml`.
7. **`Dockerfile`** — `PORTAL_DIST_STAGE` -> `SPA_DIST_STAGE`, and the
   `portal-build` / `portal-skip` / `portal-dist` stages renamed to `spa-*`.
   The stage always built BOTH bundles, so the names had outlived the thing
   they named.
8. **CI** — the `portal-checks` lane and the `portal` paths bucket are gone and
   `osclient` absorbed them. **The docker stage-isolation step moved into
   `os-checks`**, which had been free-riding on portal-checks running it: that
   one stage built both bundles, so deleting the lane would otherwise have
   silently dropped the check for the OS bundle too. The comment recording the
   free-ride is the only reason that was visible at all.
9. **Two CI guards were re-created rather than deleted**:
   `scripts/ci/spa_image_wiring_test.go` and
   `scripts/dev/os_lane_scope_test.go`. Both protect live mechanisms — the
   three-file agreement that puts a bundle in the edge image, and the
   fail-open where a `file:` dependency drops out of its consumer's bucket.

---

## What a reader should NOT conclude

- **`sdk/ts-viewkit` is not retired.** The portal was one of two consumers; the
  VS Code extension imports it from 18 modules. Only the portal's `file:`
  dependency on it went away.
- **`v1:platform:site` did not change.** The OS is a site row like any other,
  and the edge still cannot tell it apart from a customer's SPA.
- **`DeployControlService` did not change.** Deploy control was never the
  portal's to lose.
- **The `/admin/*` routes on identity are unchanged.** They still answer
  `410 Gone`; only the sentence naming where the screens went was updated.
