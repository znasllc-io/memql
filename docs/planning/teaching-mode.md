# Teaching Mode (planning)

**Status:** not started — scope + design notes only
**Owner:** tbd
**Related:** walkthrough mode (shipped), `uiNarrate` / `uiAskUser` primitives (shipped), per-user general_assistant with CoPresent Control (shipped)

## Problem

Walkthrough mode today drives a single focused flow end-to-end (e.g. "create
an agent"). Teaching mode is the longer, multi-flow cousin: a guided tour of
the whole app, a training course, a multi-step curriculum where the agent
pauses between flows to explain concepts, quiz the learner, and re-demo
sections on request. The primitives are all there (`uiNarrate`, `uiAskUser`,
`uiHighlight`, `uiPointerTo`, the CoPresent Control Widget conversational surface);
the gap is the higher-level choreography and the stamina-ish guardrails a
teaching session needs that a one-shot walkthrough doesn't.

## What we know we'll need

1. **Iteration budget:** teaching sessions chain multiple walkthroughs back
   to back. A single full Create Agent + Create Space + Settings tour can
   easily exceed 100 loop iterations. The unified
   `MEMQL_TOOL_LOOP_MAX_ITERATIONS` (component/memql/config.go +
   integrations/agent/streaming.go) caps at 200 today. Good enough for a
   first pass; we may need session-scoped budget accounting if tours run
   longer.

2. **Curriculum concept:** a `v1:training:curriculum` concept that lists
   topics, each topic maps to a walkthrough goal. The CoPresent frontend
   already has a `Curriculum` + `Segment` pair used by the onboarding tour
   (see src/hooks/useRunOnboarding.ts) — we can either extend those or
   mirror them agent-side. Decide once we're ready to land.

3. **Session-scoped progress state:** what's been taught, what's next,
   what to loop back to if the learner asks a clarifying question.
   Lives in OperatorMemory (see concepts/v1/copresent/operatormemory)
   or a purpose-built `v1:training:session` concept.

4. **Interactivity tier:** `interactivity: "teaching"` as a third value
   on `delegateTakeover` alongside `minimal` / `conversational`. Teaching
   mode implies conversational UI + longer narration cadence + periodic
   "any questions?" checkpoints via uiAskUser.

5. **Knowledge retrieval:** teaching-specific knowledge domains (not just
   `copresent_ui`). A training corpus per subject area with deeper
   explanatory content than the navigational chunks we seed today.

6. **Pacing + pause behavior:** teaching sessions need to RESPECT pauses
   (the learner might be reading, following along, typing notes) in a
   way walkthroughs don't. Prompt rules around "wait for the learner's
   'got it' before moving on" — the opposite of the current "never
   stop mid-form" discipline.

## Open questions

- Is teaching mode a new tool (e.g. `startTeachingSession`) or just a new
  `interactivity` value on delegateTakeover?
- Where does the curriculum live — as a DSL-defined `v1:training:curriculum`
  concept authored in .memql files, or user-curated via a builder UI?
- Do we record what a learner completed for credentialing / progress?
  Links to `v1:training:session` + OperatorMemory.
- How does teaching mode interact with multi-user spaces? Is a teaching
  session one-to-one or can a cohort share it?

## When to pick this up

After the walkthrough work settles and we have confidence the
narration-cadence + iteration-cap story holds up in real use. Nothing
here is urgent.
