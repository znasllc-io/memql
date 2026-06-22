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
| `TELNYX_API_KEY` | Telnyx v2 API key. Resolved at carrier selection; a missing key errors only on first use. |
| `TELNYX_CONNECTION_ID` | The Telnyx connection fronting the `livekit/sip` edge. Inbound DIDs are assigned to it so PSTN calls route to the edge. Required for `ConfigureInbound`. |

A read-only live API test (`-tags telnyx_live`) exercises search + list
against the real account without spending money:

```bash
TELNYX_API_KEY=... go test -tags telnyx_live -run TestLive ./integrations/telephony/telnyx
```

A full buy / configure / release round-trip provisions a real DID and is an
owner-driven step.
