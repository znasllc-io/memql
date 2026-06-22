---
title: Telephony -- PSTN calling for memQL voice agents
audience: public
status: stable
area: operate
sinceVersion: 0.9.90
owner: znas
---

# Telephony -- PSTN calling for memQL voice agents

memQL voice agents can answer inbound PSTN calls and place outbound PSTN
calls. A phone call is just another LiveKit room participant: a self-hosted
`livekit/sip` edge turns a SIP call into a room participant (inbound) and dials
a number into a room on demand (outbound), and the existing voice agent
answers via the OpenAI Realtime path. No Twilio.

Telephony is **core and product-agnostic**. Calls and numbers bind to a
generic [partition](../concepts/partition-scoping.md), never to a CoPresent
`space`. A product may map its own scope onto a partition in its pack; the
telephony core only ever sees `partitionId`.

> Epic 4 (memql#1906) builds this in dependency-ordered slices. This page
> grows with each slice. **Live PSTN verification** (a real inbound and a real
> outbound call against a hand-bought DID) is an owner-driven staging step.

---

## Carrier abstraction (`CarrierProvider`)

PSTN access is carrier-agnostic. `integrations/telephony` defines a
`CarrierProvider` interface and a registry selected at runtime by
`MEMQL_TELEPHONY_CARRIER` (default `telnyx`):

```go
type CarrierProvider interface {
    Name() string
    SearchNumbers(ctx, NumberQuery) ([]Number, error)
    BuyNumber(ctx, providerID) (Number, error)
    ReleaseNumber(ctx, e164) error
    ConfigureInbound(ctx, e164, sipEdgeURI) error
    ListNumbers(ctx) ([]Number, error)
}
```

Each carrier is exactly one package implementing the interface. Adding a
second carrier (Skyetel, voip.ms, ...) is a new package plus a one-line
`RegisterCarrier` -- no change to any caller. The SIP trunk itself is
abstracted for free by LiveKit SIP (a carrier is an inbound/outbound trunk
record), so switching carriers never touches the media path.

### Telnyx (first carrier)

`integrations/telephony/telnyx` implements `CarrierProvider` over the Telnyx
v2 REST API (number search / purchase / release / inbound-routing). Chosen for
a clean programmatic number API and a low per-minute trunk rate
(~$0.005/min, ~$1/DID/mo). Credentials are environment-delivered (via
external-secrets in cluster); the package never reads secrets directly.

| Env var | Purpose |
|---|---|
| `MEMQL_TELEPHONY_CARRIER` | Active carrier name (default `telnyx`). |
| `MEMQL_TELEPHONY_TELNYX_API_KEY` | Telnyx v2 API key. Resolved at carrier selection; a missing key errors only on first use. |
| `MEMQL_TELEPHONY_TELNYX_CONNECTION_ID` | The Telnyx connection fronting the `livekit/sip` edge. Inbound DIDs are assigned to it so PSTN calls route to the edge. Required for `ConfigureInbound`. |
| `MEMQL_TELEPHONY_TELNYX_BASE_URL` | Optional override for the Telnyx v2 API root (default `https://api.telnyx.com/v2`). Mainly for tests / sandboxes. |

A read-only live API test (`-tags telnyx_live`) exercises search + list
against the real account without spending money:

```bash
MEMQL_TELEPHONY_TELNYX_API_KEY=... go test -tags telnyx_live -run TestLive ./integrations/telephony/telnyx
```

A full buy / configure / release round-trip provisions a real DID and is an
owner-driven step.

---

## SIP edge (livekit/sip)

`livekit/sip` runs beside the self-hosted LiveKit server (sharing an in-cluster
Redis) and turns a PSTN call into a LiveKit room participant. Manifests:
`deploy/k8s/base/{redis,livekit-sip,externalsecret-telephony}.yaml`. SIP
signaling (UDP/TCP 5060 + TLS 5061) is exposed via a LoadBalancer locked down
with `loadBalancerSourceRanges` = the carrier signaling IPs (set per-env; an
open SIP port is a toll-fraud magnet).

## Inbound

A SIP inbound trunk + an Individual dispatch rule route a called DID to a
**partition-scoped room** `tel-<partitionId>-<auto>` (the partition is carried
in the rule metadata). The voice-agent dispatcher serves `tel-` rooms exactly
like product rooms, so the existing realtime agent answers the phone. A LiveKit
webhook writes one append-only `v1:telephony:call` per completed leg with the
real duration + disposition + cost. Provision with the `provisionInbound`
capability.

## Outbound (agent tools)

Agents place + control calls via tools exposed through the realtime bridge
(assign the telephony skill to an agent to enable them):

- `place_call(to, from, partitionId, dtmf?)` — dials the callee into a
  partition-scoped room the agent talks in; optional post-dial DTMF for IVRs.
- `end_call`, `transfer_call`, `send_dtmf` — control a live call.

Outbound trunk dial settings: `MEMQL_TELEPHONY_OUTBOUND_SIP_ADDRESS` +
`MEMQL_TELEPHONY_OUTBOUND_AUTH_USERNAME` / `_PASSWORD`.

## Provisioning (owner/admin only)

Backend functions backed by `CarrierProvider`, gated to owner/admin:
`telephonySearchNumbers`, `telephonyBuyNumber` (assigns the DID to a partition,
points it at the SIP edge, and provisions inbound routing),
`telephonyConfigureInboundNumber`, `telephonyReleaseNumber`.

## Cost controls

OpenAI Realtime audio is ~90% of per-minute cost; the carrier is ~8%. Worst
case ≈ $0.11/min. Levers (priority order): prompt **caching on** (provider
default), **trim/summarize** context past N minutes, aggressive **barge-in**
cancel, **VAD silence-gating** (`server_vad` energy gate). Silence ≈ $0. Every
`v1:telephony:call` carries a `costEstimate` (upper bound from wall-clock;
override the model rate with `MEMQL_TELEPHONY_MODEL_COST_PER_MINUTE`).

## Compliance

Outbound to an **opted-out** number is blocked (TCPA) via the `consent` concept
(`CheckOutboundAllowed` fails closed on a lookup error); a caller-ID is
required. DIDs carry `e911Registered` / `callerIdVerified` state — register
E911 before a DID goes live. STIR/SHAKEN attestation is carrier-side (Telnyx).
Manage with the owner/admin `setConsent` / `registerE911` capabilities.
