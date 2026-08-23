---
title: Anthropic Workload Identity Federation
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Anthropic workload identity federation

**What it replaces:** the one static `MEMQL_AI_ANTHROPIC_API_KEY` on the
hand-seeded `memql-secrets` Secret that every engine pod `envFrom`s.

**What it replaces it with:** the pod's own Kubernetes service-account token,
exchanged by the Anthropic SDK for a bearer that lives one hour.

The key never rotated, sat in every engine pod's environment, and let anyone
who could read the Secret spend against the account from anywhere. After the
cutover in this runbook, no long-lived Anthropic credential exists in the
cloud at all.

Epic: memql#4333. Design:
`docs/superpowers/specs/2026-08-22-anthropic-workload-identity-federation-design.md`.

---

## How it works, in one paragraph

Kubernetes projects a signed OIDC token into each engine pod, minted for the
audience `https://api.anthropic.com` and carrying the subject
`system:serviceaccount:memql:memql-engine`. The engine hands that token to the
Anthropic SDK, which POSTs it to `https://api.anthropic.com/v1/oauth/token`
(RFC 7523 `jwt-bearer`) naming a federation rule, an organization and a
service account. Anthropic fetches the cluster's public JWKS, verifies the
token, checks it against the rule, and returns a short-lived
`sk-ant-oat01-...` bearer. The SDK re-exchanges before expiry. There is no
refresh token and nothing long-lived at rest.

---

## What this needs from you, once per cluster

Four ids, produced in the Anthropic Console and written into the cloud
overlay's ConfigMap. They are **per cluster**: the rule is bound to the
cluster's OIDC issuer, so a re-created cluster has a new issuer and needs
steps 1 to 3 again.

| Value | Looks like | Where it comes from |
|---|---|---|
| `MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID` | `fdrl_...` | the rule you create in step 2 |
| `MEMQL_AI_ANTHROPIC_ORGANIZATION_ID` | a UUID | Console -> Settings -> Organization |
| `MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID` | `svac_...` | the service account you create in step 2 |
| `MEMQL_AI_ANTHROPIC_WORKSPACE_ID` | `wrkspc_...` or `default` | **usually leave empty** -- see below |

`MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE` is the fifth name and is **already
set** by `deploy/k8s/base` on every engine Deployment; you do not fill it in.

**Leave the workspace id empty** unless the rule covers more than one
workspace. Anthropic picks the rule's workspace when there is only one, and
answers `workspace_id_required` when it genuinely needs to be told.

---

## The rule that decides everything: all four, or none

The engine reads the four required values -- rule, organization, service
account, token file -- as a set:

| What is set | What the engine does |
|---|---|
| all four | federates |
| none | uses `MEMQL_AI_ANTHROPIC_API_KEY`, as before |
| all four **and** the key | federates; logs one warning that the key is ignored |
| one, two or three | **refuses to boot**, naming which are missing |

The last row is deliberate. Falling back to the key on a half-configured
federation would work in every test, boot every node, and stop working the
hour you finished step 5 of this runbook -- on whichever node nobody was
watching. A refusal at boot is loud, immediate and attributable.

---

## The cutover

### Step 1 -- Confirm the cluster's OIDC issuer is publicly reachable

Anthropic fetches the issuer's JWKS over the public internet. If it cannot,
nothing else here will work.

```bash
az aks show --resource-group <rg> --name <cluster> \
  --query oidcIssuerProfile.issuerUrl -o tsv
# -> https://<region>.oic.prod-aks.azure.com/<tenant>/<uuid>/

# From OUTSIDE the cluster -- the point is that a stranger can read it:
curl -s <issuer>/.well-known/openid-configuration | head
```

INFO: if `issuerUrl` is empty, the cluster was created without the OIDC issuer
feature. Enable it (`az aks update --enable-oidc-issuer`) and re-read; it is a
control-plane change, not a node roll.

### Step 2 -- Create the service account and the rule in the Console

Console -> Settings -> Workload identity.

