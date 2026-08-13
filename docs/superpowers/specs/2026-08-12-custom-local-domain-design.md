# Custom local domain: one `MEMQL_DOMAIN`, derived everywhere

**Issue:** memql#3593
**Status:** design approved, not implemented
**Supersedes nothing. Implements D5 of** [`2026-08-08-local-cluster-install-wizard-design.md`](2026-08-08-local-cluster-install-wizard-design.md).

---

## 1. Problem

The Add-a-cluster wizard shows a **Domain** field and lets an operator edit it.
Until memql#3590, whatever they typed reached `seedBootstrap` — which bootstrapped
identity for that domain — while `hostsBlock`, `localCA` and `frontDoor` each used
their own `local.znas.io` defaults. The cluster's issuer named one domain; its hosts
block, certificate and probe named another.

memql#3590 fixed the **installer** half: the typed domain now reaches all four steps.
The **cluster** half is untouched, and it is the half that decides what actually gets
served. ArgoCD renders `deploy/k8s/overlays/local` from the git release tag the
installer checked out (`DEFAULT_STACK_TAG`), so nothing passed on a command line
reaches those manifests. A custom domain therefore resolves — the hosts block points
it at 127.0.0.1 — and then answers as the wrong site, because traefik has no Ingress
rule for it and serves its default certificate.

So `stackPin.ts` currently refuses any domain but `local.znas.io`
(`installDomainProblem`). That refusal is honest, and it is not a design.

### 1.1 Every place the domain is pinned today

| File | What it pins |
|---|---|
| `overlays/local/cockpit-front-door.yaml` | Ingress `host:` + `spec.tls.hosts` → `cockpit.local.znas.io` |
| `overlays/local/front-door.yaml` | the same for `identity.local.znas.io` |
| `overlays/local/patches/identity-local-config.yaml` | `MEMQL_IDENTITY_BASE_URL`, expected issuer, `MEMQL_IDENTITY_BOOTSTRAP_DOMAIN`, `MEMQL_DISCOVERY_GRPC_ENDPOINT`, CORS origins, **OAuth registered-client redirect URIs** |
| `overlays/local/kustomization.yaml` | `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`, repeated for **eight** node Deployments |
| `scripts/lib/localtls.sh` | the certificate SANs (`*.local.znas.io,local.znas.io`) and the secret name `local-znas-tls` |

memql#3593 lists five sites. The two it understates are inside the identity patch and
are the ones that fail most confusingly: the **OAuth redirect URIs** and the
**discovery endpoint**. Getting the Ingress right and missing those produces exactly
the memql#3315 failure already recorded in that file — sign-in dying in CORS with an
empty identity log.

### 1.2 Why this is worth doing

Two reasons, and the second is the stronger one.

1. **Parity.** `docs/public/operate/environment-parity.md` names the domain, the DNS
   source and the TLS source as the things *allowed* to vary per environment.
   Treating the domain as the shape of the system rather than as a value is that rule
   pointed the wrong way.
2. **The engine is supposed to be product-neutral.** `znas.io` is a company's domain
   hardcoded in a public repo whose stated rule is that the engine carries no product.
   Someone cloning memQL to learn it gets a cluster named under a stranger's domain,
   and `product_neutrality_test.go` is a banned-names list that would never notice.
   Parameterising the domain is de-branding the engine; the local default becomes a
   default rather than a constraint.

---

## 2. Decisions

**D1 — Reach: the installing machine only.** No LAN, no public internet. The domain
resolves to 127.0.0.1 and the mkcert CA is trusted on that machine. Everything below
follows from this; LAN and public reach are explicitly out of scope (§7).

**D2 — Default domain `memql.localhost`, BYO as the advanced path.** This is D5 of the
install-wizard design, implemented as written. `local.znas.io` keeps working *through
the BYO path*, which that design required. The `nip.io` / `sslip.io` alternative stays
rejected on its original grounds: consumer routers' DNS-rebind protection blocks
public names resolving to 127.0.0.1, so it fails intermittently and confusingly.

