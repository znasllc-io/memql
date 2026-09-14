---
title: Configuration readiness -- MemQL
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# Configuration readiness

MemQL knows, per module and cluster-wide, whether the thing behind an app is
configured, partly configured or not configured at all. An app whose module is
not set up shows a quiet setup surface instead of opening and refusing one read
at a time, its Settings entry carries a mark while a person is needed, and the
Set up group inside points at the one place each module is configured.

This page is for the operator who has to act on that.

## What a mark means

A coloured dot appears on an app's Settings entry, and on the gear in its title
bar, in exactly two cases:

| Mark | Meaning |
|---|---|
| Red | A module the app needs is **not set up**. Sections that need it show the setup surface |
| Amber | A module is **partly set up** or **set up on some nodes** only, or one the app merely wants is missing. Nothing is gated |

**No mark is the normal state, and it is also what you see while MemQL does not
yet know.** The readiness feed lands a moment after the shell does; until it
has, nothing is gated and nothing is drawn. That is deliberate -- a cluster
that is set up correctly must never flash a setup screen, so the shell shows
the app and corrects itself if it turns out to be wrong.

## Not reported is not not-set-up

A module's verdict is folded from what every live node reported. Four answers
say "we do not know" or "it depends", and none of them is **not set up**:

- *Not set up* means the nodes looked and the configuration is absent. There
  is something for you to do.
- *Not reported* means nobody answered. The nodes that host this module may
  all be down, may have just started, or the module may not be hosted anywhere
  in this cluster. There may be nothing wrong at all.
- *Could not check* means nodes answered and none of them could finish the
  check -- the read it depends on failed (a saturated database during a
  rollout is the usual cause). Each node retries on its own.
- *Set up on some nodes* means the nodes that checked disagree: some have the
  configuration and some do not. The Cluster app names which.

Nothing is gated on *not reported* or *could not check*, and no mark is drawn
for either. If you see one where you expect an answer, check that the node type
hosting the module is running -- `make status` locally, or the pods in the
namespace -- and read the Cluster app's Modules detail, which lists every node
with its own answer.

## Why two replicas can disagree

Every node evaluates every module for itself and writes its own row: at boot,
after a provider reload, after an integration is configured, when a machine is
paired, revoked or re-advertises what it serves, and on a safety-net pass every
ten minutes or so whether or not anything happened. A pass that could not
finish is retried on its own, backing off from two seconds to a minute. A row
is rewritten only when its answer changed, or when it is ten minutes old.

The verdict you see is those rows folded:

1. Reports from nodes that are not live are dropped. A node is live when its
   health is one of healthy, connecting, degraded or draining **and** it was
   seen within the last 60 seconds.
2. Reports of `notApplicable` -- a node that does not host the module -- are
   dropped.
3. Reports of `unknown` -- a node that could not evaluate the module because
   the read it depends on failed -- are set aside. They cast no vote, and the
   verdict names them. A module whose live reports are all `unknown` reads
   *Could not check*, never *Not set up*.
4. **A report that is behind the cluster is set aside.** Inference's two fleet
   doors are read from the machine registrations alone, so every node that
   checks them at the same moment agrees; a node whose answer about them
   differs from the most recent node's has simply not re-checked since
   something changed -- a machine paired or revoked. It is named as *catching
   up* and not counted. Not every node hears about a new machine at once, and
   the identity nodes never do; each catches up on its own next pass.
5. The worst remaining state wins.
6. **If the remaining reports disagree, the verdict is `partial`** and names
   every reporter. When some of them are set up it reads *Set up on some
   nodes*.

So mid-rollout, with one replica restarted onto a new configuration and one
not, you will see *Set up on some nodes*. That is the honest answer and it
resolves itself as the rollout completes. The Cluster app's Modules section
shows the same fold beside each module's own inventory row, and a module's
detail lists every live node with its own answer, how long ago it checked, and
-- quieter, and not counted -- the nodes catching up or unable to check.

## Where each module is configured

