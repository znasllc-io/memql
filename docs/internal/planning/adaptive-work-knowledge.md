---
title: Adaptive work knowledge and evidence
audience: internal
status: draft
area: planning
sinceVersion: 0.25.0
owner: znas
---

# Adaptive work knowledge and evidence

The person supplies the desired outcome and constraints. The harness combines
that goal, observed work, tools, current instructions and selected Training to
determine the next useful action. Technical workflow coaching by an evaluator is
an intervention to measure, not a substitute for this behavior.

## Current implementation and boundaries

Initial step skills are versioned snapshots. Reviewed Training passages are
retrieved for the initial section and goal. Live work now has a bounded native
`consultWorkKnowledge` operation for requirements discovered later, and direct
access to existing semantic artifact search. Current review/source admission and
selection receipts are reused. No new Training UI, role creation flow or corpus
installation is involved. Selection is not a sufficient-knowledge assessment;
the consultation explicitly leaves readiness unassessed. General guidance asks
the model to assess applicability and resolve gaps from evidence. This first
slice enables adaptation; it is not the completed learning system below.

## Connected decisions

1. Infer a small set of current requirements from the requested outcome and
   observed environment. Refine them when evidence changes. Roles organize
   capabilities; they do not imply that every associated manual belongs in
   every context. Select applicable specialties and dependencies without
   dropping mandatory instructions or permission constraints.
2. Reuse evidence and procedures whose scope and source revisions still apply.
   Retrieve only missing knowledge. Track requirements separately from candidate
   matches, applicability judgments, unresolved gaps and subsequent results.
   A retrieval score or model confidence is not a competence test.
3. Research through permitted concept queries, indexed record passages,
   conversations, files in blob storage, tool manuals and external sources.
   Resolve hits to source contents. Keep structured exact queries for known
   identities; use similarity for discovery. Neither should replace the other.
4. Check whether evidence supports the next action. A missing prerequisite can
   lead to bounded research, a smaller experiment, another permitted tool or a
   focused human question. Distinguish insufficient knowledge, incomplete
   indexing, missing access, incompatible instructions and a genuine user
   decision. Do not ask the user to teach an ordinary technical workflow.
5. Verify outputs at their intended destination. Preserve evidence, useful
   procedures and failed approaches with applicability and limits. New material
   is a candidate for learning, not automatic proof of a general lesson.
   Existing review and procedure-certification gates remain in force.
6. Compare later use with the original evidence, including counterexamples.
   Refresh changed sources instead of repeating the entire investigation.
   Measure whether reuse reduced work without degrading correctness.

## Indexing and retrieval work still required

Index source identities and revisions alongside useful extracted passages,
content hashes and extraction/index status. Keep source bytes in their existing
blob store; do not embed entire blobs or copy workspaces into the evidence store.
Large, binary or unsupported files need explicit extraction/vision coverage and
size bounds. Preserve source links so a passage can be independently inspected.
Authorization must remain current at candidate selection and source readback;
revocation or supersession must invalidate reuse even if an embedding remains.

The existing Library search uses bounded over-fetch before owner filtering.
Replace that coverage approximation with permission-scoped candidate ranking and
test that another owner's closer matches cannot crowd out authorized results.
Reconcile source revisions with extraction and vector revisions; report pending,
failed, stale, unsupported and partially indexed material distinctly. An absent
match may justify an authorized direct query/read, not a claim that information
does not exist. Avoid silently running unlimited reindexing as a research step.

A uniform requirement/evidence ledger and calibrated readiness evaluation remain
follow-up work. Do not label a self-reported assessment as independent validation
or claim autonomous learning from the new consultation tool alone. Retained
candidate lessons, reviewed reusable knowledge and certified procedures have
different trust and lifecycle states.

## Evaluation

Repeat the original plain-language Ask change in a new MemQL conversation after
the harness changes are tested. Preserve the previous attempt's evidence first;
start from a comparable application/source state. Use Apps routing, Astra and
extra-high effort, and record actual model receipts and any fallback. MemQL must
perform the source work, rebuild/reload discovery, computer use and before/after
verification itself. Record every evaluator correction rather than hiding it as
autonomous behavior. Keep the owner's separate CI/merge/cleanup hold in force.

Compare first-response latency, active elapsed time, approval waiting, token use,
tool calls, failed actions, corrective interventions and verified outcome. Record
cold/warm caches, reused knowledge, different commits and missing measurements.
One supervised before/after task can identify improvement or regression in that
case; it cannot establish general capability. Include later document creation,
research and other task categories, short and long goals, missing Training,
stale sources, revoked access and deliberately unrelated high-scoring matches.
The evaluation/evidence suite owns aggregate results and public presentation;
this plan defines harness behavior and its honest verification boundaries.
