# Edge and Site Hosting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve any number of web surfaces — SPAs, static websites, and the memQL Portal itself — from one `edge` node, where adding a site is a graph row plus an upload rather than a Kubernetes object.

**Architecture:** A new `edge` node type, generalized from `component/portal`, resolves the request `Host` to a `v1:platform:site` row and serves that site's bundle from the URI in its `bundleRef` (`file://` from the image, `blob://` from storage). Two static Ingress rules — `*.<domain>` and the apex — point at it and never change again. The portal is site #1, seeded, with no special case in the serving path and a test that says so. Publishing uploads a whole bundle under a new version prefix and then flips one field, which makes deploy atomic and rollback a single write.

**Tech Stack:** Go (new node binary + `component/edge`), MemQL DSL (`dsl/platform/`), Azure Blob / azurite via the existing `storage` integration, Kubernetes + kustomize, k3d + traefik locally.

**Spec:** [`docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`](../specs/2026-08-13-cluster-front-door-design.md)

**Depends on:** [`2026-08-13-front-door-rules.md`](2026-08-13-front-door-rules.md) Task 2 must have merged — this plan consumes `api.<domain>` as the derived host and completes the five-host gate that plan's Task 1 installed.

## Global Constraints

- **No emojis** in documentation, script output, or user-facing text (CLAUDE.md). Use `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:` and `[ ]` / `[x]`.
- **Stage files by explicit path.** Never `git add -A` or `git add .`.
- **Every change is a branch + PR.** `main` refuses direct pushes. Merge commits, not squashes.
- **Pre-release: no backwards-compat shims.** When a contract changes, change it and delete what is superseded.
- **Environment parity is non-negotiable.** Same topology, same deploy path, same connection model everywhere. Only values vary.
- **Documentation is an acceptance criterion per task**, not a trailing task.
- **`@default` on a concept field is NEVER applied on insert** (memql#2960). Every default is documentation; mutations fill values with `??` (`args.kind ?? "spa"`). This is the single most likely thing to get wrong in Task 1.
- **`@rowAuthz` has exactly four tiers**: `owner="<field>"`, `clusterOwner`, `via="<spec>"`, `public`. There is no admin tier.
- **Multi-node is the default.** Any state crossing a node boundary needs explicit plumbing; a green single-node test is a false signal. The edge reads rows written by other nodes, so its cache invalidation is cross-node by construction.
- **Strict DSL boot**: a skipped or duplicate construct refuses boot. A construct name must be unique across core plus every registered pack.
- Go 1.26.1, workspace mode for development, `GOWORK=off` per module in the `module-boundaries` CI lane.

---

### Task 1: The `v1:platform:site` concept and its DSL surface

**Files:**
- Modify: `dsl/platform/concepts.memql`
- Modify (or create): `dsl/platform/queries.memql`, `dsl/platform/mutations.memql`, `dsl/platform/shapes.memql`
- Test: `test/dslconformance/conformance_test.go` (classification — should pass without edits)

**Interfaces:**
- Produces: concept `v1:platform:site`; query `siteByHostname(hostname)`; query `sitesAll`; mutations `createSite`, `updateSiteBundle`, `updateSiteStatus`; shape `siteFull`. Tasks 3, 6, 8 and 9 all consume these names — do not rename them.

- [ ] **Step 1: Write the concept**

Append to `dsl/platform/concepts.memql`:

```memql
/// A web surface this cluster serves at a hostname -- a single-page app, a static website, or the
/// platform's own portal. A site is DATA, not infrastructure: deploying one is an upload plus a row
/// write and rolling one back is a row write, so the number of Kubernetes objects does not grow with
/// the number of sites. The graph's own row history IS the version list.
///
/// clusterOwner tier, deliberately, and NOT owner="ownerUserId". enforceRowAuthzOnPlan ANDs the
/// owned predicate unconditionally with no cluster-owner escape on the read path, so on an
/// owner-tier concept "list every site in this cluster" is not merely unimplemented -- it is not
/// EXPRESSIBLE, and the caller silently gets a subset. That query is the portal's primary screen,
/// and cluster-wide hostname uniqueness would be uncheckable for the same reason. The tier matches
/// v1:campaigns:sendJob and :suppression for the same reason those carry it: one deployment, one
/// front door, not any individual operator's rows.
@rowAuthz(clusterOwner)
@displayCard(primary="hostname", secondary="kind", tertiary="title", status="status")
concept site {
  hostname     string!  @description("Fully qualified host this site answers on, e.g. shop.example.com. Cluster-unique -- the uniqueness check is possible only because this concept is not owner-scoped.")
  kind         enum("spa", "static")!  @description("Tail of the request-resolution order. 'spa' falls back to index.html so client-side routing works; 'static' answers 404 so a mistyped path in a multi-page site is visible rather than silently rendering the home page.")
  bundleRef    string!  @description("URI of the bundle to serve. 'blob://sites/<id>/<version>/' for an uploaded bundle, 'file:///path' for one the image ships (the portal) or a working tree (the dev inner loop). The scheme is what makes 'in the image' and 'in storage' a difference in data rather than in code path.")
  status       enum("draft", "live", "disabled")!  @description("'draft' resolves for nobody (404); 'live' serves; 'disabled' answers 503 rather than 404 so a deliberately paused site is distinguishable from a typo'd hostname. NOTE: @default is never applied on insert (memql#2960) -- createSite fills this with ??.")
  apiProxy     bool     @description("Mount /_memql/* on this origin, forwarded to the bff, so the site is same-origin with its own API. Removes CORS and the SameSite=Lax cookie problem entirely.")
  systemOwned  bool     @description("Blocks deletion. The portal only: its row is re-seeded at boot, so an operator cannot brick cluster management by deleting it. Does NOT branch the serving path -- see TestPortalHasNoSpecialCase.")
  title        string   @description("Operator-facing label shown in the portal's site list.")
  notes        string   @description("Operator-facing notes.")
}
```

- [ ] **Step 2: Run the DSL loader to verify it parses**

```bash
make dsl-lint
```

Expected: no diagnostics for `dsl/platform/concepts.memql`. A parse error here refuses boot for every node, so this must be clean before anything else.

- [ ] **Step 3: Write the shape, queries and mutations**

`dsl/platform/shapes.memql`:

```memql
use platform.concepts.{ site }

/// Full projection of v1:platform:site -- what the edge needs to serve a request and what the
/// portal's site list renders.
@description("Full site projection")
@row
shape site siteFull {
  row.id
  hostname
  kind
  bundleRef
  status
  apiProxy
  systemOwned
  title
  notes
  row.createdAt
}
```

`dsl/platform/queries.memql`:

```memql
use platform.concepts.{ site }
use platform.shapes.{ siteFull }

/// Resolve a request Host to the site that answers it. The edge's hot path -- called once per
/// cache miss, not once per request.
@description("The site serving a hostname")
query site siteByHostname {
  args {
    hostname  string  @required
  }
  filter  hostname==args.hostname
  shape   siteFull
}

/// Every site in the cluster. The portal's primary screen, and the read that would silently return
/// a subset if this concept were owner-tier.
@description("Every site in the cluster")
query site sitesAll {
  shape  siteFull
  sort   "hostname", "asc"
}
```

`dsl/platform/mutations.memql`:

```memql
use platform.concepts.{ site }

/// Create a site. Defaults are applied with ?? because a concept-field @default is never applied
/// on insert (memql#2960) -- writing the field without ?? leaves it empty and the edge refuses to
/// serve a row whose status it cannot read.
@description("Create a site")
mutate site createSite {
  args {
    siteId    string  @required
    hostname  string  @required
    kind      string  @enum("spa", "static")
    bundleRef string  @required
    apiProxy  boolean
    title     string
  }
  insert {
    id:          args.siteId
    hostname:    args.hostname
    kind:        args.kind ?? "spa"
    bundleRef:   args.bundleRef
    status:      "draft"
    apiProxy:    args.apiProxy ?? false
    systemOwned: false
    title:       args.title ?? args.hostname
    createdAt:   now
    createdBy:   actor.userId
  }
}

/// Point a site at a different bundle version. THE deploy operation, and THE rollback operation --
/// they are the same write in opposite directions, which is the whole reason bundles are stored
/// under versioned prefixes rather than overwritten.
@description("Point a site at a bundle version")
mutate site updateSiteBundle {
  args {
    siteId    string  @required
    bundleRef string  @required
  }
  update {
    id:        args.siteId
    bundleRef: args.bundleRef
  }
}

/// Move a site between draft / live / disabled.
@description("Set a site's status")
mutate site updateSiteStatus {
  args {
    siteId  string  @required
    status  string  @required  @enum("draft", "live", "disabled")
  }
  update {
    id:     args.siteId
    status: args.status
  }
}
```

- [ ] **Step 4: Run the DSL and conformance gates**

```bash
make dsl-lint
go test ./test/dslconformance/ -v
```

Expected: PASS. The classification test buckets these constructs by their concept's `clusterOwner` tier. If `TestPerRowAuthzClassification` flags anything, read what bucket it landed in — do **not** silence it with `@public`, which is matched ahead of every other tier, carries no runtime semantics, and permanently blocks tier inference for the concept afterwards.

- [ ] **Step 5: Verify the engine boots with the new constructs**

```bash
go run ./cmd/memqllint 2>&1 | tail -20
go build ./... && go test ./component/memql/ -run 'DSL|Duplicate|RowAuthz' -count=1
```

Expected: no load problems, no duplicate construct names. `createSite` / `sitesAll` / `siteFull` must not collide with anything in core or a pack.

- [ ] **Step 6: Commit**

```bash
git add dsl/platform/concepts.memql dsl/platform/queries.memql \
        dsl/platform/mutations.memql dsl/platform/shapes.memql
git commit -m "feat(dsl): add v1:platform:site

A site is data, not infrastructure: deploy is an upload plus a row write,
rollback is a row write, and the graph's own history is the version list.

clusterOwner tier deliberately, not owner=: the owned predicate is ANDed
unconditionally with no cluster-owner escape on the read path, so 'list
every site' would not be EXPRESSIBLE on an owner-tier concept -- and that
is the portal's primary screen. Cluster-wide hostname uniqueness would be
uncheckable for the same reason.

Defaults are written with ?? in the mutations because a concept-field
@default is never applied on insert (memql#2960)."
```

---

### Task 2: The `edge` node type skeleton

Boots, joins the mesh, serves nothing. Split from Task 3 because a reviewer can meaningfully accept "a new node type exists and is wired like the others" while rejecting how it resolves sites.

**Files:**
- Create: `app/build_edge.go`, `app/transport_edge.go`, `main_edge.go`
- Modify: `Makefile` (target near line 88, `.PHONY` at line 46)
- Modify: `Dockerfile` (a stage selected by `BUILD_TAGS=edge`)
- Modify: `docs/public/build/build-tags.md`

**Interfaces:**
- Consumes: `app.newApp`, `a.configAndAuth()`, `a.databaseAndConcepts()`, `a.engineAndBus()`, `a.integrationsCore()`, `a.cluster()` — the phase helpers `app/build_mcp.go` uses.
- Produces: build tag `edge`, binary `bin/memql-edge`, `App.transportEdge()`. Tasks 3, 7 and 9 extend this.

- [ ] **Step 1: Write the failing build test**

```go
// app/build_edge_test.go
//go:build edge

package app

import "testing"

// The edge node must wire the same phases every other node type does, so its
// health, lifecycle and mesh membership behave identically. A node type that
// skips a phase looks fine until the thing that phase provides is needed.
func TestEdgeBuildWiresTheStandardPhases(t *testing.T) {
	a := newApp(testLogger(t), "test", Overrides{})
	if a == nil {
		t.Fatal("newApp returned nil")
	}
	// Compile-time assertion that the transport exists; the behavioural
	// coverage is in component/edge (Task 3).
	var _ = (*App).transportEdge
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test -tags edge ./app/ -run TestEdgeBuildWiresTheStandardPhases -v
```

Expected: FAIL to build — `a.transportEdge undefined`. If `testLogger` does not exist in the `app` package, use whatever helper the neighbouring `app/*_test.go` files use rather than adding one.

- [ ] **Step 3: Write the node bootstrap**

`app/build_edge.go`:

```go
//go:build edge

package app

import "log/slog"

// Build constructs service dependencies for an EDGE node -- the surface that
// serves this cluster's web surfaces: every hosted SPA and website, and the
// memQL Portal itself, which is site #1 and takes no special path.
//
// It is a node type rather than a handler bolted onto the bff for three
// reasons. A website-hosting cluster should be able to drop to four nodes
// (identity + bff + edge + Postgres), which it cannot if the edge rides the
// API node. A site deploy must never share fate with the API. And per-node-type
// binaries selected by build tag is the pattern this repository already uses
// for exactly this kind of separation.
//
// Edge nodes need config/auth, database/concepts (they read v1:platform:site),
// engine/bus, core integrations (storage, for bundles) and the cluster mesh.
// They do NOT need the voice pipeline, the cognition pipeline, file processing
// or the agent tool surface.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsCore()
	a.transportEdge()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
```

`app/transport_edge.go`:

```go
//go:build edge

package app

// transportEdge mounts the site-serving handler on the node's HTTP server.
// The handler itself lives in component/edge; this file is only the wiring, so
// that what the edge DOES stays testable without booting an App.
func (a *App) transportEdge() {
	a.httpTransport()
}
```

`main_edge.go`:

```go
//go:build edge

package main

// The edge binary needs no init-time special casing. It is here so the build
// tag selects a main file the way every other node type does, and so a reader
// grepping for a node type finds all three of its files together.
```

- [ ] **Step 4: Run the test**

```bash
go test -tags edge ./app/ -run TestEdgeBuildWiresTheStandardPhases -v
go build -tags edge -o /tmp/memql-edge . && echo BUILD OK
```

Expected: PASS, and the binary builds. If `a.httpTransport()` is not the right helper name, read `app/transport.go` and use the one the bff uses to mount its HTTP server.

- [ ] **Step 5: Wire the Makefile and Dockerfile**

Makefile, after the `mcp:` target and in the `.PHONY` list at line 46:

```make
## Build edge node binary (serves hosted sites + the portal)
edge:
	$(GO) build $(GOFLAGS) -tags edge -o $(BIN_DIR)/memql-edge .
```

In `Dockerfile`, follow the existing `BUILD_TAGS` stage pattern. The edge stage must also copy the portal bundle, because the portal is site #1 and its `bundleRef` is `file:///app/portal` — read how the bff stage does it and mirror that, rather than inventing a second way to get the bundle into an image.

- [ ] **Step 6: Verify the build end to end**

```bash
make edge && ls -la bin/memql-edge
docker build --build-arg BUILD_TAGS=edge -t memql-edge:local . && echo IMAGE OK
```

- [ ] **Step 7: Document the tag**

Add `edge` to `docs/public/build/build-tags.md` with its purpose and what it excludes, and to the node-type list in `CLAUDE.md` (the "Distributed Node Architecture" section and the environment-parity node list).

- [ ] **Step 8: Commit**

```bash
git add app/build_edge.go app/transport_edge.go app/build_edge_test.go \
        main_edge.go Makefile Dockerfile \
        docs/public/build/build-tags.md CLAUDE.md
git commit -m "feat: add the edge node type

Its own node type rather than a handler on the bff: a website-hosting
cluster should drop to four nodes, a site deploy must never share fate with
the API, and per-node-type binaries is the pattern this repo already uses.

Boots, joins the mesh, serves nothing yet."
```

---

### Task 3: Hostname resolution and the `file://` bundle reader

**Files:**
- Create: `component/edge/edge.go`, `component/edge/resolve.go`, `component/edge/bundle.go`
- Create: `component/edge/edge_test.go`, `component/edge/resolve_test.go`
- Reference: `component/portal/portal.go` (the handler this generalizes), `component/portal/csp.go`

**Interfaces:**
- Consumes: `siteByHostname` (Task 1), an engine executor for queries.
- Produces:
  - `type Site struct { ID, Hostname, Kind, BundleRef, Status, Title string; APIProxy, SystemOwned bool }`
  - `type Resolver interface { Resolve(ctx context.Context, hostname string) (*Site, error) }`
  - `func NewResolver(exec QueryExecutor, ttl time.Duration) Resolver`
  - `type BundleOpener interface { Open(ref string) (fs.FS, error) }`
  - `func NewHandler(opts Options) *Handler` where `Options{ Resolver, Opener, Logger }`
  Tasks 4, 5, 6 and 7 all build on these.

- [ ] **Step 1: Write the failing resolution test**

```go
// component/edge/resolve_test.go
package edge

import (
	"context"
	"testing"
	"time"
)

type stubExec struct {
	calls int
	rows  map[string]*Site
}

func (s *stubExec) SiteByHostname(_ context.Context, hostname string) (*Site, error) {
	s.calls++
	return s.rows[hostname], nil
}

func TestResolveFindsTheSiteForAHostname(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	got, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil || got.ID != "site:1" {
		t.Fatalf("Resolve returned %+v, want site:1", got)
	}
}

// A miss is a nil site and no error -- an unknown hostname is a 404, not a
// server fault. Returning an error here would turn every scan of the front
// door into a page of 500s in the logs.
func TestResolveMissReturnsNilWithoutError(t *testing.T) {
	r := NewResolver(&stubExec{rows: map[string]*Site{}}, time.Minute)

	got, err := r.Resolve(context.Background(), "nope.example.com")
	if err != nil {
		t.Fatalf("Resolve returned an error for a miss: %v", err)
	}
	if got != nil {
		t.Fatalf("Resolve returned %+v for a miss, want nil", got)
	}
}

// The resolution is per-request on the hot path, so it caches. Without this
// every asset fetch on every page becomes a database round trip.
func TestResolveCachesWithinTheTTL(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "shop.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if ex.calls != 1 {
		t.Errorf("executor called %d times, want 1 -- the resolver is not caching", ex.calls)
	}
}

// A miss caches too. Otherwise a scanner hitting random hostnames drives one
// query per request, which is a denial-of-service amplifier pointed at the
// database.
func TestResolveCachesMisses(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{}}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "nope.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if ex.calls != 1 {
		t.Errorf("executor called %d times for a miss, want 1", ex.calls)
	}
}

// The Host header carries a port on a non-default port, and browsers vary on
// case. Neither should produce a miss.
func TestResolveNormalizesTheHostHeader(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	for _, in := range []string{"shop.example.com:443", "Shop.Example.Com", "SHOP.EXAMPLE.COM:8443"} {
		got, err := r.Resolve(context.Background(), in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", in, err)
		}
		if got == nil {
			t.Errorf("Resolve(%q) missed; the Host header was not normalized", in)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/edge/ -v
```

Expected: FAIL to build — the package does not exist.

- [ ] **Step 3: Write the resolver**

```go
// Package edge serves this cluster's web surfaces: every hosted SPA and
// website, and the memQL Portal, which is site #1 and takes no special path.
//
// It is component/portal generalized. That package serves exactly one bundle
// from a directory named by an env var; this one resolves the request Host to
// a v1:platform:site row and serves the bundle that row names. The portal
// keeps working because its row's bundleRef is file:///app/portal -- the same
// directory, reached through the general mechanism.
package edge

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Site is the projection of v1:platform:site the edge needs to serve a request.
type Site struct {
	ID          string
	Hostname    string
	Kind        string // "spa" | "static"
	BundleRef   string
	Status      string // "draft" | "live" | "disabled"
	Title       string
	APIProxy    bool
	SystemOwned bool
}

// QueryExecutor is the narrow read the resolver needs. Narrow deliberately:
// the edge should not be able to reach the rest of the graph.
type QueryExecutor interface {
	SiteByHostname(ctx context.Context, hostname string) (*Site, error)
}

// Resolver maps a request Host to a Site.
type Resolver interface {
	Resolve(ctx context.Context, hostname string) (*Site, error)
	Invalidate(hostname string)
}

type entry struct {
	site *Site
	at   time.Time
}

type resolver struct {
	exec QueryExecutor
	ttl  time.Duration

	mu    sync.RWMutex
	cache map[string]entry
}

// NewResolver returns a caching Resolver. The TTL bounds staleness after a
// site row changes on ANOTHER node -- the write lands wherever the portal is
// served from, and this cache lives on every edge replica, so the TTL is the
// backstop behind the change-feed invalidation in Task 9.
func NewResolver(exec QueryExecutor, ttl time.Duration) Resolver {
	return &resolver{exec: exec, ttl: ttl, cache: map[string]entry{}}
}

// normalizeHost strips the port and lowercases. A Host header carries a port
// whenever the listener is not on the scheme's default, and browsers do not
// agree on case.
func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

func (r *resolver) Resolve(ctx context.Context, hostname string) (*Site, error) {
	key := normalizeHost(hostname)

	r.mu.RLock()
	e, ok := r.cache[key]
	r.mu.RUnlock()
	if ok && time.Since(e.at) < r.ttl {
		return e.site, nil
	}

	site, err := r.exec.SiteByHostname(ctx, key)
	if err != nil {
		return nil, err
	}

	// A MISS IS CACHED TOO. Without this, a scanner walking random hostnames
	// drives one database query per request -- an amplifier pointed at the
	// database, reachable by anyone who can resolve the wildcard.
	r.mu.Lock()
	r.cache[key] = entry{site: site, at: time.Now()}
	r.mu.Unlock()

	return site, nil
}

func (r *resolver) Invalidate(hostname string) {
	key := normalizeHost(hostname)
	r.mu.Lock()
	delete(r.cache, key)
	r.mu.Unlock()
}
```

- [ ] **Step 4: Run the resolver tests**

```bash
go test ./component/edge/ -v
```

Expected: all five PASS.

- [ ] **Step 5: Write the failing bundle-opener test**

```go
// component/edge/bundle_test.go
package edge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSchemeOpensADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	fsys, err := NewFileOpener().Open("file://" + dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := fsReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if string(b) != "<html>hi" {
		t.Errorf("got %q", b)
	}
}

// An unknown scheme is an error, not a silent fallback to a local path. A
// bundleRef the edge cannot honour must surface as a broken site, because the
// alternative is serving the wrong bytes from somewhere plausible.
func TestUnknownSchemeIsRefused(t *testing.T) {
	if _, err := NewFileOpener().Open("gopher://example.com/x"); err == nil {
		t.Error("Open accepted an unknown scheme")
	}
}
```

- [ ] **Step 6: Run it, write the opener, run it again**

```go
// component/edge/bundle.go
package edge

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// BundleOpener turns a site's bundleRef into a filesystem to serve from. The
// scheme is what makes "shipped in the image" and "uploaded to storage" a
// difference in DATA rather than in code path -- which is what lets the portal
// be site #1 with no special case.
type BundleOpener interface {
	Open(ref string) (fs.FS, error)
}

type fileOpener struct{}

// NewFileOpener handles file:// -- the bundle the image ships (the portal) and
// a working tree (the dev inner loop). Task 4 composes this with blob://.
func NewFileOpener() BundleOpener { return fileOpener{} }

func (fileOpener) Open(ref string) (fs.FS, error) {
	const scheme = "file://"
	if !strings.HasPrefix(ref, scheme) {
		return nil, fmt.Errorf("edge: bundleRef %q is not a file:// reference", ref)
	}
	dir := strings.TrimPrefix(ref, scheme)
	if dir == "" {
		return nil, fmt.Errorf("edge: bundleRef %q names no directory", ref)
	}
	// os.DirFS is rooted: a path that would escape the directory is refused by
	// the filesystem itself.
	return os.DirFS(dir), nil
}

func fsReadFile(fsys fs.FS, name string) ([]byte, error) { return fs.ReadFile(fsys, name) }
```

```bash
go test ./component/edge/ -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add component/edge/
git commit -m "feat(edge): resolve a Host to a site, and open a file:// bundle

component/portal generalized: that package serves one bundle from a
directory named by an env var; this resolves the request Host to a
v1:platform:site row and serves the bundle that row names.

Misses are cached as well as hits -- without that, a scanner walking random
hostnames drives one query per request, which is an amplifier pointed at
the database and reachable by anyone who can resolve the wildcard."
```

---

### Task 4: The `blob://` scheme

**Files:**
- Create: `component/edge/blob.go`, `component/edge/blob_test.go`
- Modify: `component/edge/bundle.go` (a multiplexing opener)

**Interfaces:**
- Consumes: `BundleOpener` (Task 3); the `storage` integration (`integrations/azureblob`, exposed to DSL as `integration.storage.upload`).
- Produces: `func NewBlobOpener(client BlobClient) BundleOpener`, `func NewMuxOpener(openers map[string]BundleOpener) BundleOpener`. Task 5 consumes the mux.

- [ ] **Step 1: Write the failing test**

```go
// component/edge/blob_test.go
package edge

import (
	"context"
	"testing"
)

type stubBlob struct {
	objects map[string][]byte
}

func (s *stubBlob) Get(_ context.Context, key string) ([]byte, error) {
	b, ok := s.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func TestBlobSchemeReadsAVersionedPrefix(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{
		"sites/site-1/v3/index.html": []byte("<html>v3"),
	}}

	fsys, err := NewBlobOpener(c).Open("blob://sites/site-1/v3/")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := fsReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if string(b) != "<html>v3" {
		t.Errorf("got %q", b)
	}
}

// Two versions coexist. That is the entire reason bundles go under a
// versioned prefix rather than overwriting: rollback is a row write, and the
// bytes it points back at have to still be there.
func TestBlobVersionsCoexist(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{
		"sites/site-1/v2/index.html": []byte("<html>v2"),
		"sites/site-1/v3/index.html": []byte("<html>v3"),
	}}
	o := NewBlobOpener(c)

	for ref, want := range map[string]string{
		"blob://sites/site-1/v2/": "<html>v2",
		"blob://sites/site-1/v3/": "<html>v3",
	} {
		fsys, err := o.Open(ref)
		if err != nil {
			t.Fatalf("Open(%q): %v", ref, err)
		}
		b, _ := fsReadFile(fsys, "index.html")
		if string(b) != want {
			t.Errorf("Open(%q) served %q, want %q", ref, b, want)
		}
	}
}

// A bundleRef must not be able to read outside its own prefix. The ref comes
// from a row an operator wrote, but the request path comes from the internet.
func TestBlobRefusesPathEscape(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{"secrets/key": []byte("nope")}}

	fsys, _ := NewBlobOpener(c).Open("blob://sites/site-1/v3/")
	if _, err := fsReadFile(fsys, "../../../secrets/key"); err == nil {
		t.Error("the blob opener served a path outside its prefix")
	}
}

func TestMuxOpenerRoutesByScheme(t *testing.T) {
	mux := NewMuxOpener(map[string]BundleOpener{
		"file": NewFileOpener(),
		"blob": NewBlobOpener(&stubBlob{objects: map[string][]byte{}}),
	})
	if _, err := mux.Open("gopher://x"); err == nil {
		t.Error("the mux accepted an unknown scheme")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/edge/ -run 'TestBlob|TestMux' -v
```

Expected: FAIL to build — `NewBlobOpener`, `NewMuxOpener`, `ErrNotFound` undefined.

- [ ] **Step 3: Write the blob opener**

```go
// component/edge/blob.go
package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// ErrNotFound is what a BlobClient returns for a key that is not there. A
// distinct error rather than a nil slice, so "empty file" and "no file" stay
// distinguishable -- an empty index.html is a broken deploy, a missing one is
// a 404.
var ErrNotFound = errors.New("edge: object not found")

// BlobClient is the narrow read the edge needs from object storage. Narrow
// deliberately: the edge never writes (that is the publisher's job, Task 8)
// and never lists.
type BlobClient interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

type blobOpener struct{ client BlobClient }

// NewBlobOpener handles blob:// -- an uploaded bundle under a versioned
// prefix. Versions coexist, which is what makes rollback a single row write
// pointing back at bytes that are still there.
func NewBlobOpener(c BlobClient) BundleOpener { return blobOpener{client: c} }

func (b blobOpener) Open(ref string) (fs.FS, error) {
	const scheme = "blob://"
	if !strings.HasPrefix(ref, scheme) {
		return nil, fmt.Errorf("edge: bundleRef %q is not a blob:// reference", ref)
	}
	prefix := strings.TrimPrefix(ref, scheme)
	if prefix == "" {
		return nil, fmt.Errorf("edge: bundleRef %q names no prefix", ref)
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &blobFS{client: b.client, prefix: prefix}, nil
}

// blobFS presents one bundle prefix as a filesystem.
//
// THE PREFIX IS A BOUNDARY, NOT A CONVENTION. The bundleRef comes from a row
// an operator wrote, but the request path comes from the internet, so the
// join is where a traversal would be introduced. fs.ValidPath rejects "..",
// leading slashes and empty segments outright, which is why the check is a
// refusal rather than a sanitising rewrite -- there is no legitimate request
// for a path outside the bundle to repair.
type blobFS struct {
	client BlobClient
	prefix string
}

func (b *blobFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	data, err := b.client.Get(context.Background(), b.prefix+name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &blobFile{name: name, data: data}, nil
}

type blobFile struct {
	name string
	data []byte
	off  int
}

func (f *blobFile) Stat() (fs.FileInfo, error) { return blobInfo{name: f.name, size: int64(len(f.data))}, nil }
func (f *blobFile) Close() error               { return nil }

func (f *blobFile) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

// Seek is what makes http.ServeContent work -- range requests, and the
// Content-Length it sets without buffering. Without it every asset is served
// with chunked encoding and no range support.
func (f *blobFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(f.off) + offset
	case io.SeekEnd:
		abs = int64(len(f.data)) + offset
	default:
		return 0, fmt.Errorf("edge: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("edge: negative seek position")
	}
	f.off = int(abs)
	return abs, nil
}

type blobInfo struct {
	name string
	size int64
}

func (i blobInfo) Name() string       { return i.name }
func (i blobInfo) Size() int64        { return i.size }
func (i blobInfo) Mode() fs.FileMode  { return 0o444 }
func (i blobInfo) ModTime() time.Time { return time.Time{} }
func (i blobInfo) IsDir() bool        { return false }
func (i blobInfo) Sys() any           { return nil }
```

- [ ] **Step 4: Write the scheme mux**

Append to `component/edge/bundle.go`:

```go
type muxOpener struct{ openers map[string]BundleOpener }

// NewMuxOpener dispatches a bundleRef to the opener for its scheme. An
// unknown scheme is an ERROR, never a fallback to a local path: a bundleRef
// the edge cannot honour must surface as a broken site, because the
// alternative is confidently serving the wrong bytes from somewhere
// plausible.
func NewMuxOpener(openers map[string]BundleOpener) BundleOpener {
	return muxOpener{openers: openers}
}

func (m muxOpener) Open(ref string) (fs.FS, error) {
	i := strings.Index(ref, "://")
	if i < 0 {
		return nil, fmt.Errorf("edge: bundleRef %q has no scheme", ref)
	}
	o, ok := m.openers[ref[:i]]
	if !ok {
		return nil, fmt.Errorf("edge: bundleRef %q uses unsupported scheme %q", ref, ref[:i])
	}
	return o.Open(ref)
}
```

- [ ] **Step 5: Wire the real client and run the tests**

Back the `BlobClient` with the storage integration that already exists — read `integrations/azureblob/` and adapt it rather than adding a second Azure client. Locally that is azurite, which `deploy/k8s/overlays/local/azurite.yaml` already runs.

```bash
go test ./component/edge/ -v
```

Expected: PASS, all four.

- [ ] **Step 6: Commit**

```bash
git add component/edge/blob.go component/edge/blob_test.go component/edge/bundle.go
git commit -m "feat(edge): serve bundles from blob storage under a versioned prefix

Versions coexist, which is the entire reason bundles are written under a
versioned prefix rather than overwritten: rollback is a row write, and the
bytes it points back at have to still be there.

The prefix is a boundary, not a convention -- the ref comes from a row an
operator wrote but the request path comes from the internet."
```

---

### Task 5: The request-resolution order, kind and status

**Files:**
- Create: `component/edge/handler.go`, `component/edge/handler_test.go`
- Reference: `component/portal/portal.go:124-230` (ServeHTTP, serveFile, assetName, noCache), `component/portal/csp.go`

**Interfaces:**
- Consumes: `Resolver`, `BundleOpener` (Tasks 3-4).
- Produces: `func NewHandler(Options) *Handler` implementing `http.Handler`, with `Options{ Resolver Resolver; Opener BundleOpener; Logger *slog.Logger }`. Tasks 6, 7 and 9 mount it.

- [ ] **Step 1: Write the failing test**

```go
// component/edge/handler_test.go
package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, site *Site, files map[string]string, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Options{
		Resolver: staticResolver{site: site},
		Opener:   mapOpener(files),
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// D11's resolution order, one case per rung. A prerendered route is a real
// file on disk (products/shoe.html), which is why the .html rung exists at
// all -- without it, prerendering buys nothing because the SPA fallback would
// serve index.html for every route.
func TestResolutionOrder(t *testing.T) {
	files := map[string]string{
		"index.html":          "ROOT",
		"about/index.html":    "ABOUT-DIR",
		"products/shoe.html":  "SHOE-PRERENDERED",
		"assets/app.js":       "JS",
	}
	live := &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}

	for _, tc := range []struct{ path, want string }{
		{"/assets/app.js", "JS"},               // exact file
		{"/about", "ABOUT-DIR"},                // <path>/index.html
		{"/products/shoe", "SHOE-PRERENDERED"}, // <path>.html
		{"/cart/anything", "ROOT"},             // spa fallback
	} {
		rec := serve(t, live, files, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)
			continue
		}
		if rec.Body.String() != tc.want {
			t.Errorf("GET %s served %q, want %q", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// A static site 404s an unknown path instead of falling back. A multi-page
// site that silently renders its home page for every typo hides broken links
// from the people who could fix them.
func TestStaticKindDoesNotFallBack(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "static"},
		map[string]string{"index.html": "ROOT"},
		"/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("static site GET /nope = %d, want 404", rec.Code)
	}
}

func TestUnknownHostnameIs404(t *testing.T) {
	rec := serve(t, nil, map[string]string{}, "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown hostname = %d, want 404", rec.Code)
	}
}

func TestDraftSiteIs404(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "draft", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft site = %d, want 404", rec.Code)
	}
}

// 503, NOT 404. A deliberately paused site and a typo'd hostname are
// different situations and the operator debugging one needs to tell them
// apart.
func TestDisabledSiteIs503(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "disabled", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled site = %d, want 503", rec.Code)
	}
}

// The CSP names the SITE's own origin, not the identity base URL the portal
// handler uses today. A shared policy across every hosted site would be
// either uselessly permissive or wrong for most of them.
func TestCSPNamesTheSiteOrigin(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header")
	}
	if !containsOrigin(csp, "https://shop.example.com") {
		t.Errorf("CSP does not name the site origin: %q", csp)
	}
}

// index.html must never be cached: it is how a deploy reaches a returning
// visitor. Fingerprinted assets may be cached hard.
func TestIndexIsNotCachedButAssetsAre(t *testing.T) {
	files := map[string]string{"index.html": "ROOT", "assets/app.js": "JS"}
	live := &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}

	if cc := serve(t, live, files, "/").Header().Get("Cache-Control"); !isNoCache(cc) {
		t.Errorf("index.html Cache-Control = %q, want no-cache", cc)
	}
	if cc := serve(t, live, files, "/assets/app.js").Header().Get("Cache-Control"); isNoCache(cc) {
		t.Errorf("asset Cache-Control = %q, want a cacheable policy", cc)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/edge/ -run 'TestResolution|TestStatic|TestUnknown|TestDraft|TestDisabled|TestCSP|TestIndex' -v
```

Expected: FAIL to build — `NewHandler`, `Options`, `staticResolver`, `mapOpener`, `containsOrigin`, `isNoCache` undefined. Write the test helpers (`staticResolver`, `mapOpener` over `fstest.MapFS`, `containsOrigin`, `isNoCache`) in the test file.

- [ ] **Step 3: Write the handler**

```go
// component/edge/handler.go
package edge

import (
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Options configures a Handler. APITarget is consumed in Task 7; a Handler
// built without it simply refuses /_memql/* for every site.
type Options struct {
	Resolver  Resolver
	Opener    BundleOpener
	Logger    *slog.Logger
	APITarget string
}

// Handler serves whichever site the request's Host names.
type Handler struct {
	resolver  Resolver
	opener    BundleOpener
	logger    *slog.Logger
	apiTarget string
}

var _ http.Handler = (*Handler)(nil)

func NewHandler(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		resolver:  opts.Resolver,
		opener:    opts.Opener,
		logger:    logger,
		apiTarget: opts.APITarget,
	}
}

const apiPrefix = "/_memql/"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	site, err := h.resolver.Resolve(r.Context(), r.Host)
	if err != nil {
		h.logger.Error("edge: resolving the host failed",
			"component", "edge", "host", r.Host, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// STATUS BEFORE ANY FILE LOOKUP. An unknown host and a draft site are both
	// 404 -- neither exists as far as the internet is concerned. A DISABLED
	// site is 503, deliberately: a deliberately paused site and a typo'd
	// hostname are different situations, and the operator debugging one needs
	// to tell them apart from the status code alone.
	switch {
	case site == nil, site.Status == "draft":
		http.NotFound(w, r)
		return
	case site.Status == "disabled":
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return
	}

	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		h.serveAPI(w, r, site)
		return
	}

	fsys, err := h.opener.Open(site.BundleRef)
	if err != nil {
		h.logger.Error("edge: opening the bundle failed",
			"component", "edge", "site", site.ID, "bundleRef", site.BundleRef, "err", err)
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return
	}

	securityHeaders(w, r)
	w.Header().Set("Content-Security-Policy", policyForSite(r, site))

	if name, ok := resolveAsset(fsys, r.URL.Path); ok {
		h.serveFile(w, r, fsys, name)
		return
	}

	// The last rung of D11's order, and the only place kind is consulted. A
	// static site 404s so a mistyped path in a multi-page site is visible;
	// an spa falls back so client-side routing works.
	if site.Kind == "spa" {
		if _, err := fs.Stat(fsys, "index.html"); err == nil {
			h.serveFile(w, r, fsys, "index.html")
			return
		}
	}
	noCache(w)
	http.NotFound(w, r)
}

// resolveAsset walks D11's resolution order and returns the first name that
// exists: the exact file, then <path>/index.html, then <path>.html.
//
// The .html rung is what makes prerendering worth doing. Without it the spa
// fallback would serve index.html for every route, so a crawler would see one
// page no matter how many the build emitted.
func resolveAsset(fsys fs.FS, urlPath string) (string, bool) {
	clean := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if clean == "" || clean == "." {
		clean = "index.html"
	}
	for _, candidate := range []string{clean, path.Join(clean, "index.html"), clean + ".html"} {
		// fs.ValidPath is the backstop behind the rooted filesystem: it
		// rejects "..", leading slashes and empty segments outright, and
		// there is no legitimate request outside the bundle to repair.
		if !fs.ValidPath(candidate) {
			continue
		}
		if info, err := fs.Stat(fsys, candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		noCache(w)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	// index.html is NEVER cached: it is how a deploy reaches a returning
	// visitor. Everything else is content-addressed by the build, so it may
	// be cached hard.
	if name == "index.html" || strings.HasSuffix(name, "/index.html") {
		noCache(w)
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	if rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}); ok {
		info, _ := fs.Stat(fsys, name)
		var modTime = info.ModTime()
		http.ServeContent(w, r, path.Base(name), modTime, rs)
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		noCache(w)
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}
```

And the per-site policy, in `component/edge/csp.go` — ported from `component/portal/csp.go` with the one change that matters:

```go
// component/edge/csp.go
package edge

import "net/http"

// policyForSite builds the CSP from the SITE's own origin.
//
// component/portal/csp.go derives connect-src from MEMQL_IDENTITY_BASE_URL,
// which is correct when there is exactly one bundle on one origin. With many
// sites on many origins, one shared policy is either uselessly permissive
// (every origin allowed everywhere) or wrong for most of them. The site is
// same-origin with its own API through /_memql/* (D9), so its own origin is
// the whole of what it needs.
func policyForSite(r *http.Request, site *Site) string {
	origin := httpOriginOf(r) // scheme://host, from the request
	return "default-src 'self'; " +
		"connect-src 'self' " + wsOriginOf(origin) + "; " +
		"img-src 'self' data: blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"
}
```

Port `httpOriginOf`, `wsOriginOf` (from `webSocketOrigin`), `validHost` and `securityHeaders` from `component/portal/csp.go` unchanged — they are already correct and already tested.

- [ ] **Step 4: Run the tests**

```bash
go test ./component/edge/ -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add component/edge/handler.go component/edge/handler_test.go
git commit -m "feat(edge): request resolution, kind and status

Resolution order is exact file, then <path>/index.html, then <path>.html,
then the spa fallback. The .html rung is what makes prerendering worth
doing: without it the fallback serves index.html for every route and a
crawler sees one page.

disabled answers 503, not 404 -- a deliberately paused site and a typo'd
hostname are different situations and the operator needs to tell them
apart. CSP names the site's own origin, since one policy shared across
every hosted site is either uselessly permissive or wrong for most."
```

---

### Task 6: The portal is site #1

**Files:**
- Create: `dsl/platform/seeds.memql`
- Create: `component/edge/dogfood_test.go`
- Modify: `component/portal/doc.go` (its rationale becomes the edge's general case)
- Delete: `component/portal/` once nothing imports it — check with `grep -rn "component/portal" --include="*.go" .`

**Interfaces:**
- Consumes: the concept and mutations from Task 1; the handler from Task 5.
- Produces: the seeded row `site:portal`.

- [ ] **Step 1: Write the failing dogfood test**

```go
// component/edge/dogfood_test.go
package edge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE DOGFOOD GATE. The portal is site #1: same concept, same resolution,
// same bundle opener, same headers as any customer site. The only thing that
// differs is where its hostname comes from -- the cluster install rather than
// the portal UI -- and that is data, not a code path.
//
// This is a source scan rather than a behavioural test because the failure it
// prevents is a branch someone adds later "just for the portal", which every
// behavioural test would still pass.
func TestPortalHasNoSpecialCaseInTheServingPath(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v := strings.ToLower(strings.Trim(lit.Value, `"`))
			if strings.Contains(v, "portal") {
				t.Errorf("%s:%d names the portal in the serving path: %s\n"+
					"The portal is site #1 and must take the same path as any other "+
					"site. If it needs different DATA, put it in the seeded row.",
					name, fset.Position(lit.Pos()).Line, lit.Value)
			}
			return true
		})
	}
}
```

- [ ] **Step 2: Run it**

```bash
go test ./component/edge/ -run TestPortalHasNoSpecialCase -v
```

Expected: PASS if Tasks 3-5 were written without a portal branch. If it fails, the finding is real — remove the branch rather than the assertion.

- [ ] **Step 3: Write the seed**

```memql
use platform.concepts.{ site }

