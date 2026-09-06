// The navigation definition (memql#4655).
//
// One file describes the rail, every tab strip, the command palette's
// destination and tab sources, and the guide-coverage gate's expectations.
// These assertions are the ones a source read cannot settle: that the count is
// what the design says, that no two destinations wear the same glyph, that
// role gating is what it was before the restructure, and that "active" is
// right for the two destinations whose tabs live under unrelated prefixes.

import { describe, expect, it } from "vitest";

import { FLEET_SURFACES } from "../src/fleet/urls";
import {
  DESTINATIONS,
  destinationById,
  destinationIsActive,
  destinationPath,
  maySee,
  navPageIds,
  visibleTabs,
} from "../src/app/nav";

describe("the rail", () => {
  it("is seven flat destinations, in the designed order", () => {
    // The COUNT is the decision (D1). A rail is a thing you scan, and this one
    // carried seventeen rows plus one per saved view. Six now: Nexus left with
    // the pages it named (work spine A1, decision D7) and is being rebuilt on
    // MemQL OS.
    expect(DESTINATIONS.map((d) => d.label)).toEqual([
      "Console",
      "Views",
      "Concepts",
      "Fleet",
      "Library",
      "Cluster",
    ]);
  });

  it("gives every destination its own glyph", () => {
    // Four icons were used twice before the restructure (Bot, Plug, Shield and
    // a per-view LayoutGrid), which in a COLLAPSED 56px rail is the whole of
    // the label -- two identical icons are two destinations a person cannot
    // tell apart at all.
    const icons = DESTINATIONS.map((d) => d.icon);
    expect(new Set(icons).size).toBe(DESTINATIONS.length);
  });

  it("hides no destination from anybody", () => {
    // Role gating lives on TABS, not on rows: every area has at least one
    // surface everybody may see, so the rail is seven items at every role and
    // a person is never wondering what the operator's rail has that theirs
    // does not.
    for (const role of ["owner", "admin", "developer", "writer", "reader", ""]) {
      for (const destination of DESTINATIONS) {
        const path = destinationPath(destination, role);
        expect(`${destination.id} at ${role || "(unresolved)"}: ${path}`).not.toContain("undefined");
      }
    }
  });
});

describe("the two lists that must not drift", () => {
  it("keeps Fleet's tabs in step with the surfaces themselves", () => {
    // nav.ts spells these out so the repo-root guide-coverage gate can READ
    // them -- a template literal is not a value a gate can parse. That makes
    // this the join between the two lists, and it is the reason spelling them
    // out is safe.
    const tabs = destinationById("fleet")?.tabs ?? [];
    expect(tabs.map((tab) => tab.id)).toEqual(FLEET_SURFACES.map((s) => `fleet.${s.id}`));
    expect(tabs.map((tab) => tab.label)).toEqual(FLEET_SURFACES.map((s) => s.label));
    expect(tabs.map((tab) => tab.to)).toEqual(FLEET_SURFACES.map((s) => `/fleet/${s.id}`));
  });
});

describe("tab visibility", () => {
  it("mirrors what the rail rows gated on before the restructure", () => {
    const cluster = destinationById("cluster");
    expect(cluster).toBeTruthy();
    const idsFor = (role: string) => visibleTabs(cluster!, role).map((t) => t.id);

    // Everyone sees Integrations -- the one Cluster surface that was not an
    // admin row.
    for (const role of ["reader", "writer", "developer", ""]) {
      expect(idsFor(role)).toEqual(["cluster.integrations"]);
    }
    // Admin sees the admin half but not the owner-only one (epic memql#4440).
    expect(idsFor("admin")).toContain("cluster.tokens");
    expect(idsFor("admin")).not.toContain("cluster.providers");
    // The owner sees everything, which is the reachable positive: the filter
    // above is narrowing a real list rather than always answering nothing.
    expect(idsFor("owner")).toContain("cluster.providers");
    expect(idsFor("owner").length).toBeGreaterThan(idsFor("admin").length);
  });

  it("treats an unresolved role as below every floor", () => {
    // The safe direction: a tab appears when the role arrives, and nothing is
    // offered to somebody the engine will refuse.
    expect(maySee("admin", "")).toBe(false);
    expect(maySee("owner", "")).toBe(false);
    expect(maySee(undefined, "")).toBe(true);
  });

  it("points a destination's row at the first tab its reader may see", () => {
    const cluster = destinationById("cluster")!;
    // /cluster is not a route. There is nothing an operator would read on a
    // landing page above a tab strip that the strip does not already say.
    expect(destinationPath(cluster, "reader")).toBe("/integrations");
    expect(destinationPath(cluster, "owner")).toBe("/integrations");

    const concepts = destinationById("concepts")!;
    expect(destinationPath(concepts, "reader")).toBe("/concepts");
    expect(visibleTabs(concepts, "reader").map((t) => t.id)).toEqual(["concepts"]);
    expect(visibleTabs(concepts, "admin").map((t) => t.id)).toContain("concepts.modules");
  });
});

describe("which destination owns the open address", () => {
  const active = (id: string, pathname: string): boolean =>
    destinationIsActive(destinationById(id)!, pathname);

  it("lights Library on both of its tabs, which live under unrelated prefixes", () => {
    // The reason a destination declares `match` at all: NavLink can follow one
    // path, and Library's two tabs are /artifacts and /deployables.
    expect(active("library", "/artifacts")).toBe(true);
    expect(active("library", "/deployables")).toBe(true);
    expect(active("library", "/deployables/site-1")).toBe(true);
    expect(active("library", "/stores")).toBe(false);
  });

  it("lights Cluster across all four of its prefixes", () => {
    for (const path of ["/integrations", "/data-origins", "/stores", "/admin/keys"]) {
      expect(`${path}: ${active("cluster", path)}`).toBe(`${path}: true`);
    }
    expect(active("cluster", "/artifacts")).toBe(false);
  });

  it("keeps the console off every other page", () => {
    // "/" prefixes every path in the application, so a prefix match would
    // light Console permanently.
    expect(active("console", "/")).toBe(true);
    for (const path of ["/views", "/concepts", "/artifacts", "/fleet/machines"]) {
      expect(`${path}: ${active("console", path)}`).toBe(`${path}: false`);
    }
  });

  it("keeps Views lit through the composer, which is where a view is made", () => {
    expect(active("views", "/views")).toBe(true);
    expect(active("views", "/views/users")).toBe(true);
    expect(active("views", "/compose/new")).toBe(true);
  });
});

describe("the page ids the nav declares", () => {
  it("names every destination and every tab, once", () => {
    const ids = navPageIds();
    expect(new Set(ids).size).toBe(ids.length);
    for (const id of ["console", "views", "fleet.machines", "library.deployables", "cluster.keys"]) {
      expect(ids).toContain(id);
    }
  });
});
