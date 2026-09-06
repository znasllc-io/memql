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
// The portal's Data origins table had this right by hand -- it printed an em
// dash for absent health, with a comment reading "never run" is not "ran
// clean". Carrying it as a type rather than as a habit is what keeps the
// fifth surface from getting it wrong.

/** Why a figure has no number. Closed: a new reason is a design decision. */
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
 */
export function figureOf(value: number): Figure {
  if (!Number.isFinite(value)) return absent("unmeasured");
  return { kind: "measured", value };
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
