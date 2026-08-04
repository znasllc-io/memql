---
title: Inbound delivery -- signature-verified requests staged as graph rows
audience: internal
status: accepted
area: internal
sinceVersion: 0.12.2
owner: znas
---

# ADR: Inbound delivery -- signature-verified requests staged as graph rows

> Design deliverable for memql#2957, the counterpart to outbound delivery
> (memql#2521, [outbound-delivery-adr.md](outbound-delivery-adr.md)). Outbound is
> the outbox a product stages and the engine drains. Inbound is the inbox the
> engine stages and a product drains.

## 1. Context

A pure-DSL product can react to graph events and call queries, mutations and
logic, but the only construct that reaches outside the engine is an
`@executor("integration.<name>.*")` builtin -- and that is **outbound**: DSL
calls Go, on the DSL's schedule.

For **inbound** there was no seam at all. `component/memql/plugins.go`'s
`PluginContext` exposes no HTTP route registration, so a downstream product
could not add a webhook receiver as a bff plugin even if it were willing to
write bespoke Go.

The consequence is that every product needing to ingest from a third party
stands up its own sidecar holding a service-account JWT and speaking the WS.
That works, and fylo (`znasllc-io/fylo-warehouse-os`) shipped exactly that for
its v1. But it is a per-product reinvention of authentication, signature
verification, replay protection and delivery bookkeeping -- the same argument
that justified outbound.

## 2. Decision

A **product-agnostic receiver on the bff** at `POST /inbound/{source}` that:

1. refuses anything not on a deploy-configured **source allowlist**;
2. verifies a per-source **HMAC signature**, with an optional timestamped
   replay window;
3. stages the verified request as a `v1:platform:inboundRequest` row;
4. answers `202` and leaves the work to whichever product automation triggers
   on `node.created` for that concept.

No product Go, and no product-specific code in the engine.

### 2.1 Why HTTP, when the policy says gRPC

CLAUDE.md's endpoint-protocol policy makes gRPC the default and requires
explicit approval for a new HTTP endpoint. This is the documented external-
requirement exception, in its purest form: **the third party dials us.**
Shopify, Amazon SP-API, TikTok Shop and a POS terminal will POST to a URL and
nothing else. There is no version of this capability that speaks gRPC.

memql#2957 is that approval in writing ("A receiver endpoint on the bff"), and
CLAUDE.md's exception table carries the new row.

### 2.2 Why the bff, and only the bff

The bff is the frontend-facing node an ingress already routes external traffic
to. An internal node has no reason to carry a route a third party dials, and
mounting it there would widen the externally-reachable surface for nothing.

### 2.3 Why it mounts unconditionally

The route is registered whether or not any source is configured. With an empty
allowlist -- the default -- it answers `404` to everything, so the cost is a
route that admits nobody.

The alternative, mounting only when configured, makes "did the operator set the
env" and "is the endpoint there" two different questions with the same symptom
(a 404), which is worse to debug for no security gain.

## 3. Delivery contract

### 3.1 The status machine

The row's `status` is **product-side handling state, not delivery state** --
the inverse of outbound, and the one place the symmetry deliberately breaks. By
the time a row exists the delivery has already succeeded; what is unknown is
whether the product has dealt with it.

```
received ──▶ processing ──▶ processed
                 │
                 └────────▶ failed
```

The engine only ever writes `received`. Everything after it is the product's,
via `updateInboundRequestStatus`.

### 3.2 At-least-once, and the idempotency key

Every sender in scope retries, so redelivery is normal rather than
exceptional. The row id is **derived** from `(source, identityKey)`:

```
id          = "inb" + hex(sha256(source + NUL + identityKey))[:32]
identityKey = hex(sha256(signed))            # signed = body,
                                             #   or timestamp + "." + body
```

`identityKey` is the digest of exactly what the HMAC covered, and **only** that.
A redelivery therefore renders the *same id*, and `@createOnly("status",
"processedAt", "lastError")` on `stageInboundRequest` preserves the product's
handling state instead of resetting it to `received`.

**Why not the sender's dedupe header.** It was, and that was a defect found in
the landing review. The vendor signs the body -- and the timestamp where one is
configured -- but it does not sign `..._DEDUPE_HEADER`, because that header is
our configuration and not part of its scheme. Deriving identity from an unsigned
header meant ONE captured, still-valid request minted unbounded distinct rows:
the signature stays valid because the body is unchanged, while varying the
header varies the id. No forged signature required.

Folding the header in as a subordinate distinguisher does not fix it either --
if it contributes to the id at all, varying it still varies the id. Any unsigned
input in an identity is attacker-multipliable, so identity is signed material
alone. The header is still recorded on the row; it simply does not decide the
id. `component/inbound/identity_test.go` pins the attack and the three
properties that make dropping it affordable.

The cost is real and bounded: two deliveries whose signed bytes are identical
now collapse onto one row. For a redelivery that is the wanted behaviour. For a
sender legitimately emitting two distinct events with byte-identical payloads,
`..._TIMESTAMP_HEADER` separates them -- and being signed, it cannot be forged.

Deriving the id rather than looking up an existing row is deliberate: a
read-then-write has a race between two replicas receiving the same redelivery,
and the derived id has none.

The composition must be **injective** or two distinct events collapse onto one
row -- the memql#2980 class. It is, and the argument is now simpler than it was:
a source name is matched against `^[a-z0-9][a-z0-9_-]{0,63}$`, and `identityKey`
is always a hex digest this receiver produced. Neither can contain NUL, so the
split at the first NUL is unique and equal concatenations force equal parts.

### 3.2.1 At-least-once means the EVENT fires again

The row collapses; the event does not. Staging a redelivery is still a write,
and a write publishes `node.created`, so a product automation triggering on it
**fires again**. `@createOnly` prevents the status being reset -- it does not
prevent the second firing.

This is at-least-once delivery with a stable idempotency key, not exactly-once
processing, and the operator doc says so in those words. Making it exactly-once
would need the engine to publish conditionally on first insert, which is an
engine-wide change to event semantics and out of scope here. The contract a
product codes against is: branch on `status` before doing work.

### 3.3 What the response codes mean

| code | when | sender should |
|---|---|---|
| `202` | staged (or re-staged onto the same row) | stop |
| `404` | source unlisted, misconfigured, receiver disabled, or the path is nested | stop |
| `405` | not a POST | stop |
| `401` | signature absent, malformed, mismatched, or outside the replay window | stop |
| `413` | body over `MEMQL_INBOUND_MAX_BODY_BYTES` | stop |
| `400` | body is not valid UTF-8, or the dedupe header is malformed | stop |
| `503` | verified, but the row could not be staged | **retry** |

`503` rather than `500` on a staging failure is the load-bearing one: the
delivery was valid and we simply could not record it, so the sender must retry
-- and the derived id means the retry lands on the same row.

## 4. Security invariants

1. **Deny by default.** `MEMQL_INBOUND_SOURCE_ALLOWLIST` is empty out of the
   box and the receiver admits nothing. There is no wildcard.
2. **Misconfiguration fails closed.** A source an operator listed but did not
   finish configuring -- no scheme, unknown scheme, missing secret, missing
   header -- is *dropped* and answers 404. It is never admitted unverified. A
   receiver serving one of five sources is a visible partial outage; a receiver
   serving five with one unsigned is a silent hole.
3. **Unverified is explicit, per source, and recorded.** `scheme=none` exists
   for a sender already behind another trust boundary. It must be spelled out,
   it logs a warning at boot, and every row it stages carries
   `signatureVerified=false`. That field means "this source runs unverified",
   never "this request failed" -- a request that fails is refused at the edge
   and no row is ever staged.
4. **Secrets never touch the graph.** The HMAC key lives in env / the secret
   store. The row records the verdict, not the key.
5. **The caller learns nothing.** Every signature failure answers a flat `401`.
   "Wrong digest", "missing header" and "stale timestamp" are three facts about
   the deployment's configuration; the operator gets them in the node log.
6. **Bounded before verified.** The body cap is enforced while reading, so an
   unauthenticated caller cannot make the node allocate arbitrarily before the
   signature check has a chance to reject it. An over-cap body is *detected*,
   not truncated -- a truncated body would fail its own signature and look like
   an attack.
7. **Replay is bounded when the sender allows it.** With a timestamp header the
   window is enforced in *both* directions, and the timestamp is bound into the
   signed payload (`"<ts>.<body>"`) so it cannot be rewritten after signing.
   Without one, a signature is valid forever and `dedupeKey` is the only
   defence -- which is why it always has a value.
8. **The route is declared.** It is in `server.HandlerAuthorizedPaths()`, not
   `PublicPaths()`, so the memql#2939 boot assertion accounts for it without
   bypassing the verifier for `/inbound/*` on every verifier-consuming node.

## 5. Configuration

Global:

| var | default | meaning |
|---|---|---|
| `MEMQL_INBOUND_ENABLED` | `true` | receiver on/off |
| `MEMQL_INBOUND_SOURCE_ALLOWLIST` | *(empty)* | comma-separated source names |
| `MEMQL_INBOUND_MAX_BODY_BYTES` | `262144` | body cap |
| `MEMQL_INBOUND_TIMESTAMP_TOLERANCE_SECONDS` | `300` | replay window |

Per source (`<NAME>` uppercased, `-` mapped to `_`):

| var | required | meaning |
|---|---|---|
| `..._SIGNATURE_SCHEME` | yes | `hmac-sha256-hex`, `hmac-sha256-base64` or `none` |
| `..._SECRET` | unless `none` | shared HMAC key |
| `..._SIGNATURE_HEADER` | unless `none` | where the signature arrives |
| `..._SIGNATURE_PREFIX` | no | stripped before decoding, e.g. `sha256=` |
| `..._TIMESTAMP_HEADER` | no | enables the replay window |
| `..._DEDUPE_HEADER` | no | sender's idempotency key |

### 5.1 Why schemes rather than vendors

Every third party spells the same HMAC-SHA256 differently. GitHub sends
`sha256=`-prefixed hex; Shopify sends bare base64; the timestamped variants
sign `"<ts>.<body>"`. An engine capability cannot carry a vendor table and stay
product-agnostic, so a source names the *encoding* it uses plus the header and
optional prefix. That covers the six channels memql#2957 names without a line of
vendor code in the engine, and a seventh is a config change rather than a
release.

Worked example -- a GitHub-shaped sender:

```
MEMQL_INBOUND_SOURCE_ALLOWLIST=gh
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_SCHEME=hmac-sha256-hex
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_HEADER=X-Hub-Signature-256
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_PREFIX=sha256=
MEMQL_INBOUND_SOURCE_GH_SECRET=<shared secret>
```

and a Shopify-shaped one:

```
MEMQL_INBOUND_SOURCE_SHOPIFY_SIGNATURE_SCHEME=hmac-sha256-base64
MEMQL_INBOUND_SOURCE_SHOPIFY_SIGNATURE_HEADER=X-Shopify-Hmac-Sha256
MEMQL_INBOUND_SOURCE_SHOPIFY_DEDUPE_HEADER=X-Shopify-Webhook-Id
MEMQL_INBOUND_SOURCE_SHOPIFY_SECRET=<shared secret>
```

## 6. What a product writes

Nothing in Go. An automation on the staged row:

```memql
@trigger(event="node.created", concept="v1:platform:inboundRequest", partition="*")
automation handleShopifyOrder { ... }
```

...filtering on `source`, reading `body`, and stamping
`updateInboundRequestStatus` when it is done.

## 7. Consequences

**Good.** Authentication, signature verification, replay protection and
delivery bookkeeping are written once. A product ingests from a third party
with zero Go and no sidecar. Delivery state is ordinary graph state, so it is
queryable, auditable and reactable with the constructs a product already uses.

**Costs, stated plainly.**

- The engine now serves an externally-reachable endpoint on the bff. It is
  deny-by-default and declared, but it is surface that did not exist before.
- `body` is stored as text, so a non-UTF-8 payload is refused rather than
  staged. Every sender in scope sends JSON; a binary-payload sender would need
  a follow-up decision (base64 at the edge, or a bytes-typed field).
- There is no engine-side retry or dead-lettering of *product* handling
  failures. A row stamped `failed` sits there until something looks at it. That
  is the product's to own, and deliberately so -- the engine does not know what
  retrying a business event means.
- No per-source rate limiting. The body cap bounds a single request; it does
  not bound a flood. Ingress-level rate limiting is the deployment's job today.

## 8. References

- memql#2957 -- this story.
- memql#2521 / [outbound-delivery-adr.md](outbound-delivery-adr.md) -- the counterpart.
- memql#2939 -- the unauthenticated-surface declaration this route participates in.
- memql#2980 -- the composite-id injectivity class 3.2 is written against.
- `component/inbound/` -- config, verification, handler.
- `dsl/platform/` -- concept, shape, queries, mutations.
