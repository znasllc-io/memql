---
title: AI routing -- levels, policies and rules
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# AI routing: levels, policies and rules

Every call MemQL makes to a model is decided by three things, in this order:

1. the **level** the call declares -- how much intelligence it needs,
2. the **rule** that matches the call -- which looks at what the call is for,
3. the **policy** that rule names -- an ordered chain of places to look.

Nothing in the platform names a model at a call site. That is the point: a model
name at a call site is a release every time your fleet changes.

---

Fleet provides a persistent visual policy editor and review of typed Ask policy
proposals, following
[Supervised Visual Composition](supervised-visual-composition.md). Generating
proposals requires compatible inference to be configured. Manual policy editing
remains available independently. This page describes the underlying engine
contract.

## The three nouns

### Level

A level is how much intelligence a call needs. There are exactly four, and there
will not be a fifth -- an abstraction you cannot hold in your head is not one.

| Level | What declares it |
|---|---|
| `fast` | Triage, intake, classification, summaries, suggestions, the safety classifier |
| `strong` | An agent's reply, a conductor turn, an authoring design pass |
| `reasoning` | Emitting or repairing a construct, re-planning a run |
| `embeddings` | Every embedding call |

Every DSL `prompt` carries `@level`. A prompt without one refuses to load, and a
product bundle mounted at `MEMQL_DSL_PATH` is held to the same rule. A Go call
site with no prompt -- suggest, the safety classifier, vision, embeddings, the
chat handler -- names its level in the request.

**Modality is never declared.** Whether a call is chat, streaming chat, tools,
structured output, vision or an embedding is derived from the call itself, and
the router interface-checks it. You do not write it anywhere.

### Policy

A policy is an ordered chain of places to look. The general chains reach paid
inference last; the embeddings chain names only the bound embedder:

| Policy | Chain |
|---|---|
| `fastLocalFirst` | `fleet:fastest`, `app:*`, `federation:cheapest` |
| `localFirst` | `fleet:strongest`, `app:*`, `federation:cheapest` |
| `localOnly` | `fleet:strongest` |
| `federationStrongest` | `fleet:strongest`, `app:*`, `federation:strongest` |
| `embeddingsBinding` | `embedder:active` |

The order inside a chain is three kinds of cost, increasing:

- **`fleet:`** -- a model on hardware you already own. Costs electricity.
- **`app:`** -- a signed-in Claude Code or Codex on one of your machines. Costs a
  subscription you already pay for; MemQL is not billed for it.
- **`federation:`** -- a metered vendor. Costs money, and is therefore the hop the
  cost ceiling gates.

The ceiling is asked **only when a local door preceded the vendor in the chain**.
A chain that starts at a vendor is a decision somebody made, not a fallback, and
refusing it here would break every deliberately-paid policy.

A chain entry is one of a closed set of forms:

```
streamClaudeSonnet          a provider by name
fleet:strongest             the best local model that can serve this call
fleet:fastest               the quickest local model that can serve this call
fleet:qwen3.8:27b           one local model by id
app:*                       any signed-in app on one of the caller's machines
app:claude-code             one app by id
app:claude-code:claude-opus-5   one app by id, with its model pinned
federation:cheapest         the cheapest vendor record that qualifies
federation:strongest        the strongest vendor record that qualifies
federation:streamClaudeSonnet   one vendor record by name
policy:localFirst           another policy, expanded at load
```

`fleet:*` is retired. It said "any", which is not what it did; write
`fleet:strongest`.

**An app id is held to the engine's closed runnable set at LOAD.** `app:gemini-cli`
refuses to load, naming the set, because the engine has no protocol for a third app
-- so an entry naming one would be a chain step that could only ever be passed over,
which reads months later as a door that is shut rather than as a policy that is
wrong. Growing the set is a value change in `core/airoute/apps.go`, not a release.

**`app:<id>:<model>` pins that app's model**, the way `fleet:<modelId>` pins a fleet
model. The wildcard cannot take one: `app:*` names any signed-in app and a model name
belongs to ONE app, so `app:*:claude-opus-5` would ask Codex for a Claude model. It
refuses rather than being ignored on the apps it cannot apply to. What the app
ACTUALLY served with comes back on the decision row as `servedModel`, which is how
you see an app that rerouted or ignored the pin.

`fleet:strongest` keeps measured structured validity first, then the owner's
explicit model preference. Its size heuristic ranks active parameters per token
descending, using total parameters when the runtime has not reported an active
count; context window, recognized quantization precision, and model id break ties.
For otherwise equal variants, Q8 wins over Q4 retained after an upgrade; an explicit
Q4 preference still wins over this tie-break. A 27B dense model therefore ranks
ahead of a 35B mixture with 3B active parameters when neither has a measurement
or an explicit preference. Total parameters remain available for memory planning.

