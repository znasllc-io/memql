import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { coreAt, readiness, withOs } from "./harness";
import { fakeConnection, passkeyRow, withSession } from "../cluster/harness";
import { SetupFactsScope } from "../../src/apps/setup/SetupFactsScope";
import { SetupPresence } from "../../src/apps/setup/SetupPresence";
import { OS_REGISTRY } from "../../src/apps/registry";
import { seedDocument, useOs } from "../../src/chrome/state";
import type { Readiness } from "../../src/live/readiness";
import { installSeededAccess } from "../seededAccess";

afterEach(() => {
  cleanup();
  h.connection = null;
  // THE DESK PERSISTS. `withOs` builds a LocalDesktopStore, which reads and
  // writes localStorage, and vitest isolates per FILE rather than per test --
  // so without this the desk one case left behind is the desk the next one
  // starts from, and "absent" cases pass or fail on their neighbour's work.
  localStorage.clear();
});

// THE SEED PLACES ASK ALONE (design record
// 2026-09-07-core-gate-and-honest-install, D4).
//
// `seedDocument` runs in a React state initializer, before the person's role,
// the ladder or the feed has landed, and `roleAdmits("")` refuses every
// requirement -- so the seeded wizard was placed for NOBODY on every
// production boot, while the file's own header described that as an edge case.
describe("what a fresh desk is seeded with", () => {
  it("is exactly [ask], for every role", () => {
    for (const role of ["owner", "developer", "admin", "reader", ""]) {
      const doc = seedDocument(OS_REGISTRY, { cols: 12, rows: 8 }, role);
      const ids = Object.values(doc.surfaces)
        .flatMap((s) => Object.values(s.items))
        .filter((i) => i.kind === "widget")
        .map((i) => (i as { widgetId: string }).widgetId);
      expect(ids).toEqual(["ask"]);
    }
  });
});

/** The widget ids on the active desk, so an assertion reads as its question. */
function Roster() {
  const { state } = useOs();
  const surface = state.surfaces[state.shell.activeDeskId];
  const ids = Object.values(surface?.items ?? {})
    .filter((i) => i.kind === "widget")
    .map((i) => (i as { widgetId: string }).widgetId);
  return <p data-testid="roster">{ids.join(",")}</p>;
}

/** Takes the setup widget off the active desk, the way its `...` menu does. */
function Remover() {
  const { actions, state } = useOs();
  const surface = state.surfaces[state.shell.activeDeskId];
  const item = Object.values(surface?.items ?? {}).find(
    (i) => i.kind === "widget" && (i as { widgetId: string }).widgetId === "setup",
  );
  return (
    <button type="button" onClick={() => item && actions.removeWidget(item.id)}>
      remove setup
    </button>
  );
}

function mounted(role: string, feed: Readiness) {
  installSeededAccess(role);
  return withSession(
    <SetupFactsScope>
      {withOs(
        <>
          <SetupPresence />
          <Roster />
        </>,
        role,
      )}
    </SetupFactsScope>,
    { role: role, readiness: feed },
  );
}

const roster = () => screen.getByTestId("roster").textContent ?? "";

/** Long enough for the passkey read and the effect to have settled. */
async function settle() {
  await waitFor(() => expect(roster()).toContain("ask"));
  await new Promise((r) => setTimeout(r, 30));
}

