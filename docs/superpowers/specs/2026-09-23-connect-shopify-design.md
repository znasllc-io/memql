> Implementation update (2026-09-24): publication is independent of Shopify
> readiness. An unconnected storefront may go live for design review, with an
> amber Store action beside Traffic. Commerce still needs connection, and
> development/unreadable bindings still refuse publication. GitHub setup lives
> in Settings; the repository wizard selects saved access. The callback relays
> unchanged to the OS-hosted completion route to recover its host-only cookie.
> These decisions supersede the earlier launch gate and callback/UI descriptions
> below. See the public Shopify Connect runbook for the shipped behavior.

# Connect Shopify, and a storefront a developer can take live

- **Date:** 2026-09-23
- **Status:** design, awaiting review
- **Branch:** `epic/connect-shopify` (worktree `../epic-connect-shopify`), cut from `origin/main` at `fe32fe3de`
- **Cluster that hit the problem:** the operator's cloud cluster runs engine 0.22.9 (`ee56dd0a41e5`). It is an ancestor of `fe32fe3de`, and none of the files this design touches differ between the two.
- **Redaction:** the operator's domain is a value, not part of this record (`TestNoVendorDomainLiterals`). Where the text says what that cluster actually served it reads "the cloud cluster", and a hostname on it is written `<name>.<domain>`.

## In plain terms

A developer deploying the Fylo storefront to the cloud cluster was refused with "a storefront names a store this cluster does not have". The store was not really missing:

- the developer was not allowed to see it
- nobody, the owner included, could put a Shopify store's keys into MemQL from a screen

This design fixes both with eight small pull requests:

- **Two security fixes come first.** The new features lean on two protections that turned out to have holes: anyone signed in, of any role, can create a store or pack record, and anyone signed in can plant a sign-in ticket.
- **Developers get three permissions:** read store records, hold the store permission, and turn storefront packs on and off.
- **A storefront's first deploy becomes a draft** instead of a refusal. It cannot go live until a store is connected.
- **A new Connect Shopify button** signs in to Shopify, lets the person approve, and has MemQL save the keys and attach the store itself.

It is done when a developer can take the Fylo storefront from today's error to live, with real products, inside MemQL OS. The one exception is a node restart that makes pack changes take effect (section 20).

## 1. The problem

On 2026-09-23 a deploy of `znasllc-io/memql-fylo` at `c541ee0` was refused in its publish step with `deployable_store_unknown`, raised fatally at `component/packages/stages.go:587-593`. The storefront's manifest declares `kind: shopify_storefront` with `binding.store: fylo-9423.myshopify.com`.

Four walls produce that refusal:

1. **A developer cannot read any `v1:shopify:store` row.**
   - The concept is `@rowAuthz(clusterOwner)` (`dsl/shopify/overlay/concepts.memql:15`), and its four reads also AND `actor.isClusterOwner == true` (`dsl/shopify/overlay/queries.memql:17, 28, 40, 58`).
   - That flag means the role is exactly `owner` (`component/auth/access_context.go:211`), so no grant changes it.
   - The deploy resolves the store under the caller (`component/packages/production.go:382`) and gets zero rows.
2. **No one can put a store's keys into the cluster from a screen.**
   - A `v1:platform:globalSecret` must be sealed with `MEMQL_MASTER_KEY`, which exists only on nodes.
   - Every server-side sealer is fixed-purpose: email settings, AI providers, the GitHub App, source credentials, and the Shopify first-boot seed (`integrations/shopify/config.go:109-163`). The seed runs only when the cluster has no store at all.
   - The runbook's "the console's Secrets surface, or `memql env`" (`docs/public/operate/shopify-connector.md:153-154`) names two routes that do not exist.
3. **The existing site cannot become a storefront.**
   - `graceful-fjord.<domain>` was created as `kind: static` from an older manifest.
   - Kind is set only at creation (`createSite`, `dsl/platform/mutations.memql:406`).
   - The edge serves storefront runtime config only for `shopify_storefront` (`component/edge/runtimeconfig.go:298`).
4. **The refusal came after the cluster had restarted.** The run staged Fylo's DSL and rolled `bff-znas`, `planner` and `agent` before the publish step refused.

Two more walls sit behind the first:

- **Every later edit to a store-bound site re-checks the store as the caller.** `executeWrite` merges the stored row into the payload (`component/memql/executor_mutation.go:914`) before `validateSiteStoreBinding` runs (`:1317`).
- **Turning on the wholesale and reviews packs**, which the storefront posts forms to, is owner-only in Go (`component/memql/module_registry.go:166`).

## 2. Goal and definition of done

A person signed in to MemQL OS on the cloud cluster **as a developer** can:

1. Retire the old static site.
2. Deploy `fylo`, and get a draft storefront instead of an error.
3. Save the Fylo Shopify app's ID and secret, press Connect Shopify, and approve on Shopify.
4. Go live, and see `graceful-fjord.<domain>` serve Fylo's real products.
5. Turn on the wholesale and reviews packs in Cluster > Modules, and switch on the storefront's shopper forms on its deployable.

The packs take effect after the node that answers storefront forms restarts. That restart is the one step outside MemQL OS (section 20).

- **No owner takes part.**
- **The same journey is proven first on the local k3d cluster.**

## 3. Decisions

| # | Decision | Who, when |
|---|---|---|
| D1 | Store credentials enter MemQL through Shopify's OAuth install ("Connect Shopify"), not by pasting tokens | Jose, 2026-09-23 |
| D2 | One new HTTP endpoint: the Shopify callback | Jose, 2026-09-23 |
| D3 | Developers may read `v1:shopify:store` rows. Writes stay owner-only except through Connect. A developer holding the store part may bind any storefront they can write to any store: that includes every account-tied client storefront, live ones included, which is the same reach developers already have to pause, archive or delete those sites | Jose, 2026-09-23; the storefront reach confirmed by the user, 2026-09-23 |
| D4 | Developers may turn storefront packs on and off | Jose, 2026-09-23 |
| D5 | A storefront's first deploy creates a draft with no store; going live requires a connected store | the user, 2026-09-23; Jose reviews in the PR |
| D6 | Jose cuts the engine release | the user, 2026-09-23 |
| D7 | The Shopify app's client ID and secret are entered on the storefront's Store panel, before Connect | the user, 2026-09-23 |
| D8 | Connect mints the Storefront API token; the Store panel also takes an optional pasted token | the user, 2026-09-23 |
| D9 | Automated tests run against a fake Shopify only. The first real connection is made from the local cluster once a Shopify login is available | the user, 2026-09-23 |
| D10 | The callback lives on the identity node and reuses GitHub Connect's single-use state row | the user, 2026-09-23 |
| D11 | Build and prove everything locally, then split into stacked, one-concern draft PRs, then wait for Jose's approval to merge | the user, 2026-09-23 |
| D12 | Saving app credentials never touches a live store. They are held as *pending* and take effect only after the shop's own staff approve on Shopify | proposed by design review; approved by the user, 2026-09-23 |
| D13 | Two security fixes ride this epic as their own PRs (sections 6 and 11), because the new features are only safe with them | proposed by design review; approved by the user, 2026-09-23 |
| D14 | PR 1's floor is developer, not owner: developers and owners may create store and pack records; everyone below is refused | the user, 2026-09-23; **superseded by D15** |
| D15 | PR 1's floor is **owner**: only a cluster owner or server code creates store and pack records. Implementation review found two exploits in a developer authoring these rows directly (section 6), and no developer step needs a direct create: Connect writes the store and the Modules flip writes the pack, both as server code | proposed by implementation review; confirmed by the user, 2026-09-23 |
| D16 | Connect Shopify's state is session-bound as GitHub Connect's is | the user, 2026-09-24 |

