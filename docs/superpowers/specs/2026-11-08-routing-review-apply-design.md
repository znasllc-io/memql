# Approving a routing review changes routing

**Date:** 2026-11-08. **Owner:** znas. **Status:** proposed.
**Builds on:** epic memql#5146 D5 (the nightly evidence review) and C5 of
[levels, policies and rules](2026-09-07-levels-policies-rules-design.md), which
made the routing configuration document the one way a custom rule exists.

## Why

The nightly fold (`routingEvidenceFold`) opens a `v1:work:approval` of kind
`routingReview` whose `subject.ruleSource` is the exact rule text
`component/routingrules.GenerateRule` rendered. Approving it applies nothing:
`handleDecideApproval` has no `routingReview` branch, and since a272019c0 the
wording says so. Three more findings stand between that approval and a routing
change:

1. **Nobody can decide one.** Measured on the CI image: the fold writes the
   approval as its maintenance principal, the engine blanks a synthetic actor's
   owner stamp (`undoNonPrincipalOwnerStamp`), and the row is stored with
   `ownerUserId: ""`. Every Nexus read (`workApprovalsForOwner`,
   `workApprovalsBrowse`, `workApprovalForOwner`) and `decideApproval` resolve
   through `row.ownerUserId == actor.userId`, so neither an owner nor a writer
   lists it, and decide answers "no pending approval ... is readable by this
   caller".
2. **A saved rule cannot decide a call at three of the four levels.** The
   router takes the first matching rule, and every shipped rule is `@locked`,
   which evaluates before any custom rule. `fastLane`, `reasoningParks` and
   `embeddingsBound` match every call at `fast`, `reasoning` and `embeddings`,
   so a demotion saved at those levels never decides a call. Only a `strong`
   demotion can.
3. **Evidence rules at one level do not combine.** Every proposal uses
   precedence 50, so a second demotion at a level ties and the save refuses it.
   At any other precedence the first matching rule would hide the other.

## Decisions (the owner, 2026-11-08)

- **D1.** Owners and developers see and decide routing reviews: the people
  `routingRuleSave` admits (`auth.CanAuthor`).
- **D2.** The saved rule is parsed from the exact approved text, not rebuilt
  from the proposal's fields.
- **D3.** The fold proposes only a rule that can decide a call, and approving
  refuses a rule another rule always beats. Making exclusions work at every
  level is a follow-up.
- **D4.** One evidence demotion per level for now. Precedence stays 50; a
  second demotion at a level is refused, naming the saved rule.
- **D5.** Stated as defaults with the questions and not challenged: a
  promotion removes the matching demotion and adds nothing; a configuration
  that changed since the proposal is re-validated and saved against the
  current revision; the apply runs under the approval's lock before the
  decision is recorded, so a retried decide changes nothing.

## Design

### 1. Who sees and decides (D1)

- **One rule for "may change routing".** `configurationAuthor` in
  `component/memql/routing_configuration_rules.go` is exported as
  `RoutingConfigurationAuthor`. The routing builtins keep calling it, and the
  work integration calls the same function, so the two cannot disagree.
- **The read.** A new `@serverOnly` query, `workRoutingReviewsForCluster`,
  returns routing reviews: `actor.isClusterOwner == true &&
  row.kind == "routingReview"`, the pending ones, or one by id whatever its
  decision. It selects on the kind alone because only the fold raises one:
  every routing review is the cluster's, including any raised before
  memql#4817's owner undo, which still carry the `system:maintenance:` id
  that `row.ownerUserId == ""` would hide forever.
  The work integration runs it under `auth.ContextWithSystemActor(ctx,
  "routingReview")`, and only after the caller passed the author check.
  Not a row-authz change: `unowned="developer"` is legal only beside
  `rankVisible`, which would let every higher-ranked person read every
  lower-ranked person's approvals.
- **The surface.** `@sdk builtin workRoutingReviews { approvalId string }`
  (`integration.work.routingReviews`) answers the pending reviews, or one
  review by id, decided included, so a decided one can show its receipt. A
  signed-in person who may not change routing gets no rows, because they have
  none to decide. An anonymous, connector or synthetic caller is refused.
- **Deciding.** When the approval is not in the caller's own pending list,
  `decideApproval` looks for it through the same read, after the same author
  check. The decision is written under the system actor with internal origin,
  and `decidedBy` is the person.

### 2. Applying (D2, D4, D5)

`(*MemQLEngine).ApplyRoutingReview`, beside `routingRuleSave` in
`routing_configuration_rules.go`, takes the direction, the approved rule text
and name, and for a promotion the name of the rule to retire.

- The caller must pass `RoutingConfigurationAuthor`; `updatedBy` is the person.
- **Demotion.** `ParseRuleDecl` plus `ruleDeclToRuleConfig` on the exact text.
  Text carrying `@locked` or `@disabled`, or naming a rule other than the
  approval's `ruleName`, is refused. Then the checks `routingRuleSave` runs,
  extracted from `ruleFromConfigurationArgs` into one helper both paths call:
  shipped name, policy exists in the current snapshot, closed condition keys,
  modality, precedence range, exclusion entries, and `configuredRules` for
  duplicate names and ties. A tie is refused naming the rule that holds the
  precedence. A rule that some earlier rule always beats (section 4) is
  refused naming that rule.
