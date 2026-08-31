import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { SITE_CONCEPT } from "../../src/apps/deployables/concepts";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import {
  APEX,
  DOCS,
  MIRROR,
  PORTAL,
  SHOP,
  click,
  emit,
  fakeConnection,
  withSession,
  type FakeConnection,
} from "./harness";

// The deploy map: what it draws, how it is steered, and the one rule that keeps
// it cheap -- no three.js anywhere under clients/os.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection | null, opts: { role?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId="map"
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner" },
    ),
  );
}

function canvas(): HTMLElement {
  return screen.getByRole("application", { name: "Deploy map" });
}

function viewTransform(): string {
  return document.querySelector("[data-os-map-view]")?.getAttribute("transform") ?? "";
}

beforeEach(() => {
  h.connection = null;
});

describe("what the map draws", () => {
  it("gives every deployable a host, a site and a bundle node, grouped by domain", async () => {
    const connection = fakeConnection({ sites: [SHOP, APEX] });
    mount(connection);

    expect(await screen.findByLabelText("Host shop.memql.example.com")).toBeTruthy();
    expect(
      screen.getByLabelText("Deployable shop.memql.example.com, live, shopify_storefront"),
    ).toBeTruthy();
    expect(screen.getByLabelText(/^Bundle uploaded bundle: blob:\/\/sites\/site-shop\/v1\//)).toBeTruthy();
    expect(screen.getByLabelText("Library artifact artifact-zip")).toBeTruthy();

    // Two domains, two group labels. Scoped to the group heading, because an
    // APEX site's group label is also its hostname -- which is the correct
    // reading of an apex and would make a bare text query ambiguous.
    const groups = [...document.querySelectorAll(".os-deploy-group-label")].map(
      (el) => el.textContent,
    );
    expect(groups).toEqual(["example.org", "memql.example.com"]);
  });

  it("says a shared bundle is shared, in words as well as in the picture", async () => {
    const connection = fakeConnection({ sites: [DOCS, MIRROR] });
    mount(connection);
    const bundle = await screen.findByLabelText(/Bundle baked site, serving 2 deployables/);
    expect(bundle).toBeTruthy();
  });

  it("draws nothing and says so when there is nothing to draw", async () => {
    const connection = fakeConnection({ sites: [] });
    mount(connection);
    await waitFor(() => expect(screen.getByText(/No deployables to map yet/)).toBeTruthy());
    expect(screen.queryByRole("application", { name: "Deploy map" })).toBeNull();
  });
});

describe("selection", () => {
  it("opens a deployable's detail on click", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await click(await screen.findByLabelText(/^Deployable shop.memql.example.com/));

    const panel = await screen.findByRole("region", { name: "Deployable shop.memql.example.com" });
    expect(within(panel).getByText("blob://sites/site-shop/v1/")).toBeTruthy();
  });

  it("opens it from the keyboard too", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    const node = await screen.findByLabelText("Host shop.memql.example.com");
    fireEvent.keyDown(node, { key: "Enter" });
    expect(await screen.findByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
  });

  it("marks the selected cluster so the picture says what is open", async () => {
    const connection = fakeConnection({ sites: [SHOP, APEX] });
    mount(connection);
    await click(await screen.findByLabelText(/^Deployable shop.memql.example.com/));

    expect(document.querySelectorAll("[data-selected]").length).toBeGreaterThan(0);
    // The whole story of that deployable lights up -- host, site, bundle,
    // artifact -- not just the box that was clicked.
    expect(document.querySelectorAll("[data-in-cluster]").length).toBeGreaterThanOrEqual(3);
  });

  it("OFFERS THE CHOICE when a node serves more than one deployable", async () => {
    // There is no one detail to open, and picking one would be arbitrary.
    const connection = fakeConnection({ sites: [DOCS, MIRROR] });
    mount(connection);
    await click(await screen.findByLabelText(/Bundle baked site, serving 2 deployables/));

    expect(screen.getByText(/serves more than one deployable/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "docs.memql.example.com" }));
    expect(await screen.findByRole("region", { name: "Deployable docs.memql.example.com" })).toBeTruthy();
  });

  it("closes on a second activation of the same node", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    const node = await screen.findByLabelText(/^Deployable shop.memql.example.com/);
    await click(node);
    expect(screen.getByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
    await click(node);
    expect(screen.queryByRole("region", { name: "Deployable shop.memql.example.com" })).toBeNull();
  });
});

