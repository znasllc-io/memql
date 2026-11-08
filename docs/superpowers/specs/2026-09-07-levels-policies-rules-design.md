# Levels, policies and rules as DSL -- Design

- **Date:** 2026-09-07
- **Status:** approved in the 2026-09-07 brainstorm as epic 2 of the local-first
  inference and routing program (`2026-09-07-local-first-routing-program.md`). The owner
  set the principles: paid inference is the last resort unless a policy says otherwise;
  local picks the best, paid picks the cheapest that qualifies; a call declares the level
  of intelligence it needs rather than a model; policies compose; built-in policies are
  seeded DSL that cannot be modified, with custom ones beside them; the engine carries
  mechanism, not policy. D1-D12 are rulings recorded for the owner to overturn.
- **Owner areas:** `component/language` (the `@level` annotation, the `rule` construct,
  policy entry forms), `component/memql` (loaders, registries, the AI runtime's
  resolution), `component/router` (the selection seam, selectors, decision records),
  `dsl/policies` and `dsl/rules` (seeded), `dsl/router` (the call row), every Go call
  site that reaches a model, `component/work` and `integrations/work` (the disconnected
  failure path), `component/grpc` (the chat handlers), `docs/public`.
- **Depends on:** nothing. Epic 1 is convenient. Epics 3, 4 and 5 depend on this one.

---

## 1. Problem

The platform has six shipped policies, every one of them local-first, and exactly two of
its thirty inference call sites go through them: the interactive and background agent
turns. The other twenty-eight resolve a prompt's `@defaultProvider` straight against the
registry, or take a provider off a list hard-coded in Go (`chat54, chat54Mini, chat54Nano,
chat54Pro, chat53Latest` in `component/memql/ai_providers.go`). Every one of those
defaults is a paid vendor model, and since epic memql#5088 removed vendor keys none of
them is reachable on a local cluster. So "policies govern AI provider selection" is true
of agent chat turns and of nothing else.

Nothing between "a call happens" and "a chain is walked" looks at what the call is for.
`ResolveRequest` carries no level, no modality and no purpose; the fleet wire's `Purpose`
field exists and "steers nothing" by its own proto comment; `v1:work:step.kind` already
knows whether a step reaches a model and never reaches the router. Three of the six
policy annotations (`@maxLatencyMs`, `@maxTimeToFirstTokenMs`, `@preferredRole`) are
parsed, stored, projected and consumed by no routing decision; the role-to-policy default
is a Go string literal in `integrations/agent/replier.go` while the DSL mechanism for it
(`DefaultForRole`) is dead code. The policy registry is keyed by bare name with no
core-first check, so a product bundle declaring a policy with a shipped name silently
replaces it. A call's minimum context window is read by both eligibility checks and set
by no caller. And the work spine's "reason again only when something breaks" half, the
nine-rule symptom table, the cheapest classifier prompt, the healing subscriber and the
replan prompt, has no production caller.

## 2. What the tree already has

- **The policy construct**: declarative, empty-bodied, registry-backed
  (`component/language/dslspec/constructs.go`, `parser/policy_decl.go`); annotations
  closed at `annotations/registry.go`; runtime `PolicyConfig.ProviderChain()`; loaded by
  `component/memql/unified_policy_loader.go` into a flat `byName` map; projected as
  `v1:router:policyCatalog` by `integrations/router`.
- **The router** (`component/router/router.go`): precedence explicit provider, policy
  name, caller default, registry default; the chain walk classifies each entry by door
  (`doors.go`: `fleet:` local, `app:` app, everything else federation), asks the cost
  ceiling before a federation hop only if a local door preceded it, resolves fleet and
  app names per acting user (`EntryForUser`), interface-checks the modality, and on
  exhaustion returns `work.RefusalEveryDoorShut` with a per-door report that is thrown
  away on success. `CloudConsent` is the one escape.
