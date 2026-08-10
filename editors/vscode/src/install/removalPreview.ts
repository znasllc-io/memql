// The uninstall preview, as a view model.
//
// `previewUninstall` (session.ts, #3469) already answers the hard questions:
// which steps have a receipt entry to act on, which artifact each one names,
// and -- the load-bearing one -- which artifacts the install merely FOUND and
// must therefore leave alone. This module does not re-answer any of them. It
// takes that answer and shapes it into the rows a renderer draws.
//
// THE VERDICT IS NOT RE-DERIVED HERE. It would be easy to read the receipt a
// second time and work out "preserved" again, and that is exactly the mistake:
// a second derivation is a second chance to disagree with the first, and the
// two things that would then disagree are the list the operator consents to and
// the flags the removal actually runs with. `preserved` arrives already
// decided; this file copies it and adds nothing.
//
// NOTHING WITH AN ARTIFACT IS DROPPED, and nothing without one is invented. A
// step that will be skipped is a step whose receipt had no entry -- there is no
// artifact, so there is nothing to tell the operator about, and a row saying so
// would be a row about a thing that does not exist on their machine.
//
// Deliberately free of `vscode` imports -- like the rest of src/install, this
// has to run as plain node from cli.ts, and being pure over a plain value is
// what lets the whole mapping be tested with no renderer and no filesystem.
//
// Refs: #3476 #3469

/**
 * One row of the preview.
 *
 * This MIRRORS `RemovalItemView` from `@znasllc-io/memql-view-kit` and must
 * stay in sync with it. It is redeclared rather than imported because the
 * view-kit export lands on its own branch; the webview that renders these rows
 * is separate work, and this module is the pure half that needs neither.
 */
export interface RemovalItemView {
  /** The uninstall step this row is about. */
  id: string;
  /** The artifact, named as the operator would name it, with its target. */
  label: string;
  kind: "removed" | "preserved";
  /** Why it is preserved. Always present on a preserved item. */
  reason?: string;
}

/**
 * The half of `PlannedStep` (session.ts, #3469) this mapping reads.
 *
 * Declared structurally, and narrowed to the fields actually consumed, so a
 * real `PlannedStep` satisfies it without a cast and no field is copied here
 * that this module has no business having an opinion about.
 */
export interface PreviewStep {
  id: string;
  description: string;
  action: "run" | "skip";
  /** Why it will be skipped. Empty for a step that will run. */
  reason: string;
  /** The artifact pre-existed the install, so it stays. */
  preserved: boolean;
  /** What will be removed, ALREADY in words -- e.g. `cluster memql`. */
  target: string;
}

/** The half of `UninstallPreview` (session.ts, #3469) this mapping reads. */
export interface PreviewInput {
  /** The steps that will actually remove something. */
  removals: readonly PreviewStep[];
  /** Artifacts the install found already present. They stay. */
  preserved: readonly PreviewStep[];
}

/**
 * The reason shown when a preserved step carries none of its own.
 *
 * THIS IS A GUARD, NOT THE EXPECTED PATH. `previewUninstall` populates a
 * preserved step's reason at the source, where the receipt's pre-existence
 * verdict is actually in hand, so in practice every preserved row arrives with
 * a better sentence than this one and this constant goes unused. It survives
 * anyway because the invariant it protects belongs to THIS function: a
 * preserved row must never render bare, since a preserved artifact with no
 * stated reason reads as a removal that quietly failed -- the precise opposite
 * of what happened, which is that the installer found it already here and is
 * deliberately declining to touch it.
 *
 * Deleting it would make that property depend on a different module keeping
 * its own discipline. `reason` is typed `string`, so an empty one stays
 * representable no matter what today's producer does.
 */
const DEFAULT_PRESERVED_REASON = "it existed before the install";

/**
 * The rows the uninstall preview renders.
 *
 * REMOVALS FIRST, THEN PRESERVED, each in the order the preview already carries
 * them. Those arrays are partitioned and ordered by `previewUninstall` out of
 * the uninstall graph's own topological order -- which is NOT the reverse of
 * the install order, since mkcert has to outlive the CA it is needed to
 * uninstall. Re-sorting here would silently replace a real ordering with a
 * plausible-looking one; the renderer does not sort either. What comes back is
 * what the operator reads, top to bottom.
 */
export function removalPreviewItems(preview: PreviewInput): RemovalItemView[] {
  const items: RemovalItemView[] = [];
  for (const step of preview.removals) {
    if (isArtifactless(step)) continue;
    items.push(itemFor(step));
  }
  for (const step of preview.preserved) {
    if (isArtifactless(step)) continue;
    items.push(itemFor(step));
  }
  return items;
}

/**
 * A step with nothing behind it.
 *
 * A skip means the receipt recorded no entry for the install step this one
 * reverses, so there is no artifact on this machine and no row to draw.
 * `previewUninstall` already keeps skips out of both arrays; the check is here
 * so that the omission is a property of this mapping rather than a coincidence
 * of who calls it. A preserved step is never artifactless -- it is a step that
 * WILL run and then refuse, which is a real artifact staying put.
 */
function isArtifactless(step: PreviewStep): boolean {
  return step.action === "skip" && !step.preserved;
}

function itemFor(step: PreviewStep): RemovalItemView {
  const item: RemovalItemView = {
    id: step.id,
    label: label(step),
    kind: step.preserved ? "preserved" : "removed",
  };
  if (step.preserved) {
    item.reason = step.reason.trim() !== "" ? step.reason : DEFAULT_PRESERVED_REASON;
  }
  return item;
}

/**
 * What the step is about, plus which thing it is about.
 *
 * `target` arrives ALREADY rendered ("cluster memql-local", "path
 * /home/dev/.memql/bin/k3d"), phrased off the very params the removal will be
 * given. Re-rendering it from params here would be a second phrasing to keep in
 * step with the first, and the failure would be a preview naming something
 * other than what gets removed -- so it is passed through untouched.
 *
 * It also does the work the description cannot: three of the uninstall graph's
 * seven steps remove a `binary`, and while their descriptions differ, it is the
 * target that says WHICH file is going. When the preview has no target the
 * description stands alone -- an empty "()" would advertise a value nobody has.
 */
function label(step: PreviewStep): string {
  // Descriptions are sentences in the graph document ("Delete the local k3d
  // cluster."), and a list item that ends in a full stop before its
  // parenthetical reads as two fragments. The trailing stop comes off.
  const described = step.description.trim().replace(/\.+$/, "");
  const target = step.target.trim();
  if (described !== "" && target !== "") return `${described} (${target})`;
  // A row with no words at all is unreadable, and an unreadable row is an
  // artifact the operator cannot consent to. The step id is a poor name but it
  // is always there.
  return described || target || step.id;
}
