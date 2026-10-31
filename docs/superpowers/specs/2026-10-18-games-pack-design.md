# Games pack: memory and skill learning for external game runtimes (design)

**Status:** proposed, 2026-10-18. Plan:
[`docs/superpowers/plans/2026-10-18-games-pack.md`](../plans/2026-10-18-games-pack.md).
Branch `feat/games-pack`, based on `origin/main` `f0af5373d`.

**Owner:** znas (dev@example.com)

**Motivating consumer:** sub-project S2 of the MemPals program
(`znasllc-io/memql-znas` `docs/design/2026-10-18-mempals-program-design.md`):
AI companions that play a block-building game with children, remember every
player, learn skills, and get cheaper the more they play. Its runtime (S1,
`docs/design/2026-10-18-mempals-runtime-design.md` in the same repository)
plays in real time and calls memQL for memory, skills and model decisions.
This record names MemPals as its first consumer the way earlier records name
their instances; **the pack's code, DSL and public documentation name no game,
no product and no character.**

---

## 1. What this is

A **pack** in the default build, `packs/gamespack`, domain `games`, concepts
`v1:games:*`, shipped **disabled**. It gives a game runtime that runs
**outside** the cluster four things:

1. **Memory** about the people who play: players, episodes, preferences,
   promises, places -- scoped per player and per character (the AI companion
   the runtime animates), each episode with an importance, embedded on write,
   and recalled by relevance x recency x importance.
2. **Skill learning for an external runtime**: the runtime records the plans
   it executed; the engine generalizes two agreeing successful plans into a
   skill with holes, governs it on the **same certification ladder as learned
   procedures** (`component/work.Advance`), asks the owner once, and answers
   "which skill should run instead of a model call". The runtime executes
   every step itself.
3. **Live-graph forwarding** for its concepts, so a live memory view attached
   to one replica sees rows written through another.
4. **Engine seams** a game needs, each separately reviewable: a DSL-settable
   lane for `ai()` (in this milestone); per-call attribution, retention for
   `v1:router:call` and a typed refusal over ExecuteQuery (follow-ups, planned
   in full).

### 1.1 Goal of the local end-to-end milestone

On the owner's local k3d cluster, with the pack enabled, a client acting as
the owner can: record episodes and recall them ranked; register players,
preferences, promises and places; forget a player outright; record plan
traces and have a skill learned; compare model plans in shadow until a
promotion is proposed; approve it; report canary runs until it is trusted;
look it up by signature, trigger and meaning; and watch every one of those
rows arrive live on connections attached to both bff replicas. The S3 product
(memql-znas) builds its runtime adapters and its site on exactly the names in
section 5.

---

## 2. Decisions

