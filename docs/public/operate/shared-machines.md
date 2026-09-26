---
title: Sharing a machine
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Sharing a machine

Lending a machine you own — to named colleagues, to a group, or to everyone
on the cluster — so their work runs on hardware somebody already paid for
instead of on a metered API. Lent to everyone, it also serves the cluster's own
work: automations, maintenance, anything with no acting human behind it. Epics
[memql#5146](https://github.com/znasllc-io/memql/issues/5146) (sharing) and
[memql#5344](https://github.com/znasllc-io/memql/issues/5344) (sharing with
people and groups).

This is the one place in the product where your hardware answers to somebody
else, so the whole design is about consent and about what the lender is
entitled to know afterwards. Sharing lends a machine's **models**, never its
shell: tool dispatch and app sessions stay yours alone.

---

## Three answers, and only the owner gives them

| In Fleet | Stored as | Who the machine serves |
|---|---|---|
| **Only me** | `owner` (the default, and the absent case) | its owner |
| **Specific people and groups** | `people`, with `userIds` and `groupIds` | the listed people, and the **active** members of the listed **active** groups |
| **Everyone in this cluster** | `cluster` | everyone signed in to this cluster, **and** the cluster's own work |

The consent lives on the machine's own row, `v1:worker:registration.sharing`:
`{mode, userIds, groupIds, sharedAt, sharedBy}`, with ids stored bare. It is a
**closed** block — a misspelled key is refused rather than stored beside the
real one — and it is written only by `fleetSetSharing`, which resolves the
machine through the caller's own machines (see [Only the owner may lend a
machine](#only-the-owner-may-lend-a-machine)).

The lists mean something only under `people`, and changing to either other
mode **clears them**: a share you took back leaves no names behind for a later
reader to honour by mistake. Every reader maps a value it does not know to
`owner`, so an engine older than a mode reads it as private rather than as
open, and a `people` share that names nobody is read as `owner` too.

Group membership is read **on every call**, from the same membership rows that
decide what a person can see elsewhere in the product. Removing somebody from
a group, or archiving the group, ends their use of the machine on their next
call, on every replica. Nothing is cached.

---

## Two consents, and neither is sufficient

A machine serves anybody but its owner only when **both** of these are true:

| Consent | Where it lives | Who gives it |
|---|---|---|
| The owner is willing to lend it | `sharing.mode` is `people` or `cluster` | the machine's owner, at Fleet → the machine → Sharing |
| The machine is willing to serve | `inference.serve: cluster` in that machine's `policy.yaml` | whoever administers the machine |

They are deliberately not one setting, and the split is not ceremony:

- **The owner's half is a decision about people.** It says *these colleagues
  may have my hardware*. It is a graph row, so it survives the machine being
  asleep, reinstalled or renamed, and it can only be set by the owner —
  never by the machine.
- **The machine's half is a decision about the machine.** It says *this box
  is somewhere it can serve anyone but its owner* — plugged in, on a network
  that allows it, not a laptop about to be carried into a meeting. Only the
  machine can know that, which is why it is a file on the machine rather than
  a checkbox in a browser. It keeps its two values: `cluster` means "may serve
  people other than my owner", and **which** people is the owner's half.

The Fleet page renders **both**, always, each with its own repair, because a
machine that is not serving has exactly one reason and the person looking at
it needs to know which. A refusal from the engine names the missing half for
the same reason:

| What is missing | What you are told |
|---|---|
| both | "Neither consent is given: the owner has not shared this machine with the cluster, and its cockpit's `policy.yaml` does not set `inference.serve` to `cluster`. Both are needed." |
| the owner's | "The machine's cockpit is willing to serve the cluster, but its owner has not shared it. The owner turns this on from the machine's page in Fleet." |
| the machine's | "The owner has shared this machine, but its cockpit is not willing to serve the cluster. Set `inference.serve` to `cluster` in that machine's `policy.yaml` -- it is a decision about where the machine is, and only the machine can make it." |
| you are not on the list | "Its owner has shared it with specific people, and not with you." |
| you are on the list, the machine has not agreed | "Its owner has shared it with you, but its cockpit is not willing to serve anyone but its owner. Set `inference.serve` to `cluster` in that machine's `policy.yaml` ..." -- counted as "1 machine lent to you is waiting on its own consent" |

A refusal about somebody else's machine is **counted, never named** (epic
memql#5327, D12): a person whose call ran out of options learns that machines
exist that are not open to them, and never which ones.

### What this replaces

The `sharedInference=true` **operator label**. Pre-release, with no shim: a
machine that reports `sharedInference` in its own labels is not selected, and
a test asserts it. The label could say only one thing, and it could not say
*which* consent was missing — so a machine that had been lent and then
carried onto a train looked identical to one whose owner had never offered
it.

---

## Who you can lend it to

The share dialog offers what you can already account for, and no more:

- **The active groups you are an active member of**, each with its count of
  active people, and
- **the active people in those groups**, by display name.

If you may already read every person on the cluster — `read` on `principal`,
which owners, developers and admins hold by default and a grant can give or
take away — it offers every active person, with their email, and every active
group. Nobody else is ever handed an email address.

That is `fleetShareDirectory(registrationId)`, and it answers only a machine's
owner, about one of their own machines: a person with no machine cannot list
anybody through it, and another person's machine id answers exactly as a
made-up one does.

`fleetSetSharing` holds to the same answer. Under `people`:

- a person or group **not already on the list** must be one the directory
  offers you, and **one sentence** refuses both an id that does not exist and
  one you may not pick, so the write is never a way to learn who is on the
  cluster;
- one **already on the list** is not re-checked. Somebody who left your group
  stays on your list until you remove them; the dialog names them, marked as
  no longer available to pick, and you remove them by saving without them;
- your own id is dropped rather than refused — your own machine is yours
  already;
- a share names at least one person or group, and at most 50. A group is the
  way to lend a machine to more people than that.

---

## The cluster's own work

System work — automations and cluster maintenance, with no acting person or
under the cluster's own synthetic identity (`system:automation:<name>`,
`system:maintenance:<name>`) — reaches **only a machine lent to everyone**. A
machine shared with specific people never serves it, whatever its list says: a
list of names is consent for those people, and the cluster's own work is
nobody on the list. The routing plan says so in as many words: "Its owner has
shared it with specific people, not with the cluster's own work."

