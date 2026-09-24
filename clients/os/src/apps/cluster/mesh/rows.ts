import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { absent, figureFrom, type Figure } from "../../../kit/measure";

// The Mesh section's rows: every engine replica, and what it hears of the
// events the cluster broadcasts (epic memql#5338).
//
// ===========================================================================
// WHERE THE NUMBERS COME FROM, AND HOW OLD THEY ARE
// ===========================================================================
// Each node writes its own report onto its own `v1:cluster:node` row as the
// `mesh` object, on the one-minute heartbeat that already writes the row
// (component/node/self_status_writer.go). So every figure here is a node's
// account of ITSELF, as of that node's last heartbeat -- up to a minute old,
// and dated by the row's `lastSeen`. The verdicts below are computed against
// that report time, never against the browser's clock: a report says what
// was true when it was written, and judging it against "now" would call a
// healthy node quiet for the minute between two heartbeats.
//
// ===========================================================================
// A NODE THAT HAS NOT REPORTED IS NOT A NODE THAT HEARS NOTHING
// ===========================================================================
// A row with no `mesh` object is an engine that has not written one: a
// replica still on an older release during a rollout, or one that has not
// reached its first heartbeat. Its counts are ABSENT (em dashes), never zero
// -- zero is what a deaf node reports, and the two lead to opposite actions.

/** The row's `mesh.links[]`: one peer this node holds a stream to. */
export interface MeshLink {
  node: string;
  type: string;
  /** Which side opened the stream: "dialed" (this node), "accepted" (the
   *  peer), or "both". An unrecognised value is kept verbatim. */
  via: string;
}

/** What a node says it hears. Null on a node that has not reported. */
export interface MeshReport {
  /** False only on identity, which takes no mesh events by design. */
  receives: boolean;
  /** When the reporting process started counting. */
  since: string;
  links: MeshLink[];
  heard: Figure;
  duplicates: Figure;
  originated: Figure;
  relayed: Figure;
  dropped: Figure;
  hopLimited: Figure;
  /** Empty until the node's first event. */
  lastHeardAt: string;
}

export interface MeshNode {
  id: string;
  nodeType: string;
  address: string;
  health: string;
  /** When the node last wrote its row -- the report's own date. */
  lastSeen: string;
  createdAt: string;
  report: MeshReport | null;
}

const CLUSTER_NODE_PREFIX = "v1:cluster:node:";

function str(row: Record<string, unknown> | null | undefined, key: string): string {
  const v = row?.[key];
  return typeof v === "string" ? v : "";
}

/**
 * The payload a wire row carries. A graph EVENT's payload wraps the row's
 * fields under `payload` beside the envelope; a READ returns them flat. Both
 * arrive through the same live collection, so the projection accepts both.
 */
function fieldsOf(row: Row): Record<string, unknown> {
  const inner = (row as Record<string, unknown>).payload;
  if (inner !== null && typeof inner === "object" && !Array.isArray(inner)) {
    return { ...(row as Record<string, unknown>), ...(inner as Record<string, unknown>) };
  }
  return row as Record<string, unknown>;
}

function reportFrom(raw: unknown): MeshReport | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const m = raw as Record<string, unknown>;
  const links: MeshLink[] = [];
  if (Array.isArray(m.links)) {
    for (const one of m.links) {
      if (one === null || typeof one !== "object") continue;
      const l = one as Record<string, unknown>;
      const node = typeof l.node === "string" ? l.node : "";
      if (node === "") continue;
      links.push({
        node,
        type: typeof l.type === "string" ? l.type : "",
        via: typeof l.via === "string" ? l.via : "",
      });
    }
  }
  return {
    // Absent means the report predates the field, and every node but
    // identity takes events: default to the rule, not to the exception.
    receives: m.receives !== false,
    since: typeof m.since === "string" ? m.since : "",
    links,
    heard: figureFrom(m, "heard"),
    duplicates: figureFrom(m, "duplicates"),
    originated: figureFrom(m, "originated"),
    relayed: figureFrom(m, "relayed"),
    dropped: figureFrom(m, "dropped"),
    hopLimited: figureFrom(m, "hopLimited"),
    lastHeardAt: typeof m.lastHeardAt === "string" ? m.lastHeardAt : "",
  };
}

