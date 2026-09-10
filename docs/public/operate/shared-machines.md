---
title: Sharing a machine with the cluster
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Sharing a machine with the cluster

Lending a machine you own to the people you work with, so the cluster's own
work — automations, maintenance, anything with no acting human behind it —
can run on hardware somebody already paid for instead of on a metered API.
Epic [memql#5146](https://github.com/znasllc-io/memql/issues/5146).

This is the one place in the product where your hardware answers to somebody
else, so the whole design is about consent and about what the lender is
entitled to know afterwards.

---

## Two consents, and neither is sufficient

A machine serves the cluster only when **both** of these are true:

| Consent | Where it lives | Who gives it |
|---|---|---|
| The owner is willing to lend it | `v1:worker:registration.sharing.mode = "cluster"` | the machine's owner, at Fleet → Machines |
| The machine is willing to serve | `inference.serve: cluster` in that machine's `policy.yaml` | whoever administers the machine |

They are deliberately not one setting, and the split is not ceremony:

- **The owner's half is a decision about people.** It says *these colleagues
  may have my hardware*. It is a graph row, so it survives the machine being
  asleep, reinstalled or renamed, and it can only be set by the owner —
  never by the machine.
- **The machine's half is a decision about the machine.** It says *this box
  is somewhere it can serve* — plugged in, on a network that allows it, not
  a laptop about to be carried into a meeting. Only the machine can know
  that, which is why it is a file on the machine rather than a checkbox in a
  browser.

The Fleet page renders **both**, always, each with its own repair, because a
machine that is not serving has exactly one reason and the person looking at
it needs to know which:

> - Shared by its owner Sep 5, 2026, 02:00 AM.
> - Its cockpit is not willing to serve the cluster. Set `inference.serve` to
>   `cluster` in that machine's `policy.yaml` — it is a decision about where
>   the machine is, and only the machine can make it.

A refusal from the engine names the missing half for the same reason, and
there are **three** sentences rather than one flag, because there are three
situations and they send you to three different places:

| What is missing | What you are told |
|---|---|
| both | "Neither consent is given: the owner has not shared this machine with the cluster, and its cockpit's `policy.yaml` does not set `inference.serve` to `cluster`. Both are needed." |
| the owner's | "The machine's cockpit is willing to serve the cluster, but its owner has not shared it. The owner turns this on from the machine's page in Fleet." |
| the machine's | "The owner has shared this machine, but its cockpit is not willing to serve the cluster. Set `inference.serve` to `cluster` in that machine's `policy.yaml` -- it is a decision about where the machine is, and only the machine can make it." |

A single "not shared" would send half the operators to the wrong machine, and
the one who owns the laptop would go looking on a web page for a setting that
lives in a file on their own disk.

### What this replaces

The `sharedInference=true` **operator label**. Pre-release, with no shim: a
machine that reports `sharedInference` in its own labels is not selected, and
a test asserts it. The label could say only one thing, and it could not say
*which* consent was missing — so a machine that had been lent and then
carried onto a train looked identical to one whose owner had never offered
it.

---

## What the owner sees afterwards

One sentence a week, and it is the whole surface:

> Served 41 calls for 3 people this week.

That is the ledger, and what it leaves out is the point. Somebody who lends
their machine is entitled to know it is **being used**; they are not entitled
to read **what it was used for**. So the narrowing happens in the engine, at
the row read, which is the last point where the full decision record is in
hand and the first where the promise can be kept — not in a renderer that a
later change could quietly widen.

The type the fold is built on cannot express a prompt. Counting people uses
the acting user's id and never renders it: naming the three would tell the
machine's owner who is using their hardware for what, which is a different
disclosure wearing the same sentence.

**"Served 41 calls for 1 person" when that person is you reads as a
stranger**, so the one-person case drops the count of people entirely.

### A ledger that could not be read is not an empty ledger

If the read fails, the answer is not zero:

> This week's usage could not be read. It is not that nothing ran — nobody
> looked.

Telling somebody who lent their machine that nobody used it is a specific
claim, and a failed read is not evidence for it. The page renders that answer
as a notice rather than as a quiet caption, because a count of zero and a
question that went unanswered read alike in the same typeface.

---

## What stopping does

Stopping takes effect **on the next call**. A call already running on the
machine finishes.

That is not a limitation to be fixed later. Stopping mid-answer would lose
work somebody is waiting for and would change nothing about the prompt
already sent — the machine has it either way. The act says so at the moment
it is offered, rather than in this document.

---

## Who may use whose machine

Two properties, both security-load-bearing, and they are different rules:

- **The pairing user reaches their own machine without share.** Adding a
  machine to Fleet unlocks local inference on it for that user's Ask,
  Materialize, and Nexus work. Both consents (`sharing.mode = cluster` and
  `inference.serve: cluster`) are required only when a *different* account
  uses the machine. The owner-scoped read is caller-scoped; share is not a
  gate on your own hardware.
- **Another account needs both consents.** A call for user B never lands on
  user A's private machine. Machines with both consents may serve other users'
  calls, with the caller's own machines always preferred first.
- **System work** — automations and cluster maintenance, with no acting user
  — reaches only machines where both consents are present.

When a person's own machines and a shared one could both serve a call, **the
person's own machines come first, always**, and it is not a preference an
operator can turn off. Three reasons, and the third is the one that gets
missed: your own machine is the one you are paying for and whose load you can
see; a shared machine belongs to somebody who agreed to *help*, not to be the
default, and burning their GPU while your own laptop sits idle is a poor way
to treat the offer; and it keeps the common case **unchanged** — somebody
with their own machine sees exactly the routing they saw before this epic, so
a fleet that starts sharing does not silently reroute work that was already
working.

It is applied as a **stable partition, not a sort key**. Within each half your
routing policy still decides — `roundRobin` still rotates, `leastLoaded` still
rations. Own-first is a boundary between two groups rather than a new
comparator inside them.

**Your routing policy does not cross that boundary.** The shared half is not
re-ordered by it, because a routing policy expresses how *you* want *your*
machines used, and applying it to somebody else's machine would let a
preference travel across the ownership line this whole feature exists to hold.

Two degradations are deliberate, and both narrow rather than fail. A node
whose store cannot read across owners serves your own machines and says
nothing about anyone else's. A cross-owner read that *fails* does the same and
logs it: your own laptop being able to serve the turn should not be refused
because a read about somebody else's machine went wrong.

---

## Related

- [Local models on the fleet](local-models.md) — the machine class, the
  recommended set, and what a probe measures
- [Workers runbook](workers-runbook.md) — pairing a machine, tokens, scope
- [AI routing](ai-routing.md) — levels, policies and rules; where a shared
  machine sits in a chain