---

## What the owner sees afterwards

One sentence a week, and it is the whole surface:

| This week | The sentence |
|---|---|
| nothing ran | "No calls have run on this machine this week." |
| only your own calls | "Served 41 calls this week, all of them yours." |
| some were for other people | "Served 41 calls this week, 12 of them for 2 other people." |
| ... and some were the cluster's own work | "Served 41 calls this week, 12 of them for 2 other people and 3 for the cluster's own work." |
| none were yours | "Served 12 calls this week, all for 2 other people." |

That is the ledger, and what it leaves out is the point. Somebody who lends
their machine is entitled to know it is **being used** — and how much of its
week went to other people, which is the question a lender actually asks — but
not **what it was used for**. So the narrowing happens in the engine, at the
row read, which is the last point where the full decision record is in hand
and the first where the promise can be kept — not in a renderer that a later
change could quietly widen.

The cluster's own work is every call with no acting person, or with the
cluster's synthetic identity (an automation or a maintenance sweep) — never
"another person". The type the fold is built on cannot express a prompt. Other
people are **counted and never named**: the row carries `calls`, `people`,
`otherCalls`, `otherPeople`, `systemCalls` and calls per level, and nothing
else. With a one-person share the count of other people implies that person,
which is inherent to any count — it tells you they used the machine, never
what for.

### A ledger that could not be read is not an empty ledger

If the read fails, the answer is not zero:

> This week's usage could not be read. It is not that nothing ran — nobody
> looked.

Telling somebody who lent their machine that nobody used it is a specific
claim, and a failed read is not evidence for it. The page renders that answer
as a notice rather than as a quiet caption, because a count of zero and a
question that went unanswered read alike in the same typeface.

---

## What a person it is lent to sees

The machine's models appear in their catalog — in Fleet's model library, and
in every place a policy chain asks whether a local model is available — with
the facts routing uses about the machine: its name, whether it is online and
how busy, its memory, platform and runtimes. Those are the same catalog fields a
machine lent to everyone already shows everyone. They read no registration row,
so they do not see its labels, history, hardware inventory or who else it is
lent to.

Before epic memql#5344 a person's catalog listed **their own** machines only,
so somebody whose only route was a colleague's machine was told no local model
was available before the plan that could have served them ever ran. The
catalog now lists their own machines and every machine lent to them, and
decides availability from both.

---

## What stopping does

Stopping — or removing somebody from the list, or from a group on it — takes
effect **on the next call**. A call already running on the machine finishes.

That is not a limitation to be fixed later. Stopping mid-answer would lose
work somebody is waiting for and would change nothing about the prompt
already sent — the machine has it either way. The act says so at the moment
it is offered, rather than in this document.