/** Project a `v1:cluster:node` wire row. The id is the BARE node id. */
export function meshNodeFromRow(row: Row): MeshNode {
  const f = fieldsOf(row);
  const rawId = str(f, "id") || str(f, "nodeId");
  return {
    id: rawId.startsWith(CLUSTER_NODE_PREFIX) ? rawId.slice(CLUSTER_NODE_PREFIX.length) : rawId,
    nodeType: str(f, "nodeType"),
    address: str(f, "address"),
    health: str(f, "health"),
    lastSeen: str(f, "lastSeen"),
    createdAt: str(f, "createdAt"),
    report: reportFrom(f.mesh),
  };
}

// ---------------------------------------------------------------------------
// Which nodes are on the page
// ---------------------------------------------------------------------------

/** A node whose row has not moved for this long is not running: the self
 *  heartbeat writes it every minute, so five minutes is five missed beats. */
export const LIVE_WINDOW_MS = 5 * 60_000;

const GONE_HEALTH = new Set(["stopped", "offline"]);

/**
 * The replicas running now, and how many rows were left out. v1:cluster:node
 * keeps a row for every pod that ever ran -- every rollout adds a set -- so
 * the page would otherwise be mostly history. The count is said on the page:
 * a filter nobody can see is how a missing node reads as a healthy mesh.
 */
export function runningNodes(nodes: readonly MeshNode[], now: Date): { running: MeshNode[]; hidden: number } {
  const running: MeshNode[] = [];
  let hidden = 0;
  for (const n of nodes) {
    const seen = Date.parse(n.lastSeen);
    const fresh = Number.isFinite(seen) && now.getTime() - seen <= LIVE_WINDOW_MS;
    if (GONE_HEALTH.has(n.health.toLowerCase()) || !fresh) {
      hidden++;
      continue;
    }
    running.push(n);
  }
  return { running, hidden };
}

// ---------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------

/**
 * What a node hears, in one word. Ordered by the question an operator asks
 * first -- is anything deaf -- and decided against the REPORT's time.
 *
 *  - notReported: no report on the row. Absent, not zero.
 *  - sendsOnly:   identity. It takes no mesh events BY DESIGN, so a zero
 *                 there is the design and never an island.
 *  - starting:    up for less than STARTING_GRACE_MS; a zero is too early
 *                 to mean anything.
 *  - noLinks:     no stream in either direction. It cannot hear or send.
 *  - notHearing:  linked, up past the grace, and has heard nothing at all --
 *                 the island this epic exists to end.
 *  - quiet:       has heard, but nothing for QUIET_AFTER_MS before its
 *                 report. Every running node writes its row every minute and
 *                 that row broadcasts, so a node that hears the mesh at all
 *                 hears something every minute.
 *  - hearing:     everything else.
 */
export type HearingState =
  | "hearing"
  | "quiet"
  | "notHearing"
  | "noLinks"
  | "starting"
  | "sendsOnly"
  | "notReported";

export const STARTING_GRACE_MS = 3 * 60_000;
export const QUIET_AFTER_MS = 5 * 60_000;

