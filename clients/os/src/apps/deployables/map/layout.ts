import { bundleForm, bundleFormLabel, domainOf, siteName, type SiteRow } from "../rows";

// The deploy map's arithmetic: rows in, positioned nodes and edges out.
//
// ===========================================================================
// PURE, AND SEPARATE FROM THE RENDERER, FOR THE REASON NEXUS'S scene/ IS
// ===========================================================================
// The portal's Nexus draws with WebGL and keeps its layout in functions over
// rows, fixture-tested with no GPU. This map draws with plain SVG and keeps
// the same split, because the split is not about the renderer: a layout
// asserted through a rendered document is asserted through React, the DOM and
// a stylesheet, each of which can fail for reasons that have nothing to do
// with where a node went.
//
// What this file must never gain is a DOM read. Every number below is computed
// from the rows and the constants; nothing measures text, so the same row set
// lays out identically in a test, on a phone and in a screenshot.
//
// DETERMINISM IS A PROPERTY, NOT A COINCIDENCE. Groups sort by domain and
// sites sort by hostname INSIDE this function rather than relying on
// `sitesAll`'s own sort, because the collection also holds rows folded in from
// events, which arrive in the order the cluster sent them. A map that reshuffled
// when a row updated would be unreadable exactly when somebody is watching it.

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

/** Column origins: host, site, bundle, artifact. */
export const COLUMN_X = [0, 220, 460, 700] as const;

export const NODE_W = 200;
export const NODE_H = 44;
/** Vertical pitch between two sites in one group. */
export const ROW_PITCH = 62;
/** Space a group's domain heading occupies above its first row. */
export const GROUP_HEAD_H = 30;
/** Padding inside a group box, and the gap between two groups. */
export const GROUP_PAD = 14;
export const GROUP_GAP = 26;
/** Canvas margin, so a focus ring on an edge node is not clipped. */
export const CANVAS_PAD = 16;

/**
 * Character budgets, per column.
 *
 * CHARACTERS RATHER THAN MEASURED PIXELS, deliberately: measuring means
 * touching the DOM, which would make this function impure, untestable without
 * a browser, and dependent on whether a webfont had loaded when it ran. The
 * budgets are set from the narrowest case -- the monospace host line, whose
 * advance is the widest of the three -- so a proportional label always has room
 * to spare.
 */
export const HOST_CHARS = 26;
export const SITE_CHARS = 22;
export const SUB_CHARS = 28;

export type MapNodeKind = "host" | "site" | "bundle" | "artifact";

export interface MapNode {
  /** Stable across renders and across row updates -- the arrival cue keys on it. */
  id: string;
  kind: MapNodeKind;
  /**
   * The line rendered in the node, already fitted to it.
   *
   * SVG text neither wraps nor ellipsizes: it simply runs on, out of its box
   * and under whatever is beside it. So the fitting happens HERE, where it is a
   * function of the row and can be asserted, rather than being left to a
   * stylesheet that has no rule capable of doing it.
   */
  label: string;
  /** The quieter second line, or "". */
  sublabel: string;
  /**
   * The untruncated primary value.
   *
   * What the node is READ OUT as and what its tooltip shows, because a screen
   * reader hearing "blog.memql.exa..." has been told less than nothing. The
   * drawing is what gets fitted; the fact does not.
   */
  full: string;
  x: number;
  y: number;
  w: number;
  h: number;
  /** Domain group this node belongs to. */
  group: string;
  /**
   * Every site this node is part of.
   *
   * One entry for a host or a site node. SEVERAL for a bundle serving more
   * than one deployable, which is the case the layout exists to make visible
   * -- two sites pointing at one bundle is a fact you cannot see in a table.
   */
  siteIds: string[];
  /** Site nodes only: drives the status dot and the kind glyph. */
  status: string;
  siteKind: string;
}

export interface MapEdge {
  id: string;
  from: string;
  to: string;
  /** The site whose story this edge is part of, for selection highlighting. */
  siteId: string;
}

export interface MapGroup {
  id: string;
  /** The domain, as rendered. "" becomes the honest placeholder below. */
  domain: string;
  label: string;
  x: number;
  y: number;
  w: number;
  h: number;
  siteIds: string[];
}

export interface MapLayout {
  nodes: MapNode[];
  edges: MapEdge[];
  groups: MapGroup[];
  width: number;
  height: number;
}

export const EMPTY_LAYOUT: MapLayout = {
  nodes: [],
  edges: [],
  groups: [],
  width: 0,
  height: 0,
};

/**
 * A hostname with no domain to group under.
 *
 * It should not happen -- `hostname` is `string!` -- but a folded event
 * carries only what the write touched, so a row can reach the map mid-flight
 * with the field absent. Grouping it under a named placeholder is honest;
 * dropping it would make a deployable disappear from the map for as long as it
 * took the next event to arrive, which reads as a deleted site.
 */
