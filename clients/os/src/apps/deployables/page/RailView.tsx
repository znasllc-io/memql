import type { ReactNode } from "react";

import { Rail as KitRail, stopIsOpen, type Stop } from "../../../kit/Rail";
import { railFor, type RailInput, type RailStage } from "./rail";

// A deploy, read as a rail. THE DRAWING MOVED (epic memql#5106): the marks,
// the connector, the states and the disclosure all live in `kit/Rail.tsx`
// now, and this is the adapter from what a deploy KNOWS -- a `RailInput`
// folded by `railFor` into stages -- to what the kit draws.
//
// The props are unchanged, deliberately. Four surfaces render through here
// (the page, the compose flow, Every attempt and the list row's compact
// form), and a promotion that made each of them restate the mapping would
// have moved the drawing and left the vocabulary behind.
//
// THE BODIES ARE BUILT FOR THE OPEN STOP ONLY. `stopBody` is a pure switch in
// both callers, so calling it for every stage would be harmless -- but "the
// body of a stop nobody has opened is not built" is a property worth keeping
// true rather than merely currently-affordable.

export function Rail({
  input,
  reversed = false,
  compact = false,
  label,
  stopBody,
  openStop,
  onOpenStop,
  answerFor,
}: {
  input: RailInput;
  /** Read bottom-up: a rollback. The DOM order never changes. */
  reversed?: boolean;
  /** The five marks in a row, no labels -- the list row's form. */
  compact?: boolean;
  /** The list's accessible name. Defaults by mode. */
  label?: string;
  /**
   * What a stop holds beneath its note -- its answer once set, or the form
   * that answers it while it is open. The page mounts a stop's body here
   * rather than drawing a second rail around it. Full rail only.
   */
  stopBody?: (stage: RailStage) => ReactNode;
  /** Collapsed by default, one open. See `kit/Rail.tsx`. */
  openStop?: string;
  onOpenStop?: (stopId: string) => void;
  /**
   * A settled stop's one-line answer, shown on the collapsed line. Without
   * one, a collapsed stop reads as a label with nothing behind it -- which is
   * exactly the "is this broken or just closed" question a disclosure must
   * not raise.
   */
  answerFor?: (stage: RailStage) => string;
}) {
  const collapsible = openStop !== undefined && onOpenStop !== undefined;
  const { stages } = railFor(input);
  const name = label ?? (input.mode === "deploy" ? "Deploy stages" : "Deployable stops");

  const stops: Stop[] = stages.map((stage) => {
    const stop: Stop = {
      id: stage.id,
      name: stage.label,
      state: stage.state,
      // A stage says its blurb until it has a reason of its own -- a skipped
      // stage's "why", or a settled stop's answer.
      sentence: stage.reason === "" ? stage.blurb : stage.reason,
      answer: answerFor ? answerFor(stage) : "",
    };
    if (stopBody && stopIsOpen(stop, collapsible ? openStop : undefined)) {
      stop.body = stopBody(stage);
    }
    return stop;
  });

  return (
    <KitRail
      stops={stops}
      reversed={reversed}
      compact={compact}
      label={name}
      openStop={openStop}
      onOpenStop={onOpenStop}
    />
  );
}
