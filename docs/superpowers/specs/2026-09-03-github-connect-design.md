# GitHub Connect -- Design

- **Date:** 2026-09-03
- **Status:** approved (owner Q&A in the Deployables brainstorm session; the
  two forks below record the choice and why)
- **Scope:** `component/identity/` (one callback route, a connect-state
  store, the GitHub App configuration), `dsl/platform/` (the grant kind on
  `sourceCredential`, the connect and picker capabilities),
  `component/packages/` (installation tokens; fetch, poll, probe and the
  webhook feed under a grant), `clients/os/` (the repository picker, prefill,
  the connected-account card), the operator docs and the CLAUDE.md HTTP
  exceptions table. One new HTTP route, approved by the owner.
- **The wave this belongs to:** an epic of
  [the Deployables program](2026-09-02-deployables-program-design.md),
  landing right after the Compose epic's engine PR and before its OS PRs,
  so the Source stop is built once with the picker as its default.

## Why

The Compose design connects a private repository by pasting a token into a
credential the person owns. The owner asked for a better default: a person
signs in to GitHub once, anywhere in the product, and from then on picks a
repository from a list instead of typing a URL, with the flow prefilled from
what the repository already says about itself. The pasted token stays as the
fallback for a self-hosted GitHub or an organisation that will not install
an app.