export const NO_DOMAIN_LABEL = "no hostname yet";

/**
 * Lay a row set out.
 *
 * Sites are grouped by domain, one group per box, and within a group each site
 * occupies one horizontal band: its host on the left, the site itself beside
 * it, the bundle it serves next, and -- when the bundle was published from the
 * Library -- the artifact it came out of on the right.
 *
 * A BUNDLE NODE IS DEDUPED WITHIN ITS GROUP and centred on the sites it serves,
 * so a shared bundle is one node with two edges into it rather than two nodes
 * that happen to carry the same text. Deduping is per GROUP rather than
 * globally on purpose: a group is a self-contained cluster, and an edge running
 * the width of the canvas between two domains would cost more legibility than
 * the fact it carries is worth. A bundle serving sites under two domains is
 * therefore drawn once per domain, which is also a true reading of it -- it is
 * serving in two places.
 */
export function layout(sites: readonly SiteRow[]): MapLayout {
  const usable = sites.filter((s) => s.id !== "");
  if (usable.length === 0) return EMPTY_LAYOUT;

  const byDomain = new Map<string, SiteRow[]>();
  for (const site of usable) {
    const domain = domainOf(site.hostname);
    const held = byDomain.get(domain);
    if (held) held.push(site);
    else byDomain.set(domain, [site]);
  }

  const domains = [...byDomain.keys()].sort((a, b) => compare(a, b));

  const nodes: MapNode[] = [];
  const edges: MapEdge[] = [];
  const groups: MapGroup[] = [];

  let cursorY = CANVAS_PAD;
  let widest = 0;

  for (const domain of domains) {
    const members = [...(byDomain.get(domain) ?? [])].sort((a, b) =>
      compare(siteName(a).toLowerCase(), siteName(b).toLowerCase()),
    );

    const groupTop = cursorY;
    const firstRowY = groupTop + GROUP_HEAD_H + GROUP_PAD;

    // Shared nodes are placed once and then re-centred; `ys` remembers every
    // band that points at them so the centring is a mean rather than the
    // position of whichever site happened to be first.
    const sharedYs = new Map<string, number[]>();
    let groupRight = 0;

    members.forEach((site, index) => {
      const bandY = firstRowY + index * ROW_PITCH;
      const form = bundleForm(site.bundleRef);

      const hostId = `host:${domain}:${site.hostname}`;
      const siteId = `site:${site.id}`;

      // A hostname is unique cluster-wide, so a host node is per site in
      // practice; it is still keyed by hostname so two rows that somehow claim
      // one host collapse into the one host they actually share.
      pushOnce(nodes, {
        id: hostId,
        kind: "host",
        label: site.hostname === "" ? "--" : middleEllipsize(site.hostname, HOST_CHARS),
        sublabel: "",
        full: site.hostname || "--",
        x: COLUMN_X[0],
        y: bandY,
        w: NODE_W,
        h: NODE_H,
        group: domain,
        siteIds: [site.id],
        status: "",
        siteKind: "",
      });
      remember(sharedYs, hostId, bandY);

      nodes.push({
        id: siteId,
        kind: "site",
        // THE LABEL, NOT THE HOSTNAME. The group heading already says the
        // domain, so repeating it on every node inside that group spends the
        // whole box on the half that is identical everywhere -- and then
        // overflows it. An apex, whose hostname IS its group, keeps the lot.
        label: ellipsize(shortHost(site.hostname, domain) || siteName(site), SITE_CHARS),
        sublabel: site.title.trim() === "" ? "" : ellipsize(site.title, SUB_CHARS),
        full: siteName(site),
        x: COLUMN_X[1],
        y: bandY,
        w: NODE_W,
        h: NODE_H,
        group: domain,
        siteIds: [site.id],
        status: site.status,
        siteKind: site.kind,
      });

      edges.push({ id: `${hostId}->${siteId}`, from: hostId, to: siteId, siteId: site.id });

      // A site with no bundleRef gets no bundle node: the map draws row facts,
      // and "nothing is deployed here" is a fact best told by an absence rather
      // than by a box saying "none".
      if (form !== "none") {
        const bundleId = `bundle:${domain}:${site.bundleRef}`;
        pushOnce(nodes, {
          id: bundleId,
          kind: "bundle",
          label: bundleFormLabel(form),
          // MIDDLE-ellipsized: a bundle reference's head says which KIND of
          // reference it is and its tail says which version, and losing either
          // end makes two different bundles read the same.
          sublabel: middleEllipsize(site.bundleRef, SUB_CHARS),
          full: site.bundleRef,
          x: COLUMN_X[2],
          y: bandY,
          w: NODE_W,
          h: NODE_H,
          group: domain,
          siteIds: [],
          status: "",
          siteKind: "",
        });
        addSite(nodes, bundleId, site.id);
        remember(sharedYs, bundleId, bandY);
        edges.push({ id: `${siteId}->${bundleId}`, from: siteId, to: bundleId, siteId: site.id });
        groupRight = Math.max(groupRight, COLUMN_X[2] + NODE_W);

        if (site.artifactId.trim() !== "") {
          const artifactId = `artifact:${domain}:${site.artifactId}`;
          pushOnce(nodes, {
            id: artifactId,
            kind: "artifact",
            label: "Library artifact",
            sublabel: middleEllipsize(site.artifactId, SUB_CHARS),
            full: site.artifactId,
            x: COLUMN_X[3],
            y: bandY,
            w: NODE_W,
            h: NODE_H,
            group: domain,
            siteIds: [],
            status: "",
            siteKind: "",
          });
          addSite(nodes, artifactId, site.id);
          remember(sharedYs, artifactId, bandY);
          edges.push({
            id: `${bundleId}->${artifactId}`,
            from: bundleId,
            to: artifactId,
            siteId: site.id,
          });
          groupRight = Math.max(groupRight, COLUMN_X[3] + NODE_W);
        }
      }

      groupRight = Math.max(groupRight, COLUMN_X[1] + NODE_W);
    });

    // Re-centre every shared node on the mean of the bands that point at it.
    for (const [id, ys] of sharedYs) {
      if (ys.length < 2) continue;
      const node = nodes.find((n) => n.id === id);
      if (!node) continue;
      node.y = ys.reduce((a, b) => a + b, 0) / ys.length;
    }

    const groupHeight = GROUP_HEAD_H + GROUP_PAD * 2 + members.length * ROW_PITCH - (ROW_PITCH - NODE_H);
    groups.push({
      id: `group:${domain}`,
      domain,
      label: domain === "" ? NO_DOMAIN_LABEL : domain,
      x: 0,
      y: groupTop,
      w: groupRight,
      h: groupHeight,
      siteIds: members.map((s) => s.id),
    });

    widest = Math.max(widest, groupRight);
    cursorY = groupTop + groupHeight + GROUP_GAP;
  }

  return {
    nodes,
    edges,
    groups,
    width: widest + CANVAS_PAD * 2,
    height: Math.max(cursorY - GROUP_GAP + CANVAS_PAD, CANVAS_PAD * 2),
  };
}

