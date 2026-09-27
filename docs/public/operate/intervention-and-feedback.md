---
title: Intervention, feedback and reusable decomposition
audience: public
status: stable
area: operate
sinceVersion: 0.23.6
owner: znas
---

# Intervention, feedback and reusable decomposition

After a goal has run, every step it took is visible in Nexus, and a person can
step into any of them: run it again with a different intelligence or prompt, go
back to an earlier answer, branch into something different, and say what was
wrong in words the system acts on. Long work is cut into sections that ask the
catalog before any model is used, and every automation is labelled by evidence
as reusable, specific to one goal, or specific to one account.

Design record: [the app-session recording and learning program](../../superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md),
decisions D18 to D24 (epic memql#5414).

## Versions and the head

Every version of a step is kept. A step run again is a **new version** of the
same step; the previous one stays readable, and "going back" never deletes
anything. The run carries a **head** (`v1:work:run.head`) that names the current
version of every step.

| Act | What happens |
|---|---|
| **Run again** (`rerunStep`) | The step runs as version N+1 with what you changed, on that version only. Every step after it runs again from the new answer, each as a new version. The steps before it are reused, not run again. |
| **Make current** (`moveRunHead`) | An earlier version becomes current, together with the upstream it was computed from. Every later step that already has a version computed from that same upstream is restored without running; only the first step with no such version, and everything after it, runs again. Going back to a world that still exists runs nothing. |
| **Branch from here** (`branchRun`) | A new run (mode `fork`) that reuses the steps before the branch point by reference -- they are not executed again -- and runs the branch step live with what you changed, then everything after it. The original run is untouched. |

What you can change for one version (the override, `v1:work:step.override`):

- **Level** -- `fast`, `strong` or `reasoning`. Embeddings is never offered: a
  different embedder answers in a different vector space.
- **Model** -- a policy entry to pin instead of routing by level: a provider
  name, `fleet:<modelId>`, `app:<id>` or `app:<id>:<model>`.
- **Effort** -- `low`, `medium`, `high`, `xhigh` or `max`, for an app. The
  version records the model and effort the app REPORTED, which is the truth of
  what ran.
- **Prompt** -- for a step an app session answered, the whole prompt that
  session ran with, as you edit it; for any other step, instructions added to
  its own prompt. Which one it is follows the version you replace, wherever the
  new version is served, so an app given a step a model answered still gets the
  step's own prompt with your instructions after it.
- **Inputs** -- arguments to use instead of the step's own, by name.

A version whose prompt or inputs a person wrote records them in
`v1:work:step.authoredBy`, and is never treated as a recording of the app.

A run that is still working is never re-run under itself: the acts are offered
only once a run has stopped.

### Session steps

When the step was answered by an app session (Claude Code or Codex on one of
your machines), running it again or branching from it starts a **new session**
against the workspace as it was **before that step**. The workspace is rebuilt
from the contents the earlier sessions' recordings hold, content-addressed in
the Library, into a fresh directory. If a file's content was not recorded -- it
was above the per-file cap, or the Library write failed -- the act is refused
and names the file, because a branch from a partial workspace would diverge
without saying so. Shell commands whose file effects no recording reported are
counted and shown, since the rebuilt workspace cannot contain what they
changed.

### Steps a learned procedure served

A [learned procedure](learned-procedures.md) replays exactly what it learned,
so running its step again with a different level, model, effort, instructions
or a dislike's reason would change nothing. Such a re-run goes to the **app**
instead, the way a replay that could not finish does: the app is told the
procedure answered this goal before and why it has it now, with your
instructions, what was wrong with the previous version, and your earlier
dislikes on the goal. The app and level you chose decide which session opens;
a model that is not an app is refused, because the goal goes to an app or
nowhere. Nothing on the procedure's ladder moves, since it was not tried.
**Run again** with nothing changed still replays the procedure.

## Feedback

A verdict on a run, or on one version of one step, is a `v1:work:observation`
of kind `feedback`: **like**, **dislike** or **neutral** (absent means nobody
looked). A dislike asks one question before it is saved -- what was wrong, on
the AI Fluency framework's three Discernment axes:

| Axis | Means |
|---|---|
| Product | What it produced was wrong or incomplete. |
| Process | How it went about the work was wrong. |
| Performance | How it behaved -- its tone, pace or instruction-following -- was wrong. |

A dislike naming no axis is refused. A reason is optional and encouraged. A
later verdict is a new row; nothing is ever rewritten.

### The answer validator

When the cluster's `validateAnswers` value is on, one bounded model call at the
run's own level checks a finished goal run's answer against what the step was
asked for, on the same three axes, before a person looks. It records a
`decision` observation and the run's `validation` summary, shown beside your
verdict. It is a **pre-filter, never a certifier**: it never counts as a like,
never moves a learned procedure's ladder, and your verdict outranks it. When the
two disagree, the disagreement is kept on your feedback row as the signal that
the validator needs work. It runs only for goals asked through Nexus or the
API, only when the run reached a model, and at most once per answer version.

### What feedback changes

Feedback changes what the system does next in four places, and nowhere else:

1. **Certification.** A learned procedure whose recordings contain a disliked
   step version stays a candidate until a liked or neutral version of that step
   exists ([learned procedures](learned-procedures.md)).
2. **Repair.** When a disliked step is run again, branched, or handed back to
   the app, its axes and reason ride along as guidance to the model or the app.
3. **Learning.** A recording whose step version was disliked, or was replaced
   by a later version, leaves the learning corpus; a liked one ranks higher
   without changing a procedure's version.
4. **Description.** Dislike reasons accumulate on the goal's shape and are
   given to a model the next time one is genuinely used for that goal --
   including the app a learned procedure hands the goal back to -- and never
   to a replay, which reads rows and cannot act on text.

## Decomposition and reuse

When a goal is cut into sections, each section declares its inputs, its
outputs, a one-line purpose and a reuse intent, and each asks the catalog
before any model is used:

1. an exact hit on the section's own signature (its purpose and inputs);
2. a close match among **reusable** automations only, compared by their words
   (this costs no model);
3. only then, intelligence.

A section must end on a checkable state: a section that changes something and
declares no postcondition **ends mid-effect** and is refused, and the goal is
compiled whole instead.

A section worked out live is written, where it can be, as an **automation of
its own**. When the run succeeds, each such section is catalogued, so the next
goal with the same section finds it as an exact hit and spends no model on it.
Nothing is catalogued from a replay, from a section whose answer was disliked
or did not finish, or from a section a person re-ran with changes, since the
automation would not carry those changes.

Every automation carries a reuse label (`v1:authoring:construct.reuse`):

| Label | Evidence |
|---|---|
| Reusable | At least `reusableAfterSignatures` distinct goal shapes of yours used it. |
| For one account | Every use was tied to one account. |
| For one goal | Anything else. |

A sweep decides the label from evidence every six hours. You can set your own
label; your label is a **version** (`reuseOverride.version`) and the evidence
keeps counting underneath it. Choose **Automatic** to hand the label back to
the evidence.

## Where to see it

- **Nexus > Runs**, a finished run: every step's versions, with **Run again**,
  **Branch from here** and **Make current** on the version you are looking at,
  and your verdict beside the validator's.
- **Nexus > Automations**: each automation's reuse label on its row, a **Reuse**
  question under Refine, and on an automation's detail -- or a learned
  procedure's page -- the **Reuse** panel, where you set your own label and see
  what its use says.
- **Nexus > Overview**: your automations counted as reusable against
  goal-specific.

## Values

Both values live on the seeded singleton `v1:work:feedbackPolicy:primary`,
refreshed on every boot, and readable by every signed-in person:

| Value | Seeded | Meaning |
|---|---|---|
| `validateAnswers` | `true` | Whether the answer validator runs. |
| `reusableAfterSignatures` | `2` | Distinct goal shapes that make an automation reusable. |

## Current limits

- **Effort needs a cockpit that honours it.** The engine sends the effort you
  chose; until the cockpit applies it, the version shows what the app reported.
- **A strict replay of a branch diverges at its prefix**, because the branch
  never made the calls its prefix's source did.
- **A branch's shared prefix is not among its own versions.** It lives in the
  run the branch came from, and is read there.
