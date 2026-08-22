# `clients/` — surfaces built on the platform

A **client** is an application that a person or another system points at a
MemQL cluster: a landing page, a single-page app, a mobile app, a game, a
kiosk. `clients/<name>/` is where one lives.

This is a plural, first-class category, alongside `integrations/`. The two are
the platform's outward faces and they point in opposite directions:

| Directory | Direction | What lives there |
|-----------|-----------|------------------|
| `integrations/` | MemQL → the world | Go code the engine calls out through (AI providers, email, storage, knowledge, voice) |
| `clients/` | the world → MemQL | Applications that connect *in*, over gRPC or the `/memql/ws` bridge |
| `sdk/` | the wire itself | Libraries clients are built *with* (`sdk/go`, `sdk/ts`, `sdk/ts-viewkit`) |

## Why the engine repo has a `clients/` directory at all

MemQL is a platform other people self-host and build surfaces on. The question
"where does my SPA go, and how does it get served, built, tested and deployed
alongside the engine?" has to have an answer, and the answer is better as a
**worked example** than as prose: one real inhabitant, wired end to end, that
the `memql-project` template copies.

The engine repo carries exactly one — [`portal/`](portal), the platform's own
graphical operations console. Everything a downstream client needs is visible
in what the portal does:

- **Its own npm package**, not a workspace member. `clients/portal/package.json`
  stands alone and consumes `sdk/ts` + `sdk/ts-viewkit` as `file:`
  dependencies.
- **Served as an ordinary hosted site** (`component/edge`, memql#3711): the
  portal is site #1, resolved by hostname and served from a directory a
  `v1:platform:site` row names (`bundleRef`), the same mechanism any other
  hosted site uses. Not `go:embed` — see `component/edge/doc.go` for why that
  choice is structural rather than stylistic.
- **Its own CI lane** (`portal-checks`) and its own path-filter bucket, and the
  bucket lists its `file:` dependencies. A consumer's bucket that omits the
  packages it compiles against is a lane that silently stops running; the repo
  has been bitten by exactly that (memql#2792) and
  `scripts/dev/portal_lane_scope_test.go` is what keeps it from recurring.
- **Its own Dockerfile stage**, selected per node type, so only the node that
  serves the client pays to build it.
- **Deployed through the same GitOps path as everything else** — a component
  under `deploy/k8s/`, the same manifests locally and in the cloud.

**And it hands off to the editor rather than growing one.** A concept page in
the portal carries *Open definition in VS Code*, which is a link and nothing
more:

    vscode://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>

composed from the construct's registry key (for a concept, its id) and the
`domain` the node publishes in `GET /runtime-config.json`. The portal renders
no `.memql` source, holds no catalog of constructs, and knows nothing about
`clusters.yaml`, the editor's credentials, or whether the extension is
installed at all — which is why the install pointer sits permanently beside the
link rather than appearing when it is needed. Everything after the click is the
extension's: matching that domain against a registered cluster, connecting it
through the ordinary sign-in, and landing on the construct. That is the
boundary the two surfaces keep — the extension owns what is on your machine and
what it can reach, the portal owns what is inside a cluster — and the link shape
is open to any client that knows its cluster's domain, which is the point of
serving the domain at all.

## Rules for anything added here

1. **Never name a downstream product.** The engine is product-neutral and
   `TestEngineIsProductNeutral` enforces it over every tracked file. A client
   in *this* repo is a platform surface (the portal is the ops console); a
   product's own client belongs in the product's own repo, which is what the
   `memql-project` template is for. The rule bans product *names*, not user
   interfaces. That repo consumes the engine as **pinned images** and never
   forks it — it holds the cluster's definition, not the cluster's code; see the
   per-customer-cluster amendment in
   [platform-consolidation.md](../docs/internal/design/platform-consolidation.md).
2. **Applications are private, libraries are published.** A client is an
   application: `"private": true`, no `publishConfig`, and its
   `package-lock.json` **is** committed — it pins the tree the released bundle
   is built from. `sdk/ts` and `sdk/ts-viewkit` are libraries, are published,
   and (for `sdk/ts`) deliberately carry no lockfile.
3. **Reuse `sdk/ts-viewkit` for concept rendering.** view-kit turns rows plus
   their `@displayCard` hints into a framework-agnostic `VNode` tree, so a
   concept renders the day it is declared with no renderer change. Adapt the
   tree to your framework (`clients/portal/src/viewkit/react.ts` is ~40 lines);
   do not fork the package and do not re-implement per-concept rendering.
4. **Dial the origin you were served from.** A client served by a node should
   connect back to that node's `/memql/ws`, relative. It removes an entire
   class of configuration, CORS and mis-pointing bugs.
5. **One bucket, one lane, and the bucket lists the `file:` deps.** See rule 3
   of the portal's wiring above.

## Building

```bash
make portal-install     # deps, including building the sdk/ts + view-kit file: deps first
make portal-typecheck
make portal-test
make portal-build       # -> clients/portal/dist
make portal-clean
```

The single script behind all five is `scripts/portal/build.sh`, and the
Dockerfile's portal stage runs the same script — so an image bundle and a
locally built one cannot diverge in how they were produced.

The portal is site #1 (memql#3711): its row's `bundleRef` names the directory
the edge serves it from, the same mechanism any hosted site uses. There is no
longer an env var that repoints it -- `MEMQL_PORTAL_DIST` retired along with
`component/portal`. To serve a locally built bundle instead of the seeded
`file:///app/portal`, call the `updateSiteBundle` mutation with
`siteId: "portal"` and a `bundleRef` naming a `clients/portal/dist` the edge
pod can read.

In the local cluster the portal is already there, at its own front-door host:
**<https://portal.memql.localhost/>** after `make up`.
