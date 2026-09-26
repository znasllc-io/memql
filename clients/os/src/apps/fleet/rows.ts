import { bareShortId, rowArray, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { absent, figureFrom, figureOf, figureValue, type Figure } from "../../kit/measure";
import { flatten } from "../../kit/rows";
import { labelMapFrom, mergeLabels, type LabelMap, type MergedLabel } from "./labels";
import { hardwareFrom, type MachineHardware } from "./machines/hardware";

// The wire rows the Fleet renders, projected into the shapes its surfaces
// read.
//
// PURE, and separate from every component, for the reason the portal's
// src/fleet/rows.ts is: a projection asserted through render() is asserted
// through three layers that can each fail for unrelated reasons. Everything
// here is a function of a row and is unit-testable with no browser, no
// cluster and no React.
//
// ===========================================================================
// WHY EVERY PROJECTION FLATTENS FIRST
// ===========================================================================
// A row reaches these functions from two places: the SEED (a named query's
// result, already shape-flattened) and the SUBSCRIPTION fold (a CDC envelope).
// The envelope flattens the concept fields alongside the intrinsics, but a
// `payload`-wrapped form is what a raw graph event carries, and the two paths
// have to produce the same object or a machine would render one way on load
// and another way the moment its heartbeat lands. `flatten` (kit/rows.ts,
// promoted once the third app copied it) is that one reconciliation, applied
// before any field is read.

function stringList(row: Row, key: string): string[] {
  const raw = rowArray(row, key) ?? [];
  return raw.filter((entry): entry is string => typeof entry === "string");
}

function numberMap(row: Row, key: string): Record<string, number> {
  const raw = rowObject(row, key);
  if (raw === null) return {};
  const out: Record<string, number> = {};
  for (const [k, v] of Object.entries(raw)) {
    if (typeof v === "number") out[k] = v;
    else if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) out[k] = Number(v);
  }
  return out;
}

function nestedString(nested: Record<string, unknown> | null, key: string): string {
  const v = nested?.[key];
  return typeof v === "string" ? v : "";
}

// ---------------------------------------------------------------------------
// A machine
// ---------------------------------------------------------------------------

export interface MachineRow {
  id: string;
  ownerUserId: string;
  /** What the cockpit reported -- a hostname by default, and RE-STAMPED on
   *  every reconnect, which is why it is not the name a surface shows. */
  name: string;
  /** What the owner called it (renameWorker). Empty until renamed. */
  displayName: string;
  identityId: string;
  capabilities: string[];
  os: string;
  arch: string;
  hostname: string;
  platform: string;
  /** quartz / x11 / wayland / none, from the capability descriptor. Empty
   *  when the machine's build predates the descriptor -- a fact worth
   *  rendering as "not reported" rather than as "none". */
  displayServer: string;
  /** The descriptor's build-tag flag, not proof of a usable desktop.
   *  Render computerUseStatus when describing whether control is available. */
  computerUseAvailable: boolean;
  reportedLabels: LabelMap;
  operatorLabels: LabelMap;
  mergedLabels: MergedLabel[];
  /** Per-capability parallelism cap, e.g. {HEADLESS: 8, COMPUTERUSE: 1}. */
  concurrency: Record<string, number>;
  /** Calls in flight as of the most recent heartbeat. Up to one interval
   *  stale by construction -- a routing input, never a correctness one. */
  activeCount: number;
  lastSeenAt: string;
  /** MEMQL_NODE_ID of the replica holding this machine's stream. Empty
   *  means no replica holds it. */
  connectedNodeId: string;
  lastSelectedAt: string;
  version: string;
  buildTag: string;
  registeredAt: string;
  revokedAt: string;
  revokedBy: string;
  revokeReason: string;
  /** The local apps this machine reported (memql#4359). */
  apps: MachineApp[];
  /** What the machine IS, as its cockpit reported it (epic memql#5146, D1).
   *  `present` false means the cockpit predates the field -- NOT a machine
   *  with no memory. See machines/hardware.ts. */
  hardware: MachineHardware;
  /** The OWNER's half of the sharing consent (D6, epic memql#5344 G1). The
   *  cockpit's half is `inferenceServe` beside it, and a machine serves
   *  anybody but its owner only when the owner said `people` or `cluster`
   *  AND the cockpit said cluster. */
  sharingMode: SharingMode;
  /** Under `people`: the users it is lent to, in the spelling stored and in
   *  stored order. EMPTY under the other two modes whatever the row holds --
   *  a list that is not in force is residue, not a share (design G10). */
  sharedUserIds: string[];
  /** Under `people`: the groups whose ACTIVE members may use it. Empty
   *  otherwise, for the same reason. */
  sharedGroupIds: string[];
  sharedAt: string;
  sharedBy: string;
  /** The COCKPIT's half, from that machine's own policy.yaml. Empty reads as
   *  "owner": a cockpit that predates the field has said nothing, and silence
   *  is not agreement to run other people's work on somebody's laptop. */
  inferenceServe: string;
  /** The TCC / X11 snapshot the cockpit took at register time and refreshes
   *  on measured heartbeats. `present` false means the cockpit predates the
   *  report -- NOT a machine that was refused everything. */
  permissions: MachinePermissions;
  /** The cluster's own round trip to this machine (design record
   *  2026-09-08-cockpit-install-wizard, D11): the last Ping the holding
   *  replica sent, answered by the cockpit, in milliseconds. ZERO WITH AN
   *  EMPTY `rttAt` MEANS NOT MEASURED -- a cockpit that predates the ping
   *  never answers -- and is never rendered as a figure. */
  rttMs: number;
  rttAt: string;
  /** How far this machine's OWN CLOCK sits from the cluster's, in
   *  milliseconds, positive when the machine is ahead (epic memql#5327,
   *  design D6).
   *
   *  ZERO IS A REAL ANSWER -- the clocks agree -- so `clockSkewKnown` beside
   *  it is what distinguishes agreement from a cockpit that stamps no
   *  timestamp at all. Nothing routes on this; it exists because the
   *  difference USED to decide `online`, and a machine that read offline
   *  while it was connected now has an explanation rather than a mystery. */
  clockSkewMs: number;
  clockSkewKnown: boolean;
  /** When the credential this machine connects with stops being accepted
   *  (epic memql#5327, design D4). EMPTY MEANS NO EXPIRY, which is every
   *  token minted before expiry existed -- not "unknown", and not "expired". */
  credentialExpiresAt: string;
}

