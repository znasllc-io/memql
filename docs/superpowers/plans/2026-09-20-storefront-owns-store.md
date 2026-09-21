# The storefront deployable owns its store — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make one record of a Shopify store — `v1:platform:site.binding` references a
`v1:shopify:store` row instead of copying its domain and token reference, a store is
attached through the storefront deployable, the package manifest binds by reference, and
the `app:stores/*` capability set retires into the storefront's own.

**Architecture:** The site's `binding` becomes `{storeId}`. The edge, which already reads
the graph under a synthetic cluster-owner actor, resolves that id to a three-field
projection (`id`, `domain`, `storefrontTokenRef`) at site-resolution time and serves the
CSP and the runtime-config document from it. Binding is a cluster-owner act, gated by a
new Deployables part (`app:deployables/store`) and by a Go guard that refuses a binding
naming a store the actor cannot read. The Stores app is deleted and its function re-homed
on the storefront deployable's own page, where the store is a **connection**, beside the
cluster address, the custom domains and the client.

**Tech Stack:** Go 1.26.1, MemQL DSL, PostgreSQL + TimescaleDB, React + TypeScript
(`clients/os`, Vite + vitest).

**Spec:** `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md`
(section 9, epic 2; gap G4; decisions D5 and D8; section 11 testing).

**Issues:** Closes #5538, #5539, #5540, #5541, #5542. Epic #5530. ONE PR.

---

## Global Constraints

- **Pre-release: no backwards-compat shims or deprecation windows.** When a contract
  changes, both sides change at once and the old path is deleted. The legacy
  `{storeDomain, storefrontTokenRef}` binding gets a MIGRATION, never a fallback read.
- **Test command is `make test`.** `go test ./...` does not reach `component/memql`,
  `component/database` or `component/language`. Db-gated trees:
  `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql`.
- **Stage files by explicit path.** Never `git add -A` / `git add .` — other sessions
  share this working tree.
- **No emojis** anywhere: docs, code, copy, commit messages.
- **Commit format:** `Issue #<N>: <description>`.
- **ONE DSL CHANGE REDS SIX UNRELATED GATES.** Every task that touches `dsl/` runs, in
  this order, before it commits:
  1. `go run ./cmd/memqllint dsl/` — parse plus the load-time contract gates.
  2. `make sdk-gen` then `make sdk-gen-check` — a new CONCEPT FIELD and a new MUTATION ARG
     red it, not only a new construct; it fails in the `go-checks` lane, which does not
     look like an SDK lane. `@serverOnly` constructs are correctly excluded.
  3. `make arch-model` — any Go rename or signature change orphans model edges. Tasks 2,
     3 and 4 all add exported Go symbols, so all three run it.
  4. `component/database/embed_inventory_test.go` — a new MIGRATION `.sql` pair changes an
     embed COUNT. Task 2 adds one; the test names the map to update.
  5. `TestUndeclaredRowAuthzPopulationOnlyShrinks` (`component/memql`) — a shrink-only
     ratchet over queries on concepts declaring no `@rowAuthz` tier. `v1:shopify:store`
     declares `clusterOwner`, so `developmentStoresFor` is fine; do NOT add an exemption.
  6. `make test` — the only command that reaches `component/memql`, where (5) lives.
- **Branch:** `epic/storefront-owns-store` (already created, from `main` at `fe95ee1a6`).
- **The Admin API token reference never reaches the serving path.** `adminTokenRef` and
  `webhookSecretRef` are not projected onto anything `component/edge` holds.
- **Copy rules (`clients/os`):** sentence case, active voice, no ALL-CAPS labels, no
  em-dash-prefixed fragments as labels, no "→" appended to link text. An act keeps its
  name through the whole flow. An absence is an absence (`Figure`), never a zero.
- **DESIGN.md rules that bind this work:** rule 1 (every section opens with the Head),
  rule 5 (one control line), rule 6 (actions are verbs), rule 8 (`Panel` + `Subhead` +
  `Field`), rule 9 (real estate belongs to content), rule 12 (an act that is not legal is
  ABSENT, never disabled).

---

## File Structure

**DSL**
- `dsl/shopify/overlay/concepts.memql` — `store` gains `isDevelopment`, `developmentOfStoreId`, one relationship.
- `dsl/shopify/overlay/mutations.memql` — `createStore` / `updateStore` take the two new fields.
- `dsl/shopify/overlay/queries.memql` — `developmentStoresFor`.
- `dsl/shopify/overlay/shapes.memql` — `storeFull` projects the two new fields.
- `dsl/platform/concepts.memql` — `site.binding`'s `@description` rewritten.
- `dsl/platform/mutations.memql` — new `updateSiteStoreBinding`.
- `dsl/rbac/seeds.memql` — delete 6 `app:stores*` rows, add 1 `app:deployables/store` row.

**Go engine**
- `component/memql/platform_site_binding_guard.go` (new) + test — the readability guard.
- `component/memql/executor_mutation.go` — wire the guard beside the settings guard.
- `component/auth/rbac_model.go` — `appReadFloors` loses 4 entries, `appPartGrants` gains 1.
- `component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.{up,down}.sql` (new).
- `component/conceptfields/concept-fields.snapshot.json` — regenerated.

**Edge**
- `component/edge/resolve.go` — `BoundStore`, `Site.Store`, `StoreByID` on the executor interface, `InvalidateAll`.
- `component/edge/edge.go` — `StoreByID` implementation under `systemActorContext`.
- `component/edge/runtimeconfig.go` — read `site.Store`, not `site.Binding`.
- `component/edge/csp_storefront.go` — read `site.Store.Domain`.
- `component/edge/invalidation_subscriber.go` — `v1:shopify:store` pattern -> `InvalidateAll`.
- `component/node/routing.go` — broadcast `graph.node.*.v1:shopify:store`.

**Packages**
- `component/packages/manifest.go` — `ManifestBinding{Store string}`.
- `component/packages/analyze.go` — resolve the store, refuse when it cannot.
- `component/packages/refusal.go` — `CodeDeployableStoreUnknown`.
- `component/packages/stages.go`, `production.go` — write `{storeId}` on create, re-point on redeploy.
- `component/packages/autodeploy.go` — `bindingWord` reads `Store`.

**MemQL OS**
- `clients/os/src/apps/deployables/store/` (new dir): `health.ts`, `words.ts`, `useStore.ts`, `rows.ts`, `StorePanel.tsx`, `StorePicker.tsx`.
- `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx` — the Store slot.
- `clients/os/src/apps/deployables/page/stops/compose/fields.tsx` + `compose.ts` + `actions.ts` + `ComposePage.tsx` — the picker.
- `clients/os/src/apps/deployables/{rows.ts,parts.ts,composition.css}`.
- `clients/os/src/apps/registry.tsx` — `stores` removed.
- `clients/os/src/apps/stores/` — DELETED.

**Docs**
- `docs/public/operate/shopify-connector.md`, `docs/public/operate/shopify-storefront-checklist.md`, `docs/public/operate/packages.md`.

---

### Task 1: The store row knows a development store (#5539, part 1)

**Files:**
- Modify: `dsl/shopify/overlay/concepts.memql` (the `store` concept, after `redactedAt`)
- Modify: `dsl/shopify/overlay/shapes.memql` (`storeFull`)
- Modify: `dsl/shopify/overlay/mutations.memql` (`createStore`, `updateStore`)
- Modify: `dsl/shopify/overlay/queries.memql` (new `developmentStoresFor`)
- Modify: `component/conceptfields/concept-fields.snapshot.json` (regenerate)
- Test: `component/memql/shopify_development_store_test.go` (new)

**Interfaces:**
- Produces: concept fields `isDevelopment bool`, `developmentOfStoreId string`; query
  `developmentStoresFor(storeId: string!)`; `createStore` / `updateStore` args
  `isDevelopment boolean`, `developmentOfStoreId string`.

- [ ] **Step 1: Add the two fields and the relationship**

In `dsl/shopify/overlay/concepts.memql`, inside `concept store`, directly after the
`redactedAt datetime` field and before the closing `}`:

```memql
  /// True when this store is a DEVELOPMENT store -- the one a storefront is
  /// exercised against before it serves shoppers (design D8). It is attached
  /// and mirrored like any other store: a second row, its own storeId, and
  /// every mirrored row it produces already excluded from every read scoped
  /// to the live one. Not a thing this program provisions and specifically
  /// not a Shopify store created with generated test data, which cannot be
  /// transferred to a merchant.
  ///
  /// The flag is on the STORE rather than on the site, because what makes a
  /// store a development store is a fact about somebody else's system, and
  /// because the go-live guard has to be able to ask it of the store a site
  /// names rather than of the site.
  isDevelopment       bool
  /// The live store this development store stands in for, when it stands in
  /// for one. Empty on a live store, and empty on a development store nobody
  /// has paired yet -- an absence that means "not paired", never "live".
  ///
  /// It is what lets a storefront show the development store beside the store
  /// it is bound to: without it two rows sit in one list with nothing saying
  /// which belongs to which.
  developmentOfStoreId  string

  @relationship(type="references", as="developmentOf", field="developmentOfStoreId", target=store, direction="outgoing")
```

- [ ] **Step 2: Project them from `storeFull`**

In `dsl/shopify/overlay/shapes.memql`, add to `shape store storeFull`, after `redactedAt`:

```memql
  isDevelopment
  developmentOfStoreId
```

- [ ] **Step 3: Accept them on both store mutations**

In `dsl/shopify/overlay/mutations.memql`:

`createStore` — add to `args`:
```memql
      isDevelopment         boolean
      developmentOfStoreId  string
```
and to the `stamp` block:
```memql
        isDevelopment:        args.isDevelopment ?? false
        developmentOfStoreId: args.developmentOfStoreId ?? ""
```