describe("steering", () => {
  it("pans with the arrow keys", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");
    expect(viewTransform()).toBe("translate(0 0) scale(1)");

    fireEvent.keyDown(canvas(), { key: "ArrowRight" });
    expect(viewTransform()).toBe("translate(-48 0) scale(1)");
    fireEvent.keyDown(canvas(), { key: "ArrowDown" });
    expect(viewTransform()).toBe("translate(-48 -48) scale(1)");
    fireEvent.keyDown(canvas(), { key: "ArrowLeft" });
    fireEvent.keyDown(canvas(), { key: "ArrowUp" });
    expect(viewTransform()).toBe("translate(0 0) scale(1)");
  });

  it("zooms with + and -, and 0 puts it back", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    fireEvent.keyDown(canvas(), { key: "+" });
    expect(viewTransform()).toContain("scale(1.2)");
    fireEvent.keyDown(canvas(), { key: "-" });
    expect(viewTransform()).toContain("scale(1)");

    fireEvent.keyDown(canvas(), { key: "+" });
    fireEvent.keyDown(canvas(), { key: "0" });
    expect(viewTransform()).toBe("translate(0 0) scale(1)");
  });

  it("zooms on the wheel, toward the pointer", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    fireEvent.wheel(canvas(), { deltaY: -120, clientX: 200, clientY: 100 });
    const after = viewTransform();
    expect(after).toContain("scale(1.2)");
    // Zooming about (200, 100) moves the origin: a zoom that only changed the
    // scale would drag the point under the cursor away from it.
    expect(after).not.toContain("translate(0 0)");
  });

  it("PANS FROM A NODE without opening it -- a drag is steering, not choosing", async () => {
    // A node lives inside the canvas, so a drag that begins on one bubbles to
    // the pan handler and then ends in a click on that node. Without the
    // travel threshold, steering past a deployable opens it.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    const node = await screen.findByLabelText(/^Deployable shop.memql.example.com/);

    fireEvent.pointerDown(node, { pointerId: 1, clientX: 100, clientY: 100 });
    fireEvent.pointerMove(canvas(), { pointerId: 1, clientX: 180, clientY: 140 });
    fireEvent.pointerUp(canvas(), { pointerId: 1, clientX: 180, clientY: 140 });
    await click(node);

    expect(viewTransform()).toBe("translate(80 40) scale(1)");
    expect(screen.queryByRole("region", { name: "Deployable shop.memql.example.com" })).toBeNull();
  });

  it("still opens on a click with a shaky hand, which is most clicks", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    const node = await screen.findByLabelText(/^Deployable shop.memql.example.com/);

    fireEvent.pointerDown(node, { pointerId: 1, clientX: 100, clientY: 100 });
    fireEvent.pointerMove(canvas(), { pointerId: 1, clientX: 101, clientY: 101 });
    fireEvent.pointerUp(canvas(), { pointerId: 1, clientX: 101, clientY: 101 });
    await click(node);

    expect(screen.getByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
  });

  it("never suppresses the KEYBOARD path, however far the map was dragged", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    const node = await screen.findByLabelText(/^Deployable shop.memql.example.com/);

    fireEvent.pointerDown(canvas(), { pointerId: 1, clientX: 0, clientY: 0 });
    fireEvent.pointerMove(canvas(), { pointerId: 1, clientX: 300, clientY: 300 });
    fireEvent.pointerUp(canvas(), { pointerId: 1, clientX: 300, clientY: 300 });
    fireEvent.keyDown(node, { key: "Enter" });

    expect(await screen.findByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
  });

  it("PINCHES, and does not jump on the frame the second finger lands", async () => {
    // The baseline rule: a ratio taken against a distance nobody has measured
    // yet would scale the map by however far apart the fingers happened to be
    // when they touched down.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    fireEvent.pointerDown(canvas(), { pointerId: 1, clientX: 100, clientY: 100 });
    // NOT primary -- which is what a real second finger is, and what stops it
    // being read as the start of a fresh gesture.
    fireEvent.pointerDown(canvas(), {
      pointerId: 2,
      clientX: 200,
      clientY: 100,
      isPrimary: false,
    });
    // First two-finger move: baseline only, 100px apart.
    fireEvent.pointerMove(canvas(), { pointerId: 2, clientX: 200, clientY: 100 });
    expect(viewTransform()).toContain("scale(1)");

    // Now 200px apart: twice the span, twice the scale.
    fireEvent.pointerMove(canvas(), { pointerId: 2, clientX: 300, clientY: 100 });
    expect(viewTransform()).toContain("scale(2)");

    // Lifting one finger goes back to panning, with no jump from the pinch.
    fireEvent.pointerUp(canvas(), { pointerId: 2, clientX: 300, clientY: 100 });
    const held = viewTransform();
    fireEvent.pointerMove(canvas(), { pointerId: 1, clientX: 100, clientY: 100 });
    expect(viewTransform()).toBe(held);
  });

  it("FRAMES the whole map on first paint, and never re-frames after", async () => {
    // A map that opens clipped is one somebody has to discover they can pan
    // before they can read it. jsdom measures every box as zero, so `fitTo`
    // answers the identity here -- which is the honest behaviour for an
    // unmeasured viewport, and what this asserts is the OTHER half: that a
    // later row change does not reset a view somebody has moved.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    fireEvent.keyDown(canvas(), { key: "ArrowRight" });
    const moved = viewTransform();
    expect(moved).toBe("translate(-48 0) scale(1)");

    await emit(connection, SITE_CONCEPT, PORTAL, "NODE_CREATED");
    await screen.findByLabelText("Host portal.memql.example.com");
    // Somebody who panned to read a corner must not be thrown back to the
    // origin because a deployable arrived somewhere else on the map.
    expect(viewTransform()).toBe(moved);
  });

  it("does not read a stale pointer as a second finger", async () => {
    // A pointerup that landed outside the element leaves a ghost in the map.
    // Without the primary-down clear, the NEXT press sees two live pointers and
    // scales the view by however far apart they happen to be.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    fireEvent.pointerDown(canvas(), { pointerId: 7, clientX: 400, clientY: 400 });
    // ...and no pointerup: the gesture was lost outside the element.

    fireEvent.pointerDown(canvas(), { pointerId: 8, clientX: 100, clientY: 100 });
    fireEvent.pointerMove(canvas(), { pointerId: 8, clientX: 130, clientY: 100 });
    expect(viewTransform()).toBe("translate(30 0) scale(1)");
  });

  it("walks the nodes with Tab", async () =>{
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");
    for (const node of document.querySelectorAll(".os-deploy-node")) {
      expect(node.getAttribute("tabindex")).toBe("0");
    }
  });
});

