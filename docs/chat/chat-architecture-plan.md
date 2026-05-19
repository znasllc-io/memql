# Chat Architecture — Implementation Plan

> **STATUS (2026-05-18): SUPERSEDED — DESIGN INVERTED TO SINGLE-CHAT.**
>
> The two-thread (Group + per-user Team) design described below was
> abandoned in favor of a single-chat collapse:
> - `8975f33 single-chat assistant architecture (rename + collapse foundations)`
> - `a27db80 remove misroute classifier (dead code after single-chat collapse)`
>   — kills Phase 8 (Misroute safety net).
> - `c570875 drop copresentConversation tool, builtin, and Go integration`
>   — kills Phase 5 (`copresent_conversation` domain + tool).
> - `da2017c feat: askSpecialist tool + strict specialist gatekeeping` —
>   the post-collapse routing primitive that replaces per-user team threads.
>
> Phases 1, 4, 6, 7, 9, 10 may have partial overlap with current state;
> Phases 5 and 8 are dead. Treat this doc as historical context, not a
> work plan. Delete per the no-stale-docs convention once anything
> still relevant is consolidated elsewhere.

---

> **Status:** Brainstorm done (2026-05-09). Ten design decisions locked plus the activity-model architectural correction. Implementation not yet started.
>
> This document is the handoff for whoever (developer or AI agent) executes the chat-architecture initiative on the memql + copresent stack. It captures the working conventions, the locked decisions verbatim, the current state with file-path anchors, the phase-by-phase implementation plan, the cross-cutting conventions, and the quick-start.
>
> **Once Phase 10 lands, this document gets deleted** (per the project's no-stale-docs convention). Until then it is the canonical reference.

---

## Table of contents

0. [Pre-flight: how this user works and how to collaborate](#0-pre-flight-how-this-user-works-and-how-to-collaborate)
1. [Goal and motivation](#1-goal-and-motivation)
2. [Locked design decisions](#2-locked-design-decisions)
3. [Current state — file-path-anchored survey](#3-current-state--file-path-anchored-survey)
4. [Phase 1: Foundational data model + per-user team rosters](#4-phase-1-foundational-data-model--per-user-team-rosters)
5. [Phase 2: Two-tab chat UI (Group / Team)](#5-phase-2-two-tab-chat-ui-group--team)
6. [Phase 3: Roster panel + invite-button refactor](#6-phase-3-roster-panel--invite-button-refactor)
7. [Phase 4: Activity model — one active space per human](#7-phase-4-activity-model--one-active-space-per-human)
8. [Phase 5: `copresent_conversation` knowledge domain + tool](#8-phase-5-copresent_conversation-knowledge-domain--tool)
9. [Phase 6: Discussion mode](#9-phase-6-discussion-mode)
10. [Phase 7: Voice migration mechanics](#10-phase-7-voice-migration-mechanics)
11. [Phase 8: Misroute safety net](#11-phase-8-misroute-safety-net)
12. [Phase 9: Canvas inheritance + Share-to-group](#12-phase-9-canvas-inheritance--share-to-group)
13. [Phase 10: Daily space adjustment + cleanup](#13-phase-10-daily-space-adjustment--cleanup)
14. [Cross-cutting conventions and gotchas](#14-cross-cutting-conventions-and-gotchas)
15. [Quick-start: bringing up the chat stack for development](#15-quick-start-bringing-up-the-chat-stack-for-development)

---

## 0. Pre-flight: how this user works and how to collaborate

> The conventions in this section are the same ones documented across the active planning + handoff documents in the repo. Skim if you've seen them; read if this is your first hand-off.

### 0.1 The user

- **Name:** Jose Sanz. **Email:** `jsanz@znasllc.io`.
- **Company:** Znasllc.
- **Role:** product owner / lead engineer for the whole stack. Reviews everything before push.
- **Communication:** voice-to-text dictation often produces transcription artifacts; read for intent.
- **Preferences:** **no emojis** in any output. Professional, concise language.

### 0.2 The repos

```
~/projects/memql       Go backend / control plane
~/projects/copresent   React/Vite frontend
```

Both repos use `main` as their single long-lived branch. **No feature branches** unless explicitly asked. Stage files with `git add <path>` per file. Never `git add -A`.

The multi-repo migration brainstorm is committed (`docs/architecture/multi-repo-migration.md`) but has not been executed yet. Voice/audio architecture is similarly committed and pending (`docs/voice/voice-architecture-plan.md`). When the multi-repo migration ships, most of this chat-architecture code follows the cognition / copresent split — chat threads, mutations, automations, knowledge-domain content, discussion-mode dispatch all live in `copresent-bff` once that repo exists. The activity model touches identity (`User.activeSpaceId` is identity-scoped) and lives partly in core memql. Don't try to anticipate that split now — keep changes additive on the current layout.

### 0.3 The four-phase rule

Every non-trivial change follows: **familiarize → brainstorm → plan → execute**. This is the plan output. Phases 1–10 below are execute.

### 0.4 Triage every request

Name explicitly which repo(s) the change touches before coding. Most phases below touch both memql and copresent; some are repo-isolated. The phase headers call this out.

### 0.5 Pre-production deletion

Both repos are pre-production. When code is superseded, **delete it**. No `@deprecated`, no fallback shims. Stale docs are worse than missing docs.

The biggest deletion target in this initiative is the old GA-only-singleton restriction on the daily space concept (Phase 10) and any chat-rendering code that assumes single-thread-per-space (Phase 2).

### 0.6 Memory capture

Save user-stated rules to the persistent memory directory immediately. The decisions in this document are captured at `<memory>/project_chat_architecture.md`. The canvas-cards-not-banners working rule is captured at `<memory>/feedback_canvas_over_banners.md`. The tasks-as-the-unit-of-work direction is captured at `<memory>/project_tasks_model_direction.md`.

### 0.7 Execution mode

When the user says "execute the plan / get this going / no questions, no pausing":

- Run uninterrupted, parallel agents aggressively.
- At the end, **commit changes**. Use explicit `git add <path>` per file.
- **DO NOT push.** User validates locally before authorizing push.
- If you genuinely can't complete in one session, stop at a clean phase boundary, commit transparently, report.

### 0.8 Makefile is canonical

Every build/run/test command is a Makefile target in the relevant repo. Multi-step logic extracts to `scripts/<area>/<name>.sh` with the standard shell-script structure.

### 0.9 Canvas-not-chat for system events

This rule was locked during the brainstorm and applies to all of this initiative's work: any room-state, lifecycle, or operational event the user needs to be aware of is a `v1:copresent:canvasState` row, NOT an inline chat banner. Visibility (`public` | `private`), `forUserId`, `actor.kind` (`system` | `agent` | `user`), and `importance` (`notify` | `ambient`) are all already on the canvas-state concept. Use them. The chat panel is for utterances; the canvas is for state.

### 0.10 AAA security framing

When working with private threads, agent dispatch, and per-user data isolation, frame every change with **Authentication / Authorization / Audit**:

- **Authentication:** Who is the speaker? Identity-issued JWT verified per request.
- **Authorization:** Which thread can this caller read / write? The two-thread isolation in Q3 is the new authorization layer for chat. Each user can read their own private thread + the shared group thread; no cross-user-private access. Subscription rewriter enforces.
- **Audit:** Mutations that materially change state (private message moved between threads, private card shared to group, agent removed from someone's team) belong on `v1:identity:auditEvent`.

### 0.11 Memory pointers

| File | What it covers |
|---|---|
| `<memory>/MEMORY.md` | Index — read first. |
| `<memory>/project_chat_architecture.md` | Long-form rationale for THIS initiative. |
| `<memory>/project_repos.md` | Repo layout, main-branch rule, `git add` conventions. |
| `<memory>/project_tasks_model_direction.md` | Tasks-as-the-unit-of-work — affects how dispatches are structured. |
| `<memory>/project_voice_audio.md` | Voice/audio plan — overlaps with Phase 7 (voice migration). |
| `<memory>/project_bff_architecture.md` | Multi-repo migration brainstorm — not executed yet. |
| `<memory>/feedback_canvas_over_banners.md` | System events on canvas, not banners. |
| `<memory>/feedback_pre_prod_deletion.md` | Delete dead code. |
| `<memory>/feedback_documentation_hygiene.md` | Prune stale docs. |
| `<memory>/feedback_makefile_convention.md` | Makefile is canonical. |
| `<memory>/feedback_execute_endtoend.md` | Execution mode. |
| `<memory>/feedback_brainstorm_first.md` | Workflow. |

---

## 1. Goal and motivation

### 1.1 What the user said

> "It could be very chaotic. Imagine I create a space, and there's five AI agents and five humans all in the same space. Currently the chat shows everybody. There might be times where you just wanna talk to your general assistant."

The chat surface today is single-thread per space. Every utterance is visible to every participant. There's no way to side-bar with your own agents without contaminating the group room, and agents have no clean model for "this is private to my user vs this is for the room."

### 1.2 What this initiative ships

- **Two threads per space.** Group (everyone-public) and Team (per-user private). Tab UX in the chat panel.
- **Per-user agent teams.** Each human in the space brings their own agents into their own team chat. The group chat carries only humans + the owner's General Assistant.
- **Activity model.** Each human is active in at most one space at a time (hard cap, identity-scoped). Camera, mic, voice transport all bound to the active space. Agents are unbounded — they span spaces, work is task-driven.
- **Discussion mode.** Each user's agents are ambiently aware of the group thread and chime in proactively in the user's private team chat. Per-user activity-level setting governs how chatty.
- **Misroute safety net.** Server-side LLM classifier on every send catches messages composed in the wrong tab; confidence-tiered UX (hard pre-send block on high-confidence misroutes, soft post-send move prompt on medium).
- **`copresent_conversation` knowledge domain + `copresentConversation` tool** so agents in private can read and quote the group thread.
- **Canvas-card visibility scoping inherits the thread context.** "Share to group" action lets the user explicitly promote a private result to the room.
- **Daily space update.** Drops the GA-only singleton restriction in form; auto-provisioning still seeds only the GA, but the user can add agents and invite humans.

The cumulative result: the user gets a private team room inside every space, can side-bar, has agents working ambient context, and never bleeds private content into the group by accident.

---

## 2. Locked design decisions

These are settled in the brainstorm. Don't relitigate without explicit user approval.

### 2.1 Q1 — Per-human-team model

Every human in a space has their own team chat with their own agents. Cross-team isolation is hard. **Group chat composition: humans + only the owner's General Assistant** as the AI. No specialists in group; no other agents. Internal users (have a copresent account, have agents) bring their team in. External guests (token-invited, no account) are humans-only-in-group with no team chat.

### 2.2 Activity model

- Each human is **active in at most one space at a time.** `User.activeSpaceId` is the pointer.
- Camera, mic, voice transport, video presence — all bound to the active space.
- Agents are unbounded across spaces. Their compute is task-driven.
- Per-user-per-space agent cap = 3.
- Owner's GA is pinned in the space (cannot be removed; can be muted via per-agent audio control).
- Voice migration trigger fires on **second active human**, not on second-member.
- Discussion mode runs only while the user is active in the space; pauses on inactive.

### 2.3 Q2 — Discussion-mode trigger model + activity level

Hybrid trigger: cheap event heuristic (agent-name mentions, direct-question shape, distress signals) + windowed-batch periodic LLM analysis. Activity-level setting on `User.preferences.discussionModeActivityLevel`:

| Level | Window | Heuristic strictness | Per-window LLM cost cap |
|---|---|---|---|
| off | — | — | 0 |
| low | 60s | mentions-only | 1 LLM call/minute max |
| medium | 30s | mentions + direct questions | 2 LLM calls/minute max |
| high | 15s | mentions + questions + distress | 4 LLM calls/minute max |

Defaults: General Assistant = medium, all other agents = low.

Discussion mode runs **per private thread** — each active human has their own loop reading from the group thread.

### 2.4 Q3 — Two-thread data model

Two physically separate concepts: `v1:cognition:utterance` (group, unchanged) and the new `v1:cognition:privateUtterance` (private, with `forUserId`). Hard isolation enforced at the subscription rewriter. Team tab is **private-only** — no inline group merge. Cross-thread access is read-only ambient subscription for agents + the `copresentConversation` tool.

Tab labels: **Group** + **Team**.

### 2.5 Q4 — Voice migration mechanics

Clean-boundary trigger: when a second active human appears in the space, voice migrates from team to group at the next end-of-utterance / hold-to-talk-release / typing event. Reverse migration on last-human-leaves uses the same pattern. In-flight TTS completes; in-flight user utterance finishes in the original thread. Per-agent audio control persists across migration.

**Notifications are canvas cards, not banners** — public card on the group canvas timeline ("Carlos joined — voice is active in this chat"); private operational card on the owner's private canvas ("Voice moved to the group chat — push-to-talk or type to message your team here"). Push-to-talk in private becomes available when group is voice-active.

### 2.6 Q5 — Misroute safety net

Server-side structured-output classifier on every send (skipped for short / trivial messages). Output: `{intendedThread: 'group' | 'private', confidence: 0..1, why: string}`.

- **≥ 0.85** — hard pre-send confirm modal (`[Send to {other}]` / `[Send here anyway]`).
- **0.6 – 0.85** — send goes through; inline post-send "Did this belong in {other}? [Move]" prompt for ~10s.
- **< 0.6** — silent.

Move action = atomic `mutationDeleteUtterance` + `mutationSendOtherThreadUtterance`, audit-logged. Move events log to `v1:cognition:misrouteFeedback` for prompt-tuning. Defaults ON; opt-out via Settings.

### 2.7 Q6 — Discussion mode produces

Chat-primary output: every dispatch posts a chat message in the private thread. Task spawns are inline — when the agent's `respondToUser` envelope includes a task tool call, the chat utterance announces it ("Starting research on X — I'll let you know what I find") and renders a task chip linking to the running task.

Inter-agent dialogue uses **hard cap (max 3 turns per dispatch) + decaying threshold (each turn applies +0.1 to firing threshold) + user-input pause** (if the user types during the loop, conductor immediately receives the user input). Each turn creates a `v1:planner:task` per the tasks-as-the-unit direction.

Discussion mode runs ONLY in private; never injects into group.

### 2.8 Q7 — `copresent_conversation` knowledge domain + `copresentConversation` tool

Distinct from `copresent_ui`. Auto-ensured on every space-participant agent. Domain content: 4-6 short documents covering the two threads, visibility model, voice migration rule, discussion mode behavior, misroute safety, canvas-not-chat for system events, and tool usage.

Tool is a discriminated-union with operations:

```
copresentConversation({
  operation:
    | 'readGroupRecent'
    | 'readGroupByKeyword'
    | 'readGroupByTime'
    | 'getSpaceContext'
    | 'listParticipants',
  count?: number, keyword?: string, fromTime?: string, toTime?: string,
}) → typed result with utterance items: { speakerId, speakerName, speakerKind, timestamp, content, utteranceId }
```

READ-ONLY group-thread access; cannot read other users' private threads. Tool results render in agent replies as citation chips (`citations[].kind = 'group_thread_utterance'`).

Two reading paths: implicit (conductor digest, free) + explicit (tool call).

### 2.9 Q8 — Canvas card visibility inheritance + "Share to group"

**Inheritance rule:** card visibility = visibility of the spawning thread context. Private-thread dispatches → `visibility=private`, `forUserId=owner`. Group-thread dispatches → `visibility=public`. Discussion-mode → private (always). System lifecycle events follow Q4. Existing welcome cards keep current owner-only behavior.

**Share-to-group action.** New mutation `mutationShareCanvasStateToGroup(canvasStateId)` clones a private card to public; original stays private; audit-logged. UI: private cards carry a "Share to group" affordance in their action footer.

Subscription-rewriter scopes per-viewer: invitees see only public canvas cards; owner sees both their own private + all public.

### 2.10 Q9 — Daily space

Drops the GA-only-1-on-1 hard restriction. Auto-provisioning seeds **only the owner's General Assistant** — no other agents auto-join. User can later add agents from their roster (via the Roster tab from Phase 3) or invite humans (via the existing invite flow, owner-only).

Architecture default: polyphon (LiveKit room available so voice works the moment a human is invited).

Single team-welcome canvas card on creation introducing just the GA: "Your daily space is ready. Sofia is here to help."

Existing daily spaces are **not migrated** — they roll over naturally per `User.preferences.dailySpaceRolloverAction`. Old hardcoded GA-only restrictions get deleted.

### 2.11 Q10 — Owner-leaves + existing-space migration

Per-user team chat freezes when that user is **inactive** (refined from "disconnected" — keys on activity-model state). Group chat keeps running as long as anyone is in it; owner's GA continues to operate in group when owner is offline.

Existing spaces: **no content migration**. Today's `v1:cognition:utterance` rows are conceptually group-thread content already. Team tab starts empty for existing spaces; new private content accumulates from first use.

### 2.12 Roster panel + invite-button split

- **Roster tab in PresencePanel:** shows all members (active + inactive), each with active-state indicator. Owner sees humans + invite-human affordance + their own agents + add/remove. Internal-user invitee sees humans (read-only) + their own agents + add/remove. External guest sees humans only.
- **Invite button:** humans-only, owner-only. Calls existing internal-user-invite or guest-invite flows.
- Agent management is on the Roster tab, not on the invite button.

---

## 3. Current state — file-path-anchored survey

This snapshot is from the 2026-05-09 brainstorm context. Re-verify before editing anything; the work moves fast.

### 3.1 memql repo (`~/projects/memql`)

**Concepts:**

- `concepts/v1/cognition/utterance/` — current single-thread chat row. Stays as the GROUP concept; no schema change in Phase 1. Phase 1 adds the parallel `concepts/v1/cognition/privateUtterance/` concept.
- `concepts/v1/cognition/space/` — existing space concept (status, kind, architecture). No change for chat architecture.
- `concepts/v1/cognition/participant/` — current participant model. Likely needs a `forUserId` field on agent participants (which user's agent is this?) and the active/inactive state surfacing — verify.
- `concepts/v1/copresent/canvasState/` — already carries visibility, forUserId, actor, importance. No schema change; just consistent use across new dispatches.
- `concepts/v1/identity/user/` — active-space pointer (`User.activeSpaceId`) added in Phase 4.

**Cognition / conductor:**

- `integrations/cognition/conductor_consult.go` — per-turn conductor LLM. Phase 6 (discussion mode) reuses this with new input shape (group-thread digest + private-thread context).
- `integrations/cognition/si_router.go` — fast-path voice router. Lightly affected; will need to be aware of the new private-thread context.
- `integrations/cognition/cognition_handler.go` — routing entry point. Phase 1 / Phase 2 split routing per thread.
- `integrations/agent/replier.go` — agent prompt assembly. Auto-ensures the `copresent_conversation` knowledge domain in Phase 5.
- `integrations/agent/envelope.go` — `respondToUser` schema. `citations[]` gets the new `'group_thread_utterance'` kind in Phase 5.

**Tools / DSL:**

- `tools/v1/copresent/` — existing tool definitions. Phase 5 adds `tools/v1/copresent/conversation/copresentConversation.memql` (or similar path).
- `prompts/v1/cognition/conductorTurn.tmpl` — conductor LLM prompt. Phase 6 extends to handle discussion-mode-trigger inputs.
- `prompts/v1/cognition/cognitionPlanTriage.{memql,tmpl}` — already does structured-output classification on incoming messages. Phase 8 (misroute) creates a parallel `cognitionMisrouteCheck` prompt.

**Mutations / queries (need to author):**

- `mutations/v1/cognition/sendPrivateUtterance.memql` (Phase 1).
- `mutations/v1/cognition/movePrivateToGroup.memql` (Phase 8 misroute Move action).
- `mutations/v1/cognition/moveGroupToPrivate.memql` (Phase 8 misroute Move action).
- `mutations/v1/copresent/shareCanvasStateToGroup.memql` (Phase 9).
- `mutations/v1/cognition/setUserActiveSpace.memql` (Phase 4 activity model).
- `mutations/v1/cognition/addAgentToSpace.memql` / `removeAgentFromSpace.memql` (Phase 3 roster).
- `queries/v1/cognition/privateUtterancesForUser.memql` (Phase 1).

**Subscription rewriter:**

- `component/grpc/server.go` (or wherever the existing partition-ACL rewriter lives) — Phase 1 extends it with the per-user private-thread filter. Same pattern as the partition rewriter.

### 3.2 copresent repo (`~/projects/copresent`)

**Chat surface:**

- `src/components/chat/ChatPanel.tsx` — current single-thread chat panel. Phase 2 adds the Group/Team tab switcher and per-tab subscription routing.
- `src/components/chat/ChatMessages.tsx` — message list. Phase 2 makes it thread-aware.
- `src/components/chat/VoiceInputButton.tsx` — mic button. Phase 7 adjusts for the PTT-in-private-when-group-voice-active path.
- `src/components/chat/TranscriptionStatusBadge.tsx` — transcription pill. No direct change.

**Presence panel:**

- `src/components/PresencePanel.tsx` — current presence panel. Phase 3 adds the Roster tab.
- `src/components/presence/` — orb components. Phase 3 surfaces per-user agent ownership and active/inactive states.

**Hooks / context:**

- `src/context/SpaceSubscriptionContext.tsx` — central subscription manager. Phase 1 / Phase 2 split per-thread subscriptions.
- `src/context/WorkspaceContext.tsx` — partition resolver. Phase 4 adds an `ActiveSpaceContext` or similar for the activity-model pointer.
- `src/hooks/useDailySpace.ts` — daily space provisioning. Phase 10 updates the seeding flow.

**Modals:**

- `src/components/InviteParticipantModal.tsx` — current human-invite modal. Phase 3 keeps as humans-only.
- `src/components/agents/CreateAgentModal.tsx` — create-an-agent modal. Lightly touched for ownership-per-space concept.
- `src/components/InviteGuestModal.tsx` — guest-invite. Phase 3 keeps as humans-only-guest.

**Misroute UI surfaces (new):**

- `src/components/chat/MisrouteConfirmModal.tsx` (Phase 8 high-confidence pre-send modal).
- `src/components/chat/MisroutePostSendPrompt.tsx` (Phase 8 medium-confidence inline prompt).

### 3.3 What's NOT being changed

- The voice/audio architecture from `docs/voice/voice-architecture-plan.md` mostly stands. Phase 7 of THIS plan re-uses the per-agent audio control mechanic and adds the cleanup-boundary migration on top. Confirm the voice plan is shipped or at least Phase 1 (foundation) before starting this plan's Phase 7.
- The identity service is shipped; this initiative reuses its JWT verifier, partition ACLs, and audit log infrastructure unchanged.
- The worker initiative is shipped; not affected by chat architecture changes.

---

## 4. Phase 1: Foundational data model + per-user team rosters

**Repos touched:** memql.

**Goal:** put the new concepts, mutations, and per-user agent ownership in place so subsequent phases can build on them.

### 4.1 New concept: `v1:cognition:privateUtterance`

```memql
@description("A private chat utterance scoped to one human user in a space.")
concept Privateutterance {
  space:        v1:cognition:space.id
  forUserId:    v1:identity:user.id
  speakerId:    string  // user.id or agent.id
  speakerKind:  "user" | "agent"
  content:      string
  citations?:   array[Citation]
  // ... whatever fields v1:cognition:utterance has, mirrored
}
```

Same field shape as `v1:cognition:utterance` PLUS the `forUserId` field. Hard rule: every `Privateutterance` row has a non-null `forUserId`. The pre-insert validation guard rejects null.

### 4.2 New mutation: `mutationSendPrivateUtterance`

```memql
mutation sendPrivateUtterance({space, content, ...}) {
  insert(v1:cognition:privateUtterance, {
    space,
    forUserId: ctx.actor.userId,  // server-stamped, never client-supplied
    ...
  })
}
```

The `forUserId` is **server-stamped from the caller's identity**, not from a request field. This is a critical bleed-defense: a malicious or buggy client cannot write a private utterance with someone else's `forUserId`.

### 4.3 Subscription rewriter

Extend the existing partition-ACL rewriter (in `component/grpc/server.go` or wherever `scopeGraphPatternToPartition` lives) with a per-user private-thread filter:

```go
// Pseudo-code
func scopePrivateUtterances(ctx, pattern) pattern {
  if pattern.concept != "v1:cognition:privateUtterance" {
    return pattern
  }
  // Force the forUserId predicate to the caller's user id.
  return pattern.with(filter("forUserId", ctx.actor.userId))
}
```

Same enforcement model as the partition rewriter: server-side, can't be bypassed by client-side query rewrites.

### 4.4 Per-user agent ownership in space-participant rows

Today the participant concept tracks `participantType: 'human' | 'agent'`. Add a `forUserId` field on agent participants — which human's team is this agent on?

- Owner's agents: `forUserId = ownerId`.
- Invitee's agents: `forUserId = inviteeId`.
- Owner's GA in group: special, marked with a flag (`isGroupGA: true`) and pinned (cannot be removed).

### 4.5 Mutations: agent roster management

- `mutationAddAgentToSpace(spaceId, agentId)` — adds caller's agent to the space. Server validates the agent belongs to caller. Per-user agent count cap = 3 enforced.
- `mutationRemoveAgentFromSpace(spaceId, agentId)` — removes caller's agent. Server validates the agent belongs to caller. Owner's GA cannot be removed (server rejects).
- `mutationPinOwnerGAInSpace(spaceId)` — internal, called when space is created. Server-only.

### 4.6 Phase 1 commit boundaries

- Commit 1: `v1:cognition:privateUtterance` concept + pre-insert validation.
- Commit 2: `mutationSendPrivateUtterance` + `queries/v1/cognition/privateUtterancesForUser.memql`.
- Commit 3: Subscription rewriter extension for private utterances.
- Commit 4: Participant `forUserId` field + agent-roster mutations.
- Commit 5: Owner's GA pinning logic in space-creation flow.

---

## 5. Phase 2: Two-tab chat UI (Group / Team)

**Repos touched:** copresent.

**Goal:** ship the Group / Team tab switcher in `ChatPanel` with per-thread subscriptions and the send-target rule.

### 5.1 Tab switcher

Add a tab strip at the top of `ChatPanel`:

- **Group** — active by default for new sessions. Renders `v1:cognition:utterance` rows for the space.
- **Team** — private. Renders `v1:cognition:privateUtterance` rows scoped to the current user. Per Q1, the Team tab is **private-only** (no inline group merge).

### 5.2 Send-target rule

- Active tab = Group → `mutationSendUtterance` (existing).
- Active tab = Team → `mutationSendPrivateUtterance` (new from Phase 1).

The current-tab state is plumbed through the chat panel's send handler. The misroute safety net (Phase 8) intercepts the send before commit.

### 5.3 Disabled-Group-when-alone rule

When the owner is alone in the space (no other humans), the Group tab is visible but the input is disabled with a hint: "no humans in this space yet — start a conversation in Team." User can still scroll group history.

### 5.4 Per-tab subscriptions

`SpaceSubscriptionContext` registers two parallel subscriptions per space:

- Group: `graph.node.created.{partition}.v1:cognition:utterance` filtered by space.
- Private: `graph.node.created.{partition}.v1:cognition:privateUtterance` filtered by space + forUserId (server-rewritten, client just subscribes).

Each tab reads from its respective stream. Switching tabs does NOT unsubscribe — both streams remain active so the unread-message indicator on the inactive tab works.

### 5.5 Phase 2 commit boundaries

- Commit 1: tab switcher component + state.
- Commit 2: send-target rule + Phase-1 mutation wiring.
- Commit 3: `SpaceSubscriptionContext` per-thread split.
- Commit 4: Disabled-Group-when-alone handling + unread indicator on inactive tab.

---

## 6. Phase 3: Roster panel + invite-button refactor

**Repos touched:** copresent.

**Goal:** build the Roster tab on PresencePanel and split the invite affordances cleanly.

### 6.1 Roster tab

Add a Roster tab to PresencePanel. Three views, picked based on the viewer's role:

- **Owner view:** humans list with "+ Invite human" button (calls existing invite flows); their agents list with "+ Add agent" button (calls `mutationAddAgentToSpace`). Per-row remove on agents the owner owns. Owner's GA pinned with a non-removable indicator. Active/inactive state indicators on humans.
- **Internal-user invitee view:** humans list (read-only); their agents list with "+ Add agent" + remove. Owner's GA shown but not manageable. Active/inactive state on humans.
- **External guest view:** humans list only. No agent management.

### 6.2 Invite button — humans only

The existing invite button (top of PresencePanel today) flips to humans-only. Its modal becomes "Invite a human" — picks between internal user (search by email) and guest (email link). Owner-only.

### 6.3 Add-agent flow

"+ Add agent" → modal listing the user's agents not currently in the space + per-row "Add" button → on click, calls `mutationAddAgentToSpace`. Optimistic UI; rollback on cap-exceeded error.

### 6.4 Phase 3 commit boundaries

- Commit 1: Roster tab component + role-based views.
- Commit 2: Invite button refactor (humans-only).
- Commit 3: Add-agent modal + flow.
- Commit 4: Owner's GA pinned indicator + per-agent remove.

---

## 7. Phase 4: Activity model — one active space per human

**Repos touched:** memql, copresent.

**Goal:** enforce the 1-active-space-per-human cap, derive active/inactive participant state from the pointer, release voice/camera resources on deactivation.

### 7.1 memql changes

1. **`User.activeSpaceId` pointer.** Add to `concepts/v1/identity/user/concept.memql` (or wherever User's mutable state lives). Nullable string. When non-null, points at the `v1:cognition:space.id` the user is currently active in.

2. **`mutationSetUserActiveSpace(spaceId | null)`** — the only path to set the pointer. Server-stamps the caller's user id. Atomic — switching from X to Y in one mutation call. Side effects:
   - Emit `v1:cognition:participant:presence` deactivation event for the old space (if any).
   - Emit activation event for the new space.
   - Audit log: `user.active_space.changed`.

3. **Derived `isActive` on participant rows.** A participant is active iff `User.activeSpaceId == participant.spaceId`. Compute on-read via a query helper; subscribers receive the computed flag.

4. **LiveKit room state release.** On deactivation, the user's audio/video tracks are unpublished from the LiveKit room (handled by the Bridge Agent / voice path). Room stays open if other publishers remain.

### 7.2 copresent changes

1. **`ActiveSpaceContext` provider** — wraps the app, exposes `activeSpaceId` + `setActiveSpaceId(id)`.

2. **Set-active-on-enter.** When the user opens a space (route navigation to `/space/:id` or equivalent), `setActiveSpaceId(id)` calls `mutationSetUserActiveSpace(id)`. Switching to another space calls it with the new id. Closing the app calls it with `null`.

3. **Inactive UI.** When a user is a member of a space they're not active in, the space row in the Spaces panel shows a dimmed indicator. Clicking it calls `setActiveSpaceId` to switch.

4. **Voice/camera release on deactivation.** `usePolyphonRoom` hooks into the active-space change and disconnects from LiveKit when the space is no longer active. Fast reconnect on re-activation.

5. **Visual presence indicators in PresencePanel + Roster tab.** Active vs inactive humans visually distinct (active = colored dot, inactive = dimmed grey).

### 7.3 Phase 4 commit boundaries

- Commit 1: `User.activeSpaceId` field + mutation.
- Commit 2: Derived `isActive` computation + presence event emission.
- Commit 3: copresent `ActiveSpaceContext` + set-active-on-enter wiring.
- Commit 4: copresent voice/camera release on deactivation.
- Commit 5: copresent active/inactive visual indicators (PresencePanel, Roster tab, Spaces panel).

---

## 8. Phase 5: `copresent_conversation` knowledge domain + tool

**Repos touched:** memql.

**Goal:** ship the knowledge domain and the tool so agents can reason about and read across the new chat surfaces.

### 8.1 Knowledge domain — content authoring

Author 4-6 short markdown documents under `concepts/v1/copresent/knowledge/copresent_conversation/`:

1. `00-thread-model.md` — the two threads, who sees what, owner-vs-invitee, internal-user-vs-guest.
2. `01-visibility.md` — the visibility model, hard isolation, what agents can and cannot read.
3. `02-voice-migration.md` — the migration rule, when voice belongs in group, PTT in private.
4. `03-discussion-mode.md` — discussion mode behavior, when it fires, the cap and decaying threshold.
5. `04-misroute-safety.md` — agents respond into one thread at a time; never generate cross-thread content from a private dispatch.
6. `05-tool-usage.md` — when to use `copresentConversation`; the two reading paths (implicit digest + explicit tool); citation chip rendering.

Each document is ingested into `v1:knowledge:document` rows + chunked + embedded via the existing knowledge pipeline.

### 8.2 Auto-ensure on space-participant agents

Extend `integrations/agent/replier.go` (or wherever the retrieval-domain-set is assembled at agent-prompt-assembly time) to append `copresent_conversation` to the agent's domain set whenever the agent is a participant in any space.

Same pattern as the existing `copresent_ui` auto-ensure on `copresent_control`-capable agents.

### 8.3 Tool definition

```memql
@description("Read group-thread content and space context. Read-only.")
@executor("integration.copresent.copresentConversation")
func (Tool) copresentConversation(args any) {
  // operations: readGroupRecent, readGroupByKeyword, readGroupByTime,
  //             getSpaceContext, listParticipants
}
```

Go-side executor in `integrations/copresent/conversation_tool.go`:

- Validates the agent caller is a space participant.
- Dispatches per `operation` to the right query.
- Returns typed result; cannot read `v1:cognition:privateUtterance` ever.
- Audit-log every call: `copresent.conversation.read` with `{operation, count_returned}`.

### 8.4 Citation chip rendering

`integrations/agent/envelope.go` — extend `Citation` to support `kind: 'group_thread_utterance'` with `{utteranceId, speakerName, matchedPhrase}`.

copresent's citation renderer (`src/components/chat/`) — handle the new citation kind, render with attribution chip ("from group: Carlos said …"), click scrolls to the source utterance in the Group tab.

### 8.5 Phase 5 commit boundaries

- Commit 1: domain content authoring + `v1:knowledge:document` seeding.
- Commit 2: agent auto-ensure for `copresent_conversation`.
- Commit 3: `copresentConversation` tool definition + Go executor.
- Commit 4: citation kind + frontend rendering.

---

## 9. Phase 6: Discussion mode

**Repos touched:** memql, copresent.

**Goal:** ship the per-private-thread dispatch loop with the hybrid trigger model and activity-level controls.

### 9.1 memql — dispatch loop

1. **Per-private-thread loop.** For each (space, user) where the user is active in the space and `discussionModeActivityLevel != 'off'`, run a background loop that:
   - Subscribes to group-thread events (cheap heuristic check on each).
   - On a heuristic-trigger fire (mention, direct question, distress), dispatch immediately (subject to per-window cost cap).
   - On a windowed-batch tick (per activity-level cadence), call the conductor with a digest of recent group activity + current private context.
   - The conductor returns a plan; the agent's tool loop runs; chat utterance + optional task spawn lands in `v1:cognition:privateUtterance`.

2. **Inter-agent dialogue with cap + decaying threshold + user-input pause.**
   - Track `dispatchTurnCount` per loop iteration.
   - After each turn, threshold += 0.1.
   - If user types in their team chat, abort the inter-agent loop, conductor receives user input as new trigger.
   - Hard cap: turn count ≥ 3 ends the loop regardless.

3. **Task spawn announcement.** When the agent's `respondToUser` envelope includes a task tool call, the chat utterance text already announces it (per envelope shape). The task chip rendering on the frontend is added in Phase 6 too.

4. **`User.preferences.discussionModeActivityLevel`** field on the User concept. Default: `medium` for the General Assistant's setting (when GA is the agent), `low` for everyone else. Configurable in Settings.

### 9.2 copresent — Settings + chat-side rendering

1. **Settings UI:** add a "Discussion mode" section with the activity-level selector (off / low / medium / high) and a brief explanation of each.
2. **Task chip on chat utterance:** when a discussion-mode chat utterance carries a task spawn, render a chip with the task name + status. Click → Tasks panel.
3. **Inter-agent indicator:** when the loop is running multi-turn, show a subtle "the team is discussing" footer in Team tab. Disappears when loop ends.

### 9.3 Phase 6 commit boundaries

- Commit 1: memql discussion-mode dispatch loop.
- Commit 2: memql cap + decaying threshold + user-input pause.
- Commit 3: memql `User.preferences.discussionModeActivityLevel` field + integration.
- Commit 4: copresent Settings UI.
- Commit 5: copresent task-chip on chat utterance + inter-agent indicator.

---

## 10. Phase 7: Voice migration mechanics

**Repos touched:** memql, copresent.

**Goal:** ship the clean-boundary migration trigger, the canvas-card notifications (per the canvas-not-banners rule), and PTT-in-private-when-group-voice-active.

This phase has substantial overlap with the voice/audio plan's Phase 2 (per-agent audio control). Confirm the voice plan's Phase 2 is shipped before starting this phase, or run them in parallel with care.

### 10.1 Migration trigger

- **Server-side detection.** When a participant transitions from inactive → active in a space, count active humans. If count == 2 (the threshold), emit a `v1:cognition:voice:migration:requested` event for the space.
- **Client-side handling.** Each participant's client receives the event. At the next end-of-utterance / hold-to-talk-release / typing event, voice transport rewrites: user mic now publishes to group's audio context; team chat falls back to PTT/typing.

### 10.2 Canvas-card notifications

On the migration-requested event, server emits two canvas cards via `mutationCreateCanvasState`:

- **Public card** (`visibility=public`, `actor.kind=system`, `importance=notify`): "Carlos joined — voice is active in this chat." Lands on every participant's canvas.
- **Private operational card per existing-user** (`visibility=private`, `forUserId=eachExistingUser`, `actor.kind=system`, `importance=ambient`): "Voice moved to the group chat. Push-to-talk or type to message your team here."

Reverse migration (last human leaves, count drops below 2) emits the symmetric pair.

### 10.3 PTT in private when group is voice-active

`ChatPanel` Team tab:

- When group is voice-active, show a small dimmer mic button next to the text input. Hold-to-talk pattern (existing in creation modals, reused).
- Tap+release captures a short utterance, transcribes locally, lands as `mutationSendPrivateUtterance`.
- When user is alone in space (group not voice-active), the Team tab gets the full continuous-voice mode per voice plan Q5.

### 10.4 Phase 7 commit boundaries

- Commit 1: memql migration-trigger event emission.
- Commit 2: memql canvas-card pair on migration.
- Commit 3: copresent client-side migration handling + voice transport rewrite at clean boundary.
- Commit 4: copresent PTT-in-private mic button + flow.

---

## 11. Phase 8: Misroute safety net

**Repos touched:** memql, copresent.

**Goal:** ship the structured-output classifier and the confidence-tiered UX (hard pre-send modal + soft post-send move prompt).

### 11.1 Classifier

1. **New prompt** at `prompts/v1/cognition/cognitionMisrouteCheck.{memql,tmpl}`. Inputs: `currentTab, message, recentGroup, recentPrivate, spaceContext, participants`. Output: `{intendedThread, confidence, why}`.

2. **Mutation interception.** Both `mutationSendUtterance` and `mutationSendPrivateUtterance` (when called from a context with `misrouteCheck=true`) invoke the classifier first:
   - Skip for messages < 10 chars or trivial content.
   - Otherwise call the classifier (cheap-mini provider).
   - If `intendedThread != currentTab`:
     - Confidence ≥ 0.85 → return error code `MISROUTE_BLOCKED` with payload; do NOT insert the utterance.
     - 0.6 – 0.85 → insert normally; return success with `misrouteWarning: {confidence, why, suggestedTab}`.
   - If matches or confidence < 0.6 → silent success.

3. **Settings override.** `User.preferences.misrouteSafetyEnabled` (default `true`). When `false`, classifier is bypassed entirely.

### 11.2 UX in copresent

1. **Hard pre-send modal** (`MisrouteConfirmModal.tsx`): on `MISROUTE_BLOCKED` response, show a modal with the classifier's `why`, `[Send to {other}]` (primary), `[Send here anyway]` (secondary). Picking primary sends to the other thread; picking secondary re-sends with `misrouteCheck=false` (one-shot bypass).

2. **Post-send soft prompt** (`MisroutePostSendPrompt.tsx`): when `misrouteWarning` returns, render an inline prompt under the just-sent message: "Did this belong in your {other} chat? [Move]". Auto-dismiss after 10 seconds. [Move] → calls `mutationMoveUtteranceToOtherThread`.

3. **Settings UI.** Toggle for the safety net in Settings.

### 11.3 Move action mutations

`mutations/v1/cognition/movePrivateToGroup.memql` and `moveGroupToPrivate.memql`:
- Atomically delete the source utterance + insert a new utterance in the target thread with the same content + timestamp.
- Audit-log: `chat.utterance.moved` with source/target ids.
- Insert `v1:cognition:misrouteFeedback` row carrying the original classifier output and the user's action ("user moved" or "user dismissed").

### 11.4 Phase 8 commit boundaries

- Commit 1: misroute classifier prompt + structured-output integration.
- Commit 2: mutation interception path + return codes.
- Commit 3: copresent hard pre-send modal.
- Commit 4: copresent post-send soft prompt + Move action.
- Commit 5: misrouteSafetyEnabled preference + Settings UI + opt-out plumbing.

---

## 12. Phase 9: Canvas inheritance + Share-to-group

**Repos touched:** memql, copresent.

**Goal:** apply the visibility-inheritance rule consistently across all canvas-card-emitting paths and add the explicit Share-to-group action.

### 12.1 Inheritance rule audit

Walk every existing path that emits `v1:copresent:canvasState` rows and confirm the visibility is being set per the inheritance rule:

- Tool-path (`canvas.publish` from agent tool calls): public if dispatch was group-thread; private (with `forUserId=dispatchingUserId`) if private-thread. Update the tool's executor.
- Frontend direct mutation paths (welcome cards on agent.created, group.created, space.created): keep current owner-only behavior.
- Plan / task lifecycle automations: visibility inherits from the plan's spawning thread context. The plan needs to track `spawnedFromThread: 'group' | 'private'` + `spawnedFromUserId` for private cases.

### 12.2 Share-to-group mutation

`mutations/v1/copresent/shareCanvasStateToGroup.memql`:
- Reads the source private canvas-state row.
- Asserts caller is the `forUserId` of the source (only the card's owner can share).
- Inserts a new canvas-state row with `visibility=public`, `actor.kind=user`, content cloned, `sharedFromCanvasStateId` linking back to the source.
- Audit-log: `canvas.private.shared_to_group`.
- Returns the new public card id.

### 12.3 UI affordance

On every private canvas card: a small "Share to group" button in the action footer. On click:
- Inline confirm: "Share this with the group?"
- Confirm → call `mutationShareCanvasStateToGroup`.
- Brief animation as the new public card lands on the group canvas timeline.

### 12.4 Phase 9 commit boundaries

- Commit 1: tool-path canvas.publish visibility inheritance.
- Commit 2: plan/task lifecycle visibility inheritance.
- Commit 3: `mutationShareCanvasStateToGroup` + audit.
- Commit 4: copresent "Share to group" action UI.

---

## 13. Phase 10: Daily space adjustment + cleanup

**Repos touched:** memql, copresent.

**Goal:** drop the hardcoded GA-only-1-on-1 restriction in form, keep GA-only auto-join in practice, refresh the welcome card, and delete legacy code.

### 13.1 memql

1. **Update daily-space provisioning.** `mutationCreateDailySpace` (or wherever it lives) seeds owner + their GA only — no other agents. Architecture default: polyphon.
2. **Welcome canvas card.** Single team-welcome card on creation, introducing just the GA: "Your daily space is ready. {GA name} is here to help."
3. **Delete legacy code.** Anything that hard-locked daily spaces to "single human + single agent" (architecture asserts, automation gates, query filters that assume 1-on-1) gets deleted. No `@deprecated`. Per the pre-prod-deletion rule.

### 13.2 copresent

1. **`useDailySpace` provisioning** logic flips to the new flow: create + add owner's GA + emit welcome card.
2. **Daily space row UI:** unchanged in shape (still pinned at top, still rolls over). The new model just means it can have more participants if invited.
3. **Old GA-only welcome rendering** code gets deleted.

### 13.3 Documentation refresh

- Update memql `CLAUDE.md` — the "Spaces (three-state lifecycle + daily spaces)" section drops the "1-on-1 with GA" framing.
- Update copresent `CLAUDE.md` — Voice-First Create Modals section (CreateSpaceModal mentions the architecture choice; verify daily-space seeding language is consistent).
- Delete any docs paragraphs that refer to the old daily-space-as-GA-only model.

### 13.4 Phase 10 commit boundaries

- Commit 1: memql daily-space provisioning update.
- Commit 2: memql welcome canvas card.
- Commit 3: memql delete legacy GA-only-hardcoded restrictions.
- Commit 4: copresent `useDailySpace` flow update + delete legacy welcome rendering.
- Commit 5: documentation refresh in both repos.

---

## 14. Cross-cutting conventions and gotchas

### 14.1 Server-stamping over client-trust

Every private-utterance write server-stamps `forUserId` from the caller's identity. Never accept a client-supplied `forUserId`. This is the same pattern as identity's partition-stamping.

### 14.2 Subscription rewriter is the load-bearing wall

The bleed-defense from Q3 rests on the subscription rewriter correctly filtering `v1:cognition:privateUtterance` patterns by `forUserId`. Test this exhaustively — including patterns with wildcards, multi-concept patterns, and cross-partition patterns. A rewriter bug = silent leak.

### 14.3 Audit every privacy-affecting mutation

`v1:identity:auditEvent` rows for: `chat.utterance.moved`, `canvas.private.shared_to_group`, `agent.removed_from_space`, `user.active_space.changed`. NOT for: every utterance insert (too noisy), per-tool-call telemetry (lives on per-call telemetry concepts), discussion-mode triggers (operational).

### 14.4 Discussion mode + active-state coupling

Discussion mode runs ONLY while the user is active in the space. The dispatch loop checks `User.activeSpaceId == thisSpaceId` on every iteration. If the user becomes inactive mid-loop, the in-flight turn completes, but no new turn fires.

### 14.5 The owner's GA is special

It's the only AI in group chat. It's pinned (cannot be removed). When owner goes offline, the GA continues to operate in group. The GA is also a participant in the owner's team chat. This means it's effectively in two threads simultaneously — same agent, two contexts. Per-agent audio control settings apply per-thread context (the voice plan's Q2 mechanic supports this).

### 14.6 Canvas-not-chat for system events

Any system / lifecycle / state-change notification is a canvas card, not a chat banner. Voice migration (Phase 7), human joined/left, mic warning (from voice plan), reconnect status — all canvas. The only exceptions are inline orb-state indicators and the per-input transcription pill, which are not banners.

### 14.7 Tasks-as-the-unit-of-work

Discussion-mode dispatches create `v1:planner:task` rows. So do tool calls (eventually, per the tasks-as-the-unit direction). Don't add new agent-action paths that bypass the task model — design every new dispatch to be a task.

### 14.8 The misroute classifier is best-effort, not authoritative

Confidence < 0.6 = silent (don't pester). Confidence ≥ 0.85 = hard block. Between = soft prompt. Don't escalate the model further unless instrumentation shows the user is getting genuinely surprised by leaks. The user's bleed concern is primarily addressed by the two-thread architecture (Q3); the classifier is the safety net for human errors, not the primary defense.

### 14.9 External guests are humans-only-in-group

Token-invited guests have `participantType=human, isGuest=true`. They have no agents, no team chat. The Roster tab for them shows humans only. The Team tab itself does not render for them — when they navigate the chat panel, only the Group tab is visible (or the Team tab is disabled with an explanation).

### 14.10 Per-user agent cap = 3 is per-user-per-space, not global

A user can have many agents across many spaces. The 3-agent cap is per-space-per-user. Cross-space resource consumption is governed by the per-user concurrent-task cap (per the tasks-as-the-unit billing direction), which is separate.

---

## 15. Quick-start: bringing up the chat stack for development

### 15.1 Initial setup

```bash
cd ~/projects/memql
make dev-cluster-restart   # full cluster: bff + cognition + agent + planner + identity + postgres
# or
docker compose -f docker/docker-compose.full.yml up --build
# Single binary mode is fine for most chat-architecture work; cluster matters for cross-node routing tests.

cd ~/projects/copresent
make dev   # frontend at :8080, Express at :3000
```

### 15.2 Test the two-thread end-to-end

After Phase 2 lands, the manual test loop is:

1. Sign in as user A. Create a polyphon-architecture space.
2. Verify Group tab is visible but disabled (alone with agents). Verify Team tab is active.
3. Send a message in Team. Verify it lands as `v1:cognition:privateUtterance` in the database with `forUserId=userA`.
4. Open the same space in a different browser as user B (an internal user). Invite user B to user A's space (Phase 3).
5. User B accepts. Verify Group tab becomes active for user A. Voice migrates to group at next utterance boundary (Phase 7).
6. User B sees the Group tab; Team tab is empty for them (their own private thread).
7. User B sends in Team. Verify forUserId=userB; user A cannot see this row in their subscription (Phase 1 rewriter).

### 15.3 Test discussion mode

After Phase 6 lands:

1. Set `User.preferences.discussionModeActivityLevel` to `medium` for user A.
2. With user A and user B both active, user B sends a couple of group messages.
3. Within 30 seconds, user A should see a discussion-mode chime-in from one of their agents in their Team tab (e.g., "I noticed Carlos mentioned the prod deployment — want me to check the recent logs?").
4. Verify user B does NOT see user A's private discussion-mode chime-ins.

### 15.4 Test the misroute safety net

After Phase 8 lands:

1. In Team tab, type a message that obviously belongs in group ("@everyone, deploy is done"). Send.
2. Hard pre-send modal should appear; classifier confidence ≥ 0.85.
3. Pick `[Send to Group chat]` → message lands in group.
4. In Team tab, type a more ambiguous message. Send.
5. If classifier returns 0.6–0.85, post-send soft prompt appears; click [Move] to test the move action.

### 15.5 Useful Make targets after this initiative

```
make chat-loop-test     # Run a semi-automated end-to-end of the two-thread loop (script under scripts/chat/loop-test.sh)
make chat-misroute-test # Run a regression of misroute classifications against a fixture set
```

Both should be added by whichever phase ships them.

### 15.6 What to read once you're set up

In order:

1. `<memory>/MEMORY.md` and the `project_*` / `feedback_*` files it references.
2. `<memory>/project_chat_architecture.md` (the brainstorm rationale for THIS doc).
3. memql `CLAUDE.md` (the "Cognition (Routing + Conductor)", "Agent reply envelope", "Spaces (three-state lifecycle + daily spaces)", "Canvas state (v1)" sections).
4. copresent `CLAUDE.md` (the "Chat reply rendering", "Modal + form primitives", "Voice-First Create Modals" sections).
5. This document, end to end (until Phase 10 deletes it).

Then proceed to whichever phase is in flight. Most phases below have clean commit boundaries listed; respect them — they're picked so a partially landed phase can still be reviewed and reasoned about independently.
