---
title: The owner's run -- a bug, a goal, Claude Code as the only app, through native CI to main
audience: internal
status: draft
area: operate
sinceVersion: 0.31.2
owner: znas
---

# The owner's run with Claude Code

Owner brief, 2026-10-25. Yesterday the owner put a feature into MemQL as a
goal, routed it to an app, and Codex carried it from an Ask message to a pull
request (#6287, Ask actions moved into each message) while also auditing the
harness it ran in. Today the same run is repeated with **Claude Code as the
only app**, on **this Linux machine against the local cluster**, on a **bug
the owner and the session agreed on**, until the fix is **merged into main
through native CI**. Not deployed. Throughout, the session helps as the owner
and records every harness defect, stall and improvement it sees.

This record is the design of that run: what is already there, what was
decided, the exact steps, the boundaries, and what is deliberately out of
scope. It stands on the brainstorm of 2026-10-25, where every fork below was
put to the owner as a selectable question and answered.

## 1. The program this run belongs to

The owner's longer aim is self-repair as a platform feature: any signed-in
person reports a bug; a goal is created for them that they can watch and not
drive; the goal is assigned to a developer of that installation (the owner
when there is none); the developer's validation is what makes MemQL work the
goal; a tenant's developer validates changes to their own DSL, packs and
deployables while an engine change goes upstream to the engine maintainer,
who the tenant can only watch.

Most of that pipeline already exists on main, under names the brief did not
use (section 2). What does not exist is the part for people below developer
rank, the two-step validation, diagnosis before a scoped change request, and
the decision of which repository a bug lives in. **Those are out of scope
here** (section 10). This run is the slice that exists today: the owner is
the engine maintainer, so their request starts at once.

## 2. What exists on main (2026-10-25, `50f2ea9bf`)

- **Native CI/CD.** `.github/workflows/` is gone. The pipeline is declared in
  `memql-package.yaml`, run by `component/pipelines` and friends, documented
  in [pipelines.md](../../public/operate/pipelines.md). Releases 0.30.x and
  0.31.x were cut through it. The ONLY required check on `main` is
  `MemQL / memql` from GitHub App 4822538; the strict up-to-date policy is
  off.
- **Repository authority** (`component/repositoryauthority`,
  `dsl/repositoryauthority`, the Pipelines app's Changes tab, the
  `requestRepositoryChange` tool in the `software-development` skill). A
  person requests a scoped change: statement, operation (`propose_change` or
  `deliver_change`), allowed paths. An INSTALLED resolver
  (`MEMQL_REPOSITORY_AUTHORITY_RESOLVER` plus bindings) routes it to a
  maintainer source on this installation or to a request-only peer, meaning
  another installation. The goal opens OWNED BY THE MAINTAINER, parked on
  their approval; a requester who is the maintainer runs at once. The engine
  ships NO resolver workflow: it is installed DSL, and the local cluster has
  none configured.
- **The cross-installation boundary.** A transfer carries statement,
  operation and paths; no owner, template or decision travels. Remote
  requesters see status without the upstream goal or approval ids. This is
  the "a tenant cannot approve engine changes" rule, already enforced.
- **Automated repair as installed policy**
  ([pipelines.md](../../public/operate/pipelines.md#automated-repair-is-an-installed-policy)):
  a failed check may be diagnosed and turned into a bounded repair PR, gated
  by evidence, independent review and maintainer authority.
- **Forge** (`dsl/forge`, [forge.md](../../public/operate/forge.md)): the
  older request spine for bugs and features by ROLE. Reader submits,
  developer validates, owner approves, then `queued`, where it stops.
  Automated implementation was deferred; it is MCP-only, has no OS surface,
  and predates the RBAC catalog.
- **Local apps as execution surfaces**
  ([local-apps.md](../../public/operate/local-apps.md)): a tool-needing step
  resolved to an `app:` door becomes a headless `run` SESSION on one of the
  owner's machines, with MemQL's tools reachable over MCP. The runnable set
  is closed: `claude-code`, `codex` (`core/airoute/apps.go`). The Ask route
  picker offers Auto, Local and Apps; Apps is `app:*`.
- **Per-machine app routing** (`v1:worker:registration.appRouting`, written
  only by `fleetSetAppRouting`, read by `component/memql/app_selection.go`
  on every selection): mode `auto`, `roundRobin`, or ONE app id. A quota
  refusal with zero usage re-picks another app; this is how "Claude Code had
  no funds" was accounted for yesterday.
- **The handoff branch** `handoff/combined-readiness-20261025` at
  `639f808c1`: PR #6290's head `aeee127803` plus one commit adding
  `handoff/combined-readiness-20261025/` (README, manifest, validation and
  native-check records, a standalone cockpit patch). Verified this session:
  checksums match, the four frozen heads (#6286, #6287, #6283, the
  integration) are ancestors, the push created zero check runs. Its final
  head NEVER passed natively: the last run was cancelled by the pause, the
  head before it failed 8 test lanes that later commits claim to repair with
  local compile-only evidence for the database and agent cases.
- **CI is PAUSED** until the owner says CI/CD is ready. While paused: no PR,
  no PR head update, no CI rerun, no merge, no deploy, no pipeline-settings
  change, no benchmark. The engine pipeline on the cloud installation was
  left CONNECTED, so opening any pull request on the repository starts a
  native run. **A PR is a CI restart.**

## 3. What yesterday's run actually was, and what changes

| | Yesterday (2026-10-23/24) | This run |
|---|---|---|
| Machine | the owner's Mac | this Linux machine (k3d host, ollama, cockpit 0.20.0 installed) |
| Cluster | the cloud installation | the local k3d cluster |
| App | Codex (Claude Code had no credits) | Claude Code only, pinned |
| Engine head | main of that day | the handoff head `aeee127803` (owner's choice, D1) |
| Delivery | the app's own branch, push and PR under the owner's account; NOT the governed proposal path | the same, recorded as a finding (D5) |
| Stop | PR open, native check ran, then the pause | pushed branch and evidence; PR only after the pause lifts (D4) |

The local cluster today: one user (the owner), the engine repository
connected through the GitHub App, its pipeline row `disconnected` (poll,
cluster compute), 17 goals (16 pipeline runs and one workbench smoke test),
two registrations for this machine with labels `app:claude-code` and
`app:codex`, images built 2026-10-17, no authority resolver.

## 4. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Which head runs locally | The handoff head `aeee127803`. It carries yesterday's harness fixes, driving it is a local build that touches no CI, and it is the fastest way to learn whether the 8 unproven repairs hold. |
| D2 | Which bug | memql#5792: editing the operator account's profile domain is refused `domain_under_cluster_domain` naming the cluster's own domain. OS-visible in Accounts, reproducible on the local operator account, bounded to the accounts validation plus a legacy-row repair and a regression test. |
| D3 | How Claude Code is pinned | The product's own switch: Fleet, this machine, App routing, mode "Claude Code" (`appRouting.mode`). No engine code. A memory note records that it must be REMOVED afterwards; the alternative was removing `codex` from the local home in `~/.memql/policy.yaml`. |
| D4 | Where the run stops under the pause | At the pushed branch with its evidence. The PR, the native check and the merge happen only after the owner says CI/CD is ready. |
| D5 | Delivery path | Under the owner's own authority (the app's own branch, push and PR), as yesterday, because no governed change route is installed locally. Configuring one is a FINDING for the audit, not a step of this run. |
| D6 | Who merges | The owner, through the owner bypass (`scripts/dev/merge-as-owner.sh --pr=N --check`, then the merge): the PR's author is the owner's account, so the code-owner review needs the bypass. Auto-mode sessions do not run it. |
| D7 | The audit's shape | A findings log during the run; afterwards one GitHub issue per defect or improvement under the `claude` label, with evidence and a proposed fix, plus one progress comment on memql#5799. Each fix is its own change with its own approval. Nothing in the harness is patched mid-run. |
| D8 | Red lanes after the PR | Go back into the SAME goal as owner feedback; Claude Code repairs the same branch. The session does not fix the branch by hand. |
| D9 | Goal acceptance | The owner accepts the goal in Nexus. An agent never accepts it for them. |
| D10 | The design record | Committed on a docs branch from `origin/main` in a scratchpad worktree, pushed WITHOUT a PR (a push to a non-default branch starts nothing); its PR opens when the pause lifts. |

## 5. The runway (setup)

Nothing in this section touches GitHub or the paused CI.

1. **Worktree at the handoff head.** A detached checkout of
   `origin/handoff/combined-readiness-20261025` under
   `/home/znas/memql-projects/`, beside the existing worktrees. The shared
   tree stays on `main` for the other sessions. No branch, no commit.
2. **Rebuild the local cluster from it.** `make dev` from that worktree
   builds the seven node images and the OS bundle, imports them into k3d and
   restarts the pods. Rows survive; migrations run on start. This replaces
   the 2026-10-17 images. Check: every pod `Running`, `make status` shows one
   node id per pod and one identity keyset.
3. **Rebuild and swap the cockpit.** `make -C cockpit cockpit-computeruse-host`
   in the worktree produces `bin/memql-computeruse-linux-amd64`. Stop
   `memql-worker.service`, copy the installed binary aside as a backup, swap
   the file the `~/.memql/bin/memql` symlink points at, start the service.
   Check: the worker journal shows the local home connected, and the
   machine's registration row refreshes `lastSeenAt` with the
   `app:claude-code` label and the new cockpit version.
4. **Pin Claude Code (D3).** In MemQL OS: Fleet, this machine, App routing,
   "Claude Code". Check: the registration row carries
   `appRouting.mode = "claude-code"`.
5. **Readiness.** Claude Code signed in on this machine (the cockpit's probe
   reports it as `signedIn`); the Ask route "Apps" reads ready for the local
   cluster; the local engine pipeline stays `disconnected` so no duplicate
   check run can appear; the Accounts app opens as the owner.

## 6. The run

**The owner puts in the bug.** In MemQL OS at `os.<domain>` of the local
cluster, signed in as the owner, in Ask, route "Apps", destination Auto, the
owner's message is the issue in its own words:

> Fix memql#5792 in the engine repository. Editing the operator account's
> profile and setting its domain is refused with `domain_under_cluster_domain`,
> and the rejected value is the cluster's own domain rather than the submitted
> one; `validateAccountMemqlDomain` validates the merged `memqlDomain`, so an
> existing operator-account reservation value is incompatible with the
> client-domain guard. Expected: an owner can edit the operator
> organization's ordinary profile without the platform treating its cluster
> address as a new client-domain reservation. Keep the collision guard for
> client accounts. Repair incompatible legacy rows through the appropriate
> migration or normalization. Acceptance: reproduce it on this cluster first;
> a regression test that uses an existing stored operator row, not only a
> fresh seed, fails before the fix and passes after it; the checks for the
> changed boundary pass. Deliver on a branch from `origin/main` pushed to
> `origin`, with a commit whose message says `Closes #5792`. Do NOT open the
> pull request until I say CI/CD is ready. Report when the branch is pushed,
> with the test output and before-and-after captures of the Accounts profile
> edit.

**MemQL works it.** The goal and run open; the first tool-needing step
resolves to the `claude-code` door on this machine and becomes a headless
session with the MCP back-channel. Claude Code works in the workspace the
cockpit chooses, under the owner's authority: `gh` is signed in as the owner
and git pushes over ssh. With no authority route installed,
`inspectRepositoryAuthority` reports the route unavailable and the
`software-development` skill proceeds under the caller's own authority (D5).

**The session's role.** Watch Nexus (goal, run, steps, approvals, the
session's chunks). Answer feedback requests as the owner. Never accept the
result. Keep the findings log (section 8). Patch nothing in the harness
without asking.

**What else is pinned: nothing.** Model calls that are not sessions (review,
replans, outcome conversion) route by the shipped rules: `fleet:strongest`
first, which is this machine's local model through ollama, then `app:*`. No
federation exists locally.

**Evidence the run leaves.** The app's before-and-after captures of the
Accounts profile edit, the regression test output, the checks it ran, the
pushed branch, and the goal journal.

**Stop (D4).** The branch is pushed and the report is in. The goal stays open
and unaccepted until the pause lifts.

## 7. After the pause lifts

1. The owner says CI/CD is ready. The session tells the goal to continue, as
   the owner, in Ask or Nexus: open the pull request against `main`.
2. Claude Code opens the PR with `Closes #5792`. The cloud installation runs
   `MemQL / memql` in affected mode. The run may sit `queued` under
   [operator run admission](../../public/operate/pipelines.md#operator-run-admission)
   until the agents' admission file allows new runs; opening that is the
   owner's act.
3. A red lane returns to the same goal as owner feedback (D8).
4. Green: the owner runs `scripts/dev/merge-as-owner.sh --pr=N --check`,
   then the merge (D6). The script reads the ruleset, so it knows the one
   required App check and refuses on a check that has not reported. No
   release, no publication, no deploy.
5. The owner accepts the goal in Nexus (D9). The session verifies `main`
   ancestry and that #5792 closed.

The fix branches from `origin/main`, independent of #6290, because the cloud
installation builds what lands on `main`. If both touch one file, whichever
lands second rebases.

## 8. The audit

During the run the session appends to a findings log in its scratchpad, one
entry per observation:

- what happened (a stall, a wrong refusal, a repeated call, a confusing
  surface, a missing control, an unclear message), in one sentence;
- where: the goal, run and step ids, the row or log line, the OS surface;
- evidence: the exact text, a capture, a journal row;
- the harness rule or code it points at, when known;
- a proposed fix, sized.

Afterwards (D7) each entry becomes a GitHub issue under the `claude` label,
grouped where one cause explains several, and one progress comment goes on
memql#5799. Expected findings already in hand from the brainstorm:

- the governed proposal path is unused for an owner-maintainer run because no
  resolver ships with the engine; whether the engine should ship a default
  route for the owner's own repositories is a design question;
- memql#5817 is fixed in code (the child goal stamps `agent`) and still open:
  it needs its db-gated test and a close;
- the local engine images and cockpit lag the tree by a week with nothing
  saying so in the OS.

## 9. Boundaries

- No PR, PR head update, CI rerun, merge, deploy, pipeline-settings change or
  benchmark while CI is paused. A PR is a CI restart.
- No change to the cloud installation's settings, admission or pipeline
  connection.
- No edits in the shared tree at `/home/znas/memql-projects/memql`; every
  build and commit of this run happens in its own worktree.
- No hand-fixing the fix branch; no accepting the goal for the owner; no
  harness patch during the run.
- Every git stage names its paths (`git add <file>`); never `-A` or `.`.
- The pin (D3) is removed when the run is over, and the memory note with it.

## 10. Out of scope, recorded for the next brainstorm

The gaps between the owner's program (section 1) and main:

- A rank-100 user cannot create anything that becomes a goal; the request
  capability is seeded for owner and developer only.
- The reporter gets a receipt, not a goal; today the goal belongs to the
  maintainer.
- A bug report names neither paths nor an operation; something must diagnose
  before a bounded change request can exist.
- Nobody decides whether a bug lives in the tenant's repository or in the
  engine. The two-step validation exists only in Forge, which cannot execute.

## 11. Risks

- The handoff head is unverified natively. Running it locally may surface the
  8 unproven repairs as live defects; that is a finding, not a blocker.
- Reproducing #5792 locally depends on the local domain's shape: the guard
  derives `memql.<domain>` when `memqlDomain` is empty and refuses it when it
  equals the cluster's apex. If the local shape cannot reproduce it through
  the UI, the regression test on a stored operator row is the reproduction.
- Claude Code's quota: a quota refusal with zero usage re-picks another app,
  and with the pin there is none; the run parks and the owner decides.
- The cockpit swap: a worker that fails to start leaves the machine offline;
  the backup binary restores it in one copy.
- Capacity: `make dev` builds seven images on the machine that also hosts the
  cluster and the local model. Build before the run, not during it.

## 12. Cleanup

Fleet App routing back to Auto; the memory reminder deleted; the backup
cockpit binary kept until one full session has run on the new one; the
worktree kept as the build source while the handoff head runs locally.
