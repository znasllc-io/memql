---
title: Building a pack -- extend MemQL with your own domain
audience: public
status: stable
area: build
sinceVersion: 0.9.7
owner: znas
---

# Building a pack -- extend MemQL with your own domain

A **pack** is the unit of product-specific extension in MemQL: a bundle of Go
integration code plus a `.memql` DSL subtree that drops into the engine and
runs alongside the core domains. A product's DSL bundle plus its client is the
production reference consumer; this guide walks you through building one from
scratch, using the minimal **reference pack** at
[`examples/referencepack/`](../../../examples/referencepack) as the worked
example.

> **Three words.** A **component** is engine internals. An **integration** talks to other systems (`integrations/`). A **pack** is a client-agnostic product feature — that is what intake "plugin" means. `RegisterPlugin` is the registration primitive, not a fourth runtime. Core domains (`dsl/todos`, `dsl/calendar`, `dsl/campaigns`) cannot be shadowed. See [Component vs integration vs pack](../concepts/component-integration-pack.md).

If you want the bare contract reference instead of a walkthrough -- the exact
`PluginContext` fields, the `PluginFactory` signature, the registration
primitives -- read [Plugin SDK](plugin-sdk.md). This guide assumes that page as
background and shows how the pieces fit together.

> **Scope.** A pack has two halves that load differently. The **Go integration**
> is **compiled in via build tags** -- linking its package into a binary runs
> its `init()` registrations, and there is no runtime loading of the Go half, by
> design (see [Build Tags](build-tags.md)). The **`.memql` domains** load either
> the same way (embedded, as the reference pack below does) or **at runtime from
> disk** via `MEMQL_DSL_PATH` -- a product-agnostic engine image mounts a
> product's DSL bundle at boot with `RegisterTree(domain, os.DirFS(...))`. This
> guide embeds; the same `RegisterTree` call backs both paths.

---

## What a pack is, exactly

A pack is **three registration primitives**, all called from a build-tag-gated
`init()`:

1. **`memql.RegisterPluginForContract(name, version, factory)`** -- registers
   your Go `IntegrationProvider` (the DSL-callable capability set) against the
   Plugin SDK **contract version** you built against. The loader rejects a pack
   whose declared version is incompatible with the core's
   `memql.PluginContractVersion`, so a stale pack fails loudly at startup
   instead of silently mis-binding. (`RegisterPlugin` stamps the current version
   implicitly; third-party packs SHOULD pin it explicitly with
   `RegisterPluginForContract`.)
2. **`dsl.RegisterTree(domain, fs.FS)`** -- mounts your embedded `.memql`
   subtree under `domain/` in the unified DSL tree. **Namespace ownership** is
   validated here.
3. **`node.RegisterRoutingRule(rule)`** -- a cross-node event routing rule,
   required for any event that must cross a node boundary. The minimal reference
   pack does not cross nodes, so it skips this one. (Every cross-node event-bus
   pub/sub needs a routing rule or it silently dies in cluster mode.)

Everything else -- concepts, builtins, tools, automations, prompts, queries,
mutations, specs, shapes -- lives in the embedded `.memql` tree and rides in via
`RegisterTree`. The Go side is only the protocol-adapter capabilities the DSL
cannot express.

---

## The three registration primitives

### 1. Register your Go integration (contract-versioned)

Your pack exposes Go-backed operations to the DSL through an
`memql.IntegrationProvider`: it returns a stable `IntegrationName()` and a list
of `IntegrationCapability` values, each a named handler. A capability named
`composeGreeting` on an integration named `referencepack` becomes callable from
the DSL under the FQN `integration.referencepack.composeGreeting`.

A `memql.PluginFactory` builds that provider from the live `PluginContext`:

```go
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
    // pluck DB getters, providers, resolvers off pctx here (this minimal
    // pack needs none); return (nil, nil) to opt out when deps are missing.
    return &Provider{}, nil
}
```

You register it pinned to the contract version you built against:

```go
memql.RegisterPluginForContract("referencepack", memql.PluginContractVersion, NewProvider)
```

The app bootstrap (`app.materializePlugins`) calls
`PluginRegistration.ValidateContract` for every registered pack and aborts
startup -- fatal, with a descriptive error -- on any incompatibility.
`memql.CheckPluginContractCompat(version)` is the pure, unit-testable form of
that check.