**It is a connection, not a sign-in.** What the flow needs is authority to
act on GitHub for the person: list their repositories, read a private one,
notice pushes. That is an OAuth authorization grant. The identity service's
federation decides who a person IS, is OIDC discovery-driven, and GitHub is
not an OIDC provider for user sign-in; building this as federation would
give no repository access and would need a bespoke provider in a subsystem
that deliberately has none. The identity node still HOSTS the callback,
because it already runs browser redirect flows with server-held state, has
the engine, and its hostname is where a redirect URI is registered.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| C1 | The mechanism | **A GitHub App**, not an OAuth App and not a token. Two token kinds (a user token for the person's own actions, an installation token the engine mints for fetch, poll and auto-deploy), fine-grained permissions requested once, one webhook for every installed repository, repository selection decided at GitHub |
| C2 | Where it lands | **Its own epic, right after Compose PR 1.** Compose's engine PR lands the owned credential row and the probe; this epic adds the app, the callback, the grant kind and the picker; Compose PR 3 builds the Source stop with the picker as the default and the token field behind it. Rejected: folding into Compose (a bigger engine PR, longer to the first green); after Logs (the Source stop rebuilt twice) |
| C3 | The route | **`GET /auth/github/callback` on the identity service is approved** as an HTTP exception of the OAuth-callback class: GitHub redirects a browser to it, so gRPC cannot serve it. Declared on identity's own route table beside `/auth/oidc/callback`; the front door regenerated; the CLAUDE.md exceptions table gains its row citing this approval |
| C4 | Starting the flow | **Over the stream, not over HTTP.** `githubConnectBegin` answers the authorize URL with a server-held state bound to the signed-in user; the browser navigates to it. The callback is the only HTTP surface |
| C5 | Who the grant belongs to | **The MemQL user, as an owned `sourceCredential` of kind `github_app`.** The Compose rules hold unchanged: the engine fetches under the package owner's grant and nothing else; a cluster owner deploying somebody's package fetches under that package's grant. There is no cluster-wide GitHub grant for sources |
| C6 | Background work | **Installation tokens**, minted by the engine from the app's private key, cached per installation until expiry. A poll, an auto-deploy or a webhook-driven fetch never depends on the person's user token being alive. The user token is refreshed server-side on use and never leaves the node |
| C7 | The fallback | **The pasted token stays**, behind "Use a token instead" on the Source stop, for a host that is not github.com, an organisation that refuses the app, or a person who prefers it. It is the same credential concept with `kind: token` |
| C8 | What is asked of GitHub | **Contents read and metadata read**, nothing else. A later capability that needs more (an agent opening a pull request) requests it as its own permission change, which GitHub surfaces to every installation for re-approval |

## A. The flow

1. **Connect.** From the Source stop or Settings, Connect GitHub calls
   `githubConnectBegin`. The identity service stores a state row bound to
   the caller's user id with a short TTL and answers the authorize URL for
   the app (`https://github.com/login/oauth/authorize` with the client id,
   the state and the redirect URI). The browser navigates there; the person
   authorizes, and installs the app on the account or organisation they
   choose, selecting all or some repositories.
2. **Callback.** GitHub redirects to `GET /auth/github/callback` with `code`
   and `state` (or `installation_id` and `setup_action`, the same route
   serving as the app's setup URL). The identity service resolves the state
   row on any replica, exchanges the code for a user token and refresh
   token, reads `GET /user` for the login and id and `GET /user/installations`
   for the installations the person can reach, seals the tokens with
   `secret.Encrypt`, and writes or updates one `sourceCredential` row of
   kind `github_app` owned by the state's user. It then redirects to the OS
   with a result marker. The state row is consumed exactly once.
3. **Pick.** The Source stop lists the repositories the grant can see
   (`GET /user/installations/{id}/repositories`, every installation,
   paginated), grouped by owner, with search, visibility, default branch and
   last push. An installation pending an organisation admin's approval is
   shown as pending, by name; "Install on another organisation" links to
   the app's installation page and returns through the same callback.
4. **Prefill.** Choosing a repository runs `sourceProbe` under the grant,
   which now also reads `memql-package.yaml` through the contents API and
   answers a manifest summary (name, each deployable's name, kind and path,
   the DSL domains) plus the branch list. The What-it-is stop shows the
   summary as a preview before Analyze; the ref picker offers the branches.
   Analyze runs the real analysis over the fetched snapshot as today.
5. **Fetch, poll, webhook.** The fetcher and the poll resolve the package
   owner's grant, find the installation covering the repository
   (`GET /repos/{owner}/{repo}/installation` with the app JWT, cached), mint
   an installation token and use it as the bearer. The app's single webhook
   posts to the existing `POST /inbound/github` seam under the app's webhook
   secret; the feed matches by repository URL as it does today and also
   reads `installation` events to keep a grant's installation ids current.
6. **Disconnect.** Settings shows "Connected to GitHub as @login" with the
   installations it can reach. Disconnect revokes the grant at GitHub
   (`DELETE /applications/{client_id}/grant`) and flips the row to revoked;
   the sources fetching under it show `reconnect_required` on their next
   fetch until reconnected.

## B. Configuration

| Variable | Meaning |
|---|---|
| `MEMQL_GITHUB_APP_ID` | The app's numeric id |
| `MEMQL_GITHUB_APP_SLUG` | The app's URL slug, for the installation link |
| `MEMQL_GITHUB_APP_CLIENT_ID` | The OAuth client id |
| `MEMQL_GITHUB_APP_CLIENT_SECRET` | The OAuth client secret |
| `MEMQL_GITHUB_APP_PRIVATE_KEY_B64` | The app's private key, base64, for installation tokens |
| `MEMQL_GITHUB_APP_WEBHOOK_SECRET` | The webhook secret, also the inbound source's HMAC key |

**All six or none.** A partial configuration REFUSES BOOT on the identity
node, the Anthropic-federation precedent: a Connect button that fails per
person is worse than no button. Unset, the Source stop offers the token path
alone and says why. The redirect URI to register at GitHub is
`https://identity.<domain>/auth/github/callback`, derived through
`component/frontdoor` like the OIDC one, never typed into a manifest. A
GitHub App accepts several callback URLs, so one registration may serve
several clusters; that is the operator's choice, and each cluster still
holds the six values.

## C. Data

`v1:platform:sourceCredential` (from the Compose design, section D) gains:

| Field | Notes |
|---|---|
| `kind` | `token` (the pasted path) or `github_app` |
| `login` | the GitHub login, for the card and the chip |
| `externalId` | the GitHub user id, the stable key a reconnect updates in place |
| `refreshToken` | `@secret`, sealed; never projected |
| `expiresAt` | the user token's expiry; refreshed on use |
| `installationIds` | the installations the grant can reach, updated at connect and on `installation` webhooks |

`encryptedValue` holds the user token for `github_app` exactly as it holds
the pasted token for `token`. The shape projects `kind`, `login`,
`installationIds` and the existing card fields, and never a token.

A connect-state row lives in the identity namespace with the user id, a
nonce, the return path and a TTL; consumed once under the same
compare-and-swap discipline the magic link uses, because a callback can be
replayed.

## D. Errors

Every outcome is a typed reason rendered in place: `reconnect_required` (a
401 from GitHub under a grant, never read as "private, or not there"),
`installation_pending` (named by organisation), `repository_not_installed`
(the grant exists but the app is not installed on that repository, with the
installation link), `github_app_not_configured` (the six values are absent,
so only the token path is offered), `connect_state_invalid` (an expired or
replayed callback). Refusal codes are added to `component/packages/refusal.go`
and the OS copy table.

## E. Security

- The state is bound to the signed-in user and consumed once; the callback
  writes only to that user's row.
- Tokens are sealed under the master key and decrypted only inside a fetch,
  a poll, a probe or a refresh, in a function-local, never logged; the
  reply to every capability carries login and fingerprint at most.
- Installation tokens are minted per call from the private key and cached
  in memory per installation until expiry; they never reach a row.
- The engine resolves a grant under the package owner's actor through the
  owned tier, exactly as the pasted credential; the Compose guarantee that
  no cluster-wide source credential exists is preserved.
- The webhook is HMAC-verified by the existing inbound seam; an
  `installation` event updates installation ids and nothing else.
- The audit log records connect, reconnect, disconnect and a refused
  callback, with the login as the target.

## F. Testing

- The callback against a fake GitHub token endpoint: a valid state, an
  expired one, a replayed one, a code exchange that fails, an installation
  setup landing; each writes or refuses exactly as stated, on any replica
  (an in-process two-replica case for the state store).
- Installation-token minting against a fake app endpoint; caching and
  expiry; a 401 producing `reconnect_required`.
- Fetch, poll and probe under a `github_app` grant and under a `token`
  credential, both green, both refusing a credential the package owner
  cannot read.
- The picker: grouped, searchable, pending installations named, the token
  path behind the fold; prefill from a manifest summary; screenshots at real
  size, both modes.
- Boot: five of six values refuses the identity node with the missing names.

## G. Out of scope

Sign-in with GitHub as an identity route; permissions beyond contents and
metadata read; GitHub Enterprise Server hosts (the token path covers a
self-hosted GitHub only if its API is github.com-compatible, and today it is
refused as `source_host_unsupported`); organisation-level policy management;
using the grant for the owner-only release cut, which keeps its cluster
token because it is a cluster act.
