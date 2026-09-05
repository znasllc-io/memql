---
title: Signing in with the organization's directory (Entra ID / OIDC)
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: platform
---

# Entra ID / OIDC sign-in

**Audience:** an operator whose organization already has an identity provider
and who would rather not run a second one.
**Issue:** [memql#4611](https://github.com/znasllc-io/memql/issues/4611).

Related: [access-model.md](access-model.md) ·
[user-provisioning.md](user-provisioning.md) ·
[recovery-key.md](recovery-key.md) · [identity-service.md](identity-service.md)

## What this is for, and what it removes

MemQL is an OAuth 2.1 **authorization server**: claude.ai, the VS Code
extension and the portal authorize against it. Until this, a person always
proved themselves to MemQL directly — magic link, passkey, device code,
recovery key. For an organization already on Microsoft 365 that is the wrong
shape:

- **Directory membership becomes the invitation.** No invitation emails for
  internal staff, which removes the entire class of defect
  [memql#4601](https://github.com/znasllc-io/memql/issues/4601) exists to fix.
- **No new credential** for the user to hold, lose, or be phished out of.
- **The organization's MFA and conditional access apply**, rather than being
  approximated per-cluster.
- **Deprovisioning follows the directory.** Removing somebody from the company
  stops them signing in here. Nothing email-based can do that.

## The decision you are making

**Enabling a provider does not disable anything.** Magic links and passkeys keep
working; the sign-in page grows a second route. `MEMQL_IDENTITY_OIDC_EXCLUSIVE`
is the separate, deliberate act that closes the local routes.

**And even exclusive mode exempts the owner.** That is not a loophole, it is the
point: a federated cluster whose IdP is unreachable would otherwise have nobody
able to sign in — not the operator, not the person who could fix the federation.
No configuration may produce that state.

**The break-glass path is the owner recovery key**
([recovery-key.md](recovery-key.md)). It already has the right shape: one active
row per cluster, bound to one owner, redeemed exactly once to register a
passkey. Federation does not touch it and cannot disable it. **A cluster turning
on exclusive federation is a cluster whose recovery key had better be somewhere
the cluster is not.**

**What deprovisioning does and does not do.** Removing somebody from the
directory stops them signing in *through the IdP*. It does not retroactively
revoke a passkey they registered earlier — under exclusive mode that route is
closed to them anyway, which is the mode's actual value; under the default it is
not, and pretending otherwise would be the more dangerous claim.

## Configuring it

| Variable | Meaning |
|---|---|
| `MEMQL_IDENTITY_OIDC_ENABLED` | `true` turns the feature on. Everything else is inert without it. |
| `MEMQL_IDENTITY_OIDC_TENANT_ID` | Entra tenant id. Composes the v2.0 issuer for you. |
| `MEMQL_IDENTITY_OIDC_ISSUER` | Any OIDC issuer URL. Wins over the tenant id, so a non-Microsoft provider is not second class. |
| `MEMQL_IDENTITY_OIDC_CLIENT_ID` | This cluster's application id at the provider. |
| `MEMQL_IDENTITY_OIDC_CLIENT_SECRET` | Optional. A confidential client has one; a public client using PKCE does not, and sending an empty secret to a provider expecting none is a rejection. |
| `MEMQL_IDENTITY_OIDC_SCOPES` | Defaults to `openid email profile`. |
| `MEMQL_IDENTITY_OIDC_GROUPS_CLAIM` | Which claim carries group membership. Entra uses `groups`. Empty means groups are not read at all. |
| `MEMQL_IDENTITY_OIDC_GROUP_ROLES` | `group=role,group=role`. Highest matching role wins. |
| `MEMQL_IDENTITY_OIDC_EXCLUSIVE` | `true` disables magic-link and passkey sign-in for everyone except the owner. |
| `MEMQL_IDENTITY_OIDC_DISPLAY_NAME` | What the button says. |
| `MEMQL_IDENTITY_OIDC_DOMAIN_HINT` | Skips the account picker for a known tenant. |

**Redirect URI to register at the provider:**
`https://identity.<domain>/auth/oidc/callback`

**A half-configured provider refuses boot.** Enabled with no issuer, no client
id, a plaintext issuer, or a group mapping that does not parse — each is named
at startup rather than discovered per-user later. A federation that is on but
unusable is worse than one that is off: the button appears, people click it, and
the failure reaches everyone except the person who could fix it.

## The `directory` registration mode

`MEMQL_IDENTITY_REGISTRATION_MODE=directory` sits alongside `open`,
`domain_restricted`, `invite_only` and `waitlist`. Under it, **nobody
self-registers by email** — a person who authenticates through the provider is
admitted, and an address typed into the sign-in box is refused with
`directory_sign_in_required`.

That reason is why it is its own mode rather than a flag on `invite_only`: there
the answer is "ask an admin for an invitation", and here it is "sign in with your
organization account", which is a different instruction to somebody who has one
and does not know it applies.

**Invitations still work under it**, deliberately: a federated cluster still has
to admit a contractor or an auditor who is not in the directory.

## Account linking, and the one way it goes wrong

An existing magic-link or passkey user who later signs in via the IdP lands on
the **same row**. Two rows for one person means two sets of grants, two audit
trails, and a deprovisioning that removes one of them.

The match order is:

1. **`(issuer, subject)`** — the stable identity, and the only one the provider
   guarantees. It survives a rename, an address change, and an address being
   reassigned to somebody else. Once it exists it wins outright.
2. **Verified email**, used exactly once: the first time a known person arrives
   through the IdP, before any link exists.

**And the email must be verified by the provider.** An unverified `email` claim
is a string the directory did not check, so linking on it would mean anyone who
can set their own email at the upstream can take over the matching MemQL
account. An unverified address therefore links to nothing — the person is
treated as a stranger and is subject to the registration mode like any other.

A verified email matching a **deactivated** user is refused rather than
registered: registering would mint a second row for an address one already
holds, and hand access back to somebody it was deliberately removed from.

## Role mapping

`MEMQL_IDENTITY_OIDC_GROUP_ROLES` maps directory groups onto the cluster roles.
Group membership is a **set** and people are legitimately in several, so the
**highest** matching role wins — taking the first would make the outcome depend
on the order the directory happened to return them.

"Highest" is the cluster's one role ladder (epic memql#4832, D1), and the one
pair worth stating outright is **developer (300) over admin (200)** -- somebody
in a group mapped to `admin` and a group mapped to `developer` resolves to
**developer**. That is not the intuitive reading of the two names, and it is
what the ladder says: the two tiers are orthogonal in capability (admin holds
the principal verbs and no authoring, developer the reverse), so ranking them
is lossy in whichever direction it points. Map the group to `owner` for
somebody who needs both sets of verbs.

An unmapped group is **not a ban**. It means "the cluster default", and
conflating the two would make a missing mapping silently equivalent to
exclusion.

## What is audited

Every step keeps a distinct reason, so a support question is answerable from the
trail rather than from a guess:

| Action | When |
|---|---|
| `oidc_sign_in_refused_by_provider` | the user declined consent, or tenant policy blocked them |
| `oidc_sign_in_refused` | no state cookie, state mismatch, or no code |
| `oidc_sign_in_failed` | discovery, exchange or provisioning failed |
| `oidc_id_token_rejected` | signature, issuer, audience, expiry or nonce failed |
| `oidc_sign_in_decided` | carries the linking decision and its reason |

## The protocol, briefly

```
GET /auth/oidc/start     -> 303 to the provider's authorize endpoint
GET /auth/oidc/callback  <- the provider's redirect, carrying code + state
```

Both are documented HTTP exceptions of the kind the identity service already
carries: the other party is a browser performing an OAuth redirect, and there is
no gRPC form of "the user was sent to Microsoft and came back".

A successful callback lands on the **same seams the local factors use**:
`startBrowserSession`, which has been factor-agnostic since memql#3920 and
already stamped `source: "oidc_cookie"`, and `CreateUserOnFirstLogin`, which is
what a magic-link first sign-in calls. Reusing them is the point — a federated
cluster must not grow a second, subtly different definition of what a user row
or a session is.

**The registration mode still decides whether a new person may be created.** A
federated sign-in is not a way around `invite_only`: under any mode other than
`directory`, the ordinary policy runs with no invitation, so a cluster that
turned federation on without choosing `directory` admits only people it would
have admitted anyway.

`state`, `nonce` and the PKCE verifier live in one short-lived HttpOnly
`SameSite=Lax` cookie — never in the URL, which is handed to a third party and
appears in its logs. **Lax rather than Strict is load-bearing**: the callback is
a top-level navigation from the provider's origin, and Strict withholds the
cookie on exactly that, which would fail every sign-in with what looks like an
attack. The cookie is cleared on every outcome; a verifier that outlives its use
is a second chance at a code that should have one.