`configurableFrom` on each lane says whether a module is set from the OS or
from the deployment. The Set up group offers a button for the first and names
the variables for the second.

| Module | Set from | What it needs |
|---|---|---|
| AI providers | The OS, at Settings -> AI providers | A machine on your fleet serving an eligible model, or a federated cloud vendor |
| Email sender | The OS, at Settings -> Integrations | A mailbox this cluster can send from, through Microsoft Graph or SMTP |
| Storage | The deployment | `MEMQL_AZURE_BLOB_CONTAINER`, `MEMQL_AZURE_STORAGE_CONNECTION_STRING` |
| GitHub App | The deployment, on the identity node | `MEMQL_GITHUB_APP_ID`, `MEMQL_GITHUB_APP_SLUG`, `MEMQL_GITHUB_APP_CLIENT_ID`, `MEMQL_GITHUB_APP_CLIENT_SECRET`, `MEMQL_GITHUB_APP_PRIVATE_KEY_B64`, `MEMQL_GITHUB_APP_WEBHOOK_SECRET` |
| Campaign sending | The deployment, on the bff | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET`, `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` |
| Workbenches | The deployment, on the agent | `MEMQL_WORKBENCH_REMOTE`, `MEMQL_WORKER_PEERS` |
| Local apps | The deployment, on the agent | `MEMQL_MCP_PUBLIC_URL`, `MEMQL_NODE_BOOTSTRAP_TOKEN`, `MEMQL_IDENTITY_VERIFIER_BASE_URL` |

A module is **configured** when any one of its lanes is complete -- lanes are
alternatives, not requirements. A module with some required slots present and
no complete lane is **partly set up**.

Readiness reports presence and source only. **It never reads a value, and no
value is ever written to a readiness row**, so these rows are safe to read by
anyone signed in and safe to broadcast.

## A save is not an apply

Readiness reports the **applied** state. A provider credential saved but not
yet applied still reads as not configured, which keeps the AI providers
section's own "a save is not an apply" rule honest. Apply the change, or
restart the node, and the verdict follows within a moment.

The same holds for a developer configuring an integration: the write lands
immediately, but the recompute that refreshes the mark is owner-or-internal, so
a developer's mark catches up on the node's next safety-net pass -- within ten
minutes or so -- or the next providers reload, rather than instantly. Nothing
is lost; the configuration is already in effect.

## Adding a module

Modules are declared in the `modules:` block of `scripts/secrets/manifest.yaml`
and regenerated into the embedded copy with `make env-registry-sync`
(`make env-registry-check` gates the drift).

```yaml
modules:
  - name: storage
    core: true
    description: "Files, materialized outputs, deploy bundles and log archives live in blob storage."
    hostedBy:
      integrations: [storage]
    lanes:
      - name: azure-blob
        configurableFrom: deployment
        slots: [MEMQL_AZURE_BLOB_CONTAINER, MEMQL_AZURE_STORAGE_CONNECTION_STRING]
```

The rules, all enforced at boot rather than in review:

- **Every slot must name a registry entry.** A typo would otherwise read as a
  module nobody can ever configure.
- **A module declares lanes OR an evaluator, never both and never neither.**
  An evaluator is `inferenceStatus` (ask the provider registry which doors are
  open) or `integration:<name>` (ask that integration's own status capability).
- `hostedBy` names the integrations or node types that report on it; naming
  neither means every node evaluates it.
- `description` is operator-facing prose in one sentence, with no variable
  names in it. **The setup surface renders it verbatim**, so it is the sentence
  a person reads when an app will not open.
- `optionalSlots` count toward "somebody has started this", never toward
  completeness.

The shell carries a copy of the module ids and descriptions
(`clients/os/src/system/modules.ts`), pinned to this manifest by
`component/envregistry/os_modules_parity_test.go`, so a module added on one
side and not the other fails the build rather than shipping.

## See also

- [Environment variables](env-vars.md) -- the bootstrap envelope, and how to
  add, rotate or override an entry
- [Front door](front-door.md) -- the hosts and paths a cluster serves
