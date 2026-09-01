// The `integrationStatus` reply, in full (issue #4826 / program decision P6).
//
// `mailStatus.ts` reads ONE integration's headline for the Cluster panel and
// deliberately drops the slot arrays. This section is the surface those
// arrays exist for, so it reads the whole report -- and the envelope walk is
// shared rather than copied, because a second copy of a walk is a walk that
// drifts (mailStatus.ts now projects this).
//
// WHAT IS NEVER READ HERE: a credential VALUE. Not because this module
// filters one out -- because the reply carries none. `integrations/email/
// status.go` reports a secret slot's PRESENCE, its source and the operator
// command that rotates it, and `TestStatusNeverLeaksACredential` plants
// recognisable values in every slot and sweeps the serialized reply for them.
// The client half of that promise is `IntegrationSlot` having no field a
// value could land in for a secret slot, and the section offering no control
// that would display one.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

/**
 * The three states the section renders, plus the honest fourth.
 *
 * `not_reported` is what the engine says about every integration that
 * publishes no self-report of its own, which today is all of them except
 * email. It is an answer rather than a gap, and it is why those integrations
 * get a roll-call line instead of a card: a card is a promise that there is
 * something to configure here, and inventing eleven of them would be this
 * window making up a fact about the node.
 */
export type IntegrationState =
  | "needs_configuration"
  | "configured"
  | "unhealthy"
  | "not_reported";

/**
 * One configuration key.
 *
 * `secret` splits the two arrays the engine sends into one list without
 * losing which is which: a secret slot carries no `value` and never will, and
 * `rotate` is the operator command that changes it -- rendered because there
 * is no path from a browser to a secret write and the honest thing to show is
 * the one that works.
 *
 * `editable` is the ENGINE's answer, not a re-derivation. The boot-envelope
 * boundary is declared in `component/envregistry/manifest.yaml` and resolved
 * server-side; a value the resolver reads out of the environment cannot be
 * overridden from the graph, because the resolver reads env first and stops.
 * Re-deriving that boundary in TypeScript would produce a second opinion, and
 * the failure it produces is an editable-looking field that silently does
 * nothing.
 */
export interface IntegrationSlot {
  /** The engine's own key name: `senderAddress`, `smtpHost`. */
  name: string;
  /** What it is for, in the engine's words. */
  purpose: string;
  /** env | globalVariable | globalSecret | unset. */
  source: string;
  /** The environment variable that supplies it. Secondary text, never the label. */
  envVar: string;
  /** True for a credential slot: no value, ever. */
  secret: boolean;
  /** Whether a value resolved at all. For a secret this is the whole answer. */
  present: boolean;
  /** The resolved value. Always "" for a secret slot. */
  value: string;
  /** Whether this may be written from here at all -- see the type doc. */
  editable: boolean;
  /** The operator command that rotates a secret. "" for a setting. */
  rotate: string;
  /** Which way-of-being-configured this slot belongs to: `graph`, `smtp`.
   *  Empty where an integration declares no lanes. */
  lane: string;
  /** Whether the lane can work without it. An OPTIONAL slot left unset is a
   *  normal state and never a reason. */
  required: boolean;
  /** The engine's sentence for what is wrong with THIS slot, written for the
   *  position under the field. Empty when nothing is. */
  reason: string;
}

/**
 * One machine-readable explanation of why a state is not `configured`.
 *
 * `code` is what a surface branches on and `detail` is what it renders --
 * never the other way round. A reason NEVER carries a resolved value, which is
 * the same invariant the slot list is under; `TestReasonsNeverCarryAValue`
 * holds the server half.
 *
 * `slot` / `envVar` / `lane` are what let a surface point at the field
 * responsible. They are empty on a reason that belongs to no single slot -- a
 * split lane, a failed probe, a refused mode -- and that emptiness is the
 * distinction, not a gap.
 */
export interface IntegrationReason {
  /** missing_slot | split_lane | probe_failed | refused. Closed. */
  code: string;
  lane: string;
  slot: string;
  envVar: string;
  detail: string;
}

export interface IntegrationCard {
  name: string;
  registered: boolean;
  capabilities: readonly string[];
  state: IntegrationState;
  /** yes | no | unknown, verbatim. */
  configured: string;
  /** healthy | unhealthy | degraded | unknown, verbatim. */
  health: string;
  /** The engine's own sentences. The most useful text on the screen. */
  detail: string;
  /** Why `state` is not `configured`, one entry per thing a person would have
   *  to do. Empty when there is nothing to say. */
  reasons: readonly IntegrationReason[];
  /** The resolved lane: graph | smtp | log, or "" where the integration has none. */
  mode: string;
  probed: boolean;
  slots: readonly IntegrationSlot[];
}

