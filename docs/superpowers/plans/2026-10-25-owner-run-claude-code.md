---
title: The owner's run with Claude Code -- implementation plan
audience: internal
status: draft
area: operate
sinceVersion: 0.31.2
owner: znas
---

# The Owner's Run With Claude Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Repeat yesterday's owner run on this Linux machine against the local cluster: an owner's Ask goal naming memql#5792, routed to Claude Code as the only app, carried by Claude Code to a pushed branch, then (after the owner lifts the CI pause) to a pull request merged into `main` through native CI, with every harness defect recorded on the way.

**Architecture:** Nothing in the engine changes. The run is operated: a detached worktree at the handoff head is the build source for the local cluster's images and for the cockpit; Claude Code is pinned through the Fleet machine's App-routing row; a one-line smoke goal proves an app session can run on this machine before the real goal; the real goal stops at a pushed branch while CI is paused; the audit becomes `claude`-labelled issues.

**Tech Stack:** k3d + ArgoCD local cluster (`make dev`), the MemQL Cockpit worker (user systemd unit), MemQL OS in Chrome (the `claude-in-chrome` tools), the local Postgres through `kubectl exec`, `gh`.

**Spec:** `docs/superpowers/specs/2026-10-25-owner-run-claude-code-design.md`

## Global Constraints

