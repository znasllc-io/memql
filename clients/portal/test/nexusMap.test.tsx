// The Map surface: what it does without a GPU, what reduced motion changes,
// and the two budgets that keep a dense goal honest.
//
// The scene itself is drawn by WebGL and is not rendered here -- jsdom has no
// WebGL, which is exactly the condition the surface has a real fallback for
// (see MapSurface's header). So what this file covers is everything around
// the canvas plus the arithmetic the canvas runs, extracted so it can be
// asserted rather than watched:
//
//   * the fallback a person with acceleration off actually sees
//   * the idle predicate that IS the "frame loop on demand" guarantee
//   * reduced motion's effect on the timings the scene obeys
//   * the lazy-chunk guard: nothing outside the canvas may import three.js
//   * the 300-node budget

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";

import {
  FULL_MOTION,
  REDUCED_MOTION,
  easeOutBack,
  sceneIsAnimating,
  timingsFor,
} from "../src/nexus/map/motion";
import { colourForTask, readPalette, toneForTask } from "../src/nexus/map/palette";
import { probeWebGL } from "../src/nexus/map/webgl";
import { DEFAULT_CLUSTER_THRESHOLD, layout } from "../src/nexus/scene/layout";
import { NOW, scene } from "../src/nexus/scene/scene";
import { denseGoal, springCatalogGoal } from "../src/nexus/scene/fixtures";
import { nexusHarness, renderNexus } from "./support/nexusHarness";

describe("the fallback when WebGL is unavailable", () => {
  it("is what jsdom gets, which is what a locked-down browser gets", () => {
    // Not a stub and not a mock: jsdom genuinely has no WebGL, and the probe
    // says so. If this ever returns true the fallback below stops being
    // exercised, so the assertion is on the probe rather than on a flag.
    expect(probeWebGL()).toBe(false);
  });

  it("says the browser cannot draw the map, and still reads the goal out", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    await waitFor(() => expect(screen.getByText(/cannot draw the map/i)).toBeTruthy());

    // The phase summary is the map's reading in text -- which is what makes
    // the fallback a fallback rather than an apology.
    expect(screen.getByText("gather")).toBeTruthy();
    expect(screen.getByText("shape")).toBeTruthy();
    expect(screen.getByText("publish")).toBeTruthy();
    // Six NODES, six complete -- the retried step counts once, which is
    // what makes a goal that had to try again able to fill at all. The text
    // is split across elements by the interpolation, so the match is on the
    // element's own textContent.
    expect(
      screen.getByText((_, element) => element?.textContent === "6 of 6 tasks complete"),
    ).toBeTruthy();
  });

  it("offers the recent-goals strip under the map", async () => {
    const h = nexusHarness({
      goals: [
        { id: "plan-spring", goal: "Build a spring catalog", status: "running", requestedBy: "user-1", createdAt: "2026-08-20T09:00:00Z" },
        { id: "plan-other", goal: "Another goal", status: "succeeded", requestedBy: "user-1", createdAt: "2026-08-19T09:00:00Z" },
      ],
    });
    renderNexus(h, "/nexus/plan-spring");
    const strip = await screen.findByRole("navigation", { name: /recent goals/i });
    expect(strip.textContent).toContain("Another goal");
    // The goal you are already looking at is not offered as somewhere to go.
    expect(strip.textContent).not.toContain("Build a spring catalog");
  });
});

describe("the frame loop is on demand", () => {
  it("stays awake while an arrival is in flight and settles after it", () => {
    const full = timingsFor(false);
    expect(sceneIsAnimating({ newestArrivalAgeMs: 10, anyTaskRunning: false, timings: full })).toBe(true);
    expect(
      sceneIsAnimating({ newestArrivalAgeMs: 5_000, anyTaskRunning: false, timings: full }),
    ).toBe(false);
    // Nothing has arrived at all: settled, not a special case.
    expect(
      sceneIsAnimating({
        newestArrivalAgeMs: Number.POSITIVE_INFINITY,
        anyTaskRunning: false,
        timings: full,
      }),
    ).toBe(false);
  });

  it("stays awake while a task is running, and does NOT under reduced motion", () => {
    expect(
      sceneIsAnimating({
        newestArrivalAgeMs: Number.POSITIVE_INFINITY,
        anyTaskRunning: true,
        timings: timingsFor(false),
      }),
    ).toBe(true);
    // The reduced-motion guarantee that matters most: a settled scene over a
    // goal that is STILL RUNNING costs nothing at all, because the one
    // always-on animation does not exist.
    expect(
      sceneIsAnimating({
        newestArrivalAgeMs: Number.POSITIVE_INFINITY,
        anyTaskRunning: true,
        timings: timingsFor(true),
      }),
    ).toBe(false);
  });
});