`updateStore` — add to `args`:
```memql
      isDevelopment         boolean
      developmentOfStoreId  string
```
and add both names to the existing `accept { ... }` list.

- [ ] **Step 4: Add the named read**

Append to `dsl/shopify/overlay/queries.memql`, directly after `stores`:

```memql
/// The development stores attached to one live store.
///
/// A storefront shows the store it is bound to and the development store it
/// is exercised against; without this read the two rows sit in one list with
/// nothing saying which belongs to which.
@actor
query store developmentStoresFor {
  args {
    storeId  string!
  }
  filter    row => row.developmentOfStoreId == args.storeId
                && row.isDevelopment == true
                && actor.isClusterOwner == true
  sort      "domain", "asc"
  paginate  25
  shape     storeFull
}
```

- [ ] **Step 5: Write the failing test**

Create `component/memql/shopify_development_store_test.go`:

```go
package memql

import (
	"strings"
	"testing"
)

// The development-store fields are the half of design D8 the engine holds
// (epic memql#5530, issue memql#5539): a development store is a SECOND
// v1:shopify:store row, flagged, and paired to the live store it stands in
// for. Epic 3's go-live guard (memql#5546) reads the flag off the store a
// site names, which is why it lives here and not on the site.
func TestTheStoreConceptCarriesTheDevelopmentStoreFields(t *testing.T) {
	c := conceptFieldsFor(t, "v1:shopify:store")
	for _, want := range []string{"isDevelopment", "developmentOfStoreId"} {
		if _, ok := c[want]; !ok {
			t.Errorf("v1:shopify:store declares no %q field; the go-live guard has nothing to ask", want)
		}
	}
	// THE REACHABLE POSITIVE: a field the concept has always had, so a
	// helper that returned an empty map would fail here rather than pass.
	if _, ok := c["domain"]; !ok {
		t.Fatal("read no fields off v1:shopify:store; the helper is not reading the concept")
	}
}

func TestTheDevelopmentStoreReadIsRegistered(t *testing.T) {
	fn := registeredFunctionNamed(t, "developmentStoresFor")
	if fn == nil {
		t.Fatal("developmentStoresFor is not registered; a storefront cannot show its development store")
	}
	if !strings.Contains(fn.Filter, "developmentOfStoreId") {
		t.Errorf("developmentStoresFor does not filter on developmentOfStoreId: %q", fn.Filter)
	}
}
```

If `conceptFieldsFor` / `registeredFunctionNamed` do not already exist in the package,
implement them at the bottom of this file by loading the DSL exactly as the nearest
existing test in `component/memql` does (look at `app_resource_os_parity_test.go`'s
`seededCatalog` for the loader idiom, and at `requires_capability_builtin_test.go` for
reading a registered function). Do not invent a second loader.

- [ ] **Step 6: Run it and watch it fail**

```bash
go test github.com/znasllc-io/memql/component/memql -run 'TestTheStoreConceptCarriesTheDevelopmentStoreFields|TestTheDevelopmentStoreReadIsRegistered' -count=1 -v
```
Expected before step 1-4 are applied: FAIL. After: PASS.

- [ ] **Step 7: Regenerate the concept-field snapshot**

```bash
make concept-snapshot
git diff --stat component/conceptfields/concept-fields.snapshot.json
```
Only ADDITIONS for `v1:shopify:store` are expected. The snapshot refuses a DROP; there is
none here.

- [ ] **Step 8: Commit**

```bash
git add dsl/shopify/overlay/concepts.memql dsl/shopify/overlay/shapes.memql \
        dsl/shopify/overlay/mutations.memql dsl/shopify/overlay/queries.memql \
        component/conceptfields/concept-fields.snapshot.json \
        component/memql/shopify_development_store_test.go
git commit -m "Issue #5539: a store row knows whether it is a development store, and which store it stands in for"
```

---

### Task 2: The binding names a store row (#5538, part 1)

**Files:**
- Modify: `dsl/platform/concepts.memql:219` (the `binding` `@description`)
- Modify: `dsl/platform/mutations.memql` (new `updateSiteStoreBinding` after `updateSiteResolutionTail`)
- Create: `component/memql/platform_site_binding_guard.go`
- Create: `component/memql/platform_site_binding_guard_test.go`
- Modify: `component/memql/executor_mutation.go:1280` (wire the guard)
- Create: `component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.up.sql`
- Create: `component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.down.sql`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: mutation `updateSiteStoreBinding(siteId string!, storeId string)`;
  Go `func (e *MemQLEngine) validateSiteStoreBinding(ctx context.Context, payload map[string]any, actor string) error`.

- [ ] **Step 1: Rewrite the `binding` description**

In `dsl/platform/concepts.memql`, replace the whole `@description(...)` on the `binding`
field with:

```
  binding      object   @description("Per-kind connection to the system this site fronts. Shape is decided by `kind`, and only one kind declares one today: shopify_storefront => {storeId}, which NAMES a v1:shopify:store row (epic memql#5530). It is a REFERENCE, not a copy: the store row holds the myshopify.com domain and the Storefront token reference, and the edge resolves both through it at serve time, so an edit to the store reaches every site bound to it with no second write. It used to carry {storeDomain, storefrontTokenRef} -- the same two values the store row already held, edited in two places at two authorization tiers -- and that duplication is what this field stopped being. Empty for spa and static. The Admin API token reference and the webhook secret reference live on the store row and are never projected onto anything the serving path holds. Written only by createSite and updateSiteStoreBinding, both of which refuse a store the caller cannot read (component/memql/platform_site_binding_guard.go).")
```

- [ ] **Step 2: Add the mutation**

Append to `dsl/platform/mutations.memql`, directly after `updateSiteResolutionTail`:

```memql
/// Point a storefront deployable at the v1:shopify:store row it fronts, or clear the binding
/// (epic memql#5530, issue memql#5538).
///
/// ONE VALUE, AND IT IS A REFERENCE. The binding is written whole as {storeId} rather than merged,
/// so the legacy {storeDomain, storefrontTokenRef} shape cannot survive a write: a read-merge would
/// have kept the copy beside the reference and left two records of one store, which is the thing
/// this epic exists to end. An empty storeId writes an empty object, which is the unbound state --
/// clearing must be expressible, for updateSiteSettings' reason.
///
/// AUTHORIZATION IS TWO GATES, AND THE SECOND IS THE SUBSTANTIVE ONE.
/// @requiresCapability names the surface: `app:deployables/store` is seeded on owner alone, which
/// is exactly the population the retired Stores app admitted. Beside it, a Go guard refuses a
/// binding naming a store row the CALLER CANNOT READ -- so the answer to "who may bind a storefront
/// they own to a store they may not read" is nobody. That check needs a cross-row read no mutation
/// body can make, which is why it sits with the hostname policy rather than here
/// (component/memql/platform_site_binding_guard.go).
@requiresCapability("execute", "app:deployables/store")
mutation site updateSiteStoreBinding {
  args {
    siteId   string!
    storeId  string
  }
  update {
    id:      args.siteId
    binding: { storeId: args.storeId ?? "" }
  }
}
```

- [ ] **Step 3: Write the failing guard test**

Create `component/memql/platform_site_binding_guard_test.go`:

```go
package memql

import (
	"context"
	"strings"
	"testing"
)

// The storefront binding names a store row, and the guard is the answer to the
// question issue memql#5538 asked: who may bind a storefront they own to a store
// they may not read. Nobody.
//
// The capability (`app:deployables/store`, owner-seeded) says who may reach the
// mutation. This says which stores they may name once they have. They are
// different questions: a grant is a grant on an APP PART, and it cannot know
// which rows a cluster holds.

func TestABindingNamingAnUnreadableStoreIsRefused(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{"storeId": "nope"}}
	err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("a binding naming a store the caller cannot read was accepted")
	}
	for _, want := range []string{"nope", "v1:shopify:store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestABindingNamingAReadableStoreIsAccepted(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{"storeId": "acme"}}
	if err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("a binding naming a readable store was refused: %v", err)
	}
}

// CLEARING MUST BE EXPRESSIBLE. An empty object and an empty storeId are the
// unbound state, and neither reads a store.
func TestAnEmptyBindingReadsNoStore(t *testing.T) {
	e := &MemQLEngine{}
	reads := 0
	probe := func(context.Context, string) (bool, error) { reads++; return false, nil }
	for _, payload := range []map[string]any{
		{"binding": map[string]any{}},
		{"binding": map[string]any{"storeId": ""}},
		{"binding": nil},
		{},
	} {
		if err := e.validateSiteStoreBinding(context.Background(), payload, "u1", probe); err != nil {
			t.Errorf("payload %v was refused: %v", payload, err)
		}
	}
	if reads != 0 {
		t.Errorf("an unbound binding read a store %d times; it must read none", reads)
	}
}

// THE LEGACY SHAPE IS REFUSED, NOT IGNORED. Pre-release means no shim: a write
// still carrying the copied domain is a caller that was not migrated, and
// accepting it would leave two records of one store on that row forever.
func TestTheLegacyCopiedBindingIsRefused(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{
		"storeDomain":        "acme.myshopify.com",
		"storefrontTokenRef": "acme-storefront-token",
	}}
	err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("the retired {storeDomain, storefrontTokenRef} binding was accepted")
	}
	if !strings.Contains(err.Error(), "storeId") {
		t.Errorf("the refusal does not say what to write instead: %v", err)
	}
}
```

- [ ] **Step 4: Run it and watch it fail**

```bash
go test github.com/znasllc-io/memql/component/memql -run 'Binding' -count=1
```
Expected: FAIL, `e.validateSiteStoreBinding undefined`.

- [ ] **Step 5: Write the guard**

