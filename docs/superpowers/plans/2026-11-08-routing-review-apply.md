# Approving a Routing Review Changes Routing: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Approving a nightly `routingReview` approval saves its exact rule into the routing configuration document (a promotion removes the demotion), owners and developers can see and decide those reviews, and the fold stops proposing rules that can never decide a call.

**Architecture:** `component/memql` gains the apply (`ApplyRoutingReview`) and the "would this rule ever decide a call" check (`RoutingRuleBeatenBy`), both on the validation `routingRuleSave` already runs. `integrations/work` reads routing reviews under the system actor after the routing-author check, applies before recording the decision, and exposes `workRoutingReviews` to MemQL OS. `integrations/router`'s fold asks the check before opening an approval. Nexus merges the reviews into its approvals list.

**Tech Stack:** Go (component/memql, integrations/work, integrations/router, component/work), MemQL DSL, TypeScript/React (clients/os), Postgres+TimescaleDB CI image for DB-gated tests.

**Spec:** `docs/superpowers/specs/2026-11-08-routing-review-apply-design.md`

## Global Constraints

- Stage by explicit path (`git add <file>`); never `git add -A` / `git add .`.
- Every `go test` / `make test` runs with `MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:55441/memql?sslmode=disable` exported (container `routing-apply-db`, the CI image `ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c9cc49bdbf1e935234e8c87ab64b6d6d195fd2ecedff7b00185557631`); DB trees also `MEMQL_REQUIRE_DB=1`. Never localhost:5432.
- Whole-tree verification is `make test`, never `go test ./...`.
- Commit messages via a file (`git commit -F <file>`), ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Code comments state why: trigger, evidence, chosen path, alternatives, fallback, invariant. Docs change in the same commit as the behaviour they describe. No emojis.
- No backwards-compat shims: rename `configurationAuthor` to `RoutingConfigurationAuthor` and fix every caller.
- The precedence of every evidence proposal stays 50 (D4). The decision-write system actor is named `routingReview`.
- User-facing copy (exact): option labels "Add the rule" (demotion) / "Remove the exclusion" (promotion) / "Leave routing as it is"; decided notice "The rule was added." / "The exclusion was removed." / "Routing was left as it is."

## Review Focus

1. A writer (or admin) calling `workRoutingReviews` gets no rows and no error, and `decideApproval` on a routing review id answers the ordinary "no pending approval ... readable by this caller" refusal, so existence does not leak. Test: Task 4.
2. Two people decide the same review one after the other: the second gets the recorded decision back and no new revision is written. Test: Task 5.
3. Somebody saves their own rule at precedence 50 between the fold and the approval: approving is refused naming that rule, nothing is recorded, and declining still works. Test: Task 5.
4. A routing review raised before memql#4817's owner undo (owner `system:maintenance:routingEvidenceFold`) is listed and decidable like a new one. Test: Task 4.
5. A developer's Nexus receives no broadcast for the decided review, so the list must re-read itself after the decision. Test: Task 7.

---

### Task 1: The "always beaten" check and the shared rule validation

**Files:**
- Create: `component/memql/routing_review_apply.go`
- Modify: `component/memql/routing_configuration_rules.go`, `component/memql/rule_assist.go`, `component/memql/policy_assist.go`
- Test: `component/memql/routing_review_apply_test.go`

