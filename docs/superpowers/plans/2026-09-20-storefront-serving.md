# Epic #5529 -- the storefront kind can serve a storefront

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans.
> Steps use checkbox (`- [ ]`) syntax. **This plan is deleted in the epic's
> merge** -- it is scaffolding for one branch, not a document the tree keeps.

**Goal:** make a `shopify_storefront` site able to actually serve a
storefront -- the edge's policy admits the bound store, the resolution tail
is the site's own choice, and a fixture bundle proves the path runs.

**Architecture:** three engine changes and one doc change, all additive.
`policyForSite` gains a storefront arm derived from the site's resolved
binding. The site row gains `resolutionTail`, absent meaning exactly today's
kind-driven behaviour. A cluster-e2e leg publishes a fixture bundle, serves
it, and asserts the served policy and document against a stub standing in
for the bound store.

**Tech Stack:** Go 1.26, MemQL DSL, cluster-e2e build tag, plain HTML/CSS/JS
for the fixture bundle.

**Spec:** `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md`
(section 6 G1 and G3, section 8 Q1 and the UNVERIFIED host list, section 11).

## Global Constraints

- **`spa` and `static` policies stay BYTE-IDENTICAL.** `policyWithoutHashes`
  in `component/edge/csp_hashes_test.go` is the pin; a twin asserts it per
  kind.
- **Never a wildcard.** Every admitted host is a literal, and the
  store-specific one comes from `site.Binding["storeDomain"]` -- never from
  anything a bundle supplies.
- **`script-src` is NOT widened.** Admitting a third party's script host is
  the one widening that runs somebody else's code in the storefront's own
  origin, and nothing in the binding requires it.
- **`TestSiteKindEnumIsExactlyThreeValues` stays as it is.** The tail is a
  property of the SITE, not a fourth kind.
- **Absent `resolutionTail` == today.** `spa` and `shopify_storefront` fall
  back to `index.html`; `static` answers 404.
- **The Admin token never reaches a served byte.**
  `TestRuntimeConfigNeverCarriesTheShopifyAdminToken` already greps for it.
- Verify with `make test`, never `go test ./...` (CLAUDE.md).

---

### Task 1 (#5534): the policy admits the bound store

**Files:**
- Modify: `component/edge/csp.go` (`policyForSite`)
- Create: `component/edge/csp_storefront.go`, `component/edge/csp_storefront_test.go`

**Interfaces:**
- Produces: `storefrontSources(site *Site) storefrontCSP`, with
  `connect []string`, `img []string`, `media []string`; zero value for every
  non-storefront site.

- [ ] Write the failing tests: a storefront policy names `https://<storeDomain>`
      in connect-src, `https://cdn.shopify.com` in img-src and media-src;
      spa/static byte-identical twins; no `*` anywhere; a storefront with no
      binding gains nothing; a malformed storeDomain is dropped; a binding on
      a `spa` row is ignored; script-src is untouched.
- [ ] Run them, confirm they fail.
- [ ] Implement `csp_storefront.go` and the two-line change in `policyForSite`.
- [ ] Run, confirm pass. Commit.

### Task 2 (#5535): the resolution tail becomes the site's own choice

**Files:**
- Modify: `dsl/platform/concepts.memql` (site), `dsl/platform/shapes.memql`
  (siteFull), `dsl/platform/mutations.memql` (createSite +
  `updateSiteResolutionTail`)
- Modify: `component/edge/resolve.go` (`Site.ResolutionTail`),
  `component/edge/edge.go` (`siteFromRow`), `component/edge/handler.go`
- Modify: `component/packages/manifest.go`, `component/packages/production.go`
- Create: `component/edge/resolution_tail_test.go`

**Interfaces:**
- Produces: `fallsBackToIndex(site *Site) bool` in `handler.go` -- the one
  place the tail is decided.

- [ ] Write failing tests: absent tail reproduces today per kind;
      `not_found` 404s a storefront; `fallback` serves index.html for a
      `static` site; an unrecognised value reads as absent.
- [ ] Run, confirm fail.
- [ ] Add the DSL field, shape path and mutations; thread through the edge
      and the manifest.
- [ ] `make sdk-gen`, `make concept-snapshot`.
- [ ] Run, confirm pass. Commit.

### Task 3 (#5536): the fixture bundle, served end to end

**Files:**
- Create: `test/clustere2e/testdata/storefront-fixture/{index.html,storefront.js,storefront.css}`
- Create: `test/clustere2e/storefront_serving_test.go` (tag `clustere2e`)

- [ ] Build the fixture bundle: reads `/runtime-config.json`, calls the
      Storefront GraphQL endpoint of whatever `storeDomain` it is bound to,
      renders the catalog, loads one image. Clean and minimal per
      `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`: real empty and error
      states, no invented facts, no perpetual motion, reduced-motion honoured.
- [ ] Write the leg: publish the fixture to a `shopify_storefront` site
      bound to a stub host, serve it, assert the policy admits the stub, the
      runtime-config carries the storefront block, the Admin token appears
      nowhere, and the exact fetch the bundle makes returns a catalog.
- [ ] Run against a live cluster where one exists; confirm it SKIPS cleanly
      where one does not. Commit.

### Task 4 (#5537): the docs say what the edge admits

**Files:**
- Modify: `docs/public/operate/shopify-storefront-checklist.md`
- Modify: `docs/public/operate/shopify-connector.md`
- Modify: `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md`
  (section 8's UNVERIFIED line, and D3/Q1's engine half)

- [ ] Write the admitted-host table and where the list comes from.
- [ ] Write the runtime-not-build-time rule for a bundle previewed against
      one store and promoted against another.
- [ ] Cross-link both directions. Commit.
