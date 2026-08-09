---
title: Sign-in Paths
audience: public
status: stable
area: operate
sinceVersion: 0.15.0
owner: znas
---

# Sign-in Paths

There is more than one way to obtain a credential for a memQL cluster, and they
are not interchangeable. This page says what each path is FOR, when it is the
right one, what recovery looks like when mail does not work, and why a passkey
does not follow you from one cluster to another.

Everything here is stated against the shipped code. Where something is not
implemented, this page says so rather than describing the intent.

---

## The five paths at a glance

| Path | What it is | Who it is for | Where it ends up |
|---|---|---|---|
| **Magic link** | An emailed single-use link, 10-minute default TTL | Anyone with a working mailbox on a cluster that can send mail | OAuth auth code -> `/oauth/token` -> access + refresh token |
| **Passkey** | A WebAuthn discoverable credential, bound to this cluster's RP ID | A returning person on a cluster where they have already enrolled one | The same OAuth auth code a magic link produces |
| **Enrolment link** | A single-use `mql_enr_` URL that authorizes exactly one action: register a passkey as the named user | Someone who has no credential yet AND no mailbox that works here -- the local-install case | A passkey, which is then used to sign in |
| **Device code** | RFC 8628: a short code shown on one device, approved in a browser on another | A host that cannot bind a loopback port or cannot open a browser at all | Access + refresh token on the polling device |
| **PAT** | `mql_pat_` long-lived bearer, shown once at generation | CLI tooling talking to the **identity binary** | Nothing -- the PAT itself is the credential |

---

## Choosing between them

Work down this list and stop at the first line that is true.

1. **You are a person, in a browser, and you already have a passkey on this
   cluster.** Use the passkey. `/login` renders a "Sign in with a passkey"
   button next to the email form.
