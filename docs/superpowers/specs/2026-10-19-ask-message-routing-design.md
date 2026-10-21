---
title: Ask message routing -- answer, start a goal, or update the goals already running
audience: internal
status: draft
area: operate
sinceVersion: 0.30.0
owner: znas
---

# Ask message routing

Owner brief, 2026-10-19. While a goal runs, a message about it must change
that goal instead of starting a second one. The owner's example: "what's the
weather in Tucson?" starts a goal; "actually, I'm in Phoenix" a moment later
started a SECOND goal. The person never has to say which goal they mean, and
one message can apply to several related goals.

Every message gets one of four outcomes: answer it, start a new goal, update
one goal, or update several.

## Why it fails today

- `RunAsk` (`component/memql/ask.go`) calls `work.createGoal` for EVERY
  message before anything else. The Spine's triage
  (`dsl/planner/prompts/goalComplexityTriage.tmpl`) then decides inline answer
  versus background work, and it is told that "a new subject in this
  conversation is a new request even while earlier work waits". It never sees
  the running goals as destinations.
- Durable work guidance already exists (`component/memql/ask_guidance.go`,
  `integrations/agent/work_guidance.go`): a message recorded against a running
  root run reaches its agent loop at the next model or tool boundary. It
  reaches only ONE run, only while it is `running`, and only through an
  explicit composer mode. The "Guide this work" button that entered that mode
  was added in 171cdba6b and removed in 331b4d29c, so nothing reaches it.

## Decisions (owner, 2026-10-19)

| # | Question | Decision |
|---|---|---|
| D1 | Which goals is a message checked against? | Active goals in THIS conversation: preparing, working, or waiting on you. Not finished goals, not other conversations. |
| D2 | One message both corrects a goal and asks something new | Split it. The correction goes to the goal; the new part gets its own answer or goal. |
| D3 | A goal waiting on you | Answer, never approve. A question is answered; a permission request gets the message as a correction, never a yes; a budget pause saves it for when the limit is raised. |
| D4 | "Never mind the Tucson one" | Stop after a confirm: Stop / Keep on that goal's card. In a live call (no cards), a clear yes to the stop question is the confirm. |
| D5 | What the person sees | A label per goal on their message ("To Weather in Phoenix · Received"), a one-line reply, and the goal's card retitled. |
| D6 | Where the decision is made | Route first: a routing step before intake, only when the conversation has active goals. |
| D7 | Unclear target | Ask, do not guess: the reply is a question; nothing is delivered. |
| D8 | "Update" | Change the goal in place (its next step takes the message); not cancel-and-restart. |

## Flow

`RunAsk` is shared by typed Ask and live voice, so both get this behavior.

1. Validate and record the turn exactly as today.
2. **List the active goals** of this conversation: transcript turns whose run
   is `compiling`, `running` or `waiting`, not `cancelRequested`, with no
   `parentRunId`. At most 12, newest first.
3. **No active goals: today's path, unchanged.** No routing call.
4. **Messages over 4,000 characters are not routed** (the guidance limit),
   and neither are messages with attachments (guidance carries text only):
   today's path.
5. Otherwise run the **routing recipe** `askMessageRouting`
   (`dsl/os/automations.memql`) through `runScopedWorkflow`, the seam
   `routingPolicyProposalWorkflow` already uses. The recipe chooses the prompt
   and the one retry; Go supplies the operations and validates.
6. **Apply** a valid decision (below). An invalid decision after the retry,
   or a routing failure (no model, timeout), falls back to today's path and
   records an Activity entry saying routing was skipped.

The routing call is made for the person and under THEIR route: a model call
outside a run reads no run routing, so `ApplyRunRouting` falls back to the
turn's choice that `ContextWithGoalRouteChoice` already stamps for
`createGoal`. A person on Local is routed by a local model.

The routing call runs OUTSIDE the conversation write lock. Delivery re-reads
each goal under the lock (`ContextWithFreshRead`) before recording anything.
Every effect is durable (transcript row, run rows, approval rows), so routing
on one replica and execution on another needs no in-memory state.

### The recipe

```memql
@template
automation askMessageRouting {
 goals := builtin askRouteGoals()
 if goals.count == 0 { return "intake" }
 first := builtin askRouteDecide(prompt: "askMessageRouting", previousError: "", timeoutSeconds: 600)
 if first.valid { return "routed" }
 second := builtin askRouteDecide(prompt: "askMessageRouting", previousError: first.error, timeoutSeconds: 600)
 if second.valid { return "routed" }
 return "intake"
}
```

`askRouteGoals` and `askRouteDecide` are scoped operations: declared as
builtins, refused outside their workflow scope (like `routingProposalCatalog`).
The recipe sets each attempt's deadline (`timeoutSeconds`, waiting for a model
included, clamped by Go to 1..600, 600 when absent: the Spine classification's
budget); a timeout is a routing failure and the message goes to intake. A
shorter deadline was tried first and failed in local testing (2026-10-19): on
one local model the running goal held the model for longer than 30 s, routing
gave up, and intake opened a second goal behind the same model.

