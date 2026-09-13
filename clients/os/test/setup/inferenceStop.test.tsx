import { useRef } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { rolesOpening } from "../seededAccess";
import { OsProvider, useOs } from "../../src/chrome/state";
import { InferenceStop } from "../../src/apps/setup/InferenceStop";

// THE THREE DOORS, and where each one lands. What is asserted is the TRIPLE
// each act opens -- app, section, payload -- because that is the contract
// between this stop and the two apps that consume it, and the only part of it
// a rendered button can get wrong silently.

afterEach(cleanup);

/**
 * Records what the shell was asked to open, through the REAL provider.
 *
 * WRAPPED EXACTLY ONCE. The actions object is stable across renders, so a
 * component that re-wraps on every render wraps its own wrapper, and one
 * click then records two.
 */
function Spy({ onOpen }: { onOpen: (call: unknown[]) => void }) {
  const os = useOs();
  const wrapped = useRef(false);
  if (!wrapped.current) {
    wrapped.current = true;
    os.actions.openApp = ((...args: unknown[]) => {
      onOpen(args);
      return { kind: "none" } as never;
    }) as typeof os.actions.openApp;
  }
  return null;
}

function mount(role: string) {
  const calls: unknown[][] = [];
  render(
    <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }} layout="desktop">
      <Spy onOpen={(c) => calls.push(c)} />
      <InferenceStop />
    </OsProvider>,
  );
  return calls;
}

describe("the doors an owner is offered", () => {
  it("offers three, with the fleet one first and selected", () => {
    mount("owner");
    // THE ORDER IS THE RECOMMENDATION, and it is the only one this surface
    // makes: a machine somebody already owns costs nothing, and the two
    // vendors need an account.
    expect(screen.getAllByRole("radio").map((el) => el.textContent)).toEqual([
      "A machine on your fleet",
      "Anthropic",
      "OpenAI",
    ]);
    expect(screen.getAllByRole("radio")[0]?.getAttribute("aria-checked")).toBe("true");
  });

  it("says what the CHOSEN door costs, one sentence at a time", () => {
    // The doors carry no prose of their own: three names read at a glance,
    // and the sentence belongs to the one being considered. Three
    // descriptions at once put the act below the fold of a desk widget,
    // which the visual pass caught and no test would have.
    mount("owner");
    expect(screen.getByText("A computer you already own serves the model. Nothing leaves it, and there is no bill.")).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: "Anthropic" }));
    expect(screen.getByText("Anthropic serves the model, on their hardware and their bill.")).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: "OpenAI" }));
    expect(screen.getByText("OpenAI serves the model, on their hardware and their bill.")).toBeTruthy();
  });

  it("says nothing about how a vendor proves who this cluster is", () => {
    // THE PIN, and it is a durability claim rather than a copy one. A
    // sentence here naming a key or a federation is a sentence that goes
    // stale the next time either changes -- one of them was false the day it
    // was written -- and the credential belongs to the panel that sets it up.
    mount("owner");
    for (const vendor of ["Anthropic", "OpenAI"]) {
      fireEvent.click(screen.getByRole("radio", { name: vendor }));
      const said = document.body.textContent ?? "";
      expect(said).not.toMatch(/API key|federat/i);
    }
  });

  // THE PAYLOAD CARRIES THE CHECKBOX (epic memql#5103, design D5). The act
  // this button belongs to is "serve a model from a machine you own", and a
  // pairing panel that then asks the person to remember to tick a box has
  // handed the last step back. The shape is an OBJECT rather than the boolean
  // this used to send, so a merely-truthy payload cannot pre-select a
  // several-gigabyte download; MachinesSection changed in the same commit and
  // there is no compatibility branch, per the repo's no-shims rule.
  it("opens Fleet at Machines with the Add machine panel and the local-models box ticked", () => {
    const calls = mount("owner");
    fireEvent.click(screen.getByRole("button", { name: "Open Fleet" }));
    expect(calls).toEqual([["fleet", "machines", { addMachine: { inference: true } }]]);
  });

  it("opens Doors at the named vendor, for each federation door", () => {
    const calls = mount("owner");
    fireEvent.click(screen.getByRole("radio", { name: /Anthropic/ }));
    fireEvent.click(screen.getByRole("button", { name: "Open Doors" }));
    fireEvent.click(screen.getByRole("radio", { name: /OpenAI/ }));
    fireEvent.click(screen.getByRole("button", { name: "Open Doors" }));
    expect(calls).toEqual([
      ["settings", "providers", { vendor: "anthropic" }],
      ["settings", "providers", { vendor: "openai" }],
    ]);
  });
});

describe("the doors a developer is offered", () => {
  it("gets the fleet door in full: Fleet's Machines section has no role floor", () => {
    const calls = mount("developer");
    fireEvent.click(screen.getByRole("button", { name: "Open Fleet" }));
    expect(calls).toEqual([["fleet", "machines", { addMachine: { inference: true } }]]);
  });

  // THE PREDICTION IN THE TEST BELOW CAME TRUE IN THE SAME RELEASE.
  //
  // This asserted that a developer gets WORDS for a federation door, "never a
  // button that would be refused", because the providers section was
  // owner-only. Epic memql#5088's D7 widened that section to
  // owner-or-developer -- a developer helps an owner through setup -- so the
  // button is no longer one that would be refused, and withholding it would
  // now be the defect.
  //
  // The stop needed NO edit for this, which is what the second test was
  // pinning: it asks the registry rather than restating the rule.
  it("gets the BUTTON for a federation door, since a developer may federate", () => {
    const calls = mount("developer");
    fireEvent.click(screen.getByRole("radio", { name: /Anthropic/ }));
    fireEvent.click(screen.getByRole("button", { name: "Open Doors" }));
    expect(calls).toEqual([["settings", "providers", { vendor: "anthropic" }]]);
    expect(
      screen.queryByText("An owner can set Anthropic up in Settings, under Doors."),
    ).toBeNull();
  });

  it("asks the REGISTRY rather than restating who may federate", () => {
    // The pin, and it did its job: the section's manifest widened in
    // memql#5088 and the developer got the button with no edit to the stop.
    // That is the whole reason the check is a lookup.
    //
    // A RESOURCE, seeded on owner and developer and NOT admin: this repo's
    // ladder ranks developer (300) ABOVE admin (200), so a floor could never
    // say "developer but not admin", and the seeds say it row by row.
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.find((s) => s.id === "providers")?.requires).toBe("app:settings/providers");
    expect(rolesOpening("app:settings/providers")).toEqual(["owner", "developer"]);
  });
});

describe("with no shell to open into", () => {
  it("names both destinations in words", () => {
    render(<InferenceStop />);
    expect(screen.getByText("Pair a machine in Fleet, under Machines.")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: /Anthropic/ }));
    expect(screen.getByText("An owner can set Anthropic up in Settings, under Doors.")).toBeTruthy();
  });
});