`fleet:fastest` ranks the same effective parameter count ascending and excludes
models below 3B effective parameters, including unknown sizes. This floor is a
routing heuristic, not a quality benchmark. With 27B and 9B dense models available,
ordinary fast calls choose 9B and strong calls choose 27B. A 0.8B model remains
available through an explicit `fleet:<modelId>` pin. The floor applies to every
candidate expanded from the fast selector, including fallback candidates.

Fast ordering currently uses size, not measured throughput: token rates, audio
real-time factors and image timings are different units and are not compared.
Fast selection does not consult the owner's strongest-model preference.

**`federation:cheapest` sorts a record with no cost figures LAST**, and the
decision record says so. Sorting it first would make a price nobody filled in the
cheapest thing in your catalog.

**A policy may name a policy.** `policy:<name>` is expanded at load, cycles refuse
to load and name the loop, and the router only ever walks a chain with no
`policy:` left in it.

### Rule

A rule maps a call's metadata to a policy. It is the half a policy cannot express:
a chain says where to look and can never say which calls it is for.

```memql
/// An operator's agent reply reasons.
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localFirst")
@precedence(60)
@onUnavailable("degrade")
rule operatorReasoning { }
```

`@when` takes a closed key set. Every key is optional and all present keys are
ANDed:

| Key | Matches |
|---|---|
| `level` | the level the call declared |
| `modality` | the derived modality |
| `prompt` | the DSL prompt name |
| `role` | the **agent's** role slug |
| `actorRole` | the **calling person's** cluster role |
| `tag` | a call tag, such as `background` |
| `touches` | a concept-id prefix the call's footprint matches |

`role` and `actorRole` are different questions on purpose. An operator watching a
non-operator agent work is not an operator turn, and a rule that could not tell
them apart would route on who is watching rather than on what is acting.

The other annotations:

- **`@policy`** -- required. The chain this rule selects.
- **`@level`** -- optional. Overrides the level the call declared. Both the
  declared and the effective level land on the decision record.
- **`@precedence(N)`** -- highest first. **A tie is a load error**: a tie is
  resolved by nothing, so the rule that wins would differ between replicas.
- **`@onUnavailable("degrade" | "park")`** -- what happens when the chain is
  exhausted. Unset means degrade.
- **`@exclude("fleet:<modelId>")`** -- repeatable. Removes one concrete model from
  this rule's resolution.
- **`@locked`** -- embedded tree only. See below.

The first matching rule wins. A call that matches nothing is impossible: the
shipped `default` rule states no conditions, so it matches everything, and it
cannot be removed.

---

## Degrade or park

When the chain is exhausted at the requested level, the winning rule decides.

- **`degrade`** walks the chain again one level down -- `reasoning` to `strong` to
  `fast` -- and records `servedLevel` and `degraded: true`.
- **`park`** returns the refusal with the door report. A work run parks; an
  interactive surface shows the sentence.

`fast` is the floor and does not degrade. **`embeddings` never degrades**, at any
setting: a degraded embedder does not return a worse vector, it returns one in a
different vector space, so it does not belong in the index it is about to be
written to and every later similarity read comes back plausible and wrong.

**No degradation is silent.** The decision record and the run's step both say what
actually served.

---

## The shipped rules

Eight, all `@locked`.

| Rule | Condition | Level | Policy | Exhausted |
|---|---|---|---|---|
| `default` | none -- matches everything | -- | `localFirst` | degrade |
| `fastLane` | `level="fast"` | -- | `fastLocalFirst` | degrade |
| `backgroundLane` | `tag="background"` | -- | `localFirst` | degrade |
| `backgroundEscalation` | `tag="backgroundEscalation"` | `strong` | `localFirst` | degrade |
| `operatorReasoning` | `prompt="agentReply", role="operator"` | `reasoning` | `localFirst` | degrade |
| `reasoningParks` | `level="reasoning"` | -- | `federationStrongest` | **park** |
| `embeddingsBound` | `level="embeddings"` | -- | `embeddingsBinding` | **park** |
| `compilerLocalOnly` | `prompt="compileRule"` | -- | `localOnly` | **park** |

`operatorReasoning` degrades while `reasoningParks` parks, and the difference is
worth learning. A person is waiting on an agent reply and would rather have a
smaller model's answer than a card telling them to open a laptop. A call that
*declared* reasoning -- emitting a construct, re-planning a run -- would rather
park: a degraded reasoning call does not return a worse answer, it returns a
confident answer produced by a model that could not do the work, and nothing
downstream can tell the difference.

### What `@locked` means

Three things, and all three are enforced:

1. A locked rule evaluates **before every unlocked rule**, regardless of
   precedence. You may add rules and give them any precedence; they do not
   quietly become these.