The `.localhost` RP-id risk is already half-measured. `scripts/spikes/webauthn-rpid/`
(memql#3405) shows Chrome 151 **accepts** `memql.localhost` as a WebAuthn RP id for
origin `https://identity.memql.localhost`, with a positive control proving bare
`localhost` is refused as a public suffix. One passkey therefore covers every host
under the domain. Leg 2 of that spike — iOS/Android over hybrid transport, and
Safari — remains unmeasured and `needs-human`.

**D3 — One default everywhere.** `make up` moves to `memql.localhost` too. Two local
domains in one repo is how the drift that produced memql#3590 happened; and a default
the team never exercises is a default that rots.

**D4 — Mechanism: one `MEMQL_DOMAIN`, derived in Go.** Rejected alternatives:

- *All of it as ArgoCD `kustomize.patches`* (the issue's literal proposal). No Go
  change, follows the memql#3572 image-override precedent — but it means bash
  heredocs emitting ~11 patches that restate the container and env layout of the base
  manifests. A second copy that goes stale silently the day a node type is added.
- *ConfigMap with all 14 values derived in bash.* Same delivery, no Go change, fastest
  to ship — but the knowledge of which redirect URIs and CORS origins exist would live
  in a bring-up script rather than beside the server that validates them.

Derivation in Go is the only option that removes the drift class rather than
relocating it. The memql#3315 failure cannot recur if there is one input and the
derivation is code with tests.

**D5 — The hosts file is written only when it is needed.** The `hostsBlock` step
probes first. If every front-door hostname already resolves to 127.0.0.1, it writes
nothing and takes no elevation. One rule, both modes; the sudo prompt appears only
when it does something.

**D6 — The domain is chosen once.** Changing it invalidates every passkey (the RP id
is domain-derived), every live session and token (the issuer changes), the certificate
SANs and the hosts block. `k3d.up --domain` refuses a domain that differs from the one
already in the cluster and points at `make up-refresh`.

---

## 3. Architecture

One input, one derivation, three consumers.

```
                     MEMQL_DOMAIN = "memql.localhost"
                     (default; --domain overrides)
                                │
        ┌───────────────────────┼────────────────────────┐
        │                       │                        │
   host resolution        TLS material            cluster config
   (installer)            (installer)             (GitOps)
        │                       │                        │
  /etc/hosts block        mkcert issues            ConfigMap memql-domain
  cockpit.<d>             *.<d> + <d>              (ONE key: MEMQL_DOMAIN)
  identity.<d>            → secret                        │
  <d>                     memql-front-door-tls      envFrom on all 9 nodes
        │                       │                         │
        └──── skipped when ─────┘                   Go derives, per node:
             already resolves                        identity: BASE_URL, issuer,
             to 127.0.0.1                            bootstrap domain, discovery
                                                     endpoint, CORS, clients
                                                     others: expected issuer
                                                              │
                                              ArgoCD Application kustomize.patches
                                              (2 patches, only when overridden)
```

Everything the **engine** reads is env, so it travels as one ConfigMap key and is
derived in Go: no generated YAML, and a tenth node type needs no change anywhere. The
only two values that are genuinely Kubernetes API objects rather than process config
are the Ingress `host:` and `spec.tls.hosts`; those ride two
`spec.source.kustomize.patches` on the ArgoCD Application — the seam memql#3572 already
used for images.

After this change, **no file under `deploy/` contains a domain** except the two
Ingresses' committed default.

### 3.1 Derivation rules

Each applies **only when that specific env var is unset**, so staging, prod and any
explicit override are untouched.

| Value | Derived as |
|---|---|
| `MEMQL_IDENTITY_BASE_URL` | `https://identity.<domain>` |
| `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` | same as BASE_URL (all nine nodes) |
| `MEMQL_IDENTITY_BOOTSTRAP_DOMAIN` | `<domain>` |
| `MEMQL_DISCOVERY_GRPC_ENDPOINT` | `cockpit.<domain>:443` |
| `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS` | `https://cockpit.<domain>`, `https://app.<domain>` |
| `MEMQL_IDENTITY_REGISTERED_CLIENTS` | `cockpit` (loopback, domain-free), `portal` → `https://cockpit.<domain>/portal/auth/callback`, `app` → `https://app.<domain>/auth/callback` |

### 3.2 The one wart

Today's local overlay also admits `http://localhost:8080` and `http://localhost:3000`
— the vite dev servers for the portal and the product SPA. Those are not
domain-shaped, and folding them into the derived default would mean a staging operator
who forgot to set CORS silently gets localhost origins allowed. They become two new
**domain-free** env vars, set only in the local overlay and appended after the
derived-or-explicit sets:

- `MEMQL_IDENTITY_CORS_EXTRA_ORIGINS`
- `MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS`

Two knobs that exist purely so the derivation can stay production-honest.

---

## 4. Components

### 4.1 Engine (Go)

**NEW `component/genesis/domain.go`** — `ApplyDomainDerivations(logger)`. Reads
`MEMQL_DOMAIN`; fills each derived var only when unset; logs at INFO which ones it
filled. Called from `main.go` immediately after `genesis.ApplyLegacyEnvAliases`
(main.go:77) and from `subcommand_env.go:43`, so `memql env` reports the truth.

This is the seam that makes the whole approach cheap. Because it normalizes the
environment *before* anything reads it, all four existing readers need **no edit**:

- `component/identity/verifier/config.go:150`
- `component/portal/config.go:199`
- `app/transport_mcp.go:78`
- `app/cluster.go:885`

and a fifth added later is correct by default. The repo already has this seam —
`ApplyLegacyEnvAliases` and `ApplyLocalOverride` both normalize env at boot — so this
extends an existing convention rather than inventing one.

**`component/identity/config.go`** — two changes:

1. Narrow `isSingleProcessHost` (config.go:989) to the four literal loopback names
   (`localhost`, `127.0.0.1`, `::1`, `0.0.0.0`) and **drop the `.localhost` suffix
   rule**. See §4.5.
2. Add `MEMQL_IDENTITY_CORS_EXTRA_ORIGINS` and
   `MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS`, appended after the derived-or-explicit
   sets.

### 4.2 Deploy (git — and after this, domain-free)

- `overlays/local/patches/identity-local-config.yaml` — strip all six domain-bearing
  env vars; keep `MEMORY_NODES_DATABASE_MIGRATE_ON_START`; add the two extras from
  §3.2 carrying the vite dev-server values.
- `overlays/local/kustomization.yaml` — delete the eight inline expected-issuer
  patches. Replace with two name-regex-targeted patch files:
  - a strategic merge that `$patch: delete`s the base's
    `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` env entry (merge key `name`), because
    an explicit `env` entry beats `envFrom` in Kubernetes regardless of order;
  - a JSON 6902 `add` at `/spec/template/spec/containers/0/envFrom/-` for
    `configMapRef: {name: memql-domain}`. Two files because `envFrom` has no patch
    merge key — the same reason `bootstrap-secret-envfrom.yaml` gives for using 6902.

  Unlike that one, this ref is **not** `optional`: a node with no domain should refuse
  to boot rather than mint tokens for the wrong issuer.
- `front-door.yaml` / `cockpit-front-door.yaml` — hosts keep a **committed default of
  `memql.localhost`**, and the Application patch is emitted **only when the domain
  differs**. The common case then has zero generated YAML, and `kubectl apply -k`
  still works standalone.
- TLS secret `local-znas-tls` → `memql-front-door-tls` in both Ingresses.

### 4.3 Scripts

**One value, three spellings, stated once.** The capability param is `--domain`; its
default is resolved from the `MEMQL_LOCAL_DOMAIN` environment variable, falling back
to `memql.localhost` — which is the shape the capability-script contract requires
(`cap_param` has no env tier, so a script passes an env-resolved value as the
default). The Makefile surfaces it as `DOMAIN=`, e.g. `make up DOMAIN=lab.example.com`.

- `scripts/lib/localtls.sh` — SANs derive from the same resolved domain as
  `*.<d>,<d>`; secret-name constant renamed.
- **NEW `scripts/lib/resolve.sh`** — the getent/dig/host resolver ladder currently
  inside `verify-frontdoor.sh`, extracted so the hosts probe and the verifier share
  one copy. This is the `localtls.sh` lesson applied before it bites: two copies of a
  resolution rule is how memql#3384 happened. It honours a stub hook so tests can
  inject answers.
- `scripts/k3d/up.sh` — new `--domain` param; seeds the
  `memql-domain` ConfigMap; emits the two Ingress patches into
  `spec.source.kustomize.patches` only when overridden; reports `domain` in its result
  envelope; refuses a domain that differs from the cluster's existing one (D6).
- `scripts/k3d/seed-secrets.sh` — creates the ConfigMap; new TLS secret name.
- `scripts/install/hosts-entries.sh` — gains `--domain` (deriving the three hostnames)
  alongside `--hostnames`, and probes before writing (§5.1).
- `scripts/install/verify-frontdoor.sh`, `scripts/install/mkcert-setup.sh` — defaults
  follow the domain; mkcert additionally re-issues when SANs do not cover the request
  (§5.3).

### 4.4 VS Code extension

- `install/stackPin.ts` — `SUPPORTED_LOCAL_DOMAIN` becomes
  `DEFAULT_LOCAL_DOMAIN = "memql.localhost"`; `installDomainProblem` turns from an
  equality refusal into syntax validation: lowercase, two or more labels, no scheme,
  no port, no wildcard, not a bare IP.
- `state/addCluster.ts` — new default in `DEFAULT_INPUTS`. The field stays visible and
  editable, which it already is.
- `install/session.ts` — thread `--domain` into `clusterUp`, joining the four steps
  memql#3590 already threads it into.

### 4.5 The `isSingleProcessHost` narrowing

`component/identity/config.go:989` currently returns true for **any** host ending
`.localhost`, on the premise its comment states plainly: *"Nothing behind such a host
can be a second replica."* That gates the fail-fast guard added in memql#3400 after the
2026-06-16 auth outage (memql#1515), where two identity replicas minted different
ephemeral signing keys and roughly half of all token verifications failed with
`unknown kid`.

**Accurate impact.** Both guards live inside the `else` branch that runs only when
`MEMQL_IDENTITY_SIGNING_KEY_B64` is unset (config.go:752-800). `seed-secrets.sh` seeds
that key for every local cluster, so neither check is reached today and nothing is
broken right now.

**Why it still belongs in this change.** After the move, `*.memql.localhost` becomes a
hostname a **multi-replica k8s Service** answers on — `make scale N=2` puts two
identity pods behind traefik. The exemption's stated premise becomes false, and any
cluster that ends up without a seeded signing key (a hand-rolled overlay, a downstream
product cluster copying the local one) would start in ephemeral-key mode with no
complaint. That is precisely what memql#3400 was written to stop. The comment on that
function already names this trap for the `*.local.<domain>` case; the fix is to stop
making the same claim about `.localhost`.

Two callers, both in that one branch, so the change is contained. A genuinely
single-process dev binary at `foo.localhost` keeps its escape hatch:
`MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true`. Note `isLocalHost` delegates to
`isSingleProcessHost`, so the `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY` posture check moves
with it — also only reachable in that same branch.

---

## 5. Data flow

**Default install.** Domain `memql.localhost` → `hostsBlock` probes, finds nothing
resolving, writes the marked block for `cockpit.`, `identity.` and the apex →
`localCA` issues `*.memql.localhost` + apex → `clusterUp` runs
`k3d.up --domain=memql.localhost`, seeds the ConfigMap and TLS secret, emits **no**
Ingress patch because the domain equals the committed default → ArgoCD syncs the
unmodified overlay → every pod boots and `ApplyDomainDerivations` fills BASE_URL,
issuer, discovery endpoint, CORS and clients → `seedBootstrap` → `frontDoor` verifies
DNS, TLS and gRPC → `magicLink` / `enrolmentLink`.

**BYO install**, e.g. `lab.example.com` with a wildcard A record at 127.0.0.1 →
`hostsBlock` probes, all three hostnames already resolve, reports `skipped`, **no
elevation prompt at all** → mkcert issues `*.lab.example.com` → `clusterUp` emits the
two Ingress patches → ArgoCD renders `cockpit.lab.example.com` → identity derives
`https://identity.lab.example.com` → `frontDoor` runs the same three checks through
the same code path.

The two runs differ in exactly two observable ways: whether a hosts block was written,
and whether the Application carries two patches. Nothing branches on "am I local".

### 5.1 The hosts probe has three outcomes

| Probe result | Action |
|---|---|
| every hostname resolves to 127.0.0.1 and nothing else | write nothing, take no elevation, report `skipped` |
| no hostname resolves | write the marked block (elevation; `result.remedy` names the command) |
| mixed, or any hostname resolves to another address | **refuse, exit 3**, naming the hostname and what it answered |

Writing a block that shadows a record the operator may depend on is the wrong repair.
`verify-frontdoor.sh` already states the principle: "a hostname pointing at some other
address is a worse failure than one that does not resolve."

### 5.2 Changing the domain of an existing cluster is refused

`k3d.up --domain` compares against the `memql-domain` ConfigMap already in the
cluster. On a mismatch it exits 3 and names what would break — every passkey, every
live session and token, the certificate SANs, the hosts block. The remedy is
`make up-refresh`. Local data is disposable, so this costs nothing and prevents a
half-migrated cluster that fails at sign-in with an unrelated-looking error.

### 5.3 A stale certificate is repaired, not skipped

`mkcert-setup.sh` checks that an existing pair's SANs cover the requested hostnames
and re-issues when they do not. Today it would skip the existing pair and leave
traefik serving a certificate for the old name — the shape of memql#3384, where a
drifted path let seed-secrets skip and traefik fall back to its default cert while
`make up` still printed a green summary.

### 5.4 A missing ConfigMap is loud

The `envFrom` ref is not `optional`, so a cluster synced without seeding sits in
`CreateContainerConfigError` rather than booting with a stale issuer. Symptom and fix
(`make secrets`) go in the runbook.

### 5.5 Migration for existing dev clusters

`make up-refresh` is the blessed path. For anyone who will not: `make secrets`
re-seeds the ConfigMap and the renamed TLS secret; hosts entries need re-adding for
the new names; `~/.memql/clusters.yaml` endpoints change; passkeys need re-enrolling
(magic-link remains the universal recovery route per the sign-in design's D6).

`make up DOMAIN=local.znas.io` reproduces the old hostnames exactly — and
does so *through the new parameter*, emitting the two Ingress patches. That is the
proof the parameterisation is real rather than a rename.

---

## 6. Testing

**Go.**

- `component/genesis/domain_test.go`: a table of `MEMQL_DOMAIN` → each derived value;
  explicit-env-wins for every one; unset domain is a no-op; malformed inputs (scheme,
  port, trailing dot, uppercase) rejected.
- `component/identity/config_test.go`: `isSingleProcessHost("identity.memql.localhost")`
  is false; the four literals are true; the ephemeral-key guard fires for a
  `.localhost` issuer with no signing key.
- One test proves the normalization seam covers all four readers: a node with only
  `MEMQL_DOMAIN` set resolves the same issuer as one with
  `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` set, checked through `portal`, `mcp` and
  `cluster` rather than through the verifier alone.
- Existing tests carrying `local.znas.io` literals move with the default:
  `component/portal/config_test.go`, `component/identity/config_test.go`,
  `dial_url_test.go`, `discovery_endpoint_contract_test.go`, and the other files the
  grep in §1.1 turns up.

**Render.** `kustomize build deploy/k8s/overlays/local` and assert: no domain literal
survives outside the two Ingress hosts; every node Deployment carries the
`memql-domain` envFrom and no `EXPECTED_ISSUER` env entry. That is the test that
catches a tenth node type being added without the regex-targeted patch reaching it —
the failure the design is relying on the regex to prevent, verified instead of
assumed. A second pass applies the emitted custom-domain patches and asserts every
hostname moved together, catching the index-based JSON 6902 going stale if a rule is
ever added to an Ingress.

**Scripts.** `hosts_entries_test.go` covers all three probe outcomes (§5.1) via the
`resolve.sh` stub hook. Domain-derived defaults in `verify_frontdoor_test.go`;
SAN-coverage re-issue in `mkcert_setup_test.go`; ConfigMap creation and the secret
rename in the seed-secrets tests; new param specs through the existing
`scripts/lib/capability_contract_test.go`.

**Extension (`node --test`).** The validator accepts `memql.localhost` and
`lab.example.com` and refuses `https://x`, a single label, a bare IP, a wildcard and
uppercase; `--domain` reaches `clusterUp` as well as the four steps memql#3590 already
threads; the collect screen's default is the new constant.

**Cross-cutting guard.** A sweep asserting no `znas.io` literal remains under
`deploy/`, `scripts/`, `editors/` and `component/`. It serves the de-branding goal
directly and stops re-introduction — with the caveat CLAUDE.md already records about
banned-name lists: it catches this name, not the next one. Allowlist: this design doc
and prior specs.

**What cannot be verified from the authoring machine.** That box has no docker group,
k3d or kubectl, so `make up` is impossible there. Every cluster-gated check has to run
on an operator machine or in CI:

```bash
make up-refresh                     # default domain
make status                         # unique node ids + one shared JWKS keyset
kubectl -n memql get ingress -o wide
curl -sS https://identity.memql.localhost/.well-known/jwks.json | head

make up-refresh DOMAIN=lab.example.com   # BYO path, after a wildcard A record exists
kubectl -n argocd get app memql -o jsonpath='{.spec.source.kustomize.patches}'
```

Separately, memql#3405 leg 2 (iOS/Android hybrid passkey against a `.localhost` RP id,
and Safari) stays open and `needs-human`. It does not block this work — magic-link is
the recovery route — but the wizard copy must not promise phone enrolment until
somebody measures it.

---

## 7. Out of scope

- LAN or public-internet reach (D1). No LAN-IP mode, no port forwarding.
- Let's Encrypt / cert-manager locally. mkcert stays the local TLS source.
- Changing a domain in place after install (D6 refuses it; reinstall instead).
- The staging and prod overlays, and the base's `identity.staging.example.com`
  placeholder. Both are near-empty stubs in this repo; the real overlays live
  downstream.
- `nip.io` / `sslip.io`, rejected by D5 of the install-wizard design on router
  DNS-rebind grounds.
- Anything introducing a second connection shape, such as a port-forward — forbidden
  by `docs/public/operate/environment-parity.md`.

---

## 8. Open risk

**memql#3405 leg 2 is unmeasured.** A `.localhost` RP id is accepted by Chrome's
validator (measured), but nothing yet says what iOS and Android passkey providers do
over hybrid transport, or whether Safari agrees. If leg 2 comes back negative, the
default domain — and only the default — has to change; the mechanism in this design
does not, which is the point of making the domain a parameter.
