# Games Pack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `packs/gamespack`, a default-build pack (shipped disabled) that gives a game runtime outside the cluster a memory of its players and skills learned from its executed plans, plus the DSL-settable `ai()` lane, proven by unit, database and cluster tests and runnable on the owner's local k3d cluster.

**Architecture:** The pack is a DSL tree (seven owner-tier concepts with declared retention, shapes, queries, mutations, builtins) and a Go half registered for the pack's domain. Every builtin runs under the caller's actor and writes through the pack's own mutations; three writers are `@serverOnly` and stamped with internal origin on one rendered call each. Recall is one SQL statement scored relevance x recency x importance over a LEFT JOIN to the cluster's embedder table; the skill ladder is `component/work.Advance` applied inside a cross-replica write gate with a fresh read and a version stamped after it.

**Tech Stack:** Go 1.26, MemQL DSL (language 1.0, edition 2026), PostgreSQL 16 + TimescaleDB + pgvector, `component/work` (ladder), `component/node` (routing rules), TypeScript SDK (follow-up F4 only).

**Spec:** [`docs/superpowers/specs/2026-10-18-games-pack-design.md`](../specs/2026-10-18-games-pack-design.md). The plan argues from it; read both. Section 5 of the spec (the S3 contract) is the external interface every task must keep exactly.

## Global Constraints

- Work only in the worktree `/home/znas/memql-projects/memql-games` (branch `feat/games-pack`, base `origin/main` `f0af5373d`); never touch the main checkout.
- Run `make identity-tailwind` once in the worktree before the first Go test: it generates the ignored identity CSS, fonts and favicon that `TestEmbeddedFileCountsAreStable` counts (`component/identity/web` embeds 22 files, 16 without them).
- Three extension words only: component, integration, pack. Never write "plugin" in prose; the engine's own identifiers (`RegisterPluginForContract`, `PluginContext`) stay as they are.
- No game, product or character name in Go, DSL, tests or public docs. The design record is the only file that names the first consumer.
- No emojis in code, comments, docs or commit messages.
- Never `go test ./...`. Name packages by module path (`go test github.com/znasllc-io/memql/packs/gamespack/`); `make test` runs the whole tree.
- Database-gated tests run with `MEMQL_REQUIRE_DB=1` against the CI image on port 55432 (port 5432 on a machine running k3d is the cluster's load balancer, which accepts and then EOFs, so every database test would skip):
  `docker run -d --name games-db -e POSTGRES_USER=memql -e POSTGRES_PASSWORD=memql_dev -e POSTGRES_DB=memql -p 127.0.0.1:55432:5432 ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c9cc49bdbf1e935234e8c87ab64b6d6d195fd2ecedff7b00185557631`
  and `export GAMES_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql?sslmode=disable'`.
- Stage files by explicit path; never `git add -A`, `git add .` or a bare `git stash`.
- Commit with `git -c user.name=znas -c user.email=dev@example.com commit`; every message ends with the line `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Do not push before Task 13 is green.
- Pre-release rule: a renamed Go symbol changes every caller in the same commit; no shims, no aliases.
- Generated artifacts are regenerated, never hand-edited: `make concept-snapshot`, `make arch-model`. `embed_inventory_test.go` counts are measured, never derived.
- Do not modify the coding-app recording and replay path (`integrations/work`, `component/mcp`, `integrations/procedure`, `component/work`); the pack only calls `component/work`'s exported pure functions.
- Every refusal the pack raises leads with a stable code from `RefusalCodes` (`games_*`), written `"games_<what>: <sentence>"`.
- Row ids are bare and at most 64 characters. A hand-written statement compares owners as `ownerKey(owner)` (canonical), never the bare actor id.
- A read-modify-write of one row takes the write gate, reads with `memql.ContextWithFreshRead`, and stamps `versionAfter(read, now)`.
- Every hand-written `FROM "MemoryNodes"` statement carries a `staged-data:` comment (`GATE`, `MUST-NOT-GATE` or `INDIFFERENT`).

## Review Focus

- The model, or the whole fleet, is away while a runtime records memories and recalls them: episodes must still be written, recall must still rank by recency and importance and say `relevanceAvailable: false`, and the backlog must embed later. Pinned by Task 3 (`TestLiveEpisodesRecordedWhileTheModelIsAwayAreEmbeddedLater`) and Task 4 (`TestLiveRecallWhileTheModelIsAwayStillAnswers`).
- Two characters report runs of one skill at the same moment through two replicas: both runs must count. Pinned by Task 8 (`TestLiveConcurrentOutcomesBothCount`).
- A hand-written statement compares the bare actor id with the canonical stored `ownerUserId`: it matches nothing and says nothing. The owner must see their rows and nobody else may. Pinned by Task 4 (`TestLiveEpisodesAreTheOwnersAlone`, with its positive control).
- Two owners on one cluster teach the same goal: two skills, each its owner's, never one merged row. Pinned by Task 6 (`TestSkillIDIsDeterministicPerOwnerSignatureActionsAndGeneration`) and Task 7 (`TestLiveTwoOwnersLearnTheSameGoalApart`).
- A goal comes back after its skill retired: it must be learned again, as a new generation, in shadow. Pinned by Task 8 (`TestLiveRetiredSkillIsRelearnedAsANewGeneration`).

---

## File Structure

New package `packs/gamespack` (one responsibility per file):

| File | Responsibility | Task |
|---|---|---|
| `dsl/memql.toml`, `dsl/namespace.pin` | language line; namespace `games` | 1 |
| `dsl/concepts.memql` | the seven concepts, tiers, retention | 1 |
| `dsl/shapes.memql` | one `games<Concept>Full` shape per concept | 1 |
| `dsl/queries.memql` | the fifteen reads | 1 |
| `dsl/mutations.memql` | six client mutations, three `@serverOnly` writers | 1 |
| `dsl/builtins.memql` | the eleven builtins (grows per task) | 3, 4, 5, 7, 8, 9 |
| `pack.go` | `Domain`, concept ids, `Tree`, `Provider`, `NewProvider`, `Register` | 1, 2, 3 |
| `routing.go` | live-graph forward rules | 2 |
| `values.go` | actor and owner, id spelling, narrowing, `versionAfter`, `answer` | 3 |
| `store.go` | rows through the engine under the caller's actor | 3, 7, 8 |
| `space.go` | the cluster's embedder space | 3 |
| `capabilities.go` | the Go half's capability list (grows per task) | 3, 4, 5, 7, 8, 9 |
| `episodes.go` | `gamesRecordEpisodes`, `gamesEmbedPending` | 3 |
| `recall.go` | `gamesRecall` | 4 |
| `forget.go` | `gamesForgetPlayer` | 5 |
| `signature.go`, `template.go` | `GoalSignature`; `Template`, `Generalize`, `Bind`, `Instantiate`, `SkillID` | 6 |
| `gate.go` | the cross-replica write gate | 7 |
| `store_internal.go` | the three internal-origin writers | 7, 8 |
| `skills.go` | `gamesRecordTrace` and learning | 7 |
| `ladder.go`, `ladder_calls.go` | rows to `work.LadderState`, `moveLadder`; the four ladder builtins | 8 |
| `refusals.go` | `RefusalCodes` | 8 |
| `lookup.go` | `gamesFindSkill` | 9 |

Tests beside them: `pack_test.go`, `routing_test.go`, `capabilities_test.go`, `values_test.go`, `recall_test.go`, `signature_test.go`, `template_test.go`, `ladder_test.go`, `refusals_test.go`, `lookup_test.go` (no database); `main_dbschema_test.go`, `harness_db_test.go`, `pack_db_test.go`, `episodes_db_test.go`, `recall_db_test.go`, `forget_db_test.go`, `learn_db_test.go`, `ladder_db_test.go`, `lookup_db_test.go` (database-gated, self-skipping without one).

Modified outside the pack:

| File | Change | Task |
|---|---|---|
| `packs/anchor/anchor.go`, `anchor_test.go` | `Storefront()` becomes `Packs()`; `StorefrontDomains()` added | 1 |
| `cmd/memql-lsp/server.go`, `cmd/memqllint/main.go`, `component/packages/analyze.go`, `test/conformance/corpus_test.go` | call `anchor.Packs()` | 1 |
| `app/anchor_storefront_packs.go` to `app/anchor_packs.go`, `app/database.go`, two boot-order tests | `anchorPacks()` | 1 |
| `embed_inventory_test.go` | measured count for the pack (6, then 7) | 1, 3 |
| `component/conceptfields/concept-fields.snapshot.json`, `component/architecture/embedded/topology.model.json` | regenerated | 1, 13 |
| `call_origin_conformance_test.go` | allowlist entry `packs/gamespack` | 7 |
| `component/memql/ai_guard.go`, `ai_attribution.go`, `integrations/agents/ai.go`, `dsl/agents/builtins.memql`, docs | the `ai()` lane | 10 |
| `docs/public/build/consuming-the-games-pack.md`, `GLOSSARY.md` | consumer page | 11 |
| `test/clustere2e/games_pack_test.go` | cluster test | 12 |

---
### Task 1: The pack's data model, anchored and disabled

The DSL tree (everything but builtins), the smallest `pack.go` that registers it, and the anchor rename that links it into every binary. A reviewer judges the data model here: tiers, retention, field bounds, the reads and the writes.

**Files:**
- Create: `packs/gamespack/dsl/memql.toml`, `packs/gamespack/dsl/namespace.pin`, `packs/gamespack/dsl/concepts.memql`, `packs/gamespack/dsl/shapes.memql`, `packs/gamespack/dsl/queries.memql`, `packs/gamespack/dsl/mutations.memql`, `packs/gamespack/pack.go`, `packs/gamespack/pack_test.go`
- Modify: `packs/anchor/anchor.go:1-54`, `packs/anchor/anchor_test.go:27-33`, `cmd/memql-lsp/server.go:150`, `cmd/memqllint/main.go:229`, `component/packages/analyze.go:214`, `test/conformance/corpus_test.go:1435`, `app/database.go:44`, `app/storefront_boot_order_test.go:84,91`, `app/shopper_auth_boot_order_test.go:52`, `embed_inventory_test.go:116`, and the four live references to the old file name: `CLAUDE.md:111-114`, `component/packages/storefront_pack_analysis_test.go:14`, `docs/public/build/building-a-pack.md:230`, `packs/wholesalepack/live_e2e_test.go:105`
- Rename: `app/anchor_storefront_packs.go` to `app/anchor_packs.go` (and edit lines 9-11, 46, 59-64)
- Regenerate: `component/conceptfields/concept-fields.snapshot.json`, `component/architecture/embedded/topology.model.json`

**Interfaces:**
- Consumes: `dsl.RegisterTree(domain string, fs.FS)`, `dsl.RegisterPackDefault(domain string, enabled bool)`, `dsl.PackDefaults() map[string]bool`, `dsl.IsStorefrontPack(domain string) bool`, `memql.LoadUnifiedConcepts(nil)`.
- Produces: `gamespack.Domain = "games"`, `gamespack.DefaultEnabled = false`, `gamespack.Tree() fs.FS`, `gamespack.Register(domain string)`; unexported concept ids `conceptUser`, `conceptPlayer`, `conceptEpisode`, `conceptPreference`, `conceptPromise`, `conceptPlace`, `conceptSkill`, `conceptPlanTrace`; test helper `var registerOnce sync.Once` (pack_test.go). `anchor.Packs()`, `anchor.Domains() []string` (all three packs), `anchor.StorefrontDomains() []string` (reviews, wholesale). `(*app.App).anchorPacks()`. DSL: the concepts, shapes, queries and mutations of spec section 5.1, 5.4 and 5.5.

- [ ] **Step 1: Write the failing test**

Create `packs/gamespack/pack_test.go`:

```go
package gamespack

import (
	"sync"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// registerOnce registers the pack the way packs/anchor does, once per process:
// dsl.RegisterTree panics on a second registration of one namespace.
var registerOnce sync.Once

// The pack holds memories about the people who play; holding them is an
// owner's decision, so it ships disabled.
func TestPackShipsDisabled(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	if enabled, declared := memqldsl.PackDefaults()[Domain]; !declared || enabled {
		t.Fatalf("the games pack must declare itself disabled, got declared=%v enabled=%v", declared, enabled)
	}
	if memqldsl.IsStorefrontPack(Domain) {
		t.Fatal("the games pack is not a storefront pack: only an owner may switch it on")
	}
}

// Every concept loads, with the retention the design record states: raw
// evidence expires whole after 30 days, everything else keeps its newest
// version, and nothing is archived.
func TestConceptsLoadWithTheirRetention(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	want := map[string]bool{ // concept -> keepLatest
		conceptPlayer: true, conceptEpisode: false, conceptPreference: true, conceptPromise: true,
		conceptPlace: true, conceptSkill: true, conceptPlanTrace: false,
	}
	for id, keepLatest := range want {
		c, err := memorynodes.DefaultRegistry().Get(id)
		if err != nil {
			t.Fatalf("%s is not registered: %v", id, err)
		}
		r := c.Retention
		if r == nil || r.TTL != 30*24*time.Hour || r.KeepLatest != keepLatest || r.Archive != "none" {
			t.Errorf("%s retention = %+v, want ttl 720h, keepLatest %v, archive none", id, r, keepLatest)
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL, the package does not build:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/pack_test.go:20:27: undefined: Register
packs/gamespack/pack_test.go:20:36: undefined: Domain
packs/gamespack/pack_test.go:21:50: undefined: Domain
packs/gamespack/pack_test.go:24:31: undefined: Domain
packs/gamespack/pack_test.go:33:27: undefined: Register
packs/gamespack/pack_test.go:33:36: undefined: Domain
packs/gamespack/pack_test.go:38:3: undefined: conceptPlayer
packs/gamespack/pack_test.go:38:24: undefined: conceptEpisode
packs/gamespack/pack_test.go:38:47: undefined: conceptPreference
packs/gamespack/pack_test.go:38:72: undefined: conceptPromise
packs/gamespack/pack_test.go:38:72: too many errors
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write the DSL tree**

Create `packs/gamespack/dsl/memql.toml`:

```toml
# The language this directory's .memql files are written in.
# See docs/public/language/memql.md, "The language line".
memql = "1.0"
edition = "2026"
```

Create `packs/gamespack/dsl/namespace.pin` (one line, the namespace):

```text
games
```

Create `packs/gamespack/dsl/concepts.memql`. Every concept is `@rowAuthz(owner="ownerUserId", clusterOwner)`: the owned tier, plus the cluster-owner arm that also lets the hourly retention sweep's maintenance principal admit the rows. `ownerUserId` is a `parent` relationship to the user, so the engine stores it canonical:

```memql
// games pack -- memory and skill learning for an EXTERNAL game runtime
// (design record docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// GAME-AGNOSTIC. Nothing here names a game, a product or a character. A
// runtime outside the cluster plays the game in real time; this pack is what
// it remembers with and what it learns with. The runtime executes every
// action itself: the engine stores, scores, learns and governs the ladder.
//
// EVERY ROW IS THE OWNER'S. The runtime and the owner's browser both act as
// the owner (a person's JWT), so every concept is the owned tier with the
// cluster-owner read arm, and every mutation stamps ownerUserId from the
// actor. A character (the AI companion the runtime animates) is a plain
// characterId string chosen by the runtime, not a concept.
//
// RETENTION IS DECLARED, NOT CODED. Episodes and plan traces are raw
// evidence and expire whole after 30 days (keepLatest=false). Everything
// else keeps its newest version forever and sheds older versions after 30
// days (keepLatest=true). archive="none" everywhere: these rows describe
// the people who play, and an archive copy would outlive the deletion it
// exists to perform.

use identity.concepts.{ user }

/// A person who plays alongside the owner's characters. The owner registers each player; the
/// runtime remembers only players whose `consented` is true.
@displayCard(primary="displayName", secondary="gamertag", status="consented")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=true, archive="none")
concept player {
  ownerUserId  string!  @description("v1:identity:user.id of the owner who registered this player. Stamped from actor.userId.")
  gamertag     string!  @pii  @maxLength(32)  @description("The name the game reports for this player.")
  displayName  string!  @pii  @maxLength(64)  @description("What the characters call this player.")
  consented    bool!    @description("Whether the owner allows the characters to remember this player. A runtime writes memories only about a consented player.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}

/// One remembered moment: something a character saw, said or did, in at most a sentence. The
/// summary is embedded when the episode is recorded through gamesRecordEpisodes, so gamesRecall can
/// rank episodes by relevance, recency and importance.
@displayCard(primary="summary", secondary="kind", tertiary="characterId")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=false, archive="none")
concept episode {
  ownerUserId  string!    @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  characterId  string!    @maxLength(64)  @description("The character that remembers this, as the runtime names it.")
  playerId     string     @description("v1:games:player.id this episode is about, when it is about one.")
  worldId      string!    @maxLength(64)  @description("The game world it happened in, as the runtime names it.")
  at           datetime!  @description("When it happened in the game, which recall's recency measures from.")
  kind         enum("chat", "event", "activity", "session")!  @description("chat: an exchange with a player. event: something that happened. activity: the outcome of a plan. session: a summary of a whole play session.")
  summary      string!    @pii  @maxLength(400)  @description("The memory itself, at most about 25 words. The embedding source.")
  importance   int!       @minimum(1)  @maximum(10)  @description("How much this matters, 1 to 10. Recall weighs it at a tenth of its value.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
  @relationship(type="references", as="about", field="playerId", target=player, direction="outgoing")
}

/// What a player likes or dislikes, as reflection inferred it. One row per (player, subject),
/// rewritten as evidence accumulates.
@displayCard(primary="subject", secondary="valence", tertiary="playerId")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=true, archive="none")
concept preference {
  ownerUserId  string!   @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  playerId     string!   @description("v1:games:player.id this preference belongs to.")
  subject      string!   @pii  @maxLength(120)  @description("What the preference is about, in a few words.")
  valence      float!    @minimum(-1)  @maximum(1)  @description("-1 dislikes strongly, 0 neutral, 1 likes strongly.")
  confidence   float!    @minimum(0)  @maximum(1)  @description("How sure reflection is, 0 to 1.")
  evidence     []string  @description("Bare ids of the episodes this was inferred from. Episodes expire after 30 days; the ids may then resolve to nothing.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
  @relationship(type="references", as="about", field="playerId", target=player, direction="outgoing")
}

/// Something a character said it would do for a player, and whether it did.
@displayCard(primary="text", secondary="status", tertiary="characterId")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=true, archive="none")
concept promise {
  ownerUserId  string!    @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  characterId  string!    @maxLength(64)  @description("The character that made the promise.")
  playerId     string!    @description("v1:games:player.id the promise was made to.")
  text         string!    @pii  @maxLength(200)  @description("The promise, in a sentence.")
  status       enum("open", "kept", "dropped")!  @description("open until the character keeps it or lets it go.")
  madeAt       datetime!  @description("When the promise was made, in the game.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
  @relationship(type="references", as="madeTo", field="playerId", target=player, direction="outgoing")
}

/// A named spot in a game world, shared by every character in that world.
@displayCard(primary="name", secondary="kind", tertiary="worldId")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=true, archive="none")
concept place {
  ownerUserId   string!    @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  worldId       string!    @maxLength(64)  @description("The game world, as the runtime names it.")
  name          string!    @maxLength(80)  @description("What the characters call this place.")
  kind          string!    @maxLength(32)  @description("The runtime's own word for what the place is (for example base, farm, landmark, danger). Free text, so a new kind is not an engine change.")
  position      object!    @description("Where it is, in the runtime's coordinates (for example {x, y, z}).")
  discoveredBy  string     @maxLength(64)  @description("The character that found it.")
  at            datetime!  @description("When it was found or last confirmed, in the game.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}

/// A procedure learned from a runtime's own successful plans: a sequence of the runtime's
/// actions with holes for what varies. It climbs the same ladder as a learned procedure
/// (component/work.Advance under v1:authoring:ladderPolicy:primary): shadow, then one owner
/// approval, then canary, then trusted. Only the pack's Go half writes it.
@displayCard(primary="goalText", secondary="state", tertiary="goalSignature")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=true, archive="none")
concept skill {
  ownerUserId          string!    @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  goalText             string!    @maxLength(200)  @description("The goal, as the most recent trace it was learned from stated it. The embedding source for a near-match lookup.")
  goalSignature        string!    @pattern("^[0-9a-f]{64}$")  @description("sha256 of the normalized goal text, a vertical bar and the sorted input keys. See GoalSignature in packs/gamespack.")
  actions              []string!  @description("The action names, in order. Two skills of one signature differ here.")
  template             object!    @description("{steps: [{action, args: {key: {const: value} | {hole: id}}}], holes: [id]}. A hole id is <stepIndex>.<argKey>.")
  templateDigest       string!    @description("sha256 over the template's canonical JSON. A promotion approves exactly this digest.")
  triggers             []string   @description("The triggers the traces were recorded under. A trusted skill may be chosen for one without a model.")
  confirmation         string     @maxLength(200)  @description("A line the runtime may say when it runs the skill, taken from the newest trace's say.")
  learnedFrom          []string   @description("Bare ids of the plan traces it was generalized from.")
  learnedAt            datetime!  @description("When it was learned.")
  rung                 enum("shadow", "canary", "trusted", "retired")!  @description("The ladder rung, as component/work names it.")
  state                enum("shadow", "awaiting_approval", "canary", "trusted", "retired")!  @description("The rung as a person reads it: shadow with an open promotion is awaiting_approval. Written by the engine; clients never derive it.")
  shadowMatches        int!       @description("Consecutive shadow comparisons that matched.")
  canaryMatches        int!       @description("Consecutive clean canary runs.")
  distinctBindings     object     @description("Per hole id, the sha256 digests of the distinct values it took in the current streak.")
  failures             int!       @description("Consecutive failed runs on canary or trusted.")
  promotionApprovalId  string     @description("Non-empty while a promotion is open. It names the template digest the promotion is for.")
  promotionProposedAt  datetime   @description("When the latest promotion was proposed.")
  lastReplayAt         datetime   @description("The last shadow comparison or run. The retirement clock.")
  lastBinding          object     @description("The hole values of the newest matching comparison or clean run, for a runtime that runs the skill without a model.")
  lastReason           string     @description("Why the ladder last moved, or did not, in a sentence.")
  rungChangedAt        datetime   @description("When the rung last changed.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}

/// One plan the runtime executed, recorded after it finished: the evidence skills are learned
/// from. Written only through gamesRecordTrace, which checks the goal signature.
@displayCard(primary="goalText", secondary="outcome", tertiary="characterId")
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="30d", keepLatest=false, archive="none")
concept planTrace {
  ownerUserId    string!    @description("v1:identity:user.id of the owner. Stamped from actor.userId.")
  characterId    string!    @maxLength(64)  @description("The character that executed the plan.")
  goalText       string!    @maxLength(200)  @description("The goal the plan was for.")
  goalSignature  string!    @pattern("^[0-9a-f]{64}$")  @description("GoalSignature(goalText, keys of inputs), recomputed by the engine.")
  trigger        string!    @maxLength(64)  @description("What prompted the plan (an event kind, addressed chat, initiative).")
  inputs         object     @description("The goal's inputs. Only the KEYS enter the signature.")
  steps          []object!  @description("[{action, args, ok, durationMs, error}], in execution order.")
  actions        []string!  @description("The step action names, in order, derived by the engine.")
  outcome        enum("success", "failure", "abandoned")!  @description("How the plan ended.")
  reaction       enum("positive", "neutral", "negative")!  @description("How the player reacted. A negative trace is never learned from.")
  at             datetime!  @description("When the plan finished, in the game.")
  say            string     @maxLength(200)  @description("What the character said when it took the plan on.")
  skillId        string     @description("v1:games:skill.id when the plan was a skill run. A skill run is never learned from.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
  @relationship(type="references", as="ranSkill", field="skillId", target=skill, direction="outgoing")
}
```

Create `packs/gamespack/dsl/shapes.memql`:

```memql
// games pack -- projections. Every read in this pack names a shape, because a
// query with no shape answers the graph bundle, whose payload fields sit one
// level down, and a caller reading row.summary off it gets "" and no error.

@description("A player, whole.")
@row
shape player gamesPlayerFull {
  row.id
  ownerUserId
  gamertag
  displayName
  consented
  row.createdAt
}

@description("An episode, whole.")
@row
shape episode gamesEpisodeFull {
  row.id
  ownerUserId
  characterId
  playerId
  worldId
  at
  kind
  summary
  importance
  row.createdAt
}

@description("A preference, whole.")
@row
shape preference gamesPreferenceFull {
  row.id
  ownerUserId
  playerId
  subject
  valence
  confidence
  evidence
  row.createdAt
}

@description("A promise, whole.")
@row
shape promise gamesPromiseFull {
  row.id
  ownerUserId
  characterId
  playerId
  text
  status
  madeAt
  row.createdAt
}

@description("A place, whole.")
@row
shape place gamesPlaceFull {
  row.id
  ownerUserId
  worldId
  name
  kind
  position
  discoveredBy
  at
  row.createdAt
}

@description("A skill, whole: what it does, where it stands on the ladder and why. row.createdAt is the version's own time, which a ladder write stamps after.")
@row
shape skill gamesSkillFull {
  row.id
  ownerUserId
  goalText
  goalSignature
  actions
  template
  templateDigest
  triggers
  confirmation
  learnedFrom
  learnedAt
  rung
  state
  shadowMatches
  canaryMatches
  distinctBindings
  failures
  promotionApprovalId
  promotionProposedAt
  lastReplayAt
  lastBinding
  lastReason
  rungChangedAt
  row.createdAt
}

@description("A plan trace, whole.")
@row
shape planTrace gamesPlanTraceFull {
  row.id
  ownerUserId
  characterId
  goalText
  goalSignature
  trigger
  inputs
  steps
  actions
  outcome
  reaction
  at
  say
  skillId
  row.createdAt
}
```

Create `packs/gamespack/dsl/queries.memql`:

```memql
// games pack -- reads. Every row is the owner's, and the owned tier injects
// the owner conjunct, so no filter here names ownerUserId.

@description("Every player the caller registered, by display name.")
query player gamesPlayers {
  sort      "displayName", "asc"
  paginate  100
  shape     gamesPlayerFull
}

@description("One player by id.")
@unbounded("one player by id -- bounded to a single row")
query player gamesPlayerById {
  args {
    playerId  string!
  }
  filter  row => row.id == args.playerId
  shape   gamesPlayerFull
}

@description("One player's preferences, most confident first.")
query preference gamesPreferencesForPlayer {
  args {
    playerId  string!
  }
  filter    row => row.playerId == args.playerId
  sort      "confidence", "desc"
  paginate  50
  shape     gamesPreferenceFull
}

@description("The promises one character made to one player that are still open, newest first.")
query promise gamesOpenPromises {
  args {
    characterId  string!
    playerId     string!
  }
  filter    row => row.characterId == args.characterId && row.playerId == args.playerId && row.status == "open"
  sort      "madeAt", "desc"
  paginate  20
  shape     gamesPromiseFull
}

@description("Every promise made to one player, newest first.")
query promise gamesPromisesForPlayer {
  args {
    playerId  string!
  }
  filter    row => row.playerId == args.playerId
  sort      "madeAt", "desc"
  paginate  50
  shape     gamesPromiseFull
}

@description("The places known in one world, most recently confirmed first.")
query place gamesPlacesInWorld {
  args {
    worldId  string!
  }
  filter    row => row.worldId == args.worldId
  sort      "at", "desc"
  paginate  50
  shape     gamesPlaceFull
}

@description("One character's episodes at or after a moment, newest first, at most 40: what a session reflection reads.")
query episode gamesSessionEpisodes {
  args {
    characterId  string!
    since        datetime!
  }
  filter    row => row.characterId == args.characterId && row.at >= args.since
  sort      "at", "desc"
  paginate  40
  shape     gamesEpisodeFull
}

@description("Episodes about one player, newest first.")
query episode gamesEpisodesForPlayer {
  args {
    playerId  string!
  }
  filter    row => row.playerId == args.playerId
  sort      "at", "desc"
  paginate  50
  shape     gamesEpisodeFull
}

@description("Every skill, most recently moved first.")
query skill gamesSkills {
  sort      "rungChangedAt", "desc"
  paginate  100
  shape     gamesSkillFull
}

@description("Skills waiting for the owner's promotion decision, newest proposal first.")
query skill gamesSkillsAwaitingApproval {
  filter    row => row.state == "awaiting_approval"
  sort      "promotionProposedAt", "desc"
  paginate  50
  shape     gamesSkillFull
}

@description("One skill by id.")
@unbounded("one skill by id -- bounded to a single row")
query skill gamesSkillById {
  args {
    skillId  string!
  }
  filter  row => row.id == args.skillId
  shape   gamesSkillFull
}

@description("The skills learned for one goal signature.")
query skill gamesSkillsForSignature {
  args {
    goalSignature  string!
  }
  filter    row => row.goalSignature == args.goalSignature
  sort      "learnedAt", "desc"
  paginate  20
  shape     gamesSkillFull
}

@description("Every trusted skill, most recently used first: the pool a trigger lookup chooses from.")
query skill gamesTrustedSkills {
  filter    row => row.rung == "trusted"
  sort      "lastReplayAt", "desc"
  paginate  100
  shape     gamesSkillFull
}

@description("The successful traces of one goal signature, newest first: the evidence learning reads. The Go half drops skill runs and traces another action sequence produced.")
query planTrace gamesLearningTraces {
  args {
    goalSignature  string!
  }
  filter    row => row.goalSignature == args.goalSignature && row.outcome == "success" && row.reaction != "negative"
  sort      "at", "desc"
  paginate  20
  shape     gamesPlanTraceFull
}

@description("Every skill not yet retired, oldest learned first: what gamesSweepSkills re-examines. Oldest first because retirement reaches the least recently used, and a page would otherwise miss them.")
query skill gamesSkillsToSweep {
  filter    row => row.rung != "retired"
  sort      "learnedAt", "asc"
  paginate  200
  shape     gamesSkillFull
}
```

Create `packs/gamespack/dsl/mutations.memql`. The three writers at the end are `@serverOnly`; `recordGamesSkillLadder` stamps `createdAt: args.versionAt` so a ladder write files strictly after the version it read:

```memql
// games pack -- writes. Every mutation stamps ownerUserId from the actor, so a
// row is always the caller's own.
//
// THE CALLER NAMES EVERY ROW ID, and that is what makes these upserts. An
// insert on an id that exists writes a new version of it (a shallow
// read-merge), so a runtime that derives a stable id -- a preference from its
// player and subject, a place from its world and name -- rewrites one row
// instead of accumulating near-duplicates.
//
// THREE WRITERS ARE @serverOnly, for the work journal's reason: a
// client-reachable write would be a forgery. A plan trace whose signature
// nobody checked teaches the learner a skill nobody demonstrated, and a skill
// row a client could write is a promotion nobody earned. The pack's Go half
// calls them under internal origin with the caller's actor kept, so the owner
// tier still decides every row.

/// Register or update a player. playerId is the runtime's stable id for the player.
@actor
mutation player upsertGamesPlayer {
  args {
    playerId     string!  @maxLength(64)
    gamertag     string!  @maxLength(32)
    displayName  string!  @maxLength(64)
    consented    bool!
  }
  insert {
    accept { gamertag, displayName, consented }
    stamp {
      id: args.playerId
      ownerUserId: actor.userId
    }
  }
}

/// Record one episode. gamesRecordEpisodes is the usual path, because it also embeds the
/// summary; an episode written here directly is embedded by the next gamesEmbedPending.
@actor
mutation episode createGamesEpisode {
  args {
    episodeId    string!  @maxLength(64)
    characterId  string!  @maxLength(64)
    playerId     string
    worldId      string!  @maxLength(64)
    at           datetime!
    kind         enum("chat", "event", "activity", "session")!
    summary      string!  @maxLength(400)
    importance   int!  @minimum(1)  @maximum(10)
  }
  insert {
    accept { characterId, playerId, worldId, at, kind, summary, importance }
    stamp {
      id: args.episodeId
      ownerUserId: actor.userId
    }
  }
}

/// Write a player's preference about one subject. preferenceId is stable per (player, subject).
@actor
mutation preference upsertGamesPreference {
  args {
    preferenceId  string!  @maxLength(64)
    playerId      string!
    subject       string!  @maxLength(120)
    valence       float!  @minimum(-1)  @maximum(1)
    confidence    float!  @minimum(0)  @maximum(1)
    evidence      []string
  }
  insert {
    accept { playerId, subject, valence, confidence, evidence }
    stamp {
      id: args.preferenceId
      ownerUserId: actor.userId
    }
  }
}

/// Write a promise a character made to a player.
@actor
mutation promise upsertGamesPromise {
  args {
    promiseId    string!  @maxLength(64)
    characterId  string!  @maxLength(64)
    playerId     string!
    text         string!  @maxLength(200)
    status       enum("open", "kept", "dropped")!
    madeAt       datetime!
  }
  insert {
    accept { characterId, playerId, text, status, madeAt }
    stamp {
      id: args.promiseId
      ownerUserId: actor.userId
    }
  }
}

/// Mark a promise kept or dropped (or open again).
@actor
mutation promise setGamesPromiseStatus {
  args {
    promiseId  string!
    status     enum("open", "kept", "dropped")!
  }
  update {
    accept { status }
    stamp {
      id: args.promiseId
      ownerUserId: actor.userId
    }
  }
}

/// Write a place in a world. placeId is stable per (world, name).
@actor
mutation place upsertGamesPlace {
  args {
    placeId       string!  @maxLength(64)
    worldId       string!  @maxLength(64)
    name          string!  @maxLength(80)
    kind          string!  @maxLength(32)
    position      object!
    discoveredBy  string  @maxLength(64)
    at            datetime!
  }
  insert {
    accept { worldId, name, kind, position, discoveredBy, at }
    stamp {
      id: args.placeId
      ownerUserId: actor.userId
    }
  }
}

/// Record a plan trace. @serverOnly: gamesRecordTrace recomputes the goal signature and
/// derives the action list first, and a trace written around it would teach the learner
/// whatever its caller claimed.
@serverOnly
@actor
mutation planTrace createGamesPlanTrace {
  args {
    traceId        string!  @maxLength(64)
    characterId    string!  @maxLength(64)
    goalText       string!  @maxLength(200)
    goalSignature  string!
    trigger        string!  @maxLength(64)
    inputs         object
    steps          []object!
    actions        []string!
    outcome        enum("success", "failure", "abandoned")!
    reaction       enum("positive", "neutral", "negative")!
    at             datetime!
    say            string  @maxLength(200)
    skillId        string
  }
  insert {
    accept { characterId, goalText, goalSignature, trigger, inputs, steps, actions, outcome, reaction, at, say, skillId }
    stamp {
      id: args.traceId
      ownerUserId: actor.userId
    }
  }
}

/// Create a skill at the bottom of the ladder. @serverOnly: only the learner creates a skill,
/// from at least two successful traces it read itself.
@serverOnly
@actor
mutation skill createGamesSkill {
  args {
    skillId         string!
    goalText        string!  @maxLength(200)
    goalSignature   string!
    actions         []string!
    template        object!
    templateDigest  string!
    triggers        []string
    confirmation    string  @maxLength(200)
    learnedFrom     []string
  }
  insert {
    accept { goalText, goalSignature, actions, template, templateDigest, triggers, confirmation, learnedFrom }
    stamp {
      id: args.skillId
      ownerUserId: actor.userId
      learnedAt: now
      rung: "shadow"
      state: "shadow"
      shadowMatches: 0
      canaryMatches: 0
      distinctBindings: {}
      failures: 0
      promotionApprovalId: ""
      lastReason: "learned from two or more successful plans; it is compared with the model's plans in shadow"
      rungChangedAt: now
    }
  }
}

/// Write one ladder move: the WHOLE state component/work.Advance returned. @serverOnly: the
/// ladder writes the ladder. versionAt is stamped strictly after the version the move read, so
/// a move made on a replica with a lagging clock is still the newest version (memql#5431).
@serverOnly
@actor
mutation skill recordGamesSkillLadder {
  args {
    skillId              string!
    versionAt            datetime!
    rung                 enum("shadow", "canary", "trusted", "retired")!
    state                enum("shadow", "awaiting_approval", "canary", "trusted", "retired")!
    shadowMatches        int!
    canaryMatches        int!
    distinctBindings     object!
    failures             int!
    promotionApprovalId  string
    promotionProposedAt  datetime
    lastReplayAt         datetime
    lastBinding          object
    lastReason           string!
    rungChangedAt        datetime
  }
  update {
    accept { rung, state, shadowMatches, canaryMatches, distinctBindings, failures, promotionApprovalId, promotionProposedAt, lastReplayAt, lastBinding, lastReason, rungChangedAt }
    stamp {
      id: args.skillId
      createdAt: args.versionAt
      ownerUserId: actor.userId
    }
  }
}
```

- [ ] **Step 4: Write `pack.go`**

Create `packs/gamespack/pack.go`:

```go
// Package gamespack is the games pack: memory and skill learning for a game
// runtime that runs OUTSIDE the cluster (design record
// docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// A runtime plays in real time -- reflexes every tick, a decision every few
// seconds -- and the engine is what it remembers with and what it learns
// with. The runtime executes every action itself. This package stores,
// ranks, generalizes and governs; it never runs a game action.
//
// It is a PACK in the default build, like packs/reviewspack: linked into
// every binary by packs/anchor with no build tag, declared DISABLED, and
// governed per instance by v1:platform:packState.
package gamespack

import (
	"embed"
	"io/fs"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns: concepts are v1:games:*.
const Domain = "games"

// The pack's concept ids, and the user concept ownerUserId points at.
const (
	conceptUser       = "v1:identity:user"
	conceptPlayer     = "v1:games:player"
	conceptEpisode    = "v1:games:episode"
	conceptPreference = "v1:games:preference"
	conceptPromise    = "v1:games:promise"
	conceptPlace      = "v1:games:place"
	conceptSkill      = "v1:games:skill"
	conceptPlanTrace  = "v1:games:planTrace"
)

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it. FALSE: the pack stores memories about the people who play,
// and holding that kind of data is a decision an owner makes, not one an
// upgrade makes for them.
const DefaultEnabled = false

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("gamespack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Register wires the pack into the engine registries. packs/anchor calls it
// once per process, before the first concept load.
//
//   - RegisterPackDefault(domain, false): the pack ships disabled; an owner
//     enables it in Cluster > Modules and every node picks it up at its next
//     restart.
//
// It does NOT call RegisterStorefrontPack: the games pack has no shopper
// surface, and only an owner may switch it on.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
}
```

- [ ] **Step 5: Run the test to see it pass**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: `ok  	github.com/znasllc-io/memql/packs/gamespack`

- [ ] **Step 6: Rename the anchor and link the pack**

`anchor.Storefront()` becomes `anchor.Packs()` because it now links a pack that is not a storefront pack; `StorefrontDomains()` keeps the developer-flippable set the anchor test compares with `dsl.StorefrontPacks()`. Apply to `packs/anchor/anchor.go`:

```diff
diff --git a/packs/anchor/anchor.go b/packs/anchor/anchor.go
--- a/packs/anchor/anchor.go
+++ b/packs/anchor/anchor.go
@@ -1,5 +1,5 @@
-// Package anchor links the storefront packs, once, for everything that
-// needs an engine carrying them.
+// Package anchor links the default-build packs -- the storefront packs and
+// the games pack -- once, for everything that needs an engine carrying them.
 //
 // # Why this is a package and not a line in app/
 //
@@ -27,27 +27,36 @@ package anchor
 import (
 	"sync"
 
+	"github.com/znasllc-io/memql/packs/gamespack"
 	"github.com/znasllc-io/memql/packs/reviewspack"
 	"github.com/znasllc-io/memql/packs/wholesalepack"
 )
 
 var once sync.Once
 
-// Storefront registers every storefront pack, once per process.
+// Packs registers every pack the default build carries, once per process:
+// the storefront packs and the games pack.
 //
-// REACH IS NOT DECIDED HERE. Linking a pack is not enabling it: a
-// storefront pack ships DISABLED and its reach is governed by
-// v1:platform:packState, which is read later and folded over the declared
-// defaults. This function's whole job is that the concepts exist to be
-// imported and related to.
-func Storefront() {
+// REACH IS NOT DECIDED HERE. Linking a pack is not enabling it: each of these
+// packs ships DISABLED and its reach is governed by v1:platform:packState,
+// which is read later and folded over the declared defaults. This function's
+// whole job is that the concepts exist to be imported and related to.
+func Packs() {
 	once.Do(func() {
 		reviewspack.Register(reviewspack.Domain)
 		wholesalepack.Register(wholesalepack.Domain)
+		gamespack.Register(gamespack.Domain)
 	})
 }
 
-// Domains names what Storefront links, for a caller that reports it.
+// Domains names every pack Packs links, for a caller that reports it.
 func Domains() []string {
+	return []string{reviewspack.Domain, wholesalepack.Domain, gamespack.Domain}
+}
+
+// StorefrontDomains names the anchored packs that declare themselves
+// storefront packs (a developer may flip them). The games pack is not one:
+// it has no shopper surface, and only an owner may switch it on.
+func StorefrontDomains() []string {
 	return []string{reviewspack.Domain, wholesalepack.Domain}
 }
```

and to `packs/anchor/anchor_test.go`:

```diff
diff --git a/packs/anchor/anchor_test.go b/packs/anchor/anchor_test.go
--- a/packs/anchor/anchor_test.go
+++ b/packs/anchor/anchor_test.go
@@ -24,13 +24,13 @@ import (
 // This test holds the first; TestOnlyAnchoredPacksDeclareThemselvesStorefrontPacks
 // holds the second.
 func TestTheDeclaredStorefrontPacksAreTheAnchoredOnes(t *testing.T) {
-	Storefront()
+	Packs()
 
-	want := append([]string(nil), Domains()...)
+	want := append([]string(nil), StorefrontDomains()...)
 	sort.Strings(want)
 	if got := memqldsl.StorefrontPacks(); !reflect.DeepEqual(got, want) {
 		t.Fatalf("declared storefront packs = %v, anchored = %v. Call "+
-			"dsl.RegisterStorefrontPack from the pack's Register, or take it off anchor.Domains()",
+			"dsl.RegisterStorefrontPack from the pack's Register, or take it off anchor.StorefrontDomains()",
 			got, want)
 	}
 }
```

- [ ] **Step 7: Change every caller in the same commit**

```diff
diff --git a/cmd/memql-lsp/server.go b/cmd/memql-lsp/server.go
--- a/cmd/memql-lsp/server.go
+++ b/cmd/memql-lsp/server.go
@@ -147,7 +147,7 @@ func (s *server) buildSense(notify glsp.NotifyFunc) {
 	// pack's concept (`use wholesale.concepts.{ application }`) refuses the
 	// whole build, and the editor loses its registry and its load pass for
 	// every file of the workspace. Linking is not enabling; idempotent.
-	anchor.Storefront()
+	anchor.Packs()
 	// The lines are the build's own, as its Init resolved them, so they and
 	// the build's error are one answer even when a memql.toml changes while
 	// the build runs.
```

```diff
diff --git a/cmd/memqllint/main.go b/cmd/memqllint/main.go
--- a/cmd/memqllint/main.go
+++ b/cmd/memqllint/main.go
@@ -226,7 +226,7 @@ func run(args []string) int {
 		// Linking is not enabling: a storefront pack ships disabled and its
 		// reach is governed by v1:platform:packState. This makes the
 		// concepts importable, which is all a lint needs.
-		anchor.Storefront()
+		anchor.Packs()
 
 		parityDiags, skipped, perr := memql.LintUnifiedTree(nil, root)
 		// What the parity pass found that LOADS -- a use of a deprecated form
```

```diff
diff --git a/component/packages/analyze.go b/component/packages/analyze.go
--- a/component/packages/analyze.go
+++ b/component/packages/analyze.go
@@ -211,7 +211,7 @@ func analyzeDSL(tree fs.FS, rep *Report, logger *slog.Logger) {
 	// "dsl_refuses_boot" against a tree that loads perfectly well on a real
 	// node. Idempotent, because a node runs this analysis in-process while
 	// already carrying the packs.
-	anchor.Storefront()
+	anchor.Packs()
 
 	result, gateErr := memql.AnalyzePackageDSL(logger, dslTree)
 	if gateErr != nil {
```

```diff
diff --git a/test/conformance/corpus_test.go b/test/conformance/corpus_test.go
--- a/test/conformance/corpus_test.go
+++ b/test/conformance/corpus_test.go
@@ -1432,7 +1432,7 @@ func corpusProbeEngine(tree fs.FS) (*memql.MemQLEngine, func(), error) {
 	// needs the form it extends to exist -- and a cell that could only ever
 	// be refused would test the refusal rather than the annotation.
 	// Idempotent, so every cell in the run shares one registration.
-	anchor.Storefront()
+	anchor.Packs()
 	_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, tree)
 	stop := func() {
 		unmount()
```

Rename the app file, then edit it:

```bash
git mv app/anchor_storefront_packs.go app/anchor_packs.go
```

```diff
diff --git a/app/anchor_storefront_packs.go b/app/anchor_packs.go
similarity index 84%
rename from app/anchor_storefront_packs.go
rename to app/anchor_packs.go
--- a/app/anchor_storefront_packs.go
+++ b/app/anchor_packs.go
@@ -6,8 +6,9 @@ import (
 	"github.com/znasllc-io/memql/packs/anchor"
 )
 
-// anchor_storefront_packs.go links the STOREFRONT PACKS into the default
-// build (epic memql#5532, issue memql#5549).
+// anchor_packs.go links the DEFAULT-BUILD PACKS into every binary: the
+// storefront packs (epic memql#5532, issue memql#5549) and the games pack
+// (design record docs/superpowers/specs/2026-10-18-games-pack-design.md).
 //
 // # What this closes
 //
@@ -42,7 +43,7 @@ import (
 // after the enablement read would leave the
 // declaration unheard and reviews would ship ENABLED -- the exact outcome
 // the default exists to prevent, arriving silently.
-func (a *App) anchorStorefrontPacks() {
+func (a *App) anchorPacks() {
 	// THE REGISTRATION ITSELF LIVES IN packs/anchor, and it is idempotent.
 	// It has to be: the offline package analyzer needs an engine carrying
 	// these packs too (a product's DSL imports their concepts), and a node
@@ -55,10 +56,12 @@ func (a *App) anchorStorefrontPacks() {
 	// tag, disabled by default, reach governed by packState -- and it is
 	// anchored with reviews rather than in its own file for the reason this
 	// file exists at all: a reader asking "which packs does a default image
-	// carry?" must be able to answer it from one place.
-	anchor.Storefront()
+	// carry?" must be able to answer it from one place. THE GAMES PACK joins
+	// on the same terms; it declares no storefront surface, so only an owner
+	// may switch it on.
+	anchor.Packs()
 	if a != nil && a.Logger != nil {
-		a.Logger.Info("storefront packs linked into this build; reach is governed by "+
+		a.Logger.Info("default-build packs linked into this build; reach is governed by "+
 			"v1:platform:packState",
 			"component", memql.ComponentName,
 			"packs", anchor.Domains(),
```

```diff
diff --git a/app/database.go b/app/database.go
--- a/app/database.go
+++ b/app/database.go
@@ -41,7 +41,7 @@ func (a *App) databaseAndConcepts() {
 	// phase: otherwise a valid product import resolves during Analyze but is
 	// still bare when the database registry is built at boot. Registration
 	// declares disabled defaults; behavior remains gated by loadPackEnablement.
-	a.anchorStorefrontPacks()
+	a.anchorPacks()
 
 	// Mount any product DSL delivered at runtime via MEMQL_DSL_PATH (a bundle
 	// image's init-container populates that volume) BEFORE the first Tree()
```

```diff
diff --git a/app/storefront_boot_order_test.go b/app/storefront_boot_order_test.go
--- a/app/storefront_boot_order_test.go
+++ b/app/storefront_boot_order_test.go
@@ -81,14 +81,14 @@ concept applicationDetail {
 	if _, err := app.registry.Get("v1:wholesale:application"); err != nil {
 		t.Fatal(err)
 	}
-	for _, name := range []string{"reviews", "wholesale"} {
+	for _, name := range []string{"reviews", "wholesale", "games"} {
 		if enabled, declared := memqldsl.PackDefaults()[name]; !declared || enabled {
 			t.Fatalf("pack %s must remain declared disabled, got %v/%v", name, enabled, declared)
 		}
 	}
 	// The analyzer may anchor again in this same process; no double registration
 	// or implicit enablement may result from moving the first registration early.
-	app.anchorStorefrontPacks()
+	app.anchorPacks()
 	if memqldsl.PackDefaultEnabled("reviews") || memqldsl.PackDefaultEnabled("wholesale") {
 		t.Fatal("repeat anchor enabled a storefront pack")
 	}
```

```diff
diff --git a/app/shopper_auth_boot_order_test.go b/app/shopper_auth_boot_order_test.go
--- a/app/shopper_auth_boot_order_test.go
+++ b/app/shopper_auth_boot_order_test.go
@@ -49,7 +49,7 @@ func TestShopperAuthDeclarationsAreCapturedAfterPackAnchoring(t *testing.T) {
 	}
 	app := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), identityVerifier: &verifier.Verifier{}}
 	middleware := app.identityHTTPMiddleware()
-	app.anchorStorefrontPacks()
+	app.anchorPacks()
 	if !hasReviewPack() {
 		t.Fatal("packs declared no shopper routes")
 	}
```

Repoint the four places that name the old file (a comment, a doc example, the project layout). The dated design record `docs/superpowers/specs/2026-09-23-connect-shopify-design.md` cites a past commit's line and stays as it is:

```diff
diff --git a/CLAUDE.md b/CLAUDE.md
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -108,9 +108,10 @@ MemQL/
 │                      copied (brand_shared_source_test.go, memql#4266)
 ├── clients/os/        MemQL OS -- desktop shell + graphical ops console,
 │                      served by component/edge as a site row
-├── packs/             Storefront packs in the DEFAULT BUILD (epic memql#5532),
-│                      mounted on every node by app/anchor_storefront_packs.go
-│                      and shipped DISABLED. examples/ keeps the tag-gated packs
+├── packs/             Packs in the DEFAULT BUILD (the storefront packs, epic
+│                      memql#5532, and the games pack), mounted on every node
+│                      by app/anchor_packs.go and shipped DISABLED. examples/
+│                      keeps the tag-gated packs
 ├── component/         Core Go components (memql, grpc, events, database,
 │                      server, auth, edge, language, procedure, bus, node,
 │                      architecture, observe, envregistry, ...)
```

```diff
diff --git a/component/packages/storefront_pack_analysis_test.go b/component/packages/storefront_pack_analysis_test.go
--- a/component/packages/storefront_pack_analysis_test.go
+++ b/component/packages/storefront_pack_analysis_test.go
@@ -11,7 +11,7 @@ import (
 // pack's concepts, and the analyzer has to know that.
 //
 // THE ANALYZER MODELS AN ENGINE. Every published image links the storefront
-// packs with no build tag (app/anchor_storefront_packs.go), so a product
+// packs with no build tag (app/anchor_packs.go), so a product
 // domain that imports one loads perfectly well on a real node. The offline
 // analysis linked none of them, so the same tree came back
 // "dsl_refuses_boot" with a message telling the author to add an import
```

````diff
diff --git a/docs/public/build/building-a-pack.md b/docs/public/build/building-a-pack.md
--- a/docs/public/build/building-a-pack.md
+++ b/docs/public/build/building-a-pack.md
@@ -227,7 +227,7 @@ release.
 
 ```go
 // packs/mypack/pack.go -- no build tag, no init(); Register is called from
-// app/anchor_storefront_packs.go, which every node type runs.
+// app/anchor_packs.go, which every node type runs.
 func Register(domain string) {
     memqldsl.RegisterTree(domain, Tree())
     memqldsl.RegisterPackDefault(domain, DefaultEnabled) // false, for a storefront pack
````

```diff
diff --git a/packs/wholesalepack/live_e2e_test.go b/packs/wholesalepack/live_e2e_test.go
--- a/packs/wholesalepack/live_e2e_test.go
+++ b/packs/wholesalepack/live_e2e_test.go
@@ -102,7 +102,7 @@ func liveWholesaleEngine(t *testing.T, disabled ...string) *memql.MemQLEngine {
 		t.Fatal("nil bun.DB after migrate")
 	}
 
-	// REGISTERED THE WAY app/anchor_storefront_packs.go DOES IT, so the
+	// REGISTERED THE WAY app/anchor_packs.go DOES IT, so the
 	// adapters are in the registry and the shopper surface is declared --
 	// which is what makes this a test of the pack rather than of its DSL.
 	//
```

Confirm nothing live still names the old spellings:

Run: `git grep -n "anchor_storefront_packs\|anchorStorefrontPacks\|anchor\.Storefront()" -- ':!docs/superpowers' ':!*.json'; echo "matches=$?"`
Expected: `matches=1` (nothing found).

- [ ] **Step 8: Run the anchor and boot-order tests**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/anchor/ && go test -count=1 -run 'TestShopperAuthDeclarationsAreCapturedAfterPackAnchoring|TestDatabasePhaseResolvesProductPackImportsBeforeEnginePhase' github.com/znasllc-io/memql/app`
Expected: `ok  	github.com/znasllc-io/memql/packs/anchor` and `ok  	github.com/znasllc-io/memql/app`

- [ ] **Step 9: Prove the queries, shapes and mutations load**

`memqllint` runs the engine's `Init` offline over the core tree and every anchored pack, so a broken query refuses here, naming its file and construct.

Run: `go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `exit=0`. (A deliberate typo such as `sort      "displayNameX", "asc"` in `gamesPlayers` prints `games/queries.memql: query "gamesPlayers" (parse): sort key "displayNameX": v1:games:player declares no field "displayNameX" -- did you mean "displayName"? [sort_key_unknown]` and exits 1.)

- [ ] **Step 10: Record the measured embed count**

The pack embeds six files (`all:dsl`). Add the line after the `packs/reviewspack` entry of `embed_inventory_test.go`:

```diff
diff --git a/embed_inventory_test.go b/embed_inventory_test.go
--- a/embed_inventory_test.go
+++ b/embed_inventory_test.go
@@ -114,6 +114,7 @@ var embedInventory = map[string]int{
 	"github.com/znasllc-io/memql/examples/deploypack":    4, // all:dsl (+memql.toml, the pack's language line -- memql#5357)
 	"github.com/znasllc-io/memql/examples/referencepack": 6, // all:dsl (+memql.toml, memql#5357)
 	"github.com/znasllc-io/memql/packs/reviewspack":      8, // all:dsl (memql#4139; +memql.toml, memql#5357; +queries.memql and +shapes.memql -- the reads the pack lacked, epic memql#5532). MEASURED, never derived
+	"github.com/znasllc-io/memql/packs/gamespack":        6, // all:dsl (design record 2026-10-18-games-pack-design.md): concepts, shapes, queries, mutations, memql.toml, namespace.pin. MEASURED, never derived
 	"github.com/znasllc-io/memql/packs/wholesalepack":    8, // all:dsl (epic memql#5533): concepts, shapes, queries, mutations, builtins, tools, memql.toml, namespace.pin. The BUILTIN decides and the MUTATION writes -- a builtin's returned nodes are the expression's value and are never persisted. MEASURED, never derived
 	"github.com/znasllc-io/memql/examples/shopifypack":   8, // all:dsl -- memql#4138 attach/secrets/sync (+memql.toml, memql#5357; +namespace.pin -- epic memql#5375 retired @namespace, so a pack whose namespace diverges from its directory states it in a one-line pin instead). MEASURED from the gate, never derived
 	"github.com/znasllc-io/memql/integrations":           1, // *.json
```

Run: `go test -count=1 -run TestEmbeddedFileCountsAreStable github.com/znasllc-io/memql`
Expected: `ok  	github.com/znasllc-io/memql`. (A failure naming `component/identity/web ... 6 MISSING` means `make identity-tailwind` was not run in this worktree.)

- [ ] **Step 11: Regenerate the concept snapshot and the architecture model**

The snapshot gains the seven concepts; the model must lose `anchor.Storefront` and `app.anchorStorefrontPacks`, which no longer exist (the model gate is a subset check, so a removed symbol fails it and an added one does not).

Run: `make concept-snapshot && make concept-snapshot-check && make arch-model && make arch-model-check`
Expected: `wrote component/conceptfields/concept-fields.snapshot.json: 272 concepts, ...`, then `ok` for `TestConceptFieldSnapshotIsNotStale`, `memql-arch: wrote ... nodes`, and `ok  	github.com/znasllc-io/memql/component/architecture`.

- [ ] **Step 12: Commit**

```bash
git add packs/gamespack/dsl/memql.toml packs/gamespack/dsl/namespace.pin packs/gamespack/dsl/concepts.memql \
  packs/gamespack/dsl/shapes.memql packs/gamespack/dsl/queries.memql packs/gamespack/dsl/mutations.memql \
  packs/gamespack/pack.go packs/gamespack/pack_test.go packs/anchor/anchor.go packs/anchor/anchor_test.go \
  cmd/memql-lsp/server.go cmd/memqllint/main.go component/packages/analyze.go test/conformance/corpus_test.go \
  app/anchor_packs.go app/database.go app/storefront_boot_order_test.go app/shopper_auth_boot_order_test.go \
  CLAUDE.md component/packages/storefront_pack_analysis_test.go docs/public/build/building-a-pack.md \
  packs/wholesalepack/live_e2e_test.go embed_inventory_test.go component/conceptfields/concept-fields.snapshot.json \
  component/architecture/embedded/topology.model.json
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): the games pack's data model, anchored and disabled" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 2: Forward the pack's live-graph events across replicas

Mesh forwarding is default-deny (`component/node/routing.go:47`): without a rule, a row written through one bff replica never reaches a live view attached to the other.

**Files:**
- Create: `packs/gamespack/routing.go`, `packs/gamespack/routing_test.go`
- Modify: `packs/gamespack/pack.go` (`Register`)

**Interfaces:**
- Consumes: `node.RegisterRoutingRule(node.RoutingRule{Pattern string, TargetType string})`, `node.GraphEventTopic(verb, concept string) string`, `node.ForwardsGraphEvent(topic string) bool` (the real evaluator, `component/node/routing_reach.go:72`); the concept ids from Task 1.
- Produces: `routedConcepts []string` (player, episode, preference, promise, place, skill), `registerRoutingRules()` (idempotent, `sync.Once`), called from `Register`.

- [ ] **Step 1: Write the failing test**

Create `packs/gamespack/routing_test.go`:

```go
package gamespack

import (
	"testing"

	"github.com/znasllc-io/memql/component/node"
)

// A live memory view subscribes to these concepts on whichever replica it is
// attached to; a row written through another replica must reach it.
func TestEveryLiveConceptCrossesReplicas(t *testing.T) {
	registerRoutingRules()
	for _, concept := range routedConcepts {
		for _, verb := range []string{"created", "updated"} {
			topic := node.GraphEventTopic(verb, concept)
			if !node.ForwardsGraphEvent(topic) {
				t.Errorf("%s is not forwarded; a live view on another replica goes dark", topic)
			}
		}
	}
}

func TestPlanTracesStayOnTheirReplica(t *testing.T) {
	registerRoutingRules()
	for _, verb := range []string{"created", "updated"} {
		if node.ForwardsGraphEvent(node.GraphEventTopic(verb, conceptPlanTrace)) {
			t.Errorf("plan trace %s events are forwarded; they are one row per plan and nothing watches them live", verb)
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/routing_test.go:12:2: undefined: registerRoutingRules
packs/gamespack/routing_test.go:13:26: undefined: routedConcepts
packs/gamespack/routing_test.go:24:2: undefined: registerRoutingRules
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write the rules**

Create `packs/gamespack/routing.go` (broadcast, `TargetType: ""`; created and updated only, because nothing publishes `graph.node.deleted` for these concepts; `planTrace` deliberately absent):

```go
package gamespack

import (
	"sync"

	"github.com/znasllc-io/memql/component/node"
)

// routing.go -- the live graph's forward rules for this pack's concepts.
//
// Mesh forwarding is DEFAULT-DENY (component/node/routing.go): a
// graph.node.* event that matches no rule stays on the replica that wrote
// the row. A runtime writes through whichever bff replica the front door
// picked and a browser watches through whichever one it is attached to, so
// without these rules a live memory view is correct on load and frozen
// after -- the failure routing.go calls the worst of the three, because it
// looks like it is working.
//
// BROADCAST, and safe to: no automation in the tree triggers on a
// v1:games:* event, so a forwarded event has no consumer to double-fire, and
// row admission still decides which streams see each row (an owned row
// reaches its owner and a cluster owner).
//
// CREATED AND UPDATED ONLY. Nothing publishes graph.node.deleted for these
// concepts (forget and retention delete in SQL), and a rule for an event
// nothing emits is a rule nobody could tell was broken.
//
// v1:games:planTrace IS DELIBERATELY NOT FORWARDED: one row per executed
// plan, read on demand by learning and by nobody live -- the volume argument
// routing_reach.go records for v1:work:observation.

// routedConcepts are the concepts a live view subscribes to.
var routedConcepts = []string{
	conceptPlayer, conceptEpisode, conceptPreference, conceptPromise, conceptPlace, conceptSkill,
}

var routingOnce sync.Once

// registerRoutingRules registers the forward rules once per process.
func registerRoutingRules() {
	routingOnce.Do(func() {
		for _, concept := range routedConcepts {
			for _, verb := range []string{"created", "updated"} {
				node.RegisterRoutingRule(node.RoutingRule{Pattern: node.GraphEventTopic(verb, concept), TargetType: ""})
			}
		}
	})
}
```

Register them from `pack.go`:

```diff
diff --git a/packs/gamespack/pack.go b/packs/gamespack/pack.go
--- a/packs/gamespack/pack.go
+++ b/packs/gamespack/pack.go
@@ -58,10 +58,14 @@ func Tree() fs.FS {
 //   - RegisterPackDefault(domain, false): the pack ships disabled; an owner
 //     enables it in Cluster > Modules and every node picks it up at its next
 //     restart.
+//   - registerRoutingRules: the live graph's forward rules for this pack's
+//     concepts, without which a row written through one replica never
+//     reaches a subscriber on another (routing.go).
 //
 // It does NOT call RegisterStorefrontPack: the games pack has no shopper
 // surface, and only an owner may switch it on.
 func Register(domain string) {
 	memqldsl.RegisterTree(domain, Tree())
 	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
+	registerRoutingRules()
 }
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -count=1 -v -run 'TestEveryLiveConceptCrossesReplicas|TestPlanTracesStayOnTheirReplica' github.com/znasllc-io/memql/packs/gamespack/`
Expected: `--- PASS: TestEveryLiveConceptCrossesReplicas` and `--- PASS: TestPlanTracesStayOnTheirReplica`.

- [ ] **Step 5: Commit**

```bash
git add packs/gamespack/routing.go packs/gamespack/routing_test.go packs/gamespack/pack.go
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): forward the pack's live-graph events across replicas" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 3: The Go half, and episodes embedded on the background lane

The shared vocabulary, the seams (`rowStore`, `embeddingSpace`), the full `Provider`, the database harness every later task tests through, and the first two builtins. Review Focus line 1 is pinned here.

**Files:**
- Create: `packs/gamespack/values.go`, `packs/gamespack/store.go`, `packs/gamespack/space.go`, `packs/gamespack/capabilities.go`, `packs/gamespack/episodes.go`, `packs/gamespack/dsl/builtins.memql`, `packs/gamespack/values_test.go`, `packs/gamespack/capabilities_test.go`, `packs/gamespack/main_dbschema_test.go`, `packs/gamespack/harness_db_test.go`, `packs/gamespack/episodes_db_test.go`, `packs/gamespack/pack_db_test.go`
- Modify: `packs/gamespack/pack.go` (replaced whole), `embed_inventory_test.go:117` (6 to 7)

**Interfaces:**
- Consumes: `auth.AccessFromContext`, `memql.BareShortId`, `id.BuildNodeId`, `num.ClampInt64`, `num.ClampFloat64`; `langparser.RenderCall(name, args)`, `memql.IntegrationEngineAccess.Execute`, `memql.MaterializeRows`; `memql.CurrentEmbedderBinding`, `memql.EmbeddingVectorTable(provider, dims)`, `memql.EmbeddingVectorTables(ctx, db)`, `memql.ValidateEmbeddingVector`; `memql.ContextWithBackgroundLane`; `memql.BindPluginToPack`, `memql.RegisterPluginForContract`, `memql.PluginContractVersion`, `memql.PluginContext` (`Logger`, `Engine`, `BunDB`, `GateDB`, `InvalidateConcept`, `ResolveEmbeddingProvider`).
- Produces (later tasks use every one of these names):
  - `values.go`: `actorUserID(ctx) string`, `requireActor(ctx) (string, error)` (refuses `games_unauthenticated`), `ownerKey(owner string) string` (canonical `v1:identity:user:<id>`), `canonical(concept, value string) string`, `bare(value string) string`, `bareRow(map[string]any) map[string]any`, `asString(any) string`, `trimmed(any) string`, `asInt(v any, fallback int) int`, `asFloat(v any, fallback float64) float64`, `clampInt(v, lo, hi int) int`, `asStrings(any) []string`, `sameStrings(a, b []string) bool`, `asObject(any) map[string]any`, `timeOf(any) time.Time`, `rfc3339(time.Time) string`, `versionAfter(prior, now time.Time) time.Time`, `answer(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error)` (concept `integration:games:<kind>`).
  - `store.go`: `type rowStore interface { query(ctx, name string, args map[string]any) ([]map[string]any, error); write(ctx, name string, args map[string]any) error; embedStore(ctx, nodeID, concept, text string) error }`, `type engineStore struct{ engine memql.IntegrationEngineAccess }`, `compact(map[string]any) map[string]any`.
  - `space.go`: `type embeddingSpace interface { table(ctx) (string, error); embedQuery(ctx, text string) ([]float32, error); store(ctx, nodeID, concept, text string) error }`, `type clusterSpace struct{ resolve; rows rowStore; db func() *sql.DB }`, `vectorLiteral([]float32) string`.
  - `pack.go`: `ContractVersion`, `integrationName = "games"`, `type Provider struct{ store rowStore; space embeddingSpace; db func() *sql.DB; gates func() *sql.DB; invalidate func(string); log *slog.Logger; now func() time.Time }`, `NewProvider(memql.PluginContext) (memql.IntegrationProvider, error)`, `(*Provider).IntegrationName() string`, `(*Provider).clock() time.Time`, `(*Provider).logger() *slog.Logger`, `ctxDone(ctx) bool`.
  - `capabilities.go`: `(*Provider).Capabilities() []memql.IntegrationCapability`; this task's entries `recordEpisodes`, `embedPending`.
  - `episodes.go`: `maxEpisodesPerCall = 50`, `maxPendingPerCall = 200`, `(*Provider).recordEpisodes`, `(*Provider).embedPending`, `pendingStatement(table string) string`.
  - DSL: `builtin gamesRecordEpisodes`, `builtin gamesEmbedPending`, `builtin gamesEmbedStore` (`@executor("integration.embedding.store")`, no `@sdk`).
  - Test harness (database-gated files): `type liveGames struct{ eng *memql.MemQLEngine; provider *Provider; db *bun.DB; table string }`, `newLiveGames(t, disabled ...string) *liveGames`, `runID() string`, `asOwner(userID string) context.Context`, `(*liveGames).call(t, ctx, kind, name, args) ([]map[string]any, error)`, `(*liveGames).must(...) []map[string]any`, `type testSpace struct{ db *sql.DB; tableName string }` (bag-of-words, 16 dimensions), `type failingSpace struct{ testSpace }` (every embed fails with `errModelAway`), `episode(id, character, player, summary string, at time.Time, importance int) map[string]any`, `episodesArg(...map[string]any) []any`, `(*liveGames).vectorsFor(t, episodeIDs ...string) int`.

- [ ] **Step 1: Write the failing test for the shared values**

Create `packs/gamespack/values_test.go`:

```go
package gamespack

import (
	"testing"
	"time"
)

func TestVersionAfterIsStrictlyAfterWhatWasRead(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if got := versionAfter(time.Time{}, now); !got.Equal(now) {
		t.Fatalf("no prior version: got %v, want now", got)
	}
	if got := versionAfter(now.Add(-time.Second), now); !got.Equal(now) {
		t.Fatalf("an older prior: got %v, want now", got)
	}
	// A replica whose clock lags the one that wrote the prior version.
	ahead := now.Add(3 * time.Second)
	if got := versionAfter(ahead, now); !got.Equal(ahead.Add(time.Microsecond)) {
		t.Fatalf("a prior from the future: got %v, want one microsecond after it", got)
	}
}

func TestCanonicalComposesOnlyWhatIsBare(t *testing.T) {
	if got := canonical(conceptPlayer, "p-sam"); got != "v1:games:player:p-sam" {
		t.Fatalf("bare: %s", got)
	}
	if got := canonical(conceptPlayer, "v1:games:player:p-sam"); got != "v1:games:player:p-sam" {
		t.Fatalf("canonical: %s", got)
	}
	if got := canonical(conceptPlayer, ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/values_test.go:10:12: undefined: versionAfter
packs/gamespack/values_test.go:13:12: undefined: versionAfter
packs/gamespack/values_test.go:18:12: undefined: versionAfter
packs/gamespack/values_test.go:24:12: undefined: canonical
packs/gamespack/values_test.go:27:12: undefined: canonical
packs/gamespack/values_test.go:30:12: undefined: canonical
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write `values.go`**

`ownerKey` exists because `ownerUserId` is stored canonical while the actor carries the bare id; `versionAfter` is the "stamp after the version read" rule (`component/emailrules/fire.go:261`); `asInt` narrows through `core/num`:

```go
package gamespack

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// values.go -- the small vocabulary every handler shares: who is calling,
// how an id is spelled, how a decoded payload value narrows to a Go value,
// and the one node a builtin answers with.

// actorUserID is the caller's user id, or "" when the context carries none.
func actorUserID(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		return strings.TrimSpace(ac.UserId)
	}
	return ""
}

// requireActor refuses a call with no person behind it: every games row is
// somebody's, and a row written with no owner is readable by nobody.
func requireActor(ctx context.Context) (string, error) {
	owner := actorUserID(ctx)
	if owner == "" {
		return "", fmt.Errorf("games_unauthenticated: this call needs a signed-in owner")
	}
	return owner, nil
}

// ownerKey is the owner as a hand-written statement compares it: CANONICAL.
// ownerUserId declares @relationship(type="parent", target=user), so the
// engine canonicalizes it on insert ("v1:identity:user:<id>") while the
// actor carries the bare id; a statement comparing the two spellings matches
// nothing and says nothing.
func ownerKey(owner string) string { return canonical(conceptUser, owner) }

// canonical returns the canonical id of a row of concept, from a bare or a
// canonical spelling. Empty stays empty.
func canonical(concept, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, concept+":") {
		return value
	}
	return id.BuildNodeId(concept, memql.BareShortId(value))
}

// bare returns the bare short id of any id spelling.
func bare(value string) string { return memql.BareShortId(strings.TrimSpace(value)) }

// bareRow puts a read row's id in the bare spelling every answer carries:
// clients never compose, parse or compare canonical ids.
func bareRow(r map[string]any) map[string]any {
	if r != nil {
		r["id"] = bare(trimmed(r["id"]))
	}
	return r
}

// asString narrows a payload value to text.
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// trimmed is asString plus the trim every caller wants.
func trimmed(v any) string { return strings.TrimSpace(asString(v)) }

// asInt narrows a payload number to an int. Out of range SATURATES, through
// core/num -- the memql#4779 rule that every narrowing names its answer.
func asInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return num.ClampInt64(n)
	case float64:
		if math.IsNaN(n) {
			return fallback
		}
		return num.ClampFloat64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return fallback
		}
		return asInt(f, fallback)
	}
	return fallback
}

// asFloat narrows a payload number to a float64.
func asFloat(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fallback
		}
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return fallback
		}
		return f
	}
	return fallback
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// asStrings narrows a payload list of strings.
func asStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := trimmed(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// sameStrings reports two lists equal element by element, in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// asObject narrows a payload object.
func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// timeOf reads a stored datetime, which the engine hands back as an RFC 3339
// string (or, in-process, occasionally as a time.Time).
func timeOf(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(t)); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

// rfc3339 formats a time the way a datetime argument is written.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// versionAfter is when a read-modify-write's new version is stamped: now, or
// one microsecond after the version it read, whichever is later
// (component/emailrules firedAtAfter, memql#5431). The newest version is the
// one with the newest createdAt, so a replica whose clock lags would
// otherwise file its write BEHIND the version it read; microseconds, because
// that is what the column stores.
func versionAfter(prior, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	if prior.IsZero() {
		return now
	}
	prior = prior.UTC().Truncate(time.Microsecond)
	if now.After(prior) {
		return now
	}
	return prior.Add(time.Microsecond)
}

// answer wraps a capability's result as the one node a builtin returns. The
// node is the expression's value and is never persisted.
func answer(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("games: marshal %s answer: %w", kind, err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("games:%s:%d", kind, time.Now().UTC().UnixNano()),
		Concept:   "integration:games:" + kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `go test -count=1 -run 'TestVersionAfterIsStrictlyAfterWhatWasRead|TestCanonicalComposesOnlyWhatIsBare' github.com/znasllc-io/memql/packs/gamespack/`
Expected: `ok  	github.com/znasllc-io/memql/packs/gamespack`

- [ ] **Step 5: Write the failing tests for the Go half and episodes**

Create `packs/gamespack/main_dbschema_test.go` (routes the schema migration through the shared advisory-locked `EnsureSchema`):

```go
package gamespack

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain routes this package's schema migration through the shared,
// advisory-locked EnsureSchema so it is serialized against the other
// db-gated packages CI runs in the same lane (memql#2551). With no database
// reachable EnsureSchema is a no-op and the db-gated tests self-skip;
// MEMQL_REQUIRE_DB=1 turns that skip into a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
```

Create `packs/gamespack/harness_db_test.go`. A real engine, the pack's real DSL and Go half, real write gates; one stand-in, the embedder, which hashes words into a 16-dimension vector in a table of its own. `failingSpace` is the model being away:

```go
package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// harness_db_test.go -- a real engine, a real database, the pack's real DSL
// and Go half, and ONE stand-in: the embedder. A database test cannot reach a
// model, so testSpace embeds by hashing words into a small vector and stores
// it in a vector table of its own. Everything else -- the mutations, the
// owner tier, the write gates, the hand-written statements -- is the thing
// that ships.

// testDims is the test embedder's width; testProvider names its space.
const (
	testDims     = 16
	testProvider = "games-test-bag-of-words"
)

type liveGames struct {
	eng      *memql.MemQLEngine
	provider *Provider
	db       *bun.DB
	table    string
}

// runID makes every identifier unique per run: db-gated tests do not clean
// up, and a count asserted against a fixed id fails on the second run.
func runID() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// asOwner is a person's actor, exactly as auth.ContextWithUserActor builds it
// for a signed-in caller.
func asOwner(userID string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userID)
}

func newLiveGames(t *testing.T, disabled ...string) *liveGames {
	t.Helper()
	dsn := dbtest.DSN()
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		dbtest.Unreachable(t, "games pack live e2e", dsn, err)
	}
	_ = probe.Close()

	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("database did not become ready within 60s")
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("nil bun.DB after migrate")
	}

	registerOnce.Do(func() { Register(Domain) })
	memqldsl.SetDisabledPackDomains(disabled)
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(nil) })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	lg := &liveGames{eng: eng, db: bunDB}
	if slices.Contains(disabled, Domain) {
		return lg
	}

	gates, err := database.NewGatePool(dsn, 0)
	if err != nil {
		t.Fatalf("NewGatePool: %v", err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	table, err := memql.EnsureEmbeddingVectorTable(context.Background(), bunDB.DB, testProvider, testDims)
	if err != nil {
		t.Fatalf("EnsureEmbeddingVectorTable: %v", err)
	}
	lg.table = table
	lg.provider = &Provider{
		store:      engineStore{engine: eng},
		space:      testSpace{db: bunDB.DB, tableName: table},
		db:         func() *sql.DB { return bunDB.DB },
		gates:      func() *sql.DB { return gates },
		invalidate: eng.InvalidateCacheForConcept,
		log:        eng.Logger,
		now:        time.Now,
	}
	if err := eng.RegisterIntegration(lg.provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return lg
}

// call runs one construct of the given kind with rendered arguments.
func (lg *liveGames) call(t *testing.T, ctx context.Context, kind, name string, args map[string]any) ([]map[string]any, error) {
	t.Helper()
	rendered, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		t.Fatalf("RenderCall %s: %v", name, err)
	}
	res, err := lg.eng.Execute(ctx, kind+" "+rendered)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// must is call that fails the test on an error.
func (lg *liveGames) must(t *testing.T, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
	t.Helper()
	rows, err := lg.call(t, ctx, kind, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return rows
}

// testSpace is the stand-in embedder: each lower-cased word hashes into one of
// testDims buckets, so texts sharing words are near and texts sharing none are
// orthogonal.
type testSpace struct {
	db        *sql.DB
	tableName string
}

func (s testSpace) table(context.Context) (string, error) { return s.tableName, nil }

func (s testSpace) embedQuery(_ context.Context, text string) ([]float32, error) {
	return bagOfWords(text), nil
}

func (s testSpace) store(ctx context.Context, nodeID, concept, text string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO `+s.tableName+` (id, concept, vector_field, embedding, created_at, updated_at)
VALUES ($1, $2, 'content', $3::vector, NOW(), NOW())
ON CONFLICT (id, vector_field) DO UPDATE SET embedding = EXCLUDED.embedding, concept = EXCLUDED.concept, updated_at = NOW()`,
		nodeID, concept, vectorLiteral(bagOfWords(text)))
	return err
}

func bagOfWords(text string) []float32 {
	vec := make([]float32, testDims)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(w, ".,!?")))
		vec[h.Sum32()%testDims]++
	}
	vec[testDims-1] += 0.01 // never the zero vector
	return vec
}

// failingSpace is the embedder while the model is away: the space exists, but
// nothing can be embedded, for a read or for a write.
type failingSpace struct{ testSpace }

var errModelAway = errors.New("no model is reachable right now")

func (failingSpace) embedQuery(context.Context, string) ([]float32, error) { return nil, errModelAway }

func (failingSpace) store(context.Context, string, string, string) error { return errModelAway }
```

Create `packs/gamespack/capabilities_test.go` (every declared executor has a handler, and every handler is declared):

```go
package gamespack

import (
	"io/fs"
	"regexp"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// Every builtin the DSL declares has a Go capability, and every capability
// is declared: an executor with no handler is "unknown builtin executor" at
// the first call, and a handler with no declaration is unreachable.
func TestCapabilitiesMatchTheDeclaredBuiltins(t *testing.T) {
	raw, err := fs.ReadFile(Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, m := range regexp.MustCompile(`@executor\("integration\.games\.([A-Za-z]+)"\)`).FindAllStringSubmatch(string(raw), -1) {
		declared = append(declared, m[1])
	}
	var handled []string
	for _, c := range (&Provider{}).Capabilities() {
		if c.Handler == nil {
			t.Errorf("capability %s has no handler", c.Name)
		}
		handled = append(handled, c.Name)
	}
	sort.Strings(declared)
	sort.Strings(handled)
	if len(declared) == 0 || !sameStrings(declared, handled) {
		t.Fatalf("declared executors %v != capabilities %v", declared, handled)
	}
}

func TestContractCompat(t *testing.T) {
	if err := memql.CheckPluginContractCompat(ContractVersion); err != nil {
		t.Fatal(err)
	}
}
```

Create `packs/gamespack/episodes_db_test.go`. The second test is Review Focus line 1, write half:

```go
package gamespack

import (
	"strings"
	"testing"
	"time"
)

// episode is one gamesRecordEpisodes entry, in the meadow.
func episode(id, character, player, summary string, at time.Time, importance int) map[string]any {
	e := map[string]any{
		"episodeId": id, "characterId": character, "worldId": "meadow",
		"at": rfc3339(at), "kind": "chat", "summary": summary, "importance": importance,
	}
	if player != "" {
		e["playerId"] = player
	}
	return e
}

func episodesArg(eps ...map[string]any) []any {
	out := make([]any, len(eps))
	for i, e := range eps {
		out[i] = e
	}
	return out
}

// vectorsFor counts the vectors the test space holds for these episodes.
func (lg *liveGames) vectorsFor(t *testing.T, episodeIDs ...string) int {
	t.Helper()
	ids := make([]any, len(episodeIDs))
	marks := make([]string, len(episodeIDs))
	for i, id := range episodeIDs {
		ids[i] = canonical(conceptEpisode, id)
		marks[i] = "$" + string(rune('1'+i))
	}
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+lg.table+` WHERE id IN (`+strings.Join(marks, ", ")+`)`, ids...).Scan(&n); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	return n
}

// A batch is written under the caller and every summary is embedded.
func TestLiveRecordEpisodesWritesAndEmbeds(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-episodes-" + n)
	now := time.Now().UTC()
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-garden-"+n, "wren", "", "wren planted a garden", now, 4),
		episode("e-cave-"+n, "wren", "", "wren found a cave", now, 6),
	)})
	if asInt(res[0]["written"], 0) != 2 || asInt(res[0]["embedded"], 0) != 2 || asInt(res[0]["pending"], -1) != 0 {
		t.Fatalf("answer = %v, want 2 written, 2 embedded, 0 pending", res[0])
	}
	rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": "wren", "since": rfc3339(now.Add(-time.Minute))})
	if len(rows) != 2 {
		t.Fatalf("the caller reads %d of the episodes they recorded, want 2: %v", len(rows), rows)
	}
	if got := lg.vectorsFor(t, "e-garden-"+n, "e-cave-"+n); got != 2 {
		t.Fatalf("%d vectors stored, want one per episode", got)
	}

	tooMany := make([]map[string]any, maxEpisodesPerCall+1)
	for i := range tooMany {
		tooMany[i] = episode("e-flood-"+n+"-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "wren", "", "one of too many", now, 1)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(tooMany...)}); err == nil || !strings.Contains(err.Error(), "games_invalid_argument") {
		t.Fatalf("a batch over %d must be refused with games_invalid_argument, got %v", maxEpisodesPerCall, err)
	}
}

// The model is away: the episodes are still recorded and counted pending, and
// gamesEmbedPending gives them their vectors once it is back -- a page at a
// time, saying when there is more.
func TestLiveEpisodesRecordedWhileTheModelIsAwayAreEmbeddedLater(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-pending-" + n)
	now := time.Now().UTC()
	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}

	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-bridge-"+n, "wren", "", "wren built a bridge", now, 5),
		episode("e-river-"+n, "wren", "", "wren crossed the river", now, 5),
	)})
	if asInt(res[0]["written"], 0) != 2 || asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["pending"], 0) != 2 {
		t.Fatalf("answer = %v, want 2 written, 0 embedded, 2 pending: a failed embedding is not a failed write", res[0])
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["failed"], 0) != 2 {
		t.Fatalf("still away: %v, want 0 embedded and 2 failed", res[0])
	}

	lg.provider.space = testSpace{db: lg.db.DB, tableName: lg.table}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", map[string]any{"limit": 1}); asInt(res[0]["embedded"], 0) != 1 || res[0]["more"] != true {
		t.Fatalf("first page: %v, want 1 embedded and more", res[0])
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], 0) != 1 || res[0]["more"] != false {
		t.Fatalf("second page: %v, want the last one embedded and no more", res[0])
	}
	if got := lg.vectorsFor(t, "e-bridge-"+n, "e-river-"+n); got != 2 {
		t.Fatalf("%d vectors stored after the back-fill, want 2", got)
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], -1) != 0 {
		t.Fatalf("nothing is pending any more, yet %v", res[0])
	}
}
```

Create `packs/gamespack/pack_db_test.go`:

```go
package gamespack

import "testing"

// A disabled pack is mounted-inert: its concepts load, its behaviour does not.
func TestLiveDisabledPackIsInert(t *testing.T) {
	lg := newLiveGames(t, Domain)
	if _, err := lg.call(t, asOwner("user-games-inert-"+runID()), "query", "gamesPlayers", nil); err == nil {
		t.Fatal("a query of a DISABLED pack resolved")
	}
}
```

- [ ] **Step 6: Run them to see them fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL, the package does not build:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/harness_db_test.go:45:12: undefined: Provider
packs/gamespack/harness_db_test.go:116:17: undefined: Provider
packs/gamespack/harness_db_test.go:117:15: undefined: engineStore
packs/gamespack/harness_db_test.go:134:47: undefined: compact
packs/gamespack/harness_db_test.go:174:20: undefined: vectorLiteral
packs/gamespack/capabilities_test.go:25:22: undefined: Provider
packs/gamespack/capabilities_test.go:39:44: undefined: ContractVersion
packs/gamespack/episodes_db_test.go:66:36: undefined: maxEpisodesPerCall
packs/gamespack/episodes_db_test.go:71:83: undefined: maxEpisodesPerCall
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 7: Write the store and the embedder space**

Create `packs/gamespack/store.go`. Every row is written through one of the pack's mutations under the caller's actor, because a builtin's returned nodes are never persisted:

```go
package gamespack

import (
	"context"
	"fmt"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// store.go -- how the Go half reads and writes rows: through the engine, under
// the CALLER'S actor, by rendering the pack's own constructs.
//
// A builtin's returned nodes are the expression's VALUE and are never
// persisted, so every row this pack writes goes through one of its mutations,
// which stamps ownerUserId from the actor. A row written any other way would
// be owned by nobody.

// rowStore is the read and write surface the decisions run over.
type rowStore interface {
	// query runs one of the pack's queries (or a core read the pack is
	// allowed, ladderPolicyCurrent) and returns its shaped rows.
	query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error)
	// write runs one of the pack's client-reachable mutations.
	write(ctx context.Context, name string, args map[string]any) error
	// embedStore embeds text and stores it as nodeID's vector through the one
	// embedding write path (builtin gamesEmbedStore).
	embedStore(ctx context.Context, nodeID, concept, text string) error
}

// engineStore is the production rowStore.
type engineStore struct {
	engine memql.IntegrationEngineAccess
}

func (s engineStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return nil, err
	}
	res, err := s.exec(ctx, "query "+call)
	if err != nil {
		return nil, fmt.Errorf("games: query %s: %w", name, err)
	}
	return memql.MaterializeRows(res), nil
}

func (s engineStore) write(ctx context.Context, name string, args map[string]any) error {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation %s: %w", name, err)
	}
	return nil
}

func (s engineStore) embedStore(ctx context.Context, nodeID, concept, text string) error {
	call, err := langparser.RenderCall("gamesEmbedStore", map[string]any{
		"nodeId":      nodeID,
		"text":        text,
		"concept":     concept,
		"vectorField": "content",
	})
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, "builtin "+call); err != nil {
		return fmt.Errorf("games: builtin gamesEmbedStore: %w", err)
	}
	return nil
}

func (s engineStore) exec(ctx context.Context, statement string) (*memql.ExecuteResult, error) {
	if s.engine == nil {
		return nil, fmt.Errorf("games: the pack has no engine handle on this node")
	}
	return s.engine.Execute(ctx, statement)
}

// compact drops nil values, so an omitted optional argument is absent from
// the call rather than rendered as a null the language does not spell.
func compact(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if v != nil {
			out[k] = v
		}
	}
	return out
}
```

Create `packs/gamespack/space.go`. Reads embed directly through the cluster's binding; writes dispatch through `integration.embedding.store`, the one embedding write path:

```go
package gamespack

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// space.go -- the cluster's embedder space, as this pack sees it.
//
// THE BINDING DECIDES, NEVER THIS PACK. A vector is only comparable with
// vectors of the same model and width, so every read and every write goes
// through the cluster's embedder binding (memql.CurrentEmbedderBinding) and
// its own vector table (memql.EmbeddingVectorTable). With nothing bound,
// reads degrade to no relevance and writes leave rows pending; neither picks
// a model for the owner.
//
// READS EMBED DIRECTLY, WRITES DISPATCH. The query vector is needed in Go, to
// bind into a statement, so a read resolves the provider and embeds itself,
// as integrations/harnessrecall does. A write goes through
// integration.embedding.store (builtin gamesEmbedStore), the one embedding
// write path, which caches and creates the table.

// embeddingSpace is the seam the recall, lookup and pending paths use.
type embeddingSpace interface {
	// table names the current space's vector table when it exists, or ""
	// when nothing is bound or nothing was ever stored in it.
	table(ctx context.Context) (string, error)
	// embedQuery embeds a read's text on the caller's own lane.
	embedQuery(ctx context.Context, text string) ([]float32, error)
	// store embeds text and writes it as nodeID's vector.
	store(ctx context.Context, nodeID, concept, text string) error
}

// clusterSpace is the production embeddingSpace.
type clusterSpace struct {
	resolve func(ctx context.Context, name string) (memql.EmbeddingAIProvider, error)
	rows    rowStore
	db      func() *sql.DB
}

func (c clusterSpace) table(ctx context.Context) (string, error) {
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return "", nil
	}
	table, err := memql.EmbeddingVectorTable(binding.ProviderRef, binding.Dimensions)
	if err != nil {
		return "", err
	}
	db := c.db()
	if db == nil {
		return "", fmt.Errorf("games: no database handle on this node")
	}
	tables, err := memql.EmbeddingVectorTables(ctx, db)
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		if t == table {
			return table, nil
		}
	}
	return "", nil
}

func (c clusterSpace) embedQuery(ctx context.Context, text string) ([]float32, error) {
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	if c.resolve == nil {
		return nil, fmt.Errorf("games: no embedding resolver on this node")
	}
	provider, err := c.resolve(ctx, binding.ProviderRef)
	if err != nil {
		return nil, err
	}
	vec, err := provider.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	if err := memql.ValidateEmbeddingVector(ctx, binding.ProviderRef, provider.Dimensions(), vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func (c clusterSpace) store(ctx context.Context, nodeID, concept, text string) error {
	if c.rows == nil {
		return fmt.Errorf("games: no store on this node")
	}
	return c.rows.embedStore(ctx, nodeID, concept, text)
}

// vectorLiteral formats a vector as a pgvector literal: [0.1,0.2,...].
func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = strconv.FormatFloat(float64(v), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
```

- [ ] **Step 8: Replace `pack.go` with the full provider**

Replace the whole of `packs/gamespack/pack.go` with:

```go
// Package gamespack is the games pack: memory and skill learning for a game
// runtime that runs OUTSIDE the cluster (design record
// docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// A runtime plays in real time -- reflexes every tick, a decision every few
// seconds -- and the engine is what it remembers with and what it learns
// with. The runtime executes every action itself. This package stores,
// ranks, generalizes and governs; it never runs a game action.
//
// It is a PACK in the default build, like packs/reviewspack: linked into
// every binary by packs/anchor with no build tag, declared DISABLED, and
// governed per instance by v1:platform:packState.
package gamespack

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns: concepts are v1:games:*.
const Domain = "games"

// The pack's concept ids, and the user concept ownerUserId points at.
const (
	conceptUser       = "v1:identity:user"
	conceptPlayer     = "v1:games:player"
	conceptEpisode    = "v1:games:episode"
	conceptPreference = "v1:games:preference"
	conceptPromise    = "v1:games:promise"
	conceptPlace      = "v1:games:place"
	conceptSkill      = "v1:games:skill"
	conceptPlanTrace  = "v1:games:planTrace"
)

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

// integrationName is the <name> in every @executor("integration.<name>.*").
const integrationName = "games"

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it. FALSE: the pack stores memories about the people who play,
// and holding that kind of data is a decision an owner makes, not one an
// upgrade makes for them.
const DefaultEnabled = false

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("gamespack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider.
//
// Every dependency is a seam so the decisions are testable over values and
// the engine wiring over a real database: store reads and writes rows through
// the engine under the caller's actor, space embeds and stores vectors, db is
// the raw handle the three hand-written statements need (recall, the
// near-match lookup, the pending scan) plus forget's deletes, and gates is the
// write-gate pool every ladder move is serialized on.
type Provider struct {
	store      rowStore
	space      embeddingSpace
	db         func() *sql.DB
	gates      func() *sql.DB
	invalidate func(string)
	log        *slog.Logger
	now        func() time.Time
}

// NewProvider builds the pack's IntegrationProvider from the plugin context.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	log := pctx.Logger
	if log == nil {
		log = slog.Default()
	}
	store := engineStore{engine: pctx.Engine}
	db := func() *sql.DB {
		if pctx.BunDB == nil {
			return nil
		}
		b := pctx.BunDB()
		if b == nil {
			return nil
		}
		return b.DB
	}
	return &Provider{
		store:      store,
		space:      clusterSpace{resolve: pctx.ResolveEmbeddingProvider, rows: store, db: db},
		db:         db,
		gates:      pctx.GateDB,
		invalidate: pctx.InvalidateConcept,
		log:        log,
		now:        time.Now,
	}, nil
}

// IntegrationName implements memql.IntegrationProvider.
func (p *Provider) IntegrationName() string { return integrationName }

// Register wires the pack into the engine registries. packs/anchor calls it
// once per process, before the first concept load.
//
//   - RegisterPackDefault(domain, false): the pack ships disabled; an owner
//     enables it in Cluster > Modules and every node picks it up at its next
//     restart.
//   - registerRoutingRules: the live graph's forward rules for this pack's
//     concepts, without which a row written through one replica never
//     reaches a subscriber on another (routing.go).
//   - BindPluginToPack + RegisterPluginForContract: the Go half, skipped by
//     app's materializePlugins while the pack is disabled.
//
// It does NOT call RegisterStorefrontPack: the games pack has no shopper
// surface, and only an owner may switch it on.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
	registerRoutingRules()
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}

// clock returns the provider's notion of now, in UTC.
func (p *Provider) clock() time.Time {
	if p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

// logger never returns nil.
func (p *Provider) logger() *slog.Logger {
	if p.log == nil {
		return slog.Default()
	}
	return p.log
}

// ctxDone reports a cancelled caller, so a loop over rows stops early.
func ctxDone(ctx context.Context) bool { return ctx.Err() != nil }
```

- [ ] **Step 9: Write the capabilities, the episode handlers and their builtins**

Create `packs/gamespack/capabilities.go` (later tasks append to this list):

```go
package gamespack

import "github.com/znasllc-io/memql/component/memql"

// Capabilities implements memql.IntegrationProvider. The DSL declarations in
// dsl/builtins.memql are the contract; ArgsSchema here is advisory, and
// capabilities_test.go holds the two lists equal.
func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "recordEpisodes", Description: "Record episodes and embed their summaries on the background lane.", Handler: p.recordEpisodes,
			ArgsSchema: map[string]string{"episodes": "[]object (required)"}},
		{Name: "embedPending", Description: "Embed the caller's episodes and skills that have no vector in the current embedder space.", Handler: p.embedPending,
			ArgsSchema: map[string]string{"limit": "int (optional)"}},
	}
}
```

Create `packs/gamespack/episodes.go`. A failed embedding is logged and counted pending, never a failed write; every embedding call carries `memql.ContextWithBackgroundLane`:

```go
package gamespack

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
)

// episodes.go -- gamesRecordEpisodes and gamesEmbedPending.
//
// THE EMBEDDING RIDES THE BACKGROUND LANE. A runtime records memories in
// batches, off its game loop, and nobody waits on their vectors; a decision
// is waiting on recall's query vector. Stamping the write path's context with
// memql.ContextWithBackgroundLane counts every one of these embedding calls
// against the background rate bucket (ai_guard.go), so a burst of memories
// can never spend the interactive budget a decision needs.
//
// A FAILED EMBEDDING IS NOT A FAILED WRITE. The episode is recorded first and
// stays recorded; recall ranks it by recency and importance until
// gamesEmbedPending gives it a vector.

// maxEpisodesPerCall bounds one batch: a runtime flushes every few seconds,
// and a bigger batch is a backlog that should be split, not one statement.
const maxEpisodesPerCall = 50

// maxPendingPerCall bounds one gamesEmbedPending pass.
const maxPendingPerCall = 200

func (p *Provider) recordEpisodes(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	list, ok := args["episodes"].([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("games_invalid_argument: episodes must be a non-empty list")
	}
	if len(list) > maxEpisodesPerCall {
		return nil, fmt.Errorf("games_invalid_argument: at most %d episodes per call, got %d", maxEpisodesPerCall, len(list))
	}
	writes := make([]map[string]any, 0, len(list))
	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("games_invalid_argument: episode %d is not an object", i)
		}
		w := map[string]any{
			"episodeId":   trimmed(m["episodeId"]),
			"characterId": trimmed(m["characterId"]),
			"worldId":     trimmed(m["worldId"]),
			"at":          trimmed(m["at"]),
			"kind":        trimmed(m["kind"]),
			"summary":     trimmed(m["summary"]),
			"importance":  asInt(m["importance"], 0),
		}
		for _, key := range []string{"episodeId", "characterId", "worldId", "at", "kind", "summary"} {
			if w[key] == "" {
				return nil, fmt.Errorf("games_invalid_argument: episode %d has no %s", i, key)
			}
		}
		if player := trimmed(m["playerId"]); player != "" {
			w["playerId"] = player
		}
		writes = append(writes, w)
	}
	ids := make([]string, 0, len(writes))
	for i, w := range writes {
		if err := p.store.write(ctx, "createGamesEpisode", w); err != nil {
			return nil, fmt.Errorf("games: episode %d (%s) was not recorded; the %d before it were: %w", i, w["episodeId"], i, err)
		}
		ids = append(ids, w["episodeId"].(string))
	}
	bg := memql.ContextWithBackgroundLane(ctx)
	embedded := 0
	for _, w := range writes {
		if ctxDone(ctx) {
			break
		}
		id := w["episodeId"].(string)
		if err := p.space.store(bg, canonical(conceptEpisode, id), conceptEpisode, w["summary"].(string)); err != nil {
			p.logger().Info("games: an episode was recorded without a vector; gamesEmbedPending will retry it",
				"episodeId", id, "error", err)
			continue
		}
		embedded++
	}
	return answer("recordEpisodes", map[string]any{
		"written":  len(ids),
		"embedded": embedded,
		"pending":  len(ids) - embedded,
		"ids":      ids,
	})
}

// pendingVectorsSQL lists the caller's episodes and skills with no vector in
// one vector table, newest first. %s is the table.
//
//	$1 canonical owner  $2 limit
//
// staged-data: INDIFFERENT -- it pins v1:games:episode and v1:games:skill,
// EMBEDDED pack concepts; staging withholds promoted authored concepts only
// (memql#3980), and no authored bundle can declare one in this namespace.
const pendingVectorsSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, concept, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept IN ('v1:games:episode', 'v1:games:skill')
      AND (payload->>'ownerUserId') = $1
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id, latest.concept, COALESCE(latest.payload->>'summary', latest.payload->>'goalText', '') AS text
FROM latest
WHERE %s
ORDER BY latest."createdAt" DESC
LIMIT $2
`

// pendingStatement fills the "has no vector" predicate. With no table yet,
// every row is pending.
func pendingStatement(table string) string {
	if table == "" {
		return fmt.Sprintf(pendingVectorsSQL, "TRUE")
	}
	return fmt.Sprintf(pendingVectorsSQL,
		"NOT EXISTS (SELECT 1 FROM "+table+" nv WHERE nv.id = latest.id AND nv.vector_field = 'content')")
}

func (p *Provider) embedPending(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	limit := clampInt(asInt(args["limit"], 25), 1, maxPendingPerCall)
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	table, err := p.space.table(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, pendingStatement(table), ownerKey(owner), limit)
	if err != nil {
		return nil, fmt.Errorf("games: pending vectors: %w", err)
	}
	type pending struct{ id, concept, text string }
	var todo []pending
	for rows.Next() {
		var it pending
		if err := rows.Scan(&it.id, &it.concept, &it.text); err != nil {
			rows.Close()
			return nil, fmt.Errorf("games: pending vectors scan: %w", err)
		}
		if it.text != "" {
			todo = append(todo, it)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	bg := memql.ContextWithBackgroundLane(ctx)
	embedded, failed := 0, 0
	for _, it := range todo {
		if ctxDone(ctx) {
			break
		}
		if err := p.space.store(bg, it.id, it.concept, it.text); err != nil {
			failed++
			continue
		}
		embedded++
	}
	return answer("embedPending", map[string]any{
		"embedded": embedded,
		"failed":   failed,
		"more":     len(todo) == limit,
	})
}
```

Create `packs/gamespack/dsl/builtins.memql` (later tasks append builtins):

```memql
// games pack -- the Go half (packs/gamespack). Every capability runs under the
// CALLER'S actor: a runtime and a browser both call these as the owner, and
// every row they read or write is the owner's.
//
// THE RUNTIME EXECUTES; THE ENGINE REMEMBERS, LEARNS AND GOVERNS. Nothing here
// runs a game action. The skill builtins store traces, generalize them, move
// the ladder with component/work.Advance and answer lookups; the runtime
// decides what to do with the answer.
//
// REFUSALS LEAD WITH A STABLE CODE (games_<what>), the way the router's lead
// with every_door_shut: a client matches the code, never the sentence.

/// Record a batch of episodes and embed each summary on the background lane.
@sdk
@executor("integration.games.recordEpisodes")
@args(profile="object")
@description("Record up to 50 episodes, then embed each summary into the cluster's embedder space on the background lane, so a burst of memories never takes the interactive budget a decision needs. An embedding that fails leaves its episode recorded and pending; gamesEmbedPending retries it. Answers {written, embedded, pending, ids}.")
builtin gamesRecordEpisodes {
  episodes  []object!  @description("[{episodeId, characterId, playerId?, worldId, at, kind, summary, importance}]. episodeId is the caller's stable bare id.")
}

/// Embed the caller's episodes that have no vector in the current embedder space.
@sdk
@executor("integration.games.embedPending")
@args(profile="object")
@description("Embed up to `limit` (default 25, at most 200) of the caller's episodes and skills that have no vector in the cluster's current embedder space, newest first, on the background lane. Call it when the model comes back online. Answers {embedded, failed, more}, more being true when a full page was found.")
builtin gamesEmbedPending {
  limit  int  @description("How many to embed in this call.")
}

/// The shared embedding write path, under this pack's name.
@executor("integration.embedding.store")
@args(profile="object")
@description("Store an embedding for one games row: a second DSL name for integration.embedding.store, so the pack writes vectors through the one embedding write path rather than opening a second. vectorField stays 'content', which every recall join filters on.")
builtin gamesEmbedStore {
  nodeId       string!  @description("Canonical id of the row the vector attaches to.")
  text         string!  @description("The text to embed.")
  concept      string   @description("The row's concept.")
  vectorField  string   @description("Always 'content'.")
  provider     string   @description("Omit: the cluster's embedder binding decides.")
}
```

- [ ] **Step 10: Run the unit tests, then the database tests**

Run: `go vet github.com/znasllc-io/memql/packs/gamespack/ && go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: `ok  	github.com/znasllc-io/memql/packs/gamespack` (database tests skip without a database).

Run (database container from Global Constraints): `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v -run 'TestLive' github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```text
--- PASS: TestLiveRecordEpisodesWritesAndEmbeds
--- PASS: TestLiveEpisodesRecordedWhileTheModelIsAwayAreEmbeddedLater
--- PASS: TestLiveDisabledPackIsInert
ok  	github.com/znasllc-io/memql/packs/gamespack
```
(The engine logs JSON lines at INFO while the database starts; the grep keeps the verdicts.)

- [ ] **Step 11: Record the measured embed count and lint**

```diff
diff --git a/embed_inventory_test.go b/embed_inventory_test.go
--- a/embed_inventory_test.go
+++ b/embed_inventory_test.go
@@ -114,7 +114,7 @@ var embedInventory = map[string]int{
 	"github.com/znasllc-io/memql/examples/deploypack":    4, // all:dsl (+memql.toml, the pack's language line -- memql#5357)
 	"github.com/znasllc-io/memql/examples/referencepack": 6, // all:dsl (+memql.toml, memql#5357)
 	"github.com/znasllc-io/memql/packs/reviewspack":      8, // all:dsl (memql#4139; +memql.toml, memql#5357; +queries.memql and +shapes.memql -- the reads the pack lacked, epic memql#5532). MEASURED, never derived
-	"github.com/znasllc-io/memql/packs/gamespack":        6, // all:dsl (design record 2026-10-18-games-pack-design.md): concepts, shapes, queries, mutations, memql.toml, namespace.pin. MEASURED, never derived
+	"github.com/znasllc-io/memql/packs/gamespack":        7, // all:dsl (design record 2026-10-18-games-pack-design.md): concepts, shapes, queries, mutations, builtins, memql.toml, namespace.pin. MEASURED, never derived
 	"github.com/znasllc-io/memql/packs/wholesalepack":    8, // all:dsl (epic memql#5533): concepts, shapes, queries, mutations, builtins, tools, memql.toml, namespace.pin. The BUILTIN decides and the MUTATION writes -- a builtin's returned nodes are the expression's value and are never persisted. MEASURED, never derived
 	"github.com/znasllc-io/memql/examples/shopifypack":   8, // all:dsl -- memql#4138 attach/secrets/sync (+memql.toml, memql#5357; +namespace.pin -- epic memql#5375 retired @namespace, so a pack whose namespace diverges from its directory states it in a one-line pin instead). MEASURED from the gate, never derived
 	"github.com/znasllc-io/memql/integrations":           1, // *.json
```

Run: `go test -count=1 -run TestEmbeddedFileCountsAreStable github.com/znasllc-io/memql && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `ok  	github.com/znasllc-io/memql` and `exit=0`.

- [ ] **Step 12: Commit**

```bash
git add packs/gamespack/pack.go packs/gamespack/values.go packs/gamespack/store.go packs/gamespack/space.go \
  packs/gamespack/capabilities.go packs/gamespack/episodes.go packs/gamespack/dsl/builtins.memql \
  packs/gamespack/values_test.go packs/gamespack/capabilities_test.go packs/gamespack/main_dbschema_test.go \
  packs/gamespack/harness_db_test.go packs/gamespack/episodes_db_test.go packs/gamespack/pack_db_test.go \
  embed_inventory_test.go
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): the Go half, and episodes embedded on the background lane" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 4: Recall ranked by relevance, recency and importance

One statement over `v1:games:episode`, scoped to the caller's canonical owner and to the character's own episodes or episodes about the player, scored and limited in SQL; then preferences, open promises and places. Review Focus lines 1 (read half) and 3 are pinned here.

**Files:**
- Create: `packs/gamespack/recall.go`, `packs/gamespack/recall_test.go`, `packs/gamespack/recall_db_test.go`
- Modify: `packs/gamespack/capabilities.go` (append `recall`), `packs/gamespack/dsl/builtins.memql` (append `gamesRecall`)

**Interfaces:**
- Consumes: Task 3's `Provider`, `requireActor`, `ownerKey`, `canonical`, `bare`, `bareRow`, `clampInt`, `asInt`, `asFloat`, `timeOf`, `rfc3339`, `vectorLiteral`, `embeddingSpace`, `rowStore.query`; queries `gamesPreferencesForPlayer`, `gamesOpenPromises`, `gamesPlacesInWorld` (Task 1); test harness and `failingSpace` (Task 3).
- Produces: `type recallWeights struct{ relevance, recency, importance, halfLifeHours float64 }`, `defaultRecallWeights() recallWeights` (0.5, 0.3, 0.2, 72), `recallScore(relevance, ageHours, importance float64, w recallWeights) float64`, `recallEpisodesSQL`, `recallStatement(table string) string` (no join and relevance `0::float8` when `table == ""`), `type recallItem struct{ Rank int; Kind, ID, Text string; Score, Relevance, Recency, Importance float64; RelevanceAvailable bool; Row map[string]any }` (JSON names per spec 5.5), `(*Provider).recall`, `firstN(rows, n)`. Every answer row carries a bare `id` and `rank` (1 first). Test helper `byRank(items) []map[string]any`.

- [ ] **Step 1: Write the failing tests**

Create `packs/gamespack/recall_test.go`:

```go
package gamespack

import (
	"math"
	"strings"
	"testing"
)

// The runtime contract's own vectors for the score.
func TestRecallScoreIsTheContractFormula(t *testing.T) {
	w := defaultRecallWeights()
	cases := []struct {
		name                        string
		relevance, ageH, importance float64
		want                        float64
	}{
		{"relevant, new, important", 1, 0, 10, 1.0},
		{"irrelevant, one half-life old, importance 0", 0, 72, 0, 0.15},
		{"negative cosine counts as zero", -0.7, 0, 0, 0.3},
		{"future episode counts as new", 0, -5, 0, 0.3},
	}
	for _, c := range cases {
		if got := recallScore(c.relevance, c.ageH, c.importance, w); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: recallScore = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRecallStatementHasNoRelevanceWithoutAVectorTable(t *testing.T) {
	plain := recallStatement("")
	if strings.Contains(plain, "$10") || strings.Contains(plain, "JOIN") {
		t.Fatalf("the no-relevance statement must not bind a vector or join a table:\n%s", plain)
	}
	joined := recallStatement("node_vectors_8_0011223344556677")
	if !strings.Contains(joined, "LEFT JOIN node_vectors_8_0011223344556677 nv") || !strings.Contains(joined, "$10::vector") {
		t.Fatalf("the joined statement must LEFT JOIN the table and bind the query vector:\n%s", joined)
	}
}
```

Create `packs/gamespack/recall_db_test.go`. `TestLiveRecallWhileTheModelIsAwayStillAnswers` is Review Focus line 1: with the model there, the relevant old episode wins; with it away, the recent one does, and every row says relevance was unavailable. `TestLiveEpisodesAreTheOwnersAlone` is line 3, and its first assertion is the positive control: without it, "owner B sees nothing" would also pass for a statement that matches nobody:

```go
package gamespack

import (
	"math"
	"sort"
	"testing"
	"time"
)

// byRank orders recall rows by their rank, which is the contract; the order
// rows arrive in is the transport's.
func byRank(items []map[string]any) []map[string]any {
	out := append([]map[string]any(nil), items...)
	sort.SliceStable(out, func(i, j int) bool { return asInt(out[i]["rank"], 0) < asInt(out[j]["rank"], 0) })
	return out
}

// recordEpisodes writes and embeds; recall ranks by the contract formula and
// scopes to the character's own episodes plus episodes about the player.
func TestLiveRecallRanksAndScopes(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-recall-" + n
	ctx := asOwner(owner)
	now := time.Now().UTC().Truncate(time.Second)
	player := "p-sam-" + n

	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Sam" + n, "displayName": "Sam", "consented": true})
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-diamonds-"+n, "wren", player, "Sam found diamonds in the deep cave", now.Add(-1*time.Hour), 8),
		episode("e-bread-"+n, "wren", player, "Sam ate some bread at the farm", now.Add(-200*time.Hour), 2),
		episode("e-otis-"+n, "otis", player, "Sam showed Otis the diamonds", now.Add(-2*time.Hour), 5),
		episode("e-other-"+n, "otis", "", "Otis watched the sunset alone", now.Add(-1*time.Hour), 9),
	)})
	if got := asInt(res[0]["written"], 0); got != 4 {
		t.Fatalf("written = %d, want 4: %v", got, res[0])
	}
	if got := asInt(res[0]["embedded"], 0); got != 4 {
		t.Fatalf("embedded = %d, want 4: %v", got, res[0])
	}

	items := lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "playerId": player, "worldId": "meadow",
		"text": "diamonds cave", "asOf": rfc3339(now), "k": 8,
	})
	var episodes []map[string]any
	for _, it := range byRank(items) {
		if it["kind"] == "episode" {
			episodes = append(episodes, it)
		}
	}
	if len(episodes) != 3 {
		t.Fatalf("recall returned %d episodes, want wren's two plus otis's one about Sam (not otis's sunset): %v", len(episodes), episodes)
	}
	if episodes[0]["text"] != "Sam found diamonds in the deep cave" {
		t.Fatalf("the relevant, recent, important episode must rank first, got %v", episodes[0]["text"])
	}
	for i := 1; i < len(episodes); i++ {
		if asFloat(episodes[i-1]["score"], 0) < asFloat(episodes[i]["score"], 0) {
			t.Fatalf("episodes are not in score order: %v", episodes)
		}
	}
	// The statement and the formula agree.
	for _, ep := range episodes {
		row := asObject(ep["row"])
		age := now.Sub(timeOf(row["at"])).Hours()
		want := recallScore(asFloat(ep["relevance"], 0), age, asFloat(row["importance"], 0), defaultRecallWeights())
		if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
			t.Fatalf("score %v disagrees with recallScore %v for %v", ep["score"], want, ep["text"])
		}
		if ep["relevanceAvailable"] != true {
			t.Fatalf("relevance was available for %v", ep["text"])
		}
	}
}

// With no vector table the answer is still ranked, by recency and importance.
func TestLiveRecallWithoutAnEmbedderStillAnswers(t *testing.T) {
	lg := newLiveGames(t)
	lg.provider.space = testSpace{db: lg.db.DB, tableName: ""}
	n := runID()
	ctx := asOwner("user-games-norel-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-old-"+n, "wren", "", "an old memory", now.Add(-300*time.Hour), 9),
		episode("e-new-"+n, "wren", "", "a new memory", now.Add(-1*time.Hour), 3),
	)})
	items := lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "anything", "asOf": rfc3339(now),
	})
	items = byRank(items)
	if len(items) < 2 || items[0]["text"] != "a new memory" || items[0]["relevanceAvailable"] != false {
		t.Fatalf("without an embedder recall must rank by recency and importance and say relevance was unavailable: %v", items)
	}
}

// The embedder is bound but the model is away: recall still answers, ranked
// by recency and importance, and says relevance was unavailable. The same
// episodes rank the other way round while the model is there.
func TestLiveRecallWhileTheModelIsAwayStillAnswers(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-away-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-cave-"+n, "wren", "", "wren explored a cave", now.Add(-200*time.Hour), 9),
		episode("e-farm-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	recallArgs := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now)}

	there := byRank(lg.must(t, ctx, "builtin", "gamesRecall", recallArgs))
	if len(there) < 2 || there[0]["id"] != "e-cave-"+n || there[0]["relevanceAvailable"] != true {
		t.Fatalf("with the model there, the relevant episode must rank first: %v", there)
	}

	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}
	away := byRank(lg.must(t, ctx, "builtin", "gamesRecall", recallArgs))
	if len(away) < 2 || away[0]["id"] != "e-farm-"+n {
		t.Fatalf("with the model away, recency and importance must decide: %v", away)
	}
	for _, it := range away {
		if it["kind"] == "episode" && (it["relevanceAvailable"] != false || asFloat(it["relevance"], -1) != 0) {
			t.Fatalf("with the model away every episode must say relevance was unavailable: %v", it)
		}
	}
}

func TestLiveEpisodesAreTheOwnersAlone(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	a, b := asOwner("user-games-a-"+n), asOwner("user-games-b-"+n)
	lg.must(t, a, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-a-"+n, "wren", "", "a secret", time.Now().UTC(), 5))})
	countEpisodes := func(items []map[string]any) int {
		n := 0
		for _, it := range items {
			if it["kind"] == "episode" {
				n++
			}
		}
		return n
	}
	recallArgs := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "a secret"}
	if got := countEpisodes(lg.must(t, a, "builtin", "gamesRecall", recallArgs)); got != 1 {
		t.Fatalf("owner A recalled %d of their own episodes, want 1 -- without this the next check proves nothing", got)
	}
	if got := countEpisodes(lg.must(t, b, "builtin", "gamesRecall", recallArgs)); got != 0 {
		t.Fatalf("owner B recalled %d of owner A's episodes", got)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/recall_db_test.go:67:11: undefined: recallScore
packs/gamespack/recall_db_test.go:67:88: undefined: defaultRecallWeights
packs/gamespack/recall_test.go:11:7: undefined: defaultRecallWeights
packs/gamespack/recall_test.go:23:13: undefined: recallScore
packs/gamespack/recall_test.go:30:11: undefined: recallStatement
packs/gamespack/recall_test.go:34:12: undefined: recallStatement
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write `recall.go`**

The statement's parameters are `$1` canonical owner, `$2` character, `$3` canonical player, `$4` as-of time, `$5` half-life, `$6`-`$8` weights, `$9` k, `$10` the query vector; the join is a LEFT JOIN, so an episode with no vector scores relevance 0 and still ranks. A failure to resolve the space or embed the query degrades to the no-relevance statement rather than failing:

```go
package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// recall.go -- gamesRecall: what one character remembers that matters now.
//
// A PACK BUILTIN, NOT A CHANGE TO recall (integrations/harnessrecall). The
// score has a term recall cannot know -- importance, a field of THIS pack's
// concept -- and the scope is two of this pack's fields (the character's own
// episodes, and episodes about the player being addressed). Teaching the core
// builtin payload filters and a third weight is a platform design of its own,
// with callers in product bundles this repository cannot see; this one
// statement is the same shape as harnessrecall's, over one concept the pack
// owns.
//
// ONE STATEMENT, SCORED AND LIMITED IN SQL, because the 30-day window can hold
// tens of thousands of episodes and a decision waits on this read. The score
// is recallScore below, which the database test holds the statement to:
//
//	score = wRelevance x max(cosine, 0) + wRecency x 2^(-ageHours/halfLife) + wImportance x importance/10
//
// NO EMBEDDER, NO MODEL, STILL AN ANSWER. Relevance is a LEFT JOIN: an episode
// with no vector, a space nothing was ever stored in, or a query the model
// could not embed scores 0 relevance and is still ranked by recency and
// importance, and every row says whether relevance was available.

const ln2 = 0.6931471805599453

// recallWeights are the score's tunables. The defaults are the runtime
// contract's numbers.
type recallWeights struct {
	relevance     float64
	recency       float64
	importance    float64
	halfLifeHours float64
}

func defaultRecallWeights() recallWeights {
	return recallWeights{relevance: 0.5, recency: 0.3, importance: 0.2, halfLifeHours: 72}
}

// recallScore is the score the statement computes, as a function over values.
// A negative cosine counts as 0, a negative age as 0, and a non-positive
// half-life turns the recency term off.
func recallScore(relevance, ageHours, importance float64, w recallWeights) float64 {
	if relevance < 0 {
		relevance = 0
	}
	if ageHours < 0 {
		ageHours = 0
	}
	recency := 0.0
	if w.halfLifeHours > 0 {
		recency = math.Exp(-ln2 * ageHours / w.halfLifeHours)
	}
	return w.relevance*relevance + w.recency*recency + w.importance*importance/10
}

// recallEpisodesSQL ranks one owner's episodes for one character.
//
//	$1 canonical owner  $2 characterId  $3 canonical playerId ('' = nobody)
//	$4 now  $5 halfLifeHours  $6 wRelevance  $7 wRecency  $8 wImportance  $9 k
//	$10 query vector (the joined variant only)
//
// %[1]s is the relevance expression and %[2]s the join, filled by
// recallStatement.
//
// staged-data: INDIFFERENT -- the statement pins v1:games:episode, an EMBEDDED
// pack concept. Staged data is the tier that withholds a PROMOTED authored
// concept's rows (memql#3980); no authored bundle can declare a concept in a
// pack's namespace, so there is no staged row of this concept for a gate to
// withhold, and none it could wrongly drop.
const recallEpisodesSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:games:episode'
      AND (payload->>'ownerUserId') = $1
      AND ((payload->>'characterId') = $2 OR ($3 <> '' AND (payload->>'playerId') = $3))
    ORDER BY id, "createdAt" DESC
),
scored AS (
    SELECT latest.id, latest."createdAt", latest.payload,
           %[1]s AS relevance,
           exp(-0.6931471805599453 * GREATEST(extract(epoch FROM ($4::timestamptz - (latest.payload->>'at')::timestamptz)), 0) / (3600.0 * $5)) AS recency,
           COALESCE((latest.payload->>'importance')::float8, 1) / 10.0 AS importance
    FROM latest
    %[2]s
)
SELECT id, payload, relevance, recency, importance,
       $6 * relevance + $7 * recency + $8 * importance AS score
FROM scored
ORDER BY score DESC, "createdAt" DESC
LIMIT $9
`

// recallStatement fills the relevance expression and the join for a vector
// table, or the no-relevance form when there is none.
func recallStatement(table string) string {
	if table == "" {
		return fmt.Sprintf(recallEpisodesSQL, "0::float8", "")
	}
	return fmt.Sprintf(recallEpisodesSQL,
		"GREATEST(COALESCE(1 - (nv.embedding <=> $10::vector), 0), 0)",
		"LEFT JOIN "+table+" nv ON nv.id = latest.id AND nv.vector_field = 'content'")
}

// recallItem is one row of the answer.
type recallItem struct {
	Rank               int            `json:"rank"`
	Kind               string         `json:"kind"`
	ID                 string         `json:"id"`
	Text               string         `json:"text"`
	Score              float64        `json:"score"`
	Relevance          float64        `json:"relevance"`
	Recency            float64        `json:"recency"`
	Importance         float64        `json:"importance"`
	RelevanceAvailable bool           `json:"relevanceAvailable"`
	Row                map[string]any `json:"row"`
}

func (p *Provider) recall(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	characterID := trimmed(args["characterId"])
	worldID := trimmed(args["worldId"])
	text := trimmed(args["text"])
	if characterID == "" || worldID == "" || text == "" {
		return nil, fmt.Errorf("games_invalid_argument: characterId, worldId and text are required")
	}
	playerBare := bare(trimmed(args["playerId"]))
	k := clampInt(asInt(args["k"], 8), 1, 50)
	w := defaultRecallWeights()
	w.relevance = asFloat(args["wRelevance"], w.relevance)
	w.recency = asFloat(args["wRecency"], w.recency)
	w.importance = asFloat(args["wImportance"], w.importance)
	w.halfLifeHours = asFloat(args["halfLifeHours"], w.halfLifeHours)
	now := p.clock()
	if t := timeOf(args["asOf"]); !t.IsZero() {
		now = t
	}

	items, err := p.recallEpisodes(ctx, owner, characterID, canonical(conceptPlayer, playerBare), text, now, w, k)
	if err != nil {
		return nil, err
	}
	if playerBare != "" {
		prefs, err := p.store.query(ctx, "gamesPreferencesForPlayer", map[string]any{"playerId": playerBare})
		if err != nil {
			return nil, err
		}
		for _, r := range firstN(prefs, clampInt(asInt(args["preferences"], 3), 0, 20)) {
			items = append(items, recallItem{Kind: "preference", ID: bare(trimmed(r["id"])), Text: trimmed(r["subject"]), Score: asFloat(r["confidence"], 0), Row: bareRow(r)})
		}
		promises, err := p.store.query(ctx, "gamesOpenPromises", map[string]any{"characterId": characterID, "playerId": playerBare})
		if err != nil {
			return nil, err
		}
		for _, r := range firstN(promises, clampInt(asInt(args["promises"], 5), 0, 20)) {
			items = append(items, recallItem{Kind: "promise", ID: bare(trimmed(r["id"])), Text: trimmed(r["text"]), Row: bareRow(r)})
		}
	}
	places, err := p.store.query(ctx, "gamesPlacesInWorld", map[string]any{"worldId": worldID})
	if err != nil {
		return nil, err
	}
	for _, r := range firstN(places, clampInt(asInt(args["places"], 10), 0, 50)) {
		items = append(items, recallItem{Kind: "place", ID: bare(trimmed(r["id"])), Text: trimmed(r["name"]), Row: bareRow(r)})
	}
	return recallNodes(items)
}

// recallEpisodes runs the one statement. The query vector is embedded on the
// caller's own lane: a decision is waiting on it.
func (p *Provider) recallEpisodes(ctx context.Context, owner, characterID, playerID, text string, now time.Time, w recallWeights, k int) ([]recallItem, error) {
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	table, err := p.space.table(ctx)
	if err != nil {
		p.logger().Warn("games: recall could not resolve the embedder space; ranking without relevance", "error", err)
		table = ""
	}
	var vec []float32
	if table != "" {
		if vec, err = p.space.embedQuery(ctx, text); err != nil {
			p.logger().Info("games: recall could not embed its query; ranking without relevance", "error", err)
			table = ""
		}
	}
	params := []any{ownerKey(owner), characterID, playerID, rfc3339(now), w.halfLifeHours, w.relevance, w.recency, w.importance, k}
	if table != "" {
		params = append(params, vectorLiteral(vec))
	}
	rows, err := db.QueryContext(ctx, recallStatement(table), params...)
	if err != nil {
		return nil, fmt.Errorf("games: recall: %w", err)
	}
	defer rows.Close()
	return scanRecall(rows, table != "")
}

func scanRecall(rows *sql.Rows, relevanceAvailable bool) ([]recallItem, error) {
	var out []recallItem
	for rows.Next() {
		var (
			id                                    string
			raw                                   json.RawMessage
			relevance, recency, importance, score float64
		)
		if err := rows.Scan(&id, &raw, &relevance, &recency, &importance, &score); err != nil {
			return nil, fmt.Errorf("games: recall scan: %w", err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("games: recall row %s: %w", id, err)
		}
		row["id"] = bare(id)
		out = append(out, recallItem{
			Kind: "episode", ID: bare(id), Text: trimmed(row["summary"]), Score: score,
			Relevance: relevance, Recency: recency, Importance: importance,
			RelevanceAvailable: relevanceAvailable, Row: row,
		})
	}
	return out, rows.Err()
}

// recallNodes renders the items as the builtin's rows. Each carries its
// RANK, 1 first, and a client orders by it: the order rows arrive in is the
// transport's, and the rank is the answer's.
func recallNodes(items []recallItem) ([]memorynodes.MemoryNode, error) {
	out := make([]memorynodes.MemoryNode, 0, len(items))
	now := time.Now().UTC()
	for i, it := range items {
		it.Rank = i + 1
		raw, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        it.ID,
			Concept:   "integration:games:memoryItem",
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: now.Add(-time.Duration(i) * time.Microsecond),
			Payload:   raw,
		})
	}
	return out, nil
}

func firstN(rows []map[string]any, n int) []map[string]any {
	if n <= 0 {
		return nil
	}
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}
```

- [ ] **Step 4: Declare the capability and the builtin**

```diff
diff --git a/packs/gamespack/capabilities.go b/packs/gamespack/capabilities.go
--- a/packs/gamespack/capabilities.go
+++ b/packs/gamespack/capabilities.go
@@ -11,5 +11,7 @@ func (p *Provider) Capabilities() []memql.IntegrationCapability {
 			ArgsSchema: map[string]string{"episodes": "[]object (required)"}},
 		{Name: "embedPending", Description: "Embed the caller's episodes and skills that have no vector in the current embedder space.", Handler: p.embedPending,
 			ArgsSchema: map[string]string{"limit": "int (optional)"}},
+		{Name: "recall", Description: "Rank one character's episodes by relevance, recency and importance, then append preferences, open promises and places.", Handler: p.recall, PreserveOrder: true,
+			ArgsSchema: map[string]string{"characterId": "string (required)", "playerId": "string", "worldId": "string (required)", "text": "string (required)", "k": "int"}},
 	}
 }
```

(`PreserveOrder: true` keeps the engine from re-sorting the answer by node time.)

```diff
diff --git a/packs/gamespack/dsl/builtins.memql b/packs/gamespack/dsl/builtins.memql
--- a/packs/gamespack/dsl/builtins.memql
+++ b/packs/gamespack/dsl/builtins.memql
@@ -39,3 +39,24 @@ builtin gamesEmbedStore {
   vectorField  string   @description("Always 'content'.")
   provider     string   @description("Omit: the cluster's embedder binding decides.")
 }
+
+/// Recall what one character remembers that matters now.
+@sdk
+@executor("integration.games.recall")
+@args(profile="object")
+@description("Rank the caller's episodes for one character by 0.5 x relevance (cosine to the embedded query text, negative counted as 0) + 0.3 x recency (half-life 72 hours from the episode's `at`) + 0.2 x importance/10, over episodes that are the character's own or about the player, and return the top k; then the player's most confident preferences, the character's open promises to the player and the world's places. With no embedder or no model the relevance term is 0 for every episode and each row says relevanceAvailable false. Answers rows {rank, kind: episode|preference|promise|place, id, text, score, relevance, recency, importance, relevanceAvailable, row}; order them by rank, 1 first.")
+builtin gamesRecall {
+  characterId    string!  @description("The character recalling.")
+  playerId       string   @description("The player being addressed. Omit when nobody is.")
+  worldId        string!  @description("The world whose places to include.")
+  text           string!  @description("What to recall about: the situation or what the player said.")
+  k              int      @description("Episodes to return. Default 8, at most 50.")
+  asOf           datetime @description("The game's present moment, which recency measures from. Default: the server's clock. (Not `now`: that name is reserved.)")
+  halfLifeHours  number   @description("Recency half-life in hours. Default 72.")
+  wRelevance     number   @description("Relevance weight. Default 0.5.")
+  wRecency       number   @description("Recency weight. Default 0.3.")
+  wImportance    number   @description("Importance weight. Default 0.2.")
+  preferences    int      @description("Most confident preferences to append. Default 3.")
+  promises       int      @description("Open promises to append. Default 5.")
+  places         int      @description("Places to append. Default 10.")
+}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/ && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v -run 'Recall|OwnersAlone' github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```text
--- PASS: TestLiveRecallRanksAndScopes
--- PASS: TestLiveRecallWithoutAnEmbedderStillAnswers
--- PASS: TestLiveRecallWhileTheModelIsAwayStillAnswers
--- PASS: TestLiveEpisodesAreTheOwnersAlone
--- PASS: TestRecallScoreIsTheContractFormula
--- PASS: TestRecallStatementHasNoRelevanceWithoutAVectorTable
ok  	github.com/znasllc-io/memql/packs/gamespack
```

- [ ] **Step 6: Commit**

```bash
git add packs/gamespack/recall.go packs/gamespack/recall_test.go packs/gamespack/recall_db_test.go \
  packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): recall ranked by relevance, recency and importance" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 5: Forget a player outright

A hard delete in one transaction, scoped to the caller's canonical owner: the player, every version of every episode, preference and promise about them, every plan trace mentioning their gamertag, and their vectors in every embedder table. Skills are kept.

**Files:**
- Create: `packs/gamespack/forget.go`, `packs/gamespack/forget_db_test.go`
- Modify: `packs/gamespack/capabilities.go` (append `forgetPlayer`), `packs/gamespack/dsl/builtins.memql` (append `gamesForgetPlayer`)

**Interfaces:**
- Consumes: Task 3's `Provider` (`db`, `invalidate`), `requireActor`, `ownerKey`, `canonical`, `bare`, `answer`; query `gamesPlayerById`; `memql.ContextWithFreshRead`, `memql.EmbeddingVectorTables`.
- Produces: `forgetByPlayerSQL`, `forgetTracesSQL`, `forgetPlayerRowSQL` (each `staged-data: MUST-NOT-GATE`), `(*Provider).forgetPlayer` (refuses `games_player_not_found` unless the player is the caller's), `deleteVectors(ctx, tx, ids map[string]bool) (int, error)`, `isNoRows(error) bool` (Task 9 uses it). Answer `{playerId, deleted: {episodes, preferences, promises, planTraces, players, vectors}}`.

- [ ] **Step 1: Write the failing test**

Create `packs/gamespack/forget_db_test.go`:

```go
package gamespack

import (
	"testing"
	"time"
)

func TestLiveForgetPlayerDeletesEverythingAboutThemAndNothingElse(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-" + n
	ctx := asOwner(owner)
	now := time.Now().UTC()
	sam, jo := "p-sam-"+n, "p-jo-"+n
	for _, p := range []string{sam, jo} {
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": p, "gamertag": "Tag" + p, "displayName": p, "consented": true})
		lg.must(t, ctx, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": "pref-" + p, "playerId": p, "subject": "diamonds", "valence": 0.9, "confidence": 0.7})
		lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{"promiseId": "prom-" + p, "characterId": "wren", "playerId": p, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
		lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-"+p, "wren", p, "played with "+p, now, 4))})
	}
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": sam})
	deleted := asObject(res[0]["deleted"])
	for _, k := range []string{"episodes", "preferences", "promises", "players"} {
		if asInt(deleted[k], 0) != 1 {
			t.Fatalf("forget deleted %v of %s, want 1: %v", deleted[k], k, deleted)
		}
	}
	if asInt(deleted["vectors"], 0) != 1 {
		t.Fatalf("forget must delete the episode's vector: %v", deleted)
	}
	if rows := lg.must(t, ctx, "query", "gamesPlayerById", map[string]any{"playerId": sam}); len(rows) != 0 {
		t.Fatalf("the forgotten player is still readable: %v", rows)
	}
	if rows := lg.must(t, ctx, "query", "gamesEpisodesForPlayer", map[string]any{"playerId": jo}); len(rows) != 1 {
		t.Fatalf("forgetting Sam touched Jo: %v", rows)
	}
	if _, err := lg.call(t, asOwner("user-games-stranger-"+n), "builtin", "gamesForgetPlayer", map[string]any{"playerId": jo}); err == nil {
		t.Fatal("another owner forgot Jo: forget must reach only the caller's own players")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

The file compiles (it calls the builtin by name), so the failure is the engine's:

Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -run TestLiveForgetPlayer github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -v '^{"time"'`
Expected:

```text
--- FAIL: TestLiveForgetPlayerDeletesEverythingAboutThemAndNothingElse (5.50s)
    forget_db_test.go:21: builtin gamesForgetPlayer: function "gamesForgetPlayer" not found
FAIL
FAIL	github.com/znasllc-io/memql/packs/gamespack	5.684s
FAIL
```

- [ ] **Step 3: Write `forget.go`**

```go
package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
)

// forget.go -- gamesForgetPlayer: delete, outright, what this owner holds
// about one player.
//
// A DELETE, NOT A SOFT DELETE OR A SCRUB. The obligation "forget this player"
// is that the rows are gone, in every version, together with the vectors
// computed from them. MemQL has no delete mutation, so this is raw SQL, the
// way the Shopify connector's shop/redact purge is (integrations/shopify
// compliance.go), and every statement is scoped to the CALLER'S ownerUserId:
// a cluster owner who can read another owner's player still deletes nothing
// of theirs.
//
// WHAT GOES: the player row; every episode, preference and promise whose
// playerId is the player; every plan trace whose text mentions the player's
// gamertag (a trace carries no player id, but its arguments carry names); and
// the vectors of every deleted id, in every embedder space. WHAT STAYS:
// skills. A skill is a procedure, not a memory of a person; a constant inside
// one that names the player is a known limit the design record states.
//
// NO graph.node.deleted EVENT IS PUBLISHED, as for every delete in this
// engine; a live view drops the rows when it re-reads, which a client does
// after calling this.

// The deletes, each scoped to (concept, owner) and RETURNING the ids gone.
//
// staged-data: MUST-NOT-GATE -- a row a staged-data gate hid would SURVIVE
// the forget, and the obligation is that the player's data is gone (the
// argument component/identity/authactivity's prune and the Shopify purge make).
const (
	forgetByPlayerSQL = `DELETE FROM "MemoryNodes"
WHERE concept = $1 AND (payload->>'ownerUserId') = $2 AND (payload->>'playerId') = $3
RETURNING id`
	forgetTracesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:planTrace' AND (payload->>'ownerUserId') = $1
  AND strpos(lower(payload::text), lower($2)) > 0
RETURNING id`
	forgetPlayerRowSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:player' AND id = $1 AND (payload->>'ownerUserId') = $2
RETURNING id`
)

func (p *Provider) forgetPlayer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	playerBare := bare(trimmed(args["playerId"]))
	if playerBare == "" {
		return nil, fmt.Errorf("games_invalid_argument: playerId is required")
	}
	rows, err := p.store.query(memql.ContextWithFreshRead(ctx), "gamesPlayerById", map[string]any{"playerId": playerBare})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || ownerKey(trimmed(rows[0]["ownerUserId"])) != ownerKey(owner) {
		return nil, fmt.Errorf("games_player_not_found: player %q is not one of this owner's players", playerBare)
	}
	gamertag := trimmed(rows[0]["gamertag"])
	playerID := canonical(conceptPlayer, playerBare)

	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("games: forget: %w", err)
	}
	defer tx.Rollback()

	// The answer names what went in the words a person reads, not concept ids.
	key := map[string]string{
		conceptEpisode: "episodes", conceptPreference: "preferences", conceptPromise: "promises",
		conceptPlanTrace: "planTraces", conceptPlayer: "players",
	}
	deleted := map[string]int{}
	gone := map[string]bool{}
	collect := func(concept string, rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		ids := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids[id] = true
		}
		deleted[key[concept]] = len(ids)
		for id := range ids {
			gone[id] = true
		}
		return rows.Err()
	}
	for _, concept := range []string{conceptEpisode, conceptPreference, conceptPromise} {
		r, qerr := tx.QueryContext(ctx, forgetByPlayerSQL, concept, ownerKey(owner), playerID)
		if err := collect(concept, r, qerr); err != nil {
			return nil, fmt.Errorf("games: forget %s: %w", concept, err)
		}
	}
	if gamertag != "" {
		r, qerr := tx.QueryContext(ctx, forgetTracesSQL, ownerKey(owner), gamertag)
		if err := collect(conceptPlanTrace, r, qerr); err != nil {
			return nil, fmt.Errorf("games: forget %s: %w", conceptPlanTrace, err)
		}
	}
	r, qerr := tx.QueryContext(ctx, forgetPlayerRowSQL, playerID, ownerKey(owner))
	if err := collect(conceptPlayer, r, qerr); err != nil {
		return nil, fmt.Errorf("games: forget %s: %w", conceptPlayer, err)
	}

	vectors, err := deleteVectors(ctx, tx, gone)
	if err != nil {
		return nil, fmt.Errorf("games: forget vectors: %w", err)
	}
	deleted["vectors"] = vectors
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("games: forget: %w", err)
	}
	if p.invalidate != nil {
		for _, concept := range []string{conceptEpisode, conceptPreference, conceptPromise, conceptPlanTrace, conceptPlayer} {
			p.invalidate(concept)
		}
	}
	out := map[string]any{}
	for k, v := range deleted {
		out[k] = v
	}
	return answer("forgetPlayer", map[string]any{"playerId": playerBare, "deleted": out})
}

// deleteVectors removes the vectors of every id, in every embedder space.
// Explicit placeholder lists rather than an array parameter
// (component/identity/authactivity's reason: bun's pgdriver does not encode a
// bare []string as text[], and a wrong encoding would delete nothing while
// reporting success).
func deleteVectors(ctx context.Context, tx *sql.Tx, ids map[string]bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tables, err := memql.EmbeddingVectorTables(ctx, tx)
	if err != nil {
		return 0, err
	}
	list := make([]any, 0, len(ids))
	marks := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
		marks = append(marks, fmt.Sprintf("$%d", len(list)))
	}
	total := 0
	for _, table := range tables {
		res, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN ("+strings.Join(marks, ",")+")", list...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// isNoRows reports database/sql's empty single-row answer.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
```

- [ ] **Step 4: Declare the capability and the builtin**

```diff
diff --git a/packs/gamespack/capabilities.go b/packs/gamespack/capabilities.go
--- a/packs/gamespack/capabilities.go
+++ b/packs/gamespack/capabilities.go
@@ -13,5 +13,7 @@ func (p *Provider) Capabilities() []memql.IntegrationCapability {
 			ArgsSchema: map[string]string{"limit": "int (optional)"}},
 		{Name: "recall", Description: "Rank one character's episodes by relevance, recency and importance, then append preferences, open promises and places.", Handler: p.recall, PreserveOrder: true,
 			ArgsSchema: map[string]string{"characterId": "string (required)", "playerId": "string", "worldId": "string (required)", "text": "string (required)", "k": "int"}},
+		{Name: "forgetPlayer", Description: "Delete every row the caller holds about one player.", Handler: p.forgetPlayer,
+			ArgsSchema: map[string]string{"playerId": "string (required)"}},
 	}
 }
```

```diff
diff --git a/packs/gamespack/dsl/builtins.memql b/packs/gamespack/dsl/builtins.memql
--- a/packs/gamespack/dsl/builtins.memql
+++ b/packs/gamespack/dsl/builtins.memql
@@ -60,3 +60,12 @@ builtin gamesRecall {
   promises       int      @description("Open promises to append. Default 5.")
   places         int      @description("Places to append. Default 10.")
 }
+
+/// Delete every row this owner holds about one player.
+@sdk
+@executor("integration.games.forgetPlayer")
+@args(profile="object")
+@description("Delete, outright and in every version, the player row and every episode, preference and promise about the player, plus their vectors, and every plan trace whose text mentions the player's gamertag. Skills are kept. Only the caller's own rows. Answers {playerId, deleted: {episodes, preferences, promises, planTraces, players, vectors}}.")
+builtin gamesForgetPlayer {
+  playerId  string!  @description("The player to forget.")
+}
```

- [ ] **Step 5: Run it to see it pass**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/ && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v -run TestLiveForgetPlayer github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: `--- PASS: TestLiveForgetPlayerDeletesEverythingAboutThemAndNothingElse` and `ok`.

- [ ] **Step 6: Commit**

```bash
git add packs/gamespack/forget.go packs/gamespack/forget_db_test.go packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): forget a player outright" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 6: Goal signatures and skill templates

Pure functions over values, no database: the signature the runtime also computes offline (spec 5.3 vectors), and the template the engine learns. Review Focus line 4 (unit half) is pinned here.

**Files:**
- Create: `packs/gamespack/signature.go`, `packs/gamespack/template.go`, `packs/gamespack/signature_test.go`, `packs/gamespack/template_test.go`

**Interfaces:**
- Consumes: `work.NormalizeStatement(string) string` (`component/work/compile.go:78`); `canonical`, `conceptUser`, `sameStrings` (Task 3).
- Produces: `GoalSignature(goalText string, inputKeys []string) string` (`hex(sha256(NormalizeStatement(goal) + "|" + join(sort(trimmed non-empty keys), ",")))`), `inputKeys(map[string]any) []string`; `type PlanStep struct{ Action string; Args map[string]any }`, `type ArgSpec struct{ Hole string; Const any }` (JSON `{"hole": ...}` or `{"const": ...}`), `type TemplateStep struct{ Action string; Args map[string]ArgSpec }`, `type Template struct{ Steps []TemplateStep; Holes []string }`, `HoleID(stepIndex int, argKey string) string` (`"<i>.<key>"`), `Actions([]PlanStep) []string`, `Generalize([][]PlanStep) (Template, error)` (two or more plans, same actions and key sets; JSON-equal arguments stay constants), `Bind(Template, []PlanStep) (map[string]any, bool)`, `Instantiate(Template, map[string]any) ([]PlanStep, error)`, `(Template).Digest() string`, `(Template).AsMap() (map[string]any, error)`, `templateFrom(any) (Template, error)`, `SkillID(owner, goalSignature string, actions []string, generation int) string` (`"gs-" + hex(sha256(ownerKey|sig|actions[|gen]))[:24]`), `parsePlan(any) ([]PlanStep, error)`, `canonicalJSON(any) string`, `sortedKeys(map[string]any) []string`. Test helper `torchPlan(count float64) []PlanStep`.

- [ ] **Step 1: Write the failing tests**

Create `packs/gamespack/signature_test.go` (the vectors are spec section 5.3, shared with the runtime's own test suite):

```go
package gamespack

import "testing"

// The vectors are part of the runtime contract: a runtime computes the
// signature itself and must reach these exact strings.
func TestGoalSignatureVectors(t *testing.T) {
	cases := []struct {
		goal string
		keys []string
		want string
	}{
		{"Light up the area!", []string{"spot"}, "d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd"},
		{"light up   the area", []string{"spot"}, "d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd"},
		{"Light up the area", []string{"spot", "count"}, "8d4463cfb1db4bf8b0f154d2ba3692bb4b2607dc4c9081bc8925aa8766db0b62"},
		{"Light up the area", nil, "eb75279beebedb94ae46116ef860a3d2ff48ff45b7806fce07a465d838c2fa2a"},
		{"Go to the den.", []string{" ", ""}, "6fff361fbd783f6a11ef41e5bcf9d79e5259623e70a430eb79ba06034eea11b5"},
	}
	for _, c := range cases {
		if got := GoalSignature(c.goal, c.keys); got != c.want {
			t.Errorf("GoalSignature(%q, %v) = %s, want %s", c.goal, c.keys, got, c.want)
		}
	}
}

func TestGoalSignatureIgnoresInputValuesAndKeyOrder(t *testing.T) {
	a := GoalSignature("Build a den", inputKeys(map[string]any{"near": "home", "size": 3}))
	b := GoalSignature("build a den", inputKeys(map[string]any{"size": 9, "near": "base"}))
	if a != b {
		t.Fatalf("the signature must depend on input KEYS only: %s != %s", a, b)
	}
	if c := GoalSignature("build a den", []string{"near"}); c == a {
		t.Fatal("a different key set must give a different signature")
	}
}
```

Create `packs/gamespack/template_test.go`. `TestSkillIDIsDeterministicPerOwnerSignatureActionsAndGeneration` is Review Focus line 4: two owners teaching one goal must get two ids, because row ids are cluster-global:

```go
package gamespack

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func torchPlan(count float64) []PlanStep {
	return []PlanStep{
		{Action: "collect", Args: map[string]any{"block": "coal", "count": float64(3)}},
		{Action: "craft", Args: map[string]any{"item": "torch", "count": count}},
		{Action: "placeTorches", Args: map[string]any{"count": count}},
	}
}

func TestGeneralizeKeepsAgreeingArgumentsAndOpensDifferingOnes(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tmpl.Holes, []string{"1.count", "2.count"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("holes = %v, want %v", got, want)
	}
	if spec := tmpl.Steps[0].Args["block"]; spec.Hole != "" || spec.Const != "coal" {
		t.Fatalf("collect.block = %+v, want the constant coal", spec)
	}
	if spec := tmpl.Steps[0].Args["count"]; spec.Hole != "" || spec.Const != float64(3) {
		t.Fatalf("collect.count = %+v, want the constant 3", spec)
	}
	if spec := tmpl.Steps[2].Args["count"]; spec.Hole != "2.count" {
		t.Fatalf("placeTorches.count = %+v, want the hole 2.count", spec)
	}
}

func TestGeneralizeRefusesPlansThatDisagreeOnShape(t *testing.T) {
	if _, err := Generalize([][]PlanStep{torchPlan(4)}); err == nil {
		t.Fatal("one plan is a record, not a pattern; generalizing it must be refused")
	}
	other := torchPlan(6)
	other[2].Action = "lookAt"
	if _, err := Generalize([][]PlanStep{torchPlan(4), other}); err == nil {
		t.Fatal("plans with different action sequences must not generalize")
	}
	extra := torchPlan(6)
	extra[1].Args["near"] = "home"
	if _, err := Generalize([][]PlanStep{torchPlan(4), extra}); err == nil {
		t.Fatal("a step whose argument keys differ must not generalize")
	}
}

func TestBindReadsHoleValuesAndRefusesADifferentConstant(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := Bind(tmpl, torchPlan(8))
	if !ok {
		t.Fatal("a plan of the same shape must bind")
	}
	if binding["1.count"] != float64(8) || binding["2.count"] != float64(8) {
		t.Fatalf("binding = %v", binding)
	}
	wrong := torchPlan(8)
	wrong[0].Args["block"] = "iron_ore"
	if _, ok := Bind(tmpl, wrong); ok {
		t.Fatal("a plan whose constant differs is not an instance of the skill")
	}
	if _, ok := Bind(tmpl, torchPlan(8)[:2]); ok {
		t.Fatal("a shorter plan is not an instance of the skill")
	}
}

func TestInstantiateFillsHolesAndRefusesAnUnfilledOne(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Instantiate(tmpl, map[string]any{"1.count": float64(5), "2.count": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, torchPlan(5)) {
		t.Fatalf("plan = %+v, want %+v", plan, torchPlan(5))
	}
	if _, err := Instantiate(tmpl, map[string]any{"1.count": float64(5)}); err == nil || !strings.Contains(err.Error(), "2.count") {
		t.Fatalf("an unfilled hole must be refused by name, got %v", err)
	}
}

func TestTemplateSurvivesItsStoredForm(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	if !strings.Contains(string(raw), `"hole":"2.count"`) || !strings.Contains(string(raw), `"const":"coal"`) {
		t.Fatalf("stored form = %s", raw)
	}
	back, err := templateFrom(stored)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != tmpl.Digest() {
		t.Fatal("the digest must not change across the stored form, or no promotion could ever be decided")
	}
}

func TestEqualityIsJSONEquality(t *testing.T) {
	a := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": 4}}}
	b := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": float64(4)}}}
	tmpl, err := Generalize([][]PlanStep{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpl.Holes) != 0 {
		t.Fatalf("4 and 4.0 are one value; holes = %v", tmpl.Holes)
	}
	c := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": "4"}}}
	if tmpl2, _ := Generalize([][]PlanStep{a, c}); len(tmpl2.Holes) != 1 {
		t.Fatalf(`4 and "4" are two values; holes = %v`, tmpl2.Holes)
	}
}

func TestSkillIDIsDeterministicPerOwnerSignatureActionsAndGeneration(t *testing.T) {
	a := SkillID("u1", "sig", []string{"collect", "craft"}, 1)
	if a != SkillID("u1", "sig", []string{"collect", "craft"}, 1) || !strings.HasPrefix(a, "gs-") || len(a) != 27 {
		t.Fatalf("SkillID = %q", a)
	}
	if a != SkillID("v1:identity:user:u1", "sig", []string{"collect", "craft"}, 1) {
		t.Fatal("a bare and a canonical spelling of one owner must name one skill")
	}
	if a == SkillID("u2", "sig", []string{"collect", "craft"}, 1) {
		t.Fatal("two owners teaching one goal must get two skills; one row would merge their ladders")
	}
	if a == SkillID("u1", "sig", []string{"craft", "collect"}, 1) {
		t.Fatal("a different action order is a different skill")
	}
	if a == SkillID("u1", "sig", []string{"collect", "craft"}, 2) {
		t.Fatal("a second generation is a different skill")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/template_test.go:10:33: undefined: PlanStep
packs/gamespack/template_test.go:11:11: undefined: PlanStep
packs/gamespack/template_test.go:19:15: undefined: Generalize
packs/gamespack/template_test.go:19:30: undefined: PlanStep
packs/gamespack/signature_test.go:20:13: undefined: GoalSignature
packs/gamespack/signature_test.go:27:7: undefined: GoalSignature
packs/gamespack/signature_test.go:27:36: undefined: inputKeys
packs/gamespack/signature_test.go:28:7: undefined: GoalSignature
packs/gamespack/signature_test.go:28:36: undefined: inputKeys
packs/gamespack/signature_test.go:32:10: undefined: GoalSignature
packs/gamespack/template_test.go:19:30: too many errors
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write `signature.go`**

```go
package gamespack

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/work"
)

// GoalSignature is the key skills are learned and looked up by:
//
//	hex(sha256(NormalizeStatement(goalText) + "|" + join(sort(inputKeys), ",")))
//
// The NORMALIZATION is component/work.NormalizeStatement -- lower case,
// letters and digits kept, every other character dropped, runs of whitespace
// collapsed to one space -- so a game goal and a learned procedure fold a
// person's wording the same way.
//
// The SEPARATOR is not work.GoalSignature's "\x00args\x00", deliberately. A
// runtime computes this key itself, offline, before it asks the engine
// anything, and the runtime's contract fixes the vertical bar; the engine
// recomputes it on every recorded trace and refuses a supplied one that
// differs, so a runtime whose normalization drifts finds out on its first
// trace rather than by never matching a skill.
//
// Input keys are trimmed, blanks dropped, and sorted; only their NAMES enter
// the key, never their values.
func GoalSignature(goalText string, inputKeys []string) string {
	keys := make([]string, 0, len(inputKeys))
	for _, k := range inputKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(work.NormalizeStatement(goalText) + "|" + strings.Join(keys, ",")))
	return hex.EncodeToString(sum[:])
}

// inputKeys returns the keys of a goal's inputs object, in no particular
// order (GoalSignature sorts).
func inputKeys(inputs map[string]any) []string {
	out := make([]string, 0, len(inputs))
	for k := range inputs {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 4: Write `template.go`**

```go
package gamespack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// template.go -- a skill's steps with holes, and the three functions over it:
// Generalize (traces -> template), Bind (a plan -> hole values) and
// Instantiate (hole values -> a plan). PURE: values in, values out.
//
// WHY NOT component/procedure. That package generalizes RECORDED SHELL, FILE
// AND MCP STEPS: it parses argument strings as command lines, paths and JSON
// documents, classifies holes by data flow from earlier steps' results, and
// scores candidates by compression over a corpus. A game action's arguments
// are small typed values, a game step reports no result a later step reads,
// and the runtime's contract fixes the template shape below -- one hole per
// differing top-level argument, named <stepIndex>.<argKey>. Feeding game
// arguments through component/procedure would mean bypassing its
// canonicalization and translating its leaf-path holes back into this shape;
// what IS shared with learned procedures is the ladder (component/work), and
// that is reused whole.

// PlanStep is one step of a plan: an action name and its arguments.
type PlanStep struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// ArgSpec is one argument of a template step: a constant, or a hole a binding
// fills.
type ArgSpec struct {
	Hole  string
	Const any
}

// MarshalJSON writes {"hole": id} or {"const": value}.
func (a ArgSpec) MarshalJSON() ([]byte, error) {
	if a.Hole != "" {
		return json.Marshal(map[string]string{"hole": a.Hole})
	}
	return json.Marshal(map[string]any{"const": a.Const})
}

// UnmarshalJSON reads {"hole": id} or {"const": value}. Anything else is
// refused: a template that cannot say which an argument is cannot be bound.
func (a *ArgSpec) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if raw, ok := m["hole"]; ok {
		return json.Unmarshal(raw, &a.Hole)
	}
	raw, ok := m["const"]
	if !ok {
		return fmt.Errorf("games: a template argument is neither a hole nor a const: %s", string(b))
	}
	return json.Unmarshal(raw, &a.Const)
}

// TemplateStep is one step of a template.
type TemplateStep struct {
	Action string             `json:"action"`
	Args   map[string]ArgSpec `json:"args"`
}

// Template is a skill: steps whose arguments are constants or holes, and the
// hole ids in step order, then argument-key order.
type Template struct {
	Steps []TemplateStep `json:"steps"`
	Holes []string       `json:"holes"`
}

// HoleID names the hole for one argument of one step.
func HoleID(stepIndex int, argKey string) string {
	return fmt.Sprintf("%d.%s", stepIndex, argKey)
}

// Actions returns a plan's action names in order.
func Actions(plan []PlanStep) []string {
	out := make([]string, len(plan))
	for i, s := range plan {
		out[i] = s.Action
	}
	return out
}

// Generalize turns two or more plans of one goal into a template. Every plan
// must take the same actions in the same order with the same argument keys
// per step; an argument whose value is the same in every plan stays a
// constant and one that differs becomes a hole. Equality is JSON equality,
// so 4 and 4.0 are one value and "4" is another.
func Generalize(plans [][]PlanStep) (Template, error) {
	if len(plans) < 2 {
		return Template{}, fmt.Errorf("games: generalizing needs at least two plans, got %d", len(plans))
	}
	first := plans[0]
	for i, plan := range plans[1:] {
		if !sameStrings(Actions(plan), Actions(first)) {
			return Template{}, fmt.Errorf("games: plan %d takes %v, plan 0 takes %v", i+1, Actions(plan), Actions(first))
		}
	}
	t := Template{Steps: make([]TemplateStep, len(first)), Holes: []string{}}
	for si, step := range first {
		keys := sortedKeys(step.Args)
		t.Steps[si] = TemplateStep{Action: step.Action, Args: map[string]ArgSpec{}}
		for pi, plan := range plans[1:] {
			if !sameStrings(sortedKeys(plan[si].Args), keys) {
				return Template{}, fmt.Errorf("games: step %d of plan %d has arguments %v, plan 0 has %v", si, pi+1, sortedKeys(plan[si].Args), keys)
			}
		}
		for _, k := range keys {
			want := canonicalJSON(step.Args[k])
			constant := true
			for _, plan := range plans[1:] {
				if canonicalJSON(plan[si].Args[k]) != want {
					constant = false
					break
				}
			}
			if constant {
				t.Steps[si].Args[k] = ArgSpec{Const: step.Args[k]}
				continue
			}
			id := HoleID(si, k)
			t.Steps[si].Args[k] = ArgSpec{Hole: id}
			t.Holes = append(t.Holes, id)
		}
	}
	return t, nil
}

// Bind reads the hole values a plan gives a template. It reports false when
// the plan is not an instance of the template: a different number of steps, a
// different action, a different set of argument keys, or a constant that
// differs.
func Bind(t Template, plan []PlanStep) (map[string]any, bool) {
	if len(plan) != len(t.Steps) {
		return nil, false
	}
	out := map[string]any{}
	for si, step := range t.Steps {
		got := plan[si]
		if got.Action != step.Action || len(got.Args) != len(step.Args) {
			return nil, false
		}
		for k, spec := range step.Args {
			v, ok := got.Args[k]
			if !ok {
				return nil, false
			}
			if spec.Hole != "" {
				out[spec.Hole] = v
				continue
			}
			if canonicalJSON(v) != canonicalJSON(spec.Const) {
				return nil, false
			}
		}
	}
	return out, true
}

// Instantiate fills a template's holes from a binding. A hole the binding
// does not fill is an ERROR naming it, never an empty value: a step run with
// an argument nobody chose does something nobody asked for.
func Instantiate(t Template, binding map[string]any) ([]PlanStep, error) {
	out := make([]PlanStep, len(t.Steps))
	for si, step := range t.Steps {
		args := make(map[string]any, len(step.Args))
		for k, spec := range step.Args {
			if spec.Hole == "" {
				args[k] = spec.Const
				continue
			}
			v, ok := binding[spec.Hole]
			if !ok {
				return nil, fmt.Errorf("games: the binding does not fill hole %s", spec.Hole)
			}
			args[k] = v
		}
		out[si] = PlanStep{Action: step.Action, Args: args}
	}
	return out, nil
}

// Digest is the template's version: sha256 over its canonical JSON. A
// promotion approves exactly one digest.
func (t Template) Digest() string {
	sum := sha256.Sum256([]byte(canonicalJSON(t)))
	return hex.EncodeToString(sum[:])
}

// AsMap is the template as the JSON object a mutation stores.
func (t Template) AsMap() (map[string]any, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// templateFrom reads a stored template object back.
func templateFrom(v any) (Template, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Template{}, err
	}
	var t Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return Template{}, fmt.Errorf("games: the stored template does not read back: %w", err)
	}
	return t, nil
}

// SkillID is the deterministic id of the skill one owner learned for one
// signature and one action sequence, so two traces racing to teach the same
// skill name the same row.
//
// THE OWNER IS IN THE KEY. A row id is unique per concept across the whole
// cluster, not per owner, and an insert on an id that exists is a read-merge
// onto it: without the owner, two owners teaching the same goal would write
// one row, the second merging into the first's ladder state.
//
// GENERATION counts the skills ever learned for the pair: a retired skill is
// terminal (component/work's rule), so when the pair is learned again it is
// a NEW skill at generation 2, 3, ... -- the first generation keeps the
// plain key.
func SkillID(owner, goalSignature string, actions []string, generation int) string {
	key := ownerKey(owner) + "|" + goalSignature + "|" + strings.Join(actions, ",")
	if generation > 1 {
		key += fmt.Sprintf("|%d", generation)
	}
	sum := sha256.Sum256([]byte(key))
	return "gs-" + hex.EncodeToString(sum[:])[:24]
}

// parsePlan reads a []object of {action, args} into plan steps.
func parsePlan(v any) ([]PlanStep, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("games_invalid_argument: a plan must be a list of {action, args}")
	}
	out := make([]PlanStep, 0, len(list))
	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d is not an object", i)
		}
		action := strings.TrimSpace(asString(m["action"]))
		if action == "" {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d names no action", i)
		}
		args := map[string]any{}
		if a, ok := m["args"].(map[string]any); ok {
			args = a
		} else if m["args"] != nil {
			return nil, fmt.Errorf("games_invalid_argument: plan step %d args is not an object", i)
		}
		out = append(out, PlanStep{Action: action, Args: args})
	}
	return out, nil
}

// canonicalJSON is the comparison form of a value: encoding/json sorts map
// keys, so equal objects encode equally.
func canonicalJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("!%v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 5: Run them to see them pass**

Run: `go test -count=1 -v -run 'Signature|Generalize|Bind|Instantiate|Template|Equality|SkillID' github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: nine `--- PASS` lines (`TestGoalSignatureVectors`, `TestGoalSignatureIgnoresInputValuesAndKeyOrder`, `TestGeneralizeKeepsAgreeingArgumentsAndOpensDifferingOnes`, `TestGeneralizeRefusesPlansThatDisagreeOnShape`, `TestBindReadsHoleValuesAndRefusesADifferentConstant`, `TestInstantiateFillsHolesAndRefusesAnUnfilledOne`, `TestTemplateSurvivesItsStoredForm`, `TestEqualityIsJSONEquality`, `TestSkillIDIsDeterministicPerOwnerSignatureActionsAndGeneration`) and `ok`.

- [ ] **Step 6: Commit**

```bash
git add packs/gamespack/signature.go packs/gamespack/template.go packs/gamespack/signature_test.go packs/gamespack/template_test.go
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): goal signatures and skill templates" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 7: Record plan traces and learn skills from them

`gamesRecordTrace` validates a trace, recomputes the signature and refuses a supplied one that differs, writes the trace through a `@serverOnly` mutation under internal origin, and learns: once per (owner, signature, action sequence, generation), inside a write gate, from two or more successful, non-negative, non-skill traces. Review Focus line 4 (database half) is pinned here.

**Files:**
- Create: `packs/gamespack/gate.go`, `packs/gamespack/store_internal.go`, `packs/gamespack/skills.go`, `packs/gamespack/learn_db_test.go`
- Modify: `packs/gamespack/store.go` (interface gains `createTrace`, `createSkill`), `packs/gamespack/capabilities.go` (append `recordTrace`), `packs/gamespack/dsl/builtins.memql` (append `gamesRecordTrace`), `call_origin_conformance_test.go:920` (allowlist entry)

**Interfaces:**
- Consumes: `memql.ResolveWriteGatePool(func() *sql.DB, owner, field string)`, `memql.AcquireWriteGate(ctx, pool, key) (func(), error)` (`component/memql/write_gate.go:157`), `memql.ContextWithFreshRead` (`fresh_read.go:36`), `auth.ContextWithInternalOrigin`; Task 6's `GoalSignature`, `inputKeys`, `parsePlan`, `Actions`, `Generalize`, `SkillID`, `(Template).AsMap`, `(Template).Digest`; queries `gamesSkillsForSignature`, `gamesLearningTraces`; mutations `createGamesPlanTrace`, `createGamesSkill` (Task 1).
- Produces: `(*Provider).gate(ctx, key string) (func(), error)`; `rowStore.createTrace(ctx, args) error`, `rowStore.createSkill(ctx, args) error` and their `engineStore` implementations (each stamps internal origin on exactly one rendered call); `(*Provider).recordTrace` (answer `{traceId, goalSignature, learnedSkillId}`; refuses `games_invalid_argument`, `games_signature_mismatch`), `(*Provider).learn(ctx, owner, signature string, actions []string) (string, error)` (gate key `games-learn:<owner>:<signature>`), `stringsAsAny([]string) []any`. Test helpers `stepsArg([]PlanStep) []any`, `trace(id string, plan []PlanStep, outcome, reaction string) map[string]any`, `learnTorchSkill(t, lg, ctx, prefix string) string`.

- [ ] **Step 1: Write the failing tests**

Create `packs/gamespack/learn_db_test.go`. `TestLiveTwoOwnersLearnTheSameGoalApart` is Review Focus line 4: the bug it pins merged a second owner's internal-origin skill write into the first owner's row when skill ids did not carry the owner:

```go
package gamespack

import (
	"context"
	"strings"
	"testing"
	"time"
)

func stepsArg(plan []PlanStep) []any {
	out := make([]any, len(plan))
	for i, s := range plan {
		out[i] = map[string]any{"action": s.Action, "args": s.Args, "ok": true, "durationMs": 1200}
	}
	return out
}

// trace is one gamesRecordTrace call for the torch goal.
func trace(id string, plan []PlanStep, outcome, reaction string) map[string]any {
	return map[string]any{
		"traceId": id, "characterId": "wren", "goalText": "Light up the area!",
		"trigger": "initiative", "inputs": map[string]any{"spot": "here"},
		"steps": stepsArg(plan), "outcome": outcome, "reaction": reaction,
		"at": rfc3339(time.Now().UTC()), "say": "Torches coming up!",
	}
}

// learnTorchSkill teaches the torch skill from two agreeing successful traces
// and returns its id.
func learnTorchSkill(t *testing.T, lg *liveGames, ctx context.Context, prefix string) string {
	t.Helper()
	lg.must(t, ctx, "builtin", "gamesRecordTrace", trace(prefix+"-a", torchPlan(4), "success", "neutral"))
	res := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace(prefix+"-b", torchPlan(6), "success", "neutral"))
	skillID, _ := res[0]["learnedSkillId"].(string)
	if skillID == "" {
		t.Fatalf("two agreeing successful traces taught no skill: %v", res[0])
	}
	return skillID
}

// One trace teaches nothing, a failure and a disliked success do not count,
// and the second qualifying trace teaches the skill, in shadow, generalized.
func TestLiveTwoSuccessfulTracesTeachASkillInShadow(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-learn-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	first := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t1-"+n, torchPlan(4), "success", "neutral"))
	if first[0]["learnedSkillId"] != "" || first[0]["goalSignature"] != sig {
		t.Fatalf("one trace taught a skill, or the signature is not the contract's: %v", first[0])
	}
	if got := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t2-"+n, torchPlan(4), "failure", "neutral")); got[0]["learnedSkillId"] != "" {
		t.Fatalf("a failed plan taught a skill: %v", got[0])
	}
	if got := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t3-"+n, torchPlan(6), "success", "negative")); got[0]["learnedSkillId"] != "" {
		t.Fatalf("a success the player disliked taught a skill: %v", got[0])
	}
	second := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t4-"+n, torchPlan(6), "success", "positive"))
	skillID, _ := second[0]["learnedSkillId"].(string)
	if skillID != SkillID(owner, sig, []string{"collect", "craft", "placeTorches"}, 1) {
		t.Fatalf("two successful, non-negative traces must teach the skill at its derived id: %v", second[0])
	}

	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if len(skill) != 1 || skill[0]["state"] != "shadow" || skill[0]["rung"] != "shadow" {
		t.Fatalf("a learned skill starts in shadow: %v", skill)
	}
	tmpl, err := templateFrom(skill[0]["template"])
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(tmpl.Holes, []string{"1.count", "2.count"}) || skill[0]["templateDigest"] != tmpl.Digest() {
		t.Fatalf("the counts differed between the traces and must be the holes: %+v (digest %v)", tmpl, skill[0]["templateDigest"])
	}
	if skill[0]["confirmation"] != "Torches coming up!" || !sameStrings(asStrings(skill[0]["triggers"]), []string{"initiative"}) {
		t.Fatalf("the skill keeps what the character said and the trigger it was learned under: %v", skill[0])
	}

	again := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t5-"+n, torchPlan(9), "success", "neutral"))
	if again[0]["learnedSkillId"] != "" {
		t.Fatalf("a live skill already covers the goal; a third trace must not learn it again: %v", again[0])
	}
}

func TestLiveSignatureMismatchIsRefused(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	tr := trace("t-mismatch-"+n, torchPlan(4), "success", "neutral")
	tr["goalSignature"] = strings.Repeat("a", 64)
	if _, err := lg.call(t, asOwner("user-games-sig-"+n), "builtin", "gamesRecordTrace", tr); err == nil || !strings.Contains(err.Error(), "games_signature_mismatch") {
		t.Fatalf("a supplied signature that differs must be refused, got %v", err)
	}
}

// The ladder writes the ladder: a client cannot create or move a skill.
func TestLiveClientCannotWriteASkill(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	_, err := lg.call(t, asOwner("user-games-forge-"+n), "mutation", "recordGamesSkillLadder", map[string]any{
		"skillId": "gs-forged-" + n, "versionAt": rfc3339(time.Now()), "rung": "trusted", "state": "trusted",
		"shadowMatches": 0, "canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0,
		"promotionApprovalId": "", "lastReason": "forged",
	})
	if err == nil {
		t.Fatal("a client-origin call reached recordGamesSkillLadder; it must be @serverOnly")
	}
}

// The internal-origin writers keep the caller's actor: the rows they write
// are the caller's, and nobody else reads them (call_origin's allowlist entry
// for this package rests on it).
func TestLiveInternalOriginWritesStayTheCallers(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ownerA, ownerB := "user-games-own-a-"+n, "user-games-own-b-"+n
	a, b := asOwner(ownerA), asOwner(ownerB)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, a, "o-"+n)
	skill := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if len(skill) != 1 || ownerKey(trimmed(skill[0]["ownerUserId"])) != ownerKey(ownerA) {
		t.Fatalf("the learned skill must be owned by the caller %s: %v", ownerA, skill)
	}
	if rows := lg.must(t, b, "query", "gamesSkillById", map[string]any{"skillId": skillID}); len(rows) != 0 {
		t.Fatalf("another owner read the skill: %v", rows)
	}
	if rows := lg.must(t, a, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) < 2 {
		t.Fatalf("the caller reads %d of their traces, want 2", len(rows))
	}
	if rows := lg.must(t, b, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) != 0 {
		t.Fatalf("another owner read %d of the caller's traces", len(rows))
	}
}

// Two owners teach the same goal: two skills, each its owner's. Row ids are
// cluster-global, so a skill id that did not carry the owner would merge the
// second owner's internal-origin write into the first owner's row.
func TestLiveTwoOwnersLearnTheSameGoalApart(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ownerA, ownerB := "user-games-same-a-"+n, "user-games-same-b-"+n
	a, b := asOwner(ownerA), asOwner(ownerB)
	skillA := learnTorchSkill(t, lg, a, "sa-"+n)
	skillB := learnTorchSkill(t, lg, b, "sb-"+n)
	if skillA == skillB {
		t.Fatalf("two owners' skills for one goal share the id %s", skillA)
	}
	for _, c := range []struct {
		ctx   context.Context
		owner string
		id    string
	}{{a, ownerA, skillA}, {b, ownerB, skillB}} {
		rows := lg.must(t, c.ctx, "query", "gamesSkillById", map[string]any{"skillId": c.id})
		if len(rows) != 1 || ownerKey(trimmed(rows[0]["ownerUserId"])) != ownerKey(c.owner) {
			t.Fatalf("skill %s must still be %s's alone: %v", c.id, c.owner, rows)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -run TestLiveTwoSuccessfulTracesTeachASkillInShadow github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -v '^{"time"'`
Expected:

```text
--- FAIL: TestLiveTwoSuccessfulTracesTeachASkillInShadow (4.04s)
    learn_db_test.go:50: builtin gamesRecordTrace: function "gamesRecordTrace" not found
FAIL
FAIL	github.com/znasllc-io/memql/packs/gamespack	4.267s
FAIL
```

- [ ] **Step 3: Write the write gate**

Create `packs/gamespack/gate.go`:

```go
package gamespack

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// gate.go -- the cross-replica write gate every read-modify-write of a games
// row takes: learning (keyed on the owner and the goal signature) and every
// ladder move (keyed on the skill). A Postgres lock around the read and the
// write, a read that skips this node's result cache, and a version stamped
// after the one it read -- each alone was measured insufficient (memql#5431).

// gate takes the cross-replica write gate for key and returns its release.
func (p *Provider) gate(ctx context.Context, key string) (func(), error) {
	pool, err := memql.ResolveWriteGatePool(p.gates, "games", "PluginContext.GateDB")
	if err != nil {
		return nil, err
	}
	return memql.AcquireWriteGate(ctx, pool, key)
}
```

- [ ] **Step 4: Write the internal-origin writers**

Add the two methods to the interface in `packs/gamespack/store.go`:

```diff
diff --git a/packs/gamespack/store.go b/packs/gamespack/store.go
--- a/packs/gamespack/store.go
+++ b/packs/gamespack/store.go
@@ -26,6 +26,10 @@ type rowStore interface {
 	// embedStore embeds text and stores it as nodeID's vector through the one
 	// embedding write path (builtin gamesEmbedStore).
 	embedStore(ctx context.Context, nodeID, concept, text string) error
+	// createTrace and createSkill run two of the three @serverOnly writers
+	// (store_internal.go).
+	createTrace(ctx context.Context, args map[string]any) error
+	createSkill(ctx context.Context, args map[string]any) error
 }
 
 // engineStore is the production rowStore.
```

Create `packs/gamespack/store_internal.go`. Each writer names its construct literally in `langparser.RenderCall`, so the server-only caller gate (`test/dslconformance/server_only_callers_stamp_test.go`) sees the stamp in the same file as the call:

```go
package gamespack

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// store_internal.go -- the three writers that stamp INTERNAL ORIGIN.
//
// Each stamp is on one rendered call of one @serverOnly construct, named
// literally so the server-only caller gate (test/dslconformance) sees it:
// createGamesPlanTrace, createGamesSkill and recordGamesSkillLadder. The
// actor is never touched, so the owner tier still decides every row and a
// caller can only ever write their own. The marked context is a local of the
// one call and is never returned: the call_origin_conformance_test.go entry
// for this package rests on both.

func (s engineStore) createTrace(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("createGamesPlanTrace", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation createGamesPlanTrace: %w", err)
	}
	return nil
}

func (s engineStore) createSkill(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("createGamesSkill", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation createGamesSkill: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Write `skills.go`**

```go
package gamespack

import (
	"context"
	"fmt"
	"sort"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// skills.go -- skill learning for an external runtime, the first half: record
// a trace and learn from it. The runtime executes every step; the engine
// stores the evidence and generalizes it. The ladder is ladder.go and
// ladder_calls.go, the lookup lookup.go.
//
// LEARNING IS ONCE PER (owner, signature, action sequence, generation). The
// first time two successful, non-negative, non-skill traces of one goal agree
// on their actions, they are generalized and the skill is created in shadow
// at a DETERMINISTIC id (SkillID), so two traces racing to teach one skill
// name one row -- and the learn gate serializes them anyway. Later traces do
// not rewrite the template: a version a person approved must stay the version
// that runs (a re-generalized template would be a new digest, which the
// promotion check already refuses). A retired skill is terminal; the goal
// coming back is learned anew at the next generation.

func (p *Provider) recordTrace(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	traceID := trimmed(args["traceId"])
	characterID := trimmed(args["characterId"])
	goalText := trimmed(args["goalText"])
	trigger := trimmed(args["trigger"])
	at := trimmed(args["at"])
	if traceID == "" || characterID == "" || goalText == "" || trigger == "" || at == "" {
		return nil, fmt.Errorf("games_invalid_argument: traceId, characterId, goalText, trigger and at are required")
	}
	outcome := trimmed(args["outcome"])
	if outcome != "success" && outcome != "failure" && outcome != "abandoned" {
		return nil, fmt.Errorf("games_invalid_argument: outcome must be success, failure or abandoned, not %q", outcome)
	}
	reaction := trimmed(args["reaction"])
	if reaction == "" {
		reaction = "neutral"
	}
	if reaction != "positive" && reaction != "neutral" && reaction != "negative" {
		return nil, fmt.Errorf("games_invalid_argument: reaction must be positive, neutral or negative, not %q", reaction)
	}
	inputs := asObject(args["inputs"])
	if inputs == nil {
		inputs = map[string]any{}
	}
	plan, err := parsePlan(args["steps"])
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("games_invalid_argument: a trace has at least one step")
	}
	signature := GoalSignature(goalText, inputKeys(inputs))
	if supplied := trimmed(args["goalSignature"]); supplied != "" && supplied != signature {
		return nil, fmt.Errorf("games_signature_mismatch: the supplied goalSignature %s is not GoalSignature(goalText, input keys) = %s; check the caller's normalization", supplied, signature)
	}
	actions := Actions(plan)
	skillID := bare(trimmed(args["skillId"]))
	write := map[string]any{
		"traceId":       traceID,
		"characterId":   characterID,
		"goalText":      goalText,
		"goalSignature": signature,
		"trigger":       trigger,
		"inputs":        inputs,
		"steps":         args["steps"],
		"actions":       stringsAsAny(actions),
		"outcome":       outcome,
		"reaction":      reaction,
		"at":            at,
	}
	if say := trimmed(args["say"]); say != "" {
		write["say"] = say
	}
	if skillID != "" {
		write["skillId"] = skillID
	}
	if err := p.store.createTrace(ctx, write); err != nil {
		return nil, err
	}
	learned := ""
	if outcome == "success" && reaction != "negative" && skillID == "" {
		if learned, err = p.learn(ctx, owner, signature, actions); err != nil {
			return nil, err
		}
	}
	return answer("recordTrace", map[string]any{
		"traceId":        traceID,
		"goalSignature":  signature,
		"learnedSkillId": learned,
	})
}

// learn creates the skill for (signature, actions) once two qualifying traces
// agree, and returns its id; "" when nothing was learned.
func (p *Provider) learn(ctx context.Context, owner, signature string, actions []string) (string, error) {
	release, err := p.gate(ctx, "games-learn:"+owner+":"+signature)
	if err != nil {
		return "", fmt.Errorf("games: learning could not be locked; the trace is recorded and will count next time: %w", err)
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)

	// A LIVE SKILL ALREADY COVERS THE PAIR, or the pair is learned anew at
	// the next generation: every sibling retired means the goal came back
	// after its skill went unused for the retirement window.
	siblings, err := p.store.query(fresh, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
	if err != nil {
		return "", err
	}
	generation := 1
	for _, s := range siblings {
		if !sameStrings(asStrings(s["actions"]), actions) {
			continue
		}
		if trimmed(s["rung"]) != string(work.RungRetired) {
			return "", nil
		}
		generation++
	}
	skillID := SkillID(owner, signature, actions, generation)
	rows, err := p.store.query(fresh, "gamesLearningTraces", map[string]any{"goalSignature": signature})
	if err != nil {
		return "", err
	}
	var (
		plans    [][]PlanStep
		traceIDs []string
		triggers []string
		goalText string
		say      string
		seenTrig = map[string]bool{}
	)
	for _, r := range rows {
		if trimmed(r["skillId"]) != "" || !sameStrings(asStrings(r["actions"]), actions) {
			continue
		}
		plan, err := parsePlan(r["steps"])
		if err != nil {
			continue
		}
		plans = append(plans, plan)
		traceIDs = append(traceIDs, bare(trimmed(r["id"])))
		if goalText == "" {
			goalText, say = trimmed(r["goalText"]), trimmed(r["say"])
		}
		if t := trimmed(r["trigger"]); t != "" && !seenTrig[t] {
			seenTrig[t] = true
			triggers = append(triggers, t)
		}
	}
	if len(plans) < 2 {
		return "", nil
	}
	tmpl, err := Generalize(plans)
	if err != nil {
		p.logger().Info("games: traces of one goal and one action sequence did not generalize", "goalSignature", signature, "error", err)
		return "", nil
	}
	tmplMap, err := tmpl.AsMap()
	if err != nil {
		return "", err
	}
	sort.Strings(triggers)
	create := map[string]any{
		"skillId":        skillID,
		"goalText":       goalText,
		"goalSignature":  signature,
		"actions":        stringsAsAny(actions),
		"template":       tmplMap,
		"templateDigest": tmpl.Digest(),
		"triggers":       stringsAsAny(triggers),
		"learnedFrom":    stringsAsAny(traceIDs),
	}
	if say != "" {
		create["confirmation"] = say
	}
	if err := p.store.createSkill(ctx, create); err != nil {
		return "", err
	}
	if err := p.space.store(memql.ContextWithBackgroundLane(ctx), canonical(conceptSkill, skillID), conceptSkill, goalText); err != nil {
		p.logger().Info("games: a skill was learned without a goal vector; gamesEmbedPending will retry it", "skillId", skillID, "error", err)
	}
	return skillID, nil
}

func stringsAsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
```

- [ ] **Step 6: Declare the capability and the builtin**

```diff
diff --git a/packs/gamespack/capabilities.go b/packs/gamespack/capabilities.go
--- a/packs/gamespack/capabilities.go
+++ b/packs/gamespack/capabilities.go
@@ -15,5 +15,7 @@ func (p *Provider) Capabilities() []memql.IntegrationCapability {
 			ArgsSchema: map[string]string{"characterId": "string (required)", "playerId": "string", "worldId": "string (required)", "text": "string (required)", "k": "int"}},
 		{Name: "forgetPlayer", Description: "Delete every row the caller holds about one player.", Handler: p.forgetPlayer,
 			ArgsSchema: map[string]string{"playerId": "string (required)"}},
+		{Name: "recordTrace", Description: "Record an executed plan and learn a skill when two successful traces agree.", Handler: p.recordTrace,
+			ArgsSchema: map[string]string{"traceId": "string (required)", "goalText": "string (required)", "steps": "[]object (required)"}},
 	}
 }
```

```diff
diff --git a/packs/gamespack/dsl/builtins.memql b/packs/gamespack/dsl/builtins.memql
--- a/packs/gamespack/dsl/builtins.memql
+++ b/packs/gamespack/dsl/builtins.memql
@@ -69,3 +69,23 @@ builtin gamesRecall {
 builtin gamesForgetPlayer {
   playerId  string!  @description("The player to forget.")
 }
+
+/// Record one executed plan and learn from it.
+@sdk
+@executor("integration.games.recordTrace")
+@args(profile="object")
+@description("Record one executed plan and learn from it. The engine recomputes goalSignature from goalText and the input keys and refuses a supplied one that differs (games_signature_mismatch). When at least two successful, non-negative, non-skill traces of the signature share this trace's action sequence and no skill exists for them, it generalizes them -- an argument equal in every trace stays a constant, one that differs becomes a hole -- and creates a skill in shadow. Answers {traceId, goalSignature, learnedSkillId}.")
+builtin gamesRecordTrace {
+  traceId        string!    @description("The caller's stable bare id for this trace.")
+  characterId    string!    @description("The character that executed the plan.")
+  goalText       string!    @description("The goal the plan was for.")
+  goalSignature  string     @description("The caller's own signature, checked against the engine's. Omit to let the engine compute it.")
+  trigger        string!    @description("What prompted the plan.")
+  inputs         object     @description("The goal's inputs. Only the keys enter the signature.")
+  steps          []object!  @description("[{action, args, ok, durationMs, error}], in execution order.")
+  outcome        string!    @description("success, failure or abandoned.")
+  reaction       string     @description("positive, neutral (default) or negative.")
+  at             datetime!  @description("When the plan finished.")
+  say            string     @description("What the character said when it took the plan on.")
+  skillId        string     @description("The skill this plan ran, when it was a skill run.")
+}
```

- [ ] **Step 7: Allowlist the package's internal-origin stamp**

`TestOnlyAllowlistedPackagesStampInternalOrigin` refuses an unlisted package that stamps internal origin. Add the entry at the end of the allowlist map in `call_origin_conformance_test.go`:

```diff
diff --git a/call_origin_conformance_test.go b/call_origin_conformance_test.go
--- a/call_origin_conformance_test.go
+++ b/call_origin_conformance_test.go
@@ -917,6 +917,21 @@ func TestOnlyAllowlistedPackagesStampInternalOrigin(t *testing.T) {
 		// TestTheStampNeverEscapesItsCall counts the site with comments
 		// stripped.
 		"integrations/compose": "the Materializer's writers -- the stamp opens its @serverOnly mutations (the composition writers, and createLibraryFile once its key is held to the caller's own library/{owner}/ prefix) and changes no actor, so the composite owner tier still decides every row and a caller can only ever write their own (epic memql#4977)",
+		// The games pack's three @serverOnly writers (design record
+		// docs/superpowers/specs/2026-10-18-games-pack-design.md).
+		// REQUEST-DERIVED: a game runtime and a browser call the pack's
+		// builtins as the owner, and a builtin stamps internal origin on
+		// exactly one rendered call of exactly one writer --
+		// createGamesPlanTrace after it recomputed the goal signature,
+		// createGamesSkill after it read the evidence under the caller's
+		// actor inside the learn gate, recordGamesSkillLadder with the state
+		// component/work.Advance returned for a row it read under the
+		// caller's actor inside the skill's gate. The actor is never
+		// touched, so ownerUserId is stamped from the caller and the owned
+		// tier decides every row; the marked context is a local of the one
+		// call (packs/gamespack/store_internal.go) and is never returned. Asserted by
+		// packs/gamespack's TestLiveInternalOriginWritesStayTheCallers.
+		"packs/gamespack": "the games pack's trace, skill and ladder writers -- REQUEST-DERIVED, one stamp per rendered @serverOnly call, after the builtin read its evidence under the caller's own actor; the actor is unchanged, so the owned tier decides every row and a caller only ever writes their own",
 	}
 
 	// The wire path. These must never appear, allowlist or not: every handler
```

- [ ] **Step 8: Run the tests to see them pass**

Run: `go vet github.com/znasllc-io/memql/packs/gamespack/ && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v -run 'TestLiveTwoSuccessful|TestLiveSignatureMismatch|TestLiveClientCannot|TestLiveInternalOrigin|TestLiveTwoOwners' github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```text
--- PASS: TestLiveTwoSuccessfulTracesTeachASkillInShadow
--- PASS: TestLiveSignatureMismatchIsRefused
--- PASS: TestLiveClientCannotWriteASkill
--- PASS: TestLiveInternalOriginWritesStayTheCallers
--- PASS: TestLiveTwoOwnersLearnTheSameGoalApart
ok  	github.com/znasllc-io/memql/packs/gamespack
```

The origin gates list TRACKED files (`git ls-files`), so stage before running them; an unstaged `store_internal.go` reads as "stale allowlist entry packs/gamespack -- it no longer stamps internal origin".

Run: `git add packs/gamespack/gate.go packs/gamespack/store_internal.go packs/gamespack/skills.go packs/gamespack/store.go packs/gamespack/learn_db_test.go packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql call_origin_conformance_test.go && go test -count=1 -run 'TestOnlyAllowlistedPackagesStampInternalOrigin' github.com/znasllc-io/memql && go test -count=1 -run ServerOnly github.com/znasllc-io/memql/test/dslconformance/ && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `ok  	github.com/znasllc-io/memql`, `ok  	github.com/znasllc-io/memql/test/dslconformance`, `exit=0`.

- [ ] **Step 9: Commit**

```bash
git add packs/gamespack/gate.go packs/gamespack/store_internal.go packs/gamespack/skills.go packs/gamespack/store.go \
  packs/gamespack/learn_db_test.go packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql \
  call_origin_conformance_test.go
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): record plan traces and learn skills from them" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 8: The skill ladder, on component/work's state machine

Four builtins move a skill: shadow comparisons, the owner's one decision, canary and trusted runs, and the sweep. Each is one `ladderMove` that `moveLadder` applies inside the skill's write gate, over a fresh read, writing the whole state `work.Advance` returns with a version stamped after the one read. Review Focus lines 2 and 5 are pinned here.

**Files:**
- Create: `packs/gamespack/ladder.go`, `packs/gamespack/ladder_calls.go`, `packs/gamespack/refusals.go`, `packs/gamespack/ladder_test.go`, `packs/gamespack/ladder_db_test.go`, `packs/gamespack/refusals_test.go`
- Modify: `packs/gamespack/store.go` and `packs/gamespack/store_internal.go` (`recordLadder`), `packs/gamespack/capabilities.go` (append four), `packs/gamespack/dsl/builtins.memql` (append four)

**Interfaces:**
- Consumes: `work.Advance(state work.LadderState, ev work.LadderEvent, policy work.LadderPolicy) work.Transition` (`component/work/ladder.go:271`), `work.LadderState`, `work.LadderEvent` (`EventShadowCompared`, `EventReplayed`, `EventPromotionDecided`, `EventSweep`), `work.LadderPolicy{...}.Normalize()`, `work.DefaultLadderPolicy()`, `work.ParseRung`, `work.Rung*`, `work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})` (`component/work/replay.go:84`), `work.ServeConstruct`; query `ladderPolicyCurrent` (core), `gamesSkillById`, `gamesSkillsForSignature`, `gamesSkillsToSweep`; mutation `recordGamesSkillLadder` (Task 1); Task 7's `gate`, `learnTorchSkill`, `trace`; Task 6's `Bind`, `templateFrom`, `canonicalJSON`, `torchPlan`.
- Produces: `promotionIDFor(templateDigest string) string` (`"promotion-" + digest[:16]`), `stateOf(rung work.Rung, promotionApprovalID string) string`, `ladderStateOf(row map[string]any) work.LadderState`, `(*Provider).policy(ctx) work.LadderPolicy`, `type ladderMove func(row map[string]any, state work.LadderState) (event work.LadderEvent, extra map[string]any, skip bool, refusal error)`, `(*Provider).moveLadder(ctx, skillID string, move ladderMove) (work.Transition, map[string]any, error)` (gate key `games-skill:<id>`; refuses `games_skill_not_found`), `bindingsArg`, `bindingValues(map[string]any) map[string]string`, `serveOf(work.Rung) (serve string, standby bool, reason string)`; `rowStore.recordLadder`; handlers `shadowCompare`, `reportSkillOutcome` (refuses `games_skill_not_serving`), `decidePromotion` (refuses `games_skill_version_changed`, `games_no_promotion_open`), `sweepSkills`; `var RefusalCodes []string` (the eight codes of spec 5.8). Test helpers `planArg`, `earnPromotion(t, lg, ctx, sig)`, `climbToCanary(t, lg, ctx, prefix) (skillID, sig string)`.

- [ ] **Step 1: Write the failing tests**

Create `packs/gamespack/ladder_test.go` (the mapping onto `work.Advance`, over values):

```go
package gamespack

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

func TestStateOfNamesAwaitingApproval(t *testing.T) {
	cases := []struct {
		rung work.Rung
		open string
		want string
	}{
		{work.RungShadow, "", "shadow"},
		{work.RungShadow, "promotion-abc", "awaiting_approval"},
		{work.RungCanary, "", "canary"},
		{work.RungTrusted, "", "trusted"},
		{work.RungRetired, "", "retired"},
	}
	for _, c := range cases {
		if got := stateOf(c.rung, c.open); got != c.want {
			t.Errorf("stateOf(%s, %q) = %s, want %s", c.rung, c.open, got, c.want)
		}
	}
}

func TestLadderStateReadsTheStoredRow(t *testing.T) {
	row := map[string]any{
		"rung":                "canary",
		"shadowMatches":       float64(0),
		"canaryMatches":       float64(3),
		"failures":            float64(1),
		"promotionApprovalId": "",
		"distinctBindings":    map[string]any{"1.count": []any{"sha256:a", "sha256:b"}},
		"lastReplayAt":        "2026-10-18T10:00:00Z",
	}
	s := ladderStateOf(row)
	if s.Rung != work.RungCanary || s.CanaryMatches != 3 || s.Failures != 1 || len(s.DistinctBindings["1.count"]) != 2 {
		t.Fatalf("state = %+v", s)
	}
	if !s.LastReplayAt.Equal(time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastReplayAt = %v", s.LastReplayAt)
	}
}

// The ladder is component/work.Advance; this pins how the games pack feeds
// it: k distinct values PER HOLE, counted over canonical JSON.
func TestPromotionNeedsFiveMatchesAndTwoValuesOfEveryHole(t *testing.T) {
	holes := []string{"1.count", "2.count"}
	s := work.LadderState{Rung: work.RungShadow}
	p := work.DefaultLadderPolicy()
	at := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for i, count := range []float64{4, 4, 4, 4, 4} {
		tr := work.Advance(s, work.LadderEvent{Kind: work.EventShadowCompared, At: at, Match: true, FreeParameters: holes,
			Bindings: bindingValues(map[string]any{"1.count": count, "2.count": count})}, p)
		if tr.Propose {
			t.Fatalf("match %d proposed with one value per hole", i+1)
		}
		s = tr.State
	}
	tr := work.Advance(s, work.LadderEvent{Kind: work.EventShadowCompared, At: at, Match: true, FreeParameters: holes,
		Bindings: bindingValues(map[string]any{"1.count": float64(6), "2.count": float64(6)})}, p)
	if !tr.Propose {
		t.Fatalf("six matches across two values of every hole must propose: %s", tr.Reason)
	}
}

func TestBindingValuesKeepTypesApart(t *testing.T) {
	a := bindingValues(map[string]any{"0.count": float64(4)})
	b := bindingValues(map[string]any{"0.count": "4"})
	if a["0.count"] == b["0.count"] {
		t.Fatal(`4 and "4" must count as two bindings`)
	}
}

func TestServeOfFollowsDecideServe(t *testing.T) {
	cases := []struct {
		rung    work.Rung
		serve   string
		standby bool
	}{
		{work.RungTrusted, "skill", false},
		{work.RungCanary, "skill", true},
		{work.RungShadow, "model", false},
		{work.RungRetired, "model", false},
	}
	for _, c := range cases {
		serve, standby, _ := serveOf(c.rung)
		if serve != c.serve || standby != c.standby {
			t.Errorf("serveOf(%s) = %s/%v, want %s/%v", c.rung, serve, standby, c.serve, c.standby)
		}
	}
}

func TestPromotionIDPinsTheDigest(t *testing.T) {
	if promotionIDFor("abcdef0123456789ffff") != "promotion-abcdef0123456789" {
		t.Fatalf("promotion id = %s", promotionIDFor("abcdef0123456789ffff"))
	}
}
```

Create `packs/gamespack/ladder_db_test.go`. `TestLiveConcurrentOutcomesBothCount` is Review Focus line 2 (without the gate, the fresh read and the stamped version, one of the two runs is lost); `TestLiveRetiredSkillIsRelearnedAsANewGeneration` is line 5:

```go
package gamespack

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func planArg(plan []PlanStep) []any {
	out := make([]any, len(plan))
	for i, s := range plan {
		out[i] = map[string]any{"action": s.Action, "args": s.Args}
	}
	return out
}

// earnPromotion makes five matching comparisons across two values of every
// hole, which proposes the skill for the owner's approval.
func earnPromotion(t *testing.T, lg *liveGames, ctx context.Context, sig string) {
	t.Helper()
	for i, count := range []float64{4, 4, 4, 4, 6} {
		res := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(count))})
		if asInt(res[0]["matched"], 0) != 1 {
			t.Fatalf("comparison %d did not match: %v", i+1, res[0])
		}
	}
}

// climbToCanary learns the torch skill, earns its promotion and approves it.
func climbToCanary(t *testing.T, lg *liveGames, ctx context.Context, prefix string) (skillID, sig string) {
	t.Helper()
	sig = GoalSignature("Light up the area!", []string{"spot"})
	skillID = learnTorchSkill(t, lg, ctx, prefix)
	earnPromotion(t, lg, ctx, sig)
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	decided := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true})
	if decided[0]["rung"] != "canary" {
		t.Fatalf("approval must make it canary: %v", decided[0])
	}
	return skillID, sig
}

// The whole ladder, through the real engine: a shadow skill runs nothing and
// has nothing to decide, earns a promotion, takes one approval, becomes
// trusted after five clean canary runs, and two failures send it back.
func TestLiveSkillClimbsTheLadder(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-ladder-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, ctx, "t-"+n)
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]

	if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err == nil || !strings.Contains(err.Error(), "games_skill_not_serving") {
		t.Fatalf("a shadow skill runs for nobody; reporting a run must be refused, got %v", err)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true}); err == nil || !strings.Contains(err.Error(), "games_no_promotion_open") {
		t.Fatalf("a skill that earned nothing has nothing to decide, got %v", err)
	}

	earnPromotion(t, lg, ctx, sig)
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["state"] != "awaiting_approval" {
		t.Fatalf("five matches across two values of every hole must propose: %v", skill)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillsAwaitingApproval", nil); len(rows) != 1 {
		t.Fatalf("the awaiting list has %d skills, want 1", len(rows))
	}

	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": "0000", "approved": true}); err == nil || !strings.Contains(err.Error(), "games_skill_version_changed") {
		t.Fatalf("a decision about another version must be refused, got %v", err)
	}
	decided := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true})
	if decided[0]["rung"] != "canary" {
		t.Fatalf("approval must make it canary: %v", decided[0])
	}
	if open := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]["promotionApprovalId"]; open != "" && open != nil {
		t.Fatalf("a decided promotion must be closed on the row, got promotionApprovalId %v", open)
	}

	for i := 0; i < 5; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true, "binding": map[string]any{"1.count": 5, "2.count": 5}})
	}
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["rung"] != "trusted" {
		t.Fatalf("five clean canary runs must make it trusted: %v", skill)
	}

	for i := 0; i < 2; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": false})
	}
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["rung"] != "shadow" || skill["state"] != "shadow" {
		t.Fatalf("two failures must demote to shadow: %v", skill)
	}
}

// Two characters report runs of one skill at once: both count.
func TestLiveConcurrentOutcomesBothCount(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-race-" + n)
	skillID, _ := climbToCanary(t, lg, ctx, "r-"+n)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if asInt(skill["canaryMatches"], 0) != 2 {
		t.Fatalf("two concurrent clean runs counted %v times, want 2", skill["canaryMatches"])
	}
}

// A skill unused past the retirement window retires, and the goal coming back
// teaches a NEW skill: a retired one is terminal.
func TestLiveRetiredSkillIsRelearnedAsANewGeneration(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-retire-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	actions := []string{"collect", "craft", "placeTorches"}
	if first := learnTorchSkill(t, lg, ctx, "g1-"+n); first != SkillID(owner, sig, actions, 1) {
		t.Fatalf("first generation id = %v", first)
	}
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)
	if asInt(swept[0]["retired"], 0) != 1 {
		t.Fatalf("a skill unused for 31 days must retire: %v", swept[0])
	}
	lg.provider.now = time.Now
	// The goal comes back. Its earlier traces are still within their 30 days,
	// so the first new success already makes two that agree.
	second := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("g2a-"+n, torchPlan(5), "success", "neutral"))[0]["learnedSkillId"]
	if second != SkillID(owner, sig, actions, 2) {
		t.Fatalf("the goal coming back after retirement must teach generation 2, got %v", second)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": second}); len(rows) != 1 || rows[0]["state"] != "shadow" {
		t.Fatalf("generation 2 must start in shadow: %v", rows)
	}
}
```

Create `packs/gamespack/refusals_test.go` (the codes are a contract: declared equals raised, both ways):

```go
package gamespack

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The refusal codes are a contract: every code the source raises is in
// RefusalCodes, and every code in RefusalCodes is one the source raises.
func TestEveryRefusalCodeIsDeclaredAndRaised(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	raised := map[string]bool{}
	code := regexp.MustCompile(`"(games_[a-z_]+):`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range code.FindAllStringSubmatch(string(src), -1) {
			raised[m[1]] = true
		}
	}
	for c := range raised {
		if !slices.Contains(RefusalCodes, c) {
			t.Errorf("%s is raised but not declared in RefusalCodes", c)
		}
	}
	for _, c := range RefusalCodes {
		if !raised[c] {
			t.Errorf("%s is declared in RefusalCodes but nothing raises it", c)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/ladder_test.go:23:13: undefined: stateOf
packs/gamespack/ladder_test.go:39:7: undefined: ladderStateOf
packs/gamespack/ladder_test.go:57:14: undefined: bindingValues
packs/gamespack/ladder_test.go:64:13: undefined: bindingValues
packs/gamespack/ladder_test.go:71:7: undefined: bindingValues
packs/gamespack/ladder_test.go:72:7: undefined: bindingValues
packs/gamespack/ladder_test.go:90:24: undefined: serveOf
packs/gamespack/ladder_test.go:98:5: undefined: promotionIDFor
packs/gamespack/ladder_test.go:99:33: undefined: promotionIDFor
packs/gamespack/refusals_test.go:33:23: undefined: RefusalCodes
packs/gamespack/refusals_test.go:33:23: too many errors
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write the ladder writer**

```diff
diff --git a/packs/gamespack/store.go b/packs/gamespack/store.go
--- a/packs/gamespack/store.go
+++ b/packs/gamespack/store.go
@@ -30,6 +30,9 @@ type rowStore interface {
 	// (store_internal.go).
 	createTrace(ctx context.Context, args map[string]any) error
 	createSkill(ctx context.Context, args map[string]any) error
+	// recordLadder runs the third, which writes a skill's whole ladder state
+	// (store_internal.go).
+	recordLadder(ctx context.Context, args map[string]any) error
 }
 
 // engineStore is the production rowStore.
```

```diff
diff --git a/packs/gamespack/store_internal.go b/packs/gamespack/store_internal.go
--- a/packs/gamespack/store_internal.go
+++ b/packs/gamespack/store_internal.go
@@ -39,3 +39,14 @@ func (s engineStore) createSkill(ctx context.Context, args map[string]any) error
 	}
 	return nil
 }
+
+func (s engineStore) recordLadder(ctx context.Context, args map[string]any) error {
+	call, err := langparser.RenderCall("recordGamesSkillLadder", compact(args))
+	if err != nil {
+		return err
+	}
+	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
+		return fmt.Errorf("games: mutation recordGamesSkillLadder: %w", err)
+	}
+	return nil
+}
```

- [ ] **Step 4: Write `ladder.go`**

```go
package gamespack

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// ladder.go -- a skill's certification ladder, as rows.
//
// THE STATE MACHINE IS component/work.Advance, unchanged and shared with
// learned procedures: the same rungs, the same events, the same values from
// v1:authoring:ladderPolicy:primary. This file reads a skill row into a
// work.LadderState, hands Advance one event, and writes the WHOLE state it
// returns. It decides nothing itself.
//
// THREE MAPPINGS a reader needs:
//
//   - A skill is born in SHADOW, never candidate: two successful plans of one
//     goal that agree on their actions are already comparable, which is all
//     candidate waits for.
//   - "Awaiting approval" is shadow with an open promotion. Advance opens one
//     by answering Propose; a games promotion is not a v1:work:approval
//     (those require a run, and a skill has none), so the open promotion is
//     the skill's own promotionApprovalId, set to promotionIDFor(digest) --
//     which pins it to the template digest the owner is shown.
//   - A run's "insufficient precondition" has no game meaning: a runtime
//     reports only whether a run succeeded, so Insufficient is always false.
//
// EVERY MOVE IS ONE CRITICAL SECTION, for integrations/procedure's measured
// reason (review finding C1 there): a ladder write made from a state read
// before somebody else's write puts their move back. So each move takes a
// write gate keyed on the skill (memql.AcquireWriteGate), reads the row FRESH
// inside it (memql.ContextWithFreshRead), and stamps its version strictly
// after the version it read (versionAfter). Two characters reporting runs of
// one skill on two replicas therefore count twice, not once.

// promotionIDFor is the open promotion for one template version.
func promotionIDFor(templateDigest string) string {
	d := templateDigest
	if len(d) > 16 {
		d = d[:16]
	}
	return "promotion-" + d
}

// stateOf is the rung as a person reads it.
func stateOf(rung work.Rung, promotionApprovalID string) string {
	if rung == work.RungShadow && promotionApprovalID != "" {
		return "awaiting_approval"
	}
	return string(rung)
}

// ladderStateOf reads a skill row's ladder half.
func ladderStateOf(row map[string]any) work.LadderState {
	rung, ok := work.ParseRung(trimmed(row["rung"]))
	if !ok {
		rung = work.RungNone
	}
	bindings := map[string][]string{}
	for hole, digests := range asObject(row["distinctBindings"]) {
		bindings[hole] = asStrings(digests)
	}
	return work.LadderState{
		Rung:                rung,
		ShadowMatches:       asInt(row["shadowMatches"], 0),
		CanaryMatches:       asInt(row["canaryMatches"], 0),
		DistinctBindings:    bindings,
		Failures:            asInt(row["failures"], 0),
		PromotionApprovalId: trimmed(row["promotionApprovalId"]),
		LastReplayAt:        timeOf(row["lastReplayAt"]),
	}
}

// policy reads the ladder's values under the caller's actor. Absent or
// unreadable is the design record's numbers, always normalized
// (integrations/procedure readPolicy, the same rule).
func (p *Provider) policy(ctx context.Context) work.LadderPolicy {
	rows, err := p.store.query(ctx, "ladderPolicyCurrent", nil)
	if err != nil || len(rows) == 0 {
		return work.DefaultLadderPolicy()
	}
	r := rows[0]
	return work.LadderPolicy{
		ShadowMatches:        asInt(r["shadowMatches"], 0),
		DistinctBindings:     asInt(r["distinctBindings"], 0),
		CanaryMatches:        asInt(r["canaryMatches"], 0),
		FailuresToDemote:     asInt(r["failuresToDemote"], 0),
		InsufficientToDemote: asInt(r["insufficientToDemote"], 0),
		RetireAfterDays:      asInt(r["retireAfterDays"], 0),
	}.Normalize()
}

// ladderMove is one event, built from the skill row read inside the gate.
// skip reports that the row is not one the event applies to, and nothing is
// written; refusal is returned to the caller as an error.
type ladderMove func(row map[string]any, state work.LadderState) (event work.LadderEvent, extra map[string]any, skip bool, refusal error)

// moveLadder applies one ladder event to one skill as a single critical
// section and writes the whole resulting state. It returns the transition
// and the row it read; a skipped move returns a zero transition.
func (p *Provider) moveLadder(ctx context.Context, skillID string, move ladderMove) (work.Transition, map[string]any, error) {
	release, err := p.gate(ctx, "games-skill:"+bare(skillID))
	if err != nil {
		return work.Transition{}, nil, fmt.Errorf("games: the skill's ladder could not be locked; nothing moved: %w", err)
	}
	defer release()

	rows, err := p.store.query(memql.ContextWithFreshRead(ctx), "gamesSkillById", map[string]any{"skillId": bare(skillID)})
	if err != nil {
		return work.Transition{}, nil, err
	}
	if len(rows) == 0 {
		return work.Transition{}, nil, fmt.Errorf("games_skill_not_found: skill %q is not readable by this caller", bare(skillID))
	}
	row := rows[0]
	state := ladderStateOf(row)
	event, extra, skip, refusal := move(row, state)
	if refusal != nil {
		return work.Transition{}, row, refusal
	}
	if skip {
		return work.Transition{}, row, nil
	}
	t := work.Advance(state, event, p.policy(ctx))
	if t.Propose {
		t.State.PromotionApprovalId = promotionIDFor(trimmed(row["templateDigest"]))
	}
	args := map[string]any{
		"skillId":             bare(skillID),
		"versionAt":           rfc3339(versionAfter(timeOf(row["createdAt"]), p.clock())),
		"rung":                string(t.To),
		"state":               stateOf(t.To, t.State.PromotionApprovalId),
		"shadowMatches":       t.State.ShadowMatches,
		"canaryMatches":       t.State.CanaryMatches,
		"distinctBindings":    bindingsArg(t.State.DistinctBindings),
		"failures":            t.State.Failures,
		"promotionApprovalId": t.State.PromotionApprovalId,
		"lastReason":          t.Reason,
	}
	if !t.State.LastReplayAt.IsZero() {
		args["lastReplayAt"] = rfc3339(t.State.LastReplayAt)
	}
	if t.Propose {
		args["promotionProposedAt"] = rfc3339(p.clock())
	}
	if t.From != t.To {
		args["rungChangedAt"] = rfc3339(p.clock())
	}
	for k, v := range extra {
		args[k] = v
	}
	if err := p.store.recordLadder(ctx, args); err != nil {
		return work.Transition{}, row, err
	}
	return t, row, nil
}

// bindingsArg is the distinctBindings map as the object the mutation stores.
// Never nil: an empty object is how a cleared streak is written.
func bindingsArg(b map[string][]string) map[string]any {
	out := make(map[string]any, len(b))
	for hole, digests := range b {
		list := make([]any, len(digests))
		for i, d := range digests {
			list[i] = d
		}
		out[hole] = list
	}
	return out
}

// bindingValues is a binding as the strings Advance digests: each value's
// canonical JSON, so 4 and "4" are different bindings and {"x":1} has one
// spelling.
func bindingValues(binding map[string]any) map[string]string {
	out := make(map[string]string, len(binding))
	for hole, v := range binding {
		out[hole] = canonicalJSON(v)
	}
	return out
}

// serveOf is what a lookup tells the runtime to do with a skill on a rung:
// work.DecideServe's answer for a live goal, in the runtime's words.
func serveOf(rung work.Rung) (serve string, standby bool, reason string) {
	v := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})
	if v.Source == work.ServeConstruct {
		return "skill", v.Standby, v.Reason
	}
	return "model", false, v.Reason
}
```

- [ ] **Step 5: Write `ladder_calls.go`**

```go
package gamespack

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/work"
)

// ladder_calls.go -- the four builtins that move a skill on its ladder:
// gamesShadowCompare (a model's plan compared with the shadow skills of its
// goal), gamesDecideSkillPromotion (the owner's one decision),
// gamesReportSkillOutcome (a canary or trusted run) and gamesSweepSkills
// (demotion and retirement re-applied). Each builds one ladderMove per skill
// and moveLadder (ladder.go) applies it inside the skill's write gate.

func (p *Provider) shadowCompare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	signature := trimmed(args["goalSignature"])
	if signature == "" {
		return nil, fmt.Errorf("games_invalid_argument: goalSignature is required")
	}
	plan, err := parsePlan(args["plan"])
	if err != nil {
		return nil, err
	}
	skills, err := p.store.query(ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
	if err != nil {
		return nil, err
	}
	compared, matched := 0, 0
	proposed := []string{}
	for _, s := range skills {
		if trimmed(s["rung"]) != string(work.RungShadow) || !sameStrings(asStrings(s["actions"]), Actions(plan)) {
			continue
		}
		var bound map[string]any
		t, _, err := p.moveLadder(ctx, trimmed(s["id"]), func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			if state.Rung != work.RungShadow {
				return work.LadderEvent{}, nil, true, nil
			}
			tmpl, err := templateFrom(row["template"])
			if err != nil {
				return work.LadderEvent{}, nil, false, err
			}
			binding, ok := Bind(tmpl, plan)
			event := work.LadderEvent{Kind: work.EventShadowCompared, At: p.clock(), Match: ok, FreeParameters: tmpl.Holes}
			var extra map[string]any
			if ok {
				event.Bindings = bindingValues(binding)
				bound = binding
				extra = map[string]any{"lastBinding": binding}
			}
			return event, extra, false, nil
		})
		if err != nil {
			return nil, err
		}
		if t.From == "" && t.To == "" {
			continue
		}
		compared++
		if bound != nil {
			matched++
		}
		if t.Propose {
			proposed = append(proposed, bare(trimmed(s["id"])))
		}
	}
	return answer("shadowCompare", map[string]any{
		"compared": compared,
		"matched":  matched,
		"proposed": proposed,
	})
}

func (p *Provider) reportSkillOutcome(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	skillID := bare(trimmed(args["skillId"]))
	ok, isBool := args["ok"].(bool)
	if skillID == "" || !isBool {
		return nil, fmt.Errorf("games_invalid_argument: skillId and ok are required")
	}
	binding := asObject(args["binding"])
	t, _, err := p.moveLadder(ctx, skillID, func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
		if state.Rung != work.RungCanary && state.Rung != work.RungTrusted {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_skill_not_serving: skill %q is %s; only a canary or trusted skill runs for real", skillID, state.Rung)
		}
		var extra map[string]any
		if ok && binding != nil {
			extra = map[string]any{"lastBinding": binding}
		}
		return work.LadderEvent{Kind: work.EventReplayed, At: p.clock(), Match: ok}, extra, false, nil
	})
	if err != nil {
		return nil, err
	}
	return answer("reportSkillOutcome", map[string]any{
		"skillId": skillID,
		"rung":    string(t.To),
		"state":   stateOf(t.To, t.State.PromotionApprovalId),
		"reason":  t.Reason,
	})
}

func (p *Provider) decidePromotion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	skillID := bare(trimmed(args["skillId"]))
	digest := trimmed(args["templateDigest"])
	approved, isBool := args["approved"].(bool)
	if skillID == "" || digest == "" || !isBool {
		return nil, fmt.Errorf("games_invalid_argument: skillId, templateDigest and approved are required")
	}
	t, _, err := p.moveLadder(ctx, skillID, func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
		if current := trimmed(row["templateDigest"]); current != digest {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_skill_version_changed: the decision is about template %s and the skill is now %s; decide the version it is now", digest, current)
		}
		if state.Rung != work.RungShadow || state.PromotionApprovalId == "" {
			return work.LadderEvent{}, nil, false, fmt.Errorf("games_no_promotion_open: skill %q has no promotion waiting for a decision", skillID)
		}
		return work.LadderEvent{Kind: work.EventPromotionDecided, At: p.clock(), Approved: approved}, nil, false, nil
	})
	if err != nil {
		return nil, err
	}
	return answer("decidePromotion", map[string]any{
		"skillId": skillID,
		"rung":    string(t.To),
		"state":   stateOf(t.To, t.State.PromotionApprovalId),
		"reason":  t.Reason,
	})
}

func (p *Provider) sweepSkills(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	skills, err := p.store.query(ctx, "gamesSkillsToSweep", nil)
	if err != nil {
		return nil, err
	}
	examined, demoted, retired := 0, 0, 0
	for _, s := range skills {
		if ctxDone(ctx) {
			break
		}
		t, _, err := p.moveLadder(ctx, trimmed(s["id"]), func(row map[string]any, state work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			if state.Rung == work.RungRetired || state.Rung == work.RungNone {
				return work.LadderEvent{}, nil, true, nil
			}
			lastUsed := state.LastReplayAt
			if learned := timeOf(row["learnedAt"]); learned.After(lastUsed) {
				lastUsed = learned
			}
			event := work.LadderEvent{Kind: work.EventSweep, At: p.clock(), LastUsedAt: lastUsed}
			// A sweep that would change nothing writes nothing: the ladder's
			// version history is a record of moves, not of looks.
			if preview := work.Advance(state, event, p.policy(ctx)); !preview.Demoted && !preview.Retired {
				return work.LadderEvent{}, nil, true, nil
			}
			return event, nil, false, nil
		})
		if err != nil {
			return nil, err
		}
		examined++
		if t.Demoted {
			demoted++
		}
		if t.Retired {
			retired++
		}
	}
	return answer("sweepSkills", map[string]any{
		"examined": examined,
		"demoted":  demoted,
		"retired":  retired,
	})
}
```

- [ ] **Step 6: Write `refusals.go`**

```go
package gamespack

// RefusalCodes are the stable codes this pack's refusals lead with. A client
// matches the code, never the sentence around it; refusals_test.go holds this
// list equal to the codes the source actually raises.
var RefusalCodes = []string{
	"games_unauthenticated",       // the call carried no signed-in owner
	"games_invalid_argument",      // a required argument is missing or malformed
	"games_signature_mismatch",    // a supplied goalSignature is not GoalSignature(goalText, input keys)
	"games_skill_not_found",       // the skill is not readable by this caller
	"games_skill_not_serving",     // an outcome was reported for a skill that is not canary or trusted
	"games_skill_version_changed", // a promotion decision named a template digest the skill no longer has
	"games_no_promotion_open",     // a promotion decision for a skill with no open promotion
	"games_player_not_found",      // the player is not one of this owner's players
}
```

- [ ] **Step 7: Declare the capabilities and the builtins**

```diff
diff --git a/packs/gamespack/capabilities.go b/packs/gamespack/capabilities.go
--- a/packs/gamespack/capabilities.go
+++ b/packs/gamespack/capabilities.go
@@ -17,5 +17,13 @@ func (p *Provider) Capabilities() []memql.IntegrationCapability {
 			ArgsSchema: map[string]string{"playerId": "string (required)"}},
 		{Name: "recordTrace", Description: "Record an executed plan and learn a skill when two successful traces agree.", Handler: p.recordTrace,
 			ArgsSchema: map[string]string{"traceId": "string (required)", "goalText": "string (required)", "steps": "[]object (required)"}},
+		{Name: "shadowCompare", Description: "Compare a model's plan with the shadow skills of its goal.", Handler: p.shadowCompare,
+			ArgsSchema: map[string]string{"goalSignature": "string (required)", "plan": "[]object (required)"}},
+		{Name: "reportSkillOutcome", Description: "Report one canary or trusted skill run.", Handler: p.reportSkillOutcome,
+			ArgsSchema: map[string]string{"skillId": "string (required)", "ok": "bool (required)", "binding": "object"}},
+		{Name: "decidePromotion", Description: "The owner's decision on a skill's promotion.", Handler: p.decidePromotion,
+			ArgsSchema: map[string]string{"skillId": "string (required)", "templateDigest": "string (required)", "approved": "bool (required)"}},
+		{Name: "sweepSkills", Description: "Re-apply demotion and retirement to every skill.", Handler: p.sweepSkills,
+			ArgsSchema: map[string]string{}},
 	}
 }
```

```diff
diff --git a/packs/gamespack/dsl/builtins.memql b/packs/gamespack/dsl/builtins.memql
--- a/packs/gamespack/dsl/builtins.memql
+++ b/packs/gamespack/dsl/builtins.memql
@@ -89,3 +89,43 @@ builtin gamesRecordTrace {
   say            string     @description("What the character said when it took the plan on.")
   skillId        string     @description("The skill this plan ran, when it was a skill run.")
 }
+
+/// Compare a model's plan with the skills in shadow for its goal.
+@sdk
+@executor("integration.games.shadowCompare")
+@args(profile="object")
+@description("Compare the model's plan for a goal with every shadow skill of the signature that has the same action sequence. A plan whose arguments bind the skill's holes is a match and counts toward promotion: five consecutive matches across at least two distinct values of every hole propose it for the owner's approval. A plan with the same actions and a differing constant resets the streak. Answers {compared, matched, proposed: [skillId]}.")
+builtin gamesShadowCompare {
+  goalSignature  string!    @description("The goal signature of the model's plan.")
+  plan           []object!  @description("[{action, args}], the model's plan.")
+}
+
+/// Report how a canary or trusted skill run went.
+@sdk
+@executor("integration.games.reportSkillOutcome")
+@args(profile="object")
+@description("Report one run of a canary or trusted skill. Five consecutive clean canary runs make it trusted; two consecutive failures on canary or trusted send it back to shadow without asking. Answers {skillId, rung, state, reason}.")
+builtin gamesReportSkillOutcome {
+  skillId  string!  @description("The skill that ran.")
+  ok       bool!    @description("Whether every step succeeded.")
+  binding  object   @description("The hole values it ran with, kept as the skill's lastBinding on a clean run.")
+}
+
+/// The owner's one decision on a skill's promotion.
+@sdk
+@executor("integration.games.decidePromotion")
+@args(profile="object")
+@description("Approve or decline the promotion a skill in shadow earned. templateDigest must be the skill's current digest: a decision about one version never promotes another (games_skill_version_changed). Approving moves it to canary; declining keeps it in shadow and it must earn a new proposal. Answers {skillId, rung, state, reason}.")
+builtin gamesDecideSkillPromotion {
+  skillId         string!  @description("The skill.")
+  templateDigest  string!  @description("The digest of the version being decided.")
+  approved        bool!    @description("The decision.")
+}
+
+/// Re-apply the ladder's demotion and retirement rules to every skill.
+@sdk
+@executor("integration.games.sweepSkills")
+@args(profile="object")
+@description("Re-apply the ladder's demotion rules under the current v1:authoring:ladderPolicy values and retire every skill unused for longer than its retirement window (30 days by default). Call it at runtime start and daily. Answers {examined, demoted, retired}.")
+builtin gamesSweepSkills {
+}
```

- [ ] **Step 8: Run the tests to see them pass**

Run: `go vet github.com/znasllc-io/memql/packs/gamespack/ && go test -count=1 github.com/znasllc-io/memql/packs/gamespack/ && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v -run 'TestLiveSkillClimbs|TestLiveConcurrent|TestLiveRetired' github.com/znasllc-io/memql/packs/gamespack/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```text
--- PASS: TestLiveSkillClimbsTheLadder
--- PASS: TestLiveConcurrentOutcomesBothCount
--- PASS: TestLiveRetiredSkillIsRelearnedAsANewGeneration
ok  	github.com/znasllc-io/memql/packs/gamespack
```

Run: `git add packs/gamespack/store_internal.go && go test -count=1 -run TestOnlyAllowlistedPackagesStampInternalOrigin github.com/znasllc-io/memql && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `ok  	github.com/znasllc-io/memql` and `exit=0`.

- [ ] **Step 9: Commit**

```bash
git add packs/gamespack/ladder.go packs/gamespack/ladder_calls.go packs/gamespack/refusals.go \
  packs/gamespack/ladder_test.go packs/gamespack/ladder_db_test.go packs/gamespack/refusals_test.go \
  packs/gamespack/store.go packs/gamespack/store_internal.go packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): the skill ladder, on component/work's state machine" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 9: Find the skill to run instead of asking a model

Three keys, one answer shape, `work.DecideServe`'s verdict on it: by signature the trusted skill else the canary one; by trigger the most recently used trusted skill; by meaning the trusted skill nearest the embedded text at a cosine of at least 0.82.

**Files:**
- Create: `packs/gamespack/lookup.go`, `packs/gamespack/lookup_test.go`, `packs/gamespack/lookup_db_test.go`
- Modify: `packs/gamespack/capabilities.go` (append `findSkill`), `packs/gamespack/dsl/builtins.memql` (append `gamesFindSkill`)

**Interfaces:**
- Consumes: Task 8's `serveOf`, `climbToCanary`; Task 5's `isNoRows`; Task 3's `embeddingSpace`, `vectorLiteral`, `ownerKey`, `bare`; queries `gamesSkillsForSignature`, `gamesTrustedSkills`, `gamesSkillById`.
- Produces: `defaultMinSimilarity = 0.82`, `nearestSkillSQL` (`staged-data: INDIFFERENT`), `(*Provider).findSkill` (exactly one of `goalSignature`, `trigger`, `text`, else `games_invalid_argument`), `bestServable(rows []map[string]any) map[string]any`, `(*Provider).nearestSkill`, `skillAnswer(row map[string]any, similarity float64) ([]memorynodes.MemoryNode, error)` (concept `integration:games:skillMatch`, bare `id`, adds `serve`, `standby`, `serveReason`, `similarity`).

- [ ] **Step 1: Write the failing tests**

Create `packs/gamespack/lookup_test.go`:

```go
package gamespack

import "testing"

func TestBestServablePrefersTrustedThenRecentUse(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "rung": "shadow"},
		{"id": "b", "rung": "canary", "lastReplayAt": "2026-10-18T12:00:00Z"},
		{"id": "c", "rung": "trusted", "lastReplayAt": "2026-10-01T12:00:00Z"},
		{"id": "d", "rung": "trusted", "lastReplayAt": "2026-10-17T12:00:00Z"},
	}
	if best := bestServable(rows); best == nil || best["id"] != "d" {
		t.Fatalf("best = %v, want d", best)
	}
	if best := bestServable(rows[:1]); best != nil {
		t.Fatalf("a shadow skill never serves, got %v", best)
	}
}
```

Create `packs/gamespack/lookup_db_test.go`:

```go
package gamespack

import (
	"strings"
	"testing"
)

// A canary skill serves by signature with the model standing by and is not
// reachable without a model; a trusted one is found by its trigger and by
// meaning, and an unrelated message matches nothing.
func TestLiveSkillIsFoundBySignatureTriggerAndMeaning(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-lookup-" + n)
	skillID, sig := climbToCanary(t, lg, ctx, "l-"+n)

	served := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig})
	if len(served) != 1 || served[0]["id"] != skillID || served[0]["serve"] != "skill" || served[0]["standby"] != true {
		t.Fatalf("a canary skill serves by signature with the model standing by: %v", served)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"trigger": "initiative"}); len(got) != 0 {
		t.Fatalf("only a trusted skill runs with no model asked; a canary one was found by trigger: %v", got)
	}

	for i := 0; i < 5; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true})
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"trigger": "initiative"}); len(got) != 1 || got[0]["id"] != skillID || got[0]["serve"] != "skill" || got[0]["standby"] != false {
		t.Fatalf("a trusted skill must be found by its trigger and serve alone: %v", got)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": "light up the area"}); len(got) != 1 || got[0]["id"] != skillID || asFloat(got[0]["similarity"], 0) < defaultMinSimilarity {
		t.Fatalf("a trusted skill must be found by meaning: %v", got)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": "go fishing by the river"}); len(got) != 0 {
		t.Fatalf("an unrelated message matched a skill: %v", got)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig, "trigger": "initiative"}); err == nil || !strings.Contains(err.Error(), "games_invalid_argument") {
		t.Fatalf("two keys must be refused, got %v", err)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL:

```text
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/lookup_db_test.go:31:182: undefined: defaultMinSimilarity
packs/gamespack/lookup_test.go:12:13: undefined: bestServable
packs/gamespack/lookup_test.go:15:13: undefined: bestServable
FAIL	github.com/znasllc-io/memql/packs/gamespack [build failed]
FAIL
```

- [ ] **Step 3: Write `lookup.go`**

```go
package gamespack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/work"
)

// lookup.go -- gamesFindSkill: which skill to run instead of asking a model.
//
// THREE KEYS, ONE ANSWER SHAPE. By goal signature, after a model planned a
// goal: the trusted skill, else the canary one (work.DecideServe says canary
// serves with the model standing by). By trigger, for an event or an
// initiative with no model call at all: the most recently used trusted skill
// learned under it. By meaning, for an addressed message: the trusted skill
// whose goal text is nearest, at or above the near-match floor -- 0.82, the
// engine's own near-match threshold (integrations/planner).

// defaultMinSimilarity is the near-match floor.
const defaultMinSimilarity = 0.82

// nearestSkillSQL finds the caller's trusted skill nearest a query vector.
// %s is the vector table.
//
//	$1 canonical owner  $2 query vector
//
// staged-data: INDIFFERENT -- it pins v1:games:skill, an EMBEDDED pack
// concept; staging withholds promoted authored concepts only (memql#3980),
// and no authored bundle can declare one in this namespace.
const nearestSkillSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:games:skill'
      AND (payload->>'ownerUserId') = $1
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id, 1 - (nv.embedding <=> $2::vector) AS similarity
FROM latest
JOIN %s nv ON nv.id = latest.id AND nv.vector_field = 'content'
WHERE latest.payload->>'rung' = 'trusted'
ORDER BY nv.embedding <=> $2::vector
LIMIT 1
`

func (p *Provider) findSkill(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	signature := trimmed(args["goalSignature"])
	trigger := trimmed(args["trigger"])
	text := trimmed(args["text"])
	keys := 0
	for _, k := range []string{signature, trigger, text} {
		if k != "" {
			keys++
		}
	}
	if keys != 1 {
		return nil, fmt.Errorf("games_invalid_argument: pass exactly one of goalSignature, trigger and text")
	}
	switch {
	case signature != "":
		rows, err := p.store.query(ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
		if err != nil {
			return nil, err
		}
		if best := bestServable(rows); best != nil {
			return skillAnswer(best, 0)
		}
		return nil, nil
	case trigger != "":
		rows, err := p.store.query(ctx, "gamesTrustedSkills", nil)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			for _, t := range asStrings(r["triggers"]) {
				if t == trigger {
					return skillAnswer(r, 0)
				}
			}
		}
		return nil, nil
	default:
		return p.nearestSkill(ctx, owner, text, asFloat(args["minSimilarity"], defaultMinSimilarity))
	}
}

// bestServable is the skill a signature lookup serves: trusted before canary,
// and within a rung the most recently used. Shadow and retired never serve.
func bestServable(rows []map[string]any) map[string]any {
	var best map[string]any
	rank := func(r map[string]any) int {
		switch trimmed(r["rung"]) {
		case string(work.RungTrusted):
			return 2
		case string(work.RungCanary):
			return 1
		}
		return 0
	}
	for _, r := range rows {
		if rank(r) == 0 {
			continue
		}
		if best == nil || rank(r) > rank(best) ||
			(rank(r) == rank(best) && timeOf(r["lastReplayAt"]).After(timeOf(best["lastReplayAt"]))) {
			best = r
		}
	}
	return best
}

// nearestSkill embeds the text on the caller's own lane -- a reply is waiting
// on it -- and answers the trusted skill nearest it at or above the floor.
func (p *Provider) nearestSkill(ctx context.Context, owner, text string, floor float64) ([]memorynodes.MemoryNode, error) {
	table, err := p.space.table(ctx)
	if err != nil {
		return nil, err
	}
	if table == "" {
		return nil, nil
	}
	vec, err := p.space.embedQuery(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("games: the text could not be embedded, so no skill can be matched by meaning: %w", err)
	}
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	var (
		id         string
		similarity float64
	)
	err = db.QueryRowContext(ctx, fmt.Sprintf(nearestSkillSQL, table), ownerKey(owner), vectorLiteral(vec)).Scan(&id, &similarity)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("games: nearest skill: %w", err)
	}
	if similarity < floor {
		return nil, nil
	}
	rows, err := p.store.query(ctx, "gamesSkillById", map[string]any{"skillId": bare(id)})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return skillAnswer(rows[0], similarity)
}

// skillAnswer renders a skill row plus the lookup's verdict.
func skillAnswer(row map[string]any, similarity float64) ([]memorynodes.MemoryNode, error) {
	rung, _ := work.ParseRung(trimmed(row["rung"]))
	serve, standby, reason := serveOf(rung)
	out := make(map[string]any, len(row)+4)
	for k, v := range row {
		out[k] = v
	}
	out["id"] = bare(trimmed(row["id"]))
	out["serve"] = serve
	out["standby"] = standby
	if reason != "" {
		out["serveReason"] = reason
	}
	if similarity > 0 {
		out["similarity"] = similarity
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        bare(trimmed(row["id"])),
		Concept:   "integration:games:skillMatch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}
```

- [ ] **Step 4: Declare the capability and the builtin**

```diff
diff --git a/packs/gamespack/capabilities.go b/packs/gamespack/capabilities.go
--- a/packs/gamespack/capabilities.go
+++ b/packs/gamespack/capabilities.go
@@ -25,5 +25,7 @@ func (p *Provider) Capabilities() []memql.IntegrationCapability {
 			ArgsSchema: map[string]string{"skillId": "string (required)", "templateDigest": "string (required)", "approved": "bool (required)"}},
 		{Name: "sweepSkills", Description: "Re-apply demotion and retirement to every skill.", Handler: p.sweepSkills,
 			ArgsSchema: map[string]string{}},
+		{Name: "findSkill", Description: "Find the skill to run by signature, trigger or meaning.", Handler: p.findSkill,
+			ArgsSchema: map[string]string{"goalSignature": "string", "trigger": "string", "text": "string", "minSimilarity": "number"}},
 	}
 }
```

```diff
diff --git a/packs/gamespack/dsl/builtins.memql b/packs/gamespack/dsl/builtins.memql
--- a/packs/gamespack/dsl/builtins.memql
+++ b/packs/gamespack/dsl/builtins.memql
@@ -129,3 +129,15 @@ builtin gamesDecideSkillPromotion {
 @description("Re-apply the ladder's demotion rules under the current v1:authoring:ladderPolicy values and retire every skill unused for longer than its retirement window (30 days by default). Call it at runtime start and daily. Answers {examined, demoted, retired}.")
 builtin gamesSweepSkills {
 }
+
+/// Find a skill to run instead of asking a model.
+@sdk
+@executor("integration.games.findSkill")
+@args(profile="object")
+@description("Find the skill to run. By goalSignature: the trusted skill of the signature, else the canary one (the runtime asked a model first and runs the skill instead of its plan). By trigger: the most recently used trusted skill learned under it. By text: the trusted skill whose goal text is nearest the embedded text, when the cosine is at least minSimilarity (default 0.82). Exactly one of goalSignature, trigger and text. Answers one row {skill fields..., serve: skill|model, standby, serveReason, similarity} or none. A text lookup fails while the text cannot be embedded; treat that as no match.")
+builtin gamesFindSkill {
+  goalSignature  string  @description("Look up by goal signature.")
+  trigger        string  @description("Look up by trigger.")
+  text           string  @description("Look up by meaning: an addressed message or a goal text.")
+  minSimilarity  number  @description("The near-match floor for a text lookup. Default 0.82.")
+}
```

- [ ] **Step 5: Run the whole package, unit and database**

Run: `go vet github.com/znasllc-io/memql/packs/gamespack/ && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 -v github.com/znasllc-io/memql/packs/gamespack/ > /tmp/games-pack-test.txt 2>&1; echo "exit=$?"; grep -c -- '--- PASS' /tmp/games-pack-test.txt; grep -c -- '--- FAIL\|--- SKIP' /tmp/games-pack-test.txt`
Expected: `exit=0`, then `44` (every test of the package passed), then `0` (nothing failed and, with `MEMQL_REQUIRE_DB=1`, nothing skipped).

Run: `go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `exit=0`.

- [ ] **Step 6: Commit**

```bash
git add packs/gamespack/lookup.go packs/gamespack/lookup_test.go packs/gamespack/lookup_db_test.go \
  packs/gamespack/capabilities.go packs/gamespack/dsl/builtins.memql
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(games): find the skill to run instead of asking a model" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 10: A DSL-settable lane for `ai()` (engine seam 1)

`builtin ai` gains `lane enum("interactive", "background")`. `background` does the two things a background agent turn already does (`integrations/agent/nonstreaming.go:133,271`): its model calls count against the background rate bucket (`memql.ContextWithBackgroundLane`, `component/memql/ai_guard.go:82`), and the request carries `airoute.TagBackground`. Under the shipped rules that tag selects the same chain as the default rule, so a fast-level prompt keeps `fastLane`; locked rules evaluate before any rule an owner adds, so an owner's rule for the prompt is not consulted for a background-tagged call; and a prompt pinned with `@defaultProvider` keeps its pin. An unknown lane is refused before the runtime is asked. This is the one seam in the milestone (spec 4.7): it is how S3's reflection stays off the decide path's interactive budget.

**Files:**
- Modify: `component/memql/ai_guard.go:89` (export a reader), `component/memql/ai_attribution.go:72,92,132` (`Tags`), `integrations/agents/ai.go:152,189,259` (`promptLane`), `dsl/agents/builtins.memql:174` (`lane`), `docs/public/language/functions.md:744`, `docs/public/ai/llm-cost-control.md:527`
- Test: `component/memql/ai_attribution_test.go`, `integrations/agents/ai_test.go`

**Interfaces:**
- Consumes: `memql.ContextWithBackgroundLane(ctx)`, `memql.AICallAttributionFromContext(ctx) (AICallAttribution, bool)`, `memql.ContextWithAICallAttribution(ctx, AICallAttribution)`, `applyCallAttribution(ctx, airoute.ResolveRequest) airoute.ResolveRequest` (`component/memql/ai_attribution.go:108`), `airoute.TagBackground`, `(airoute.ResolveRequest).HasTag(string) bool`.
- Produces: `memql.BackgroundLaneFromContext(ctx) bool` (exported for in-process callers' tests); `AICallAttribution.Tags []string`, added to the request without duplicates and never replacing the call site's own tags; `promptLane(ctx, templateId string, raw any) (context.Context, error)` in `integrations/agents`; the `lane` argument of `builtin ai`.

- [ ] **Step 1: Write the failing tests**

Append to `component/memql/ai_attribution_test.go`:

```diff
diff --git a/component/memql/ai_attribution_test.go b/component/memql/ai_attribution_test.go
--- a/component/memql/ai_attribution_test.go
+++ b/component/memql/ai_attribution_test.go
@@ -171,3 +171,18 @@ func TestEmptyAttributionDoesNotShadow(t *testing.T) {
 		t.Fatalf("requestId=%q; an empty stamp overwrote a richer one", req.RequestId)
 	}
 }
+
+// A STAMPED TAG IS ADDED, NEVER A REPLACEMENT (games pack design record, seam
+// 1): the `ai` builtin's background lane stamps airoute.TagBackground, and a
+// tag the call site set itself survives beside it, once.
+func TestStampedTagsAreAddedToTheRequest(t *testing.T) {
+	ctx := ContextWithAICallAttribution(context.Background(), AICallAttribution{Tags: []string{airoute.TagBackground}})
+	req := applyCallAttribution(ctx, airoute.ResolveRequest{Tags: []string{"own"}})
+	if !req.HasTag("own") || !req.HasTag(airoute.TagBackground) {
+		t.Fatalf("tags=%v, want the call site's and the stamped one", req.Tags)
+	}
+	again := applyCallAttribution(ctx, airoute.ResolveRequest{Tags: []string{airoute.TagBackground}})
+	if len(again.Tags) != 1 {
+		t.Fatalf("tags=%v, a tag already present must not be added twice", again.Tags)
+	}
+}
```

Change `integrations/agents/ai_test.go` (the recording engine keeps the context it was called with, then three tests):

```diff
diff --git a/integrations/agents/ai_test.go b/integrations/agents/ai_test.go
--- a/integrations/agents/ai_test.go
+++ b/integrations/agents/ai_test.go
@@ -20,10 +20,12 @@ import (
 	"encoding/json"
 	"errors"
 	"fmt"
+	"slices"
 	"strings"
 	"testing"
 
 	"github.com/znasllc-io/memql/component/memql"
+	"github.com/znasllc-io/memql/core/airoute"
 )
 
 // promptRecordingEngine is the AI runtime as this executor sees it: one method that
@@ -33,13 +35,15 @@ type promptRecordingEngine struct {
 
 	gotTemplate string
 	gotData     map[string]any
+	gotCtx      context.Context
 	calls       int
 	reply       any
 	err         error
 }
 
-func (r *promptRecordingEngine) InvokeAI(_ context.Context, templateId string, data map[string]any) (any, error) {
+func (r *promptRecordingEngine) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
 	r.calls++
+	r.gotCtx = ctx
 	r.gotTemplate = templateId
 	r.gotData = data
 	return r.reply, r.err
@@ -278,3 +282,48 @@ func TestStructuredPromptRejectsUndecodableObject(t *testing.T) {
 		}
 	}
 }
+
+// THE LANE (games pack design record, seam 1). A background call counts
+// against the background rate bucket and carries the background routing tag;
+// no lane is the interactive lane; anything else is refused before the
+// runtime is asked.
+func TestTheBackgroundLaneReachesTheRuntime(t *testing.T) {
+	engine := &promptRecordingEngine{reply: "ok"}
+	if _, err := callAi(t, engine, map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": "background"}); err != nil {
+		t.Fatal(err)
+	}
+	if !memql.BackgroundLaneFromContext(engine.gotCtx) {
+		t.Fatal("a background ai() call reached the runtime on the interactive lane: its model calls would spend the interactive rate budget")
+	}
+	stamped, _ := memql.AICallAttributionFromContext(engine.gotCtx)
+	if !slices.Contains(stamped.Tags, airoute.TagBackground) {
+		t.Fatalf("a background ai() call must carry the %q tag the backgroundLane rule routes on, got %v", airoute.TagBackground, stamped.Tags)
+	}
+}
+
+func TestTheInteractiveLaneIsTheDefault(t *testing.T) {
+	for _, lane := range []any{nil, "interactive"} {
+		engine := &promptRecordingEngine{reply: "ok"}
+		args := map[string]any{"templateId": "decide", "data": map[string]any{}}
+		if lane != nil {
+			args["lane"] = lane
+		}
+		if _, err := callAi(t, engine, args); err != nil {
+			t.Fatal(err)
+		}
+		if memql.BackgroundLaneFromContext(engine.gotCtx) {
+			t.Fatalf("lane %v put the call on the background lane", lane)
+		}
+	}
+}
+
+func TestAnUnknownLaneIsRefused(t *testing.T) {
+	engine := &promptRecordingEngine{reply: "ok"}
+	_, err := callAi(t, engine, map[string]any{"templateId": "reflect", "data": map[string]any{}, "lane": "bg"})
+	if err == nil || !strings.Contains(err.Error(), "lane must be interactive or background") {
+		t.Fatalf("an unknown lane must be refused by name, got %v", err)
+	}
+	if engine.calls != 0 {
+		t.Fatal("the runtime was asked although the lane was refused")
+	}
+}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 -run 'TestTheBackgroundLane|TestTheInteractiveLane|TestAnUnknownLane|TestStampedTags' github.com/znasllc-io/memql/integrations/agents/ github.com/znasllc-io/memql/component/memql/`
Expected: FAIL, neither package builds:

```text
# github.com/znasllc-io/memql/component/memql [github.com/znasllc-io/memql/component/memql.test]
component/memql/ai_attribution_test.go:179:78: unknown field Tags in struct literal of type AICallAttribution
# github.com/znasllc-io/memql/integrations/agents [github.com/znasllc-io/memql/integrations/agents.test]
integrations/agents/ai_test.go:295:12: undefined: memql.BackgroundLaneFromContext
integrations/agents/ai_test.go:299:30: stamped.Tags undefined (type memql.AICallAttribution has no field or method Tags)
integrations/agents/ai_test.go:300:133: stamped.Tags undefined (type memql.AICallAttribution has no field or method Tags)
integrations/agents/ai_test.go:314:12: undefined: memql.BackgroundLaneFromContext
FAIL	github.com/znasllc-io/memql/integrations/agents [build failed]
FAIL	github.com/znasllc-io/memql/component/memql [build failed]
FAIL
```

- [ ] **Step 3: Export the lane reader and carry tags on the attribution**

```diff
diff --git a/component/memql/ai_guard.go b/component/memql/ai_guard.go
--- a/component/memql/ai_guard.go
+++ b/component/memql/ai_guard.go
@@ -86,6 +86,13 @@ func ContextWithBackgroundLane(ctx context.Context) context.Context {
 	return context.WithValue(ctx, laneCtxKey{}, true)
 }
 
+// (as built: the existing unexported reader is renamed to the exported
+// BackgroundLaneFromContext, every caller changed with it; no forwarding
+// wrapper is added, and the doc comment states why it is exported: a caller
+// that promises the background lane is tested against a fake engine.)
+
 // backgroundLaneFromContext reports whether ctx was tagged as the
 // background execution lane. Defaults to false (interactive) for any
 // context that wasn't stamped -- so chat, voice, suggest, and every legacy
```

```diff
diff --git a/component/memql/ai_attribution.go b/component/memql/ai_attribution.go
--- a/component/memql/ai_attribution.go
+++ b/component/memql/ai_attribution.go
@@ -69,6 +69,12 @@ type AICallAttribution struct {
 	// Touches is the call's footprint: the concept ids or knowledge-domain
 	// ids the call is about. A rule matches it with startsWith semantics.
 	Touches []string
+	// Tags are call tags a rule may branch on (core/airoute's TagBackground).
+	// The `ai` builtin's background lane stamps TagBackground here, so a
+	// background prompt call is routed by the shipped backgroundLane rule
+	// exactly as a background agent turn is -- unless a pin wins, as a pin
+	// always does.
+	Tags []string
 }
 
 type aiCallAttributionKey struct{}
@@ -83,7 +89,7 @@ func ContextWithAICallAttribution(ctx context.Context, a AICallAttribution) cont
 	if ctx == nil {
 		ctx = context.Background()
 	}
-	if strings.TrimSpace(a.RequestId) == "" && strings.TrimSpace(a.AgentId) == "" && len(a.Touches) == 0 {
+	if strings.TrimSpace(a.RequestId) == "" && strings.TrimSpace(a.AgentId) == "" && len(a.Touches) == 0 && len(a.Tags) == 0 {
 		return ctx
 	}
 	return context.WithValue(ctx, aiCallAttributionKey{}, a)
@@ -123,6 +129,13 @@ func applyCallAttribution(ctx context.Context, req airoute.ResolveRequest) airou
 		// record that outlives the caller's slice.
 		req.Touches = append([]string(nil), stamped.Touches...)
 	}
+	// TAGS ARE ADDED, NEVER REPLACED: a tag the call site set is something
+	// it knows, and a stamped one is one more condition a rule may read.
+	for _, tag := range stamped.Tags {
+		if tag = strings.TrimSpace(tag); tag != "" && !req.HasTag(tag) {
+			req.Tags = append(append([]string(nil), req.Tags...), tag)
+		}
+	}
 
 	// THE RUN AND STEP ARE THE SESSION DOOR'S HINGE (design D7) and nothing
 	// filled them for a prompt call, so an app door resolved for a tool-needing
```

- [ ] **Step 4: Apply the lane in the `ai` executor and declare it**

```diff
diff --git a/integrations/agents/ai.go b/integrations/agents/ai.go
--- a/integrations/agents/ai.go
+++ b/integrations/agents/ai.go
@@ -149,6 +149,8 @@ import (
 	"time"
 
 	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
+	"github.com/znasllc-io/memql/component/memql"
+	"github.com/znasllc-io/memql/core/airoute"
 )
 
 // invokePromptCapName is the capability the `ai` builtin's @executor names.
@@ -184,6 +186,9 @@ func (i *Integration) handleInvokePrompt(ctx context.Context, args map[string]an
 		// fact; the layers below name their own causes.
 		return nil, fmt.Errorf("ai(%q): no engine handle on this node, so the AI runtime is unreachable and the prompt cannot be called", templateId)
 	}
+	if ctx, err = promptLane(ctx, templateId, args["lane"]); err != nil {
+		return nil, err
+	}
 
 	// THE LIVE SEAM. Everything a prompt call is -- resolve the prompt,
 	// validate the data against its body, render, route by @level through
@@ -251,6 +256,32 @@ func (i *Integration) handleInvokePrompt(ctx context.Context, args map[string]an
 	}}, nil
 }
 
+// promptLane applies the call's LANE (design record
+// docs/superpowers/specs/2026-10-18-games-pack-design.md, seam 1).
+//
+// "interactive", or no lane, is every call as it was. "background" is a call
+// nobody is waiting on -- a reflection, a summary written after the fact --
+// and it is the same two things the background agent lane is
+// (integrations/agent/nonstreaming.go): its model calls count against the
+// BACKGROUND rate bucket (memql.ContextWithBackgroundLane, ai_guard.go), so a
+// burst of them can never spend the interactive budget a person's request
+// needs, and the request carries airoute.TagBackground, so the shipped
+// backgroundLane rule routes it unless a pin wins. An unknown lane is
+// REFUSED: a typo that silently ran a reflection in the interactive bucket is
+// the failure the lane exists to prevent.
+func promptLane(ctx context.Context, templateId string, raw any) (context.Context, error) {
+	switch lane := strings.TrimSpace(asString(raw)); lane {
+	case "", "interactive":
+		return ctx, nil
+	case "background":
+		stamped, _ := memql.AICallAttributionFromContext(ctx)
+		stamped.Tags = append(append([]string(nil), stamped.Tags...), airoute.TagBackground)
+		return memql.ContextWithAICallAttribution(memql.ContextWithBackgroundLane(ctx), stamped), nil
+	default:
+		return ctx, fmt.Errorf("ai(%q): lane must be interactive or background, not %q", templateId, lane)
+	}
+}
+
 // promptData coerces the `data` argument to the map InvokeAI takes.
 //
 // A MISSING data object is refused rather than sent as {}. The prompt's body
```

```diff
diff --git a/dsl/agents/builtins.memql b/dsl/agents/builtins.memql
--- a/dsl/agents/builtins.memql
+++ b/dsl/agents/builtins.memql
@@ -171,6 +171,7 @@ builtin ai {
   data        object!
   responseSchema object @description("Optional JSON Schema object for a strict structured response. Routing, metering and the work journal are preserved; reply contains the JSON text.")
   progress    bool @description("Persist public output snapshots for an owned work run as the model generates them. No private reasoning is recorded.")
+  lane        enum("interactive", "background") @description("interactive (the default) for a call someone is waiting on. background for one nobody is -- a reflection, a summary written after the fact: its model calls count against the background rate bucket instead of the interactive one, and the request carries the background tag the shipped backgroundLane rule routes on. A pinned prompt keeps its pin.")
 }
 
 // ---- builtin requestUserFeedback -------------------------------------
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go vet github.com/znasllc-io/memql/integrations/agents/ && go test -count=1 github.com/znasllc-io/memql/integrations/agents/ && go test -count=1 -run 'Attribution|Lane|Tags|Stamped' github.com/znasllc-io/memql/component/memql/`
Expected: `ok  	github.com/znasllc-io/memql/integrations/agents` and `ok  	github.com/znasllc-io/memql/component/memql`.

- [ ] **Step 6: Document the lane where the feature is described**

```diff
diff --git a/docs/public/language/functions.md b/docs/public/language/functions.md
--- a/docs/public/language/functions.md
+++ b/docs/public/language/functions.md
@@ -741,7 +741,7 @@ domain that declares it (`use agents.builtins.{ ai }`). See
 
 | Builtin | Description | Called as |
 |---------|-------------|-----------|
-| `ai` | Calls one named prompt with a data object and returns `{prompt, reply}`. Optional `responseSchema` constrains `reply` to JSON matching that schema through structured model routing and adds the decoded object as `structured` for DSL field access. `progress: true` records public lifecycle and the completed result; partial text is available only for unconstrained streaming. Synchronous; the prompt's `@level` and the routing rules choose the model, so the call never names one | `builtin ai(templateId: "docSummary", data: { content: args.content })` |
+| `ai` | Calls one named prompt with a data object and returns `{prompt, reply}`. Optional `responseSchema` constrains `reply` to JSON matching that schema through structured model routing and adds the decoded object as `structured` for DSL field access. `progress: true` records public lifecycle and the completed result; partial text is available only for unconstrained streaming. `lane: "background"` is for a call nobody is waiting on (a reflection, a summary written after the fact): its model calls count against the background rate bucket, and the request carries the `background` tag the shipped `backgroundLane` rule routes on, unless the prompt pins a provider. Omitted or `"interactive"` is the default; any other lane is refused. Synchronous; the prompt's `@level` and the routing rules choose the model, so the call never names one | `builtin ai(templateId: "docSummary", data: { content: args.content })` |
 | `agent` | Opens a `v1:work:goal` naming the `invokeAgent` template and returns `{goalId, runId}`; a run dispatcher claims it on an agent node | `builtin agent(name: "assistant", prompt: args.question, partitionId: "system")` |
 | `runAgentTurn` | Runs one agent turn in line and returns its reply. Answers only on an agent node | `builtin runAgentTurn(agentId: args.agentId, prompt: args.question)` |
 
```

```diff
diff --git a/docs/public/ai/llm-cost-control.md b/docs/public/ai/llm-cost-control.md
--- a/docs/public/ai/llm-cost-control.md
+++ b/docs/public/ai/llm-cost-control.md
@@ -524,6 +524,13 @@ ceiling as no-spend: the ordinary numeric ceiling uses zero to mean unset.
 | `MEMQL_LLM_RATE_WINDOW_SECONDS` | 10 |
 | `MEMQL_LLM_BG_MAX_CALLS_PER_WINDOW` | 40 |
 
+The second bucket is the background lane's. A call is on it when Go marks its
+context with `memql.ContextWithBackgroundLane` (the agent's non-streaming
+loop does) or when DSL calls `builtin ai(..., lane: "background")`. That lane
+also tags the request `background`, which the shipped `backgroundLane` rule
+matches. The bucket is the effect that always applies; the tag does not
+change a pinned prompt's routing, and an owner's rule for the prompt is not
+consulted. A burst of reflections never spends the interactive budget a
+person's request needs.
+
 ### Layer 2 — automation budget
 | env | default |
 |---|---|
```

Run: `go test -count=1 -run 'Docs|Doc' github.com/znasllc-io/memql && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `ok  	github.com/znasllc-io/memql` and `exit=0`.

- [ ] **Step 7: Commit**

```bash
git add component/memql/ai_guard.go component/memql/ai_attribution.go component/memql/ai_attribution_test.go \
  integrations/agents/ai.go integrations/agents/ai_test.go dsl/agents/builtins.memql \
  docs/public/language/functions.md docs/public/ai/llm-cost-control.md
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(agents): a DSL-settable lane for ai()" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 11: Consuming the games pack, documented

One public page for a runtime author, built from spec section 5, and its index line. It names no game, product or character, links only public pages, and carries no ```` ```memql ```` block (the snippet gate loads those; the calls shown are client query strings, labelled `text`).

**Files:**
- Create: `docs/public/build/consuming-the-games-pack.md`
- Modify: `GLOSSARY.md:63`

**Interfaces:**
- Consumes: the names and answers of spec section 5 exactly as Tasks 1-10 implemented them.
- Produces: the public page; `sinceVersion` is the release this ships in (the `VERSION` file reads `0.29.31` on the base commit, so `0.29.32`; re-read `VERSION` before merging and bump if a release was cut meanwhile).

- [ ] **Step 1: Write the page**

Create `docs/public/build/consuming-the-games-pack.md`:

````markdown
---
title: Consuming the games pack from a game runtime
audience: public
status: stable
area: build
sinceVersion: 0.29.32
owner: znas
---

# Consuming the games pack from a game runtime

The **games pack** (`packs/gamespack`, domain `games`) gives a game runtime
that runs **outside** the cluster two things: a **memory** of the people who
play, and **skills** learned from the plans the runtime executed. The runtime
executes every action itself. The engine stores, ranks, generalizes and
governs; it never runs a game action.

If you have not read [Building a pack](building-a-pack.md), read it first. A
**pack** is a client-agnostic product feature, the third of the [three
extension words](../concepts/component-integration-pack.md).

> **The pack ships disabled.** It stores memories about the people who play,
> and holding that data is the owner's decision. Enable it in **Cluster →
> Modules → games**, then restart every node: enablement is read once, at
> boot. Until then its concepts load and nothing else does, and a call answers
> `function "<name>" not found`.

## Who calls, and whose rows they are

Every concept is on the owned tier with the cluster-owner read arm, and every
write stamps `ownerUserId` from the caller. The runtime and the owner's own
pages therefore sign in **as the owner** and read and write the same rows. A
live subscription on one of the pack's concepts reaches the owner's streams
and a cluster owner's, and nobody else's.

Three writers are server-only: plan traces, skills and the skill ladder are
written by the pack's builtins, never by a client.

## What you are adopting

| Concept | What it is | Retention |
|---|---|---|
| `v1:games:player` | A person the owner registered: `gamertag`, `displayName`, `consented` | newest version kept; older versions expire after 30 days |
| `v1:games:episode` | One remembered moment: `characterId`, `playerId?`, `worldId`, `at`, `kind` (`chat`, `event`, `activity`, `session`), `summary`, `importance` 1-10 | expires whole after 30 days |
| `v1:games:preference` | What a player likes: `playerId`, `subject`, `valence` -1..1, `confidence` 0..1, `evidence` | newest version kept |
| `v1:games:promise` | What a character promised a player: `characterId`, `playerId`, `text`, `status` (`open`, `kept`, `dropped`), `madeAt` | newest version kept |
| `v1:games:place` | A place in a world, shared by every character: `worldId`, `name`, `kind`, `position`, `discoveredBy?`, `at` | newest version kept |
| `v1:games:skill` | A learned skill and its place on the ladder | newest version kept |
| `v1:games:planTrace` | One executed plan, the evidence skills are learned from | expires whole after 30 days |

`characterId` and `worldId` are your runtime's own stable slugs. Retention is
the engine's hourly sweep; your runtime prunes nothing.

**Ids are yours, bare, at most 64 characters, and unique per concept across
the whole cluster.** An insert on an existing id writes a new version of that
row, so derive ids that cannot collide with another owner's: put the owner's
token subject in the hash. With `h(x)` the first 20 hex digits of
`sha256(x)`:

| Id | Derivation |
|---|---|
| `playerId` | `"p-" + h(sub + "|" + lower(gamertag))` |
| `preferenceId` | `"pf-" + h(sub + "|" + playerId + "|" + normalize(subject))` |
| `placeId` | `"pl-" + h(sub + "|" + worldId + "|" + normalize(name))` |
| episode, promise, trace | a prefix (`e-`, `pr-`, `t-`) and 32 random hex digits |

`normalize` is the goal-signature normalization below. Skill ids are the
engine's.

## Memory

**Record.** `builtin gamesRecordEpisodes(episodes: [...])` writes up to 50
episodes, then embeds each summary on the **background** rate bucket, so a
burst of memories never spends the interactive budget a decision needs. It
answers `{written, embedded, pending, ids}`. An embedding that fails leaves
its episode recorded and pending, and `builtin gamesEmbedPending(limit: 25)`
embeds the backlog when the model is back, answering `{embedded, failed,
more}`. Call it until `more` is false; that is also how memory heals after
the cluster's embedder binding changes.

**Recall.** One call returns what a character remembers that matters now:

```text
builtin gamesRecall(characterId: "<character>", playerId: "<player>", worldId: "<world>", text: "<what was said>")
```

Episodes are the character's own or about the player, scored

```text
score = 0.5 x relevance + 0.3 x recency + 0.2 x importance / 10
relevance = max(cosine(query, summary), 0)
recency   = exp(-ln 2 x ageHours / 72), age measured from the episode's `at` to `asOf`
```

and the top `k` (default 8, at most 50) are followed by the player's most
confident preferences (3), the character's open promises to the player (5)
and the world's places (10). Each row is `{rank, kind, id, text, score,
relevance, recency, importance, relevanceAvailable, row}`. **Order by
`rank`**: the order rows arrive in is the transport's. `asOf`,
`halfLifeHours`, the three weights and the three counts are optional
arguments.

With no embedder bound, or while the model cannot embed the query, relevance
is 0 for every episode, recency and importance still rank them, and every row
says `relevanceAvailable: false`. Recall never fails because a model is away.

**Forget.** `builtin gamesForgetPlayer(playerId: "<player>")` deletes, in
every version, the player, every episode, preference and promise about them,
their vectors, and every plan trace whose text mentions their gamertag. Only
the caller's own rows. Skills are kept. It answers `{playerId, deleted:
{episodes, preferences, promises, planTraces, players, vectors}}`.

The plain writes are mutations: `upsertGamesPlayer`, `createGamesEpisode`,
`upsertGamesPreference`, `upsertGamesPromise`, `setGamesPromiseStatus` and
`upsertGamesPlace`. The reads are queries: `gamesPlayers`, `gamesPlayerById`,
`gamesPreferencesForPlayer`, `gamesOpenPromises`, `gamesPromisesForPlayer`,
`gamesPlacesInWorld`, `gamesSessionEpisodes` and `gamesEpisodesForPlayer`.

## Skills

A skill is a plan your runtime executed often enough, the same way, that it
can run again without asking a model.

**Record.** After every plan, `builtin gamesRecordTrace(...)` with the
`goalText`, the goal's `inputs`, the `steps` (`{action, args, ok,
durationMs, error}`), the `outcome` (`success`, `failure`, `abandoned`) and
the player's `reaction` (`positive`, `neutral`, `negative`). It answers
`{traceId, goalSignature, learnedSkillId}`.

**The goal signature** is how a goal is recognized again:

```text
normalize(s)  = lower-case; keep letters and digits; a run of whitespace becomes one space; drop everything else; trim
GoalSignature = hex(sha256(normalize(goalText) + "|" + join(sort(input keys), ",")))
```

`GoalSignature("Light up the area!", ["spot"])` is
`d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd`. Only the
input KEYS enter it. Pass your own as `goalSignature` and the engine checks
it: a mismatch is refused with `games_signature_mismatch` on the first trace,
rather than never matching anything.

**Learn.** When two successful traces of one signature, neither disliked and
neither a skill run, share an action sequence, the engine generalizes them:
an argument equal in both stays a constant, one that differs becomes a hole
named `<stepIndex>.<argKey>`. The skill is born in **shadow**, once per
goal and action sequence.

```text
template = {"steps": [{"action": "collect", "args": {"block": {"const": "coal"}}},
                      {"action": "craft", "args": {"item": {"const": "torch"}, "count": {"hole": "1.count"}}}],
            "holes": ["1.count"]}
binding  = {"1.count": 5}
```

**The ladder** is the one learned procedures use, under the cluster's ladder
policy, whose defaults are:

| Call | What it does |
|---|---|
| `gamesShadowCompare(goalSignature, plan)` | Your model planned the goal; compare its plan with the shadow skills. Five consecutive matches across at least two values of every hole propose a promotion. Answers `{compared, matched, proposed}`. |
| `gamesDecideSkillPromotion(skillId, templateDigest, approved)` | The owner's one decision, pinned to the version they were shown. Approval makes the skill canary. |
| `gamesReportSkillOutcome(skillId, ok, binding)` | One canary or trusted run. Five clean canary runs make it trusted; two consecutive failures send it back to shadow. |
| `gamesSweepSkills()` | Re-applies demotion, and retires a skill unused for 30 days. Call it at start and daily. |

A skill's `state` is `shadow`, `awaiting_approval`, `canary`, `trusted` or
`retired`. A retired skill is final; the goal coming back is learned again as
a new skill.

**The owner approves, the runtime does not.** The runtime and the owner's
pages are the same identity, so nothing in the engine can tell a person's
click from your runtime calling `gamesDecideSkillPromotion`. Never call it
from the runtime.

**Lookup.** `builtin gamesFindSkill` takes exactly one of:

| Key | Answers |
|---|---|
| `goalSignature` | the trusted skill of the goal, else the canary one; your runtime asked a model first |
| `trigger` | the most recently used trusted skill learned under it; no model asked at all |
| `text` | the trusted skill whose goal text is nearest, at a cosine of at least `minSimilarity` (0.82) |

It answers one row, the skill plus `serve` (`skill` or `model`), `standby`
(a canary skill runs with the model standing by), `serveReason` and, for
`text`, `similarity`, or no row. Fill the holes with your own binding and
execute the steps yourself. A `text` lookup fails while the text cannot be
embedded; treat that as no match.

## Live views

Subscribe with `subscribeGraph` to `v1:games:player`, `episode`,
`preference`, `promise`, `place` and `skill`, actions `created` and
`updated`; the events cross replicas. Plan traces are not forwarded, and
deletes publish nothing, so a live view re-reads after a forget.

## Refusals

Refusals lead with a stable code. Match the code, not the sentence.

| Code | When |
|---|---|
| `games_unauthenticated` | the call carried no signed-in owner |
| `games_invalid_argument` | a required argument is missing or malformed |
| `games_signature_mismatch` | your `goalSignature` is not the engine's |
| `games_skill_not_found` | the skill is not readable by this caller |
| `games_skill_not_serving` | an outcome for a skill that is not canary or trusted |
| `games_skill_version_changed` | a decision about a template digest the skill no longer has |
| `games_no_promotion_open` | a decision for a skill with nothing to decide |
| `games_player_not_found` | the player is not one of this owner's |

Model calls meet the engine's own refusals too; see [LLM cost
control](../ai/llm-cost-control.md).

## Putting reflection on the background lane

Your own prompts reach the model through `builtin ai`. A call nobody is
waiting on, such as a reflection written after a session, belongs on the
background lane: `builtin ai(templateId: "<prompt>", data: {...}, lane:
"background")`. See [Functions](../language/functions.md).
````

- [ ] **Step 2: Index it**

```diff
diff --git a/GLOSSARY.md b/GLOSSARY.md
--- a/GLOSSARY.md
+++ b/GLOSSARY.md
@@ -60,6 +60,7 @@ path, or use this detailed reference index. Layout + rules:
 
 ### Build Against It (`build/`)
 - [Building a Spine](docs/public/build/building-a-spine.md) — selectable DSL planning workflows, immutable run snapshots and bounded native operations
+- [Consuming the games pack](docs/public/build/consuming-the-games-pack.md) — memory and skill learning for a game runtime outside the cluster: recall ranked by relevance, recency and importance, forgetting a player outright, and skills learned from executed plans on the learned-procedure ladder. Ships disabled
 - [Audio Streaming](docs/public/build/audio-streaming.md) · [Build Tags](docs/public/build/build-tags.md) · [Plugin SDK](docs/public/build/plugin-sdk.md) · [Building a Pack](docs/public/build/building-a-pack.md) — worked-example developer guide: the `examples/referencepack` teaching pack, and how a STOREFRONT pack under `packs/` ships in the default build and how a shopper writes to one (epic memql#5532)
 - Generated reference: the concept catalog, rendered from the DSL by `cmd/docs-gen bundle` into each release's docs set as `reference/concepts.md` (never written into `docs/public`). The bundle, its boundary gate and its manifest are [DOCS_STANDARD.md](docs/DOCS_STANDARD.md) section 5.
 
```

- [ ] **Step 3: Run the documentation gates**

Front matter, the public boundary (a relative link from a public page to a non-public one fails), relative links, retired vocabulary, construct names and the bundle build all live in the root package.

Run: `go test -count=1 -run 'Docs|Doc|Glossary' github.com/znasllc-io/memql`
Expected: `ok  	github.com/znasllc-io/memql`

- [ ] **Step 4: Commit**

```bash
git add docs/public/build/consuming-the-games-pack.md GLOSSARY.md
git -c user.name=znas -c user.email=dev@example.com commit -m "docs(games): consuming the games pack from a game runtime" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 12: The pack's rows cross the bff replicas (cluster test)

What only a cluster proves is the hop: rows written through one connection must reach subscribers on ten connections spread across both bff replicas, and recall and learning must work through different connections. The file carries the `clustere2e` build tag, so `make test` compiles none of it; Task 14 runs it.

**Files:**
- Create: `test/clustere2e/games_pack_test.go`

**Interfaces:**
- Consumes: the suite's own helpers in `test/clustere2e/delivery_test.go` (`token(t) string`, which skips without `MEMQL_E2E_TOKEN`; `endpoint()`, `MEMQL_E2E_ENDPOINT`; `openConnections(ctx, t, tok, n) []*memqlclient.Connection`; `connCount = 10`); `memqlclient.NewQueryClient(d).ExecuteNamed(ctx, name, call)`, `memqlclient.NewSubscriptionManager(d).SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{Concept, Actions})`, `modules.NewClient(d).SetPackEnabled(ctx, domain string, enabled bool, reason string)`, `langparser.RenderCall`, `id.NewShortId`.
- Produces: `gamesCall(ctx, t, conn, kind, name string, args map[string]any) []memqlclient.Row`, `gamesEventID(memqlclient.Event) string`, `TestClusterGamesPackSetEnabled` (runs only with `MEMQL_E2E_GAMES_ENABLE=1` or `0`), `TestClusterGamesMemoryCrossesReplicas`, `TestClusterGamesSkillLearnsAcrossReplicas`.

- [ ] **Step 1: Write the cluster test**

Create `test/clustere2e/games_pack_test.go`:

```go
//go:build clustere2e

package clustere2e

// games_pack_test.go -- the games pack on the 2-replica k3d cluster (design
// record docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// The pack's own tests prove its decisions over values and its wiring over a
// real database in one process. What only a cluster can prove is the hop: a
// game runtime writes through whichever bff replica the front door picked,
// and a browser watching the live memory view is attached to whichever one it
// is attached to. These tests write through one connection and observe on
// every connection, so with connCount connections round-robined across two
// replicas a missing routing rule shows as a subscriber that never hears.
//
// THE PACK MUST BE ENABLED and every node restarted since: it ships disabled.
// TestClusterGamesPackSetEnabled is the scripted form of Cluster > Modules >
// games > Enable, and runs only when asked (MEMQL_E2E_GAMES_ENABLE=1 or 0).
//
// RUN through the front door, which spreads connections across the bff
// replicas (a port-forward pins one pod and proves nothing about the hop):
//
//	MEMQL_E2E_ENDPOINT=https://api.memql.localhost MEMQL_E2E_TOKEN=<owner PAT or JWT> \
//	  go test -tags clustere2e -run TestClusterGames -count=1 -timeout=300s \
//	  github.com/znasllc-io/memql/test/clustere2e/

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/modules"
)

// gamesCall runs one named construct ("builtin", "mutation" or "query") on
// one connection.
func gamesCall(ctx context.Context, t *testing.T, conn *memqlclient.Connection, kind, name string, args map[string]any) []memqlclient.Row {
	t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	res, err := memqlclient.NewQueryClient(conn.Dispatcher()).ExecuteNamed(ctx, name, kind+" "+call)
	if err != nil {
		// A disabled pack's constructs are not loaded at all ("function ... not
		// found"); a DSL loaded without its Go half has no executor.
		if strings.Contains(err.Error(), `function "`+name+`" not found`) || strings.Contains(err.Error(), "unknown builtin executor") {
			t.Fatalf("%s is not loaded: enable the games pack (TestClusterGamesPackSetEnabled with MEMQL_E2E_GAMES_ENABLE=1) and restart every node: %v", name, err)
		}
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return res.Rows()
}

// gamesEventID is the bare id a graph event carries, at the top of its payload
// or inside its node (planIDFor's two places).
func gamesEventID(ev memqlclient.Event) string {
	if got, _ := ev.Payload["id"].(string); got != "" {
		return got
	}
	if node, _ := ev.Payload["node"].(map[string]any); node != nil {
		got, _ := node["id"].(string)
		return got
	}
	return ""
}

// TestClusterGamesPackSetEnabled flips the pack, and only when asked: it is an
// operator's act, and the state is left as requested. Every node reads the
// row at its NEXT boot, so a restart must follow.
func TestClusterGamesPackSetEnabled(t *testing.T) {
	want := os.Getenv("MEMQL_E2E_GAMES_ENABLE")
	if want != "1" && want != "0" {
		t.Skip("set MEMQL_E2E_GAMES_ENABLE=1 to enable the games pack, or 0 to disable it")
	}
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn := openConnections(ctx, t, tok, 1)[0]
	defer conn.Close()
	flip, err := modules.NewClient(conn.Dispatcher()).SetPackEnabled(ctx, "games", want == "1", "games pack local end-to-end test")
	if err != nil {
		t.Fatalf("set games pack enabled=%s: %v", want, err)
	}
	t.Logf("games pack: enabled %v -> %v; restart required: %v (kubectl rollout restart every node)", flip.PriorEnabled, flip.Enabled, flip.RestartRequired)
}

// An episode written through one replica reaches a subscriber on every
// replica, and recall through another replica finds it.
func TestClusterGamesMemoryCrossesReplicas(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	episodeID := "e-clustere2e-" + id.NewShortId()
	seen := make([]<-chan string, len(conns))
	for i, c := range conns {
		sm := memqlclient.NewSubscriptionManager(c.Dispatcher())
		_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
			Concept: "v1:games:episode",
			Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
		})
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		ch := make(chan string, 16)
		go func() {
			for ev := range events {
				if got := gamesEventID(ev); got != "" {
					ch <- got
				}
			}
		}()
		seen[i] = ch
	}
	time.Sleep(2 * time.Second) // let every subscription settle before the write

	gamesCall(ctx, t, conns[0], "builtin", "gamesRecordEpisodes", map[string]any{"episodes": []any{map[string]any{
		"episodeId": episodeID, "characterId": "clustere2e", "worldId": "clustere2e",
		"at": time.Now().UTC().Format(time.RFC3339), "kind": "event",
		"summary": "a cluster test watched the sunrise", "importance": 3,
	}}})

	for i, ch := range seen {
		deadline := time.After(20 * time.Second)
	wait:
		for {
			select {
			case got := <-ch:
				if got == episodeID {
					break wait
				}
			case <-deadline:
				t.Fatalf("connection %d never heard episode %s: the v1:games:episode forward rule is missing or the event did not cross replicas", i, episodeID)
			}
		}
	}

	rows := gamesCall(ctx, t, conns[len(conns)-1], "builtin", "gamesRecall", map[string]any{
		"characterId": "clustere2e", "worldId": "clustere2e", "text": "sunrise",
	})
	found := false
	for _, r := range rows {
		if r["kind"] == "episode" && r["id"] == episodeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("recall through another connection did not return episode %s: %v", episodeID, rows)
	}
}

// Two traces through two connections teach one skill, and its creation
// reaches every replica.
func TestClusterGamesSkillLearnsAcrossReplicas(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	goal := "clustere2e goal " + id.NewShortId()
	trace := func(traceID string, count int) map[string]any {
		return map[string]any{
			"traceId": traceID, "characterId": "clustere2e", "goalText": goal, "trigger": "clustere2e",
			"steps": []any{
				map[string]any{"action": "collect", "args": map[string]any{"block": "stone", "count": count}, "ok": true, "durationMs": 10},
			},
			"outcome": "success", "at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	created := make(chan string, 64)
	for _, c := range conns {
		sm := memqlclient.NewSubscriptionManager(c.Dispatcher())
		_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
			Concept: "v1:games:skill",
			Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
		})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		go func() {
			for ev := range events {
				if got := gamesEventID(ev); got != "" {
					created <- got
				}
			}
		}()
	}
	time.Sleep(2 * time.Second)

	gamesCall(ctx, t, conns[0], "builtin", "gamesRecordTrace", trace("t-a-"+id.NewShortId(), 3))
	rows := gamesCall(ctx, t, conns[1], "builtin", "gamesRecordTrace", trace("t-b-"+id.NewShortId(), 5))
	skillID, _ := rows[0]["learnedSkillId"].(string)
	if skillID == "" {
		t.Fatalf("two successful traces through two connections taught no skill: %v", rows)
	}
	heard := 0
	deadline := time.After(20 * time.Second)
	for heard < len(conns) {
		select {
		case got := <-created:
			if got == skillID {
				heard++
			}
		case <-deadline:
			t.Fatalf("%d of %d connections heard skill %s created", heard, len(conns), skillID)
		}
	}
}
```

- [ ] **Step 2: Prove it builds and skips cleanly without a cluster**

Run: `go vet -tags clustere2e github.com/znasllc-io/memql/test/clustere2e/ && go test -tags clustere2e -count=1 -run TestClusterGames github.com/znasllc-io/memql/test/clustere2e/`
Expected: `ok  	github.com/znasllc-io/memql/test/clustere2e` (every test skips: `MEMQL_E2E_TOKEN not set`).

- [ ] **Step 3: Commit**

```bash
git add test/clustere2e/games_pack_test.go
git -c user.name=znas -c user.email=dev@example.com commit -m "test(games): the pack's rows cross the bff replicas" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 13: Branch verification

Everything the CI lanes run for what this branch touched, locally, before anything is pushed.

**Files:**
- Modify (regenerated): `component/architecture/embedded/topology.model.json`

**Interfaces:**
- Consumes: every task above.
- Produces: a green branch and a model that includes the pack.

- [ ] **Step 1: The whole tree**

Run: `make test > /tmp/games-make-test.log 2>&1; echo "exit=$?"; grep -E '^(FAIL|--- FAIL)' /tmp/games-make-test.log | head`
Expected: `exit=0` and no FAIL lines (about 265 `ok` packages).

- [ ] **Step 2: The database-gated trees the branch touches**

`packs` is already in `scripts/ci/db-gated-packages.sh --trees`; `app` carries the boot path the anchor rename touched, and `component/grpc` is the stream every client call rides.

Run: `for t in packs component/grpc app; do MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 github.com/znasllc-io/memql/$t/... 2>&1 | grep -E '^(ok|FAIL|---)'; done`
Expected: `ok` for `packs/anchor`, `packs/gamespack`, `packs/reviewspack`, `packs/wholesalepack`, `component/grpc` and `app`; nothing else.

- [ ] **Step 3: The generators and the language gates**

Run: `go run ./cmd/memqllint dsl/; echo "memqllint exit=$?"; make concept-snapshot-check && make platform-graph-check && make docs-matrix-check && make docs-grammar-check && go vet -tags clustere2e github.com/znasllc-io/memql/test/clustere2e/ && echo GATES-OK`
Expected: `memqllint exit=0`, an `ok` line per check, then `GATES-OK`.

- [ ] **Step 4: Refresh the architecture model**

The model gate is a subset check, so additions never fail it; refresh it once at the end so the model describes the pack.

Run: `make arch-model && make arch-model-check && git status --short`
Expected: `memql-arch: wrote ... nodes`, `ok  	github.com/znasllc-io/memql/component/architecture`, and ` M component/architecture/embedded/topology.model.json` as the only change.

- [ ] **Step 5: Commit**

```bash
git add component/architecture/embedded/topology.model.json
git -c user.name=znas -c user.email=dev@example.com commit -m "chore(games): refresh the architecture model" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 14: Local deployment, end to end, and back to main

The owner has allowed the local k3d engine (ArgoCD application `memql-local`, kube context `k3d-memql`) to run this branch's images for an end-to-end test, then to return to `main`. The pack changes no manifest, so ArgoCD keeps reconciling the manifests while `make dev` replaces the seven `memql-<type>:local` images. Nothing here is a code change and nothing is committed. Every command names the context explicitly.

**Files:** none.

**Interfaces:**
- Consumes: Tasks 1-13 on the branch; the cluster as found: `memql-local` tracks `main` at path `deploy/k8s/overlays/local` with automated sync (prune, self-heal); seven node Deployments (`identity`, `bff`, `agent`, `planner`, `workbench`, `mcp`, `edge`) at 2 replicas on `memql-<type>:local` images; database pod `memql-db-1`; the front door `https://api.memql.localhost`, trusted by the host; the owner `v1:identity:user:eab25597-d4cd-413b-b626-b728bdec4814` (dev@example.com), a cluster owner.
- Produces: a cluster running the branch with the pack enabled, both cluster tests green; then a cluster on `main` again.

- [ ] **Step 1: Record what the cluster runs now**

Run:
```bash
kubectl --context k3d-memql -n argocd get application memql-local -o jsonpath='{.spec.source.targetRevision}{"\n"}'
kubectl --context k3d-memql -n memql get deploy -o custom-columns=NAME:.metadata.name,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[0].image
kubectl --context k3d-memql -n memql exec memql-db-1 -c postgres -- psql -d memql -tAc \
  "SELECT id, payload->>'enabled' FROM \"MemoryNodes\" WHERE concept = 'v1:platform:packState' ORDER BY id, \"createdAt\" DESC"
```
Expected: `main`; the seven node Deployments at `2` ready on `memql-<type>:local`; no packState row for `games` (an absent row means the declared default, disabled). Write the three answers down: Step 9 restores them.

The branch's images must not be older than the build the database last migrated under (migrations run forward at boot). Check the running build:

Run:
```bash
RUNNING=$(kubectl --context k3d-memql -n memql logs deploy/bff | grep -m1 'memql node build identity' | sed -E 's/.*"commit":"([0-9a-f]+)".*/\1/'); echo "$RUNNING"
git -C /home/znas/memql-projects/memql-games merge-base --is-ancestor "$RUNNING" HEAD && echo OLDER-OK || echo REBASE-FIRST
```
Expected: a commit (it was `ff2c92cd4f36` when this plan was written) and `OLDER-OK`. On `REBASE-FIRST`, rebase the branch onto `origin/main`, rerun Task 13, then continue.

- [ ] **Step 2: Pin the manifests to the branch's base**

`main` moves while you test; a manifest from a newer `main` can expect code these images do not have. Pin the application to the commit the branch is built on:

Run:
```bash
BASE=$(git -C /home/znas/memql-projects/memql-games merge-base HEAD origin/main); echo "$BASE"
kubectl --context k3d-memql -n argocd patch application memql-local --type merge \
  -p "{\"spec\":{\"source\":{\"targetRevision\":\"$BASE\"}}}"
kubectl --context k3d-memql -n argocd get application memql-local -o jsonpath='{.status.sync.status} {.status.health.status}{"\n"}'
```
Expected: the base SHA (`f0af5373d980491c0f215c9a4233100310898808` unless the branch was rebased); then, within a minute, `Synced Healthy`.

- [ ] **Step 3: Build and roll out the branch's images**

`make dev` builds the seven node images from the worktree, imports them into k3d, restarts the pods and waits (expect several minutes per image on a first build).

Run: `cd /home/znas/memql-projects/memql-games && make dev`
Expected: it ends with every Deployment rolled out. Then confirm the new binary is the one serving:

Run: `kubectl --context k3d-memql -n memql logs deploy/bff --since=10m | grep -m1 'default-build packs linked'`
Expected: a line whose `packs` field lists `reviews`, `wholesale` and `games`.

- [ ] **Step 4: Mint a token for the owner**

Run:
```bash
export MEMQL_E2E_TOKEN=$(kubectl --context k3d-memql -n memql exec deploy/identity -- \
  /app/memql pat mint --user-id v1:identity:user:eab25597-d4cd-413b-b626-b728bdec4814 --label games-local)
echo "${MEMQL_E2E_TOKEN:0:8}"
```
Expected: `mql_pat_`. The command's stderr names the identity id (`minted id=... (revoke later: memql pat revoke --id ...)`): keep it for Step 10.

- [ ] **Step 5: Enable the pack, then restart every node**

Either Cluster > Modules > games > Enable in MemQL OS, or the scripted form:

Run:
```bash
cd /home/znas/memql-projects/memql-games
MEMQL_E2E_ENDPOINT=https://api.memql.localhost MEMQL_E2E_GAMES_ENABLE=1 \
  go test -tags clustere2e -count=1 -v -run TestClusterGamesPackSetEnabled github.com/znasllc-io/memql/test/clustere2e/
```
Expected: `games pack: enabled false -> true; restart required: true` and `--- PASS: TestClusterGamesPackSetEnabled`.

Enablement is read once, at boot, on every node:

Run:
```bash
for d in identity bff agent planner workbench mcp edge; do kubectl --context k3d-memql -n memql rollout restart deploy/$d; done
for d in identity bff agent planner workbench mcp edge; do kubectl --context k3d-memql -n memql rollout status deploy/$d --timeout=300s; done
```
Expected: `deployment "<name>" successfully rolled out` seven times.

- [ ] **Step 6: Verify the flip landed**

Run:
```bash
kubectl --context k3d-memql -n memql exec memql-db-1 -c postgres -- psql -d memql -tAc \
  "SELECT payload->>'packDomain', payload->>'enabled' FROM \"MemoryNodes\" WHERE id = 'v1:platform:packState:games' ORDER BY \"createdAt\" DESC LIMIT 1"
kubectl --context k3d-memql -n memql logs deploy/bff --since=10m | grep -m1 'packs loading mounted-inert'
```
Expected: `games|true`; the log line's `disabledPacks` lists `reviews` and `wholesale` and not `games`.

- [ ] **Step 7: Run the cluster tests through the front door**

The front door spreads the ten connections across both bff replicas; a port-forward would pin one pod and prove nothing about the hop.

Run:
```bash
MEMQL_E2E_ENDPOINT=https://api.memql.localhost go test -tags clustere2e -count=1 -v -timeout=300s \
  -run 'TestClusterGamesMemoryCrossesReplicas|TestClusterGamesSkillLearnsAcrossReplicas' \
  github.com/znasllc-io/memql/test/clustere2e/ 2>&1 | grep -E '^(--- |ok|FAIL)|games_pack_test'
```
Expected: `--- PASS: TestClusterGamesMemoryCrossesReplicas`, `--- PASS: TestClusterGamesSkillLearnsAcrossReplicas`, `ok`. A failure saying `is not loaded: enable the games pack` means Step 5's restart did not reach that node; a connection that "never heard" an id means a forward rule is missing (Task 2).

- [ ] **Step 8: Disable the pack before switching back**

So the stored decision says what the owner wants when the pack later ships, flip it back off:

Run: `MEMQL_E2E_ENDPOINT=https://api.memql.localhost MEMQL_E2E_GAMES_ENABLE=0 go test -tags clustere2e -count=1 -v -run TestClusterGamesPackSetEnabled github.com/znasllc-io/memql/test/clustere2e/`
Expected: `games pack: enabled true -> false; restart required: true`. (Leave it enabled instead only if the owner says so; the row is then simply inert on `main` images, which carry no games pack.)

- [ ] **Step 9: Switch back to main**

Build the images from `origin/main` in a throwaway worktree (never the main checkout), then return the application to `main`:

Run:
```bash
git -C /home/znas/memql-projects/memql fetch origin main
git -C /home/znas/memql-projects/memql worktree add --detach /tmp/memql-main-images origin/main
cd /tmp/memql-main-images && make dev
kubectl --context k3d-memql -n argocd patch application memql-local --type merge -p '{"spec":{"source":{"targetRevision":"main"}}}'
git -C /home/znas/memql-projects/memql worktree remove --force /tmp/memql-main-images
```
Expected: `make dev` ends with every Deployment rolled out; then:

Run:
```bash
kubectl --context k3d-memql -n argocd get application memql-local -o jsonpath='{.spec.source.targetRevision} {.status.sync.status} {.status.health.status}{"\n"}'
kubectl --context k3d-memql -n memql logs deploy/bff --since=10m | grep -m1 'packs linked'
```
Expected: `main Synced Healthy`, and a log line whose `packs` field lists `reviews` and `wholesale` only. The deployments match Step 1 again.

- [ ] **Step 10: Revoke the token**

Run: `kubectl --context k3d-memql -n memql exec deploy/identity -- /app/memql pat revoke --id <the id Step 4 printed>` and `unset MEMQL_E2E_TOKEN`
Expected: the revoke exits 0.

- [ ] **Step 11 (optional): Remove the test rows**

Only on the owner's say-so. The rows are inert on `main` images; this deletes the cluster tests' episodes, skills and traces, their vectors, and nothing else:

Run:
```bash
kubectl --context k3d-memql -n memql exec memql-db-1 -c postgres -- psql -d memql -c "
DO \$\$ DECLARE t text; BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'node_vectors%' LOOP
    EXECUTE format('DELETE FROM %I WHERE concept LIKE %L', t, 'v1:games:%');
  END LOOP;
END \$\$;
DELETE FROM \"MemoryNodes\" WHERE concept LIKE 'v1:games:%';"
```
Expected: `DO` and `DELETE <n>`.

---

## Follow-up tasks (not part of the milestone)

Spec section 4.7 seams 2-4, each its own reviewable change and its own pull request after the milestone merges. Land F3 first, before any multi-day soak.

The three are written in landing order (F3, F2, F4); each diff below applies on top of the previous one.

### Task F3 (follow-up, seam 3): `v1:router:call` keeps 45 days, archived before it is deleted

Every model call writes a `v1:router:call` row and nothing prunes them (`dsl/router/concepts.memql:79`). The generic sweep already honours a declaration (`component/retention/sweep.go:67`, hourly `retireDeclaredConceptVersions`). 45 days outlasts every reader (the weekly evidence fold and the sharing ledger read one ISO week; a monthly period is at most 31 days). `archive="required"` archives to the instance's blob container (`MEMQL_AZURE_BLOB_CONTAINER`, set on the local and cloud overlays) before deleting; an instance with none keeps the rows and its sweep reports `v1:router:call: verified archive unavailable; records preserved` every hour while it goes on with every other concept.

**Files:**
- Modify: `dsl/router/concepts.memql:77-84`, `docs/public/operate/ai-routing.md:347`
- Create: `component/router/call_retention_test.go`

**Interfaces:**
- Consumes: `memql.LoadUnifiedConcepts(nil)`, `memorynodes.DefaultRegistry().Get(id)`, `(*memorynodes.Concept).Retention{TTL, KeepLatest, Archive}`.
- Produces: the declaration `@retention(ttl="45d", keepLatest=false, archive="required")` on `concept call`.

- [ ] **Step 1: Write the failing test**

Create `component/router/call_retention_test.go`:

```go
package router

import (
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// THE DECISION LEDGER HAS A LIFETIME (games pack design record, seam 3):
// 45 days, archived before it is deleted. A game loop writes a v1:router:call
// row per model call, and nothing pruned them.
func TestRouterCallsAreRetainedFortyFiveDaysAndArchived(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.DefaultRegistry().Get("v1:router:call")
	if err != nil {
		t.Fatal(err)
	}
	r := c.Retention
	if r == nil || r.TTL != 45*24*time.Hour || r.KeepLatest || r.Archive != "required" {
		t.Fatalf("v1:router:call retention = %+v, want ttl 45 days, keepLatest false, archive required", r)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 -run TestRouterCallsAreRetainedFortyFiveDaysAndArchived github.com/znasllc-io/memql/component/router/`
Expected:

```text
--- FAIL: TestRouterCallsAreRetainedFortyFiveDaysAndArchived (0.13s)
    call_retention_test.go:24: v1:router:call retention = <nil>, want ttl 45 days, keepLatest false, archive required
FAIL
FAIL	github.com/znasllc-io/memql/component/router	0.144s
FAIL
```

- [ ] **Step 3: Declare the retention and document it**

```diff
diff --git a/dsl/router/concepts.memql b/dsl/router/concepts.memql
--- a/dsl/router/concepts.memql
+++ b/dsl/router/concepts.memql
@@ -74,6 +74,14 @@ concept budget {
 /// undeclared concept. An anonymous caller is refused now. What it does not
 /// change is which signed-in callers reach it through an unbound read; that
 /// stays the surfaces' business, and saying so plainly is the point.
+///
+/// RETENTION: 45 days, then archived to the instance's blob container and
+/// deleted (games pack design record, seam 3). Every model call writes a row
+/// and nothing pruned them. 45 days outlasts every reader: the weekly
+/// evidence fold and the sharing ledger read one ISO week, and a monthly
+/// period is at most 31 days. archive="required": with no container
+/// configured the sweep deletes nothing, which is the status quo.
+@retention(ttl="45d", keepLatest=false, archive="required")
 @rowAuthz(public, requiresIdentity)
 @displayCard(primary="model", secondary="promptName", tertiary="vendor", status="outcome")
 concept call {
```

```diff
diff --git a/docs/public/operate/ai-routing.md b/docs/public/operate/ai-routing.md
--- a/docs/public/operate/ai-routing.md
+++ b/docs/public/operate/ai-routing.md
@@ -344,6 +344,12 @@ the tokens, cost and latency the row already carried, a decision carries:
 | `callerKind` | what KIND of caller made the call: `user`, `system`, `connector`, `anonymous` or `unattributed` |
 | `cacheKind` | which CACHE answered, when one did: `exact` or `semantic`. Absent means a provider answered |
 
+**The ledger keeps 45 days.** `v1:router:call` declares
+`@retention(ttl="45d", keepLatest=false, archive="required")`: the hourly retention
+sweep archives older rows to the instance's blob container and then deletes them.
+An instance with no container keeps them, and its sweep reports
+`verified archive unavailable; records preserved` for this concept every hour.
+
 **`session` is a fourth door, not a flag on `app`.** `app` means a subscription app
 answered this TURN; `session` means one was handed the whole STEP and drove its own
 loop (design D7 of the app-session record). They are separate values because this
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -count=1 github.com/znasllc-io/memql/component/router/ && make concept-snapshot-check && go test -count=1 -run 'Docs|Doc' github.com/znasllc-io/memql && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: `ok` three times and `exit=0`. (Retention is not a snapshot field, so the snapshot does not change.)

- [ ] **Step 5: Commit**

```bash
git add dsl/router/concepts.memql docs/public/operate/ai-routing.md component/router/call_retention_test.go
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(router): v1:router:call keeps 45 days, archived before it is deleted" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task F2 (follow-up, seam 2): per-call attribution from `ai()` onto `v1:router:call`

`builtin ai` gains `attribution {actorRef, activity}`, carried through `airoute.ResolveRequest` and `router.CallRecord` onto two new `v1:router:call` fields, so a cluster can count model calls per character and per activity from its own ledger. Free text, clipped to 128 characters rather than refused (the row records a call that happened), omitted when empty, and read by no rule.

**Files:**
- Modify: `core/airoute/request.go:168`, `component/router/types.go:129`, `component/router/observer.go:263`, `component/router/fallback.go:360`, `component/router/cache_served.go:92`, `component/router/router.go:1088,1099`, `dsl/router/mutations.memql:70,76`, `dsl/router/concepts.memql:144`, `component/memql/ai_attribution.go:72,97,133`, `integrations/agents/ai.go:192,286`, `dsl/agents/builtins.memql:174`, `docs/public/operate/ai-routing.md:346`, `docs/public/language/functions.md:744`
- Create: `component/router/attribution_ledger_test.go`
- Test: `component/memql/ai_attribution_test.go`, `integrations/agents/ai_test.go`
- Regenerate: `component/conceptfields/concept-fields.snapshot.json`

**Interfaces:**
- Consumes: Task 10's `promptLane` and `AICallAttribution.Tags`; the router test helpers `levelRouter`, `countingLedger`, `cacheResolution` (`component/router`'s existing tests); `(*Router).RecordCacheServed`.
- Produces: `airoute.ResolveRequest.ActorRef`, `.Activity`; `router.CallRecord.ActorRef`, `.Activity`; `clipAttribution(string) string`; `recordRouterCall(..., actorRef, activity)`; `v1:router:call.actorRef`, `.activity` (`@maxLength(128)`); `memql.AICallAttribution.ActorRef`, `.Activity`; `promptAttribution(ctx, raw any) context.Context`; the `attribution` argument of `builtin ai`.

- [ ] **Step 1: Write the failing tests**

Create `component/router/attribution_ledger_test.go`:

```go
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
)

// A CALL A GAME RUNTIME ATTRIBUTED CARRIES ITS ACTOR AND ACTIVITY ONTO THE
// LEDGER (games pack design record, seam 2), and a call that named none
// writes neither field.
func TestLedgerRowCarriesTheCallersActorAndActivity(t *testing.T) {
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "alice", Role: auth.RoleWriter})

	r.RecordCacheServed(ctx, ResolveRequest{
		Level: airoute.LevelStrong, Modality: airoute.ModalityChat,
		ActorRef: "wren", Activity: strings.Repeat("a", 200),
	}, cacheResolution(), airoute.CacheKindExact, 2)
	args := ledger.args(t)
	if args["actorRef"] != "wren" {
		t.Fatalf("actorRef=%v, want wren", args["actorRef"])
	}
	if got, _ := args["activity"].(string); len(got) != 128 {
		t.Fatalf("activity has %d characters, want it clipped to 128 rather than the row refused", len(got))
	}

	r.RecordCacheServed(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat},
		cacheResolution(), airoute.CacheKindExact, 2)
	args = ledger.args(t)
	if _, present := args["actorRef"]; present {
		t.Fatalf("a call that named no actor wrote actorRef=%v", args["actorRef"])
	}
	if _, present := args["activity"]; present {
		t.Fatalf("a call that named no activity wrote activity=%v", args["activity"])
	}
}
```

```diff
diff --git a/component/memql/ai_attribution_test.go b/component/memql/ai_attribution_test.go
--- a/component/memql/ai_attribution_test.go
+++ b/component/memql/ai_attribution_test.go
@@ -186,3 +186,18 @@ func TestStampedTagsAreAddedToTheRequest(t *testing.T) {
 		t.Fatalf("tags=%v, a tag already present must not be added twice", again.Tags)
 	}
 }
+
+// THE CALLER'S OWN ATTRIBUTION REACHES THE REQUEST (games pack design record,
+// seam 2): a game runtime names the character and the activity, and the
+// router's ledger can count calls by them.
+func TestCallAttributionCarriesActorAndActivity(t *testing.T) {
+	ctx := ContextWithAICallAttribution(context.Background(), AICallAttribution{ActorRef: "wren", Activity: "sig-abc"})
+	req := applyCallAttribution(ctx, airoute.ResolveRequest{})
+	if req.ActorRef != "wren" || req.Activity != "sig-abc" {
+		t.Fatalf("actorRef=%q activity=%q, want the stamped values", req.ActorRef, req.Activity)
+	}
+	kept := applyCallAttribution(ctx, airoute.ResolveRequest{ActorRef: "otis"})
+	if kept.ActorRef != "otis" {
+		t.Fatalf("actorRef=%q; a value the call site named must be kept", kept.ActorRef)
+	}
+}
```

```diff
diff --git a/integrations/agents/ai_test.go b/integrations/agents/ai_test.go
--- a/integrations/agents/ai_test.go
+++ b/integrations/agents/ai_test.go
@@ -327,3 +327,22 @@ func TestAnUnknownLaneIsRefused(t *testing.T) {
 		t.Fatal("the runtime was asked although the lane was refused")
 	}
 }
+
+// THE ATTRIBUTION (games pack design record, seam 2) reaches the runtime on
+// the context, merged with the lane's tag rather than replacing it.
+func TestTheAttributionReachesTheRuntime(t *testing.T) {
+	engine := &promptRecordingEngine{reply: "ok"}
+	if _, err := callAi(t, engine, map[string]any{
+		"templateId": "decide", "data": map[string]any{}, "lane": "background",
+		"attribution": map[string]any{"actorRef": "wren", "activity": "sig-abc"},
+	}); err != nil {
+		t.Fatal(err)
+	}
+	stamped, _ := memql.AICallAttributionFromContext(engine.gotCtx)
+	if stamped.ActorRef != "wren" || stamped.Activity != "sig-abc" {
+		t.Fatalf("attribution = %+v, want actorRef wren and activity sig-abc", stamped)
+	}
+	if !slices.Contains(stamped.Tags, airoute.TagBackground) {
+		t.Fatalf("the attribution replaced the lane's tag: %+v", stamped)
+	}
+}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 -run 'TestLedgerRowCarries|TestCallAttributionCarries|TestTheAttributionReaches' github.com/znasllc-io/memql/component/router/ github.com/znasllc-io/memql/component/memql/ github.com/znasllc-io/memql/integrations/agents/`
Expected: FAIL, the packages do not build:

```text
# github.com/znasllc-io/memql/component/router [github.com/znasllc-io/memql/component/router.test]
component/router/attribution_ledger_test.go:23:3: unknown field ActorRef in struct literal of type ResolveRequest
component/router/attribution_ledger_test.go:23:21: unknown field Activity in struct literal of type ResolveRequest
# github.com/znasllc-io/memql/component/memql [github.com/znasllc-io/memql/component/memql.test]
component/memql/ai_attribution_test.go:194:78: unknown field ActorRef in struct literal of type AICallAttribution
component/memql/ai_attribution_test.go:194:96: unknown field Activity in struct literal of type AICallAttribution
component/memql/ai_attribution_test.go:196:9: req.ActorRef undefined (type airoute.ResolveRequest has no field or method ActorRef)
component/memql/ai_attribution_test.go:196:35: req.Activity undefined (type airoute.ResolveRequest has no field or method Activity)
component/memql/ai_attribution_test.go:197:68: req.ActorRef undefined (type airoute.ResolveRequest has no field or method ActorRef)
component/memql/ai_attribution_test.go:197:82: req.Activity undefined (type airoute.ResolveRequest has no field or method Activity)
component/memql/ai_attribution_test.go:199:59: unknown field ActorRef in struct literal of type airoute.ResolveRequest
component/memql/ai_attribution_test.go:200:10: kept.ActorRef undefined (type airoute.ResolveRequest has no field or method ActorRef)
component/memql/ai_attribution_test.go:201:74: kept.ActorRef undefined (type airoute.ResolveRequest has no field or method ActorRef)
...
```

- [ ] **Step 3: Carry the attribution through the router onto the row**

```diff
diff --git a/core/airoute/request.go b/core/airoute/request.go
--- a/core/airoute/request.go
+++ b/core/airoute/request.go
@@ -165,6 +165,18 @@ type ResolveRequest struct {
 	// one ignores it.
 	Effort string
 
+	// ActorRef and Activity say what the CALLER says a call was for: the
+	// character a game runtime animates, and the activity -- a goal
+	// signature, an action name -- the call served (games pack design record
+	// docs/superpowers/specs/2026-10-18-games-pack-design.md, seam 2). Free
+	// text the caller chose, carried onto v1:router:call so a cluster can
+	// count model calls per actor and per activity from its own ledger.
+	//
+	// NOTHING ROUTES ON EITHER: no @when key reads them, for callerKind's
+	// reason -- a rule that read them would turn evidence into an input.
+	ActorRef string
+	Activity string
+
 	// Attribution, unchanged in meaning from the pre-rules router.
 	RequestId string
 	UserId    string
```

```diff
diff --git a/component/router/types.go b/component/router/types.go
--- a/component/router/types.go
+++ b/component/router/types.go
@@ -126,6 +126,12 @@ type CallRecord struct {
 	// without one, because "nobody said" is itself one of the five answers.
 	CallerKind string
 
+	// ActorRef and Activity are the caller's own attribution (the request's
+	// fields of the same names): which character, which activity. Empty when
+	// the caller named none, and then omitted from the row.
+	ActorRef string
+	Activity string
+
 	// CacheKind is set ONLY on a row for a call served from a CACHE, and
 	// names which one: "exact" or "semantic" (memql#5581). EMPTY MEANS A
 	// PROVIDER ANSWERED, which is what every row written before this was.
```

```diff
diff --git a/component/router/observer.go b/component/router/observer.go
--- a/component/router/observer.go
+++ b/component/router/observer.go
@@ -260,6 +260,8 @@ func buildRecord(
 		AgentId:            req.AgentId,
 		UserId:             req.UserId,
 		CallerKind:         req.CallerKind,
+		ActorRef:           req.ActorRef,
+		Activity:           req.Activity,
 		PromptName:         req.PromptName,
 		Vendor:             resolved.Vendor,
 		Model:              resolved.Model,
```

```diff
diff --git a/component/router/fallback.go b/component/router/fallback.go
--- a/component/router/fallback.go
+++ b/component/router/fallback.go
@@ -357,6 +357,8 @@ func fallbackRecord(req ResolveRequest, failedResolved Resolved, err error) Call
 		AgentId:           req.AgentId,
 		UserId:            req.UserId,
 		CallerKind:        req.CallerKind,
+		ActorRef:          req.ActorRef,
+		Activity:          req.Activity,
 		PromptName:        req.PromptName,
 		Vendor:            failedResolved.Vendor,
 		Model:             failedResolved.Model,
```

```diff
diff --git a/component/router/cache_served.go b/component/router/cache_served.go
--- a/component/router/cache_served.go
+++ b/component/router/cache_served.go
@@ -89,6 +89,8 @@ func cacheServedRecord(
 		AgentId:    req.AgentId,
 		UserId:     req.UserId,
 		CallerKind: req.CallerKind,
+		ActorRef:   req.ActorRef,
+		Activity:   req.Activity,
 		PromptName: req.PromptName,
 
 		// The provider the chain picked, which is also the provider whose
```

```diff
diff --git a/component/router/router.go b/component/router/router.go
--- a/component/router/router.go
+++ b/component/router/router.go
@@ -1085,9 +1085,28 @@ func buildRouterCallArgs(rec CallRecord, callId string) map[string]any {
 	if airoute.ValidCacheKind(rec.CacheKind) {
 		args["cacheKind"] = rec.CacheKind
 	}
+	// The caller's attribution is written only when named: an absent field is
+	// "the caller said nothing", which is every row written before it.
+	if a := clipAttribution(rec.ActorRef); a != "" {
+		args["actorRef"] = a
+	}
+	if a := clipAttribution(rec.Activity); a != "" {
+		args["activity"] = a
+	}
 	return args
 }
 
+// clipAttribution trims a caller's attribution to the 128 characters the
+// concept accepts. Clipped rather than refused: the row is a ledger of a call
+// that HAPPENED, and an over-long label must not lose the whole record.
+func clipAttribution(s string) string {
+	s = strings.TrimSpace(s)
+	if r := []rune(s); len(r) > 128 {
+		return string(r[:128])
+	}
+	return s
+}
+
 // consideredArgs renders the door report as the plain []map the mutation's
 // []object argument takes. It returns an EMPTY SLICE rather than nil for an
 // empty report, because nil and "the walk considered nothing" are different
```

```diff
diff --git a/dsl/router/mutations.memql b/dsl/router/mutations.memql
--- a/dsl/router/mutations.memql
+++ b/dsl/router/mutations.memql
@@ -67,9 +67,13 @@ mutation call recordRouterCall {
     minContextTokens    int
     /// Whose machine served a local call. Empty until shared machines land.
     machineOwnerUserId  string
+    /// The caller's own name for what acted (a game character); absent when it named none.
+    actorRef            string  @maxLength(128)
+    /// The caller's own name for the activity served (a goal signature, an action); absent when it named none.
+    activity            string  @maxLength(128)
   }
   insert {
-    accept { requestId, agentId, userId, callerKind, cacheKind, promptName, policyName, vendor, model, providerName, inputTokens, outputTokens, cachedInputTokens, tokensEstimated, inputCost, outputCost, cachedInputCost, totalCost, pricingConfigured, timeToFirstTokenMs, totalDurationMs, tokensPerSec, streaming, outcome, errorCategory, errorMessage, fallbackFromModel, executionSurface, level, requestedLevel, servedLevel, servedModel, servedEffort, degraded, rule, policy, door, considered, touches, minContextTokens, machineOwnerUserId }
+    accept { requestId, agentId, userId, callerKind, cacheKind, promptName, policyName, vendor, model, providerName, inputTokens, outputTokens, cachedInputTokens, tokensEstimated, inputCost, outputCost, cachedInputCost, totalCost, pricingConfigured, timeToFirstTokenMs, totalDurationMs, tokensPerSec, streaming, outcome, errorCategory, errorMessage, fallbackFromModel, executionSurface, level, requestedLevel, servedLevel, servedModel, servedEffort, degraded, rule, policy, door, considered, touches, minContextTokens, machineOwnerUserId, actorRef, activity }
     stamp {
       id: args.callId
       // NOT in accept{}: a concept-level @default is documentation and is
```

```diff
diff --git a/dsl/router/concepts.memql b/dsl/router/concepts.memql
--- a/dsl/router/concepts.memql
+++ b/dsl/router/concepts.memql
@@ -141,6 +141,8 @@ concept call {
   machineOwnerUserId  string  @description("Whose machine served a local call. Empty until shared team machines land (epic memql#5146); a local call on the caller's own machine leaves it empty rather than repeating userId.")
 
   billing             enum("metered", "subscription", "local", "unknown")  @description("Who PAID for this call (memql#4362, memql#4681). metered = MemQL's own vendor spend, which is what totalCost is about and what the dollar ceiling counts. subscription = the call ran inside an app the user already pays for, so the tokens are real but MemQL was not billed for them. local = the call ran on a model hosted by one of the user's OWN MACHINES, so there is no vendor and no bill at all; it must be STAMPED EXPLICITLY by the fleet path and is never inferred, because inferring it would claim work ran on somebody's hardware when nobody established that. unknown = the surface could not tell, which is recorded AS unknown rather than folded into any of the others -- the number the owner asked for, what they were actually charged for, is only worth having if silence stays visible. An absent value reads as metered, the conservative direction: unattributed spend counts against the ceiling rather than disappearing into a bucket the ceiling cannot see.")
+  actorRef            string  @maxLength(128)  @description("What the CALLER says acted -- a game runtime's character -- as free text it chose (games pack design record, seam 2). Absent when the caller named none. Nothing routes on it.")
+  activity            string  @maxLength(128)  @description("What the CALLER says the call served -- a goal signature, an action name -- as free text it chose. Absent when the caller named none. With actorRef, what counts model calls per character and per activity from the ledger alone. Nothing routes on it.")
   executionSurface    string  @description("Where the call ran (memql#4362, memql#4681): empty for MemQL's own provider calls, 'cockpit-app:<appId>' for a call made by a local app on a user's machine through a delegated session, 'fleet:<registrationId>' for a model served by a machine in the user's fleet. Lets a cost reader separate 'what we spent' from 'what ran somewhere we do not pay for' without joining to the session or registration row -- and, for the fleet case, answers 'which of my machines did this' from the ledger alone.")
 }
 
```

- [ ] **Step 4: Stamp it from the `ai` builtin**

```diff
diff --git a/component/memql/ai_attribution.go b/component/memql/ai_attribution.go
--- a/component/memql/ai_attribution.go
+++ b/component/memql/ai_attribution.go
@@ -69,6 +69,11 @@ type AICallAttribution struct {
 	// Touches is the call's footprint: the concept ids or knowledge-domain
 	// ids the call is about. A rule matches it with startsWith semantics.
 	Touches []string
+	// ActorRef and Activity are the caller's own words for what the call was
+	// for (airoute.ResolveRequest's fields of the same names): recorded,
+	// never routed on.
+	ActorRef string
+	Activity string
 	// Tags are call tags a rule may branch on (core/airoute's TagBackground).
 	// The `ai` builtin's background lane stamps TagBackground here, so a
 	// background prompt call is routed by the shipped backgroundLane rule
@@ -89,7 +94,8 @@ func ContextWithAICallAttribution(ctx context.Context, a AICallAttribution) cont
 	if ctx == nil {
 		ctx = context.Background()
 	}
-	if strings.TrimSpace(a.RequestId) == "" && strings.TrimSpace(a.AgentId) == "" && len(a.Touches) == 0 && len(a.Tags) == 0 {
+	if strings.TrimSpace(a.RequestId) == "" && strings.TrimSpace(a.AgentId) == "" && len(a.Touches) == 0 && len(a.Tags) == 0 &&
+		strings.TrimSpace(a.ActorRef) == "" && strings.TrimSpace(a.Activity) == "" {
 		return ctx
 	}
 	return context.WithValue(ctx, aiCallAttributionKey{}, a)
@@ -124,6 +130,12 @@ func applyCallAttribution(ctx context.Context, req airoute.ResolveRequest) airou
 	if strings.TrimSpace(req.AgentId) == "" {
 		req.AgentId = strings.TrimSpace(stamped.AgentId)
 	}
+	if strings.TrimSpace(req.ActorRef) == "" {
+		req.ActorRef = strings.TrimSpace(stamped.ActorRef)
+	}
+	if strings.TrimSpace(req.Activity) == "" {
+		req.Activity = strings.TrimSpace(stamped.Activity)
+	}
 	if len(req.Touches) == 0 && len(stamped.Touches) > 0 {
 		// Copied rather than aliased: the request travels onto a decision
 		// record that outlives the caller's slice.
```

```diff
diff --git a/integrations/agents/ai.go b/integrations/agents/ai.go
--- a/integrations/agents/ai.go
+++ b/integrations/agents/ai.go
@@ -189,6 +189,7 @@ func (i *Integration) handleInvokePrompt(ctx context.Context, args map[string]an
 	if ctx, err = promptLane(ctx, templateId, args["lane"]); err != nil {
 		return nil, err
 	}
+	ctx = promptAttribution(ctx, args["attribution"])
 
 	// THE LIVE SEAM. Everything a prompt call is -- resolve the prompt,
 	// validate the data against its body, render, route by @level through
@@ -282,6 +283,26 @@ func promptLane(ctx context.Context, templateId string, raw any) (context.Contex
 	}
 }
 
+// promptAttribution carries the caller's {actorRef, activity} onto the call
+// (games pack design record, seam 2), merged into whatever attribution an
+// outer frame stamped so neither replaces the other. Free text, recorded on
+// v1:router:call, never routed on; an absent or empty object changes nothing.
+func promptAttribution(ctx context.Context, raw any) context.Context {
+	m, _ := raw.(map[string]any)
+	actor, activity := strings.TrimSpace(asString(m["actorRef"])), strings.TrimSpace(asString(m["activity"]))
+	if actor == "" && activity == "" {
+		return ctx
+	}
+	stamped, _ := memql.AICallAttributionFromContext(ctx)
+	if actor != "" {
+		stamped.ActorRef = actor
+	}
+	if activity != "" {
+		stamped.Activity = activity
+	}
+	return memql.ContextWithAICallAttribution(ctx, stamped)
+}
+
 // promptData coerces the `data` argument to the map InvokeAI takes.
 //
 // A MISSING data object is refused rather than sent as {}. The prompt's body
```

```diff
diff --git a/dsl/agents/builtins.memql b/dsl/agents/builtins.memql
--- a/dsl/agents/builtins.memql
+++ b/dsl/agents/builtins.memql
@@ -171,6 +171,7 @@ builtin ai {
   data        object!
   responseSchema object @description("Optional JSON Schema object for a strict structured response. Routing, metering and the work journal are preserved; reply contains the JSON text.")
   progress    bool @description("Persist public output snapshots for an owned work run as the model generates them. No private reasoning is recorded.")
+  attribution object @description("{actorRef, activity}: the caller's own words for what acted and what the call served -- a game character and a goal signature. Recorded on the call's v1:router:call row; nothing routes on it.")
   lane        enum("interactive", "background") @description("interactive (the default) for a call someone is waiting on. background for one nobody is -- a reflection, a summary written after the fact: its model calls count against the background rate bucket instead of the interactive one, and the request carries the background tag the shipped backgroundLane rule routes on. A pinned prompt keeps its pin.")
 }
 
```

- [ ] **Step 5: Document it, regenerate the snapshot, run everything it touches**

```diff
diff --git a/docs/public/operate/ai-routing.md b/docs/public/operate/ai-routing.md
--- a/docs/public/operate/ai-routing.md
+++ b/docs/public/operate/ai-routing.md
@@ -343,6 +343,7 @@ the tokens, cost and latency the row already carried, a decision carries:
 | `minContextTokens` | the context floor this resolution was made against |
 | `callerKind` | what KIND of caller made the call: `user`, `system`, `connector`, `anonymous` or `unattributed` |
 | `cacheKind` | which CACHE answered, when one did: `exact` or `semantic`. Absent means a provider answered |
+| `actorRef` / `activity` | the caller's own words for what acted and what the call served, from `ai(..., attribution: {actorRef, activity})`, clipped to 128 characters. Absent when the caller named none; nothing routes on them |
 
 **The ledger keeps 45 days.** `v1:router:call` declares
 `@retention(ttl="45d", keepLatest=false, archive="required")`: the hourly retention
```

```diff
diff --git a/docs/public/language/functions.md b/docs/public/language/functions.md
--- a/docs/public/language/functions.md
+++ b/docs/public/language/functions.md
@@ -741,7 +741,7 @@ domain that declares it (`use agents.builtins.{ ai }`). See
 
 | Builtin | Description | Called as |
 |---------|-------------|-----------|
-| `ai` | Calls one named prompt with a data object and returns `{prompt, reply}`. Optional `responseSchema` constrains `reply` to JSON matching that schema through structured model routing and adds the decoded object as `structured` for DSL field access. `progress: true` records public lifecycle and the completed result; partial text is available only for unconstrained streaming. `lane: "background"` is for a call nobody is waiting on (a reflection, a summary written after the fact): its model calls count against the background rate bucket, and the request carries the `background` tag the shipped `backgroundLane` rule routes on, unless the prompt pins a provider. Omitted or `"interactive"` is the default; any other lane is refused. Synchronous; the prompt's `@level` and the routing rules choose the model, so the call never names one | `builtin ai(templateId: "docSummary", data: { content: args.content })` |
+| `ai` | Calls one named prompt with a data object and returns `{prompt, reply}`. Optional `responseSchema` constrains `reply` to JSON matching that schema through structured model routing and adds the decoded object as `structured` for DSL field access. `progress: true` records public lifecycle and the completed result; partial text is available only for unconstrained streaming. `lane: "background"` is for a call nobody is waiting on (a reflection, a summary written after the fact): its model calls count against the background rate bucket, and the request carries the `background` tag the shipped `backgroundLane` rule routes on, unless the prompt pins a provider. Omitted or `"interactive"` is the default; any other lane is refused. `attribution: {actorRef, activity}` is recorded on the call's `v1:router:call` row and routes nothing. Synchronous; the prompt's `@level` and the routing rules choose the model, so the call never names one | `builtin ai(templateId: "docSummary", data: { content: args.content })` |
 | `agent` | Opens a `v1:work:goal` naming the `invokeAgent` template and returns `{goalId, runId}`; a run dispatcher claims it on an agent node | `builtin agent(name: "assistant", prompt: args.question, partitionId: "system")` |
 | `runAgentTurn` | Runs one agent turn in line and returns its reply. Answers only on an agent node | `builtin runAgentTurn(agentId: args.agentId, prompt: args.question)` |
 
```

Run: `make concept-snapshot && make concept-snapshot-check && go test -count=1 github.com/znasllc-io/memql/core/airoute/ github.com/znasllc-io/memql/component/router/ github.com/znasllc-io/memql/integrations/agents/ && go test -count=1 -run 'Attribution|Lane|Tags|Stamped' github.com/znasllc-io/memql/component/memql/ && go test -count=1 -run 'Docs|Doc' github.com/znasllc-io/memql && go run ./cmd/memqllint dsl/; echo "exit=$?"`
Expected: the snapshot gains `actorRef` and `activity` on `v1:router:call`; `ok` for every package; `exit=0`. (memqllint is what proves `recordRouterCall`'s `accept` list names declared arguments and concept fields.)

- [ ] **Step 6: Commit**

```bash
git add core/airoute/request.go component/router/types.go component/router/observer.go component/router/fallback.go \
  component/router/cache_served.go component/router/router.go component/router/attribution_ledger_test.go \
  dsl/router/mutations.memql dsl/router/concepts.memql component/memql/ai_attribution.go \
  component/memql/ai_attribution_test.go integrations/agents/ai.go integrations/agents/ai_test.go \
  dsl/agents/builtins.memql docs/public/operate/ai-routing.md docs/public/language/functions.md \
  component/conceptfields/concept-fields.snapshot.json
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(router): per-call attribution from ai() onto v1:router:call" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task F4 (follow-up, seam 4): typed refusal codes over ExecuteQuery

The server fills `QueryError.metadata` only from a `StructuredError` (`component/grpc/server.go:866-895`, `memql.proto:828`), and the TS SDK throws a plain `Error` (`sdk/ts/src/client/query.ts:152`), so a client searches a sentence. A closed registry of codes, matched as whole words in the error message (`work.InferenceRefusalCode`'s reason: the raising end is often another module), lifts the code into `metadata["refusal"]`; the TS SDK throws `QueryRefusalError` carrying it, with the message unchanged so every existing catch keeps working. The games pack registers its eight codes.

**Files:**
- Create: `component/memql/refusal_codes.go`, `component/memql/refusal_codes_test.go`, `component/grpc/query_refusal.go`, `component/grpc/query_refusal_test.go`
- Modify: `component/grpc/server.go:891`, `sdk/ts/src/client/wire.ts:630`, `sdk/ts/src/client/query.ts:82,179`, `sdk/ts/src/client/index.ts:17`, `packs/gamespack/pack.go` (`Register`), `docs/public/build/consuming-the-games-pack.md` (Refusals)
- Test: `sdk/ts/test/query.test.ts`, `packs/gamespack/refusals_test.go`

**Interfaces:**
- Consumes: `work.RefusalNoLocalModel`, `work.RefusalNoApp`, `work.RefusalEveryDoorShut`, `work.RefusalCeilingReached` (`component/work/inference.go:53`), `memql.ErrLLMGuardBlocked`, `gamespack.RefusalCodes` (Task 8).
- Produces: `<the deferred refusal-code registry call>(codes ...string)` (lower snake-case words only), `memql.RefusalCodeOf(message string) string` (longest registered whole word, or `llm_guard_blocked` for the guard's sentence), `memql.RefusalLLMGuardBlocked`; `withRefusal(err error, metadata map[string]string) map[string]string` in `component/grpc`; TS `QueryRefusalError` (`construct`, `code`, `refusal?`, `metadata`) exported from `sdk/ts/src/client/index.ts`; `QueryErrorPayload.error.metadata`.

- [ ] **Step 1: Write the failing tests**

Create `component/memql/refusal_codes_test.go`:

```go
package memql

import (
	"fmt"
	"testing"
)

func TestRefusalCodeOfFindsTheRegisteredWord(t *testing.T) {
	RegisterRefusalCodes("games_test_refusal")
	cases := []struct{ msg, want string }{
		{`MemQL engine failed to execute query. Details: builtin ai: ai("decide"): every_door_shut: no door to a model is open for this call`, "every_door_shut"},
		{`no_local_model_available: no machine could serve fleet:qwen`, "no_local_model_available"},
		{`builtin gamesRecall: games_test_refusal: nope`, "games_test_refusal"},
		{fmt.Sprintf("ai(%q): %v", "x", &LLMGuardBlock{Layer: GuardLayerRateCeiling, Reason: "too many"}), RefusalLLMGuardBlocked},
		{`the model said every_door_shutting is fine`, ""},
		{`an ordinary failure`, ""},
	}
	for _, c := range cases {
		if got := RefusalCodeOf(c.msg); got != c.want {
			t.Errorf("RefusalCodeOf(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestRefusalCodesMustBeSnakeCaseWords(t *testing.T) {
	RegisterRefusalCodes("NotACode", "plain")
	if RefusalCodeOf("NotACode here") != "" || RefusalCodeOf("plain here") != "" {
		t.Fatal("a code that is not a lower snake-case word must not be registered")
	}
}
```

Create `component/grpc/query_refusal_test.go`:

```go
package memql

import (
	"errors"
	"testing"
)

func TestAQueryRefusalCarriesItsCode(t *testing.T) {
	md := withRefusal(errors.New(`builtin ai: ai("decide"): every_door_shut: no door to a model is open for this call`), nil)
	if md["refusal"] != "every_door_shut" {
		t.Fatalf("metadata = %v, want refusal every_door_shut", md)
	}
	kept := withRefusal(errors.New("no_local_model_available: asleep"), map[string]string{"code": "X"})
	if kept["code"] != "X" || kept["refusal"] != "no_local_model_available" {
		t.Fatalf("metadata = %v; the structured code must be kept beside the refusal", kept)
	}
	if md := withRefusal(errors.New("an ordinary failure"), nil); md != nil {
		t.Fatalf("an error with no refusal code made metadata %v", md)
	}
}
```

```diff
diff --git a/packs/gamespack/refusals_test.go b/packs/gamespack/refusals_test.go
--- a/packs/gamespack/refusals_test.go
+++ b/packs/gamespack/refusals_test.go
@@ -6,6 +6,8 @@ import (
 	"slices"
 	"strings"
 	"testing"
+
+	"github.com/znasllc-io/memql/component/memql"
 )
 
 // The refusal codes are a contract: every code the source raises is in
@@ -40,3 +42,14 @@ func TestEveryRefusalCodeIsDeclaredAndRaised(t *testing.T) {
 		}
 	}
 }
+
+// The codes are the engine's too: Register adds them to the closed refusal
+// set, so the server lifts each one into QueryError.metadata["refusal"].
+func TestRefusalCodesAreRegisteredWithTheEngine(t *testing.T) {
+	registerOnce.Do(func() { Register(Domain) })
+	for _, c := range RefusalCodes {
+		if got := memql.RefusalCodeOf("builtin gamesRecordTrace: " + c + ": the sentence around it"); got != c {
+			t.Errorf("RefusalCodeOf found %q for %s; Register must call <the deferred refusal-code registry call>(RefusalCodes...)", got, c)
+		}
+	}
+}
```

```diff
diff --git a/sdk/ts/test/query.test.ts b/sdk/ts/test/query.test.ts
--- a/sdk/ts/test/query.test.ts
+++ b/sdk/ts/test/query.test.ts
@@ -8,7 +8,7 @@
 import test from "node:test";
 import assert from "node:assert/strict";
 
-import { QueryClient, refusalSentence } from "../src/client/query.js";
+import { QueryClient, QueryRefusalError, refusalSentence } from "../src/client/query.js";
 import type { Dispatcher } from "../src/client/dispatcher.js";
 import type {
   ClientMessage,
@@ -223,3 +223,44 @@ test("refusalSentence -- leaves alone what is not one of its frames", () => {
 test("refusalSentence -- an empty named frame carries no sentence", () => {
   assert.equal(refusalSentence("campaignSaveJourney:", "campaignSaveJourney"), "");
 });
+
+// ---------------------------------------------------------------------
+// Typed refusal: the code a program branches on rides the error
+// ---------------------------------------------------------------------
+
+test("executeNamed -- a refusal carries its stable code and keeps its sentence", async () => {
+  const mock = new MockDispatcher();
+  const qc = new QueryClient(mock.asDispatcher());
+  const promise = qc.executeNamed("gamesRecall", "builtin gamesRecall()");
+  mock.reply({
+    queryError: {
+      requestId: mock.lastExecuteQuery().requestId,
+      error: {
+        code: "Internal",
+        message: "MemQL engine failed to execute query. Details: every_door_shut: no door to a model is open",
+        metadata: { refusal: "every_door_shut" },
+      },
+    },
+  });
+  await assert.rejects(promise, (err: unknown) => {
+    assert.ok(err instanceof QueryRefusalError);
+    assert.equal(err.refusal, "every_door_shut");
+    assert.equal(err.code, "Internal");
+    assert.equal(err.construct, "gamesRecall");
+    assert.equal(refusalSentence(err, "gamesRecall"), "every_door_shut: no door to a model is open");
+    return true;
+  });
+});
+
+test("executeNamed -- a refusal with no code has none", async () => {
+  const mock = new MockDispatcher();
+  const qc = new QueryClient(mock.asDispatcher());
+  const promise = qc.executeNamed("gamesRecall", "builtin gamesRecall()");
+  mock.reply({ queryError: { requestId: mock.lastExecuteQuery().requestId, error: { message: "an ordinary failure" } } });
+  await assert.rejects(promise, (err: unknown) => {
+    assert.ok(err instanceof QueryRefusalError);
+    assert.equal(err.refusal, undefined);
+    assert.equal(err.message, "gamesRecall: an ordinary failure");
+    return true;
+  });
+});
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 -run Refusal github.com/znasllc-io/memql/component/memql/ github.com/znasllc-io/memql/component/grpc/ github.com/znasllc-io/memql/packs/gamespack/`
Expected: FAIL, the packages do not build:

```text
# github.com/znasllc-io/memql/component/memql [github.com/znasllc-io/memql/component/memql.test]
component/memql/refusal_codes_test.go:9:2: undefined: RegisterRefusalCodes
component/memql/refusal_codes_test.go:14:102: undefined: RefusalLLMGuardBlocked
component/memql/refusal_codes_test.go:19:13: undefined: RefusalCodeOf
component/memql/refusal_codes_test.go:26:2: undefined: RegisterRefusalCodes
component/memql/refusal_codes_test.go:27:5: undefined: RefusalCodeOf
FAIL	github.com/znasllc-io/memql/component/memql [build failed]
# github.com/znasllc-io/memql/packs/gamespack [github.com/znasllc-io/memql/packs/gamespack.test]
packs/gamespack/refusals_test.go:51:19: undefined: memql.RefusalCodeOf
# github.com/znasllc-io/memql/component/grpc [github.com/znasllc-io/memql/component/grpc.test]
component/grpc/query_refusal_test.go:9:8: undefined: withRefusal
component/grpc/query_refusal_test.go:13:10: undefined: withRefusal
component/grpc/query_refusal_test.go:17:11: undefined: withRefusal
...
```

- [ ] **Step 3: The registry and the lift**

Create `component/memql/refusal_codes.go`:

```go
package memql

// refusal_codes.go -- the refusal a client can BRANCH on (games pack design
// record docs/superpowers/specs/2026-10-18-games-pack-design.md, seam 4).
//
// A refusal reaches a client as a sentence: the engine wraps a builtin's
// error in the call's name and the server frames an internal error as "MemQL
// engine failed to execute query. Details: ...". A program that has to decide
// what to do -- a game runtime telling "the fleet is asleep" from "the pack
// is off" -- had to search that sentence for a word. These codes are the
// words, and the server lifts the one an error carries into
// QueryError.metadata["refusal"], so a client compares a field instead.
//
// MATCHED ON THE MESSAGE, deliberately, for work.InferenceRefusalCode's
// reason: the raising end is often another module and the error crosses
// layers that keep only its text. The set is CLOSED -- only a registered code
// is ever lifted -- so a word that merely looks like a code is never one.

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/work"
)

var (
	refusalCodesMu sync.RWMutex
	refusalCodes   = map[string]struct{}{}
)

// RefusalLLMGuardBlocked is the code for a call one of the LLM guards stopped
// (ai_guard.go). Its message has no code word of its own, so it is recognised
// by ErrLLMGuardBlocked's sentence.
const RefusalLLMGuardBlocked = "llm_guard_blocked"

func init() {
	RegisterRefusalCodes(
		work.RefusalNoLocalModel, work.RefusalNoApp, work.RefusalEveryDoorShut, work.RefusalCeilingReached,
		// A machine's own refusal when every slot of the model is busy
		// (memql-cockpit), forwarded verbatim on the fleet path.
		"model_concurrency_exceeded",
	)
}

// RegisterRefusalCodes declares stable refusal codes an error message may
// carry. Call it from init() or a pack's Register. A code is lower snake
// case; anything else is ignored rather than registered, because the matcher
// reads codes as whole words.
func RegisterRefusalCodes(codes ...string) {
	refusalCodesMu.Lock()
	defer refusalCodesMu.Unlock()
	for _, c := range codes {
		if c = strings.TrimSpace(c); refusalCodeShape.MatchString(c) {
			refusalCodes[c] = struct{}{}
		}
	}
}

var refusalCodeShape = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)

// RefusalCodeOf returns the registered refusal code a message carries as a
// whole word, the longest when several match, or "".
func RefusalCodeOf(message string) string {
	if strings.Contains(message, ErrLLMGuardBlocked.Error()) {
		return RefusalLLMGuardBlocked
	}
	refusalCodesMu.RLock()
	codes := make([]string, 0, len(refusalCodes))
	for c := range refusalCodes {
		codes = append(codes, c)
	}
	refusalCodesMu.RUnlock()
	sort.Slice(codes, func(i, j int) bool { return len(codes[i]) > len(codes[j]) })
	for _, c := range codes {
		for from := 0; ; {
			at := strings.Index(message[from:], c)
			if at < 0 {
				break
			}
			at += from
			end := at + len(c)
			if !wordByte(message, at-1) && !wordByte(message, end) {
				return c
			}
			from = at + 1
		}
	}
	return ""
}

// wordByte reports whether message[i] continues a snake-case word.
func wordByte(message string, i int) bool {
	if i < 0 || i >= len(message) {
		return false
	}
	b := message[i]
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
```

Create `component/grpc/query_refusal.go`:

```go
package memql

import (
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// withRefusal lifts the refusal code an engine error carries into the query
// error's metadata as "refusal" (games pack design record, seam 4), so a
// client branches on a field rather than searching a sentence. Metadata is
// created only when there is a code to put in it.
func withRefusal(err error, metadata map[string]string) map[string]string {
	if err == nil {
		return metadata
	}
	refusal := memqlengine.RefusalCodeOf(err.Error())
	if refusal == "" {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["refusal"] = refusal
	return metadata
}
```

```diff
diff --git a/component/grpc/server.go b/component/grpc/server.go
--- a/component/grpc/server.go
+++ b/component/grpc/server.go
@@ -888,6 +888,7 @@ func (s *service) executeQueryAuthored(ctx context.Context, query string, client
 			// Use the structured error code for the gRPC code field
 			metadata["code"] = string(se.Code)
 		}
+		metadata = withRefusal(err, metadata)
 
 		return nil, &structuredQueryError{
 			code:     code,
```

- [ ] **Step 4: Register the games pack's codes**

```diff
diff --git a/packs/gamespack/pack.go b/packs/gamespack/pack.go
--- a/packs/gamespack/pack.go
+++ b/packs/gamespack/pack.go
@@ -121,6 +121,9 @@ func (p *Provider) IntegrationName() string { return integrationName }
 //   - registerRoutingRules: the live graph's forward rules for this pack's
 //     concepts, without which a row written through one replica never
 //     reaches a subscriber on another (routing.go).
+//   - RegisterRefusalCodes: the pack's games_* codes join the engine's closed
+//     refusal set, so the server lifts each into QueryError.metadata
+//     ["refusal"] and a client branches on a field (refusals.go).
 //   - BindPluginToPack + RegisterPluginForContract: the Go half, skipped by
 //     app's materializePlugins while the pack is disabled.
 //
@@ -130,6 +133,7 @@ func Register(domain string) {
 	memqldsl.RegisterTree(domain, Tree())
 	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
 	registerRoutingRules()
+	<the deferred refusal-code registry call>(RefusalCodes...)
 	memql.BindPluginToPack(domain, domain)
 	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
 }
```

```diff
diff --git a/docs/public/build/consuming-the-games-pack.md b/docs/public/build/consuming-the-games-pack.md
--- a/docs/public/build/consuming-the-games-pack.md
+++ b/docs/public/build/consuming-the-games-pack.md
@@ -195,7 +195,9 @@ deletes publish nothing, so a live view re-reads after a forget.
 
 ## Refusals
 
-Refusals lead with a stable code. Match the code, not the sentence.
+Refusals lead with a stable code. Match the code, not the sentence. Over
+`ExecuteQuery` the code also arrives as `QueryError.metadata["refusal"]`, and
+the TypeScript SDK throws a `QueryRefusalError` whose `refusal` is that code.
 
 | Code | When |
 |---|---|
```

- [ ] **Step 5: The TypeScript SDK**

```diff
diff --git a/sdk/ts/src/client/wire.ts b/sdk/ts/src/client/wire.ts
--- a/sdk/ts/src/client/wire.ts
+++ b/sdk/ts/src/client/wire.ts
@@ -627,7 +627,9 @@ export interface QueryResultPayload {
 
 export interface QueryErrorPayload {
   requestId: string;
-  error?: { code?: string; message?: string };
+  // metadata.refusal is the stable refusal code the cluster lifted out of the
+  // error (every_door_shut, games_signature_mismatch, ...), when it carried one.
+  error?: { code?: string; message?: string; metadata?: Record<string, string> };
 }
 
 export interface EventPayload {
```

```diff
diff --git a/sdk/ts/src/client/query.ts b/sdk/ts/src/client/query.ts
--- a/sdk/ts/src/client/query.ts
+++ b/sdk/ts/src/client/query.ts
@@ -79,6 +79,32 @@ export function refusalSentence(error: unknown, construct?: string): string {
   return text.replace(PACKAGE_NAME, "");
 }
 
+/**
+ * A call the cluster refused. The message is exactly what executeNamed has
+ * always thrown (`<construct>: <cluster's words>`), so refusalSentence and
+ * every existing catch keep working; what this adds is the refusal CODE a
+ * program branches on -- `every_door_shut` when no model is reachable,
+ * `games_signature_mismatch` from the games pack -- read from the error's
+ * metadata, never parsed out of the sentence.
+ */
+export class QueryRefusalError extends Error {
+  readonly construct: string;
+  /** The transport's status code word (`Internal`, `InvalidArgument`, ...). */
+  readonly code: string;
+  /** The stable refusal code, when the refusal carried one. */
+  readonly refusal?: string;
+  readonly metadata: Record<string, string>;
+
+  constructor(construct: string, error?: { code?: string; message?: string; metadata?: Record<string, string> }) {
+    super(`${construct}: ${error?.message ?? "(no message)"}`);
+    this.name = "QueryRefusalError";
+    this.construct = construct;
+    this.code = error?.code ?? "";
+    this.metadata = { ...(error?.metadata ?? {}) };
+    if (this.metadata.refusal) this.refusal = this.metadata.refusal;
+  }
+}
+
 export interface QueryCallOptions {
   /** Live refreshes share a smaller budget, leaving capacity for user actions. */
   priority?: "background";
@@ -150,7 +176,7 @@ export class QueryClient {
     );
     const payload = readServerPayload(resp);
     if (payload?.kind === "queryError") {
-      throw new Error(`${name}: ${payload.value.error?.message ?? "(no message)"}`);
+      throw new QueryRefusalError(name, payload.value.error);
     }
     if (payload?.kind === "queryResult") {
       return resultFromQueryPayload(payload.value);
```

```diff
diff --git a/sdk/ts/src/client/index.ts b/sdk/ts/src/client/index.ts
--- a/sdk/ts/src/client/index.ts
+++ b/sdk/ts/src/client/index.ts
@@ -14,7 +14,7 @@ export {
   type ReconnectOptions,
 } from "./connection.js";
 export { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
-export { QueryClient, refusalSentence, type QueryCallOptions, type ConceptRegistryFollow } from "./query.js";
+export { QueryClient, QueryRefusalError, refusalSentence, type QueryCallOptions, type ConceptRegistryFollow } from "./query.js";
 export {
   ModulesClient,
   type Module,
```

- [ ] **Step 6: Run everything it touches**

Run: `go test -count=1 github.com/znasllc-io/memql/component/grpc/ github.com/znasllc-io/memql/packs/gamespack/ && go test -count=1 -run Refusal github.com/znasllc-io/memql/component/memql/ && make sdk-ts-install && make sdk-ts-typecheck && make sdk-ts-test 2>&1 | grep -E '^# (tests|pass|fail)'`
Expected: `ok` for each Go package, a clean typecheck, then `# tests 464`, `# pass 464`, `# fail 0`.

Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$GAMES_DSN" go test -count=1 github.com/znasllc-io/memql/component/grpc/... 2>&1 | grep -E '^(ok|FAIL)'`
Expected: `ok  	github.com/znasllc-io/memql/component/grpc`.

- [ ] **Step 7: Commit**

Call out the wire change in the body: `QueryError.metadata` gains `refusal`, an additive field a frontend may read.

```bash
git add component/memql/refusal_codes.go component/memql/refusal_codes_test.go component/grpc/query_refusal.go \
  component/grpc/query_refusal_test.go component/grpc/server.go packs/gamespack/pack.go packs/gamespack/refusals_test.go \
  docs/public/build/consuming-the-games-pack.md sdk/ts/src/client/wire.ts sdk/ts/src/client/query.ts \
  sdk/ts/src/client/index.ts sdk/ts/test/query.test.ts
git -c user.name=znas -c user.email=dev@example.com commit -m "feat(grpc): typed refusal codes over ExecuteQuery" \
  -m "QueryError.metadata gains \"refusal\", the stable code an error carried (additive; frontends may branch on it)." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
