import { useEffect, useId, useRef } from "react";
import { ArrowLeft } from "lucide-react";

import { ChoiceStack, Subhead } from "../kit";
import { InlineSkeleton } from "../kit/ContentSkeleton";
import { ASK_LEVELS, routingFor, whereOf, type AskLevel, type AskRouting, type RouteWhere } from "./askRoute";
import { routeOptions, useRouteFacts } from "./routeSources";

// The Ask route picker (design brief "routing in MemQL OS", section 6).
//
// It REPLACES the Ask panel's content rather than floating over it: a popover
// over a streaming transcript is two things fighting for one place, and a
// person choosing where their next question goes is not reading the last
// answer. Back (or Escape) returns to the conversation with nothing changed.
//
// CHOOSING IS THE ACTION. There is no Apply: a Where row applies to this
// conversation and returns -- the wizard's "a step that is one choice is
// answered by choosing". Effort applies in place and stays, because it
// modifies the Where choice; a person setting both does it in one visit.
//
// Nothing here edits a route or a rule; it only chooses for this
// conversation. That is said ONCE, to assistive technology, in the region's
// description -- on screen it would be a standing sentence restating what the
// control already does (rule 7).

export function AskRoutePicker({
  routing,
  onChoose,
  onBack,
  onManageRoutes,
}: {
  routing: AskRouting;
  /** Apply a choice to this conversation. */
  onChoose: (routing: AskRouting) => void;
  /** Return to the conversation. */
  onBack: () => void;
  /** Open Fleet's routing; absent where there is no shell to open it in. */
  onManageRoutes?: () => void;
}) {
  const facts = useRouteFacts();
  const options = routeOptions(facts, routing.level);
  const current = whereOf(routing.source);
  const root = useRef<HTMLDivElement | null>(null);
  const describedBy = useId();

  // Focus lands on the current choice, so a keyboard is one Enter from
  // keeping it and one Tab from the next.
  useEffect(() => {
    root.current?.querySelector<HTMLElement>('[role="radio"][aria-checked="true"]')?.focus();
  }, []);

  // Escape unwinds ONE layer: back to the conversation. Capture phase, so it
  // runs before the sheet's own listener would close Ask altogether.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.stopPropagation();
      onBack();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [onBack]);

  function chooseWhere(value: string) {
    const option = options.find((o) => o.where === value);
    if (!option || option.state !== "ready") return;
    onChoose(routingFor(option.where, routing.level));
    onBack();
  }

  function chooseLevel(level: AskLevel) {
    // A source this picker does not offer (a named route kept from
    // elsewhere) keeps its selector; only the level moves.
    const where: RouteWhere | null = current;
    onChoose(where === null ? { source: routing.source, level } : routingFor(where, level));
  }

  return (
    <div ref={root} className="os-ask-route" role="region" aria-label="Route" aria-describedby={describedBy}>
      <p id={describedBy} className="os-sr-only">
        Applies to this conversation only. Routes and rules stay as they are.
      </p>
      <button type="button" className="os-ask-route-back" aria-label="Back to the conversation" onClick={onBack}>
        <ArrowLeft size={14} aria-hidden /> Conversation
      </button>
      <Subhead>Where</Subhead>
      <ChoiceStack
        name="ask-route-where"
        label="Where"
        voice="prose"
        value={current ?? ""}
        onChange={chooseWhere}
        options={options.map((option) => ({
          value: option.where,
          label: option.label,
          description: option.state === "pending" ? <InlineSkeleton label={`Checking ${option.label}`} /> : option.note,
          unavailable: option.state !== "ready",
        }))}
      />
      <Subhead>Effort</Subhead>
      <div className="os-choice-row os-ask-route-effort" role="radiogroup" aria-label="Effort">
        {ASK_LEVELS.map((level) => (
          <button
            key={level.label}
            type="button"
            role="radio"
            aria-checked={routing.level === level.value}
            className="os-choice"
            onClick={() => chooseLevel(level.value)}
          >
            {level.label}
          </button>
        ))}
      </div>
      {onManageRoutes ? (
        <button type="button" className="os-ask-route-manage" onClick={onManageRoutes}>
          Manage routes
        </button>
      ) : null}
    </div>
  );
}
