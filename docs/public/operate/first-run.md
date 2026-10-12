---
title: What an owner sees first
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# What an owner sees first

A fresh cluster boots ownerless. Somebody claims it through the identity
service's `/setup` page and a magic link, signs in — and MemQL OS does not open
a desk. It holds them on **the core gate** until this cluster can reach a
model.

Once it can, the gate lifts by itself and the desk opens, carrying **Ask** and,
while anything is still outstanding, the **Set up** card.

## The gate

It renders in place of the desk. Behind it there is no app, no dock and no
launcher: they are not hidden, they are not mounted.

**It keys on inference alone.** Storage is always configured on a local cluster
and mail is log-only there by design, and neither is needed to use the OS. The
other core stops are on the rail, unlit and non-blocking, and they follow the
Set up card onto the desk once the gate lifts.

**Nothing dismisses it and nothing is remembered in a browser.** The readiness
verdict is the state, so there is no Skip and nothing to skip past. It lifts on
a feed change with no reload — including for somebody who cannot act on it and
is simply waiting.

**A door that is configured but not live still lifts it.** A machine that
serves a qualifying model but is asleep is a configured door: the OS opens, and
a call that needs it parks with a report naming the machine. Configuration
opens the OS; presence decides a call.

### Two variants

An **owner** or a **developer** gets the rail of core stops, with the inference
stop already open on its three doors, and Sign out under a rule beneath it.

Everybody else gets one sentence — *An owner or developer has to set up
inference before anyone can use it* — a line saying the page opens by itself
once they have, and Sign out. **It does not name the owner.** A person's name
is not the cluster's to publish, and the sentence is exactly as actionable
without one.

### What it does when it cannot say

Three silences, and only one of them holds anybody.

| The feed says | The gate |
|---|---|
| nothing yet — it has not loaded | draws nothing, and the shell opens |
| `unreported` — no live node reported inference | draws nothing, and the shell opens |
| `configured`, `partial` | draws nothing, and the shell opens |
| `unconfigured` | holds |

**A broken cluster is not an unconfigured one.** `unreported` means no live
node answered, which is a different problem with a different fix — and a gate
that held on it would lock somebody out of the Cluster app they need in order
to go and find out why nothing is reporting.

The one exception is the role ladder: while the cluster is unconfigured AND the
ladder has not landed, nothing is drawn at all. Which variant to show is not a
question a wrong answer gets nearly right, and telling an owner to go and find
an owner is worse than a beat of empty ground.

## The Set up card, once the gate has lifted

It is the same rail, on the desk, for the stops that are still outstanding.

| Stop | What it is | Where it is set |
|---|---|---|
| **Your passkey** | A credential on your account that is not a link in your mailbox | The identity service, under Devices |
| **AI providers** | A model this cluster can reach | Fleet, or Settings → AI providers |
| **Storage** | Where files, materialized outputs, deploy bundles and log archives live | The deployment |
| **Email sender** | A mailbox this cluster can send from | Settings → Integrations |

The three modules come from the cluster's own readiness feed — the same reading
the Cluster app's Readiness section and every app's Set up group use — so a
stop lights up when the cluster says the module is configured, not when
somebody clicks a button saying they did it.

### Its presence is derived, never seeded

The card is not placed when a desk is created. An effect watches the readiness
feed and the role ladder, and:

- puts it on the **active** desk when the role admits it and a core stop is
  unsettled;
- retires it — after one beat with every mark lit — when every stop settles;
- puts it back **on any desk** the moment a stop unsettles again.

So "where did it go" has an honest answer, and so does "why is it back": a
module stopped being configured. Removing it by hand from its `...` menu works
like any widget, and it returns on the next change of the feed or the ladder,
because the card is the state rather than a note about it.

### The passkey comes first, and that is the only ordering

A sign-in link is the only way back into an account without a passkey, so
losing access to your mailbox loses the account. That is the one law on this
rail and the card states it in one sentence.

Everything after it may be done in any order. There is no Next, no Back and no
step number: the rail opens the first stop still outstanding, you may open any
other whenever you like, and finishing one moves the rail on by itself.

**On the gate, the inference stop opens first instead.** That surface exists
because inference is missing and nothing else on it lifts the gate. The
passkey-first law is untouched — the stop is still first on the rail and still
says why — but the disclosure sits on the one thing that will let the person
in.

### The inference stop offers three doors

Any combination is legal, and the order is the recommendation:

- **A machine on your fleet** — a computer you already own serves the model.
  Nothing leaves it and there is no bill. The act opens Fleet's guided install with local models already selected, so
  the person leaves with one install line and comes back when the machine
  registers and its checks settle.
- **Anthropic** — Anthropic serves the model, on their hardware and their bill.
  The act opens Settings → AI providers at the Anthropic panel.
- **OpenAI** — OpenAI serves the model, on their hardware and their bill.

A **developer** gets all three doors: Fleet's Machines section has no role
floor, and the AI providers section admits owner or developer. The rail offers
a button only where the registry says this actor can reach the section, and a
sentence naming who can otherwise, so it never offers an act that is not legal.

### What counts as a configured door

The `ai` verdict is computed from rows, identically on every node type:

- an unrevoked machine advertising a model that does structured output with a
  context window of at least 8192 tokens;
- an unrevoked machine with a known, allowed, signed-in local app;
- a federated cloud vendor whose identity ids are present.

Each is one lane on the readiness row, and each lane carries a second fact —
whether that door is open **right now**. The gate reads the first.

### Storage says where it is set, and does not pretend otherwise

Storage has no OS surface and none is invented for it. Its stop names the
variables that are still missing and says "Set in the deployment, not from
here". It is on the rail because five apps sit on it, not because the card can
configure it.

## Ask, before there is a door

Both entry points — the widget on the desk and the sheet the keyboard shortcut
opens — say the same sentence and offer no input while inference is
unconfigured. Neither sends a question, so neither can answer one with the
engine's own "no streaming provider available".

## What it does NOT cover

The core, and deliberately nothing else. Every app has its own **Set up** group
at the top of its Settings section — Email's unsubscribe secret, the GitHub
App, workbenches, local apps — and those stay there. This is what a cluster
needs before anybody gets useful work out of it, not everything a cluster can
be told.

**The gate is a surface, not an enforcement.** The engine's per-call refusals
are what actually stop work, and they are unchanged: a cluster with no
inference refuses the calls that need it whether or not anybody is looking at
this screen. What the gate stops is a person walking into a console whose every
feature will refuse them, with nothing on screen saying why.

## Related

- [Access model](auth/access-model.md) — the roles, and what each may do
- [Identity service](auth/identity-service.md) — passkeys, magic links,
  `/me/devices`
- [Settings, AI](ai-settings.md) — where the inference stop sends you, and
  what the four screens behind it do
- [Workers runbook](workers-runbook.md) — pairing a machine on your fleet
- [Environment variables](env-vars.md) — the deployment half of Storage
