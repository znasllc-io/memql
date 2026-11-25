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
while anything is still outstanding, the **Set up** card. Until then, anybody
who is not an owner or developer of the cluster is kept out on a page that
says so (see [everybody else](#everybody-else-memql-is-not-set-up-yet)). Once
the cluster is set up everybody enters, and a person with no model access of
their own gets the desk without Ask (see
[Ask needs model access](#ask-needs-model-access)).

## The gate

It renders in place of the desk. Behind it there is no app, no dock and no
launcher: they are not hidden, they are not mounted.

**It asks one question: can this cluster reach a model?** Until it can, the
gate stands in place of the desk for everybody, and shows each person one of
two things.

- The cluster's **owners and developers** get the setup screen, because they
  are the ones who set it up.
- **Everybody else** gets the page *MemQL is not set up yet*, whatever model
  access of their own they have.

Being the owner or developer of an account does not make anyone one of the
first group; only the cluster's own owner and developer roles do.

### The setup screen: owners and developers

An **owner** or a **developer** gets the rail of core stops, with the inference
stop already open on its three doors, then a quiet line, *An API key of your
own lets you in, and nobody else*, with **Add an API key**, and Sign out under
a rule beneath it. **Add an API key** opens Settings > API keys straight into
its add wizard, and the screen steps aside while that window is open, as for
its other acts. Closing the window asks again whether a key of theirs can pay.

**It keys on inference alone.** Storage is always configured on a local cluster
and mail is log-only there by design, and neither is needed to use the OS. The
other core stops are on the rail, unlit and non-blocking, and they follow the
Set up card onto the desk once the screen lifts.

**Nothing dismisses it and nothing is remembered in a browser.** The readiness
verdict is the state, so there is no Skip and nothing to skip past. It lifts on
a feed change with no reload.

**A door that is configured but not live still lifts it.** A machine that
serves a qualifying model but is asleep is a configured door: the OS opens, and
a call that needs it parks with a report naming the machine. Configuration
opens the OS; presence decides a call.

**An API key that can pay lets an owner or developer past it.** Model access is
bring-your-own, so an owner or developer whose own key, or a key lent to them,
can pay for a vendor's models on a route they choose is not held, whatever the
cluster's verdict. The screen asks for their answer (`inferenceStatus`,
`keyAccount`) whenever it would show, and again when a window opened from it
closes. An answer it cannot read keeps the screen. A key lets nobody else into
a cluster that is not set up. ([API keys](api-keys.md))

### Everybody else: MemQL is not set up yet

Anybody who is not an owner or developer of the cluster sees one page in place
of the desk while the cluster cannot reach a model:

- the title, *MemQL is not set up yet*;
- one sentence: *An owner or developer has to set up inference before anyone
  can use it.*;
- *This page opens by itself once they have*;
- Sign out.

**It offers nothing to add.** An API key of their own does not let such a
person into a cluster that is not set up, so the page offers none.

**It does not name the owner.** A person's name is not the cluster's to
publish, and the page is as actionable without one.

**It lifts by itself.** The readiness feed is live, so the desk opens the
moment the cluster reports it is set up, with no reload. It comes back the
same way if the cluster stops being set up, open windows or not.

### Ask needs model access

Once the cluster is set up, everybody enters. Ask runs on the person's own
model access, so the OS asks the cluster at sign-in whether they have any of
these set up:

- a machine of theirs, or one lent to them, that runs a usable model;
- Claude Code or Codex on a machine of theirs;
- an API key of theirs, or one lent to them;
- the cluster's own vendor account, where it pays for them.

Set up counts even when it cannot answer right now: a lent laptop that is
asleep, a key out of credit, an app out of usage. With something set up, the
person has Ask, and Ask says what is asleep. With nothing set up they have no
Ask: no button in the dock or the phone bar, no Ask widget on the desk or in
the launcher, and no "Ask about this" in any app. Everything else their role
gives them works. The cluster's owners and developers keep Ask with nothing set
up.

**Ask appears by itself.** Nothing an administrator changes reaches the
person's connection, so the OS asks again:

- when the window regains focus;
- when the connection returns;
- every 30 seconds while they have no model access and the tab is visible.

**An administrator finds out why in Logs.** When the check finds nothing set
up, the cluster writes one line naming the person and each source it checked
with why it does not count. The full list of reasons, and how to find the
line, are in
[Who gets into MemQL OS](auth/access-model.md#who-gets-into-memql-os).

### What it does when it cannot say

The readiness feed has three silences, and only one of them holds anybody.

| The feed says | The gate |
|---|---|
| nothing yet — it has not loaded | draws nothing, and the shell opens |
| `unreported` — no live node reported inference | draws nothing, and the shell opens |
| `configured`, `partial` | draws nothing, and the shell opens |
| `unconfigured` | holds owners and developers on the setup screen, and everybody else on *MemQL is not set up yet* |

**A broken cluster is not an unconfigured one.** `unreported` means no live
node answered, which is a different problem with a different fix — and a gate
that held on it would lock somebody out of the Cluster app they need in order
to go and find out why nothing is reporting.

**A person's own check that cannot be answered takes nothing away.** An outage
is not evidence that a person lacks access, and the cluster refuses their model
calls either way, so they keep Ask. On the way in to a cluster that is set up,
for anybody not already known to be an owner or developer, the opening frame
waits for the first answer for up to three seconds, the same bound as for the
feed, so the desk is first drawn knowing whether Ask belongs on it. An answer
that comes later removes Ask when it lands. A check that fails ends the wait at
once. A later check that fails keeps the answer the OS already has.

One more wait has no time limit. While the cluster is unconfigured AND the role
ladder or the signed-in identity has not landed, nothing is drawn at all. Who is
looking decides between the setup screen and the page everybody else gets, and
telling an owner that an owner has to set the cluster up is worse than a beat
of empty ground. If a window is already open when the identity goes away, the
desk stays as it is until the identity is back.

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

- **Local** — a computer you own runs the model, with nothing leaving it and
  no bill. The act opens Fleet's guided install with local models already
  selected, so the person leaves with one install line and comes back when
  the machine registers and its checks settle.
- **Anthropic** — Anthropic serves the model, on their hardware and their bill.
  The act opens Settings → AI providers at the Anthropic panel.
- **OpenAI** — OpenAI serves the model, on their hardware and their bill.

A **developer** gets all three doors: Fleet's Machines section has no role
floor, and the AI providers section admits owner or developer. The rail offers
a button only where the registry says this actor can reach the section, and a
sentence naming who can otherwise, so it never offers an act that is not legal.

### What counts as a configured door

The `ai` verdict is computed from rows, identically on every node type. It
needs ONE of these doors:

- an unrevoked machine advertising a model that does structured output with a
  context window of at least 8192 tokens;
- an unrevoked machine with a known, allowed, signed-in local app;
- a federated cloud vendor whose identity ids are present;

**and an embedding model**: the cluster's bound embedder, or an embedding model
an online machine advertises. Library search and memory recall need one, so a
door alone is not set up -- an app answers, but cannot make embeddings. When a
door is open and no embedding model is, the inference stop says "Add an
embedding model". A Local setup pulls one with the recommended models, and the
engine binds the cluster's first embedding model by itself.

Each is one lane on the readiness row (`local`, `app`, `federation`,
`embeddings`), and each lane carries a second fact — whether it is open
**right now**. The gate reads the first.

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
