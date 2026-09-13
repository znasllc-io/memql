import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { coreAt, readiness, withOs, withSetupFacts } from "./harness";
import { fakeConnection, passkeyRow, withSession } from "../cluster/harness";
import { EXIT_HOLD_MS, SetupGate } from "../../src/apps/setup/SetupGate";
import { SetupWidget } from "../../src/apps/setup/SetupWidget";
import { setupWidget } from "../../src/apps/setup/manifest";
import { rolesOpening } from "../seededAccess";
import { WidgetHost } from "../../src/widgets/WidgetFrame";

// THE WIDGET, THROUGH ITS REAL HOST. Mounted as the desk mounts it -- gate,
// frame, body -- because the claim that matters most about this surface is
// what it draws when it has nothing to say, and the frame is the part that
// would draw anyway.

afterEach(() => {
  cleanup();
  h.connection = null;
});

const NO_PASSKEY = fakeConnection({ passkeysForSelf: [] });
const ONE_PASSKEY = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });

function mount(
  { role = "owner", feed = coreAt("unconfigured", "unconfigured", "unconfigured"), retire = vi.fn() } = {},
) {
  const view = render(
    withSession(
      withSetupFacts(withOs(<WidgetHost manifest={setupWidget} onRemove={retire} />, role)),
      { role: role, readiness: feed },
    ),
  );
  return { view, retire };
}

describe("what the wizard draws when it cannot say", () => {
  beforeEach(() => {
    h.connection = NO_PASSKEY;
  });

  it("draws NOTHING -- not even the frame -- while the readiness feed has not loaded", async () => {
    mount({ feed: readiness(false, []) });
    expect(document.querySelector("[data-os-widget='setup']")).toBeNull();
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();
    // Settled, and STILL nothing: an unloaded feed is not a state the passkey
    // read can rescue, so the wizard stays silent rather than drawing a rail
    // of one stop it happens to know about.
    await waitFor(() => expect(NO_PASSKEY.query.passkeysForSelf).toHaveBeenCalled());
    expect(document.querySelector("[data-os-widget='setup']")).toBeNull();
  });

  it("draws nothing while the passkey read has not landed, and the rail once it has", async () => {
    // The connection answers, but nothing has awaited it yet: this is the
    // first paint, and a rail that rearranged itself a tick later would move
    // under somebody's cursor.
    mount();
    expect(document.querySelector("[data-os-widget='setup']")).toBeNull();
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
  });

  it("draws nothing when the feed is loaded and names no core module", async () => {
    mount({ feed: readiness(true, []) });
    await waitFor(() => expect(NO_PASSKEY.query.passkeysForSelf).toHaveBeenCalled());
    expect(document.querySelector("[data-os-widget='setup']")).toBeNull();
  });
});

