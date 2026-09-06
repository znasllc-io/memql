import type { Figure } from "./figure";
import { absentSentence } from "./figure";

// Rendering the other half of a Figure.
//
// An absent figure draws an EM DASH, not a zero and not an empty cell. The
// dash is the shell's existing spelling for "no answer" (the Deployables
// facts rows use it), and it carries the reason as a title so the sentence
// is one hover away without spending a line of the table on it.
//
// The dash is deliberately not muted into invisibility: somebody scanning a
// column of numbers for the one that is missing has to be able to SEE the
// gap. `.os-figure-absent` is the muted ink, not the disabled one.

export function FigureValue({
  figure,
  format,
  suffix,
}: {
  figure: Figure;
  /** How a measured value is written. Default is the plain integer. */
  format?: (value: number) => string;
  /** Unit, appended to a measured value only. An absent figure has no unit. */
  suffix?: string;
}) {
  if (figure.kind === "absent") {
    return (
      <span className="os-figure-absent" title={absentSentence(figure)}>
        &mdash;
      </span>
    );
  }
  const text = format ? format(figure.value) : String(figure.value);
  return (
    <span className="os-figure">
      {text}
      {suffix ? <span className="os-figure-unit">{suffix}</span> : null}
    </span>
  );
}