- **Promotion.** Removes the demotion `evidenceDemotion_<model>_<level>`. The
  work integration derives that name with `component/router.RuleName`, since
  `component/memql` cannot import that module. A shipped name is refused; an
  absent rule means nothing to do. The promotion's own text is never saved: it
  excludes nothing, would send the whole level to `localFirst`, and would tie
  at 50.
- **Nothing to do is not a change.** If the document already holds exactly
  this rule, or the demotion is already gone, no revision is written.
- **Another writer in between.** `CompareAndSwap` against the revision just
  read. If it moved, re-read and re-validate against the newer document, up to
  three attempts. A proposal is refused only when it fails validation against
  routing as it is now, never merely because routing changed since the fold
  ran: the person who approves it never saw a revision.
- The reply carries the rule's name, the revision and whether anything changed.

### 3. Order inside decideApproval (D5)

Author check (routing reviews only), resolve, the existing artifact-hash gate
(the subject hash covers `ruleSource`), then for `approved` the apply, then the
decision is recorded. The reply adds `routingRule`, `routingRevision` and
`routingChanged` to `{approvalId, decision, runResumed: false}`.

- A refusal at apply records nothing, so the person reads why and can decline.
- A crash between apply and record: the retry finds the review pending, the
  apply is a no-op, and the decision is recorded.
- A retry after the decision landed: the review is no longer pending, and a
  recovery read answers with the recorded decision and applies nothing.
- Declining records the decision and changes no routing.
- The existing decision gate serializes decisions on one approval. Two
  approvals at one level race at `CompareAndSwap`; the loser is re-validated
  and refused as a tie.

### 4. The fold proposes only where it works (D3)

- `(*MemQLEngine).RoutingRuleBeatenBy(ctx, source)` names the rule that would
  evaluate first and match every call the given rule matches if it were saved
  now, or nothing.
- "Matches every call it matches" is decided conservatively: an earlier rule P
  beats R when every condition P states is stated by R with the same value
  (for `touches`, R's prefix extends P's). Earlier means a locked rule other
  than `default`, which always sorts last, or an unlocked rule of higher
  precedence.
- The router integration reaches it through its engine, the way it reaches
  the renderer: a node without it refuses to propose. After rendering a
  demotion the fold asks. A beaten rule opens no approval, the evidence row
  records `none`, a log line names the model, the level and the beating rule,
  and the fold's result counts `proposalsWithheld`.
- Today this withholds every demotion at `fast`, `reasoning` and `embeddings`.

### 5. Wording and docs

The interim wording a272019c0 added goes, with two corrections to what was
there before it:

- The option label follows the direction: "Add the rule" for a demotion,
  "Remove the exclusion" for a promotion.
- Nexus's decided notice takes the direction from the subject: "The rule was
  added." or "The exclusion was removed."; a decline still reads "Routing was
  left as it is."
- Also: the `routingEvidenceFold` description in `dsl/common/builtins.memql`,
  the two reasons in `dsl/router/automations.memql` with the automation
  corpus golden, `approvalKindMeaning` and its test assertion, the "Rules the
  nightly review proposes" section of `docs/public/operate/ai-routing.md`, and
  the end of C5, which points here.

### 6. MemQL OS (Nexus)

- A live collection of routing reviews, seeded and re-read through
  `workRoutingReviews`.
- Nexus merges it into the approvals overview and the approvals list, with the
  merged-feed helper the users roster uses. The selected record reads a
  routing review through the same builtin.
- A routing review decision re-reads that collection: row admission delivers
  a row nobody owns to cluster owners only, so a developer gets no broadcast
  when it is decided.

## Tests

- **component/memql:** the beaten-by check (`fastLane` beats a `fast`
  demotion, nothing beats a `strong` one, a custom `@when()` rule at a higher
  precedence beats it); the apply's refusals (locked text, a name mismatch, a
  tie naming the holder); the no-op; and the retry against a store that moves
  once.
- **integrations/work, DB-gated on the CI image:** a review written the way
  the fold writes it is listed for an owner and a developer and not for a
  writer. A developer approves a demotion: the configuration gains the rule
  under a new revision, a second engine over the same database sees it in
  `SnapshotRouting`, and the decision records the developer. A retried decide
  answers the recorded decision with no new revision. A second `strong`
  demotion is refused with nothing recorded. A writer is refused. Approving a
  promotion removes the demotion under a new revision. Declining records only
  the decision.
- **integrations/router:** the fold withholds a beaten demotion and counts it,
  and still proposes a `strong` one.
- **The automation corpus golden** is regenerated for the reverted reasons.
- **clients/os:** Nexus lists a routing review from `workRoutingReviews`,
  tells the person before they decide that approving changes routing, and
  re-reads the list after a decision.

## Out of scope

- **Exclusions that work at every level and combine:** an exclusion held in
  the routing configuration and applied whichever rule matched. Until then
  demotions at `fast`, `reasoning` and `embeddings` are withheld.
- **Nothing raises a promotion.** The fold's host never passes a probe
  measurement, so the promotion branch of `routingEvidenceProposal` cannot
  fire. The apply side is ready for it.
- **A failing vendor model is never excluded.** The demotion excludes
  `fleet:<model>`, which matches fleet candidates only.
- **An expired review stays decidable**, as it is today.

## Known limits

- The beaten-by check is conservative. A rule beaten only by a combination of
  earlier rules is still proposed, and approving it saves a rule that decides
  nothing.