### Facts from Shopify that shape the design

These are from shopify.dev, read 2026-09-23. Sources are in section 21.

- **No app installs on many client stores without App Store review,** and a reviewed public app may not start installation outside Shopify (App Store requirement 2.3.1). So each client store gets its own custom-distribution app, created once in ZNAS's Shopify Dev Dashboard.
- **A non-embedded app with its own backend uses the authorization code grant.** Token exchange is embedded-only. The client credentials grant needs app and store in one Shopify organization.
- **Custom-distribution apps keep non-expiring offline tokens.** The 2027-01-01 expiring-token rule covers public apps. Connect needs no refresh logic.
- **One secret, three uses.** The app's client secret keys the callback HMAC, the code exchange and webhook signatures.
- **Minting the Storefront token has conditions.** `storefrontAccessTokenCreate` requires `unauthenticated_*` scopes held as required scopes, including `unauthenticated_read_product_listings`. A shop allows at most 100 active Storefront tokens per app.

## 4. The journey

**Once per client store, in Shopify** (a runbook ships with PR 7):

1. In ZNAS's Dev Dashboard, create an app with custom distribution for `<store>.myshopify.com`.
2. Set both its App URL and its redirect URL to `https://identity.<domain>/auth/shopify/callback`.
3. Turn embedding off.
4. Add the section 12.8 scopes as required scopes.
5. Release the version, and copy the client ID and client secret.

**In MemQL OS, as a developer:**

1. Pause, archive, then delete the old static site. Deleting releases the hostname (`component/packages/integration.go:383`).
2. Deploy `fylo`. The storefront is created as a draft with no store, and the run finishes with a note that it must be connected before it can go live.
3. Open the storefront's Store panel, which reads **Not connected**. Enter the client ID and client secret, then **Save**. The secret is sealed on the server and held as pending.
4. Press **Connect Shopify**. Shopify's approve page opens for the store, and the person approves.
5. The browser returns to the Store panel, which reads **Connected**. MemQL has:
   - sealed the keys
   - written the store record
   - minted the Storefront token
   - attached the store
   - registered webhooks
6. Optionally, paste a Storefront token to replace the minted one.
7. **Go live** is now offered; it was absent before. Press it.
8. Switch on the storefront's shopper forms. Turn on `wholesale` and `reviews` in Cluster > Modules, then have the forms node restarted (section 20).

**Rules around it:**

- **Pressing Connect again** updates the same store and never creates a second one.
- **Failure:**
  - Every failure returns to the Store panel with a sentence saying what happened and what to do.
  - A failure before the writes changes nothing apart from spending the single-use state.
  - A failure between writes is repaired by pressing Connect again, because every write is keyed by the store id.

## 5. The pull requests

| # | PR | Base |
|---|---|---|
| 1 | Store and pack records refuse creates by anyone but an owner or server code | main |
| 2 | Developers can read Shopify store records | 1 |
| 3 | Developers hold the store permission | 2 |
| 4 | Developers can turn storefront packs on and off | 1 |
| 5 | Storefronts deploy as drafts until connected | 3 |
| 6 | Only server code writes connect states | 5 |
| 7 | Connect Shopify: the engine | 6 |
| 8 | Connect Shopify: MemQL OS | 7 |

**Why the stack looks like this**

- A GitHub PR has one base, so the chain is 1 → 2 → 3 → 5 → 6 → 7 → 8, with 4 on 1.
- PR 5 does not depend on 3 in code; it sits there only to keep the chain linear.
- This design record rides PR 1.
- Locally an integration branch merges all eight. Before any PR opens, the branches are checked to reproduce that tree exactly.

**Bundled on purpose**

- **PR 5** carries draft-first together with the go-live rule, because draft-first alone would let an empty storefront go live.
- **PR 7** carries saving the app credentials, because Connect cannot run without them and they have no other use.

**Security prerequisites (D13)**

- **PR 1** closes a create hole: `createStore` and `setPackEnabled` are ungated inserts today. The design's "writes stay owner-only" relies on it.
- **PR 6** closes a forgery hole that also affects today's GitHub App setup.

Each is its own PR because it can be reviewed and reverted alone.

**Wire additions,** to be named in each commit body per the frontend-ping rule. Nothing is removed or renamed.

- **PR 4:** an optional `ModuleInfo.mayFlip` field.
- **PR 5:** the readiness refusal code `storefront_not_connected`.
- **PR 7:**
  - four `@sdk` builtins
  - the callback-to-OS return parameters `shopify` and `site`

**Lead-developer-owned paths** (`.github/CODEOWNERS`):

| PR | Paths |
|---|---|
| 2 | `docs/public/operate/auth/` |
| 3 | `component/auth/`, `docs/public/operate/auth/` |
| 4 | `component/grpc/`, `component/auth/`, `docs/public/operate/auth/` |
| 7 | `component/identity/`, `app/`, `CLAUDE.md` |

## 6. PR 1: store and pack records refuse creates by anyone but an owner or server code

**The hole.**

- The row-authz write guard does not judge a create on a cluster-owner-tier concept: with no row at the id it returns nil (`component/memql/rowauthz_write_guard.go:199-219`).
- `createStore` (`dsl/shopify/overlay/mutations.memql:7`) and `setPackEnabled` (`dsl/platform/mutations.memql:981`) are plain inserts with no gate.
- The raw `insert(...)` literal reaches the same rows with no mutation at all: it is public language and names no construct.
- So today any signed-in caller, of any role, can register a store, or enable a pack that nobody has flipped yet. Enabling `wholesale` publishes a shopper write endpoint.

**The fix (D15).**

- A **create floor at the write seam**: `executeWrite` refuses a create of `v1:shopify:store` or `v1:platform:packState` by a caller below owner (`component/memql/create_rank_floor.go`). Developer (300), admin (200), writer and everyone below are refused.
- **Not `@requiresRank` on the two mutations.** That was the first version, and review showed the raw `insert(...)` literal walks past it: a construct floor fires only when a named construct is resolved. `executeWrite` is where the named mutation and the raw literal converge, which is also why the write guard lives there.
- The check reuses `refuseBelowRequiredRank` the way the plan gate does, so internal origin passes (`component/memql/requires_rank.go:70-77`). The first-boot seed, the connector, the Connect writes (section 12) and the new pack-flip method (section 9) are unaffected.
- `updateStore` and `setStoreStatus` write existing rows, which the write guard already protects, so changing an existing store stays owner-only (or through Connect).