- **CI is paused** until the owner says, in words, that MemQL CI/CD is ready. Until then: no pull request opened or updated, no CI rerun, no merge, no deploy, no pipeline-settings change, no benchmark. A pull request on the repository IS a CI restart (the cloud installation's engine pipeline is connected).
- **No edits in the shared tree** `/home/znas/memql-projects/memql`. Builds run from the handoff worktree; docs commits run from the scratchpad worktree on `docs/claude-code-owner-run-5792-design`.
- **No harness patch during the run**, and no hand edits to the fix branch. A defect is a findings-log entry, then an issue.
- **The goal is accepted by the owner in Nexus**, never by the session.
- **Only the app door is pinned** (`appRouting.mode = "claude-code"` on this machine's registration). The pin is removed in Task 9 and the memory note with it.
- **Stage by explicit path** (`git add <file>`); never `-A` or `.`.
- **No operator hostnames** of the cloud installation in anything committed (`TestNoVendorDomainLiterals`); the local hosts `os.memql.localhost`, `api.memql.localhost`, `mcp.memql.localhost` are fine.
- **No secrets** in the findings log, issues or comments: no tokens, no `.credentials.json` content, no cookie values.
- **The cockpit's own service unit on this machine is not changed by this plan.** If the smoke session shows the unit's confinement stops an app session, the plan stops and the owner decides (Task 5).

## Review Focus

1. **The worker's service confinement versus an app session.** The cockpit's shipped Linux unit leaves only `~/.memql` writable in the home directory, while a session's chosen workspace is under `~/.local/share/memql/workspaces` and Claude Code's shell sandbox needs bubblewrap. A person expects a clear refusal in Ask naming the cause, not a silent stall. Task 5's smoke session pins this: it records exactly what Ask, Nexus and the worker log say.
2. **Claude Code opens the pull request despite the instruction.** The owner's message forbids it until CI/CD is ready. Task 6 Step 7 checks `gh pr list --head` after the push and treats an opened PR as a finding to report to the owner at once, never as something to cancel on the cloud.
3. **The pin is honoured only on an online machine whose `features.apps` is true.** Task 4 verifies both on the registration row, not only the `appRouting` field.
4. **A quota refusal with nothing else to pick.** With the pin, a zero-usage quota refusal has no other app to re-pick; the run parks. Task 6 Step 5 watches for an `inferenceUnavailable` or `budget` approval and hands it to the owner.
5. **The Ask route "Apps" reads not ready.** After the cockpit swap the machine may be offline for a minute, and the route refuses the Workbench destination. Task 5 Step 1 checks the picker before sending anything.

---

### Task 1: Preflight, the handoff worktree, and the findings log

**Files:**
- Create (worktree): `/home/znas/memql-projects/memql-handoff` (detached at `639f808c1`)
- Create: `/tmp/claude-1000/-home-znas-memql-projects-memql/61d53756-0916-4733-ab28-869f7baff0a7/scratchpad/findings.md`

**Interfaces:**
- Produces: the worktree path `WT_HANDOFF=/home/znas/memql-projects/memql-handoff` and the log path `FINDINGS=.../scratchpad/findings.md`, used by every later task; the shell helper `Q` for database reads.

- [ ] **Step 1: Confirm the shared tree is untouched and the pause assumptions still hold**

```bash
cd /home/znas/memql-projects/memql && git status -sb | head -2
gh pr view 6290 --repo znasllc-io/memql --json headRefOid,state --jq '"\(.headRefOid[:9]) \(.state)"'
gh api repos/znasllc-io/memql/commits/639f808c19a2924f8ebb70c6284a4d788dbcfbfa/check-runs --jq '.total_count'
```
Expected: `## main...origin/main` with no changes; `aeee12780 OPEN`; `0`. If the PR head moved or a check run exists, stop and tell the owner: somebody resumed qualification.

- [ ] **Step 2: Add the worktree at the handoff head**

```bash
cd /home/znas/memql-projects/memql && git fetch origin && \
git worktree add --detach /home/znas/memql-projects/memql-handoff origin/handoff/combined-readiness-20261025 && \
git -C /home/znas/memql-projects/memql-handoff log -1 --format='%h %s'
```
Expected: `639f808c1 Add portable combined-readiness handoff`.

- [ ] **Step 3: Start the findings log with the entry template**

```bash
cat > /tmp/claude-1000/-home-znas-memql-projects-memql/61d53756-0916-4733-ab28-869f7baff0a7/scratchpad/findings.md <<'LOG'
# Findings log -- the owner's run with Claude Code, 2026-10-25

Entry template (copy per observation):

## F<n>: <one sentence: what happened>
- Where: <goal id / run id / step key / OS surface / log file>
- Evidence: <exact text, row, log line, capture path>
- Points at: <harness rule, file, or "unknown">
- Proposed fix: <one or two sentences, sized>
- Severity: <blocks the run | wrong answer | confusing | cosmetic>

LOG
echo ok
```
Expected: `ok`.

- [ ] **Step 4: Define the database read helper used by every later task**

Put this in each shell that needs it (shell state does not persist between tool calls):
```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select count(*) from \"MemoryNodes\";"
```
Expected: a row count (109114 at planning time; any number is fine).

- [ ] **Step 5: Record the preflight in the log**

Append an entry `F0` with the worktree sha, the PR head, the pod image dates (`docker image inspect memql-bff:local --format '{{.Created}}'`) and the installed cockpit version (`~/.memql/bin/memql --version`). No commit: the log is scratchpad.

---

### Task 2: Rebuild the local cluster from the handoff head

**Files:**
- Modify (cluster state only): the k3d cluster `memql`'s seven `memql-<type>:local` images and Deployments. No repository file changes.

**Interfaces:**
- Consumes: `WT_HANDOFF` from Task 1.
- Produces: a local cluster running the handoff head; the OS at `https://os.memql.localhost` serving the handoff's bundle.

- [ ] **Step 1: Check headroom, and that no pipeline step or app session is running on this machine**

```bash
df -h / | tail -1
tail -n 5 ~/.memql/state/worker.log | grep -oE '"msg":"[^"]+"' | sort | uniq -c
```
Expected: hundreds of GB free; the last log lines are not a `worker tool dispatch START` without its `OK`. If a step is running, wait for its `OK` line.

- [ ] **Step 2: Build, import and restart from the worktree**

```bash
cd /home/znas/memql-projects/memql-handoff && time make dev 2>&1 | tail -25
```
Expected: one JSON envelope on stdout ending with the restarted Deployments and `"status":"ok"`; the human log on stderr names each of the seven node images built and imported. Budget: tens of minutes on a cold Docker cache. If `docker build` fails for a node, read the failing stage: the Dockerfile builds the identity CSS and the OS bundle itself, so a fresh worktree needs no generated assets.

- [ ] **Step 3: Verify the rollout and the mesh**

```bash
kubectl get pods -n memql --no-headers | awk '{print $1, $2, $3}'
cd /home/znas/memql-projects/memql-handoff && make status 2>&1 | tail -20
for i in bff agent edge; do docker image inspect memql-$i:local --format "$i {{.Created}}"; done
```
Expected: every pod `1/1 Running` (or `2/2` for the database); `make status` shows one `MEMQL_NODE_ID` per pod and one shared identity keyset; the three image timestamps are today.

- [ ] **Step 4: Verify the served OS is the handoff's bundle**

In Chrome (`claude-in-chrome`): open `https://os.memql.localhost`, open Ask. Expected: no header "Activity" control; hovering a message shows a top-right menu with Copy message, Copy details and Activity. That is #6287, present only on the handoff head. Record a capture path in the log.

- [ ] **Step 5: Verify the engine came up clean**

```bash
kubectl logs -n memql deploy/bff --since=10m 2>/dev/null | grep -E '"level":"(ERROR|FATAL)"' | head -5
kubectl logs -n memql deploy/agent --since=10m 2>/dev/null | grep -E '"level":"(ERROR|FATAL)"' | head -5
```
Expected: no lines. Any line is finding `F1`: the handoff head's boot on a database that ran 0.29/0.30 rows.

---

### Task 3: Rebuild and swap the cockpit

**Files:**
- Create: `/home/znas/memql-projects/memql-handoff/cockpit/bin/memql-computeruse-linux-amd64`
- Create (backup): `~/.memql/bin/memql-computeruse-linux-amd64.bak-0.20.0`
- Modify: `~/.memql/bin/memql-computeruse-linux-amd64` (the file the `~/.memql/bin/memql` symlink points at)

**Interfaces:**
- Consumes: the worktree.
- Produces: a running worker at cockpit 0.20.1 from the handoff head, registered with both homes; the registration row's `version` reflects it.

- [ ] **Step 1: Build the computer-use variant for this host**

```bash
cd /home/znas/memql-projects/memql-handoff && make -C cockpit cockpit-computeruse-host 2>&1 | tail -5 && \
./cockpit/bin/memql-computeruse-linux-amd64 --version
```
Expected: `memql 0.20.1+g639f808c1... (computeruse)`. The CGO build needs gcc and the X11 development libraries the cockpit Makefile names; the installed 0.20.0 binary was built on this host, so they are present. A missing-header failure names the library; stop and report rather than installing packages.

- [ ] **Step 2: Confirm nothing is mid-flight, then stop the worker and the menu**

```bash
tail -n 3 ~/.memql/state/worker.log | cut -c1-160
systemctl --user stop memql-worker.service memql-menu.service && systemctl --user is-active memql-worker.service memql-menu.service
```
Expected: last lines are registrations or `OK`s, not an open `START`; then `inactive` twice.

- [ ] **Step 3: Back up and swap**

```bash
cp -p ~/.memql/bin/memql-computeruse-linux-amd64 ~/.memql/bin/memql-computeruse-linux-amd64.bak-0.20.0 && \
cp /home/znas/memql-projects/memql-handoff/cockpit/bin/memql-computeruse-linux-amd64 ~/.memql/bin/memql-computeruse-linux-amd64 && \
~/.memql/bin/memql --version && ls -la ~/.memql/bin/
```
Expected: the version line shows `0.20.1`; the listing shows the symlink `memql -> .../memql-computeruse-linux-amd64`, the new file and the `.bak-0.20.0` copy.

- [ ] **Step 4: Start the services and watch the registration**

```bash
systemctl --user start memql-worker.service memql-menu.service && sleep 20 && \
systemctl --user is-active memql-worker.service memql-menu.service && \
tail -n 40 ~/.memql/state/worker.log | grep -E 'worker registered with cluster|connect failed|level":"(ERROR|WARN)"' | cut -c1-200
```
Expected: `active` twice; a `worker registered with cluster` line for `api.memql.localhost` and one for the cloud home; no `connect failed`. If the worker fails to start, restore: `systemctl --user stop memql-worker.service; cp ~/.memql/bin/memql-computeruse-linux-amd64.bak-0.20.0 ~/.memql/bin/memql-computeruse-linux-amd64; systemctl --user start memql-worker.service memql-menu.service`, then record finding `F2`.

- [ ] **Step 5: Verify the row the engine reads**

```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select m.id, m.payload->>'version', m.payload->>'lastSeenAt', m.payload->'labels'->>'app:claude-code', m.payload->'features'->>'apps' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:worker:registration:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'revokedAt' is null;"
```
Expected: one unrevoked row (the local home's registration of this machine) with `version` naming 0.20.1, `lastSeenAt` within the last minute, the `app:claude-code` label present with a version, and `features.apps` not `false`.

---

### Task 4: Pin Claude Code through the Fleet machine setting

**Files:**
- Modify (row only): `v1:worker:registration` of this machine, field `appRouting`, via the OS.
- Modify: `/home/znas/.claude/projects/-home-znas-memql-projects-memql/memory/ci-paused-handoff-branch-20261025.md` (add the registration id the pin lives on).

**Interfaces:**
- Produces: `appRouting.mode = "claude-code"` on the registration, which `component/memql/app_selection.go` reads on every app selection.

- [ ] **Step 1: Set the mode in the OS**

In Chrome: `https://os.memql.localhost`, open Fleet, Machines, this machine (named `pop-os`), the Apps tab. The App routing control offers Auto, Round robin, Claude Code only, Codex only. Choose **Claude Code only** and save. Record a capture path in the log.

- [ ] **Step 2: Verify the row**

```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select m.id, m.payload->'appRouting'->>'mode', m.payload->'appRouting'->>'revision', m.payload->'features'->>'apps', m.payload->>'lastSeenAt' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:worker:registration:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'revokedAt' is null;"
```
Expected: `claude-code` with a non-empty revision, `features.apps` not `false`, and `lastSeenAt` within the last minute (the pin is read only for an online machine whose apps feature is on). If the UI saved but the row reads `auto`, that is finding `F3` (the control and the row disagree); stop and report.

- [ ] **Step 3: Record where the pin lives**

Append to the memory note `ci-paused-handoff-branch-20261025.md` one line: `Pin applied <date>: appRouting.mode=claude-code on <registration id>; remove in Task 9.` No repository commit.

---

### Task 5: The smoke session

**Files:**
- Modify: the findings log.

**Interfaces:**
- Produces: a verdict, "an app session runs on this machine" or the exact refusal, before the real goal.

- [ ] **Step 1: Check the route picker reads ready**

In Chrome, in Ask, open the Route menu. Expected: "Apps" is selectable with the hint "Runs apps on your computer; unavailable with Workbench"; the destination is Auto. If Apps is disabled, read its reason line, record it, and wait for the machine to show online in Fleet before continuing (the worker re-registers within a minute of Task 3).

- [ ] **Step 2: Send the smoke message**

Route: Apps. Destination: Auto. Message, verbatim:

> Reply with exactly the word READY and nothing else. Use no tool.

Note the time.

- [ ] **Step 3: Watch the three places at once**

```bash
tail -f ~/.memql/state/worker.log | grep -E 'app session|workspace|sandbox|claude' | cut -c1-240
```
and, in a second call after two minutes:
```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select m.id, m.payload->>'status', left(m.payload->>'statement',60) from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:work:goal:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" order by m.\"createdAt\" desc limit 2;"
Q "select m.id, m.payload->>'status', m.payload->>'goalId' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:work:run:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" order by m.\"createdAt\" desc limit 2;"
Q "select m.id, m.payload->>'status', m.payload->>'appId', left(coalesce(m.payload->>'error',''),160) from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:worker:appSession:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" order by m.\"createdAt\" desc limit 2;"
```
Expected on success: the worker log shows `app session named no workspace; running in one this machine chose`, then `app session running at its level`, then `app session end`; Ask shows `READY`; the goal's run is `succeeded`.

- [ ] **Step 4: If it fails, read the refusal in the owner's seat first**

Record, as finding `F4`, what Ask shows, what Nexus shows on the run, and the worker log's wording, each verbatim. The cockpit's own troubleshooting table (`cockpit/docs/local-apps.md`, the rows for "workspace refused", "no workspace", "sandbox is unavailable", MCP "failed") says what each wording means. Then STOP. The owner decides what to do about this machine's service unit; the cockpit's documentation (`cockpit/docs/pipelines.md`, "The environment a step sees") calls that a decision to make deliberately, and this plan does not make it. Ask the owner with the evidence, and resume at Step 1 after their change.

- [ ] **Step 5: On success, record the baseline**

Append `F5` with: time from send to READY, the run id, the session id, the workspace directory the cockpit chose (from the log line), and the `model` and `effort` the session end reported (`Q` on the appSession row).

---

### Task 6: The run

**Files:**
- Modify: the findings log.
- Produces (on GitHub, by Claude Code, not by the session): one branch on `origin` from `origin/main` whose tip commit says `Closes #5792`.

**Interfaces:**
- Consumes: a green smoke session from Task 5.
- Produces: the branch name `FIX_BRANCH`, read from `git ls-remote`, used by Task 7.

- [ ] **Step 1: Send the owner's message**

Route: Apps. Destination: Auto. The message is the spec's section 6 paragraph, pasted verbatim:

> Fix memql#5792 in the engine repository. Editing the operator account's profile and setting its domain is refused with `domain_under_cluster_domain`, and the rejected value is the cluster's own domain rather than the submitted one; `validateAccountMemqlDomain` validates the merged `memqlDomain`, so an existing operator-account reservation value is incompatible with the client-domain guard. Expected: an owner can edit the operator organization's ordinary profile without the platform treating its cluster address as a new client-domain reservation. Keep the collision guard for client accounts. Repair incompatible legacy rows through the appropriate migration or normalization. Acceptance: reproduce it on this cluster first; a regression test that uses an existing stored operator row, not only a fresh seed, fails before the fix and passes after it; the checks for the changed boundary pass. Deliver on a branch from `origin/main` pushed to `origin`, with a commit whose message says `Closes #5792`. Do NOT open the pull request until I say CI/CD is ready. Report when the branch is pushed, with the test output and before-and-after captures of the Accounts profile edit.

Note the time and the goal id (newest `v1:work:goal` row).

- [ ] **Step 2: Watch, every few minutes, without steering**

```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
G='<goal id>'
Q "select m.id, m.payload->>'status', m.payload->>'currentStepKey' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:work:run:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'goalId'='$G';"
Q "select m.payload->>'key', m.payload->>'status', m.payload->>'kind', m.payload->>'childRunId' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:work:step:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'runId' in (select id from \"MemoryNodes\" where id like 'v1:work:run:%' and payload->>'goalId'='$G') order by m.\"createdAt\";"
Q "select m.id, m.payload->>'kind', m.payload->>'decision', left(m.payload->>'question',120) from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:work:approval:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'decision'='' order by m.\"createdAt\" desc limit 5;"
tail -n 20 ~/.memql/state/worker.log | grep -E 'app session' | cut -c1-200
```
Also open the run in Nexus. Expected: a `subrun` step with a `childRunId`; session chunks arriving; no pending approval, or one that names a real question.

- [ ] **Step 3: Answer approvals as the owner, in Nexus or Ask**

A `feedback` approval with a question: answer it with what the owner would say, and record the question and answer in the log. A `sideEffect` or `scopeElevation` approval: approve only what the goal statement already authorizes (reading the repository, running tests, pushing the branch); refuse anything outside it, and record it. Never answer a question with a code change.

- [ ] **Step 4: Log every observation as it happens**

One `F<n>` entry per stall over ten minutes, wrong refusal, repeated model call, unclear message, or control that was missing when needed. Capture the OS surface when the finding is visual.

- [ ] **Step 5: Watch for the parked states Review Focus names**

An `inferenceUnavailable` or `budget` approval means the session could not be served or ran out of ceiling. Record it, do not decide it alone: tell the owner what it says and what each option costs.

- [ ] **Step 6: Recognise the stop**

The run reports the branch pushed. Verify:
```bash
cd /home/znas/memql-projects/memql && git fetch origin --prune 2>/dev/null
for b in $(git ls-remote --heads origin | awk '{print $2}' | sed 's#refs/heads/##'); do t=$(git log -1 --format='%ct' origin/$b 2>/dev/null); [ -n "$t" ] && [ "$t" -gt "$(date -d '-6 hours' +%s)" ] && echo "$b  $(git log -1 --format='%h %s' origin/$b)"; done
```
Expected: exactly one new branch whose tip commit message contains `Closes #5792`, based on `origin/main`:
```bash
FIX_BRANCH='<name>'
git merge-base --is-ancestor origin/main origin/$FIX_BRANCH && echo "based on main"
git log --oneline origin/main..origin/$FIX_BRANCH
git diff --stat origin/main...origin/$FIX_BRANCH | tail -3
```

- [ ] **Step 7: Verify the pause was respected**

```bash
gh pr list --repo znasllc-io/memql --head "$FIX_BRANCH" --state all --json number,state --jq 'length'
gh api repos/znasllc-io/memql/commits/$(git rev-parse origin/$FIX_BRANCH)/check-runs --jq '.total_count'
```
Expected: `0` and `0`. If a PR exists, record `F<n>` immediately and tell the owner; do not close it or cancel anything on the cloud yourself.

- [ ] **Step 8: Verify the evidence the goal left**

In Nexus, open the goal: the report names the test that fails before and passes after, the checks run, and the captures. The goal stays `open`, unaccepted. Record the report's location (Library file ids) in the log.

---

### Task 7: After the owner lifts the pause

**Files:**
- Produces (on GitHub, by Claude Code): the pull request; (by the owner) the merge.

**Interfaces:**
- Consumes: `FIX_BRANCH` and the goal id from Task 6; the owner's words "CI/CD is ready".

- [ ] **Step 1: Resume the goal as the owner**

In the same Ask conversation, route Apps:

> CI/CD is ready. Open the pull request for the pushed branch against main, with "Closes #5792" in its body, and report its number.

- [ ] **Step 2: Verify the PR**

```bash
gh pr list --repo znasllc-io/memql --head "$FIX_BRANCH" --json number,author,baseRefName,body --jq '.[] | "\(.number) \(.author.login) -> \(.baseRefName)\n\(.body[:300])"'
```
Expected: one PR by `znas-io` against `main`, body containing `Closes #5792`. Record `PR=<number>`.

- [ ] **Step 3: Watch the native check**

```bash
gh api repos/znasllc-io/memql/commits/$(git rev-parse origin/$FIX_BRANCH)/check-runs --jq '.check_runs[] | "\(.name) \(.status) \(.conclusion // "-") \(.details_url)"'
```
Expected: `MemQL / memql` moves from `queued` to `in_progress` to `completed success`. A run that stays `queued` is operator run admission on the cloud installation; tell the owner, who owns that file.

- [ ] **Step 4: A red lane goes back into the goal**

Read the failed step's name from the check summary. In Ask, same conversation:

> The native check failed on step `<name>`: <its last lines>. Repair it on the same branch and report when pushed.

Then repeat Step 3 for the new head. Record each lane as a finding if the cause was the harness rather than the fix.

- [ ] **Step 5: The owner merges**

The owner runs, from the shared tree:
```bash
scripts/dev/merge-as-owner.sh --pr=$PR --check
scripts/dev/merge-as-owner.sh --pr=$PR
```
Expected: `--check` lists the ruleset's required check `MemQL / memql (App 4822538)` as success and a `base` drift of zero; the merge lands. The session does not run these.

- [ ] **Step 6: Verify the outcome**

```bash
gh pr view $PR --repo znasllc-io/memql --json state,mergedAt,mergeCommit --jq '"\(.state) \(.mergedAt) \(.mergeCommit.oid[:9])"'
gh issue view 5792 --repo znasllc-io/memql --json state --jq .state
cd /home/znas/memql-projects/memql && git fetch origin && git merge-base --is-ancestor origin/$FIX_BRANCH origin/main && echo "on main"
```
Expected: `MERGED <time> <sha>`, `CLOSED`, `on main`.

- [ ] **Step 7: The owner accepts the goal**

In Nexus, the owner accepts the goal. Verify:
```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select payload->>'status', payload->'acceptance'->>'acceptedBy', payload->'acceptance'->>'acceptedAt' from \"MemoryNodes\" where id='<goal id>' order by \"createdAt\" desc limit 1;"
```
Expected: `closed`, the owner's user id, a timestamp.

- [ ] **Step 8: Open the docs PR**

Only now, the docs branch carrying the spec and this plan gets its PR:
```bash
cd /tmp/claude-1000/-home-znas-memql-projects-memql/61d53756-0916-4733-ab28-869f7baff0a7/scratchpad/wt-spec && git log --oneline origin/main..HEAD
gh pr create --repo znasllc-io/memql --base main --head docs/claude-code-owner-run-5792-design --title "docs: design record and plan for the owner's run with Claude Code" --body-file - <<'BODY'
The 2026-10-25 brainstorm's design record and its implementation plan: repeating the owner's app-routed goal run with Claude Code as the only app on the local cluster, on memql#5792, through native CI to main.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
BODY
```

---

### Task 8: The audit becomes issues

**Files:**
- Create (GitHub): one issue per findings-log entry worth acting on, label `claude`; one comment on memql#5799.

**Interfaces:**
- Consumes: the findings log.

- [ ] **Step 1: Group the log**

Read every `F<n>`; merge entries with one cause; drop cosmetic ones into a single "polish" entry. Expected inputs already known at planning time:
- the governed proposal path is unused for an owner-maintainer run because no resolver ships with the engine (should the engine ship a default route for the owner's own repositories?);
- memql#5817 is fixed in code (the child goal stamps `agent`) and still open: it needs its db-gated test and a close;
- the local engine images and cockpit lagged the tree by a week with nothing in the OS saying so;
- whatever Task 5 recorded about this machine's service confinement and app sessions on Linux, if anything.

- [ ] **Step 2: File each issue**

```bash
gh issue create --repo znasllc-io/memql --label claude --title "<one sentence>" --body-file /tmp/claude-1000/-home-znas-memql-projects-memql/61d53756-0916-4733-ab28-869f7baff0a7/scratchpad/issue-F<n>.md
```
Each body has the four parts: what happened, where (ids and surfaces), evidence (verbatim text, no secrets), proposed fix. Expected: an issue URL per call. Record the numbers in the log.

- [ ] **Step 3: One progress comment on #5799**

```bash
gh issue comment 5799 --repo znasllc-io/memql --body-file /tmp/claude-1000/-home-znas-memql-projects-memql/61d53756-0916-4733-ab28-869f7baff0a7/scratchpad/comment-5799.md
```
The comment: the run in three sentences (the goal, the branch, the merge), the list of issues filed, and what stayed out of scope. Do not close #5799.

---

### Task 9: Cleanup

**Files:**
- Modify (row only): the registration's `appRouting` back to Auto.
- Delete: `/home/znas/.claude/projects/-home-znas-memql-projects-memql/memory/ci-paused-handoff-branch-20261025.md`'s pin line (the note itself stays while CI is paused; delete the whole note once the owner lifts the pause and #6290 is settled) and the index line when the note goes.

- [ ] **Step 1: Remove the pin in the OS**

Fleet, this machine, Apps tab, App routing: **Auto**, save. Verify:
```bash
Q() { kubectl exec -n memql memql-db-1 -c postgres -- psql -U postgres -d memql -At -F $'\t' -c "$1" 2>&1; }
Q "select m.payload->'appRouting'->>'mode' from \"MemoryNodes\" m join (select id, max(\"createdAt\") mx from \"MemoryNodes\" where id like 'v1:worker:registration:%' group by id) l on l.id=m.id and l.mx=m.\"createdAt\" where m.payload->>'revokedAt' is null;"
```
Expected: `auto`.

- [ ] **Step 2: Update the memory**

Remove the pin line from the memory note. If the pause has lifted and #6290 is merged or closed, delete the note and its line in `MEMORY.md`.

- [ ] **Step 3: Leave the machine as agreed**

Keep `~/.memql/bin/memql-computeruse-linux-amd64.bak-0.20.0` until one full session has run on the new binary (Task 6 counts). Keep the handoff worktree while the handoff head runs locally. Report the final state to the owner in one message: what merged, what was filed, what is pinned (nothing), what remains.
