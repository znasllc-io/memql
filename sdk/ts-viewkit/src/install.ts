// Install-run and uninstall-preview rendering (memql#3474, #3476).
//
// The add-a-cluster page draws two lists that look alike and mean different
// things: the STEPS of an install or repair as they run, and the ARTIFACTS an
// uninstall is about to remove. Both live here, for the reason cluster.ts
// gives for the topology grid -- the VS Code page and, later, the portal both
// want them, so a VS Code-specific DOM would be rebuilt downstream.
//
// WHY THIS IS NOT `renderChecklist`
//
// The prior design named renderChecklist as "the exact shape of a wizard's
// step list". It is not, for two reasons that only show up on contact:
//
//   - Its `done` slot is `kinds: ["boolean"]`. A step is not done-or-not: it is
//     pending, running, done, skipped or failed, and executor.ts already models
//     that distinction because a skipped step whose condition is SATISFIED lets
//     its dependents run while a failed one does not. Flattening five states
//     into a tick renders a graph that stopped four steps in as merely
//     "unfinished".
//   - It is row/concept-driven (`renderChecklist(rows, concept, options)`),
//     fitting fields by name against a ConceptLike. An install step is not a
//     graph row and inventing a synthetic concept to describe one would put a
//     fake schema in front of a real record.
//
// WHY TWO TYPES, WHEN `preserved` APPEARS IN BOTH
//
// Because they are two different MOMENTS, not two names for one fact:
//
//   - `RemovalItemView.kind` is a CLASSIFICATION, made from the receipt before
//     anything runs. It answers "will the uninstall touch this at all?", and it
//     is what the operator approves -- the preview IS the confirmation (D6).
//   - `InstallStepState` is an OUTCOME, observed after a step has run.
//     `executor.ts` reports `preserved` when an uninstall step's script refuses
//     on `--pre-existing=true`, which is the system working as designed rather
//     than a failure.
//
// So the preview says "this WILL be kept" and the run says "this WAS kept", and
// a surface that folded them would lose the difference between a plan and a
// result -- which is exactly the difference an operator checks when a run does
// not match what they approved.
//
// What stays separate is the AXIS. An artifact's kind is not a degree of
// progress: `removed` is not "further along" than `preserved`. So the preview
// keeps its own type instead of borrowing the step states and leaving three of
// them meaningless. An install run, for its part, never produces `preserved`
// at all.
//
// NO JUDGEMENT LIVES HERE, per the rule cluster.ts states: this module is
// handed strings and enum members a caller already decided, and draws them. It
// does not read a receipt, rank a failure, or decide what pre-existed.
//
// VALUES ARE `data-*` ATTRIBUTES, NEVER PER-VALUE CLASSES -- the same contract
// `.vk-row-status[data-status]` follows. A host that wants failed-is-red or
// preserved-is-grey writes those rules against the attribute in its own sheet,
// and the two surfaces cannot drift apart into different colour vocabularies
// because they share this markup.

import { h, text, type VNode } from "./vnode.js";

/**
 * How far a single graph step has got.
 *
 * `preserved` is here because the executor genuinely produces it as a step
 * OUTCOME: an uninstall step whose script refuses on `--pre-existing=true`
 * exits 3, and `executor.ts` records that as `preserved` rather than a
 * failure, because it is the system working as designed. A run cannot report
 * what this type cannot spell.
 *
 * That does NOT make it the same thing as `RemovalItemView.kind` -- see the
 * module note. The two are different moments: the kind is a CLASSIFICATION
 * made from the receipt before anything runs, and this is an OUTCOME observed
 * after a step has run. An install run never produces `preserved`; only an
 * uninstall does.
 */
export type InstallStepState =
  | "pending"
  | "running"
  | "done"
  | "skipped"
  | "preserved"
  | "failed";

/** Whether an uninstall will take an artifact, or deliberately leave it. */
export type RemovalItemKind = "removed" | "preserved";

/**
 * What privilege removing an artifact needs, straight off the uninstall graph's
 * `elevation` field.
 *
 * This exists because the preview IS the confirmation (design D6): there is no
 * separate yes/no box afterwards, so the list is the ONLY moment the operator
 * consents. Consent to "delete a directory" is not consent to "remove a CA
 * from your system trust store" or "edit /etc/hosts as root" -- and two of the
 * seven uninstall steps change machine-wide state outside memQL's own
 * footprint. Invisible here means approved unseen.
 */
