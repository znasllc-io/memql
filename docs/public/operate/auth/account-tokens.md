---
title: Account tokens -- a credential issued to a user, on behalf of an account
audience: public
status: stable
area: operate
sinceVersion: 0.15.0
owner: znas
---

# Account tokens

An **account token** is a credential an operator mints against one of the
customers they manage (`v1:identity:account`). This page says what it is, what
it authorizes, and -- the part that matters most -- what it deliberately does
not.

Related: [access model](access-model.md) ·
[service-account JWT](service-account-jwt.md) ·
[per-row authz audit](per-row-authz-audit.md) · the design note behind the
`account` concept, `docs/internal/design/account-isolation-model.md`.

---

## 1. The one sentence

> **An account token is issued TO A USER, ON BEHALF OF an account. The
> authenticated subject is the operator's `v1:identity:user`. The account is a
> binding on the credential, not a subject and not a scope.**

Everything below follows from that sentence, including the awkward part in §3.

---

## 2. Why the account cannot be the subject

Because nothing in memQL can authenticate as one, and that is structural rather
than unfinished.

An `account` row is **a record the operator keeps about a customer**. It holds
no credential, mints no session, and has no login. Every authorization gate in
the tree resolves authority through `actor.userId`, and the actor envelope's
field set is closed: `userId`, `role`, `identityId`, `isClusterOwner` (plus
`now` and allow-listed `config.*`). There is no seat at that table for an
account.

Giving an account a login would mean minting `v1:identity:identity` rows that
point at a non-user subject -- a second kind of principal that every existing
gate would have to learn about. A customer-facing login, if one is ever wanted,
is a **`v1:identity:user`** whose reach into an account is a grant row.

So: the credential's subject is the operator. That is stated in the concept
description, in the mint reply (`subject_user_id`), on the audit event
(`detail.subjectKind = "user"`), and in the portal's copy. Four places, on
purpose -- the failure mode this feature has to avoid is a year passing and
someone reasonably concluding that "the account authenticated".

---

## 3. What an account token authorizes: nothing, today, on purpose

This is the honest state and it is worth stating plainly rather than burying.

**No verifier resolves the `mql_acct_` prefix. No interceptor admits it. No
query looks a credential row up by its stored digest.** Presenting one as a
bearer gets you nothing.

That is a decision, not an omission, and it is forced by ruling out both
alternatives:

| Design | Why not |
|---|---|
| The token authenticates **as the account** | §2. There is no such subject, and inventing one means teaching every gate in the tree about a second principal kind. |
| The token authenticates **as the operator, narrowed to the account** | The narrowing is not expressible. The resolved actor carries no tenancy dimension, so an account predicate can only compare a payload field against a **caller-supplied argument** -- authorization by honour system. A credential named "Acme" that in fact carries the operator's entire authority is *worse* than a plain PAT, because its name understates its blast radius. |

So what ships is **custody**, which is the half that is honest now:

- mint good secret material (32 bytes from `crypto/rand`, `mql_acct_` +
  base64url);
- return the plaintext **exactly once**, in the mint reply;
- persist **only** the SHA-256 digest;
- list every credential issued for an account, revoked ones included;
- revoke immediately;
- audit both ends, including refusals.

The authorization half lands when the resolved actor carries an account set
(`actor.accountIds`). At that point the `accountId` already stored on every
credential row is exactly the value that dimension would be populated from --
which is why the binding is worth storing now.

### 3.1 Do not hand one to a customer

There is nothing for a customer to do with it, and if a future release makes
`mql_acct_` live, a credential already in a third party's hands would become
live with it. Treat an account token as **the operator's own secret, labelled by
customer** until this page says otherwise.

---

## 4. What it is good for right now

Three things, all of them real:

1. **One place that mints secrets correctly.** Entropy, prefixing, show-once,
   digest-only storage and an audit trail are easy to get subtly wrong, and
   every integration that rolls its own gets them wrong differently.
2. **Grouped lifecycle.** "Revoke everything I issued for Acme" is one list and
   N clicks, rather than a search through a flat credential store.
3. **Attribution.** Every mint and revoke lands on `v1:identity:auditEvent`
   carrying the account binding, so "who issued what for this customer, and
   when" is answerable from the trail.

---

## 5. Where it lives

| Piece | Location |
|---|---|
| Wire format | `mql_acct_<43 base64url chars>` -- 32 random bytes |
| Mint / hash | `component/identity/accounttoken` (`Mint`, `Hash`, `IsAccountToken`) |
| Storage | `v1:identity:identity`, `identityType="account_token"`; `credentials = { accountId, keyHash, mintedBy, expiresAt }` |
| Subject | the row's `userId` -- the operator |
| Reads | `query accountTokensForAccount(accountId)` and `query accountTokenById(identityId)`, both gated on `userId==actor.userId`, both projecting `accountTokenSummary` (**no digest**) |
| Writes | `mutation createAccountTokenIdentity` / `revokeAccountTokenIdentity` |
| Wire | `CreateAccountTokenMsg` / `RevokeAccountTokenMsg` on `MemqlService.Stream` |
| Handlers | `component/grpc/account_token_handlers.go` |
| UI | the portal's Customers view (`clients/portal/src/accounts/`) |