2. Locked definitions are **re-read from the embedded tree on every boot**, so
   nothing done to one at runtime survives a restart.
3. A runtime-authored rule may not **carry `@locked`** and may not **take a
   shipped name**.

---

## Adding your own rule

Two ways, and both produce the same thing.

**In the DSL**, in a product bundle mounted at `MEMQL_DSL_PATH`: write a `rule`
construct with a precedence above the shipped one you want to win against.

**At runtime**, through `routingRuleActivate`, which takes the structured form
(conditions, policy, precedence, on-unavailable, excludes), renders the construct
deterministically -- no model is involved -- runs the same gates the DSL path
runs, and activates it live. Owner or developer.

Either way the rule is refused if it names a shipped rule or policy, carries
`@locked`, or collides on precedence with another unlocked rule.

### "I want a paid model for everything"

Write one rule above the default:

```memql
@when()
@policy("myVendorFirst")
@precedence(500)
rule alwaysVendor { }
```

with a policy whose chain starts at that vendor. It is explicit, it is named on
every decision record, and the cost ceiling still governs it.

---

## Reading what the router decided

Every resolution writes one `v1:router:call` row, on success and on a park. Beyond
the tokens, cost and latency the row already carried, a decision carries:

| Field | Says |
|---|---|
| `level` / `servedLevel` / `degraded` | what was asked for, what served, and whether it was a step down |
| `rule` | which rule matched |
| `policy` | which chain it named |
| `door` | `local`, `app`, `federation` or `session` |
| `servedModel` / `servedEffort` | what the SURFACE reported serving it with, empty when it said nothing |
| `considered` | every entry the walk passed over, its door, and why -- **kept on success as well as on a refusal** |
| `touches` | the call's footprint |
| `minContextTokens` | the context floor this resolution was made against |

**`session` is a fourth door, not a flag on `app`.** `app` means a subscription app
answered this TURN; `session` means one was handed the whole STEP and drove its own
loop (design D7 of the app-session record). They are separate values because this
field is what you filter on, and folding them would make the entire history of a
cluster whose only open door is a signed-in Claude Code read as ordinary chat turns.

**`servedModel` is a REPORT, never the request.** `model` is what the chain resolved
-- for an app door that is the door's own name, `claude-code` -- and what actually
ran is the app's to state. Both fields are EMPTY when the surface said nothing, which
reads as unknown: a value copied from the request would record as measured something
nobody measured, in the one case anybody would want to look at. Claude Code's headless
output states no effort at all, so an empty `servedEffort` is the ordinary case.

Read them with `routerDecisionsRecent`, filtering by `since`, `level`, `door`,
`rule` and `outcome`. Floored at **admin**, which on this ladder admits admin,
developer and owner: a decision record carries no prompt content and no error
message -- both are deliberately left out of the projection -- and an admin
answering "why did this go to a vendor" needs it.

`considered` being kept on success is the part that makes a rule falsifiable. What
a chain did *not* pick is half the decision, and before this the report was
accumulated and then dropped the moment an entry won.

These rows are **never broadcast** -- one row per model call is the same volume
argument that excludes `v1:worker:invocation` from the live feeds.

---

## What is still in Go, and why

A kill switch a policy can author around is not a kill switch. These stay in code,
outside the rule system, and no rule can reach them:

- the process-wide LLM rate ceiling, the identical-request circuit breaker and the
  cumulative spend kill switch (`component/memql/ai_guard.go`, mirrored for fleet
  and app calls by `ai_guard_fleet.go` against the same shared accounting);
- the run's token and cost ceilings (`component/work/budget.go`);
- door classification, provider availability, and the refusal codes.

See [LLM cost control](../ai/llm-cost-control.md) for the full defence-in-depth
picture.

---

## When a call cannot be served

The refusal names every door and why each did not open, because "no provider
available" is a sentence with no action in it -- the fixes live in different
places and which one applies is exactly what the door list says.

- **`every_door_shut`** -- the chain was exhausted. A condition the world changes:
  a lid opens, somebody signs in, a model finishes pulling. Worth re-checking.
- **`ceiling_reached`** -- a paid provider could have served the call and the cost
  ceiling is reached. A condition only a person changes. Worth asking about once.

The local door's report goes further than the door: it names the machines it
considered and why each was ruled out, because "your fleet is unavailable" sends
you nowhere while "laptop: offline; desktop: does not offer qwen3.8:27b" names the
laptop to open and the model to pull.

---

## Related

- [LLM cost control](../ai/llm-cost-control.md) -- the defence-in-depth accounting
- [The MemQL language](../language/memql.md) -- the `rule` construct and `@level`
- [Workers runbook](workers-runbook.md) -- machine routing, which happens *after* a
  model is chosen and is a separate decision
