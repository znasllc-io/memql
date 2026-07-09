---
title: Environment parity — one topology everywhere
audience: public
status: stable
area: operate
sinceVersion: 0.10.0
owner: znas
---

# Environment parity — one topology everywhere

**The standard, non-negotiable:** local, staging, and production run the
**same topology**, the **same deployment process**, and the **same connection
model**. Whether memQL is running on a laptop in k3d or on a server in the
cloud, the *architecture* is identical. The **only** things that change between
environments are **configuration values** and the **hardware resources** the
workloads run on — never the shape of the system.

If you find yourself adding an environment-specific path — a special command
that only makes sense locally, a `if env == "local"` branch, a port-forward
that stands in for real infrastructure — **stop**. That is a deviation from
this standard and it is the thing this document exists to prevent.

## What is FIXED across every environment (the topology)

These are the same everywhere. Changing them for one environment is a bug.

- **Node topology** — the product-agnostic engine mesh (identity / bff /
  cognition / agent / planner / workbench / voice / mcp) + Postgres. Engine-only
  clusters opt into the `bff` via `deploy/k8s/components/engine-bff`; products
  add their own `bff-<product>` + DSL bundle. Same node graph everywhere.
- **The deployment process** — GitOps: one `deploy/k8s/base` composed by one
  per-env overlay, reconciled by ArgoCD. `make up` locally applies the same
  manifests ArgoCD applies in the cloud. A release is `{engine version, bundle
  digest, client digest}` pinned in the overlay. There is no second, "local"
  way to deploy.
- **The client connection model** — every client (the Cockpit, SDKs, a product
  SPA) reaches the mesh through an **ingress → TLS-terminate → gRPC → bff**
  front door at `https://cockpit.<domain>` (or the product's host). The client
  dials `https://<host>` and the SDK does gRPC-over-TLS on 443 — the **same dial
  path** in every environment. There is no local-only endpoint, no port-forward
  in the connection path.

## What is ALLOWED to vary (configuration + hardware)

These are per-environment **values**, not architecture. They live in the
overlay, the cluster registry, or the environment — never in application logic.

| Knob | local | staging / prod |
|---|---|---|
| Image references | `:local` (built + imported to k3d) | pinned `@sha256` digests |
| Replicas / resources | 1×, small | N×, sized per env |
| Domain | `local.znas.io` | `staging.<domain>` / prod domain |
| DNS | `/etc/hosts` wildcard → 127.0.0.1 | real DNS → ingress IP |
| TLS cert source | mkcert `*.local.znas.io` (`local-znas-tls`) | cert-manager / Let's Encrypt |
| Ingress controller | k3s-bundled **traefik** (`serversscheme: h2c`) | **nginx** (`backend-protocol: GRPC`) |
| Secrets source | `make secrets` from the genesis envelope | External Secrets + Key Vault |

Everything in this table is a config cell. The topology above it is untouched.
The ingress-controller row is the one place the *manifest* genuinely differs
(k3d ships traefik, the cloud runs nginx) — that divergence is annotation-level,
sits entirely below the connection abstraction, and is the same divergence
accepted for every host (`identity.local.znas.io`, `cockpit.local.znas.io`).

## The connection model in practice

Local and cloud front the `bff` gRPC edge identically — a hostname-routed
ingress terminating TLS on 443 and forwarding HTTP/2 to `svc/bff:50051`:

```
client ──https://cockpit.<domain>──▶ [ingress :443, TLS] ──h2 gRPC──▶ svc/bff:50051
   local:  cockpit.local.znas.io      traefik + mkcert wildcard        (h2c)
   cloud:  cockpit.<env-domain>        nginx + cert-manager cert        (GRPC)
```

So the Cockpit connects the same way everywhere — it is just another cluster
entry in `~/.memql/clusters.yaml` (`Domain` → `Endpoint = https://cockpit.<domain>`):

```bash
memql-cockpit --cluster local      # https://cockpit.local.znas.io
memql-cockpit --cluster staging    # https://cockpit.staging.<domain>
```

Locally this needs the two config knobs the cloud gets from its provider: the
`*.local.znas.io` wildcard in `/etc/hosts` (→ 127.0.0.1) and a trusted mkcert
cert (`mkcert -install`) — both already required to reach `identity.local.znas.io`.
A raw `kubectl port-forward svc/bff 50051` (`make forward`) remains available for
low-level debugging, but it is **not** part of the connection path.

## Anti-patterns (reject these in review)

- **Port-forward as architecture.** A port-forward is a debugging tool, not a
  connection model. If "how a client connects" differs between local and cloud,
  that is a deviation. (This is why `run-local` was removed — one `run` connects
  everywhere via the cluster registry.)
- **Environment-specific commands or code branches.** No `make run-local`, no
  `if env == "local"` in application logic. The environment is *data* (the
  overlay + the registry), read the same way everywhere.
- **A second way to deploy.** Local uses the same base+overlay+ArgoCD path as
  the cloud. `make up` is that path on k3d, not a bespoke script.
- **"It's just local, we'll do it differently."** The moment local diverges in
  shape, local stops proving anything about staging/prod. Parity is what makes
  the local cluster a real rehearsal.

## Enforcement mechanism

The base + overlay + component split is how the standard is enforced mechanically:

- **`deploy/k8s/base`** — the topology. Product-agnostic, no per-env values.
- **`deploy/k8s/overlays/<env>`** — the per-env config: image digests/tags,
  replicas, the domain/cert/DNS wiring, the ingress-controller annotations.
- **`deploy/k8s/components/*`** — opt-in capabilities (`engine-bff`,
  `dsl-bundle`) an overlay composes; they add topology without an env branch.

When you add something new, ask: *is this the shape of the system, or a value?*
Shape goes in base/components (everywhere); values go in the overlay (per env).
If a change only makes sense in one environment, it is almost certainly in the
wrong layer.