describe("the widget's presence follows the feed and the ladder", () => {
  it("is ensured for an owner once the feed and the ladder have landed", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(mounted("owner", coreAt("unconfigured", "configured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));
  });

  it("is ensured for a developer, who can act on the stops too", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(mounted("developer", coreAt("unconfigured", "configured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));
  });

  it("is absent for a reader, who would be refused at every stop", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(mounted("reader", coreAt("unconfigured", "configured", "configured")));
    await settle();
    expect(roster()).not.toContain("setup");
  });

  it("is absent while every stop is settled", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(mounted("owner", coreAt("configured", "configured", "configured")));
    await settle();
    expect(roster()).not.toContain("setup");
  });

  it("comes back the moment one stop unsettles", async () => {
    // "IT COMES BACK ON A FRESH DESK" was documented and never implemented,
    // and this is not that: it comes back on ANY desk the moment a core stop
    // unsettles, which is the honest answer to "where did it go".
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    const view = render(mounted("owner", coreAt("configured", "configured", "configured")));
    await settle();
    expect(roster()).not.toContain("setup");

    view.rerender(mounted("owner", coreAt("configured", "unconfigured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));
  });

  it("draws nothing while the feed has not loaded", async () => {
    // NOT KNOWN IS NOT UNSETTLED. Adding it while the feed is still seeding
    // would put a wizard on the desk of a cluster that turns out to be set up.
    h.connection = fakeConnection({ passkeysForSelf: [] });
    render(mounted("owner", readiness(false, [])));
    await settle();
    expect(roster()).not.toContain("setup");
  });

  it("draws nothing while the passkey read is in flight", async () => {
    // The passkey stop is `unknown` until the read lands, and an unknown stop
    // makes the whole rail unknown -- so the widget waits with it rather than
    // appearing and then rearranging itself.
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(mounted("owner", coreAt("unconfigured", "configured", "configured")));
    expect(roster()).not.toContain("setup");
    await waitFor(() => expect(roster()).toContain("setup"));
  });

  it("does NOT put it straight back when somebody takes it off by hand", async () => {
    // THE EFFECT'S DEPS ARE THE FEED, THE LADDER, THE ROLE AND THE ACTIVE
    // DESK -- deliberately not the desk's CONTENTS. A card that reappeared the
    // instant it was removed could not be moved out of the way even for a
    // moment, which is a widget somebody would come to resent.
    //
    // It still comes back on the next change of the feed or the ladder, which
    // the case below asserts: the widget IS the state, and this is only about
    // when it is re-read.
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    render(
      withSession(
        <SetupFactsScope>
          {withOs(
            <>
              <SetupPresence />
              <Roster />
              <Remover />
            </>,
            "owner",
          )}
        </SetupFactsScope>,
        { role: "owner", readiness: coreAt("unconfigured", "configured", "configured") },
      ),
    );
    await waitFor(() => expect(roster()).toContain("setup"));

    fireEvent.click(screen.getByRole("button", { name: "remove setup" }));
    expect(roster()).not.toContain("setup");
    await new Promise((r) => setTimeout(r, 40));
    expect(roster()).not.toContain("setup");
  });

  it("...and DOES put it back on the next reading of the feed", async () => {
    // The other half, and the one that makes the widget the state rather than
    // a note about it. A re-render carrying a NEW feed object is what a live
    // collection produces on every change; the effect re-runs on it and the
    // card returns, even though the verdicts themselves did not move.
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    const tree = (feed: Readiness) =>
      withSession(
        <SetupFactsScope>
          {withOs(
            <>
              <SetupPresence />
              <Roster />
              <Remover />
            </>,
            "owner",
          )}
        </SetupFactsScope>,
        { role: "owner", readiness: feed },
      );
    const view = render(tree(coreAt("unconfigured", "configured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));

    fireEvent.click(screen.getByRole("button", { name: "remove setup" }));
    expect(roster()).not.toContain("setup");

    // A DIFFERENT OBJECT, THE SAME ANSWER -- exactly what the live feed hands
    // down when any readiness row is rewritten.
    view.rerender(tree(coreAt("unconfigured", "configured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));
  });

  it("adds it once, however many times the feed changes", async () => {
    h.connection = fakeConnection({ passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
    const view = render(mounted("owner", coreAt("unconfigured", "configured", "configured")));
    await waitFor(() => expect(roster()).toContain("setup"));
    for (let i = 0; i < 3; i += 1) {
      view.rerender(mounted("owner", coreAt("unconfigured", "configured", "configured")));
    }
    await new Promise((r) => setTimeout(r, 30));
    expect(roster().split(",").filter((id) => id === "setup")).toHaveLength(1);
  });
});
