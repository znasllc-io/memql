# Vendor accounts, API keys and the API route

- **Status:** written 2026-11-21. The owner gave the go-ahead to build it the same day, before
  reading it, so the calls listed at the end stand until the owner changes one.
- **Checked against:** `main` at 8290669a4 (2026-11-21). The first draft was read from a
  checkout 650 commits older; the files this record rests on were unchanged between the two.
- **Related:** design records 2026-08-22 (Anthropic workload identity federation),
  2026-09-06 (OpenAI federation and key removal, which this partly reverses),
  2026-09-23 (machine sharing grants), 2026-10-18 (routes) and 2026-10-31
  (deployer fleet); `docs/public/operate/ai-routing.md`,
  `docs/public/operate/auth/anthropic-federation.md`,
  `docs/public/operate/auth/openai-federation.md`,
  `docs/public/operate/shared-machines.md`, `docs/public/ai/llm-cost-control.md`.
- **Marking:** a decision marked **Owner** was made by the owner in the
  brainstorm of 2026-11-21. Every other decision is a proposal by the session
  that wrote this record. The ones that change what a person gets are listed
  again under [Calls to check](#calls-to-check).

## The problem

Fleet reaches a model three ways: a local model on a machine, an app on a
machine (Claude Code, Codex), and a metered vendor (Anthropic, OpenAI). The
third way is workload identity federation and nothing else, and it has four
gaps.

1. **It has never been run against a vendor.** Both federations were built from
   the vendors' documentation. Reading the code against that documentation again
   predicts that Anthropic refuses all but the first sign-in a node makes in each
   token rotation ([How it works today](#how-it-works-today)).
2. **It cannot be tested where development happens.** The runbooks, the root
   `CLAUDE.md` and Settings > Vendors all say a local cluster can never
   federate. Both vendors' documentation says otherwise: each accepts the
   cluster's public keys pasted in, and then never contacts the cluster.
3. **One account per vendor, chosen by whoever set up the cluster.** A person
   who holds another account at the same vendor (a personal one, a second
   organization) cannot use it. Manually entered keys were removed on
   2026-09-06 because a cluster-wide key in the environment was the wrong
   shape. The need that remains is a person's own key.
4. **Ask cannot ask for a vendor.** Its Route menu offers Auto, Local and Apps.
   The vendor sources exist in the engine, but the selector meant to pick the
   strongest vendor model picks by name order.

## What the owner asked for and decided

Asked for, 2026-11-21: polish and really test federation with both vendors,
locally if that can be done; add API keys back safely, as sealed and named
secrets, several per vendor; add an API route to Ask that covers both
federation and keys.

| | Owner's decision |
|---|---|
| O1 | A key is personal. It belongs to the person who adds it, and that person may share it. |
| O2 | Work the cluster runs as itself never spends a key. There is no cluster key: the cluster's own vendor account is federation. |
| O3 | Keys are checked automatically to confirm they still work, at minimal token cost. |
| O4 | Auto spends a key only after the key's owner switches that on, one switch per key. |
| O5 | A key is an account behind the vendor models, not a source of its own ([Approaches](#approaches)). |
| O6 | Every interface built for this follows the interface guidance already documented in the repository. |

## Goals, non-goals and anti-goals

**Goals**

- G1. Federation works against both vendors, on every node type that calls
  one, for as long as a pod lives. That is observed, not predicted.
- G2. Federation can be set up and tested on a local cluster.
- G3. A person can add, name, check, lend and remove API keys for Anthropic and
  OpenAI. The key's value is sealed on arrival and never leaves the engine.
- G4. A model call that reaches a vendor is paid by exactly one account, chosen
  by a stated rule, and its decision record says which.
- G5. Ask offers an API route that serves from vendors only, paid by the
  person's key or the cluster's account.
- G6. "The best vendor model for this job" is a real ranking, and the shipped
  vendor models are current.

**Non-goals**

- Other vendors, a custom base address, or an OpenAI-compatible server of
  someone's own.
- A cluster-wide key (O2).
- Keys paying for speech, transcription or embeddings (K17).
- A spending cap per key inside MemQL. A vendor's console caps a key, and
  every existing ceiling still applies (K14).
- Replacing a key's value in place. A new key is added and the old one removed.
- Showing a vendor's balance or bill.
- Changing how Auto ranks its chains.
- A cluster-wide table of every node's sign-in. Settings > Vendors shows the
  answering node's; each other node type is checked with `memql provider-auth
  check`, as the runbooks already say (A5).

**Anti-goals.** Any one of these makes the work a failure even if everything
above works.

- A key's value readable by anyone, in a reply, an event, a log line, a
  decision record, an error message or a message between nodes.
- A key paying for work its owner did not allow: the cluster's own work (O2),
  Auto without the switch (O4), a person it was not lent to, or work a
  visitor's click started.
- A vendor call whose record does not say which account paid.
- A check that spends tokens (O3).
- A second sign-in with one identity token, which is the failure being
  repaired.
- A screen outside the interface language (O6).

## How it works today

Read in code on 2026-11-21. Nothing in this section was run against a vendor.

- **One credential per model record.** Every vendor record in
  `dsl/providers/providers.memql` builds its own SDK client through one
  constructor per vendor (`newAnthropicClient` in
  `component/memql/ai_anthropic_federation.go`, `newOpenAIClient` in
  `component/memql/ai_providers.go`). An Anthropic client gets its own token
  cache from `option.WithFederationTokenProvider`; an OpenAI client gets its own
  `openaiExchanger` (`component/memql/ai_openai_federation.go`).
  `MemQLEngine.OpenAIBearer` builds one more exchanger for each transcription
  path wired at start-up (`app/integrations_stt.go`), `ReloadAIProviders`
  rebuilds every client, and `memql provider-auth check`
  (`component/memql/provider_auth_check.go`) forces a sign-in from a second
  process in the same pod.
- **The predicted failure.** Anthropic's documentation says an identity token
  that carries a `jti` is accepted once. Kubernetes projected tokens carry one,
  and a pod's token changes at 80 percent of its 3600 second lifetime
  (`expirationSeconds` in `deploy/k8s`). The four Claude records therefore make
  four sign-ins with one token, and only the first should be accepted until the
  token rotates, about 48 minutes later. OpenAI has the same one-per-record
  shape across its records and documents no replay rule.
- **Vendor availability is one answer for everybody.**
  `ProviderConfigEntry.Available` is set when the registry is built, and
  `routeReadiness` (`component/memql/routes_builtin.go`) reads one
  `doors.Federation` flag.
- **The per-person seam already exists for the other two ways.**
  `ProviderRegistry.EntryForUser` resolves `fleet:` and `app:` names against the
  acting person, and `Router.providerLookup` (`component/router/router.go`)
  binds per-call values onto a client through optional interfaces (`WithLevel`,
  `WithEffort`, `WithMinContextTokens`, `WithAppDoorPin`). Vendor clients
  implement none of them.
- **"Strongest vendor" is not a ranking.** `federationCandidates`
  (`component/router/selectors.go`) lists every registry record in name order,
  and `walkChain` takes the first that is available and serves the modality. No
  vendor record declares a level or a strength. The shipped models are a
  generation behind both vendors' current lists.
- **Routes.** Auto, Local and Apps are a table in `component/memql/routes.go`.
  Ask sends the choice as `route:<id>` in `AiChatMsg.provider`, and
  `routesForActor` reports each route's readiness and its reason.
- **A sealed personal secret already exists.** `v1:platform:sourceCredential`
  (`dsl/platform/concepts.memql`) is a person's token, sealed once on the node
  that received it by `component/secret.Encrypt`, read by its owner and (as
  metadata only) by a cluster owner, never deleted, and created by
  `sourceCredentialCreate`, which takes the value once in a `@secret` argument.
- **Lending already exists.** A machine is lent to named people and groups
  (`fleetSetSharing`, `component/worker/sharing.go`, design 2026-09-23). A
  cluster-wide share can no longer be created, a share never serves the
  cluster's own work, and lending to a person covers all of that person's work,
  automated or not (ruling of 2026-10-04).
- **A leftover.** `routerSetApiKey` (`dsl/common/builtins.memql`,
  `integrations/router/integration.go`) seals a key into a partition secret
  that nothing reads and no screen calls.
- **The gate.** `TestNoVendorApiKeyEntryPoint` (`vendor_api_key_gate_test.go`)
  bans the retired key variables and the retired key-setting builtin by name in
  every tracked file. This record does not spell them, so it needs no entry in
  that gate's allow-list.
- **Local clusters.** The engine does not refuse federation on a local cluster.
  Settings > Vendors does: it reads the domain (`localityOf` in
  `clients/os/src/apps/settings/providerFacts.ts`) and shows no form.

From the vendors' documentation, read 2026-11-21:

- Both accept a pasted set of public keys in place of fetching the issuer's,
  and compare the issuer as a string. Anthropic calls this inline keys; OpenAI
  calls it uploaded keys.
- Listing models sends no prompt. Neither vendor states a charge for it.
- Anthropic answers 401 for a malformed, revoked or expired key, 403 for a
  missing permission, 402 for billing, and 400 at an organization or workspace
  spend limit. OpenAI answers 401 for a deleted, deactivated or revoked key and
  also for a key lacking an endpoint's permission, and 429 with a quota code
  when credit or a spend limit is exhausted.
- Neither documents a balance endpoint for an ordinary key.
- At OpenAI, listing models is a separate permission from making model
  requests, so a restricted key may be unable to list models. The exact
  refusal is not documented.

## Approaches

Two ways remained after the weak ones were dropped. They differ in what a key
is to the router.

**A. A key is an account behind the vendor models. Picked by the owner (O5).**
Routing keeps choosing a model exactly as it does now. One step follows: which
account pays for this call. The API route then covers keys and federation with
nothing extra to set up; a route can pin one account on a vendor source; a
refused key is followed by the same model on the next account; and federation
is repaired in the same change, because it becomes one account per vendor
shared by every model. The cost is that the way the engine signs vendor calls
changes, and whether a vendor is available becomes a per-person answer.

**B. A key is a source of its own.** A key sits in the source list beside local
models and apps, and routes list keys by name. Nothing is implicit. But a key
is an account, not a model, so every key source also needs a model choice and
the source list grows by keys times models; the existing vendor sources would
not use keys, so no existing route gains from a new key; the built-in API route
still needs an "any of your keys" source with an ordering rule, which is A's
rule plus a new kind of source for the route editor, Auto and the rules; and
federation's repair stays a separate change.

**Dropped.** A private copy of the vendor's models per key: the registry is one
list built at start-up for everyone. A proxy service in front of the vendors: a
second place secrets live. Keys in the cluster's environment: O2 rules it out.

## The design

### Words

- **Account** (engine word): what pays for one call at a vendor. There are two
  kinds. The **cluster's account** is the cluster's federation with that
  vendor, at most one per vendor. An **API key** is a person's.
- Screens say "API key", the key's own name, and "the cluster's account". The
  engine keeps `federation:` for a vendor source in ids and DSL, and a routing
  screen still never renders that word
  (`clients/os/src/apps/fleet/routing/vocabulary.ts`).

### Part 1. Vendor accounts and the federation repair

| | Decision | Why |
|---|---|---|
| A1 | **One sign-in per vendor per process.** The engine holds one credential for the cluster's account at each vendor. Every client of that vendor uses it: chat, streaming, speech, transcription, embeddings, the transcription socket's bearer and Verify. A provider reload keeps it while the federation ids and token file are unchanged. | The failure is N sign-ins with one token. The invariant to test is: at most one sign-in per identity token per process, however many clients exist. |
| A2 | **The engine never presents an accepted identity token twice to Anthropic, and never retries a refused one on its own at either vendor.** The token file is re-read on every sign-in, as now. After a refusal the engine waits until the file's content has changed, keeps serving with the last good bearer while that is valid, and records the refusal once. One exception, because a refusal may be something the operator has just fixed at the vendor: pressing Verify, or Apply (`providersReload`, which already reaches every node), lets a node present its current token once more if its last sign-in was refused. Verify reaches the one node that answers it; Apply reaches all of them. **As built after the review of part 1:** a sign-in answered with 408, 425 or 429 is not a refusal. The vendor asked to be asked again, so it is retried after the short pause a transport fault gets and is not counted as a denial. | Retrying on a timer cannot succeed under a replay rule, and each retry is a refusal in the vendor's console. A deliberate press is one more try, not a loop. |
| A3 | **The identity token must rotate sooner than the vendor's access token expires.** `expirationSeconds` for Anthropic's token drops from 3600 to 600 in this change, so that token is replaced about every 8 minutes; the access token Anthropic mints keeps the 3600 seconds its federation rule names. OpenAI's token stays at 3600: OpenAI's runbook records that its bearer never outlives the token it was minted from, so a 600 second token would force a sign-in every 8 minutes with about 90 seconds between rotation and expiry, and OpenAI documents no replay rule. The first real sign-in at each vendor measures the access token's lifetime (the check already reports it); if Anthropic's is shorter than about 10 minutes, the fallback in A4 applies. The Anthropic render test that pins 3600 moves with it. | If the access token expired first, the engine would have to sign in again with a token it has already used. 600 is the lowest value Kubernetes accepts. |
| A4 | **A container that restarts inside a living pod presents the same token again, and loses the vendor for at most about 8 minutes.** The new process tries once. If it is refused and the token was issued before the process started, the record says the token may already have been used and when the next one is due, so it does not read as a broken configuration. **As built after the review of part 1:** that mark is made for Anthropic only (OpenAI has no replay rule), and it cannot tell a restarted container from a new pod's first token, which kubelet mints before the container starts. The same mark is made when the engine itself presented the token before and the vendor never answered: the retry may be the repeat. So the engine settles it itself: it waits until the token on disk says its replacement is due, looks at the file before presenting anything, looks again a bounded number of times if kubelet is late, and presents the next token with nobody calling a model. A rule that really is wrong is then reported as refused, and a node with no traffic does not stay "not verified". The same follow-up serves a node that is waiting for a token after an Apply changed its ids. The target is that one pod regains the vendor within 10 minutes of such a restart, which A3's 600 meets; its sibling replicas keep serving throughout. The real test restarts one container on purpose and records the time. The recorded fallback, not built here, is the engine requesting its own identity token for each sign-in, which needs a new permission for the engine's service accounts and is the owner's decision. | A new pod gets a new token; a crashed process in the same pod does not. Nothing in the repository covers this today. |
| A5 | **The engine is the only process that signs in.** When federation is set up for a vendor, each node that calls models signs in once at start, and again when the ids change, lists that vendor's models with the new bearer, and records the result: in memory, in the log, in the counter, and in a small file in the pod's temporary directory that holds the vendor, the outcome, the times and the vendor's reason, and never a credential. `memql provider-auth check` no longer signs in. It reports that file, the configuration and the identity token's claims, and waits up to its timeout for a record. **As built after the review of part 1,** the engine records every vendor, usable or not, because only it can see ids applied through Settings: an accepted sign-in with whether the vendor's API then took the bearer (the model list, or a served call), a refusal, a vendor nobody set up, ids the engine itself refuses (a partial set, an identity token it cannot use), and a node type that signs in to nothing. The check answers from that record alone: 0 verified (accepted and the API took the bearer), 1 refused (by the vendor, by its API, or by the engine), 3 not set up on this node, 4 cannot say yet. It used to exit 0 for a pod with no ids, which the installer read as "federation verified". `scripts/install/verify-provider-key.sh` keeps its own exit codes (0, 2, 3, 4, 5) and maps the check's 3 to its 3, with words that say the ids did not reach the pod. Settings > Vendors' Verify (`providerVerify`) keeps listing models through the running node's client. A 401 on one API call drops the bearer at Anthropic, where it means the credential, and not at OpenAI, where a missing endpoint permission is also a 401. The identity node is the exception: it calls no models and runs under a service account a vendor's rule does not admit, so it opts out of signing in at start (`app/build_identity.go`), and `make federation-keys` does not list its subject. | Two signers in one pod cannot both succeed under a replay rule. If the check signed in first, the engine would lose the vendor for that rotation; if the engine signed in first, the check would report a working federation as refused. |
| A6 | **A local cluster can federate.** Three changes, all built in part 1. A capability script, `scripts/k3d/federation-keys.sh` behind `make federation-keys`, prints the cluster's issuer, the subject and audience of each node type's token, and the public keys, for pasting at each vendor. Settings > Vendors offers its form on a local cluster too, with one sentence saying the vendor must be given the cluster's public keys and where they come from. The runbooks and the root `CLAUDE.md` lose every statement that a local cluster cannot federate and gain a local section. | Both vendors accept pasted public keys. The script prints public material only and works on any cluster, so it is not a second way to deploy. |
| A7 | **The signing key across `make up-refresh`.** Recreating the local cluster creates a new signing key, which makes the pasted keys stale. A throwaway spike, on a cluster under another name with no port mappings, decides between two outcomes. (i) The local cluster keeps its signing key in a file outside the repository, mode 0600, reused when the cluster is recreated and deleted by `make down PURGE=1` (and only by that: `make up-refresh` tears the cluster down with `--keep-signing-key`). (ii) Nothing is kept, and after a refresh the person runs `make federation-keys` and pastes again. The spike must cover `SERVERS=2`. (i) is preferred if it works on both shapes. **Result, 2026-11-22: (i) works.** On k3s v1.32.13 a key mounted at `/var/lib/rancher/k3s/server/tls/service.key` on every server gave an identical JWKS across delete and create, with one server and with two. | Whoever holds that file can mint tokens the vendor accepts for that rule, so (i) is a secret on a developer's disk. The runbook says so, and says to bind the vendor's rule to a workspace or project with a low spend limit. |
| A8 | **Availability is asked per person.** Built at the start of part 2 (see Order of work): in part 1 it would be a refactor with no behaviour to test. One resolver answers "which accounts can pay for this person at this vendor". Before keys exist it knows only the cluster's account. `ProviderRegistry.EntryForUser`, `routeReadiness` and `inferenceDoors` ask it instead of reading a flag set at start-up. | Part 2 adds keys to the same resolver. Nothing else in routing changes when it does. |
| A9 | **The real test is the acceptance for this part.** With both vendors set up, every node type that calls a vendor makes calls with every shipped record through at least three token rotations. `memql_ai_federation_exchanges_total` shows one sign-in per rotation per pod per vendor and no denial. A4's restart is exercised. The transcription socket is tried once with a federated bearer, and the OpenAI runbook records the answer to its open question. | G1. |

The vendor-console steps for A9 (the federation rule or identity provider, the
service account, pasting the keys) are the owner's. The session prepares the
exact values from `make federation-keys`.

### Part 2. API keys

| | Decision | Why |
|---|---|---|
| K1 | **Two rows per key.** The key row holds what its owner decides: vendor, name, the sealed value, whether it is the default, the Auto switch, sharing, and removal. A state row holds what the engine observes: the last check and its result, a hold-off time, and the last use. Only the owner's builtins write the first; only the engine writes the second. Proposed names: `v1:router:vendorKey` and `v1:router:vendorKeyState`. | The engine stamps state from any replica, often. If it wrote the row that holds sharing, a stale engine write could undo an owner's decision to stop lending. |
| K2 | **Sealed exactly as a source credential is.** `component/secret.Encrypt` under `MEMQL_MASTER_KEY`, on the node that received it, in a `@secret` field no client-readable shape projects, with a fingerprint of the last four characters for telling keys apart. Every write to either row requires internal origin, held by a write guard as the source credential's is (`component/memql/platform_source_credential_write_guard.go`). Both concepts declare `@belongsTo(cluster)`, as every router concept and the source credential do, and a `@secret` field never travels in an account export. | One pattern for a person's sealed secret. |
| K3 | **Anthropic and OpenAI only, at the vendor's own address.** A key is never sent anywhere but the vendor's public API host. Where a cluster overrides a vendor's base address, keys for that vendor are refused when added and passed over when resolved. | A key sent to an address somebody configured is a key given away. |
| K4 | **Who may add one.** A new part, `app:fleet/keys`: `read` shows the Fleet section, `execute` adds and changes the caller's own keys. Both are seeded on owner and developer, the people who can open Fleet. Anyone else needs a grant of the part and of Fleet. | The same people bring machines. A grant is the existing way to widen that. |
| K5 | **Who may do what to a key.** Its owner: everything. A cluster owner: read every key's facts (never its value), check it, and remove it. Nobody but its owner may use it, lend it, rename it, make it the default or switch Auto on. A person a key is lent to reads a short card of it: name, vendor, owner, state. | A cluster owner must be able to stop a key on their cluster, as with any credential there. Taking control of one would be spending someone else's money. |
| K6 | **Lending is a machine's lending.** The key row carries the same sharing block a machine does, without the cluster-wide mode: only me, or named people and groups, each of whom can be paused or removed. The picker is `fleetShareDirectory`'s. Admission is `worker.Sharing.Admits`, not a second rule. Lending covers all of the borrower's work, subject to K8. | One lending model. A cluster-wide share was withdrawn for machines and O2 forbids it here. |
| K7 | **One default per person per vendor.** A person's first key at a vendor is their default there; they can move the default. Their other keys at that vendor are used only by a route that pins them (R5). At most ten active keys per person per vendor, and a name is unique among a person's active keys. | "Which of my two accounts pays" must have an answer a person can state. |
| K8 | **Which account pays.** For a call resolved to a model at vendor V, in this order: (1) no acting person, or a synthetic identity (`auth.ActsForNoPerson`): the cluster's account only. (2) Work whose acting person was borrowed for a visitor (a shopper form, an access-code call, a deployed workflow's deployer): never a key. (3) A source that pins an account: that account only. (4) A route the person chose, or a model the person pinned themselves (`PinnedBy` is the acting person): their default key at V, then keys lent to them at V, the key that was added earliest first, then the cluster's account. (5) Anything else, which is Auto and every call nobody chose a route for: the same order, counting only keys whose Auto switch is on. | O2, O4, and the anti-goals. Choosing API or one's own route is the telling; Auto is not. |
| K9 | **An account is passed over** when it is removed, refused, held for credit, paused for this person, not lent to this person, or its owner is no longer an active user. The cluster's account is passed over when federation is not set up for V. When nothing remains, the model is unavailable for this person, and the decision record's `considered` says why. | The reason has to be readable afterwards. |
| K10 | **A failing account is followed by the next account for the same model.** When the vendor's answer is about the account (not signed in, no permission, no credit, a spend limit), the call moves to the next account in K8's order before the route moves to the next model. Any other failure follows today's fallback. The chain the fallback wrapper walks therefore carries model and account together. | The route chose the model. The account is only who pays. |
| K11 | **Checks cost no tokens (O3).** Adding a key lists the vendor's models with it before anything is saved; a refused key is not saved. Every real call is a check: success marks the key good, stamped at most once an hour. An account failure marks it refused, or held for credit for one hour, after which the next real call tries it again. A rate limit or a vendor outage is not a verdict on the key. Once a day, on the cron leader, any active key with no successful call or check in 24 hours has its models listed, refused keys included. Check now does the same on request. | Listing models sends no prompt. The daily pass is the cluster using a key, but it spends nothing, so it is consistent with O2. |
| K12 | **A key that cannot list models.** If OpenAI's refusal names a missing permission, the key is saved as "checked by use only", the wizard says so, and the daily pass skips it. Any other refusal means the key is not saved. The exact response is confirmed with a real restricted key during the build. | Otherwise a restricted key could never be added. |
| K13 | **One email when a good key goes bad.** When a key that was good becomes refused or held for credit, its owner gets one email with no action link, and an audit event is written. No second email until the key has been good in between. This needs a new notice on the existing `EngineEmailSender`. It is the last step of this part and can be cut without touching the rest. | A lent key that dies fails over to another account, and nobody would otherwise notice. |
| K14 | **A key's spend is metered spend.** `billing` stays `metered`. The process cost ceiling, the run ceilings and the loop caps count a key-paid call as they count the cluster's. A person's monthly budget counts the calls that person made, whoever's key paid. No new exemption and no per-key cap. | A person's money needs the protection the cluster's has. |
| K15 | **The decision record says who paid.** `v1:router:call` gains the account's kind (`cluster` or `key`), the key's id and the key owner's id. It never gains the key's name or fingerprint. A screen resolves the name through the key's own read, so a person who may not see the key reads "a key". A `fallback_used` row names the account that failed. | The call record is readable by every signed-in person. |
| K16 | **Removing is one-way, and the row stays.** A removed key is never unsealed again. The confirmation says to delete the key at the vendor too: removal here does not revoke it there, and the sealed value remains in the row's history. A key stops serving when its owner stops being an active user. | The source credential's rule. A deleted row answers no question after an incident. |
| K17 | **Text model calls only.** Speech, transcription and embeddings stay on the cluster's account in this version. | Embeddings bind the cluster's shared memory search to one model and must not follow a person's key. Speech and transcription are resolved outside routes today; putting them under K8 is its own piece of work. |
| K18 | **The leftover goes.** `routerSetApiKey`, its handler and its stale comments are deleted. Local checkouts of the four product repositories were searched on 2026-11-21 and none calls the builtin; the builder repeats the search on their current `main` first. If one does call it, it leaves through the DSL's deprecation window instead. | It seals a key nothing reads. |
| K19 | **The gate stays and its words change.** `TestNoVendorApiKeyEntryPoint` keeps its banned names: what it bans is still banned. Its header comment and failure message are rewritten to say what is true after this change: the cluster has no key, and a person's key is entered in Fleet and sealed. | The gate guards O2. Its present comment says no key field exists anywhere. |

Builtins, all on the stream and all `@sdk`. Names are proposals.

| Builtin | Does | Gate |
|---|---|---|
| `vendorKeyAdd { vendor, label, key @secret }` | Refuses a wrong shape and an organization admin key, lists models, seals, writes both rows, audits. Returns the card. | `execute app:fleet/keys` |
| `vendorKeyUpdate { keyId, label, isDefault, autoUse }` | The owner's settings. | owner of the row |
| `vendorKeySetSharing { keyId, mode, userIds, groupIds, pausedUserIds, pausedGroupIds }` | As `fleetSetSharing`, with no cluster-wide mode. | owner of the row |
| `vendorKeyCheck { keyId }` | K11's check on request. | owner of the row, or a cluster owner |
| `vendorKeyRemove { keyId }` | K16. | owner of the row, or a cluster owner |
| `vendorKeysForActor { }` | The caller's keys, and cards of keys lent to them. A cluster owner may ask for every key. | `read app:fleet/keys`; a borrower's cards need no part |
| `vendorKeyActivity { keyId }` | Calls and estimated cost this month, in total and per person it is lent to, from the call records. | owner of the row |

No HTTP endpoint is added. A credential of a machine class (`service_account`,
`app_session`) is refused by every builtin above that writes.

### Part 3. The API route

| | Decision | Why |
|---|---|---|
| R1 | **A vendor model declares its job and its rank.** Every shipped vendor text record declares the level or levels it serves and an integer rank, validated at load. A record that declares no level is passed over by both vendor selectors, and `considered` says so; it still resolves by name. | No record says today what it is for. |
| R2 | **The vendor selectors follow the job.** `federation:strongest` means: among vendor records that declare the call's level, have an account for this person (K8), hold the context floor and serve the modality, highest rank first, ties by name. `federation:cheapest` means the same set by declared price, unpriced last as now. | Today the first is name order and neither reads the level. Every shipped chain that names them, and Auto with them, gets the fix. |
| R3 | **The catalog is refreshed.** `dsl/providers/providers.memql` is brought to both vendors' current text models, with level, rank, price and context window taken from the vendors' documentation on the day of the change, never from memory. A record name that exists today keeps resolving while its vendor still serves the model, because product bundles pin record names. | G6. |
| R4 | **API is a fourth built-in route.** Id `api`, name "API". Its shipped job list is `federation:strongest` at every level. An owner or developer can edit it for the cluster, as Local and Apps are edited, and saving refuses any source that is not a vendor source. Search and memory still use the cluster's embedding model, as on every route. Ask sends `route:api`. | O5 and G5. A route named API that listed a local model would say something untrue. |
| R5 | **A person's own route can pin an account.** A vendor source in a personal route may name one account: the cluster's, one of the person's keys, or a key lent to them. Saving checks the person can use it. At call time an unusable pin passes the source over with its reason. Built-in routes and DSL policies cannot carry a pin: they are the cluster's, and a key is a person's. This is the last step of the part and can be cut without touching the rest. | It is how a second account at one vendor is used without changing the default. |
| R6 | **Readiness is the person's.** `routesForActor` reports API ready when, for each job, some vendor record declaring that level has an account for this person. Otherwise its reason is one short sentence, such as "Needs an API key or a vendor account". Each job's "serves now" names the model and the account. | An option that cannot serve says why in the option itself. |
| R7 | **Auto is unchanged except for who pays.** Auto still ranks its three shipped chains. Their vendor hop uses K8's Auto rule and gains R2's ranking. Every gate in front of a vendor today (a run's no-paid-providers ceiling, the cost ceiling, cloud consent where it applies) is asked as now, before any account is considered. | O4. A key is a paid provider. |

`routesForActor` rows gain the kind `api`. The OS drops a row whose kind it
does not know (`routeOptionsFrom` in `clients/os/src/ask/askRoute.ts`), so the
engine and the OS change together. Any other client that draws routes is
checked in the same change.

### Interface

O6 governs this section. Before any interface work the builder loads
`.agents/skills/memql-ui-design/SKILL.md`, which routes to
`clients/os/DESIGN.md` and `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`. What
follows names content, acts and states, and the existing pattern each surface
must reuse. Arrangement inside those patterns is the builder's, and a rule in
`DESIGN.md` wins over anything written here.

| | Surface | Content and pattern |
|---|---|---|
| U1 | **Fleet > API keys**, a new section | A section is a noun (`DESIGN.md`, "A tab is a different kind of thing"). The Head carries the title and the shared icon-only `AddButton`, named "Add an API key". Rows are `RecordList` and `RecordRow`: the key's name; quiet facts (vendor, fingerprint, how many it is lent to, estimated cost this month); the state in words, drawn only when the list mixes states. Keys lent to the person appear as rows with their lender as a fact and open a read-only page. A cluster owner reaches every key through `Refine`. Loading is `RecordListSkeleton`; an empty read and a failed read are different states. A long list loads its next page on scroll (`NextPage`). Money is written with `kit/format`'s `formatMoney`. No badges, chips or refresh button. |
| U2 | **Add an API key**, the shared `Wizard` | One question a step: Vendor (answered by choosing), Name, Key (a field that hides its value), Check (the cluster's turn, drawn as a wait). The forward act lives on the floor. A refusal lands in place in the vendor's own words and the pasted value stays in the field for another try. Nothing is written before the check passes, so Cancel is simply leaving. The typed value is never stored by the browser, never kept in a draft and never included in a client error report. |
| U3 | **A key's page** | Opens in place of the list, with the trail `API keys > <name>`. `Details`, `DetailsSection` and `SettingRow`, as a machine's Details and Settings do. Facts: vendor, fingerprint, added, last used, last checked and its result, estimated cost this month. Settings: name; default for its vendor, when the person has more than one; **Let Auto use this key**, a switch that is off for a new key; **Lend this key**, a switch with the Users and groups collection beneath it while on (`CardCollectionEditor`, a live call count, pause, resume and remove on each card, as machine sharing does); Remove, with its consequence and confirmation beside it (`InPlaceAsk`). The state and **Check now** stand in one `ActionBar` (rule 12). An act that is not legal is absent. |
| U4 | **Ask > Route** | API is an option after Apps, with a short neutral description in the option. When it cannot serve it stays visible and disabled with R6's reason (rule 5). Choosing it never opens a warning. |
| U5 | **Fleet > Routing** | API stands with the built-in routes and is edited like Local and Apps. A vendor source in a personal route can name its account (R5). History and a call's detail say which account paid, by the key's name where the reader may see the key and "a key" otherwise. |
| U6 | **Settings > Vendors** | Stays the home of the cluster's account, and no key is entered there. It offers its form on a local cluster with A6's sentence and shows the answering node's last sign-in result (A5), named as that node's. Every sentence saying no API key exists anywhere is corrected to say where a person's key goes. |
| U7 | **Fleet > Model library > Inference sources** | Its third line is worded for vendors, and says whether the cluster's account and a key of the reader's can serve. |

Words: "API", "API key", "the cluster's account". The retired routing words
stay retired on routing surfaces (`retiredWordsIn`). Estimated cost is always
labelled as MemQL's estimate from its price list, never as the vendor's bill,
and a record with no price reads "Not priced", never zero.

Acceptance is `DESIGN.md`'s: rendered results in light and dark, wide and
narrow, empty and populated, with keyboard, focus and reduced motion checked.
Credential-changing cases use fixtures. No real key is minted or pasted to
populate a review screen.

### Safety rules

| | Rule |
|---|---|
| S1 | A key's value crosses the wire once, inside the authenticated stream, in a `@secret` argument, and is sealed on the node that received it. No builtin returns it. |
| S2 | The value exists in memory only inside the call that uses it. Nothing caches it in the clear. |
| S3 | The sealed value never appears in a query result, a graph event, a log line, a decision record or a message between nodes. A test subscribes to the key concept's events as the owner, as a borrower and as a cluster owner, and asserts that the sealed field is in none of them. If the source credential's pattern cannot guarantee that for events, the sealed value moves to a row of its own that publishes none, as access codes do. |
| S4 | A vendor's error text is scrubbed of anything shaped like a key before it is logged, stored or shown. OpenAI echoes part of a rejected key. |
| S5 | A key reaches only the vendor's own address (K3), through the guarded client and its circuit breaker. |
| S6 | An organization admin key is refused when added, by its shape, and never stored. The builder confirms each vendor's shapes from its documentation. |
| S7 | Every add, removal, lend, pause, default change and Auto switch, and every change of a key from good to bad, writes an audit event. |
| S8 | A key never reaches a machine, an app session or a workbench. Vendor calls paid by a key are made by the cluster. |
| S9 | Use is decided per call from the rows (K8, K9). A call naming a key its acting person may not use resolves nothing, exactly as a made-up id does. |
| S10 | The file A7 may keep is a secret: outside the repository, mode 0600, ignored by git, removed by purge, and described as a secret in the runbook. |

### Across nodes

| | Rule |
|---|---|
| M1 | Any replica resolves accounts per call from the rows. Removal, a pause, a change of lending and a refused state take effect on every replica by the next call. The resolver's reads follow machine sharing's (rows per call, group membership cached for 30 seconds with broadcast invalidation), and the new concepts have the routing rules that carry their invalidation across the mesh. |
| M2 | The node that makes the vendor call unseals the key. A forwarded call (`AiForwardRouter`) carries the account's id, or the executing node resolves again; it never carries the value. Every node type that calls a vendor must hold `MEMQL_MASTER_KEY`, which the builder confirms in `deploy/k8s`. |
| M3 | State is written from any replica. A stale "good" from a call that started earlier must not overwrite a later "refused": the transition is stamped with the time of the evidence and older evidence is ignored. |
| M4 | The daily pass and K13's email run on the cron leader. The email is sent once per transition, under a Postgres advisory lock. |
| M5 | The federation credential is per process. Each pod signs in with its own token. |
| M6 | The tests exercise the hop: a call forwarded from the bff to an agent is paid by the right key, and a key removed through one replica is refused on the other. |

### What changes with the code

In the same changes, as the root `CLAUDE.md` requires of harness work:

- `docs/public/operate/auth/anthropic-federation.md` and
  `openai-federation.md`: the one-sign-in rule, A3 and A4 with measured values,
  Verify, the local section, and the answer to the transcription question.
- `docs/public/operate/ai-routing.md` and `docs/public/ai/llm-cost-control.md`:
  accounts, K8, K14, the API route, R1 and R2.
- A new `docs/public/operate/api-keys.md`, the sibling of `shared-machines.md`.
- `docs/public/operate/auth/access-model.md`: the new part.
- `docs/internal/ops/harness-decisions.md`: the account rule and its
  fallbacks, placed beside the routing section.
- The root `CLAUDE.md`: the AI Integration section, which says today that no
  manually entered key is left in the product and that a local cluster reaches
  neither vendor.
- `clients/os/DESIGN.md`, only if a kit piece is promoted.
- `make concept-snapshot`, the SDK generation gate, the conformance
  classification of every new query and mutation, and
  `TestOsRegistryRequiresMatchTheAppSeeds` for the new section.

### Tests and acceptance

- **Part 1.** A fake token endpoint that refuses a repeated `jti`: N clients
  and a provider reload make one sign-in; a refused sign-in is not retried
  until the file changes, except once on Verify or Apply; the last good bearer
  keeps serving; a restarted process's refusal is recorded as A4 says; the
  check command reports the engine's record and never signs in. The script's
  envelope and exit codes under the capability contract. Then A9 against the
  real vendors.
- **Part 2.** Each line of K8 and K9, including work with no acting person and
  work borrowed for a visitor. K10's order on each kind of account failure and
  on a rate limit. K11's transitions and the hourly stamp. K12 with a recorded
  restricted-key response. S3's event test. S4 with a response that echoes a
  key. The write guard on the key row. M3's ordering and M6's hop.
- **Part 3.** A level with and without a declaring record; ranks and ties; a
  record with no level; R4's save refusals; R5's save and call-time checks; R6
  for a person with a key and no federation, federation and no key, and
  neither.
- **Acceptance for the whole.** On a local cluster with no federation, a
  person adds a key, chooses API in Ask and gets an answer whose record names
  the key; Auto does not use the key until its switch is on; lending it lets a
  second person's API route use it and pausing stops that; a key revoked at
  the vendor is marked refused by the next call or the next check and its
  owner is told. With federation set up, a person with no key gets an answer
  on API paid by the cluster's account.

### Order of work

One project in three parts. Each part lands on `main` on its own with its
tests and documents.

1. **Vendor accounts** (A1 to A7, A9). The spike (A7) comes first. The code can
   land against the fake endpoint; A9 runs when the owner has set the vendors
   up, and parts 2 and 3 do not wait for it. A8 lands at the start of part 2,
   where keys give it something to answer.
2. **API keys** (K1 to K19, U1 to U3, U6, the safety and node rules). K13 last.
3. **The API route** (R1 to R7, U4, U5, U7). R5 last.

### Left to the builder

- Every concept, field, builtin and annotation name, and the DSL spelling of a
  record's level and rank.
- How a per-call account is attached in each SDK, and whether Anthropic's SDK
  shares one token source across clients or the engine owns the exchange as
  it does for OpenAI. A1's invariant is the test either way. Read in
  `anthropic-sdk-go` v1.79.1 (`internal/auth/middleware.go`), not run: the SDK
  keeps one token cache per option value and per `*http.Client`, so clients
  share a sign-in only when built from the same option and the same client,
  and the engine builds a new option and client for every record and another
  client for streaming. Its cache starts refreshing 120 seconds before expiry
  and retries a failed refresh every 5 seconds, which A2 has to stop. Its
  federation middleware stands aside for a request that already carries a key,
  which is one way a per-call key can ride the shared client.
- Whether the sealed value shares the key row or has its own (S3).
- The form a pinned account takes in a route's stored sources (R5).
- The model list, prices, context windows and ranks (R3), from the vendors'
  documentation at build time.
- The vendors' admin-key shapes (S6) and the restricted-key response (K12).
- The seams where an acting person is borrowed for a visitor, and whether
  each can reach a model at all (K8, line 2).
- Arrangement of each surface inside the documented patterns.

## Calls to check

Made by the session that wrote this record, not by the owner. Each can be
changed without disturbing O1 to O6.

1. **K4:** only owners and developers add keys by default; anyone else by a
   grant.
2. **K5:** a cluster owner can see, check and remove any key, and can never
   use or lend one.
3. **K7 and K8:** one default key per vendor; a person's other keys are used
   only where a route pins them; keys lent to a person come before the
   cluster's account.
4. **K8:** the Auto switch is the key owner's and covers everyone the key is
   lent to. A person the key is lent to cannot turn it on.
5. **K8:** work a visitor's click started never spends a key.
6. **K11:** the cluster lists a key's models once a day on its own. It spends
   nothing.
7. **K13:** one email to a key's owner when a good key goes bad.
8. **K14:** no spending cap per key inside MemQL.
9. **K17:** keys pay for text model calls only. Speech and transcription stay
   on the cluster's account for now.
10. **R4:** the API route may list vendor sources only, and an owner or
    developer can edit it for the cluster.
11. **R5:** a personal route can pin an account.
12. **A5:** `memql provider-auth check` never signs in; it reports the engine's
    own sign-in. Each node that calls models signs in once at start when a
    vendor is set up; the identity node does not.
13. **A7:** a spike decides whether a local cluster keeps its signing key on
    the developer's disk.
14. **U1:** keys live in a new Fleet section named API keys.
15. **A3 and A4:** Anthropic's identity token lifetime drops to 600 seconds, and a pod
    may lose the vendor for up to about 8 minutes after its container
    restarts. The engine minting its own tokens is recorded as a fallback and
    not built.
16. **A5:** Settings > Vendors shows only the answering node's sign-in, with
    no cluster-wide table.
17. **K8:** keys lent to a person are tried in the order the keys were added.

## Owner decisions after the independent review (2026-11-22)

The whole-branch review of part 2 left five things only the owner could
settle. He answered them one at a time. Where an answer changes a decision
above, the answer is what the code does, and the row above is the history.

1. **A coding agent's choice of route is not the person's.** A key pays for a
   route somebody chose only when that somebody is the person, in a signed-in
   session or with their own access token. An app session, a service account
   and work continued on a person's behalf are not, so they reach a key only
   through Auto, when its owner switched that on (`auth.ChoiceIsThePersons`).
   This confirms what the review's fix built.
2. **The cluster's own vendor account pays for the cluster's work, and for a
   person's calls only when that person is allowed.** The owner's rule is
   bring-your-own for everything: a connected cluster account is not a free
   pool for everybody on the cluster. The cluster owner is allowed from the
   start and can allow others. This narrows K8's last fallback. The
   permission is `execute app:settings/clusteraccount`, seeded on the owner
   role alone; the person judged is the one a call is for, from their stored
   role and grants; pictures, speech, transcription and images follow the same
   rule, and embeddings do not, because the memory space is the cluster's
   (`component/memql/vendor_account_cluster.go`).
3. **API keys live in Settings, and anybody signed in may add one** (changes K4
   and U1). His words: a key's owner uses it whatever their role, and must be
   able to manage it. Fleet opens for owners and developers only. So the
   section is Settings > API keys with no floor of its own, a person's own keys
   are listed whatever their role holds, and adding is `execute
   app:settings/apikeys`, seeded on every predefined role. An operator
   withholds adding from a person or a group with a deny.
4. **The record of a key-paid call that failed keeps who paid and the router's
   own words.** The vendor's sentence is about the key owner's account and the
   record is readable by every signed-in person. The key's owner reads the
   vendor's words on the key's page.
5. **A person's source credentials never change hands either.** The ownership
   transfer moved them like any owned row, so an heir fetched private sources
   as the person who left. They are held to the rule the keys are: the transfer
   skips them and the engine refuses the move for every writer. A package that
   was handed over stops fetching until its new owner selects a connection of
   their own.

## Part 3 as built (2026-11-22)

What the builder decided where this record left it open, and what was cut.

- **R5 is not built.** A personal route cannot name the account that pays for
  a vendor source. The record marks it as the step that can be cut; the rest
  of the part does not depend on it. Until it exists, a person's non-default
  key at a vendor is not spent.
- **R1's spelling.** A record declares `levels "<words>"` (one to three of
  fast, strong, reasoning) and `rank <1..1000>` in its `params`. Either on a
  record that is not a vendor's text model refuses the record at load.
- **R3, the catalog.** Read from both vendors' models, pricing and
  deprecations pages on 2026-11-22; nothing was run against either. Added:
  Claude Opus 5.5, Sonnet 5.5 and Haiku 5.5; GPT-5.6 Sol, Terra and Luna;
  GPT-5.5. Not added: Claude Fable 5.2, which costs twice Claude Opus 5.5 and
  would be the model a call gets with nobody having chosen it, and OpenAI's
  GPT-6 family, which takes no function calls on the one OpenAI API the engine
  speaks. Three names
  whose models OpenAI shut down are repointed to the replacement it names and
  declare no level. A record's `contextWindow` is the prompt size its one
  price is true for. Ranks across vendors are an order (the cheaper of two
  models of one tier first), not a claim about quality.
- **Structured output for the current Claude models.** Claude Opus 5.5 and
  Sonnet 5.5 refuse a forced tool choice, so their records ask by format
  (`structuredOutput "format"`), with the schema fitted to the keywords the
  vendor accepts. A limit it does not accept is asked for in the field's
  description and is not enforced. The format takes closed objects only: an
  object that takes any key is refused with the field named, and the route's
  next source serves.
- **R2's level is the level the call is served at.** The record says "the
  call's level". A rule's `@level` moves a call before its chain is walked,
  and the selectors read the level after that, so a call a rule raised to
  reasoning is offered reasoning records.
- **A declined reply is a failure.** A Claude model answers a request it
  declines with an ordinary response and the stop reason `refusal`. That is
  an error on every Claude record, and so is a structured reply by format cut
  off at the output limit: the route's next source serves, and the decision
  says the model declined. Found by the independent review; the first build
  recorded an empty answer as served.
- **R4's line.** API's description names the cluster's account only to a
  person it pays for (decision 2 above); to anybody else it reads "Vendor
  models, paid by an API key".
- **R6's sentence.** A third reason joins the two in the record: a person who
  has an account, and no vendor model for a job, is told the job "needs a
  vendor model your account can pay for".
- **U5.** Which account paid is said in a call's detail in History, not on the
  list row. A key the reader cannot list reads "an API key".

Decided by the owner after landing (2026-11-22):

- **A role scoped to one organization may hold the API keys part.** Such a
  role was barred from every part of Settings but Access, so decision 3
  ("everyone adds") was true of the five predefined roles only. The bar now
  exempts `app:settings/apikeys` and nothing else; the role still has to be
  given the part and opening Settings, by its author or by a grant. The
  cluster's vendor account stays barred to it.

- **An API key that can pay lets its holder past the setup screen.** On a
  cluster with no machine, no app and no vendor account, MemQL OS held
  everybody at "Set up this cluster", and somebody who was not an owner or
  developer could not even reach Settings to add a key. The owner's decision
  (2026-11-23), as built:
  - The screen holds only while the cluster's inference readiness is
    `unconfigured` AND the person's own `inferenceStatus.keyAccount` is not
    true: no key of theirs, or lent to them, can pay on a route they choose.
    The cluster's readiness is unchanged; a key lets its holder in, nobody
    else.
  - That answer is read when the screen would show, inside the bounded first
    wait, and again each time the screen would come back after a window
    closes. An unanswered or refused read keeps the screen: the cluster's
    verdict stands.
  - The screen offers "Add an API key", which opens Settings > API keys
    straight into the add wizard. For somebody who is not an owner or
    developer it is the act beside the one sentence they had; for an owner or
    developer it is a quiet line under the cluster's doors, saying a key lets
    them in alone. Somebody withheld from adding keys sees what they saw
    before.

- **The cluster's own work is said, never inferred.** Decision 2 was built on
  the absence of a person: a blank acting id counted as the cluster's work, so
  a path that lost its caller, and the anonymous reader, were paid by the
  cluster's account with no permission asked. Offered "find them first, then
  refuse", "refuse now" and "keep it", the owner chose the first (2026-11-23).
  As built:
  - A call is the cluster's own only when it says so: an id minted for the
    cluster's own actors (`system:`, `cluster:`, a connector), or a call that
    names nobody under the cluster's own actor or a job wired as the
    cluster's (`auth.ContextForClusterWork`). A call that names a person is
    that person's, whatever its context carries, a job's stamp included.
    Anything else is refused the cluster's account, with "the call does not
    say whom it is for" on its record. Who a call is for is what its request
    names: the seams that build a request fill it from the context and the
    run's owner first, and the rule does not read a person off the context
    again (that would offer the cluster's account while leaving the person's
    own keys out).
  - The sweep found these paths naming nobody, and each now says whom it is
    for: Ask's suggestions answered on a BFF with no agent replica, a
    transcription opened on the speech node and an agent turn on a direct
    stream (each takes its stream's caller); the command-safety check,
    resolved once at start for nobody (a named job now, at the resolution
    and on every call, so the cluster keeps paying for it as it did; whether
    it should is a separate question still open with the owner), and the
    healer, written the same way though nothing wires it today; and a request
    built outside the engine inside a run (the router, and its cache-hit
    record, now read the run's owner, as the engine seam did).
  - A machine lent to cluster work may still serve a call that names nobody:
    the fleet reads a blank acting id as cluster work. Who lent what is not
    this decision; the vendor account is.
  - Left naming nobody, and so refused: the planner's responsibility intake,
    which nothing subscribes, and the context-free `Router.Resolve*`
    methods, which nothing in production calls.
  - A job's name rather than the system actor, because the fleet reads the
    acting id to choose machines: the system actor's `system:` id would have
    taken the job off the machines lent to cluster work.

Left open by the independent review, for the owner:

- **A route with one vendor selector at every level does not degrade inside
  itself.** The walk skips a level whose list equals one already walked,
  which was sound before a selector read the level. API ships that shape.
  With the shipped catalog every level has records at both vendors, so
  nothing changes today.
