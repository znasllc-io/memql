import { useState } from "react";

import { Rail, type Stop } from "../../kit";
import { InferenceStop } from "./InferenceStop";
import { ModuleStop } from "./ModuleStop";
import { PasskeyStop } from "./PasskeyStop";
import { useSetupFacts } from "./context";
import { PASSKEY_STOP, drawnState, openStopFor, type SetupStop } from "./stops";
import type { ModuleId } from "../../system/modules";

// THE FIRST-RUN WIZARD (design record 2026-09-06-first-run-wizard, D1 to D6).
//
// ===========================================================================
// STOPS THAT LIGHT UP, NOT STEPS WITH NEXT
// ===========================================================================
// There is no Next, no Back and no step number, because only ONE ordering
// here is a law and the widget states it in one sentence: a passkey before
// anything else, since a sign-in link is the only way back into an account
// without one. The rest are three independent decisions, and a stepper over
// three independent decisions is a form wearing a sequence's clothes
// (interface rule 12).
//
// So the rail opens the first stop still outstanding, every other reachable
// stop is one click away, and a stop somebody opens themselves stays open
// until it is settled -- at which point the rail moves on by itself, which is
// the law and the only motion this surface has.
//
// ===========================================================================
// THE BODY IS ONLY EVER HALF THE WIDGET
// ===========================================================================
// Whether it draws at all is `SetupGate`'s decision, above the frame. This
// component is never mounted while the reading is unsettled and never mounted
// once the core is configured, so it has no "loading" state and no "all done"
// state -- both would be a card on the desk saying nothing.

export function SetupWidget() {
  const facts = useSetupFacts();
  const [override, setOverride] = useState<string | null>(null);
  if (facts === null) return null;

  const { stops, readiness, identityUrl } = facts;
  const openStop = openStopFor(stops, override);
  const outstanding = stops.filter((s) => s.state === "waiting").length;

  const drawn: Stop[] = stops.map((stop) => ({
    id: stop.id,
    name: stop.name,
    state: drawnState(stop, openStop),
    // SAY IT ONCE (interface rule 7). A stop's sentence is drawn under its
    // line while the stop is open, and the inference stop's body is three
    // doors that ARE that sentence enumerated -- "a machine on your fleet
    // serving a model, or a federated cloud vendor" beside a list reading
    // exactly that put the same answer on one card twice, and pushed the act
    // it was introducing below the fold.
    sentence: stop.id === "ai" ? undefined : stop.sentence,
    answer: stop.answer,
    body: bodyFor(stop),
    // A SETTLED STOP IS NOT A DISCLOSURE. Three of the four have nothing at
    // all behind them once they are done -- a configured module's act is
    // absent, because telling somebody where to configure a thing that is
    // configured is an instruction with nothing behind it -- so a chevron
    // there opens an empty body. The one thing a finished passkey stop could
    // offer, a link to manage them, is the identity service's surface and the
    // Cluster app's Readiness section already carries it.
    openable: stop.state === "waiting",
  }));

  return (
    <div className="os-setup-widget">
      <p className="os-setup-widget-lead">{lead(outstanding)}</p>
      <Rail
        stops={drawn}
        label="Set up this cluster"
        openStop={openStop}
        onOpenStop={(id) => setOverride(id)}
      />
    </div>
  );

  /**
   * The one sentence, in three readings.
   *
   * ZERO IS THE EXIT BEAT, not an impossible state: the gate keeps this
   * mounted for a moment after the last stop lights, with every mark lit,
   * before the card folds away. "0 things" in that moment would be the last
   * thing a person read before it went.
   */
  function lead(left: number): string {
    if (left === 0) return "The core is set up. This card goes now.";
    if (left === 1) return "One thing left before this cluster can do useful work. This card goes when it is done.";
    // NO ORDERING CLAUSE. An earlier draft read "in any order after the
    // first", which is false the moment the passkey is done and the "first"
    // it names is no longer on the rail. The one ordering law is stated by
    // the stop it governs, which is where it can be true (rule 7).
    return `${left} things left before this cluster can do useful work. This card goes when they are done.`;
  }

  function bodyFor(stop: SetupStop) {
    // Only an outstanding stop has a body, which is the same rule `openable`
    // above states -- written once as a guard so a stop that stops being
    // openable cannot keep a body nothing can reach.
    if (stop.state !== "waiting") return undefined;
    if (stop.id === PASSKEY_STOP) return <PasskeyStop identityUrl={identityUrl} />;
    const id = stop.id as ModuleId;
    if (id === "ai") return <InferenceStop />;
    return <ModuleStop id={id} verdict={readiness?.of(id) ?? null} />;
  }
}