/**
 * What the machine may do on its own desktop, as the worker probed it
 * (`registration.permissions`: accessibility, screen_recording, x11_display,
 * detail).
 *
 * PRESENCE IS DECIDED BY CONTENT, the `hardware` rule: an object with no key
 * set and no key at all read the same, and a cockpit that never sent the
 * snapshot has said nothing -- reading that silence as "denied" would send a
 * person to System Settings to grant something that was never asked about.
 */
export type PermissionDecision = "granted" | "denied" | "unknown";
export interface MachinePermissions {
  present: boolean;
  accessibility: boolean;
  screenRecording: boolean;
  x11Display: boolean;
  accessibilityState?: PermissionDecision;
  screenRecordingState?: PermissionDecision;
  x11DisplayState?: PermissionDecision;
  checkedAt?: string;
  probeContext?: string;
  detail: string;
}

const NO_PERMISSIONS: MachinePermissions = {
  present: false,
  accessibility: false,
  screenRecording: false,
  x11Display: false,
  detail: "",
};

export function permissionDecision(p: MachinePermissions, key: "accessibility" | "screenRecording" | "x11Display"): PermissionDecision {
  return p[`${key}State`] ?? (!p.present ? "unknown" : p[key] ? "granted" : "denied");
}

export function permissionsFrom(raw: unknown): MachinePermissions {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return NO_PERMISSIONS;
  const obj = raw as Record<string, unknown>;
  const detail = typeof obj.detail === "string" ? obj.detail : "";
  const stub = detail.toLowerCase().includes("not yet implemented");
  const decision = (key: string): PermissionDecision => {
    if (stub) return "unknown";
    if (`${key}_state` in obj) return obj[`${key}_state`] === "granted" ? "granted" : obj[`${key}_state`] === "denied" ? "denied" : "unknown";
    return typeof obj[key] === "boolean" ? obj[key] ? "granted" : "denied" : "unknown";
  };
  const a = decision("accessibility"), screen = decision("screen_recording"), x = decision("x11_display");
  const present = ["accessibility", "screen_recording", "x11_display"].some(k => typeof obj[k] === "boolean" || `${k}_state` in obj) || detail.trim() !== "";
  if (!present) return NO_PERMISSIONS;
  return { present, accessibility: a === "granted", screenRecording: screen === "granted", x11Display: x === "granted", accessibilityState: a, screenRecordingState: screen, x11DisplayState: x, detail, checkedAt: typeof obj.checked_at === "string" ? obj.checked_at : "", probeContext: typeof obj.probe_context === "string" ? obj.probe_context : "" };
}

/** Read the desktop facts together: a computer-use binary can run on
 *  Wayland, without a display, or without the required macOS permissions. */
export function computerUseStatus(machine: MachineRow): {
  state: "available" | "unavailable" | "unknown";
  answer: string;
} {
  const display = machine.displayServer.trim().toLowerCase();
  // An absent optional descriptor projects to an empty display and false
  // support flag. That is missing evidence, not an installation diagnosis.
  if (display === "") {
    return { state: "unknown", answer: "Desktop session not reported. Computer-use availability is unknown." };
  }
  if (display === "wayland") {
    return { state: "unavailable", answer: "Unavailable on Wayland. Computer use requires an X11 session." };
  }
  if (!machine.computerUseAvailable) {
    return { state: "unavailable", answer: "Unavailable — Computer-use support is not reported. Install the computer-use version of Cockpit." };
  }
  if (display === "none") {
    return { state: "unavailable", answer: "Unavailable — No display reported. Sign in to a desktop session (X11 on Linux)." };
  }
  if (!machine.capabilities.includes("COMPUTERUSE")) {
    return { state: "unavailable", answer: "Unavailable — This machine has not registered for computer use." };
  }
  if (display !== "x11" && display !== "quartz") {
    return { state: "unknown", answer: "Desktop session not reported. Computer-use availability is unknown." };
  }
  const permissions = machine.permissions;
  if (display === "quartz" || machine.os === "darwin") {
    const states = [{ name: "Accessibility", state: permissionDecision(permissions,"accessibility") }, { name: "Screen Recording", state: permissionDecision(permissions,"screenRecording") }];
    const denied = states.filter(s => s.state === "denied").map(s => s.name);
    if (denied.length) return { state: "unavailable", answer: `Unavailable — ${denied.join(" and ")} not granted to the running worker. Allow that process in macOS Privacy & Security.` };
    if (states.some(s => s.state === "unknown")) return { state: "unknown", answer: "macOS permissions are not fully verified by the running worker. Terminal setup success does not verify its LaunchAgent." };
    return { state: "available", answer: "Available — Accessibility and Screen Recording granted." };
  }
  const x11 = permissionDecision(permissions,"x11Display");
  if (x11 === "unknown") return { state: "unknown", answer: "The running worker has not verified access to its X11 display." };
  if (x11 === "denied") return { state: "unavailable", answer: "Unavailable — The X11 display is not available to the worker." };
  return { state: "available", answer: "Available — X11 desktop reported." };
}