/**
 * Byte-order comparison rather than `localeCompare`.
 *
 * A locale-aware sort is a function of the READER's browser, so two people
 * looking at the same cluster would see the map in two orders, and a fixture
 * asserted on one machine would be asserted on nothing. Hostnames are ASCII by
 * construction here, so there is nothing a locale would improve.
 */
function compare(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * A hostname with its group's domain taken off -- `blog` under
 * `memql.example.com`.
 *
 * Returns "" when the hostname is not under that domain at all, which is the
 * APEX case (`example.org` IS its group): the caller then keeps the whole
 * hostname, because there is nothing repeated to remove.
 */
export function shortHost(hostname: string, domain: string): string {
  const host = hostname.trim().toLowerCase();
  if (host === "" || domain === "") return "";
  const suffix = `.${domain}`;
  return host.endsWith(suffix) ? host.slice(0, -suffix.length) : "";
}

/** Trim the tail, keeping a character of budget for the ellipsis. */
export function ellipsize(text: string, max: number): string {
  return text.length <= max ? text : `${text.slice(0, Math.max(0, max - 1))}\u2026`;
}

/**
 * Trim the MIDDLE.
 *
 * For a URI or a hostname, both ends carry meaning -- the scheme and the
 * version, the label and the domain -- and a tail trim throws one of them away.
 * Two bundles differing only in their version would then render identically,
 * which is worse than showing less of each.
 */
export function middleEllipsize(text: string, max: number): string {
  if (text.length <= max) return text;
  const keep = max - 1;
  const head = Math.ceil(keep / 2);
  const tail = keep - head;
  return `${text.slice(0, head)}\u2026${tail === 0 ? "" : text.slice(-tail)}`;
}

function pushOnce(nodes: MapNode[], node: MapNode): void {
  if (nodes.some((n) => n.id === node.id)) return;
  nodes.push(node);
}

function addSite(nodes: MapNode[], id: string, siteId: string): void {
  const node = nodes.find((n) => n.id === id);
  if (node && !node.siteIds.includes(siteId)) node.siteIds.push(siteId);
}

function remember(map: Map<string, number[]>, id: string, y: number): void {
  const held = map.get(id);
  if (held) held.push(y);
  else map.set(id, [y]);
}

/** The centre of a node, which is where an edge attaches. */
export function nodeCentre(node: MapNode): { x: number; y: number } {
  return { x: node.x + node.w / 2, y: node.y + node.h / 2 };
}
