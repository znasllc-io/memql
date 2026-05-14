# Handoff prompt — copy this to brief a new implementer

This is the prompt to paste into a new chat (or hand to a developer) when onboarding someone onto any of the in-flight initiatives. Each project has a comprehensive plan document committed to the memql repo; this prompt gets the implementer pointed at the right doc and explains how the user works.

---

## Prompt to paste

```
You're picking up work on the memql / copresent product stack. Several
initiatives are in flight; pick the one I direct you to (or the one
with the next pending phase).

REPOSITORIES (both already cloned, both on `main` branch only):
  /Users/znas/projects/memql      — Go backend / control plane
  /Users/znas/projects/copresent  — TypeScript/React frontend

SHIPPED (no work pending — read the operational doc if you need to
reason about how it works):

  IDENTITY SERVICE (in-house auth)
    Magic-link login, JWKS, admin web app, setup wizard, copresent
    cutover. All in main. Operational notes:
    memql/docs/auth/identity-service.md.

  COPRESENT WORKERS (agent-callable computer-use tools)
    Operational reference at memql/docs/workers/runbook.md. Source:
    component/worker/, integrations/agent/worker/,
    cmd/memql-cockpit/internal/worker/. The implementation plan
    has been removed per the no-stale-docs convention.

  VOICE / AUDIO ARCHITECTURE
    Shipped 2026-05-09; the plan doc was deleted per the
    no-stale-docs rule. Canonical voice catalog, agent.gender +
    auto-assigned voice, per-agent audio control overlay, and the
    LiveKit-only ASR path in a Polyphon room are all live. See the
    "Voice Pipeline (Polyphon)" section of memql/CLAUDE.md for the
    operational reference.

  DEEPGRAM MIGRATION
    Shipped 2026-05-11; the plan doc was deleted per the
    no-stale-docs rule. NVIDIA Riva is gone (DSL builtin, bridge
    branch, app wiring, voice-catalog hints, config/proto, the
    integrations/riva package, and all infra/docs). Deepgram is
    the new auto-selected default for both ASR (Nova-3 streaming
    WS) and TTS (Aura-2 /v1/speak REST) when
    MEMQL_DEEPGRAM_API_KEY is set; OpenAI is the startup-time
    fallback. Mid-session failover is explicitly out of scope.
    See the "Voice + Video Pipeline" section of memql/CLAUDE.md
    and docs/polyphon-architecture.md for the operational
    reference.

  INITIATIVE C — REALTIME VOICE + VIDEO ARCHITECTURE
    Shipped through Phase 11 in 2026-05; the plan doc was
    deleted per the no-stale-docs rule. Voice transport
    collapsed to 1:1 GA-only on LiveKit Agents 1.5 (Deepgram
    Nova-3 STT + custom memql LLM plugin BYO + Deepgram Aura-2
    TTS), the Go Bridge Agent retired in favor of the Python
    voice-agent process under voice-agent/, Anam + Simli
    avatar plugins live, parallel audioControl + videoControl
    fields, PresencePanel orb toggles bigger / bounded /
    responsive. Open debugging tickets continue under
    docs/voice/voice-agent-handoff.md. Operational reference:
    "Voice + Video Pipeline (LiveKit Agents 1.5)" section of
    memql/CLAUDE.md.

  INITIATIVE D — POLICIES + DSL HYGIENE REFACTOR + DSL CONSOLIDATION
    Phases 0-10 of the policies initiative shipped (plan doc
    self-deleted at 65d7088). Follow-on dependency-tree
    refactor shipped 2026-05:
      Phase A — specs broaden to atomic boolean predicates
                (row-spec compiles to SQL; context-spec
                evaluates in-process; bodies that mix both
                are rejected).
      Phase B — struct-form `query NAME { concept ... filter
                ... shape ... }` syntax; 115 simple queries
                migrated mechanically.
      Phase C — engine rejects policies whose body would
                compile as a context-spec (pure caller
                booleans must live in specs/ and be called
                via `spec("name")`).
    Operational reference: the "DSL dependency tree",
    "Specs", "Policies", and "Key Concepts" sections of
    memql/CLAUDE.md.

PLANNED (brainstorm done, comprehensive plan committed, not yet
executed — pick whichever I direct you to):

  INITIATIVE A — MULTI-REPO BFF MIGRATION
    Plan: memql/docs/architecture/multi-repo-migration.md
    Memory: <memory>/project_bff_architecture.md
    Splits memql into a core Go library + per-client BFF repos
    (copresent-bff, memql-cockpit-bff, portal-bff), tied together
    via go.work for development. 8 phases.

  INITIATIVE B — CHAT ARCHITECTURE
    Plan: memql/docs/chat/chat-architecture-plan.md
    Memory: <memory>/project_chat_architecture.md
    Two-thread chat (Group + per-user Team) with per-human agent
    teams, hard 1-active-space-per-human cap, agents unbounded
    across spaces, discussion mode (per-private-thread, hybrid
    trigger, activity-level controlled), confidence-tiered misroute
    safety net, copresent_conversation knowledge domain +
    copresentConversation tool, canvas inheritance + Share-to-group,
    daily-space adjustment. 10 phases.

EVERY PLAN opens with a "Section 0: Pre-flight" that captures every
working convention I expect — repo rules, the four-phase workflow
(familiarize → brainstorm → plan → execute), the triage rule, the
pre-prod deletion policy, doc hygiene, the Makefile-canonical rule,
the AAA security framing, my brainstorming style, and pointers to
the persistent memory directory at:

  /Users/znas/Library/Application Support/Claude/local-agent-mode-sessions/<...>/spaces/<...>/memory/

That memory directory's MEMORY.md is the index of all rules and
project context. project_repos.md, project_chat_architecture.md,
project_realtime_voice_video.md, project_policies_feature.md,
project_bff_architecture.md, project_voice_provider_evaluation.md,
project_tasks_model_direction.md, and the feedback_*.md files
(especially feedback_canvas_over_banners.md and
feedback_dsl_ctx_convention.md) are the ones you'll care about most.

GROUND RULES THAT MATTER MOST (full detail in Section 0 of each plan):

  - Commit directly to `main` in both repos. Never feature-branch
    unless I explicitly ask.
  - Stage files with `git add <path>` per file. Never `git add -A`
    or `git add .` (multiple sessions run against the same tree).
  - When given a multi-step plan, execute end-to-end without
    pausing between phases. Use parallel agents aggressively.
  - At the end, COMMIT changes locally. DO NOT push. I validate
    locally before authorizing push.
  - Pre-production: delete dead code outright. No @deprecated, no
    fallback shims, no "TODO: remove later."
  - Stale docs are worse than missing docs. Update CLAUDE.md and
    docs/ in the same change as the code.
  - No emojis in any output (code, commits, docs, replies).
  - The Makefile is the canonical entry point for every build/run/
    test command. Multi-step logic extracts to scripts/<area>/<name>.sh
    following the project's bash conventions.

WHAT TO DO RIGHT NOW:

  1. Read the relevant plan document end to end.
  2. Read the memory file pointed to by that plan
     (project_bff_architecture.md, project_chat_architecture.md,
     project_realtime_voice_video.md, or
     project_policies_feature.md).
  3. Verify your dev environment (each plan has a quickstart
     section near the end). Confirm the existing stack boots
     cleanly before touching new code.
  4. Tell me which phase you're starting and what the commit
     boundary will be. Then go.

If anything in the plan contradicts what's in memory, memory wins
and we update the plan. If something seems wrong or unclear, ask
before assuming.
```

---

## How to use this prompt

- Default: send the full prompt above and tell the implementer which initiative (A or B) to take. They read that plan, ask which phase to start, and execute end-to-end and commit (don't push) at the phase boundary.
- For a focused hand-off, edit the prompt to mention only the relevant initiative.
- Once they've read the plan, they should ask for the phase to start with, then execute end-to-end and commit (don't push) at the phase boundary.

The full memory directory + the implementation plan docs together carry every decision, every rule, every dev-env setup step they need. They shouldn't have to ask you to re-explain anything that's already captured — and if they do, that's a signal something's missing from the docs and worth adding.