**Interfaces:**
- Produces:
  - `func RoutingConfigurationAuthor(ctx context.Context) (string, error)` (renamed `configurationAuthor`; same body and message)
  - `func (e *MemQLEngine) stageConfiguredRule(doc PolicyDocument, defaults map[string]*PolicyConfig, r *RuleConfig) (PolicyDocument, *RuleRegistry, error)`: the checks `ruleFromConfigurationArgs` runs today after building its `RuleConfig` (ValidateRuleProposal against `configuredNames{rules: e.rules, policies: snapshot}`, modality, precedence 0..1000000, stage into `doc.Rules[r.Name]`, `configuredRules`); returns the staged document and the finalized registry. `ruleFromConfigurationArgs` calls it.
  - `func ruleBeatenBy(rules *RuleRegistry, r *RuleConfig) *RuleConfig`: the first rule in `rules.Ordered()` that evaluates before `r` and covers it, else nil. Earlier = locked and not `default`, or unlocked with a higher precedence; a rule with `r`'s own name is skipped.
  - `func ruleCovers(earlier, r *RuleConfig) bool`: every key `earlier.When` has is present in `r.When` with an equal value; for `touches`, `strings.HasPrefix(r.When.Touches, earlier.When.Touches)`.
  - `func parseApprovedRule(source string) (*RuleConfig, error)`: `languageParser.ParseRuleDecl` + `ruleDeclToRuleConfig(decl, "routing configuration")`; refuses `Locked` or `Disabled` text.
  - `func (e *MemQLEngine) RoutingRuleBeatenBy(ctx context.Context, source string) (string, error)`: parses, reads the current configuration, and returns the name of `ruleBeatenBy` over the configured rules (`configuredRules(e.rules, doc)`, without staging the rule, so a tie does not hide the answer), or "".

- [ ] **Step 1: Write the failing tests** in `routing_review_apply_test.go`, building `&MemQLEngine{policies: policyFixture(store), rules: base}` as `policy_customization_test.go` does, with `base` holding `default` (locked, 0), `fastLane` (locked, 45, `@when(level="fast")`, policy `localFirst`) and `backgroundLane` (locked, 40, `@when(tag="background")`):
  - `TestRuleBeatenByNamesTheLockedLevelRule`: source `@when(level="fast") @policy("localFirst") @precedence(50) @exclude("fleet:m") rule evidenceDemotion_m_fast { }` -> `RoutingRuleBeatenBy` returns `"fastLane"`.
  - `TestNothingBeatsAStrongDemotion`: the same at `level="strong"` -> `""` (`backgroundLane` states `tag`, which the rule does not, so it does not cover).
  - `TestAHigherCustomCatchAllBeatsIt`: save a custom `@when()` rule at precedence 500 through `routingRuleSaveBuiltin`, then the strong demotion -> its name.
  - `TestRuleCoversIsConservative`: `touches="v1:identity:"` covers `touches="v1:identity:user"`, not the reverse; a present empty `prompt=""` does not cover an absent `prompt`.
  - `TestParseApprovedRuleRefusesLockedAndDisabled`.
- [ ] **Step 2: Run** `go test ./component/memql/ -run 'RuleBeatenBy|NothingBeats|HigherCustomCatchAll|RuleCovers|ParseApprovedRule' -count=1` and confirm they fail to compile (functions undefined).
- [ ] **Step 3: Implement** the functions above; rename `configurationAuthor` at its three callers in `routing_configuration_rules.go`, `rule_assist.go` and `policy_assist.go`; move the post-construction checks of `ruleFromConfigurationArgs` into `stageConfiguredRule`.
- [ ] **Step 4: Run** the new tests plus `go test ./component/memql/ -run 'Routing|Policy|RuleProposal|Rule' -count=1`; all pass, including `TestRoutingRuleSaveRefusesAPrecedenceTie` and `TestRoutingConfigurationRulesConsumeCustomPolicies` unchanged.
- [ ] **Step 5: Commit** the four Go files and the test.

### Task 2: ApplyRoutingReview

**Files:**
- Modify: `component/memql/routing_review_apply.go`
- Test: `component/memql/routing_review_apply_test.go`

**Interfaces:**
- Consumes: Task 1's `stageConfiguredRule`, `ruleBeatenBy`, `parseApprovedRule`, `RoutingConfigurationAuthor`.
- Produces:
  - `type RoutingReviewChange struct { SaveName, SaveSource, RetireName string }`: exactly one of `SaveSource` (with `SaveName`) or `RetireName` is set.
  - `type RoutingReviewApplied struct { Rule string; Revision int64; Changed bool }`
  - `func (e *MemQLEngine) ApplyRoutingReview(ctx context.Context, change RoutingReviewChange) (RoutingReviewApplied, error)`

