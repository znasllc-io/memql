# Memory Search Model Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A cluster owner sees and changes the memory-search (embedding) model in Cluster > Settings > Memory search. A switch rebuilds the index in the background, then search moves over by itself. Old indexes are kept for 30 days. Every embedding runs on the owner's machines, or on the vendor the owner chose, and never falls back to a vendor on its own.

**Architecture:**
- The binding row records its INDEXER: the owner whose machines embed everything.
- One seam in embedding resolution makes every memory-search embedding resolve as that indexer.
- A source registry rebuilds each indexed item's text from its row.
- One scheduled index job fills a target table under an advisory lock, saving a cursor in Postgres after every batch. It serves a switch's rebuild, the everyday backfill and the catch-up passes, then flips the binding in one transaction.
- Owner builtins and an OS section drive it.

**Tech Stack:** Go (`component/memql`, `integrations/{embedding,library,knowledge,similarity,harnessrecall}`, `app`), MemQL DSL (`dsl/platform`, `dsl/rbac`), Postgres and pgvector, TypeScript and React (`clients/os`), generated SDKs.

**Spec:** `docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md` (approved by the owner 2026-11-24).

## Global Constraints

**Embedding and the indexer**
- Every memory-search embedding resolves as the INDEXER, whoever wrote the item or runs the search. That covers indexing an item, a search query, a knowledge chunk and a seed.
- A `fleet:` binding means the indexer's machines and the machines lent to them. A vendor binding is paid by the cluster's account, as today (embeddings are already exempt from per-person accounts, `component/router/accounts.go:54,77`).
- Never an automatic vendor fallback. `autoBindEmbedder` considers only models offered by an active cluster owner's machines (their own and lent to them).
- The indexer must be an active user with the stored role `owner`. When the indexer stops being one, or none of their machines offers the model, indexing waits. Items are not counted failed while it waits.

**The job and the switch**
- One switch at a time. One job runner at a time across replicas: `pg_try_advisory_lock` on a dedicated connection, key `hashtextextended('memql:memory-index', 0)`.
- The cursor and counts are saved after every batch, so a restart or a different replica resumes.
- The flip is automatic only when every item is done and none is unsourced or failed. Otherwise the switch waits for the owner (Switch anyway, or Cancel). The flip is one transaction under the existing binding lock.

**Retention**
- 30 days: `const memoryIndexRetention = 30 * 24 * time.Hour`.
- A retired table is dropped only when it is past `retireAt` and is neither active nor a switch's target.

**Data and schema**
- New concept fields are OPTIONAL. Never add `@required` to a concept with rows; never drop a field. Run `make concept-snapshot` after concept changes.
- Engine machinery writes through `@serverOnly` mutations with internal origin, as `embedder_runtime.go:201` does.
- No new HTTP endpoint.
- Owner surfaces are `@sdk` builtins with `@requiresCapability(..., "app:cluster/memorysearch")`, seeded on `owner` only.

**Process**
- Multi-node: all state lives in Postgres. The db-gated tests drive two engines over one database.
- Keep each new db test to a few seconds: `component/memql`'s db package runs near its 1200 s budget (memory `memql-db-suite-budget`).
- Tests run through `make test`, plus the db trees with `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@127.0.0.1:55432/memql?sslmode=disable -p=1`, plus `make os-test` and `make os-typecheck`, plus `make sdk-gen-check`. Never bare `go test ./...`.
- Read `.agents/skills/memql-ui-design/SKILL.md` and `clients/os/DESIGN.md` before the OS task. No emojis. Stage by explicit path.
- Docs and code comments change in the same commits as the behaviour they describe.

## Review Focus

- A switch started while the indexer's machines are all offline. The job waits with a reason; nothing is counted failed, and it resumes when a machine returns.
- Two owners. While owner A's switch runs, owner B's switch is refused. The indexer is the owner who chose.
- A width change, say 1024 to 768. Readers and writers use the new table and width only after the flip, and the `embedding_cache` key keeps the widths apart (`integrations/embedding/embedding.go:546,566`).
- A switch back started in the same minute the daily sweep runs. The target table is never dropped.
- A Training lesson row in `v1:work:observation`. A rebuild writes `lessonPassage(l)`, never the "Manual Training lesson: ..." event text (`integrations/library/training_lessons.go:141`).

---

### Task 1: The indexer on the binding, and the seam that embeds as the indexer