**Why owner and not developer (D15).** The first version used a developer floor (D14). Review found two exploits in a developer creating these rows directly, each shown with a database test:

- **A store row naming someone else's secret.** A store's `storefrontTokenRef` is a globalSecret NAME. A developer-created store could name any secret; with PR 2 the developer binds a storefront to it, and the edge decrypts whatever the name resolves to into the public `/runtime-config.json`.
- **A pack row under a fresh id.** `setPackEnabled` takes `id` and `packDomain` separately, and the pack reader keys on `packDomain`. A developer's create under a new id naming an existing pack overrides that pack's switch, owner-set or not, with no audit event.

No developer step needs a direct create: Connect writes the store (section 12) and the Modules flip writes the pack (section 9), both under internal origin. So the floor is the tier's own answer. With it, a direct `setPackEnabled` below owner is refused whether or not the pack has a row.

**Tests.** Database-backed:
- an admin and a writer are refused `createStore` on a fresh id, `setPackEnabled` for a pack with no `packState` row, and a raw `insert(...)` of either concept, with nothing written
- a developer and an owner succeed at all four
- internal origin succeeds

## 7. PR 2: developers can read Shopify store records

**The concept tier.**

- `dsl/shopify/overlay/concepts.memql:15` becomes `@rowAuthz(clusterOwner, rankFloor="developer")`.
- `rankFloor` widens reads only (`component/memql/rowauthz_read_floor.go`; `rowauthz_enforce.go:436-455`). Writes stay cluster-owner (`rowauthz_write_guard.go:136-151`).
- The floor admits rank 300 and above, which is developer and owner. Admin is 200.
- Precedents: `dsl/bench/concepts.memql:36`, `dsl/rbac/concepts.memql:125`.

**The four reads.**

- Remove `&& actor.isClusterOwner == true` from `storeById`, `storeByDomain`, `stores` and `developmentStoresFor`.
- The tier term is ANDed into each plan (`rowauthz_enforce.go:122`), so the floor does nothing while the conjunct stays.

**Not `@requiresRank`.**

- It returns an error below the floor rather than zero rows (`component/memql/requires_rank.go`).
- `canReadStore` (`platform_site_binding_guard.go:140`), `resolveStore` (`production.go:382`) and the rankless auto-deploy writer (`stages.go:561-569`) all depend on zero rows.

**Gate.**

- Add the four reads to `tierDecidesTheRead` (`component/memql/rowauthz_enforce_gate_test.go:137`). Its text at `:133` calls a new entry a design decision.
- The PR cites D3.

**Test.** Database-backed:
- developer and owner read the row
- admin and writer get zero rows with no error
- a developer's `updateStore` and `setStoreStatus` are refused

**Docs.** `docs/public/operate/auth/per-row-authz-audit.md`, `shopify-connector.md`, `storefront-preview.md`.

## 8. PR 3: developers hold the store permission

**The capability.**

- Add `cap-developer-execute-app-deployables-store` in `dsl/rbac/seeds.memql` beside `:694`.
- Change the compiled mirror `component/auth/rbac_model.go:242` to `{RoleOwner, RoleDeveloper}`. `TestSeedMatchesCompiledMirror` pins the pair.
- Rewrite the owner-only comments at `seeds.memql:684-693`, `rbac_model.go:237-241` and `clients/os/src/apps/deployables/parts.ts:24-28`.

**The consequence, stated.**

- After PRs 2 and 3, any developer may bind any storefront they can write to any store on the cluster. That includes every account-tied client storefront, live ones included, because the site tier's account grant admits staff (developer and above) to write them -- the same reach that already lets a developer pause, archive or delete those sites. D3 accepts this (confirmed by the user, 2026-09-23). Every store row is registered by a cluster owner or server code (D15), so the token references a binding exposes are ones an owner or Connect chose.
- The PR rewrites the invariant the code states today at `component/packages/production.go:370-381` and the refusal text at `component/memql/platform_site_binding_guard.go:125`.

**In the OS,** the part check is data-driven, so the Store slot and panel appear for developers.

- Hide pause and resume (`StorePanel.tsx`) from non-owners: they write an existing store row, which stays owner-only. The register-by-secret-names form (`store/StorePicker.tsx`) is owner-only too: it creates a store row, which PR 1 refuses below owner (D15).
- The copy at `StorePanel.tsx:176, 193` becomes "Only someone holding the store permission can attach one."

**Tests.**

- Database-backed: a developer's `updateSiteStoreBinding` to a readable store succeeds.
- OS: for a developer, the slot and attach are drawn, and register and pause are not.

**Docs.**

- `docs/public/operate/auth/access-model.md`, `shopify-connector.md`, `storefront-preview.md`.
- The two binding mutations' doc comments (`dsl/platform/mutations.memql:621-622, 772-776`), then `make sdk-gen`.

## 9. PR 4: developers can turn storefront packs on and off

**What a storefront pack is.** A pack that declares itself with a new `RegisterStorefrontPack(domain)` in the `dsl` package, called from `wholesalepack.Register` and `reviewspack.Register`.

- A test pins the declared set to `anchor.Domains()` (`packs/anchor/anchor.go:51`).
- The pack's default plays no part. A future pack that ships disabled for safety must not become developer-flippable silently.

**Authorization.**

- `AuthorizeSetPackEnabled(ctx, packDomain)` (`component/memql/module_registry.go:166`) passes an owner as today.
- Otherwise it requires two things: the caller holds `execute app:cluster/modules` (a new seed on owner and developer, with its mirror), and the pack is a storefront pack.
- Other packs stay owner-only. That covers `referencepack` and the other example packs, and runtime-mounted product domains such as `fylo`.

**Reading the list.** `AuthorizeModuleRead` asks `auth.CapableFor(ctx, subject, "read", "app:cluster/modules")`, now seeded on developer (beside `dsl/rbac/seeds.memql:327-329`), so the engine and the OS registry read one rule.

**The write.**

- It moves into `component/memql`: a new method runs `setPackEnabled` under `auth.ContextWithInternalOrigin` after the check, with the actor kept so provenance stays the person.
- `component/grpc` may not stamp internal origin (`call_origin_conformance_test.go:657`).
- The `packState` tier stays `clusterOwner`, and PR 1's rank gate keeps a direct `mutation setPackEnabled` owner-only.

**The internal-origin allowlist.** Extend the `component/memql` reason with the pack flip, marked request-derived and downstream of `AuthorizeSetPackEnabled`. Add a precondition test that the engine is not reached when the check refuses. The model is `integrations/identity/internal_origin_precondition_test.go`.

