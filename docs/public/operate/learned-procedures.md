---
title: Learned procedures
audience: public
status: stable
area: operate
sinceVersion: 0.23.6
owner: znas
---

# Learned procedures

When a coding app on one of your machines does the same kind of work again
and again, MemQL learns the procedure from the recordings and, once it has
earned it, runs that procedure itself -- with no model and no app. This page
explains how a procedure is learned, how it earns trust, what you approve,
and what happens when the world changes under it.

Epics [memql#5402](https://github.com/znasllc-io/memql/issues/5402) (learning)
and [memql#5408](https://github.com/znasllc-io/memql/issues/5408)
(certification and replay); design record
`docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`.

## From recordings to a procedure

Every action an app takes in a delegated session -- a command, a file it wrote
or read, a page it fetched, a MemQL tool it called -- is recorded as a step of
the work spine ([local apps](local-apps.md)). When a recorded session
succeeds, MemQL reads your recordings of the same goal (same statement, same
input names) and looks for the steps they share.

- **Two recordings is the floor.** One recording is a record, not a pattern.
- **No model is spent learning.** The steps are generalized by algorithms over
  the recordings: what every recording did the same way stays literal, a value
  one step took from an earlier step's result becomes a reference, and what
  varied becomes a parameter -- named after the goal input it came from when
  every recording agrees.
- **Paths are relative to the workspace.** Each session ran in its own
  workspace, so the workspace's own path is not part of the procedure.
- **Only replayable steps count.** A step no executor can run again (a nested
  automation, an app's final prose answer) keeps the procedure a candidate.

The result is a construct in your catalog: the steps as MemQL source you can
read, the generalized template a replay executes, the environment facts that
held at every recorded start (tool versions, platform, an empty workspace),
and where it came from -- the app, model and effort it was recorded from, and
the recordings themselves. Learning never activates anything.

## The certification ladder

A learned procedure climbs four rungs. It asks you exactly once.

| Rung | What serves your goal | How it moves on |
|---|---|---|
| **Candidate** | The app | Two uses, every step replayable, and no step you disliked without a later version you liked |
| **Shadow** | The app; the procedure replays beside it in a sandbox and every step is compared | After enough consecutive matches across enough different inputs, MemQL asks you to promote it |
| **Canary** | The procedure, for real, with the app standing by | Enough clean replays in a row, and it is trusted -- nobody is asked again |
| **Trusted** | The procedure, with no model | It stays trusted while its replays keep succeeding |

A procedure that no goal has used for the retirement window is **retired**:
nothing serves it or replays it again. A procedure that changes -- a later
recording generalizes differently -- goes back to the first rungs: what you
approved was one version, and an approval never carries over to another.

### The numbers are the cluster's policy

The thresholds are values in one row, `v1:authoring:ladderPolicy:primary`,
seeded with these defaults and refreshed on every boot. MemQL OS shows them
under **Settings > Procedures**.

| Value | Default | Meaning |
|---|---|---|
| Shadow matches | 5 | consecutive matches beside the app before promotion is proposed |
| Distinct bindings | 2 | different values each parameter must have taken across those matches |
| Canary matches | 5 | consecutive clean canary replays before a procedure is trusted |
| Failures to demote | 2 | failed replays in a row that send a canary or trusted procedure back to shadow |
| Insufficient preconditions to demote | 1 | replays that diverged although every learned precondition held |
| Retire after | 30 days | unused this long, a procedure retires |

### What a match is

Shadow and canary replays are compared step by step on what any executor can
report the same way: whether the step failed, its exit code, the type of its
output, and the exact bytes of every file it wrote or read. Where every
recording agreed on a value, the replay must reproduce it exactly; where the
recordings themselves varied, only the type must agree. The app's own wording
of a tool's output is never compared -- two executors never phrase it alike.

A procedure that only touches its workspace, the network and MemQL's own tools
replays in the **workbench**, a sandbox in your cluster. One that touches files
or apps on your own machine replays **on that machine** through the Cockpit;
in shadow it is compared without running (the steps it would have taken
against the steps the app took), so a shadow replay never acts on your machine.

In the workbench a shadow replay runs its commands for real, inside the
sandbox -- except a command that sends something out of it: a `curl` or `wget`
that posts or uploads, `git push`, a package publish, `ssh`, `scp`, `rsync`,
and the cloud and cluster CLIs (`gh`, `kubectl`, `az`, `aws`, `gcloud`). Those
are compared without running, so the app's webhook call or push is not made a
second time beside it. A replay reads MemQL through the same read-only surface
the app's session had.

### Parameters are values, never code

A parameter a goal supplies is always passed to a command as one literal word,
quoted, whatever it contains: `x;rm -rf ~` is a file name, not a second
command. A parameter only takes values of the kind the recordings showed it --
a value that starts with `-`, `/` or `~`, or climbs out with `..`, is refused
unless a recording's did too -- and a path segment is exactly one segment.
Everything else in a replayed command is the recorded command, byte for byte.

A procedure stays a **candidate** when a parameter would be code rather than a
value: the script of `sh -c` or `bash -lc`, inline code for `python -c` or
`node -e`, the argument of `eval`, or a line inside a heredoc. So does one
whose recorded value came through a shell expansion (`"$HOME/..."`), and one
with a parameter no goal input supplies -- a replay could not choose it.

## The one approval

When a procedure has matched the app enough times, a **Promotion** approval
appears in Nexus > Approvals. It names the procedure, its version (the digest
of its source, template and preconditions), the evidence, and what it was
recorded from. The question says where it would run -- in the workbench, or
on your machine -- and, for one that touches your machine, that its matches
compared the commands it would run with the app's own, because nothing ran.
Approving lets it run for real with the app standing by; rejecting keeps it in
shadow, and it must earn the proposal again. If the procedure changed after the
approval was raised, approving is refused: approve the version it is now.

## When the world changes

Before a canary or trusted replay starts, the procedure's learned
preconditions are measured on the target. A mismatch -- a different tool
version, a workspace that is not empty -- means the procedure does not run:
your goal goes to the app, and the mismatch is recorded on the replay run.

During a replay every step is checked as it finishes. On the first step that
does not match, the replay **stops**. The steps that already ran are listed,
with their idempotency keys, in the guidance handed to the app -- which
finishes the goal knowing what not to redo -- and the app's session is
recorded as a new recording the learner reads next time. A replay interrupted
mid-step (its node restarted) never runs that step again if it could have
reached your machine or the outside world: the app is told the step may have
run.

Two failed replays in a row, or one divergence while every precondition held,
send a trusted or canary procedure back to shadow without asking. A start
refused because a precondition did not hold counts as a failed replay, so a
procedure whose preconditions keep failing is demoted rather than retried. Two
sweeps run in the background: one re-applies the demotion rules to stored
evidence under the current values, one retires what is unused.

A target that is not there says nothing about the procedure. A machine that is
asleep or offline, a dropped connection, or a workbench being restarted sends
your goal to the app, and the ladder does not count it. A step that times out
is a failure; each step runs as long as the app's own call was allowed to.

## Where to see it

- **Nexus > Automations** lists learned procedures beside authored
  automations, each with its rung. A procedure's page shows the ladder, the
  evidence, what it was recorded from, its preconditions and its steps.
- **Nexus > Approvals** holds the Promotion approval.
- **Settings > Procedures** shows the ladder's values.

## What the proving suite measures

Two figures carry the headline claims, each with a control that must read the
opposite way:

- `amortizedCost.replaysServedWithoutModel` -- goals a trusted procedure
  answered with no model and no app call. Its control: a procedure left in
  shadow serves none.
- `durability.duplicatedSideEffectsAcrossDivergence` -- side effects delivered
  twice when a replay diverged and the app took over. It must read zero; its
  control shows the same divergence without the guidance does duplicate.

See the [proving scorecard](../overview/proving-scorecard.md).

## Current limits

- **Files a command creates in the workbench are not kept.** A file the
  procedure writes directly is saved to your Library, as the app's writes are;
  a file a command produces (`python gen.py > out.csv`) stays in the replay's
  sandbox, which is removed when the replay ends.
- **A script can still reach the network in shadow.** Only the commands named
  above are compared without running; a script the procedure runs
  (`python3 sync.py`) runs inside the sandbox with its network.
- **The app's takeover needs the machine on the same agent node.** When a
  replay diverges and hands the goal back, the app session starts on the
  machine only if its connection is held by the node running the replay; with
  two agent replicas that is about half the time, and otherwise the goal fails
  with the reason recorded.
- **Replaying a goal run that a procedure served runs the procedure again**,
  for real, rather than reading back what it did.
- **A step compared without running produces nothing.** In shadow, a later
  step that reads a file such a step would have written cannot match, so a
  procedure that posts with `curl -X POST ... -o reply.json` and then reads the
  reply does not climb.
- **An owner with several machines** may see one replay's steps routed to
  different machines.