**Files:**
- Modify:
  - `dsl/platform/concepts.memql`: `concept embedderBinding` at :775. Add the optional `indexedBy string`, `chosenBy string`, `retireAt datetime` and `droppedAt datetime`, each with a `///` doc. Fix the doc's mention of a `reembedLibrary` workflow at :757.
  - `dsl/platform/mutations.memql`, beside :3329 and :3356. Add three `@serverOnly` mutations:
    - `flipEmbedderBinding`: update `providerRef`, `dimensions`, `previousRef`, `activatedAt`, `indexedBy` and `chosenBy` on a given binding id. NOT `@createOnly`.
    - `setEmbedderBindingChoice`: update `indexedBy` and `chosenBy`.
    - `recordRetiredEmbedder`: insert or update a row with id `retired-<16 hex>`, from `providerRef`, `dimensions`, `indexedBy`, `retireAt` and `droppedAt`.
  - `dsl/platform/shapes.memql`: `embedderBindingFull` at :370, with the new fields.
  - `component/memql/embedder_binding.go`: `EmbedderBinding` at :38 gains `IndexedBy`, `ChosenBy` and `RetireAt`.
  - `component/memql/embedder_runtime.go`: `readEmbedderPlan` decodes the new fields.
  - `app/ai_call_sites.go`: `embeddingResolveRequest` at :81.
  - `app/plugins.go`: `ResolveEmbeddingProvider` at :135.
  - `component/memql/ai_attribution.go` (:153-162): it must not overwrite a UserId the request already fixed.
  - `integrations/knowledge/release_seeds.go` (:160-165): stop deferring a fleet model that has an indexer.
  - `component/conceptfields/concept-fields.snapshot.json`, through `make concept-snapshot`.
- Test:
  - `component/memql/embedder_indexer_test.go`
  - `app/embedding_indexer_test.go`

**Interfaces:**
- Produces:
  - `func ContextWithEmbeddingIndexer(ctx context.Context, userId string) context.Context`
  - `func EmbeddingIndexerFromContext(ctx context.Context) (string, bool)`
  - `func IndexerFor(b EmbedderBinding) (userId string, fleet bool)`: `fleet` is true for a `fleet:` ref.
  - `func RetiredEmbedderBindingID(providerRef string, dims int) string`: `"retired-" + the 16-hex part of EmbeddingVectorTable(providerRef, dims)`.
- The rule in the resolver:
  - When the requested provider is a `fleet:` ref, the request's `UserId` becomes, in order: the context's explicit indexer (the job sets it); else, if the name equals the ACTIVE binding's ref, `binding.IndexedBy`; else the caller as today.
  - A vendor ref is unchanged.
  - The decision record then names the indexer.

- [ ] **Step 1: Write the failing tests:**
  - `TestAnEmbeddingRunsOnTheIndexersMachinesWhoeverAsks`. The caller has no machines; the indexer `owner-1` has one offering `qwen3-embedding:0.6b`. Resolving the bound `fleet:qwen3-embedding:0.6b` under the caller's context succeeds, with the request's `UserId == "owner-1"`.
  - `TestTheJobsIndexerOverridesTheActiveOne`. With `ContextWithEmbeddingIndexer(ctx, "owner-2")`, resolution uses `owner-2`.
  - `TestAVendorBindingIsResolvedAsBefore`. The request's `UserId` stays the caller's.
  - `TestABindingRowCarriesItsIndexer`. A row with `indexedBy` and `chosenBy` decodes into `EmbedderBinding`.
  - `TestRetiredIdsFollowTheTableHash`. `RetiredEmbedderBindingID("fleet:a", 1024)` has the prefix `retired-`, and its 16 hex characters equal the suffix of `EmbeddingVectorTable("fleet:a", 1024)`.
