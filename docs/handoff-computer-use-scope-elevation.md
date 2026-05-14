# Handoff — Computer Use scope-elevation flow (per-task approval card)

This handoff covers ONE specific in-flight feature: making the agent's
"I need elevated computer-use scope to do this" path land an approval
card on the canvas, get user input, and resume. Three commits up the
recent history (`574a0a5` agent: scope-elevation flow, `cbf9611` agent
context auto-inject, `6b702bb` request-scope handler + prompt fixes)
got the plumbing 80% of the way. It still doesn't work end to end and
the user is frustrated. The remaining work is in this doc.

## Working rules (READ THIS FIRST)

These are non-negotiable and worth re-reading before every commit.

### 1. No deprecation, no backwards-compat

This is pre-release. When a contract changes, fix BOTH repos and the
DB-side concept in ONE commit and delete what's no longer needed. Do
NOT add legacy adapters, fallback code paths, or "keep working while
we migrate" layers. If old DB rows would break under the new shape,
expect the user to wipe + re-seed (`make dev-cluster-restart-purge`)
and tell them to do it.

### 2. Get things done end-to-end before handing back

The user is the tester. They expect to read a "shipped X, run
`make dev-refresh`, test by doing Y" handoff message, run that, and
either confirm or paste a log. They do NOT want a "I've started, more
to do, here are options" response. Plan, ship the whole vertical
slice, commit, push, summarize, hand back. If a phase needs the user
to test before continuing, say so explicitly — don't fork the work
mid-flight unless the user asks.

### 3. Two repos, both on `main` only

```
/Users/znas/projects/memql      — Go backend / control plane
/Users/znas/projects/copresent  — TypeScript/React frontend
```

Commit directly to `main` on both. No PRs, no feature branches. Use
explicit paths for `git add` (NEVER `-A` or `.`), since the user runs
multiple Claude sessions concurrently in the same tree. Push with
`git push origin main`. The remote shows a "Changes must be made
through a pull request" warning that you can ignore — the push
succeeds (GitHub admin bypass).

### 4. Commit messages via heredoc / file, not inline

Heredoc breaks on colons + quotes. Write the body to `/tmp/commit-msg.txt`
then `git commit -F /tmp/commit-msg.txt`. Co-author trailer:

```
Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```

### 5. Other rules from MEMORY.md to internalize

- Backend: `SI`. Frontend + user-facing copy: `AI`.
- The user runs the CoPresent dev server (`npm run dev`) themselves;
  do NOT preview-start it.
