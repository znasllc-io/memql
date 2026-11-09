// A number an operator surface may show, or the reason there is no number.
//
// ===========================================================================
// WHY THIS IS A TYPE AND NOT A CONVENTION
// ===========================================================================
// Every surface in the Cluster and Stores apps reports counts somebody makes
// a decision on: how far a connector is behind, how many rows drifted, how
// many dead letters are waiting, how many tokens a delegated run spent. For
// each of them there is a state where the honest answer is not a number --
// the sweep has never run, the app reported no usage, the read came back
// refused -- and the failure mode is always the same shape: rendering that
// state as `0`.
//
// A zero is a measurement. It says "we looked, and the answer is none". An
// absent figure says "we did not look, or we could not". They lead to
// opposite actions -- a `0` next to "dead letters" is a reason to stop
// worrying, and it is exactly what a connector that has never run also
// produces if the surface reaches for `?? 0`.
//
// So the two are different VALUES here, and a Figure carries one or the
// other and never both. `figureOf` is deliberately the only way to make a
// measured one, and it refuses a non-finite number rather than passing NaN
// through to the pixel. This mirrors component/proving/figure, which made
// the same decision in Go for the same reason (epic memql#4993): "an absent
// figure and a zero are different answers".
//
// Promoted from src/cluster/figure.ts into the kit (epic memql#5153, D3):
// thirteen consumers across Email, Cluster, Fleet, Settings and Stores,
// and this epic adds Levels and the fleet-wide measured column. The
// COMPONENT half is kit/Measure.tsx; this half stays pure so a .ts module
// can import it without pulling JSX through the barrel.
//
// The portal's Data origins table had this right by hand -- it printed an em
// dash for absent health, with a comment reading "never run" is not "ran
// clean". Carrying it as a type rather than as a habit is what keeps the
// fifth surface from getting it wrong.

/**
 * Why a figure has no number. Closed: a new reason is a design decision.
 *
 * ===========================================================================
 * WHAT THESE FOUR CANNOT SAY, AND WHY THAT IS LEFT OPEN
 * ===========================================================================
 * They cannot express "the mechanism exists, but not in THIS mode" -- a figure
 * that is unmeasurable on one path and perfectly measurable on another. The
 * nearest value is `unmeasured`, which would report a figure nothing on this
 * path can produce as one nobody has got round to: right about the absence,
 * wrong about who can fix it, and it sends the reader somewhere there is
 * nothing to do.
 *
 * `component/proving/figure` hit this in Go and its answer is the shape to
 * copy -- `seamNotBuilt` became `notMeasurableOnReplay` when the seam was
 * built, because THE REASON CHANGED WHILE THE ABSENCE DID NOT. That is the
 * property a fifth value would have to preserve.
 *
 * It is not added here because this set already has twelve consumers, and
 * widening a closed vocabulary hands every one of them a value none of them
 * handles. Add the fifth when a surface actually has the mode -- epic
 * memql#5146's probe is the likely first -- and treat it as a design decision
 * with its own reading, not as an enum entry.
 */
export type AbsentReason =
  /** Nothing has ever reported this. A sweep that has not run, an app that
   *  reported no usage. NOT an error, and NOT zero. */
  | "unmeasured"
  /** The read came back refused. The caller may not ask this question. */
  | "refused"
  /** The read failed. Different from refused: refused is an answer. */
  | "failed"
  /** The read has not been made yet, or is in flight. */
  | "unread";

export interface MeasuredFigure {
  readonly kind: "measured";
  readonly value: number;
  /**
   * THE VALUE IS A FLOOR: the read stopped at a window and found more, so it
   * is "this many or more" (memql#6033). Still a measurement -- somebody
   * counted -- but never an exact one, and it is drawn with "or more"
   * wherever it is drawn. Present only on a floor.
   */
  readonly atLeast?: true;
}

export interface AbsentFigure {
  readonly kind: "absent";
  readonly reason: AbsentReason;
  /** The server's own sentence, when there is one. Never invented here. */
  readonly detail: string;
}

export type Figure = MeasuredFigure | AbsentFigure;

/**
 * A measured figure. Refuses a non-finite value: NaN and Infinity are what
 * arithmetic over a missing field produces, and rendering either is how an
 * absent number reaches a person looking like a measured one.
 *
 * `atLeast` makes it a floor: the read stopped at a window and found more.
 */
export function figureOf(value: number, atLeast = false): Figure {
  if (!Number.isFinite(value)) return absent("unmeasured");
  return atLeast ? { kind: "measured", value, atLeast: true } : { kind: "measured", value };
}

/**
 * A `count` query's answer as a figure (the SDK's `Result.count()`): exact,
 * or -- when the engine stopped at its window, which is configurable and so
 * never guessed here -- a floor. No count at all is unmeasured, not zero.
 */
export function countFigure(answer: { count: number; atLeast: boolean } | null): Figure {
  if (answer === null) return absent("unmeasured");
  return figureOf(answer.count, answer.atLeast);
}

export function absent(reason: AbsentReason, detail = ""): Figure {
  return { kind: "absent", reason, detail };
}