export type RemovalElevation = "none" | "sudo" | "user-trust";

// One step of an install or repair run, already projected by the caller.
export interface InstallStepView {
  /** The graph step id. Emitted as data-step-id for the host's delegation. */
  id: string;
  /** What the step DOES, in the operator's terms. */
  label: string;
  state: InstallStepState;
  /**
   * One sentence qualifying the state -- a skip's reason, a failure's summary.
   *
   * Load-bearing on `skipped`: "already satisfied" and "you passed --skip" are
   * the same state and completely different news.
   */
  detail?: string;
  /**
   * The capability contract's exit code: 2 bad param, 3 refused, 4 prerequisite
   * missing, 5 operation failed. Rendered as a marker and exposed as
   * data-exit-code so a host can present each kind differently. Ignored unless
   * the step failed.
   */
  exitCode?: number;
  /**
   * The step's stderr, VERBATIM.
   *
   * Rendered inside a <details> rather than inline: it is often long and
   * always secondary to the sentence above it, but truncating the one text
   * that says what actually broke would be the wrong economy.
   */
  error?: string;
}

// One artifact an uninstall has decided about, already classified by the caller
// from the receipt.
export interface RemovalItemView {
  /** The receipt entry / step id. Emitted as data-item-id. */
  id: string;
  /** The artifact, named the way the operator would name it. */
  label: string;
  kind: RemovalItemKind;
  /**
   * Why this artifact is being left alone. Rendered only for `preserved`,
   * where the absence of a reason would read as an omission rather than a
   * decision.
   */
  reason?: string;
  /**
   * The privilege this removal needs.
   *
   * OMITTING THIS RENDERS NOTHING -- it does NOT default to "none". "I was not
   * told" and "this needs no privileges" are different claims, and quietly
   * promoting the first into the second is how a preview promises something
   * the run then breaks by stopping for a password. A caller that knows says
   * so explicitly, including when the answer is "none".
   */
  elevation?: RemovalElevation;
}

/**
 * The step list for an install or repair run.
 *
 * Order is the CALLER'S -- the graph's wave order -- and this function does not
 * sort. Re-ordering here would show a sequence that is a property of the
 * renderer rather than of the dependency graph the executor actually walked.
 */
export function renderInstallSteps(steps: InstallStepView[]): VNode {
  if (steps.length === 0) {
    return h("div", { class: "vk-empty" }, [text("No steps to run.")]);
  }

  return h(
    "ol",
    { class: "vk-steps" },
    steps.map((step) => {
      const attrs: Record<string, string> = {
        class: "vk-step",
        "data-step-id": step.id,
        "data-state": step.state,
      };
      if (step.state === "failed" && step.exitCode !== undefined) {
        attrs["data-exit-code"] = String(step.exitCode);
      }

      const children: VNode[] = [
        // The marker is TEXT, not a background image or a pseudo-element, so
        // the list still reads as a list in a copy-paste, a screen reader, or
        // a host that ships no stylesheet at all.
        h("span", { class: "vk-step-marker", "data-state": step.state }, [
          text(stepMarker(step.state)),
        ]),
        h("span", { class: "vk-step-label" }, [text(step.label)]),
        h("span", { class: "vk-step-state", "data-state": step.state }, [text(step.state)]),
      ];

      if (step.state === "failed" && step.exitCode !== undefined) {
        children.push(
          h("span", { class: "vk-step-exit", title: exitCodeMeaning(step.exitCode) }, [
            text(`exit ${step.exitCode}`),
          ]),
        );
      }
      if (step.detail !== undefined && step.detail !== "") {
        children.push(h("span", { class: "vk-step-detail" }, [text(step.detail)]));
      }
      if (step.error !== undefined && step.error !== "") {
        children.push(
          h("details", { class: "vk-step-error" }, [
            h("summary", {}, [text("Output")]),
            h("pre", {}, [text(step.error)]),
          ]),
        );
      }

      return h("li", attrs, children);
    }),
  );
}

/**
 * The uninstall preview: what goes, and what stays.
 *
 * This list IS the confirmation (design D6) -- there is no separate yes/no box
 * behind it -- so it renders both kinds in ONE list rather than hiding the
 * preserved ones behind a disclosure. An operator deciding whether to proceed
 * is deciding about the whole set, and a preserved artifact they expected to
 * see removed is exactly the surprise worth stopping on.
 */