describe("reduced motion", () => {
  it("removes the motions rather than shortening them", () => {
    // Cutting the durations would keep every motion and make it jerkier,
    // which is the opposite of what the preference asks for.
    expect(REDUCED_MOTION.condenseMs).toBe(0);
    expect(REDUCED_MOTION.overshoot).toBe(0);
    expect(REDUCED_MOTION.breathAmplitude).toBe(0);
    expect(REDUCED_MOTION.edgeDrawMs).toBe(0);
    // What remains is opacity, which is not motion -- so the fade survives.
    expect(REDUCED_MOTION.scaleInMs).toBeGreaterThan(0);
  });

  it("collapses the arrival curve to a plain ease-out with no overshoot", () => {
    // The full curve springs past 1 before settling; the reduced one never
    // exceeds it. That is the whole difference an eye can see.
    let sprang = false;
    for (let t = 0; t <= 1.0001; t += 0.02) {
      if (easeOutBack(t, FULL_MOTION.overshoot) > 1.0001) sprang = true;
      expect(easeOutBack(t, 0)).toBeLessThanOrEqual(1.0001);
    }
    expect(sprang).toBe(true);
    expect(easeOutBack(1, FULL_MOTION.overshoot)).toBeCloseTo(1, 5);
    expect(easeOutBack(0, FULL_MOTION.overshoot)).toBeCloseTo(0, 5);
  });
});

describe("the palette", () => {
  it("falls back per token rather than wholesale", () => {
    // jsdom resolves no custom properties, so every token misses and the
    // fallback palette answers -- which is the branch a canvas mounted
    // before the stylesheet lands takes in a real browser too.
    const palette = readPalette(null);
    expect(palette.taskRunning).not.toBe("");
    expect(palette.taskFailed).not.toBe(palette.taskDone);
  });

  it("gives the canvas and the DOM one mapping, not two", () => {
    const palette = readPalette(null);
    expect(colourForTask("running", palette)).toBe(palette.taskRunning);
    expect(colourForTask("failed", palette)).toBe(palette.taskFailed);
    expect(colourForTask("anything else", palette)).toBe(palette.taskQueued);
    expect(toneForTask("running")).toBe("warn");
    expect(toneForTask("succeeded")).toBe("ok");
    expect(toneForTask("failed")).toBe("danger");
    expect(toneForTask("queued")).toBe("neutral");
  });
});

