---
title: Connected — a site that stays where it is
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Connected — a site that stays where it is

There are three ways a customer's web application can relate to a MemQL
cluster, and the cheapest one needs nothing built. **Their site stays exactly
where it already runs — Vercel, Netlify, a VPS, whatever they have — adds the
MemQL SDK, and points at `api.<domain>`.** MemQL is the backend; the hosting
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
| **3. Hosted (container)** | Anything that runs | deferred, see below | You own their runtime |

**Moving up the ladder is nearly free, and that is the property worth
understanding before choosing a rung.** The SDK integration is *identical* at
every rung: the same generated client, the same queries, the same
subscriptions, the same auth flow. What changes is where the bytes are served
from, not how the application talks to MemQL.

Moving up will in fact *remove* code. A hosted site is designed to be served by
the edge on its own origin with `/_memql/*` proxied to the bff, so it is
same-origin with its own API — which deletes the CORS configuration and the
cross-origin auth handling this page spends most of its length on. Nobody has to
unpick a Connected integration to host it later.

> **INFO: rung 3 is deferred; rungs 1 and 2 work today.** The `edge` node
> type serves site rows and mounts the same-origin proxy, both shipped with
> epic memql#3700. **Rung 3 is NOT on the roadmap** — the container kind was
> decided in memql#3718 and then closed unbuilt, because its motivation is
> migrating a customer's existing Rails/Laravel/Django/Express app and no such
> migration is in front of us. The reasoning is preserved on that closed issue;
> it is a decision to revisit when a real migration arrives, not a queued task.
> Until then a customer who "has a site that runs" takes rung 1, which owes them
> nothing about their runtime.

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
`QueryClient` for every query, mutation and logic declared in `**/*.memql`,
plus every builtin marked `@sdk` — most builtins are engine-internal and stay
off the client surface, so the marker is the opt-in (memql#4239). The
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

> **INFO: `make sdk-gen` in this repo emits both targets.** The engine's own
> typed surface — `dsl/`'s queries, mutations and logics plus its `@sdk`
> builtins — is generated into `sdk/go/client` and `sdk/ts/src/client` and
> ships inside `@znasllc-io/memql-sdk-core`; the portal consumes it that way
> (memql#4232), and `make sdk-gen-check` fails CI on drift. A customer's typed
> TS surface is generated from **core DSL ∪ their own bundle** — which is what
> the composed `--dsl` above does — with `--ts-import-from` aiming the emitted
> `declare module` augmentation at the published package. The emitters are
> `sdk/gen/emit_ts.go` (methods, via TypeScript declaration merging) and
> `sdk/gen/emit_concepts.go` (concept ids and CDC topic/filter constants, so
> nothing hand-writes a `graph.node.<action>.<concept>` string).

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
  **disabled by default** (`MEMQL_IDENTITY_OAUTH_DCR_ENABLED`, memql#3719); set
  it true on clusters that expose an MCP surface, and it answers
  `403 registration_disabled` until you do. Returns a public `client_id` with no
  secret, which is why PKCE is mandatory.
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
> hole rather than a convenience.
>
> The sharpest form of that argument is worth carrying: **RFC 8252 loopback
> redirects are normal on this concept**, so a derivation would silently admit
> `http://127.0.0.1` and hand every local process on somebody's machine
> cookie-bearing read access to identity. See
> [memql#3716](https://github.com/znasllc-io/memql/issues/3716).
>
> The symptom, if you assume otherwise, is a `client_id` that works, an
> `/authorize` flow that completes, and a browser that refuses to read the
> `/oauth/token` response.

## 3. Getting their origin allowed

This is the step with no shortcut, and the one to do before writing any
application code.

**The mechanism is an owner-or-admin grant on the client's own row.** The
allowance is `corsOriginsJSON` on `v1:identity:oauthClient` — a JSON array of
origins, each scheme plus host plus optional port and nothing else. It is
written **only** through `IdentityAdminMsg` on `MemqlService.Stream`, onto
`adminops.SetOAuthClientCORSOrigins` (`component/identity/adminops/cors.go`),
the package that already carries the owner-or-admin gate. The same call grants
and revokes. A client cannot set its own, and the field is **never** derived
from `redirectURIsJSON` — see the warning below. Design and rationale:
[memql#3716](https://github.com/znasllc-io/memql/issues/3716).

The effective allowlist is the boot list (`MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS`,
carrying the platform's own origins derived from the cluster's domain) **union**
the granted rows, which `component/identity/http/cors.go` reads live on a miss
against the boot list. **So a grant takes effect with no restart** — within a
10-second window rather than the same millisecond, because the middleware may
reuse the set it last read for that long. That window is not a latency
optimisation: `cors()` wraps around fifteen routes including every
unauthenticated preflight, `POST /register` and the WebAuthn login pair, so
without it an anonymous flood of unknown origins would be one database query per
request against the auth surface.

Two things about it that will otherwise cost an afternoon:

> **WARNING: revoking a grant looks like it did not work, for up to ten
> minutes.** Two separate windows stack here and it is worth not conflating them.
> Server-side the revoke lands within the 10 seconds above. The browser is the
> slow half: identity's CORS middleware sets `Access-Control-Max-Age: 600`
> (`component/identity/http/cors.go`), so a browser that has already been told
> "yes" keeps reusing that cached preflight for ten minutes. A hard reload does
> not clear it. Test a revoke in a fresh private window rather than concluding
> the revoke is stuck.

**And an origin has to be named in more than one place.** The grant covers
identity's credentialed endpoints. It does not cover the two allowlists on the
bff, which a cross-origin client also crosses:

| Surface | Where the origin is named | Notes |
|---|---|---|
| `/oauth/token`, `/auth/refresh`, `/auth/logout` on identity | the admin grant above | Credentialed, so `*` buys you nothing: identity **refuses** a wildcard from either source — the boot list or a granted row — and skips it rather than failing loudly, so explicit entries beside it keep working. There is no shortcut to skip the grant with |
| `wss://api.<domain>/memql/ws` | `MEMQL_WS_ORIGIN_PATTERNS` on the bff (`component/server/memqlws`) | **Not CORS.** A WebSocket handshake is not subject to CORS, so the server checks `Origin` itself. Unset falls back to a wildcard and logs a WARN on every upgrade — populate it in any real deployment |
| HTTP exceptions on `api.<domain>` (for example a multipart attachment upload) | `MEMQL_SERVER_ALLOWED_ORIGINS` on the bff | Defaults to `*`, which here degrades to a credential-less posture rather than allowing credentialed reads. See [env-vars.md](env-vars.md) |

All three of those are on our side of the fence. There is a **fourth** place the
same origins have to be named, on theirs — `connect-src` in the customer's own
CSP — and getting the cluster's three right while missing that one produces a
sign-in that fails at its last step with nothing in any server log. See
[the `connect-src` subsection below](#the-origin-grant-is-necessary-and-not-sufficient-connect-src).

A hosted site (rung 2) needs none of this, because it is same-origin. That is
the concrete content of "moving up the ladder removes code".

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

The flow that does work is the one the MemQL Portal already uses, described
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

### The origin grant is necessary and not sufficient: `connect-src`

**Two independent gates stand between a cross-origin SPA and a working sign-in,
and only one of them is visible from the cluster.** The admin CORS grant is the
first. The second is the customer's own Content-Security-Policy, enforced by the
browser — the same shape as the `SameSite=Lax` problem above: a browser-side
rule that makes a correct server-side configuration insufficient. A reader who
hits one of these will hit the other.

MemQL emits no CSP for a Connected site and cannot: the customer serves their
own site and their own headers, so there is no setting on our side to go looking
for. What we can tell them is **which origins to allow**. If they set a CSP at
all, `connect-src` has to name them:

```
Content-Security-Policy: connect-src 'self' https://api.<domain> wss://api.<domain> https://identity.<domain>;
```

**Two origins, not one, and this is the part people get wrong.** The API lives
at `api.<domain>`, but the OAuth token exchange in step 3 is a `fetch()` to
`identity.<domain>` — a different origin — and so is the refresh in step 5. A
`connect-src` listing only the API origin passes every test a developer thinks
to run, and then fails at the exchange.

**The failure mode is why this earns a paragraph rather than a footnote.** Step
1 is a *navigation*, and `connect-src` does not govern navigations — so sign-in
appears to work. The redirect happens, the person authenticates, the callback
lands. It dies at the token exchange, the very last step. And **identity's logs
are empty**, because a CSP refusal means the request never left the browser: the
one place an operator would look for the cause holds no evidence that anything
was even attempted. The only signal is a CSP violation in the customer's own
browser console, so that is where the diagnosis is.

None of this is theoretical, and MemQL has already been on the receiving end of
it. `component/edge/csp.go` names the cluster's identity origin in
`connect-src` for exactly this reason (memql#3711 fix round 2) — and, because
the edge serves every hosted site rather than one bundle on one origin, it
does this for every site alike, not as a portal-specific carve-out
(`TestPortalHasNoSpecialCaseInTheServingPath`). Without that origin, the OAuth
token exchange is refused by the browser's own CSP before it reaches the
network, while the top-level `/authorize` redirect still works — it is a
navigation, and `connect-src` does not govern navigations — so, in that
file's own words, *"sign-in appears to proceed and then fails silently at the
callback."* A Connected site has the same defect available to it, with the
difference that no code of ours can prevent it.

Two practical notes carried over from that file's reasoning:

- **Name the `wss:` form explicitly** rather than trusting the `https:` entry
  to cover the WebSocket. CSP3 says `connect-src 'self'` matches `ws:`/`wss:`
  on the same origin, and Chrome/Firefox implement that, but Safari's support
  has been inconsistent for long enough that `component/edge/csp.go` names
  the same-origin WebSocket origin explicitly (`wsOriginOf`) rather than
  relying on it. A cross-origin `wss://api.<domain>` is not the same-origin
  case that guards against, but it is the same scheme-matching family, and
  spelling it out costs one token.
- **Emit the origin, never a path.** The edge's policy deliberately carries
  only the identity *origin*, so the value survives an endpoint path
  changing. Do the same.

For contrast, MemQL *does* generate the policy for pages it serves itself, from
the same configuration the bundle reads, so the two cannot drift apart. A
Connected site is outside that mechanism by construction — which is the whole
reason it needs writing down here.

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

The one-system property is the substantive difference. Under one MemQL cluster
per customer, "we build it, then hand it over if they want to run it
themselves" is a promise you can keep about the cluster — and Connected leaves
their hosting outside the thing you would be handing over. That is fine when
they want it that way and a limitation when they do not.

## Reference

- Design: `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
  (epic [memql#3700](https://github.com/znasllc-io/memql/issues/3700)) —
  decision D9 is the same-origin proxy that rung 2 gets instead of this page's
  CORS work.
- Generator: `scripts/sdk-gen/main.go`, `sdk/gen/emit_ts.go`,
  `sdk/gen/emit_concepts.go`.
- The runtime core's own surface: `sdk/ts/README.md`.