Create `component/memql/platform_site_binding_guard.go`. The header comment must say what
the guard decides and why it is Go rather than DSL (it is a CROSS-ROW read, which no
mutation body can make — the same reason `platform_package_source_policy.go` gives). The
signature takes an injected reader so the unit tests above need no database:

```go
package memql

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// storeReadable reports whether the calling actor may read the named
// v1:shopify:store row. Injected so the guard's own tests need no database;
// the engine passes canReadStore.
type storeReadable func(ctx context.Context, storeId string) (bool, error)

// bindingStoreIdKey is the ONE key a shopify_storefront binding carries.
const bindingStoreIdKey = "storeId"

// retiredBindingKeys are the two the binding used to copy off the store row.
// They are refused rather than ignored: pre-release means no shim, and a write
// that still carries them is a caller nobody migrated.
var retiredBindingKeys = []string{"storeDomain", "storefrontTokenRef"}

func (e *MemQLEngine) validateSiteStoreBinding(
	ctx context.Context,
	payload map[string]any,
	actor string,
	readable storeReadable,
) error {
	if payload == nil {
		return nil
	}
	raw, present := payload["binding"]
	if !present || raw == nil {
		return nil
	}
	binding, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: binding must be an object naming the store this storefront fronts, {storeId: \"...\"}; got %T",
			raw,
		)
	}

	var retired []string
	for _, key := range retiredBindingKeys {
		if _, found := binding[key]; found {
			retired = append(retired, key)
		}
	}
	if len(retired) > 0 {
		sort.Strings(retired)
		return fmt.Errorf(
			"v1:platform:site: binding carries %s, which the storefront binding no longer holds (epic memql#5530). The store row holds the domain and the Storefront token reference; the binding names it with storeId and the edge resolves both through it.",
			strings.Join(retired, " and "),
		)
	}

	storeId := strings.TrimSpace(stringFromAny(binding[bindingStoreIdKey]))
	if storeId == "" {
		// The unbound state, and it must stay expressible: a storefront can be
		// created before its store is attached, and a store can be detached.
		return nil
	}
	if readable == nil {
		return fmt.Errorf("v1:platform:site: cannot check that %q is readable -- no store reader is wired", storeId)
	}
	ok, err := readable(ctx, storeId)
	if err != nil {
		return fmt.Errorf("v1:platform:site: could not read v1:shopify:store %q to bind it: %w", storeId, err)
	}
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: %q may not bind this storefront to v1:shopify:store %q -- it is not a store this caller can read. A store is cluster-owner-tier; binding a storefront to one you cannot read would publish that store's Storefront token under your own hostname.",
			actor, storeId,
		)
	}
	return nil
}

// canReadStore is the engine's own reader: the named query, under the CALLER's
// actor, deliberately -- the whole point is that the answer is the caller's,
// not the deployment's.
func (e *MemQLEngine) canReadStore(ctx context.Context, storeId string) (bool, error) {
	if !e.canResolve() {
		return false, ErrEngineNotInitialized
	}
	res, err := e.Execute(ctx, fmt.Sprintf("query storeById(storeId: %s)", languageParser.QuoteString(storeId)))
	if err != nil {
		return false, err
	}
	return len(MaterializeRows(res)) > 0, nil
}
```

Note on `langparserQuoteString`: use the same import + call the rest of `component/memql`
uses for quoting a call-string argument (`langparser.QuoteString`) — memory
`quotestring-not-go-quoting-for-call-strings`. Import it under the alias the file's
neighbours use, and delete the placeholder name above.

- [ ] **Step 6: Run the test again**

```bash
go test github.com/znasllc-io/memql/component/memql -run 'Binding' -count=1 -v
```
Expected: PASS.

- [ ] **Step 7: Wire the guard**

In `component/memql/executor_mutation.go`, inside the existing
`if conceptMeta.Name == conceptPlatformSite { ... }` block, directly after the
`validateSiteSettings` call, add:

```go
		// The storefront binding (epic memql#5530, issue memql#5538), beside the
		// four above and for their reason: whether the caller may read the store
		// row the binding NAMES is a cross-row question, and no mutation body can
		// ask it. Without it a site owner could bind their own deployable to any
		// store in the cluster and the edge would serve that store's Storefront
		// token under their hostname. See platform_site_binding_guard.go.
		if err := e.validateSiteStoreBinding(ctx, payload, actor, e.canReadStore); err != nil {
			return nil, meta, err
		}
```

- [ ] **Step 8: Write the migration**

Create `component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.up.sql`:

```sql
-- v1:platform:site.binding stops COPYING the store and starts NAMING it
-- (epic memql#5530, issue memql#5538).
--
-- WHAT CHANGES. A shopify_storefront's binding was {storeDomain,
-- storefrontTokenRef} -- the same two values v1:shopify:store already held,
-- edited in two places at two authorization tiers (gap G4). It becomes
-- {storeId}, and component/edge resolves the domain and the token reference
-- through the store row at serve time.
--
-- WHY A MIGRATION AND NOT A FALLBACK READ. Pre-release means no shim
-- (CLAUDE.md): the edge reads storeId and nothing else, so a row still
-- carrying the old shape would resolve no store, serve no storefront block
-- and name no store in its Content-Security-Policy -- a live storefront going
-- dark, silently, at the moment the engine rolled. Two writers produced the
-- old shape: createSite through the OS compose form, and the deploy pipeline
-- from memql-package.yaml. Both are converted in the same change.
--
-- SCOPED BY CONCEPT, NEVER BY KEY NAME. `binding` is v1:platform:site's, and
-- `storeDomain` is exactly the kind of key a product bundle mounted at
-- MEMQL_DSL_PATH may declare on a concept this repository has never seen.
--
-- THE JOIN IS ON THE ONE IDENTIFIER SHOPIFY NEVER CHANGES. A store row's
-- `domain` is the myshopify.com host, which is what the old binding copied.
-- A site whose domain matches no store row is left ALONE rather than cleared:
-- an unconverted binding is visible (the storefront stops resolving a store
-- and the OS says so), while a cleared one is indistinguishable from a
-- storefront nobody ever bound. The operator repairs it by attaching the
-- store on the deployable.
--
-- APPEND-ONLY ROWS, EVERY VERSION. Idempotent: the guard matches nothing on a
-- cluster whose site rows were all written after this change.

UPDATE "MemoryNodes" AS s
SET payload = jsonb_set(
      s.payload,
      '{binding}',
      jsonb_build_object('storeId', split_part(st.id, ':', 4))
    )
FROM (
  SELECT DISTINCT ON (payload->>'domain') id, payload->>'domain' AS domain
  FROM "MemoryNodes"
  WHERE concept = 'v1:shopify:store'
    AND payload->>'domain' IS NOT NULL
    AND payload->>'domain' <> ''
  ORDER BY payload->>'domain', "createdAt" DESC
) AS st
WHERE s.concept = 'v1:platform:site'
  AND s.payload->>'kind' = 'shopify_storefront'
  AND s.payload->'binding' ? 'storeDomain'
  AND s.payload->'binding'->>'storeDomain' = st.domain;
```

VERIFIED against the shared throwaway database (`docker exec memql-throwaway-laneb psql
-U memql -d memql`; `psql` is not on the host PATH): ids are stored CANONICAL
(`v1:platform:site:site-shop-site-4165002`), so `split_part(id, ':', 4)` is the bare short
id, which is the form `binding.storeId` carries and the form `query storeById` resolves on
inbound. That database also holds a real `shopify_storefront` row whose binding is still
`{"storeDomain": "example-store.myshopify.com", "storefrontTokenRef":
"SHOPIFY_STOREFRONT_TOKEN"}`, which is the row this migration exists for. It holds NO
`v1:shopify:store` rows, so the join matches nothing there — that is the idempotent case,
not evidence the migration works. Say so if you report on it.

Create the `.down.sql`:

```sql
-- Irreversible, and deliberately a no-op rather than a guess.
--
-- The up migration replaces a copied {storeDomain, storefrontTokenRef} with a
-- reference to the row that already held both. Writing the copy back would
-- re-create the duplication this epic removed, and it could only be written
-- back from the store row -- which is to say from the reference, which is to
-- say it was never lost.
--
-- Rolling this back means rolling back the engine version that made the
-- change, at which point component/edge reads the copy again and the rows it
-- reads no longer carry one. That is a version rollback, not a data
-- migration, and the repair is to re-run the older engine's own writers.

SELECT 1;
```

- [ ] **Step 9: Run the migration gates, INCLUDING the embed count**

```bash
go test github.com/znasllc-io/memql/component/database -count=1
go test github.com/znasllc-io/memql/... -run 'TestRetiredConceptFieldsAreMigratedSafely' -count=1
```

`component/database/embed_inventory_test.go` counts the embedded migration files and WILL
fail on this new up/down pair — that is the gate working, and its message names the map to
update. Update it. (No concept FIELD is retired here: `binding` stays declared and only the
SHAPE of the object it holds changes, so the `conceptfields` ledger has nothing to record
and `make concept-snapshot` has nothing to refuse.)

Also run, because this task adds exported Go symbols and a DSL construct:

```bash
go run ./cmd/memqllint dsl/
make sdk-gen && make sdk-gen-check
make arch-model
```

- [ ] **Step 10: Commit**

```bash
git add dsl/platform/concepts.memql dsl/platform/mutations.memql \
        component/memql/platform_site_binding_guard.go \
        component/memql/platform_site_binding_guard_test.go \
        component/memql/executor_mutation.go \
        component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.up.sql \
        component/database/memory-nodes/migrations/20260920120000_site_binding_store_reference.down.sql
git commit -m "Issue #5538: the storefront binding names a store row, and nobody binds a store they cannot read"
```

---