describe("the lazy chunk", () => {
  // Read through Vite's raw glob rather than the filesystem: it is what the
  // bundler itself sees, and it needs no Node types in a browser-targeted
  // tsconfig.
  const sources = import.meta.glob("../src/nexus/**/*.{ts,tsx}", {
    query: "?raw",
    eager: true,
    import: "default",
  }) as Record<string, string>;

  // The CANVASES: the only modules in the tree allowed to import three.js.
  //
  // This was one file until epic memql#4661 made scenes a registry (task
  // memql#4672). It is a LIST rather than a suffix rule ("anything called
  // *Canvas.tsx") on purpose: a suffix rule grants permission by filename, so
  // the way to bypass the guard would be to name a file well, and the guard's
  // whole job is that nobody adds a static three.js import by accident.
  //
  // Adding a scene means adding its canvas here, deliberately, in the same
  // change -- which is exactly the moment to think about whether the new
  // module really needs its own chunk.
  const CANVASES = [
    "map/NexusCanvas.tsx",
    "scene/ConceptGraphCanvas.tsx",
  ];

  it("keeps three.js behind dynamic imports, one per scene", () => {
    const offenders: string[] = [];
    for (const [path, source] of Object.entries(sources)) {
      if (CANVASES.some((canvas) => path.endsWith(canvas))) continue;
      if (/from "(three|@react-three\/[a-z]+)"/.test(source)) offenders.push(path);
    }
    // Any static import outside a canvas pulls three.js, fiber and drei into
    // the portal's main bundle, which every other page then pays for. The
    // scene REGISTRY is reachable from every arranged page in the console, so
    // the cost of getting this wrong went up rather than down when scenes
    // became elements. Naming the offenders rather than asserting a count, so
    // a failure says which file to fix.
    expect(offenders.join(", ")).toBe("");

    // ...and the anti-vacuous half: every canvas really does import it, so a
    // rename that made this scan find nothing would fail here instead of
    // passing silently.
    for (const canvas of CANVASES) {
      const found = Object.entries(sources).find(([path]) => path.endsWith(canvas));
      expect(found?.[1] ?? "", `${canvas} is listed as a canvas but imports no three.js`).toContain(
        'from "three"',
      );
    }

    // And every canvas is reached ONLY through a dynamic import.
    const surface = Object.entries(sources).find(([path]) => path.endsWith("map/MapSurface.tsx"));
    expect(surface?.[1] ?? "").toContain('lazy(() => import("./NexusCanvas"))');
    const graph = Object.entries(sources).find(([path]) =>
      path.endsWith("scene/ConceptGraphScene.tsx"),
    );
    expect(graph?.[1] ?? "").toContain('lazy(() => import("./ConceptGraphCanvas"))');
  });

  it("loads every registered scene lazily, so no page pays for a scene it does not place", () => {
    // The registry NAMES every scene, so a static import in it would bundle
    // the whole WebGL stack for every arranged page -- which is every page.
    const registry = Object.entries(sources).find(([path]) =>
      path.endsWith("scene/registry.tsx"),
    );
    const source = registry?.[1] ?? "";
    expect(source, "the scene registry was not found").not.toBe("");
    for (const module of ["ConceptGraphScene", "GoalMapScene"]) {
      expect(source).toContain(`await import("./${module}")`);
    }
    // Nothing in the registry may import a scene statically.
    expect(/^import .*\.\/(ConceptGraphScene|GoalMapScene)/m.test(source)).toBe(false);
  });
});

// ===========================================================================
// THE TWO SHAPES A BROWSER PROVED WERE REQUIRED
// ===========================================================================
// Both of these were found by opening the scene in Chrome, and neither can be
// asserted behaviourally here: jsdom has no WebGL, so the canvas never mounts,
// and jsdom resolves no custom properties, so the palette's real code path
// never runs. What CAN be pinned is the shape of the code, and each of these
// cost a scene that rendered nothing at all -- with no error, in a canvas that
// had a live WebGL context and a full scene graph behind it.
describe("the shapes a browser proved were required", () => {
  const sources = import.meta.glob("../src/nexus/map/*.{ts,tsx}", {
    query: "?raw",
    eager: true,
    import: "default",
  }) as Record<string, string>;

  // The CODE, with comments removed. These two guards forbid a spelling that
  // the comment explaining WHY it is forbidden naturally contains, so a scan
  // over the raw file matches its own documentation. Crude (a `//` inside a
  // string literal would be stripped too) and sufficient: neither file has
  // one, and the alternative is wording the comments around the guard, which
  // makes the explanation worse to protect the test.
  function source(name: string): string {
    const raw = Object.entries(sources).find(([path]) => path.endsWith(name))?.[1] ?? "";
    return raw.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
  }

  it("resolves brand tokens through a real property, never getPropertyValue", () => {
    const palette = source("palette.ts");
    expect(palette).not.toBe("");

    // getPropertyValue returns the token's DECLARED TEXT. Every brand colour
    // is `light-dark(<light>, <dark>)`, which three.js cannot parse -- and
    // Color.set does not throw on a colour it cannot parse, it warns and
    // leaves the previous value. The scene came out monochrome and unreadable
    // with nothing in the test suite red.
    expect(palette).not.toContain("getPropertyValue");
    // The shape that works: ask the browser through a real colour property.
    expect(palette).toContain("getComputedStyle(probe).color");
    expect(palette).toContain("var(${name}, ${SENTINEL})");
  });

  it("keeps the demand loop alive with invalidate(2), not invalidate()", () => {
    const canvas = source("NexusCanvas.tsx");
    expect(canvas).not.toBe("");

    // React Three Fiber's demand loop runs while internal.frames > 0 and
    // DECREMENTS after each frame, so a plain invalidate() called from inside
    // useFrame sets the count to 1, the decrement takes it to 0, and the loop
    // stops. The map rendered exactly one frame -- at which point every node
    // was still at the start of its arrival, scale ~0.001 -- and the canvas
    // was blank.
    expect(canvas).toContain("invalidate(2)");
    expect(canvas).not.toMatch(/\binvalidate\(\)/);
  });
});