Behaviour (spec section 2): author check first (`updatedBy` = the author's id); save path refuses text naming a rule other than `SaveName`, a tie (message names the rule holding the precedence and says to remove or re-number it in Fleet > Routing > Rules, or decline), and a beaten rule (message names the beating rule); retire path refuses a name in `e.rules` (shipped or product bundle) and answers `Changed: false` for an absent rule. A document already holding a JSON-identical rule (save) or lacking the rule (retire) writes nothing. On a `CompareAndSwap` error, re-read; if the revision moved, re-validate and retry, three attempts in all; otherwise return the error.

- [ ] **Step 1: Write the failing tests:**
  - `TestApplySavesTheExactApprovedRuleUnderANewRevision`: as a developer (`auth.RoleDeveloper`), save the strong demotion; `store.doc.Revision` is old+1, `UpdatedBy` is the developer, `store.doc.Rules["evidenceDemotion_m_strong"]` has `Excludes == []string{"fleet:m"}`, `Precedence == 50`, `When.Level == "strong"`; a peer `policyFixture(store).SnapshotRouting(ctx, base)` holds it.
  - `TestApplyIsANoOpWhenAlreadySaved`: a second identical apply returns `Changed: false` and the revision is unmoved.
  - `TestApplyRefusesAWriter`, `TestApplyRefusesANameMismatch`, `TestApplyRefusesABeatenRule` (fast demotion -> error naming `fastLane`, revision unmoved).
  - `TestApplyRefusesASecondDemotionAtTheLevel`: save `evidenceDemotion_a_strong`, then `evidenceDemotion_b_strong` -> error contains `"evidenceDemotion_a_strong"`, `Rules` length 1.
  - `TestApplyRetiresTheDemotion`: retire removes it under a new revision; retiring again is `Changed: false`; retiring `fastLane` is refused.
  - `TestApplyRetriesOnceRoutingMoved`: a `movingStore` (wraps `testPolicyStore`, on its first `CompareAndSwap` saves an unrelated policy override first, so the revision moves) -> the apply succeeds at old+2 and the override survives.
- [ ] **Step 2: Run** `go test ./component/memql/ -run 'TestApply' -count=1`; confirm failures.
- [ ] **Step 3: Implement** `ApplyRoutingReview`.
- [ ] **Step 4: Run** the same command plus Task 1's; all pass.
- [ ] **Step 5: Commit.**

### Task 3: The fold proposes only where it works

**Files:**
- Modify: `integrations/router/evidence_workflow.go`, `integrations/router/integration.go`, `integrations/router/evidence_fold.go` (header comment)
- Test: `integrations/router/evidence_policy_test.go`

**Interfaces:**
- Consumes: Task 1's `RoutingRuleBeatenBy`, through a type assertion on the router integration's engine: `interface{ RoutingRuleBeatenBy(context.Context, string) (string, error) }`.
- Produces:
  - `type ruleChecker func(ctx context.Context, source string) (string, error)`
  - `func proposeEvidence(ctx context.Context, w routerlib.Window, direction string, validity float64, measured bool, render routerlib.Renderer, beatenBy ruleChecker) (routerlib.Proposal, string, bool, error)`: the string is the beating rule's name when a demotion is withheld. A nil checker on a demotion is an error, as a nil renderer is. Promotions are not checked (their text is never saved).
  - `func (i *Integration) ruleBeatenBy(ctx context.Context, source string) (string, error)`: refuses when the engine lacks the method.
  - The fold result gains `proposalsWithheld` (int); a withheld window logs `model`, `level` and `beatenBy` at Info, and its evidence row records `none` (unchanged DSL).

- [ ] **Step 1: Update the test helpers** (`proposeExclusion`, `proposePromotion`, the direct call in `TestTheWhenBlockCarriesOnlyTheKeysThatAreSet`) to pass `noRuleBeatsIt := func(context.Context, string) (string, error) { return "", nil }` and the new return arity. Add `TestABeatenDemotionIsWithheld` (checker returns `"fastLane"` -> ok false, withheld `"fastLane"`, empty proposal), `TestADemotionNeedsAChecker` (nil checker -> error), `TestAPromotionIsNotChecked` (checker that fails the test if called).
- [ ] **Step 2: Run** `go test ./integrations/router/ -count=1`; confirm the new tests fail.
- [ ] **Step 3: Implement**, passing `i.ruleBeatenBy` from `routingEvaluateEvidence` and counting `withheld`.
- [ ] **Step 4: Run** `go test ./integrations/router/ -count=1`; pass.
- [ ] **Step 5: Commit.**

### Task 4: Owners and developers can see routing reviews

**Files:**
- Modify: `dsl/work/queries.memql`, `dsl/work/builtins.memql`, `integrations/work/integration.go` (capability), `integrations/work/internal_origin_test.go`
- Create: `integrations/work/routing_review.go`, `integrations/work/routing_review_db_test.go`
- Regenerate: `sdk/go/client/generated_*.go`, `sdk/ts/src/client/generated_*.ts` (`make sdk-gen`)

**Interfaces:**
- Produces:
  - DSL: `@serverOnly @actor query approval workRoutingReviewsForCluster { args { approvalId string } filter row => actor.isClusterOwner == true && row.kind == "routingReview" && ((args.approvalId == nil && row.decision == "") || row.id == args.approvalId) sort "requestedAt", "asc" paginate 100 shape workApprovalFull }`, with a doc comment saying why it selects on kind alone (spec section 1).
  - DSL: `@sdk @executor("integration.work.routingReviews") builtin workRoutingReviews { approvalId string }`.
  - `func (i *Integration) routingReviews(ctx context.Context, approvalId string) ([]map[string]any, error)`: runs the query through `i.store().queryInternal` under `auth.ContextWithSystemActor(memql.ContextWithFreshRead(ctx), "routingReview")` built inline as the argument. Callers check the author first.
  - `func (i *Integration) handleRoutingReviews(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error)`: `requirePrincipal`; a synthetic caller is refused; a caller failing `memql.RoutingConfigurationAuthor` gets zero nodes and no read; otherwise the rows as `v1:work:approval` nodes.
  - Test helper (DB file): `func raiseRoutingReview(t *testing.T, eng *memqlengine.MemQLEngine, p routerlib.Proposal) string`: writes `createWorkApproval` under the fold's maintenance actor exactly as `openRoutingReview` does (claims, token, access, internal origin; subject and hash from `work.RoutingReviewApproval`).

- [ ] **Step 1: Write the failing tests:**
  - `internal_origin_test.go`: add `routingReviews` to `TestCapabilitiesRefuseAnUnauthenticatedCaller`.
  - `routing_review_db_test.go`, `TestRoutingReviewsDBAreTheirDecidersOnly`: one raised review; `handleRoutingReviews` lists it for `auth.RoleOwner` and `auth.RoleDeveloper` callers; a `auth.RoleWriter` and an `auth.RoleAdmin` get zero rows and no error. A review written with owner `system:maintenance:routingEvidenceFold` through the internal path with the owner field set explicitly is listed too (Review Focus 4).
- [ ] **Step 2: Run** `go test ./integrations/work/ -run 'RoutingReviews|CapabilitiesRefuse' -count=1` with the DB env; confirm failures.
- [ ] **Step 3: Implement** the query, builtin, capability and handler; run `make sdk-gen`.
- [ ] **Step 4: Run** the step 2 command and `go test ./integrations/work/ -run 'CapabilityNamesMatchTheDSL|RegistrationName' -count=1` plus `make sdk-gen-check`; pass.
- [ ] **Step 5: Commit** (DSL, Go, generated SDK files by path).

### Task 5: Deciding a routing review applies it

**Files:**
- Modify: `integrations/work/approval.go`, `integrations/work/routing_review.go`
- Test: `integrations/work/routing_review_db_test.go`

**Interfaces:**
- Consumes: Task 2's `ApplyRoutingReview` through `type routingReviewApplier interface { ApplyRoutingReview(context.Context, memqlengine.RoutingReviewChange) (memqlengine.RoutingReviewApplied, error) }` asserted on `i.engine` (absent -> refuse "routing configuration is unavailable on this node"); Task 4's `routingReviews`.
- Produces:
  - In `handleDecideApproval`: an approval absent from the caller's pending list is looked up with `routingReviews(ctx, approvalId)` when `RoutingConfigurationAuthor(ctx)` passes, before the recovery chain; `recoverRoutingReviewDecision` joins the chain. `answered` on a routing review is refused before anything is read further. After the artifact gate, a routing review returns through `decideRoutingReview` and never reaches training or resume.
  - `func (i *Integration) decideRoutingReview(ctx context.Context, approval map[string]any, decision, decidedBy string, now time.Time) ([]memorynodes.MemoryNode, error)`: for `approved`, maps `subject.direction` with `component/router` constants: demotion -> `RoutingReviewChange{SaveName: subject.ruleName, SaveSource: subject.ruleSource}`; promotion -> `RetireName: routerlib.RuleName(routerlib.DirectionDemotion, subject.modelId, subject.level)`; anything else refused. Applies, then records with `decideApprovalRow` under `auth.ContextWithSystemActor(ctx, "routingReview")` (built inline). Reply: `{approvalId, runId: "", decision, runResumed: false}` plus, when approved, `routingRule`, `routingRevision`, `routingChanged`.
  - `func (i *Integration) recoverRoutingReviewDecision(ctx context.Context, approvalId, decision string) (map[string]any, error)`: an author whose review is already decided `decision` gets `{approvalId, runId: "", decision, runResumed: false}`; otherwise nil.

- [ ] **Step 1: Write the failing DB tests** in `routing_review_db_test.go`, each starting from the configuration's current revision and removing what it saved in `t.Cleanup` (the database is shared):
  - `TestRoutingReviewDBDemotionIsSavedAndSeenByAPeer`: two engines (`openWorkTestEngine` twice). A developer approves a strong demotion through `handleDecideApproval` on engine A: reply has `routingChanged: true` and revision old+1; engine B's `eng.Policies().SnapshotRouting(ctx, eng.Rules())` holds the rule with `Excludes == ["fleet:<model>"]`; the review row reads `decision: approved` and `decidedBy` the developer. A second decide (an owner) answers the recorded decision, revision unmoved (Review Focus 2).
  - `TestRoutingReviewDBSecondDemotionAtTheLevelIsRefused`: with one saved, approving another strong demotion errors naming the first; the review stays pending; rejecting it then succeeds and the revision is unmoved (Review Focus 3, with a rule of the person's own at 50 as a second case).
  - `TestRoutingReviewDBPromotionRetiresTheDemotion`: save a demotion, raise a promotion for the same model and level, approve it: the demotion is gone under a new revision, and engine B no longer holds it.
  - `TestRoutingReviewDBWriterIsRefused`: a writer's decide gets the existing "no pending approval" error; nothing changes.
  - `TestRoutingReviewDBRejectChangesNoRouting` and `TestRoutingReviewDBAnsweredIsRefused`.
- [ ] **Step 2: Run** `MEMQL_REQUIRE_DB=1 go test ./integrations/work/ -run 'RoutingReview' -count=1 -p=1`; confirm failures.
- [ ] **Step 3: Implement** the decide branch, `decideRoutingReview` and the recovery.
- [ ] **Step 4: Run** the step 2 command, then the whole package `MEMQL_REQUIRE_DB=1 go test ./integrations/work/ -count=1 -p=1 -timeout 40m`; pass.
- [ ] **Step 5: Commit.**

### Task 6: Wording and docs match the behaviour

**Files:**
- Modify: `component/work/approval_routing.go` (+ `approval_routing_test.go`), `dsl/common/builtins.memql` (`routingEvidenceFold` description), `dsl/router/automations.memql` (both reasons), `component/automations/steps/testdata/automation_corpus/router/routingEvidenceProposal.json` (regenerated), `docs/public/operate/ai-routing.md` ("Rules the nightly review proposes"), `docs/superpowers/specs/2026-09-07-levels-policies-rules-design.md` (end of C5), the spec's status line.

Copy, exact:
- Reasons: demotion ends "Approving adds a rule excluding it at that level; declining records the decision so this week's evidence does not ask again."; promotion ends "Approving retires the exclusion; declining records the decision so this week's evidence does not ask again."
- `RoutingReviewApproval` options follow `p.Direction` (Global Constraints labels).
- The fold description and ai-routing.md say: approving saves the rule into the routing configuration under a new revision, which every replica reads on the next call (a promotion removes the demotion); owners and developers decide; the review proposes only at levels a saved rule can decide, which today is `strong`, because the shipped locked rules `fastLane`, `reasoningParks` and `embeddingsBound` match every call at theirs; one evidence demotion per level, a second refused naming the first.

- [ ] **Step 1: Write the failing test** `TestRoutingReviewOptionsFollowTheDirection` in `component/work/approval_routing_test.go` (labels above for both directions; values `approved` / `rejected`).
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/work -run RoutingReview -count=1`; fails.
- [ ] **Step 3: Implement** the label; edit the DSL, docs and records; regenerate the golden with the corpus test's `-update` flag (`TestAutomationCorpusRuns`, in the module that owns `component/automations/steps`).
- [ ] **Step 4: Run** the component/work test, the corpus test without `-update`, and `go test ./test/dslconformance/ -count=1`; pass. `git diff` on the golden shows only the two reason strings.
- [ ] **Step 5: Commit.**

### Task 7: Nexus shows and decides routing reviews

**Files:**
- Modify: `clients/os/src/apps/nexus/useNexus.ts`, `useNexusDirectory.ts`, `NexusApp.tsx`, `ApprovalsSection.tsx`, `words.ts`
- Test: `clients/os/test/nexus/harness.tsx`, `clients/os/test/nexus/approvals.test.tsx`

**Interfaces:**
- Consumes: the generated TS SDK method `workRoutingReviews({ approvalId? })` (Task 4).
- Produces:
  - `export function useRoutingReviews(enabled = true): LiveCollectionHandle<Row>` in `useNexus.ts`: key `"work:routing-reviews"`, seed `workRoutingReviews({})`, reread `workRoutingReviews({ approvalId })`, `inScope` kind `routingReview` and pending, `paged: false`.
  - `useApprovalDirectory` merges the person's browse feed with `useRoutingReviews` through `useMergedPagedCollections` (the users roster's pattern), oldest first, same search match; `NexusApp`'s overview count includes them; `useWorkRecord("approval", id)` falls back to `workRoutingReviews({ approvalId })` and admits a `routingReview` row whose owner is not the viewer.
  - After a decision whose reply carries `routingRevision` or whose approval kind is `routingReview`, the section calls the routing reviews collection's `reseed()`.
  - `approvalDecidedNext(kind: string, decision: string, direction = ""): string`; `approvalKindMeaning("routingReview")` = "The nightly review of routing measured a model at a level and proposes a change to the routing rules. Approving makes exactly this change; nothing is parked on it."

- [ ] **Step 1: Write the failing tests** (`harness.tsx` gains `seed.routingReviews` and a `workRoutingReviews` mock):
  - Replace the assertion in "the routing review" test with: the review is seeded only through `routingReviews` with `ownerUserId: ""`, it is listed, the meaning sentence above is shown, and nothing says routing is unchanged.
  - "approving a routing review re-reads the list": decide -> `workRoutingReviews` called again, and the row leaves the list once the mock returns it decided (Review Focus 5).
  - `words` unit cases: approved demotion "The rule was added.", approved promotion "The exclusion was removed.", rejected "Routing was left as it is.".
- [ ] **Step 2: Run** `make os-install` once (builds sdk/ts and installs the client), then `cd clients/os && npx vitest run test/nexus`; confirm failures.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd clients/os && npx vitest run test/nexus` and `make os-typecheck`; pass.
- [ ] **Step 5: Commit.**

### Task 8: Verify, land, clean up

- [ ] **Step 1:** `make identity-tailwind` (fresh worktree), then with the DB env exported: `make test`, and the touched DB trees (`./component/memql/...`, `./integrations/work/...`) with `MEMQL_REQUIRE_DB=1 go test -count=1 -p=1 -timeout 40m`. All pass, or a failure is shown to be unrelated by running it on `origin/main`.
- [ ] **Step 2:** `make sdk-gen-check`, `make frontdoor-paths-check` (unchanged expected), `make os-test`, `make os-typecheck`.
- [ ] **Step 3:** Delete this plan file (a spent plan is deleted in its own merge, docs/CLAUDE.md) and mark the spec implemented, in one commit.
- [ ] **Step 4:** `git fetch origin main`, rebase onto it, re-run the touched packages' tests, push to `main` as the owner, and watch the `MemQL / memql / push` check on the pushed commit.