### The routing prompt

`askMessageRouting`, `@level("fast")`, `dsl/os/prompts/askMessageRouting.tmpl`.
Input: the message, a bounded slice of recent conversation (8 turns, 600
characters per side), the active goals as opaque handles `g1..g12` (title,
original request, state, what it waits on, updates already sent, whether it can
take a message now), goals with an unanswered stop question, an unresolved
clarification from the previous turn, and the clock. The model sees handles,
never ids.

Output (strict JSON schema):

```json
{
  "updates":      [{"goal": "g1", "title": "Weather in Phoenix"}],
  "stops":        ["g2"],
  "confirmStops": ["g3"],
  "newRequest":   "What time is it in Phoenix right now?",
  "reply":        "Switching the weather check to Phoenix.",
  "clarify":      "",
  "resolvesClarification": false
}
```

`resolvesClarification` is true only when the message answers the router's
own pending question; the earlier message then travels with this one.

### Validation (Go)

- Every handle is one of this call's handles; no duplicates within a list.
- A goal is not both updated and stopped.
- `confirmStops` only names goals with an unanswered stop question.
- `resolvesClarification` only when the previous turn is an unresolved
  clarification, and only alongside updates.
- `clarify` excludes every other action.
- `reply` is required (at most 280 characters) when anything is acted on or a
  question is asked; `title` at most 80; `newRequest` at most 4,000.
- No action at all means intake with the ORIGINAL message. `newRequest` is used
  only when the message was split (it accompanies another action); a message
  that is only a new request keeps the person's own words.

### Apply

In this order:

1. `confirmStops`: cancel each goal (`cancelGoal`, as the person) and record
   the decision on the stop question's turn.
2. `stops`: record a stop question on this turn. Nothing is cancelled.
3. `updates`: deliver the message to each goal by its state (table below);
   when delivered and a title was given, retitle the goal.
4. If every update target had finished before delivery and nothing else was
   decided, the message goes to intake (a late "actually Phoenix" still gets
   Phoenix).
5. `newRequest` (split): intake with that statement; the reply line comes first.
6. `clarify`: the reply is the question; nothing else happens. The next
   message's routing sees the unresolved message, and a resolution delivers
   both messages, verbatim, to the chosen goal(s).

The message is delivered WORD FOR WORD. The model picks destinations; it
never rewrites the person's words for a goal.