describe("the rail a fresh cluster shows", () => {
  beforeEach(() => {
    h.connection = NO_PASSKEY;
  });

  it("names the four stops, with the passkey first and open", async () => {
    mount();
    const rail = await screen.findByRole("list", { name: "Set up this cluster" });
    expect([...rail.querySelectorAll(".os-rail-label")].map((el) => el.textContent)).toEqual([
      "Your passkey",
      "Inference",
      "Storage",
      "Email sender",
    ]);
    expect([...rail.querySelectorAll("li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "open",
      "waiting",
      "waiting",
      "waiting",
    ]);
  });

  it("says how much is left and that the card goes when it is done", async () => {
    mount();
    expect(
      await screen.findByText(
        "4 things left before this cluster can do useful work. This card goes when they are done.",
      ),
    ).toBeTruthy();
  });

  it("counts what is OUTSTANDING, not what is on the rail", async () => {
    // Which is why the lead makes no claim about ordering: the stop the one
    // ordering law names leaves the count as soon as it is done, and a
    // sentence saying "in any order after the first" would then name a first
    // that is no longer there.
    h.connection = ONE_PASSKEY;
    mount();
    expect(
      await screen.findByText(
        "3 things left before this cluster can do useful work. This card goes when they are done.",
      ),
    ).toBeTruthy();
  });

  it("offers the passkey act on the open stop, and says where it goes", async () => {
    mount();
    expect(await screen.findByRole("button", { name: "Register a passkey" })).toBeTruthy();
    expect(screen.getByText("This opens the identity service. You will land back here.")).toBeTruthy();
  });

  it("moves the open stop to inference once a passkey is held", async () => {
    h.connection = ONE_PASSKEY;
    mount();
    const rail = await screen.findByRole("list", { name: "Set up this cluster" });
    await waitFor(() =>
      expect([...rail.querySelectorAll("li")].map((li) => li.getAttribute("data-state"))).toEqual([
        "done",
        "open",
        "waiting",
        "waiting",
      ]),
    );
    // ...and the inference stop's three doors are what it holds.
    expect(screen.getByRole("radio", { name: /A machine on your fleet/ })).toBeTruthy();
  });

  it("makes a SETTLED stop no disclosure at all -- no chevron, nothing to open", async () => {
    // Three of the four have nothing behind them once they are done, so a
    // chevron there opens an empty body. The rail's own rule, applied to the
    // finished end as well as the unreached one.
    h.connection = ONE_PASSKEY;
    mount();
    const rail = await screen.findByRole("list", { name: "Set up this cluster" });
    await waitFor(() =>
      expect(rail.querySelector('li[data-state="done"]')).not.toBeNull(),
    );
    const done = rail.querySelector('li[data-state="done"]') as HTMLElement;
    expect(done.querySelector("button")).toBeNull();
    // ...and it still reads as a row: the name and the state word in the same
    // columns as every other stop's. Falling back to the plain label-and-note
    // form would have replaced "Set up" with the stop's whole sentence.
    expect(done.querySelector(".os-rail-label")?.textContent).toBe("Your passkey");
    expect(done.querySelector(".os-rail-answer")?.textContent).toBe("Set up");
    expect(done.querySelector(".os-rail-chev")).toBeNull();
    // ...while the one with something to do still is one.
    const waiting = rail.querySelector('li[data-state="open"]') as HTMLElement;
    expect(waiting.querySelector("button")).not.toBeNull();
  });

  it("names the deployment variables on the storage stop rather than offering a button", async () => {
    h.connection = ONE_PASSKEY;
    render(
      withSession(
        withSetupFacts(withOs(<WidgetHost manifest={setupWidget} onRemove={vi.fn()} />, "owner")),
        {
          role: "owner",
          readiness: readiness(true, [
            { ...vFor("ai", "configured"), core: true },
            {
              ...vFor("storage", "unconfigured"),
              core: true,
              lanes: [
                {
                  name: "azure-blob",
                  configurableFrom: "deployment",
                  complete: false,
                  slots: [
                    { name: "MEMQL_AZURE_BLOB_CONTAINER", present: true, source: "env" },
                    { name: "MEMQL_AZURE_STORAGE_CONNECTION_STRING", present: false, source: "unset" },
                  ],
                },
              ],
            },
            { ...vFor("email", "configured"), core: true },
          ]),
        },
      ),
    );
    expect(await screen.findByText("Set in the deployment, not from here.")).toBeTruthy();
    // Only the slot that is still MISSING: the one already set is not work.
    expect(screen.getByText("MEMQL_AZURE_STORAGE_CONNECTION_STRING")).toBeTruthy();
    expect(screen.queryByText(/MEMQL_AZURE_BLOB_CONTAINER/)).toBeNull();
  });
});

describe("who gets the wizard", () => {
  beforeEach(() => {
    h.connection = NO_PASSKEY;
  });

  it("a developer gets the rail, with words instead of a federation button", async () => {
    h.connection = ONE_PASSKEY;
    mount({ role: "developer" });
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
    // The fleet door is offered in full -- Fleet's Machines section has no
    // role floor -- and that is why it comes first.
    expect(screen.getByRole("button", { name: "Open Fleet" })).toBeTruthy();
  });

  it("the manifest names its resource, seeded on owner and developer, which is what the desk enforces", () => {
    expect(setupWidget.requires).toBe("app:setup");
    expect(rolesOpening("app:setup")).toEqual(["owner", "developer"]);
  });
});

describe("the retire rule", () => {
  it("goes at once, drawing nothing at all, on a cluster that was already set up", async () => {
    h.connection = ONE_PASSKEY;
    const { retire } = mount({ feed: coreAt("configured", "configured", "configured") });
    await waitFor(() => expect(retire).toHaveBeenCalledTimes(1));
    // NOTHING WAS DRAWN. A configured cluster must never flash a wizard, not
    // even for the frame of an exit animation.
    expect(document.querySelector("[data-os-widget='setup']")).toBeNull();
    expect(document.querySelector(".os-setup-gate")).toBeNull();
  });

  it("holds the finished rail for a beat before it folds, when somebody finished it", async () => {
    vi.useFakeTimers();
    try {
      h.connection = ONE_PASSKEY;
      const retire = vi.fn();
      const view = render(at(coreAt("unconfigured", "configured", "configured"), retire));
      // Let the passkey read settle. Inside `act`, because a fake-timer clock
      // does not make React's queue anybody else's problem.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByRole("list", { name: "Set up this cluster" })).toBeTruthy();

      // The last stop lands.
      view.rerender(at(coreAt("configured", "configured", "configured"), retire));

      // STILL ON SCREEN, with every mark lit and the sentence that says so.
      // This is the whole claim: finishing the last stop is acknowledged by
      // the rail itself, not by a screen somebody has to dismiss.
      expect(document.querySelector('.os-setup-gate[data-exiting="true"]')).not.toBeNull();
      expect(screen.getByText("The core is set up. This card goes now.")).toBeTruthy();
      expect([...document.querySelectorAll(".os-rail-stage")].map((li) => li.getAttribute("data-state"))).toEqual([
        "done",
        "done",
        "done",
        "done",
      ]);
      expect(retire).not.toHaveBeenCalled();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(EXIT_HOLD_MS);
      });
      expect(retire).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  function at(feed: ReturnType<typeof coreAt>, retire: () => void) {
    return withSession(
      withSetupFacts(withOs(<WidgetHost manifest={setupWidget} onRemove={retire} />, "owner")),
      { role: "owner", readiness: feed },
    );
  }
});

describe("the body alone", () => {
  it("renders nothing outside its gate, which is the only thing that decides it draws", () => {
    render(withSession(withOs(<SetupWidget />, "owner"), { role: "owner" }));
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();
  });

  it("is the manifest's component, and SetupGate is its gate", () => {
    expect(setupWidget.component).toBe(SetupWidget);
    expect(setupWidget.gate).toBe(SetupGate);
  });
});

function vFor(module: string, state: "configured" | "unconfigured" | "partial" | "unreported") {
  return { module, state, core: true, disagreement: [] as string[], nodes: [], lanes: [] };
}