/**
 * Read a figure off a wire row's field.
 *
 * An ABSENT KEY and a null are "unmeasured" -- the writer never reported
 * this. A present number is measured, zero included: a connector that ran
 * and found nothing to do genuinely did measure zero, and that is a
 * different sentence from one that has never run.
 *
 * A string is parsed, because the wire carries large integers as strings;
 * an unparseable one is unmeasured rather than NaN.
 */
export function figureFrom(row: Record<string, unknown> | null | undefined, key: string): Figure {
  const raw = row?.[key];
  if (raw === undefined || raw === null) return absent("unmeasured");
  if (typeof raw === "number") return figureOf(raw);
  if (typeof raw === "string") {
    if (raw.trim() === "") return absent("unmeasured");
    return figureOf(Number(raw));
  }
  return absent("unmeasured");
}

/** The number, or null. For callers that must branch, never to render. */
export function figureValue(figure: Figure): number | null {
  return figure.kind === "measured" ? figure.value : null;
}

/** What `Measure` writes for a measured figure, for a copy control. An absent
 *  figure has nothing to copy, so it answers undefined rather than a dash. */
export function figureText(figure: Figure, format?: (value: number) => string, suffix = ""): string | undefined {
  if (figure.kind === "absent") return undefined;
  return `${format ? format(figure.value) : String(figure.value)}${suffix}${figure.atLeast ? " or more" : ""}`;
}

/**
 * Whether a figure is a measured value above zero -- the one question a
 * surface asks often enough to be worth a name, because `> 0` on a
 * `figureValue` result silently reads null as "not above zero" and hides
 * the distinction this whole module exists to keep.
 */
export function isPositive(figure: Figure): boolean {
  return figure.kind === "measured" && figure.value > 0;
}

/**
 * The sentence for an absent figure, for a caption or a title attribute.
 *
 * These are the words the surfaces use, in one place, so five sections do
 * not each invent their own phrasing for the same state.
 */
export function absentSentence(figure: AbsentFigure): string {
  if (figure.detail.trim() !== "") return figure.detail;
  if (figure.reason === "unmeasured") return "Nothing has reported this yet.";
  if (figure.reason === "refused") return "This is not yours to read.";
  if (figure.reason === "failed") return "The read failed.";
  return "Not read yet.";
}

/** A figure as a person reads it: the count (with "or more" when it stopped at a window), or "--" when it was not measured. */
export function figureWords(figure: Figure | undefined): string {
  return figure?.kind === "measured" ? `${figure.value.toLocaleString()}${figure.atLeast ? " or more" : ""}` : "--";
}

/**
 * How a figure moved against the same figure for the period before it: by a
 * share of what it was, from nothing to something (`new`), or not at all
 * that can be said (`none`).
 *
 * `none` IS AN ANSWER ABOUT THE COMPARISON, NOT ABOUT THE FIGURE. It is what
 * a period with no period before it gives (twelve months, spec D4), and what
 * either side being absent gives -- a website whose visitors were not counted
 * last week did not grow from zero, and the badge that says so would be the
 * absent-is-zero mistake this module exists to prevent, one figure removed.
 */
export type Change = { kind: "none" } | { kind: "new" } | { kind: "ratio"; ratio: number };

/**
 * The change from `previous` to `current`.
 *
 * Absent on either side, or no previous figure at all, is `none`. A previous
 * zero is `new` when there is something now and no change when there is still
 * nothing. Otherwise the difference as a share of the previous figure.
 *
 * A FLOOR ON EITHER SIDE IS `none` too: "5,000 or more" against 4,000 is at
 * least a quarter up, and "+25%" would state the least it could be as what it
 * is. A negative figure (nothing here counts below zero) has no share to
 * speak of either.
 */
export function changeOf(current: Figure, previous: Figure | undefined): Change {
  if (previous === undefined || current.kind !== "measured" || previous.kind !== "measured") return { kind: "none" };
  if (current.atLeast || previous.atLeast) return { kind: "none" };
  if (current.value < 0 || previous.value < 0) return { kind: "none" };
  if (previous.value === 0) return current.value > 0 ? { kind: "new" } : { kind: "ratio", ratio: 0 };
  return { kind: "ratio", ratio: (current.value - previous.value) / previous.value };
}

/** Below half a percent either way, a change rounds to no change rather than to a signed zero. */
const NO_CHANGE_BELOW = 0.005;

/**
 * A change as a person reads it: "+12%", "−8%" (a true minus sign, U+2212,
 * the width of the plus), "No change", "New", or nothing for `none`.
 */
export function changeLabel(change: Change): string {
  if (change.kind === "none") return "";
  if (change.kind === "new") return "New";
  const { ratio } = change;
  if (!Number.isFinite(ratio)) return "";
  if (Math.abs(ratio) < NO_CHANGE_BELOW) return "No change";
  const percent = Math.round(Math.abs(ratio) * 100).toLocaleString("en-US");
  return `${ratio > 0 ? "+" : "−"}${percent}%`;
}