describe("a feed that is behind", () => {
  it("DIMS and says so -- a frozen map must not read as a healthy fleet", async () => {
    // No connection at all is the disconnected case, which is the one a person
    // is most likely to be looking at without knowing it.
    mount(null);
    expect(await screen.findByText(/Not connected to the cluster/)).toBeTruthy();
  });

  it("re-derives live, and pulses what changed", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    await emit(connection, SITE_CONCEPT, { ...SHOP, status: "disabled" });

    expect(
      await screen.findByLabelText("Deployable shop.memql.example.com, disabled, shopify_storefront"),
    ).toBeTruthy();
    await waitFor(() =>
      expect(document.querySelector(".os-deploy-node[data-arrival='updated']")).not.toBeNull(),
    );
  });

  it("reshapes when a deployable arrives", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");

    await emit(connection, SITE_CONCEPT, PORTAL, "NODE_CREATED");
    expect(await screen.findByLabelText("Host portal.memql.example.com")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The module-graph guard
// ---------------------------------------------------------------------------

describe("no three.js under clients/os", () => {
  // Read through Vite's raw glob rather than the filesystem: it is what the
  // bundler itself sees, and it needs no Node types in a browser tsconfig.
  const sources = import.meta.glob("../../src/**/*.{ts,tsx}", {
    query: "?raw",
    eager: true,
    import: "default",
  }) as Record<string, string>;

  const DETECTOR = /from "(three|@react-three\/[a-z]+)"/;

  it("finds no importer anywhere in the shell", () => {
    // The portal's Nexus is the platform's ONE 3D surface and pays for it with
    // a lazy chunk and a guard of its own. This map answers a flat question --
    // which host, which site, which bundle -- so a WebGL renderer would buy it
    // nothing while making every OS window carry the largest dependency the
    // portal has.
    const offenders = Object.entries(sources)
      .filter(([, source]) => DETECTOR.test(source))
      .map(([path]) => path);
    expect(offenders.join(", ")).toBe("");
  });

  it("is not in the OS's dependencies either", async () => {
    // A static import is one way in. A dependency somebody added for a
    // dynamic import is the other, and the scan above cannot see it.
    const pkg = (await import("../../package.json")) as unknown as {
      default: { dependencies: Record<string, string>; devDependencies: Record<string, string> };
    };
    const named = [
      ...Object.keys(pkg.default.dependencies),
      ...Object.keys(pkg.default.devDependencies),
    ];
    expect(named.filter((n) => n === "three" || n.startsWith("@react-three/"))).toEqual([]);
  });

  it("WOULD CATCH A VIOLATION, which is the only thing that makes the scan evidence", () => {
    // The guard is a regex over source. A regex that stopped matching -- a
    // changed import spelling, a form nobody anticipated -- reports a clean
    // tree forever, and its silence is indistinguishable from compliance.
    for (const source of [
      'import * as THREE from "three";',
      'import { Canvas } from "@react-three/fiber";',
      'import { OrbitControls } from "@react-three/drei";',
    ]) {
      expect(DETECTOR.test(source), `the detector does not match: ${source}`).toBe(true);
    }
    // ...and does NOT fire on what the map legitimately imports.
    for (const source of [
      'import { layout } from "./layout";',
      'import type { SiteRow } from "../rows";',
      "// three.js is deliberately not used here",
    ]) {
      expect(DETECTOR.test(source), `the detector false-positives on: ${source}`).toBe(false);
    }
  });

  it("really did read the map's own source, so an empty result is about the tree", () => {
    const map = Object.entries(sources).find(([p]) => p.endsWith("deployables/map/DeployMap.tsx"));
    expect(map?.[1] ?? "").toContain("os-deploy-map-canvas");
    expect(Object.keys(sources).length).toBeGreaterThan(30);
  });
});

describe("the hooks the stylesheet paints through", () => {
  // ==========================================================================
  // WHAT A jsdom TEST CAN AND CANNOT SAY ABOUT A DRAWING
  // ==========================================================================
  // jsdom applies no stylesheet, so nothing here can prove a pixel -- and
  // vitest hands back an empty string for a CSS import, so not even a text scan
  // of the sheet is available. What IS assertable is the contract between the
  // component and the stylesheet: the attributes the CSS selects on. A rule
  // that stops matching because the attribute moved is the failure this can
  // catch; a rule that was deleted is one only looking can.
  //
  // The map was checked by eye for the rest (`clients/os/README.md` records the
  // house method: tint it and look at it).

  it("marks a behind feed on the map root, which is what the dim rule selects", async () => {
    mount(null);
    await screen.findByText(/Not connected to the cluster/);
    // With no rows there is no canvas to dim, so the seeded case is the one
    // that carries the attribute -- assert it where it can exist.
    const connection = fakeConnection({ sites: [SHOP] });
    const view = mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");
    expect(view.container.querySelector(".os-deploy-map[data-behind]")).toBeNull();
  });

  it("marks each site node with its own status, which is what the dot rule selects", async () => {
    const connection = fakeConnection({ sites: [SHOP, DOCS] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");
    const statuses = [...document.querySelectorAll(".os-deploy-node-dot")].map((el) =>
      el.getAttribute("data-status"),
    );
    expect(statuses.sort()).toEqual(["draft", "live"]);
  });

  it("marks a changed node with the arrival kind, which is what the pulse rule selects", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByLabelText("Host shop.memql.example.com");
    await emit(connection, SITE_CONCEPT, { ...SHOP, status: "draft" });
    await waitFor(() =>
      expect(document.querySelector(".os-deploy-node[data-arrival='updated']")).not.toBeNull(),
    );
  });
});
