---
title: Proof of work
description: Trace a MemQL capability claim to a real goal, the system that performed it, and verifiable results.
audience: public
status: stable
area: overview
sinceVersion: 0.28.0
owner: znas
---

# Proof of work

A capability claim needs a worked example that a reader can inspect. A proof
of work starts with a real request and follows it through to the resulting
artifact, independent checks and any human help. It identifies the complete
MemQL setup, including the harness, skills and models used for each role.

## Case library

| Skill area | Starting point | Evidence status |
|---|---|---|
| Software development | [App-session child-goal defect, issue 5817](https://github.com/znasllc-io/memql/issues/5817) | In progress; not a completed proof of work |
| Document creation | Coming soon | No published case |
| Research | Coming soon | No published case |

There is no completed live case published on this page yet. The software case
will qualify when its regression, resulting change, checks and delivery are
verified and linked. An edited file or an open issue alone does not establish
that the requested job was completed.

## What a case must show

| Part | Evidence |
|---|---|
| Request | Original issue or goal, starting revision, acceptance criteria and authorized scope. |
| System | MemQL engine and harness revisions; worker version; configured model roles, routing rules and fallback order. |
| Skills | Skill area exercised; exact selected skill and lesson versions, content digests and selection receipts when captured. A skill inferred from the task is labelled as an area, not a verified binding. |
| Work | A concise sequence of actions with source receipts: investigation, changes, test runs, review responses and delivery. Attempts and confirmed effects remain distinct. |
| Result | Artifact or PR, exact resulting commit, independent check output and the verdict for each acceptance criterion. |
| Human help | Approvals, corrections, coaching, supplied context and any implementation performed outside MemQL. |
| Model use | Actual model-call receipts by role, including fallbacks. A configured model is not evidence that it was called. Unattributed calls remain unattributed. |
| Measurements | Elapsed time, calls, tokens and cost, each labelled measured, estimated or unavailable with its source. |
| Limits | Failed or unfinished criteria, remaining blockers, missing evidence and the circumstances in which the result was obtained. |

A finished case leads with a bounded statement such as “MemQL completed this
issue under this setup, with these interventions,” followed by the evidence.
An assisted completion remains useful proof; it is presented as assisted.
Approval is recorded separately from coaching or a human-authored patch so
readers can understand the kind of assistance involved.

An issue-to-PR case needs the behavioral regression failing against the
starting tree and passing against the proposed fix. A merge claim additionally
needs review, checks and approval for the revision that actually merged.
Screenshots and a recording can illustrate the work; the checkable artifacts
carry the claim. Private source content, credentials and unrelated work are
excluded from the published evidence.

## Examples and repeatable evaluations

A case establishes what happened in one observed task. A repeatable evaluation
uses a versioned goal, fixture and acceptance contract across named MemQL
setups, including unsuccessful runs and the number of attempts. It supports
comparison when those contracts and measurement methods match. One selected
success cannot establish a general success rate or that a particular skill
caused an improvement; the latter needs a controlled comparison.

The [proving scorecard](proving-scorecard.md) identifies its replay and live
tiers explicitly. Replayed checks are evidence about the harness properties
they test; they are not live model completions. Proof-of-work cases and goal
evaluations retain that same distinction.

These case records are documentation backed by existing work receipts and
artifacts. Publishing a case does not grant tools, approve future actions,
promote a private lesson or change a model's weights.

## Reading results across setups

Each results row identifies a complete MemQL setup and a goal area. Expanding
the setup shows its model assignments by role: reasoning, implementation,
review, retrieval or any other role that the run actually declares. One model
may serve several roles; several models may contribute to one goal. The case
records configured routes separately from the models observed in call receipts.

Use the same setup identifier in the result and its worked example, so a
reader can move from an aggregate result to the individual issues, changes
and checks supporting it. Software development is the initial area; other
skill areas remain Coming soon until their own cases and evaluations exist.
