# Magic-link hardening -- device-bound links, approve-on-click, and what a shared mailbox can do

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project A of nine)
**Owner:** identity service (`component/identity`)

Sub-project A of the 2026-08-22 backlog brief. The brief's other sub-projects
(subscription row-authz, portal chrome pass 2, audit noise, Anthropic WIF,
artifacts and deployables, the fleet section, MCP delegation, Nexus) each get
their own design; this one stands alone, with one named hand-off to the portal
chrome work (section 7.2).

---

## 1. Problem

A MemQL account can be registered with a group alias -- `team@example.com`,
fanning out to several people. Member A requests a magic link from computer A.
Member B also receives it and clicks first, on computer B.

Read against the code at `d426999b`, the outcome depends on where A started:

| A started at | What B's click does | What A sees |
|---|---|---|
| The portal (`/authorize`, PKCE S256) | Consumes the link; lands on the portal callback with an auth code B's browser cannot redeem (`clients/portal/src/auth/AuthProvider.tsx:313-320`). B is locked out. | "This sign-in link has already been used." A is locked out too. |
| `identity.<domain>/login`, or a `/me/*` page that bounced A there on session expiry | `GET /auth/complete` mints a first-party `memql_admin` cookie on **B's** machine (`component/identity/http/complete.go:87-111`, `:230-237`) -- no PKCE, no device check. From there B reaches `/me/devices` and can enrol **their own passkey** (`web/me_passkeys.go:106`; `http/webauthn_register.go:520-545` accepts any user-class bearer), and the `/authorize` fast path mints B a full 30-day portal session against B's own PKCE pair (`web/redirect_authenticated.go:162-187`). | Same "already used" page. Nobody is notified. A cannot see or revoke B's session. |

The second row is the one that matters. Three properties of the current flow
make it possible:

1. **Nothing binds the link to the device that asked for it.** The request row
   records `sourceIP` and `userAgent` (`magiclink/issuer.go:305-315`,
   `store.go:128-148`) and never compares them. No cookie or nonce is set on the
   requesting browser.
