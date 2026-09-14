import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import type { ReactNode } from "react";

import { coreAt, readiness, withOs } from "../setup/harness";
import { fakeConnection, passkeyRow, withSession } from "../cluster/harness";
import { SetupFactsScope } from "../../src/apps/setup/SetupFactsScope";
import { CoreGate } from "../../src/chrome/CoreGate";
import { useOs } from "../../src/chrome/state";
import type { Readiness } from "../../src/live/readiness";

// THE CORE GATE, mounted the way Shell.tsx mounts it: inside the facts scope
// AND INSIDE THE SHELL PROVIDER, with the desk as its children.
//
// The provider is not scenery. The gate's stops offer their acts through
// `useAppReach`, which reads `useOsIfPresent()` -- so a harness that left the
// provider out would render every act as its words-only fallback and every
// case here would still pass, while production shipped a full-screen gate
// whose only working control was Sign out. `mountsInsideTheShell` below is the
// case that pins the composition itself.
//
// What every other case asserts is which of the three things is on screen --
// the rail, the sentence, or the desk -- because that is the whole of what
// this surface decides.

afterEach(() => {
  cleanup();
  h.connection = null;
});

const DESK = <p data-testid="desk">the desk</p>;

const READER_SENTENCE = "An owner or developer has to set up inference before anyone can use it.";

function gate(role: string, feed: Readiness, ladderLoaded = true): ReactNode {
  return withSession(
    <SetupFactsScope>
      {withOs(<CoreGate onSignOut={() => {}}>{DESK}</CoreGate>, role)}
    </SetupFactsScope>,
    { role: role, readiness: feed, ladderLoaded },
  );
}

const UNCONFIGURED = () => coreAt("unconfigured", "configured", "configured");

/** Opens and closes a real window, the way the gate's own acts do. */
function WindowDriver() {
  const { actions, state } = useOs();
  return (
    <>
      <button type="button" onClick={() => actions.openApp("fleet", "machines")}>
        open fleet
      </button>
      <button
        type="button"
        onClick={() => Object.keys(state.shell.windows).forEach((id) => actions.closeWindow(id))}
      >
        close all
      </button>
    </>
  );
}

describe("what the gate does while it cannot say", () => {
  it("OPENS THE DESK while the feed has not loaded, and draws no gate", async () => {
    // ONLY POSITIVE EVIDENCE HOLDS. A shell that drew nothing until the feed
    // seeded would put a blank ground in front of every person on every boot
    // of every cluster, to spare the rare unconfigured one a brief desk. The
    // record's own words are "the gate draws nothing, the shell opens".
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", readiness(false, [])));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(document.querySelector("[data-os-core-gate]")).toBeNull();
  });

  it("draws nothing at all while the ladder has not landed AND the cluster is unconfigured", async () => {
    // The one place it fails CLOSED, and only because the question here is
    // WHICH VARIANT. The cluster is decidedly unconfigured, so the desk must
    // not open -- and telling an owner to go and find an owner is worse than a
    // beat of ground.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", UNCONFIGURED(), false));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId("desk")).toBeNull();
    expect(document.querySelector("[data-os-core-gate]")).toBeNull();
  });

  it("still opens the desk with no ladder when the cluster IS configured", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", coreAt("configured", "configured", "configured"), false));
    expect(await screen.findByTestId("desk")).toBeTruthy();
  });
});

describe("what the gate lets through", () => {
  it("opens the desk when the feed loads and names no core module", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", readiness(true, [])));
    expect(await screen.findByTestId("desk")).toBeTruthy();
  });

  it("opens the desk when ai is UNREPORTED", async () => {
    // A BROKEN CLUSTER IS NOT AN UNCONFIGURED ONE. The feed loaded and no live
    // node reported `ai`; the shell opens and the Readiness signpost says
    // nobody reported. A gate that claimed otherwise would lock somebody out
    // of the Cluster app they need in order to go and fix it.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", coreAt("unreported", "configured", "configured")));
    expect(await screen.findByTestId("desk")).toBeTruthy();
  });

  it("opens the desk when ai is configured but another core module is not", async () => {
    // D2: the gate keys on inference ALONE. Storage is always configured on a
    // local cluster and mail is log-only there by design, and neither is
    // needed to use the OS.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", coreAt("configured", "unconfigured", "unconfigured")));
    expect(await screen.findByTestId("desk")).toBeTruthy();
  });

  it("opens the desk on a partial ai verdict", async () => {
    // `partial` is a mid-rollout disagreement, not a cluster with no door.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", coreAt("partial", "configured", "configured")));
    expect(await screen.findByTestId("desk")).toBeTruthy();
  });
});