- The dev cluster restart with DB wipe is `make dev-cluster-restart-purge`
  (NOT `dev-refresh-purge`, which doesn't exist). Plain `make dev-refresh`
  rebuilds memql containers without wiping.
- Ids: canonical form is `<partition>:v1:<concept>:<bareSlug>`. Bare
  slugs from payloads never match canonical-form filters; use
  `canonicalId()` or rely on the engine's `@relationship` auto-canon
  on insert.

## What "shipped" actually means here

The flow is wired schema-side and 80% wired in code. The user can
chat with Sofia (the GA), Sofia knows about Computer Use, the
worker (memql-cockpit) connects and shows in the header pill, but
the **scope-elevation card never appears on the canvas** when the
agent asks for elevation. The agent now correctly calls
`requestComputerUseScope` (after the actor-context fix in `6b702bb`)
but the card doesn't reach the user's canvas. That's the bug to chase.

## What user wanted that just landed (this handoff's prep)

The user asked for the standing scope picker in **Settings → Computer
Use** to be REMOVED. Their reasoning: scope should be per-task, not a
global toggle. A "Full" standing grant means an agent can run any
shell command at any time, which the user doesn't want to opt into
universally just to run one command once. Removed in this handoff's
prep commit (frontend `src/components/settings/ComputerUseSection.tsx`).
The Settings panel now shows pairing / disconnect / status only;
scope grants happen exclusively via the per-task elevation card.

That removal is committed. Don't undo it. The whole point of the
remaining work is to make the elevation card actually show up.

## The chain that should work

1. User asks Sofia (or any agent with `computer_use`) to do something
   needing more scope than they have. Example: "create
   `~/birds_research` and put a file in it."
2. Sofia recognizes it needs `full` scope (shell exec + fs writes).
3. Sofia calls `requestComputerUseScope({intent, requestedScope, summary})`.
4. The agent-side handler `handleRequestScope` in
   `integrations/agent/worker/integration.go` runs
   `mutationCreateScopeElevationPlan` (with a synthetic user actor on
   the context, fix in `6b702bb`).
5. The mutation lands a `v1:copresent:plan` row with
   `kind="scopeElevation"`, `status="awaitingFeedback"`,
   `feedbackReason="scope_elevation_required"`,
   `computerUseScope=<requested>`.
6. `automations/v1/copresent/emitScopeElevationCanvasCard/automation.memql`
   triggers on `graph.node.created.*.v1:copresent:plan` filtered by
   `payload.kind=="scopeElevation"` and writes a `v1:copresent:canvasState`
   row via `mutationCreateCanvasState`. Visibility=private,
   forUserId=requestedBy.
7. Frontend `CanvasStateRenderer.tsx` dispatches
   `plan.scopeElevationRequested` to `PlanScopeElevationCard.tsx`.
8. User sees the card on the active space's canvas with: intent,
   summary, scope picker (capped at requested), Allow / Deny buttons.
9. Allow → updates the GA's `agentAuthorization.computerUseScope`
   row (per-Plan grant TBD; today it updates the standing scope as
   a side-effect of the approval) + marks the Plan succeeded.
10. User re-asks Sofia. Now her scope is elevated. `workerHost.exec`
    dispatches. Directory created. Done.

What works as of `6b702bb`:
- Steps 1-5 work. Sofia calls the tool, the Plan lands. Verifiable
  via `psql` against the DB after a test interaction.
- Step 7 is wired — `CanvasStateRenderer.tsx` has the dispatch
  branch and `PlanScopeElevationCard.tsx` exists.

What does NOT work (or needs verifying):
- Step 6 — does the `emitScopeElevationCanvasCard` automation
  actually fire? The trigger pattern is
  `graph.node.created.*.v1:copresent:plan`; the `*` should match the
  partition. The filter is `payload.kind=="scopeElevation"`. Verify
  with `docker logs memql-cognition --tail 500 | grep
  emitScopeElevationCanvasCard`.
- Step 6 — does it successfully emit? Check that the
  `v1:copresent:canvasState` row lands:
  ```
  docker exec memql-db psql -U memql -d memql -c "SELECT id, payload::jsonb->>'kind' AS kind, payload::jsonb->'data'->>'variant' AS variant FROM \"MemoryNodes\" WHERE concept='v1:copresent:canvasState' ORDER BY \"createdAt\" DESC LIMIT 5;"
  ```
- Step 8 — does the frontend even subscribe to canvasState updates
  for the active space? If subscriptions filter by visibility/owner,
  verify the elevation card (which is private + forUserId) actually
  reaches the renderer.

## Suspected failure modes (in priority order)

### A. Visibility / forUserId filtering on the frontend subscription

The card is emitted as `visibility="private"` with `forUserId =
requestedBy`. The frontend's canvas subscription likely filters on
visibility+ownerId. If the filter mismatches the user's id format
(canonical vs bare), the card lands in the DB but never reaches the
React tree. **Most likely culprit.**

Check by:
1. Reproducing the elevation request (just ask Sofia to do
   something requiring full scope).
2. After her reply, query the DB for the canvasState row.
3. If the row is in the DB but the card isn't visible, the frontend
   subscription is the bug.

### B. `emitScopeElevationCanvasCard` automation not firing

The automation file is at
`automations/v1/copresent/emitScopeElevationCanvasCard/automation.memql`.
Check that it loaded by searching cognition logs for the registration
line at startup. If the automation didn't load, it might be a parse
error or a path mismatch.

### C. The Plan's `spaceId` field empty or mismatched

The card automation derives `space` from `event.payload.spaceId`. If
that's empty (because `requestComputerUseScope` was called outside an
active-space context, e.g. a standalone direct-message turn), the
card lands with empty `space` and the renderer can't place it.
Inspect `event.payload.spaceId` value via the Plan row in the DB.

### D. Browser cache / stale frontend bundle

The user runs `npm run dev` for CoPresent locally. Vite HMR should
pick up changes, but if the browser tab hasn't been hard-refreshed
(Cmd-Shift-R) since `574a0a5` landed, the dispatch branch for
`plan.scopeElevationRequested` may not be in the loaded bundle.
Easy to rule out — ask the user to hard-refresh and try again.

