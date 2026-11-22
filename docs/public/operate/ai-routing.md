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
  cost ceiling gates. Whose money is a second question, answered after the
  model is chosen: [which account pays](#which-account-pays-for-a-vendor-call).

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
federation:cheapest         the cheapest vendor model that serves the call's level
federation:strongest        the strongest vendor model that serves the call's level
federation:claudeSonnet55   one vendor record by name
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

### What a vendor record declares

"The strongest vendor model" means something only because every vendor text
record in `dsl/providers/providers.memql` says what it is for:

```memql
@extends("anthropic")
@model("claude-opus-5-5")
provider claudeOpus55 {
  params {
    streaming                  true
    levels                     "strong reasoning"
    rank                       900
    structuredOutput           "format"
    contextWindow              1000000
    maxTokens                  20000
    inputCostPerMillion        4.00
    outputCostPerMillion       20.00
    cachedInputCostPerMillion  0.20
  }
}
```

| Param | What it says | Checked at load |
|---|---|---|
| `levels` | The jobs the model is for: one to three of `fast`, `strong`, `reasoning` | Any other word, a repeated word or `embeddings` refuses the record |
| `rank` | Its place among the records of a level, 1 to 1000, higher first | A value outside the range refuses the record |
| `inputCostPerMillion`, `outputCostPerMillion` | The vendor's standard rate | -- (a record with no price loads, reads as cost unknown, and is last under `federation:cheapest`) |
| `contextWindow` | The largest prompt the record is offered | -- |
| `structuredOutput` | How a Claude record asks for structured output: `tool` (the default) or `format` | Any other word, or the param on a record that is not a Claude text model, refuses the record |

**The two vendor selectors read only those.** For a call at a level:

- `federation:strongest` is the records that declare the level, highest
  `rank` first.
- `federation:cheapest` is the same records, lowest input plus output price
  first, a record with no price last.

Ties are broken by name, so two replicas answer alike. A record that declares
no level still resolves by name and is passed over by both selectors, with
that reason in `considered`; a record that serves other levels is left out
without a line. The level a selector reads is the one the call is served at:
a rule's `@level` moves the call first. The walk then asks each candidate, in order,
whether it holds the call's context floor, serves its modality, and has an
[account that can pay](#which-account-pays-for-a-vendor-call) for the person.
The first that does serves.

Before records declared a level, `federation:strongest` was name order and
neither selector read the level: with Anthropic set up, Claude Haiku answered a
reasoning call ahead of Claude Opus.

**`contextWindow` is the prompt size the record's one price is true for.**
OpenAI charges more past 272,000 prompt tokens and Claude Haiku 5.5 past
100,000, so those records declare that size, not the model's whole window.
Every call states a floor estimated from its prompt, so a larger prompt goes
to the next record that holds it at a true price.

**A current Claude model refuses things an older record sends.** Claude Opus
5.5 and Sonnet 5.5 answer a forced tool choice with an error, which is how the
engine asked Claude for structured output. A record that says
`structuredOutput "format"` sends the schema as the vendor's own output
format, fitted to the schema keywords the vendor accepts; a limit it does not
accept (a maximum, a length, a pattern) is kept in the field's description
and is not enforced. An object that takes any key (a map, or an object with no
declared properties) cannot be said in that format, so such a record refuses
the call, naming the field, and the route's next source serves. Those records
also declare no sampling parameter and no thinking budget, and keep
`maxTokens` under 21,333: above it the vendor's SDK refuses a call that does
not stream.

**A reply a Claude model declined is a failure, not an empty answer.** The
vendor reports a declined request as an ordinary response with the stop reason
`refusal`. Every Claude record turns that into an error, and so does a
structured reply by format that was cut off at the output limit. The route's
next source then serves, and the decision record says the model declined.

**A record name is never removed.** A product bundle pins record names, so a
record whose vendor shut its model down is repointed to the replacement the
vendor names, and declares no level. `chat53Latest`, `codex51Max` and
`reasoning4Mini` are such names.

The catalog was brought to both vendors' current text models on 2026-11-22,
from their own models, pricing and deprecations pages. **Nothing was run
against either vendor**: the first call through a new record is its first real
check. Claude Fable 5.2 and OpenAI's GPT-6 family are not in it; the file says
why.

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
- **`@level`** -- optional. Overrides the level the call declared, for the
  whole walk: a vendor selector offers the records of the level the rule set.
  Both the declared and the effective level land on the decision record.
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

The general ones, all `@locked`. The rest of `dsl/rules/rules.memql` pins
particular prompts (the rule compiler, document research) to local models.

| Rule | Condition | Level | Policy | Exhausted |
|---|---|---|---|---|
| `default` | none -- matches everything | -- | `localFirst` | degrade |
| `fastLane` | `level="fast"` | -- | `fastLocalFirst` | degrade |
| `backgroundLane` | `tag="background"` | -- | `localFirst` | degrade |
| `backgroundEscalation` | `tag="backgroundEscalation"` | `strong` | `localFirst` | degrade |
| `operatorReasoning` | `prompt="agentReply", role="operator"` | `reasoning` | `localFirst` | degrade |
| `reasoningParks` | `level="reasoning"` | -- | `federationStrongest` | **park** |
| `pipelineLocalFirst` | `level="reasoning", tag="pipeline"` | -- | `localFirstStrongest` | **park** |
| `embeddingsBound` | `level="embeddings"` | -- | `embeddingsBinding` | **park** |
| `compilerLocalOnly` | `prompt="compileRule"` | -- | `localOnly` | **park** |

`operatorReasoning` degrades while `reasoningParks` parks, and the difference is
worth learning. A person is waiting on an agent reply and would rather have a
smaller model's answer than a card telling them to open a laptop. A call that
*declared* reasoning -- emitting a construct, re-planning a run -- would rather
park: a degraded reasoning call does not return a worse answer, it returns a
confident answer produced by a model that could not do the work, and nothing
downstream can tell the difference.

### Pipeline work runs local first

A pipeline's AI -- the independent PR review, a failed run's diagnosis, the
repository change a diagnosis requests -- carries the `pipeline` tag: the review
because the pipeline runner makes the call, the others because their work goal
arrived through a pipeline (`requestedVia: "pipeline"`, read off the goal by the
replica doing the work, a planner's replan of such a run included). `pipelineLocalFirst` outranks `reasoningParks` for
those calls and names `localFirstStrongest`: the strongest local model that can
hold the call, then a signed-in app, then the strongest vendor, **in that
order**. `federationStrongest` has the same entries but is one of the chains
Auto ranks, and at `reasoning` Auto puts an app above a local model.

The context floor still decides the local door per call. A review renders at
most 1 MiB, which is a floor of 266,240 tokens: no 262,144-token local model
holds it, so a full-size review goes straight to the app, and a small one runs
locally. When nothing can serve the call the work parks.

### What `@locked` means

Three things, and all three are enforced:

1. A locked rule evaluates **before every unlocked rule**, regardless of
   precedence. You may add rules and give them any precedence; they do not
   quietly become these.
2. Locked definitions are **re-read from the embedded tree on every boot**, so
   nothing done to one at runtime survives a restart.
3. A custom rule, whether saved at runtime or declared in a product bundle,
   cannot **carry `@locked`** and may not **take a shipped name**.

---

## Adding your own rule

Two ways, and in both the router reads the rule on every call.

**In the DSL**, in a product bundle mounted at `MEMQL_DSL_PATH`: write a `rule`
construct in your product's own domain, with a precedence above the shipped one
you want to win against. It loads at boot with the rest of the bundle, and the
load refuses it if it takes a name another rule has, carries `@locked`, or ties
on precedence with another unlocked rule.

**At runtime**, in Fleet > Routing > Rules, or through the builtins behind it.
Anyone signed in can read the list; every other call is owner or developer.

| Builtin | What it does |
|---|---|
| `routingRules()` | Every rule in evaluation order, shipped and custom, with the configuration's current `revision`. |
| `routingRuleValidate(...)` | Checks a definition against the current rules and policies. Saves nothing. |
| `routingRuleSave(...)` | Saves a custom rule, or replaces the custom rule of that name. |
| `routingRuleRemove(name, expectedRevision)` | Removes a custom rule. |
| `routingRuleDescribe(sentence)` | Compiles a draft from a sentence using local inference. Saves nothing. |

`routingRuleSave` takes `name`, `policy`, `precedence` (0 to 1,000,000) and
`expectedRevision`, and optionally `conditions`, `level`, `onUnavailable`,
`excludes` and `description`. `conditions` is an object of the `@when` keys: a
key you leave out is no condition, and a key present with an empty value
matches only an empty value.

A saved rule lives in the cluster's **routing configuration**: one revisioned
document that every replica reads on every call, so the rule decides the next
matching call wherever it lands. A save or a removal names the revision it was
read at, and one made after somebody else changed the configuration is refused:
read the rules again and retry. A save is refused if the rule takes a shipped
rule's name, names a policy this cluster does not have (policies you saved
count), uses a condition key outside the closed set, or ties on precedence with
another unlocked rule. There is no `@locked` to ask for. A shipped rule cannot
be removed: out-rank it with a custom rule at a higher precedence.

A `rule` inside an authored bundle, from an authoring session or a promoted
construct, is refused at validation with a message naming `routingRuleSave`.
Authored constructs are not part of the rule set the router reads.

### Rules the nightly review proposes

Each night MemQL folds the week's calls per model and level. Where a model's
structured-failure rate at a level is above 30% over at least twenty calls, it
opens a **Routing rule** approval in Nexus carrying the exact text of a rule
that would exclude the model there. Owners and developers see it and decide
it: the people who can save a rule by hand.

**Approving saves exactly that rule** into the routing configuration under a
new revision, so it decides the next matching call on every replica, and the
revision records who approved it. Declining is recorded, so the same week's
evidence does not ask again. The rule then appears in Fleet > Routing > Rules
like any other custom rule, and removing it there routes the model again. An
approval that proposes retiring an exclusion removes the rule instead; the
review raises one only from a probe measurement, which it does not take yet.

Two limits follow from how the router matches rules:

- **Demotions are proposed only where a saved rule can decide a call.** The
  router takes the first matching rule, and the shipped rules are locked, so
  they are tried before any saved rule. `fastLane`, `reasoningParks` and
  `embeddingsBound` take every call at `fast`, `reasoning` and `embeddings`,
  so a demotion saved there would decide nothing. The review proposes none at
  those levels: it logs the model, the level and the rule in the way, and
  counts the demotions it withheld. Today it proposes only at `strong`.
- **One evidence demotion per level.** Every proposal sits at precedence 50,
  and two custom rules cannot share a precedence, so approving a second
  demotion at a level is refused, naming the rule already there. Remove or
  re-number that rule in Fleet > Routing > Rules and approve again, or decline.

Approving checks the rule against routing as it is at that moment, not as it
was when the review was proposed. It is refused only when the rule could not
be saved now: when it ties with another rule's precedence, or when another
rule would always be tried first.

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

## Routes a person picks in Ask

A **route** decides which source does each job for one message. Ask offers
five kinds, and the turn carries the choice as its source, `route:<id>`:

| Route | Quick tasks (`fast`) | Main work (`strong`) | Deep reasoning (`reasoning`) |
|---|---|---|---|
| **Auto** (`route:auto`, or no source) | the rules decide, ranked | the rules decide, ranked | the rules decide, ranked |
| **Local** (`route:local`) | `fleet:fastest` | `fleet:strongest` | `fleet:strongest` |
| **Apps** (`route:apps`) | `app:*` | `app:*` | `app:*` |
| **API** (`route:api`) | `federation:strongest` | `federation:strongest` | `federation:strongest` |
| **Your own** (`route:<id>`) | your sources | your sources | your sources |

**API is vendor models, and nothing else.** Its jobs take the strongest vendor
model that [declares the job's level](#what-a-vendor-record-declares): a quick
task gets the strongest quick model, not the strongest model there is. It is
paid by an API key of the person's, a key lent to them, or the cluster's
account where that [pays for them](#which-account-pays-for-a-vendor-call).
Saving it with a local model or an app in any job is refused
(`API lists vendor models only`), because a route named API that listed one
would say something untrue.

**Search and memory is never a route's.** Every route serves embeddings from
the cluster's bound embedder: the index holds vectors from exactly one model.
Vision, speech, transcription and image generation follow the rules in every
route.

**A route never falls through.** A call made under Local, Apps, API or one of
your own walks that route's sources for its level and nothing else. When they are
exhausted it degrades inside the route (`reasoning` to `strong` to `fast`,
through the route's own lists) and then refuses with `every_door_shut`, naming
the route. A level whose list is the same as one already walked is not walked
again, and that includes a vendor selector: a route that lists
`federation:strongest` at every level, as API ships, tries the records of the
level asked for and does not go on to the records of a lower one. It never
reaches a rule's chain or cloud consent. The decision record carries the route
in `route`.

**Local, Apps and API can be changed for the whole cluster** by an owner or a
developer: each job holds one to four sources (`fleet:`, `app:` or
`federation:`; on API, `federation:` only). The change is an override in the
routing document, beside the policy and rule overrides and under the same
revision; Restore default removes it. **Your own routes** are `v1:router:route` rows: named by you, private to
you, made from a copy of Local, Apps or nothing.

A turn naming a route you do not have is refused with `route_policy_unknown`;
one whose route cannot serve every job for you right now is refused with
`route_not_ready` and the reason ("Needs an embedding model", "No signed-in app
on your machines", "Needs an API key or a vendor account"), before any goal
exists.

**Whether a vendor source can serve is asked for the person, job by job.** A
vendor selector's answer is the first record, in its order at the job's level,
that has an account for them on a route they choose; a source that names one
record asks that record. This is the answer the walk reaches for a call, so a
person whose only key is at one vendor is not told a job naming the other
vendor's model is ready. Each job's "serves now" names the model and the kind
of account that pays (`claude-opus-5-5, paid by your API key`; a key is never
named). A job only vendors serve says what is missing: an API key or a vendor
account; an API key, where the cluster's account is not open to the person; or
a vendor model their account can pay for, when they have an account and no
model for that job.

### App selection on a machine

Fleet > Machines > your machine > Apps owns the app strategy. Ask keeps its
Route and Where controls; it does not carry an app-strategy override.

- **Auto** (default) balances observed token usage, response time and successful
  calls. Usage balance, Speed and Reliability weights are separate from the
  general routing Auto balance below.
- **Round robin** alternates available apps for new calls.
- **Claude Code only** and **Codex only** use that app exclusively, including
  when it is unavailable.

The usage score uses up to 200 recent owner-scoped app sessions from the last
two hours. Unknown usage stays unknown; token counts are not remaining-credit
estimates. Availability is tracked separately for each app on each owned
machine, and only an app its machine has found ready is used. A quota refusal
stays effective until a check on that machine finds the app working again; a reset
passing does not send a real call to find out. This evidence survives restarts and
replica changes and does not disappear as other sessions enter the usage window.
Auto and Round robin can use another enabled app; an Only strategy cannot.

After purchasing credits or signing in, there is nothing to press. The machine keeps
checking, within five minutes for Codex and fifteen for Claude Code, and the app comes
back when a check finds it working. A Cockpit that predates those checks reports none,
so its refusal lasts until the reset the app named, or fifteen minutes when it named
none, and the next call is what finds out. A later quota refusal blocks new calls
again, and a successful call clears it. Quota evidence from older builds is honored
while present in recent history; newly recorded refusals have a durable marker
independent of that history.

Each active session stays with its app. New calls receive their full supplied
conversation and contract, and delegated steps receive their inputs and
workspace context. A fallback repeats that same handover only after a refusal
proven to precede any work: no served model, no result, no tokens, and no recorded
or dropped actions. An app that runs out of usage or loses its login partway hands
the step to the next ready app with a record of what already ran, so the next app
carries on rather than starting again
([local apps](local-apps.md#when-an-app-stops-partway)). An uncertain interruption is
never automatically replayed.
The existing per-level model mapping remains in the app harness, and records
name the model actually reported by the app.

Selecting Apps with Where Auto permits an enabled app session on the owner's
machine. It does not authorize general computer tools or override an explicit
Workbench destination. Ownership, app permissions, kill switches and active-run
checks remain mandatory.

### Auto

Auto keeps the rules. What it adds is the order inside the three general
shipped chains -- `localFirst`, `fastLocalFirst` and `federationStrongest` --
which it ranks for each call on four signals:

- **Speed:** the median latency each kind of source served this level with,
  over the last two hours of decisions.
- **Reliability:** the share of those calls that succeeded.
- **Cost:** zero for your own machines and subscription apps, the whole scale
  for a metered vendor.
- **Quality:** a fixed, level-aware rank -- at quick tasks every source is
  nearly equal; at reasoning a strong vendor model, then an app, then a local
  model.

An unmeasured signal is neutral, and ties keep the chain's own local-first
order, so a cluster nobody has measured routes as it always did. The signals
are weighed by the cluster's **Auto balance** -- Speed, Cost and Quality, 0 to
100 each, 50 each recommended -- and reliability always weighs their mean.
A person's **monthly budget** (`myBudget`, `myBudgetSave`) bends the order:
from 80% of it vendors go after every free source, and at 100% they are left
out of that person's calls. The budget counts the calls that person made,
whichever account paid: a call paid by an API key somebody lent them counts
toward their budget, not the key owner's. A chain Auto reordered asks the cost
ceiling before every vendor hop. Auto spends a person's API key only when the
key's owner turned on **Let Auto use this key**. The decision record keeps one `(auto)` entry in `considered`
with every score.

Builtins: `routesForActor`, `routeSave`, `routeDelete`, `routeReset`,
`autoBalance`, `autoBalanceSave`, `autoBalanceReset`, `myBudget` and
`myBudgetSave`.

### Fleet capacity

Under every route, a fleet source admits a machine only when it can take the
call now. A machine without room is ruled out with the figures in `considered`.
Room is its free memory plus what idle models other than this one hold, and the
call wants what it still has to load plus a reserve: a sixteenth of the pool,
between 1 and 4 GB. Where models share memory with a machine's workspaces, its free
memory leaves out the 3 GB kept for them. What it has to load comes from the catalog's `memoryNeedBytes`
or the estimate from its `sizeBytes`, whichever is larger, with 2 GB more for each
32K of context above 32K, less what is already loaded. A machine with every slot
in use, or one whose recent calls mostly failed or ran over three times slower
than its peers, goes behind every machine that passes and is used only when none
does. Memory is judged from the cockpit's capacity report (free memory and loaded
models, on every heartbeat); a machine that does not report is admitted on its
slots alone. `fleetCapacity` returns what each of your machines can take on: slots
in use, memory, loaded models, recent latency and tokens per second, whether it is
degraded, and for each model the room it wants and whether it fits.

The machine has the last word, and it runs the same sum on the same reading of
free memory. Its cockpit checks its own memory before handing the call to a
runtime, and a machine that cannot take the call ends it with
`insufficient_memory` having run nothing. That now happens mainly when memory has
moved since the last heartbeat. The call then goes to the next machine holding the
model, and when there is none, to the route's next source. No other failure moves
a call to another machine: one a runtime was handed may have generated, and running
it again would be the same generation twice.

## Explicit API choices and automatic Ask routing

Ask uses the cluster's routing policies automatically for each step. It has no
conversation-wide source picker. API callers may still supply an explicit
**source** and **level** on a turn (`AiChatMsg.provider` and `AiChatMsg.level`).
Those explicit choices apply to
**every model call of the turn's goal on every node** -- the planner's triage and
compile as much as the agent's reply.

| Source | Means |
|---|---|
| empty | Auto: the rules decide, exactly as without a choice |
| `app:claude-code`, `app:codex`, `app:*` | pin that app on the owner's machine |
| `fleet:strongest`, `fleet:fastest`, `fleet:<modelId>` | pin a local model |
| `federation:cheapest`, `federation:strongest` | pin a vendor |
| `policy:<name>` | a **route**: that policy's chain is walked instead of the rule's |
| `route:local`, `route:apps`, `route:<id>` | a person's route: its sources per level, never falling through (above) |

**A pin is the owner's own and is never substituted.** It is walked as a one-entry
chain, it skips every rule, and when it cannot serve the call refuses with
`every_door_shut` naming what was pinned (`[pinned to app:claude-code: a pinned
source is never substituted]`). Because the pin is the owner's, an app door reached
through it passes the app gate on their machine.

**A route fails over.** Its chain is walked entry by entry like a rule's -- so a
planner that cannot reach an app door yet answers from the entry behind it. A route
removed after the turn chose it is a plain error, not a park: no machine waking
would ever serve it.

**The level binds the run's steps** -- the reply, a compose. Calls made while
compiling (triage, the compile pass) and the failure classifier run outside any
step and keep the level they declare, while still going where the source says.
Embeddings, vision and audio are never re-routed by a choice.

A person's override for **one step** (re-run or branch) is more specific than their
explicit API choice for the goal, so its model and level each win over it.

**Where it lives.** The choice is written onto `v1:work:goal.routing` and
`v1:work:run.routing` (`{source, level, by}`, `by` stamped by the server) -- never
into the goal's input, which is what the model reads. The planner's compile claim
and the agent's dispatch each build their run context from the run ROW, and the
router applies the choice to every request it resolves (`router.ResolveFor` ->
`memql.ApplyRunRouting`), so no node needs anything from the one that took the
turn. A fork, a replay and a branch keep their source run's choice; a goal a run's
step opens (a compose, `agent()`) inherits it for the same owner.

**Refusals before any goal exists.** A word outside the table, or a level other than
empty / `fast` / `strong` / `reasoning`, refuses the turn with gRPC
`InvalidArgument` and metadata `code` = `route_source_invalid`,
`route_level_invalid` or `route_policy_unknown` (an unknown route), `field` =
`source` or `level`.

---

## Which account pays for a vendor call

Everything above chooses a **model**. For a text model at Anthropic or OpenAI
one step follows: which **account** pays. There are two kinds. The **cluster's
account** is the cluster's own sign-in with the vendor; it pays for the
cluster's own work, and for a person only when that person may use it
([who the cluster's account pays for](api-keys.md#who-the-clusters-account-pays-for)).
An [**API key**](api-keys.md) is a person's.

| The call | Accounts, in order |
|---|---|
| Has no person behind it (a schedule, a sweep, a seed, a synthetic identity) | the cluster's account only |
| Runs under a person's authority borrowed for a visitor (a shopper form, an access-code call) | the cluster's account, when the person whose authority is borrowed may use it. Never a key |
| Was made on a route its person chose, or a source they pinned themselves | their default key at the vendor, then keys lent to them (the key added earliest first), then the cluster's account when they may use it |
| Anything else: Auto, and every call nobody chose a route for | the same order, counting only keys whose owner turned on **Let Auto use this key** |

A choice is the acting person's own or it is not a choice: an agent's stored
pin is its owner's, and a run shared with another person keeps its opener's
route. It must also have been made by the person themselves. A source named
through an app session, a badge, delegated access or by the cluster on a
person's behalf is attributed to that person and counted as work nobody chose
(`byMachine` on the run's `routing`, `ChoiceByMachine` on the request).

**The account rides the chain.** A vendor record is walked once per account
that can pay, as `<record>@key:<id>` and then `<record>@cluster`. An answer
that is about the account (the key is not accepted, has no permission, is out
of credit or at a spending limit) is followed by **the same model on the next
account** before the route moves to its next model. Any other failure skips the
record's remaining accounts and follows the ordinary fallback. `providerName`
on the decision record stays the plain record name.

**Nothing that stops the cluster's account lets a key through.** The question
is asked after every gate that already stands in front of a vendor: a run's
no-paid-provider ceiling, the cost ceiling, the context floor and the modality
check. A record with no account that can pay is passed over like a shut door,
with each account's reason on its line in `considered`. A reason never names a
key.

**Judged again at the call.** Each chain entry is resolved by name when the
call is made, and a key is checked against its rows again there: one removed,
paused or refused since the resolution is not spent, and a reference to a key
the acting person may not use resolves nothing, exactly as a made-up one does.

**Text surfaces only**: chat, streaming chat, both tool surfaces and structured
output. Vision, embeddings, speech, transcription and images take no key: they
are the cluster's account or nothing. So for a person the cluster's account
does not pay for, a picture, speech, transcription or an image at a vendor is
passed over like a shut door, with that reason in `considered`. Embeddings are
paid by the cluster's account for every caller: the memory space is the
cluster's, one model for everybody.

**The cluster's account is judged per person, from rows.** Whether it pays is
`execute app:settings/clusteraccount` for the person the call is for (their
stored role, their groups and their grants; never the stand-in role work
continued on a person's behalf runs under). It is asked only where the cluster
has an account for the record, once per vendor in one resolution, and again
when `<record>@cluster` is resolved at the call.

[API keys](api-keys.md) has the rest: who may add one, lending, checks, what a
key's owner is told when it stops working, and what a cluster owner can do.

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
| `callerKind` | what KIND of caller made the call: `user`, `system`, `connector`, `anonymous` or `unattributed` |
| `cacheKind` | which CACHE answered, when one did: `exact` or `semantic`. Absent means a provider answered |
| `accountKind` / `accountKeyId` / `accountKeyOwnerUserId` | which ACCOUNT paid a vendor text call: `cluster`, or `key` with the key's id and its owner's. Absent for a source that takes no account. Never the key's name or fingerprint; a `fallback_used` row names the account that FAILED. When a key paid and the call failed, the row's `errorMessage` is the router's code and sentence, never the vendor's own words ([API keys](api-keys.md#reading-which-account-paid)) |
| `executionSurface` | which machine took the call, as `fleet:<registrationId>`, whether it served it or failed it. `outcome` says which, and a failed call counts against that machine in [fleet capacity](#fleet-capacity), including one that ran out of its caller's time. A machine that declined before handing the call to a runtime, or that nothing had been heard from when its caller gave up, is neither named nor counted |

**`session` is a fourth door, not a flag on `app`.** `app` means a subscription app
answered this TURN; `session` means one was handed the whole STEP and drove its own
loop (design D7 of the app-session record). They are separate values because this
field is what you filter on, and folding them would make the entire history of a
cluster whose only open door is a signed-in Claude Code read as ordinary chat turns.

**`callerKind` is what makes an empty `userId` an ANSWER.** A blank user used to
say two very different things at once -- "a person made this call and nobody wrote
down which one" and "no person made this call at all, it was a boot seed or a
retention sweep" -- so every unattributed row had to be read as a possible gap.
`system` is the cluster acting and correctly has no user; `unattributed` is a Go
call site that established no caller at all, which is the one honest answer for a
context nobody stamped. It is DERIVED from the context where the request is built,
never supplied by a caller, and **nothing routes on it**: no `@when` key reads it,
because a rule that did would turn evidence into an input.

**`cacheKind` is how you tell a cache hit from a provider call.** The exact-hash
response cache is consulted AFTER the router resolves -- its key folds in the
resolved provider name, which is what invalidates a cached answer when routing
changes -- so a hit is a call that matched a rule, walked a chain and took a door
without reaching a provider. Such a row names its cache and carries a REAL ZERO for
every token and every cost: nothing was sent, nothing was billed, and
`tokensEstimated` / `pricingConfigured` are false because nothing was estimated and
no cost was computed. Absent means a provider answered, which is what every row
written before this was. The whole decision travels with it, `considered` included
-- the calls a cache serves are by definition the ones that repeat, so dropping
their records would make them the decisions the rule corpus can never be checked
against. Before this a warm page wrote no rows at all and was indistinguishable
from an idle cluster.

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
argument that excludes `v1:worker:invocation` from the live feeds. They are written
through ONE bounded queue drained by ONE writer, so the ledger costs a node one
goroutine and at most one concurrent write however many calls are in flight; a full
queue drops the record and counts it on `RecordsDropped`, because a node whose
database cannot keep up with its own model calls must not grow its heap instead.

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
- [API keys](api-keys.md) -- a person's own account at a vendor, and who it pays for
- [The MemQL language](../language/memql.md) -- the `rule` construct and `@level`
- [Workers runbook](workers-runbook.md) -- machine routing, which happens *after* a
  model is chosen and is a separate decision

Fleet History displays [inference cost estimates](inference-cost-estimates.md)
with model/source evidence separately from billing. Local calls need an
owner-confirmed tariff and measured or explicitly estimated energy; unavailable
electricity evidence is not interpreted as zero cost.