/// The memQL Portal, as site #1.
///
/// SEEDED, not special-cased. The portal is how sites are managed, so if it
/// were an ordinary row the cluster would ship with an empty site table and no
/// way to reach the screen that fills it. Seeding resolves that without a
/// branch in the serving path -- and re-seeding at boot means an operator
/// cannot brick cluster management by deleting the row.
///
/// bundleRef is file:///app/portal -- the directory the image already ships
/// and that MEMQL_PORTAL_DIST already pointed at. That is the whole of the
/// difference between "in the image" and "in storage": a URI scheme.
///
/// The hostname is the ONE thing a customer site gets from the portal UI and
/// the portal gets from the cluster install.
@description("The memQL Portal as site #1")
seed site portal {
  id:          "portal"
  hostname:    "portal.memql.localhost"
  kind:        "spa"
  bundleRef:   "file:///app/portal"
  status:      "live"
  apiProxy:    true
  systemOwned: true
  title:       "memQL Portal"
}
```

The hostname carries the committed default the same way the Ingress hosts do (memql#3593); an install on another domain overrides it through the same seam.

- [ ] **Step 4: Verify the seed loads and the row appears**

```bash
make dsl-lint
make dev NODE=edge
kubectl -n memql logs deploy/edge | grep -i "seed\|site"
```

Expected: the seed registers, the row exists, and the edge resolves `portal.memql.localhost`.

- [ ] **Step 5: Verify deletion re-seeds**

Delete the row through the API, restart the edge, confirm it returns. This is the property `systemOwned` plus re-seeding is for, and it is worth proving once rather than assuming.

- [ ] **Step 6: Retire `component/portal`**

```bash
grep -rn "component/portal" --include="*.go" . | grep -v "^./component/portal"
```

If the only remaining references are in `app/transport_portal.go` and the bff wiring, remove them and delete the package — pre-release, no shims. Move the `doc.go` rationale (why not `go:embed`) into `component/edge/doc.go`, since it now explains the general case rather than one bundle.

- [ ] **Step 7: Commit**

```bash
git add dsl/platform/seeds.memql component/edge/ && \
git rm -r component/portal app/transport_portal.go 2>/dev/null; \
git commit -m "feat: the portal is site #1

