---
title: OpenAI Workload Identity Federation
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# OpenAI workload identity federation

**What it replaces:** the one static OpenAI API key on the hand-seeded
`memql-secrets` Secret that every engine pod `envFrom`s.

**What it replaces it with:** the pod's own Kubernetes service-account token,
exchanged by the engine for a bearer that lives at most an hour.

The key never rotated, sat in every engine pod's environment, and let anyone
who could read the Secret spend against the account from anywhere. After this
runbook, no long-lived OpenAI credential exists in the cloud at all -- and,
unlike the Anthropic cutover, there is nothing left to remove afterwards,
because the key path is gone from the product rather than merely unused.

Epic: memql#5088. Design:
`docs/superpowers/specs/2026-09-06-openai-federation-and-key-removal-design.md`.
The one-sign-in rule, the check command's behaviour and the local-cluster path
are from design record
`docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md`.

The Anthropic runbook is the sibling of this one:
[anthropic-federation.md](anthropic-federation.md). Read whichever vendor you
are turning on; the shape is deliberately the same, and the two share one
workload identity.

---

## How it works, in one paragraph

Kubernetes projects a signed OIDC token into each engine pod, minted for the
audience `https://api.openai.com/v1` and carrying the subject
`system:serviceaccount:memql:memql-engine`. The engine POSTs that token to
OpenAI's token endpoint as an RFC 8693 token exchange, naming an identity
provider and a service account. OpenAI verifies the token against the
cluster's public keys, checks it against the provider's mapping, and returns a
short-lived bearer. The engine signs in again before the bearer expires. There
is no refresh token and nothing long-lived at rest.

**Each engine process keeps one sign-in for OpenAI**, shared by chat,
streaming, speech, transcription, embeddings and the transcription socket. It
used to make one per provider record and one more for each transcription
path. Every node type that calls models signs in once when it starts, so there
is a result to read before the first call, and a sign-in OpenAI refused is not
retried on a timer: pressing **Verify** or **Apply** in Settings > Vendors
lets every node try once more.

The projected token stays at 3600 seconds, unlike Anthropic's 600. An OpenAI
bearer never outlives the token it was minted from, so a 600 second token
would force a sign-in every 8 minutes with about 90 seconds between
Kubernetes replacing the token and the bearer expiring. OpenAI documents no
rule against presenting a token twice, so the restarted-container case that
motivates Anthropic's short token does not arise here.

### CONFIRM THE TOKEN ENDPOINT BEFORE THE FIRST CUTOVER

The engine posts to **`https://auth.openai.com/oauth/token`**
(`openaiTokenEndpoint`, component/memql/ai_openai_federation.go).

**Checked against OpenAI's documentation on 2026-11-21, and not yet against
the live endpoint.** OpenAI's workload identity federation reference names
this address and the five JSON fields the engine sends. It is still the single
most load-bearing constant in the feature: every other id can be wrong in a
way that produces a refusal you can read, and this one wrong produces a
connection error at a hostname, which reads like a network problem.

So the first cutover confirms it: after a node starts, step 4's
`provider-auth check` reports a sign-in OpenAI answered, or a fault at that
address. If OpenAI moves it, change that one constant; nothing else in the
exchange depends on it.

Two properties of that bearer are worth knowing before you plan around it: it
never outlives the subject token it was minted from, and it is scoped to a
Platform service account in a project, which excludes the Admin API. So a
credential obtained this way can spend on inference and cannot administer the
organization.

---

## This is a SECOND projected token, not a shared one

The Anthropic federation already puts a projected token in every engine pod. It
cannot be reused here, and the reason is structural rather than a policy
choice: a projected service-account token is minted for exactly ONE audience,
and OpenAI's is `https://api.openai.com/v1`. Handing it Anthropic's token would
be refused at the exchange.

So every engine Deployment carries two projected volumes and two mounts:

| Vendor | Volume | Audience | Mount |
|---|---|---|---|
| Anthropic | `anthropic-identity` | `https://api.anthropic.com` | `/var/run/secrets/anthropic.com` |
| OpenAI | `openai-identity` | `https://api.openai.com/v1` | `/var/run/secrets/openai.com` |

What they DO share is the identity itself: one `memql-engine` ServiceAccount,
one subject, declared once in `deploy/k8s/base/anthropic-federation.yaml`. Do
not add a second ServiceAccount for OpenAI -- a second subject would mean two
mappings to keep in step and two answers to "what is this cluster's identity".

`deploy/k8s/overlays/render_openai_federation_test.go` pins all of it, including
that the two audiences are different strings on every engine Deployment in every
overlay. That last assertion exists because the cheapest wrong implementation of
this feature is to mount the Anthropic token at a second path and call it
OpenAI's: it renders, deploys, and fails only at the first exchange.

---

## What this needs from you, once per cluster

