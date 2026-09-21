# Storefront packs foundation -- implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use `- [ ]`.

**Goal:** Give a client-agnostic pack a delivery path into the default build, give a shopper a way to write to and read from one without being a MemQL principal, and make reviews consumable on a storefront.

**Architecture:** Three seams. (1) A pack DECLARES its default enablement at registration; `v1:platform:packState` absence stops meaning "enabled" and starts meaning "the pack's declared default", so reviews ships mounted-inert. (2) A pack DECLARES a shopper surface -- named forms (writes) and named reads -- and nothing else on that pack is reachable without a bearer; the edge owns the per-site switch, the effective `storeId` and the abuse controls, the bff owns the route and runs the construct under the SITE OWNER'S borrowed authority. (3) Reviews gains `storeId`, a shopper-authored mutation, a derived moderation state and a public read builtin.

**Tech Stack:** Go 1.26.1, MemQL DSL edition 2026, React/TS (clients/os), PostgreSQL+TimescaleDB.

**Spec:** `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md` (section 9, epic 4; G5, G6, D9; Q2, Q3).

**Issues:** memql#5532 (epic); #5549, #5550, #5551, #5552, #5553, #5554. ONE PR on `epic/storefront-packs-foundation`.

## Global Constraints

- **Owner decisions, taken 2026-09-21.** Q3: storefront packs link into the DEFAULT BUILD and default to **disabled by declared default**. Q2: **a declared, narrowly scoped unauthenticated route per pack** is built now; the verified Shopify customer is RECORDED as the answer for reading a buyer's own data and built when epic 5 needs it. #5551's endpoint approved as `bff route, edge proxies and stamps`, classified `servedButNotExternallyRouted`.
- **A shopper is never a principal, not even an anonymous one.** `@rowAuthz(public)` (memql#4541) publishes a whole CONCEPT; a review is owned by the merchant so it can be moderated. Reach is therefore declared per ROUTE, and the route runs under the site owner's borrowed authority -- the campaigns precedent (`auth.ContextWithUserActor(ctx, job.campaignOwnerUserId)`).
- **Pre-release: no shims, no deprecation windows.** Replace and delete.
- **Storefront packs ship concepts, mutations, tools and builtins -- no automations and no logic** (record section 7). A client's process attaches from its own repository.
- **No emojis** anywhere. Text indicators only.
- **`make test`, never `go test ./...`** -- the latter misses the engine.
- **Stage by explicit path.** Never `git add -A`.
- Commit subject form: `Epic #5532: <what>`.

---

### Task 1: A pack declares its default enablement

**Files:**
- Modify: `dsl/pack_enablement.go`
- Modify: `component/memql/pack_state.go:120-128`
- Modify: `app/pack_enablement.go:42`
- Modify: `dsl/platform/concepts.memql` (the `packState` doc comment)
- Modify: `component/memql/module_registry.go` (pack row `StateDetail`)
- Test: `dsl/pack_enablement_default_test.go`, `component/memql/pack_state_default_test.go`

**Interfaces:**
- Produces: `dsl.RegisterPackDefault(domain string, enabled bool)`, `dsl.PackDefaultEnabled(domain string) bool` (true when undeclared), `dsl.PackDefaults() map[string]bool`; `memql.DisabledPackDomains(states map[string]PackStateRow, defaults map[string]bool) []string`.
- Consumes: nothing.

- [ ] **Step 1: Write the failing tests**

```go
// dsl/pack_enablement_default_test.go
func TestAnUndeclaredPackDefaultsToEnabled(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	if !PackDefaultEnabled("nobody-declared-me") {
		t.Fatal("an undeclared pack must default to enabled")
	}
}

func TestADeclaredDefaultIsReported(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	RegisterPackDefault("reviews", false)
	if PackDefaultEnabled("reviews") {
		t.Fatal("a pack declaring defaultEnabled=false must report disabled")
	}
	if got := PackDefaults()["reviews"]; got {
		t.Fatalf("PackDefaults()[reviews] = %v, want false", got)
	}
}
```

```go
// component/memql/pack_state_default_test.go
func TestARowOverridesADeclaredDefaultInBothDirections(t *testing.T) {
	defaults := map[string]bool{"reviews": false, "harness": true}
	// No row: the declared default decides.
	got := DisabledPackDomains(map[string]PackStateRow{}, defaults)
	if len(got) != 1 || got[0] != "reviews" {
		t.Fatalf("no rows: got %v, want [reviews]", got)
	}
	// A row saying enabled beats a default saying disabled.
	got = DisabledPackDomains(map[string]PackStateRow{"reviews": {Enabled: true}}, defaults)
	if len(got) != 0 {
		t.Fatalf("row enabled: got %v, want []", got)
	}
	// A row saying disabled beats a default saying enabled.
	got = DisabledPackDomains(map[string]PackStateRow{"harness": {Enabled: false}}, defaults)
	if len(got) != 1 || got[0] != "harness" {
		t.Fatalf("row disabled: got %v, want [harness]", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd /home/znas/memql-projects/wt-storefront-packs && go test ./dsl/ -run TestADeclaredDefault -count=1`
Expected: FAIL, `undefined: RegisterPackDefault`.

- [ ] **Step 3: Implement**

In `dsl/pack_enablement.go`, beside `disabledPacks`:

```go
var (
	packDefaultsMu sync.RWMutex
	packDefaults   = map[string]bool{}
)

// RegisterPackDefault declares what a pack's enablement is when NO
// v1:platform:packState row governs it. Called from the pack's Register,
// before app phase 3 reads the rows.
//
// ABSENCE OF A ROW MEANS THE PACK'S DECLARED DEFAULT, and the default's
// default is enabled -- so every pack that declares nothing behaves
// exactly as it did before this existed. A storefront pack declares false:
// "absence means enabled" would switch a reviews write path and a public
// reviews read on for every cluster that ever upgrades, with no row
// anywhere saying so (epic memql#5532, issue memql#5549).
func RegisterPackDefault(domain string, enabled bool) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return
	}
	packDefaultsMu.Lock()
	defer packDefaultsMu.Unlock()
	packDefaults[trimmed] = enabled
}

// PackDefaultEnabled reports a pack's declared default. An UNDECLARED pack
// answers true, which is what keeps this additive.
func PackDefaultEnabled(domain string) bool {
	packDefaultsMu.RLock()
	defer packDefaultsMu.RUnlock()
	enabled, declared := packDefaults[strings.TrimSpace(domain)]
	return !declared || enabled
}

// PackDefaults returns every declared default, for the boot projection and
// the module inventory.
func PackDefaults() map[string]bool {
	packDefaultsMu.RLock()
	defer packDefaultsMu.RUnlock()
	out := make(map[string]bool, len(packDefaults))
	for k, v := range packDefaults {
		out[k] = v
	}
	return out
}

// ResetPackDefaultsForTest clears the declared defaults. Test seam only.
func ResetPackDefaultsForTest() {
	packDefaultsMu.Lock()
	defer packDefaultsMu.Unlock()
	packDefaults = map[string]bool{}
}
```

In `component/memql/pack_state.go`, REPLACE `DisabledPackDomainsFromStates`:

```go
// DisabledPackDomains folds the packState rows over the packs' declared
// defaults and returns the disabled set, sorted.
//
// A ROW ALWAYS WINS, in both directions: it is the operator's explicit act
// and the declared default is only what governs in its absence. Sorted
// because the set reaches a log line and the module inventory, and an
// unstable order there reads as a flip nobody made.
func DisabledPackDomains(states map[string]PackStateRow, defaults map[string]bool) []string {
	disabled := map[string]struct{}{}
	for domain, enabled := range defaults {
		if !enabled {
			disabled[domain] = struct{}{}
		}
	}
	for domain, row := range states {
		if row.Enabled {
			delete(disabled, domain)
			continue
		}
		disabled[domain] = struct{}{}
	}
	out := make([]string, 0, len(disabled))
	for d := range disabled {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
```

In `app/pack_enablement.go:42` swap the call:

```go
	disabled := memql.DisabledPackDomains(states, memqldsl.PackDefaults())
```

...adding the `memqldsl "github.com/znasllc-io/memql/dsl"` import, and widen the log line so a default-disabled pack is not reported as an operator's act:

```go
	if len(disabled) > 0 {
		a.Logger.Info("packs not loading behavioural constructs on this node; concepts stay mounted",
			"component", memql.ComponentName,
			"disabledPacks", disabled,
			"declaredDefaults", memqldsl.PackDefaults())
	}
```

- [ ] **Step 4: Amend the packState concept doc**

In `dsl/platform/concepts.memql`, in the `packState` doc comment, replace the sentence `ABSENCE OF A ROW MEANS ENABLED, so a pack ships with zero behavior change and a fresh database needs no seeding.` with:

```
/// ABSENCE OF A ROW MEANS THE PACK'S DECLARED DEFAULT (epic memql#5532, issue memql#5549), and a
/// pack that declares nothing defaults to ENABLED -- so a fresh database still needs no seeding and
/// every pack that predates the declaration behaves exactly as it did. A STOREFRONT pack declares
/// false: reviews and wholesale carry a shopper write path and a public read, and "absence means
/// enabled" would switch both on for every cluster that ever upgrades, with no row anywhere saying
/// so. A ROW ALWAYS WINS over a declared default, in both directions, because it is the operator's
/// explicit act.
```

- [ ] **Step 5: Make the inventory honest about WHY a pack is off**

In `component/memql/module_registry.go`, where the pack row's state is assembled, distinguish the two causes. Find the pack-row branch and set `StateDetail` to `"disabled by an operator" + reason` when a row governs, and to `"disabled by default; no v1:platform:packState row. Enable it to load this pack's behavioural constructs."` when the declared default is what disabled it. The row's `State` stays what it was.

- [ ] **Step 6: Run the tests**

Run: `go test ./dsl/ ./component/memql/ -run 'PackDefault|DisabledPackDomains' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add dsl/pack_enablement.go dsl/pack_enablement_default_test.go \
        component/memql/pack_state.go component/memql/pack_state_default_test.go \
        component/memql/module_registry.go app/pack_enablement.go dsl/platform/concepts.memql
git commit -m "Epic #5532: a pack declares its default enablement"
```

---

### Task 2: The shopper surface registry

**Files:**
- Create: `component/memql/shopper_surface.go`
- Test: `component/memql/shopper_surface_test.go`

**Interfaces:**
- Produces: `memql.ShopperField`, `memql.ShopperForm`, `memql.ShopperRead`, `memql.RegisterShopperForm(ShopperForm)`, `memql.RegisterShopperRead(ShopperRead)`, `memql.ShopperFormFor(pack, name string) *ShopperForm`, `memql.ShopperReadFor(pack, name string) *ShopperRead`, `memql.ShopperSurface() []ShopperSurfaceEntry`, `memql.ReservedShopperFieldNames() []string`, `memql.ResetShopperSurfaceForTest()`.

The registry is the WHOLE of what a shopper can reach. A pack that registers nothing here is unreachable without a bearer, which is the default and the point.

Key rules, each with a test:
- A field name in `{"storeId","siteId","ownerUserId","id","createdAt","createdBy","submittedAt"}` is REFUSED at registration (panic, init-time programming error): those are stamped server-side and a declared one would let a form supply its own.
- `Pack`, `Name`, `Construct` and at least one field are required.
- A duplicate `(pack, name)` is REFUSED rather than last-wins -- the duplicate-name rule the policy and rule registries already keep.
- `Name` and `Pack` must match `^[a-z][a-zA-Z0-9]{0,39}$`: they are path segments.
- A form declares `RedirectOK` and `RedirectError` as SITE-RELATIVE paths beginning `/` and containing no `//` or `..`; an absolute URL is refused, because the 303 must not become an open redirect.

- [ ] **Step 1: Write the failing test**

```go
func TestAFormMayNotDeclareAServerStampedField(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	defer func() {
		if recover() == nil {
			t.Fatal("declaring storeId as a form field must be refused")
		}
	}()
	RegisterShopperForm(ShopperForm{
		Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields: []ShopperField{{Name: "storeId"}},
		RedirectOK: "/thanks", RedirectError: "/error",
	})
}

func TestADuplicateFormNameIsRefused(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	f := ShopperForm{Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields: []ShopperField{{Name: "body", Required: true, MaxLength: 100}},
		RedirectOK: "/thanks", RedirectError: "/error"}
	RegisterShopperForm(f)
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate (pack, name) must be refused, not last-wins")
		}
	}()
	RegisterShopperForm(f)
}

func TestAnAbsoluteRedirectIsRefused(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	defer func() {
		if recover() == nil {
			t.Fatal("an absolute redirect target must be refused")
		}
	}()
	RegisterShopperForm(ShopperForm{Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields: []ShopperField{{Name: "body", Required: true, MaxLength: 100}},
		RedirectOK: "https://elsewhere.example/thanks", RedirectError: "/error"})
}

func TestLookupIsByPackAndName(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	RegisterShopperForm(ShopperForm{Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields: []ShopperField{{Name: "body", Required: true, MaxLength: 100}},
		RedirectOK: "/thanks", RedirectError: "/error"})
	if ShopperFormFor("reviews", "review") == nil {
		t.Fatal("the registered form must resolve")
	}
	if ShopperFormFor("reviews", "nope") != nil {
		t.Fatal("an unregistered name must not resolve")
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test ./component/memql/ -run TestAForm -count=1`
Expected: FAIL, `undefined: RegisterShopperForm`.

- [ ] **Step 3: Implement `component/memql/shopper_surface.go`**

Carry the file comment stating the decision (Q2), why the anonymous row-authz tier was not used, and that the registry is the whole reach. Types:

```go
type ShopperField struct {
	Name      string
	Required  bool
	MaxLength int      // 0 means the package default, shopperFieldDefaultMaxLength
	Enum      []string // empty means unconstrained
}

type ShopperForm struct {
	Pack          string
	Name          string
	Construct     string // the mutation this form writes through
	Fields        []ShopperField
	RedirectOK    string
	RedirectError string
}

type ShopperRead struct {
	Pack      string
	Name      string
	Construct string // the builtin or query this read calls
	Kind      string // "builtin" or "query"
	Fields    []ShopperField
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./component/memql/ -run Shopper -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/memql/shopper_surface.go component/memql/shopper_surface_test.go
git commit -m "Epic #5532: the shopper surface registry -- a pack declares what a shopper may reach"
```

---

### Task 3: The site row's shopper-forms switch

**Files:**
- Modify: `dsl/platform/concepts.memql` (`site.shopperForms`)
- Modify: `dsl/platform/mutations.memql` (`updateSiteShopperForms`)
- Modify: `dsl/platform/queries.memql` if a shape projects the site's fields explicitly
- Modify: `component/edge/resolve.go` (`Site.ShopperForms`, and the projection that fills it)
- Test: `component/edge/resolve_test.go` addition

- [ ] **Step 1** Add the concept field with its full `@description`: the switch is OFF by default and absent reads as off; an open form endpoint on every hosted site is the relay `proxy.go`'s comment warns about; it governs BOTH the declared forms and the declared reads; and it is independent of `apiProxy`, because a storefront that never wants the authenticated API may still want a review form.
- [ ] **Step 2** Add `updateSiteShopperForms` mirroring `updateSiteSettings`'s shape, carrying `@requiresCapability("execute", "app:deployables/store")`? NO -- use the same gate the other site writes use; read `updateSiteSettings` and match it exactly.
- [ ] **Step 3** Add `ShopperForms bool` to `Site` with the comment stating it is read on the serving path and what it gates, and fill it in the resolver's projection beside `APIProxy`.
- [ ] **Step 4** Test: a row with the field absent resolves `ShopperForms=false`.
- [ ] **Step 5** Run `go test ./component/edge/ -count=1`, then commit.

---

### Task 4: The edge half -- the gate, the store, the abuse controls

**Files:**
- Create: `component/edge/shopper.go`
- Create: `component/edge/shopper_test.go`
- Modify: `component/edge/handler.go` (a branch above the `/_memql/` proxy branch)
- Modify: `component/edge/proxy.go` (`bffRootPrefixes` gains `/forms`)
- Modify: `component/envregistry/manifest.yaml` (two knobs)

**Interfaces:**
- Produces: the wire contract between edge and bff -- headers `X-MemQL-Shopper-Site`, `X-MemQL-Shopper-Store`, `X-MemQL-Shopper-Owner`; path `/_memql/forms/{pack}/{name}` on the site's origin mapping to `/forms/{pack}/{name}` on the bff.

The four properties, each a test:
1. **OFF BY DEFAULT.** `site.ShopperForms == false` answers 404 -- not 403. A site that has not turned it on must look like it has no such path.
2. **THE EFFECTIVE STORE IS FREE.** `ServeHTTP` has already replaced `site` with `previewSite(site, grant)` before this branch, so `site.Store` IS the effective store: a submit made while previewing carries the DEVELOPMENT store's id and is invisible to every live-scoped read. D9 falls out of D7 rather than being implemented twice.
3. **THE HEADERS ARE STRIPPED THEN SET.** Any inbound `X-MemQL-Shopper-*` is deleted before ours is written, so a client cannot supply its own.
4. **RATE LIMITED AND SIZE CAPPED.** Per `(siteID, client address)`, a token bucket refilling at `MEMQL_SHOPPER_FORM_RATE_PER_MINUTE` (default 10, burst 10); `Content-Length` over `MEMQL_SHOPPER_FORM_MAX_BYTES` (default 65536) is refused BEFORE the proxy hop. Both answer the shopper-facing page of Task 6, not a bare status line.

A site whose `ownerUserId` is empty is REFUSED (`shopper_surface_unowned`): the write runs under the owner's borrowed authority and a cluster-owned site has no owner to borrow.

- [ ] **Step 1** Write `component/edge/shopper_test.go` covering the four properties plus the unowned refusal, against the real `Handler`.
- [ ] **Step 2** Run it, watch it fail.
- [ ] **Step 3** Implement `serveShopperSurface` in `component/edge/shopper.go` plus the bucket; wire the branch into `handler.go` above the `apiPrefix` branch and below the preview substitution; add `/forms` to `bffRootPrefixes` with a comment saying why the marker is stripped rather than swapped.
- [ ] **Step 4** Add the two manifest entries (`component: platform`, `scope: node`, `optional: true`, with defaults and a description naming what refuses and what the shopper sees).
- [ ] **Step 5** Run `go test ./component/edge/ -count=1`; commit.

---

### Task 5: The bff half -- the route, the re-resolution, the borrowed authority

**Files:**
- Create: `component/server/shopper_handler.go`
- Create: `component/server/shopper_handler_test.go`
- Modify: `component/server/nethttp.go` (`ShopperSurfacePaths()`)
- Modify: `component/server/unauthenticated_surface.go` (`HandlerAuthorizedPaths()`)
- Modify: `cmd/frontdoorpaths/main.go` (`servedButNotExternallyRouted`)
- Create: `app/transport_shopper_surface.go` (`//go:build bff`)
- Modify: `app/transport.go` or `app/build_bff.go` to call the mount

**THE HEADER IS A POINTER, NEVER AN ASSERTION.** The handler re-reads the site row named by `X-MemQL-Shopper-Site` and refuses unless that row has `shopperForms` on, is not deleted, and the presented store id is either its `binding.storeId` or its `previewBinding.storeId`. So the most a forged header can do is submit to a site that has forms on, under a store that site is actually bound to. This is stated in the handler comment rather than claimed, and it is what makes `servedButNotExternallyRouted` a defence in depth rather than the only control.

- [ ] **Step 1** Write the failing tests:
  - no `X-MemQL-Shopper-Site` header -> 404, nothing executed (fails closed with no credentials, which is what `HandlerAuthorizedPaths()` membership means)
  - a header naming a site whose `shopperForms` is off -> 404, nothing executed
  - a store id that is neither the binding's nor the preview binding's -> 404, nothing executed
  - an unregistered `(pack, name)` -> 404
  - a pack that is DISABLED -> 404 (mounted-inert means its mutation is not in the registry; assert the refusal is the same 404 and not a 500)
  - a declared field over `MaxLength` -> the error page, nothing executed
  - an UNDECLARED form field -> dropped, the write proceeds with the declared ones only
  - a good post -> the construct is called with the declared fields plus the stamped `storeId`/`siteId`, under `auth.ContextWithUserActor` for the site owner, and the reply is 303 to the form's `RedirectOK`
- [ ] **Step 2** Run, watch fail.
- [ ] **Step 3** Implement. `ShopperSurfacePaths() []string { return pathsWithBase("/forms/") }`, a doc comment in the idiom of `TrackingPaths` recording why it is HTTP (a plain HTML form post is a form post or it is not one; `script-src 'self'` forbids a JS submit handler), why it is a PREFIX, and why it is in `HandlerAuthorizedPaths()` rather than `PublicPaths()`.
- [ ] **Step 4** Add the `frontdoorpaths` entry with the evidence sentence -- the inverse pricing: routing this one costs EXPOSURE, and the edge is the only intended caller.
- [ ] **Step 5** Regenerate: `make frontdoor` then `make frontdoor-paths-check` and `make frontdoor-hosts-check`.
- [ ] **Step 6** Run `go test ./component/server/ ./cmd/frontdoorpaths/ -count=1`; commit.

---

### Task 6: What a shopper sees when it does not work

**Files:**
- Create: `component/server/shopper_pages.go`
- Create: `component/server/shopper_pages_test.go`

Rendered by the engine, on somebody's storefront, to a member of the public, under `script-src 'self'` with NO JavaScript. Follows `component/campaigns/unsubscribe.go`'s render shape and improves on it: a single self-contained page, system font stack, light and dark through `prefers-color-scheme`, no external asset, no emoji, no invented facts.

Five states, each naming what happened and what the person can do: `too_large`, `rate_limited`, `not_available` (the surface is off or the pack is disabled -- one message for both, because distinguishing them tells a prober which), `invalid` (a declared field failed its own rule -- names the field), `failed` (the write was refused downstream). Plus the success page, used only when a form declares no `RedirectOK`.

- [ ] **Step 1** Test: every state renders 1 page, escapes its inputs (`html.EscapeString` at the sink), carries no `<script>`, and carries the right status code.
- [ ] **Step 2..4** Implement, run, commit.

---

### Task 7: Reviews promoted, and made consumable

**Files:**
- Move: `examples/reviewspack/` -> `packs/reviewspack/` (`git mv`)
- Delete: `packs/reviewspack/register_reviewspack.go`
- Create: `app/anchor_storefront_packs.go`
- Modify: `app/engine.go:68` (anchor before `loadPackEnablement`)
- Modify: `packs/reviewspack/pack.go`, `dsl/concepts.memql`, `dsl/mutations.memql`, `dsl/builtins.memql`, `dsl/tools.memql`
- Create: `packs/reviewspack/dsl/queries.memql`
- Modify: `packs/reviewspack/pack_test.go`

**The concept changes.** `review` gains `storeId string!` (server-stamped, never from the form), `authorName string`, `authorEmail string` (the shopper as DATA -- Q2's answer made literal), and `moderationState enum("visible","hidden")!`. `moderationAction` and `reviewSettings` each gain `storeId string!`.

**`ownerUserId` becomes `string`, not `string!`,** and its description changes: it is the MERCHANT -- the owner of the site the review was submitted through -- rather than the review's author. A shopper has no MemQL identity, so a review's author is `authorName`/`authorEmail` and nothing else. This is the single most important sentence in the pack.

**Moderation hides through the Go half, and the DSL mutation is removed.** `recordModerationAction` as a `.memql` mutation is DELETED: the visibility flip is a write to a SECOND row and no mutation body can make one, so a DSL-written moderation row would append a decision that hid nothing. The Go capability already exists, already refuses a provider operator, and now writes both rows. `tools.memql` points at the builtin.

**The public read is a builtin, not a query.** `reviewsPublishedForProduct(storeId, productHandle, limit)` reads `reviewSettings` for the store FIRST and returns an empty list when `publicDisplay` is false, then returns `moderationState == "visible"` reviews for that `(storeId, productHandle)`. Two reads and a gate between them is not expressible in a filter, and putting the gate in the caller would mean every future caller had to remember it.

- [ ] **Step 1** `git mv examples/reviewspack packs/reviewspack && git rm packs/reviewspack/register_reviewspack.go`
- [ ] **Step 2** Write `app/anchor_storefront_packs.go` -- NO build tag, so every node type mounts it; the comment says why it is unconditional (a pack's reach is now governed by `packState` rather than by which binary linked it) and why it must run before `loadPackEnablement` (the declared default has to be registered before the rows are folded over it).
- [ ] **Step 3** Call it from `app/engine.go` immediately above `a.loadPackEnablement()`.
- [ ] **Step 4** Edit the DSL: concepts, `submitReview`, the queries file, the builtin, the tools.
- [ ] **Step 5** `pack.go`: `Register` also calls `memqldsl.RegisterPackDefault(Domain, false)`, `memql.RegisterShopperForm(...)` and `memql.RegisterShopperRead(...)`; `recordModerationAction` additionally flips the review's `moderationState`; add `reviewsPublishedForProduct`.
- [ ] **Step 6** Tests: the pack loads; a disabled pack is inert (its mutation is absent from the registry while its concepts are present); a row written under one `storeId` is invisible to a read scoped to another; `publicDisplay=false` returns empty; a hidden review is absent from the public read.
- [ ] **Step 7** Regenerate what a new construct reds: `make concept-snapshot`, `make sdk-gen`, `make arch-model` as each gate demands -- run the gates first and regenerate only what they name.
- [ ] **Step 8** `make test`; commit.

---

### Task 8: The operator surfaces

**Files:**
- Modify: `clients/os/src/apps/cluster/modules/ModulesSection.tsx`, `ModuleDetail.tsx`, `rows.ts`
- Modify: `clients/os/src/apps/deployables/` (the site detail's settings area)
- Test: the app's existing vitest files

Held to `clients/os/SUPERVISED-VISUAL-COMPOSITION.md` and `clients/os/DESIGN.md`:
- Honest state. "Disabled by default, no row" and "disabled by an operator" are DIFFERENT and the screen says which. Enabled-vs-loaded is reported honestly in the window between a flip and a restart -- restart-required stated in words, not implied.
- The flip is a real switch (a true on/off setting), with its consequence stated before the click: what loads, what stops loading, and that it takes effect as each node restarts.
- Empty and unreadable states get the same care as populated ones. A pack whose state cannot be read says so; it never renders as enabled.
- Keyboard reaches every action; no invented facts; no emoji.
- Deployables gains the shopper-forms switch beside the site's other settings, with the consequence stated: an endpoint on this site's own origin that accepts a form post from anyone.

- [ ] **Step 1** Read `clients/os/README.md` (live-collection contract, arrival cues) and `DESIGN.md` before touching a component.
- [ ] **Step 2** Tests first where the app has them; then implement; then `make os-test` and `npm run typecheck` in `clients/os`.
- [ ] **Step 3** Commit.

---

### Task 9: The documentation, and the record

**Files:**
- Modify: `docs/public/build/building-a-pack.md`
- Modify: `docs/public/operate/auth/access-model.md`
- Modify: `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md` (Q2 and Q3 ANSWERED; a derived D12)
- Modify: `CLAUDE.md` (the HTTP exception row; the `packs/` directory; the pack default)
- Modify: `GLOSSARY.md` if it indexes the pack guide

`building-a-pack.md` gains, in the order a reader needs it: how a storefront pack ships (the default build, `packState`, the declared default, and that `examples/` is still where an EXAMPLE pack lives); the shopper surface, what declaring one costs, and the four controls; `storeId` on every shopper-written row and why preview forces it; and the storefront-pack convention -- concepts, mutations, tools and builtins, no automations and no logic.

- [ ] **Step 1..3** Write, check every claim against the code, commit.

---

### Task 10: Verification, PR, merge

- [ ] `make test` green from the worktree.
- [ ] `MEMQL_REQUIRE_DB=1` against the real Postgres for the db-gated trees the epic touches.
- [ ] `make frontdoor-paths-check`, `make frontdoor-hosts-check`.
- [ ] `make os-test`.
- [ ] Negative controls: for each of the four headline properties, break the fix and confirm the test fails for the RIGHT reason.
- [ ] Delete `docs/superpowers/plans/2026-09-21-storefront-packs-foundation.md` (the epic body: the plan is deleted in the epic's merge).
- [ ] One PR, `Closes #5549` ... one keyword per issue on its own line.
- [ ] Watch CI; `scripts/dev/merge-as-owner.sh --pr=<n> --check` then merge.
