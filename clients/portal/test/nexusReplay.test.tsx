// Replay: the goal's own history, scrubbable.
//
// Two altitudes again. The scrubber's arithmetic is pure (src/nexus/replay/
// timeline.ts) -- where a moment maps to, what the ends do, what a position
// means -- and is asserted directly. The page then covers what only the page
// can be wrong about: the `?at=` round trip, the keyboard, and the fact that
// the event list is the map's accessible index rather than a decoration
// beside it.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";

import { events as buildEvents } from "../src/nexus/scene/events";
import { layout } from "../src/nexus/scene/layout";
import {
  BEFORE_FIRST,
  atForIndex,
  clampIndex,
  indexForAt,
  isLive,
  phaseMarks,
  sceneMomentFor,
  stepMs,
} from "../src/nexus/replay/timeline";
import { PLAYBACK_SPEEDS } from "../src/nexus/map/motion";
import { MOMENT, emptyGoal, springCatalogGoal } from "../src/nexus/scene/fixtures";
import { nexusHarness, renderNexus } from "./support/nexusHarness";

const WORLD = springCatalogGoal();
const TIMELINE = buildEvents(WORLD);

describe("the scrubber's arithmetic", () => {
  it("clamps to both ends, and the low end is BEFORE the first event", () => {
    expect(clampIndex(TIMELINE, -99)).toBe(BEFORE_FIRST);
    expect(clampIndex(TIMELINE, 0)).toBe(0);
    expect(clampIndex(TIMELINE, 99_999)).toBe(TIMELINE.length - 1);
    // An empty timeline has exactly one position: before everything.
    expect(clampIndex([], 4)).toBe(BEFORE_FIRST);
  });

  it("maps a moment to the last event at or before it", () => {
    expect(indexForAt(TIMELINE, "")).toBe(BEFORE_FIRST);
    expect(indexForAt(TIMELINE, MOMENT(-1))).toBe(BEFORE_FIRST);
    // Exactly AT an event's own moment includes it -- the off-by-one that
    // would otherwise make a shared link land one event short.
    const third = TIMELINE[2]!;
    expect(indexForAt(TIMELINE, third.at)).toBeGreaterThanOrEqual(2);
    expect(TIMELINE[indexForAt(TIMELINE, third.at)]!.at).toBe(third.at);
    expect(indexForAt(TIMELINE, MOMENT(9999))).toBe(TIMELINE.length - 1);
  });

  it("round-trips a moment through a position", () => {
    for (let i = 0; i < TIMELINE.length; i += 1) {
      const at = atForIndex(TIMELINE, i);
      // Events share timestamps, so the round trip lands on the LAST event of
      // that instant rather than on i -- and the moment is what a URL carries,
      // so the moment is what has to survive.
      expect(atForIndex(TIMELINE, indexForAt(TIMELINE, at))).toBe(at);
    }
    // Before the first event there is no moment, and none is invented.
    expect(atForIndex(TIMELINE, BEFORE_FIRST)).toBe("");
  });

  it("shows NOW at the live end and a strictly-earlier moment before the first event", () => {
    expect(sceneMomentFor(TIMELINE, TIMELINE.length - 1, true)).toBe("");
    expect(isLive(TIMELINE, TIMELINE.length - 1)).toBe(true);
    expect(isLive(TIMELINE, 0)).toBe(false);
    expect(isLive([], BEFORE_FIRST)).toBe(true);
    // Strictly earlier than every event, so the scene filters to nothing --
    // using the first event's own stamp would include it, which is a rewind
    // that stops one frame short of empty.
    expect(sceneMomentFor(TIMELINE, BEFORE_FIRST, false) < TIMELINE[0]!.at).toBe(true);
  });

  it("plays one arrival per tick, faster with the speed", () => {
    expect(stepMs(1, 0.45)).toBe(450);
    expect(stepMs(4, 0.45)).toBe(112.5);
    expect(stepMs(16, 0.45)).toBeCloseTo(28.125, 3);
    // A nonsense speed does not produce a zero-length timer.
    expect(stepMs(0, 0.45)).toBeGreaterThan(0);
    expect(PLAYBACK_SPEEDS).toEqual([1, 4, 16]);
  });

  it("marks the phase boundaries through the layout, not through label text", () => {
    const { nodes, phases } = layout(WORLD);
    const marks = phaseMarks(TIMELINE, nodes, phases.map((phase) => phase.name));
    expect(marks.map((mark) => mark.name)).toEqual(["gather", "shape", "publish"]);
    // In order along the scrubber, because the phases are.
    for (let i = 1; i < marks.length; i += 1) {
      expect(marks[i - 1]!.index).toBeLessThan(marks[i]!.index);
    }
    // A phase with no event of its own gets no mark rather than a wrong one.
    expect(phaseMarks(TIMELINE, nodes, ["a-phase-that-never-ran"])).toEqual([]);
  });
});

