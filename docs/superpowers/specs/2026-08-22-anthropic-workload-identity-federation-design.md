# Anthropic workload identity federation -- the engine proves who it is instead of holding a key

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project E of nine)
**Owner:** `component/memql` (provider construction), `deploy/k8s` (the workload's identity), `docs/public/operate`

Sub-project E of the 2026-08-22 backlog brief. Replaces the long-lived
Anthropic API key in the cloud with a short-lived token the engine obtains by
presenting its Kubernetes service-account identity, while the local cluster
keeps the key as an allowed value difference.

---

## 1. Problem

The engine authenticates to Anthropic with one static key,
`MEMQL_AI_ANTHROPIC_API_KEY` (`dsl/providers/providers.memql:7-12`), resolved
once at init (`component/memql/ai_providers.go:930-975`:
`globalSecret` -> `globalVariable` -> env) and handed to
`anthropic.NewClient(option.WithAPIKey(...))` at `:1940-1949` and
`:2162-2170`. In the cloud that key is an extra entry on the hand-seeded
`memql-secrets` Secret that every node `envFrom`s
(`deploy/k8s/base/agent.yaml:57-59` and siblings); ESO manages seven other
keys of the same Secret and not this one. It never rotates, it lives in
every engine pod's environment, and anyone who can read the Secret can spend
against the account from anywhere.

Anthropic's Workload Identity Federation (GA since 2026-06-17) exists for
exactly this: a workload presents an OIDC JWT from its own issuer to
`POST https://api.anthropic.com/v1/oauth/token` (RFC 7523
`urn:ietf:params:oauth:grant-type:jwt-bearer`) naming a federation rule, an
organization and a service account, and receives a short-lived
`sk-ant-oat01-...` bearer; the SDK re-exchanges before expiry. No refresh
token, nothing long-lived at rest.

Two facts decide the shape of the change:

1. **The Go SDK MemQL pins already does this natively.**
   `github.com/anthropics/anthropic-sdk-go v1.63.1` (`component/memql/go.mod:10`)
   carries federation since v1.39.0: `option.WithFederationTokenProvider(option.IdentityTokenFile(path), option.FederationOptions{FederationRuleID, OrganizationID, ServiceAccountID, WorkspaceID})`
   (`option/requestoption.go:99-137`), with refresh handled inside the client
   (advisory at `exp-120s`, mandatory at `exp-30s`, token file re-read on
   every exchange). The exchange sends
   `anthropic-beta: oauth-2025-04-20,oidc-federation-2026-04-01` and a JSON
   body; the assertion must be under 16 KiB.
2. **But an explicit `option.WithAPIKey` suppresses the SDK's whole
   credential chain** (SDK `client.go:33-51`, `:176-190`). Setting every
   `ANTHROPIC_*` variable on the pod today would change nothing; the
   constructor has to choose federation deliberately.

---

## 2. What the tree already has

### 2.1 A provider auth block that resolves placeholders per key

`provider_decl.go:111-143` stores `env("VAR")` as the literal `${VAR}` in a
`map[string]string`; `resolveAuthPlaceholders` resolves every key through the
same three-tier lookup. New keys cost nothing in the parser; the constructor
decides what they mean.

### 2.2 The AKS cluster already has a public OIDC issuer in use

ESO authenticates to Key Vault through Azure workload identity
(`deploy/external-secrets/externalsecret-memql.yaml:23-34`, `secretstore.yaml:19-27`),
and so does the CloudNativePG backup identity
(`deploy/k8s/overlays/cloud/kustomization.yaml:135-149`); both depend on the
cluster's OIDC issuer URL (`https://<region>.oic.prod-aks.azure.com/<tenant>/<uuid>/`),
whose JWKS is publicly fetchable. Anthropic's `discovery` mode fetches the
issuer's JWKS over the public internet, which is what makes the direct
path possible. (Inferred from the manifests; no `az aks create` flags are
committed. The runbook's first step verifies it.)

### 2.3 Engine pods have no identity posture at all

No `ServiceAccount`, `serviceAccountName` or projected token appears under
`deploy/k8s/base` or `components/`; every engine pod runs as the namespace
`default` SA with only the default `kube-api-access-*` token (verified on the
live local cluster). A projected token with a custom `audience` is standard
Kubernetes; the pod side works identically on k3d and AKS.

### 2.4 k3d's issuer is private

`https://kubernetes.default.svc.cluster.local`, JWKS at a node IP (live
`kubectl get --raw /.well-known/openid-configuration`). Anthropic cannot
reach it; `inline` JWKS mode would mean a Console-registered issuer per
developer cluster, re-pasted after every `make up-refresh`. Local federation
is not a reproducible path and is not attempted.

### 2.5 The parity rule already allows this class of difference

`docs/public/operate/environment-parity.md` lists the TLS source (mkcert vs
cert-manager) and the secrets source as VALUE differences. The credential
source for one vendor is the same class, provided the manifests ship the
same shape everywhere and the engine never branches on target
(`TestNoEnvironmentBranchingInEngineCode`).

### 2.6 The LLM guard wraps the same client and ignores the exchange

`ai_guard.go:890-899` installs one `http.RoundTripper` on every provider
client; `isGuardedLLMPath` (`:719-726`) fingerprints only `/messages` and
`/chat/completions`. The exchange POST passes through unguarded and
uncounted -- correct, since it is not an LLM call; it only needs to be
observed.

### 2.7 The env registry gates names in both directions

`cmd/envscan` fails on a name read in Go but unregistered, and on a name
registered but read nowhere (`docs/public/operate/env-vars.md:31-45`);
`TestOwnedVarsArePrefixed` refuses non-`MEMQL_` names outside the `external`
allow-list. Reading federation config through MemQL-owned names keeps all
of that honest; relying on the SDK's `ANTHROPIC_*` discovery would not.

---

## 3. Decisions

### D1 -- The Kubernetes service-account token, presented directly

Chosen over the Entra two-hop (an app registration, a UAMI and a federated
credential per cluster, an `azidentity` dependency, Azure-specific Go in a
provider-neutral engine) and over an out-of-process exchanger (the token must
travel as `Authorization: Bearer`, not `x-api-key`; providers resolve auth
once at init). Fewest moving parts; the SDK owns refresh; Anthropic's own
Azure guide says skipping Entra is fine. Cost: the issuer URL is per cluster,
an install-time value like the CNPG client id.

### D2 -- Configuration is five sibling keys in the provider's `auth` block

MemQL-owned names, registered, resolved like `apiKey`. The ids are values
(they name objects; the token proves identity) and may live in a ConfigMap
or a `globalVariable`; the token file path is a value; the key stays a
secret.

### D3 -- Federation is chosen deliberately, partial config is refused

All four of rule id, organization id, service account id, token file set:
federate. None set: the key is required, as today. Both: federation wins and
boot warns once. One to three: **boot refuses**. A half-configured
federation is a misconfiguration, and the alternative -- silently falling
back to a key that is about to be deleted -- is the failure mode this design
exists to remove.

### D4 -- One shape, two values

The ServiceAccount, the projected token volume and the token-file path are
base manifests, everywhere. The four ids are a ConfigMap the cloud overlay
fills and the local overlay leaves empty. `make up` boots on the key with
the same YAML the cloud runs.

### D5 -- Least privilege on the Anthropic side

One service account for the engine, one rule pinned to
`system:serviceaccount:memql:memql-engine` and the exact audience, the
MemQL workspace, scope `workspace:inference`, one-hour tokens.
`workspace:developer` only if inference proves insufficient for the
Messages API, and the runbook says to try inference first.

### D6 -- The key leaves the cloud once federation is verified

The cutover ends with `MEMQL_AI_ANTHROPIC_API_KEY` removed from the cloud
`memql-secrets`. A long-lived key then exists only on operators' local
clusters, where the blast radius is one developer's laptop.

---

## 4. The change

### 4.1 DSL

```memql
@base
@type("Anthropic")
provider anthropic {
  auth {
    apiKey             env("MEMQL_AI_ANTHROPIC_API_KEY")
    federationRuleId   env("MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID")
    organizationId     env("MEMQL_AI_ANTHROPIC_ORGANIZATION_ID")
    serviceAccountId   env("MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID")
    workspaceId        env("MEMQL_AI_ANTHROPIC_WORKSPACE_ID")
    identityTokenFile  env("MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE")
  }
}
```

`workspaceId` is optional on the Anthropic side (required only when a rule
spans more than one workspace; the exchange answers `workspace_id_required`
otherwise) and optional here.

### 4.2 Constructor

A small `anthropicCredential(cfg)` helper used by both constructors:

```
switch {
case all four federation keys set:
    opts = WithFederationTokenProvider(IdentityTokenFile(path), FederationOptions{...})
    if apiKey != "": warn once ("federation configured; MEMQL_AI_ANTHROPIC_API_KEY ignored")
case any federation key set:
    return error naming the missing keys   // strict boot refuses
default:
    if apiKey == "": return error (unchanged)
    opts = WithAPIKey(apiKey)
}
client = anthropic.NewClient(opts..., option.WithHTTPClient(guarded))
```

Boot preflight, when federated: read the token file once; it must exist, be
a JWT, and carry `aud` containing `https://api.anthropic.com` and a
`sub` beginning `system:serviceaccount:`; otherwise fail the way a missing
key fails today, naming the file and the claim.

### 4.3 Observation

The guard transport gains a narrow observer for `/v1/oauth/token` on the
Anthropic host: `memql_ai_federation_exchanges_total{outcome=ok|denied|error}`
and a warn log carrying the Anthropic error body on denial (the Console's
history tab shows the same reason: `match_subject_prefix`,
`workspace_id_required`, ...). `isGuardedLLMPath` stays as it is -- the
exchange is not an LLM call and must not count toward the rate ceiling or
the cost kill-switch.

A `memql provider-auth check [--provider anthropic]` subcommand builds the
provider exactly as boot does, forces one exchange, calls `models.list`, and
prints which credential path was used and the token's expiry. It is what the
runbook and `scripts/install/verify-provider-key.sh` call from inside a pod
(`kubectl exec`); the script gains a federation branch beside its
`x-api-key` curl.

### 4.4 Manifests

`deploy/k8s/base`:

- `serviceaccount.yaml`: `memql-engine`, no RBAC.
- Every engine Deployment (bff, cognition, agent, planner, voice, voice-agent,
  workbench, mcp, edge, identity -- identity does not call Anthropic, but the
  shape is the shape): `serviceAccountName: memql-engine`; a projected volume
  `anthropic-identity` with one `serviceAccountToken` source
  (`audience: https://api.anthropic.com`, `expirationSeconds: 3600`,
  `path: token`) mounted read-only at `/var/run/secrets/anthropic.com`;
  `MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE=/var/run/secrets/anthropic.com/token`
  in the base env.
- A `memql-anthropic-federation` ConfigMap carrying the four id variables,
  `envFrom` with `optional: true` on the same Deployments. The base ships it
  with empty values; `deploy/k8s/overlays/cloud` (and `cloud-entry`) patch the
  real ids; the local overlay leaves it empty.

A kustomize-build test asserts the SA, the volume and the env on every
engine Deployment in both overlays, and that the local build carries empty
ids.

### 4.5 Registry and docs

Five entries in `scripts/secrets/manifest.yaml` (`component: ai`,
`scope: node`, `optional: true`, `kind: value` except the key), synced to the
embedded manifest; `docs/public/operate/env-vars.md` documents them and the
precedence rule of D3; `environment-parity.md` adds the credential-source
row; a new `docs/public/operate/auth/anthropic-federation.md` runbook
(section 5); `docs/public/ai/llm-cost-control.md` notes the exchange is
outside the guard's fingerprint by design.

---

## 5. The cutover runbook (summary; the doc is the task)

1. Confirm the AKS issuer: `az aks show --query oidcIssuerProfile.issuerUrl`
   and `curl <issuer>/.well-known/openid-configuration` from outside the
   cluster.
2. In the Anthropic Console (Settings -> Workload identity): register the
   issuer (discovery mode, max JWT lifetime 3600); create service account
   `memql-engine` (organization role developer, member of the MemQL
   workspace); create one rule: `subject_prefix`
   `system:serviceaccount:memql:memql-engine`, `audience`
   `https://api.anthropic.com`, the workspace, `oauth_scope`
   `workspace:inference`, `token_lifetime_seconds` 3600. Use "Verify issuer"
   before saving.
3. Put `fdrl_...`, the organization UUID, `svac_...` and the workspace id into
   the cloud overlay's ConfigMap patch; merge; let ArgoCD roll.
4. `kubectl exec deploy/agent -- memql provider-auth check` -> `federation`,
   `models.list` ok, expiry ~1h. Watch
   `memql_ai_federation_exchanges_total` for a few refresh cycles.
5. Remove `MEMQL_AI_ANTHROPIC_API_KEY` from `memql-secrets`; roll; `check`
   again; revoke the key in the Console.
6. Record the four ids in the install notes (they are per cluster; a
   re-created cluster has a new issuer and needs steps 1-3 again).

---

## 6. Testing

1. Constructor: federation chosen when all four are set; partial
   configuration refused naming the missing keys; both set warns once; no
   keys errors as before; the guarded transport is attached in every branch.
2. An `httptest` server standing in for `api.anthropic.com`: the client
   exchanges a fixture JWT at `/v1/oauth/token` (assert the body fields and
   the beta header), then a Messages call carries `Authorization: Bearer
   <token>` and no `x-api-key`; the counter moves; a denial is logged with
   the body.
3. Preflight: a missing file, a non-JWT file, a wrong audience each refuse
   boot with a message naming the problem.
4. `provider-auth check` reports the credential path and expiry.
5. Kustomize: both overlays build; SA, volume, env and ConfigMap on every
   engine Deployment; local ids empty.
6. `make env-registry-check` and `TestOwnedVarsArePrefixed` pass with the
   five new names.
7. `make up` on k3d boots and serves an AI call on the key (no federation
   ids); the cloud cutover is verified by the runbook's step 4.

---

## 7. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the engine | DSL keys, constructor, preflight, exchange observer, `provider-auth check`, registry entries | nothing |
| 2 -- the cluster and the runbook | base SA + projected token + ConfigMap seam, overlay patches, verify-script branch, runbook, env-vars and parity docs | nothing (merging before PR 1 is harmless: the file mounts, the ids are empty) |

One `Closes #N` line per issue. The cutover itself (runbook steps 2-6) is
operator work after both merge, not a PR.

---

## 8. Out of scope

- OpenAI: no federation mechanism exists on that side; its key stays.
- Scripting the Console setup through the Admin API (a later capability
  script once the manual path has been run once).
- Local federation via inline JWKS (not reproducible; D1 / 2.4).
- The Entra two-hop (rejected in D1; revisit only if Entra conditional
  access becomes a requirement).
- The separate drift bug that `authConceptLookupNames` bridges only
  `MEMQL_SI_*` names (`ai_providers.go:868-874`), filed on its own.

---

## 9. References

- Code: `dsl/providers/providers.memql`, `component/memql/ai_providers.go`,
  `component/memql/ai_guard.go`, `component/language/parser/provider_decl.go`,
  `deploy/k8s/base/*.yaml`, `deploy/k8s/overlays/{cloud,cloud-entry,local}`,
  `deploy/external-secrets/`, `scripts/secrets/manifest.yaml`,
  `scripts/install/verify-provider-key.sh`, `cmd/envscan`.
- Anthropic: Workload identity federation overview, WIF reference, the
  Kubernetes and Azure provider guides, the Admin API (all under
  `platform.claude.com/docs/en/manage-claude/`); Go SDK changelog v1.39.0 /
  v1.40.0.
- Docs: `docs/public/operate/env-vars.md`, `environment-parity.md`,
  `docs/public/ai/llm-cost-control.md`, `azure-entry-install.md`.
