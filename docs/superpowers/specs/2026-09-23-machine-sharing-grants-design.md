# Machine sharing grants -- design record

- **Date:** 2026-09-23
- **Epic:** memql#5344 (tasks #5345-#5350). One PR for all six.
- **Source:** the 2026-09-13 connection-layer audit on memql#5327, finding M-1 and
  rule 2 ("share the MACHINE, never the data").
- **Builds on:** epic memql#5146 D6 (two consents, the ledger) and epic
  memql#5327 D10 (shared inference across the replica hop), D11 (the ledger read
  is `@serverOnly`), D12 (refusals never enumerate foreign machines) and the M-2
  ruling (only a machine's owner may lend it). None of those is reopened here.
- **Status:** G1-G4 were chosen by the owner in the 2026-09-23 brainstorm. G5-G15
  are this record's rulings, for the owner to overturn.

## Why

A machine could be lent to the whole cluster or to nobody. "The Mac Studio in the
office serves the design team" was not expressible, so an owner who wanted to help
three colleagues had to offer the machine to everyone -- clients included -- and to
every automation the cluster runs. The audit recorded it as M-1: the share set is the
whole cluster, and system work rides it.

## Locked decisions

| # | Decision | Ruling |
|---|---|---|
| G1 | Where the share list lives | On the machine's own row. `registration.sharing.mode` gains `people`, with `userIds` and `groupIds` beside it. Written only by the machine's owner through `fleetSetSharing`, exactly as today. Not a `machine:<id>` resource on `v1:rbac:grant` |
| G2 | Who the owner can pick | People who share an active group with the owner, and those groups. A caller who may already read every person -- `read` on `principal` through `auth.CapableFor`, which owner, developer and admin hold by seed and a grant can give or withhold -- sees every active person and every active group. (First implemented as a rank floor; the review showed a rank hands the roster to an admin with a deny grant, so the capability is the owner's rule, "anyone who can already see everyone", stated exactly) |
| G3 | The cluster's own work | Only a machine shared with **everyone** (`cluster`) serves a call with no acting person. A `people` share never does |
| G4 | The ledger | Counts split by who the calls were for: "Served 41 calls this week, 12 of them for 2 other people." Still counts only, never names |
| G5 | The cockpit's consent keeps its two values | `inference.serve: cluster` now means "may serve people other than its owner"; the owner decides which. No cockpit release |
| G6 | Resolution is per call, from the rows | A person's groups come from the membership source the account scope and the grant resolver already read: active membership in an active group. Resolved lazily, only when a candidate carries a group share. Nothing is cached, so removal from a group ends access on the next call, on every replica |
| G7 | One predicate | `workerservice.ServesPerson(sharing, cockpitServe, userId, groupIds)`. Every reader that admits a shared machine for a PERSON asks it; system work asks `ServesTheCluster`, which is unchanged |
| G8 | A person's catalog includes machines shared with them | Measured before fixing (test first). Before this, `Catalog(ctx, user)` read the person's own machines only, so a person whose only route was a colleague's shared machine was refused at the router before the shared plan could run |
| G9 | The write validates NEW subjects only | A subject not already on the row must be in the caller's directory. One refusal sentence for "does not exist" and "not yours to pick", so the write is not an oracle. The owner is dropped from the list, duplicates collapse, at most 50 subjects, and `people` needs at least one |
| G10 | Changing mode clears the list | `owner` and `cluster` store empty lists. A saved but inactive list is residue a later reader could honour by mistake. The dialog keeps its draft while it is open |
| G11 | `sharing` becomes a closed nested block | `mode`, `userIds`, `groupIds`, `sharedAt`, `sharedBy`. A block holding a consent must refuse a misspelled key (memql#3641). Rows written before this carry only `mode`, `sharedAt` and `sharedBy`, so they still validate |
| G12 | "Not shared with you" stays aggregated | It joins D12's count. A parity test pins every sharing refusal sentence as foreign noise |
| G13 | Mixed versions fail closed | Every existing reader maps anything but `cluster` to `owner`, so during a rollout a `people` machine is private, never open |
| G14 | What a person the machine is shared with sees | The machine's models in their catalog and model library, with the facts routing uses about it: its name, whether it is online and how busy, its memory, platform and runtimes -- the same catalog fields a machine lent to everyone already shows everyone. They read no registration row: not its labels, history, hardware inventory or who else it is lent to |
| G15 | Attention | A runtime marker, published only to a person who owns an unrevoked machine, at that machine's Sharing view |

## 1. The data model (#5346)

```memql
sharing {
  mode      enum("owner", "people", "cluster")
  userIds   []string
  groupIds  []string
  sharedAt  datetime
  sharedBy  string
}
```

- `owner` is the default and the absent case: the machine serves its owner and nobody
  else. `cluster` is everyone. `people` is exactly the listed users plus the ACTIVE
  members of the listed ACTIVE groups.
- The lists are meaningful only under `people` (G10). Ids are stored BARE -- the
  client contract's spelling -- whatever spelling the caller sent, and every
  comparison is bare/canonical tolerant using `ParseNodeId`'s rule (the text after
  a `v<N>:domain:entity` prefix, the whole value otherwise), never "the text after
  the last colon".
- `setWorkerSharing` stays `@serverOnly` and gains `userIds` and `groupIds`;
  `fleetSetSharing` is its only renderer.
- In `component/worker`, `Sharing` gains `UserIds` and `GroupIds`, and
  `SharingFromRow` reads them only under `people`. `Admits(userId, groupIds)` is the
  owner half: `cluster` admits any person; `people` admits a listed user or a member
  of a listed group; `owner` admits nobody.

## 2. Resolution (#5347)

`ServesPerson` = the cockpit said `cluster` AND `Sharing.Admits(person)`. Four places
admit a shared machine for a person, and each now asks it:

| Where | Before | After |
|---|---|---|
| `fleetcatalog.Reader.Catalog(ctx, user)`: availability and `fleet:strongest` / `fleet:fastest` expansion | own machines only | own machines, plus every machine where `ServesPerson` holds |
| `Router.PlanUserModelWithShared` | `ServesCluster()` | `ServesPerson(user, groups)`; own-first and the stable partition unchanged |
| `ForwardHandler.verifySharedRegistration` (the replica hop) | `ServesCluster()` | `ServesPerson` for the VERIFIED authority's subject, with groups resolved on the receiving replica |
| `ForwardHandler.ownerOfSharedMachine` | unchanged | unchanged. It checks the live stream against the row, not consent |

Where the groups come from: an injectable `GroupResolver` on the Router, the
ForwardHandler and the catalog reader. It defaults to `auth.InstalledMembershipSource()`,
which every node installs at engine start, and a nil source means no groups. That
narrows: listed users and `cluster` shares still work, and group shares do not.

Tool dispatch (`WorkerForward`, `workerHost`) and app sessions stay owner-only (D10):
sharing lends the GPU, never the shell.

## 3. System work (#5348)

`PlanSharedModel`, `Catalog(ctx, "")` and `fleetEntry` with no acting user keep asking
`ServesTheCluster`, which requires `mode == cluster`. A `people` machine is
rejected with its own sentence: "Its owner has shared it with specific people,
not with the cluster's own work."

**A synthetic actor is the cluster's own work too.** Automations run as
`system:automation:<name>` and maintenance as `system:maintenance:<name>`, so their
calls carry a non-empty id and take the PERSON path. `Sharing.Admits` answers
`auth.IsSystemActorId` with "only under `cluster`", so no such actor is ever on a
people list by construction (the review found a last-colon comparison could match
`system:automation:ana` to a listed `ana`), and the ledger counts their calls as
the cluster's own work. One consequence of G8 follows and is intended: with the
catalog now reading machines lent to the caller, an automation under a synthetic
actor reaches machines lent to EVERYONE, which D6 always said system work should
and the own-only catalog had silently prevented.

## 4. The directory and the write

`fleetShareDirectory(registrationId)` is a new `@sdk` builtin. It resolves the machine
through the caller's OWN machines (the `modelPullMachineFor` path), so it answers
only for an owner, about one of their machines, and a person with no machine
cannot enumerate anyone through it. It returns:

- `people`: `{id, name, detail}`. Everyone active for a caller at admin rank or
  above, with `detail` the email they already see in Users. Otherwise the ACTIVE
  members of the caller's ACTIVE groups, with no email.
- `groups`: `{id, name, members}`. Every active group at admin rank or above;
  otherwise the caller's own active groups.
- `current`: the subjects already stored on this machine, named even when they have
  left the directory, with `inDirectory` false so the dialog can mark them.

The reads are direct `MemoryNodes` reads beside `activeGroupIdsForUser`, each with a
`staged-data:` verdict. The group and membership reads are MUST-NOT-GATE: the
directory must offer exactly the set the resolver honours.

`fleetSetSharing(registrationId, mode, userIds, groupIds)` applies G9 and G10 and
answers a receipt whose sentence names the cockpit's half when it is still missing.

## 5. The OS (#5349)

The machine's Sharing view becomes one panel with a dialog behind one button.

- **State**, in one line: "Only you", "Shared with Ana Ruiz and Design (4)", or
  "Everyone in this cluster", plus the two consent lines (unchanged in meaning,
  reworded for `people`).
- **Dialog**: three choices: **Only me**, **Specific people and groups**, **Everyone
  in this cluster**. Under the second, a search over the directory, with chosen
  subjects as removable chips. A stored subject no longer in the directory is shown
  and removable, marked "no longer available to pick". It says what the owner will
  and will not see, and that stopping takes effect on the next call.
- **Save** is disabled until something changed. A refusal keeps the draft and shows
  the engine's sentence (the supervision rules in SUPERVISED-VISUAL-COMPOSITION.md).
  Success appears only after the write succeeds.
- **The ledger line** shows for `people` as well as `cluster`, as the engine's sentence.
- The workspace's anchor link reads "Personal inference" or "Shared inference"
  from the owner's mode and the cockpit's consent together.
- Keyboard: the dialog traps focus, Escape closes, and focus returns to its trigger.
  Every action is reachable without a pointer.

## 6. The ledger (#5349)

`FoldLedger` takes the machine's owner, and the entry gains `otherCalls`,
`otherPeople` and `systemCalls`. System calls are rows with no acting person. The
sentence:

| Case | Sentence |
|---|---|
| none | "No calls have run on this machine this week." |
| all yours | "Served 41 calls this week, all of them yours." |
| mixed | "Served 41 calls this week, 12 of them for 2 other people." |
| with system | "... and 3 for the cluster's own work." |
| all others | "Served 12 calls this week, all for 2 other people." |

Still counts: the rendered row is walked by `TestTheLedgerCarriesNoPromptContent`, and
nothing names a person. With a one-person share the count implies that person. That
is inherent to any count, and the owner chose the list.

## 7. Failure modes

- A group is archived or a member removed: that person's next call is refused. A
  call in flight completes (the D6 rule).
- The membership read fails: no groups, which narrows to listed users and `cluster`.
- A stored subject was deleted: it admits nobody. The dialog shows it as unknown and
  offers removal.
- A mixed-version rollout: G13.
- A machine is shared with people whose cockpit says `owner`: nobody else is served,
  and the panel says which half is missing, as today. A person ON the list whose
  call is refused for it gets their own count line -- "1 machine lent to you is
  waiting on its own consent" -- rather than the foreign "not shared with you",
  which would be false and would send them to the owner for the machine's repair.

## 8. Testing (#5350)

Every ruling lands with a test that fails against the code it replaces:

- `component/worker`: `Admits` and `ServesPerson` across mode x consent x
  user/group/neither, with bare and canonical ids, and the refusal sentences.
- `fleetcatalog`: a person's catalog includes a machine shared with them and excludes
  one shared with someone else (the G8 regression, test first).
- `integrations/agent/worker`: the shared plan admits exactly the listed people; own
  machines still come first; system work never reaches a `people` machine; the hop
  admits a listed person and a group member resolved on the receiving replica, and
  refuses everyone else.
- `component/memql`: the ledger fold and sentence table, and the directory and the
  write (db-gated, including a row written before this change surviving a
  heartbeat-style update under the closed block).
- `clients/os`: the panel and dialog: modes, search, chips, stale subjects, refusal
  with the draft kept, keyboard and focus, and the attention destination.

## 9. Out of scope

Expiring shares; sharing tool dispatch or app sessions; a "shared with me" machine
list for the person a machine is lent to (G14); notifying people when they are added;
any cockpit change.