/** One local app on a machine. */
export interface MachineApp {
  id: string;
  label: string;
  version: string;
  signedIn: boolean;
  allowed: boolean;
  /** unknown | none | present, as the app REPORTS it. Never inferred. */
  subscription: string;
  runnable: boolean;
  /** Empty when runnable; otherwise which half is missing, so a person
   *  reading the panel knows what to go and fix. */
  why: string;
}

// The engine's CLOSED runnable set (component/planner's executor registry and
// integrations/agent/worker/cockpitapp.go), mirrored so this surface agrees
// with selection. An id outside it is DISPLAYED -- the machine really has it
// -- and never marked runnable, because this engine has no protocol for it.
//
// ORDERED, and exported as the ordered list rather than as the set, because
// the delegation policy's `appOrder` is a PRIORITY and its editor has to
// offer the members in a stable order. The set is derived from the list so
// the two cannot disagree about membership (epic memql#5009).
export const RUNNABLE_APPS = ["claude-code", "codex"] as const;
const RUNNABLE_APP_IDS = new Set<string>(RUNNABLE_APPS);

const APP_LABELS: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
};

export function appLabel(appId: string): string {
  return APP_LABELS[appId] ?? appId;
}

function appsFrom(row: Row): MachineApp[] {
  const raw = rowArray(row, "apps");
  if (raw === null) return [];
  return raw
    .map((item): MachineApp | null => {
      if (typeof item !== "object" || item === null || Array.isArray(item)) return null;
      const entry = item as Record<string, unknown>;
      const id = typeof entry["id"] === "string" ? entry["id"] : "";
      if (id === "") return null;
      const allowed = entry["allowed"] === true;
      const signedIn = entry["signedIn"] === true;
      const known = RUNNABLE_APP_IDS.has(id);
      const runnable = known && allowed && signedIn;
      return {
        id,
        label: appLabel(id),
        version: typeof entry["version"] === "string" ? entry["version"] : "",
        signedIn,
        allowed,
        subscription: typeof entry["subscription"] === "string" ? entry["subscription"] : "unknown",
        runnable,
        why: runnable
          ? ""
          : !known
            ? "Not supported by MemQL"
            : !allowed
              ? "Not allowed by Cockpit"
              : "not signed in",
      };
    })
    .filter((app): app is MachineApp => app !== null)
    .sort((a, b) => a.id.localeCompare(b.id));
}

export function machineFromRow(raw: Row): MachineRow {
  const row = flatten(raw);
  const platformInfo = rowObject(row, "platformInfo");
  const descriptor = rowObject(row, "capabilityDescriptor");
  const sharing = sharingFrom(row["sharing"]);
  const reportedLabels = labelMapFrom(row["labels"]);
  const operatorLabels = labelMapFrom(row["operatorLabels"]);
  // platformInfo is the register-time snapshot; the descriptor repeats the
  // platform for machines that send one. platformInfo is preferred because
  // every machine has it.
  const os = nestedString(platformInfo, "os") || nestedString(descriptor, "platform");
  const arch = nestedString(platformInfo, "arch");

  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    name: rowString(row, "name"),
    displayName: rowString(row, "displayName"),
    identityId: rowString(row, "identityId"),
    capabilities: stringList(row, "capabilities"),
    os,
    arch,
    hostname: nestedString(platformInfo, "hostname"),
    platform: arch === "" ? os : `${os}/${arch}`,
    displayServer: nestedString(descriptor, "displayServer"),
    computerUseAvailable: descriptor?.["computerUseAvailable"] === true,
    reportedLabels,
    operatorLabels,
    mergedLabels: mergeLabels(reportedLabels, operatorLabels),
    concurrency: numberMap(row, "concurrency"),
    activeCount: rowNumber(row, "activeCount"),
    lastSeenAt: rowString(row, "lastSeenAt"),
    connectedNodeId: rowString(row, "connectedNodeId"),
    lastSelectedAt: rowString(row, "lastSelectedAt"),
    version: rowString(row, "version"),
    buildTag: rowString(row, "buildTag"),
    registeredAt: rowString(row, "registeredAt"),
    revokedAt: rowString(row, "revokedAt"),
    revokedBy: rowString(row, "revokedBy"),
    revokeReason: rowString(row, "revokeReason"),
    apps: appsFrom(row),
    hardware: hardwareFrom(row["hardware"]),
    sharingMode: sharing.mode,
    sharedUserIds: sharing.userIds,
    sharedGroupIds: sharing.groupIds,
    sharedAt: nestedString(objectAt(row["sharing"]), "sharedAt"),
    sharedBy: nestedString(objectAt(row["sharing"]), "sharedBy"),
    inferenceServe: nestedString(descriptor, "inferenceServe") === "cluster" ? "cluster" : "owner",
    permissions: permissionsFrom(row["permissions"]),
    rttMs: rowNumber(row, "rttMs"),
    rttAt: rowString(row, "rttAt"),
    clockSkewMs: rowNumber(row, "clockSkewMs"),
    // PRESENCE OFF THE RAW FIELD, not off the number: rowNumber answers 0 for
    // an absent key and for a measured zero alike, and here those are opposite
    // readings -- "this cockpit does not stamp its beats" and "its clock
    // agrees with ours to the millisecond".
    clockSkewKnown: row["clockSkewMs"] !== undefined && row["clockSkewMs"] !== null,
    credentialExpiresAt: rowString(row, "credentialExpiresAt"),
  };
}

