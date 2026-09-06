// The LAZY-CHUNK guard for the scene registry.
//
// This is what survived clients/portal/test/nexusMap.test.tsx when the work
// spine's epic A1 deleted the Nexus pages (decision D7). The rest of that file
// asserted the goal map -- its WebGL fallback, its frame-loop predicate, its
// reduced-motion timings, its 300-node budget -- and went with the pages it
// was about. This one guard did not, because it is about the PORTAL's bundle
// rather than about Nexus: the scene registry is reachable from every arranged
// page in the console, so a static three.js import anywhere in the scene tree
// is paid for on every page load.
//
// The scan is a regex over source, so the negative control below is not
// optional: without it, an empty offender list is a statement about the regex
// rather than about the tree.

import { describe, expect, it } from "vitest";

describe("the lazy chunk", () => {
  // Read through Vite's raw glob rather than the filesystem: it is what the
  // bundler itself sees, and it needs no Node types in a browser-targeted
  // tsconfig.
  const sources = import.meta.glob("../src/scenes/**/*.{ts,tsx}", {
    query: "?raw",
    eager: true,
    import: "default",
  }) as Record<string, string>;

  // The CANVASES: the only modules allowed to import three.js.
  //
  // A LIST rather than a suffix rule ("anything called *Canvas.tsx"), on
  // purpose: a suffix rule grants permission by filename, so the way to bypass
  // the guard would be to name a file well, and the guard's whole job is that
  // nobody adds a static three.js import by accident.
  //
  // Adding a scene means adding its canvas here, deliberately, in the same
  // change -- which is exactly the moment to think about whether the new
  // module really needs its own chunk.
  const CANVASES = ["ConceptGraphCanvas.tsx"];

  it("finds the scene tree at all", () => {
    // The anti-vacuous precondition for everything below: a glob that matched
    // nothing would make every assertion in this file pass while measuring an
    // empty set, and the move from src/nexus/scene/ to src/scenes/ is exactly
    // the kind of change that breaks a glob silently.
    expect(Object.keys(sources).length).toBeGreaterThan(2);
    expect(
      Object.keys(sources).some((p) => p.endsWith("registry.tsx")),
      "the scene registry is not in the scanned set",
    ).toBe(true);
  });

  it("keeps three.js behind dynamic imports, one per scene", () => {
    const offenders: string[] = [];
    for (const [path, source] of Object.entries(sources)) {
      if (CANVASES.some((canvas) => path.endsWith(canvas))) continue;
      if (/from "(three|@react-three\/[a-z]+)"/.test(source)) offenders.push(path);
    }
    // Naming the offenders rather than asserting a count, so a failure says
    // which file to fix.
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
    const graph = Object.entries(sources).find(([path]) =>
      path.endsWith("ConceptGraphScene.tsx"),
    );
    expect(graph?.[1] ?? "").toContain('lazy(() => import("./ConceptGraphCanvas"))');
  });

  it("would CATCH a violation, which is the only thing that makes the scan above evidence", () => {
    // The guard is a regex over source. A regex that stopped matching -- a
    // changed import spelling, a bundler alias, an `import * as THREE` form
    // nobody anticipated -- reports a clean tree forever, and its silence is
    // indistinguishable from compliance.
    //
    // So the detector is run over sources that DO violate it. This is the
    // reachable positive behind the empty offender list.
    const violating = [
      'import * as THREE from "three";',
      'import { Canvas } from "@react-three/fiber";',
      'import { OrbitControls } from "@react-three/drei";',
    ];
    for (const source of violating) {
      expect(
        /from "(three|@react-three\/[a-z]+)"/.test(source),
        `the lazy-chunk detector does not match: ${source}`,
      ).toBe(true);
    }

    // ...and does NOT fire on the things a scene module legitimately imports,
    // so the guard cannot be "passing" by flagging everything.
    for (const source of [
      'import { Skeleton } from "../ui";',
      'import type { ConceptGraph } from "./conceptGraph";',
      "// three.js is imported by the canvas, not here",
    ]) {
      expect(
        /from "(three|@react-three\/[a-z]+)"/.test(source),
        `the detector false-positives on: ${source}`,
      ).toBe(false);
    }
  });

  it("loads every registered scene lazily, so no page pays for a scene it does not place", () => {
    // The registry NAMES every scene, so a static import in it would bundle
    // the whole WebGL stack for every arranged page -- which is every page.
    const registry = Object.entries(sources).find(([path]) => path.endsWith("registry.tsx"));
    const source = registry?.[1] ?? "";
    expect(source, "the scene registry was not found").not.toBe("");
    for (const module of ["ConceptGraphScene"]) {
      expect(source).toContain(`await import("./${module}")`);
    }
    // Nothing in the registry may import a scene statically.
    expect(/^import .*\.\/(ConceptGraphScene)/m.test(source)).toBe(false);
  });
});