**What each person may flip.**

- `ModuleInfo` gains an optional `mayFlip`, computed by the same function.
- It flows through `component/grpc/memql.proto`, the Go and TS SDKs and the OS rows.
- `clients/os/src/apps/cluster/modules/ModuleDetail.tsx` draws the switch only where `mayFlip` is true.

**Unchanged.**

- Every attempt is one audit event carrying the actor's role.
- A flip takes effect at each node's next start.
- **No attention marker:** developers reach Modules when they come to turn on a storefront's packs, which the runbook names.

**Tests.**

- `TestAuthorizeModuleRoles` gains developer cases: read allowed; storefront flip allowed; other flip refused; admin refused.
- Database-backed: the internal write lands for a pack with **no** `packState` row, and a developer's direct mutation on a pack that already HAS a row is refused by the write guard.
- OS: the switch appears on `reviews` and not on `referencepack`.
- The proto and SDK parity checks.

**Docs.** `docs/public/concepts/modules.md`, `docs/public/operate/auth/access-model.md`, the mutation doc plus `make sdk-gen`.

## 10. PR 5: storefronts deploy as drafts until connected

### Deploy (engine)

In `component/packages/stages.go`, the branch `case bound == ""` (`:587-593`) records a non-fatal note instead of refusing.

- The code stays `deployable_store_unknown`, and `storeId` stays empty.
- `EnsureSite` then creates a draft with no binding, which it already does when no store id is given (`production.go:456-470`).
- This applies to a first deploy and to any redeploy of an unattached site.
- **The note:**

  > deployable %q names store %q, and this cluster has no store by that name that you may read, so the storefront is not attached to a store. It cannot go live until a store is connected on its Store panel.

**Unchanged:**
- the analysis-time refusal of a storefront whose manifest names no store (`component/packages/analyze.go:125-128`)
- the re-point to the manifest's store once it is readable (`stages.go:630-647`)

**Flipped tests:** `component/packages/store_binding_test.go:86, 299`.

### Go-live rule (engine)

`SiteGoLiveRefusal` (`component/memql/site_preview_rules.go:140`) is the one rule behind both the write guard (`platform_site_preview_guard.go:223-235`) and the readiness answer (`integrations/sitepreview/integration.go:209`).

**Carrying the token fact.**

- `PreviewBoundStore` (`site_preview_rules.go:94-106`) gains `HasStorefrontToken`, meaning `storefrontTokenRef` is non-empty.
- It is populated in both builders: `boundStoreFacts` (`platform_site_preview_guard.go:59-105`, which reads the store as a synthetic operator), and `integrations/sitepreview/integration.go:275-280`, from the `StorefrontTokenRef` it already reads.

**The rule.** It refuses going live, and promoting a candidate, for a `shopify_storefront` in either of two cases:
- the serving binding is empty
- the bound store has no Storefront token

Both use a new code, `storefront_not_connected`, with the remedy "Connect Shopify on the Store panel".

**Code to change.**

- Rewrite the comment at `:120-138`, which today says an unbound storefront is allowed.
- Update the `readableStore` helper (`site_preview_rules_test.go:18-20`) so live-store cases carry a token.
- Flip `site_preview_rules_test.go:65, 74, 184-199` and `platform_site_preview_guard_test.go:466-477`. The latter's detach-plus-go-live delta now expects `storefront_not_connected`.
- Add a test that the readiness answer and the write guard give the same refusal for the same row.

**Unchanged:**
- the development-store refusal
- detaching the store from, or re-binding the store of, a site that is already live (section 15)

### MemQL OS

**Go live on the deployable page.**

- Readiness is fetched (`page/DeployablePage.tsx:166`) and parsed (`preview/rows.ts:177-203`), but only `hasCandidate` and `canPromote` reach the bar (`:181-183`).
- Add `canGoLive` and `goLiveRefusal` to the `preview` prop (`page/acts.ts:128`).
- Go live is offered only when `canGoLive`. Otherwise the refusal sentence and an "Open the store" action appear.
- This also fixes today's development-store case.

**Go live in the compose flow.** AND readiness into `canGoLive` (`page/ComposePage.tsx:672`).

**Copy.**

- `preview/PreviewSection.tsx:424-427` lists `storefront_not_connected` among the store-shaped codes.
- The unattached Store slot reads **Not connected**.
- `packages/refusals.ts` rewrites the `deployable_store_unknown` copy so it is true for an unattached storefront.

**Docs.** `docs/public/operate/packages.md:75-77, 167`, and the go-live passage in `docs/public/operate/deployables.md`.

## 11. PR 6: only server code writes connect states

**The hole.** `v1:identity:githubConnectState` (`dsl/identity/concepts.memql:884-896`) declares no row tier.

- The write guard returns nil for an undeclared concept (`rowauthz_write_guard.go:181-186`).
- The raw `insert(...)` literal is public language that bypasses a mutation's `@serverOnly` (`component/memql/parser.go:88-101`; `docs/public/language/memql.md`, Mutations).
- So any signed-in caller can plant a state row naming any user, purpose, return path, and in PR 7 site and shop.
- **Today this already reaches GitHub App setup.** A planted `app_setup` state naming the owner passes the callback's "still an active cluster owner" check, because that check reads the planted user id.

This was established from code reading, not run. The PR's first test proves it.

**The fix.**

- A guard beside the existing ones in `executeWrite` refuses any write to `v1:identity:githubConnectState` that does not carry internal origin. Its shape follows `validateIdentityCredentialActorScope` (`component/memql/identity_credential_actor_validation.go:93`).
- `component/identity/store_githubconnect.go` already stamps internal origin on every legitimate write (`:241-245`, `:326-336`).

**Tests.**
- A raw `insert` and a direct `createGithubConnectState` by a signed-in user are refused.
- The GitHub Connect and App-setup flows still pass their existing tests.

## 12. PR 7: Connect Shopify, the engine

### 12.1 Which store

The shop is never taken from the browser. Every action below takes a `siteId`, and the server resolves the store in one exported function in `integrations/shopify`:

1. Read the site under the caller. Refuse `site_not_writable` unless the caller can write it, and `not_a_storefront` unless its kind is `shopify_storefront`.
2. Take `binding.store` from `report.deployables[name == site.packageDeployableName]` of the most recent `v1:platform:packageDeployment` for `site.packageId` whose outcomes carry this site's id. That is the run that last published it (`component/packages/report.go:94-112`). No such run answers `store_not_named`.
3. `NormalizeShopDomain` (`integrations/shopify/urlsafety.go:43-60`) gives the shop, and `StoreIDForDomain` (`config.go:89-100`) the store id.
4. Read the store row by id under `operatorContext` (`integrations/shopify/connector.go:117`), never the caller's actor, never `connectorContext`, and never through `StoreRegistry`. Refuse `store_redacted` if `redactedAt` is set.

### 12.2 `shopifyConnectStatus(siteId)`