describe("the two role variants", () => {
  it("holds an owner on the rail, with no desk, dock or window behind it", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(gate("owner", UNCONFIGURED()));
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();
    // BEHIND THE GATE THERE IS NO APP, DOCK OR LAUNCHER (D6): the gate renders
    // in PLACE of its children, so none of it is even mounted.
    expect(document.querySelector("[data-os-root]")).toBeNull();
    expect(document.querySelector("[data-os-window]")).toBeNull();
  });

  // THE ONE THING THE VERDICT CANNOT SAY FOR ITSELF (memql#5259). The nodes
  // that voted found no door; a node that could not read the fleet did not
  // vote, and the owner looking at a held gate is told so -- one quiet line,
  // no node ids, and only while the gate holds.
  it("says quietly when some nodes could not read the fleet", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    const feed = UNCONFIGURED();
    const ai = feed.of("ai")!;
    ai.unknown = ["bff-a", "edge-a"];
    render(gate("owner", feed));
    expect(await screen.findByText(/2 nodes could not read the fleet just now/)).toBeTruthy();
    expect(screen.queryByText(/bff-a|edge-a/)).toBeNull();
  });

  it("says nothing extra when every node voted", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(gate("owner", UNCONFIGURED()));
    await screen.findByRole("list", { name: "Set up this cluster" });
    expect(document.querySelector("[data-os-core-gate-unknown]")).toBeNull();
  });

  it("gives a developer the same rail", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(gate("developer", UNCONFIGURED()));
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
  });

  it("opens the INFERENCE stop, not the passkey, and keeps the passkey first", async () => {
    // The one place the gate departs from the widget's rail: this surface
    // exists because inference is missing, and nothing else on it lifts the
    // gate. The passkey-first LAW is untouched -- the stop is still first.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", UNCONFIGURED()));
    const list = await screen.findByRole("list", { name: "Set up this cluster" });
    const names = Array.from(list.querySelectorAll("li")).map((li) => li.textContent ?? "");
    expect(names[0]).toContain("Your passkey");
    // The inference stop's body is its three doors; their presence is what
    // says it is the open one.
    expect(await screen.findByText("A machine on your fleet")).toBeTruthy();
  });

  it("gives every other role one sentence and Sign out, and never names the owner", async () => {
    for (const role of ["writer", "reader", "admin", ""]) {
      cleanup();
      h.connection = fakeConnection({ passkeysForSelf: [] });
      render(gate(role, UNCONFIGURED()));
      expect(await screen.findByText("MemQL is not set up yet")).toBeTruthy();
      expect(screen.getByText(READER_SENTENCE)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
      expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();
      expect(screen.queryByTestId("desk")).toBeNull();
      // A person's name is not the cluster's to publish (D6). The harness's
      // own signed-in identity is the one thing that could leak here.
      expect(document.body.textContent).not.toContain("me@example.com");
    }
  });

  it("OFFERS A WORKING ACT, not prose pointing at an app it did not mount", async () => {
    // THE CASE THAT WOULD HAVE CAUGHT THE COMPOSITION BUG. The gate was
    // mounted OUTSIDE ShellRoster, so `useAppReach` read a null shell, every
    // section list was empty, and the inference stop's act degraded to its
    // words-only fallback: "Pair a machine in Fleet, under Machines" -- prose
    // pointing at an app the gate had not mounted. On a local cluster, which
    // reaches no federation by design, that left an owner with Sign out as the
    // only working control on a screen demanding they set up inference.
    //
    // Every other case here passed against that, which is why this one asserts
    // the ACT rather than the label above it.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", UNCONFIGURED()));
    expect(await screen.findByRole("button", { name: "Open Fleet" })).toBeTruthy();
    // And the fallback prose is NOT on screen -- its presence is the symptom.
    expect(screen.queryByText(/Pair a machine in Fleet, under Machines/)).toBeNull();
  });

  it("steps aside for the window its own act opened, and comes back when it closes", async () => {
    // A HOLD, NOT A PRISON. `openApp` puts a window in the shell's state, and
    // a window is drawn by the desk this renders in place of -- so the gate
    // has to yield to it, or the act it just offered goes nowhere visible.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(
      withSession(
        <SetupFactsScope>
          {withOs(
            <>
              <CoreGate onSignOut={() => {}}>{DESK}</CoreGate>
              <WindowDriver />
            </>,
            "owner",
          )}
        </SetupFactsScope>,
        { role: "owner", readiness: UNCONFIGURED() },
      ),
    );
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "open fleet" }));
    await waitFor(() => expect(screen.getByTestId("desk")).toBeTruthy());
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();

    // Closed with the door still shut: the gate returns rather than leaving
    // somebody on a desk they were held off.
    fireEvent.click(screen.getByRole("button", { name: "close all" }));
    await waitFor(() => expect(screen.getByRole("list", { name: "Set up this cluster" })).toBeTruthy());
  });

  it("offers the owner a way out too", async () => {
    // An owner held on a surface whose only exit is closing the tab is a small
    // trap. It is not one of the rail's acts, and it is not styled as one.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(gate("owner", UNCONFIGURED()));
    expect(await screen.findByRole("button", { name: "Sign out" })).toBeTruthy();
  });
});

describe("lifting", () => {
  it("lifts on a feed change, with no reload", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    const view = render(gate("owner", UNCONFIGURED()));
    expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();

    view.rerender(gate("owner", coreAt("configured", "configured", "configured")));
    await waitFor(() => expect(screen.getByTestId("desk")).toBeTruthy());
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();
  });

  it("lifts for a reader too, without them doing anything", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [] });
    const view = render(gate("reader", UNCONFIGURED()));
    expect(await screen.findByText(READER_SENTENCE)).toBeTruthy();
    view.rerender(gate("reader", coreAt("configured", "configured", "configured")));
    await waitFor(() => expect(screen.getByTestId("desk")).toBeTruthy());
  });

  it("comes back when the door goes away again", async () => {
    // A cluster can stop being configured: a machine is revoked, federation is
    // cleared. The verdict IS the state, so the gate returns.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    const view = render(gate("reader", coreAt("configured", "configured", "configured")));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    view.rerender(gate("reader", UNCONFIGURED()));
    await waitFor(() => expect(screen.getByText(READER_SENTENCE)).toBeTruthy());
    expect(screen.queryByTestId("desk")).toBeNull();
  });
});
