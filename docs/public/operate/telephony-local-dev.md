---
title: Local-dev telephony against LiveKit Cloud
audience: public
status: stable
area: operate
sinceVersion: 0.11.3
owner: znas
---

# Local-dev telephony against LiveKit Cloud

The telephony media plane is **environment-selectable** (Epic #2184):

- **Local dev → LiveKit Cloud.** The local k3d cluster (`make up`) connects
  **outbound** to a LiveKit Cloud project. There is **no** local
  `livekit-server` / `livekit/sip` / coturn for the dev loop — LiveKit Cloud is
  the SIP + WebRTC media plane and its TURN handles NAT.
- **Staging / production → self-hosted LiveKit** (unchanged). See
  [telephony.md](telephony.md) for the self-hosted plane.

This page is the documented bring-up flow for telephony on the **local** plane:
answer an inbound PSTN call and place an outbound call from the local cluster,
end to end, against LiveKit Cloud. It is config only — telephony uses
`lksdk.NewSIPClient` + `NewRoomServiceClient` and the voice-agent dials
`LIVEKIT_URL`, so cloud-vs-self-host is entirely which URL/key/secret the env
points at (#2186).

> **Live calls are owner-run.** Bringing the cluster up and provisioning are
> reproducible by any developer, but *placing/answering a real PSTN call* spends
> carrier + OpenAI money and needs a real DID, so the call steps below are an
> owner-driven step. The full audio + cost verification is
> [#2190](https://github.com/znasllc-io/memql/issues/2190).

---

## Prerequisites

1. **A LiveKit Cloud project** — its `wss://<project>.livekit.cloud` URL plus an
   API key/secret pair.
2. **A Telnyx account** with:
   - a DID (the test line is **+15206400819**),
   - a **SIP Connection** (FQDN/credential) whose destination is the LiveKit
     Cloud **SIP URI** (Cloud is the edge on this plane — see
     `MEMQL_TELEPHONY_SIP_EDGE_URI` below),
   - an **Outbound Voice Profile** for `place_call`.
3. The standard k3d prerequisites (`docker`, `k3d`, `kubectl`) — see
   [reproduce-staging-locally.md](reproduce-staging-locally.md).

---

## Env matrix (local / Cloud plane)

| Var | Value on the local Cloud plane | Delivered via |
|---|---|---|
| `LIVEKIT_URL` | `wss://<project>.livekit.cloud` | `livekit-secrets` (seed-secrets.sh sources it from your env) |
| `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | the Cloud project key/secret (voice-agent pair) | `livekit-secrets` |
| `MEMQL_POLYPHON_LIVEKIT_URL` / `_PUBLIC_URL` | the same Cloud project URL | `livekit-secrets` |
| `MEMQL_POLYPHON_LIVEKIT_API_KEY` / `_API_SECRET` | the same Cloud project key/secret (telephony + token-minter pair) | `livekit-secrets` |
| `MEMQL_TELEPHONY_TELNYX_API_KEY` | Telnyx v2 API key | genesis envelope (global-scope secret) |
| `MEMQL_TELEPHONY_TELNYX_CONNECTION_ID` | the Telnyx Connection pointing at the Cloud SIP URI | genesis envelope |
| `MEMQL_TELEPHONY_SIP_EDGE_URI` | **unset/empty** — see below | n/a |
| `MEMQL_TELEPHONY_OUTBOUND_SIP_ADDRESS` | outbound trunk dial target (Telnyx Outbound Voice Profile) | genesis envelope |
| `MEMQL_TELEPHONY_OUTBOUND_AUTH_USERNAME` / `_PASSWORD` | outbound trunk credentials | genesis envelope |

**Why `MEMQL_TELEPHONY_SIP_EDGE_URI` is empty on the Cloud plane.** On the
self-hosted plane it points at the in-cluster `livekit/sip` edge. On Cloud,
**LiveKit Cloud is the edge**: the Telnyx Connection
(`MEMQL_TELEPHONY_TELNYX_CONNECTION_ID`) is pre-provisioned to send the DID to
the Cloud SIP URI, and the inbound trunk + dispatch rule are created on LiveKit
Cloud via the API (`CreateSIPInboundTrunk` / `CreateSIPDispatchRule`).
`carrier.ConfigureInbound` only requires the connection id, so an empty
`SIP_EDGE_URI` is correct — it is recorded on the `v1:telephony:call` trunk row
for documentation only.

---

## 1. Bring the cluster up pointed at LiveKit Cloud

Export the Cloud project credentials, then `make up`. `seed-secrets.sh` writes
them into `livekit-secrets` (both the bare `LIVEKIT_*` and the
`MEMQL_POLYPHON_LIVEKIT_*` pairs point at the **same** project), and the local
overlay repoints the voice / voice-agent / bff pods at that secret (#2186):

```bash
export LIVEKIT_URL="wss://<your-project>.livekit.cloud"
export LIVEKIT_API_KEY="<cloud-api-key>"
export LIVEKIT_API_SECRET="<cloud-api-secret>"
make up
```

Confirm both consumers dial Cloud outbound (no in-cluster livekit Service
exists locally any more):

```bash
kubectl get pods -n memql -l app=voice
kubectl exec -n memql deploy/voice-agent -- sh -c 'echo "$LIVEKIT_URL"'   # -> wss://<project>.livekit.cloud
kubectl logs -n memql deploy/bff --tail 100 | grep -i livekit              # token mint against the cloud URL
```

The Telnyx + outbound creds are global-scope secrets delivered via the genesis
envelope (`MEMQL_GENESIS_AUTOLOAD`); seed them into your dev envelope to enable
telephony locally (see [env-vars.md](env-vars.md)).

## 2. Provision inbound on LiveKit Cloud

Run the owner/admin `configureInboundNumber` capability
(`telephonyConfigureInboundNumber` → `handleConfigureInbound`). On the Cloud
plane this:

1. points the Telnyx DID at the Connection that fronts the Cloud SIP URI
   (`carrier.ConfigureInbound`), and
2. creates the **inbound trunk + Individual dispatch rule on LiveKit Cloud**
   (`CreateSIPInboundTrunk` + `CreateSIPDispatchRule`) routing the DID to a
   partition-scoped room `tel-<partitionId>-<auto>`.

## 3. Inbound answer (owner-run)

Call the DID (**+15206400819**). The dispatch rule rings it into a `tel-` room;
the voice-agent dispatcher serves `tel-` rooms exactly like product rooms, so
the realtime agent answers — audio both directions. A LiveKit webhook writes one
append-only `v1:telephony:call` per leg.

## 4. Outbound place_call (owner-run)

From an agent with the telephony skill, `place_call(to, from, partitionId)`
dials out through the Telnyx Outbound Voice Profile via LiveKit Cloud
(`MEMQL_TELEPHONY_OUTBOUND_SIP_ADDRESS` + `_AUTH_USERNAME` / `_PASSWORD`).

---

## What "done" looks like

- `make up` brings telephony up against LiveKit Cloud with **no** self-hosted
  `livekit-server` / `livekit/sip` / coturn in the dev loop.
- An inbound call to the DID is answered and an outbound `place_call` connects,
  both from the local cluster (the live audio + ~$0-on-silence cost checks are
  [#2190](https://github.com/znasllc-io/memql/issues/2190)).
- Staging/prod are untouched — still self-hosted, with the no-cloud-leak guard
  (`scripts/deploy/livekit_cloud_guard_test.go`) keeping cloud out of those
  overlays.