1. **Register the issuer.** Mode `discovery` (Anthropic fetches the JWKS
   itself), maximum JWT lifetime `3600`. Use "Verify issuer" before saving --
   it does the same fetch you just did by hand and fails here rather than at
   the first exchange.
2. **Create a service account** named `memql-engine`. Organization role
   `developer`; make it a member of the workspace your inference should be
   billed to. Note its `svac_...` id.
3. **Create ONE federation rule:**

   | Field | Value |
   |---|---|
   | subject match | `subject_prefix` = `system:serviceaccount:memql:memql-engine` |
   | audience | `https://api.anthropic.com` |
   | target | the `memql-engine` service account |
   | workspace | the workspace from step 2 |
   | `oauth_scope` | `workspace:inference` |
   | `token_lifetime_seconds` | `3600` |

   Note its `fdrl_...` id.

`workspace:inference` is the least privilege that serves the Messages API.
Try it first. Widen to `workspace:developer` only if a call fails for scope
and the denial says so -- and record why, because it is the difference
between "this credential can spend" and "this credential can also administer".

### Step 3 -- Put the ids in the overlay

Edit `deploy/k8s/overlays/cloud/kustomization.yaml` (or `cloud-entry`) and
replace the three `REPLACE-WITH-ANTHROPIC-*` placeholders in the
`memql-anthropic-federation` ConfigMap patch. Leave the workspace id empty
unless step 2 gave you a reason not to.

Merge. ArgoCD reconciles and rolls the engine Deployments.

INFO: nothing under `deploy/` may name a real rule, organization or account
id -- `TestTheCloudOverlaysCarryFederationPlaceholders` fails the build if the
committed overlay carries anything but a placeholder. The real ids belong in
your install notes.

### Step 4 -- Verify, before removing anything

```bash
kubectl exec -n memql deploy/agent -- /app/memql provider-auth check
```

Expect:

```
provider:              streamClaudeSonnet
credential:            federation
federationRuleId:      fdrl_...
serviceAccountId:      svac_...
identityTokenFile:     /var/run/secrets/anthropic.com/token
tokenSubject:          system:serviceaccount:memql:memql-engine
tokenAudience:         https://api.anthropic.com
exchange:              ok
tokenExpires:          2026-08-23T18:04:11Z (in 1h0m0s)
models.list:           12 models returned
```

Exit code 0 means Anthropic accepted the credential -- not merely that the
config parses. Run it against more than one node type; each pod holds its own
token, so "it works on agent" is not "it works".

Then watch the exchange counter across a refresh cycle or two:

```
memql_ai_federation_exchanges_total{outcome="ok"}       # should tick up slowly
memql_ai_federation_exchanges_total{outcome="denied"}   # must stay flat
```

A steady low `ok` rate is the healthy shape -- roughly one per token lifetime
per client. **Alert on `denied`.** A denial does not break traffic
immediately: the last good bearer keeps working until it expires, so the
outage arrives up to an hour after the cause and looks unrelated to whatever
was changed.

`scripts/install/verify-provider-key.sh` wraps the same check for the
installer:

```bash
scripts/install/verify-provider-key.sh \
  --provider=anthropic --federation-deploy=agent --namespace=memql
```

### Step 5 -- Remove the key

Only after step 4 passes on every engine node type.

```bash
# Drop the one key from the Secret, leaving the rest untouched.
kubectl get secret memql-secrets -n memql -o json \
  | jq 'del(.data.MEMQL_AI_ANTHROPIC_API_KEY)' \
  | kubectl apply -f -

kubectl rollout restart -n memql deploy/agent deploy/cognition deploy/planner \
  deploy/bff deploy/workbench deploy/voice deploy/edge deploy/mcp

kubectl exec -n memql deploy/agent -- /app/memql provider-auth check
```

The second check is the one that matters: it proves nothing was quietly
leaning on the key. Then **revoke the key in the Console** -- deleting it from
the Secret makes the cluster stop using it, not the key stop working.

### Step 6 -- Record the ids

