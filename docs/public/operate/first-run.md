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
while anything is still outstanding, the **Set up** card. Anybody who is not an
owner or developer of the cluster is held by a different question: whether
model access is set up for them personally (see
[the held page](#the-held-page-everybody-else)).

## The gate

It renders in place of the desk. Behind it there is no app, no dock and no
launcher: they are not hidden, they are not mounted.

**It asks two questions, each about different people.**

- For the cluster's **owners and developers**: can this cluster reach a model?
  Until it can, they get the setup screen.
- For **everybody else**: is model access set up for this person? Until it is,
  they get the held page, whether or not the cluster as a whole is set up.

Being the owner or developer of an account does not exempt anyone from the second question; only the cluster's own owner and developer roles do.

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
closes. An answer it cannot read keeps the screen. For everybody else a key is
one of the things the held page counts. ([API keys](api-keys.md))

### The held page: everybody else

Ask, goals and file summaries run on the person's own model access, so somebody
with none would get a desktop whose features fail one at a time. When the
person signs in, the OS asks the cluster whether they have any of these set up:

- a machine of theirs, or one lent to them, that runs a usable model;
- Claude Code or Codex on a machine of theirs;
- an API key of theirs, or one lent to them;
- the cluster's own vendor account, where it pays for them.

Set up counts even when it cannot answer right now: a lent laptop that is
asleep, a key out of credit, an app out of usage. With something set up, the
person enters. With nothing, they see the held page in place of the desk:

- the title, *Your access is not set up yet*;
- one sentence asking them to have an administrator set a model up, and to give
  the administrator the reference beneath it, which has a copy control;
- *This page opens by itself once it is set up*;
- **Add an API key**, for anybody who may add one (every predefined role may);
- Sign out.

**It does not name the owner.** A person's name is not the cluster's to
publish, and the page is as actionable without one.

**It lifts by itself.** Nothing an administrator changes reaches the held
person's connection, so the page asks again:

- when the window regains focus;
- when the connection returns;
- every 30 seconds while it is visible;
- after a key is added.

Open windows never keep the desk from it. Access taken away while somebody is
inside shows the page at the next check.

**Add an API key runs the key wizard on the page**, in the same frame, not in a
Settings window: a window needs the desk drawn under it, and the desk is what a
held person does not get. Cancel returns to the page. A key added asks again,
and the page opens if the key counts.

**The reference is what an administrator looks up.** It is `ERR-` and six hex
characters. Before the page shows it, the cluster writes one line under it to
the log store, naming the person and each source it checked with why it does
not count. An administrator, or anyone of higher rank, searches the reference
in Logs and finds that line
([the held person's reference](logs.md#the-one-line-that-is-never-dropped-a-held-persons-reference)).
A check that still holds the person keeps the same reference. If the line could
not be stored, the page shows no reference and does not ask for one.

### What it does when it cannot say

The readiness feed has three silences, and only one of them holds anybody.

| The feed says | The gate |
|---|---|
| nothing yet — it has not loaded | draws nothing, and the shell opens |
| `unreported` — no live node reported inference | draws nothing, and the shell opens |
| `configured`, `partial` | draws nothing, and the shell opens |
| `unconfigured` | holds owners and developers on the setup screen |

**A broken cluster is not an unconfigured one.** `unreported` means no live
node answered, which is a different problem with a different fix — and a gate
that held on it would lock somebody out of the Cluster app they need in order
to go and find out why nothing is reporting.

**A person's own check that cannot be answered lets them in.** An outage is not
evidence that a person lacks access, and the cluster refuses their model calls
either way. On the way in, for anybody not already known to be an owner or
developer, the opening frame waits for the first answer for up to three
seconds, the same bound as for the feed, so a held person whose answer comes in
time sees the page rather than the desk. An answer that comes later replaces
the desk with the page. A check that fails ends the wait at once. A check that
fails after one has held the person leaves the page up until the next answer.

One more wait has no time limit. While the cluster is unconfigured AND the role
ladder or the signed-in identity has not landed, nothing is drawn at all. Who is
looking decides between the setup screen and the desk, and opening the desk for
an owner who has to set the cluster up is worse than a beat of empty ground.

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