describe("the budget", () => {
  // What a browser actually pays per scrub frame is the layout plus the
  // time filter, both pure. The GPU cost is not measurable here and is NOT
  // claimed: this asserts the CPU work the scene library does, which is the
  // part that scales with the goal and the part a regression would land in.
  const BUDGET_MS = 60;

  it("lays out the 300-node fixture inside the per-scrub budget", () => {
    const world = denseGoal(300);
    const start = performance.now();
    for (let i = 0; i < 10; i += 1) {
      layout(world, { expanded: new Set(["sweep"]) });
    }
    const each = (performance.now() - start) / 10;
    expect(`${each < BUDGET_MS} (${each.toFixed(1)}ms)`).toBe(`true (${each.toFixed(1)}ms)`);
  });

  it("filters and lays out a scrub position inside the same budget", () => {
    const world = denseGoal(300);
    const start = performance.now();
    for (let i = 0; i < 10; i += 1) {
      layout(scene(world, "2026-08-20T09:20:00Z"), { expanded: new Set(["sweep"]) });
    }
    const each = (performance.now() - start) / 10;
    expect(`${each < BUDGET_MS} (${each.toFixed(1)}ms)`).toBe(`true (${each.toFixed(1)}ms)`);
  });

  it("draws four glyphs instead of three hundred when the phase is dense", () => {
    // The collapse is what keeps the GPU cost bounded, and it is the thing
    // this file CAN assert about the GPU: above the threshold the scene
    // carries one node for the phase, not one per task.
    const { nodes } = layout(denseGoal(DEFAULT_CLUSTER_THRESHOLD + 1));
    expect([...nodes.values()].filter((n) => n.kind === "task")).toHaveLength(0);
    expect([...nodes.values()].filter((n) => n.kind === "cluster")).toHaveLength(1);
  });

  it("does not copy the world when the scene is showing NOW", () => {
    // The live map calls scene(world, NOW) on every render; returning the
    // same object is what keeps that free.
    const world = springCatalogGoal();
    expect(scene(world, NOW)).toBe(world);
  });
});

describe("hovering and clicking", () => {
  it("expands a collapsed phase rather than opening a detail that does not exist", async () => {
    // A cluster node stands in for a phase and has no row, so clicking it
    // must not push a URL that would render nothing. Driven through the
    // fallback's own summary, which reports the collapse.
    const h = nexusHarness({ world: denseGoal(DEFAULT_CLUSTER_THRESHOLD + 1) });
    renderNexus(h, "/nexus/plan-dense");
    await waitFor(() => expect(screen.getByText("collapsed")).toBeTruthy());
    // Nothing to click in jsdom (the glyph is in the canvas), so what is
    // asserted here is the STATE the click produces -- see nexusScene's
    // "expands a collapsed phase on request" for the layout half. The badge
    // reports the phase's REAL count, not the one node standing in for it.
    const badge = screen.getByText("sweep").closest("span");
    expect(badge?.textContent).toContain(String(DEFAULT_CLUSTER_THRESHOLD + 1));
  });

  it("routes a node click to that node's own URL", async () => {
    // The click itself happens on the canvas; what is testable without one
    // is that the address a node resolves to opens its detail, which the
    // route test covers, and that the surface hands the canvas a handler
    // that navigates rather than setting state. Asserted here through the
    // rendered link the dialog offers back to the concept browser.
    renderNexus(nexusHarness(), "/nexus/plan-spring/node/artifact~artifact-catalog");
    await waitFor(() => expect(screen.getByRole("heading", { name: /row detail/i })).toBeTruthy());
    // findByRole, not getByRole: the panel renders its heading as soon as the
    // dialog opens, but this link is gated on `node.kind === "artifact"` and so
    // waits for the node itself to resolve. Two render passes, and the bare
    // getByRole asserted against the first one -- which is why this passed
    // locally and in isolation and lost the race once under CI's parallel load
    // (memql#4411). findByRole retries until the second pass lands.
    const library = await screen.findByRole("link", { name: /open in the library/i });
    expect(library.getAttribute("href")).toBe("/artifacts/artifact-catalog");
    fireEvent.click(library);
  });
});