/** Whether the cluster has measured a round trip to this machine at all. */
export function hasRoundTrip(m: Pick<MachineRow, "rttAt">): boolean {
  return m.rttAt.trim() !== "";
}

/**
 * How a machine's credential stands (epic memql#5327, design D4).
 *
 * FOUR STATES, NOT A BOOLEAN, because the repairs differ and so does the
 * urgency:
 *
 *   never    the token has no expiry. Every token minted before D4 is here,
 *            and it is not a problem to fix -- rotation gives it one.
 *   valid    it expires, and not soon. Nothing to do.
 *   soon     it expires within the warning window. The machine will
 *            disconnect itself on that date unless its cockpit rotates first.
 *   expired  the date has passed. The machine cannot reconnect.
 *
 * `soon` is fourteen days because rotation is the cockpit's job and a cockpit
 * that is not running cannot do it: two weeks is long enough for somebody to
 * notice a laptop that has been shut, and short enough that the warning is
 * still about something happening.
 */
export type CredentialStanding = "never" | "valid" | "soon" | "expired";

/** How many days before expiry the Fleet starts saying so. */
export const CREDENTIAL_WARNING_DAYS = 14;

export function credentialStanding(
  m: Pick<MachineRow, "credentialExpiresAt">,
  now: Date,
): CredentialStanding {
  const raw = m.credentialExpiresAt.trim();
  if (raw === "") return "never";
  const at = new Date(raw);
  // An UNPARSEABLE date is not an expiry. Reading it as `expired` would
  // declare a working machine dead on a string nobody can read; reading it as
  // `never` is the quiet answer, and the row's own value is still on the
  // facts list for anybody investigating.
  if (Number.isNaN(at.getTime())) return "never";
  const msLeft = at.getTime() - now.getTime();
  if (msLeft <= 0) return "expired";
  if (msLeft <= CREDENTIAL_WARNING_DAYS * 24 * 60 * 60 * 1000) return "soon";
  return "valid";
}

/**
 * Whether this machine's clock disagrees with the cluster's by enough to
 * change an answer somebody reads (epic memql#5327, design D6).
 *
 * The threshold is the ONLINE WINDOW, and that is not an arbitrary round
 * number: it is the figure at which a machine's own timestamps would have put
 * it outside the window that decides whether it shows as online. Below it the
 * skew is a curiosity; at or above it, it is the explanation for a machine
 * that read offline while it was connected and beating.
 */
/**
 * A clock offset, split into the figure and the direction.
 *
 * ONE READING OF THE NUMBER, TWO RENDERINGS OF IT. The facts list wants it
 * terse ("3m 32s behind") and the advisory above wants it in a sentence ("3
 * minutes behind the cluster's"), and those are genuinely different jobs --
 * but a second implementation of "how long is 212000 ms" is a second answer
 * waiting to disagree with the first.
 *
 * THE UNIT FOLLOWS THE SIZE, which is the whole reason this exists rather
 * than a template literal at each site: the first rendered pass of the facts
 * list showed `-212000 ms`, a figure nobody reads as three and a half
 * minutes, sitting directly under a round trip written as `34 ms, checked 30s
 * ago`. Milliseconds are right for a clock that is nearly right and useless
 * for one that is not.
 */
export function clockOffsetParts(ms: number): { amount: string; direction: "ahead" | "behind" } {
  const direction = ms > 0 ? "ahead" : "behind";
  const abs = Math.abs(ms);
  if (abs < 1000) return { amount: `${abs} ms`, direction };
  const seconds = Math.round(abs / 1000);
  if (seconds < 120) return { amount: `${seconds}s`, direction };
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return { amount: rest === 0 ? `${minutes}m` : `${minutes}m ${rest}s`, direction };
}

/**
 * The offset as the facts list shows it: the figure, then the direction.
 *
 * A MEASURED ZERO IS "in step", not "0 ms ahead". The clocks agreeing is the
 * answer somebody is looking for, and a signed zero with a direction on it
 * reads as a measurement that came out oddly rather than as agreement.
 */
export function formatClockOffset(ms: number): string {
  if (ms === 0) return "in step with the cluster";
  const { amount, direction } = clockOffsetParts(ms);
  return `${amount} ${direction}`;
}

export function clockSkewMatters(
  m: Pick<MachineRow, "clockSkewMs" | "clockSkewKnown">,
  onlineWindowMs: number,
): boolean {
  if (!m.clockSkewKnown) return false;
  return Math.abs(m.clockSkewMs) >= onlineWindowMs;
}

/** The owner's three answers (epic memql#5344, design G1): `owner` keeps it,
 *  `people` lends it to the users and groups listed, `cluster` to everyone
 *  signed in and to the cluster's own work. */
export type SharingMode = "owner" | "people" | "cluster";

interface Sharing {
  mode: SharingMode;
  userIds: string[];
  groupIds: string[];
}

/**
 * The owner's sharing consent, read the way the engine reads it
 * (ParseMachineSharing in component/memql, SharingFromRow in
 * component/worker).
 *
 * ANYTHING THAT IS NOT `cluster` OR `people` IS `owner` -- a typo, a value
 * from a future engine, a half-written row. The failure direction here is a
 * stranger's prompt running on somebody's machine, so the reading that must
 * not be generous is the permissive one. The mode is trimmed first only
 * because the engine trims it: two readers of one consent that disagreed
 * would show a machine as private that the engine lends out.
 *
 * TWO MORE WAYS TO BE `owner`. A `people` share naming nobody is one -- the
 * engine admits nobody through it, so calling it shared would describe a
 * share that serves no one (design review focus 2). And the lists are read
 * under `people` ONLY: residue left under another mode is not in force, and a
 * surface that named it would be naming people the machine does not serve.
 */
