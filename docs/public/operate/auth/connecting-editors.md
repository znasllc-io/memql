---
title: Connecting an Editor
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Connecting an Editor

How the MemQL VS Code extension signs in to a cluster, what it needs from that
cluster (nothing), who is allowed to do it, and what to do when it refuses.

The short version: **an editor connects to a cluster with no configuration on
either side.** If that is not what you are seeing, the
[troubleshooting](#troubleshooting) section names the two failures worth
recognising.

---

## The two flows, and when each engages

| Flow | What happens | When it runs |
|---|---|---|
| **Browser code flow** | The extension binds a loopback listener on an ephemeral port, opens `https://identity.<domain>/authorize` in your browser, and receives the authorization code back on that port | The default, on a laptop or a desktop |
| **Device flow** (RFC 8628) | The extension shows a short code; you approve it in a browser on any device; the extension polls for the result | Automatically, when the loopback path cannot serve |

You do not choose between them. The extension tries loopback, and falls back to
the device flow when the host cannot do loopback -- either because no port would
bind, or because there is no browser to open. **Remote-SSH, Codespaces and dev
containers are the ordinary case for the fallback**: the browser runs on a
different machine from the extension host, so a callback to `127.0.0.1` reaches
the wrong computer.

Both flows end the same way: an OAuth authorization code, redeemed at
`POST /oauth/token` for an access token and a refresh token, which the extension
stores for you.

---

## The built-in client: `memql-vscode`

Every identity node carries the editor as a **compiled-in first-party OAuth
client**. There is no operator step: a cluster serves it on the day it is
installed, and `MEMQL_IDENTITY_OAUTH_DCR_ENABLED` has nothing to do with it.

| Property | Value | Why |
|---|---|---|
| `client_id` | `memql-vscode` | Fixed. A released extension carries this string, so changing it strands every editor already installed |
| Display name | MemQL for VS Code | What the consent page shows |
| Redirect URI | `http://127.0.0.1/callback` | Loopback only, and **portless** -- see below |
| Client type | Public (no secret) | The extension ships to every user's machine, so a baked-in secret would be a secret in name only |
| PKCE | Required (S256) | What actually binds the authorization code to the process that asked for it |
| Role floor | developer and above | The editor is a management surface -- see [Who may connect](#who-may-connect) |

**The portless redirect URI is load-bearing.** The loopback listener takes
whatever ephemeral port the kernel hands it, so the URI the browser returns to
carries a different number every sign-in. RFC 8252 section 7.3 grants an
any-port exception to a registered loopback URI **with no explicit port**; one
that carries a port opts back into exact matching. A port added to that value
would break every callback -- and the failure would appear on the second
sign-in, not the first.

### Why this is not dynamic client registration

`POST /register` (RFC 7591) is for clients nobody configured in advance: a
third-party MCP connector added from someone else's product. It is an
unauthenticated write, and the caller chooses the `client_name` a human is later
shown when asked to approve access, so it is **off by default** and belongs only
on clusters that expose an MCP surface. The reasoning is in
[identity-service.md](identity-service.md#registering-an-oauth-client-grants-nothing).

**First-party editors never use it.** The editor ships with the product, so
identity knows it the way it knows any operator-configured relying party. The
extension makes zero requests to `/register`, and enabling DCR neither helps nor
hinders editor sign-in.

---

## Who may connect

Sign-in from an editor requires the **developer role or above** on the cluster:

| Role | May connect an editor |
|---|---|
| owner | yes |
| admin | yes |
| developer | yes |
| writer | no |
| reader | no |
| (no cluster-wide role) | no |

The editor manages the cluster -- it edits DSL, runs constructs and drives
deploy controls -- so the floor is a property of what the editor **is**, not a
general OAuth setting. There is no environment variable for it. Admin is
included deliberately: an admin operates the console's admin surfaces, and
refusing them the editor while admitting them there would be incoherent.

**A refused person sees a sentence naming their role**, in the editor, in both
flows:

> MemQL for VS Code manages this cluster. Your role on it is reader, and signing
> in from an editor needs developer or above. Ask a cluster owner or admin to
> raise your role.

Every refusal writes an audit event (`editor_signin_refused_role`, category
`identity`) carrying the client id, the role required and the role held.

Two things the floor does **not** do:

- It does not touch any other client. Static clients from
  `MEMQL_IDENTITY_REGISTERED_CLIENTS` and self-registered DCR clients are
  unaffected, so the console and every MCP connector behave exactly as before.
- It does not re-check on token refresh. The floor runs at approval time, when a
  human is present and can be told why. A role lowered after sign-in takes
  effect at the person's next sign-in; to cut an existing session immediately,
  revoke it (identity's own /me/devices, or `v1:identity:authSession`).

An operator who needs different policy shadows the client id in
`MEMQL_IDENTITY_REGISTERED_CLIENTS` -- a static entry replaces the built-in
whole, and carries no floor. That is an explicit, visible act rather than a
default nobody reviewed.

---

## The `clusters.yaml` fields that matter

The extension's registry lives at `~/.memql/clusters.yaml`.

| Field | What it is | Who writes it |
|---|---|---|
| `domain` | The cluster's domain. Everything else derives from it | you |
| `issuer` | The identity service URL. Defaults to `https://identity.<domain>` | you, only for a non-standard front door |
| `endpoint` | The gRPC front door. Defaults to `api.<domain>:443` | you, only for a non-standard front door |
| `token` | The identity-issued JWT access token | **sign-in** |
| `refresh_token` | Renews the access token as it expires | **sign-in** |
| `clientId` | An OAuth client id to use **instead of** `memql-vscode` | you, and only if you mean it |

In the normal path you set a name and a domain, run **MemQL: Sign In**, and
touch nothing else. `token` and `refresh_token` are things sign-in writes; hand-
editing them is for an unattended setup, not for a person at a keyboard.

**`clientId` is an override, and it is usually wrong to set one.** It exists for
two cases: an operator who configured a custom static client in
`MEMQL_IDENTITY_REGISTERED_CLIENTS`, and an entry left over from before this
feature that still carries an id minted by the old registration path. Both keep
working -- the value is simply read -- and nothing migrates or rewrites the file.

---

## Troubleshooting

### `.../register returned 403: registration_disabled`

**You are on a cluster that predates this feature.** The extension you are
running is old enough to self-register, or the cluster is old enough not to
carry the built-in client. The message reads like an instruction to enable
registration; it is not. Enabling `MEMQL_IDENTITY_OAUTH_DCR_ENABLED` would open
an unauthenticated write endpoint to work around a problem that has a better
answer.

Update the engine and the extension. If you cannot yet, use the
[interim workaround](#interim-workaround-for-clusters-that-predate-this-feature).

### `Unknown client` on the consent page, or `invalid_client` from `/device/code`

The cluster does not carry `memql-vscode`. Either the engine predates the
built-in client, or a `clientId` in `clusters.yaml` is naming something this
cluster does not have. Check that field first -- an override left behind by an
old sign-in is the common cause.

### `Invalid redirect URI`

The registered redirect URI has stopped being portless, which breaks the RFC
8252 any-port exception. If you shadowed `memql-vscode` in
`MEMQL_IDENTITY_REGISTERED_CLIENTS`, your entry's `redirectURIs` must contain
`http://127.0.0.1/callback` with **no port**.

### Sign-in was refused and named my role

That is the [role floor](#who-may-connect). Ask a cluster owner or admin to
raise your role to developer or above.

### `dial ... failed (missingCredential)` after signing in

Sign-in stored a token and the dial found none, which means the two are looking
at different cluster entries -- usually a duplicate name in `clusters.yaml`.
Check that the entry you signed in to is the one that is selected.

---

## Interim workaround for clusters that predate this feature

A cluster running an engine older than the built-in client can still serve the
editor today: configure `memql-vscode` as a **static** client, which every
released engine already supports.

On the identity deployment:

```
MEMQL_IDENTITY_REGISTERED_CLIENTS='[{"clientId":"memql-vscode","redirectURIs":["http://127.0.0.1/callback"],"name":"MemQL for VS Code"}]'
```

The portless redirect URI is load-bearing here for the reason given
[above](#the-built-in-client-memql-vscode): with a port, RFC 8252's any-port
matching does not apply and every callback fails validation.

On the cluster entry in `~/.memql/clusters.yaml`:

```yaml
clusters:
  - name: production
    domain: example.com
    clientId: memql-vscode
```

`clientId` here makes an extension that still self-registers skip `/register`
entirely.

**Two caveats.** This is a full replacement of the client rather than a
pre-seeding of it, so `MEMQL_IDENTITY_REGISTERED_CLIENTS` must list every other
static client the cluster needs in the same JSON array. And the **role floor
does not exist on those releases** -- any role that can sign in at all can
connect an editor.

Once the cluster carries the built-in client, remove both: the env var, so the
floor applies, and the `clientId` override, so the default is what runs.

---

## See also

- [Sign-in Paths](sign-in-paths.md) -- the five ways to obtain a credential
- [Identity Service](identity-service.md) -- operator env vars, and the DCR decision
- [Access Model](access-model.md) -- the role spectrum the floor reads