Put the four ids in the install notes for this cluster
(`docs/public/operate/azure-entry-install.md` lists them among the
per-cluster values). A re-created cluster gets a new OIDC issuer, which
invalidates the rule and needs steps 1 to 3 again.

---

## When it says no

The Console's Workload identity -> authentication events tab shows a reason
for every refusal, and the engine logs the same string from Anthropic's
response body on `anthropic federation: token exchange DENIED`.

| Reason | What it means | What to do |
|---|---|---|
| `match_subject_prefix` | the token's `sub` is not under the rule's prefix | compare `tokenSubject` from `provider-auth check` against the rule. A pod running as the `default` service account is the usual cause |
| `match_audience` | the token was minted for a different audience | the projected volume's `audience` field; the render gate pins it to `https://api.anthropic.com` |
| `workspace_id_required` | the rule covers more than one workspace | set `MEMQL_AI_ANTHROPIC_WORKSPACE_ID` |
| `issuer_not_found` / JWKS fetch failure | Anthropic cannot reach the cluster's issuer | re-run step 1 from outside your network |
| `token_expired` | clock skew, or a token older than the rule's max lifetime | check node clocks; the projected token is re-minted hourly |
| `service_account_disabled` | the account was disabled or deleted | Console -> Settings -> Service accounts |

Boot-time refusals are a different class and never reach Anthropic:

| Message | Cause |
|---|---|
| `HALF-CONFIGURED for Anthropic workload identity federation` | one to three of the four ids are set. The message names both halves |
| `cannot read the projected identity token at ...` | the Deployment is missing the `anthropic-identity` volume or its mount |
| `does not carry the "https://api.anthropic.com" audience` | the projected volume names a different audience |
| `sub=... is not a Kubernetes service account` | the token is not a projected service-account token |

---

## One node is not on the engine service account

Every engine Deployment runs as `memql-engine` except **identity**, which runs
as `memql-deploy` -- the account holding the deploy console's Rollout and
Application grants (memql#4257). Moving identity onto `memql-engine` would
either strip that RBAC or put it on the account every engine node runs as,
handing the whole mesh a privilege one node needs.

Identity still carries the rest of the shape: the projected volume, the mount
and `MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE`. What it does NOT carry is a
matching subject -- its token says
`system:serviceaccount:memql:memql-deploy`, which the rule's
`subject_prefix` does not match.

This costs nothing today: identity does not call Anthropic. If it ever needs
to, add a second rule (or widen the prefix to
`system:serviceaccount:memql:`) in the Console -- not a change to the
manifests. `provider-auth check` run against identity will report the
mismatch as `match_subject_prefix`, which is the honest answer rather than a
surprise.

---

## What is deliberately NOT here

- **Local federation.** k3d's issuer is
  `https://kubernetes.default.svc.cluster.local`, whose JWKS lives on a node
  IP that Anthropic cannot reach. `inline` JWKS mode would mean registering
  an issuer per developer cluster and re-pasting it after every
  `make up-refresh`. The local cluster keeps the API key, and that is an
  allowed value difference, not a divergence in shape -- see
  [environment-parity.md](../environment-parity.md). The manifests are
  identical; only the four ids differ, and locally they are empty.
- **OpenAI.** There is no federation mechanism on that side. Its key stays,
  and `verify-provider-key.sh --provider=openai --key-file=...` still verifies
  it.
- **Scripting the Console setup.** The Admin API could do steps 1 and 2, but
  the manual path has to be walked once before it is worth automating.

---

## Related

- [access-model.md](access-model.md) -- how MemQL's own credentials work.
  This document is about a VENDOR credential and shares nothing with them.
- [env-vars.md](../env-vars.md) -- the five names and their precedence.
- [../../ai/llm-cost-control.md](../../ai/llm-cost-control.md) -- why the
  exchange is outside the LLM guard.
- `component/memql/ai_anthropic_federation.go` -- the credential decision.
- `deploy/k8s/base/anthropic-federation.yaml` -- the ServiceAccount and the
  ConfigMap seam.