function sharingFrom(v: unknown): Sharing {
  const block = objectAt(v);
  const mode = nestedString(block, "mode").trim();
  if (mode === "cluster") return { mode: "cluster", userIds: [], groupIds: [] };
  if (mode !== "people") return { mode: "owner", userIds: [], groupIds: [] };
  const userIds = subjectIdsFrom(block?.["userIds"]);
  const groupIds = subjectIdsFrom(block?.["groupIds"]);
  if (userIds.length === 0 && groupIds.length === 0) return { mode: "owner", userIds: [], groupIds: [] };
  return { mode: "people", userIds, groupIds };
}

/**
 * A stored list of people or groups: trimmed, empties and non-strings
 * dropped, and one subject's two spellings (bare and canonical) collapsed to
 * the FIRST one stored -- the engine's normalizeShareIds rule, so the id this
 * surface sends back on a save is the one the row already holds.
 */
function subjectIdsFrom(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  const seen = new Set<string>();
  for (const entry of v) {
    if (typeof entry !== "string") continue;
    const id = entry.trim();
    const key = bareShortId(id);
    if (key === "" || seen.has(key)) continue;
    seen.add(key);
    out.push(id);
  }
  return out;
}

function objectAt(v: unknown): Record<string, unknown> | null {
  return v !== null && typeof v === "object" ? (v as Record<string, unknown>) : null;
}

/**
 * The name to render: `displayName` falling back to the reported `name`,
 * falling back to the id. ONE derivation, so the list, the detail panel and
 * the revoke confirmation cannot disagree about what a machine is called.
 */
export function machineName(m: Pick<MachineRow, "displayName" | "name" | "id">): string {
  return m.displayName.trim() || m.name.trim() || m.id;
}

/** A machine is revoked once `revokedAt` is written; the row survives as
 *  audit history, so "revoked" is a state rather than a deletion. */
export function isRevoked(m: Pick<MachineRow, "revokedAt">): boolean {
  return m.revokedAt.trim() !== "";
}

// ---------------------------------------------------------------------------
// The routing policy
// ---------------------------------------------------------------------------

/** The closed strategy set, in the order the editor offers them: the
 *  pre-policy default first, then the three that need a reason. */
export const ROUTING_STRATEGIES = ["firstFit", "roundRobin", "leastLoaded", "labelMatch"] as const;
export type RoutingStrategy = (typeof ROUTING_STRATEGIES)[number];

export const ROUTING_FALLBACKS = ["none", "nextMatching"] as const;
export type RoutingFallback = (typeof ROUTING_FALLBACKS)[number];

/** What the router does with no policy row at all. Named rather than
 *  written twice, because the "no policy" caption and the draft an editor
 *  opens with have to agree or the editor's first save would change
 *  behaviour the caption said was already in force. */
export const DEFAULT_STRATEGY: RoutingStrategy = "firstFit";
export const DEFAULT_FALLBACK: RoutingFallback = "nextMatching";

// What each value MEANS, in an operator's terms rather than the schema's.
// Rendered beside the control, because "leastLoaded" does not say what it is
// least-loaded against.
export const STRATEGY_BLURB: Record<RoutingStrategy, string> = {
  firstFit: "Registration order. What the router did before policies existed.",
  roundRobin:
    "Longest since last chosen first, so two replicas rotate the same way with no shared counter.",
  leastLoaded: "Fewest calls in flight first, against each capability's own cap.",
  labelMatch: "Most preferred labels matched first, then registration order.",
};

export const FALLBACK_BLURB: Record<RoutingFallback, string> = {
  none: "Report the refusal.",
  nextMatching:
    "Try the next candidate. Only ever before a call has started -- never a re-run.",
};

export interface RoutingPolicyRow {
  id: string;
  ownerUserId: string;
  strategy: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  /**
   * An explicit ordered list of model ids, consulted when a policy names
   * `fleet:*` (epic memql#5096). It ORDERS; it does not filter -- a model
   * absent from the list is still eligible, tried after every model the list
   * names. Empty for most people, which is why the default ordering
   * (parameters, then context window, then id, unknown size LAST) has to be
   * good on its own rather than a fallback nobody exercises.
   */
  modelPreference: string[];
  fallback: string;
  active: boolean;
  createdAt: string;
}

export function routingPolicyFromRow(raw: Row): RoutingPolicyRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    strategy: rowString(row, "strategy"),
    requireLabels: labelMapFrom(row["requireLabels"]),
    preferLabels: labelMapFrom(row["preferLabels"]),
    modelPreference: (rowArray(row, "modelPreference") ?? []).filter(
      (v): v is string => typeof v === "string",
    ),
    fallback: rowString(row, "fallback"),
    active: row["active"] === true,
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * The policy the editor edits: the newest ACTIVE row, or nothing.
 *
 * myRoutingPolicies already sorts newest first, so "the first active row" is
 * the same choice routingPolicyForOwner makes server-side. That agreement is
 * the point -- an editor that picked a different row from the one the router
 * reads would write its edits somewhere nothing dispatches through.
 */
export function activePolicy(policies: readonly RoutingPolicyRow[]): RoutingPolicyRow | null {
  return policies.find((policy) => policy.active) ?? null;
}

// ---------------------------------------------------------------------------
// An invocation, and the routing record on it
// ---------------------------------------------------------------------------

