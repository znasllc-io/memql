import { act, render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FleetSettings, FleetSettingsStore } from "../../src/apps/fleet/settings";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MachinesProvider } = await import("../../src/live/machines");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { DEFAULT_FLEET_SETTINGS } = await import("../../src/apps/fleet/settings");
const { fakeConnection, withSession } = await import("./harness");

// ARRIVING FROM THE WIZARD (epic memql#5106). The first-run wizard's fleet
// door opens Fleet at Machines with `{ addMachine: { inference: true } }`, and
// this is the receiving half: the guided install (design record
// 2026-09-08-cockpit-install-wizard) is already open when they land, with
// "Run local models" already ticked.
//
// LANDING ON THE LIST WITH THE BUTTON STILL TO FIND is what this prevents.
// The act a person took was "pair a machine that serves a model" -- delivering
// them to a directory of machines completes about half of it, and delivering
// them to the panel with the model box unticked completes most of the rest and
// then hands the last step back.
//
// THE PAYLOAD IS AN OBJECT, AND THAT IS THE SAFETY PROPERTY (epic memql#5103).
// Presence of the object opens the panel; `inference` inside it is a separate
// question. A boolean payload could be satisfied by any truthy value, and the
// thing it would be pre-selecting is a several-gigabyte download onto somebody
// else's machine. `{ addMachine: true }` was the old shape and is now one of
// the malformed ones -- there is no compatibility branch, per the repo's
// no-shims rule, and the producer changed in the same commit.

afterEach(cleanup);

function memoryStore(initial: FleetSettings): FleetSettingsStore {
  let held = initial;
  return { load: () => held, save: (next) => void (held = next) };
}

async function open(payload: Record<string, unknown> | null) {
  h.connection = fakeConnection({ myWorkersWithStatus: [] });
  const consume = vi.fn();
  render(
    withSession(
      <MachinesProvider>
        <FleetApp
          sectionId="machines"
          navigate={vi.fn()}
          askContext={vi.fn()}
          store={memoryStore(DEFAULT_FLEET_SETTINGS)}
          intent={payload === null ? undefined : { id: "intent-3", payload }}
          consumeIntent={consume}
        />
      </MachinesProvider>,
    ),
  );
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return { consume };
}

describe("Fleet, opened to add a machine", () => {
  it("opens the guided install and consumes the intent by id", async () => {
    const { consume } = await open({ addMachine: {} });
    expect(screen.getByRole("region", { name: "Add a machine" })).toBeTruthy();
    // The page replaced the list: its Head carries the way back, and the
    // list's own Add control is gone with the list.
    expect(screen.getByRole("button", { name: "Back to Machines" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add a machine" })).toBeNull();
    expect(consume).toHaveBeenCalledExactlyOnceWith("intent-3");
  });

  // ===========================================================================
  // THE FLAG HAS TO SURVIVE THE SEAM, NOT JUST THE PANEL HAVE OPENED
  // ===========================================================================
  // The panel opening and the box being ticked are two different things, and
  // between them sits a prop. Asserting only the first would stay green while
  // the flag was dropped -- a person arriving from the wizard's "serve a model"
  // door would get a pairing panel that pairs a machine which runs no models,
  // and nothing anywhere would say so.
  it("pre-selects the local-models box when the intent asks for it", async () => {
    await open({ addMachine: { inference: true } });
    const box = screen.getByRole("switch", { name: /Run local models/i });
    expect((box as HTMLInputElement).checked).toBe(true);
  });

  it("leaves the box unticked for a bare open request", async () => {
    await open({ addMachine: {} });
    const box = screen.getByRole("switch", { name: /Run local models/i });
    expect((box as HTMLInputElement).checked).toBe(false);
  });

  // ===========================================================================
  // AN INTENT CONSUMED ONCE MUST NOT RE-ARM
  // ===========================================================================
  // A person who arrived from the wizard's inference door, left the page and
  // opened it again from the Head must NOT find "Run local models"
  // ticked again: a several-gigabyte download pre-selected by an intent that
  // was spent minutes earlier, with nothing on screen explaining why. The
  // hook starts every flow from an empty draft unless the START says
  // otherwise, which is what makes this hold.
  it("does not re-tick the box when the page is left and re-opened", async () => {
    await open({ addMachine: { inference: true } });
    expect(
      (screen.getByRole("switch", { name: /Run local models/i }) as HTMLInputElement)
        .checked,
    ).toBe(true);

    // Before a mint, the Head's arrow leaves at once: nothing was created.
    await act(async () => {
      screen.getByRole("button", { name: "Back to Machines" }).click();
    });
    expect(screen.queryByRole("region", { name: "Add a machine" })).toBeNull();

    await act(async () => {
      screen.getByRole("button", { name: "Add a machine" }).click();
    });
    expect(
      (screen.getByRole("switch", { name: /Run local models/i }) as HTMLInputElement)
        .checked,
    ).toBe(false);
  });

  it("leaves the panel closed when the shell hands it no intent", async () => {
    const { consume } = await open(null);
    expect(screen.queryByRole("region", { name: "Add a machine" })).toBeNull();
    expect(consume).not.toHaveBeenCalled();
  });

  it("leaves it closed for an intent that says something else", async () => {
    // Fleet's Logs section takes intents of its own, and a truthiness test
    // here would open the pairing panel for one of those.
    const { consume } = await open({ logLineId: "v1:observability:logLine:x" });
    expect(screen.queryByRole("region", { name: "Add a machine" })).toBeNull();
    expect(consume).not.toHaveBeenCalled();
  });

  // Every one of these is a value that would have passed a truthiness test.
  // `true` is here because it was the SUPPORTED shape until this epic: after
  // the change it is malformed like the rest, and pinning it stops the old
  // form from being quietly re-accepted by a future "be lenient" edit.
  it.each([
    ["a string", "yes"],
    ["a number", 1],
    ["the old boolean", true],
    ["an array", []],
  ])("is not fooled by %s", async (_name, value) => {
    const { consume } = await open({ addMachine: value });
    expect(screen.queryByRole("region", { name: "Add a machine" })).toBeNull();
    expect(consume).not.toHaveBeenCalled();
  });
});
