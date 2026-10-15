import type { Act } from "../../kit/ActionBar";

// THE ONE SHAPE EVERY EMAIL ACTS TABLE RETURNS (DESIGN.md rule 12).
//
// A record with a lifecycle carries one `ActionBar`, and which acts it offers is
// decided by its STATE alone: a campaign's, an audience's, a template's, an
// automation's and a sender's are each a pure function from the record (and
// what is being asked of it) to a list of these. Keeping the decision pure is
// what lets "an act that is not legal is ABSENT, never disabled" be a property
// of a list that does not contain the act, asserted without a DOM, rather than
// of a button somebody remembered to hide.
//
// A table says WHAT is offered and how it is dressed; it does not say what the
// act does. The page that draws the bar owns that: `toKitActs` joins the two,
// and every act reaches the page by its `id`.

export interface ActSpec {
  /** Names the act to the page that runs it. Unique within one table. */
  id: string;
  /** The label, which is also the promise: an act keeps its name through the whole flow. */
  label: string;
  /** `primary` is what this state is for (last on the bar); `danger` cannot be undone. */
  tone?: "primary" | "danger";
  /** Drawn as a quiet label rather than a button: the way out, or a lesser act. */
  text?: boolean;
}

/**
 * A pure table to the kit's `Act[]`.
 *
 * Order, tone and text dress are kept exactly as the table has them (primary
 * last is the table's to get right), and only the act whose id is `busyId` is
 * busy: one write is in flight, and the act that started it is the one that
 * says so.
 */
export function toKitActs(specs: readonly ActSpec[], run: (id: string) => void, busyId?: string): Act[] {
  return specs.map((spec) => ({
    label: spec.label,
    tone: spec.tone,
    text: spec.text,
    busy: spec.id === busyId,
    onAct: () => run(spec.id),
  }));
}
