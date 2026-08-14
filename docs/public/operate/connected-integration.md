---
title: Connected — a site that stays where it is
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Connected — a site that stays where it is

There are three ways a customer's web application can relate to a memQL
cluster, and the cheapest one needs nothing built. **Their site stays exactly
where it already runs — Vercel, Netlify, a VPS, whatever they have — adds the
memQL SDK, and points at `api.<domain>`.** memQL is the backend; the hosting
stays theirs.

This page is that option, called **Connected**, and the ladder it sits at the
bottom of.

Related: [front-door.md](front-door.md) ·
[auth/identity-service.md](auth/identity-service.md) ·
[portal.md](portal.md)

---

## The ladder

| Rung | What it is | Needs | Commitment |
|---|---|---|---|
| **1. Connected** | Their site stays put; add the SDK | [memql#3716](https://github.com/znasllc-io/memql/issues/3716) | You own the API, not their uptime |
| **2. Hosted (static)** | Their site is a site row | [memql#3700](https://github.com/znasllc-io/memql/issues/3700) | You own their hosting |
| **3. Hosted (container)** | Anything that runs | [memql#3718](https://github.com/znasllc-io/memql/issues/3718) | You own their runtime |

**Moving up the ladder is nearly free, and that is the property worth
understanding before choosing a rung.** The SDK integration is *identical* at
every rung: the same generated client, the same queries, the same
subscriptions, the same auth flow. What changes is where the bytes are served
from, not how the application talks to memQL.

Moving up will in fact *remove* code. A hosted site is designed to be served by
the edge on its own origin with `/_memql/*` proxied to the bff, so it is
same-origin with its own API — which deletes the CORS configuration and the
cross-origin auth handling this page spends most of its length on. Nobody has to
unpick a Connected integration to host it later.

> **INFO: rungs 2 and 3 are design, not deployment.** The `edge` node type
> exists, but the site-serving path and the same-origin proxy ship with epic
> memql#3700, and the container kind with memql#3718. Rung 1 is the one that
> works against a cluster today, which is most of the reason this page exists.

So Connected is a legitimate destination, not a staging area. Pick it when the
customer already has a deployment pipeline they like, when their site is built
by someone else, or when you want a working integration this week.

## What Connected actually is

```
   their origin                              your cluster
   https://shop.example.com
        |
        |  static bytes: their host, their CDN, their pipeline
        |
   [ browser ] ---- OAuth + PKCE ---------> https://identity.<domain>
        |
        '--------- wss://api.<domain>/memql/ws ----> svc/bff-http:8085
                   (bearer in the WebSocket subprotocol)      |
                                                             '-> MemqlService.Stream
```

Three moving parts, in the order you set them up:

1. A **typed client** generated from the cluster's DSL.
2. An **OAuth client** registered for their origin.
3. Their **origin allowed**, so the browser will let them read the responses.

Everything after that is application code.

## 1. A typed client from the cluster's DSL

`scripts/sdk-gen` walks one or more DSL trees and emits typed methods on
`QueryClient` for every query, mutation and logic declared in `**/*.memql`. The
generator itself is the importable `sdk/gen` package; the script is a thin CLI
over it.

```bash
go run ./scripts/sdk-gen \
  --dsl=dsl,/path/to/their/bundle \
  --out= \
  --ts-out=/path/to/their/app/src/memql \
  --ts-import-from=@znasllc-io/memql-sdk-core
```

| Flag | Meaning |
|---|---|
| `--dsl` | DSL tree root; repeatable or comma-separated, so roots compose |
| `--out` | Go output directory (`sdk/go/client` by default; empty skips Go) |
| `--ts-out` | TypeScript output directory (`sdk/ts/src/client` by default; empty skips TS) |
| `--ts-import-from` | Module the generated TS imports `QueryClient` from and augments via `declare module` |
| `--check` | Exit non-zero if regenerating would change anything — the CI gate shape |

> **INFO: `make sdk-gen` in this repo passes `--ts-out=` deliberately.** The
> engine emits no TypeScript for itself, because `@znasllc-io/memql-sdk-core` is
> client-agnostic on purpose. A customer's typed TS surface is generated from
> **core DSL ∪ their own bundle** — which is what the composed `--dsl` above
> does. The emitters are `sdk/gen/emit_ts.go` (methods, via TypeScript
> declaration merging) and `sdk/gen/emit_concepts.go` (concept ids and CDC
> topic/filter constants, so nothing hand-writes a
> `graph.node.<action>.<concept>` string).

Wire the generated methods onto a connection, exactly as any other client does:

```ts
import { Connection } from "@znasllc-io/memql-sdk-core";

const conn = await Connection.dial({
  endpoint: "wss://api.example.com/memql/ws",
  auth: { bearer: accessToken },
});
```

Run the generator again — and commit the result — whenever the cluster's DSL
changes. Pin it with `--check` in their CI if you want drift to be a build
failure rather than a runtime surprise.

## 2. An OAuth client for their origin

Clients live in the graph, as `v1:identity:oauthClient` rows. There are two
ways to get one:

- **Dynamic registration** — `POST /register` on the identity host, RFC 7591,
  enabled by default (`MEMQL_IDENTITY_OAUTH_DCR_ENABLED`). Returns a public
  `client_id` with no secret, which is why PKCE is mandatory.
- **Configured at boot** — `MEMQL_IDENTITY_REGISTERED_CLIENTS`, a JSON array of
  `{clientId, redirectURIs[]}`. This is how the platform's own clients (the
  portal, the Cockpit) are seeded.

Either way, `redirect_uri` is matched by **exact string**. A trailing slash, a
missing port or `http` where the registered value says `https` is a 400 at
`/authorize` reading "Invalid redirect URI", and nothing appears in the
application's own logs.

> **WARNING: registering a client does not allow its origin.** A
> dynamically-registered client is created with **no** CORS allowance and cannot
> grant itself one. This is deliberate: `POST /register` is unauthenticated by
> design — it exists so strangers can call it — and deriving a credentialed-CORS
> origin from a redirect URI anyone can self-register would be a session-theft
> hole rather than a convenience. See
> [memql#3716](https://github.com/znasllc-io/memql/issues/3716) for the full
> reasoning.
>
> The symptom, if you assume otherwise, is a `client_id` that works, an
> `/authorize` flow that completes, and a browser that refuses to read the
> `/oauth/token` response.

## 3. Getting their origin allowed

This is the step with no shortcut, and the one to do before writing any
application code.

**The mechanism is an owner-or-admin grant on the client's own row.** The
allowance is a field on `v1:identity:oauthClient`, writable **only** through
`IdentityAdminMsg` on `MemqlService.Stream`, onto
`component/identity/adminops` — the package that already carries the
owner-or-admin gate. A client cannot set its own. The effective allowlist is
the boot list (the platform's own origins, derived from the cluster's domain)
**union** the origins an admin has granted, cached and invalidated on the
concept's change feed, **so a grant takes effect with no restart.** See
[memql#3716](https://github.com/znasllc-io/memql/issues/3716).

Two things about it that will otherwise cost an afternoon:

> **WARNING: revoking a grant looks like it did not work, for up to ten
> minutes.** The revoke is effective immediately server-side — no restart — but
> identity's CORS middleware sets `Access-Control-Max-Age: 600`
> (`component/identity/http/cors.go`), so the browser keeps using its cached
> preflight. A hard reload does not clear it. Test a revoke in a fresh private
> window, or wait out the ten minutes, rather than concluding the grant is
> stuck.

**And an origin has to be named in more than one place.** The grant covers
identity's credentialed endpoints. It does not cover the two allowlists on the
bff, which a cross-origin client also crosses:

| Surface | Where the origin is named | Notes |
|---|---|---|
| `/oauth/token`, `/auth/refresh`, `/auth/logout` on identity | the admin grant above | Credentialed. **Never `*`**: identity echoes the request's own `Origin` and sets `Access-Control-Allow-Credentials: true`, so a wildcard entry makes every origin a credentialed one rather than a harmless one |
| `wss://api.<domain>/memql/ws` | `MEMQL_WS_ORIGIN_PATTERNS` on the bff (`component/server/memqlws`) | **Not CORS.** A WebSocket handshake is not subject to CORS, so the server checks `Origin` itself. Unset falls back to a wildcard and logs a WARN on every upgrade — populate it in any real deployment |
| HTTP exceptions on `api.<domain>` (for example a multipart attachment upload) | `SERVER_ALLOWED_ORIGINS` on the bff | Defaults to `*`, which here degrades to a credential-less posture rather than allowing credentialed reads. See [env-vars.md](env-vars.md) |

A hosted site (rung 2 or 3) is designed to need none of this, because it is
same-origin. That is the concrete content of "moving up the ladder removes
code".

## Auth from a cross-origin SPA

**OAuth 2.1 authorization code + PKCE, and the access token lives in memory.
Not in the cookie, and not in `localStorage`.**

This deserves its own section because the cookie is the intuitive answer and it
does not work.

`component/server/token_cookie.go:53` sets the `memql_auth` cookie
`SameSite=Lax`. `Lax` permits a cookie on a **top-level cross-site
navigation** and on nothing else — **not** on a cross-site `fetch`, `XHR` or
WebSocket handshake, in any current browser. Those are exactly the requests an
SPA makes. So an application on `shop.example.com` talking to
`api.example.com` will never see that cookie attached, and `Lax` is the right
setting for a cookie the bff issues to its own origin, so this is not a bug to
be fixed. It is true today, before third-party cookie deprecation is considered
at all; deprecation only removes the workarounds.

The flow that does work is the one the memQL Portal already uses, described
step by step in [portal.md](portal.md#how-the-portal-authenticates):

1. Top-level navigation to `https://identity.<domain>/authorize` with
   `response_type=code`, the client id, the exact registered `redirect_uri`,
   `state`, and `code_challenge` + `code_challenge_method=S256`.
2. The callback returns an authorization code on the URL. Scrub it from the
   address bar once consumed; it is single-use and PKCE-bound, but it does not
   belong in browser history.
3. `POST /oauth/token` with the code and the `code_verifier` → an access token
   in the JSON body.
4. Hold that token in a **closure variable**. Dial `/memql/ws` with it in the
   WebSocket subprotocol (`Sec-WebSocket-Protocol: bearer, <jwt>`), never in a
   query parameter — a query parameter lands in every ingress access log.
5. Refresh before expiry and install the new token on the **live stream** via
   `rotateAuth`, so subscriptions survive a token rollover.

Two consequences specific to being cross-origin:

- **Refresh is a credentialed cross-origin request**, which is exactly why the
  origin grant above is required rather than optional. Without it the
  application signs in, works, and signs itself out roughly one access-token
  lifetime later — the single most confusing failure mode on this path.
- **The refresh cookie is set on the identity origin**, so if the customer's
  site and the cluster are on different registrable domains, set
  `MEMQL_IDENTITY_REFRESH_COOKIE_SAMESITE=none` on identity. `Lax` there has
  the same problem as the paragraph above, for the same reason.

`localStorage` is the wrong home for the access token for a reason that has
nothing to do with being cross-origin: it outlives the page, is readable by any
script that ever runs on the origin at any later time, and syncs with the
browser profile. In-memory is strictly less exposure at no functional cost,
since a cold load rebuilds the session in one request.

## Live subscriptions work here too

Nothing about the reactive path is hosted-only. The same multiplexed
WebSocket carries request/reply and the event fanout, so a cross-origin SPA
subscribes exactly as a hosted one does:

```ts
const unsubscribe = conn.subscriptions.subscribeGraph(
  (event) => applyToUI(event),
  { concept: "v1:cognition:utterance", actions: ["created", "updated"] },
);
```

The server composes the bus topic from the concept and the actions, so the
client never writes a topic string — and `sdk/gen/emit_concepts.go` emits the
concept ids so nothing hand-types one either. See
[events.md](../concepts/events.md).

## What Connected gives up

Stated plainly, because the trade is real and the answer is not always "host
it":

| | Connected | Hosted |
|---|---|---|
| Origin | Cross-origin: a CORS grant, WS origin patterns, a token in memory | Same-origin via `/_memql/*`; none of that is needed |
| Systems to operate | Two — your cluster and their hosting | One |
| Handover | Two conversations; their pipeline is not yours to hand over | One repo, one cluster, one handover |
| Deploy of their site | Theirs, on their clock | An upload plus a row write |
| Their existing pipeline | Kept | Replaced |
| Needs from you | Nothing built | The edge and the site machinery |

The one-system property is the substantive difference. Under one memQL cluster
per customer, "we build it, then hand it over if they want to run it
themselves" is a promise you can keep about the cluster — and Connected leaves
their hosting outside the thing you would be handing over. That is fine when
they want it that way and a limitation when they do not.

## Reference

- Design: `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
  (epic [memql#3700](https://github.com/znasllc-io/memql/issues/3700)) —
  decision D9 is the same-origin proxy that rungs 2 and 3 get instead of this
  page's CORS work.
- Generator: `scripts/sdk-gen/main.go`, `sdk/gen/emit_ts.go`,
  `sdk/gen/emit_concepts.go`.
- The runtime core's own surface: `sdk/ts/README.md`.
