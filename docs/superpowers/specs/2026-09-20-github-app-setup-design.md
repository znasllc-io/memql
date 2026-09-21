# Registering the cluster's GitHub App from the product -- Design

- **Date:** 2026-09-20
- **Status:** approved (owner, in the Deployables session: "Yes, build the
  in-product GitHub setup"; the two HTTP routes in D4 were asked for and
  approved separately, by name)
- **Scope:** `component/identity/githubconnect/` (the manifest, the code
  exchange, resolution of the app's six values), `component/identity/` and
  `component/identity/http|web/` (the state's purpose, the two routes, the six
  rows), `integrations/identity/` and `dsl/identity/` (three capabilities),
  `component/packages/githubapp/` (a client whose app can arrive after boot),
  `component/inbound/` and `app/` (a webhook source the cluster registered for
  itself), `clients/os/` (the Repository step, Settings > Sources, the Set up
  group), the operator guide and the CLAUDE.md HTTP exceptions table.
- **Builds on:** [GitHub Connect](2026-09-03-github-connect-design.md)
  (decisions C1-C8). Nothing there is reopened; this removes its one
  prerequisite that only an operator could meet.

## Why

GitHub Connect needs the cluster to have a GitHub App: six values an operator
copies out of GitHub's settings pages into the deployment. On a cluster nobody
had done that for, a cluster owner pressing Connect GitHub was refused with
"ask an operator to set up the GitHub App" -- addressed to the one person
reading it who was the operator -- and what they had asked for was what the
product describes everywhere else: press a button, approve on GitHub, come back
and pick a repository.

GitHub has a flow for exactly this. A **GitHub App Manifest** registers an app
from a JSON document: a browser posts the manifest to github.com, the person
confirms the app's name, and GitHub redirects back with a code that is exchanged
once for the app id, slug, OAuth pair, webhook secret and private key. The six
values, with nobody copying anything.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| D1 | Where a registration's six values live | As ROWS, under the same six `MEMQL_GITHUB_APP_*` names: the three identifiers in `v1:platform:globalVariable`, the three credentials sealed under `MEMQL_MASTER_KEY` in `v1:platform:globalSecret`. Row ids are the seeder's (`var-global-<slug>`, `secret-global-<slug>`), so a second registration replaces the first |
| D2 | How the app is resolved | `githubconnect.Resolve`: the environment if ANY of the six is set there -- whole, never completed from rows -- else the rows, all six or none. Asked for per use through a `Resolver` with a ten-second memo; never read once at boot |
| D3 | The capabilities | `githubAppStatus` (any signed-in caller; never a credential), `githubAppSetupBegin` and `githubAppRemove` (cluster owners; refused when the environment owns the app) |
| D4 | The routes | Two, on identity, owner-approved: `GET /auth/github/app/new` (the page that posts the manifest) and `GET /auth/github/app/callback` (the code exchange) |
| D5 | The webhook | Active and subscribed to pushes when the cluster's domain could be reached by GitHub; registered OFF, at an inert placeholder address, when it could not. The bff admits the source from the stored webhook secret for a registered app only |

## D1 -- rows under the environment's own names

The alternative was a new concept for "the cluster's GitHub App". It was
rejected because everything that already reads these six values reads them by
NAME through a resolver that walks the environment and then the instance-wide
stores: the `githubApp` readiness module reports a registered cluster as set up
with no new code, and an operator looking at cluster configuration sees a
registered value in the same place as a seeded one.

The credentials are sealed in the frame that receives them. The plaintext goes
into no error, no log and no statement; what a statement carries is ciphertext
and a four-character fingerprint.

Six statements are not a transaction, and the order is the mitigation:
identifiers first, the private key last. Resolution needs all six, so an app
becomes visible only when the final write lands; a failure part-way reads as no
app, and a retry overwrites every row by id. Removal is the mirror -- private
key first -- and overwrites with blank, because the stores are versioned and
have no delete. That is stated to the owner where it matters: removing an app
does not take its credentials out of the store's history, and does not delete
the app at GitHub, which is the act that makes leaked credentials worthless.

## D2 -- the environment wins, whole

A cluster whose deployment sets some of the six is a cluster whose operator is
mid-setup, and the identity node refuses boot on it, naming both halves. Rows
must not quietly complete that set: the refusal exists so a Connect button never
fails per person, and a half-operator, half-registered app is the same defect
with a longer fuse. So any value in the environment makes the environment the
source, and begin, callback and remove all refuse
(`github_app_managed_by_environment`).

The identity node is filtered out of the mesh's configuration broadcasts, and
every node shares one database, so the resolver does not wait to be told: it
asks, memoizes for ten seconds, and is invalidated by the node that wrote.
`githubAppStatus` is the one reader that does not use the memo: a surface asks
it at exactly the moments ANOTHER process has just changed the answer -- coming
back from GitHub after the identity node stored a registration -- and, unlike a
fetch, asks once and believes what it hears. It reads fresh and leaves what it
read in the memo for the `githubConnectBegin` that follows. The packages client
keys its parsed private key and cached installation tokens on a fingerprint of
the app id and key, so a replaced app drops both.

## D3 -- three capabilities, and what none of them accepts

`githubAppSetupBegin {returnPath, organization}` answers `{startUrl, reason}`.
**The manifest is not an argument.** It is composed on the identity node from
the cluster's own domain, so nothing a browser sends can ask GitHub for a wider
app under this cluster's name, or move the callback every later authorization
is sent to. `organization` is validated against GitHub's login grammar before
anything is written, because it becomes a path segment of the URL a form posts
to. `returnPath` is a path, re-validated at every use.

`githubAppStatus` answers `{configured, source, slug, installUrl, canSetup}` so
a surface can know before it offers: Connect where there is an app, Set up
GitHub where there is none and the caller may register one, and nothing to
press otherwise.

## D4 -- two routes, and why there had to be any

The flow BEGINS with a browser form POST to github.com. MemQL OS forbids
cross-origin form posts by policy (`form-action 'self'`), and that policy is
deliberately site-agnostic -- a test forbids special-casing a site -- because the
edge serves every site the cluster hosts. Widening it would widen it for all of
them. So the post happens from one page on the identity service, under a policy
written for that page alone (`form-action 'self' https://github.com`).

`GET /auth/github/app/new?state=` validates a live `app_setup` state WITHOUT
consuming it, composes the manifest, and auto-submits; a guard stops the Back
button re-posting. `GET /auth/github/app/callback` is the OAuth-callback class
of exception, like its neighbour: bearer-less, authorized by a single-use state
resolved on any replica and consumed once under the Postgres advisory lock. It
checks more than its neighbour because it writes more:

- the state is spent FOR ITS PURPOSE. The state row carries `purpose`
  (`connect`, blank for every earlier row, or `app_setup`), checked inside the
  locked section, so a connect state -- which anybody who can press Connect can
  mint -- is one this route has never seen, and is left unspent;
- the person is STILL a cluster owner, read from their row now. Ten minutes is
  long enough to be demoted in, and this write outlives the session;
- the environment still does not manage the app;
- the app is the one the cluster asked for: what GitHub reports it created is
  checked against the manifest's permissions, and anything wider is refused and
  its credentials dropped. The owner can edit only the app's NAME on GitHub's
  page.

Then it keeps going. A registered app that is installed nowhere reaches no
repository, so a success does not return to the OS: it mints an ordinary connect
state for the same person and return path and sends the browser to the app's
installation page. GitHub installs, authorizes, and redirects to the existing
Connect callback. One press, and the owner lands back in the wizard with their
repositories listed. If that second state cannot be written the registration is
still done, and the OS is told `github_app_registered`, where Connect GitHub is
one press away.

The write happens under the identity service's owner-role system actor, with
`addedBy` naming the person. The one internal-origin stamp is the readiness
recompute that follows, inline on a single call, in a package the allowlist
already names.

## D5 -- the webhook, on a cluster GitHub cannot reach

The manifest registers `https://api.<domain>/inbound/github`, active, subscribed
to pushes, when the domain is one GitHub could plausibly deliver to. On a name
that can never resolve publicly the webhook is registered off. Its URL still has
to pass GitHub's validator, and whether that validator accepts a `.localhost`
name can only be learned inside an owner's signed-in GitHub session, on a page
where a refusal is something they cannot fix -- so the manifest does not depend
on the answer: the address is `https://example.com/inbound/github`, reserved by
RFC 2606, syntactically public and guaranteed inert. Nothing is delivered to it,
because with the webhook off nothing is delivered at all, and the ten-minute
poll notices pushes as it always has locally.

The reachability test is a guess from the name and says so. It errs toward
ACTIVE: a real domain on a private network gets failing deliveries where its
owner can see them while the poll goes on working; the other mistake is a
production cluster whose update cue is ten minutes late for ever with nothing
saying why.

GitHub generates the webhook secret while it creates the app, so no environment
ever held it. The inbound seam therefore gains a "registered source" tier --
after the environment's allowlist, before connectors, fail-closed -- and the bff
wires it for `github` only when the app's source is the cluster's own rows. An
operator who configured the app by hand also opts the receiver in by hand, and
this must not make that choice for them.

## What the surfaces do

The Repository step and Settings > Sources read the status as they open. Null
is "not known", and not known keeps the old offer: Connect, refused in place by
a cluster with no app. A status that ANSWERED "no app" replaces Connect with Set
up GitHub for a cluster owner -- one question, whose GitHub account the app is
registered under -- and for anybody else with a sentence naming who can, and no
act (absent, never disabled). In the wizard the act is on the floor, where every
forward act of that wizard is. Settings also carries, for cluster owners, the
app the cluster uses and Remove for one registered from the product.

The Set up group's `githubApp` row used to name six deployment variables. Its
lane is now `configurableFrom: os`, and the shell's module map names the app
whose settings configure it, so the row opens Deployables' Sources -- or, on
that very page, says "Below, under Sources".

## Refusal codes

`github_app_managed_by_environment`, `github_app_setup_forbidden`,
`github_app_setup_invalid`, `github_app_setup_state_invalid`,
`github_app_setup_failed`. Raised on the identity node and catalogued in
`component/packages/refusal.go`, where a parity test scans the identity raise
sites so a code cannot be answered without being catalogued, and the OS's own
parity test requires copy for each. Four are somebody's next step and render in
the warn tone; `github_app_setup_failed` is a fault.

## Not done here

- **Deleting the app at GitHub on Remove.** It needs the app's own JWT against
  an endpoint that deletes irreversibly, from a button in Settings. The guide
  tells the owner to do it there, and why.
- **Turning the webhook on when a cluster moves to a public domain.** A
  registered app keeps the webhook it was created with; the guide says how to
  change it at GitHub.
- **Verifying the trip against GitHub in CI.** The exchange is tested against a
  fake GitHub; the real page requires a person's signed-in session and their
  press of Create, which is theirs to make.
