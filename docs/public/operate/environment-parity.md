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
  front door at `https://api.<domain>` (or the product's host). The client
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
| Domain | `memql.localhost` | `staging.<domain>` / prod domain |
| DNS | `/etc/hosts` wildcard → 127.0.0.1 | real DNS → ingress IP |
| TLS cert source | mkcert `*.memql.localhost` (`memql-front-door-tls`) | cert-manager / Let's Encrypt |
| Ingress controller | k3s-bundled **traefik** (`serversscheme: h2c`) | **nginx** (`backend-protocol: GRPC`) |
| Secrets source | `make secrets` from the genesis envelope | External Secrets + Key Vault |

Everything in this table is a config cell. The topology above it is untouched.
The ingress-controller row is the one place the *manifest* genuinely differs
(k3d ships traefik, the cloud runs nginx) — that divergence is annotation-level,
sits entirely below the connection abstraction, and is the same divergence
accepted for every host (`identity.memql.localhost`, `api.memql.localhost`).

## The connection model in practice

Local and cloud front the `bff` gRPC edge identically — a hostname-routed
ingress terminating TLS on 443 and forwarding HTTP/2 to `svc/bff:50051`:

```
client ──https://api.<domain>──▶ [ingress :443, TLS] ──h2 gRPC──▶ svc/bff:50051
   local:  api.memql.localhost          traefik + mkcert wildcard    (h2c)
   cloud:  api.<env-domain>             nginx + cert-manager cert    (GRPC)
```

So the Cockpit connects the same way everywhere — it is just another cluster
entry in `~/.memql/clusters.yaml` (`Domain` → `Endpoint = https://api.<domain>`):

```bash
memql-cockpit --cluster local      # https://api.memql.localhost
memql-cockpit --cluster staging    # https://api.staging.<domain>
```

Locally this needs the two config knobs the cloud gets from its provider: the
`*.memql.localhost` wildcard in `/etc/hosts` (→ 127.0.0.1) and a trusted mkcert
cert (`mkcert -install`) — both already required to reach `identity.memql.localhost`.
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

  Since memql#3766 this half of the rule is **enforced by a test** rather than
  by review, for the environments where it bites hardest.
  `TestNoEnvironmentBranchingInEngineCode` (`environment_branching_test.go`)
  fails the build when engine code so much as **names** `prod`, `production` or
  `staging` — as a comparison, a `switch` case, or a map key, because those are
  the same thing with different punctuation and a gate that looked only for
  `==` would miss the map. `component/deploycontrol` is exempted, with its
  reason recorded in the file: translating between the deployment record's enum
  and the console's env is that component's entire subject.

  `development` and `local` are deliberately **outside** the gate. They
  distinguish deploy *targets* — a k3d cluster on a laptop versus AKS, which
  really are different infrastructure reached by different machinery — and two
  live sites depend on that distinction (`deploycontrol` refusing the retired
  `docker-local` provider; `app/cluster.go` labelling a development cluster's
  provider). Neither can tell staging from production, which is the boundary
  that matters: those two run the **same images from the same base** in two
  namespaces of one cluster, so any code able to tell them apart is the second
  way to deploy this standard rejects.
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

Staging and production are the sharpest instance of that split (memql#3766):
they are **two namespaces in one cluster**, reconciled by one ArgoCD from one
base through `overlays/prod` and `overlays/staging`. Diff those two
kustomizations and the only differences are the namespace, two ConfigMap
entries, nine replica counts and the image digests —
`TestBothEnvironmentsRenderTheSameSystem` keeps it that way by rendering both
and comparing the resource inventories. Two overlay trees maintained in
parallel were the drift risk this standard exists to prevent; one base with two
value sets is the standard's own prescription.

One installation, then, and not two. The standard is satisfied MORE completely
by one base with two value sets than by the two parallel overlay trees it
replaces -- worth stating outright, because "staging" and "production" are words
that ordinarily imply separate installations and the inference is wrong here.
The operator model, including what a promotion moves between them and what it
deliberately does not, is [environments.md](environments.md).

The boundary between the two environments' **data** follows the same rule: it
is a configuration value, not a code path. Each namespace's pods get a
different Postgres schema search path (`memql_prod, public` /
`memql_staging, public`) applied to every connection the driver opens, so no
query has to remember which environment it is in
(`component/database/search_path.go`).

When you add something new, ask: *is this the shape of the system, or a value?*
Shape goes in base/components (everywhere); values go in the overlay (per env).
If a change only makes sense in one environment, it is almost certainly in the
wrong layer.