### Task 3: The edge resolves through the store row (#5538, part 2)

**Files:**
- Modify: `component/edge/resolve.go` (the `Site` struct, the `QueryExecutor` interface, `Invalidate` -> add `InvalidateAll`, the resolver's site load)
- Modify: `component/edge/edge.go` (`StoreByID`)
- Modify: `component/edge/runtimeconfig.go` (`storefrontForSite`)
- Modify: `component/edge/csp_storefront.go` (`storefrontSources`)
- Modify: `component/edge/invalidation_subscriber.go` (the store pattern)
- Modify: `component/node/routing.go` (broadcast the store concept)
- Modify: `component/edge/runtimeconfig_test.go`, `csp_storefront_test.go`, `storefront_fixture_serve_test.go`, `edge_test.go`
- Modify: `test/clustere2e/storefront_serving_test.go`
- Create: `component/edge/bound_store_test.go`

**Interfaces:**
- Consumes: mutation and binding shape from Task 2.
- Produces: `edge.BoundStore{ID, Domain, StorefrontTokenRef string}`; `Site.Store *BoundStore`;
  `QueryExecutor.StoreByID(ctx context.Context, storeId string) (*BoundStore, error)`;
  `Resolver.InvalidateAll()`.

- [ ] **Step 1: Write the failing tests**

Create `component/edge/bound_store_test.go`:

```go
package edge

import (
	"reflect"
	"testing"
)

// THE EDGE HOLDS THREE FIELDS OF A STORE AND NO MORE (epic memql#5530, issue
// memql#5538).
//
// The store row also carries adminTokenRef and webhookSecretRef. Neither is
// projected here, and that is the point: the serving path cannot leak a
// credential reference it was never handed. TestRuntimeConfigNeverCarriesThe
// ShopifyAdminToken greps the SERVED document; this greps the STRUCT, one
// rung earlier, so the admin token cannot reach a header or a log line either.
func TestTheBoundStoreCarriesOnlyWhatTheServingPathNeeds(t *testing.T) {
	want := []string{"ID", "Domain", "StorefrontTokenRef"}
	typ := reflect.TypeOf(BoundStore{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BoundStore fields = %v, want exactly %v. A field added here is a field the edge can serve.", got, want)
	}
}

// A STORE EDIT REACHES THE SITE WITH NO SECOND WRITE -- the epic's own test
// (design section 11). The site row is untouched; only the store changed.
func TestAStoreEditReachesTheSiteWithNoSecondWrite(t *testing.T) {
	exec := &storeStubExec{
		site:  storefrontSiteBoundTo("store-1"),
		store: &BoundStore{ID: "store-1", Domain: "before.myshopify.com", StorefrontTokenRef: "ref"},
	}
	r := newTestResolver(exec)

	first, err := r.Resolve(testCtx(), "shop.example.com")
	if err != nil || first == nil || first.Store == nil {
		t.Fatalf("first resolve did not bind a store: site=%v err=%v", first, err)
	}
	if first.Store.Domain != "before.myshopify.com" {
		t.Fatalf("first resolve read %q", first.Store.Domain)
	}

	// The STORE row changes. The site row does not.
	exec.store = &BoundStore{ID: "store-1", Domain: "after.myshopify.com", StorefrontTokenRef: "ref"}
	r.InvalidateAll()

	second, err := r.Resolve(testCtx(), "shop.example.com")
	if err != nil || second == nil || second.Store == nil {
		t.Fatalf("second resolve did not bind a store: site=%v err=%v", second, err)
	}
	if second.Store.Domain != "after.myshopify.com" {
		t.Errorf("after a store edit the edge still reads %q; the site was never rewritten and the edit must reach it anyway", second.Store.Domain)
	}
}

// A BINDING NAMING A STORE THAT IS GONE SERVES NO STOREFRONT BLOCK AND NAMES
// NO STORE. Drop, never guess -- the same discipline validHost already applies
// to a malformed domain.
func TestAnUnresolvableStoreLeavesTheSiteServable(t *testing.T) {
	exec := &storeStubExec{site: storefrontSiteBoundTo("gone"), store: nil}
	r := newTestResolver(exec)
	site, err := r.Resolve(testCtx(), "shop.example.com")
	if err != nil {
		t.Fatalf("an unresolvable store failed the whole resolve: %v", err)
	}
	if site == nil {
		t.Fatal("an unresolvable store made the site itself unresolvable; the bundle must still serve")
	}
	if site.Store != nil {
		t.Errorf("Store = %+v, want nil", site.Store)
	}
	if got := policyForSite(nil, site, func(string) string { return "" }, ""); contains(got, "myshopify") {
		t.Errorf("the policy named a store the edge could not read: %q", got)
	}
}
```

Write `storeStubExec`, `storefrontSiteBoundTo`, `newTestResolver`, `testCtx` and
`contains` at the bottom of this file, modelled on the existing stubs in
`component/edge/edge_test.go` and `resolve_test.go`. Reuse a helper that already exists
rather than adding a second one with a different name.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test github.com/znasllc-io/memql/component/edge -run 'BoundStore|StoreEdit|UnresolvableStore' -count=1
```
Expected: FAIL, `undefined: BoundStore`.

- [ ] **Step 3: Add `BoundStore` and `Site.Store`**

In `component/edge/resolve.go`, replace the `Binding map[string]any` field's doc block and
add the new type. `Binding` STAYS (it is how `storeId` arrives off the row); `Store` is
what everything downstream reads:

```go
// BoundStore is the v1:shopify:store row as the SERVING PATH sees it: the three
// fields it needs and not one more (epic memql#5530, issue memql#5538).
//
// The store row also holds adminTokenRef and webhookSecretRef. Neither is here,
// and the omission is the safety property rather than an economy: a credential
// reference the edge was never handed cannot reach a response header, a served
// document or a log line. TestTheBoundStoreCarriesOnlyWhatTheServingPathNeeds
// pins the field list so adding one is a decision.
//
// It is resolved at SITE-RESOLUTION time, under the same synthetic cluster-owner
// actor the site read uses, and cached with the site. That is what makes "an edit
// to the store reaches the site with no second write" true: the site row is never
// rewritten, and the store's own event flushes the cache.
type BoundStore struct {
	// ID is the bare v1:shopify:store row id -- what binding.storeId names.
	ID string
	// Domain is the myshopify.com host: the Storefront API origin, and the one
	// source admitted into connect-src / img-src / media-src.
	Domain string
	// StorefrontTokenRef NAMES a v1:platform:globalSecret row. The edge resolves
	// it at serve time into the runtime-config document, and that is still the
	// only place it is dereferenced.
	StorefrontTokenRef string
}
```

and on `Site`:

```go
	// Store is the row binding.storeId names, resolved. Nil for every kind that
	// has no binding, for an unbound storefront, and for a storefront whose
	// store cannot be read -- three states the serving path treats alike,
	// because the answer in all three is the same: serve the bundle, serve no
	// storefront block, and name no store in the policy.
	Store *BoundStore
```

- [ ] **Step 4: Add `StoreByID` to the executor and the interface**

In `component/edge/resolve.go`'s `QueryExecutor` interface, add:

```go
	// StoreByID resolves the v1:shopify:store row a storefront binding names.
	// Returns (nil, nil) for a miss -- a store that is gone is not an error,
	// it is a storefront with nothing to talk to.
	StoreByID(ctx context.Context, storeId string) (*BoundStore, error)
```

In `component/edge/edge.go`, implement it beside `SiteByHostname`, under
`systemActorContext` and using `langparser.QuoteString` for the argument, running
`query storeById(storeId: ...)` and projecting exactly `id`, `domain`,
`storefrontTokenRef` through the existing `rowString` / `memql.BareShortId` helpers. Its
doc comment must say that it deliberately does not project `adminTokenRef` or
`webhookSecretRef`.

VERIFIED: the edge binary DOES carry the shopify DSL domain. `dsl/embed.go:54`'s
`//go:embed` line names `all:shopify` and carries no build tag, so every node type embeds
the whole tree and `query storeById` is registered on the edge exactly as on the bff. You
do not need to move the query, mount anything, or add a build-tagged registration.

`storeById`'s filter is `row.id == args.storeId && actor.isClusterOwner == true`
(`dsl/shopify/overlay/queries.memql`), and `systemActorContext` stamps
`auth.RoleOwner`, which is what `AccessContext.IsClusterOwner()` reads. Both the
hand-written conjunct and the concept's `@rowAuthz(clusterOwner)` therefore admit it. Pin
that with a test in the shape of the existing
`TestEngineExecutorRunsUnderASyntheticClusterOwnerActor` (`component/edge/edge_test.go:44`)
— capture the ctx in a fake engine and assert `IsClusterOwner()`. Without it, a StoreByID
that quietly ran under no actor would read zero rows and every storefront would serve no
store, while a stub-driven test kept passing.

VERIFIED insertion point: `resolver.Resolve` in `component/edge/resolve.go` builds the
site inside a `r.sf.Do(key, func() (any, error) {...})` closure that tries three lookups in
order (`SiteByHostname`, then `SiteForCustomDomain`, then `SiteForAccountFrontDoor`) and
then writes `r.cache[key] = entry{site: site, at: time.Now()}`. Add the store resolution
BETWEEN the third lookup and that cache write, so the resolved store is cached with the
site and all three resolution paths get it:

```go
		// THE BOUND STORE (epic memql#5530, issue memql#5538), resolved with
		// the site and cached with it.
		//
		// Here rather than in siteFromRow because it is a SECOND read, and
		// here rather than per-request because the policy is built on every
		// asset response. Caching it with the site is what makes the TTL and
		// the invalidation apply to both: a store event flushes this map
		// (InvalidateAll), which is how an edit to a store reaches every site
		// bound to it without the site row being written at all.
		//
		// A FAILED STORE READ LEAVES THE SITE SERVABLE. The bundle is not the
		// store's to take down: an unreadable store means no storefront block
		// and no store named in the policy, which is what a storefront nobody
		// has bound yet already gets.
		if site != nil && site.Kind == storefrontKind {
			if id := strings.TrimSpace(bindingStoreId(site.Binding)); id != "" {
				store, serr := r.exec.StoreByID(ctx, id)
				if serr != nil {
					// Logged, not returned. See above.
					slog.Warn("edge: could not resolve the bound store", "siteId", site.ID, "storeId", id, "error", serr)
				} else {
					site.Store = store
				}
			}
		}
```

`bindingStoreId(map[string]any) string` is the one accessor that replaces `bindingString`;
put it in `runtimeconfig.go` where `bindingString` was, with a comment saying the binding
carries exactly one key now. Use the package's existing logger idiom rather than a bare
`slog` import if `component/edge` already threads one (check `handler.go`'s `Options`).

Real type names in that file, so you do not guess: the cache is
`cache map[string]entry` and its value type is `type entry struct { site *Site; at time.Time }`.
`Resolver` is an INTERFACE (`Resolve`, `Invalidate`) — add `InvalidateAll()` to it as well
as to the `resolver` struct, or the subscriber cannot call it.

- [ ] **Step 5: Add `InvalidateAll`**

In `component/edge/resolve.go`, beside `Invalidate`:

```go
// InvalidateAll drops every cached resolution.
//
// It is what a v1:shopify:store event triggers. The cache is keyed by HOSTNAME
// and a store row carries none, so there is no entry to evict by name -- the
// edge would need a reverse index from store to site to do better, and a store
// edit is an operator act measured in ones per day while the cache is a 30s TTL
// over a handful of hostnames. Flushing it is cheaper than the index.
func (r *resolver) InvalidateAll() {
	r.mu.Lock()
	r.cache = map[string]entry{}
	r.mu.Unlock()
}
```

Widen the `invalidator` interface in `component/edge/invalidation_subscriber.go`:

```go
type invalidator interface {
	Invalidate(hostname string)
	InvalidateAll()
}
```

- [ ] **Step 6: Subscribe to store events**

In `component/edge/invalidation_subscriber.go`, add beside `customDomainPattern`:

```go
// storePattern is v1:shopify:store (epic memql#5530). A storefront's binding
// NAMES a store row now rather than copying it, so the domain the policy admits
// and the token reference the runtime document resolves both live on a row no
// site write touches. Without this rule an operator who re-points a store, or
// rotates its Storefront token reference, changes nothing anybody is served
// until the TTL backstop expires on each replica independently.
//
// It evicts EVERYTHING, because the cache is keyed by hostname and this row
// carries none -- see Resolver.InvalidateAll.
const storePattern = "graph.node.*.v1:shopify:store"
```

Subscribe to it alongside the other two, and route it to `InvalidateAll()` in `handle`.

In `component/node/routing.go`, add after the custom-domain rules, with a comment in the
house style explaining the consumer and the volume:

```go
		{Pattern: "graph.node.created.v1:shopify:store", TargetType: ""},
		{Pattern: "graph.node.updated.v1:shopify:store", TargetType: ""},
```

- [ ] **Step 7: Re-point the two consumers**

`component/edge/runtimeconfig.go`, `storefrontForSite`: read `site.Store` instead of
`bindingString(site.Binding, ...)`. A nil `site.Store` returns a block carrying the kind
and an empty domain — NO: return `nil`, the same as a non-storefront kind, because a
storefront with no store has nothing for the block to say and an empty `storeDomain`
would read as a store at the empty host. State that in the comment.

`component/edge/csp_storefront.go`, `storefrontSources`: read `site.Store.Domain`, keeping
the `validHost` check exactly as it is — the domain is still row data.

`bindingString` now has no caller. Delete it and its doc comment.

- [ ] **Step 8: Update the existing edge tests**

Every fixture that builds `site.Binding = map[string]any{"storeDomain": ..., "storefrontTokenRef": ...}`
becomes `site.Store = &BoundStore{ID: ..., Domain: ..., StorefrontTokenRef: ...}`:
`runtimeconfig_test.go` (`storefrontSite()`), `csp_storefront_test.go` (all four
`TestAMalformedStoreDomainIsDropped` vectors and the rest),
`storefront_fixture_serve_test.go`, `edge_test.go`.

`TestABindingOnANonStorefrontRowAdmitsNothing` keeps its meaning: set `Store` on a
`static` row and assert the policy is byte-identical.

Add one NEW assertion to `TestRuntimeConfigNeverCarriesTheShopifyAdminToken`: the resolver
is asked for exactly ONE secret name, and it is the storefront token reference.

- [ ] **Step 9: Update the cluster-e2e leg**

In `test/clustere2e/storefront_serving_test.go`, the site is created with
`Binding: {storeDomain, storefrontTokenRef}`. It must now create a `v1:shopify:store` row
first (`mutation createStore(...)`, as the cluster owner the header already requires) and
bind the site to it. Update the header note that explains why the token is deliberately
absent — it is now absent from the STORE row, not from the binding.

- [ ] **Step 10: Run the edge suite**

```bash
go test github.com/znasllc-io/memql/component/edge -count=1
go test github.com/znasllc-io/memql/component/node -count=1
```
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add component/edge component/node/routing.go test/clustere2e/storefront_serving_test.go
git commit -m "Issue #5538: the edge resolves a storefront's domain and token reference through its store row"
```

---

### Task 4: The package manifest binds by reference (#5540)

**Files:**
- Modify: `component/packages/manifest.go:123-131` (`ManifestBinding`)
- Modify: `component/packages/analyze.go:87-95,106-127,362-366`
- Modify: `component/packages/refusal.go` (new code)
- Modify: `component/packages/stages.go:444-455,514-533`
- Modify: `component/packages/production.go:405-453`
- Modify: `component/packages/autodeploy.go:110-118`
- Modify: `component/packages/fixtures_test.go`, `analyze_test.go`, `resolution_tail_test.go`, `render_parse_test.go`, `pipeline_test.go`
- Create: `component/packages/store_binding_test.go`
- Modify: `docs/public/operate/packages.md:47-62,152`

**Interfaces:**
- Consumes: the `{storeId}` binding shape from Task 2; `BoundStore` is NOT used here.
- Produces: `ManifestBinding{Store string}` (yaml/json key `store`);
  `CodeDeployableStoreUnknown = "deployable_store_unknown"`;
  `type StoreResolver func(ctx context.Context, domain string) (string, error)` + `Deps.Stores`;
  `Publisher.BindSiteToStore(ctx context.Context, siteId, storeId string) error`.

- [ ] **Step 1: Write the failing tests**

Create `component/packages/store_binding_test.go` with four cases. Use `validManifest`
from `fixtures_test.go` as the control in each, exactly as `analyze_test.go:170-174` does:

1. `TestAStorefrontManifestNamesItsStoreRatherThanCopyingIt` — a manifest whose binding is
   `{store: acme.myshopify.com}` analyzes clean OFFLINE and the report carries the
   declaration (`Binding.Store`). Add a negative control in the same test: `Analyze` is
   handed no `Deps` at all and still answers, which is the D12 property.
2. `TestAManifestNamingAnUnknownStoreIsRefused` — drive `d.publish()` with a `Stores`
   resolver that answers `""`; expect `RefusalCode(err) == CodeDeployableStoreUnknown`, and
   the message names the store and says where to attach one. NOT an `Analyze` test:
   `Analyze` stays offline and never resolves anything.
3. `TestARedeployKeepsItsBinding` — drive `d.publish()` twice against a stub publisher
   with an existing site row; assert `EnsureSite` is called once and `BindSiteToStore` is
   called zero times when the manifest's store is unchanged.
4. `TestARedeployRePointsABindingTheManifestChanged` — same, with the manifest naming a
   different store the second time; assert `BindSiteToStore` is called once with the new
   store id. Its comment must say why this is not "set on create only": the manifest is
   the declaration, and a manifest that says one store while the site serves another is a
   storefront quietly talking to the wrong merchant.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test github.com/znasllc-io/memql/component/packages -run 'StoreBinding|StorefrontManifest|UnknownStore|Redeploy' -count=1
```
Expected: FAIL.

- [ ] **Step 3: Change `ManifestBinding`**

```go
// ManifestBinding is the per-kind connection to the system a deployable
// fronts. Only shopify_storefront declares one, and since epic memql#5530 it
// NAMES the store rather than describing it: `store` is a v1:shopify:store
// row's myshopify.com domain, which the pipeline resolves to a row id at
// deploy and writes onto the site as {storeId}.
//
// WHY THE DOMAIN AND NOT THE ROW ID. A manifest is committed to a product's
// repository and read by whoever deploys it; a row id is a fact about one
// cluster's database and means nothing in another. The myshopify.com domain is
// the one identifier Shopify never changes and the one an operator can check
// by eye. It is not a hostname in the sense the manifest refuses -- that rule
// is about THIS cluster's addresses, which are chosen at deploy.
//
// It still carries no secret. The Storefront token reference moved to the
// store row with the domain; the manifest names neither.
type ManifestBinding struct {
	Store string `yaml:"store,omitempty" json:"store,omitempty"`
}
```

`hasBinding` becomes `b != nil && strings.TrimSpace(b.Store) != ""`.

`analyze.go`'s existing `CodeDeployableBindingMissing` message becomes:
`"deployable %q is a %s but names no store. A storefront names the v1:shopify:store row it fronts -- binding: {store: <shop>.myshopify.com} -- and the domain, the Storefront token reference and everything else about that store live on the row."`

- [ ] **Step 4: Add the refusal code**

In `component/packages/refusal.go`, beside `CodeDeployableBindingMissing`:

```go
	// CodeDeployableStoreUnknown: the manifest names a store this cluster has
	// no row for, or one this caller may not read (epic memql#5530).
	//
	// DELIBERATELY ONE CODE FOR BOTH. Telling a caller which of the two it was
	// would answer "does a store with this domain exist on this cluster" for
	// somebody who may not read stores, and that is the question the
	// cluster-owner tier exists to refuse. The repair is the same sentence
	// either way: attach the store on the deployable, as somebody who may.
	CodeDeployableStoreUnknown = "deployable_store_unknown"
```

- [ ] **Step 5: Resolve the store at PUBLISH, never in `Analyze`**

**`Analyze` MUST STAY OFFLINE.** Its own doc comment is the constraint: "Nothing here
writes, fetches, or reaches a cluster. That is D12: 'this DSL would refuse boot' is an
answer produced here, offline, before a pod is ever asked to run it."
(`component/packages/analyze.go:46-49`). Resolving a store row is a cluster read, so it
does not belong there and `Analyze`'s signature does not change.

So the work splits, and the split is the honest one:

- **`Analyze` carries the DECLARATION.** `DeployableReport.Binding` already travels from
  the manifest into the report untouched (`analyze.go:93`); with `ManifestBinding{Store}`
  that is the store's myshopify.com domain, which is what the confirm gate should show
  ("Fronts acme.myshopify.com"). `CodeDeployableBindingMissing` stays exactly where it is,
  for a storefront that names no store at all — a manifest fact, checkable offline.
  Do NOT add `DeployableReport.StoreId`.
- **`publish` carries the RESOLUTION.** Add to the `Deps` struct, beside `Credentials` and
  `Roles` (which exist for exactly this reason — a cluster read a stage needs and the
  pipeline injects):

  ```go
  	// Stores resolves the v1:shopify:store row a storefront's manifest NAMES,
  	// by its myshopify.com domain, to the bare row id (epic memql#5530, issue
  	// memql#5540). Returns "" for a miss.
  	//
  	// It runs under the CALLER's actor, deliberately, which is the same answer
  	// updateSiteStoreBinding's Go guard gives: a caller who may not read a
  	// store may not bind a storefront to it. A store is cluster-owner-tier, so
  	// resolving under the deployment instead would let anyone who can deploy a
  	// package point a storefront at any merchant on the cluster.
  	//
  	// NIL IS A REFUSAL, NOT A GAP, as it is for Credentials: a storefront
  	// deployed on a node that cannot resolve stores is refused by name rather
  	// than published unbound.
  	Stores StoreResolver
  ```

  with `type StoreResolver func(ctx context.Context, domain string) (string, error)` beside
  `CredentialResolver`, wired in `component/packages/production.go` to
  `query storeByDomain(domain: ...)` reading the bare `id` off the one row it returns.

  Call it in `stages.go`'s `publish`, for every storefront deployable, BEFORE the
  `siteId == ""` branch — so the refusal lands whether this is a first deploy or a
  redeploy. On `""` or a nil resolver, `refuseScoped(CodeDeployableStoreUnknown, dep.Name, ...)`.

- **The OS copy.** Add `deployable_store_unknown` to
  `clients/os/src/apps/deployables/packages/refusals.ts` beside `deployable_binding_missing`
  (the entries are `{title, next}`; `title` names what happened, `next` names the repair),
  and to `STOP_FOR_CODE` in `clients/os/src/apps/deployables/page/rail.ts` mapped to
  `"whatItIs"`, which is where its sibling `deployable_binding_missing` already goes. The
  sentence names the Store panel as the repair.
  `clients/os/src/apps/deployables/packages/ReportView.tsx:115-116` renders
  `d.binding?.storeDomain` as "Fronts {storeDomain}"; it becomes `d.binding?.store`. Its
  row type is `clients/os/src/apps/deployables/packages/rows.ts:259`.

- [ ] **Step 6: Write `{storeId}` on create, re-point on redeploy**

`EnsureSiteRequest.Binding *ManifestBinding` becomes `StoreId string`, filled by `publish`
from the `Stores` resolution rather than from the manifest.
`enginePublisher.EnsureSite` writes `, binding: {"storeId": "<id>"}` when `StoreId != ""`
(still `json.Marshal` of a `map[string]string`, so quoting is handled).

Add to the `Publisher` interface and to `enginePublisher`:

```go
	// BindSiteToStore re-points an EXISTING site at the store its manifest
	// names. Called only when the two differ.
	BindSiteToStore(ctx context.Context, siteId, storeId string) error
```

implemented as `mutation updateSiteStoreBinding(siteId: ..., storeId: ...)`.

In `stages.go`'s `publish`, in the branch where `siteId != ""` (the redeploy), compare the
existing row's `binding->storeId` to `dep.StoreId` and call `BindSiteToStore` when they
differ. `sitesForPackage` already returns the row, so the comparison needs no extra read.

- [ ] **Step 7: Fingerprint**

`bindingWord` becomes:

```go
func bindingWord(b *ManifestBinding) string {
	if b == nil {
		return ""
	}
	// The store the manifest NAMES. Re-pointing a storefront at another
	// merchant is the change most worth stopping an automatic deploy for.
	return b.Store
}
```

- [ ] **Step 8: Update the fixtures**

`fixtures_test.go`, `resolution_tail_test.go`, `analyze_test.go` and
`render_parse_test.go` all carry `binding: {storeDomain: ..., storefrontTokenRef: ...}`.
Each becomes `binding: {store: acme.myshopify.com}`. `analyze_test.go`'s "a storefront
with half a binding" case no longer exists (there is one field) — replace it with "a
storefront naming a store this cluster does not have", expecting
`CodeDeployableStoreUnknown`. `pipeline_test.go`'s `fakePublisher` gains
`BindSiteToStore`.

- [ ] **Step 9: Update `packages.md`**

Change the manifest example's `binding:` block to `store: acme.myshopify.com`, add the
missing `resolutionTail` key to the same example, add a `deployable_store_unknown` row to
the refusal table, and fix the stale `deployable_binding_missing` row (its second clause
became `deployable_hostname_unchosen`, which the table does not list at all).

- [ ] **Step 10: Run the suite**

```bash
go test github.com/znasllc-io/memql/component/packages -count=1
```
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add component/packages docs/public/operate/packages.md
git commit -m "Issue #5540: a manifest names the store it fronts, and a redeploy keeps its binding"
```

---

### Task 5: `app:deployables/store` replaces `app:stores/*` (#5541, part 1)

**Files:**
- Modify: `dsl/rbac/seeds.memql` (delete lines 581-594; add one part row in the parts block)
- Modify: `component/auth/rbac_model.go:212-215` (delete) and `:224-230` (`appPartGrants`)
- Modify: `clients/os/src/apps/deployables/parts.ts`
- Test: `component/memql/app_resource_os_parity_test.go` (its hard-coded part list)

**Interfaces:**
- Consumes: the `@requiresCapability("execute", "app:deployables/store")` written in Task 2.
- Produces: part `store` on `DEPLOYABLE_PARTS`; seed `cap-owner-execute-app-deployables-store`.

- [ ] **Step 1: Delete the `app:stores` seeds**

Remove the whole `// ---- app:stores (registry floor: min owner) ----` block from
`dsl/rbac/seeds.memql` — the header comment and all six rows with their `@description`
lines.

- [ ] **Step 2: Add the store part**

In the Deployables parts block of `dsl/rbac/seeds.memql`, after the `domains` pair:

```memql
// ---- app:deployables/store -- attach the Shopify store a storefront fronts ----
//
// OWNER ONLY, and that is not an oversight beside the five above. It is what
// the retired app:stores set granted: the Stores app was `min: "owner"` and
// v1:shopify:store is @rowAuthz(clusterOwner), so the population that could
// manage a store was cluster owners and nobody else. Seeding this part on
// developer too would put a control on a developer's screen that the row tier
// then serves nothing to -- a refusal rendered as an empty panel, which is the
// failure mode memql#5216 was filed for.
@description("owner: the store part of the Deployables app -- attach or change the Shopify store a storefront fronts (memql#5541).")
seed capability cap-owner-execute-app-deployables-store { roleSlug: "owner"  verb: "execute"  resourceType: "app:deployables/store"  predefined: true }
```

- [ ] **Step 3: Update the compiled mirror**

In `component/auth/rbac_model.go`, delete the four `"app:stores..."` lines from
`appReadFloors`, and add to `appPartGrants`:

```go
	"app:deployables/store":   {RoleOwner},
```

with a short comment saying why it is owner-only while its five neighbours are not.

- [ ] **Step 4: Add the part to the client**

In `clients/os/src/apps/deployables/parts.ts`: add `"store"` to `DEPLOYABLE_PARTS`, to
`NO_PARTS`, to `ALL_PARTS`, and a line to the header comment's part table:

```
//   store     attach or change the Shopify store a storefront fronts
```

Note in that comment that `store` is seeded on OWNER alone while the other five are owner
and developer, and why.

- [ ] **Step 5: Update the Go part list**

`component/memql/app_resource_os_parity_test.go`'s `assertKnownResourcesNameEveryApp`
hard-codes the five Deployables parts. Add `store`.

- [ ] **Step 6: Run the parity gates**

```bash
go test github.com/znasllc-io/memql/component/auth -count=1
go test github.com/znasllc-io/memql/component/memql -run 'Seed|Rbac|AppResource|Capability' -count=1
```
Expected: `TestOsRegistryRequiresMatchTheAppSeeds` FAILS here — the registry still carries
the `stores` manifest requiring `app:stores`. That is Task 7's half; the failure is
expected and named in the commit message.

- [ ] **Step 7: Commit**

```bash
git add dsl/rbac/seeds.memql component/auth/rbac_model.go \
        clients/os/src/apps/deployables/parts.ts \
        component/memql/app_resource_os_parity_test.go
git commit -m "Issue #5541: the storefront's store part replaces the app:stores capabilities"
```

---

### Task 6: MemQL OS — the store is a connection on the storefront deployable (#5541, part 2)

**Files:**
- Create: `clients/os/src/apps/deployables/store/rows.ts`, `health.ts`, `words.ts`, `useStore.ts`, `StorePanel.tsx`, `StorePicker.tsx`
- Delete: `clients/os/src/apps/stores/health.ts`, `words.ts` (moved)
- Modify: `clients/os/src/apps/deployables/rows.ts` (`storefrontBinding` -> `boundStoreId`)
- Modify: `clients/os/src/apps/deployables/page/DeployableWorkspace.tsx`
- Modify: `clients/os/src/apps/deployables/page/DeployablePage.tsx` (the `store` detail)
- Modify: `clients/os/src/apps/deployables/page/stops/WhatItIs.tsx` (the store facts leave)
- Modify: `clients/os/src/apps/deployables/page/compose.ts`, `actions.ts`, `ComposePage.tsx`, `page/stops/compose/fields.tsx`, `page/stops/compose/Source.tsx`
- Modify: `clients/os/src/apps/deployables/composition.css`
- Test: `clients/os/test/deployables/store.test.tsx` (new), and the existing
  `compose.test.tsx`, `rows.test.ts`, `harness.tsx`, `runScope.test.tsx`

**Design (this is the brief; follow it, do not re-invent it):**

**Where the store goes.** It leaves "App configuration" — where it sat as a chip pointing
at the BUILD REPORT, which is neither what it is nor where it is configured — and becomes
a first-class slot at the TOP of the Connections column, above Cluster address, Custom
domains and Client. All four answer one question: what is this thing connected to. This
costs no new layout language and fixes a real mis-filing.

```
  Source            App configuration        Connections
  +-----------+     +-------------------+    +----------------------------+
  | GitBranch |-----| App configuration |----| Store                      |
  | acme/web  |     |  Build settings   |    |   acme.myshopify.com       |
  | 2 apps    |     |  App values  3    |    |   Live . checked 2 min ago |
  +-----------+     +-------------------+    +----------------------------+
                                             | Cluster address            |
                                             | Custom domains             |
                                             | Client                     |
                                             +----------------------------+
```

- The slot renders only when `site.kind === "shopify_storefront"` AND `can.store`.
  Rule 12: a control a person's grants do not reach is ABSENT.
- Its `detail` line: the store's domain and its state word when bound; `"Attach a store"`
  when not. An empty state is an invitation to act, not an apology.
- Icon: `ShoppingBag` (already imported in that file).

**The Store panel is a PANE, not a dialog, and that is a design decision rather than a
convenience.** `DetailDialog` is `min(680px, 100vw - 36px)` with its own `<h3>` header,
and it is right for Traffic and App values — a short read and a small form. A store's
detail is the Stores app's whole 428-line page: facts, a scope comparison, a per-domain
mirror table, a paired development store and two acts. DESIGN.md rule 9 says real estate
belongs to content and rule 11 says a tall detail replaces its list rather than sharing a
scroll column. `DeployablePage` already does exactly this for ONE detail — `whereItLives`
returns a full pane wrapped in `Panel` + `Head` with breadcrumbs and `back` instead of
opening the dialog. Follow that branch, verbatim in shape:

```tsx
  if (detail === "store") {
    const toOverview = () => setDetail(null);
    return <div className="os-deploy-pane deployable-workspace" data-os-page-context={JSON.stringify({ page: "Deployable", siteId: site.id, hostname: site.hostname, name, view: "Store" })}><div className="os-deploy-scroll">
      <Panel label={`Store for ${siteName(site)}`}>
        <Head title="Store" breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: toOverview }, { label: "Store" }]}
          back={{ label: name, onSelect: toOverview }} />
        <StorePanel site={site} canBind={can.store} />
      </Panel>
    </div></div>;
  }
```

Do NOT add `"store"` to `detailTitle`'s map — that map is for the dialog, and a `store`
entry there would be a second title for a pane that already has a `Head`. Add `"store"` to
the `WorkspaceDetail` union and handle it in the branch above, before the dialog render.

**Inside `StorePanel`**, under that one `Head` (rule 1: the section's one heading), in order:

1. The store's domain on the first line, with the one primary action beside it:
   `Change store`. Nothing else stands.
2. `Facts` — Name, Plan, API version, Protected data level. Every absence is a `Figure`
   absence, never a zero and never an invented fact.
3. A `Subhead` `Scopes` — granted against needed, with the mismatch named in a sentence.
4. A `Subhead` `Mirror` — the per-domain sync table, READ-ONLY, carrying verbatim the
   sentence that names where the per-domain acts live. Move it; do not re-word it.
5. A `Subhead` `Development store` — the paired row when one exists, else a single
   `Attach a development store` action.
6. The two acts that are this store's alone, on one control line at the foot of the panel:
   `Pause ingestion` / `Resume ingestion` and `Reconcile subscriptions`. An act that is
   not legal from the current state is absent. These do NOT get a second `ActionBar` — the
   window already draws one for the DEPLOYABLE's lifecycle and two bars is what rule 12
   forbids; they sit inline exactly as `RuntimeSettingsPanel`'s controls do. Port the
   legality table from `apps/stores/words.ts` rather than restating it.

**Kit pieces to build with** (all from `clients/os/src/kit`): `Panel`, `Head`, `Subhead`,
`Facts`, `Fact`, `Field`, `Select`, `Input`, `Button`, `Caption`, `Notice`, `Chip`,
`Chips`, and `Measure` + `figureFrom` / `absent` for every number. Nothing new is invented;
DESIGN.md's applying note says a surface needing a control the kit lacks promotes it on
second use rather than respelling it locally.

**Attaching a store** in the add-a-deployable wizard: the two free-text fields
(`storeDomain`, `storefrontTokenRef`) are replaced by ONE field — a `Select` of the
cluster's stores, plus an `Add a store` action that opens the store's own fields inside
the same step. The wizard's forward act stays on the bar (DESIGN.md: "the forward act
lives nowhere else").

**Copy, exactly:**
- Slot label: `Store`. Unbound detail: `Attach a store`.
- Panel title: `Store`. Primary action: `Change store`.
- Pause: `Pause ingestion`; the state word after it succeeds: `Paused`.
- `Reconcile subscriptions`; while it runs, `Reconciling subscriptions`.
- Empty scopes: `No scopes reported yet. The store reports them when it is first reached.`
- Unresolvable store: `This storefront names a store that is not on this cluster. Attach
  one to serve a catalog.`

**Interfaces:**
- Consumes: `updateSiteStoreBinding` (Task 2), `developmentStoresFor` + the two new store
  fields (Task 1), `can.store` (Task 5).
- Produces: `boundStoreId(site: SiteRow): string` in `deployables/rows.ts`;
  `StorePanel`, `StorePicker`; `WorkspaceDetail` gains `"store"`.

- [ ] **Step 1: Move the two pure modules**

```bash
git mv clients/os/src/apps/stores/health.ts clients/os/src/apps/deployables/store/health.ts
git mv clients/os/src/apps/stores/words.ts  clients/os/src/apps/deployables/store/words.ts
```
Fix their imports (the `kit` path gains one `../`). Do not rewrite their logic — the
`Figure` absence semantics and the state/act tables are the functionality this epic must
not lose.

- [ ] **Step 2: Write the failing test**

Create `clients/os/test/deployables/store.test.tsx`. Model the harness on
`clients/os/test/stores/harness.tsx` (the fake connection sitting under `executeNamed`,
`builtinReply`'s id-keyed node map) and on `clients/os/test/deployables/harness.tsx`.
Cases:

1. `renders the store as a connection, not a build setting` — a storefront site with
   `can.store` shows a `Store` slot in the connections column and NO `Shopify binding`
   chip under App configuration.
2. `hides the store from somebody whose grants do not reach it` — `partsWithout("store")`
   renders no store control anywhere; assert absent, not disabled.
3. `invites an unbound storefront to attach a store` — detail line reads `Attach a store`.
4. `offers no per-domain act, and names where they live` — port verbatim from
   `clients/os/test/stores/stores.test.tsx:355-365`, including the four forbidden button
   patterns and the `live in Cluster . Data origins` assertion.
5. `never renders a disabled lifecycle act` — port from `stores.test.tsx:271-310`.
6. `shows an absence as an absence` — port the unmeasured cases from
   `stores.test.tsx:218-250`.
7. `binds through the picker rather than two text fields` — the compose step renders a
   store select and no `Storefront token reference` input; accepting it produces a
   `createSite` call whose binding names a `storeId`.

- [ ] **Step 3: Run it and watch it fail**

```bash
cd clients/os && npx vitest run test/deployables/store.test.tsx
```
(Run from `clients/os`, never the repo root — memory `vitest-from-repo-root-runs-os-tests-without-setup`.)
Expected: FAIL.

- [ ] **Step 4: Build it**

Write `store/rows.ts` (the `StoreRow` shape + `storeFromRow`), `store/useStore.ts` (the
reads: `storeById`, `stores`, `developmentStoresFor`, the on-demand `shopifyStoreHealth`
builtin, and the writes, each re-reading after an accepted write — port from
`apps/stores/useStores.ts`), `store/StorePanel.tsx` and `store/StorePicker.tsx` per the
design above.

In `deployables/rows.ts`, replace `StorefrontBinding` / `storefrontBinding` with:

```ts
/**
 * The v1:shopify:store row id this storefront is bound to, or "" when it is
 * not bound yet (epic memql#5530).
 *
 * THE ROW IS THE RECORD. The binding used to carry a copy of the store's
 * domain and its Storefront token reference; it names the store now, and the
 * domain, the token reference, the scopes, the plan and the health are read
 * from the store row itself. That is what makes an edit to the store reach
 * every storefront bound to it with no second write.
 */
export function boundStoreId(site: SiteRow): string {
  const v = site.binding["storeId"];
  return typeof v === "string" ? v : "";
}
```

In `DeployableWorkspace.tsx`: delete the `binding ? <button ... Shopify binding` chip; add
`<StorePiece>` as the first child of `.deployable-connections`. Add `"store"` to
`WorkspaceDetail`. In `DeployablePage.tsx`, render `<StorePanel>` for `detail === "store"`.
In `WhatItIs.tsx`, delete the two storefront `Fact`s and the `storefrontBinding` import —
the store is not a build fact.

In the compose flow, replace `storeDomain` + `storefrontTokenRef` on `ComposeDraft` with
`storeId`, and `bindingReady` becomes `draft.kind !== "shopify_storefront" || Boolean(draft.storeId?.trim())`.
`actions.ts` writes `binding: { storeId: ... }`.

- [ ] **Step 5: Run the OS suite — BUILD THE SDK FIRST**

`clients/os` typechecks against `sdk/ts/dist`'s built `.d.ts`, NOT against the SDK source.
`make sdk-gen` rewrites `sdk/ts/src/client/generated_*.ts` and rebuilds nothing, so
`updateSiteStoreBinding` and `developmentStoresFor` are invisible to the OS until you
build. Skipping this produces
`Property 'updateSiteStoreBinding' does not exist on type 'QueryClient'`, which reads like
a missing mutation rather than a stale build.

```bash
make sdk-gen
cd sdk/ts && npm run build && cd -
cd clients/os && npm run typecheck && npx vitest run
```
Expected: the new file PASSES; `test/stores/*` still fails (Task 7 deletes it).
Run vitest from `clients/os`, NEVER from the repo root — the root run picks the OS tests
up without their setup file and they fail for the wrong reason.

- [ ] **Step 6: Commit**

```bash
git add clients/os/src/apps/deployables clients/os/test/deployables/store.test.tsx
git rm -r --cached clients/os/src/apps/stores/health.ts clients/os/src/apps/stores/words.ts 2>/dev/null || true
git commit -m "Issue #5541: a storefront's store is a connection on its deployable, not a build setting"
```

---

### Task 7: Delete the Stores app (#5541, part 3)

**Files:**
- Delete: `clients/os/src/apps/stores/` (whatever remains), `clients/os/test/stores/`
- Modify: `clients/os/src/apps/registry.tsx` (the `stores` manifest, its import, `OS_REGISTRY.apps`)
- Modify: `clients/os/test/settings/access.test.tsx`, `test/system/rankLadder.test.tsx`, `test/settings/hiddenSurfaces.test.ts`, `test/system/readinessMapping.test.ts`

- [ ] **Step 1: Remove the manifest**

Delete the `stores` const, its header comment, its `StoresApp` / `STORES_SECTIONS`
imports, and its entry in `OS_REGISTRY.apps`.

- [ ] **Step 2: Delete the directories**

```bash
git rm -r clients/os/src/apps/stores clients/os/test/stores
```

- [ ] **Step 3: Re-point the cross-cutting tests**

`access.test.tsx` uses `app:stores` as its canonical "a resource this role does not hold"
fixture in 18 places. Replace it with `app:cluster/origins` — seeded on owner alone
(`dsl/rbac/seeds.memql`, `cap-owner-read-app-cluster-origins`; mirror at
`component/auth/rbac_model.go`, `"app:cluster/origins": {RoleOwner}`), so every assertion
keeps its exact meaning. Read each site: the surrounding copy assertions move with it
(`Allow Stores to Ada Lovelace` becomes `Allow Data origins to Ada Lovelace`) and the call
assertion at `:330` becomes `resourceType: "app:cluster/origins"`. Run the file and read
the failures rather than assuming a blind substitution is enough — the label comes from
the registry, not from the resource string.

`rankLadder.test.tsx:117-121` lists `Stores` in the owner's desktop and in four `not:`
arrays — remove it from all five.

`hiddenSurfaces.test.ts` — VERIFIED: the anti-vacuous floor there is FOUR assertions, not
one (`Settings -- Integrations`, `Settings -- Doors`, `Cluster -- Audit trail`, `Stores`).
Delete the `Stores` line only; the other three keep the floor and no replacement is needed.
Also fix the docblock at line ~45, which uses `"Stores -- Stores"` as its worked example of
a section listed beneath a hidden app — re-point it at a surface that still exists.

`readinessMapping.test.ts:86` lists `"stores"` among apps declaring no `needs`/`wants` —
remove it.

- [ ] **Step 4: Run every gate this touches**

```bash
cd clients/os && npm run typecheck && npx vitest run
cd - && go test github.com/znasllc-io/memql/component/memql -run 'OsRegistry|AppResource|KnownResources' -count=1
go test github.com/znasllc-io/memql/component/auth -count=1
```
Expected: all PASS. `TestOsRegistryRequiresMatchTheAppSeeds` is now green in BOTH
directions — no seed names an app that is gone, and no manifest names a seed that is.

- [ ] **Step 5: The capability sweep the design asks for**

```bash
git grep -n "app:stores" -- . ':!docs/superpowers/specs'
```
Expected: no output. The design record's own two mentions are history and stay.

- [ ] **Step 6: Commit**

```bash
git add clients/os/src/apps/registry.tsx clients/os/test
git commit -m "Issue #5541: delete the Stores app; its function lives on the storefront deployable"
```

---

### Task 8: The operator docs follow the store to its deployable (#5542)

**Files:**
- Modify: `docs/public/operate/shopify-connector.md` (Step 3 at :134, "Operating it" at :410-455, the cost-bucket line at :324)
- Modify: `docs/public/operate/shopify-storefront-checklist.md` (section 1 at :35, section 6's table at :157, section 7 at :215)

- [ ] **Step 1: Rewrite `shopify-connector.md` Step 3**

`## Step 3 -- enter the store in MemQL` becomes `## Step 3 -- attach the store to its
storefront`. The surface is `MemQL OS -> Deployables -> the storefront -> Store ->
Attach a store`. Keep the field table verbatim — the fields did not change — and keep the
"three token fields are REFERENCES" paragraph. Add, in one place, the sentence the issue
asks for:

> **A store row and a site binding are one record.** The storefront's binding NAMES this
> row; it does not copy it. Editing the store here reaches every storefront bound to it
> with no second write, and there is no second place to keep the domain or the Storefront
> token reference in step with.

Keep `### The environment seed` as it is — it still seeds a store row.

- [ ] **Step 2: Rewrite "Operating it"**

`### The Stores page` becomes `### The storefront's Store panel`, pointing at
`MemQL OS -> Deployables -> the storefront -> Store`. Keep the "Backfilling, reconciling
and pausing an individual DOMAIN are not here" paragraph verbatim — it is still true and
still the boundary. `### Pausing a store` keeps its text; the surface name changes.

In `### The dev-store smoke`, step 2's "add the store in MemQL, and confirm the Stores app
shows `configured`" becomes "attach the store on the storefront deployable, and confirm
its Store panel shows `configured` with no missing scopes". Step 1 gains the development
store as what it is: a second store row, flagged, paired to the live one.

Line 324 (`- The store's current bucket is on the MemQL OS Stores app.`) becomes
`- The store's current bucket is on the storefront's Store panel.`

- [ ] **Step 3: Update the checklist**

Line 35-36 becomes:

```
- [ ] Both tokens are `globalSecret` rows, and the store row references them.
      The site binding names the STORE, so the edge resolves the domain and the
      Storefront token reference through that one row.
```

Line 157's table row: `| `connect-src` | `https://<storeDomain>` | the bound store row's `domain` |`
and the two asset rows beside it.

Line 171: `- [ ] The bound store row's `domain` is the host the storefront actually calls.`

Section 7's bullet about reading the store's domain and token from `runtime-config.json`
keeps its meaning; add one line that the document's `storefront` block is now resolved
from the store row rather than from a copy on the site.

- [ ] **Step 4: Run the docs gates**

```bash
go test github.com/znasllc-io/memql/... -run 'Docs|Claim|Vocabulary|Glossary' -count=1
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/public/operate/shopify-connector.md docs/public/operate/shopify-storefront-checklist.md
git commit -m "Issue #5542: the operator docs follow the store to its deployable"
```

---

### Task 9: Green the tree, and delete this plan

- [ ] **Step 1: Regenerate everything derived**

```bash
make sdk-gen
cd sdk/ts && npm run build && cd -
make frontdoor-hosts-check && make frontdoor-paths-check
```

- [ ] **Step 2: The whole suite**

```bash
make test
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql \
  go test -count=1 github.com/znasllc-io/memql/component/memql/... \
                   github.com/znasllc-io/memql/component/database/...
cd clients/os && npm run typecheck && npx vitest run
```

- [ ] **Step 3: Delete the plan**

The epic body says the plan is deleted in the epic's merge (epic #5529 did the same).

```bash
git rm docs/superpowers/plans/2026-09-20-storefront-owns-store.md
```

- [ ] **Step 4: Commit and open ONE PR**

```bash
git add -u
git commit -m "Epic #5530: green the tree and retire the plan"
git push -u origin epic/storefront-owns-store
```

The PR body closes all five with separate `Closes` lines — memory
`closes-keyword-links-only-the-first-issue`: `Closes #a, #b` links only `#a`.
