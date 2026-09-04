import type { ReactNode } from "react";
import { Check, Minus, X } from "lucide-react";

import { railFor, type RailInput, type RailStage, type StopState } from "./rail";

// The rail, drawn. Everything it SAYS comes from `railFor` (rail.ts); this
// file only decides how a state looks, and there is exactly one of it: the
// page, the compose flow, every entry under Every attempt and the list row's
// compact five-dot form all render through here, so a person reads a row as
// the shape they will see on the page.
//
// COMPACT renders the marks alone, in a row, with no labels: each mark
// carries the stop's label and state as its accessible name, because a row
// of five dots with no name is a row of five dots.

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
  /**
   * COLLAPSED BY DEFAULT, ONE OPEN (epic memql#4937, design section C).
   *
   * When set, only this stop renders its body; every other is one line --
   * mark, label, its answer, a chevron -- and reopens on click. That is what
   * takes a deployable page from 5,069px to one screen, and what takes
   * thirteen rails on a page down to one.
   *
   * ABSENT means every stop renders its body, which is what the compose
   * reading wants: there the rail IS the form, and a stop the flow has not
   * reached yet already draws nothing.
   */
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
  const ordered = reversed ? [...stages].reverse() : stages;
  const name = label ?? (input.mode === "deploy" ? "Deploy stages" : "Deployable stops");

  if (compact) {
    return (
      <ol className="os-rail" data-compact="true" data-reversed={reversed ? "true" : "false"} aria-label={name}>
        {ordered.map((stage) => (
          <li key={stage.id} className="os-rail-stage" data-state={stage.state}>
            <span className="os-rail-mark" role="img" aria-label={`${stage.label}, ${stateSentence(stage.state)}`}>
              <StopGlyph state={stage.state} size={7} />
            </span>
          </li>
        ))}
      </ol>
    );
  }

  return (
    <ol className="os-rail" data-reversed={reversed ? "true" : "false"} aria-label={name}>
      {ordered.map((stage) => {
        // A stop nobody can reach yet is never a disclosure: there is nothing
        // behind it, and a chevron would promise otherwise.
        const reachable = stage.state !== "pending" && stage.state !== "ahead";
        const open = !collapsible || (openStop === stage.id && reachable);
        const answer = answerFor ? answerFor(stage) : "";
        const note = stage.reason === "" ? stage.blurb : stage.reason;

        return (
          <li key={stage.id} className="os-rail-stage" data-state={stage.state} data-open={open ? "true" : undefined}>
            <span className="os-rail-mark" aria-hidden>
              <StopGlyph state={stage.state} size={11} />
            </span>
            <span className="os-rail-body">
              {collapsible && reachable ? (
                <button
                  type="button"
                  className="os-rail-line"
                  aria-expanded={open}
                  onClick={() => onOpenStop?.(open ? "" : stage.id)}
                >
                  <span className="os-rail-label">{stage.label}</span>
                  <span className="os-rail-answer">{answer === "" ? note : answer}</span>
                  <span className="os-rail-chev" aria-hidden>
                    &#9656;
                  </span>
                </button>
              ) : (
                <>
                  <span className="os-rail-label">{stage.label}</span>
                  <span className="os-rail-note">{note}</span>
                </>
              )}
              {/* The note stays visible under an OPEN collapsed stop -- but
                  ONLY when it says something the line above does not. The
                  answer and the note are frequently the SAME string (a
                  settled stop's answer IS its note), and rendering both put
                  the sentence on screen twice. */}
              {collapsible && reachable && open && answer !== "" && note !== answer ? (
                <span className="os-rail-note">{note}</span>
              ) : null}
              {open && stopBody ? stopBody(stage) : null}
            </span>
            <span className="os-visually-hidden">{stateSentence(stage.state)}</span>
          </li>
        );
      })}
    </ol>
  );
}

function StopGlyph({ state, size }: { state: StopState; size: number }) {
  switch (state) {
    case "done":
    case "complete":
      return <Check size={size} />;
    case "skipped":
      return <Minus size={size} />;
    case "stopped":
      return <X size={size} />;
    case "current":
    case "open":
      // The moving stop gets no glyph at all: it is the one thing on the
      // rail that is moving, and the animated ring around it says so more
      // clearly than a symbol inside it would. The open stop holds the same
      // ring, still -- it is waiting on the person, and nothing moves until
      // they act.
      return null;
    default:
      // NOR DOES AN UNREACHED ONE. A crossed circle reads as "forbidden",
      // which is a different statement from "has not happened yet" -- and on a
      // rail six stages long it put five refusal symbols on a healthy deploy.
      // An empty ring says exactly as much and says nothing wrong.
      return null;
  }
}

function stateSentence(state: StopState): string {
  switch (state) {
    case "done":
      return "finished";
    case "complete":
      return "complete";
    case "current":
      return "running now";
    case "open":
      return "waiting on you";
    case "skipped":
      return "skipped";
    case "stopped":
      return "stopped here";
    case "pending":
      return "not reachable yet";
    default:
      return "not reached";
  }
}