export interface RoutingRecord {
  policyId: string;
  strategy: string;
  /** Registration ids the router filtered down to, in the order it would
   *  try them. */
  candidatesConsidered: string[];
  attempts: number;
  selectedBy: string;
  /** "workbench", or "worker:<registrationId>". Empty when nothing was
   *  rerouted. */
  reroutedFrom: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  /**
   * False for a row written before the router existed, and for a path that
   * never picked (a denial before the choice). Rendered as "no routing
   * decision recorded" rather than as an empty routing table, which would
   * read as "chose nothing" -- a different and wrong claim.
   */
  present: boolean;
}

export interface InvocationRow {
  id: string;
  createdAt: string;
  startedAt: string;
  tool: string;
  action: string;
  outcome: string;
  durationMs: number;
  errorCode: string;
  errorMessage: string;
  routing: RoutingRecord;
}

function routingFrom(row: Row): RoutingRecord {
  const raw = rowObject(row, "routing");
  const candidatesRaw = raw?.["candidatesConsidered"];
  const candidates = Array.isArray(candidatesRaw)
    ? candidatesRaw.filter((entry): entry is string => typeof entry === "string")
    : [];
  const attempts = raw?.["attempts"];
  return {
    policyId: nestedString(raw, "policyId"),
    strategy: nestedString(raw, "strategy"),
    candidatesConsidered: candidates,
    attempts: typeof attempts === "number" ? attempts : 0,
    selectedBy: nestedString(raw, "selectedBy"),
    reroutedFrom: nestedString(raw, "reroutedFrom"),
    requireLabels: labelMapFrom(raw?.["requireLabels"]),
    preferLabels: labelMapFrom(raw?.["preferLabels"]),
    present: raw !== null && Object.keys(raw).length > 0,
  };
}

export function invocationFromRow(raw: Row): InvocationRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    tool: rowString(row, "tool"),
    action: rowString(row, "action"),
    outcome: rowString(row, "outcome"),
    durationMs: rowNumber(row, "durationMs"),
    errorCode: rowString(row, "errorCode"),
    errorMessage: rowString(row, "errorMessage"),
    routing: routingFrom(row),
  };
}

/** Outcomes that are not a plain success, so a call reads as what it was.
 *  `rerouted` is deliberately here: the call ran, but not where the router
 *  first sent it, and that is the fact this surface exists to expose. */
export const OUTCOME_TONE: Record<string, "ok" | "warn" | "error"> = {
  success: "ok",
  rerouted: "warn",
  cancelled: "warn",
  timeout: "error",
  failure: "error",
  denied_by_scope: "error",
  denied_by_policy: "error",
  denied_by_classifier: "error",
  kill_switch_engaged: "error",
  no_worker_available: "error",
};

// ---------------------------------------------------------------------------
// A workbench workspace
// ---------------------------------------------------------------------------

export interface WorkspaceRow {
  id: string;
  runId: string;
  ownerUserId: string;
  /** MEMQL_NODE_ID of the workbench replica whose disk holds the directory. */
  nodeId: string;
  status: string;
  storageRoot: string;
  createdAt: string;
  lastUsedAt: string;
  releasedAt: string;
  releasedReason: string;
}

export function workspaceFromRow(raw: Row): WorkspaceRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    runId: rowString(row, "runId"),
    ownerUserId: rowString(row, "ownerUserId"),
    nodeId: rowString(row, "nodeId"),
    status: rowString(row, "status"),
    storageRoot: rowString(row, "storageRoot"),
    createdAt: rowString(row, "createdAt"),
    lastUsedAt: rowString(row, "lastUsedAt"),
    releasedAt: rowString(row, "releasedAt"),
    releasedReason: rowString(row, "releasedReason"),
  };
}

/**
 * What each release reason MEANS. `node_lost` is the one an operator has to
 * be able to read off the screen without going to the source: the files are
 * gone with the replica and were NOT migrated, which is a deliberate design
 * decision (memql#4354) rather than a failure to recover them.
 */
export const RELEASE_REASON_BLURB: Record<string, string> = {
  run_terminal: "The run finished, so its workspace was torn down.",
  explicit: "Released by hand, from a fleet surface or a mutation.",
  ttl_expired: "Aged out by the idle sweep.",
  node_lost:
    "The workbench replica holding this directory left the mesh. The files went with it -- they are not migrated -- and the run was given a fresh workspace elsewhere.",
};

// ---------------------------------------------------------------------------
// A workbench replica
// ---------------------------------------------------------------------------

export interface WorkbenchNodeRow {
  id: string;
  nodeType: string;
  address: string;
  health: string;
  lastSeen: string;
  createdAt: string;
}

export function nodeFromRow(raw: Row): WorkbenchNodeRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    nodeType: rowString(row, "nodeType"),
    address: rowString(row, "address"),
    health: rowString(row, "health"),
    lastSeen: rowString(row, "lastSeen"),
    createdAt: rowString(row, "createdAt"),
  };
}

/** The nodeType value that makes a v1:cluster:node row a workbench replica. */
export const WORKBENCH_NODE_TYPE = "workbench";

/**
 * Collapse the append-only node stream to one row per id.
 *
 * v1:cluster:node is append-only -- every liveness transition writes a new
 * row under the same id -- and `clusterNodes` declares no `asOf latest`, so
 * it returns the WHOLE history. Its own DSL comment says the CLI collapses in
 * Go; this is that same collapse, and without it a single replica renders
 * once per heartbeat it has ever sent.
 *
 * Ties (equal createdAt, or none at all) keep the LAST one seen, which is the
 * later row in the query's own order.
 */
export function latestPerId(nodes: readonly WorkbenchNodeRow[]): WorkbenchNodeRow[] {
  const byId = new Map<string, WorkbenchNodeRow>();
  for (const node of nodes) {
    const held = byId.get(node.id);
    if (held === undefined || node.createdAt >= held.createdAt) byId.set(node.id, node);
  }
  return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id));
}