## Concrete first moves for the next agent

1. **Read recent git logs** for context:
   ```
   cd /Users/znas/projects/memql && git log --oneline -30 main
   cd /Users/znas/projects/copresent && git log --oneline -10 main
   ```
   Pay attention to the last 5–6 memql commits — they're all the
   computer-use scope flow.

2. **Reproduce the bug**:
   - Ensure cluster is running (`docker ps`).
   - Ensure user has paired their cockpit (header pill green).
   - In CoPresent, ask Sofia: "create a directory called
     `~/temp_test` and put a file in it called `birds.txt` with the
     names of 10 birds."
   - Watch the agent log:
     ```
     docker logs -f memql-agent | grep -iE "requestComputerUseScope|scopeElevation|workerStatus"
     ```
   - Watch cognition log:
     ```
     docker logs -f memql-cognition | grep -iE "emitScopeElevation"
     ```

3. **Verify each layer**:

   ```sql
   -- Did the Plan land?
   SELECT id, payload::jsonb->>'kind' AS kind,
          payload::jsonb->>'status' AS status,
          payload::jsonb->>'feedbackReason' AS reason,
          payload::jsonb->>'spaceId' AS space_id
   FROM "MemoryNodes"
   WHERE concept='v1:copresent:plan'
     AND payload::jsonb->>'kind'='scopeElevation'
   ORDER BY "createdAt" DESC LIMIT 3;

   -- Did the canvas card row land?
   SELECT id, partition, payload::jsonb->>'space' AS space,
          payload::jsonb->'data'->>'variant' AS variant,
          payload::jsonb->>'visibility' AS vis,
          payload::jsonb->>'forUserId' AS for_user
   FROM "MemoryNodes"
   WHERE concept='v1:copresent:canvasState'
   ORDER BY "createdAt" DESC LIMIT 5;
   ```

   - If the Plan exists but the card doesn't → automation isn't
     firing OR is erroring. Check logs.
   - If both exist but the user doesn't see the card → frontend
     subscription / visibility filter mismatch (Failure mode A).

4. **Fix end-to-end. Commit. Push. Tell the user to hard-refresh
   their browser + ask Sofia to retry**. Don't fork the work.

## Files of interest

Backend (memql):
- `tools/v1/agent/worker/requestComputerUseScope.memql` — tool def
- `queries/v1/worker/builtin/builtinAgentworkerRequestScope.memql` — bridge
- `integrations/agent/worker/integration.go` — `handleRequestScope`
  (where the actor-context fix lives)
- `mutations/v1/copresent/createScopeElevationPlan.memql`
- `automations/v1/copresent/emitScopeElevationCanvasCard/automation.memql`
- `prompts/v1/agent/agentReply.tmpl` — Sofia's prompt; the
  `Scope-elevation flow` section under `{{if .computerUseStatus}}`

Frontend (copresent):
- `src/components/canvas/cards/PlanScopeElevationCard.tsx`
- `src/components/canvas/CanvasStateRenderer.tsx` — dispatch branch
  on `case 'plan.scopeElevationRequested':`
- `src/types/copresent.ts` — `PlanScopeElevationCardData` interface
- `src/components/settings/ComputerUseSection.tsx` — scope picker
  REMOVED here; only pairing/disconnect/status now

## Open follow-ups (NOT this handoff's job, but worth knowing)

- `v1:copresent:agentAuthorization` has no `@relationship` on
  `agentId` or `userId`. Auto-canon doesn't fire; values are stored
  as bare slugs. The frontend's `pickGAAuthRow` matcher (in
  `PlanScopeElevationCard.tsx`) compensates with a tolerant
  bare-or-canonical match. Adding the annotations would be cleaner
  but requires DB wipe to canonicalize existing rows.
- `chat54Mini` (the GA's default model) is small and loses
  instructions in long prompts. The Scope-elevation section was
  strengthened in `6b702bb` but it might still be brittle. If the
  agent stops calling `requestComputerUseScope` and reverts to
  conversational refusal, the prompt needs another pass.
- The "Allow" path on `PlanScopeElevationCard` updates the
  STANDING `agentAuthorization.computerUseScope`. The user wants
  per-task scope eventually, not standing. Per-Plan scope
  enforcement requires the planner-level path (Plan-declared
  scope vs standing scope, narrower-only). That's a v0.x → v1
  feature and not in scope for this handoff.
