---
title: Self-hosted LiveKit (staging/prod) — provisioning runbook
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Self-hosted LiveKit (staging/prod) — provisioning runbook

LiveKit powers CoPresent's realtime voice/video. We **self-host** the
open-source `livekit-server` (Apache-2.0) in the AKS cluster — there is **no
LiveKit Cloud and no third-party credential**. The "API key/secret" is a
self-chosen shared-secret pair: `livekit-server` validates tokens signed with
it, and the BFF mints room tokens with the same pair.

Tracking issue: znasllc-io/memql#1043.

## What's in GitOps (durable, no manual steps)

- `deploy/k8s/base/livekit.yaml` — `livekit-server` Deployment + ConfigMap, a
  ClusterIP **signaling** Service (`:7880`), a **LoadBalancer** RTC Service
  (UDP `7882` + TCP `7881`, mixed-protocol, `externalTrafficPolicy: Local`,
  `use_external_ip`), and an Ingress for `wss://livekit.<env>.copresent.ai`.
- `deploy/k8s/base/externalsecret-livekit.yaml` — ESO reconciles the
  `livekit-secrets` k8s Secret from Key Vault (declarative; mirrors
  `memql-secrets`).
- `bff.yaml` / `voice.yaml` — `POLYPHON_LIVEKIT_URL` + `_PUBLIC_URL` (non-secret)
  and `_API_KEY` / `_API_SECRET` (from `livekit-secrets`).
- The staging overlay digest-pins `livekit/livekit-server`.

ArgoCD (auto-sync from `main`) applies all of the above. The BFF is a
blue/green Rollout, so a manifest change rolls a **preview** color that must be
**promoted** (`kubectl argo rollouts promote bff -n memql`) after its gate.

## The two steps GitOps can't do

### 1. Seed the Key Vault secrets (scripted, idempotent)

```
make livekit-provision                 # staging (reuses an existing pair)
make livekit-provision DRY_RUN=1       # plan only
make livekit-provision ARGS=--rotate   # generate a fresh pair, then roll pods
make livekit-provision ENV=production
```

This seeds three secrets into `kv-memql-<env>` —
`livekit-keys` (`<apiKey>: <secret>`), `polyphon-livekit-api-key`,
`polyphon-livekit-api-secret` — which the ExternalSecret syncs into
`livekit-secrets`. Re-runs reuse the existing pair (convergent no-op). Requires
`az login`.

Verify the sync:
```
kubectl get externalsecret livekit-secrets -n memql   # READY=True
```

### 2. DNS A record (registrar-side, manual)

Add an A record for the **signaling** host pointing at the ingress-nginx
LoadBalancer IP:

```
livekit.staging.copresent.ai  ->  <ingress-nginx EXTERNAL-IP>
```

(`kubectl get svc ingress-nginx-controller -n ingress-nginx` for the IP.) The
cert issues automatically via cert-manager once the record resolves.

The **media** plane needs no DNS — it rides the `livekit-rtc` LoadBalancer's own
public IP, which LiveKit advertises directly via ICE:
```
kubectl get svc livekit-rtc -n memql -o wide   # EXTERNAL-IP = media endpoint
```

**Important — the advertised media IP is pinned per-env, not auto-discovered.**
On AKS, `use_external_ip` (STUN) discovers the node's *egress* IP, which is NOT
the inbound LoadBalancer IP browsers must reach, so ICE fails. The base config
sets `use_external_ip: false`; each overlay sets a `NODE_IP` env on the livekit
container = its `livekit-rtc` LoadBalancer EXTERNAL-IP
(`deploy/k8s/overlays/<env>/kustomization.yaml`).

So after first standing up LiveKit in a new env (or if the `livekit-rtc`
Service is recreated and Azure assigns a new IP): read the EXTERNAL-IP above and
set `NODE_IP` in the overlay to match, then let it roll. For a fully stable
value, reserve a static Azure public IP for the service (`loadBalancerIP` +
`azure-load-balancer-resource-group` annotation) and pin `NODE_IP` to it.

If browsers behind restrictive NATs still can't establish media (UDP blocked),
TCP 7881 is already exposed as fallback; add a TURN server for the strictest
networks.

## Verify end-to-end

- BFF logs `polyphon: local room provider enabled (LiveKit token generation)`.
- Mic toggle in CoPresent mints a room token (no "provider not configured").
- Browser joins the LiveKit room; the General Assistant participates once the
  voice-agent is wired with `LIVEKIT_URL` / `_API_KEY` / `_API_SECRET` +
  `MEMQL_DEEPGRAM_API_KEY`.
