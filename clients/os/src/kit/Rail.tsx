import type { ReactNode } from "react";
import { Check, Minus, X } from "lucide-react";

// THE RAIL, drawn. Promoted here on its second use (epic memql#5106): the
// Deployables page has read a deploy as a rail since epic memql#4937, and the
// first-run wizard reads the core modules the same way.
//
// ===========================================================================
// ONE PICTURE, TWO TENSES
// ===========================================================================
// Deployables' rail is a RECORD -- what a run did, and what it did not. The
// setup rail is a SET OF DOORS -- what is done, and what is still yours to
// do. The picture is the same because the reading is the same: a column of
// marks joined by a line, where the marks that are NOT lit carry as much
// information as the ones that are.
//
// The state set is what keeps the two honest. `ahead` and `pending` mean
// NOT REACHABLE -- they dim, and they carry no disclosure, because there is
// nothing behind them yet. `waiting` means reachable and not done: full
// opacity, a chevron, and a person may open it whenever they like. A rail
// whose unfinished stops all dimmed would tell a first-run reader that three
// of their four stops were somebody else's business.
//
// COMPACT renders the marks alone, in a row, with no labels: each mark
// carries the stop's name and state as its accessible name, because a row of
// five dots with no name is a row of five dots.

/**
 * One closed set of states, across every rail in the shell.
 *
 * `done` / `complete` draw alike (compose says a stop is complete; a record
 * says a stage is done). `ahead` / `pending` draw alike (not reached; not
 * reachable yet) and are the only two that DIM. `open` is a stop waiting on
 * the person -- a held ring, never a pulse, because nothing is moving until
 * they act. `current` is the one thing that moves. `waiting` is open's
 * quieter sibling: reachable, not done, and not the one the rail has opened.
 * `unknown` is a stop whose state has not been read yet -- distinct from
 * every other value, because "we do not know" is not "not yet".
 */
export type StopState =
  | "done"
  | "complete"
  | "current"
  | "open"
  | "waiting"
  | "skipped"
  | "stopped"
  | "ahead"
  | "pending"
  | "unknown";

export interface Stop {
  id: string;
  /** What the stop is called, in the reader's words. */
  name: string;
  state: StopState;
  /**
   * The line beneath the name: what this stop is for, or why a stage was
   * skipped. Shown under a closed stop only when there is no `answer`, and
   * under an open one whenever it says something the line above does not.
   */
  sentence?: string;
  /**
   * A settled stop's one-line answer, shown on the collapsed line in place of
   * the sentence. Without one, a collapsed stop reads as a name with nothing
   * behind it -- exactly the "is this broken or just closed" question a
   * disclosure must not raise.
   */
  answer?: string;
  /** What the stop holds when it is open. */
  body?: ReactNode;
  /** Independent controls beside the open step's title. */
  headerActions?: ReactNode;
  /**
   * Whether this stop is a disclosure at all. Defaults to "it is reachable",
   * which is right for a rail over a RECORD: every reached stage of a deploy
   * has an answer to show.
   *
   * A rail over a SET OF DOORS is different -- a finished one has nothing
   * behind it -- and the header's rule applies just as much to a stop that is
   * DONE as to one that is not reachable yet: a chevron promises something
   * behind it, and one that opens an empty body is worse than no chevron.
   */
  openable?: boolean;
}

/** Whether a stop can be opened at all: there is something behind it. */
export function stopIsReachable(state: StopState): boolean {
  return state !== "pending" && state !== "ahead" && state !== "unknown";
}

/**
 * Whether a stop renders its body.
 *
 * A rail with no `openStop` is not a disclosure at all and every stop renders
 * -- which is what the compose reading wants, where the rail IS the form and
 * an unreached stop already draws nothing.
 */
export function stopIsOpen(stop: Stop, openStop: string | undefined): boolean {
  if (openStop === undefined) return true;
  return openStop === stop.id && stopOpens(stop);
}

/** Whether a stop discloses: reachable, unless the caller says otherwise. */
export function stopOpens(stop: Stop): boolean {
  return stop.openable ?? stopIsReachable(stop.state);
}

/**
 * The stop a rail opens when nobody has chosen one: the FIRST that is not
 * settled.
 *
 * This is the whole of "stops, not steps" (interface rule 12). There is no
 * Next, no Back and no step number -- the rail simply opens the first thing
 * still to do, and every other reachable stop stays one click away. An empty
 * string when every stop is settled, which is a rail with nothing open.
 */
export function nextOpen(stops: readonly Stop[]): string {
  const next = stops.find(
    (s) => s.state !== "done" && s.state !== "complete" && s.state !== "skipped" && stopIsReachable(s.state),
  );
  return next ? next.id : "";
}

