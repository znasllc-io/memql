# `clients/` — surfaces built on the platform

A **client** is an application that a person or another system points at a
MemQL cluster: a landing page, a single-page app, a mobile app, a game, a
kiosk. `clients/<name>/` is where one lives.

This is a plural, first-class category, alongside `integrations/`. The two are
the platform's outward faces and they point in opposite directions:

| Directory | Direction | What lives there |
|-----------|-----------|------------------|
| `integrations/` | MemQL → the world | Go code the engine calls out through (AI providers, email, storage, knowledge, commerce) |
| `clients/` | the world → MemQL | Applications that connect *in*, over gRPC or the `/memql/ws` bridge |
| `sdk/` | the wire itself | Libraries clients are built *with* (`sdk/go`, `sdk/ts`, `sdk/ts-viewkit`) |

## Why the engine repo has a `clients/` directory at all

MemQL is a platform other people self-host and build surfaces on. The question
"where does my SPA go, and how does it get served, built, tested and deployed
alongside the engine?" has to have an answer, and the answer is better as a
**worked example** than as prose: one real inhabitant, wired end to end, that
the `memql-project` template copies.

The engine repo carries two platform surfaces — [`portal/`](portal), the operations console, and [`os/`](os), the named OS shell. Everything a downstream client needs is visible
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
6. **Every list a person watches rides a collection; every one-shot read
   renders its staleness honestly.** See Freshness below.

## Freshness: what is stale, and who is responsible for it

Three layers sit between a row in the store and a row on somebody's screen, and
a client that does not know all three ships a surface that looks current and is
not.

**The engine caches your reads by default.** A hint-free pure read gets a 60s
in-process backstop per node, keyed on the plan signature plus the resolved
actor whenever the plan depends on the caller. Invalidation is event-driven, so
a read whose answer can only change through a write to a concept it depends on
is fresh the moment that write lands, whatever the TTL says. The reads that
need thinking about are the ones whose answer can change with NO write --
anything filtering on `now`, and anything over rows that reach the store by raw
SQL and therefore publish no invalidation. Those are only as fresh as their
TTL. See [the language doc's caching
attributes](../docs/public/language/memql.md) and the design record for the
reviewed adoption table.

**The socket keeps a list current, and says when it could not.** Pull once,
subscribe, fold by id; a drop, an overflow or a reconnect is a GAP the client
can see, and the answer to a gap is to re-run the read. `sdk/ts`'s
`LiveCollection` is that machine -- subscribe-then-seed, the authorized re-read
for id-only events, the scope re-filter, reference counting so navigating away
and back issues no new read, and a `seeding | live | degraded | disconnected`
state. It is in-memory only: a full page reload starts clean, by construction.
The rules a hand-rolled fold owes itself are in
[events.md](../docs/public/concepts/events.md#how-to-consume-events-without-diverging-from-the-store-memql4536-4540).

**Everything else is a one-shot read, and must SAY so.** A surface that reads
once and renders forever is fine -- an admin console pane, a provider list --
as long as it offers an explicit Refresh and does not imply it is live. What is
not fine, and is the specific defect this contract exists to end, is a table
that keeps rendering after its stream has died: rows an operator has no reason
to distrust, under a heading that says live. Keep the last known answer, label
it stale, and let them refresh.

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