export function renderRemovalPreview(items: RemovalItemView[]): VNode {
  if (items.length === 0) {
    // Not a neutral empty: an uninstall with nothing to do means the receipt
    // recorded nothing, which the operator needs told rather than shown as a
    // blank panel that looks like it is still loading.
    return h("div", { class: "vk-empty" }, [
      text("The receipt records no artifacts, so this uninstall would remove nothing."),
    ]);
  }

  return h(
    "ul",
    { class: "vk-removals" },
    items.map((item) => {
      const attrs: Record<string, string> = {
        class: "vk-removal",
        "data-item-id": item.id,
        "data-kind": item.kind,
      };
      // Absent stays absent -- see the field's note. All three values reach the
      // attribute when given, so a host keeps full control of the styling even
      // for "none"; only the visible marker below is selective.
      if (item.elevation !== undefined) attrs["data-elevation"] = item.elevation;

      const children: VNode[] = [
        h("span", { class: "vk-removal-marker", "data-kind": item.kind }, [
          text(item.kind === "removed" ? "[-]" : "[=]"),
        ]),
        h("span", { class: "vk-removal-label" }, [text(item.label)]),
        h("span", { class: "vk-removal-kind", "data-kind": item.kind }, [text(item.kind)]),
      ];

      // A preserved artifact ALWAYS says why, falling back to the rule itself
      // when the caller passed no sentence. "Preserved" with no reason invites
      // the reading that the uninstall failed to remove it.
      if (item.kind === "preserved") {
        children.push(
          h("span", { class: "vk-removal-reason" }, [
            text(
              item.reason !== undefined && item.reason !== ""
                ? item.reason
                : "it existed before the install",
            ),
          ]),
        );
      } else if (item.reason !== undefined && item.reason !== "") {
        children.push(h("span", { class: "vk-removal-reason" }, [text(item.reason)]));
      }

      // The VISIBLE marker is drawn only where there is something to warn
      // about. "none" beside five of seven rows is noise that trains the eye
      // to skip the column the other two need it to stop at -- and `none` is
      // still on the data attribute above for a host that wants it. This is a
      // presentation choice about emphasis, not a judgement about the value:
      // the renderer is told the elevation and never derives it.
      if (item.elevation !== undefined && item.elevation !== "none") {
        children.push(
          h(
            "span",
            { class: "vk-removal-elevation", "data-elevation": item.elevation, title: elevationMeaning(item.elevation) },
            [text(`[${item.elevation}]`)],
          ),
        );
      }

      return h("li", attrs, children);
    }),
  );
}

/**
 * What an elevation will actually do to the operator's machine, as a tooltip.
 *
 * The bare word is the warning; this is the specifics. "sudo" and "user-trust"
 * both mean "you will be prompted", but they prompt for different things and
 * change different state, and an operator deciding whether to approve the list
 * unattended needs to know which.
 */
function elevationMeaning(elevation: Exclude<RemovalElevation, "none">): string {
  switch (elevation) {
    case "sudo":
      return "needs root -- this edits a system file outside memQL's own footprint";
    case "user-trust":
      return "needs your trust store -- this removes a certificate authority your browsers trust";
  }
}

/** The text marker for a step state. Kept next to the type so a new state cannot be added without one. */
function stepMarker(state: InstallStepState): string {
  switch (state) {
    case "pending":
      return "[ ]";
    case "running":
      return "[~]";
    case "done":
      return "[x]";
    case "skipped":
      return "[-]";
    case "preserved":
      return "[=]";
    case "failed":
      return "[!]";
  }
}

/**
 * The capability-script contract's exit codes, as a tooltip.
 *
 * The number alone tells an operator nothing, and the four cases genuinely ask
 * for different next actions -- a refusal is the system working, a missing
 * prerequisite is something to go install. Source:
 * docs/internal/design/capability-script-contract.md.
 */
function exitCodeMeaning(code: number): string {
  switch (code) {
    case 2:
      return "bad parameter -- the installer passed something the script would not accept";
    case 3:
      return "refused -- the script declined to act";
    case 4:
      return "prerequisite missing -- something this step needs is not on the machine";
    case 5:
      return "operation failed -- the step ran and did not succeed";
    default:
      return `exit ${code}`;
  }
}
