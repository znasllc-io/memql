---
title: Azure keep-it -- sanctioned first bring-up
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Azure keep-it: sanctioned first bring-up

**Audience:** operators standing up the ZNAS keep-it cluster on Azure
(`rg-znas-memql`). **Issue:** memql#4204.

First bring-up is **Argo in `aks-znas-memql` reconciling
`deploy/k8s/overlays/cloud-entry`**. It is not `make deploy`. It is not
`aks-deploy.sh` (that script is gone). Those are digest rolls against an
already-installed cluster. This page is the sanctioned keep-it path.

Related: [front-door.md](front-door.md) ·
[reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md) ·
[deploy-bundle-runbook.md](deploy-bundle-runbook.md) ·
[environment-parity.md](environment-parity.md)

## What this instance is

MemQL ships one installation shape. A second environment is a second
instance -- own AKS, own Argo, own domain -- not a staging/prod split
inside one cluster (epic memql#3943). `overlays/cloud` stays on
`cnpg-db/presets/top` and mesh 2; it is what
`deploy/argocd/apps/memql.yaml` points at. **Do not apply
`overlays/cloud` to `rg-znas-memql`.** Do not add a second Application
next to `deploy/argocd/apps/memql.yaml`.

`cloud-entry` is the same base and the same CNPG shape, with the
numbers a keep-it / client install actually wants:

| | `overlays/cloud` | `overlays/cloud-entry` |
|---|---|---|
| Database | `cnpg-db/presets/top` | `cnpg-db/presets/entry` (1 instance, 32+16 GiB) |
| Mesh replicas | 2 | 1 |
| Voice / LiveKit / MCP | running pins + fail-closed placeholders | replicas 0 (voice-off) |

Voice-off is first-class replicas 0 on `voice`, `voice-agent`,
`livekit`, `livekit-sip`, `mcp`, and `livekit-redis`. It is not
`NODE_IP=0.0.0.0` and not an all-zeros MCP digest.

## Domain and hosts

The committed default in the overlay is `memql.localhost`. No file
under `deploy/` names a real domain (memql#3593). An install patches
`MEMQL_DOMAIN=memql.znas.io` and the Ingress / certificate hosts
through the ArgoCD Application's `spec.source.kustomize.patches`.
`.localhost` is unroutable externally, so a cluster reconciled before
its domain is set fails visibly instead of half-working.

Apex `znas.io` is the existing company site. **Do not point it at
AKS.** The cluster domain is `memql.znas.io`:

| Role | Host | Notes |
|---|---|---|
| identity | `identity.memql.znas.io` | sign-in |
| api | `api.memql.znas.io` | gRPC + HTTP edge |
| sites (wildcard) | `*.memql.znas.io` | portal is a site on this rule |
| sites (apex) | `memql.znas.io` | same edge Service |
| mcp | `mcp.memql.znas.io` | exists in the generator; **leave it dark** |

The MCP host is part of the closed role set. Voice-off already sets
`mcp` replicas to 0. Do not publish it, do not send clients there.

## DNS and certificates

Waiting at GoDaddy, then off nip.io:

| Name | Type | Value |
|---|---|---|
| `memql.znas.io` | A | `4.255.64.83` |
| `*.memql.znas.io` | A | `4.255.64.83` |

The front-door certificate (`memql-front-door-tls`) requests two SANs:
`*.<domain>` and the apex. A TLS wildcard matches exactly one label, so
the apex has to be named. The issuer is `letsencrypt-prod` and **must
resolve DNS-01** -- a wildcard SAN cannot be issued over HTTP-01. An
issuer configured for HTTP-01 alone does not fail at apply time; the
Certificate sits Pending and the Ingresses serve the controller's
default cert.

## Argo host-patch

`cmd/frontdoorhosts` writes `overlays/cloud-entry/front-door.generated.yaml`
with the committed default. Install-time patches repoint every host
the generator emitted, plus the `memql-domain` ConfigMap:

- Certificate `memql-front-door-tls` `spec.dnsNames` → `*.memql.znas.io`, `memql.znas.io`
- Ingress `api-front-door` and `api-front-door-grpc` → `api.memql.znas.io`
- Ingress `identity-front-door` → `identity.memql.znas.io`
- Ingress `mcp-front-door` → `mcp.memql.znas.io` (dark; still patch the name so it cannot drift)
- Ingress `edge-front-door` → `*.memql.znas.io` and `memql.znas.io`
- ConfigMap `memql-domain` key `MEMQL_DOMAIN` → `memql.znas.io`

The same derivation feeds both sides: `cmd/frontdoorhosts` composes
the Ingress hosts, and `component/envregistry/domain.go` composes the
issuer, CORS origins and redirect URIs, both through
`component/frontdoor`. Two copies of that rule would be two copies
that can disagree.

`make frontdoor-hosts` (`go run ./cmd/frontdoorhosts`) writes both
instance overlays. `make frontdoor-paths` on this cut still writes
local + cloud only; fill the cloud-entry path block with
`go run ./cmd/frontdoorpaths --write deploy/k8s/overlays/cloud-entry/front-door.generated.yaml`
until that Makefile line lands. Do not copy
`overlays/cloud/front-door.generated.yaml` into `cloud-entry` --
kustomize refuses a sibling path (load restrictor).

## First bring-up

1. Cluster `aks-znas-memql` in `rg-znas-memql` (AKS Free, entry node
   pools). Backup RG is `rg-znas-memql-backup`. `rg-memql-staging`
   ACR / Key Vault stays.
2. Point that cluster's Argo at `znasllc-io/memql`, path
   `deploy/k8s/overlays/cloud-entry`, and the host-patches above.
   Do not retarget `deploy/argocd/apps/memql.yaml`.
3. Seed secrets the way a cloud install already does (External Secrets
   + Key Vault). Seed `memql-domain` with `MEMQL_DOMAIN=memql.znas.io`.
4. Wait for DNS, then wait for the certificate to become Ready.
5. Verify:

```bash
scripts/install/verify-frontdoor.sh --hosts api.memql.znas.io,identity.memql.znas.io
```

That script establishes dns / tls / grpc / precedence for `api.` and
`identity.` only. `mcp.` is inconclusive by design (it does not serve
`/healthz` on the front-door port). Portal and the apex are wildcard
sites -- there is no exact-host precedence to prove for them.

## Later rolls (not this bring-up)

Cockpit digest rolls stay owner-gated and still default to
`overlayPath=deploy/k8s/overlays/cloud`. This instance must pass
`cloud-entry` explicitly:

```bash
memql-cockpit deploy --ref main --role owner --actor "$USER" \
  --input '{"provider":"azure","overlayPath":"deploy/k8s/overlays/cloud-entry"}'
```

Live `--apply` stays owner-gated. See
[deploy-bundle-runbook.md](deploy-bundle-runbook.md). No
`overlays/cloud`. No `top`. No extra monitoring addon.

## What is not this page

- The local k3d + Argo cluster --
  [reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md)
- The five host rules in general -- [front-door.md](front-door.md)
- Twice-daily entry deploys and the client-repo template -- later
  tickets, not this bring-up