/**
 * Group workspaces by the replica whose disk holds them, newest first within
 * each group.
 *
 * A workspace with no `nodeId` is its own group under "" rather than being
 * dropped: a row written before memql#4354 stamped the field is still a
 * directory somewhere, and hiding it would answer "where did my files go"
 * with silence.
 */
export function workspacesByNode(
  workspaces: readonly WorkspaceRow[],
): Array<{ nodeId: string; workspaces: WorkspaceRow[] }> {
  const byNode = new Map<string, WorkspaceRow[]>();
  for (const one of workspaces) {
    const held = byNode.get(one.nodeId);
    if (held) held.push(one);
    else byNode.set(one.nodeId, [one]);
  }
  return [...byNode.entries()]
    .map(([nodeId, rows]) => ({
      nodeId,
      workspaces: [...rows].sort((a, b) =>
        a.createdAt === b.createdAt ? (a.id < b.id ? 1 : -1) : a.createdAt < b.createdAt ? 1 : -1,
      ),
    }))
    .sort((a, b) => a.nodeId.localeCompare(b.nodeId));
}

// ---------------------------------------------------------------------------
// The delegation policy, and delegated app sessions (epic memql#5009)
// ---------------------------------------------------------------------------

/**
 * The planner task kinds worth delegating to a local coding app.
 *
 * A CLOSED LIST IN THIS BUILD, and deliberately not "every kind the planner
 * has": the rest are engine work with nothing to gain from a laptop, and
 * offering them would invite a policy that routes a persist step to somebody's
 * machine and then falls back in-process every time.
 */
export const DELEGATABLE_KINDS = [
  "runCommand",
  "fileProcessor",
  "callTool",
  "persistResult",
] as const;

/** How many sessions at once the editor offers. The concept's own default is
 *  1; 4 is the ceiling this surface will set, not a cluster limit. */
export const MAX_CONCURRENT_SESSION_CHOICES = [1, 2, 3, 4] as const;

export interface DelegationPolicyRow {
  id: string;
  ownerUserId: string;
  preferSubscriptionApps: boolean;
  eligibleKinds: string[];
  appOrder: string[];
  maxConcurrentSessions: number;
  workspaceRoot: string;
  credentialLifetimeSeconds: number;
  updatedAt: string;
}

/**
 * The values the PLANNER applies to a person with no policy row.
 *
 * Named here rather than spelled into the editor, for the reason
 * DEFAULT_STRATEGY / DEFAULT_FALLBACK are: the surface states what is in
 * force today, and a caption that could drift from the draft it describes is
 * a caption that will. `preferSubscriptionApps: false` is the master switch
 * OFF -- an absent row means never delegate.
 */
export const DELEGATION_POLICY_DEFAULTS: Omit<DelegationPolicyRow, "id" | "ownerUserId" | "updatedAt"> = {
  preferSubscriptionApps: false,
  eligibleKinds: [],
  appOrder: [],
  maxConcurrentSessions: 1,
  workspaceRoot: "",
  credentialLifetimeSeconds: 14400,
};

export function delegationPolicyFromRow(raw: Row): DelegationPolicyRow {
  const row = flatten(raw);
  // `rowNumber` answers 0 for an absent key, which is exactly what the
  // concept says to read as the default rather than as "none".
  const max = rowNumber(row, "maxConcurrentSessions");
  const lifetime = rowNumber(row, "credentialLifetimeSeconds");
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    preferSubscriptionApps: row["preferSubscriptionApps"] === true,
    eligibleKinds: stringList(row, "eligibleKinds"),
    appOrder: stringList(row, "appOrder"),
    // ZERO READS AS THE DEFAULT, not as "none" -- the concept says so, and it
    // has to: a zero here would silently disable a feature the person turned
    // on, which is the opposite of what a blank field means.
    maxConcurrentSessions: max > 0 ? max : DELEGATION_POLICY_DEFAULTS.maxConcurrentSessions,
    workspaceRoot: rowString(row, "workspaceRoot"),
    credentialLifetimeSeconds:
      lifetime > 0 ? lifetime : DELEGATION_POLICY_DEFAULTS.credentialLifetimeSeconds,
    updatedAt: rowString(row, "updatedAt"),
  };
}

/**
 * What an app REPORTED about its own spend.
 *
 * `known` is the app's own answer to "did you say anything", and it is why
 * the token counts are Figures rather than numbers: an app that reported
 * nothing did not report zero, and rendering `0` next to "tokens" is the one
 * mistake this whole reading exists to avoid (src/kit/measure.ts).
 */
export interface AppSessionUsage {
  known: boolean;
  inputTokens: Figure;
  outputTokens: Figure;
  costUSD: Figure;
}

export interface AppSessionRow {
  id: string;
  ownerUserId: string;
  workerId: string;
  app: string;
  kind: string;
  runId: string;
  stepId: string;
  status: string;
  billing: string;
  /** A Figure, not a number: `rowNumber` answers 0 for an absent key, and
   *  0 is the code a CLEAN run reports. The two must not read alike. */
  exitCode: Figure;
  usage: AppSessionUsage;
  startedAt: string;
  endedAt: string;
}