| # | Decision | Why, in one line |
|---|---|---|
| D1 | A **pack in the default build** (`packs/gamespack`), anchored in `packs/anchor`, no build tag, `RegisterPackDefault("games", false)`, governed by `v1:platform:packState`. Not a storefront pack. | Same shape and lifecycle as `packs/reviewspack` (`packs/reviewspack/pack.go:42,360`); holding data about children is the owner's decision, not an upgrade's; no shopper surface, so only an owner flips it. |
| D2 | `anchor.Storefront()` is renamed `anchor.Packs()` (it now also registers games), `anchor.Domains()` lists all three, and `anchor.StorefrontDomains()` keeps the developer-flippable set. `app.anchorStorefrontPacks` becomes `app.anchorPacks` in `app/anchor_packs.go`. | The anchor exists so "which packs does a default image carry" has one answer (`app/anchor_storefront_packs.go:54-58`); pre-release rule: rename and fix every caller, and every live reference to the file, in one commit. |
| D3 | Concepts `player`, `episode`, `preference`, `promise`, `place`, `skill`, `planTrace`, all `@rowAuthz(owner="ownerUserId", clusterOwner)`, ownerUserId stamped from the actor. The plan-trace concept is `planTrace`, not `trace`. | Every row is the owner's (the runtime and the site both act as the owner); `clusterOwner` is also what lets the hourly retention sweep's maintenance principal admit the rows; `trace` is a reserved engine name. |
| D4 | The AI companion is a **`characterId` string**, not a concept. | The brief's concept list has no actor concept; a free slug keeps the pack game-agnostic and needs no relationship; a `character` concept is a follow-up if the live graph wants character hubs. |
| D5 | Retention is **declared**: `@retention(ttl="30d", keepLatest=false, archive="none")` on `episode` and `planTrace`; `keepLatest=true` with the same TTL on every other concept. | The engine's generic sweep (`component/retention/sweep.go:67`, hourly `retireDeclaredConceptVersions`, `dsl/platform/automations.memql:187`) already does this; "raw episodes 30 days" is one line; `archive="none"` because an archive copy of a child's memories would outlive the deletion. |
| D6 | **Recall is a pack builtin**, `gamesRecall`, one SQL statement over `v1:games:episode` scored and limited in SQL, plus the player's preferences, the character's open promises and the world's places. **Not** a change to core `recall`. | The score needs a pack field (importance) and the scope two pack fields (character, player); generalizing `recall` (`integrations/harnessrecall/recall.go:355-363` scopes by owner only) is a platform design with callers in bundles this repo cannot see; the statement is harnessrecall's shape over one concept the pack owns. |
| D7 | Relevance is a **LEFT JOIN**: no embedder bound, no vector stored, or a query the model cannot embed scores relevance 0 and still ranks by recency and importance; every row says `relevanceAvailable`. | The fleet is a laptop that sleeps; recall must never fail because a model is away. |
| D8 | Embeddings are **written on create** by `gamesRecordEpisodes` through `integration.embedding.store` (declared again as `gamesEmbedStore`), on the **background lane**; a failed embedding leaves the episode pending; `gamesEmbedPending` back-fills. No automation. | One write path for vectors (the library's precedent, `dsl/library/builtins.memql:106-126`); an automation would run as a synthetic reader-role actor and reach only system-opted fleet machines (`component/automations/executor.go:68`) and journal every firing; the background bucket keeps memory bursts off the decide path's interactive budget. |
| D9 | Skill learning **reuses `component/work`** -- `Advance`, `LadderPolicy` read from `v1:authoring:ladderPolicy:primary`, `DecideServe`, `NormalizeStatement` -- and **does not reuse `component/procedure`**. | The ladder is the shared contract ("the same ladder as memQL's learned procedures"); `component/procedure` generalizes recorded shell/file/MCP steps (argv and path parsing, data-flow holes from step results, corpus compression, environment preconditions), none of which a game action has, and its leaf-path holes do not fit the runtime's one-hole-per-argument template. |
| D10 | Goal signature = `hex(sha256(NormalizeStatement(goalText) + "|" + join(sort(inputKeys), ",")))`; the engine recomputes it and refuses a supplied one that differs (`games_signature_mismatch`). | The runtime computes it offline and its contract fixes the vertical bar; `work.GoalSignature` (`component/work/compile.go:58`) uses `"\x00args\x00"`, so the S1 spec's "same construction" is true of the normalization only, and a refusal on the first trace beats a silent never-match. |
| D11 | Learning: two or more **successful, non-negative, non-skill-run** traces of one signature with the **same action sequence** generalize -- an argument equal (JSON equality) in every trace is a constant, one that differs is a hole named `<stepIndex>.<argKey>` -- into a skill born in **shadow**. Learned **once per (owner, signature, action sequence, generation)**; a retired skill is terminal and the pair is learned again at the next generation. | Mirrors S1's rule; disliked recordings and replays are excluded exactly as learned procedures exclude them; a template a person approved must stay the version that runs. |
| D12 | The ladder maps onto `work.Advance` unchanged: shadow comparisons are `EventShadowCompared`, runs are `EventReplayed` (`Insufficient` always false), the owner's decision is `EventPromotionDecided`, the sweep is `EventSweep`. k distinct bindings are counted **per hole**. "Awaiting approval" is shadow with an open promotion. | Sharing the state machine is the point; per-hole counting is `Advance`'s semantics and is stricter than S1's whole-binding count -- the engine's rule wins and S3 must not re-implement it. |
| D13 | The promotion is **the skill's own field** (`promotionApprovalId = "promotion-" + digest[:16]`), decided by `gamesDecideSkillPromotion(skillId, templateDigest, approved)`; not a `v1:work:approval`. | An approval requires a run in effect (`dsl/work/concepts.memql:193`) and a game skill has none; pinning the digest keeps "an approval never carries to another version". |
| D14 | Lookup answers with `work.DecideServe`'s verdict: by signature the trusted skill else the canary one; by trigger the most recently used trusted skill; by meaning the trusted skill nearest at or above **0.82**. | One serving rule with learned procedures (`component/work/replay.go:84`); 0.82 is the engine's near-match floor (`integrations/planner/agent_loop_authoring.go:55`). |
| D15 | Every ladder move is **one critical section**: `memql.AcquireWriteGate` keyed on the skill, the row read with `memql.ContextWithFreshRead`, the version stamped strictly after the one read. Learning takes a gate keyed on (owner, signature). | The measured lost-update rule (CLAUDE.md, memql#5431; `integrations/procedure/ladder.go` review finding C1); two characters report runs of one skill through two replicas. |
| D16 | Three writers are `@serverOnly` and called under internal origin with the caller's actor kept: `createGamesPlanTrace`, `createGamesSkill`, `recordGamesSkillLadder`. Every other write is a plain owner-tier mutation. | A client-reachable ladder write is a promotion nobody earned and a trace nobody checked teaches a skill nobody showed (the work journal's argument, `dsl/work/mutations.memql:6-18`). |
| D17 | Live graph: **broadcast** forward rules for `graph.node.created|updated.v1:games:{player,episode,preference,promise,place,skill}`; `planTrace` deliberately local. | Default-deny would freeze a live view on the other replica (`component/node/routing.go:47`); no automation consumes these events; plan traces are per-plan volume nobody watches live. |
| D18 | **Forget** is a hard SQL delete scoped to the caller's ownerUserId, in one transaction: the player; every episode, preference and promise about them; whatever else of the caller's names their gamertag (now or ever held) as a whole name -- plan traces, skills, and the episodes of anyone, preferences, promises and places -- and the vectors of all of it in every embedder space. A skill that names nobody stays. | The obligation is that the data is gone, wherever it was written down; the Shopify purge is the precedent (`integrations/shopify/compliance.go:494-521`), and S3's runtime deletes the same set in its local forget, so the two agree on what "forgotten" means. |
| D19 | Seam 1, a **DSL-settable lane for `ai()`**, is in the milestone; seams 2-4 are follow-up tasks, planned in full. | Section 4.7. |
| D20 | Row ids are **client-chosen bare slugs**, unique per concept **across the cluster**; the contract derives them with the owner's subject in the hash. The engine derives skill ids the same way. | An insert on an existing id is a read-merge onto it, and the owner -- a cluster owner -- passes the owned tier's write guard on any row, so a shared id silently merges two owners' rows. |
| D21 | Refusals **lead with stable codes** (`games_*`), declared in `RefusalCodes` and held to the source by a test. | The router's convention (`component/work/inference.go:45-55`); a client matches a word, and the follow-up seam 4 lifts the same words into a field. |

---

## 3. What exists, read on `origin/main` `f0af5373d`

- **Packs.** A storefront pack embeds `all:dsl`, registers its tree, declares
  its default and binds its Go half to its domain
  (`packs/reviewspack/pack.go:360-373`); `packs/anchor/anchor.go:43-53` links
  them once per process for the app, `memqllint`, the LSP, the package
  analyzer and the conformance corpus. Enablement is a `v1:platform:packState`
  row folded over declared defaults at app phase 3
  (`app/pack_enablement.go:32-64`, `component/memql/pack_state.go:64,137`); a
  disabled pack is **mounted-inert** (concepts load, behaviour does not) and a
  flip is **restart-required** (`dsl/pack_enablement.go:21-47`). An owner
  flips it with `SetPackEnabledMsg` (OS **Cluster > Modules**, or
  `sdk/go/modules.Client.SetPackEnabled`), which runs `mutation
  setPackEnabled` under internal origin (`component/memql/module_registry.go:225`,
  `dsl/platform/mutations.memql:1180`).
- **A builtin writes through a mutation.** "A builtin's returned nodes are the
  expression's VALUE and are never persisted" (`packs/wholesalepack/application.go:227-232`);
  the Go half renders its own mutations with `langparser.RenderCall` and
  `engine.Execute` under the caller's actor.
- **Live graph.** `node.RegisterRoutingRule` from Go; block rules first, then
  forward rules, else local (`component/node/routing.go:33,47,917`);
  `node.ForwardsGraphEvent` is the exported answer (`routing_reach.go:72`).
  Row admission gates every graph event per stream (CLAUDE.md "Authorization
  model"); an owned row reaches its owner's streams and a cluster owner's.
- **Memory today.** `recall` scores recency x relevance in one statement but
  filters by owner only (`integrations/harnessrecall/recall.go:355-412`);
  `belief` has no subject field (`dsl/memory/concepts.memql:26`); nightly
  consolidation is a placeholder (`dsl/memory/logic.memql:72-87`).
- **Embeddings.** The cluster's embedder binding decides the model and the
  vector table (`component/memql/embedder_binding.go:105,227`,
  `embedder_runtime.go:104`); `integration.embedding.store` is the one write
  path (`integrations/embedding/embedding.go:401`).
- **Learned procedures are coding-app only.** Recording opens from a cockpit
  app session (`integrations/work/session.go:118`); a browser, PAT or service
  account records nothing (`component/mcp/recording.go:113-118`); replay
  targets are `workbench | machine` (`component/work/target.go:36-38`). The
  ladder itself is pure: `component/work/ladder.go:119` (policy defaults
  5/2/5/2/1/30), `:271` (`Advance`); serving is `replay.go:84`
  (`DecideServe`); the policy row is `dsl/authoring/concepts.memql:184`, read
  under the owner as `integrations/procedure/ladder.go:284` does.
- **`ai()`.** `builtin ai(templateId, data, responseSchema, progress)`
  (`dsl/agents/builtins.memql:169`, `integrations/agents/ai.go:169`) routes by
  the prompt's `@level`; a `@defaultProvider` pin wins over every rule. The
  rate guard has an interactive bucket (20 calls / 10 s) and a background
  bucket (40 / 10 s) chosen by `ContextWithBackgroundLane`
  (`component/memql/ai_guard.go:82,172,182,894`); fleet calls pass the same
  guard at the provider seam (`ai_guard_fleet.go:81`, `fleet_provider.go:1030`),
  embeddings included. The only producer of the background lane is the agent's
  non-streaming loop (`integrations/agent/nonstreaming.go:133,271`), which also
  tags its requests `background` for the `backgroundLane` rule
  (`dsl/rules/rules.memql:44-49`). Attribution reaches the request from the
  context (`component/memql/ai_attribution.go:63,108`).
- **`v1:router:call`** has no retention (`dsl/router/concepts.memql:79`).
- **Refusals over ExecuteQuery.** The server sends `QueryError{code, message,
  metadata}` and fills metadata only from a `StructuredError`
  (`component/grpc/server.go:866-895`, `memql.proto:828`); the TS SDK throws
  a plain `Error("<name>: <message>")` (`sdk/ts/src/client/query.ts:152`).
- **Concurrency primitives.** `memql.AcquireWriteGate`
  (`component/memql/write_gate.go:157`), `memql.ContextWithFreshRead`
  (`fresh_read.go:36`), and the "stamp after the version read" rule
  (`component/emailrules/fire.go:261`).

---

## 4. Design

### 4.1 The pack

```
packs/gamespack/
  pack.go            Domain, DefaultEnabled=false, Tree, Provider, NewProvider, Register
  capabilities.go    the Go half's capability list
  routing.go         live-graph forward rules
  values.go          concept ids, actor/owner, id spelling, number narrowing, versionAfter
  store.go           rows through the engine under the caller's actor
  store_internal.go  the three @serverOnly writers, under internal origin
  space.go           the cluster's embedder space (read vectors, dispatch writes)
  episodes.go        gamesRecordEpisodes, gamesEmbedPending
  recall.go          gamesRecall
  forget.go          gamesForgetPlayer
  signature.go       GoalSignature
  template.go        Template, Generalize, Bind, Instantiate, SkillID
  gate.go            the write gate
  skills.go          gamesRecordTrace + learning
  ladder.go          rows <-> work.LadderState, moveLadder, serving
  ladder_calls.go    gamesShadowCompare, gamesReportSkillOutcome, gamesDecideSkillPromotion, gamesSweepSkills
  lookup.go          gamesFindSkill
  refusals.go        RefusalCodes
  dsl/               memql.toml, namespace.pin, concepts, shapes, queries, mutations, builtins
```

`Register("games")` registers the tree, declares the default (false),
registers the routing rules, binds the Go half to the domain and registers it
for the plugin contract. It does **not** call `RegisterStorefrontPack`.
`packs/anchor.Packs()` calls it once per process; `anchor_test.go`'s
storefront checks move to `StorefrontDomains()`.

**Enabling** (owner only): Cluster > Modules > games > Enable, or
`SetPackEnabledMsg`; then restart every engine node (and a product bff that
mounts product DSL). Until then the concepts load and nothing else does.

### 4.2 Concepts

Section 5.1 is the field-level contract. Shape of the model:

- `player` -- a person the owner registered: `gamertag`, `displayName`,
  `consented`. Personal fields carry `@pii`.
- `episode` -- one remembered moment: `characterId`, optional `playerId`
  (relationship `about` to player), `worldId`, `at` (game time), `kind`
  (`chat | event | activity | session`), `summary` (<= 400, the embedding
  source), `importance` 1-10.
- `preference` -- `playerId`, `subject`, `valence` -1..1, `confidence` 0..1,
  `evidence` (episode ids).
- `promise` -- `characterId`, `playerId`, `text`, `status`
  (`open | kept | dropped`), `madeAt`.
- `place` -- `worldId`, `name`, `kind` (free text: a new kind is not an engine
  change), `position` (object), `discoveredBy`, `at`. Shared by every
  character in the world.
- `skill` -- `goalText`, `goalSignature`, `actions`, `template`,
  `templateDigest`, `triggers`, `confirmation`, `learnedFrom`, `learnedAt`,
  and the ladder: `rung`, `state`, `shadowMatches`, `canaryMatches`,
  `distinctBindings`, `failures`, `promotionApprovalId`,
  `promotionProposedAt`, `lastReplayAt`, `lastBinding`, `lastReason`,
  `rungChangedAt`.
- `planTrace` -- one executed plan: `characterId`, `goalText`,
  `goalSignature`, `trigger`, `inputs`, `steps`, `actions`, `outcome`,
  `reaction`, `at`, `say`, optional `skillId` (relationship `ranSkill`).

`ownerUserId` declares `@relationship(type="parent", target=user)` on every
concept, so the engine stores it **canonical** (`v1:identity:user:<id>`)
while the actor carries the bare id -- every hand-written statement compares
`ownerKey(owner)`, never the bare actor id (measured in the prototype: a bare
comparison matches nothing and says nothing).

### 4.3 Memory

**Write.** `gamesRecordEpisodes(episodes)` validates up to 50 episodes, writes
each with `createGamesEpisode` under the caller, then embeds each summary
with `gamesEmbedStore` on `memql.ContextWithBackgroundLane`. A failed
embedding is logged and counted `pending`. `gamesEmbedPending(limit)` lists
the caller's episodes and skills with no vector in the **current** embedder
table (all of them when the table does not exist yet) and embeds them,
answering `more` when a full page was found. A binding change therefore heals
by calling it until `more` is false.

**Recall.** One statement (`recall.go`):

```
score = wRelevance x max(cosine, 0) + wRecency x exp(-ln2 x ageHours / halfLife) + wImportance x importance / 10
defaults 0.5 / 0.3 / 0.2, halfLife 72 h, k 8 (1..50)
scope    ownerUserId = canonical owner AND (characterId = $character OR playerId = canonical $player)
ageHours from the episode's `at` to `asOf` (default: now)
```

The pure `recallScore` is the formula and the database test holds the
statement to it (prototype-verified: SQL and Go agree to 1e-6). After the
episodes come the player's first `preferences` (default 3) by confidence,
the character's open `promises` (default 5) to the player, and the world's
`places` (default 10). Every row carries a **`rank`** (1 first); clients
order by rank, because the order rows arrive in is the transport's (measured:
an in-process read reversed it).

**Forget.** `gamesForgetPlayer(playerId)` reads the player under the caller
(fresh), refuses `games_player_not_found` unless it is the caller's, then in
one transaction deletes, scoped to the canonical owner and in every version:
the player row; every episode/preference/promise whose `playerId` is the
player; every episode, preference, promise and place of the owner whose text
names a gamertag the player has or ever had, as a whole name in any case,
whoever the row is about (the same rule as the traces', on the one free text a
person or a character wrote in each: an episode's `summary`, a preference's
`subject`, a promise's `text`, a place's `name`); every plan trace whose payload
text names a gamertag the player has or ever held, as a whole name (not a
substring: `Tag` leaves `Tagger`, `stage` and `tags`) in any case, with the
trace's own particulars (owner, character, skill, outcome, reaction, time)
left out of what is matched; every skill of the owner whose words name it; and the vectors of every deleted id in
every `node_vectors*` table; then evicts the seven concepts' caches on every
replica. A skill's words are its goal, the line it says, the constants of its
template and the values of its last binding, matched by the traces' rule on
values only (a key, an action's name and a hole's id are the runtime's words,
not a person's); a skill that names the player in any one version is deleted
in every version, with its ladder and its vector, and one that names nobody is
kept (a skill is a procedure, not a memory of a person). A row of the other
kinds that names the player in any one version goes the same way.

*What is not read.* Identifiers and enums, by contract and in every producer: an
id, a `characterId`, a `worldId`, a `playerId`, a `kind`, a place's finder
(`discoveredBy`, "the character that found it"), the ids a preference was
inferred from (`evidence`, "bare ids of the episodes"), a time, a status. A
gamertag that equals or begins one of them would delete every row that has it:
a player tagged like a pal (the pal's id is what `discoveredBy` holds, and the
runtime leaves a player's name out of it) would take every place the pal found,
and a player tagged like the prefix of the ids the runtime makes (evidence holds
ids such as `ses-<32 hex>`, in which a name before a dash is a whole name) would
take every preference that has evidence. Reading them could only over-delete by
collision, which deletes the owner's learning and erases nothing a person wrote;
so the concepts say these two fields must not hold a player's name, and a
forget does not read them. (A first version of 8b read both, and was reverted
for exactly these collisions.)

*The answer's keys (the choice).* The rows that name the gamertag are counted
in the keys the rows about the player already have: `episodes`, `preferences`
and `promises` count both together, each row once however many rules found it;
the places, which are about nobody, are a new key, `places`. A key for each
rule (`namedEpisodes`...) was the other way; a caller asks how many episodes
went, not by which rule, and would only have added the two up.

*Skills are fenced against their ladder moves.* A move holds the gate
`games-skill:<id>` from its fresh read to its write (D15); a version it commits
after the forget's delete began is one the delete's snapshot never sees, and the
skill would survive with the words that name the player while its vector was
gone. So forget first lists the skills it will delete (those that name the
player at that moment; section 8 says what is left), takes each one's gate on
its own transaction (a transaction-scoped advisory lock on the gate's own key,
`pg_advisory_xact_lock`, borrowed through `gatecontext.WithTransaction` so that
no connection of the gate pool of four is held while forget works), and keeps
it until it commits: a move in flight finishes first and the delete sees its
version; a move that starts meanwhile waits for the commit and finds no skill
(`games_skill_not_found`, passed over by a comparison and a sweep). The ids are
taken in order, so two forgets only wait for each other. A gate not had within
six seconds refuses the forget whole (nothing is deleted, the busy error is
returned, and it may be sent again), and one forget can hold as many skills as
the server's lock table has room for (a few thousand), failing whole beyond.
The other limits are in section 8. Matching follows the database collation; a gamertag equal to a plain word or a JSON literal (`look`, `true`, `5`) deletes the caller's traces that contain it; a literal backslash followed by `b`, `f`, `n`, `r` or `t` counts as a boundary; earlier gamertags are known only while older versions of the player survive; `displayName` is not matched; free-text mentions with no `playerId` of the player are found only by the gamertag; embedding-cache entries keyed by a text digest stay until their 30-day expiry.

No `graph.node.deleted` event is published, as for every delete in this
engine; a live view re-reads.

### 4.4 Skill learning for external runtimes

The path is record -> learn -> ladder -> lookup, with the runtime executing.

**Record.** `gamesRecordTrace` validates the trace, recomputes the signature
(D10) from `goalText` and the KEYS of `inputs`, derives `actions` from the
steps and writes `createGamesPlanTrace` under internal origin. A trace that
is a success, not `negative`, and not a skill run (`skillId` empty) then
**learns**.

**Learn** (`skills.go`), inside the gate `games-learn:<owner>:<signature>`
with fresh reads:

1. Read the skills of the signature. If any with the same action sequence is
   not retired, stop (learned once). Otherwise the generation is 1 + the
   number of retired siblings.
2. Read the newest 20 successful, non-negative traces of the signature;
   keep those with the same action sequence and no `skillId`.
3. Fewer than two: stop. Otherwise `Generalize` them (equal arguments stay
   constants, differing ones become holes `<i>.<key>`; differing key sets
   refuse), and create the skill at `SkillID(owner, signature, actions,
   generation)` in shadow with the newest trace's goal text and `say` as
   `confirmation`, the union of triggers, and the trace ids. Embed the goal
   text (background lane) for meaning lookups. The gate is held for the reads
   and the write and let go before the embedding, which can take as long as a
   model takes; a skill missing its vector is back-filled by
   `gamesEmbedPending`. Before taking the gate, an ungated look at the skills
   of the signature (possibly from this node's cache) stops early when a live
   skill of these actions exists.

**Ladder** (`ladder.go`, `ladder_calls.go`). `moveLadder(skillId, move)`:
gate `games-skill:<skillId>` -> fresh read -> refuse `games_skill_not_found`
unless the skill is the caller's (a cluster owner reads every owner's skills,
and the mutation stamps the actor as the owner) -> build one `work.LadderEvent`
from the row -> `work.Advance` under the policy read from
`ladderPolicyCurrent` (defaults when unreadable) -> on `Propose` set
`promotionApprovalId = promotionIDFor(templateDigest)` -> write the WHOLE
state with `recordGamesSkillLadder`, `versionAt = versionAfter(read version,
now)`, `state = stateOf(rung, promotionApprovalId)`.

| Call | Event | Notes |
|---|---|---|
| `gamesShadowCompare(goalSignature, plan)` | `EventShadowCompared{Match, Bindings, FreeParameters}` for each of the caller's shadow skills of the signature with the same action sequence | match = the plan binds the template; bindings are each hole's canonical JSON; a match also writes `lastBinding`. |
| `gamesDecideSkillPromotion(skillId, templateDigest, approved)` | `EventPromotionDecided{Approved}` | refuses `games_skill_version_changed` (digest differs) and `games_no_promotion_open`. |
| `gamesReportSkillOutcome(skillId, ok, binding)` | `EventReplayed{Match: ok}` | refuses `games_skill_not_serving` unless canary or trusted; a clean run (`ok: true`) with a binding writes `lastBinding`, and the binding must fill every hole, name no other and hold no null or the call is refused and the run is not counted; with `ok: false` the binding is ignored and the run counts. |
| `gamesSweepSkills()` | `EventSweep{LastUsedAt: max(lastReplayAt, learnedAt)}` | examines every skill of the caller that is not retired (the query's pages are walked to the end, the one written to longest ago first) and writes only those it demotes or retires. |

With the seeded policy this is exactly the S1 ladder: five consecutive
matching comparisons across at least two distinct values of every hole
propose; one approval makes canary; five clean canary runs make trusted; two
consecutive failures on canary or trusted demote to shadow; 30 days unused
retires.

**Lookup** (`lookup.go`). `gamesFindSkill` takes exactly one of
`goalSignature`, `trigger`, `text` and answers one row -- the skill plus
`serve` (`skill | model`), `standby`, `serveReason`, and for text
`similarity` -- or none. The runtime runs a `serve: skill` answer itself with
`Instantiate(template, binding)`; for a trusted skill chosen without a model
the binding is its own choice, and `lastBinding` is the engine's suggestion.

**Not modified:** the coding-app path (`integrations/work`, `component/mcp`,
`integrations/procedure`, `component/work`) -- the pack only calls
`component/work`'s pure functions.

### 4.5 Many replicas

- **State that crosses replicas** is rows only; the Go half holds none.
- **Read-modify-write** (ladder moves, learning): write gate + fresh read +
  version stamped after (D15). Proven by a database test in which two
  concurrent clean runs both count.
- **Events:** the forward rules (D17), proven at three levels: a unit test
  over `node.ForwardsGraphEvent` (the real evaluator), the in-process engine
  tests, and a cluster test (`test/clustere2e/games_pack_test.go`) that writes
  through one connection and observes on ten, through the front door
  (`https://api.memql.localhost`, which spreads connections across both bff
  replicas).

### 4.6 Authorization

- Every concept is the owned tier with the cluster-owner read arm; every
  mutation stamps `ownerUserId: actor.userId`.
- **The runtime acts as the owner** with a user JWT and a rotating refresh
  token (S3; service accounts reach no fleet machines). The site signs in as
  the owner in v1. Both therefore read and write the same rows and receive
  the same live events.
- **Fan-out:** a `graph.node.*` event reaches a stream only if row admission
  admits the row for that stream's actor -- the owner's streams and a cluster
  owner's. Nobody else's subscription sees a games row.
- **Internal origin** is stamped on exactly three rendered calls
  (`store_internal.go`), never on a returned context, and the actor is never
  touched; `call_origin_conformance_test.go` gains an entry for
  `packs/gamespack`, asserted by `TestLiveInternalOriginWritesStayTheCallers`.
- **Honest limit:** the owner's approval is a convention, not a credential
  boundary. The runtime and the site are the same identity, so the engine
  cannot tell a person's click from the runtime calling
  `gamesDecideSkillPromotion`; S3's runtime must never call it. Follow-up:
  refuse promotion decisions from the runtime's OAuth client.
- **Ids are cluster-global** (D20): a client that derives ids without the
  owner's subject can collide with another user's row; on the owner's own
  cluster the owner passes the write guard and would merge into it.

### 4.7 Engine seams, prioritized

**Seam 1 -- DSL-settable lane for `ai()` (milestone).** `builtin ai` gains
`lane enum("interactive", "background")`. `background` stamps
`memql.ContextWithBackgroundLane` (the background rate bucket) and adds
`airoute.TagBackground` to the call's attribution, so the shipped
`backgroundLane` rule routes it unless a pin wins -- the same two things a
background agent turn is. Unknown lanes are refused. `AICallAttribution`
gains `Tags`, applied additively by `applyCallAttribution`.
*Why in the milestone:* the program's compute decision is "decisions on a
fast lane, reflection on a background lane"; reflection is product logic
calling `ai()`, and this seam is the only way to put it on the background
bucket; it is three small changes; and it keeps the decide path's
interactive budget (shared with the owner's own chat on that replica) from
being spent by reflection and summaries.

**Seam 2 -- per-call attribution on `ai()` (follow-up, Task F2).** `builtin
ai` gains `attribution {actorRef, activity}`; `airoute.ResolveRequest`,
`router.CallRecord`, `recordRouterCall` and `v1:router:call` gain `actorRef`
and `activity` (<= 128, clipped not refused, omitted when empty; nothing
routes on them). *Why a follow-up:* the "gets cheaper" proof is counted from
the runtime's own decision telemetry (source per decision), which S3
persists; router-side attribution is a cross-check, not on the milestone's
path.

**Seam 3 -- retention for `v1:router:call` (follow-up, Task F3, first to land
after the milestone).** `@retention(ttl="45d", keepLatest=false,
archive="required")`. 45 days outlasts every reader (the weekly evidence fold
and sharing ledger read one ISO week; a monthly period is at most 31 days);
`archive="required"` archives to the instance's blob container before
deleting and deletes nothing where none is configured (there the hourly
sweep reports this concept as preserved, every hour, and goes on with the
rest; the local and cloud overlays both configure a container). *Why a
follow-up:* a
few days of local play adds tens of thousands of rows, not a disk problem;
it changes every cluster's ledger lifetime and deserves its own review --
but it should land before the multi-day soak.

**Seam 4 -- typed refusal over ExecuteQuery (follow-up, Task F4).** A closed
registry of refusal codes (`<the deferred refusal-code registry call>`, the router's four,
the guard's `llm_guard_blocked`, the machine's `model_concurrency_exceeded`,
and the games pack's `RefusalCodes`); the server lifts the code a message
carries into `QueryError.metadata["refusal"]`; the TS SDK throws
`QueryRefusalError` (`refusal`, `code`, `metadata`, the same message). *Why a
follow-up:* the milestone's refusals already lead with stable codes (D21),
so S3 can match the words now and switch to the field later with no code
change.

Not an S2 seam: the **lane on the fleet wire** (`ModelCallStart.lane`) that
S4's per-lane slots would need -- an additive proto field coordinated with
memql-cockpit, proposed in section 9.

---

## 5. S3 contract

Everything S3 (the product, its runtime adapters and its site) builds on.
Names are stable; removing a concept field or an enum value bricks stored rows
(CLAUDE.md, memql#5199) and is refused by `make concept-snapshot`.

Calls use the internal query form over `MemqlService.Stream` (TS SDK
`QueryClient.executeNamed(name, call)`, Go SDK `QueryClient.ExecuteNamed`),
e.g. `builtin gamesRecall(characterId: "wren", worldId: "meadow", text: "...")`,
`mutation upsertGamesPlayer(...)`, `query gamesPlayers()`. These constructs
are not in the generated core SDK (sdk-gen reads `dsl/`); S3 either wraps
`executeNamed` or points sdk-gen at a root whose `games/` directory is
`packs/gamespack/dsl`.

### 5.1 Concepts

Tier for all: `@rowAuthz(owner="ownerUserId", clusterOwner)`; `ownerUserId`
is stamped, never sent. Retention: E = expires whole after 30 days; V = newest
version kept, older versions expire after 30 days. Live: forwarded
created/updated across replicas.

| Concept | Fields (`!` required) | Retention | Live |
|---|---|---|---|
| `v1:games:player` | `gamertag string! <=32 @pii`, `displayName string! <=64 @pii`, `consented bool!` | V | yes |
| `v1:games:episode` | `characterId string! <=64`, `playerId string` (-> player, `about`), `worldId string! <=64`, `at datetime!`, `kind enum(chat, event, activity, session)!`, `summary string! <=400 @pii`, `importance int! 1..10` | E | yes |
| `v1:games:preference` | `playerId string!` (-> player), `subject string! <=120 @pii`, `valence float! -1..1`, `confidence float! 0..1`, `evidence []string` | V | yes |
| `v1:games:promise` | `characterId string! <=64`, `playerId string!` (-> player, `madeTo`), `text string! <=200 @pii`, `status enum(open, kept, dropped)!`, `madeAt datetime!` | V | yes |
| `v1:games:place` | `worldId string! <=64`, `name string! <=80`, `kind string! <=32`, `position object!`, `discoveredBy string <=64`, `at datetime!` | V | yes |
| `v1:games:skill` | `goalText string! <=200`, `goalSignature string! (64 hex)`, `actions []string!`, `template object!`, `templateDigest string!`, `triggers []string`, `confirmation string <=200`, `learnedFrom []string`, `learnedAt datetime!`, `rung enum(shadow, canary, trusted, retired)!`, `state enum(shadow, awaiting_approval, canary, trusted, retired)!`, `shadowMatches int!`, `canaryMatches int!`, `distinctBindings object`, `failures int!`, `promotionApprovalId string`, `promotionProposedAt datetime`, `lastReplayAt datetime`, `lastBinding object`, `lastReason string`, `rungChangedAt datetime` | V | yes |
| `v1:games:planTrace` | `characterId string! <=64`, `goalText string! <=200`, `goalSignature string!`, `trigger string! <=64`, `inputs object`, `steps []object!`, `actions []string!`, `outcome enum(success, failure, abandoned)!`, `reaction enum(positive, neutral, negative)!`, `at datetime!`, `say string <=200`, `skillId string` (-> skill, `ranSkill`) | E | **no** |

Reads return these fields plus `id`, `ownerUserId` and `createdAt` (the
version's time). Ids come back bare.

### 5.2 Ids

Bare slugs, no colon, at most 64 characters, chosen by the client, **unique per
concept across the cluster** (D20). With `sub` the owner's JWT subject exactly
as issued and `h(x)` = the first 20 hex digits of `sha256(x)`:

| Id | Derivation |
|---|---|
| `playerId` | `"p-" + h(sub + "|" + lower(gamertag))` |
| `preferenceId` | `"pf-" + h(sub + "|" + playerId + "|" + NormalizeStatement(subject))` |
| `placeId` | `"pl-" + h(sub + "|" + worldId + "|" + NormalizeStatement(name))` |
| `episodeId`, `promiseId`, `traceId` | random: a prefix (`e-`, `pr-`, `t-`) and 32 random hex digits |
| `skillId` | engine-derived: `"gs-" + first 24 hex of sha256(canonical owner + "|" + signature + "|" + join(actions, ",") [+ "|" + generation when > 1])` |

`characterId` and `worldId` are the runtime's own stable slugs.

### 5.3 Goal signature

```
NormalizeStatement(s): lower-case; keep Unicode letters and decimal digits; a run of whitespace -> one space; drop everything else; trim
GoalSignature(goal, keys) = hex(sha256(NormalizeStatement(goal) + "|" + join(sort(trimmed non-empty keys), ",")))
```

| goal | keys | signature |
|---|---|---|
| `Light up the area!` | `[spot]` | `d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd` |
| `light up   the area` | `[spot]` | `d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd` |
| `Light up the area` | `[spot, count]` | `8d4463cfb1db4bf8b0f154d2ba3692bb4b2607dc4c9081bc8925aa8766db0b62` |
| `Light up the area` | `[]` | `eb75279beebedb94ae46116ef860a3d2ff48ff45b7806fce07a465d838c2fa2a` |
| `Go to the den.` | `[" ", ""]` | `6fff361fbd783f6a11ef41e5bcf9d79e5259623e70a430eb79ba06034eea11b5` |

### 5.4 Writes

Mutations (plain owner tier; an insert on an existing id writes a new version):

| Call | Args |
|---|---|
| `mutation upsertGamesPlayer` | `playerId!`, `gamertag!`, `displayName!`, `consented!` |
| `mutation createGamesEpisode` | `episodeId!`, `characterId!`, `playerId`, `worldId!`, `at!`, `kind!`, `summary!`, `importance!` -- prefer `gamesRecordEpisodes` |
| `mutation upsertGamesPreference` | `preferenceId!`, `playerId!`, `subject!`, `valence!`, `confidence!`, `evidence` |
| `mutation upsertGamesPromise` | `promiseId!`, `characterId!`, `playerId!`, `text!`, `status!`, `madeAt!` |
| `mutation setGamesPromiseStatus` | `promiseId!`, `status!` |
| `mutation upsertGamesPlace` | `placeId!`, `worldId!`, `name!`, `kind!`, `position!`, `discoveredBy`, `at!` |

`createGamesPlanTrace`, `createGamesSkill` and `recordGamesSkillLadder` are
`@serverOnly`: a client call is refused.

Builtins (each answers one row):

| Call | Args | Answer |
|---|---|---|
| `builtin gamesRecordEpisodes` | `episodes []object!` (<= 50 of the createGamesEpisode fields) | `{written, embedded, pending, ids}` |
| `builtin gamesEmbedPending` | `limit` (default 25, <= 200) | `{embedded, failed, more}` |
| `builtin gamesForgetPlayer` | `playerId!` | `{playerId, deleted: {episodes, preferences, promises, places, planTraces, skills, players, vectors}}` -- `episodes`, `preferences` and `promises` count the rows about the player and the rows that name the gamertag together, each once; `places` the places that name it (4.3) |
| `builtin gamesRecordTrace` | `traceId!`, `characterId!`, `goalText!`, `goalSignature`, `trigger!`, `inputs`, `steps []object!` (`{action, args, ok, durationMs, error}`), `outcome!`, `reaction` (default neutral), `at!`, `say`, `skillId` | `{traceId, goalSignature, learnedSkillId}` (`""` when nothing was learned) |
| `builtin gamesShadowCompare` | `goalSignature!`, `plan []object!` (`{action, args}`) | `{compared, matched, proposed: [skillId]}` |
| `builtin gamesReportSkillOutcome` | `skillId!`, `ok!`, `binding` | `{skillId, rung, state, reason}` |
| `builtin gamesDecideSkillPromotion` | `skillId!`, `templateDigest!`, `approved!` | `{skillId, rung, state, reason}` |
| `builtin gamesSweepSkills` | -- | `{examined, demoted, retired}` |

The arguments of the ladder builtins are checked before a gate is taken (all
but the fit of a binding to a skill's template, which needs the skill and is
judged inside its gate), and a bad one is `games_invalid_argument` naming it:

- `skillId` is a bare id (or `v1:games:skill:<id>`) of at most 64 characters,
  with no colon in the id and no NUL character.
- `goalSignature` is 64 lower-case hexadecimal digits, as `GoalSignature`
  computes it, and `plan` has at least one step.
- `gamesReportSkillOutcome`'s `binding` is an object (or absent). It is read
  only on a clean run (`ok: true`): then it must fill every hole of the skill's
  template, name no other hole and hold no null, or the call is refused and the
  run is **not counted**. With `ok: false` a binding is ignored and the run
  counts as the failure it is.

A skill that is not the caller's, or not there, is `games_skill_not_found`
whichever it is, and a comparison or a sweep passes over a skill deleted since
it was listed.

### 5.5 Reads

| Call | Args | Order, page |
|---|---|---|
| `query gamesPlayers` | -- | displayName asc, 100 |
| `query gamesPlayerById` | `playerId!` | one |
| `query gamesPreferencesForPlayer` | `playerId!` | confidence desc, 50 |
| `query gamesOpenPromises` | `characterId!`, `playerId!` | madeAt desc, 20 |
| `query gamesPromisesForPlayer` | `playerId!` | madeAt desc, 50 |
| `query gamesPlacesInWorld` | `worldId!` | at desc, 50 |
| `query gamesSessionEpisodes` | `characterId!`, `since!` | at desc, 40 |
| `query gamesEpisodesForPlayer` | `playerId!` | at desc, 50 |
| `query gamesSkills` | -- | rungChangedAt desc, 100 |
| `query gamesSkillsAwaitingApproval` | -- | promotionProposedAt desc, 50 |
| `query gamesSkillById` | `skillId!` | one |
| `query gamesSkillsForSignature` | `goalSignature!` | learnedAt desc, 20 |
| `query gamesTrustedSkills` | -- | lastReplayAt desc, 100 |
| `query gamesSkillsToSweep` | -- | createdAt asc (the one written to longest ago first: only roughly the least recently used, since a decision, a demotion and learning write versions too), 200 a page, walked page by page (not retired) |
| `query gamesLearningTraces` | `goalSignature!` | at desc, 20 (success, not negative) |

**Every query that pages names the caller's owner in its filter**
(`row.ownerUserId == actor.userId`, which needs `@actor`), as well as leaving
it to the tier: the tier's read arm for a cluster owner returns every owner's
rows, and a page of them is cut at its limit, so without the conjunct a cluster
owner's own rows fell off a page shared with everyone's (a cluster owner's
comparison with twenty others' skills of one goal never found its own). The two
queries that answer one row by id (`gamesPlayerById`, `gamesSkillById`) do not
page and do not carry it: they read whoever's the row is, and their callers
compare the owner themselves (`moveLadder`, `gamesForgetPlayer`). A test
(`owner_scope_db_test.go`) pins every paged query to the conjunct.

| Builtin | Args | Answer |
|---|---|---|
| `builtin gamesRecall` | `characterId!`, `playerId`, `worldId!`, `text!`, `k`, `asOf`, `halfLifeHours`, `wRelevance`, `wRecency`, `wImportance`, `preferences`, `promises`, `places` | rows `{rank, kind: episode|preference|promise|place, id, text, score, relevance, recency, importance, relevanceAvailable, row}`; `score` is the formula for episodes, `confidence` for preferences, 0 otherwise |
| `builtin gamesFindSkill` | exactly one of `goalSignature`, `trigger`, `text`; `minSimilarity` (default 0.82) | zero or one row: the skill's fields plus `serve: skill|model`, `standby`, `serveReason`, `similarity` (text only). A `text` lookup fails while the text cannot be embedded; treat that as no match |

### 5.6 Skill template and state

```
template = {"steps": [{"action": "<name>", "args": {"<key>": {"const": <json>} | {"hole": "<stepIndex>.<key>"}}}], "holes": ["<stepIndex>.<key>", ...]}
binding  = {"<hole>": <json>}           Instantiate fills every hole or refuses
templateDigest = hex(sha256(canonical JSON of template))
state = awaiting_approval when rung = shadow and promotionApprovalId != "", else rung
```

S1's `Template` is this JSON; S1's `SkillStats` maps `shadowMatches`,
`canaryClean <- canaryMatches`, `failures`, `lastUsedAt <- lastReplayAt`,
`bindings <- distinctBindings` (per hole: digests).

### 5.7 Subscriptions

`subscribeGraph({concept, actions: ["created", "updated"]})` for
`v1:games:player | episode | preference | promise | place | skill` delivers
across replicas to the owner's streams. Do not subscribe to
`v1:games:planTrace` (not forwarded). Deletes publish nothing.

### 5.8 Refusal codes

`games_unauthenticated`, `games_invalid_argument`, `games_signature_mismatch`,
`games_skill_not_found`, `games_skill_not_serving`,
`games_skill_version_changed`, `games_no_promotion_open`,
`games_player_not_found`. A client also meets the engine's: `every_door_shut`,
`no_local_model_available`, `ceiling_reached`, `no_app_available` (no model
reachable), `blocked by the LLM guard` (rate or loop guard),
`model_concurrency_exceeded` (the machine is busy), `function "<name>" not
found` (the pack is disabled, or a node has not restarted since it was
enabled) and `unknown builtin executor` (the DSL loaded without the pack's Go
half). Match the code word, not the sentence.

### 5.9 S1 interfaces onto S2

| S1 | S2 |
|---|---|
| `MemoryStore.recordEpisode(e)` (batched) | `gamesRecordEpisodes` (`palId -> characterId`, `at` as RFC 3339) |
| `recall(q)` | `gamesRecall(characterId: q.palId, playerId, worldId, text, asOf: q.now, k)` |
| `upsertPreference(p)` / `upsertPromise(p)` / `upsertPlace(p)` | `upsertGamesPreference` / `upsertGamesPromise` (+ `setGamesPromiseStatus`) / `upsertGamesPlace` |
| `forgetPlayer(id)` | `gamesForgetPlayer` |
| `openPromises(pal, player)` / `topPreferences(player, n)` / `sessionEpisodes(pal, since, max)` | `gamesOpenPromises` / `gamesPreferencesForPlayer` then first n / `gamesSessionEpisodes` (<= 40) then first max |
| `prune(now)` | no-op returning 0: retention is the engine's |
| `SkillStore.findSkill(q)` | `gamesFindSkill` |
| `recordTrace(t)` | `gamesRecordTrace` (`kidReaction -> reaction`) |
| `shadowCompare(sig, plan)` | `gamesShadowCompare` |
| `reportOutcome(id, o)` | `gamesReportSkillOutcome(skillId, ok: o.ok, binding)` |
| `candidates()` / `list()` | `gamesSkillsAwaitingApproval` / `gamesSkills` |
| `approve(id)` | `gamesDecideSkillPromotion(skillId, templateDigest: <the skill's>, approved: true)` -- the site only, never the runtime |

Differences from S1's local stores, by decision: per-hole distinct bindings
(D12); negative-reaction and skill-run traces never teach (D11); thresholds
come from the cluster's ladder policy (defaults equal S1's); learning happens
on `gamesRecordTrace`.

### 5.10 What S3 still owns

The `Brain` (product prompts pinned to the fleet, called through product
logic with `builtin ai(..., lane: "background")` for reflection), the
decision-metrics concept behind the efficiency chart (a product concept has
no Go routing rule, so a live view of it needs the product bff at one replica,
or the attribution follow-up for an engine-side count), OAuth clients and the
runtime's device-code session, the site, and deployment.

---

## 6. Testing and proving

| Level | What | Where |
|---|---|---|
| Unit (no database) | signature vectors; generalize/bind/instantiate/digest; owner-scoped generations; `recallScore`; statement variants; ladder state mapping; per-hole promotion through `work.Advance`; `DecideServe` mapping; `versionAfter`; canonical ids; forward rules; pack ships disabled; concepts and retention load; capabilities match the declared builtins; refusal codes declared and raised | `packs/gamespack/*_test.go` |
| Database (`MEMQL_REQUIRE_DB=1`, a real Postgres + TimescaleDB + pgvector; `packs` is already in `scripts/ci/db-gated-packages.sh`) | a real engine and the pack's real DSL and Go half, one stand-in (a bag-of-words embedder in its own vector table, and a failing one for a model that is away): episodes and embedding, and episodes recorded while the model is away embedded later; recall ranks, scopes, agrees with the formula, degrades with no embedder and with the model away, never crosses owners (with a positive control); forget (all of it or none, the rows that name the gamertag and the decoys it must not take, and the fence: a ladder move completes while a forget is uncommitted and the skill is still gone after it commits); every query that pages answers the caller's rows only; learn; signature refusal; `@serverOnly` refusal; internal-origin rows stay the caller's; two owners learn one goal apart; the whole ladder and its refusals; concurrent outcomes both count; retire and relearn; lookups by signature, trigger and meaning; a disabled pack is inert | `packs/gamespack/*_db_test.go` |
| Cluster (`-tags clustere2e`, `MEMQL_E2E_TOKEN`) | rows written through one connection reach ten connections across both bff replicas; recall and learning through different connections | `test/clustere2e/games_pack_test.go` |
| Gates | `memqllint dsl/` (anchors the pack), concept snapshot, embed inventory, call-origin allowlist, staged-read classification, architecture model, `make test` | existing |

**No proving-suite world type in this milestone.** The proving suite measures
claims published on the scorecard (`docs/public/overview/proving-scorecard.md`);
S2's claim -- fewer model calls per activity as skills climb -- is measured by
the product's telemetry in the S1 soak. A scorecard figure
(`skillsServedWithoutModel` with a shadow-only control) is a follow-up for
when the claim is published.

All of the above was run during planning, replayed task by task on a
throwaway worktree of the base commit: every task's tests pass at its own
commit (database tests against
`ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c...` with
`MEMQL_REQUIRE_DB=1`), and at the end `make test`, the database-gated
`packs`, `component/grpc` and `app` trees, `memqllint dsl/`, the snapshot,
model, platform-graph and documentation checks, the cluster test's build and
the TypeScript SDK suite are green, follow-ups included.

---

## 7. Local deployment

The plan's Task 14 is the runbook. In short: after checking that the
cluster's running build is older than the branch (migrations only run
forward), the pack changes no manifest, so
`memql-local` keeps tracking the manifests (pinned to the branch's merge base
for the duration) while `make dev` from the worktree builds and imports the
seven `memql-<type>:local` images; an owner PAT minted in the identity pod
drives the scripted enable and the cluster test through the front door; every
node restarts after the flip. Switching back rebuilds the images from
`origin/main` in a throwaway worktree, restores `targetRevision: main`,
optionally disables the pack, and revokes the PAT. Rows of `v1:games:*`
concepts left in the database are inert on images without the pack.

---

## 8. Risks and honest limits

- **Interactive rate ceiling.** Every fleet call, embeddings included, counts
  against the replica's interactive bucket (20 / 10 s) unless on the
  background lane. Decide calls plus recall's query embedding stay
  interactive; memory and skill embeddings and reflection go background.
  Two pals at peak are well inside it; S4 or an operator raises
  `MEMQL_LLM_MAX_CALLS_PER_WINDOW` if a soak says otherwise.
- **Owner's approval by convention** (4.6).
- **Orphaned vectors after retention.** The generic sweep deletes rows, not
  their `node_vectors*` entries; recall never surfaces them (the join is from
  live rows), but they hold space. Follow-up: retention purges vectors of the
  ids it deletes.
- **A constant learned from the first traces stays constant.** Learning is
  once per (signature, action sequence, generation); a template is never
  re-generalized, so a value both first traces shared is fixed in the skill.
- **A skill that keeps a player's name is deleted whole.** `gamesForgetPlayer`
  does not rewrite a skill (4.3): one whose words name the player goes in every
  version, and what it had learned goes with it. A skill learned from the
  player's traces that keeps none of their words stays.
- **A write about the player that commits at the same moment as the forget can
  survive it.** The engine has no gate per player, and of the skills only those
  that name the player when forget lists them are fenced (4.3). A skill that
  comes to name the player after that -- through a comparison in flight
  (`gamesShadowCompare`) or a run report in flight (`gamesReportSkillOutcome`),
  whose plan or run arguments carry the gamertag and are written as the skill's
  `lastBinding` -- is the same class as the rest: an episode inserted after the
  statement that deletes the player's episodes ran, a skill learned from traces
  read before the forget deleted them (and created after it listed the skills
  it holds), and a read-merge update of a promise (`setGamesPromiseStatus`
  reads the row before the delete and writes the merged row after it) each
  commit a version the forget never saw. **The product runtime marks the player
  and drains its own queue of writes about them before it asks the engine**, so
  that the forget is the last write about that player; a caller that does not
  has not been promised more.
- **One forget fences as many skills as the server's lock table holds** (a few
  thousand) and waits at most six seconds for all of their gates together; past
  either it fails whole, deleting nothing, and is sent again.
- **Consent is the runtime's to enforce.** The engine stores whatever the
  owner's runtime writes; an engine-side consent gate is phase 2 (COPPA).
- **Signature drift** between runtime and engine is refused at the first
  trace, by design.
- **A lookup by meaning needs the model.** `gamesFindSkill(text: ...)` fails
  while the text cannot be embedded; the runtime treats that as no match and
  falls back to its own model path (which is then likely away too).
- **The S1 spec says the signature is "the same construction as memQL's
  learned procedures".** It is not (D10); S1's own formula is the contract.
- **Version history before retention.** Each ladder move and each upsert is a
  version; `keepLatest=true` with 30 days bounds the history.

---

## 9. Follow-ups

Planned in full in the plan (tasks F2-F4), separately reviewable:

- **F2** per-call attribution on `ai()` (`actorRef`, `activity` on
  `v1:router:call`).
- **F3** `@retention(ttl="45d", keepLatest=false, archive="required")` on
  `v1:router:call` -- land before the multi-day soak.
- **F4** typed refusal over ExecuteQuery (`metadata.refusal`,
  `QueryRefusalError`), registering the games pack's codes.

Recorded, not planned:

- `ModelCallStart.lane` on the worker wire, for S4's per-lane machine slots
  (additive; coordinated with memql-cockpit).
- Refuse `gamesDecideSkillPromotion` from the runtime's OAuth client.
- Retention purges vectors of deleted ids (platform-wide).
- An engine-side consent gate for episodes about unconsented players.
- A `character` concept, if the live graph wants character hubs.
- Re-generalizing a skill whose shadow comparisons keep differing in one
  constant (a new version, ladder reset).
- A proving-suite figure for skills served without a model.
- A games-pack `decision` concept with a routing rule, if S3 wants its
  efficiency chart live across product bff replicas.
- A `v1:work:approval` kind for game promotions, if Nexus > Approvals should
  show them.

---

## 10. Out of scope

The runtime (S1), the product DSL, prompts, site and deployables (S3), fleet
tuning (S4), parent registration and consent, production hosting, and any
change to the coding-app recording and replay path.
