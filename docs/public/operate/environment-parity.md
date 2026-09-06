---
title: Environment parity — one topology everywhere
audience: public
status: stable
area: operate
sinceVersion: 0.10.0
owner: znas
---

# Environment parity — one topology everywhere

**The standard, non-negotiable:** every MemQL installation runs the **same
topology**, the **same deployment process**, and the **same connection model**.
Whether MemQL is running on a laptop in k3d or on a server in the cloud, the
*architecture* is identical. The **only** things that change are
**configuration values** and the **hardware resources** the workloads run on —
never the shape of the system.

If you find yourself adding a target-specific path — a special command that
only makes sense locally, an `if env == "local"` branch, a port-forward that
stands in for real infrastructure — **stop**. That is a deviation from this
standard and it is the thing this document exists to prevent.

> MemQL ships **one installation shape** (epic memql#3943). There is no
> staging-versus-production dimension inside the product: an operator who wants
> a second environment installs a second instance, with its own domain, its own
> ArgoCD and its own database. So "parity" here is between the LOCAL cluster and
> a CLOUD cluster — two installs of the same system, not two environments of one.

## What is FIXED everywhere (the topology)

These are the same everywhere. Changing them for one install is a bug.

- **Node topology** — the product-agnostic engine mesh (identity / bff / agent /
  planner / workbench / mcp / edge) + Postgres. Engine-only clusters opt into
  the `bff` via `deploy/k8s/components/engine-bff`; products add their own
  `bff-<product>` + DSL bundle. Same node graph everywhere.
- **The deployment process** — GitOps: one `deploy/k8s/base` composed by one
  overlay (`local`, `cloud` or `cloud-entry`), reconciled by ArgoCD. `make up` locally applies the same
  manifests ArgoCD applies in the cloud. A release is `{engine version, bundle
  digest, client digest}` pinned in the overlay. There is no second, "local"
  way to deploy.
- **The client connection model** — every client (the Cockpit, SDKs, a product
  SPA) reaches the mesh through an **ingress → TLS-terminate → gRPC → bff**
  front door at `https://api.<domain>` (or the product's host). The client
  dials `https://<host>` and the SDK does gRPC-over-TLS on 443 — the **same dial
  path** everywhere. There is no local-only endpoint, no port-forward in the
  connection path.

## What is ALLOWED to vary (configuration + hardware)

These are **values**, not architecture. They live in the overlay, the cluster
registry, or the environment — never in application logic.

| Knob | local | cloud |
|---|---|---|
| Image references | `:local` (built + imported to k3d) | pinned `@sha256` digests |
| Replicas / resources | 1×, small | N×, sized for the cluster |
| Domain | `memql.localhost` | the operator's own domain |
| DNS | `/etc/hosts` wildcard → 127.0.0.1 | real DNS → ingress IP |
| TLS cert source | mkcert `*.memql.localhost` (`memql-front-door-tls`) | cert-manager / Let's Encrypt |
| Ingress controller | k3s-bundled **traefik** (`serversscheme: h2c`) | **nginx** (`backend-protocol: GRPC`) |
| Secrets source | `make secrets` writes `memql-secrets` directly | External Secrets + Key Vault |
| Credential source for a vendor | Anthropic API key | Anthropic workload identity federation |

Everything in this table is a config cell. The topology above it is untouched.
The ingress-controller row is the one place the *manifest* genuinely differs
(k3d ships traefik, the cloud runs nginx) — that divergence is annotation-level,
sits entirely below the connection abstraction, and is the same divergence
accepted for every host (`identity.memql.localhost`, `api.memql.localhost`).

The **credential-source** row is the newest one and passes the same test the
others do, so it is worth saying exactly how (memql#4333). The manifests are
IDENTICAL: `deploy/k8s/base` gives every engine Deployment the same
`memql-engine` ServiceAccount, the same projected `anthropic-identity` token
volume with audience `https://api.anthropic.com`, and the same
`MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE`. What varies is four ids in one
ConfigMap: the cloud overlay fills them, the local overlay leaves them empty,
and empty means "not federating", so `make up` keeps booting on the API key
with the very YAML the cloud runs. The engine branches on the VALUES, never on
a target — there is no environment for it to read, and
`TestNoEnvironmentBranchingInEngineCode` still holds.

The reason local cannot federate is a fact about k3d rather than a choice:
its OIDC issuer is `https://kubernetes.default.svc.cluster.local`, whose JWKS
sits on a node IP Anthropic cannot reach. The alternative (`inline` JWKS mode)
would mean registering an issuer per developer cluster and re-pasting it after
every `make up-refresh` — not a reproducible path. Runbook:
[auth/anthropic-federation.md](auth/anthropic-federation.md).

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
memql-cockpit --cluster acme       # https://api.acme.example
```

Locally this needs the two config knobs the cloud gets from its provider: the
`*.memql.localhost` wildcard in `/etc/hosts` (→ 127.0.0.1) and a trusted mkcert
cert (`mkcert -install`) — both already required to reach `identity.memql.localhost`.
A raw `kubectl port-forward svc/bff 50051` remains available for low-level
debugging, but it is **not** part of the connection path. There is deliberately
no make target wrapping it: a wrapper is what turns a debugging escape hatch
into a documented way to connect.

## Anti-patterns (reject these in review)

- **Port-forward as architecture.** A port-forward is a debugging tool, not a
  connection model. If "how a client connects" differs between local and cloud,
  that is a deviation. (This is why `run-local` was removed — one `run` connects
  everywhere via the cluster registry.)
- **Target-specific commands or code branches.** No `run-local` make target, no
  `if env == "local"` in application logic. Configuration is *data* (the
  overlay + the registry), read the same way everywhere.

  This half of the rule is **enforced by a test** rather than by review.
  `TestNoEnvironmentBranchingInEngineCode` (`environment_branching_test.go`)
  fails the build when engine code so much as **names** `prod`, `production` or
  `staging` — as a comparison, a `switch` case, or a map key, because those are
  the same thing with different punctuation and a gate that looked only for
  `==` would miss the map. Its exemption map is **EMPTY** since epic
  memql#3943: nothing in the tree is environment-aware by design, because there
  is no environment for anything to be aware of.

  `development` and `local` are deliberately **outside** the gate. They
  distinguish deploy *targets* — a k3d cluster on a laptop versus AKS, which
  really are different infrastructure reached by different machinery — and that
  distinction carries its own field, `provider` (`docker-local` | `azure`).
  Two live sites depend on it: `deploycontrol` refusing the retired
  `docker-local` provider, and the deploy automation's `switch provider`
  choosing between a k3d image import and a digest pin + ArgoCD sync.
- **A second way to deploy.** Local uses the same base+overlay+ArgoCD path as
  the cloud. `make up` is that path on k3d, not a bespoke script.
- **"It's just local, we'll do it differently."** The moment local diverges in
  shape, local stops proving anything about the cloud. Parity is what makes the
  local cluster a real rehearsal.

## Enforcement mechanism

The base + overlay + component split is how the standard is enforced mechanically:

- **`deploy/k8s/base`** — the topology. Product-agnostic, no per-install values.
- **`deploy/k8s/overlays/{local,cloud,cloud-entry}`** — the config: image digests/tags,
  replicas, the domain/cert/DNS wiring, the ingress-controller annotations.
- **`deploy/k8s/components/*`** — opt-in capabilities (`engine-bff`,
  `dsl-bundle`) an overlay composes; they add topology without a branch.

There are three overlays -- `local`, `cloud` (the top-sized instance, what
`deploy/argocd/apps/memql.yaml` points at) and `cloud-entry` (the entry /
client instance, memql#4203) -- and the two cloud ones are two INSTANCE
shapes, not two environments (epic memql#3943). The gates keep all three
honest against one derivation rather than against each other:
`render_cloud_test.go` and `render_cloud_entry_test.go` render the two cloud
overlays and assert namespace containment, stated replica counts and (for
`cloud`) the ArgoCD wiring, `frontdoor_hosts_test.go` computes the host set
from `component/frontdoor` and asserts both generated front doors serve
exactly it, and the local overlay's `render_frontdoor_test.go` asserts the
same of the hand-authored one.

When you add something new, ask: *is this the shape of the system, or a value?*
Shape goes in base/components (everywhere); values go in the overlay. If a
change only makes sense on one deploy target, it is almost certainly in the
wrong layer.