2. **The click consumes on GET, before any interaction** (`magiclink/verifier.go:126`),
   read-then-write rather than compare-and-swap (the race is acknowledged at
   `verifier.go:119-125`). Outlook SafeLinks, Gmail's proxy, and mail-security
   appliances burn links the same way a human does. The confirmation page that
   would prevent this exists as a template (`web/templ/landing.templ`, "opened
   this sign-in link in a different browser") and is listed as CSRF-exempt at
   `web/server.go:320-328`, but no route mounts it and no Go code calls it.
3. **The browser-session path has no PKCE and leaves no trace.** `AdminSession`
   is set whenever no registered OAuth client matched (`web/handlers.go:229`),
   the challenge is only forwarded when one did (`:231-236`), and
   `startBrowserSession` issues an access token with no `SessionId`
   (`complete.go:216-225`), so no `authSession` row exists for it -- even though
   `authSession.source` already declares an `oidc_cookie` variant for precisely
   this session (`dsl/identity/concepts.memql:167`). The "Active sessions" panel
   on `/me/devices` is a permanent spinner, and `POST /me/devices/revoke-all` is
   not mounted.

Two adjacent facts the design also has to own: `/oauth/token` only verifies
PKCE when the code carries a challenge and accepts `plain`
(`http/token.go:141`, `:334-338`); and the abuse stack (Turnstile, per-IP
velocity, disposable-domain and MX checks) wraps only `POST /auth/magic-link`
(`http/server.go:220-225`) -- the web `POST /login` path calls the issuer
directly (`app/integrations_identity.go:210-239`).

**The honest framing.** The hijack race is fixable; the shared mailbox is not.
Anyone who can read `team@example.com` can request their *own* link. Device
binding stops B from riding A's link. It does not stop B from signing in as the
alias. That second problem needs a policy and a second factor, which is why
this design has two halves.

---

## 2. What the tree already has

The design leans on existing machinery wherever it can. These are the pieces,
and the constraints they impose.

### 2.1 The request row already carries most of the state

`v1:identity:magicLinkRequest` (`dsl/identity/concepts.memql:430`) has
`email`, `tokenHash`, `expiresAt`, `consumedAt`, `consumedFromIP`,
`oauthCtxJSON`, `sourceIP`, `userAgent`, `invitationId`. It gains four fields
(section 4.1). Every identity replica reads it from Postgres, so an approval
recorded by one pod is visible to the poll served by another.

### 2.2 `/check-email` is a page we own

`GET /check-email` is server-rendered (`web/server.go:374`,
`web/handlers.go:390-406`, `web/templ/check_email.templ`). Both issue paths
redirect there (`handlers.go:280`, `:675`). It is the natural home for the
poller: the requesting browser is sitting on it while the email is read.

### 2.3 PKCE is mandatory at `/authorize` and conditional at `/oauth/token`

`/authorize` requires `code_challenge` + `code_challenge_method=S256`
(`web/authorize.go:77-81`); the portal and the VS Code extension always enter
there. `/oauth/token` verifies a challenge only if the code has one
(`token.go:141`). Making it unconditional breaks only a client that linked
straight to `/login` -- which, pre-release, is the right thing to break.

### 2.4 Sessions come in two kinds, and only one is recorded

`issueSessionForUser` creates an `authSession` row (`http/token_session.go:105-120`);
`startBrowserSession` does not. The gRPC revoke handlers already exist
(`component/grpc/auth_session_handlers.go:192` revoke-current, `:259`
revoke-all) and operate on rows, so a browser session that has a row becomes
revocable for free.

### 2.5 Passkeys, enrolment tokens and recovery keys already exist

Registration (#3406), usernameless login (#3407), enrolment tokens (#3408),
management on `/me/devices` (#3409) and the owner recovery key (#3958) are all
shipped. The passkey-only switch in section 6 is a policy over an existing
factor, not a new factor. Enrolment and invitation links are shown to the
issuing admin and never emailed (`adminops/enrolment.go:106-113`), so they do
not have the alias exposure -- unless a human pastes one into a mail to the
alias, in which case the consequence is a permanent credential for any
recipient. This design leaves them alone and says so in the docs.

### 2.6 The email sender sends exactly two messages

`emailsender/sender.go:48-104`: the magic link and the bootstrap claim. The
magic-link body says "If you did not request this link, you can safely ignore
this email" (`:137`). There is no "new sign-in" message of any kind.

### 2.7 Audit rows carry device facts, and the user cannot read them

`magic_link_issued`, `magic_link_consumed`, `admin_session_started`,
`session_created` all carry `SourceIP` + `UserAgent` (`audit.go:50-51`,
`audit_db.go:72-73`). The audit queries require owner/admin
(`dsl/identity/queries.memql`, `auditEventsByActor`), so a non-admin user
cannot see a sign-in they did not make. Visibility has to reach the user some
other way -- section 7.

### 2.8 Two identity replicas in the cloud

`deploy/k8s/base/identity.yaml` runs two replicas behind one front door. Any
state the new flow introduces lives on the request row, never in process
memory; the cookie carries a nonce whose hash is on the row, so whichever
replica serves the poll can verify it.

### 2.9 The abuse stack is a middleware, not a property of the issuer

`http/server.go:220-225` composes it around one route. Wrapping the second
issue path is a wiring change, not a new control.

---

## 3. Decisions

### D1 -- Allow shared mailboxes; make them visible; give the owner a passkey-only switch

Three options were considered: refuse group/role aliases at registration and
invitation; allow them but flag the account and offer a passkey-only sign-in
policy; allow them silently and fix only the race. The owner chose the second.

Refusing aliases has false positives (`info@` for a solo operator) and says
nothing about accounts that already exist. Fixing only the race leaves the
shared mailbox as a shared account. Flag-plus-policy is the only option that
closes the hole: an alias account with `signInPolicy=passkey_only` can be
entered only by someone holding a passkey, and the flag tells everyone who
looks at the account why that matters.

### D2 -- Cookie binding plus approve-on-click

Three mechanisms were considered for binding the link to the device that asked:

- **Strict cookie binding.** The link only completes in the requesting
  browser; anywhere else, "open this on the device where you started."
  Simplest. Breaks the phone-click-for-a-laptop-request case.
- **Cookie binding plus approve-on-click.** The link still only *completes* in
  the requesting browser, but a click anywhere else *approves* the request,
  and the requesting tab notices and completes itself.
- **Cookie binding plus a 6-digit code** typed into the requesting tab. Same
  security property as the second option; more surface (code generation, entry
  UI, attempt limits, lockout).

The owner chose the second. Its property is the one that makes the alias
scenario structurally harmless: **a session can only ever land on the device
that asked for it.** If B clicks, A signs in. Cross-device "click on the phone,
the laptop signs in" works as users expect. Prefetchers that GET the link do
nothing, because approval is a POST from a page a human is looking at.

### D3 -- A GET never changes state

`GET /auth/complete` renders and nothing else. Consumption and approval happen
on POST. This costs one click in every case -- including the same-device case,
where a mail client opens the link in a new tab of the browser that holds the
cookie. The owner accepted that cost as an assumption of the approved design;
it is reversible in isolation (let a cookie-bearing GET complete) without
touching anything else, and the spec records it so that reversal is a
decision rather than a drift.

### D4 -- The landing page decides by cookie, and the cookie is the only credential

Continue with the binding cookie present: finish here, in this tab. Continue
without it: approve only, and tell the person to go back to the device where
they asked. There is no third path -- in particular no "continue here anyway"
for the no-cookie case, because that is exactly B's click.

### D5 -- Browser sessions get rows; PKCE is unconditional; `plain` is gone; both issue paths are abuse-checked

`startBrowserSession` creates an `authSession` row with `source=oidc_cookie`
and carries the session id in the token, so the session is listable and
revocable like any other. `/oauth/token` refuses a code with no challenge and
refuses `plain`. `POST /login` refuses a matched client that sent no challenge.
The web `/login` issue path runs through the same abuse middleware as
`/auth/magic-link`. Pre-release: no compatibility shim for clients that skipped
`/authorize`.

### D6 -- Alias policy is two fields on the user

`sharedMailbox bool` -- a hint, set by a local-part heuristic at
registration and invitation, editable by the user and by admins. It never
blocks anything; it drives copy and an audit signal.

`signInPolicy enum("any", "passkey_only") @default("any")` -- the user can set
`passkey_only` only while holding at least one active passkey. A magic-link
request for such an account renders the normal check-email page and sends a
*notice* ("sign-in links are disabled for this account; use your passkey")
instead of a link -- informative to the owner, useless to B. Admins can reset
it to `any` over `IdentityAdminMsg`, audited; that is the rescue path, and it
is no more power than the enrolment-token issuance admins already hold.

### D7 -- Visibility reaches the mailbox and the profile

A new-sign-in email on every new session, with time, IP and client label and
no action link. On a shared mailbox that lands in front of everyone, which is
the point. A self-scoped sessions read plus revoke-one and revoke-all, exposed
over `MemqlService.Stream` for the portal's profile page (sub-project C) and
wired into identity's own `/me/devices` panel. New audit actions record the
*approving* device, so a B click leaves a row that names B's IP.

### D8 -- Two new HTTP routes, inside the existing auth exception

`GET /auth/magic-link/status` and `POST /auth/magic-link/finish`, plus
mounting the already-exempt `POST /auth/landing`. They are the magic-link flow
CLAUDE.md already carves out of the gRPC-first rule ("browser form posts and
redirects"), and the owner approved them explicitly in the brainstorm. They
are declared through the identity server's own route table, not
`component/server`, so the bff's front-door path generator is unaffected.

---

## 4. The flow

```
 requesting browser (A)            identity                        any browser (A or B)
 -------------------------         -----------------------------   ---------------------
 POST /login (email)  ---------->  mint request row
                                   bindingHash = sha256(nonce)
                      <----------  303 /check-email  +  Set-Cookie memql_ml=<nonce>
 GET /check-email                  (page carries requestId; poller starts)
                                   email with /auth/complete?ml=<token>  -------->  opened here
                                                                                   GET /auth/complete?ml=
                                   verify hash + expiry, NO consume   <----------
                                   render landing: "Sign in as team@? [Continue]"  -------->
                                                                                   POST /auth/landing {ml}
                                   cookie present?  -> finish (below)  <----------
                                   cookie absent?   -> approvedAt=now, IP/UA; "go back to your device"
 GET /auth/magic-link/status?request=<id>  (every 2 s, cookie required)
                      <----------  {state: approved}
 POST /auth/magic-link/finish {request}  (cookie required)
                                   CAS consume; mint auth code OR browser session (+ authSession row)
                      <----------  303 to redirect_uri?code=..&state=..   OR   303 to post-login landing
```

### 4.1 Issue

Both issue paths (`POST /login` in `web/handlers.go:193-281`, `POST /auth/magic-link`
in `http/magic_link.go:48`, and the bootstrap path at `handlers.go:661`):

- Generate a 32-byte CSPRNG nonce. Store `bindingHash = sha256hex(nonce)` on
  the row. Set `memql_ml=<nonce>` as `HttpOnly; Secure; SameSite=Lax;
  Path=/auth; Max-Age=<magic-link TTL>`. `Lax` is required so that a top-level
  navigation from a mail client carries the cookie; `Path=/auth` covers
  `/auth/complete`, `/auth/landing`, `/auth/magic-link/status` and
  `/auth/magic-link/finish` and nothing else.
- The emailed URL is `/auth/complete?ml=<token>`. The `state` query parameter
  leaves the URL: it lives in `oauthCtxJSON` and in the requesting tab, and the
  verifier's `state` comparison (`verifier.go:140-143`) moves to finish, where
  it compares the row against the tab that started the flow.
- `/check-email` renders the `requestId` (not secret; the cookie is the
  credential) and starts the poller.
- A second request from the same browser overwrites the cookie. The older
  link then behaves as a no-cookie click (approve only). Documented, accepted.

New row fields on `magicLinkRequest`: `bindingHash string`,
`approvedAt datetime`, `approvedFromIP string`, `approvedUserAgent string`.
`createMagicLinkRequest` (`dsl/identity/mutations.memql`) gains `bindingHash`;
`Store.CreateMagicLinkRequest` (`store.go:128`) threads it through.

### 4.2 Click -- `GET /auth/complete?ml=<token>`

Registered at `http/server.go:227`. New behaviour: look the row up by token
hash, check expiry and `consumedAt`, and **render** the landing page
(`webtempl.Landing`) with the masked email and a form whose hidden field
carries the token. The four failure states (invalid, expired, already used,
and -- new -- already approved from another device) each render their own
message, as the enrolment page does today. No write of any kind.

### 4.3 Continue -- `POST /auth/landing`

Mounted in `web/server.go` (the path is already CSRF-exempt at `:320-328`; the
token in the form body is the proof of possession, so CSRF protection adds
nothing here). Two branches:

- **Cookie present and `sha256(cookie) == row.bindingHash`:** this is the
  requesting browser (a mail client opened a new tab of the same profile).
  Run finish (4.5) in this request.
- **Cookie absent or mismatched:** conditional write -- set `approvedAt`,
  `approvedFromIP`, `approvedUserAgent` only if `approvedAt` and `consumedAt`
  are both empty. Render "Done. Go back to the device where you requested the
  link." Audit `magic_link_approved` with this device's IP and user agent. A
  second approval of the same row is idempotent and audited
  `magic_link_approval_denied{already_approved}`.

### 4.4 Poll -- `GET /auth/magic-link/status?request=<id>`

Requires the cookie; `sha256(cookie)` must equal the row's `bindingHash` or the
response is 404 (indistinguishable from an unknown id). Returns one of
`pending | approved | consumed | expired`. It reads a single row and is safe to
serve at a 2-second interval for the request's lifetime; it stops when the
request expires. Served by the identity web server, so the row is read from
Postgres and either replica can answer.

### 4.5 Finish -- `POST /auth/magic-link/finish`

Requires the cookie and `approvedAt` (or arrives via the cookie-present branch
of 4.3). Then:

1. **Consume exactly once.** `ConsumeMagicLinkRequest` becomes a
   compare-and-swap: the write succeeds for exactly one caller and every other
   caller gets "already used". Either a conditional update on `consumedAt` with
   an affected-rows check, or the Postgres advisory-lock gate
   `integrations/cognition`'s `dispatchGate` already uses for exactly-once
   greetings -- implementation's choice, the requirement is the property.
2. **Mint**, exactly as `complete.go` does today: an auth code bound to the
   stored challenge when `oauthCtxJSON` names a client
   (`Store.CreateAuthCode`), otherwise a browser session via
   `startBrowserSession` -- which now also creates an `authSession` row
   (section 5).
3. **Redirect** with 303 to `redirect_uri?code=..&state=..` or to the post-login
   landing (`TakePostLoginRedirect`, `complete.go:105-109`). The `/check-email`
   poller submits a real form on `approved`, so the 303 is a normal navigation
   in the tab that holds the PKCE state.

Audit: `magic_link_consumed` (unchanged) plus `magic_link_completed{mode:
same_device | cross_device}`; on refusal `magic_link_finish_blocked{reason}`.

### 4.6 Edge cases

| Case | Behaviour |
|---|---|
| Requesting tab closed before the click | Approval succeeds; nothing polls; the request expires at its TTL. The landing page's "go back" copy adds: "If you closed that page, request a new link from the device you want to use." |
| Prefetcher or scanner GETs the link | Renders the page. No state changes. The link stays usable. |
| Same device, mail client opens a new tab | Cookie present (same profile, `SameSite=Lax`, top-level navigation) -- finish runs in that tab. |
| B clicks first, then A clicks | B approves. A's click: cookie present -- finish runs; A signs in. (If A's poller already finished, A's click renders "already used".) |
| Two identity replicas | Row is the state; cookie hash is on the row. No affinity needed. |
| Bootstrap claim link | Same path. The `/setup` submit sets the cookie; the claim completes on the `/check-email` page of the browser that ran the wizard. |
| Expired or consumed at any step | The matching message; no write. |

---

## 5. Session and PKCE rules

- `startBrowserSession` (`complete.go:187-237`) creates an `authSession` row
  (`source=oidc_cookie`, `clientLabel` from the user agent,
  `firstAuthenticatedAt`, `expiresAt`) and issues the access token with that
  row's id as `SessionId`, the way `issueSessionForUser` does
  (`http/token_session.go:105-120`). The passkey web login
  (`http/webauthn_login.go:368-389`) goes through the same function and gets
  the same row.
- `/oauth/token` (`token.go:141`): a code with an empty `codeChallenge` is
  refused (`auth_code_redemption_blocked{no_pkce}`). `verifyPKCE`
  (`:334-338`) accepts `S256` only.
- `POST /login` (`web/handlers.go:231-236`): a matched client without a
  challenge is refused with a 400 that names the omission.
- `POST /login`'s issue call runs through the same abuse middleware as
  `POST /auth/magic-link` (`http/server.go:220-225`). The wiring is in
  `app/integrations_identity.go:210-239`.
- `randomState` (`web/handlers.go:865-868`, a seeded LCG) is replaced by
  `crypto/rand`. It is not security-critical after this design (state no
  longer travels in the email), and it stops being a thing a reviewer has to
  explain.

---

## 6. Alias policy

### 6.1 `sharedMailbox`

A new `bool` on `v1:identity:user`. Set at registration and invitation by a
local-part heuristic: RFC 2142 role names (`postmaster`, `hostmaster`,
`abuse`, `noc`, `security`, `webmaster`, `www`, `info`, `marketing`, `sales`,
`support`) plus the common team names `team`, `hello`, `contact`, `office`,
`admin`, `dev`, `ops`, `it`, `hr`, `billing`, `finance`, `legal`, `noreply` /
`no-reply`. The list lives in one Go file in `component/identity/registration`
with a test that pins it. The heuristic is a hint: the user can clear or set
the flag on `/me/settings`; an admin can do the same over `IdentityAdminMsg`.
Every change is audited `shared_mailbox_changed{by: self | admin}`.

Where it shows: identity's `/me/settings` and `/me` overview ("This account's
address looks like a shared mailbox. Anyone who can read it can sign in.
Consider passkey-only sign-in."), the `/login` page when a flagged address is
entered, and the portal's People view, which renders concept fields
generically. The portal profile page (sub-project C) picks it up from the same
field.

### 6.2 `signInPolicy`

A new `enum("any", "passkey_only") @default("any")` on `v1:identity:user`.

- **Setting it to `passkey_only`** (self, on `/me/settings`) requires at least
  one active `passkey` identity for the user; the control is disabled with an
  explanation otherwise. Audited `sign_in_policy_changed`.
- **Effect on issue.** `magiclink.Issuer.Issue` resolves the email to a user
  before minting. If the user's policy is `passkey_only`: no request row is
  written, no link is sent, the caller is still redirected to `/check-email`
  (no enumeration signal), and the sender emits the **notice** email:
  "Someone asked for a sign-in link for this account. Sign-in links are
  disabled for it -- sign in with your passkey. If this wasn't you, nothing
  has happened." Audited `magic_link_refused_policy`. A registration-mode
  flow for an address with no user is unaffected.
- **Effect elsewhere.** Passkey login is unchanged. Enrolment tokens and the
  owner recovery key are unchanged (they exist for exactly the "I lost my
  passkey" case).
- **Rescue.** An owner or admin resets the policy to `any` over a new
  `IdentityAdminMsg` action, audited `sign_in_policy_reset_by_admin`. This is
  the same trust level as issuing an enrolment token for the user, which
  admins can already do.

---

## 7. Visibility

### 7.1 New-sign-in email

A new `Sender` method beside `SendMagicLink` (`emailsender/sender.go:48`),
fired wherever an `authSession` row is created -- one seam, covering the
token exchange, the browser session and the passkey session. Content: time,
IP, client label, the cluster's brand name, and "If this wasn't you, sign in
and revoke sessions from your profile page." **No action link**: an
unauthenticated revoke link mailed to a shared mailbox is a denial-of-service
handle. Refresh rotations never notify. Audited `sign_in_notification_sent`.

### 7.2 Sessions, self-scoped

- A query `authSessionsForSelf` over `v1:identity:authSession`: the caller's
  rows (`userId==actor.userId`, with `subject` as the canonical fallback the
  concept describes), excluding revoked and expired, shaped as `row.id`,
  `source`, `clientLabel`, `firstAuthenticatedAt`, `lastActivityAt`,
  `lastRefreshedAt`, `expiresAt`. No token hashes in the shape.
- `MyAccessResult` (`component/grpc/memql.proto:2044-2055`) gains
  `session_id`, so a client can mark "this device" without decoding the JWT
  (the portal's standing rule, `clients/portal/src/cluster/useMyAccess.ts:6-18`).
- Revoke-one and revoke-all already exist as gRPC handlers
  (`auth_session_handlers.go:192`, `:259`); the SDK exposes them if it does
  not already.
- Identity's `/me/devices` "Active sessions" panel is rendered server-side
  from the same read, and `POST /me/devices/revoke-all` is mounted onto the
  revoke-all path. The portal profile page (sub-project C) renders the same
  list; this design supplies the data, not the portal page.

### 7.3 Audit actions added

| Action | Category | Outcome | Carries |
|---|---|---|---|
| `magic_link_approved` | auth | success | approving device IP + UA; `detail.crossDevice` |
| `magic_link_approval_denied` | auth | blocked | `failureReason`: already_approved, consumed, expired, unknown |
| `magic_link_completed` | auth | success | `detail.mode`: same_device / cross_device |
| `magic_link_finish_blocked` | auth | blocked | `failureReason`: not_approved, consumed, expired, cookie_mismatch |
| `magic_link_refused_policy` | auth | blocked | the policy |
| `auth_code_redemption_blocked` (existing) | auth | blocked | new `failureReason`: no_pkce, plain_not_allowed |
| `sign_in_notification_sent` | auth | success | session id |
| `sign_in_policy_changed` / `sign_in_policy_reset_by_admin` | identity / admin | success | from, to |
| `shared_mailbox_changed` | identity | success | by, from, to |

`action` on `auditEvent` is an unconstrained string (`dsl/identity/concepts.memql:43`);
its description is updated to list these and to drop `refresh_succeeded` /
`refresh_token_theft_detected`, which nothing emits (sub-project D owns the
refresh lifecycle).

---

## 8. Security posture

| Threat | Before | After |
|---|---|---|
| B clicks A's link first | B signs in (identity path) or locks A out (portal path) | B approves A's request; A signs in. B gets nothing. A row names B's IP. |
| Mail scanner / proxy GETs the link | Link burned; on the identity path the scanner is handed a session cookie it discards | Page rendered, no state change. |
| B requests their own link to the alias | B signs in | Unchanged by the race fix. Mitigated by `passkey_only`, and by visibility: the notice or the new-sign-in email lands in the shared mailbox. |
| Double click / double submit | Read-then-write race (`verifier.go:119-125`) | CAS: exactly one consume. |
| Stolen `memql_ml` cookie | n/a | Worth one completion of one request within its TTL, and only together with the emailed token or an approval. Host-only, `HttpOnly`, `Path=/auth`. |
| Forged approval POST | n/a | Requires the token from the email; the path is CSRF-exempt because the token is the proof. |
| Code replay / challenge-less code | Code without a challenge redeems for anyone (`token.go:141`) | Refused. `plain` refused. |
| Account enumeration via passkey-only | n/a | Same page and redirect whether or not the policy applies; the only difference is which email arrives, and only the mailbox sees that. |
| Polling abuse | n/a | Cookie-gated per request; a single-row read; stops at TTL. Per-IP rate limit shared with the abuse stack. |
| Unbound browser session | `memql_admin` minted on any click, no row | Minted only from a cookie-bound finish; has a row; revocable. |

What this design does not change: an enrolment or recovery link pasted into a
mail to the alias still enrols whoever opens it (section 2.5); passkey login
is as strong as the authenticator; an admin can still reset the policy.

---

## 9. Testing

All in the identity Go suite, written failing-first:

1. `GET /auth/complete` with a valid token changes nothing on the row and
   renders the landing page; a second GET still renders it.
2. `POST /auth/landing` without the cookie sets `approvedAt` + device facts,
   mints no session, sets no cookie, audits `magic_link_approved`; a repeat is
   idempotent and audits the denial.
3. The status endpoint returns 404 without the cookie or with a mismatched
   one; returns `approved` after step 2 with the right cookie.
4. `finish` with the cookie after approval consumes once and redirects with a
   code (OAuth context) or sets `memql_admin` + creates an `authSession`
   row with `source=oidc_cookie` (no context); `finish` before approval is
   refused; two concurrent finishes yield exactly one consume.
5. `POST /auth/landing` with the cookie present finishes directly.
6. `/oauth/token` refuses a code with no challenge and a `plain` challenge;
   `POST /login` refuses a matched client without one.
7. `POST /login` is rejected by the abuse middleware under the same conditions
   `POST /auth/magic-link` is.
8. Registration of `support@acme.test` sets `sharedMailbox=true`;
   `jane@acme.test` does not; self and admin edits audit.
9. A `passkey_only` user requesting a link gets no row, the notice email, the
   normal redirect, and `magic_link_refused_policy`; the switch cannot be
   enabled with zero active passkeys; admin reset audits.
10. Creating any `authSession` row sends exactly one new-sign-in email; a
    refresh rotation sends none.
11. `authSessionsForSelf` returns only the caller's live rows and no hashes;
    `MyAccessResult.session_id` matches the current row; revoke-one and
    revoke-all close the browser-cookie session too.
12. Cluster e2e (`test/clustere2e/`, 2 identity replicas): approve on one
    replica, poll and finish on the other.

The two retired behaviours get negative tests so they cannot return: a
cookie-less GET that consumes, and a browser session with no row.

---

## 10. Delivery

Two PRs, along the one real dependency seam (browser sessions must have rows
before the notification and the sessions list can depend on them):

| PR | Contains | Closes |
|---|---|---|
| 1 -- the flow | DSL + store fields and CAS consume; issue-time cookie; landing, status and finish routes; the `/check-email` poller; browser-session rows; PKCE and abuse-stack hardening | the three flow tasks |
| 2 -- the policy and the mirror | `sharedMailbox` + `signInPolicy` + notice email + admin reset; new-sign-in email; self-scoped sessions read, `session_id` on `MyAccess`, `/me/devices` wiring; docs | the four remaining tasks |

PR 2 branches off PR 1 and targets `main` so it gets CI in parallel. One
`Closes #N` line per issue in each PR body.

---

## 11. Out of scope

- Refresh-token reuse detection and the refresh audit lifecycle -- sub-project
  D.
- The portal profile page and its sessions list UI -- sub-project C; this
  design supplies the read and the `session_id`.
- Emailing enrolment or recovery links (they are shown to the admin, never
  mailed; unchanged).
- Blocking aliases at registration (rejected in D1).
- A "revoke all sessions" action link in the notification email (rejected in
  7.1).
- Rate-limit tuning of the abuse stack; it is reused as is.

---

## 12. References

- Code: `component/identity/magiclink/{issuer,verifier}.go`,
  `component/identity/http/{complete,token,token_session,magic_link,server}.go`,
  `component/identity/web/{handlers,server,redirect_authenticated,me_passkeys}.go`,
  `component/identity/web/templ/{landing,check_email,me_devices}.templ`,
  `component/identity/emailsender/sender.go`,
  `component/identity/registration/flow.go`, `component/identity/abuse/`,
  `component/grpc/{auth_session_handlers,my_access_handler}.go`,
  `dsl/identity/{concepts,mutations,queries}.memql`.
- Related designs: `2026-08-09-signin-without-email-design.md` (passkeys,
  enrolment, D6 "magic-link stays as the universal recovery route" -- still
  true; this design changes how the link completes, not that it exists).
- Issues: #3406-#3409 (passkeys), #3958 (recovery key), #4240/#4241
  (portal chrome, where the sessions list will render).
