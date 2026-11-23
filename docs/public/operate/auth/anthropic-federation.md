---
title: Anthropic Workload Identity Federation
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Anthropic workload identity federation

**What it replaces:** the one static Anthropic API key on the hand-seeded
`memql-secrets` Secret that every engine pod `envFrom`s.

**What it replaces it with:** the pod's own Kubernetes service-account token,
exchanged by the engine for a bearer that lives one hour.

The key never rotated, sat in every engine pod's environment, and let anyone
who could read the Secret spend against the account from anywhere. After the
cutover in this runbook, no long-lived Anthropic credential exists in the
cloud at all.

Epic: memql#4333. Design:
`docs/superpowers/specs/2026-08-22-anthropic-workload-identity-federation-design.md`.
The one-sign-in rule, the check command's behaviour and the local-cluster path
are from design record
`docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md`.

---

## How it works, in one paragraph

Kubernetes projects a signed OIDC token into each engine pod, minted for the
audience `https://api.anthropic.com` and carrying the subject
`system:serviceaccount:memql:memql-engine`. The engine POSTs that token to
`https://api.anthropic.com/v1/oauth/token` (RFC 7523 `jwt-bearer`, through the
SDK's own exchange function) naming a federation rule, an organization and a
service account. Anthropic verifies the token against the cluster's public
keys, checks it against the rule, and returns a short-lived
`sk-ant-oat01-...` bearer. The engine signs in again before the bearer expires,
with the next token Kubernetes has projected. There is no refresh token and
nothing long-lived at rest.

---

## One sign-in per process, and why

Anthropic accepts an identity token **once**. A projected token carries a
`jti`, and a second sign-in with the same token is refused as a replay. The
engine is built around that.

- **Each engine process keeps one sign-in for Anthropic**, shared by every
  Claude provider. A provider reload keeps it while the ids are unchanged. It
  used to make one per provider record, so only the first could be accepted.
- **The engine never presents an accepted token twice.** When the bearer nears
  expiry it signs in with the token Kubernetes has projected since. A sign-in
  Anthropic refused is not retried on a timer. After fixing something in the
  Console, press **Apply** in Settings > Vendors: it lets every node present
  its refused token once more. **Verify** does the same for the one node that
  answers it, and no other.
- **"Come back later" is not a refusal.** A sign-in Anthropic answers with
  408, 425 or 429 is tried again after a short pause, by itself. It is not
  counted as a denial.
- **The projected token lives 600 seconds**, so Kubernetes replaces it about
  every 8 minutes. The bearer still lives the hour the rule's
  `token_lifetime_seconds` names. The short token is for one case: a container
  that restarts inside a living pod reads the token its predecessor already
  used, is refused, and waits for the next one. That wait is at most about 8
  minutes, on that one pod; its sibling replicas keep serving.
  `provider-auth check` reports that state as not verified yet, with the time
  the next token is due, never as a refusal of your configuration.
- **The engine presents the next token by itself.** When a node is waiting for
  a token, or was refused in a way a used token alone explains, it signs in
  with the next token shortly after that token is due, with nobody calling a
  model. A new pod's first token cannot be told apart from a used one, so a
  rule that really is wrong on a new pod is reported as refused once that
  second token has been presented, about 8 minutes after the pod started.
- **Every node type that calls models signs in once when it starts** and lists
  Anthropic's models with the bearer, so there is a result to read before the
  first call. Only both halves count as verified: a bearer the API then
  refuses is a refusal. The identity node signs in to nothing: see
  [Separate Kubernetes identities](#separate-kubernetes-identities).

WARNING: none of this has been run against Anthropic. It is built from
Anthropic's documentation and tested against a stand-in that refuses a
repeated token. The first cluster to federate confirms it: note the bearer's
lifetime the check reports, restart one container on purpose, and record how
long that pod went without Anthropic.

---

## What this needs from you, once per cluster

Four ids, produced in the Anthropic Console and written into the cloud
overlay's ConfigMap. They are **per cluster**: the rule is bound to the
cluster's OIDC issuer, so a re-created cloud cluster has a new issuer and
needs steps 1 to 3 again. A local cluster keeps its issuer and keys across
recreation: see [On a local cluster](#on-a-local-cluster).

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

The engine needs four values to federate: rule, organization, service account
and token file. The three ids decide whether Anthropic is configured at all:

| What is set | What the engine does |
|---|---|
| all three ids and the token file | federates |
| no id | leaves the Anthropic providers registered as unavailable |
| one or two ids, or every id with no token file | **refuses the vendor**: every Anthropic provider registers as unavailable, with a message naming what is set and what is missing. `provider-auth check` prints that message and exits 1 |

**The token file's path is not yours to set, and does not count on its own.**
Every Deployment sets `MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE` and mounts the
projected token, on every cluster, so it is present on a cluster nobody has set
up. With no id the vendor is simply not configured. The engine used to count
the path as one of four values somebody had filled in, which made every pod of
an untouched cluster report itself half-configured.

**THERE IS NO KEY ARM ANY MORE** (epic memql#5088). This table used to have a
fourth row -- "none: uses the API key, as before" -- and a fifth for a cluster
carrying both. The key path is deleted from the product, for both vendors, so
"none" now means the Anthropic providers register as unavailable, which is not
an error: a cluster with no federated vendor boots, serves everything that
needs no model, and reports `ai` as unconfigured.

The last row is deliberate, and it is the reason the key arm could go without
anyone noticing a gap. Falling back on a half-configured federation would work
in every test, boot every node, and stop working the hour you finished step 3
of this runbook -- on whichever node nobody was watching. A refusal that names
both halves, on every call that would have used the vendor and in
`memql provider-auth check`, is loud, immediate and attributable.

---

## The cutover

### Step 1 -- Confirm the cluster's OIDC issuer is publicly reachable

Anthropic fetches the issuer's JWKS over the public internet. A cluster whose
issuer cannot be reached that way, which is every local one, gives Anthropic
its keys directly instead: skip to [On a local cluster](#on-a-local-cluster).

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

The command reports what the engine in that pod recorded when it started, and
waits up to its `--timeout` for the engine to have signed in and listed
Anthropic's models. It contacts nobody itself: a second process signing in
with the pod's token would use the token up.

Expect:

```
provider:              claudeHaiku
vendor:                anthropic
credential:            federation
federationRuleId:      fdrl_...
organizationId:        ...
serviceAccountId:      svac_...
workspaceId:           (none -- Anthropic picks the rule's workspace)
tokenSubject:          system:serviceaccount:memql:memql-engine
tokenAudience:         https://api.anthropic.com
signIn:                ok
signInAt:              2026-08-23T17:04:11Z
tokenExpires:          2026-08-23T18:04:11Z (in 59m12s)
api:                   took the bearer, last shown 2026-08-23T17:04:12Z
models.list:           12 models returned to the engine
```

`signIn` is Anthropic accepting the identity token. `api` is Anthropic's API
taking the bearer that sign-in bought, which is the half that makes the vendor
usable.

| Exit | Meaning |
|---|---|
| 0 | Verified: Anthropic accepted the engine's sign-in, and its API took the bearer (a model list, or a served call). Not merely that the config parses |
| 1 | Refused: by Anthropic at sign-in, by its API when the bearer was used, or by the engine itself (a partial set of ids, an identity token it cannot use). The reason is on stderr, in the words of whoever refused |
| 3 | Not set up: this pod holds no Anthropic ids, or it is a node type that signs in to no vendor |
| 4 | Cannot say yet: nothing is recorded (the node has only just started), the engine is waiting for its next identity token, a refusal a used token alone would explain, Anthropic did not answer, or nothing has used the bearer. Run it again |

The ids printed are the ones the engine used, including ids applied through
Settings > Vendors. Run it against more than one node type; each pod holds its
own token, so "it works on agent" is not "it works".

In MemQL OS, **Settings > Vendors > Verify** asks the node that answers to
list Anthropic's models through its own client. It costs no tokens. It makes a
sign-in only when that node's last one was refused, which Verify lets it try
once more; a second press reaches whichever node answers next, so use
**Apply** to reach every node at once.

Then watch the exchange counter across a refresh cycle or two:

```
memql_ai_federation_exchanges_total{vendor="anthropic",outcome="ok"}      # should tick up slowly
memql_ai_federation_exchanges_total{vendor="anthropic",outcome="denied"}  # must stay flat
```

**Include the vendor label** (memql#5088 added it). Both federating vendors
share this counter, so a query without it sums the two -- an OpenAI denial
would fire an alert an operator reads as Anthropic's, and a healthy OpenAI
`ok` rate would mask Anthropic's having gone to zero.

A steady low `ok` rate is the healthy shape -- roughly one per bearer lifetime
per pod. **Alert on `denied`.** A denial does not break traffic
immediately: the last good bearer keeps working until it expires, so the
outage arrives up to an hour after the cause and looks unrelated to whatever
was changed. One `denied` right after a container restarted in its pod is the
expected case above; it clears with the next token.

`scripts/install/verify-provider-key.sh` wraps the same check for the
installer:

```bash
scripts/install/verify-provider-key.sh \
  --provider=anthropic --federation-deploy=agent --namespace=memql
```

Exit 3 means the check ran and the credential is not usable: Anthropic refused
it, or the pod holds no Anthropic ids at all, and the message says which. Exit
5 means the check could not be run, or ran and could not say yet, which says
nothing about the credential.

### Step 5 -- There is nothing left to remove

This step used to drop the Anthropic key out of `memql-secrets` and roll the
mesh. Epic memql#5088 removed the key path from the product entirely -- there
is no env name for a vendor key any more, and nothing reads one -- so a key
still sitting in your Secret from before the cutover is INERT rather than in
use.

Delete it for hygiene if it is there, and then **revoke it in the Console**.
Removing it from the Secret makes the cluster stop carrying it, not the key
stop working.

```bash
kubectl get secret memql-secrets -n memql -o json | jq '.data | keys'
# anything vendor-key-shaped in that list is left over; drop it and re-apply.
```

### Step 6 -- Record the ids

Put the four ids in the install notes for this cluster
(`docs/public/operate/azure-entry-install.md` lists them among the
per-cluster values). A re-created cloud cluster gets a new OIDC issuer, which
invalidates the rule and needs steps 1 to 3 again.

---

## When it says no

The Console's Workload identity -> authentication events tab shows a reason
for every refusal, and the engine logs the same string from Anthropic's
response body.

**The log message names no vendor; the vendor is a FIELD** (memql#5088 made one
observer serve both). The line to look for is

```
WARN federation: token exchange DENIED -- the cluster is running on a
     credential the vendor will not renew   vendor=anthropic status=401
     vendorError="..." runbook=docs/public/operate/auth/anthropic-federation.md
```

Filter on `vendor=anthropic`, not on the message: grepping for a vendor name
inside the message finds nothing at all.

| Reason | What it means | What to do |
|---|---|---|
| `match_subject_prefix` | the token's `sub` is not under the rule's prefix | compare `tokenSubject` from `provider-auth check` against the rule. A pod running as the `default` service account is the usual cause |
| `match_audience` | the token was minted for a different audience | the projected volume's `audience` field; the render gate pins it to `https://api.anthropic.com` |
| `workspace_id_required` | the rule covers more than one workspace | set `MEMQL_AI_ANTHROPIC_WORKSPACE_ID` |
| `issuer_not_found` / JWKS fetch failure | Anthropic cannot reach the cluster's issuer | re-run step 1 from outside your network |
| `token_expired` | clock skew, or a token older than the issuer's maximum JWT lifetime | check node clocks; the projected token is replaced about every 8 minutes |
| a `401` for a token already used, right after a container restarted | the new process read the token its predecessor presented | nothing. The engine presents the next token when Kubernetes has projected it, within about 8 minutes and with no call needed; `provider-auth check` exits 4 until then |
| `408`, `425` or `429` on the sign-in | Anthropic asked the engine to come back | nothing. The engine tries again after a short pause; it is logged as a fault, not counted as a denial |
| `service_account_disabled` | the account was disabled or deleted | Console -> Settings -> Service accounts |

Boot-time refusals are a different class and never reach Anthropic:

| Message | Cause |
|---|---|
| `HALF-CONFIGURED for Anthropic workload identity federation` | one to three of the four ids are set. The message names both halves |
| `cannot read the projected identity token at ...` | the Deployment is missing the `anthropic-identity` volume or its mount |
| `does not carry the "https://api.anthropic.com" audience` | the projected volume names a different audience |
| `sub=... is not a Kubernetes service account` | the token is not a projected service-account token |

---

## Separate Kubernetes identities

The workbench uses `memql-engine-workbench`, which matches the documented
`system:serviceaccount:memql:memql-engine` subject prefix. Verify that match in
the installed federation rule before rollout; do not broaden it to every
service account in the namespace. Other inference nodes use `memql-engine`.
**Identity** runs as `memql-deploy` -- the account holding the deploy console's Rollout and
Application grants (memql#4257). Moving identity onto `memql-engine` would
either strip that RBAC or put it on the account every engine node runs as,
handing the whole mesh a privilege one node needs.

Identity still carries the rest of the shape: the projected volume, the mount
and `MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE`. What it does NOT carry is a
matching subject -- its token says
`system:serviceaccount:memql:memql-deploy`, which the rule's
`subject_prefix` does not match.

This costs nothing today: identity does not call Anthropic. For the same
reason it does not sign in when it starts, as every other node type does: the
sign-in would be refused on every boot. `provider-auth check` run against
identity therefore reports that this node type signs in to no vendor, and
exits 3. If identity
ever needs Anthropic, add a second rule (or widen the prefix to
`system:serviceaccount:memql:`) in the Console, and remove its opt-out in
`app/build_identity.go` -- not a change to the manifests.

---

## On a local cluster

A local cluster can federate. What it cannot do is be fetched from: k3d's
issuer is `https://kubernetes.default.svc.cluster.local`, and its keys live
where Anthropic cannot reach. Anthropic's **inline** mode takes the keys pasted
in instead, and after that never contacts the cluster. An earlier version of
this runbook said a local cluster federates with neither vendor; that was
wrong.

1. Run `make federation-keys`. It prints the issuer, the public keys, the two
   engine subjects and the audience. It prints public material only.
2. In the Console, register the issuer in **inline** mode and paste the `jwks`
   value from the result. Create the service account and the rule exactly as
   in step 2 of the cutover.
3. In MemQL OS, open Settings > Vendors, choose **Set up Anthropic**, enter the
   ids and press **Apply**. A local cluster has no overlay to edit; the ids are
   stored in the cluster and every node reads them on Apply.
4. Verify as in step 4 of the cutover.

**The signing key.** `make up` keeps a local cluster's token signing key in
`~/.memql/k3d/<cluster>/service-account.key` and gives it back to the cluster
each time the cluster is created. `make up-refresh` therefore leaves the
issuer and its keys unchanged, and what you pasted stays good. A cluster
created before this existed gets the file the next time it is recreated; run
`make federation-keys` and paste once more then. `make down PURGE=1` deletes
the file, and so does uninstalling the local cluster when the install created
it. A machine without `openssl` creates the cluster without one and says so.

**That file is a secret.** Anyone who holds it can mint a token Anthropic
accepts for this cluster's rule. It is readable by its owner only and is never
printed. Bind the rule to a workspace with a low spend limit, and delete the
rule in the Console when the cluster is gone for good.

The manifests are identical everywhere; only the ids differ. See
[environment-parity.md](../environment-parity.md).

---

## What is deliberately NOT here

- **OpenAI, which is its own runbook rather than an absence.** An earlier
  version of this document said there was no federation mechanism on that
  side. That was wrong when it was written: OpenAI's workload identity
  federation has been generally available since 2025-05-26. It is now wired,
  on this same `memql-engine` ServiceAccount with a second projected token --
  see [openai-federation.md](openai-federation.md).
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
- `component/memql/ai_vendor_signin.go` -- the one sign-in a process keeps per
  vendor, and the record `provider-auth check` reads.
- `scripts/k3d/federation-keys.sh` -- what a vendor is given to trust a cluster.
- `deploy/k8s/base/anthropic-federation.yaml` -- the ServiceAccount and the
  ConfigMap seam.