> **Order gotcha (custom harnesses / tests).** A pack's builtin resolves its
> Go capability by FQN (`integration.<name>.<capability>`) at **dispatch
> time**, against the integration capabilities registered on the engine.
> Register the provider **after** `engine.Init` -- registering before Init,
> then calling Init, leaves the capability out of the dispatch map and the
> builtin fails with `unknown builtin executor "integration.<name>.<cap>"`.
> The normal app path is automatic: `app.materializePlugins` registers every
> pack provider *after* the engine is initialized. You only have to think
> about this when wiring an engine by hand (e.g. an integration test). See
> `examples/referencepack/live_e2e_test.go`.

### 2. Register your embedded DSL tree (namespace-owned)

Embed your `.memql` files and mount them under your domain:

```go
//go:embed all:dsl
var packFS embed.FS

func Tree() fs.FS {
    sub, _ := fs.Sub(packFS, "dsl")
    return sub
}

dsl.RegisterTree("referencepack", Tree())
```

After this, every DSL loader that walks `dsl.Tree()` sees your files under
`referencepack/` alongside the core domains, and your concepts/tools/builtins
register into the same engine registries core uses.

**Namespace ownership is validated at registration.**
`dsl.RegisterTree` calls `dsl.ValidatePackDomain(domain, coreDomains, existing)`
before mounting, and **panics** on a violation:

- the domain must be non-empty and contain no `/`;
- it must **not collide with a core embedded domain** -- core domains are
  canonical and owned by MemQL; a pack cannot shadow or extend one;
- it must **not collide with another pack's already-registered domain** -- two
  packs claiming the same namespace is ambiguous and rejected.

