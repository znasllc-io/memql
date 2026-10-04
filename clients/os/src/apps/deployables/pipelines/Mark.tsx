import { StopGlyph, stopStateSentence, type StopState } from "../../../kit/Rail";

// One rail mark on its own: a run's outcome at the head of its row, a step's
// state at the head of its line. The same circle, glyph and colour the rails
// draw (`.os-rail-stage[data-state] .os-rail-mark`), so a list of runs, a
// run's stops and a deploy's rail are one visual vocabulary.
export function Mark({ state, label }: { state: StopState; label?: string }) {
  return (
    <span className="os-rail-stage pipeline-mark" data-state={state}>
      <span className="os-rail-mark" {...(label ? { role: "img", "aria-label": `${label}, ${stopStateSentence(state)}` } : { "aria-hidden": true })}>
        <StopGlyph state={state} size={9} />
      </span>
    </span>
  );
}
