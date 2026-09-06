import { describe, expect, it } from "vitest";

import {
  COLUMN_X,
  HOST_CHARS,
  NO_DOMAIN_LABEL,
  SUB_CHARS,
  ellipsize,
  layout,
  middleEllipsize,
  nodeCentre,
  shortHost,
  type MapNode,
} from "../../src/apps/deployables/map/layout";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { APEX, DOCS, MIRROR, PLATFORM_SITE, SHOP, siteRow } from "./harness";

// The map's arithmetic, on fixtures. No DOM, no GPU, no React -- which is what
// lets the layout be asserted rather than watched.

function model(rows: Parameters<typeof siteFromRow>[0][]) {
  return layout(rows.map(siteFromRow));
}

function byId(nodes: MapNode[], id: string): MapNode {
  const node = nodes.find((n) => n.id === id);
  if (!node) throw new Error(`no node ${id} (have: ${nodes.map((n) => n.id).join(", ")})`);
  return node;
}

describe("one domain", () => {
  const m = model([SHOP]);

  it("draws host -- site -- bundle -- artifact, in that order across", () => {
    expect(byId(m.nodes, "host:memql.example.com:shop.memql.example.com").x).toBe(COLUMN_X[0]);
    expect(byId(m.nodes, "site:site-shop").x).toBe(COLUMN_X[1]);
    expect(byId(m.nodes, "bundle:memql.example.com:blob://sites/site-shop/v1/").x).toBe(COLUMN_X[2]);
    expect(byId(m.nodes, "artifact:memql.example.com:artifact-zip").x).toBe(COLUMN_X[3]);
  });

  it("links them", () => {
    const pairs = m.edges.map((e) => `${e.from} -> ${e.to}`);
    expect(pairs).toEqual([
      "host:memql.example.com:shop.memql.example.com -> site:site-shop",
      "site:site-shop -> bundle:memql.example.com:blob://sites/site-shop/v1/",
      "bundle:memql.example.com:blob://sites/site-shop/v1/ -> artifact:memql.example.com:artifact-zip",
    ]);
    expect(m.edges.every((e) => e.siteId === "site-shop")).toBe(true);
  });

  it("carries the row facts the drawing renders", () => {
    const site = byId(m.nodes, "site:site-shop");
    expect(site.status).toBe("live");
    expect(site.siteKind).toBe("shopify_storefront");
    expect(byId(m.nodes, "bundle:memql.example.com:blob://sites/site-shop/v1/").label).toBe(
      "uploaded bundle",
    );
  });

  it("puts it in one group named for the domain", () => {
    expect(m.groups.map((g) => g.label)).toEqual(["memql.example.com"]);
    expect(m.groups[0]?.siteIds).toEqual(["site-shop"]);
  });
});

describe("a site with no bundle", () => {
  it("draws no bundle node -- an absence says it better than a box saying none", () => {
    const m = model([siteRow({ id: "site-bare", hostname: "bare.memql.example.com", bundleRef: "" })]);
    expect(m.nodes.filter((n) => n.kind === "bundle")).toHaveLength(0);
    expect(m.nodes.map((n) => n.kind).sort()).toEqual(["host", "site"]);
  });
});

describe("several domains", () => {
  const m = model([SHOP, APEX, PLATFORM_SITE]);

  it("groups by domain and sorts the groups", () => {
    expect(m.groups.map((g) => g.label)).toEqual(["example.org", "memql.example.com"]);
  });

  it("stacks the groups without overlapping them", () => {
    const [first, second] = m.groups;
    expect(first && second).toBeTruthy();
    expect(second!.y).toBeGreaterThan(first!.y + first!.h);
  });

  it("keeps an apex in its own group rather than folding it under a TLD", () => {
    expect(m.groups.find((g) => g.label === "example.org")?.siteIds).toEqual(["site-apex"]);
  });

  it("sorts sites within a group by hostname", () => {
    const under = m.groups.find((g) => g.label === "memql.example.com");
    expect(under?.siteIds).toEqual(["site-os", "site-shop"]);
  });
});

describe("a bundle serving two deployables", () => {
  const m = model([DOCS, MIRROR]);
  const bundleId = "bundle:memql.example.com:file:///app/sites/docs";

  it("is ONE node, not two that happen to carry the same text", () => {
    expect(m.nodes.filter((n) => n.kind === "bundle")).toHaveLength(1);
    expect(byId(m.nodes, bundleId).siteIds.sort()).toEqual(["site-docs", "site-mirror"]);
  });

  it("takes an edge from each deployable", () => {
    const into = m.edges.filter((e) => e.to === bundleId);
    expect(into.map((e) => e.siteId).sort()).toEqual(["site-docs", "site-mirror"]);
  });

  it("sits between the two rows it serves rather than on top of one of them", () => {
    const bundle = byId(m.nodes, bundleId);
    const docs = byId(m.nodes, "site:site-docs");
    const mirror = byId(m.nodes, "site:site-mirror");
    expect(nodeCentre(bundle).y).toBeCloseTo((nodeCentre(docs).y + nodeCentre(mirror).y) / 2, 5);
    expect(bundle.y).toBeGreaterThan(Math.min(docs.y, mirror.y));
    expect(bundle.y).toBeLessThan(Math.max(docs.y, mirror.y));
  });
});

