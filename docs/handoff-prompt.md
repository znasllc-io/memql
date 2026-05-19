# Handoff prompt — copy this to brief a new implementer

The prompt below onboards a new agent (or developer) on the memql
stack. The pattern is "point at CLAUDE.md + the relevant handoff,
work end-to-end, open a PR." This doc itself ages quickly when it
enumerates specifics; the source-of-truth pointers below age
slowly.

---

## Prompt to paste

```
You're picking up work on the memql stack. Repos (Linux, post-split):

  ~/projects/memql/
  ├── go.work                   workspace tying the three Go modules together
  ├── memql/                    core: engine, gRPC, identity, DSL, node-type binaries
  ├── memql-bff-copresent/      BFF for the CoPresent product (Go, imports memql)
  └── memql-cockpit/            terminal IDE / ops console (Go, consumes memql-sdk-go)

The CoPresent React/Vite frontend lives in its own repo elsewhere on the
operator's machine; if you need to touch it, ask for the path.

GitHub org: znasllc-io. Email: jsanz@znasllc.io.

SOURCE OF TRUTH for operational state:

  - memql/CLAUDE.md                        engine + DSL + node architecture
  - memql/docs/CLAUDE.md                   docs layout + index
  - memql/component/CLAUDE.md              Go components
  - memql/integrations/CLAUDE.md           Go integrations + plug-in path
  - memql-bff-copresent/README.md          BFF-specific bootstrapping
  - memql-cockpit/README.md                cockpit-specific bootstrapping

  Domain-specific deep-dives are linked from each CLAUDE.md.

AUTO-MEMORY (persists across sessions, indexed by MEMORY.md):

  ~/.claude/projects/-home-znas-projects-memql/memory/

  Read MEMORY.md first; it indexes every saved user / feedback /
  project / reference memory. Apply feedback memories immediately;
  treat project memories as snapshots that may be stale.

HANDOFF DOCS (currently open work, in repo at memql/docs/):

  - handoff-ctx-purge.md                   historical record of the
                                           shipped ctx-envelope purge;
                                           deletable once a release ships.
  - handoff-computer-use-scope-elevation.md  feature absent from tree;
                                           needs product call before
                                           anyone resumes.
  - planning/portal-ai-router-handoff.md   product spec for the Portal
                                           team; under product review.

  Audit on demand: `ls memql/docs/handoff*.md memql/docs/planning/*.md`
  and read the STATUS banner at the top of each.

WORKING CONVENTIONS (the load-bearing rules):

  Workflow
    - PR-based. One feature branch per task; commit to the branch,
      push it, open a PR, merge via the GitHub UI or `gh pr merge`.
    - Before opening a PR (and before any merge), `git fetch origin
      main` and rebase the branch onto it. If the rebase produces
      conflicts, resolve them on the branch -- never on main.
    - When work spans repos, one PR per repo; cross-link them in the
      PR descriptions.
    - Push to feature branches is fine; pushes to `main` are blocked
      by policy and should never be attempted.

  Diffs + staging
    - Stage files by explicit path (`git add <file>`). Never `git add
      -A` or `git add .` -- the operator runs multiple sessions in
      the same tree and untracked files from a sibling session must
      not get swept in.
    - Commit messages via `git commit -F /tmp/msg.txt`; heredoc
      breaks on colons + quotes.
    - Sign every commit with the co-author trailer:
        Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>

  Code
    - Pre-production: delete dead code outright. No @deprecated, no
      fallback shims, no "TODO: remove later." Update CLAUDE.md and
      docs/ in the SAME change as the code -- stale docs are worse
      than missing docs.
    - The Makefile is canonical for every build/run/test command.
      Multi-step logic extracts to `scripts/<area>/<name>.sh` with
      `#!/usr/bin/env bash` + `set -euo pipefail` + function-based
      structure.
    - No emojis anywhere (code, commits, docs, PR bodies, replies).
    - Backend identifies as "SI"; user-facing copy says "AI".

  Execution
    - When the operator says "do it end-to-end," run uninterrupted.
      Don't pause between phases. Use parallel agents aggressively.
    - The operator verifies UX; the agent verifies lower-level state
      (docker logs, DB queries, build/test). Don't ask the operator
      to grep / psql / navigate the UI.
    - "Branch off what you have so far" = preserve current branch's
      commits in the new branch, not branch off main fresh.

WHAT TO DO RIGHT NOW:

  1. Read the relevant CLAUDE.md(s) end-to-end.
  2. Read the handoff doc the operator pointed you at (if any).
  3. Skim ~/.claude/projects/-home-znas-projects-memql/memory/MEMORY.md.
  4. Confirm the dev stack boots cleanly before touching new code:
       cd ~/projects/memql/memql && make dev-cluster-restart
     (or `make dev-refresh` for non-purging rebuilds).
  5. Tell the operator which branch you're cutting and what the
     commit boundary will be. Then go.

If memory and a doc conflict, memory wins and you update the doc in
the same PR. If anything is genuinely unclear, ask before assuming.
```

---

## How to use this prompt

- Default: paste the full prompt above and tell the implementer
  which handoff (or `git log` slice) to start from. They read the
  pointers, ask which branch they're cutting, and execute.
- For a focused handoff, edit the prompt to mention only the
  relevant CLAUDE.md / handoff doc and remove unrelated context.
- The CLAUDE.md files + memory directory carry every operational
  decision and convention. The implementer shouldn't have to ask
  for re-explanations of anything that's already captured -- if
  they do, that's a signal the source-of-truth pointer above is
  missing or stale; add it.
