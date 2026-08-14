---
title: The cluster front door — five host rules
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# The cluster front door — five host rules

A memQL cluster has **one** door: port 443 on one L7 proxy — the k3s-bundled
traefik locally, nginx in the cloud — terminating TLS once with a wildcard plus
apex certificate and routing by hostname. Port 80 exists only to redirect.

Behind that door are **five host rules**, committed to `deploy/k8s` and the same
in every environment, plus a **separate media plane** that does not and cannot
go through it.

Five is the number, and it stays five no matter how many customers,
applications or websites the cluster serves. That property is what this page is
about.

Related: [environment-parity.md](environment-parity.md) ·
[install-prerequisites.md](install-prerequisites.md) ·
[reproduce-staging-locally.md](reproduce-staging-locally.md)

---

## The five hosts

| Host | Backend | Protocol |
|---|---|---|
| `api.<domain>` | `svc/bff:50051` **and** `svc/bff-http:8085` | h2c (gRPC) + http |
| `identity.<domain>` | `svc/identity:8085` | https |
| `mcp.<domain>` | `svc/mcp:8090` | http |
| `*.<domain>` | `svc/edge:8085` | http |
| `<domain>` (apex) | `svc/edge:8085` | http |

**`api.<domain>`** is the API edge, and it is named for that role rather than
for the first client that happened to connect to it. Everything that speaks to
the engine dials it: the Cockpit, the VS Code extension, `sdk/go`, `sdk/ts`,
workers, and the browser bridge at `/memql/ws`. So do strangers — a webhook
POSTing to `/inbound/<source>`, a recipient's mail client executing RFC 8058
one-click against `/unsubscribe`. It is the one host carrying two backends;
[the next section](#one-constraint-explains-most-of-this-page) is why.

**`identity.<domain>`** is the identity service: `/login`, the WebAuthn
ceremonies, the OAuth endpoints, `/.well-known/jwks.json`, `/me/*`, `/enroll`
and `/device`. It serves TLS in-cluster under the internal CA, which is why the
proxy speaks https to it and skips verification on that hop — the hop stays
inside the cluster network. See
[auth/identity-service.md](auth/identity-service.md).

**`mcp.<domain>`** is the MCP Streamable HTTP protocol head. It gets a host of
its own rather than a path under `api.` for the same per-Service reason below,
and it is not proxied through the edge either: MCP clients configure a URL,
they are not browsers, and an extra hop on a tool-calling path buys nothing.
See [mcp-connect.md](mcp-connect.md).

**`*.<domain>` and the apex** both point at the `edge` node, which resolves the
request `Host` header against a `v1:platform:site` row and serves that site's
bundle. The apex is not a special case: for a customer's own cluster the bare
domain **is** their main website, so it is a site row like every other one.

> **INFO: the edge route is live; the edge backend is not deployed yet.** Two
> separate things, worth keeping apart.
>
> **The route exists**: `deploy/k8s/overlays/local/edge-front-door.yaml`, both
> rules, on `main`. **The backend does not**: there is no `svc/edge`, because the
> edge Deployment and Service ship with **memql#3714**. The `edge` node type
> itself exists — build tag `edge`, `make edge`, see
> [build-tags.md](../build/build-tags.md) — so what is missing is a deployment
> of it, not the code.
>
> **What that produces is a 404, not a 503.** Traefik drops the entire router
> when its backend Service is absent, so the request matches no router at all
> and gets traefik's own 404 — it never reaches a handler that could answer 503.
> Traefik says so on every reconcile:

```
ERR Cannot create service error="service not found" ingress=edge-front-door serviceName=edge servicePort=8085
```

An operator seeing 404 on a hosted-site hostname will reasonably conclude the
*rule* is missing. The rule is fine and the node is not deployed, and those are
different fixes — so check `kubectl -n memql get svc edge` before touching
anything under `deploy/`.

The site-hosting runbook covering how to deploy a site, roll one back and what
the bundle contract is arrives with the same work. This page deliberately says
nothing more about sites than the rule that routes them.

### Exact-versus-wildcard precedence is a real question, and it is not settled

`*.<domain>` also matches `api.<domain>`, `identity.<domain>` and
`mcp.<domain>`. So whether the four named hosts keep their own backends is not a
detail — it is a **load-bearing assumption of the five-host design**, and it is
worth knowing the state of it.

**Precedence between an exact host and a wildcard host is
implementation-defined.** The Ingress specification says what a wildcard host
*matches* — exactly one DNS label, never the apex — and says nothing about which
rule wins when an exact rule and a wildcard rule both match. There is no spec
guarantee to lean on.

**On traefik's defaults it probably points the wrong way.** Traefik's Kubernetes
Ingress provider compiles a wildcard host into a `HostRegexp` matcher, and
traefik's default router priority is **the length of the matcher rule**. In
shape, the two rules are:

```
Host(`api.memql.localhost`) && PathPrefix(`/`)
HostRegexp(`^[a-zA-Z0-9-]+\.memql\.localhost$`) && PathPrefix(`/`)
```

The wildcard's rule is the **longer** string, so by that default heuristic it
outranks the exact one. (The precise regex traefik emits varies by version; the
relative length is the part that carries the argument, and it is not close.) The
consequence is not a narrow band of unusual hostnames — it is that
`api.<domain>/` goes to the site server, so **the entire API would be served by
the edge**: every SDK, the Cockpit, the VS Code extension and every worker dials
that host. ingress-nginx ranks differently again — longest path, then oldest
Ingress — so it would not even be the same wrong answer in both environments.

**So precedence has to be declared, not inherited.** The fix is an explicit
`traefik.ingress.kubernetes.io/router.priority` on the exact hosts, with the
ingress-nginx equivalent worked out beside it so the two environments rank
identically rather than coincidentally. **No file under
`deploy/k8s/overlays/local/` sets a priority today**, and the declaration lands
with the edge Deployment (memql#3714) — outstanding work, not something already
handled.

**Until then the property is unestablished — but no longer for want of anything
looking.** `scripts/install/verify-frontdoor.sh` carries a `precedence` check,
reported per host for `api.` and `identity.` (never `mcp.`, whose Ingress routes
`:8090` while the `/healthz` the check reads is on `:8085`), and today it reports
`inconclusive` rather than passed — so the gap is observable in a run instead of
asserted on this page. A probe run today could not settle it either, for the
reason the callout above gives — with `svc/edge` absent, traefik drops the
wildcard router entirely, so there is nothing for an exact host to take
precedence *over*. An
exact host answering from its own backend under those conditions measures the
absence of the wildcard router, not precedence. Reading that result as the
latter is exactly how this assumption came to be written down as a fact.

Worth being clear about even once a check exists: a probe compares responses at
the paths it probes, so it is evidence for the design rather than proof of it at
every path. A **declared** priority holds at every path, which is why declaring
it matters more than measuring it.

## One constraint explains most of this page

**An ingress controller's backend protocol is a per-Service setting, not a
per-port one.**

That single fact shapes the whole front door:

- It is why `bff` and `bff-http` exist as **two Services over one Deployment**.
  Locally the `bff` Service carries
  `traefik.ingress.kubernetes.io/service.serversscheme: h2c` so traefik speaks
  HTTP/2 cleartext to the gRPC edge on `:50051` — and that annotation applies
  to every port of that Service. The cloud has the mirror-image constraint
  (nginx `backend-protocol: GRPC`). An HTTP/1.1 asset fetch dialed as h2c does
  not work against Go's `net/http` server, so the portal bundle would simply
  fail to load.
- It is why `api.<domain>` needs **two rules**, not one: `/` to
  `svc/bff:50051` for gRPC, and the declared HTTP path set to
  `svc/bff-http:8085`.
- It is why MCP is a **host** rather than a path: `svc/mcp` is a different
  Service on a different port speaking plain HTTP.
- It is why the HTTP path set has to be **complete** — which is the next
  section, and the reason it is generated.

Splitting protocols across Services makes each backend's protocol
unambiguous, and it is the same shape in every environment. Only which ingress
controller points at it differs, and that is a value rather than architecture.

## The one generated rule

Exactly one thing in the front door is derived rather than authored: the list
of HTTP paths on `api.<domain>`. It lives between markers in
`deploy/k8s/overlays/local/api-front-door.yaml`:

```yaml
          # BEGIN generated bff HTTP paths -- make frontdoor-paths
          # END generated bff HTTP paths
```

| Command | What it does |
|---|---|
| `make frontdoor-paths` | Regenerates the block from the code |
| `make frontdoor-paths-check` | Fails when the checked-in block is stale |

The set is exactly `server.PublicPaths()` ∪ `HandlerAuthorizedPaths()` ∪
`SelfAuthenticatedPaths()` minus the gRPC surface. Those functions already
exist, and `TestContractRoutesMatchesRegistration` already verifies them
against what the handler really registers — so the front door reads from a list
the repo keeps honest instead of from somebody's memory. Hand-edits between the
markers are reverted by the generator; the pair mirrors `make arch-model` /
`make arch-model-check`.

**Why it is generated is memQL's own history.** `POST /inbound/{source}` and
`GET+POST /unsubscribe` are documented public HTTP exceptions that third
parties dial — a Shopify webhook, a mail client executing RFC 8058 — and **no
overlay in this repository routed either one.**

And here is the part worth memorising, because it is what makes a
hand-maintained list of ingress paths a bad idea rather than merely a chore:

> **WARNING: a missing rule does not 404.** The HTTP/1.1 request falls through
> to the catch-all `/` rule, reaches the gRPC backend, and comes back as an
> HTTP `415` describing a content-type problem.

```
HTTP/1.1 415 Unsupported Media Type
Content-Type: application/grpc
Grpc-Status: 3
Grpc-Message: invalid gRPC request content-type ""
Content-Length: 0
```

Measured on a live cluster against `api.memql.localhost`, on four paths that
were not yet in the generated block at the time: `/unsubscribe`,
`/inbound/shopify`, `/memql/query` and `/spaces/x/attachments`. All four
answered exactly that, with an empty body.

Read the trap carefully, because it is worse than an error that says nothing.
The transport is fine — curl completes, TLS is fine, the door is up. The
response **names a cause, and it is the wrong one**: it says the caller sent no
gRPC content-type, when what actually happened is that a routing rule is
missing and the request was handed to a server that only speaks gRPC. Nobody
debugging a webhook that stopped arriving is going to read "invalid gRPC request
content-type" and think "Ingress".

What makes it findable is that `Grpc-Status: 3` on a path you expected to be
plain HTTP is a specific, greppable signature. If you see it, check whether the
path is in the generated block before you look at anything else.

That is why the list became generated output with a CI gate: a new public HTTP
path on the bff either reaches the front door or breaks the build. For contrast,
on the same host the routed paths answer 200 — `/healthz` returns the bff's
health JSON, `/portal/` returns the portal's `index.html`.

See [inbound-delivery.md](inbound-delivery.md) and
[campaign-sending.md](campaign-sending.md) for the two exceptions themselves.

## Why the count is five, and must not grow

Because **a site is data, not infrastructure.**

`v1:platform:site` carries a hostname, a kind, a bundle reference and a status.
Deploying a site is an upload plus a row write; rolling one back is a row
write. There is no Kubernetes object per site, no git commit per site and no
ArgoCD reconcile per site — so adding the tenth website to a cluster adds a
**row**, not a routing rule, not a certificate SAN and not an overlay patch.

The `*.<domain>` rule is what makes that possible: one wildcard rule routes
every present and future site name to the one node that knows how to look them
up — every name, that is, that the four exact hosts have not claimed, which is
an assumption about rule ranking rather than a given
([above](#exact-versus-wildcard-precedence-is-a-real-question-and-it-is-not-settled)).
The certificate is the matching SAN pair (`*.<domain>` and `<domain>`), issued
once at install — by mkcert locally, cert-manager in the cloud — so a new site
needs no reissue either.

This is not a hole in "ArgoCD is the only deploy path". That rule is about the
shape of the system: the edge Deployment lives in git and is reconciled like
everything else. A customer's landing page is no more a Kubernetes object than
a chat message is.

Two consequences worth stating plainly:

- **Adding a sixth host rule is a design change, not a configuration change.**
  If a proposal needs one, the thing being added is probably a site.
- **A wildcard DNS record is not a wildcard hosts file.** In the cloud
  `*.<domain>` resolves at the DNS layer and nothing local is involved. On a
  developer machine there is no wildcard in `/etc/hosts`, so each name has to
  be listed in the managed block `scripts/install/hosts-entries.sh` owns. That
  is a property of hosts files, not of the front door.

## The media plane is separate, permanently

Voice is not one of the five hosts and never will be. **WebRTC media is UDP and
cannot traverse an HTTP front door.**

| Environment | Media plane |
|---|---|
| Cloud | `livekit.<domain>` for signaling, plus a LoadBalancer carrying UDP 7882 and TCP 7881 for media |
| Local | LiveKit Cloud (the local overlay deletes the self-hosted livekit workloads) |

So the honest statement of the topology is **five HTTP host rules plus a
separate media plane** — said here rather than left for someone to discover
while wondering which Ingress rule is missing. See
[livekit-provision.md](livekit-provision.md) and
[telephony.md](telephony.md).

## Local versus cloud

The front door is the same shape everywhere. What differs is which
implementation of each interchangeable part is in play.

| | Varies | Does not vary |
|---|---|---|
| Proxy | traefik locally (`serversscheme` annotations) / nginx in the cloud (`backend-protocol`) | that there is exactly one L7 proxy on 443 |
| Certificate | mkcert locally / cert-manager in the cloud | the `*.<domain>` + apex SAN pair, issued at install |
| DNS | the `/etc/hosts` managed block locally / real DNS in the cloud | the hostnames themselves |
| Secrets | `make secrets` locally / External Secrets + Key Vault in the cloud | — |

Fixed everywhere: **the five hosts, the paths behind them, and the dial path a
client uses** (`https://api.<domain>`, TLS at the proxy, gRPC to the bff). The
domain itself is a value too — one `MEMQL_DOMAIN`, from which every
domain-shaped setting is derived at boot — so no file under `deploy/` names a
domain except the Ingress hosts, which carry a committed default.

The standard those rows answer to, and the reason the divergences are confined
to annotations and secret sources, is
[environment-parity.md](environment-parity.md). This page does not restate it.

## Port 5432 is debug-only

The local cluster maps Postgres on `5432` and that is **not a connection
path**. It is there so an operator can attach `psql` to a local database. No
client reaches memQL that way in any environment, nothing in the connection
model depends on it, and a proposal that routes application traffic through it
is the port-forward-as-architecture anti-pattern
[environment-parity.md](environment-parity.md) rejects.

## Verifying a front door

Requires a running cluster, so it belongs on an operator machine or in CI.

```bash
kubectl -n memql get ingress -o wide             # the Ingress objects and their hosts
scripts/install/verify-frontdoor.sh              # dns + tls + precedence per host, h2 once
curl -sS --http1.1 https://api.memql.localhost/healthz
```

`verify-frontdoor.sh` runs **seven** checks against the default host set, not
three per host: DNS, TLS and precedence for each of `api.` and `identity.`, plus
**one** gRPC/h2 check, against the first host only. `precedence` is the one that
can come back neither passed nor failed — an `inconclusive` check counts in
neither tally, so it leaves `allPassed` alone
([above](#exact-versus-wildcard-precedence-is-a-real-question-and-it-is-not-settled)).

```
allPassed: true, passedCount: 5, failedCount: 0, inconclusiveCount: 2
PASS dns  api.memql.localhost: resolves to 127.0.0.1
PASS tls  api.memql.localhost: TLS handshake ok against a trusted certificate (HTTP 415)
PASS dns  identity.memql.localhost
PASS tls  identity.memql.localhost: ... (HTTP 302)
PASS grpc api.memql.localhost: negotiated HTTP/2 (HTTP 415) -- gRPC can ride this door
INCONCLUSIVE precedence api.memql.localhost: ... the wildcard router is not loaded, so there is nothing for an exact host to take precedence over
INCONCLUSIVE precedence identity.memql.localhost: ... (the same)
```

Two things in that output are deliberate and neither is obvious.

**The status code is informational, not a pass criterion.** Note the `PASS`
beside `HTTP 415`. The script measures *transport reachability* — does the name
resolve to the expected address, does TLS handshake against a trusted
certificate, does the door negotiate h2 — not application health. A 4xx passes
because a door that answers 415 is a door that is up. (That 415 is the same one
described above: the probe hits `/`, which is the gRPC backend.)

**The probe set is smaller than the hosts block, on purpose.** `DEFAULT_HOSTS`
is `api.` plus `identity.` only, while the managed block
`scripts/install/hosts-entries.sh` writes carries five names. Those are
different jobs: `/etc/hosts` has no wildcard semantics so it needs every name
spelled out, whereas the probe set is only the doors we assert are up.

> **WARNING: an h2 probe passes while an HTTP/1.1 path is broken.** That is
> exactly how `/unsubscribe` stayed unrouted without anyone noticing — and note
> the h2 check runs once, so it is not even asserting h2 on every host. Extending
> the script with a plain HTTP/1.1 probe of one bff HTTP path is tracked on epic
> memql#3700; until it lands, the `curl` line above is that check done by hand.
> `--http1.1` is not decoration — curl prefers HTTP/2 over TLS by default, and
> an HTTP/2 request is not what a mail client or a webhook sender will send.

## Reference

- Design: `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
  (epic [memql#3700](https://github.com/znasllc-io/memql/issues/3700)) —
  decisions D3 (the five rules), D5 (a site is data) and D9 (the same-origin
  proxy for hosted sites), each with the alternatives that were rejected.
- The Service split and its annotations:
  `deploy/k8s/components/engine-bff/bff.yaml`.
- The path declarations the block is generated from:
  `component/server/unauthenticated_surface.go`.
