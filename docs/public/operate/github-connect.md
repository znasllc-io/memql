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
token stays, behind "Use a token instead", for a host this is not, or an
organisation that will not install an app.

**It is a connection, not a sign-in.** Nothing here changes who a person IS in
this cluster. Connecting writes one `v1:platform:sourceCredential` row that the
person owns, exactly as pasting a token does; the identity model, the sign-in
routes and the role ladder are untouched. A cluster can use
[OIDC federation](auth/oidc-federation.md) for sign-in, GitHub Connect for
sources, both, or neither.

Unset, Connect is simply absent: the Source stop offers the token path alone and
says why.

---

## Creating the GitHub App

One app per cluster is the simple choice; one app across several clusters also
works, because a GitHub App accepts several callback URLs. See
[Sharing one app across clusters](#sharing-one-app-across-clusters) before you
decide.

At <https://github.com/settings/apps/new> (or your organisation's equivalent
under Settings -> Developer settings -> GitHub Apps):

| Field | Value |
|---|---|
| **GitHub App name** | Anything; it is what people see on the authorization screen. "MemQL on `<your domain>`" reads well |
| **Homepage URL** | `https://os.<domain>` |
| **Callback URL** | `https://identity.<domain>/auth/github/callback` |
| **Request user authorization (OAuth) during installation** | **checked** |
| **Setup URL** | `https://identity.<domain>/auth/github/callback` (the same route) |
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

The callback URL and the setup URL are the same route on purpose: it serves the
OAuth return and the post-install landing alike, and tells them apart by the
query GitHub sends.

Then, on the app's page:

1. **Generate a private key.** GitHub downloads a `.pem` once. Base64 it whole,
   newlines included: `base64 -w0 your-app.private-key.pem`.
2. **Note the App ID** (top of the page), the **Client ID**, and generate a
   **client secret**.
3. **Note the app's slug** -- the last path segment of its public page,
   `https://github.com/apps/<slug>`. It is how the product builds the "Install
   on another organisation" link.

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

## Organisation approval

A person who installs the app on an organisation they do not own does not get an
installation -- they get an installation REQUEST, and an owner of that
organisation has to approve it. Until then:

- the repository picker shows that organisation as a group with one sentence
  naming who has to act, rather than as an empty group or an error;
- a source pointed at one of its repositories refuses with
  `installation_pending`, naming the organisation.

There is nothing an operator can do about it from this side, which is exactly
why the surface names the organisation rather than apologising.

---

## What a person sees

**Settings -> Sources**, connected: "Connected to GitHub as @login", the
installations the grant reaches as chips, any pending ones marked, a link to
install on another organisation, and Disconnect.

**The Source stop of a new deployable**, connected: a searchable list of the
repositories that grant can reach, grouped by owner, each with its visibility,
default branch and last push. Choosing one runs the probe under the grant, fills
the ref picker with the repository's branches (default first), and previews what
the manifest says the package contains -- all before Analyze runs.

**Not connected, app configured:** Connect GitHub, with "Use a token instead"
folded below it.

**Not connected, no app:** the URL-and-token form is the whole stop, with the
sentence saying this cluster has no GitHub connection set up.

Every refusal renders in place, with the product's headline above and the
server's own sentence beneath:

| Code | What it means |
|---|---|
| `reconnect_required` | GitHub refused the grant itself -- the tokens are spent, or the person revoked the authorization at GitHub. One click to repair, and never read as "private, or not there" |
| `repository_not_installed` | The grant is good and the app is not installed on that repository. An installation link, not another credential |
| `installation_pending` | An organisation owner has not approved the installation yet, named by organisation |
| `github_app_not_configured` | The six values are absent. An operator's condition, and the surface says so rather than implying somebody mistyped something |
| `connect_state_invalid` | The connect link was expired, replayed, or never issued |

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
| Which installations the card shows | Stored on the grant, refreshed whenever the owner's own actor is present: connecting, reconnecting, returning from "Install on another organisation", opening the picker, or probing a repository |

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

**Webhooks do not**, because GitHub cannot reach a laptop. The polling fallback
covers it: every ten minutes, each repo-sourced package's upstream head is
compared against what is deployed. So an update cue appears within ten minutes
locally instead of within seconds. Nothing else differs.

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