### 5.1 Why the mint is a gRPC envelope and the rest is not

Creating, editing, archiving an account and listing its tokens are ordinary
named queries and mutations the client runs over the normal query path. Only
two operations get their own message:

- **the mint**, because the plaintext exists in exactly one place -- the reply
  -- and every additional hop is another place it could be logged. The client
  calls the mint RPC **directly**, the same precedent `CreateWorkerTokenMsg`
  set;
- **the revoke**, because it is audited and the audit id comes back on the
  reply.

---

## 6. Authorization of the management surface

Distinct from §3, and this part *is* enforced.

`v1:identity:account` declares `@rowAuthz(owner="ownerUserId")`, so the engine
ANDs `ownerUserId == actor.userId` into every read of an account and refuses
every update whose target row the caller does not own. The mint handler's
ownership check is simply **`query accountById` executed as the caller** -- there
is no hand-written comparison in the handler, deliberately, because a second
copy of the tier is a copy that drifts.

Consequences worth knowing:

- **A caller who does not own an account cannot mint against it.** The read
  returns nothing and the mint is refused with `permission_denied` and a
  `blocked` audit event.
- **"No such account" and "not yours" are the same answer.** Distinguishing them
  would make the endpoint an oracle for which account ids exist.
- **There is no admin override, and that is not an oversight.** Read enforcement
  ANDs the owned predicate unconditionally -- there is no cluster-owner escape
  on the read path -- so an admin reading another operator's account gets zero
  rows too. An override in the handler would mint a credential against a row the
  minting path cannot actually see.
- **Revoking** resolves the credential through `accountTokenById` as the caller,
  whose filter carries `userId==actor.userId`.

---

## 7. Audit

Every mint and every revoke writes one `v1:identity:auditEvent`, including the
refusals.

| action | category | outcome | when |
|---|---|---|---|
| `account_token_created` | `auth` | `success` | a credential was minted |
| `account_token_create_blocked` | `auth` | `blocked` | the caller does not own the account (`failureReason=not_account_owner`) |
| `account_token_revoked` | `auth` | `success` | a credential was deactivated |
| `account_token_revoke_blocked` | `auth` | `blocked` | the caller does not own the credential (`failureReason=not_token_owner`) |

`targetType` is `identity` and `targetId` is the credential row -- the target of
the event is the credential, and the account is an attribute of it, carried in
`detail.accountId` alongside `detail.label`, `detail.subjectKind="user"` and
`detail.credentialFamily="account_token"`.

The reply's `audit_event_id` is the event's **`correlationId`**, matching the
deploy console's `ActionResult.audit_event_id` convention. It is not the audit
row's primary key; search the trail by `correlationId`.

Neither the plaintext nor the digest ever appears in an audit event.

---

## 8. Operator recipes

**Issue a credential.** Customers view -> select the customer -> *Issue token*.
Name it after the thing that will hold it ("Acme nightly export"), not after the
customer -- the list is already grouped by customer, and a list of four
credentials all called "Acme" cannot be revoked with confidence.

**Copy it now.** The plaintext is shown once. There is no "show again": the
server kept only a digest, so nobody -- including an administrator with database
access -- can recover it. If it is lost, revoke and mint a new one.

**Revoke.** Same panel, *Revoke*. It takes effect on the row immediately and the
revoked credential stays in the list, marked, so you can see the revocation
landed rather than watching a row disappear and having to trust that the right
one went.

**Rotate.** Mint the new one, move it into place, revoke the old one. There is
no in-place rotation, deliberately: a rotation that swaps the digest under a live
integration has no window in which both credentials work.

---

## 9. Known limits

- **`mql_acct_` is admitted nowhere** (§3). Say it twice; it is the thing
  someone will assume otherwise.
- **`revokeAccountTokenIdentity` carries no engine-level actor check.** Ownership
  is enforced in the handler, which resolves the row as the caller first. A raw
  wire call could deactivate another operator's credential -- a nuisance against
  a credential that authorizes nothing, not a disclosure. The systemic fix is a
  `@rowAuthz(owner="userId")` declaration on `v1:identity:identity`, which would
  put this revoke and the PAT / worker-token / badge / node-token revokes behind
  the engine's write guard at once. `revokePATIdentity` and friends have exactly
  the same shape today.
- **No expiry enforcement.** `expiresAt` is stored and displayed; nothing
  currently rejects an expired credential, because nothing accepts a valid one
  either. Revocation is the operative control.
- **No cross-operator view.** There is no "every account on the cluster" query
  and therefore no cluster-wide credential inventory. §6 is why.
