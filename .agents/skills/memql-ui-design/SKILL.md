---
name: memql-ui-design
description: Design, implement and review MemQL interfaces using the owner's minimal UI/UX rules. Use for OS apps, shared chrome, identity and setup flows, or human-facing installer and terminal output.
---

# MemQL UI/UX

Use the established MemQL visual language: simple, minimal and fully functional.
This skill is a repository-owned entry point, not another design system.

## Load the relevant rules

- Read [the interface language](../../../clients/os/DESIGN.md), including
  Applying them and the section for the surface being changed. It owns layout,
  controls, copy, help, entry transitions, wizard actions and terminal presentation.
- For graphical interfaces, read [Supervised Visual Composition](../../../clients/os/SUPERVISED-VISUAL-COMPOSITION.md)
  for object relationships, supervision, action reachability and scope. Historical
  prototypes illustrate the direction; their sample data and old controls are
  not requirements.
- For OS implementation, read [the OS README](../../../clients/os/README.md)
  for live collections, navigation, readiness and attention behavior.
- If `frontend-design` is available in the session, read its catalog-provided
  `SKILL.md`. Use its plan/build/critique process within the MemQL brief; do not
  replace the existing palette, typography or kit with a generic new aesthetic.
  If unavailable, say so briefly and continue using these repository rules.

## Apply and verify

State a short plan for the user's task: the relevant existing pattern, control
placement, legal actions and meaningful states. Use the requested behavior and
conversation context; do not ask the owner to repeat settled preferences.

Build with the existing brand tokens and shared components. Put a newly shared
interaction in the kit, and update its authoritative rule instead of copying
instructions into individual apps. Simplification must retain capabilities,
accessible names, keyboard/focus behavior and material consequences.

Critique the rendered result at desktop and narrow sizes in light and dark.
Check relevant empty, pending, failure, retry and completion states, reduced
motion, and actual click targets. Test behavior and lifecycle edge cases rather
than asserting wording or implementation details alone. Reuse existing fixtures
for destructive or credential-changing cases; distinguish fixture checks from
live verification. When local delivery is requested, use the supported build
path, verify the actual browser result, and report any unverified behavior.