- An `@sdk` builtin with `@requiresCapability("execute", "app:deployables/store")`, running 12.1.
- **Returns:** `{reason, storeId, shopDomain, appSaved, pendingApp, connected, storefrontTokenSet, requiredScopes, grantedScopes}`.
  - `appSaved`: the store has an `appClientId`, or pending credentials exist.
  - `connected`: the store has an `adminTokenRef`.
- The Store panel's state is read from this, never from the binding.

### 12.3 Saving the app credentials: `shopifyStoreAppSave(siteId, clientId, clientSecret)`

The same capability and 12.1 resolution.

**Pending only (D12).** The call writes the client ID as the globalVariable `SHOPIFY_<ID>_PENDING_CLIENT_ID` and seals the secret as the globalSecret `SHOPIFY_<ID>_PENDING_CLIENT_SECRET`.
- It never touches the store row, the live `appClientId` or the live webhook secret.
- Credentials change a store only after a successful Shopify approval (12.6), so the caller must be staff on that shop. Saving proves nothing about the shop, so it can move nothing that verifies webhooks.

**How it seals.**
- `seedSecret` (`config.go:168-189`) gains `description` and `addedBy` parameters. The save passes a description naming the Store panel and the caller's user id.
- It writes at the existing row's id when one row carries the name, and refuses `secret_name_ambiguous` when more than one does. The resolver takes the first row by name (`component/memql/engine_variables.go:129-150`).
- It runs under `operatorContext`.

**Audit.** One event:
- category `configuration`
- target `shopifyStore`
- action `shopify_app_saved`
- actor the person
- no value or fingerprint

### 12.4 Starting: `shopifyConnectBegin(siteId, returnPath)`

- The same capability and 12.1 resolution.
- **Credentials used:** the pending ones if they exist, otherwise the store's current `appClientId` and webhook secret. With neither it answers `shopify_app_not_saved`.
- **The state.** It mints 32 random bytes and writes the digest through `componentIdentity.Store.CreateGithubConnectState`, which stamps internal origin itself (`store_githubconnect.go:219-259`), so the handler stamps nothing. The row carries:
  - purpose `shopify_connect`
  - the person's `userId`
  - `returnPath`, cleaned by `SafeRelativeRedirect`
  - a 10-minute expiry
  - new optional fields `shopDomain`, `siteId`, `clientId` and `credentialSource` (`pending` or `current`)
- **Where the fields change:**
  - the concept, including the `purpose` description (`dsl/identity/concepts.memql:891`)
  - `createGithubConnectState` (`dsl/identity/mutations.memql:3296-3319`)
  - `githubConnectStateFull` (`dsl/identity/shapes.memql:588-600`)
  - in `store_githubconnect.go`, the seed, create query, row type and getter (`:155-174, 202-249, 346-362`)
  - `make concept-snapshot`
- **Answers:** `{authorizeUrl, reason}`, where `authorizeUrl` is:

  ```
  https://<shop>/admin/oauth/authorize?client_id=<clientId>&scope=<section 12.8, comma-separated>&redirect_uri=https://identity.<domain>/auth/shopify/callback&state=<state>
  ```

  It sends no `grant_options[]`, so the token is offline.
- **Where it lives:** in `integrations/shopify`, beside 12.3, sharing the 12.1 resolver.

### 12.5 Pasting a Storefront token: `shopifyStorefrontTokenSet(siteId, token)`

The same capability and 12.1 resolution.

**It refuses:**
- `store_in_use`, unless the caller is a cluster owner or can write every site bound to this store (serving or preview, read under `operatorContext`)
- an empty token while a live site is bound

**It checks a non-empty token** with one Storefront API request (`{ shop { name } }`) to the store's domain, then seals it as `SHOPIFY_<ID>_STOREFRONT_TOKEN` and sets `storefrontTokenRef`.

**An empty token** clears the reference, so the next Connect mints one.

**Audit:** `shopify_storefront_token_set` or `shopify_storefront_token_cleared`, as in 12.3.

### 12.6 The callback: `GET /auth/shopify/callback` on the identity node

**Where it is mounted.**

- In `component/identity/http/server.go` `Mount`, beside `/auth/github/callback` (`:335`), with the same `wrap` (system actor plus security headers).
- No front-door change: the identity host routes `/` to identity in the cloud (`cmd/frontdoorhosts/manifest.go:310`) and locally (`deploy/k8s/overlays/local/front-door.yaml:50-57`).
- It declares no `*Paths()` in `component/server`, which would publish it on `api.<domain>`.
- It adds a row to CLAUDE.md's HTTP exceptions table (D2).

**The checks, in order.**

1. **HTTPS only.** A plaintext request gets `requireSecureRequest`'s 403 (`component/identity/http/pair.go:42-68`).
2. **The install-link landing.** A request with no `code` and no `state` is Shopify opening the App URL.
   - Redirect to `https://os.<domain>/?connect=deployables&shopify=installed`.
   - Echo no request value, log `shop` as unverified, and write nothing.
3. **Look up the state** by digest **without spending it** (`LookupGithubConnectState`). An unknown, wrong-purpose, spent or expired state ends as `connect_state_invalid`.
4. **Verify the HMAC** with the client secret the state row's `credentialSource` names for its `shopDomain`.
   - Remove `hmac`, sort the rest, and join them as `key=value` with `&`.
   - HMAC-SHA256, then compare with `hmac.Equal` on the hex-decoded bytes.
   - A mismatch ends as `signature_invalid`. A forged request therefore cannot burn a real state.
5. **Check the shop.** `shop` must pass `NormalizeShopDomain` and equal the row's `shopDomain`, else `signature_invalid`.
6. **Spend the state** under the advisory lock with `ConsumeGithubConnectStateFor(..., "shopify_connect")`, which re-checks all four conditions inside the lock (`store_githubconnect.go:310-344`).
7. **Re-derive, don't trust.**
   - The row's `siteId` must still name a `shopify_storefront`.
   - 12.1 run for that site must still give the row's `shopDomain`.
   - Otherwise `connect_state_invalid`.
   - With PR 6, only server code can have written the row, so its `userId` is the person who pressed Connect.
8. **Re-check the person, now.**
   - Read their `v1:identity:user` row. It must be active.
   - Build their subject from the row's real role: `auth.ContextWithAccess` with `AccessContext{UserId, Role: <user.role>}`, **not** Unranked, with claims matching. The shape follows `platform_site_preview_guard.go:76-82`.
   - `auth.ContextWithUserActor` must not be used: it stamps a synthetic Unranked writer (`component/auth/access_context.go:185-207`) that holds no store capability and ranks below the read floor.
   - Refuse `permission_lost` unless all of these hold under that subject: `execute app:deployables/store`, the store reads back, and the site reads back writable.