**An honest reply.** The model's line is a proposal. When any delivery fails,
Go replaces it with a plain account of what happened ("Sent to Weather in
Phoenix. Hike plan already finished, so it didn't get this.").

## Delivery by goal state

| Goal state | Delivery | Label |
|---|---|---|
| Working (`running`) | Work guidance; the agent reads it at its next model or tool boundary | Queued, then Received |
| Preparing (`compiling`) | Work guidance; read at the first step | Queued, then Received |
| Waiting on something other than you (timer, retry, repair, re-plan, sub-run, external) | Work guidance; read when it resumes | Queued, then Received |
| Waiting on your question (`feedback`, or a failure question) | `decideApproval(answered, {feedback: message})` | Answered |
| Waiting on a permission (`sideEffect`, `scopeElevation`) | The same call: guidance instead of approval; the goal reconsiders, nothing is granted | Sent, not approved |
| Paused on its budget (native checkpoint) | The same call saves the guidance; it applies when the limit is raised | Saved |
| Waiting on a request that takes no guidance (plan review and the others `ApprovalAcceptsGuidance` refuses) | Not delivered; the reply says to open the request | Not delivered |

- The existing per-run cap holds: 16 messages, 16,000 characters.
- Work guidance stays with its goal for the rest of the run: automatic
  retries, repairs, re-plans and a person's own re-run of a step all keep
  it (2026-10-19 ruling; the per-attempt guidance epoch is gone). A queued
  delivery whose run ENDED before reading it becomes **Not received**.
- Guidance never grants permission or raises a limit.

## Data

### The turn

The explicit guidance fields (`canGuideWork`, `guidanceTargetRunId`,
`guidanceStatus`, `guidanceEpoch`) are REMOVED. A routed message carries:

```go
type AskRouting struct {
	Targets   []AskRouteTarget  `json:"targets,omitempty"`
	Stops     []AskStopQuestion `json:"stops,omitempty"`
	Reply     string            `json:"reply,omitempty"`     // shown before a split request's own answer
	Statement string            `json:"statement,omitempty"` // what intake received when split
	Clarify   bool              `json:"clarify,omitempty"`
	Resolved  bool              `json:"resolved,omitempty"`  // a clarification a later message settled
	Skipped   string            `json:"skipped,omitempty"`   // why routing fell back to intake
}

type AskRouteTarget struct {
	GoalID     string `json:"goalId"`
	RunID      string `json:"runId"`
	Title      string `json:"title"`
	Status     string `json:"status"` // queued | received | answered | notApproved | saved | notDelivered | notReceived
	ApprovalID string `json:"approvalId,omitempty"`
	Reason     string `json:"reason,omitempty"` // ended | review | limit
	Text       string `json:"text"`             // exactly what the goal receives
}

type AskStopQuestion struct {
	GoalID   string `json:"goalId"`
	RunID    string `json:"runId"`
	Title    string `json:"title"`
	Decision string `json:"decision,omitempty"` // "" | stopped | kept | ended
}
```

A target with no `approvalId` is WORK GUIDANCE (read by the agent loop); one
with an `approvalId` was delivered through `decideApproval` and is never read
as guidance. Every snapshot re-projects a target's `title` from its run's
current title, so a retitle shows on the label without rewriting the turn.

`AskTurn.Routed *AskRouting` (`json:"routed,omitempty"`). The transcript is
a free `object` on `v1:os:askConversation`, so no concept field changes.
For a turn with no new goal, `Answer` holds the reply; for a split turn,
`Routed.Reply` holds it and `Answer` stays the new goal's own answer, so the
snapshot's run projection never overwrites it.

### The goal's title

A new optional field `title` on `v1:work:run` holds a title set after intake.
Ask (`ask_snapshot.go`) and Nexus (`apps/nexus/rows.ts`) prefer it over
`classification.workTitle`, which stays the intake decision. It is written by
a new owner-gated builtin `work.workRetitleRun {runId, title}` (executor `integration.work.retitleRun`) in
`integrations/work`. A separate field, because a late compile writing
`classification` must not erase a retitle.

### Reading guidance (agent side)

`readAskWorkGuidance` reads `Routed.Targets` for its run (no approval id,
queued or received), for the life of the run. Acknowledgement marks those
targets `received`. Ids are the turn id (one target per run per turn).

### Stop decisions

`askStopDecision {conversationId, turnId, runId, decision: "stop" | "keep"}`,
`@sdk`, owner-gated, idempotent: `stop` calls `cancelGoal`; both record the
decision. A run no longer active projects `ended`.

## Interface

Built from the existing Ask pieces and the kit, under `clients/os/DESIGN.md`.

- **The message.** One line per destination: "To Weather in Phoenix ·
  Received". Labels: Queued, Received, Answered, Sent, not approved, Saved,
  Not delivered, Not received. The byline keeps its "Guidance" tag, and
  Propose lesson stays on routed messages.
- **The reply** is a normal MemQL message. A split turn shows the reply line,
  then the new request's answer or acknowledgement with its usual work link.
- **Cards** pick up a new title on the next refresh.
- **Stop confirm.** That goal's card gains a row: "Stop this work? It can't be
  resumed." with Keep and Stop (destructive last), inside the kit's
  `InPlaceAsk`, so presses are held for a moment after it appears (rule 12).
  A card with an unanswered stop question is never folded into the
  "N tasks in progress" card. Card markup stops being one button: the open
  link and the confirmation are siblings.
- **Live voice** speaks the reply; a clear yes to the stop question confirms.
- **Clarification** is an ordinary MemQL message; the person answers in words.
- **Removed:** the explicit guidance mode (`beginWorkGuidance`,
  `workGuidance` state and its banner), the `askGuideWork` builtin and its SDK
  bindings, and the README text describing them.
- The Activity log names the routing call ("Routing message") and a skip.

## Failure handling

- Routing unavailable, timed out or invalid after one retry: today's path; an
  Activity entry records `skipped`.
- Retrying the same turn id returns the saved outcome and never routes or
  delivers again.
- The routing call is attributed to the person in the router's decision
  record. Its input is bounded as above.
- Delivery errors per target become that target's label; they never fail the
  whole turn unless nothing could be recorded.

## Testing

Red first (test-driven development):

- **Go unit:** decision validation; the delivery table; the honest reply;
  idempotent retry; the clarification lifecycle.
- **Database-gated, stub model:** Tucson to Phoenix (no new goal, guidance
  recorded, retitled); a split message; two related goals; question answered;
  permission corrected without a grant; budget saved; plan review not
  delivered; a goal finishing mid-route becomes a new request; no active goals
  makes no routing call; stop then confirm; stop then keep.
- **Cross-node:** a target recorded on one replica is read and acknowledged by
  an agent loop holding none of that replica's state; a preparing run's
  first execution receives a message recorded while it compiled.
- **DSL conformance and `make test`**, plus the database-gated lanes.
- **OS:** labels per state, reply ordering, retitle, stop confirm (both
  answers, the press hold), a folded queue with a pending stop, removal of
  guidance mode.
- **Visual:** QA page at desktop and narrow, light and dark.
- **Live:** the local cluster with a real model, through a scripted scenario
  list; results reported as observed.

## Rollout

- Built on `feature/ask-composer` (#6223), which changes the same files.
- One issue, one PR (engine and OS together: the wire contract changes for
  both). No product-repo change.
- Docs: the Ask section of `clients/os/README.md` and
  `docs/public/operate/ask-and-voice.md`.

## Out of scope

Goals in other conversations or Nexus; reworking goals that already finished;
quick-pick buttons for clarifications; undoing a delivery that went to the
wrong goal (the label shows where it went, and a follow-up corrects it).