export interface IntegrationsReport {
  /** RFC3339, stamped SERVER-side by the handler. Not a client clock. */
  checkedAt: string;
  /** True when this reply came from a run that made the live check. */
  probed: boolean;
  integrations: readonly IntegrationCard[];
}

const MAX_ENVELOPE_DEPTH = 4;

/**
 * Dig the report out of the builtin envelope.
 *
 * A top-level `builtin X(...)` does not come back as a row set: the engine
 * marshals the handler's node map into one value keyed by node id, and that
 * id is bare-ified on the way out. So this SEARCHES for the object carrying
 * an `integrations` array rather than walking a fixed path a rename would
 * silently turn into `undefined`.
 */
export function readIntegrationsReport(rows: readonly Row[]): IntegrationsReport | null {
  for (const row of rows) {
    const found = find(row, 0);
    if (found) return found;
  }
  return null;
}

function find(value: unknown, depth: number): IntegrationsReport | null {
  if (depth > MAX_ENVELOPE_DEPTH || value === null || typeof value !== "object") return null;
  if (Array.isArray(value)) {
    for (const entry of value) {
      const found = find(entry, depth + 1);
      if (found) return found;
    }
    return null;
  }

  const bag = value as Record<string, unknown>;
  const integrations = bag["integrations"];
  if (Array.isArray(integrations)) {
    return {
      checkedAt: str(bag, "checkedAt"),
      probed: bool(bag, "probed"),
      integrations: integrations
        .filter((e): e is Record<string, unknown> => !!e && typeof e === "object")
        .map(cardOf),
    };
  }

  for (const nested of Object.values(bag)) {
    const found = find(nested, depth + 1);
    if (found) return found;
  }
  return null;
}

function cardOf(entry: Record<string, unknown>): IntegrationCard {
  return {
    name: str(entry, "name"),
    registered: bool(entry, "registered"),
    capabilities: strings(entry, "capabilities"),
    state: stateOf(entry),
    configured: str(entry, "configured"),
    health: str(entry, "health"),
    detail: str(entry, "detail"),
    reasons: objects(entry, "reasons").map((raw) => ({
      code: str(raw, "code"),
      lane: str(raw, "lane"),
      slot: str(raw, "slot"),
      envVar: str(raw, "envVar"),
      detail: str(raw, "detail"),
    })),
    mode: str(entry, "mode"),
    probed: bool(entry, "probed"),
    slots: slotsOf(entry),
  };
}

const DECLARED_STATES: readonly string[] = [
  "needs_configuration",
  "configured",
  "unhealthy",
  "not_reported",
];

/**
 * The state, FROM THE ENGINE.
 *
 * A `state` field is preferred and is what every integration will carry once
 * the status capability is generalized. Until then the engine answers in two
 * fields, and translating that PAIR through one table here is a rename rather
 * than an inference: nothing is decided that the engine did not already say.
 * What would be an inference -- calling something configured because its slots
 * look filled -- this function never does.
 *
 * CONFIGURED IS READ FIRST, AND THE ORDER IS THE SEMANTICS. `unhealthy` has
 * to mean configured and failing, because that is the state the section
 * spends its error voice on. An install that must deliver mail with nothing
 * configured reports health=unhealthy, and reading health first would put the
 * error voice on a cluster that has simply not been set up yet. It needs
 * configuration; the engine's own sentence says the rest, including that
 * sends are refused meanwhile.
 *
 * A configured integration whose health is `degraded` reads as configured.
 * That pair is not produced today, and of the readings available it is the
 * one that asserts least -- the detail carries the qualification.
 */
export function stateOf(entry: Record<string, unknown>): IntegrationState {
  const declared = str(entry, "state");
  if (DECLARED_STATES.includes(declared)) return declared as IntegrationState;

  const configured = str(entry, "configured");
  if (configured === "no") return "needs_configuration";
  if (configured !== "yes") return "not_reported";
  return str(entry, "health") === "unhealthy" ? "unhealthy" : "configured";
}