export function hearingState(node: MeshNode): HearingState {
  const r = node.report;
  if (r === null) return "notReported";
  if (!r.receives) return "sendsOnly";
  const reportedAt = Date.parse(node.lastSeen);
  const since = Date.parse(r.since);
  const up = Number.isFinite(reportedAt) && Number.isFinite(since) ? reportedAt - since : Number.POSITIVE_INFINITY;
  if (up < STARTING_GRACE_MS) return "starting";
  if (r.links.length === 0) return "noLinks";
  if (r.heard.kind === "measured" && r.heard.value === 0) return "notHearing";
  const lastHeard = Date.parse(r.lastHeardAt);
  if (Number.isFinite(reportedAt) && Number.isFinite(lastHeard) && reportedAt - lastHeard > QUIET_AFTER_MS) {
    return "quiet";
  }
  return "hearing";
}

/** The state word a row carries. Sentence case, one or two words. */
export function stateWord(state: HearingState): string {
  switch (state) {
    case "hearing":
      return "Hearing";
    case "quiet":
      return "Quiet";
    case "notHearing":
      return "Not hearing";
    case "noLinks":
      return "No links";
    case "starting":
      return "Starting";
    case "sendsOnly":
      return "Sends only";
    case "notReported":
      return "Not reported";
  }
}

/** The dot's tone: accent for the good state, warn for the three that ask
 *  for a person, muted for everything that is not a problem. */
export function stateTone(state: HearingState): "accent" | "warn" | "muted" {
  if (state === "hearing") return "accent";
  if (state === "quiet" || state === "notHearing" || state === "noLinks") return "warn";
  return "muted";
}

/** Whether a state asks for a person. */
export function needsAttention(state: HearingState): boolean {
  return stateTone(state) === "warn";
}