- [ ] **Step 2:** Run them. Expected: FAIL (undefined, or `UserId` is the caller's).
- [ ] **Step 3:** Implement the fields, the mutations, the context helpers and the resolver rule. Then remove the fleet deferral in `release_seeds.go`, whose comment says why it existed: a system actor had no machines.
- [ ] **Step 4:** Run the tests: PASS. Run `make concept-snapshot`, then `make test`: no new failure. Run the db tree for `integrations/knowledge`: PASS.
- [ ] **Step 5: Commit** with explicit paths: "memql: memory search embeds as its indexer, whoever asks".

### Task 2: The automatic first choice is an owner's machine model

**Files:**
- Modify:
  - `component/memql/embedder_runtime.go`:
    - `autoBindEmbedder` at :232: owner candidates only, with the `federationEmbedders()` fallback removed.
    - `bindEmbedder` at :157: record `indexedBy` and `chosenBy`.
    - A new `adoptEmbedderIndexer`.
  - `component/memql/routes_builtin.go`: delete `federationEmbedders` at :670 if nothing else calls it.
- Test: `component/memql/embedder_autobind_test.go`

**Interfaces:**
- Produces:
  - `func (e *MemQLEngine) ownerEmbeddingCandidates(ctx context.Context) ([]embedderCandidate, error)`. It takes every ACTIVE user whose stored role is `owner`, reads that owner's fleet catalog (their own machines and those lent to them), and keeps the online models with `Embeddings`. The candidate type is `type embedderCandidate struct { ProviderRef, OwnerUserId string }`, sorted by model id, then owner id.
  - `func (e *MemQLEngine) adoptEmbedderIndexer(ctx context.Context) error`. When the active binding is `fleet:` with an empty `IndexedBy`, it sets `IndexedBy` to the first active owner whose catalog offers the model, online or not. `ChosenBy` stays empty.
- The rules:
  - `bindEmbedder(ctx, provider)`, from the owner builtin, sets `chosenBy` to the caller, and `indexedBy` to the caller for a `fleet:` ref (empty for a vendor ref).
  - The automatic path sets `indexedBy` to the candidate's owner and `chosenBy` to empty.
  - `adoptEmbedderIndexer` runs where `autoBindEmbedder` runs (`WriteModuleReadiness`, `readiness_write.go:449`).

- [ ] **Step 1: Write the failing tests:**
  - `TestTheAutomaticChoiceNeverBindsAVendorModel`: no owner machines, a vendor embedder available, so no binding is written.
  - `TestTheAutomaticChoiceTakesOnlyAnOwnersMachineModel`: a writer's machine offers an embedder, so no binding. An owner's machine does, so it binds with `indexedBy == owner`.
  - `TestAMachineLentToAnOwnerCounts`.
  - `TestAnExistingFleetBindingAdoptsAnOwnerAsIndexer`.
  - `TestBindEmbedderRecordsWhoChoseIt`.
- [ ] **Step 2:** Run: FAIL.
- [ ] **Step 3:** Implement. Read owners with an existing engine read of `v1:identity:user` filtered on role `owner` and active. The builder finds the one the accounts export uses to mail every owner (accounts program P13), or adds a `@serverOnly` query.
- [ ] **Step 4:** Run: PASS. `make test`: no new failure. `readiness_db_test.go` still passes on the db tree.
- [ ] **Step 5: Commit**: "memql: the first memory-search model is an owner's machine model, never a vendor by itself".

### Task 3: The sources that rebuild an item's text

**Files:**
- Create:
  - `component/memql/memory_sources.go`
  - `component/memql/memory_sources_test.go`
- Modify:
  - `component/memql/executor_mutation.go`: extract the text rule of `embedWorkObservation` at :2381 into `workObservationEmbedText` and use it there.
  - `integrations/library/plugin.go` (or the integration's init): register the Library chunk source and the Training lesson source.
  - `integrations/knowledge/plugin.go`: register the knowledge chunk source.
  - The integrations' `_test.go` files, for their recipes.

**Interfaces:**
- Produces:
  - `type MemoryItem struct { Id, Text string }`
  - `type MemorySource struct { Name, Concept, VectorField string; Rows func(payload map[string]any) (text string, ok bool); Items func(ctx context.Context, after string, limit int) (items []MemoryItem, next string, err error) }`. Exactly one of `Rows` and `Items` is set. A `Rows` source is walked by the engine over the latest version of each row of `Concept`.
  - `func RegisterMemorySource(s MemorySource)`, refusing a duplicate `Name`.
  - `func MemorySources() []MemorySource`, sorted by `Name`.
  - `func workObservationEmbedText(payload map[string]any) (string, bool)`.
  - `func (e *MemQLEngine) memorySourceItems(ctx context.Context, s MemorySource, after string, limit int) ([]MemoryItem, string, error)`. It reads the latest version per id, `id > after`, ordered by id, with internal origin and a fresh read.
- The recipes (from the writers' own code; each must produce the SAME text as its writer):

| Name | Concept, field | Text |
|---|---|---|
| `workObservation` | `v1:work:observation`, content | `workObservationEmbedText`. Lesson rows are skipped; the `trainingLesson` source owns them |
| `conversationEvidence` | `v1:memory:conversationEvidence`, content | the row's `content` |
| `workAcknowledgement` | `v1:memory:workAcknowledgement`, content | the row's `request`. Expired rows and texts over 1200 runes are skipped |
| `authoringConstruct` | `v1:authoring:construct`, content | the row's `catalogMatchText`. Automations and uncatalogued rows are skipped |
| `libraryFileChunk` | `v1:library:fileChunk`, content | the row's `text` |
| `knowledgeChunk` | `v1:knowledge:documentChunk`, content | the row's text. For a seed row (prefix `<!--seed:`), only its body, as `storeSeedChunk` embeds it (`seed_domain_content.go:249,302`). Rejected chunks are skipped |
| `trainingLesson` | `v1:work:observation`, content, via `Items` | `lessonPassage(l)` for each current enabled, reviewed lesson head |

- [ ] **Step 1: Write the failing tests:**
  - `TestEveryIndexedPairHasASource`. The six (concept, field) pairs above each have at least one registered source. The test's comment names each writer's file.
  - `TestTheObservationRecipeMatchesTheWriter`, with table cases copied from `TestWorkObservationIndexesMeaningInsteadOfArchiveLabels`: the summary when `contextHash` is set; skipped for `providerAdmission`, `progressCredit`, `execution`, `continuation` and `delegatedEvidence`.
  - `TestTheSeedChunkRecipeEmbedsTheBody`.
  - `TestLessonRowsComeFromTheLessonSourceOnly`.
  - `TestADuplicateSourceNameIsRefused`.
- [ ] **Step 2:** Run: FAIL.
- [ ] **Step 3:** Implement the registry and the engine walk, and register the recipes. The live writers then call the same recipe functions, so the two paths cannot disagree.
- [ ] **Step 4:** Run: PASS. `make test`, and the db trees for library and knowledge: PASS.
- [ ] **Step 5: Commit**: "memql: every indexed item's text can be rebuilt from its row, by the writer's own rule".

### Task 4: The index job

**Files:**
- Modify:
  - `dsl/platform/concepts.memql`: a new `concept memoryIndexRun`.
  - `dsl/platform/mutations.memql`: `@serverOnly` `recordMemoryIndexRun`.
  - `dsl/platform/automations.memql`: `memoryIndexTick`, `@trigger(schedule="0 * * * * *")`.
  - `dsl/platform/builtins.memql`: an internal builtin `memoryIndexStep`. It has no `@sdk`; its handler refuses any call without internal origin.
  - `component/memql/engine_types.go` and `executor_builtin.go`: register it.
  - `component/node/routing.go`: a broadcast rule for `v1:platform:memoryIndexRun`, beside :171.
- Create:
  - `component/memql/memory_index_job.go`
  - `component/memql/memory_index_job_db_test.go`
  - `component/memql/memory_index_job_test.go`

**Interfaces:**
- `concept memoryIndexRun` has literal row ids `switch` and `backfill`, and is `@rowAuthz(clusterOwner)`. Its fields:
  - `kind enum("switch","backfill")`
  - `state enum("indexing","catchingUp","needsDecision","switching","finishing","done","cancelled","waiting")`
  - `targetRef string`, `targetDimensions int`, `targetTable string`, `previousRef string`
  - `indexedBy string`, `startedBy string`, `startedAt datetime`, `finishedAt datetime`
  - `cursor object`: source name to the last id
  - `counts object`: `{total, done, skipped, unsourced, failed}`
  - `waitingReason string`, `lastPassAt datetime`
- Produces:
  - `type MemoryIndexCounts struct { Total, Done, Skipped, Unsourced, Failed int }`
  - `func (e *MemQLEngine) memoryIndexStep(ctx context.Context, budget time.Duration) error`: one bounded tick. It takes the lock or returns, and stops at `budget` (45 s from the automation).
  - `type MemoryIndexRun struct { Kind, State, TargetRef, TargetTable, PreviousRef, IndexedBy, StartedBy, WaitingReason string; TargetDimensions int; StartedAt, FinishedAt, LastPassAt time.Time; Cursor map[string]string; Counts MemoryIndexCounts }`: the Go mirror of the concept.
  - `func (e *MemQLEngine) flipMemoryIndex(ctx context.Context, run MemoryIndexRun) error`
- What a tick does:
  - **A switch run is active:** walk every source in name order from its cursor. For each item missing from the target table (`id, vector_field` absent), embed it under `ContextWithEmbeddingIndexer` (fleet) or as cluster work (vendor), and upsert it through the store path. A batch is 32 items; save the cursor and counts after each batch.
  - **End of a walk:** go to `catchingUp`. A walk that finds nothing missing, with `failed == 0` and `unsourced == 0`, goes to `switching`, which flips and then goes to `finishing`. A last walk over the new active table then makes it `done`.
  - **Unsourced:** count the target-independent rows of the ACTIVE table whose concept has no source.
  - **A failed walk:** with failures or unsourced items it goes to `needsDecision`.
  - **The indexer's machines all offline** (`no machine offering ... is online`, or `every_door_shut` with a pinned fleet source): set `waiting` with a plain `waitingReason`, keep the cursor, and return. The next tick retries.
  - **The indexer is no longer an active owner** (checked at the start of every tick): set `waiting` with the `waitingReason` "the owner who chose this model is no longer an owner; an owner can choose again". Nothing is embedded.
  - **No switch:** the `backfill` run walks the active table the same way at most once every 10 minutes. That is what catches items missed while the indexer was offline.
- The flip, in ONE transaction under `lockAskConversationKind(ctx, "active", "embedder-binding")`:
  - `flipEmbedderBinding` on `active`, with the previous ref recorded;
  - `recordRetiredEmbedder` for the previous binding, `retireAt = now + memoryIndexRetention`;
  - `DELETE FROM semantic_ai_cache`;
  - state `finishing`.

- [ ] **Step 1: Write the failing unit tests:**
  - `TestAWalkSavesItsCursorEveryBatch`
  - `TestAnOfflineIndexerMakesTheRunWaitNotFail`
  - `TestFailuresOrUnsourcedItemsHoldTheFlip`
- [ ] **Step 2: Write the failing db-gated tests.** Each uses two engines over one database and a stub embedder that answers per provider with a fixed-width vector:
  - `TestASwitchFlipsOnlyWhenEveryItemIsIndexed`
  - `TestSearchUsesTheOldIndexUntilTheFlip`: `similarTo` before the flip reads the old table, after it the new one.
  - `TestAnInterruptedSwitchResumesOnAnotherReplica`: engine A runs a tick with a tiny budget, engine B continues from the saved cursor, and no item is embedded twice.
  - `TestItemsWrittenDuringTheSwitchReachTheNewIndex`, including one written after the last walk and before the flip.
  - `TestAnItemMissedWhileTheIndexerWasOfflineIsBackfilled`
  - `TestOneReplicaRunsTheJobAtATime`: two concurrent ticks, and one returns without work.
  - `TestAWidthChangeMovesTablesOnlyAtTheFlip`: 1024 to 768. Writers and readers use the 1024 table until the flip, and the 768 table after it.
  - `TestAnIndexerWhoIsNoLongerAnOwnerMakesTheRunWait`
- [ ] **Step 3:** Run them: FAIL.
- [ ] **Step 4:** Implement the concept, the mutations, the automation, the builtin, the routing rule and the job.
- [ ] **Step 5:** Run them: PASS, each db test under about 5 s. Run `make concept-snapshot`, then `make test`.
- [ ] **Step 6: Commit**: "memql: one index job rebuilds, backfills and flips memory search across replicas".

### Task 5: The owner's builtins, and the 30-day retention

**Files:**
- Modify:
  - `dsl/platform/builtins.memql`: six `@sdk` builtins.
  - `dsl/rbac/seeds.memql`: in the cluster block at :362-404, add `cap-owner-read-app-cluster-memorysearch` and `cap-owner-execute-app-cluster-memorysearch` in the one-line shape.
  - `component/memql/requires_capability.go` (:368): add `app:cluster/memorysearch` to the known resources if the parity tests require it.
  - `dsl/platform/automations.memql`: `memoryIndexRetentionSweep`, `@trigger(schedule="0 20 3 * * *")`, calling the internal builtin `memoryIndexRetire`.
  - The executor registration, and the SDKs through `make sdk-gen`.
- Create:
  - `component/memql/memory_search_builtins.go`
  - `component/memql/memory_search_builtins_test.go`
  - `component/memql/memory_search_builtins_db_test.go`

**Interfaces:**

| Builtin | Gate | Answer |
|---|---|---|
| `memorySearchStatus {}` | read | one row, id `current`, described below |
| `memorySearchChoices {}` | read | one row per choice: `{providerRef, kind: "machine" or "vendor", label, machines int, paidByCluster bool, available bool, hint}`. Machine models come from the caller's catalog (their own machines and those lent to them), embeddings only. Vendor embedding records show only where the cluster's account is set up |
| `memorySearchSwitch { provider string! }` | execute | Refused while a switch runs. With no vectors in the active table, it binds at once through `bindEmbedder`. With the same ref as active, nothing changes. Otherwise it writes `memoryIndexRun:switch` (`indexing`, the target, `indexedBy` set to the caller for a fleet ref, `startedBy`) and ensures the target table. A retained table is reused, so only missing items are embedded |
| `memorySearchCancel {}` | execute | state `cancelled`; records the target as retired with `retireAt = now + 30 days` |
| `memorySearchSwitchAnyway {}` | execute | `needsDecision` only: flips and continues |
| `memorySearchConfirm {}` | execute | an active VENDOR binding with an empty `chosenBy`: sets `chosenBy` to the caller |
| `memoryIndexRetire {}` | internal | drops each retired table past `retireAt` that is neither active nor a switch target, then sets `droppedAt` |

The status row carries:
- `providerRef`, `kind`, `label`
- `indexedBy`, `indexedByName`, `chosenBy`
- `needsConfirmation`: a vendor binding with an empty `chosenBy`
- `indexed`: the count in the active table
- `backlog`: the backfill run's last missing count
- `waitingReason`
- the `switch` run summary (state, target, counts, `startedAt`, `startedBy`)
- `retained`: a list of `{providerRef, retireAt}`

- [ ] **Step 1: Write the failing tests:**
  - `TestOnlyTheOwnerHoldsTheMemorySearchParts`: seeds, and refusal for a developer.
  - `TestAFirstBindingOnAnEmptyCorpusActivatesAtOnce`
  - `TestASecondSwitchIsRefusedWhileOneRuns`
  - `TestCancelKeepsThePartialIndexForThirtyDays`
  - `TestSwitchAnywayFlipsWithTheHeldItems`
  - `TestSwitchingBackReusesTheKeptIndex`: db-gated; only items written since are embedded.
  - `TestARetiredIndexIsDroppedAfterItsDate`: db-gated.
  - `TestTheActiveAndTargetTablesAreNeverDropped`: db-gated.
  - `TestStatusReportsTheBacklogAndWhyItWaits`
  - `TestAVendorBindingPickedBeforeAsksForConfirmationOnce`
  - `TestChoicesMarkVendorModelsAsPaidByTheCluster`
- [ ] **Step 2:** Run: FAIL.
- [ ] **Step 3:** Implement. Run `make sdk-gen`.
- [ ] **Step 4:** Run: PASS. Then `make sdk-gen-check`, `make test` (including `TestOsRegistryRequiresMatchTheAppSeeds`, once Task 6 adds the section), and the db tree.
- [ ] **Step 5: Commit**: "memql: owners switch the memory-search model; old indexes are kept for 30 days".

### Task 6: Cluster > Settings > Memory search, and Fleet's pointer to it

**Files:**
- Modify:
  - `clients/os/src/apps/cluster/settings.ts`: at :63-77 add `{ id: "memorysearch", name: "Memory search", parent: "settings", requires: "app:cluster/memorysearch" }`. Keep it LITERAL; the parity test parses it.
  - `clients/os/src/apps/cluster/ClusterApp.tsx`:
    - a dispatch branch for `memorysearch`, because an unknown id falls through to `ReadinessSection` at :141;
    - in `ClusterSettingsSection` (:144-169), a link row "Memory search" showing the current model, for people who hold `read app:cluster/memorysearch`;
    - leave the "Start page" select's option set unchanged.
  - `clients/os/src/apps/fleet/machines/ModelAssignmentBoard.tsx:158` and `modelSetup.ts:60`: "Memory search keeps {model}. Change it in Cluster > Settings > Memory search."
- Create:
  - `clients/os/src/apps/cluster/memorysearch/MemorySearchSection.tsx`
  - `clients/os/src/apps/cluster/memorysearch/useMemorySearch.ts`
  - `clients/os/test/cluster/memorySearch.test.tsx`

**Interfaces:**
- `useMemorySearch()` returns `{ status, choices, switchTo(ref), cancel(), switchAnyway(), confirm(), error }`. It reads `memorySearchStatus` and `memorySearchChoices` through `useReleaseRead` (`apps/cluster/releases/useReleaseRead.ts:11`): every 5 s while a switch is running and the section is visible, otherwise 30 s. The acts go through `connection.query.memorySearch*`.
- What the section draws, with `Head`, `Details`, `DetailsSection` and `SettingRow`:
  - **In use:** the model; "Indexed on <name>'s machines", or "Paid by the cluster's vendor account"; the items indexed; and the backlog line, e.g. "37 items waiting: none of your machines offering qwen3-embedding:0.6b is online".
  - **Change model:** a `ChoiceStack` of the choices. Machine models show their machine count; vendor models add "Paid by the cluster's vendor account".
  - **The confirmation** is an `InPlaceAsk` with this text: "Everything is re-indexed in the background {on your machines | on the cluster's vendor account, about N items}. New items are indexed the same way from now on. Search keeps using {current} until the rebuild finishes, then switches by itself. The current index is kept for 30 days in case you switch back."
  - **While a switch runs:** an `os-progress-track` and "Re-indexing {done} of {total} items. Search uses {current} until this finishes.", with Cancel.
  - **A switch that needs a decision:** "{n} items could not be indexed ({kinds})." with Switch anyway and Cancel.
  - **Afterwards:** "The previous model's index is kept until {date}." with Switch back.
  - **A vendor binding picked before:** "This cluster uses {model}, paid by the cluster's vendor account. Keep it, or choose a model on your machines." with Keep.

- [ ] **Step 1: Write the failing tests** (`fakeConnection` and `withSession` from `test/cluster/harness.tsx`; `readTheBar()` for the confirmation hold):
  - "only an owner sees Memory search".
  - "it shows the model, the indexer and the backlog".
  - "a change asks first, with the vendor cost line for a vendor model".
  - "progress shows while a switch runs, and Cancel cancels".
  - "a held switch offers Switch anyway and Cancel".
  - "Switch back appears while the previous index is kept".
  - "a vendor binding picked before asks once".
  - "the Fleet board points to Cluster settings".
- [ ] **Step 2:** `make os-test`: FAIL.
- [ ] **Step 3:** Implement. Check the result at desktop and narrow width, in light and dark, through the `clients/os/qa` harness: add a `memory-search` view to `qa/main.tsx`'s `VIEWS` map.
- [ ] **Step 4:** Run `make os-test` and `make os-typecheck`: PASS. `make test`: `TestOsRegistryRequiresMatchTheAppSeeds` PASSES.
- [ ] **Step 5: Commit**: "os(cluster): owners see and change the memory-search model in Settings".

### Task 7: Clean up and document

**Files:**
- Modify:
  - **Delete** the unused activation path. From `component/memql/embedder_activation.go`, remove `EmbedderActivator`, `PlanActivation` and `ActivationPlan`. From `embedder_tables.go`, remove `CountVectors`, `ReembedComplete`, `EnsureVectorTable` and `VectorTableFor`, if nothing else calls them. Keep `createVectorTableSQL`, and remove their tests from `embedder_binding_test.go`.
  - `dsl/library/builtins.memql:128`: correct the provider doc, which says it defaults to embedding3Small; it defaults to the active binding.
  - `docs/public/operate/memory-search.md`, created with the standard front matter: the model, the indexer, the automatic choice, switching, re-indexing, the backlog, retention and Switch back.
  - The memory-search paragraphs in `docs/public/operate/ai-routing.md` and `docs/public/operate/api-keys.md` change to "embedded by the indexer".
  - `docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md`:
    - status: approved 2026-11-24, built;
    - the writer list gains the knowledge paths and the Training lessons;
    - "memory beliefs" is removed, because no writer exists.
- [ ] **Step 1:** Make the edits. Run `make test` (docs gates, and `TestNoDatabaseProductClaims`).
- [ ] **Step 2: Commit**: "docs: choosing and switching the memory-search model".
