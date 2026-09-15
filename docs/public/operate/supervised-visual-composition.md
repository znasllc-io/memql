---
title: Supervised Visual Composition
audience: public
status: draft
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Supervised Visual Composition

**Supervised Visual Composition** is the approved design direction for MemQL OS:
a clean workspace where you compose objects, their capabilities, and their
relationships directly, or review MemQL working with the same visible state.
You can inspect, correct, and intervene throughout the work.

A machine and its models belong together. A routing policy should make its
ordered sources visible. Creation can use a short guided wizard; ongoing work
belongs in a coherent workspace with contextual navigation and a clear way back.
Simpler presentation must preserve the available operations and their effects.

## How control works

Mouse and keyboard are first-class. Where dragging helps, an equivalent control
must also work without dragging. Typed Ask and optional voice are intended to
use the same semantic actions, validation, authorization, and state as manual
controls. A proposal remains a proposal until the relevant operation succeeds.

Subtle activity highlights should follow actual events. Proposed, running,
completed, and failed activity needs a text or icon cue, distinct from selection
and keyboard focus. Preserve drafts and focus, respect reduced motion, and avoid
unexpected navigation. A visual animation is not evidence that MemQL is acting.

Reusable controls and layouts should form a family across apps while preserving
each app's needs. This is not a game, and it does not call for decorative graphs,
fake autonomous behavior, clutter, or fewer capabilities.

## Availability and rollout

This is a design contract, not a claim that every OS app already works this way.
Status recorded **2026-09-15**:

| Scope | Status |
|---|---|
| Existing OS apps | Available according to their current engine APIs, app grants, and configuration. Manual controls remain the baseline. |
| Fleet composition | Available: visual composition, persistent routing-policy editing, shared activity targets, and typed Ask policy proposal review. Proposal generation requires configured compatible inference. |
| New Settings and per-app Logs layouts | Proposed; awaiting prototype approval. Existing Settings and Logs remain the current surfaces. |
| Deployables composition redesign | Implemented and verified on the local deployment. Availability on other clusters depends on their deployed revision. |
| General autonomous UI driving and voice orchestration | Future work. Activity hooks and a policy proposal do not establish this broader capability. |

When evaluating a preview, distinguish real service activity from labeled
fixtures. Use the status table above to separate design intent from availability
on your cluster.

[MemQL OS guide](memql-os.md) · [AI routing](ai-routing.md) ·
[Status and direction](../overview/roadmap.md).
