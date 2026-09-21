---
title: GitHub Connect -- the app, the grant, and the repository picker
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# GitHub Connect

**Audience:** operators setting a cluster up, and anyone answering "why does
Deployables ask me for a token".
**Design:** `docs/superpowers/specs/2026-09-03-github-connect-design.md`
(decisions C1-C8), and program decisions P10 and P11 in
`docs/superpowers/specs/2026-09-02-deployables-program-design.md`.

A person connects GitHub once, anywhere in the product, and from then on picks a
repository from a list instead of typing a URL and pasting a token. The pasted
token stays, as the other answer to the same question ("A token" beside
"GitHub"), for a host this is not, or an organization that will not install an
app.

**It is a connection, not a sign-in.** Nothing here changes who a person IS in
this cluster. Connecting writes one `v1:platform:sourceCredential` row that the
person owns, exactly as pasting a token does; the identity model, the sign-in
routes and the role ladder are untouched. A cluster can use
[OIDC federation](auth/oidc-federation.md) for sign-in, GitHub Connect for
sources, both, or neither.

Connect needs the cluster to have a **GitHub App**. A cluster owner registers one
from the product in a single trip to GitHub
([Registering the app from the product](#registering-the-app-from-the-product));
an operator can instead register one by hand and set it in the deployment
([Creating the GitHub App](#creating-the-github-app)). Until one of them has,
Connect is not offered: the surfaces say the cluster is not linked to GitHub yet,
who can change that, and offer the token path meanwhile.

---

## Registering the app from the product

**Who:** a cluster owner. **Where:** the Repository step of Add a deployable, or
Deployables -> Settings -> Sources. **What it takes:** one press here and one on
GitHub.

On a cluster with no GitHub App, those two surfaces do not offer Connect GitHub
-- it could not work. A cluster owner is offered **Set up GitHub** instead, with
the one question registering an app has: whose GitHub account it is registered
under, **your account** or **an organization** (named by its login; you must be
able to create apps for it). Everybody else is told a cluster owner sets it up,
and is offered the token path.

What happens when it is pressed:

1. The cluster mints a single-use setup state, bound to that owner, good for ten
   minutes, and sends the browser to a page on the identity service
   (`GET /auth/github/app/new`). That page exists for one reason: GitHub's
   manifest flow begins with a browser form POST to github.com, and both MemQL OS
   and the identity pages forbid cross-origin form posts by policy. It carries a
   policy of its own that allows a post to `github.com` and nothing else.
2. The page posts the cluster's **manifest** to GitHub. GitHub shows the app it is
   about to create and lets the owner edit one thing, its name. The default is
   "MemQL on `<your domain>`". App names are unique across all of GitHub, so on a
   domain many clusters share -- the local default above all -- GitHub may say
   the name is taken; change it on that page and carry on. The name is only what
   people read on the authorization screen.
3. GitHub sends the browser back (`GET /auth/github/app/callback`) with a
   one-time code. The cluster exchanges it, once, for the six values an operator
   would otherwise have copied out of a settings page, and stores them.
4. The browser is sent straight on to the new app's **installation page**, so the
   owner chooses which account and repositories it may read, authorizes, and
   lands back where they started -- connected, with their repositories listed.
   Registering and connecting are one trip.

**The manifest is the cluster's, and nothing a browser sends can change it.** It
is composed from `MEMQL_DOMAIN` on the identity node and is exactly the
registration the manual steps below describe: contents read and metadata read
(decision C8), authorization requested during installation, installable on any
account, the callback the cluster derives for itself. What comes back is checked
against it: an app carrying any permission the manifest did not ask for is
refused and its credentials are not kept.

**Where the six values live.** In the two instance-wide stores every node already
reads, under the same six names the deployment would use: the app id, slug and
client id as `v1:platform:globalVariable` rows, and the client secret, webhook
secret and private key as `v1:platform:globalSecret` rows sealed under
`MEMQL_MASTER_KEY`. No restart is needed; nodes notice within seconds. The
`githubApp` readiness module reads the same names and reports the cluster as set
up.

**The environment wins, whole.** If ANY `MEMQL_GITHUB_APP_*` value is set in the
deployment, the deployment owns the app: the stored rows are not read, not even
to fill a gap, and Set up GitHub is refused with
`github_app_managed_by_environment`. A half-set environment stays the boot
refusal it always was rather than being quietly completed from rows.

**The webhook.** On a domain GitHub can reach, the app is registered with its
webhook active, pointed at `https://api.<domain>/inbound/github` and subscribed
to pushes, and the bff admits that source using the app's own stored webhook
secret -- none of the `MEMQL_INBOUND_SOURCE_GITHUB_*` lines below are needed for
an app registered this way. On a name that can never resolve publicly
(`*.localhost`, `*.local`, `*.test`, `*.internal`, a bare host, an IP address)
the webhook is registered **off**, with an inert placeholder address, and the
ten-minute poll notices pushes instead. See [Locally](#locally).

**Removing it.** Deployables -> Settings -> Sources -> GitHub App -> Remove, for
an app registered from the product. It clears the six stored values, so every
GitHub connection on the cluster stops fetching until an app is set up again and
each person reconnects. It does **not** delete the app at GitHub, and earlier
versions of the sealed rows remain in the store's history like any rotated
secret. If you are removing an app because its credentials may have leaked,
**delete it at GitHub** -- that is what makes them worthless. An app set by the
deployment has no Remove; it is changed where the cluster is deployed.

Every registration and removal writes an audit event (category `configuration`)
naming who did it and the app's slug. No credential is ever logged, audited or
returned to a client: `githubAppStatus` answers whether there is an app, where
it came from and its slug, and nothing else.

If the trip fails part-way, nothing is half-kept on the cluster -- the stored
values are all six or none. GitHub may nevertheless have created the app before
the cluster failed to keep it, and app names are unique across GitHub, so delete
the leftover there before trying again.

---

## Creating the GitHub App

This is the manual path, for an operator who wants the app's credentials in the
deployment rather than in the cluster's own stores. An app made this way and one
registered from the product are the same app.

One app per cluster is the simple choice; one app across several clusters also
works, because a GitHub App accepts several callback URLs. See
[Sharing one app across clusters](#sharing-one-app-across-clusters) before you
decide.

At <https://github.com/settings/apps/new> (or your organization's equivalent
under Settings -> Developer settings -> GitHub Apps):

| Field | Value |
|---|---|
| **GitHub App name** | Anything; it is what people see on the authorization screen. "MemQL on `<your domain>`" reads well |
| **Homepage URL** | `https://os.<domain>` |
| **Callback URL** | `https://identity.<domain>/auth/github/callback` |
| **Request user authorization (OAuth) during installation** | **checked** |
| **Setup URL** | leave empty -- GitHub disables it once the box above is checked, and sends the post-install landing to the Callback URL instead |
| **Redirect on update** | checked |
| **Webhook -> Active** | checked |
| **Webhook URL** | `https://api.<domain>/inbound/github` |
| **Webhook secret** | generate one; it is `MEMQL_GITHUB_APP_WEBHOOK_SECRET` below |
| **Repository permissions -> Contents** | Read-only |
| **Repository permissions -> Metadata** | Read-only (GitHub selects this for you) |
| **Subscribe to events** | Push |
| **Where can this app be installed** | your choice |

Nothing else. **Contents read and metadata read is the whole ask** (decision
C8), which is what makes the authorization screen short enough to read. A later
capability that needs more -- an agent opening a pull request -- requests it as
its own permission change, which GitHub then surfaces to every installation for
re-approval.

There is one route on purpose. With authorization requested during installation
GitHub sends the post-install landing to the callback URL as well, and the route
tells the two apart by the query GitHub sends.

Then, on the app's page:

1. **Generate a private key.** GitHub downloads a `.pem` once. Base64 it whole,
   newlines included: `base64 -w0 your-app.private-key.pem`.
2. **Note the App ID** (top of the page), the **Client ID**, and generate a
   **client secret**.
3. **Note the app's slug** -- the last path segment of its public page,
   `https://github.com/apps/<slug>`. It is how the product builds the "Install
   on another organization" link.

---

## The six values

| Variable | Where it comes from |
|---|---|
| `MEMQL_GITHUB_APP_ID` | the app's numeric App ID |
| `MEMQL_GITHUB_APP_SLUG` | the last segment of `https://github.com/apps/<slug>` |
| `MEMQL_GITHUB_APP_CLIENT_ID` | the app's Client ID |
| `MEMQL_GITHUB_APP_CLIENT_SECRET` | a generated client secret |
| `MEMQL_GITHUB_APP_PRIVATE_KEY_B64` | `base64 -w0` of the downloaded `.pem` |
| `MEMQL_GITHUB_APP_WEBHOOK_SECRET` | the webhook secret you generated |

They travel as keys on the `memql-secrets` Secret, which every node type
`envFrom`s, so no Deployment changes.

**All six or none.** A partial configuration REFUSES BOOT on the identity node,
naming both halves -- which you have and which you lack -- because the operator
is mid-setup and needs to know both. It is the
[Anthropic federation](auth/anthropic-federation.md) precedent, for the same
reason: a Connect button that fails per person is worse than no button.

Locally, `make secrets` reads all six from your environment and seeds whatever
is set. If some but not all are present it warns, names the exact exports, and
seeds none of them -- so a half-set never crash-loops the identity node on a
laptop.

In the cloud they are six entries in `deploy/external-secrets/externalsecret-memql.yaml`
backed by six Key Vault secrets, following the `memql-x` naming that file
documents.

**The redirect URI is never typed into a manifest.** It is derived from
`MEMQL_IDENTITY_BASE_URL`, itself derived from `MEMQL_DOMAIN`, so a cluster
serving `lab.example.com` registers
`https://identity.lab.example.com/auth/github/callback` and nothing in `deploy/`
names a domain. Register at GitHub exactly what the derivation produces.

---

## The webhook

The app's single webhook posts to the existing inbound seam. It needs one more
line of configuration than the app itself, because the seam is deny-by-default:

```bash
MEMQL_INBOUND_SOURCE_ALLOWLIST=github
MEMQL_INBOUND_SOURCE_GITHUB_SIGNATURE_SCHEME=hmac-sha256-hex
MEMQL_INBOUND_SOURCE_GITHUB_SIGNATURE_HEADER=X-Hub-Signature-256
MEMQL_INBOUND_SOURCE_GITHUB_SIGNATURE_PREFIX=sha256=
MEMQL_INBOUND_SOURCE_GITHUB_SECRET=<the same value as MEMQL_GITHUB_APP_WEBHOOK_SECRET>
```

The secret is the same value twice on purpose: the app signs with it and the
receiver verifies with it. A source that is allowlisted but does not resolve to a
usable policy is dropped and answers 404 -- never admitted unverified. Full
reference: [inbound delivery](inbound-delivery.md).

The webhook carries **pushes**, which is what lights the update cue on a
deployable. It deliberately does not drive anything else; see
[what stays current, and how](#what-stays-current-and-how).

---

## Organization approval

A person who installs the app on an organization they do not own does not get an
installation -- they get an installation REQUEST, and an owner of that
organization has to approve it. Until then:

- the repository picker shows that organization as a group with one sentence
  naming who has to act, rather than as an empty group or an error;
- a source pointed at one of its repositories refuses with
  `installation_pending`, naming the organization.

There is nothing an operator can do about it from this side, which is exactly
why the surface names the organization rather than apologising.

---

## What a person sees

**Settings -> Sources**, connected: "Connected to GitHub as @login", the
installations the grant reaches as chips, any pending ones marked, a link to
install on another organization, and Disconnect.

**The Source stop of a new deployable**, connected: a searchable list of the
repositories that grant can reach, grouped by owner, each with its visibility,
default branch and last push. Choosing one runs the probe under the grant, fills
the ref picker with the repository's branches (default first), and previews what
the manifest says the package contains -- all before Analyze runs.

**Not connected, app configured:** what connecting is for, with Connect GitHub
as the step's forward act and "A token" as the other choice beside "GitHub".

**No app, cluster owner:** the sentence saying this cluster is not linked to
GitHub yet, the one question (your account, or an organization), and Set up
GitHub -- on the wizard's floor, or beside the question in Settings.

**No app, anybody else:** the same sentence naming who can change it, and no
act. The token path is one choice away.

Both are known before anybody presses anything: the surfaces ask
`githubAppStatus`, a read that writes nothing, as they open. A cluster that
cannot answer it keeps the older behaviour -- Connect is offered, the cluster
refuses it, and the URL-and-token form becomes the whole stop under the
sentence saying why.

Every refusal renders in place, with the product's headline above and the
server's own sentence beneath:

| Code | What it means |
|---|---|
| `reconnect_required` | GitHub refused the grant itself -- the tokens are spent, or the person revoked the authorization at GitHub. One click to repair, and never read as "private, or not there" |
| `repository_not_installed` | The grant is good and the app is not installed on that repository. An installation link, not another credential |
| `installation_pending` | An organization owner has not approved the installation yet, named by organization |
| `github_app_not_configured` | The cluster has no GitHub App. The cluster's condition, and the surface says so -- and who can change it -- rather than implying somebody mistyped something |
| `connect_state_invalid` | The connect link was expired, replayed, or never issued |
| `github_app_managed_by_environment` | The deployment sets `MEMQL_GITHUB_APP_*`, so the app is not registered or removed from the product |
| `github_app_setup_forbidden` | The caller is not a cluster owner -- or stopped being one between pressing Set up GitHub and GitHub sending them back |
| `github_app_setup_invalid` | The organization named is not something GitHub accepts as a login |
| `github_app_setup_state_invalid` | The setup link was expired, replayed, never issued, or belongs to the Connect flow |
| `github_app_setup_failed` | GitHub sent the owner back and the app could not be kept: the one-time code was refused, the app asked for more than the manifest did, or the values could not be stored. Nothing is half-kept |

---

## Disconnect

Disconnect revokes the authorization at GitHub
(`DELETE /applications/{client_id}/grant`) and flips the local row to revoked.
Sources fetching under it refuse with `reconnect_required` at their next fetch,
until the person reconnects or switches them to another credential.

**The row is never deleted.** It is the history of what fetched under it, and a
deleted row answers no question anybody asks after an incident. Reconnecting
updates the same row in place, keyed on GitHub's numeric user id -- so renaming a
GitHub account does not mint a second grant, and disconnecting and reconnecting
does not either.

A failure at GitHub does not block the local revoke. The person asked to
disconnect, and the local row is the thing that actually stops fetches.

---

## What stays current, and how

Three kinds of fact, three different mechanisms, and it is worth knowing which is
which when something looks stale.

| Fact | How it stays current |
|---|---|
| Which installation covers a repository, at fetch time | Asked live, per fetch, with the app's own JWT. Never cached on a row |
| Which repositories the picker offers | Asked live, per open, with the person's token |
| Which installations the card shows | Stored on the grant, refreshed whenever the owner's own actor is present: connecting, reconnecting, returning from "Install on another organization", opening the picker, or probing a repository |

The third is the only stored one, and it is deliberately not driven by the
`installation` webhook. A delivery names a GitHub identity and never a MemQL
user, so acting on it would mean reading across owners as a synthetic cluster
owner, over a body that arrives from outside the cluster -- in exchange for
noticing an uninstall performed elsewhere between one visit and the next. The
consequence of that staleness is one stale chip on a card, never a refused
fetch. Section G of the design record states the whole argument.

**Tokens.** A user token lasts eight hours and the engine refreshes it
server-side when a call needs one, so a person is not sent back through the
browser daily. The refresh token lasts six months; after that, reconnecting is
the repair and the fetch says so by name. Background work -- a poll, a
webhook-driven fetch, an auto-deploy -- runs on **installation tokens** minted
from the app's private key and cached in memory until they expire, so it never
depends on anybody's user token being alive. No token of any kind is ever
written to a row: what a row holds is the user token and the refresh token,
sealed under `MEMQL_MASTER_KEY`, projected by no client-readable shape and
unsealed only inside a fetch.

---

## Locally

The callback works: `identity.memql.localhost` is served over TLS by the local
front door, and GitHub will redirect a browser to it because the redirect
happens in the person's own browser, not from GitHub's network.

Registering the app from the product works too, for the same reason: every
redirect in that trip happens in the owner's own browser, and the one call the
cluster makes -- exchanging the code -- goes out to GitHub, not in from it.

**Webhooks do not**, because GitHub cannot reach a laptop. The polling fallback
covers it: every ten minutes, each repo-sourced package's upstream head is
compared against what is deployed. So an update cue appears within ten minutes
locally instead of within seconds. Nothing else differs. An app registered from
the product on such a domain is created with its webhook off for exactly this
reason; if the cluster later moves to a public domain, turn the webhook on in
the app's settings at GitHub and point it at `https://api.<domain>/inbound/github`.

---

## Sharing one app across clusters

A GitHub App accepts several callback URLs, so one registration can serve a
production cluster and a local one. Each cluster still holds its own six values,
and they are the same six values.

It is a real trade. One app means one authorization screen name, one webhook
secret, and one set of installations that every cluster sharing it can see -- so
a person who connects on the local cluster has connected the same GitHub account
to a grant on that cluster, with the same reach. Separate apps keep those
separate at the cost of a second registration. Neither is wrong; the shared one
is convenient for a development cluster and the separate one is right whenever
the clusters belong to different people.

---

## Related

- [Deployables](deployables.md) -- what a deployable is and how one is composed
- [Packages](packages.md) -- the source, the analysis and the pipeline
- [Inbound delivery](inbound-delivery.md) -- the webhook seam and its signatures
- [Environment variables](env-vars.md) -- where these six sit among the rest
- [OIDC federation](auth/oidc-federation.md) -- sign-in, which this is not
