---
title: AI settings and routing -- vendors, levels, routes, rules and history
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# AI settings and routing

How a model call finds something to answer it lives in two places in MemQL
OS, split by what each thing is:

- **Settings** keeps what is a credential or a cluster-wide value:
  **Vendors** (the two vendors' federation ids) and **Levels** (what each
  level resolves to right now). Each carries one quiet link, "Routing is in
  Fleet".
- **Fleet > Routing** is where routing is decided and read, in four tabs:
  **Routes**, **Rules**, **Machines** and **History**.

| Place | The question | The shape it takes |
|---|---|---|
| Settings > Vendors | Which vendors can this cluster reach? | Two rows, each with its form |
| Settings > Levels | What would happen if something asked for this? | Four sentences |
| Fleet > Routing > Routes | Which sources serve my messages, job by job? | Auto, Local, Apps and your own |
| Fleet > Routing > Rules | Which calls take which chain? | An ordered list, first match wins; Chains one step under |
| Fleet > Routing > Machines | Which of my machines takes a call sent to them? | One choice per person |
| Fleet > Routing > History | What actually happened? | A log |

There is no API key field on any of them, or anywhere else in the product.

## The words

The screens use five nouns, and only five:

| Word | Means | The engine's name |
|---|---|---|
| **Source** | Something that can answer a call: a local model, an app (Claude Code, Codex) on a machine, or a vendor | a chain entry (`fleet:`, `app:`, `federation:`) |
| **Route** | What a person picks for a message in Ask: which sources serve each job | a route (`routesForActor`) |
| **Chain** | An ordered list of sources, tried in order until one can serve | a policy (`dsl/policies`, `routerListPolicies`) |
| **Rule** | "When work looks like X, take chain Y" | a rule (`dsl/rules/rules.memql`) |
| **Level** | How much intelligence a call needs: Fast, Strong, Reasoning (Embeddings is internal) | a level |

The engine keeps its own names in ids, query names and the DSL; the screens
never show them. A refusal the engine writes is shown with its words swapped
for these.

---

## The order is the point

The shipped routes try the sources in this order and take the first one that
can serve:

1. **Your machines** -- a model running on hardware you own. Nothing is billed.
2. **Signed-in apps** -- Claude Code or Codex on a machine you own, spending a
   subscription you already pay for. Nothing is billed here either.
3. **Vendors** -- Anthropic or OpenAI, billed per call, which is why they are
   last.

An owner who genuinely wants a paid model or an app first changes a route, or
writes a rule that takes a route starting there. That is explicit, and every
decision record then reports it.

## Settings > Vendors

One row per vendor: what is true of it now, and its form.

**Saving is not applying.** Saving writes the ids; the registry each node
resolved at boot does not move until Apply broadcasts. They are two controls
because they are two facts.

**A half-configured vendor is the state that shouts**, because the engine
refuses to boot on it -- hours after the save that caused it. One to three of
Anthropic's four ids, or one of OpenAI's two, and the fleet goes down at its
next restart. That row carries the engine's own sentence, which names exactly
which ids are set.

**A local cluster is told, not asked.** Its OIDC issuer is not reachable from
the public internet, so neither vendor can verify a token it mints. The forms
are absent there with a sentence in their place -- no id you enter could ever
work.

## Settings > Levels

A level is how much intelligence a call needs. There are four: `fast`,
`strong`, `reasoning`, `embeddings`. Every call names one and none of them
names a model, which is what lets a model change without a release.

This screen has no controls. It is a mirror: it says what each level resolves
to right now, and the way to change the answer is to change a route or write a
rule in Fleet > Routing.

Each row is a sentence rather than a table cell:

> **reasoning** -- No model on your fleet, so a signed-in app takes this. Your
> subscription, nothing billed here.

**`embeddings` never degrades**, at any setting. A smaller embedder answers in
a different vector space, so the vector does not belong in the index it is
about to be written to, and every later similarity read comes back plausible
and wrong.

## Fleet > Routing > Routes

The routes a person picks from for each message in Ask:

- **Auto** -- picks the best balance of speed, cost and quality for every call.
- **Local** -- local models for every job.
- **Apps** -- Claude Code or Codex for every job.
- **Yours** -- routes you make, such as "Out of credits" or "Emergency". Only
  you see and use them. **New route** is on the section heading.

Each row says whether the route can serve every job for you right now, or why
not: "Needs an embedding model", "No signed-in app on your machines".

### A route's page

A route decides which source serves each of four jobs:

| Job | What it is for | Level |
|---|---|---|
| Quick tasks | Triage, summaries and suggestions | `fast` |
| Main work | Replies and most real work | `strong` |
| Deep reasoning | Planning and hard problems | `reasoning` |
| Search and memory | Finding things in your Library and memory | `embeddings` |

- The page is the machine Model setup board, applied to a route: a **tray** of
  sources (Models, Apps, Vendors), each saying where it serves from or why it
  cannot, beside the **jobs**, each listing up to four sources in the order
  they are tried.
- Select a source, then **Add** on a job; or drag it into place. Arrow keys
  move a source within its job, Delete removes it. A job that cannot take the
  selected source says why: "Already in this job", "A job holds 4 sources at
  most".
- Each job says what serves it now ("Serves with qwen3.8:27b") or why nothing
  does. It is read from the cluster, never simulated.
- **Search and memory is the cluster's embedding model, the same in every
  route.** The index holds vectors from one model, and another embedder answers
  in a different vector space, so no route chooses one. The job names the bound
  model; when there is none, it links to Fleet > Models.
- Images and voice follow Auto.
- The bar carries the acts, each only where it is legal: Discard changes and
  Save route while there are changes, Restore default on a changed Local or
  Apps, Delete on a route of yours. Restore default and Delete ask first, in
  the bar.

**A machine that cannot take a call says why.** Where the cluster measures
what each machine can take right now, a local source that cannot take a call
says so in a few words -- "Busy, 2 of 2", "Needs 20 GB, 9 GB free" -- and its
job moves on to the next source. A cluster that does not measure it shows
readiness as before.

### New route

**New route** is Name -> Start from -> Jobs. Start from copies Local or Apps as
they are on this cluster now, or starts empty; a later change to Local or Apps
does not reach a route made from them. A name is at most 40 characters, is not
one of yours already, and is not Auto, Local or Apps.

### Auto

- **Balance** -- Speed, Cost and Quality, 0 to 100 each, with MemQL's
  recommended value marked on each track. An owner or developer saves a change
  for the whole cluster, or returns it with Reset to recommended. Reliability
  always counts, so no setting trades it away.
- **Budget** -- your monthly budget and what you have spent with metered
  vendors this month; your own machines and app subscriptions never count.
  Near the budget Auto favours your machines and apps, and once it is reached
  Auto stops using vendors until the month resets. The budget is set in your
  profile. A cluster that does not track budgets has no Budget section.
- **What Auto picks now** -- for each job, the source Auto would take right
  now, and, when a local model cannot take the call, why.

### Who can do what

| Act | Who |
|---|---|
| Open Routes; pick a route in Ask | Everyone who can open Fleet |
| Make, change and delete a route of your own | Everyone, for their own |
| Change or restore Local and Apps | Owner or developer (`app:settings/rules`) |
| Change Auto's balance | Owner or developer (`app:settings/rules`) |

An act a person cannot take is absent, and the page says who can.

**Built-in routes, Auto's balance, chains and rules share one revision.** Every
write to them names the revision it read; a write elsewhere since then is
refused, the screen says "Routing changed since this page read it", and the
page is read again. A route of your own carries no revision: only you change
it.

### Chains, under Rules

The ordered lists of sources that rules name are **chains**, one step under
Rules (**Rules > Chains**). Opening one shows the composer:

- **Slots** in a row, in the order they are tried, and a **tray** of every
  source the cluster knows, grouped. A source that cannot serve right now
  stays in the tray, quiet, with the reason.