describe("the Replay page", () => {
  it("lists every event with its moment, newest last", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    const list = await screen.findByRole("listbox", { name: /goal events/i });
    const options = within(list).getAllByRole("option");
    expect(options).toHaveLength(TIMELINE.length);
    expect(options[0]!.textContent).toContain("Goal set");
    expect(options[options.length - 1]!.textContent).toContain("Goal reached");
  });

  it("marks a retry as a retry -- the one thing the map's re-light cannot say", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    const list = await screen.findByRole("listbox", { name: /goal events/i });
    expect(within(list).getAllByText("retry").length).toBeGreaterThan(0);
    expect(within(list).getByText(/started again \(attempt 2\)/i)).toBeTruthy();
  });

  it("opens at the live end, and says so", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    // Live rather than at the beginning: the default view of a finished goal
    // must not be an empty scene.
    await waitFor(() =>
      expect(
        screen.getByText((_, element) => (element?.textContent ?? "").startsWith("live ·")),
      ).toBeTruthy(),
    );
    expect(screen.getByTestId("location").textContent).toBe("/nexus/plan-spring/replay");
  });

  it("reads ?at= on a cold load and pins the scrubber to it", async () => {
    const moment = TIMELINE[3]!.at;
    renderNexus(nexusHarness(), `/nexus/plan-spring/replay?at=${encodeURIComponent(moment)}`);
    const slider = await screen.findByRole("slider", { name: /replay position/i });
    await waitFor(() =>
      expect(Number((slider as HTMLInputElement).value)).toBe(indexForAt(TIMELINE, moment)),
    );
    // The announced value is the MOMENT, not the index -- "17" says nothing.
    expect(slider.getAttribute("aria-valuetext")).toBe(moment);
  });

  it("writes the moment back into the URL when the scrubber moves", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    const slider = await screen.findByRole("slider", { name: /replay position/i });
    fireEvent.change(slider, { target: { value: "2" } });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toContain(
        `at=${encodeURIComponent(TIMELINE[2]!.at)}`,
      ),
    );
  });

  it("drops ?at= again at the live end, so a running goal keeps moving", async () => {
    renderNexus(nexusHarness(), `/nexus/plan-spring/replay?at=${encodeURIComponent(TIMELINE[2]!.at)}`);
    const slider = await screen.findByRole("slider", { name: /replay position/i });
    fireEvent.change(slider, { target: { value: String(TIMELINE.length - 1) } });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/nexus/plan-spring/replay"),
    );
  });

  it("rewinds to before the goal, where the scene is empty", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    fireEvent.click(await screen.findByRole("button", { name: "Rewind" }));
    const slider = screen.getByRole("slider", { name: /replay position/i });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(BEFORE_FIRST));
    expect(slider.getAttribute("aria-valuetext")).toBe("before the goal");
  });

  it("moves the scrubber from the event list's own keyboard", async () => {
    // Design 4.4: the list is the map's accessible index, so the arrow keys
    // belong to it rather than to a control beside it.
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    const list = await screen.findByRole("listbox", { name: /goal events/i });
    const slider = screen.getByRole("slider", { name: /replay position/i });

    fireEvent.keyDown(list, { key: "Home" });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(BEFORE_FIRST));
    fireEvent.keyDown(list, { key: "ArrowDown" });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(0));
    fireEvent.keyDown(list, { key: "ArrowDown" });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(1));
    fireEvent.keyDown(list, { key: "ArrowUp" });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(0));
    fireEvent.keyDown(list, { key: "End" });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBe(TIMELINE.length - 1));
  });

  it("opens the node at its own address on Enter, keeping the moment", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    const list = await screen.findByRole("listbox", { name: /goal events/i });
    fireEvent.keyDown(list, { key: "Home" });
    fireEvent.keyDown(list, { key: "ArrowDown" });
    fireEvent.keyDown(list, { key: "Enter" });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toContain("/nexus/plan-spring/replay/node/"),
    );
    // The moment survives the navigation -- a single node address under the
    // Map would have thrown it away.
    expect(screen.getByTestId("location").textContent).toContain("at=");
  });

  it("plays and pauses", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    // At the live end there is nowhere to play to, so start from the top.
    fireEvent.click(await screen.findByRole("button", { name: "Rewind" }));
    const play = screen.getByRole("button", { name: "Play" });
    fireEvent.click(play);
    await waitFor(() => expect(screen.getByRole("button", { name: "Pause" })).toBeTruthy());
    // 16x, so the assertion does not wait 450ms per event.
    fireEvent.click(screen.getByRole("button", { name: "16x" }));
    const slider = screen.getByRole("slider", { name: /replay position/i });
    await waitFor(() => expect(Number((slider as HTMLInputElement).value)).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(screen.getByRole("button", { name: "Play" })).toBeTruthy();
  });

  it("says a goal with no dated history has nothing to scrub", async () => {
    // Every stamp stripped, not just the plan's: an agent's createdAt is a
    // dated event too, so a world with only the plan's stamp removed still
    // has a timeline and would not exercise the empty case at all.
    const world = emptyGoal();
    const h = nexusHarness({
      world: {
        ...world,
        plan: world.plan === null ? null : { ...world.plan, createdAt: "" },
        planner: null,
        agents: [],
      },
    });
    renderNexus(h, "/nexus/plan-empty/replay");
    await waitFor(() => expect(screen.getByText(/recorded no dated history/i)).toBeTruthy());
    // Replay reads the rows' own timestamps and invents nothing -- said out
    // loud rather than shown as an empty scrubber.
    expect(screen.getByText(/invents nothing/i)).toBeTruthy();
  });
});