- **Fleet resolution** (`component/memql/fleet_provider.go`): `fleet:*` resolves at call
  time; `orderModels` is preference rank, then parameters descending, then context
  window descending, missing attributes last; `eligibleFor` gates on structured, tools,
  embeddings and a minimum context nobody sets. Machine choice is the owner's
  `v1:worker:routingPolicy` strategy.
- **The three resolution paths**: the router (agent turns only); the prompt path
  (`ai_runtime.go` `resolveProviderName`: context override, `prompt.DefaultProvider`,
  registry default; `engine_ai.go` `InvokeAIStructured` with a registry-wide scan and a
  schema-in-user-turn last resort; `ai_tool_loop.go` with a context-free `Entry`); and
  the direct path (`ChatProvider`, `StructuredChatProvider`, `SuggestChatProvider`,
  `ChatStreamProvider`, `VisionProvider`, `EmbeddingProvider` off the registry).
- **`@defaultProvider` may not name a policy**: `component/memql/prompt_default_provider.go`
  records the silent downgrade that rule prevents.
- **Cost figures** are on the provider records (`inputCostPerMillion`,
  `outputCostPerMillion`, `cachedInputCostPerMillion`); `component/work/budget.go` reads
  the run's ceilings; `component/memql/ai_guard.go` is the path-agnostic rate, repeat
  and cumulative-spend chokepoint, mirrored for fleet calls by `ai_guard_fleet.go`.
- **`v1:router:call`** (`dsl/router/concepts.memql`) records `promptName`, `policyName`,
  `vendor`, `model`, `providerName`, tokens, cost, latency, `outcome`, `billing`,
  `executionSurface`, `fallbackFromModel`.
- **Sealing**: core DSL domains cannot be shadowed by a pack (`dsl/pack_validation.go`)
  or a runtime mount (`dsl/runtime_mount.go`); the RBAC roles are re-seeded on every boot
  and guarded immutable; the runtime authoring pipeline resolves core first and refuses
  to promote a name a sealed construct owns (`component/memql/authoring_resolver.go`,
  `authoring_session.go`); `v1:authoring:construct.kind` already lists `policy` while
  `authoring_sandbox.go` refuses it; `component/emailrules` renders a construct from a
  form deterministically and activates it live.
- **The disconnected failure path**: `component/work/symptom.go` (`ClassifyByRules`,
  `ActFor`, `ApprovalKindFor`), `dsl/work/prompts.memql` (`classifySymptom`,
  `replanGap`, `compileGoal`), `component/healing/repair_loop.go`,
  `integrations/planner/work_heal.go`. None has a production caller.

## 3. Decisions

### D1 -- Three nouns: level, policy, rule

A **level** is how much intelligence a call needs, a closed set of four: `fast`, `strong`,
`reasoning`, `embeddings`. A **policy** is an ordered chain whose entries may be a
provider, a selector, or another policy. A **rule** maps a call's declared metadata to a
policy, in explicit precedence order. Modality is never declared; it is derived from the
call (chat, structured, tools, embedding, and, from epic 3, vision, audio in, audio out,
image). Chosen over more levels (an abstraction a person cannot hold in their head is
not one) and over naming models at call sites (a model name at a call site is a release
every time the fleet changes).

### D2 -- One seam: every call to a model goes through the router

Chosen over re-pointing thirty `@defaultProvider` annotations at local models. The prompt
path and the direct path are deleted as resolution paths. `MemQLEngine.InvokeAI`,
`InvokeAIStructured`, `InvokeAIChatWithFilteredTools`, the gRPC chat handlers, suggest,
the safety classifier, vision, embeddings, transcription and speech all build a
`router.ResolveRequest` and take what the router returns. `@defaultProvider` survives as
an explicit pin that rides `ExplicitProvider`, still refused when it names a policy. A
Go AST gate, in the spirit of `TestEveryServerPathDeclarationIsClassified`, fails the
build on a call to `providers.Entry`, `ChatProvider`, `StructuredChatProvider`,
`SuggestChatProvider`, `ChatStreamProvider`, `VisionProvider` or `EmbeddingProvider`
outside `component/router` and the registry itself.