- **By exact name** takes any source the engine accepts that the tray does not
  list: an app with its model pinned (`app:claude-code:<model>`), a model no
  machine offers yet (`fleet:<model>`), a vendor or provider by its name.
- **The line** runs from "Request" up to the first slot that is ready right
  now, and that slot says "Serves now".
- Equip, reorder and remove by click, drag, or keyboard (arrow keys move a
  slot, Delete removes it, Enter chooses it so the next source replaces it).
- The bar carries the acts: Cancel, Restore shipped (on a changed shipped
  chain, after asking), Save chain. A chain change is cluster-wide: the bar
  says how many rules take the chain, and "Taken by N rules" opens Rules on
  exactly those.

What each route does in the router, and the builtins behind them, is in
[AI routing](ai-routing.md#routes-a-person-picks-in-ask).

## Fleet > Routing > Rules

A rule looks at what a call is -- its level, its kind, its prompt, the role
acting or watching, a tag, what it touches -- and picks a route. Rules are
tried in order and the first match wins. Each row reads as the rule:
**Fast work -> Fast local first**.

### Shipped rules, and the floor

Shipped rules carry a lock, come back on every restart, and cannot be edited
or removed. All but one run **before** the rules you write, regardless of
precedence. So a rule of yours with the same conditions as a shipped one never
fires: the list marks it "a shipped rule decides first", and adding one says
so and offers to change the shipped rule's route instead. To send fast work
(planner triage, for example) to Claude Code, change the **Fast local first**
route, not a new rule.

The exception is **the floor** -- the shipped rule that states no conditions.
It runs **last**. If it ran with the other shipped rules it would match every
call before any rule of yours was consulted.

Reorder your own rules by dragging, or with Alt+Up / Alt+Down. A move writes
precedences so no two of your rules ever share one, even mid-move; if a move
stops part-way the screen says the order changed part-way.

### Adding a rule

**Add a rule** is When -> Route -> Review. **Describe it instead** takes a
sentence in your own words and compiles it, with three states whose acts
appear only when legal: Compile it, then Check it, then Activate. Editing the
sentence withdraws the check, because a check belongs to one exact rule.

A rule's own page holds every field: the conditions (each a checkbox plus a
value -- an unticked condition is not sent at all, which is different from one
sent empty), the level it asks for, its route, what happens when nothing is
ready, and its precedence. Remove asks first.

## Fleet > Routing > Machines

Which of your machines takes a call a route sent to your machines: the
strategy, the fallback and your model preference. Per person; everyone who
can open Fleet sees it.

## Fleet > Routing > History

Every routed call, newest first: the rule that decided it, the route it took,
the source that served it, the model, what it cost and how long it took.
Refine by level, source, outcome and rule (a rule since removed stays
offered).

**A free call never shows a money figure.** A call your own machine answered,
or one a subscription paid for, cost this cluster nothing -- and `$0.00` would
claim a measurement was taken and came out zero. Those rows say "your own
machine, no charge" instead. A call whose billing nobody reported says "not
reported".

**A row expands to the walk**: every source the call looked at and why it was
passed over -- the half that separates "there was nothing else to try" from
"everything else was shut".

## Who can see what

| Place | Who |
|---|---|
| Settings > Vendors | Owner or developer |
| Settings > Levels | Owner, developer or admin |
| Routing > Routes | Everyone who can open Fleet; changing the built-ins and Auto's balance is owner or developer |
| Routing > Rules, Chains | Owner or developer (`app:settings/rules`) |
| Routing > History | Owner, developer or admin (`app:settings/decisions`) |
| Routing > Machines | Everyone who can open Fleet |

A tab a person cannot open is absent. History is readable one rung wider
because a decision record carries neither the prompt nor the error message,
so an admin answering "why did this go to a vendor" can have it without being
shown anything they should not see. On this cluster's role ladder **developer
outranks admin**.

## Related

- [What an owner sees first](first-run.md) -- the gate that holds a fresh
  cluster until it has a source.
- [Local models](local-models.md) -- pulling a model onto a machine you own.
- [Workers runbook](workers-runbook.md) -- pairing a machine.
