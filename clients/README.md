# `clients/` — surfaces built on the platform

A **client** is an application that a person or another system points at a
memQL cluster: a landing page, a single-page app, a mobile app, a game, a
kiosk. `clients/<name>/` is where one lives.

This is a plural, first-class category, alongside `integrations/`. The two are
the platform's outward faces and they point in opposite directions:

| Directory | Direction | What lives there |
|-----------|-----------|------------------|
| `integrations/` | memQL → the world | Go code the engine calls out through (AI providers, email, storage, knowledge, voice) |
| `clients/` | the world → memQL | Applications that connect *in*, over gRPC or the `/memql/ws` bridge |
| `sdk/` | the wire itself | Libraries clients are built *with* (`sdk/go`, `sdk/ts`, `sdk/ts-viewkit`) |

## Why the engine repo has a `clients/` directory at all

memQL is a platform other people self-host and build surfaces on. The question
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
- **Served by a Go package** (`component/portal`) from a directory named by an
  env var, mounted on a node's HTTP server at a sub-path. Not `go:embed` — see
  `component/portal/doc.go` for why that choice is structural rather than
  stylistic.
- **Its own CI lane** (`portal-checks`) and its own path-filter bucket, and the
  bucket lists its `file:` dependencies. A consumer's bucket that omits the
  packages it compiles against is a lane that silently stops running; the repo
  has been bitten by exactly that (memql#2792) and
  `scripts/dev/portal_lane_scope_test.go` is what keeps it from recurring.
- **Its own Dockerfile stage**, selected per node type, so only the node that
  serves the client pays to build it.
- **Deployed through the same GitOps path as everything else** — a component
  under `deploy/k8s/`, the same manifests locally and in the cloud.

## Rules for anything added here

1. **Never name a downstream product.** The engine is product-neutral and
   `TestEngineIsProductNeutral` enforces it over every tracked file. A client
   in *this* repo is a platform surface (the portal is the ops console); a
   product's own client belongs in the product's own repo, which is what the
   `memql-project` template is for. The rule bans product *names*, not user
   interfaces.
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

To serve a locally built bundle from a binary you are running yourself:

```bash
make portal-build
MEMQL_PORTAL_DIST=clients/portal/dist go run .   # then open /portal/ on the HTTP port
```

In the local cluster the portal is already there, at the same front door
everything else uses: **<https://cockpit.local.znas.io/portal/>** after
`make up`.