Two ids, produced in the OpenAI Platform console and written into the cloud
overlay's ConfigMap. They are **per cluster**: the identity provider is bound to
the cluster's OIDC issuer, so a re-created cloud cluster has a new issuer and
needs steps 1 to 3 again. A local cluster keeps its issuer and keys across
recreation: see [On a local cluster](#on-a-local-cluster).

| Value | Where it comes from |
|---|---|
| `MEMQL_AI_OPENAI_IDENTITY_PROVIDER_ID` | the identity provider you register in step 2 |
| `MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID` | the service account you create in step 2 |

`MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE` is the third name and is **already set**
by `deploy/k8s/base` on every engine Deployment; you do not fill it in.

---

## The rule that decides everything: all three, or none

The engine reads the three values -- identity provider, service account, token
file -- as a set:

| What is set | What the engine does |
|---|---|
| all three | federates |
| none | leaves the OpenAI providers registered as unavailable |
| one or two | **refuses to boot**, naming which are missing |

The middle row is not an error. A cluster with no federated vendor boots,
serves everything that needs no model, and reports `ai` as unconfigured; that
is the normal state of a fresh cluster and of every local one.

The last row is deliberate, and there is no fourth row. The Anthropic switch
still has a "none" arm because a cluster may reasonably federate one vendor and
not the other; neither switch has a KEY arm any more. A half-configured
federation that quietly fell back to something would work in every test, boot
every node, and stop working the hour you finished step 3 -- on whichever node
nobody was watching. A refusal at boot is loud, immediate and attributable.

---

## The cutover

### Step 1 -- Confirm the cluster's OIDC issuer is publicly reachable

OpenAI fetches the issuer's JWKS over the public internet. A cluster whose
issuer cannot be reached that way, which is every local one, uploads its keys
instead: skip to [On a local cluster](#on-a-local-cluster).

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

INFO: on AKS the identity provider is registered by DISCOVERY -- OpenAI reads
the issuer document above. A cluster whose issuer is not reachable uploads its
keys instead, and uploads them again whenever its signing keys change. Prefer
a discoverable issuer where there is one.

### Step 2 -- Create the service account and the identity provider in the console

OpenAI Platform -> Settings -> Workload identity federation.

1. **Register the identity provider** for the issuer URL from step 1. Note its
   id: it is `MEMQL_AI_OPENAI_IDENTITY_PROVIDER_ID`.
2. **Create a service account** named `memql-engine` in the project your
   inference should be billed to. Note its id: it is
   `MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID`.
3. Add a derived attribute named `engine_workload` (the dashboard supplies
   the `openai.` prefix). Its CEL expression admits exactly the two engine
   subjects that make model calls:

   ```text
   assertion.sub == "system:serviceaccount:memql:memql-engine" || assertion.sub == "system:serviceaccount:memql:memql-engine-workbench"
   ```

4. Create or update the **one existing mapping** for this provider and OpenAI
   service account. Require `openai.engine_workload` = `true`, retaining the
   issuer, audience `https://api.openai.com/v1`, target account and permissions.
   Replace the former single-subject `sub` condition; leaving it alongside the
   new condition would still exclude the workbench. Keep any unrelated
   restrictions.

OpenAI converts boolean transformation results to mapping strings and allows
one mapping per provider/account pair. Do not create a competing mapping for
this same pair. [Official federation guide](https://developers.openai.com/api/docs/guides/workload-identity-federation).

The namespace is a deployment value: change both subjects if it is not
`memql`. Before rolling out the dedicated workbench identity, verify exchanges
for both subjects. The provider and target service-account IDs stay unchanged.

### Step 3 -- Put the ids in the overlay

Edit `deploy/k8s/overlays/cloud/kustomization.yaml` (or `cloud-entry`) and
replace the two `REPLACE-WITH-OPENAI-*` placeholders in the
`memql-openai-federation` ConfigMap patch.

Merge. ArgoCD reconciles and rolls the engine Deployments.

INFO: nothing under `deploy/` may name a real identity provider or service
account -- `TestTheCloudOverlaysCarryOpenAIFederationPlaceholders` fails the
build if the committed overlay carries anything but a placeholder. The real ids
belong in your install notes.

### Step 4 -- Verify

```bash
kubectl exec -n memql deploy/agent -- /app/memql provider-auth check --provider=openai
```

The command reports the sign-in the engine in that pod made when it started,
and waits up to its `--timeout` for one to be recorded. It contacts nobody
itself.

| Exit | Meaning |
|---|---|
| 0 | OpenAI accepted the engine's sign-in and listed its models with the bearer. Not merely that the config parses |
| 1 | OpenAI refused it. The reason is on stderr, in OpenAI's words |
| 4 | Cannot say yet: nothing is recorded (the node has only just started), or OpenAI did not answer. Run it again |

The ids printed are the ones the engine used, including ids applied through
Settings > Vendors. Run it against more than one node type; each pod holds its
own token, so "it works on agent" is not "it works".

In MemQL OS, **Settings > Vendors > Verify** asks the node that answers to
list OpenAI's models through its own client. It costs no tokens and no extra
sign-in.

Then watch the exchange counter across a refresh cycle or two:

```
memql_ai_federation_exchanges_total{vendor="openai",outcome="ok"}      # should tick up slowly
memql_ai_federation_exchanges_total{vendor="openai",outcome="denied"}  # must stay flat
```

A steady low `ok` rate is the healthy shape -- roughly one per bearer lifetime
per pod. **Alert on `denied`.** A denial does not break traffic immediately:
the last good bearer keeps working until it expires, so the outage arrives up to
an hour after the cause and looks unrelated to whatever was changed.

`scripts/install/verify-provider-key.sh` wraps the same check for the installer,
and takes the vendor:

```bash
scripts/install/verify-provider-key.sh \
  --provider=openai --federation-deploy=agent --namespace=memql
```

It has no key mode. Exit 3 means the check RAN and OpenAI refused -- go back to
step 2. Exit 5 means the check could not be run, or ran and could not say yet;
neither says anything about the credential.

### Step 5 -- There is nothing to remove

The Anthropic runbook has a "remove the key" step. This one does not, and the
difference is the point of the epic: the OpenAI key path was deleted from the
product in the same change that opened this door. There is no OpenAI API-key
env name to drop from a Secret, no key field in MemQL OS, and nothing left to
set.

If your `memql-secrets` still carries a key from before the cutover, it is inert
-- nothing reads it. Delete it for hygiene and **revoke it in the console**:
removing it from the Secret makes the cluster stop carrying it, not the key stop
working.

### Step 6 -- Record the ids

Put the two ids in the install notes for this cluster
([azure-entry-install.md](../azure-entry-install.md) lists them among the
per-cluster values). A re-created cloud cluster gets a new OIDC issuer, which
invalidates the identity provider and needs steps 1 to 3 again.

---

## Streaming transcription: what this runbook is verifying, not assuming

MemQL's streaming transcription (`AiTranscribeStream*`) reaches OpenAI's
Realtime API over a WebSocket, which sets its `Authorization` header once at
dial time. OpenAI's published documentation does not say whether a federated
bearer is accepted there.

So it is a **finding of this runbook, not an assumption of the design**. After
step 4, exercise transcription against the cluster and check the ASR client's
handshake status in the agent log. Two outcomes:

- **Accepted** -- nothing to do; the bearer source feeds the dial exactly as it
  feeds the HTTP clients.
- **Refused** -- the ASR client reports the handshake status and the audio
  websocket stays disabled. That is a recorded limitation, not a crash: the
  rest of the OpenAI surface keeps working on the same credential.

Record which one you got in your install notes. Whisper (the batch transcription
path) is an ordinary HTTPS call and carries the bearer the same way every other
request does.

---

## When it says no

The console's authentication events show a reason for every refusal, and the
engine logs the same string from OpenAI's response body.

**The log message names no vendor; the vendor is a FIELD.** One observer serves
both, so the line to look for is

```
WARN federation: token exchange DENIED -- the cluster is running on a
     credential the vendor will not renew   vendor=openai status=403
     vendorError="..." runbook=docs/public/operate/auth/openai-federation.md
```

Filter on `vendor=openai`, not on the message: a cluster federating with both
emits the same sentence for Anthropic, and grepping for a vendor name inside
the message finds nothing at all.

| Symptom | What it means | What to do |
|---|---|---|
| the subject does not match | the token's `sub` is not the value the mapping names | compare the subject `provider-auth check` reports against the mapping. A pod running as the `default` service account is the usual cause; so is installing into a namespace other than `memql` without updating the mapping |
| the audience does not match | the token was minted for a different audience | the projected volume's `audience` field; the render gate pins it to `https://api.openai.com/v1` |
| the issuer cannot be reached | OpenAI cannot fetch the cluster's JWKS | re-run step 1 from outside your network |
| the token has expired | clock skew, or a token older than the projected lifetime | check node clocks; the projected token is re-minted hourly |
| the service account is gone | the account was deleted or removed from the project | Platform -> Settings -> Service accounts |

Boot-time refusals are a different class and never reach OpenAI:

| Message | Cause |
|---|---|
| `HALF-CONFIGURED for OpenAI workload identity federation` | one or two of the three values are set. The message names both halves |
| `cannot read the projected identity token at ...` | the Deployment is missing the `openai-identity` volume or its mount |
| `does not carry the "https://api.openai.com/v1" audience` | the projected volume names a different audience |
| `sub=... is not a Kubernetes service account` | the token is not a projected service-account token |

---

## Separate Kubernetes identities

The workbench runs as `memql-engine-workbench`, admitted by the exact allowlist
above. Other inference nodes use `memql-engine`. **Identity** runs as `memql-deploy` -- the account holding the deploy console's Rollout and
Application grants (memql#4257). Moving identity onto `memql-engine` would
either strip that RBAC or put it on the account every engine node runs as,
handing the whole mesh a privilege one node needs.

Identity still carries the rest of the shape: the projected volume, the mount
and `MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE`. What it does NOT carry is a matching
subject -- its token says `system:serviceaccount:memql:memql-deploy`, which the
mapping does not name.

This costs nothing today: identity does not call OpenAI. If it ever needs to,
review its required authority and extend the explicit allowlist in the existing
mapping; do not change its Kubernetes identity to inherit another node's grants.
For the same reason identity does not sign in when it starts, as every other
node type does: the sign-in would be refused on every boot. `provider-auth
check` run against identity therefore reports that nothing is recorded and
exits 4. If identity ever needs OpenAI, remove its opt-out in
`app/build_identity.go` as well.

---

## On a local cluster

A local cluster can federate. What it cannot do is be fetched from: k3d's
issuer is `https://kubernetes.default.svc.cluster.local`, and its keys live
where no vendor can reach. OpenAI takes the keys **uploaded** instead, and
after that never contacts the cluster. An earlier version of this runbook said
no local cluster federates with either vendor; that was wrong.

1. Run `make federation-keys`. It prints the issuer, the public keys, the two
   engine subjects and the audience. It prints public material only.
2. In the console, register the identity provider for that issuer, choose
   uploaded keys for token verification and upload the `jwks` value from the
   result. Create the service account and the mapping exactly as in step 2 of
   the cutover.
3. In MemQL OS, open Settings > Vendors, choose **Set up OpenAI**, enter the
   ids and press **Apply**. A local cluster has no overlay to edit; the ids are
   stored in the cluster and every node reads them on Apply.
4. Verify as in step 4 of the cutover.

**The signing key.** `make up` keeps a local cluster's token signing key in
`~/.memql/k3d/<cluster>/service-account.key` and gives it back to the cluster
each time the cluster is created. `make up-refresh` therefore leaves the
issuer and its keys unchanged, and what you uploaded stays good. A cluster
created before this existed gets the file the next time it is recreated; run
`make federation-keys` and upload once more then. `make down PURGE=1` deletes
the file.

**That file is a secret.** Anyone who holds it can mint a token OpenAI accepts
for this cluster's mapping. It is readable by its owner only and is never
printed. Put the service account in a project with a low spend limit, and
remove the identity provider in the console when the cluster is gone for good.

A local cluster that has not been set up this way has no vendor, which is how
every cluster is installed:

- **Cloud models** are reached through a signed-in app on a fleet machine, or
  not at all. See [local-models.md](../local-models.md) for the machine-local
  option and the fleet route.
- **Streaming transcription and Whisper are off.** Both are OpenAI calls and
  both need a federated bearer.
- **Everything that needs no model still works**, which is most of the product.

The manifests are identical everywhere; only the two ids differ. See
[environment-parity.md](../environment-parity.md) -- credential source for a
vendor is an allowed value difference. CI installs with no vendor, so that is
the path CI exercises.

---

## What is deliberately NOT here

- **Scripting the console setup.** The Admin API could do steps 1 and 2, but the
  manual path has to be walked once before it is worth automating -- and a
  federated credential cannot reach the Admin API, so the automation would need
  a credential of a kind this epic exists to remove.
- **Per-project or per-workspace scoping beyond the service account.** The
  service account's project is what decides billing; there is no second dial.

---

## Related

- [anthropic-federation.md](anthropic-federation.md) -- the sibling vendor, the
  same shape, the same ServiceAccount.
- [access-model.md](access-model.md) -- how MemQL's own credentials work. This
  document is about a VENDOR credential and shares nothing with them.
- [env-vars.md](../env-vars.md) -- the three names and where they travel.
- [local-models.md](../local-models.md) -- what a cluster with no federated
  vendor can still do.
- [../../ai/llm-cost-control.md](../../ai/llm-cost-control.md) -- why the
  exchange is outside the LLM guard.
- [../../build/audio-streaming.md](../../build/audio-streaming.md) -- the
  transcription surface whose bearer this runbook decides.
- `component/memql/ai_openai_federation.go` -- the credential decision and the
  exchange.
- `component/memql/ai_vendor_signin.go` -- the one sign-in a process keeps per
  vendor, and the record `provider-auth check` reads.
- `scripts/k3d/federation-keys.sh` -- what a vendor is given to trust a cluster.
- `deploy/k8s/base/openai-federation.yaml` -- the ConfigMap seam.