export function Rail({
  stops,
  reversed = false,
  compact = false,
  label,
  openStop,
  onOpenStop,
  scale,
}: {
  stops: readonly Stop[];
  /** Read bottom-up: a rollback. The DOM order never changes. */
  reversed?: boolean;
  /** The marks in a row, no labels -- the list row's form. */
  compact?: boolean;
  /** The list's accessible name. */
  label: string;
  /**
   * COLLAPSED BY DEFAULT, ONE OPEN (epic memql#4937, design section C).
   *
   * When set, only this stop renders its body; every other is one line --
   * mark, name, its answer, a chevron -- and reopens on click. That is what
   * takes a deployable page from 5,069px to one screen, and what fits four
   * setup stops inside a desk widget.
   */
  openStop?: string;
  onOpenStop?: (stopId: string) => void;
  /**
   * PAGE SCALE. The rail was sized for a desk widget and for a page where it
   * is one part among several. In a wizard it is the page's SPINE -- the only
   * structure there is -- so the marks, the names and the rhythm step up to
   * the content size. Same element, same states, same colours.
   */
  scale?: "page";
}) {
  const collapsible = openStop !== undefined && onOpenStop !== undefined;
  const ordered = reversed ? [...stops].reverse() : stops;

  if (compact) {
    return (
      <ol className="os-rail" data-compact="true" data-reversed={reversed ? "true" : "false"} aria-label={label}>
        {ordered.map((stop) => (
          <li key={stop.id} className="os-rail-stage" data-state={stop.state}>
            <span className="os-rail-mark" role="img" aria-label={`${stop.name}, ${stopStateSentence(stop.state)}`}>
              <StopGlyph state={stop.state} size={7} />
            </span>
          </li>
        ))}
      </ol>
    );
  }

  return (
    <ol className="os-rail" data-reversed={reversed ? "true" : "false"} data-scale={scale} aria-label={label}>
      {ordered.map((stop) => {
        // A stop with nothing behind it is never a disclosure -- not one that
        // cannot be reached yet, and not one that is finished. A chevron
        // promises something there.
        const reachable = stopOpens(stop);
        const open = stopIsOpen(stop, collapsible ? openStop : undefined);
        const answer = stop.answer ?? "";
        const note = stop.sentence ?? "";

        const heading = collapsible && reachable ? (
                <button
                  type="button"
                  className="os-rail-line"
                  aria-expanded={open}
                  onClick={() => onOpenStop?.(open ? "" : stop.id)}
                >
                  <span className="os-rail-label">{stop.name}</span>
                  <span className="os-rail-answer">{answer === "" ? note : answer}</span>
                  <span className="os-rail-chev" aria-hidden>
                    &#9656;
                  </span>
                </button>
              ) : collapsible && stop.openable === false ? (
                // A stop the CALLER declared closed, on a rail whose others
                // open. The same line without the button and the chevron, so
                // its answer reads in the same column as every other stop's
                // -- dropping to the plain label-and-note form would replace
                // "Set up" with the module's whole description and lose the
                // one word the row exists to carry.
                //
                // Gated on the EXPLICIT `false` so a rail that never sets
                // `openable` cannot reach this branch: an unreached deploy
                // stage keeps the form it has always had.
                <span className="os-rail-line" data-static="true">
                  <span className="os-rail-label">{stop.name}</span>
                  <span className="os-rail-answer">{answer === "" ? note : answer}</span>
                </span>
              ) : (
                <>
                  <span className="os-rail-label">{stop.name}</span>
                  <span className="os-rail-note">{note}</span>
                </>
              );

        return (
          <li key={stop.id} className="os-rail-stage" data-state={stop.state} data-open={open ? "true" : undefined}>
            <span className="os-rail-mark" aria-hidden>
              <StopGlyph state={stop.state} size={11} />
            </span>
            <span className="os-rail-body">
              {open && stop.headerActions ? <span className="os-rail-heading">{heading}{stop.headerActions}</span> : heading}
              {/* The note stays visible under an OPEN collapsed stop -- but
                  ONLY when it says something the line above does not. The
                  answer and the note are frequently the SAME string (a
                  settled stop's answer IS its note), and rendering both put
                  the sentence on screen twice. */}
              {collapsible && reachable && open && answer !== "" && note !== answer ? (
                <span className="os-rail-note">{note}</span>
              ) : null}
              {open ? stop.body ?? null : null}
            </span>
            <span className="os-visually-hidden">{stopStateSentence(stop.state)}</span>
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

export function stopStateSentence(state: StopState): string {
  switch (state) {
    case "done":
      return "finished";
    case "complete":
      return "complete";
    case "current":
      return "running now";
    case "open":
      return "waiting on you";
    case "waiting":
      return "not set up yet";
    case "skipped":
      return "skipped";
    case "stopped":
      return "stopped here";
    case "pending":
      return "not reachable yet";
    case "unknown":
      return "not known";
    default:
      return "not reached";
  }
}