/** One session in full -- the detail read's extra fields, the recording included. */
export interface AppSessionDetailRow extends AppSessionRow {
  workspace: string;
  prompt: string;
  inputArtifactIds: string[];
  /** The v1:work:run holding this session's recorded actions. Empty when
   *  nothing recorded it -- a node with no recorder, or a session that
   *  predates the recording -- which is not the same as a session that did
   *  nothing. */
  sessionRunId: string;
  /** The Library file holding the session's prose. Empty while the session
   *  runs, and empty afterwards when the Library write failed. */
  transcriptFileId: string;
  /** Actions recorded, and actions LOST. Figures, not numbers: an absent
   *  count is a session nothing recorded, and 0 is a session that recorded
   *  and found nothing to record. */
  recordedSteps: Figure;
  droppedActions: Figure;
  transcriptTruncated: boolean;
  producedArtifactIds: string[];
  appSessionRef: string;
  mcpEndpoint: string;
  credentialExpiresAt: string;
  errorMessage: string;
  cancelReason: string;
}

/** The two non-terminal statuses. A session in either is still being written
 *  to; anything else is a record that will never change again. */
export const LIVE_SESSION_STATUSES = ["starting", "running"] as const;

export function sessionIsLive(status: string): boolean {
  return (LIVE_SESSION_STATUSES as readonly string[]).includes(status);
}

function usageFrom(row: Row): AppSessionUsage {
  const usage = rowObject(row, "usage");
  // AN ABSENT `usage` OBJECT IS "the app said nothing", which is exactly
  // `unmeasured` -- not three zeros. `figureFrom` reads an absent key and a
  // null the same way, so the three fields answer honestly even when the
  // object is present but partial.
  if (usage === null) {
    return {
      known: false,
      inputTokens: absent("unmeasured"),
      outputTokens: absent("unmeasured"),
      costUSD: absent("unmeasured"),
    };
  }
  const known = usage["known"] === true;
  return {
    known,
    inputTokens: known ? figureFrom(usage, "inputTokens") : absent("unmeasured"),
    outputTokens: known ? figureFrom(usage, "outputTokens") : absent("unmeasured"),
    costUSD: known ? figureFrom(usage, "costUSD") : absent("unmeasured"),
  };
}

export function appSessionFromRow(raw: Row): AppSessionRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    workerId: rowString(row, "workerId"),
    app: rowString(row, "app"),
    kind: rowString(row, "kind"),
    runId: rowString(row, "runId"),
    stepId: rowString(row, "stepId"),
    status: rowString(row, "status"),
    // `unknown` is a REAL enum member here (the app reported nothing), so an
    // absent field falls to it rather than to an empty chip.
    billing: rowString(row, "billing") || "unknown",
    exitCode: figureFrom(row, "exitCode"),
    usage: usageFrom(row),
    startedAt: rowString(row, "startedAt"),
    endedAt: rowString(row, "endedAt"),
  };
}

export function appSessionDetailFromRow(raw: Row): AppSessionDetailRow {
  const row = flatten(raw);
  return {
    ...appSessionFromRow(raw),
    workspace: rowString(row, "workspace"),
    prompt: rowString(row, "prompt"),
    inputArtifactIds: stringList(row, "inputArtifactIds"),
    // THE RECORDING, not the prose (epic memql#5396). The transcript used to
    // be a bounded string on this row with every chunk flattened into it and
    // the stream and sequence discarded; what an app DID is now one
    // v1:work:step per action in sessionRunId, and the prose is one Library
    // file. An absent count reads as unmeasured rather than as zero, because
    // "nothing recorded this" and "it recorded and there was nothing" are
    // different answers about a run.
    sessionRunId: rowString(row, "sessionRunId"),
    transcriptFileId: rowString(row, "transcriptFileId"),
    recordedSteps: figureFrom(row, "recordedSteps"),
    droppedActions: figureFrom(row, "droppedActions"),
    transcriptTruncated: row["transcriptTruncated"] === true,
    producedArtifactIds: stringList(row, "producedArtifactIds"),
    appSessionRef: rowString(row, "appSessionRef"),
    mcpEndpoint: rowString(row, "mcpEndpoint"),
    credentialExpiresAt: rowString(row, "credentialExpiresAt"),
    errorMessage: rowString(row, "errorMessage"),
    cancelReason: rowString(row, "cancelReason"),
  };
}

/**
 * The tone a session's status wears: ended = ok, failed = danger, cancelled =
 * warn, starting / running = neutral.
 *
 * A STATUS WORD, NOT A `Chip`. The kit's chip tones are deliberately closed at
 * neutral / accent / muted, and the stylesheet says so: a fourth would make a
 * status colour out of a vocabulary that has none. Deployables' own
 * `.os-deploy-status` carries ok and warn only and belongs to that app, so the
 * Apps surface spells its own word rather than bolting a third tone onto
 * somebody else's class.
 */
export type SessionTone = "ok" | "warn" | "danger" | "neutral";

export function statusTone(status: string): SessionTone {
  if (status === "ended") return "ok";
  if (status === "failed") return "danger";
  if (status === "cancelled") return "warn";
  return "neutral";
}

/**
 * Input plus output, or ABSENT.
 *
 * Absent the moment EITHER half is: a sum over a missing addend is not a
 * smaller total, it is a different question. `figureValue` is the documented
 * way to branch on a figure without letting `?? 0` quietly answer the one
 * distinction that module exists to keep.
 */
export function totalTokens(session: AppSessionRow): Figure {
  const input = figureValue(session.usage.inputTokens);
  const output = figureValue(session.usage.outputTokens);
  if (input === null || output === null) return session.usage.inputTokens;
  return figureOf(input + output);
}

/** Newest run first: what somebody scanning this list is looking for. */
export function sessionsNewestFirst(rows: readonly AppSessionRow[]): AppSessionRow[] {
  return [...rows].sort(
    (a, b) => b.startedAt.localeCompare(a.startedAt) || b.id.localeCompare(a.id),
  );
}
