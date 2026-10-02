---
title: Connecting an Editor
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Connecting an Editor

How the MemQL extension for Visual Studio Code and Cursor signs in to a cluster,
what it needs from that cluster (nothing), who is allowed to do it, and what to do when it refuses.

The short version: **an editor connects to a cluster with no configuration on
either side.** If that is not what you are seeing, the
[troubleshooting](#troubleshooting) section names the two failures worth
recognising.

---

Materializer collects the brief, sources, and output settings. Its finished
files open directly in the chosen editor for review and editing. ZIP outputs
use the regular authenticated download path and remain intact.

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

VS Code for the Web uses device approval with PKCE; a browser extension cannot
bind a native loopback listener. Enter the cluster domain in **MemQL: Add
Cluster**, then sign in. The web extension keeps its cluster list in the editor
profile and credentials in VS Code SecretStorage. Native local-cluster
installation remains a desktop operation. Desktop and web support macOS and
Linux; Windows is outside the supported platform matrix.

---

## The built-in client: `memql-vscode`

Every identity node carries the editor as a **compiled-in first-party OAuth
client**. There is no operator step: a cluster serves it on the day it is
installed, and `MEMQL_IDENTITY_OAUTH_DCR_ENABLED` has nothing to do with it.

| Property | Value | Why |
|---|---|---|
| `client_id` | `memql-vscode` | Fixed. A released extension carries this string, so changing it strands every editor already installed |
| Display name | MemQL for Visual Studio Code and Cursor | What the consent page shows |
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

> MemQL for Visual Studio Code and Cursor manages this cluster. Your role on it is reader, and signing
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

The desktop registry lives at `~/.memql/clusters.yaml`. MemQL and Cockpit share
cluster definitions, with a cross-process lock and atomic writes that preserve
unknown fields. Selecting an editor connection does not redirect a Cockpit
worker or change its enrollment.

| Field | What it is | Who writes it |
|---|---|---|
| `domain` | The cluster's domain. Everything else derives from it | you |
| `issuer` | The identity service URL. Defaults to `https://identity.<domain>` | you, only for a non-standard front door |
| `endpoint` | The gRPC front door. Defaults to `api.<domain>:443` | you, only for a non-standard front door |
| `token` / `refresh_token` | Credentials belonging to another registry client, when present | The client that owns them; editor sign-in uses SecretStorage |
| `client_id` | Another tool's OAuth client (the Cockpit writes `cockpit`). The editor does not use it | the tool that owns it |

Set a name and a domain, then run **MemQL: Sign In**. Editor access and refresh
tokens stay in VS Code SecretStorage; a locked or unavailable secret store
produces an actionable error instead of falling back to a plaintext file.
Signing out of the editor clears its credentials, leaving Cockpit's session
and worker connection alone.

**The editor always signs in as `memql-vscode`.** `clusters.yaml` is shared with
the MemQL Cockpit, which records its own client there (`client_id: cockpit`,
registered for the Cockpit's own callback path). A `client_id` in the file
belongs to the tool that wrote it, so the editor never signs in or refreshes
with it. It keeps the client each refresh token was issued to beside that token
in its own secret storage, and presents it on refresh.

**MemQL Productivity Tools** consumes the core extension's versioned connection
API. It has no separate sign-in, cluster registry, or credential store. An open
document retains the cluster and connection it came from; switching clusters
cannot retarget its save. A reconnect requires comparing the latest revision
before a stale edit can be saved. See [productivity tools](../../language/vscode.md#productivity-tools).

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

### "This cluster doesn't accept sign-in from VS Code."

Before it opens a browser, the editor asks the identity service whether it
accepts the sign-in request, the way MemQL OS does. This message is its answer
when identity refused the client or its redirect URI. The cluster does not carry
`memql-vscode` (the engine predates the built-in client), or a shadowing
`MEMQL_IDENTITY_REGISTERED_CLIENTS` entry lost the portless redirect URI (see
below). Identity logs each refusal at INFO as `authorize refused`, with the
client id and redirect URI, and MemQL OS now shows the refusal's own heading
("Invalid redirect URI", "Unknown client") rather than "Bad Request".

### `Unknown client` on the consent page, or `invalid_client` from `/device/code`

The cluster does not carry `memql-vscode`: the engine predates the built-in
client. Update the engine, or use the
[interim workaround](#interim-workaround-for-clusters-that-predate-this-feature).

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
MEMQL_IDENTITY_REGISTERED_CLIENTS='[{"clientId":"memql-vscode","redirectURIs":["http://127.0.0.1/callback"],"name":"MemQL for Visual Studio Code and Cursor"}]'
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

`clientId` here makes an extension old enough to self-register skip `/register`
entirely. A current extension ignores it and signs in as `memql-vscode`
regardless.

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
