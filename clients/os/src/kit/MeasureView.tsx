import type { Figure } from "./measure";
import { absentSentence } from "./measure";

// Rendering the other half of a Figure.
//
// PROMOTED INTO THE KIT (epic memql#5153, D3). It arrived in src/cluster/ as
// FigureValue and had thirteen consumers before this epic asked for a
// fourteenth -- a figure with its own provenance, on Levels, on the machine
// page and in the fleet-wide measured column. The promotion rule in
// controls.tsx is "a control earns a place here when a SECOND surface needs
// it"; this one was overdue rather than early, and the name it earns here is
// the one the design record asks for.
//
// An absent figure draws an EM DASH, not a zero and not an empty cell. The
// dash is the shell's existing spelling for "no answer" (the Deployables
// facts rows use it), and it carries the reason as a title so the sentence
// is one hover away without spending a line of the table on it.
//
// The dash is deliberately not muted into invisibility: somebody scanning a
// column of numbers for the one that is missing has to be able to SEE the
// gap. `.os-figure-absent` is the muted ink, not the disabled one.

export function Measure({
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