9. **Exchange the code.**
   - A form-encoded `POST https://<shopDomain>/admin/oauth/access_token` with `client_id`, `client_secret` and `code` in the body, never the URL. The host comes from the state row.
   - Read at most 1 MiB, and never put the body in an error.
   - Failure ends as `exchange_failed`, audited as that fixed token, never `err.Error()`. This follows `githubconnect.Client.ExchangeCode` (`component/identity/githubconnect/client.go:117-172`).
10. **Check the scopes.** Refuse `scopes_missing` only when a section 12.8 Storefront scope is missing. Record every granted scope in `scopesGranted`.

**Then the writes,** all under `operatorContext` and keyed by the store id.

11. **Seal** `SHOPIFY_<ID>_ADMIN_TOKEN`. If `credentialSource` is `pending`:
    - promote the pending secret to `SHOPIFY_<ID>_WEBHOOK_SECRET`, the name the first-boot seed uses for the same value (`config.go:142`)
    - clear both pending rows by overwriting them with blanks, following the GitHub removal pattern (`component/identity/store_githubapp.go:166-194`)
12. **Create or update the store row.**
    - `createStore` if absent (12.1's read under `operatorContext`), otherwise `updateStore`. `createStore` is never re-run on an existing row, because it re-stamps status, scopes and health (`dsl/shopify/overlay/mutations.memql:7-44`).
    - **Fields:**
      - `domain`, and `appClientId` when promoted
      - `adminTokenRef`, and `webhookSecretRef` pointing at the webhook-secret row
      - `apiVersion = generated.APIVersion` and `scopesGranted`
      - `plan = shop.plan.publicDisplayName`, from a one-field Admin query `{ shop { plan { publicDisplayName } } }`
    - `ownerUserId` is set to the person **only when the row has none**, and is never overwritten. It decides whose Library receives `customers/data_request` exports (`dsl/shopify/overlay/concepts.memql:62-72`).
13. **The Storefront token.** Only when `storefrontTokenRef` is empty:
    - mint one with `storefrontAccessTokenCreate(input:{title:"MemQL storefront"})` through `AdminClient.Do` (`integrations/shopify/admin.go:202-229`) with `userErrorsFrom`
    - then seal `SHOPIFY_<ID>_STOREFRONT_TOKEN` and write `storefrontTokenRef` straight away
    - a failure between the mint and the seal can leak one token toward Shopify's cap, and is logged
    - failure to mint ends as `storefront_token_failed`, with the Admin connection already saved, so pressing Connect again retries only the mint
14. **Attach the store to the site** with `updateSiteStoreBinding`, under the step 8 subject.
    - The capability gate and the binding guard judge the person; PRs 2 and 3 are what let a developer pass.
    - It is a no-op when a deploy already attached it.
    - The site stays a draft.
15. **Register webhooks** with `EnsureSubscriptionsForStore`, then `recordSubscriptionHealth` and `StoreRegistry.Invalidate`. A failure does not undo the connection; it shows in the panel's subscription health.
16. **Audit.**
    - category `configuration`, target type `shopifyStore` (in the enum at `dsl/identity/concepts.memql:81`)
    - action `shopify_connected` or `shopify_reconnected`; a refusal is `shopify_connect_refused` with its fixed reason token
    - no token, secret or response body
17. **Redirect.** A 303 to `https://os.<domain><returnPath>` with `shopify=<result>&site=<siteId>`, both query-escaped.
    - The URL is composed by a sibling of `identity.GithubReturnURL` (`component/identity/github_return.go:34-48`).
    - With no resolved state, it goes to `/?connect=deployables&shopify=<result>` with no site.

**Result tokens:** `connected`, `reconnected`, `installed`, `connect_state_invalid`, `signature_invalid`, `permission_lost`, `exchange_failed`, `scopes_missing`, `storefront_token_failed`. They are catalogued in `component/packages/refusal.go` and the OS `packages/refusals.ts`. The new file joins `identityRaiseSites` (`refusal_identity_parity_test.go:64`).

### 12.7 How the identity node reaches the Shopify code

`component/identity` is its own Go module, below `integrations`, so `component/identity/http` cannot import `integrations/shopify`.

- Steps 4, 5, 7, 8 and 9-15 go behind a Go interface field on the identity HTTP `Server`, implemented in `integrations/shopify`.
- It is wired in `app/integrations_identity.go` the way `OIDCSignIn` and `OnUserProvisioned` are (`:244-267`), from `a.engine.IntegrationByName(shopify.ConnectorName).(*shopify.Integration)` as `app/integrations_shopify.go:45` does.
- The identity node loads the Shopify plug-in (`app/plugins_core.go:110`).
- It is not a builtin, because a builtin is client-callable and cannot be `@serverOnly`.

### 12.8 Scopes

One list in code, returned by `shopifyConnectStatus` and printed in the runbook.

**Storefront scopes, required.** Without these, Connect refuses. They are derived from the Storefront API calls in `memql-fylo/clients/storefront/src/lib`:

| Scope | Covers |
|---|---|
| `unauthenticated_read_product_listings` | Products, collections, search, predictive search, product metafields; also required for the token mint |
| `unauthenticated_read_checkouts` | Cart reads |
| `unauthenticated_write_checkouts` | `cartCreate`, `cartLinesAdd/Update/Remove`, `cartDiscountCodesUpdate`, `cartNoteUpdate`, `cartAttributesUpdate`, `cartBuyerIdentityUpdate` |
| `unauthenticated_read_customers` | The trade buyer-context reads, `@inContext(buyer:{customerAccessToken})` |

A test pins the list.

**Admin read scopes:** `generated.Scopes` (`integrations/shopify/generated/model.go:211-245`), for the mirror and webhooks. They are requested and recorded, but a missing one does not block Connect. The panel shows what is missing.

**Not requested:** write scopes for wholesale provisioning (section 15).

### 12.9 Cross-node behavior

- **Direct reads.** Begin runs on the bff and the callback on identity. Both read the store and the pending credentials directly under `operatorContext`, never through a `StoreRegistry`, and the callback calls `Invalidate()` before its writes.
- **Edge caches.** Store rows broadcast (`component/node/routing.go:427-428`), so every node's edge cache flushes on the write.
- **Other nodes' registries** can hold a stale row for up to 30 seconds (`integrations/shopify/store.go:107`). A webhook arriving in that window can 404, and Shopify retries. This is accepted.

### 12.10 The internal-origin allowlist

- Rewrite the `integrations/shopify` reason (`call_origin_conformance_test.go:448`) to name the request-derived `operatorContext` writes in 12.3, 12.5 and 12.6, each downstream of the capability and site checks.
- Add `integrations/shopify/connect_precondition_test.go`, asserting no write is reached when a check refuses.

### 12.11 Docs and generated files

- **New runbook:** `docs/public/operate/shopify-connect.md`. It covers the one-time Shopify app setup, the Store panel, every result token with its repair, and reconnecting.
- **Existing docs:** `docs/public/operate/shopify-connector.md` step 3 points to Connect.
- **CLAUDE.md:** the HTTP exceptions row.
- **Generated files:** `make sdk-gen` for the four builtins, and `make concept-snapshot` for the state fields.

### 12.12 As built (PR 7)

Where the built engine says more than 12.1 to 12.11, or differs:

- **Step 8 asks the store part twice:** the person's own, and at the storefront's organization (memql#5598), through the engine's `MayChangeStoreBinding` with the site treated as bound to nothing, so a reconnect cannot skip it. It is the question the attach's two guards ask.
- **Step 8 on a first Connect.** With no store row to read back, step 8 asks the store's read tier instead (`MemQLEngine.MayReadConcept`, which answers the `clusterOwner` tier and its read floor without a row). Without it, a writer granted the store part passed step 8, and the Admin token, the promotion, the store row and the Storefront token were all written before the attach refused them.
- **`MayWriteRow`** (`component/memql/rowauthz_may_write.go`) answers the row-authz write guard without writing. 12.1's first check and step 8 ask it. The organization boundary is a separate question, which step 8 asks through `MayChangeStoreBinding`.
- **D16, session binding.** Begin records the caller's browser session (the `sid` claim) as the state's `sessionId`, and refuses a call that has none. The callback proves the same live session with GitHub Connect's own check, `githubSessionMatches`, reading the HTTP-only refresh cookie. It runs in step 3, after the lookup and before the signature and the spend. A callback without that session is `connect_state_invalid`, audited as `session_invalid`, and spends nothing. No PKCE: Shopify's authorization code grant takes none, so the state carries no verifier.
- **D12, the secret that is promoted** is the one the code was exchanged with, carried from step 9 to step 11 on the grant (hidden from `String()` as the token is). The pending row is not read again at write time. The pending pair is cleared only if it still names the state's client ID and that secret, compared as SHA-256 digests. A Save made during the approval stays pending.
- **Step 15 runs after the redirect,** in the background with a detached context and a ten-minute timeout. Each subscribed topic is one paced Admin call, and a browser is waiting on the redirect.
- **The audit of a partial result.** Once steps 11 and 12 have landed, the write reports that it kept a connection. The audit is then `shopify_connected` or `shopify_reconnected` (a success), even when the mint or the attach failed afterwards, and `detail.reason` names the step that failed. `shopify_connect_refused` means nothing was kept.
- **`appSaved`** in 12.2 uses Begin's own test: pending credentials, or a current `appClientId` whose webhook secret row exists. An app ID with no secret to verify with is not saved.
- **Reason codes beyond the spec,** answered by the builtins and catalogued with the rest:
  - `app_credentials_invalid`: a Save missing the client ID or secret, or with one too long
  - `store_not_connected`: a token pasted for a store with no row
  - `storefront_token_required`: the empty token refused while a live storefront is bound (12.5 describes this refusal without naming it)
  - `storefront_token_invalid`: the `{ shop { name } }` check failed
  - Begin also answers `connect_state_invalid` when the state row cannot be stored, as GitHub Connect's begin does.
- **`sitesBoundToStore`** (`dsl/platform/queries.memql`) is a new query: every site whose serving or preview binding names the store. 12.5's `store_in_use` reads it under `operatorContext`.
- **`publishedStoreName` reads the newest 50 runs** of the package, one page of `packageDeployments`. A storefront last published further back answers `store_not_named` until it is redeployed.

## 13. PR 8: Connect Shopify, MemQL OS

The Store panel (`clients/os/src/apps/deployables/store/StorePanel.tsx`) reads its state from `shopifyConnectStatus`:

| State | Shows |
|---|---|
| Not connected, no app saved | What Connect does; the required scopes, copyable; Client ID and Client secret fields; **Save** |
| Not connected, app saved | **Connect Shopify** as the one primary action; "Change app details" |
| Connected | The shop, status, granted scopes and any missing mirror scopes; **Reconnect**; "Change app details" (saved as pending, taking effect on the next approval); an optional "Use my own Storefront token" field |

**Replacing the picker.** For a storefront, this panel replaces the auto-opened `StorePicker` (`StorePanel.tsx:110`). The picker stays behind "Change store" for owners.

**The hook.** A new `store/useShopifyConnect.ts`, shaped like `sources/useGithubConnect.ts:93-127`, calls begin, then `window.location.assign(authorizeUrl)`, and blocks a second click while busy.

**The return.**

- `sources/connectReturn.ts` learns the `shopify` and `site` parameters, captured in `main.tsx` before React.
- With a `site`, the dispatcher opens Deployables on that deployable's Store panel. `openRequest` gains `detail`, and `DeployablePage` gains `initialDetail`.
- Without one, the sentence shows over the Deployables list.
- The result is shown once, and only these parameters are removed with `replaceState`.

**Interface rules** (`clients/os/DESIGN.md`):

- One primary per head.
- An act that is not legal is absent, not disabled.
- No attention marker: the button sits where an unattached storefront already sends people.

**Tests:**

- the three states from status
- begin and redirect
- the return opening the right panel and removing only its own parameters
- copy for every result token

## 14. Security summary

- **The shop comes from the server,** never the browser (12.1).
- **Credentials change only on approval.** A store's credentials change only after Shopify approval by the shop's own staff (12.3 and 12.6 step 11).
- **Callback order.** The callback verifies Shopify's signature before spending the state, re-derives every stored field, and judges the person by their real role at the moment of the callback (12.6).
- **The browser that began finishes (D16).** The state is bound to the browser session that pressed Connect, and a callback without that live session spends nothing (12.12).
- **Secrets stay hidden.** They never appear in URLs, logs, audit events or errors. Exchanges use the request body, and failures are recorded as fixed tokens.
- **The privacy-export owner never moves:** `ownerUserId` is never overwritten.
- **Two security holes close first:** creates on store and pack records by anyone but an owner or server code, by any path including the raw `insert(...)` literal (PR 1), and forged states (PR 6).
- **An accepted consequence (D3):** any developer holding the store part can bind any storefront they can write, client storefronts included, to any store (section 8).

## 15. Out of scope

Each item is proposed as its own task:

- **The binding guard judges the merged payload** (`executor_mutation.go:914`, then `:1317`). After PR 2 it no longer blocks developers. It still blocks admins, writers, and the auto-deploy feed's rankless writer, so an armed source cannot republish a storefront once it is connected.
- **The preview guard's presence checks** assume a delta while receiving the merged payload. This may misclassify ordinary deploys as promotions after a candidate was cleared. Inferred, not run.
- **Fatal publish-stage refusals still arrive after the roll** (`stages.go:610, 655-660`). PR 5 removes only the one that hit this deploy. Moving the store and hostname checks ahead of `staging_dsl` is its own task.
- **Shopify builtins with no gate:** `shopifyStoreHealth`, `shopifyEnsureSubscriptions`, `shopifyRunComplianceJobs`.
- **The same create hole on every other `@rowAuthz(clusterOwner)` concept.** The write guard judges no create, so a raw `insert(...)` by any signed-in caller creates a row on any cluster-owner-tier concept that has no create guard of its own. PR 1 closes it for the two concepts this epic needs; an engine-wide fix has to sweep who legitimately creates each such concept first, and is its own task (memql#5624).
- **The secret lookup interpolates the name unescaped** (`readNamedRowFields`, `component/memql/engine_variables.go`): memql#5625.
- **The edge publishes whatever secret `storefrontTokenRef` names,** with no check that it is a Storefront token. D15 keeps the writer an owner or server code, but the edge should not depend on that: memql#5626.
- **`setPackEnabled` as `@serverOnly`,** so an owner's direct flip goes through the audited path too. Below owner, PR 1 already refuses the direct call.
- **Secret naming.**
  - Shopify's seed uses `sec-<slug>` ids where every other sealer uses `secret-global-<slug>`.
  - The store id's hyphen stays in secret names (`SHOPIFY_FYLO-9423_...`), while Fylo's runbook says `_`.
- **Untiered `globalSecret` writes.** Already filed separately. PR 7's `secret_name_ambiguous` refusal limits, but does not close, the resolver ambiguity it causes.
- **The first-boot seed writes under `connectorContext`** (`config.go:132`), which the store concept refuses (`connector.go:64-68`).
- **The misleading OS stage label:** a refused run is shown stopped at Roll when Roll finished (`clients/os/src/apps/deployables/page/rail.ts:448`).
- **The wholesale form drops Fylo's 14 extension fields,** because the cloud cluster's edge forwards shopper forms to the engine `bff`, which does not mount Fylo's DSL. This is a `memql-znas` config change.
- **Development stores through Connect,** with preview binding.
- **Wholesale write scopes and provisioning.** The `shopifyB2B` approval cannot work under any permission, because the approval automation runs as a reader.
- **A site that is already live:** detaching its store, or re-binding it to another store.

## 16. Testing

**Automated, in each PR.**

- Run `make test`, never `go test ./...`, plus `make sdk-gen-check`, `make concept-snapshot-check` and `make proto-gen-check` where they apply.
- Database-backed tests use `MEMQL_REQUIRE_DB=1` against a real Postgres with TimescaleDB and pgvector. The k3d port 5432 accepts and then drops connections, so tests would skip silently.

**PR 7 against a fake Shopify.** Hosts are fields so `httptest` can serve them, as in `integrations/shopify/harness_test.go` and `component/identity/githubconnect/client.go`. The tests are modelled on `component/identity/http/github_callback_test.go`:

- one store and binding per connect; a reconnect updates in place
- pending credentials promoted only after a successful exchange
- a Save never changes a connected store's live credentials
- expired, replayed, unknown, wrong-purpose and planted states write nothing
- a bad HMAC, or a `shop` differing from the state, writes nothing and leaves the state unspent
- a failed exchange or mint writes nothing more
- no token or secret in any statement, audit event, log line or URL
- a person demoted between begin and callback gets `permission_lost`
- the attach succeeds for a developer and is refused for an admin
- the install landing
- plaintext refused
- the Storefront token is minted only when the store has none
- `ownerUserId` is never overwritten

**The hop test** (CLAUDE.md, multi-node). In-process with a shared database:
- begin through engine A's builtin
- then the callback through an identity server wired to a separate engine B
- assert the store row, the binding and the spent state
- a Save on A less than 30 seconds before the callback on B must still succeed

The model is `test/clustere2e/magic_link_replica_test.go`, as an optional clustere2e lane.

**OS:** vitest in `clients/os/test`.

**Baseline in this worktree (2026-09-23).**

- A fresh worktree needs `make identity-tailwind` and a build of `sdk/ts`.
- After that, `clients/os` is 244 of 244 files and 3,570 tests green.
- The engine is green except `scripts/ci`, `scripts/install` and `scripts/lib`, which fail identically on the main checkout: this Intel Mac (darwin/amd64) is unsupported by the local installer, and macOS ships bash 3.2.
- New failures outside those three packages are regressions.

## 17. Local proof, once a Shopify login is available

- Run the section 4 journey as a developer on `make up SERVERS=2` with `make scale N=2`.
- It needs a custom app whose redirect URL is `https://identity.memql.localhost/auth/shopify/callback`, installed on a ZNAS development store if one can be created, otherwise on the live Fylo store.

## 18. Unverified

The first real connection settles these:

- Whether Shopify accepts a `.localhost` redirect URL. If not, the first real connection happens on the cloud cluster after release, and a bug found there costs another PR round.
- Whether a direct `/admin/oauth/authorize` works for a custom-distribution app before its install link was opened. The fallback is the install landing, then Connect.
- How a Dev Dashboard app's managed installation treats our `scope` parameter.
- Whether the minted Storefront token sees the same products as the Headless channel token. The fallback is the pasted-token field.
- Whether the trade buyer-context reads need `unauthenticated_read_customers`, or more.
- Whether custom apps need approval for `read_all_orders`.
- **Webhooks locally.** On the local cluster, webhooks point at `api.memql.localhost`, which Shopify cannot reach and may refuse to create. A failed local subscription health is expected, and step 15 is proven only on the cloud cluster.

## 19. What a developer still cannot do, deliberately

- Create or change a store record directly (PR 1, D15). Both go through Connect.
- Read the mirrored Shopify orders and customers data.
- Change `ownerUserId`.
- Flip non-storefront packs.
- Reach the cluster's master key.

## 20. Rollout

1. Prove the section 4 journey locally (section 17).
2. Check that the eight branches reproduce the integration tree exactly.
3. Open eight draft PRs, each naming its wire additions and lead-developer paths.
4. Jose reviews and merges, and cuts the engine release. The version bump in `memql-znas` rolls it out.
5. On the cloud cluster, as a developer:
   - Run the section 4 journey against the live Fylo store.
   - Then restart the node that answers storefront forms, so the packs take effect, through a small `memql-znas` change, which merges without approvals.

## 21. Sources

- [Authorization code grant](https://shopify.dev/docs/apps/build/authentication-authorization/access-tokens/authorization-code-grant)
- [Distribution methods](https://shopify.dev/docs/apps/launch/distribution/select-distribution-method)
- [App Store requirements](https://shopify.dev/docs/apps/launch/shopify-app-store/app-store-requirements)
- [Expiring offline tokens required for all public apps (changelog, 2026-05-20)](https://shopify.dev/changelog/expiring-offline-access-tokens-required-for-all-public-apps-as-of-january-1-2027)
- [storefrontAccessTokenCreate](https://shopify.dev/docs/api/admin-graphql/latest/mutations/storefrontAccessTokenCreate)
- [Access scopes](https://shopify.dev/docs/api/usage/access-scopes)
- [Webhook HMAC](https://shopify.dev/docs/apps/build/webhooks/subscribe/https)
- [Client credentials grant](https://shopify.dev/docs/apps/build/authentication-authorization/access-tokens/client-credentials-grant)
