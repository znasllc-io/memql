---
title: Inbound delivery -- the webhook receiver, source allowlists, and signatures
audience: public
status: stable
area: operate
sinceVersion: 0.12.2
owner: znas
---

# Inbound delivery

**Audience:** product authors ingesting third-party events from pure DSL, and
operators configuring per-deployment ingress.
**Design:** `docs/internal/design/inbound-delivery-adr.md` (memql#2957).

The counterpart to [outbound delivery](outbound-delivery.md). Outbound is the
outbox a product stages and the engine drains; inbound is the inbox the engine
stages and a product drains.

A third party POSTs to `/inbound/{source}` on the bff. The receiver checks the
source against a deploy-configured allowlist, verifies the request's HMAC
signature, stages it as a `v1:platform:inboundRequest` row, and answers `202`.
A product automation triggers on the new row. No product Go, no sidecar.

## Nothing is admitted until you say so

The route is mounted on every bff, and out of the box it answers `404` to
everything. A source is served only when it is both **listed** and **fully
configured**; a source that is listed but not configured is dropped and also
answers 404, with the reason in the node's log at boot.

That is deliberate. A receiver serving one of five sources is a visible partial
outage. A receiver serving five sources with one of them unsigned is a silent
hole.

## Configuring a source

Global knobs:

| var | default | meaning |
|---|---|---|
| `MEMQL_INBOUND_ENABLED` | `true` | receiver on/off |
| `MEMQL_INBOUND_SOURCE_ALLOWLIST` | empty (admits nothing) | comma-separated source names |
| `MEMQL_INBOUND_MAX_BODY_BYTES` | `262144` | larger requests are refused with `413` |
| `MEMQL_INBOUND_TIMESTAMP_TOLERANCE_SECONDS` | `300` | replay window, for sources that sign a timestamp |

Per source. `<NAME>` is the source name uppercased with `-` mapped to `_`, so
the source `big-corp` reads `MEMQL_INBOUND_SOURCE_BIG_CORP_*`:

| var | required | meaning |
|---|---|---|
| `..._SIGNATURE_SCHEME` | yes | `hmac-sha256-hex`, `hmac-sha256-base64`, or `none` |
| `..._SECRET` | unless `none` | the shared HMAC key |
| `..._SIGNATURE_HEADER` | unless `none` | which header carries the signature |
| `..._SIGNATURE_PREFIX` | no | stripped before decoding, e.g. `sha256=` |
| `..._TIMESTAMP_HEADER` | no | turns on the replay window |
| `..._DEDUPE_HEADER` | no | the sender's own idempotency key |

There is no vendor list. A source names the *encoding* its signature uses, so a
new sender is a config change rather than a release.

A GitHub-shaped sender:

```bash
MEMQL_INBOUND_SOURCE_ALLOWLIST=gh
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_SCHEME=hmac-sha256-hex
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_HEADER=X-Hub-Signature-256
MEMQL_INBOUND_SOURCE_GH_SIGNATURE_PREFIX=sha256=
MEMQL_INBOUND_SOURCE_GH_SECRET=<shared secret>
```

A Shopify-shaped one:

```bash
MEMQL_INBOUND_SOURCE_SHOPIFY_SIGNATURE_SCHEME=hmac-sha256-base64
MEMQL_INBOUND_SOURCE_SHOPIFY_SIGNATURE_HEADER=X-Shopify-Hmac-Sha256
MEMQL_INBOUND_SOURCE_SHOPIFY_DEDUPE_HEADER=X-Shopify-Webhook-Id
MEMQL_INBOUND_SOURCE_SHOPIFY_SECRET=<shared secret>
```

`..._SECRET` is secret material. It belongs in the deployment's secret store,
never in an overlay.

### `scheme=none`

Accepts without verifying, for a sender already behind another trust boundary
(a mesh-internal producer, a gateway that verified upstream). It has to be
spelled out per source -- an unset scheme is an error, never a silent downgrade
-- it logs a warning at boot, and every row it stages carries
`signatureVerified=false` so an audit query finds them.

## What a product writes

```memql
@trigger(event="node.created", concept="v1:platform:inboundRequest", partition="*")
automation handleShopifyOrder { ... }
```

Filter on `source`, read `body`, and stamp `updateInboundRequestStatus` with
`processing` / `processed` / `failed` as you work it. The engine only ever
writes the initial `received`; everything after that is yours.

The row also carries `contentType`, `dedupeKey`, `signatureVerified` and
`receivedAt`. Query staged rows with `inboundRequestsByStatus(status:
"received")`, or look one up with `inboundRequestByDedupeKey`.

## Redelivery

Every sender in scope retries, so redelivery is normal. The row id is derived
from `(source, dedupeKey)` -- the sender's key when one is configured,
otherwise the SHA-256 of the body -- so a redelivery lands on the **same row**,
and the mutation preserves your handling state rather than resetting it to
`received`. You will not process the same event twice because it arrived twice.

## Reading the response codes

| code | meaning |
|---|---|
| `202` | staged; the row exists |
| `404` | source unlisted, misconfigured, receiver disabled, or a nested path |
| `401` | signature absent, malformed, mismatched, or outside the replay window |
| `413` | body over the cap |
| `400` | body is not valid UTF-8, or the dedupe header is malformed |
| `503` | verified, but staging failed -- **the sender should retry** |

`401` is deliberately flat: the caller is unauthenticated, so *why* a check
failed is a fact about your configuration and is not theirs to learn. The
reason is in the node log.

## Troubleshooting

**Everything 404s.** The source is not in `MEMQL_INBOUND_SOURCE_ALLOWLIST`, or
it is listed but incomplete. Check the node log at boot: a dropped source logs
an error naming the exact env var that is missing.

**Everything 401s.** Usually the prefix. GitHub sends `sha256=<hex>`; without
`..._SIGNATURE_PREFIX=sha256=` the receiver tries to decode the whole thing as
hex and refuses. After that, check the secret matches the one configured at the
sender, and that the scheme's encoding is right -- hex and base64 of the same
digest are both "valid signatures" and neither decodes as the other.

**Everything 401s only sometimes.** If the source has a `..._TIMESTAMP_HEADER`,
the replay window is in force in both directions and clock skew between the
sender and the node will show up as intermittent refusals. Widen
`MEMQL_INBOUND_TIMESTAMP_TOLERANCE_SECONDS` or fix the clock.

**Rows arrive but nothing happens.** The receiver's job ends at `received`.
Check the product automation's trigger and filter.

## What this does not do

- **No per-source rate limiting.** The body cap bounds a single request; it does
  not bound a flood. Rate limiting is the ingress's job today.
- **No engine-side retry of product handling failures.** A row stamped `failed`
  sits there until something looks at it -- the engine does not know what
  retrying a business event means.
- **No binary payloads.** `body` is text; a non-UTF-8 request is refused with
  `400` rather than staged corrupted.