### D3 -- Every prompt declares `@level`; every Go call site names one

`@level("fast")` is required on a `prompt`; a prompt without one refuses load. A Go call
site with no prompt (suggest, safety, vision, healing, embeddings, the chat handlers)
names its level in the request. The assignment shipped with this epic, from the
brainstorm's inventory: `fast` for triage, intake, `classifySymptom`, `docSummary`,
`askSpecialist`, `seedDomainBridge`, `seedDomainContent`, every suggest domain, the
safety classifier, healing's patch proposal; `strong` for `agentReply`,
`agentFactoryAnalyze`, `reactiveConductor`, `authoringDesign`, `trainerAgent`, the Ask
surface; `reasoning` for `authoringEmit`, `authoringRepair`, `replanGap`, `compileGoal`;
`embeddings` for every embedding site. The product pack's `agentReply` carries its own
`@level`, and the pack loader refuses a prompt without one exactly as the embedded tree
does.

### D4 -- Selectors, and a policy may name a policy

Policy entries are a closed grammar: a provider name; `fleet:strongest`, `fleet:fastest`,
`fleet:<modelId>`; `app:*`, `app:<id>`; `federation:cheapest`, `federation:strongest`,
`federation:<providerName>`; `policy:<name>`. `fleet:*` is retired in favour of
`fleet:strongest`; the shipped policies and the conformance gate are re-pointed, and
`fleet:*` refuses load with the new spelling in the message. `strongest` is measured
capability when present (epic 4), else parameters then context window, missing last;
`fastest` is measured throughput when present, else fewest parameters; `cheapest` among
federated records is input plus output cost per million ascending among available records
meeting the call's floor, a record with no cost figures sorting last and reported as
such. A policy naming a policy is expanded at load, cycles refuse load, and the expanded
chain is what the router walks. This is the composition the owner asked for: a rule names
a policy, the policy names a resolver.

### D5 -- The `rule` construct

Declarative, empty-bodied, registry-backed, in the `rule` keyword:

```memql
@description("Operators reason at the reasoning level")
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localFirst")
@precedence(50)
rule operatorReasoning { }
```