describe("determinism", () => {
  it("lays the same rows out identically whatever order they arrive in", () => {
    // The collection folds events in the order the cluster sent them, so input
    // order is not something this function may depend on: a map that reshuffled
    // on an update would be unreadable exactly when somebody is watching it.
    const forwards = model([SHOP, DOCS, MIRROR, APEX, PLATFORM_SITE]);
    const backwards = model([PLATFORM_SITE, APEX, MIRROR, DOCS, SHOP]);
    expect(backwards).toEqual(forwards);
  });

  it("is a function of the rows and nothing else -- twice over the same input agrees", () => {
    expect(model([SHOP, DOCS])).toEqual(model([SHOP, DOCS]));
  });
});

describe("the edges of the input", () => {
  it("draws nothing for no rows", () => {
    expect(layout([])).toEqual({ nodes: [], edges: [], groups: [], width: 0, height: 0 });
  });

  it("drops a row with no id -- there is nothing to select or key on", () => {
    expect(layout([siteFromRow({ hostname: "orphan.example.com" })]).nodes).toHaveLength(0);
  });

  it("keeps a row whose hostname has not arrived, under a named group", () => {
    // A folded event carries only what the write touched. Dropping the row
    // would make a deployable disappear from the map until the next event,
    // which reads as a deletion.
    const m = model([siteRow({ id: "site-partial", hostname: "" })]);
    expect(m.groups.map((g) => g.label)).toEqual([NO_DOMAIN_LABEL]);
    expect(byId(m.nodes, "host::").label).toBe("--");
  });
});

describe("the canvas", () => {
  it("is at least as large as the content it holds", () => {
    const m = model([SHOP, DOCS, MIRROR, APEX]);
    for (const node of m.nodes) {
      expect(node.x + node.w).toBeLessThanOrEqual(m.width);
      expect(node.y + node.h).toBeLessThanOrEqual(m.height);
    }
  });
});

// ---------------------------------------------------------------------------
// Fitting text to a box
// ---------------------------------------------------------------------------
//
// SVG text neither wraps nor ellipsizes -- it runs on, out of its box and under
// whatever is beside it. The first cut of this map shipped exactly that: a
// hostname running under the kind glyph, which is legible in a test and
// unreadable on a screen. So the fitting is arithmetic, here, where it can be
// asserted.

describe("the short name a site node carries", () => {
  it("drops the domain the group heading already says", () => {
    expect(shortHost("blog.memql.example.com", "memql.example.com")).toBe("blog");
  });

  it("answers EMPTY for an apex, which is its own group and has nothing repeated", () => {
    // The caller then keeps the whole hostname: `example.org` under
    // `example.org` is not "" -- it is the name of the thing.
    expect(shortHost("example.org", "example.org")).toBe("");
  });

  it("answers empty rather than guessing when the hostname is not under that domain", () => {
    expect(shortHost("elsewhere.test", "memql.example.com")).toBe("");
    expect(shortHost("", "memql.example.com")).toBe("");
    expect(shortHost("blog.memql.example.com", "")).toBe("");
  });

  it("is what the site node renders, while the FULL value stays on the node", () => {
    const m = model([SHOP, APEX]);
    const shop = byId(m.nodes, "site:site-shop");
    expect(shop.label).toBe("shop");
    expect(shop.full).toBe("shop.memql.example.com");
    // An apex keeps the lot, because there is nothing repeated to remove.
    expect(byId(m.nodes, "site:site-apex").label).toBe("example.org");
  });
});

describe("truncation", () => {
  it("leaves anything that fits alone", () => {
    expect(ellipsize("shop", 10)).toBe("shop");
    expect(middleEllipsize("shop", 10)).toBe("shop");
    expect(ellipsize("exactlyten", 10)).toBe("exactlyten");
  });

  it("trims the tail for a label, and stays within budget", () => {
    const out = ellipsize("a-very-long-deployable-name", 10);
    expect(out).toBe("a-very-lo\u2026");
    expect(out.length).toBe(10);
  });

  it("trims the MIDDLE for a reference, keeping both ends", () => {
    // A bundle reference's head says which KIND it is and its tail says which
    // version. Losing either end makes two different bundles read the same.
    const ref = "blob://sites/site-shop/v7f3c19a2bb01/";
    const out = middleEllipsize(ref, 24);
    expect(out.length).toBe(24);
    expect(out.startsWith("blob://")).toBe(true);
    expect(out.endsWith("bb01/")).toBe(true);
    expect(out).toContain("\u2026");
  });

  it("keeps two references that differ only in their version DISTINGUISHABLE", () => {
    // The property the middle trim exists for, stated as one.
    const a = middleEllipsize("blob://sites/site-shop/v7f3c19a2bb01/", SUB_CHARS);
    const b = middleEllipsize("blob://sites/site-shop/v9c11a2000000/", SUB_CHARS);
    expect(a).not.toBe(b);
  });

  it("fits every rendered string to its column", () => {
    const m = model([SHOP, DOCS, MIRROR, APEX, PLATFORM_SITE]);
    for (const node of m.nodes) {
      const budget = node.kind === "host" ? HOST_CHARS : Math.max(HOST_CHARS, SUB_CHARS);
      expect(node.label.length, `${node.id} label`).toBeLessThanOrEqual(budget);
      expect(node.sublabel.length, `${node.id} sublabel`).toBeLessThanOrEqual(SUB_CHARS);
    }
  });

  it("never truncates the value a node is READ OUT as", () => {
    // `full` is what the aria-label and the tooltip use. A screen reader told
    // "blog.memql.exa..." has been given less than nothing.
    const m = model([SHOP]);
    expect(byId(m.nodes, "bundle:memql.example.com:blob://sites/site-shop/v1/").full).toBe(
      "blob://sites/site-shop/v1/",
    );
    expect(byId(m.nodes, "host:memql.example.com:shop.memql.example.com").full).toBe(
      "shop.memql.example.com",
    );
  });
});