---

## Who may use whose machine

- **The pairing user reaches their own machine without sharing.** Adding a
  machine to Fleet unlocks local inference on it for that user's Ask,
  Materialize, and Nexus work. Sharing is not a gate on your own hardware.
- **Another person needs both consents, and needs to be lent the machine.**
  A call for user B lands on user A's machine only when A shared it with
  everyone, with B, or with a group B is an active member of — and the
  machine itself agreed. Machines lent to you may serve your calls, with your
  own machines always preferred first.
- **System work** reaches only machines lent to everyone, with both consents.

When a person's own machines and a lent one could both serve a call, **the
person's own machines come first, always**, and it is not a preference an
operator can turn off. Three reasons, and the third is the one that gets
missed: your own machine is the one you are paying for and whose load you can
see; a lent machine belongs to somebody who agreed to *help*, not to be the
default, and burning their GPU while your own laptop sits idle is a poor way
to treat the offer; and it keeps the common case **unchanged** — somebody
with their own machine sees exactly the routing they saw before sharing
existed, so a fleet that starts sharing does not silently reroute work that
was already working.

It is applied as a **stable partition, not a sort key**. Within each half your
routing policy still decides — `roundRobin` still rotates, `leastLoaded` still
rations. Own-first is a boundary between two groups rather than a new
comparator inside them.

**Your routing policy does not cross that boundary.** The lent half is not
re-ordered by it, because a routing policy expresses how *you* want *your*
machines used, and applying it to somebody else's machine would let a
preference travel across the ownership line this whole feature exists to hold.

Three degradations are deliberate, and all of them narrow rather than fail. A
node whose store cannot read across owners serves your own machines and says
nothing about anyone else's. A cross-owner read that *fails* does the same and
logs it: your own laptop being able to serve the turn should not be refused
because a read about somebody else's machine went wrong. And a membership read
that answers nothing admits nobody through a group, while people listed by
name are still served.

### It works when the machine is on another replica, and for a year it did not

A machine's stream terminates on **exactly one** agent replica, and the turn
that wants it is served wherever the mesh routed the request — at the default
two replicas, a coin flip. The router picked the shared machine correctly and
the receiving replica then resolved it through *the caller's own* machines,
which by construction cannot contain somebody else's. So the call was refused
`registration_refused` and surfaced as `no_local_model_available` — the
sentence that means "your fleet is asleep", told to somebody looking at a lent
machine they could see was on.

It worked exactly when the stream happened to be local. Fixed in epic
[memql#5327](https://github.com/znasllc-io/memql/issues/5327) design D10: the
receiving replica admits a machine that is the caller's own **or** unrevoked
and lent to them, resolved through the same cross-owner read the router used
to pick it. Both consents are still checked, on the receiver as well as in the
router — and for a machine lent to people, the receiver asks about the
**verified** caller and reads their groups **itself**. Nothing in the
forwarded envelope says who is in which group.

**Tool dispatch is deliberately not widened.** Sharing a machine lends its GPU,
not its shell: `workerHost` stays owner-only across the hop exactly as it is
locally.

### Only the owner may lend a machine

The sharing consent is written through `fleetSetSharing`, which resolves the
machine through the **caller's own** machines. The underlying mutation is
`@serverOnly`.

That is a narrowing, and it closed a real hole (memql#5327, finding M-2). The
mutation's own comment asserted that the row's write guard refused any actor
but the owner. It does not: `v1:worker:registration` declares the composite
`owner=..., clusterOwner` tier, and the guard grants the cluster-owner escape
on it — so a cluster owner could lend hardware they do not own, and un-lend
hardware somebody else had lent. On the one row whose entire content is a
person's consent, that escape is wrong.

Removing a machine keeps the cluster-owner arm, and the difference is the
point: offboarding somebody's laptop is an operator act the Fleet's operator
view already implies, while giving their hardware to somebody is not.

---

## Related

- [Local models on the fleet](local-models.md) — the machine class, the
  recommended set, and what a probe measures
- [Workers runbook](workers-runbook.md) — pairing a machine, tokens, scope
- [AI routing](ai-routing.md) — levels, policies and rules; where a shared
  machine sits in a chain


## Re-pairing the same machine

Each cockpit install keeps a stable `machineId` (under its state directory) and
sends it on every Register. When you mint a new worker token for a machine that
is already registered, the cluster **rebinds** that registration to the new
token instead of creating a second row for the same install. Display names and
hostnames are not the identity — two MacBooks can share a hostname.
