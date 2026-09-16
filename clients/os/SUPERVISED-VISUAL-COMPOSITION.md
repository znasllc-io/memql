# Supervised Visual Composition

Owner-approved design direction, 2026-09-15. Fleet is the first implementation.
This complements [the OS interface language](DESIGN.md); it does not authorize a
redesign of every app. The stable contract is the behavior below, not a particular
prototype's sample content or pixel positions.

## Presentation and composition

Keep the interface clean while preserving **full functionality**. Write for
moderately technical users: short labels, clear consequences, and accessible info
details for supporting explanations. Simplification changes placement and wording;
it must not delete less common operations or conceal material effects.

Make important objects and their capabilities tangible. A machine can anchor its
models, apps and work; a policy can expose ordered sources. Show relationships and
compatible choices where they help a decision. The inspiration is an equipment
editor, not a game, puzzle, decorative science-fiction dashboard or graph for every
kind of information. Empty states and incomplete reports deserve the same care as
populated examples. Never turn missing measurements into invented facts.

Use guided workflows for setup and creation, then compositional workspaces for
ongoing configuration. Progress reflects the actual lifecycle. Consolidate
fragmented navigation around the user's entity or task; retain separate
destinations where scope or complexity justifies them. Drilldowns need a clear
return. Reuse layout and control patterns while allowing real differences between
apps.

## Human control and supervision

Mouse and keyboard must reach every action. Drag and drop, when useful, needs
non-drag equivalents. Typed Ask and optional voice should operate through the same
semantic actions, authorization, validation and state as manual controls.

Supervision makes actual activity visible without taking over:

- Show subtle theme-accent attention or action outlines only from real events.
- Distinguish proposed, running, completed and error states with text or icons;
  keep them distinct from selection and keyboard focus.
- Respect reduced motion and accessible navigation. Avoid artificial cursors,
  fake cognition and perpetual pulses.
- Preserve user drafts and focus. Do not navigate unexpectedly or silently
  overwrite an edit. Let the person inspect, correct, intervene and retry.
- Describe a proposal as a proposal. Success appears only after the underlying
  operation succeeds; a refusal keeps useful context and explains the remedy.

Fleet currently provides semantic activity targets and an actual Ask policy
proposal review. General supervised UI automation and voice orchestration remain
future work, not capabilities implied by an animated prototype.

## Policies and rules

A policy chooses a preferred compatible source/model and ordered fallbacks. A rule
matches work to a policy. Show their scopes honestly: a cluster-wide policy is not
an assignment to the selected machine, and an eligible app is not every installed
app. Configuring a source does not guarantee availability or grant permissions.

Show shipped policies, permit supported customization and new policies, and restore
customized shipped policies from immutable original defaults. Manual edits and Ask
proposals share durable configuration and conflict detection. Preserve engine
invariants, including the active embedding-space binding, source compatibility,
locked rule priority and explicit spending/permission consequences.

## Related patterns and rollout boundaries

Settings and per-app Logs should form a reusable family. Suitable true on/off
settings use switches; multi-selection is a different control problem. A slim
per-app log surface should provide useful search, filtering, concise events,
details, and a contextual handoff to the full Logs app when needed.

The owner approved the complete Fleet redesign and shared window chrome on
2026-09-15, including Settings/Logs, and separately approved Deployables.
The current shared header uses the real app registry selector and groups controls
as Ask/Search, Settings/Logs, and Minimize/Maximize/Close. Search finds the current
app's destinations; app switching focuses existing windows and preserves drafts.
Maximize moves the same window to a dedicated MemQL desktop. Restore returns to
its previous slot when available; if that desktop filled, it restores in place.

Back follows navigation origin: explicit tabs select clean peer destinations;
content links retain a contextual return path, including cross-tab links. Retained
panes preserve selection, drafts and scroll. Home is Fleet's main destination;
cluster workspaces keep their technical name. Local rollout is authorized; this
approval does not authorize public releases or cloud deployment.

## Acceptance

Judge actual rendered interfaces at useful desktop and narrow sizes, in light and
dark, and with empty and populated state. Verify keyboard/focus, real data freshness,
all prior action entry points and end-to-end behavior. State what was tested through
real services versus fixtures. Do not mint credentials, pair machines or alter owner
data merely to populate a review screen.

Prototype entities and simulated actions are illustrative, never production facts.
The approved [Fleet concept](../../work/visualizations/fleet-composition-preview.html)
is one example of the direction. Implementation details and the action-reachability
map are recorded in [Fleet Visual Composition](../../docs/internal/design/2026-09-15-fleet-visual-composition.md).