/** The two arrays, folded into one list in the engine's own order. */
function slotsOf(entry: Record<string, unknown>): IntegrationSlot[] {
  const out: IntegrationSlot[] = [];
  for (const raw of objects(entry, "settings")) {
    const source = str(raw, "source");
    out.push({
      name: str(raw, "name"),
      purpose: str(raw, "purpose"),
      source,
      envVar: str(raw, "envVar"),
      secret: false,
      present: source !== "" && source !== "unset",
      value: str(raw, "value"),
      editable: bool(raw, "editable"),
      rotate: "",
      lane: str(raw, "lane"),
      required: bool(raw, "required"),
      reason: str(raw, "reason"),
    });
  }
  for (const raw of objects(entry, "credentials")) {
    out.push({
      name: str(raw, "name"),
      purpose: str(raw, "purpose"),
      source: str(raw, "source"),
      envVar: str(raw, "envVar"),
      secret: true,
      present: bool(raw, "present"),
      // Never populated, and there is no key to populate it from: the reply
      // carries no value for a credential slot. Held as "" so one row type
      // serves both kinds without an optional a renderer could forget.
      value: "",
      // A credential is not editable from here whatever the reply says: the
      // only write path is the operator command below.
      editable: false,
      rotate: str(raw, "rotate"),
      lane: str(raw, "lane"),
      required: bool(raw, "required"),
      reason: str(raw, "reason"),
    });
  }
  return out;
}

/**
 * The reasons worth rendering beside the card's own summary.
 *
 * The engine emits a probe verdict and a refusal BOTH as a reason and, by
 * concatenation, inside `detail` -- so a surface that renders the summary and
 * the list puts the same sentence on the screen twice, a few centimetres
 * apart, and a reader assumes something is broken. Dropping the summary
 * instead is not the fix: it carries the log-only sentence (memql#4477) that
 * no reason states, and that is the most important sentence this feature has.
 *
 * The test is CONTAINMENT rather than a list of codes, deliberately. It is
 * self-correcting: the day the engine stops concatenating, the reason appears
 * on its own with no edit here -- where a code list would keep hiding it. And
 * the failure direction is safe: a miss shows a sentence twice, never zero
 * times.
 */
export function visibleReasons(card: IntegrationCard): readonly IntegrationReason[] {
  const summary = card.detail.trim();
  return card.reasons.filter((reason) => {
    const detail = reason.detail.trim();
    return detail !== "" && !summary.includes(detail);
  });
}

export interface IntegrationLane {
  /** The engine's lane name, or "" for slots belonging to no lane. */
  name: string;
  slots: readonly IntegrationSlot[];
}

/**
 * The slots, grouped by lane, in the order the lanes first appear.
 *
 * GROUPED BECAUSE THE ENGINE SAYS SO, not because it looks tidier. A lane is
 * an alternative way of being configured and the resolver takes one WHOLE or
 * not at all, so eleven flat fields invite somebody to fill half of each --
 * the one arrangement that resolves to nothing. `lane` is carried on every
 * slot for exactly this (memql#4825); grouping by a prefix in the slot NAME,
 * which is what a client would have had to do before, would have been this
 * window inventing structure the node did not send.
 *
 * Order is FIRST APPEARANCE rather than sorted: the manifest lists lanes in
 * preference order (the first that resolves whole wins), so the reply's own
 * order is the one an operator should read them in.
 */
export function lanesOf(card: IntegrationCard): readonly IntegrationLane[] {
  const order: string[] = [];
  const byLane = new Map<string, IntegrationSlot[]>();
  for (const slot of card.slots) {
    if (!byLane.has(slot.lane)) {
      byLane.set(slot.lane, []);
      order.push(slot.lane);
    }
    byLane.get(slot.lane)!.push(slot);
  }
  return order.map((name) => ({ name, slots: byLane.get(name)! }));
}

/** The integrations with something to configure. */
export function configurableCards(
  report: IntegrationsReport | null,
): readonly IntegrationCard[] {
  if (report === null) return [];
  return report.integrations.filter(
    (card) => card.state !== "not_reported" || card.slots.length > 0,
  );
}

/** Everything else the node registered: a roll-call, not a card. */
export function silentCards(report: IntegrationsReport | null): readonly IntegrationCard[] {
  if (report === null) return [];
  return report.integrations.filter(
    (card) => card.state === "not_reported" && card.slots.length === 0,
  );
}

function str(bag: Record<string, unknown>, key: string): string {
  const value = bag[key];
  return typeof value === "string" ? value : "";
}

function bool(bag: Record<string, unknown>, key: string): boolean {
  const value = bag[key];
  if (typeof value === "boolean") return value;
  return value === "true";
}

function strings(bag: Record<string, unknown>, key: string): string[] {
  const value = bag[key];
  if (!Array.isArray(value)) return [];
  return value.filter((v): v is string => typeof v === "string");
}

function objects(bag: Record<string, unknown>, key: string): Record<string, unknown>[] {
  const value = bag[key];
  if (!Array.isArray(value)) return [];
  return value.filter((v): v is Record<string, unknown> => !!v && typeof v === "object");
}