Same concept, same resolution, same opener, same headers as any customer
site. The only difference is where the hostname comes from -- the cluster
install rather than the portal UI -- and that is data.

Seeded rather than special-cased: the portal is how sites are managed, so
an ordinary row would ship an empty table and no way to reach the screen
that fills it. Re-seeded at boot so deleting the row cannot brick cluster
management.

TestPortalHasNoSpecialCaseInTheServingPath is a source scan, not a
behavioural test, because the failure it prevents is a branch added later
that every behavioural test would still pass."
```

---

### Task 7: The same-origin `/_memql/*` proxy

**Files:**
- Create: `component/edge/proxy.go`, `component/edge/proxy_test.go`
- Modify: `component/edge/handler.go` (dispatch before site resolution)

**Interfaces:**
- Consumes: `Site.APIProxy` (Task 1), the handler (Task 5).
- Produces: `func NewAPIProxy(target string) http.Handler` mounted at `/_memql/`.

- [ ] **Step 1: Write the failing test**

```go
// component/edge/proxy_test.go
package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIProxyForwardsWhenEnabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: true,
		}},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream-Path"); got != "/memql/ws" {
		t.Errorf("upstream saw %q, want /memql/ws -- the /_memql prefix must be stripped", got)
	}
}

// A site that did not opt in must not get an API path. Otherwise every hosted
// site is an open relay to the cluster's API surface.
func TestAPIProxyIsRefusedWhenNotEnabled(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: false,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: "http://127.0.0.1:1",
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("a site with apiProxy=false reached the API")
	}
}

// The WebSocket upgrade must survive the hop. This is the failure mode a unit
// test on a plain GET would miss entirely.
func TestAPIProxyPreservesTheUpgradeHeaders(t *testing.T) {
	var sawUpgrade, sawConnection string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUpgrade = r.Header.Get("Upgrade")
		sawConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: true,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.EqualFold(sawUpgrade, "websocket") {
		t.Errorf("upstream saw Upgrade=%q; the proxy dropped it", sawUpgrade)
	}
	if !strings.Contains(strings.ToLower(sawConnection), "upgrade") {
		t.Errorf("upstream saw Connection=%q; the proxy dropped it", sawConnection)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/edge/ -run TestAPIProxy -v
```

Expected: FAIL to build — `h.serveAPI` undefined (the handler in Task 5 calls it).

- [ ] **Step 3: Write the proxy**

```go
// component/edge/proxy.go
package edge

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// serveAPI forwards /_memql/* to the bff so a hosted site is same-origin with
// its own API.
//
// WHY THIS EXISTS. component/server/token_cookie.go sets the auth cookie
// SameSite=Lax, which is not sent on a cross-site request in any browser
// today -- before third-party cookie deprecation is even considered. A site
// on its own origin calling api.<domain> would therefore have no cookie, and
// would also need CORS and the cluster domain compiled into its bundle. Going
// through the site's own origin removes all three problems at once.
//
// OPT-IN PER SITE. A site that did not ask for an API path must not get one:
// otherwise every hosted site is an open relay to the cluster's API surface,
// including sites belonging to whoever else this cluster serves.
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, site *Site) {
	if !site.APIProxy || strings.TrimSpace(h.apiTarget) == "" {
		http.NotFound(w, r)
		return
	}

	target, err := url.Parse(h.apiTarget)
	if err != nil {
		h.logger.Error("edge: MEMQL_EDGE_API_TARGET is not a URL",
			"component", "edge", "target", h.apiTarget, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// /_memql/ws -> /memql/ws. The prefix is the edge's marker, not
			// part of the API's path space.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/_memql")
			pr.Out.URL.RawPath = ""
			// SetXForwarded, plus the site's own Host, so the bff sees which
			// origin the request actually arrived on rather than the edge's
			// service name.
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			h.logger.Error("edge: proxying to the API failed",
				"component", "edge", "site", site.ID, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}

	// ReverseProxy handles the WebSocket upgrade itself when the request
	// carries Upgrade/Connection -- it detects a 101 and switches to a raw
	// byte pipe. TestAPIProxyPreservesTheUpgradeHeaders proves it rather than
	// trusting it, because a plain GET test would pass while every WebSocket
	// silently died.
	proxy.ServeHTTP(w, r)
}
```

- [ ] **Step 4: Run the tests**

```bash
go test ./component/edge/ -v
```

Expected: PASS. `TestAPIProxyPreservesTheUpgradeHeaders` is the one that matters — if it fails, the `Rewrite` function is stripping hop-by-hop headers and every browser connection from a hosted site will fail at upgrade rather than at request time.

- [ ] **Step 5: Commit**

```bash
git add component/edge/proxy.go component/edge/proxy_test.go component/edge/handler.go
git commit -m "feat(edge): same-origin /_memql/* proxy, opt-in per site

A hosted site reaches the API through its own origin: no CORS, no cluster
domain compiled into the customer's bundle, and no dependence on a
SameSite=Lax cookie that is not sent cross-site in any browser today.

Opt-in per site, because a site that did not ask for it must not become an
open relay to the cluster's API surface. The upgrade-header test is the
point of this file -- a plain GET would pass while every WebSocket died."
```

---

### Task 8: The publish path

**Files:**
- Create: `component/edge/publish.go`, `component/edge/publish_test.go`
- Modify: `component/server/unauthenticated_surface.go` (declare the route)
- Modify: `CLAUDE.md` (the endpoint-protocol exception table)
- Modify: `docs/public/operate/front-door.md` (from Plan 1, Task 5)

**Interfaces:**
- Consumes: `updateSiteBundle` (Task 1); the blob client (Task 4); the `class="service_account"` verifier (memql#691).
- Produces: `POST /sites/{id}/bundles` returning `{version, bundleRef}`.

> **This adds an HTTP endpoint and therefore needs explicit approval under the endpoint-protocol policy.** The reasoning is the one already recorded for `/spaces/{id}/attachments`: multipart bundles map poorly to gRPC. Do not start this task until that approval is recorded on the issue.

- [ ] **Step 1: Write the failing atomicity test**

```go
// component/edge/publish_test.go
package edge

import (
	"testing"
)

// ATOMICITY IS THE POINT. A partially-uploaded bundle must never be reachable:
// the whole thing lands under a NEW version prefix and only then does the row
// flip. A failure mid-upload leaves the previous version live and untouched.
func TestFailedUploadLeavesThePreviousVersionLive(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	store.failAfter = 2 // die partway through
	if _, err := p.Publish(t.Context(), "s1", bundleWith(4)); err == nil {
		t.Fatal("Publish succeeded despite an upload failure")
	}

	if got := sites.bundleRef("s1"); got != "blob://sites/s1/v1/" {
		t.Errorf("bundleRef moved to %q after a failed upload; it must still be v1", got)
	}
}

func TestSuccessfulPublishFlipsTheRowOnce(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	res, err := p.Publish(t.Context(), "s1", bundleWith(3))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if sites.writes != 1 {
		t.Errorf("the row was written %d times, want exactly 1", sites.writes)
	}
	if sites.bundleRef("s1") != res.BundleRef {
		t.Errorf("row says %q, publish returned %q", sites.bundleRef("s1"), res.BundleRef)
	}
	if res.BundleRef == "blob://sites/s1/v1/" {
		t.Error("publish reused the previous version prefix; versions must not be overwritten")
	}
}

// Rollback is the same write in the other direction, and the bytes must still
// be there. This is what versioned prefixes buy.
func TestRollbackPointsAtBytesThatStillExist(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	first, _ := p.Publish(t.Context(), "s1", bundleWith(2))
	if _, err := p.Publish(t.Context(), "s1", bundleWith(2)); err != nil {
		t.Fatal(err)
	}

	if !store.hasPrefix(first.BundleRef) {
		t.Error("the previous version's bytes were removed; rollback would 404")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/edge/ -run 'TestFailedUpload|TestSuccessfulPublish|TestRollback' -v
```

Expected: FAIL to build — `NewPublisher`, `newFakeBlobStore`, `newFakeSiteStore`, `bundleWith` undefined. Write the three fakes in the test file: `fakeBlobStore` with a `failAfter int` counter and a `hasPrefix(ref string) bool`; `fakeSiteStore` with a `writes int` counter and `bundleRef(id string) string`; and `bundleWith(n int) Bundle` returning `n` files including `index.html`.

- [ ] **Step 3: Write the publisher**

```go
// component/edge/publish.go
package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Bundle is the set of files a build produced, keyed by their path within the
// site (e.g. "index.html", "assets/app.js").
type Bundle map[string][]byte

// BlobWriter is the write half of object storage. Separate from BlobClient so
// the serving path cannot write, which is a real boundary and not just tidiness.
type BlobWriter interface {
	Put(ctx context.Context, key string, data []byte) error
}

// SiteStore is the one mutation the publisher performs.
type SiteStore interface {
	UpdateBundleRef(ctx context.Context, siteID, bundleRef string) error
}

// Result is what a successful publish produced.
type Result struct {
	Version   string
	BundleRef string
}

type Publisher struct {
	blobs BlobWriter
	sites SiteStore
}

func NewPublisher(blobs BlobWriter, sites SiteStore) *Publisher {
	return &Publisher{blobs: blobs, sites: sites}
}

// version derives the version id from the bundle's CONTENT, not from a clock.
//
// Two reasons, and the second is the load-bearing one. A content hash makes a
// republish of identical bytes a no-op rather than a new version accumulating
// storage forever. And it makes this function deterministic, so the tests
// above are repeatable and a version id can be verified against the bytes it
// names -- a timestamp can be neither.
func version(b Bundle) string {
	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(b[name]))
		h.Write(b[name])
	}
	return "v" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Publish uploads the whole bundle under a NEW version prefix and only then
// flips the row.
//
// THE ORDER IS THE FEATURE. A failure at any point during the upload leaves
// the row pointing at the previous version, whose bytes are untouched -- so a
// half-uploaded bundle is never reachable, and there is no cleanup path to get
// wrong. Overwriting a prefix in place would make a deploy non-atomic AND
// destroy the bytes rollback needs.
func (p *Publisher) Publish(ctx context.Context, siteID string, b Bundle) (Result, error) {
	if len(b) == 0 {
		return Result{}, fmt.Errorf("edge: refusing to publish an empty bundle to %s", siteID)
	}
	if _, ok := b["index.html"]; !ok {
		// A bundle with no index.html serves nothing at "/" and nothing at
		// any spa-fallback path. Refusing here turns a broken build into a
		// failed publish rather than a live site that 404s its own homepage.
		return Result{}, fmt.Errorf("edge: bundle for %s has no index.html", siteID)
	}

	v := version(b)
	prefix := fmt.Sprintf("sites/%s/%s/", siteID, v)

	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic upload order, so a failure is reproducible

	for _, name := range names {
		if err := p.blobs.Put(ctx, prefix+name, b[name]); err != nil {
			return Result{}, fmt.Errorf("edge: uploading %s for %s: %w", name, siteID, err)
		}
	}

	ref := "blob://" + prefix
	if err := p.sites.UpdateBundleRef(ctx, siteID, ref); err != nil {
		// The bytes are uploaded and orphaned. That is the RIGHT failure:
		// storage is cheap, and the alternative -- flipping the row first --
		// serves a bundle that may not be fully there.
		return Result{}, fmt.Errorf("edge: pointing %s at %s: %w", siteID, ref, err)
	}

	return Result{Version: v, BundleRef: ref}, nil
}
```

- [ ] **Step 4: Run the tests**

```bash
go test ./component/edge/ -run 'TestFailedUpload|TestSuccessfulPublish|TestRollback' -v
```

Expected: PASS, all three.

- [ ] **Step 5: Declare and gate the route**

Add `/sites/` to `server.HandlerAuthorizedPaths()` — **not** `PublicPaths()`. The handler enforces the `class="service_account"` credential itself, which is what `HandlerAuthorizedPaths` means. Add the row to the endpoint-protocol exception table in `CLAUDE.md` with the multipart reasoning.

- [ ] **Step 6: Verify the surface tests still hold**

```bash
go test ./component/server/ -run 'Unauthenticated|PublicPaths|ContractRoutes' -v
make frontdoor-paths && make frontdoor-paths-check
```

Expected: PASS, and the generated Ingress block now carries `/sites/` — which is the mechanism from Plan 1 Task 3 doing its job on the first new route after it landed.

- [ ] **Step 7: Commit**

```bash
git add component/edge/publish.go component/edge/publish_test.go \
        component/server/unauthenticated_surface.go CLAUDE.md \
        deploy/k8s/overlays/local/api-front-door.yaml docs/public/operate/front-door.md
git commit -m "feat(edge): publish a bundle atomically

The whole bundle lands under a NEW version prefix and only then does the
row flip, so a failure mid-upload leaves the previous version live and
untouched, and rollback is one write to bytes that are still there.

HTTP multipart as a documented exception, on the reasoning already recorded
for /spaces/{id}/attachments. Declared in HandlerAuthorizedPaths (the
handler enforces a class=service_account credential), not PublicPaths."
```

---

### Task 9: Deploy the edge and complete the five-host gate

**Files:**
- Create: `deploy/k8s/base/edge.yaml`
- Create: `deploy/k8s/overlays/local/edge-front-door.yaml`
- Modify: `deploy/k8s/base/kustomization.yaml`, `deploy/k8s/overlays/local/kustomization.yaml`
- Modify: `deploy/k8s/overlays/local/render_domain_test.go` (add `edge` to `nodes`)
- Modify: `scripts/install/hosts-entries.sh` (add `portal.`)
- Modify: `component/edge/resolve.go` (change-feed invalidation)

**Interfaces:**
- Consumes: everything above; `frontDoorHosts` from Plan 1 Task 1.
- Produces: `svc/edge:8085`; the `*.<domain>` and apex Ingress rules that make Plan 1's gate go green.

- [ ] **Step 1: Confirm the gate still fails**

```bash
go test ./deploy/k8s/overlays/local/ -run TestFrontDoorServesExactlyTheFiveHosts -v
```

Expected: FAIL naming `*.memql.localhost` and `memql.localhost`.

- [ ] **Step 2: Write the Deployment and Service**

Model on `deploy/k8s/components/engine-bff/bff.yaml`: `200m/256Mi` requests, `/healthz` startup and readiness probes, `/livez` liveness, `MEMQL_NODE_TYPE=edge`, per-pod `MEMQL_NODE_ID` via `fieldRef: metadata.name`, `MEMQL_PARENT_ADDRESS=bff-active:50058`, the `memql-secrets` and `memql-db-pool` envFrom, and the `memql-ca` volume. Add `MEMQL_EDGE_API_TARGET=http://bff-http:8085` for the Task 7 proxy.

- [ ] **Step 3: Write the two Ingress rules**

One file, two rules — the wildcard and the apex, both to `svc/edge:8085`. The apex matters: for a customer cluster the bare domain **is** their main website, so it is a site row like any other. Note in the file header that these two rules are the last front-door rules that will ever be added, because everything after this is a graph row.

- [ ] **Step 4: Run the gate**

```bash
go test ./deploy/k8s/overlays/local/ -v
```

Expected: `TestFrontDoorServesExactlyTheFiveHosts` PASS at last, plus `TestEveryNodeReadsTheDomainConfigMap` passing with `edge` added to `nodes` — that list is deliberately hand-maintained precisely so a new node type has to be added to it consciously.

- [ ] **Step 5: Wire cross-node cache invalidation**

The site row is written wherever the portal is served from and read on every edge replica, so the TTL from Task 3 is a backstop, not the mechanism. Subscribe to the concept's change feed the way `CodeProfileSubscriber` does for `v1:observability:codeProfile` and call `Resolver.Invalidate` on each change. **Add coverage that exercises the hop** — a write on one node, a read on another — in `test/clustere2e/`; a green single-node test is a false signal for this class of bug and the repo has been bitten by it repeatedly.

- [ ] **Step 6: Verify on a two-replica cluster**

```bash
make up SERVERS=2 && make scale N=2 && make status
curl -sS https://portal.memql.localhost/ | head -5
curl -sS -o /dev/null -w '%{http_code}\n' https://nothing-here.memql.localhost/   # want 404
```

Then create a site through the API, confirm both edge replicas serve it, change its status, and confirm both replicas reflect the change within the TTL.

- [ ] **Step 7: Regenerate the architecture model**

```bash
make arch-model && make arch-model-check
```

- [ ] **Step 8: Commit**

```bash
git add deploy/k8s/base/edge.yaml deploy/k8s/overlays/local/edge-front-door.yaml \
        deploy/k8s/base/kustomization.yaml deploy/k8s/overlays/local/kustomization.yaml \
        deploy/k8s/overlays/local/render_domain_test.go \
        scripts/install/hosts-entries.sh component/edge/resolve.go \
        test/clustere2e/ component/architecture/embedded/topology.model.json
git commit -m "feat: deploy the edge and complete the five-host front door

The wildcard and the apex are the LAST front-door rules that will ever be
added: everything after this is a graph row.

Cache invalidation rides the concept change feed, not the TTL -- the site
row is written wherever the portal is served from and read on every edge
replica, so this is cross-node by construction and gets cluster-e2e
coverage rather than a single-node test that would pass either way."
```

---

### Task 10: The site-hosting runbook

**Files:**
- Create: `docs/public/operate/site-hosting.md`
- Modify: `GLOSSARY.md`, `clients/README.md`, `docs/public/operate/portal.md`
- Modify: `docs/internal/design/account-isolation-model.md` (the D12 status note)

- [ ] **Step 1: Write the runbook**

Front-matter per `docs/DOCS_STANDARD.md`. Cover: the bundle contract (what a build must emit, and that prerendered HTML is for crawlers and first paint while live values arrive from the graph on hydrate); publishing from CI with a `class="service_account"` credential; rollback as a single row write; `spa` versus `static` and the resolution order; `draft`/`live`/`disabled`; `apiProxy` and when to enable it; adding the hostname (DNS in the cloud, the hosts block locally); and the limits — one domain per cluster, and custom domains being an install-time change with a pointer to §11 of the spec.

- [ ] **Step 2: Update the neighbouring docs**

`clients/README.md`'s "served by a Go package at a sub-path" description becomes "a site row served by the edge"; `docs/public/operate/portal.md` gets the portal's own origin; `account-isolation-model.md` gets the one-line status note that per-cluster-per-customer is the chosen isolation model and §6(a)/(b)/(c) are parked.

- [ ] **Step 3: Verify no stale references**

```bash
grep -rn "MEMQL_PORTAL_DIST\|/portal/" docs/ clients/ CLAUDE.md | grep -v site-hosting
```

Expected: only references that are still true after Task 6.

- [ ] **Step 4: Commit**

```bash
git add docs/public/operate/site-hosting.md GLOSSARY.md clients/README.md \
        docs/public/operate/portal.md docs/internal/design/account-isolation-model.md
git commit -m "docs: add the site-hosting runbook

The bundle contract, publishing from CI, rollback, the resolution order,
and the limits -- one domain per cluster, custom domains an install-time
change.

Records that prerendered HTML is a snapshot for crawlers and first paint
while live values come from the graph on hydrate; without that a price
change makes every prerendered page wrong until the next build."
```

---

## Merge order

Tasks 1-2 may merge independently. Tasks 3-5 should merge together (the handler is not useful in pieces). Task 6 depends on 5, Task 7 on 5, Task 8 on 4 **and** on the endpoint-protocol approval, Task 9 on everything, Task 10 last.

Task 9 is what turns Plan 1's `TestFrontDoorServesExactlyTheFiveHosts` green. Until it lands, that test failing on the two edge hosts is the expected state and must not be "fixed" by weakening the assertion.