function minutesBetween(later: string, earlier: string): number | null {
  const a = Date.parse(later);
  const b = Date.parse(earlier);
  if (!Number.isFinite(a) || !Number.isFinite(b)) return null;
  return Math.max(0, Math.round((a - b) / 60_000));
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/**
 * A span of whole minutes in the unit a person would say it in: minutes up to
 * two hours, then hours up to two days, then days. "180 minutes" is a sum the
 * reader has to do; "3 hours" is the answer. Rounded DOWN past the minutes, so
 * a span is never said longer than it was.
 */
export function spanOf(minutes: number): string {
  if (minutes < 120) return plural(minutes, "minute", "minutes");
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return plural(hours, "hour", "hours");
  return plural(Math.floor(hours / 24), "day", "days");
}

/**
 * The verdict as one sentence, for the node's page and a row's hover. States
 * a fact the report supports and stops there: it never guesses a cause the
 * page cannot see.
 */
export function stateSentence(node: MeshNode, state: HearingState = hearingState(node)): string {
  const r = node.report;
  switch (state) {
    case "notReported":
      return "This node has not reported what it hears. It may be on an older release, or not yet at its first heartbeat.";
    case "sendsOnly":
      return r !== null && r.links.length === 0
        ? "Identity takes no mesh events by design, and no node holds a stream to it, so nothing it writes reaches the rest of the cluster."
        : "Identity takes no mesh events by design. It sends its own events down the streams other nodes open to it.";
    case "starting":
      return "This node started less than three minutes before its report, so what it has heard is too early to read.";
    case "noLinks":
      return "No stream links this node to the mesh in either direction, so it can neither hear nor send an event.";
    case "notHearing": {
      const up = r === null ? null : minutesBetween(node.lastSeen, r.since);
      return up === null
        ? "This node has heard nothing since it started, although it is linked to the mesh."
        : `This node has heard nothing in the ${spanOf(up)} since it started, although it is linked to the mesh.`;
    }
    case "quiet": {
      const gap = r === null ? null : minutesBetween(node.lastSeen, r.lastHeardAt);
      return gap === null
        ? "This node has heard nothing for a while before its report."
        : `This node heard nothing for ${spanOf(gap)} before its report. A node that hears the mesh hears something every minute.`;
    }
    case "hearing":
      return "This node hears the cluster.";
  }
}

// ---------------------------------------------------------------------------
// The list, grouped
// ---------------------------------------------------------------------------

/** Node types in the order a reader meets them: the front, the workers, the
 *  surfaces at the edge, then identity. An unknown type sorts last, by name. */
const TYPE_ORDER = ["bff", "agent", "planner", "workbench", "edge", "mcp", "identity"];

export function typeLabel(nodeType: string): string {
  switch (nodeType) {
    case "bff":
      return "BFF";
    case "mcp":
      return "MCP";
    case "":
      return "Unknown type";
    default:
      return nodeType.charAt(0).toUpperCase() + nodeType.slice(1);
  }
}

export interface MeshGroup {
  nodeType: string;
  label: string;
  nodes: MeshNode[];
}

/** Group by node type, types in TYPE_ORDER, nodes by id within each. */
export function groupByType(nodes: readonly MeshNode[]): MeshGroup[] {
  const byType = new Map<string, MeshNode[]>();
  for (const n of nodes) {
    const held = byType.get(n.nodeType);
    if (held) held.push(n);
    else byType.set(n.nodeType, [n]);
  }
  const rank = (t: string) => {
    const i = TYPE_ORDER.indexOf(t);
    return i < 0 ? TYPE_ORDER.length : i;
  };
  return [...byType.entries()]
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
    .map(([nodeType, group]) => ({
      nodeType,
      label: typeLabel(nodeType),
      nodes: [...group].sort((x, y) => x.id.localeCompare(y.id)),
    }));
}

// ---------------------------------------------------------------------------
// The distribution, and the one sentence above the list
// ---------------------------------------------------------------------------

export interface MeshTally {
  hearing: number;
  /** Linked and has heard nothing, or has no link at all: deaf. */
  notHearing: number;
  /** Heard, then nothing for QUIET_AFTER_MS before its report. */
  quiet: number;
  sendsOnly: number;
  starting: number;
  notReported: number;
  /** The deaf nodes and the quiet ones, each in list order. */
  notHearingNodes: MeshNode[];
  quietNodes: MeshNode[];
}

export function tally(nodes: readonly MeshNode[]): MeshTally {
  const t: MeshTally = {
    hearing: 0,
    notHearing: 0,
    quiet: 0,
    sendsOnly: 0,
    starting: 0,
    notReported: 0,
    notHearingNodes: [],
    quietNodes: [],
  };
  for (const group of groupByType(nodes)) {
    for (const n of group.nodes) {
      switch (hearingState(n)) {
        case "hearing":
          t.hearing++;
          break;
        case "notHearing":
        case "noLinks":
          t.notHearing++;
          t.notHearingNodes.push(n);
          break;
        case "quiet":
          t.quiet++;
          t.quietNodes.push(n);
          break;
        case "sendsOnly":
          t.sendsOnly++;
          break;
        case "starting":
          t.starting++;
          break;
        case "notReported":
          t.notReported++;
          break;
      }
    }
  }
  return t;
}

/**
 * A piece of the sentence above the list: words, or a node it names. A named
 * node is its own piece so the page can draw it as the way to that node's
 * page, and keep an id whole on one line -- an id broken at its hyphen reads
 * as two names.
 */
export type MeshSentencePart = string | { node: string };

/** Up to two names, then a count: "a and b", "a, b and 3 others". */
function nameSome(nodes: readonly MeshNode[]): MeshSentencePart[] {
  const named = nodes.slice(0, 2).map((n): MeshSentencePart => ({ node: n.id }));
  const rest = nodes.length - named.length;
  if (rest > 0) return [named[0]!, ", ", named[1]!, ` and ${plural(rest, "other", "others")}`];
  return named.length === 2 ? [named[0]!, " and ", named[1]!] : named;
}

/**
 * The page's sentence about the whole mesh. A deaf node and a quiet one are
 * DIFFERENT FACTS with different causes -- one never heard, one stopped
 * hearing -- so each gets its own clause, the deaf first, and each names its
 * nodes (two by name, then a count). With neither, it says once that every
 * node taking events hears. It never calls a mesh healthy on the strength of
 * nodes that did not report: those are counted, not assumed.
 */
export function meshSentenceParts(t: MeshTally): MeshSentencePart[] {
  const parts: MeshSentencePart[] = [];
  if (t.notHearing > 0) {
    if (t.notHearing === 1) parts.push(...nameSome(t.notHearingNodes), " is not hearing the cluster.");
    else parts.push(`${plural(t.notHearing, "node is", "nodes are")} not hearing the cluster: `, ...nameSome(t.notHearingNodes), ".");
  }
  if (t.quiet > 0) {
    if (parts.length > 0) parts.push(" ");
    if (t.quiet === 1) parts.push(...nameSome(t.quietNodes), " has gone quiet.");
    else parts.push(`${plural(t.quiet, "node has", "nodes have")} gone quiet: `, ...nameSome(t.quietNodes), ".");
  }
  if (parts.length > 0) return parts;
  const taking = t.hearing + t.starting;
  if (taking === 0) return ["No node has reported what it hears yet."];
  const unreported = t.notReported > 0 ? ` ${plural(t.notReported, "node has", "nodes have")} not reported.` : "";
  return [`Every node that takes events is hearing the cluster.${unreported}`];
}

/** The same sentence as plain text -- what a person reads. */
export function meshSentence(t: MeshTally): string {
  return meshSentenceParts(t)
    .map((p) => (typeof p === "string" ? p : p.node))
    .join("");
}

// ---------------------------------------------------------------------------
// A node's links, as the patch bay draws them
// ---------------------------------------------------------------------------

export interface LinkedPeer {
  id: string;
  type: string;
  /** The peer's own row, when it is on the page; null for a peer this page
   *  does not list (stopped, or not reporting a row at all). */
  node: MeshNode | null;
}

/**
 * Split a node's links by which side opened the stream. A peer linked both
 * ways appears on BOTH sides: two streams exist, and the page says what is
 * true. Sorted by type order, then id, so the two columns scan alike.
 */
export function linkSides(node: MeshNode, byId: ReadonlyMap<string, MeshNode>): { opened: LinkedPeer[]; accepted: LinkedPeer[] } {
  const opened: LinkedPeer[] = [];
  const accepted: LinkedPeer[] = [];
  for (const l of node.report?.links ?? []) {
    const peer: LinkedPeer = { id: l.node, type: l.type, node: byId.get(l.node) ?? null };
    if (l.via === "dialed" || l.via === "both") opened.push(peer);
    if (l.via === "accepted" || l.via === "both") accepted.push(peer);
  }
  const rank = (t: string) => {
    const i = TYPE_ORDER.indexOf(t);
    return i < 0 ? TYPE_ORDER.length : i;
  };
  const order = (a: LinkedPeer, b: LinkedPeer) => rank(a.type) - rank(b.type) || a.id.localeCompare(b.id);
  return { opened: opened.sort(order), accepted: accepted.sort(order) };
}

/** Distinct peers a node is linked to, whichever way. */
export function linkCount(node: MeshNode): number {
  return new Set((node.report?.links ?? []).map((l) => l.node)).size;
}

/** The figure for a count on a node with no report: absent, never zero. */
export function reportFigure(node: MeshNode, pick: (r: MeshReport) => Figure): Figure {
  return node.report === null ? absent("unmeasured") : pick(node.report);
}

/**
 * The arrival-cue fingerprint (clients/os/README.md, "a heartbeat is not
 * news"). Only what a PERSON would call a change: the verdict word, the
 * node's health, and its set of links. Every counter moves on every report,
 * forever, and naming one would make the list ring once a minute per node.
 */
export function meshFingerprint(node: MeshNode): string {
  const links = (node.report?.links ?? []).map((l) => `${l.node}:${l.via}`).sort().join(",");
  return `${hearingState(node)}|${node.health}|${links}`;
}