This is the namespace-ownership rule from the pack model
([Plugin SDK -> Pack model](plugin-sdk.md#pack-model--load-time-validation)).
Pick a unique domain. The core domain set is read from the embedded tree's
top-level directories, so it always reflects the real `//go:embed` directive.
Partition scoping -- the canonical *tenant* dimension a pack scopes its data to
-- is a separate axis; see [Partition scoping](../concepts/partition-scoping.md).

### 3. Register routing rules (only if you cross nodes)

If your pack emits an event on one node type that must be consumed on another,
register a routing rule with `node.RegisterRoutingRule(...)` -- otherwise the
event silently dies in cluster mode. The reference pack stays on one node, so it
omits this. The contract is documented in [Plugin SDK](plugin-sdk.md).

---

## Build-tag gating: how a pack is loaded (and how it is kept out)

A pack must run **only** in the binaries that should carry it. The mechanism is
Go build tags on the file that holds the pack's `init()`:

```go
//go:build mypack

package mypack

func init() {
    Register("mypack") // dsl.RegisterTree + memql.RegisterPluginForContract
}
```

The `init()` runs only when the binary is built with that tag, and the package
is anchored into the binary via a blank import in the app bootstrap. A node type
built without the tag never links the registration, so the pack never loads
there. This is exactly how a product gates a compiled-in Go integration to the
node types that should carry it. See [Build Tags](build-tags.md).

**Keeping a pack out of production entirely.** The reference pack demonstrates
the inverse: a pack that `go build ./...` compiles (so CI verifies it builds)
but that **no production binary ever loads**. The trick is twofold:

1. The pack package (`pack.go`) is **normal, untagged Go** with **no
   unconditional `init()`** -- merely linking it in does *not* register it.
   Registration is the explicit `Register(domain)` function, which tests call
   directly.
2. A separate file `register_referencepack.go` carries
   `//go:build referencepack` and the only `init()`:

   ```go
   //go:build referencepack

   package referencepack

   func init() { Register(Domain) }
   ```

   This is the *real* build-tag-gated auto-register pattern a production pack
   uses -- but the `referencepack` tag is never set in any production build, so
   the `init()` never runs and the pack never auto-loads. Swap `referencepack`
   for your product tag and you have a production pack.

The takeaway: **put the pack's `init()` in a build-tag-gated file**, never in
the always-compiled package body. That single rule decides where a pack loads.

---

## Storefront packs: in the default build, governed by a row

Build-tag gating above is one of **two** delivery paths, and which tag a pack
names decides whether it ships at all. The engine images are built with
`BUILD_TAGS=<node type>` and nothing else, so:

- a pack gated on its **own name** (`referencepack`, `shopifypack`) reaches
  nothing, because no image sets that tag. It could once arrive through a
  carrier binary, and that route is retiring (memql#2472). A pack behind a tag
  nobody sets is a pack nobody has;
- a pack gated on a **node type** does ship, to that node. `examples/deploypack`
  is `//go:build identity` plus an unconditional anchor, which is how the
  deploy lifecycle automations reach the node that writes deployment records.

Neither gives what a customer-facing pack needs: presence on **every** node,
with its reach changeable by an operator rather than by a release.

**A storefront pack lives under `packs/` and links into every binary, with no
tag at all** (epic memql#5532). Its reach is then governed by
`v1:platform:packState` rather than by which binary happened to link it, which
is what makes "enable reviews on this cluster" one operator act instead of a
release.

```go
// packs/mypack/pack.go -- no build tag, no init(); Register is called from
// app/anchor_storefront_packs.go, which every node type runs.
func Register(domain string) {
    memqldsl.RegisterTree(domain, Tree())
    memqldsl.RegisterPackDefault(domain, DefaultEnabled) // false, for a storefront pack
    registerShopperSurface()                             // see below
    memql.BindPluginToPack(domain, domain)
    memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}
```

**A storefront pack ships DISABLED**, and that is a decision rather than
caution. `v1:platform:packState` absence used to mean *enabled*; it now means
**the pack's declared default**, and a pack that declares nothing still
defaults to enabled, so nothing that predates the declaration changed. A
storefront pack declares `false` because enabling one publishes a write
endpoint and a public read on every deployable whose `shopperForms` is on --
an operator's decision, not an upgrade's. A `packState` **row always wins**
over a declared default, in both directions.

A disabled pack is **mounted-inert**: its concepts still load so imports and
relationships resolve and its existing rows stay browsable, and every
behavioural construct is skipped. A flip takes effect as each node restarts.
Operators see all of this at **Cluster > Modules**, which names why a pack is
off -- ships-disabled, or switched off by somebody -- and what enabling it
would publish.

**Register the default before the rows are read.** `app/engine.go` anchors the
storefront packs immediately above `loadPackEnablement()` for exactly this
reason: a declared default heard after the rows have been folded over it is a
default that did nothing, and the pack ships enabled with nothing saying so.

**Packs are snapshotted.** `make concept-snapshot` covers `packs/*/dsl` as well
as `dsl/`, because a pack in the default build holds rows that dropping a field
would brick -- silently, since CI's db-tests run on a fresh database.

---

## How a shopper writes to a pack

A shopper is **nobody to MemQL**: no user row, no session, no bearer, no actor
-- not even an anonymous one. `@rowAuthz(public)` exists and is the wrong
instrument here, because it publishes a whole **concept**; a review is owned by
the merchant so it can be moderated, and a wholesale application is somebody's
tax identifier.

So reach is declared **per route**. A pack names the forms a shopper may post
and the reads a shopper may make, in Go, at registration time
(`component/memql/shopper_surface.go`). Everything the pack did not name is
exactly as unreachable as it was.

```go
memql.RegisterShopperForm(memql.ShopperForm{
    Pack: Domain, Name: "review", Construct: "submitReview",
    Fields: []memql.ShopperField{
        {Name: "productHandle", Required: true, MaxLength: 200},
        {Name: "body", Required: true, MaxLength: 4000},
        {Name: "rating", Numeric: true, MaxLength: 2},
    },
    RedirectOK: "/reviews/thank-you", RedirectError: "/reviews/problem",
})
```

A declared form is reachable at `POST /_memql/forms/{pack}/{name}` on the
site's own origin, and a declared read at `GET /_memql/reads/{pack}/{name}`.
It takes `application/x-www-form-urlencoded` and multipart from a **plain HTML
form with no JavaScript** -- which is the requirement that shapes the whole
path: the first storefront's wholesale form is `method="post"` so a federal tax
identifier never reaches a query string, and its served policy is
`script-src 'self'`, so a JS submit handler is not available to it.

**What declaring one costs.** Four controls stand in front of it and none of
them is the registry:

| Control | Where | What it decides |
|---|---|---|
| `site.shopperForms` | the site row, off by default | whether this deployable has the endpoint at all |
| rate limit | the edge, per address per site | how often one visitor may post |
| size cap | the edge and the bff | `MEMQL_SHOPPER_FORM_MAX_BYTES`, 64 KiB by default |
| not externally routed | `cmd/frontdoorpaths` | the bff route has no front-door rule, so the edge is the only way in |

The registry's job is the one nothing else can do: it bounds **what** a
reachable request may name.

**The write runs under the SITE OWNER'S borrowed authority.** The merchant owns
what is written through their storefront; the shopper is data on the row --
their name and email are fields, never an identity. This is the campaigns
pattern (`auth.ContextWithUserActor`), and it means a pack's shopper mutation
is an ordinary `@actor` mutation. A `@serverOnly` construct is deliberately out
of reach.

**The owner header is self-checking.** The bff re-reads the site row *under*
the named user, and `v1:platform:site` is composite-owner tier, so a user who
does not own that site reads zero rows. A forged owner refuses itself.

**Every shopper-written row carries its store.** `storeId` is stamped
server-side from the binding the write arrived through -- the **preview**
binding when the submission was made while exercising a candidate version --
and `storeId`, `siteId`, `ownerUserId` and the row intrinsics are **refused as
declared field names**. A form that could supply its own `storeId` could aim a
row at a store the shopper is not on, which is exactly the preview-versus-live
confusion the field exists to prevent. Every storefront read filters on it.

**A refusal answers 303 to the page the pack declared**, with `?reason=` naming
what happened, so it renders in the merchant's own design and language. Only
the refusals the edge must make before a declaration is in hand -- rate
limited, too large, surface off -- are engine-rendered pages.

**One submission, one id.** The bff mints a `submissionId` per POST and stamps
it beside `storeId` and `siteId`. A pack's shopper construct **declares it in
its args block and writes the row at it** -- it is reserved as a field name, so
a shopper can never supply one. Before this the row id was engine-derived and
known to nobody, which left a client's related row with nothing to point at.

### A client's own fields: `@shopperFormExtension`

A pack declares the minimum its concept needs to be what it is, and no field
beyond it. The first client collects an EIN and the next will not, so a client
declares its own concept with an `@relationship` to the pack's row -- typed and
queryable, where a `metadata` blob on the pack would take every client's fields
in turn.

Getting a shopper's value *into* that concept is the extension. A client's DSL
domain adds fields to a form the pack already declares, and names the mutation
that stores them:

```memql
@shopperFormExtension(pack="wholesale", form="application")
mutation applicationDetail recordFyloApplicationDetail {
  args {
    submissionId  string!            // stamped; never offered to a shopper
    storeId       string!            // stamped
    ein           string   @maxLength(20)
    address       string!  @maxLength(200)
  }
  insert {
    accept { ein, address }
    stamp {
      id:            args.submissionId
      applicationId: args.submissionId
      storeId:       args.storeId
      ownerUserId:   actor.userId
    }
  }
}
```

**It adds fields to a route; it never opens one.** Putting a public endpoint on
a merchant's origin stays the privilege of Go compiled into the engine. An
extension therefore inherits the pack's gate -- wholesale's `applicationsOpen`
lives inside the pack's own construct -- along with its rate limit, its size
cap and its redirect pages, rather than re-implementing any of them.

**The field list is the args block minus the stamped names.** Derived, not
restated, so the two cannot drift. The rules are the ones a pack's own fields
pass: reserved names refused, and a name the pack already declares refused too
-- the client reads the pack's value through the relationship instead.

**What the bff does with it**, in this order:

1. validates the pack's fields **and** the extension's -- a failure in either
   refuses the submission with no row of either kind written;
2. mints the submission id;
3. runs the pack's construct, so the pack's own gate decides;
4. runs the extension's mutation with the same stamps.

**A failed extension write leaves the pack's row standing** and still answers
the success page (`memql_shopper_extension_write_failed_total{pack,form}` plus
an Error log naming the submission id). A trade desk gets an application with
its client fields visibly blank rather than no application at all -- and the
shopper is not sent to an error page from which they resubmit, since the pack
has no dedupe and one business would become two rows.

Legal on a **mutation** alone: a logic may call builtins, and a public form
pointed at one would reach them under the site owner's borrowed authority.
**At most one extension per form.** Refusals are load-time, on the
`LoadReport`, so strict boot rejects them -- including an extension naming a
form whose pack this cluster has **disabled**, which declares no forms at all.
Full reasoning:
[the design record](../../superpowers/specs/2026-09-21-shopper-form-extension-design.md).

---

## The storefront-pack convention

`reviewspack` and the wholesale pack ship **concepts, mutations, queries, tools
and builtins, and no automations and no logic**. `deploypack` and
`referencepack` ship both, so this is a choice the storefront packs made rather
than a rule of the platform.

The reason is [decision D6](../concepts/component-integration-pack.md): a pack
is **nouns and invariants**; the client's process is theirs. Two clients with
different approval processes must both be able to express theirs without
editing the pack, so who approves, in what order and what is notified attaches
from the client's own repository:

```memql fragment
use reviews.concepts.{ review }

@trigger(event="node.created", concept="v1:reviews:review")
automation notifyOnReview { ... }
```

Client-specific fields are a **related concept**, not a blob: the client
declares its own concept in its own domain with an `@relationship` to the
pack's, which keeps it typed and queryable and keeps the pack's schema from
drifting toward one client.

---

## A guided tour of the reference pack

The reference pack at [`examples/referencepack/`](../../../examples/referencepack)
is intentionally minimal but real -- it builds, loads into the engine
registries, and extends a core service, proven by Go tests under the default
`go test ./...`. Here is every file and what it demonstrates.

```
examples/referencepack/
├── pack.go                      Go: embed + Domain/ContractVersion + Provider + NewProvider + Register
├── register_referencepack.go    Go: //go:build referencepack -> init() { Register(Domain) }
├── reference_pack_test.go        test: concept load + provider capability + contract gate (exported API)
└── dsl/
    ├── memql.toml                the language line the pack's domain is written in (memql = "1.0", edition = "2026")
    ├── concepts.memql            one concept (greeting, owned-tier ownerUserId)
    ├── builtins.memql            one builtin backed by @executor("integration.referencepack.composeGreeting")
    ├── tools.memql               one tool surfacing the builtin to the agent tool loop
    └── automations.memql         one automation hooking the CORE v1:notes:note node.created event
```

**`dsl/concepts.memql`** -- a single `greeting` concept. Its id assembles from
`@version("1.0.0")` + `@namespace("referencepack")` + the name `greeting` into
`v1:referencepack:greeting`. It carries an `ownerUserId` so it models the
**owned** authorization tier (per-row authz key) the same way core concepts do,
not just an empty schema.

**`dsl/builtins.memql`** -- a single `referencePackComposeGreeting` builtin whose
`@executor("integration.referencepack.composeGreeting")` names the pack's Go
capability. This is the DSL end of the wire; the Go `Provider.Capabilities()`
in `pack.go` is the other end. The FQN's middle segment
(`referencepack`) is the provider's `IntegrationName()`; the last segment
(`composeGreeting`) is the capability `Name`.

**`dsl/tools.memql`** -- a single `referencePackGreet` tool. Its
`@handler(type="function", name="referencePackComposeGreeting")` points at the
builtin by name, so an agent that calls the tool ultimately runs the pack's Go
handler. This is how a pack surfaces a capability into an agent's tool loop.

**`dsl/automations.memql`** -- a single automation that **hooks a core service
event**: `@trigger(event="node.created", concept="v1:notes:note", ...)`.
When the core engine creates a `note` row, this pack-owned automation fires --
the core has no knowledge of the pack. Its step calls the pack's own builtin via
a kind-prefixed call (`builtin referencePackComposeGreeting ( userName: ownerUserId )` -- the owner bound via the automation's typed `args { }` contract, G5 #2367),
so it also exercises the pack's Go capability. The automation pulls the builtin
into scope with a file-top `use referencepack.builtins.{ referencePackComposeGreeting }`
import -- the standard cross-file dependency mechanism.

**`pack.go`** -- the Go core:

- `Domain` and `ContractVersion` consts (the latter pinned to
  `memql.PluginContractVersion` the pack compiled against).
- `Tree() fs.FS` -- the `//go:embed all:dsl` subtree, re-rooted so the files
  appear directly (so `RegisterTree(Domain, Tree())` mounts `concepts.memql` at
  `referencepack/concepts.memql`).
- `Provider` -- the `IntegrationProvider`, with `IntegrationName()` returning
  `referencepack` and `Capabilities()` returning the one `composeGreeting`
  capability whose handler builds a greeting node.
- `NewProvider(pctx)` -- the `PluginFactory`.
- `Register(domain string)` -- the single entry point that does
  `dsl.RegisterTree(domain, Tree())` + `memql.RegisterPluginForContract(domain,
  ContractVersion, NewProvider)`. `domain` is a parameter so a test can mount
  the same tree under a throwaway namespace.

**`register_referencepack.go`** -- the build-tag-gated `init()` (covered above).

### How the tests prove load + extend

Two test files, both running under the default `go test ./...`:

- [`examples/referencepack/reference_pack_test.go`](../../../examples/referencepack/reference_pack_test.go)
  (external `package referencepack_test`, exported API only):
  - mounts `Tree()` under a unique throwaway domain via `dsl.RegisterTree`, with
    `t.Cleanup(dsl.UnregisterTree)`;
  - runs `memql.LoadUnifiedConcepts` (the loader the engine runs at boot) and
    asserts `v1:referencepack:greeting` is now in `memorynodes.DefaultRegistry()`
    -- the pack **extending** the core concept registry;
  - builds `NewProvider` with a minimal `PluginContext` and asserts its
    `Capabilities()` include `composeGreeting` with a non-nil handler;
  - asserts `memql.CheckPluginContractCompat(ContractVersion) == nil` and that a
    `PluginRegistration{...}.ValidateContract()` passes.
- [`component/memql/reference_pack_load_test.go`](../../../component/memql/reference_pack_load_test.go)
  (in-package `package memql`, for the unexported `ToolRegistry` constructor):
  - mounts the pack's real `tools.memql` and runs `LoadUnifiedTools`, asserting
    `referencePackGreet` resolves in the engine's tool registry -- the pack
    **extending** the core tool surface;
  - loads the real `builtins.memql` via `LoadUnifiedBuiltins` and asserts the
    builtin registers;
  - runs `dslimports.Load` over the pack's whole `dsl/` tree, asserting every
    artifact (including the core-service-hook automation and its `use` import)
    parses and resolves.

Together they prove the pack builds, loads into the engine registries alongside
core, and extends both the concept and tool surfaces -- with no database.

---

## Build your own pack: the checklist

1. Create a package directory with a `dsl/` subdir for your `.memql` files,
   and declare the language they are written in with a `dsl/memql.toml`
   (`memqlmigrate --rewrite=language-line -w dsl/` writes it). The engine
   refuses to boot a pack domain without one -- see
   [The language line](../language/memql.md#the-language-line).
2. Write your concept(s) with `@version` + `@namespace("yourdomain")`; model
   authz with an `ownerUserId` (owned tier) or the granted/admin/public pattern.
3. Write any builtins (`@executor("integration.yourdomain.<fn>")`), tools, and
   automations. Cross-file deps go through file-top `use` imports.
4. Implement an `IntegrationProvider` whose `IntegrationName()` matches your
   `@executor` middle segment and whose `Capabilities()` back each builtin.
5. Write a `PluginFactory` (`NewProvider(pctx)`).
6. Embed `dsl/` and write a `Register(domain)` that calls `dsl.RegisterTree` +
   `memql.RegisterPluginForContract` (+ `node.RegisterRoutingRule` if you cross
   nodes).
7. Put the auto-register `init()` in a **build-tag-gated file** for your product
   tag, and anchor the package via a blank import in the app bootstrap.
8. Pick a **unique domain** -- it must not collide with a core domain or another
   pack.
9. Verify with `go build ./...`, `go vet`, and a load-test modeled on the
   reference pack's.

---

## Production dogfood example: the deploy pack

The reference pack is the minimal teaching example. The **deploy pack** at
[`examples/deploypack/`](../../../examples/deploypack) (Epic 2 / #2095) is the
production-shaped sibling: it packages MemQL's OWN deployment workflow as a pack,
dogfooding the model. Same primitives, but its capabilities are the REAL deploy
effects.

```
examples/deploypack/
├── pack.go                   Go: Provider holding a deploycontrol.Executor + engine; 4 effect capabilities
├── register_deploypack.go    Go: //go:build deploypack -> init() { Register(Domain) }
├── deploy_pack_test.go        test: capability exposure + per-effect routing (exported API)
└── dsl/
    ├── memql.toml             the pack domain's language line
    ├── builtins.memql         5 builtins -> @executor("integration.deploypack.{commitOverlay,argoSync,runPromote,recordBack,observeReconciledState}")
    ├── automations.memql      two CDC automations on v1:cluster:deployment status
    └── logic.memql            driveDeploymentInProgress (promote + transition) + recordReconciledState (Model A record-back)
```

The pack also shows a pack **hooking a core CDC event**: `dsl/automations.memql`
triggers on `graph.node.updated.v1:cluster:deployment` and `dsl/logic.memql`
ports `component/deploycontrol/deploy.go`'s imperative apply+transition into a
declarative chain -- when a deployment enters `in_progress`, fire `runPromote`
(the live azure effect) and transition the record to `succeeded`/`failed` on the
in-band outcome. A second automation (`recordReconciledState`, the Model A
record-back loop) observes the ArgoCD-reconciled state via
`observeReconciledState` and appends the observed per-node readiness back into
the deployment concept once the deploy reports `succeeded`. The logic imports the
pack's own effects (`use deploypack.builtins.{ ... }`) AND a core mutation
(`use cluster.mutations.{ updateDeploymentStatus }`) -- the standard
cross-namespace `use` mechanism.

The key difference from the reference pack: the deploy pack's `Provider` holds a
**`deploycontrol.Executor`** (the SAME side-effect boundary the Deploy Console
uses -- `promote.sh` / `git` / `kubectl argo rollouts`) plus an engine handle.
Its `NewProvider(pctx)` builds the Executor anchored at `MEMQL_DEPLOY_REPO_ROOT`
(mirroring `app/integrations_deploy_control.go`) and takes the engine from
`pctx.Engine`. The four capabilities are the deploy effects:

- `commitOverlay` / `argoSync` -> `Executor.Git` (Model A: author + commit the
  overlay, push so ArgoCD reconciles -- never a direct cluster apply).
- `runPromote` -> `Executor.RunPromote` -- THE live azure deploy effect
  (`scripts/release/promote.sh` via the Argo Rollout), invoked through the same
  method the Deploy Console uses. Exposing it through the pack is **additive**;
  the imperative path is untouched until E2.5 thins it.
- `recordBack` -> engine mutations (`updateDeploymentStatus` +
  `createDeploymentNodeSpec`) -- Model A record-back: mirror the
  GitOps-reconciled state into the deployment concept.

This is the canonical example of a pack contributing **effects backed by an
existing Go side-effect boundary** rather than a fresh capability. The E2.3
chained automations fire these effects on deployment status transitions; the
pack is the substrate they call into.

---

## Shipped packs

Beyond the two teaching examples above, the engine tree carries two shipped
packs worth reading as real worked examples:

- `packs/reviewspack` (memql#4139, promoted by epic memql#5532) -- the
  client-agnostic reviews pack, and the worked example of everything the two
  sections above describe: it is in the default build under `packState`, it
  ships disabled, it declares a shopper form and a shopper read, and every row
  it writes carries the store it was written against. A pack with no external
  system behind it.
- `examples/shopifypack` (memql#4138) -- the Shopify product feature (console
  views for shop, secrets, sync), layered on the `integrations/shopify`
  integration. The pair is the canonical worked example of the
  [component / integration / pack](../concepts/component-integration-pack.md)
  split: the integration talks to Shopify; the pack is the product feature;
  the thin product index stays core (`dsl/shopify`).

---

## See also

- [Plugin SDK](plugin-sdk.md) -- the contract reference (PluginContext,
  PluginFactory, primitives, contract version, load-time validation).
- [Build Tags](build-tags.md) -- the node-type tag model that gates which
  binaries carry your pack.
- [Partition scoping](../concepts/partition-scoping.md) -- the canonical tenant
  dimension your pack scopes data to.
