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

> **INFO: the two edge hosts are not serving yet.** The `edge` node type exists
> — build tag `edge`, `make edge`, documented in
> [build-tags.md](../build/build-tags.md) — but `svc/edge` and the site-serving
> path ship with the site-hosting work (memql#3714 and the rest of epic
> memql#3700). Until that lands, treat the last two rows of the table as the
> committed design rather than as something to curl. The site-hosting runbook
> that covers deploying a site, rolling one back and the bundle contract is
> forthcoming on the same work; this page deliberately says nothing more about
> sites than the rule that routes them.

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

> **WARNING: a missing rule does not 404. It hands an HTTP/1.1 request to an
> h2c backend and fails with a protocol error naming nothing.**

There is no 404 in the access log pointing at the path, no handler to add a log
line, and nothing on either side that names the cause. That is why the list
became generated output with a CI gate: a new public HTTP path on the bff
either reaches the front door or breaks the build.

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
up. The certificate is the matching SAN pair (`*.<domain>` and `<domain>`),
issued once at install — by mkcert locally, cert-manager in the cloud — so a
new site needs no reissue either.

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
scripts/install/verify-frontdoor.sh              # dns + tls + h2, per host
curl -sS --http1.1 https://api.memql.localhost/healthz
```

`verify-frontdoor.sh` records three checks per host: DNS resolution, TLS, and
whether the door negotiates h2 — gRPC cannot exist over HTTP/1.1, so a door
that answers but will not speak h2 is a broken door.

> **WARNING: an h2 probe passes while an HTTP/1.1 path is broken.** That is
> exactly how `/unsubscribe` stayed unrouted without anyone noticing. Extending
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