`@when` takes a closed key set, every key optional, all present keys ANDed: `level`,
`modality`, `prompt`, `role` (the agent's role slug, or the actor's cluster role under
`actorRole`), `tag` (a call tag such as `background`), `touches` (a concept id prefix the
call's footprint matches, `startsWith` semantics). `@policy` is required. `@level` on a
rule overrides the call's declared level. `@onUnavailable("degrade" | "park")` says what
happens when the chain is exhausted at the level. `@exclude("fleet:<modelId>")`, repeatable,
removes a concrete model from the chain's resolution (the demotion vehicle of epic 4).
`@precedence(N)` orders rules, highest first; a tie is a load error. `@locked` is
accepted only in the embedded tree and makes a rule evaluate before every unlocked one
regardless of precedence. The first matching rule wins; a call no rule matches falls to
the shipped `default` rule, which always matches.

### D6 -- The shipped rules and policies express paid-last

`dsl/rules/rules.memql` ships: `default` (no conditions, precedence 0, policy
`localFirst`, degrade); `reasoningParks` (`level="reasoning"`, park); `embeddingsPark`
(`level="embeddings"`, park, because a degraded embedder is a different vector space);
`backgroundLane` (`tag="background"`, `localFirst`, degrade); `backgroundEscalation`
(`tag="backgroundEscalation"`, level `strong`); `operatorReasoning` as above. The role
default in `replier.go` and the lane constants in `subagent.go` are deleted in favour of
these. `dsl/policies/policies.memql` ships `localFirst` (`fleet:strongest`, `app:*`,
`federation:cheapest`), `localOnly` (`fleet:strongest`), `federationStrongest`
(`fleet:strongest`, `app:*`, `federation:strongest`) and nothing else; `balancedChat`,
`strongReasoning`, `cheapestCapable`, `fastCoding`, `backgroundExecution` and
`backgroundEscalation` are deleted, since a policy nothing names is a decoration (the
file's own rule). The `@primary` and `@fallback` annotations stay; `@maxLatencyMs`,
`@maxTimeToFirstTokenMs` and `@preferredRole` are removed from the grammar and the
catalog projection, and `DefaultForRole` is deleted. An owner who wants a paid model for
everything adds a custom rule at a higher precedence naming a policy that starts at that
vendor; it is explicit, recorded on every decision, and the ceiling still governs it.

### D7 -- Locked means core-first, re-seeded, and never redefined

The `policies` and `rules` domains are core, so a pack cannot mount them and a runtime
mount colliding with them is skipped, which the tree already does. Three things are
added. The policy and rule registries refuse a duplicate name across the whole corpus at
load, replacing bare-name last-wins. Shipped definitions are re-read from the embedded
tree on every boot, so nothing an owner did to a shipped one survives a restart. A
runtime-authored rule or policy may add and may take precedence, and is refused when it
names a shipped one or carries `@locked`, which is the authoring pipeline's core-first
rule applied to two more kinds. `authoring_sandbox.go` gains compile paths for `rule` and
`policy`; the deterministic path takes a structured form (conditions, policy,
precedence, on-unavailable) from the OS and activates it live, the `component/emailrules`
shape. The natural-language path is epic 3.

*The deterministic path was superseded on 2026-11-08: see C5 in section 4b.*

### D8 -- The request carries what a rule can branch on

`ResolveRequest` gains `Level`, `Modality`, `Needs` (`Structured`, `Tools`, `Vision`,
`AudioIn`, `AudioOut`, `Image`, `MinContextTokens`), `PromptName`, `Tags`, `Role`,
`Touches` and keeps `UserId`, `RequestId`, `ExplicitProvider`. `MinContextTokens` is
produced from the rendered prompt plus the expected output by a fixed estimate (four
characters per token, rounded up, plus the prompt's declared or default completion
budget), never zero, and every entry whose context window is below it is skipped with a
reason. `Touches` is the step's footprint union for a work step and the knowledge domains'
concept ids for an agent turn, empty otherwise. A worker `Purpose` on the fleet wire is
set from the level and prompt name so the machine's ledger says what it served.

### D9 -- Degrade or park is a rule attribute, and the served level is a fact

When the chain is exhausted at the requested level and the winning rule says `degrade`,
the router walks the chain again at the next level down (`reasoning` to `strong` to
`fast`), records `servedLevel` and `degraded: true`, and returns the entry. When the rule
says `park`, the router returns the refusal with the door report, and the caller does
what it does today: a work step parks, an interactive surface shows the sentence. The
defaults are D6's. No degradation is silent: the decision record and the run's step both
say what served.

### D10 -- Every resolution is a decision record

`v1:router:call` gains `level`, `servedLevel`, `degraded`, `rule`, `policy`, `door`,
`considered` (the door report as `[{entry, door, reason}]`, kept on success as well as on
refusal), `touches`, `machineOwnerUserId` (empty until epic 4). One row per resolution,
written by the same writer the row has today, never broadcast (the volume argument that
excludes `v1:worker:invocation`), read on demand through a new `routerDecisionsRecent`
query gated owner-or-developer with `limit`, `since`, `level`, `door`, `rule` and
`outcome` arguments. A rule is falsifiable only if the decisions it made can be read.

### D11 -- The chat handlers go through the router and refuse with a code

`AiChatMsg` builds a request at level `strong`, modality chat or streaming chat, prompt
name `ask`, and refuses with gRPC `FailedPrecondition` carrying the refusal code and the
door report, never `codes.Internal`. The three spellings of "no provider available" in
`component/grpc/ai_handlers.go` and `component/memql/engine_ai.go` are deleted.

### D12 -- The disconnected failure path is wired, and only a table miss costs a call

The work dispatcher's failure path calls `ClassifyByRules` first, invokes
`classifySymptom` at level `fast` only on a miss, maps through `ActFor`, and acts:
retry inside the budget, `replanGap` at level `reasoning` for a plan miss with the
completed prefix kept, repair for a contract miss, and the healer for an environment
miss. `NewWorkHealer` is subscribed in the agent wiring. What stays in Go, and why: the
guards in `ai_guard.go` and `ai_guard_fleet.go`, the ceiling arithmetic in `budget.go`,
door classification, availability, refusal codes. A kill switch a policy can author
around is not a kill switch.

## 4. The change

- `component/language`: `@level` on `prompt` and `rule`; the `rule` construct (`dslspec`,
  parser, AST, annotations registry with the closed `@when` keys); the policy entry
  grammar with selectors and `policy:`; the three annotations removed.
- `component/memql`: `unified_rule_loader.go` (new), the policy loader with duplicate
  refusal, cycle detection and boot re-seed; `ai_runtime.go`, `engine_ai.go`,
  `ai_tool_loop.go` building requests instead of resolving names; the registry's direct
  accessors removed or made router-internal; `MinContextTokens` estimate in the
  invocation; the AST gate test.
- `component/router`: `rules.go` (evaluation in precedence, locked first), `selectors.go`
  (strongest, fastest, cheapest with the cost ordering), `resolve.go` (levels, needs,
  degrade or park), `decision.go` (the record); `types.go` for the request and resolution.
- `dsl/rules/rules.memql` (new), `dsl/policies/policies.memql` (rewritten),
  `dsl/router/concepts.memql` (the call row's fields), `dsl/router/queries.memql`
  (`routerDecisionsRecent`); every prompt in `dsl/**/prompts.memql` gains `@level`.
- `integrations/agent/replier.go`, `nonstreaming.go`, `subagent.go`: policy names and
  lane constants deleted; tags set instead. `component/grpc/ai_handlers.go`: the chat
  handlers. `integrations/embedding`, `knowledge`, `similarity`, `harnessrecall`,
  `component/memql/ai_semantic_cache.go`: embedding requests at level `embeddings`
  (the pinned model id stays until epic 3 owns the binding; the request names it as
  the explicit pin so nothing changes on the wire in this epic). `component/safety/llm`,
  `component/fileprocessor`, `component/healing`: requests with a level.
- `integrations/work` and `app/integrations_work_dispatch.go`: the failure path;
  `app/integrations_worker_agent.go` or its sibling: the healer subscribed.
- `component/memql/authoring_sandbox.go` and the pipeline: the `rule` and `policy`
  kinds; a `routingRuleActivate` builtin taking the structured form, owner or developer.
- `docs/public/language/memql.md` (the `rule` construct, `@level`, the entry grammar),
  `docs/public/ai/llm-cost-control.md` (the seam, what stayed in Go),
  `docs/public/operate/ai-routing.md` (new: levels, policies, rules for an operator),
  `CLAUDE.md` (the AI Integration and Policies sections), the architecture model
  regenerated.

## 4a. Corrections made while building (2026-09-07)

Recorded here rather than edited into section 4, so the record still shows what
was designed and what building it found. All four are also on issue memql#5127.

**C1 -- the vocabulary is in `core/airoute`, not `component/router/types.go`.**
`component/router` is its own Go module and IMPORTS `component/memql`; the
dependency does not go back. Every call site D2 re-points -- `InvokeAI`,
`InvokeAIStructured`, `InvokeAIChatWithFilteredTools` (which lives in
`ai_tool_loop.go`, not `engine_ai.go`), the semantic cache -- is inside
`component/memql`, so declaring the request beside the router would have made
the whole re-pointing an import cycle. Separately `component/safety`,
`component/fileprocessor`, `component/healing`, `component/work` and
`component/planner` depend on neither and take an injected
`common.ChatStructuredProvider` from their caller, so a home in
`component/memql` would have added five module edges to name a four-value enum.
`Level`, `Modality`, `Needs`, `ResolveRequest`, `Resolution` and `Decision` are
therefore in `core/airoute`, a package inside the existing `core` module that
every module already requires; `component/router` type-aliases the names, and a
test asserts the package imports nothing outside the standard library. The
engine reaches the router through an `AIResolver` interface it declares and
`app/` installs; unwired, it REFUSES rather than falling back, because the
fallback would be the registry default this epic deletes.

**C2 -- `@when`'s keys are validated by the rule parser itself.**
`annotations.KeywordArgs` looks like the closed-key mechanism and is not one:
its only consumers are `sense/complete.go` and `sense/hover.go`, so it drives
completion and hover only, and the generic `parseAttribute` accepts any key with
no validation -- `@trigger`, `@handler` and `@relationship` do not reject an
unknown key either. The registry entry is added for the editor as well as, not
instead of.

**C3 -- the work failure seam is `component/automations/journal.go`.** #5134
names `integrations/work/dispatch.go`; that file's `FailRun` handles only the
two PRE-execution failures, both of which happen before the executor opens its
journal. `closeRun` is where a terminal `failed` run is decided, and the branch
that wrote `status=failed` for everything that was not an inference refusal is
where `ClassifyByRules` went. The ACT is recorded on the run as a `waiting`
state and served by the sweep on a node that can, because the executor runs on
an agent replica while compile, replan and the sweeps run on the planner.

**C4 -- three things D2 names have nothing live to re-point.** `TTSProvider` /
`TTSProviderByName` and `openAITTSProvider.Synthesize` have ZERO callers and
there is no `AiSpeechMsg` handler at all -- speech is a wiring gap, not a call
site. Transcription goes through `integrations/stt.StreamingProvider`, which
never touches the provider registry. `memql.ContextWithCloudConsent` likewise
has no non-test caller; only the request field is read. The seam serves chat,
streaming chat, tools, streaming tools, structured, vision and embeddings, and
says in a comment why the other two are absent.

**Three smaller findings that changed acceptance.**

- **The shipped `default` rule made the whole authored tier unreachable.** It is
  `@locked` and states no conditions, and locked-first ordering put it ahead of
  every unlocked rule -- so it matched every call before any owner-authored rule
  was consulted, and D7's "a runtime-authored rule may add and may take
  precedence" was false of every rule anybody wrote. `default` is now exempt
  from the locked partition and always sorts LAST, by identity rather than by
  precedence value. It keeps `@locked` for the other two things the annotation
  means.
- **`MinContextWindow` had never run**, so `eligibleFor`'s context gate was dead
  code. Live and fail-closed, it refused every embedder, because neither
  embedding provider record declared a `contextWindow`. Both now declare the
  vendor's real 8191-token limit.
- **`v1:router:call` had no read surface, and it still has no tier.**
  `routerDecisionsRecent` ships a shape as well as a query. A tier was tried and
  REVERTED: `userId` on that row is ATTRIBUTION rather than ownership -- the row
  is the deployment's, the writer's actor is `system:router`, and
  `owner="userId"` asserts a guarantee nothing provides while reading as safe,
  which is what makes an auditor stop looking. `clusterOwner` is honest and too
  narrow: half of all decisions are system-triggered with an empty `userId`, and
  a developer asking why a call went to a vendor needs the ledger rather than
  their slice of it. The right shape -- an unowned row with a rank floor
  deciding who reads it -- is not expressible, because `unowned=` requires
  `rankVisible` which requires `owner=`. Filed as memql#5162; the read is gated
  by `@requiresRank("admin")` on the query meanwhile -- which on this ladder
  admits admin, developer and owner, so it is the WIDER of the two recorded
  readings and satisfies #5132's "owner or developer" rather than contradicting
  it -- and the construct is listed in the undeclared-population gate naming
  that issue. The width is the AI-settings record's D7, and it agrees with the
  shape this query projects, which deliberately omits the prompt and the error
  message so that an admin CAN read the list.

**One thing the epic deleted that section 4 did not name.**
`integrations/agents/factory.go` stamped `providerConfig.llm.policyName =
"balancedChat"` on every new agent row, defaulted from the role catalog's
`recommendedPolicySlug`, which 97 seeded role rows carried. The replier stopped
reading it when this epic re-pointed it, so the whole surface was a dead write
naming a deleted policy. Both fields are removed: an agent no longer names a
policy, and its ROLE is what a rule matches on.

## 4b. Corrections made after shipping

**C5 (2026-11-08) -- a custom rule is routing configuration, not an authored
construct.** D7's deterministic path rendered a form into a `rule` construct and
armed it into the owner-scoped authored runtime (`routingRuleActivate`). Nothing
that routes a call reads that runtime: the rule set the router walks is the
shipped and product-bundle rules plus the routing configuration document
(`PolicyRegistry.SnapshotRouting`), so an armed rule read active and decided no
call. Until memql#6022's follow-up Gate 1 also refused every generated rule, so
none had ever armed. The builtin was unreachable besides: it carried no `@sdk`,
so MemQL OS's fallback to it found no method, and its `when` argument is a
keyword that no call text can pass, so any rule it armed would have matched
every call. It checked no precedence tie against the live rules, and its policy
check read only the shipped policies.

The OS had already moved to `routingRuleSave`, which writes the one revisioned
document every replica reads on every call, under the loader's own
shipped-name, closed-key, policy-exists and precedence-tie checks
(`RuleRegistry.Finalize`). The owner chose to keep that path and retire the
other rather than make armed constructs route, which would have needed a second
store of custom rules checked against the first under a shared lock, with
cluster-wide names that memql#6022's per-owner bundle ids do not give. So
`routingRuleActivate`, `routingRuleRetire`, their router handlers, the app
adapters and the arming half of `component/routingrules` were deleted, and
Gate 1 refuses a `rule` construct in any bundle, naming `routingRuleSave`.
Nothing outside this repository called either builtin, and the deprecation
window does not apply: it covers language forms, and the surface baseline lists
no builtins. The `@args(when=, excludes=)` keys stay accepted and inert, because
dropping them would narrow a recorded annotation form.

What D7 says about locked rules stands. The failure mode below, "two custom
rules at one precedence", is refused at SAVE with both names.
`component/routingrules.GenerateRule` survives as the nightly evidence review's
one renderer (epic memql#5146, D5). Nothing applies an approved review yet:
approving records the decision and does not change routing, and the approval's
wording now says so. Wiring an approval into the routing configuration is
separate work.

## 5. Failure modes

- A rule set with no match: impossible, `default` always matches and refuses removal.
- Two custom rules at one precedence: refused at activation with both names.
- A policy chain that is empty after `@exclude`: the door report says so and the
  `onUnavailable` attribute decides.
- A federation entry with no cost figures under `federation:cheapest`: sorts last and the
  decision record names it, so a vendor record missing its prices is visible rather than
  silently free.
- The estimate under-counts a prompt's tokens: the provider truncates, as today, and the
  decision record's `minContextTokens` beside the model's window makes the miss
  diagnosable. The estimate is deliberately conservative.
- A bundle mounted at `MEMQL_DSL_PATH` declaring a prompt without `@level`: strict boot
  refuses, `MEMQL_DSL_ALLOW_SKIPS` is the break-glass, as for every contract gate.
- The failure path classifies a novel error as `unknown`: the act is `ask`, which is what
  it was before anything was wired, so wiring can only reduce the number of times a
  person is asked.

## 6. Testing

- Language: parse and refuse cases for `rule`, `@when` keys, `@level` values, the entry
  grammar, `fleet:*` refused with the hint, `@locked` refused outside the embedded tree.
- Loaders: duplicate names refused across namespaces; cycles refused; boot re-seed
  restores a shipped policy; the authoring path refuses a shipped name.
- Router: precedence and locked-first; every selector's ordering on fixtures, including
  the cost ordering and the no-cost case; capability and context filtering; degrade
  walks down and records; park returns the report; `ExplicitProvider` still wins.
- The AST gate for direct registry access.
- Conformance: every prompt in the corpus carries `@level`; every shipped rule names a
  shipped policy; the assignment in D3 is asserted by name.
- Call sites: a request from each site carries the expected level and modality; the
  chat handler's `FailedPrecondition` with the code.
- Decision records: fields on success and on refusal; `routerDecisionsRecent` gating and
  filters.
- The failure path: a transient error reaches the table and costs zero calls; a novel
  error costs exactly one call; a precondition miss raises a `planReview`; a plan miss
  invokes `replanGap` with the prefix kept. The proving suite gains a scenario for the
  first two, whose negative control is the classifier being reached.
- `TestNoDatabaseProductClaims` and `TestNoVendorApiKeyEntryPoint` unaffected; the
  llm-cost-control doc's accounting kept current.

## 7. Delivery

Three PRs.

- **PR 1, language and seeds.** Task 1: `@level` on prompts and the call-site request
  field, with the corpus assignment. Task 2: the `rule` construct, the entry grammar with
  selectors and `policy:`, cycle detection, duplicate refusal, boot re-seed, the three
  annotations removed. Task 3: `dsl/rules/rules.memql` and the rewritten
  `dsl/policies/policies.memql`, the conformance gates, the retired policies and the
  Go literals they replace.
- **PR 2, the seam.** Task 4: rule evaluation, selectors, needs and context estimate,
  degrade or park in `component/router`. Task 5: decision records on `v1:router:call`
  and `routerDecisionsRecent`. Task 6: every call site through the seam, the direct
  accessors removed, the AST gate, the chat handlers' refusal.
- **PR 3, the failure path and docs.** Task 7: `ClassifyByRules`, `classifySymptom`,
  `ActFor`, `replanGap` and the healer wired, with the proving scenario. Task 8: the
  authoring compile paths for `rule` and `policy` with the structured activation
  builtin. Task 9: docs and the architecture model.

The picking-up session writes the plan from this record with `superpowers:writing-plans`
and deletes it in the epic's merge.

## 8. Out of scope

- Natural-language rule authoring, simulation and the free-form classifier (epic 3).
- The model catalog and the embedder binding (epic 3); measured capability and
  `@exclude` proposals (epic 4); shared machines (epic 4).
- Any change to `ai_guard.go`, `ai_guard_fleet.go` or `budget.go`.
- Worker routing strategies (`v1:worker:routingPolicy`): they choose a machine after a
  model is chosen and stay as they are.
- An OS surface for rules beyond what epic 5 builds; this epic ships the reads and
  writes it needs.

## 9. Facts to re-verify before starting

The policy annotation set in `annotations/registry.go`; that `resolveChain` still gates
the ceiling only after a local door; the `orderModels` and `eligibleFor` signatures; that
`MinContextWindow` still has no producer; the exact list of direct registry accessors
and their callers; that `ClassifyByRules`, `classifySymptom`, `NewWorkHealer` and
`replanGap` still have no production caller; that `authoring_sandbox.go` still refuses
`policy`; the `v1:router:call` field list. All were read on 2026-09-07 at commit
907e385fb.
