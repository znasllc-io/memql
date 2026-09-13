import { useState } from "react";

import { Button, Caption, ChoiceStack, useAppReach, type ChoiceOption } from "../../kit";

// THE INFERENCE STOP: three doors, and the fleet one first (design record
// 2026-09-06-first-run-wizard, D4).
//
// ===========================================================================
// DOORS, NOT A LIST OF WHAT IS OPEN
// ===========================================================================
// Which doors this cluster can already walk through is the readiness feed's
// answer, and the rail line above carries it. These are the three ACTS a
// person can take to open one, in the order a fresh cluster should try them:
// a machine they already own costs nothing and keeps the model on their
// hardware, and the two vendors need an account and a bill. Any combination
// is legal, so the stop is a choice a person makes once and can come back to
// -- not a decision that forecloses the others.
//
// ===========================================================================
// AN ACT THAT IS NOT LEGAL IS THE WORDS INSTEAD (interface rule 12)
// ===========================================================================
// Settings' `providers` section is OWNER-ONLY, so a developer choosing either
// vendor gets a sentence naming who can do it rather than a button that
// navigates a window to a section the registry will not return. That is asked
// of the registry through `useAppReach` -- never restated here -- so the day
// the provider gates widen to owner-or-developer, this surface follows with
// no edit.

type DoorId = "fleet" | "anthropic" | "openai";

// THE DOORS CARRY NO DESCRIPTION, AND THE CHOSEN ONE'S SENTENCE SITS UNDER
// THEM. Three named cards each with two lines of prose is 210px inside a desk
// widget, which pushed the act itself below the fold -- and a first-run
// surface whose one button has to be scrolled to has not done its job. Three
// names read at a glance, and a person reads the sentence for the one they
// are actually considering, which is what a selection is for.
const DOORS: readonly (ChoiceOption & { value: DoorId })[] = [
  { value: "fleet", label: "A machine on your fleet" },
  { value: "anthropic", label: "Anthropic" },
  { value: "openai", label: "OpenAI" },
];

// WHAT EACH DOOR COSTS, AND NOTHING ABOUT HOW THE CREDENTIAL WORKS.
//
// The first drafts of these said "federate the cluster, so no API key exists
// anywhere" and "OpenAI publishes no federation mechanism yet" -- the second
// of which was false when it was written and both of which stop
// discriminating between the two vendor doors the moment they are reached the
// same way (epic memql#5088). Neither belongs here in any case: how a vendor
// proves who this cluster is belongs to the panel that sets it up, and this
// stop is choosing between somebody's own hardware and somebody else's.
//
// So each sentence states the trade the CHOICE turns on, which does not
// change when a credential mechanism does.
const SAYS: Record<DoorId, string> = {
  fleet: "A computer you already own serves the model. Nothing leaves it, and there is no bill.",
  anthropic: "Anthropic serves the model, on their hardware and their bill.",
  openai: "OpenAI serves the model, on their hardware and their bill.",
};

export function InferenceStop() {
  const [door, setDoor] = useState<DoorId>("fleet");
  const fleet = useAppReach("fleet");
  const settings = useAppReach("settings");

  const canPair = fleet.sections.includes("machines") && fleet.canOpenWindows;
  const canOpenProviders = settings.sections.includes("providers") && settings.canOpenWindows;

  return (
    <div className="os-setup-stop">
      <ChoiceStack
        name="setup-inference-door"
        label="How this cluster reaches a model"
        voice="prose"
        value={door}
        onChange={(next) => setDoor(next as DoorId)}
        options={DOORS}
      />
      <Caption>{SAYS[door]}</Caption>
      <div className="os-setup-stop-act">{act()}</div>
    </div>
  );

  function act() {
    if (door === "fleet") {
      return canPair ? (
        <Button
          tone="primary"
          onClick={() => {
            // The Add machine panel, opened for them WITH the local-models
            // box already ticked. Fleet's Machines section has no role floor,
            // so this is offered to a developer in full -- which is the whole
            // reason the fleet door comes first.
            //
            // The flag is what makes the act complete rather than
            // approximately right: the person pressed a button that says
            // "serve a model from a machine you own", and a pairing flow that
            // then asks them to remember to tick a box has handed the last
            // step back.
            fleet.open("machines", { addMachine: { inference: true } });
          }}
        >
          Open Fleet
        </Button>
      ) : (
        <Caption>Pair a machine in Fleet, under Machines.</Caption>
      );
    }
    const vendor = door === "anthropic" ? "Anthropic" : "OpenAI";
    // THE SCREEN THIS OPENS IS CALLED "Doors" (epic memql#5153, D1). The
    // section ID is still `providers` -- deliberately, so this call and
    // MODULE_SETTINGS_SECTION keep working -- but a button that says "Open AI
    // providers" and lands on a page headed "Doors" is a broken signpost, and
    // it is on the one screen a fresh owner cannot dismiss.
    return canOpenProviders ? (
      <Button tone="primary" onClick={() => settings.open("providers", { vendor: door })}>
        Open Doors
      </Button>
    ) : (
      <Caption>An owner can set {vendor} up in Settings, under Doors.</Caption>
    );
  }
}
