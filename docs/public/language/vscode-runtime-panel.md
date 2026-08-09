---
title: VS Code Runtime Panel
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# VS Code Runtime Panel

The memQL extension's activity-bar panel connects VS Code to a running
cluster: pick a cluster, browse every registered concept, and inspect
rows without leaving the editor.

Verifying a change to this panel: [VS Code Runtime Panel -- Manual Verification
Checklist](vscode-runtime-panel-verification.md), which also states what the
automated `make vscode-test-host` smoke lane covers and what it deliberately
leaves to a human.

## Requirements

- A **trusted** workspace. Language features (highlighting, diagnostics,
  completion, hover, signature help) work in an untrusted workspace; the
  runtime panel does not. It reads credentials and opens a network
  connection, which a malicious workspace must not be able to trigger.
- A cluster in `~/.memql/clusters.yaml` with an endpoint and a Personal
  Access Token.

## Clusters

The panel reads the same `~/.memql/clusters.yaml` the memQL Cockpit uses,
so a cluster added in either tool appears in both. The file is watched:
an external edit refreshes the view.

Click a cluster to make it the working cluster. The selection persists to
`selected_cluster`, so the cockpit resumes on the same cluster.

| Icon | Meaning |
|---|---|
| Filled green circle | Connected |
| Spinner | Connecting |
| Red error icon | The cluster is the problem -- unreachable, or the connection died |
| Yellow key | The CREDENTIAL is the problem -- expired, missing, or the wrong class |
| Yellow warning | Not configured -- no endpoint |
| Hollow circle | Configured, not connected |

The key and the red dot are deliberately different pictures. "Your token
ran out" and "the cluster went away" have completely different next
actions, and rendering both as a red dot made the first one unreadable
(memql#3385).

**Add Cluster** and **Edit Cluster** collect a name, domain, endpoint,
access token and (optionally) refresh token. Writes preserve comments and
any field a newer cockpit wrote, because the file is shared.

### Authentication

The panel dials with the `token` field, which must hold an
**identity-issued JWT access token** -- the `access_token` from
`POST <identity>/oauth/token`.

**A Personal Access Token does not work here, and cannot.** PAT
verification is a database lookup wired only into the identity binary, so
every mesh node (bff, voice, cognition, agent, planner) rejects an
`mql_pat_...` bearer *before* looking anything up. The panel detects one
in the `token` field and refuses it by name rather than letting it fail as
an unexplained handshake error (memql#3383).

Access tokens are short-lived -- identity issues them with a 900-second
TTL. Set `refresh_token` alongside the token and the panel renews it
itself: proactively before each connect, and in place on a live stream via
the SDK's re-auth hook, so a long session never has to be re-credentialed
by hand (memql#3385).

The refresh token is a 30-day credential, so the panel does not leave it
in `clusters.yaml`. The `refresh_token` key is an **ingest path only**: on
the first successful exchange the rotated token is moved into VS Code's
`SecretStorage` and the plaintext key is deleted from the file. The access
token stays in the file, because it is short-lived and because the memQL
Cockpit shares this registry and needs to see it.

A cluster entry therefore looks like this:

```yaml
clusters:
  - name: local
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    issuer: https://identity.local.znas.io   # optional; derived from domain
    client_id: cockpit                       # optional
    token: eyJhbGciOiJSUzI1NiIsImtpZCI6...   # JWT access token
    refresh_token: <ingest only -- moved to SecretStorage on first use>
    local: true
selected_cluster: local
```

`issuer` is where the refresh exchange is POSTed. When it is absent the
panel derives `https://identity.<domain>` (or the `identity.` sibling of a
`cockpit.<host>` endpoint); a cluster with neither is told which field to
supply rather than having a host guessed for it.

## Concepts

The Concepts view lists every registered concept on the connected
cluster, grouped by domain, read from the engine's own registry. A
concept added to the DSL appears with no extension update.

Click a concept to open its browser tab: rows on the left, detail on the
right.

- Rows are labelled using whatever `@displayCard` slots the concept
  declares. A concept that declares none falls through to the stated
  fallback contract -- a title inferred from a `name` / `title` / `label`
  field, a status inferred from a lifecycle field, the row id when neither
  is present. See
  [display-cards.md](../concepts/display-cards.md).
- **Load more** pages through the keyset cursor; a concept larger than one
  page is fully walkable.
- Selecting a row shows its full nested shape -- payload, provenance and
  intrinsics -- with no flattening, so the intrinsics stay visible.
- The list updates live as rows are created, updated or deleted.

There is no concept-specific rendering anywhere in the panel. That is
deliberate: it is what makes a newly declared concept work the day it is
declared.

## What this panel does not do yet

Executing constructs, running automations, and driving deployments are
later increments.