2. **You are a person, in a browser, and the cluster can send mail.** Use the
   magic link. It is the default path on `/login` and the universal recovery
   route (see [Recovery](#recovery-when-mail-does-not-work)).
3. **You are a person with no credential on a cluster that cannot send mail**
   -- typically a freshly installed local cluster. You need an **enrolment
   link**, minted by an owner/admin from the portal's People surface or by the
   install wizard.
4. **You are an editor or a CLI on a machine with a browser and a bindable
   loopback port.** Use the browser sign-in flow (the VS Code extension's
   **memQL: Sign In**). It runs the standard authorization-code + PKCE flow
   against a one-shot `127.0.0.1` listener.
5. **You are on a headless box, in a container, or behind a network where the
   browser cannot reach `127.0.0.1`.** Use the **device code** flow -- in the
   editor, **memQL: Sign In With a Device Code**. See
   [Reaching the device grant from the editor](#reaching-the-device-grant-from-the-editor).
6. **You are a script talking to the identity service itself.** Use a **PAT**.
   Note the hard limit below: a PAT does not authenticate against mesh nodes.

WARNING: a PAT (`mql_pat_...`) and a worker token (`mql_wkr_...`) are refused by
every non-identity node. PAT verification is a database lookup that only the
identity binary wires (`component/identity/verifier/verifier.go` returns
`PAT path not wired on this node` when no PAT verifier is present), so a valid
PAT fails against the bff exactly as a forged one does. To dial the bff you need
an identity-issued JWT access token.

---

## Magic link

The default, and the one every other path is measured against.

- Issued by `POST /auth/magic-link`, consumed by `GET /auth/complete`.
- Single-use, token-hashed at rest, 10-minute default TTL
  (`MEMQL_IDENTITY_MAGIC_LINK_TTL_SECONDS`, default 600).
- The row carries the in-flight OAuth context -- `clientId`, `redirectURI`,
  `state`, `codeChallenge`, `codeChallengeMethod` -- so consuming the link mints
  an auth code for the client that started the flow.
- `POST /auth/magic-link` is the one route wrapped in the anti-abuse stack:
  per-IP rate limit, Turnstile, disposable-domain blocklist, MX validation, risk
  scoring. See [identity-service.md](identity-service.md).

It needs one thing the other paths do not: **outbound mail**. With neither
Microsoft Graph nor SMTP configured, the `email` integration falls back to a log
sender, which writes the message body -- link included -- to the identity pod's
log instead of delivering it. That is exactly the situation the enrolment link
exists for, and it is also what the install wizard's `magicLink` step reads.

## Passkey

A passkey is the tenth variant of `v1:identity:identity`, and the only credential
family in that union whose stored material is public: the row carries a COSE
public key, and possession is proved by a signature over a server-issued
challenge.

**Registration** -- `POST /auth/webauthn/register/{begin,finish}`. Two
authorizations are accepted, and only these two:

- an authenticated user-class Bearer JWT (you already have a session and are
  adding an authenticator), or
- `Authorization: Enrolment mql_enr_<...>` -- an enrolment link, which is how a
  FIRST passkey is obtained with no session at all.

**Login** -- `POST /auth/webauthn/login/{begin,finish}`. Unauthenticated by
nature; this pair IS the authentication. It is **usernameless**: the challenge
carries an empty `allowCredentials`, because no email has been typed when the
button is pressed. That works only because registration mints credentials with
`residentKey=required` and `userVerification=required`, so the authenticator
holds the user handle itself. The assertion resolves to a row by credential id
alone, which is why a credential id already bound to another account is refused
cluster-wide at registration.

Properties worth knowing as an operator:

- **The RP ID comes from `MEMQL_IDENTITY_BASE_URL`, never from the request
  `Host` header.** A Host-derived RP ID would let anyone who can reach the
  service under another name have credentials minted for a domain they control.
  A base URL that yields no usable RP ID makes the passkey routes refuse rather
  than guess.
- Challenges are server-minted, single-use, ceremony-tagged (a registration
  challenge cannot be redeemed as a login assertion), and TTL'd at 5 minutes.
- The challenge store is **in-memory and therefore per-replica**. That is
  correct for identity as deployed -- a ceremony is two requests seconds apart --
  but it is the thing that has to move to the graph if identity is ever put
  behind a round-robin mid-ceremony.
- A **sign-count regression** is refused and audited as the cloned-authenticator
  signal. A counter that is zero on both sides is NOT that case: iCloud Keychain
  and Windows Hello never implement counters and report 0 forever.
- The login ceremony ends in the same OAuth auth code `/auth/complete` produces,
  PKCE binding intact. **No client learns which factor ran.**

The `/login` passkey button is a progressive enhancement twice over: the block
renders hidden and only `passkey-login.js` reveals it, and it renders at all
only when a relying party is in scope (`client_id` and `redirect_uri` both
present). With no client there is nothing for an assertion to produce, so the
magic-link form is the way in.

## Enrolment link

The credential that removes email from the critical path.

- Wire format `mql_enr_<43 base64url chars>` -- 32 CSPRNG bytes. SHA-256 hex at
  rest; the plaintext is returned to the issuer once, travels in the link, and
  is never persisted or logged.
- **Single-use**, marked by a `consumedAt` stamp. The stamp lands on the
  registration FINISH call, server-side -- so a reload or an abandoned tab does
  not burn a link that produced nothing.
- **15-minute default TTL**, clamped to a **24-hour ceiling**. A request above
  the ceiling is clamped, not refused.
- It authorizes **exactly one action**: register a passkey as the named user.
  That narrowness is what makes it safe to hand out as a plain bearer in a URL.
- Redeemed at `GET /enroll?code=...`, which validates the token and renders the
  registration page. HTTPS is required on both issue and redeem, the route is
  per-IP rate-limited, and every outcome -- including each refusal -- is audited
  with the source IP.

Four rejection states, each with its own message and its own error code, because
each asks the holder for a different next step:

| State | HTTP | Error code | Means |
|---|---|---|---|
| `invalid` | 401 | `enrolment_invalid` | No row matched -- a typo, a truncated link, or a token from another cluster |
| `expired` | 410 | `enrolment_expired` | The row aged past `expiresAt` before anyone used it |
| `already-used` | 409 | `enrolment_already_used` | `consumedAt` is stamped. This is where a replay lands |
| `revoked` | 403 | `enrolment_revoked` | The issuer killed it before it was used |

Two issuers, and the split matters:

- **An owner or admin, from the portal's People surface.** Rides
  `IdentityAdminMsg` on `MemqlService.Stream`; the gate lives in
  `component/identity/adminops`. The composed link must be `https` -- the issuer
  refuses to emit a plaintext one.
- **The install wizard**, via the `enrolmentLink` graph step, which runs
  `memql enrolment-token mint` inside the identity pod
  (`scripts/install/enrolment-link.sh`, `kubectl exec deploy/identity`). This is
  the only authority available at that moment: nothing can authenticate to a
  cluster whose owner has just been bootstrapped from env, and `/setup` is
  already gone (it 404s once any user exists, and the wizard's unattended
  bootstrap completes setup from env before `/setup` would ever render).

The CLI form takes `--user-id` or `--user-email`, an optional `--ttl`, and
refuses to emit an `http://` link unless `--allow-insecure` is passed.

## Device code

RFC 8628, for the case the loopback redirect cannot serve.

- `POST /device/code` mints two credentials: a machine-held
  `device_code` (`mql_dvc_<43>`, 32 random bytes) and a human-typed `user_code`
  rendered `XXXX-XXXX`. Neither plaintext is persisted -- only SHA-256 digests
  reach the engine.
- The `user_code` alphabet is `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` -- 32 symbols,
  8 characters, 40 bits. Every confusable pair is broken by REMOVING one member
  (no `0`/`O`, no `1`/`I`) rather than by aliasing on input. Input is accepted
  case-insensitively, with or without the separator.
- Default window **10 minutes** (`MEMQL_IDENTITY_DEVICE_CODE_TTL_SECONDS`,
  capped at 30 minutes). Default poll interval **5 seconds**.
- The human half is `GET`/`POST /device`, which **requires a signed-in user
  before it will resolve a code at all**, and shows the approver which
  application is asking, from which IP, calling itself what. A device flow that
  shows only the code is a phishing primitive.
- Redemption is a grant on `POST /oauth/token` with
  `grant_type=urn:ietf:params:oauth:grant-type:device_code`.

The five error codes the token endpoint returns, and what each means:

| Code | Meaning | Terminal |
|---|---|---|
| `authorization_pending` | The human has not answered yet | no |
| `slow_down` | You polled sooner than `interval`; the interval has been raised | no |
| `access_denied` | The human said no | yes |
| `expired_token` | The window closed | yes |
| `invalid_grant` | Unknown, replayed, or wrong `client_id` | yes |

INFO: `slow_down` is not advisory. The server raises the interval on the
persisted row (+5s per offence, capped at 60s) and judges every later poll
against the raised value, so a client that ignores the `interval` in the error
body ratchets itself to the ceiling.

If the node has no device-code store wired, `POST /device/code` answers
`temporarily_unavailable` and the grant refuses -- a binary without an engine
does not half-serve the flow.

## Personal access token (PAT)

- `mql_pat_<43 base64url chars>`, 32 random bytes. Shown once at generation;
  only the SHA-256 hex hash is stored, on a `v1:identity:identity` row of
  `identityType="api_key"`.
- Long-lived by design -- there is no TTL on the token itself.
- Issued from the identity web UI at `/me/tokens`.
- **Identity-binary only.** See the warning at the top of this page.

---

## The editor's sign-in

The VS Code extension exposes **memQL: Sign In** and **memQL: Sign Out** on each
cluster in the Clusters view (`memql.clusters.signIn` / `memql.clusters.signOut`),
plus **memQL: Sign In With a Device Code** in the palette.

The flow needs an **issuer**, which is a different fact from the endpoint the
stream dials -- `identity.<domain>` versus `cockpit.<domain>`. A cluster naming
neither an `issuer` nor a `domain` has nowhere to sign in to, which is why a dial
that fails on the credential offers a **Sign in** button only when both
`signInCanRecover` (the failure is one a fresh token fixes) and `canSignIn` (an
issuer exists) hold. The context-menu command itself is always present.

Where the resulting tokens live is a deliberate split:

| Credential | Stored in | Why |
|---|---|---|
| Access token | `~/.memql/clusters.yaml`, `token:` | Short-lived, and the memQL Cockpit reads the same file. A cluster the extension has signed in to but which carries no credential at all would read, to the Cockpit, as a cluster nobody has signed in to |
| Refresh token | VS Code `SecretStorage`, keyed per cluster | A 30-day credential has no business in a plaintext shared file. `clusters.yaml`'s `refresh_token:` key is an INGEST path only -- on the first successful exchange the token moves into `SecretStorage` and the plaintext key is deleted |
| Access-token expiry | `SecretStorage`, beside the refresh token | Redundant for today's JWT (`exp` is inside it); it exists so an opaque access token could still be renewed proactively |

`SecretStorage` cannot be enumerated -- there is `get`, `store` and `delete` and
no way to ask what keys exist -- so the store keeps its own index of the clusters
it has written for, and offers a rename-as-move and a reconcile sweep over it.
Neither of those two is called from the extension today; see
[Not implemented](#not-implemented).

### Reaching the device grant from the editor

**memQL: Sign In With a Device Code** (`memql.clusters.signInWithCode`, palette
only) runs the device grant deliberately, skipping loopback. Use it when you
already know this host cannot do a browser round trip -- otherwise you sit
through the callback deadline first only to be told so.

The extension also carries a fallback rule for deciding automatically. It is
**not currently reached from memQL: Sign In** (see
[Not implemented](#not-implemented)), but it is the rule to expect if you drive
`signInWithDeviceCodeFallback` yourself, and it is worth knowing because it says
which failures are environment limitations and which are refusals. It triggers
on **environment limitations only**, branching on the failure's `kind` and never
on message text:

- `bindFailed` -- the one-shot loopback listener could not bind `127.0.0.1:0`.
- `timeout` -- the listener bound and the browser was opened, but nothing came
  back. On a remote or firewalled host that is what "the browser could not reach
  `127.0.0.1`" looks like from inside the extension.
- `browserUnavailable` -- this host cannot open a browser at all.

It deliberately does **not** trigger on `cancelled` (the user stopped it),
`stateMismatch` (a security refusal -- retrying it through a second channel is
the bug the rule exists to prevent), `exchangeRejected`, `authorizationDenied`,
`invalidCallback`, `misconfigured` or `registrationFailed`.

---

## Recovery when mail does not work

**Where mail works, magic-link remains the universal recovery route.** It needs
no prior credential, no enrolled device and no second screen: type the address,
open the link. Every other path on this page assumes something you might have
lost.

**On a local cluster with no mail configured, recovery is the wizard re-minting
an enrolment link.** That is the intended answer, not a gap being papered over,
and the reason is worth stating plainly rather than leaving a reader to wonder
whether it is a hole:

> Minting an enrolment link on a local cluster requires the ability to run
> `kubectl exec` against the identity pod on that machine -- which is to say,
> filesystem and Docker access to the machine the cluster runs on. Anyone with
> that already has total authority over the cluster: they can read the database
> directly, edit the manifests, replace the images, or set
> `MEMQL_IDENTITY_ENABLED=false` and be admitted as the cluster owner. **The
> enrolment link grants nothing its holder did not already have.** What it buys
> is that the recovery is a supported, audited, single-use, 15-minute action
> instead of an improvised one.

Concretely, from the machine hosting the cluster:

```bash
kubectl exec -n memql deploy/identity -- \
  /app/memql enrolment-token mint --user-email owner@example.com
```

Open the printed link, create a passkey, and you are back in. The wizard's
`enrolmentLink` graph step is the same call, run for you.

Two things this recovery is NOT:

- It is **not** a remote recovery route. It deliberately requires local
  authority over the cluster, which is precisely why it is safe.
- It is **not** available to a person who has lost access to a cluster somebody
  else operates. There, the answer is a magic link if mail works, or an
  owner/admin issuing them an enrolment link from the portal's People surface.

The install wizard also retains the pod-log magic-link read as a documented
fallback (its `magicLink` graph step) for the case where the enrolment link
cannot be minted or opened.

---

## Per-cluster passkey semantics

**This is expected behaviour, and it is the first thing to explain to someone
who hits it.**

A passkey is bound to its **RP ID**, and this cluster derives that RP ID from
`MEMQL_IDENTITY_BASE_URL`. Two consequences follow directly, and neither is a
defect:

1. **A passkey does not work across clusters.** A credential enrolled against
   `identity.example.com` is not offered by the browser at
   `identity.other.example.com`, and could not be verified there if it were --
   the RP ID is hashed into the credential. Staging and production are different
   relying parties; so are two local installs on different domains. Enrol once
   per cluster you sign in to.
2. **Reinstalling a local cluster orphans its passkeys.** A reinstall creates a
   fresh database, so the `v1:identity:identity` rows carrying the public keys
   are gone. The authenticator still holds its half and the browser may still
   offer it, but the server has nothing to resolve the credential id against and
   the assertion is refused as unknown. The stale entry is harmless -- it is a
   public key nobody can use -- and is deleted from the operating system's or
   password manager's own passkey list, not from memQL, which ships no passkey
   management surface (see [Not implemented](#not-implemented)).

**The mitigation is structural: the install wizard mints a fresh enrolment link
on every install.** A reinstall therefore ends the way a first install does --
with a link that produces a working passkey for the new cluster -- rather than
with an operator wondering why the passkey they enrolled last week no longer
signs them in.

### What the RP ID actually is

It is the **hostname** of `MEMQL_IDENTITY_BASE_URL`, with any port stripped (a
port belongs to an origin, not to a domain). The expected ceremony origin is the
full `scheme://host[:port]`, which is what makes a `localhost` dev deployment on
a non-standard port work with no special case.

### Cross-device (phone / QR) enrolment on a local cluster: UNVERIFIED

Do not plan around scanning a QR code with a phone to enrol a passkey on a local
cluster. The question splits into two legs, and only one of them is settled
(memql#3405):

- **The browser's RP ID validator: settled.** A `.localhost`-family RP ID passes
  the HTML *is a registrable domain suffix of or is equal to* algorithm, both
  when the RP ID equals the origin's host and when it is a parent domain. That
  turns on `localhost` not appearing on the Public Suffix List, verified against
  the live list. Note the direction of that result: it passes *because nothing
  has been asserted about it*, which is a thinner guarantee than a spec
  carve-out.
- **The authenticators: not answered.** Whether the iOS and Android passkey
  providers accept such an RP ID over hybrid (cross-device) transport requires
  physical hardware and a person to scan a code and present a face or a finger.
  **That measurement has not been run.** The mechanism is sound in principle --
  in hybrid transport the desktop browser does all the network I/O and the phone
  only does cryptography, so the RP ID reaches it as an opaque string -- but
  "sound in principle" is exactly what the spike exists to stop us shipping on.
  The same is untested for a local `*.local.znas.io`-style domain, so this is
  not narrowly about `.localhost`.

So this page promises the **platform authenticator only**: Touch ID, Windows
Hello, or the screen lock on the device in front of you. That is a constraint on
what is PROMISED, not on what works -- the browser will still offer whatever it
supports, cross-device transport included, and a user for whom that works gets
it. The `/enroll` page's copy is written under the same constraint and names the
platform authenticator only.

---

## Not implemented

Stated here so nobody plans against it:

- **There is no passkey management surface.** `/me/devices` lists active
  *sessions*, not passkeys, and its "Sign out everywhere" revokes sessions. The
  DSL ships `createPasskeyIdentity` and `recordPasskeyAssertion` and **no
  passkey revoke mutation** -- unlike PATs, worker tokens, badges, node tokens
  and account tokens, which each have one. The login ceremony does refuse an
  inactive row, so the generic `updateIdentity` mutation could be used to flip
  one by hand, but nothing user-facing does. Listing, renaming and revoking
  passkeys is tracked separately (memql#3409).
- **There is no passkey-only account model.** A user row still carries a primary
  email; the enrolment link removes email from the sign-in path, not from the
  data model.
- **The editor's automatic device-code fallback is not reachable from Sign In.**
  `signInWithDeviceCodeFallback` is implemented and tested, but **memQL: Sign
  In** runs the loopback flow alone. The device grant is reached through the
  separate **memQL: Sign In With a Device Code** command, which skips loopback
  deliberately.
- **Renaming a signed-in cluster in the editor does not move its refresh
  token.** `renameClusterCredentials` and `reconcileClusterCredentials` are both
  implemented and neither is called, so the secret is orphaned under the old
  cluster name. Sign in again after a rename.
- **The editor's "+" Install / Repair actions do not run an installer.** They
  print the CLI command with a Copy Command button and say so.

---

## Related

- [Identity Service (Operator Guide)](identity-service.md) -- env vars,
  endpoints, key management.
- [Access Model](access-model.md) -- enforcement layers and the role spectrum.
- [User Provisioning](user-provisioning.md) -- registration modes and the
  magic-link flow.
- [Account Tokens](account-tokens.md), [Node JWT](node-jwt.md),
  [Service-account JWT](service-account-jwt.md),
  [Voice-agent JWT](voice-agent-jwt.md) -- the machine-credential families.
