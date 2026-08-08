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
| Red error icon | Connection failed; hover for the message |
| Yellow warning | Not configured -- no endpoint, or no credential |
| Hollow circle | Configured, not connected |

**Add Cluster** and **Edit Cluster** collect a name, domain, endpoint and
PAT. Writes preserve comments and any field a newer cockpit wrote,
because the file is shared.

### Authentication

This panel authenticates with the `pat` field. Mint a token at the
identity binary's `/me/tokens`.

A cluster configured for OIDC with no PAT reports that it must be
authenticated in the memQL Cockpit first -- the panel cannot yet read the
cockpit's keyring credential store.

## Concepts

The Concepts view lists every registered concept on the connected
cluster, grouped by domain, read from the engine's own registry. A
concept added to the DSL appears with no extension update.

Click a concept to open its browser tab: rows on the left, detail on the
right.

- Rows are labelled using whatever `@displayCard` slots the concept
  declares, falling back to the row id when it declares none.
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
