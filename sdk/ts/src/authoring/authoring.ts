// The authoring surface: validate a .memql bundle, then session-define it
// (memql#2128 / C1, consumed by the VS Code run loop in memql#3309).
//
// This is the substrate for RUNNING AN EDITOR BUFFER. Nothing else on the
// stream can do it: ExecuteQueryMsg invokes a construct the engine already
// knows, so without session-define the only way to run a construct you just
// typed is to save it, redeploy, and hope. Session-define registers the
// bundle into the CALLER'S OWN owner-scoped authored registry, which makes
// the buffer callable by name -- and only for that caller, on that stream.
//
// Three properties of that registration are load-bearing for any consumer:
//
//   STREAM-SCOPED.  The definitions die with the WebSocket. After a
//     reconnect they are simply gone, and the engine does not say so: the
//     next call by that name silently resolves to the DEPLOYED construct
//     instead. That failure is invisible at this layer, so a consumer that
//     re-runs across a reconnect must retain the last bundle it injected and
//     re-inject before honouring the re-run. This client deliberately does
//     NOT do that for you -- it has no view of connection lifetime, and
//     hiding the requirement inside a stateless RPC wrapper is how it gets
//     forgotten.
//
//   NEVER SHADOWS CORE.  A session-defined construct cannot take over a name
//     the shared registry already owns, so a bundle cannot be used to
//     redefine engine behaviour for its own session.
//
//   NON-DURABLE.  Nothing is persisted and nothing is visible to another
//     caller. Making a construct durable is a separate, owner-gated pair of
//     messages -- durablePromoteBundle and its inverse durableDemoteBundle --
//     which this module wraps below.
//
// Validation (validateBundle) is the Gate-1 half on its own: a compile + bind
// against a READ-ONLY CLONE of the live registry, with no engine mutation at
// all. Running it before session-define is what lets an editor put compile
// errors in front of the developer without touching the cluster.
//
// THE DURABLE PAIR (memql#3760) is a different order of commitment. A promote
// persists the constructs, registers them into the SHARED registry, and
// broadcasts them so every node serves them within seconds and a restart
// replays them; a demote withdraws them the same way. Three things about that
// pair a consumer has to get right:
//
//   OWNER-ONLY, AND NOT CHECKED HERE.  Both are gated strictly tighter than
//     validate / session-define, which admit owner OR developer. This client
//     does NOT pre-empt the check: it has no trustworthy view of the caller's
//     role, and a client-side guess would either block a legitimate owner or
//     promise a non-owner a call the engine refuses anyway. The engine's
//     refusal names the required role and surfaces here as a thrown error, the
//     same as any other wire-level refusal.
//
//   PROMOTE ANSWERS WITH A DIFF.  A concept re-promoted over a version the
//     cluster already runs is CLASSIFIED: additive changes land, a breaking one
//     (a removed field, a changed field type, a new required field, a narrowed
//     enum) is REFUSED, and the classification comes back either way -- because
//     a refusal IS a diff. Read `conceptDiffs`, not `error`: the error is prose
//     for a human, while the field name, the row count and the constructs that
//     reference it are all values on the diff.
//
//   DEMOTE ANSWERS WITH AN OUTCOME.  A concept with rows under it is RETIRED,
//     not removed: it stays registered, its rows stay readable, new writes to
//     it are refused, and its name stays claimed. Only a concept with zero rows
//     is removed and its name freed. Both are ok=true, so which one happened
//     cannot be recovered from success -- read `outcomes`.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type {
  AuthoringConstructWire,
  AuthoringDiagnosticWire,
  ConceptSchemaChangeWire,
  ConceptSchemaDiffWire,
  DurableDemoteOutcomeWire,
  DurablePromoteBundlePayload,
} from "../client/wire.js";

// -----------------------------------------------------------------------------
// Plain result types (no wire shapes leak to consumers)
// -----------------------------------------------------------------------------

/**
 * One construct's compile-and-bind outcome.
 *
 * The four position fields are 1-BASED and in BUNDLE-FILE coordinates -- they
 * index the `sources` string the caller submitted, not any file on disk. A
 * consumer that concatenated several buffers into one bundle owns the offset
 * table that maps them back.
 *
 * ZERO MEANS "NO POSITION", on every one of the four. The engine emits no
 * position rather than a wrong one (memql#2375), so 0 must never be read as
 * line 0 / column 0 -- doing that parks every positionless diagnostic on the
 * first line of the bundle, which is virtually always the wrong file.
 * `endLine` / `endColumn` are 0 independently of `line` / `column`: a
 * diagnostic can know where it starts and have no end anchor.
 */
export interface AuthoringDiagnostic {
  name: string;
  kind: string;
  /** True when this construct compiled and bound cleanly. */
  ok: boolean;
  /**
   * True when this construct's KIND is one the pass does not compile. A
   * skipped construct reports ok=false and does NOT fail the bundle, so the
   * failure test is `!ok && !skipped` -- reading `!ok` alone turns every
   * concept or shape carried along in a bundle into a spurious error.
   */
  skipped: boolean;
  /** Compile/bind failure or skip reason; empty on success. */
  error: string;
  line: number;
  column: number;
  endLine: number;
  endColumn: number;
}

/** One construct a session-define registered, by kind and name. */
export interface AuthoringConstruct {
  kind: string;
  name: string;
}

/** Gate-1 validation outcome. */
export interface ValidateBundleResult {
  /** True only when every non-skipped construct compiled cleanly. */
  ok: boolean;
  diagnostics: AuthoringDiagnostic[];
}

/** Session-define outcome. */
export interface SessionDefineBundleResult {
  ok: boolean;
  /** The constructs that became callable by name on this stream. */
  defined: AuthoringConstruct[];
  diagnostics: AuthoringDiagnostic[];
  /**
   * Non-empty when the bundle was REJECTED outright -- validation failed, or
   * registration did. Distinct from a per-construct diagnostic: this is the
   * bundle-level refusal, and when it is set nothing was defined.
   */
  error: string;
}

/**
 * One classified difference between the version of a concept the cluster is
 * running and the version a promote is putting over it (memql#3757).
 *
 * `breaking` is the classification the ENGINE made, not one to re-derive here.
 * A consumer that recomputed it would be a second authority on the same
 * question, and the two would drift the first time the engine's rules moved.
 */
export interface ConceptSchemaChange {
  concept: string;
  /**
   * The payload field, dotted for one inside a nested block
   * ("preferences.theme"). EMPTY for a concept-level change -- an edited
   * description, a changed node type, an added or removed relationship -- so
   * an empty string here is "this change is not about a field", never "the
   * field name was lost".
   */
  field: string;
  /**
   * A closed set: field_added, field_removed, field_type_changed,
   * field_required_added, field_required_dropped, enum_widened, enum_narrowed,
   * description_changed, relationship_added, relationship_removed,
   * node_type_changed.
   */
  kind: string;
  breaking: boolean;
  /** The two sides in the concept's own vocabulary ("string" -> "number"). */
  was: string;
  now: string;
  /**
   * A REAL count taken against the live table, never an estimate -- and
   * MEANINGFUL ONLY WHEN `rowCountKnown` IS TRUE.
   *
   * A node with no database cannot count rows, and its zero would be a claim it
   * is not entitled to make. So the two travel as separate values rather than
   * as one sentinel, and a consumer that renders `rowsAffected` without first
   * checking `rowCountKnown` will tell an operator "0 rows carry it" about a
   * count nobody took -- which reads as "this is safe" at exactly the moment it
   * is unknown.
   */
  rowsAffected: number;
  rowCountKnown: boolean;
  /**
   * The loaded constructs the change reaches, as "kind:name"
   * ("query:spaceParticipants"), read off the live registries.
   */
  referencedBy: string[];
  /** The engine's one-line reason, already carrying the counts. */
  detail: string;
}

/**
 * The whole classification of one concept re-promote.
 *
 * `breaking` is true when ANY change in `changes` is breaking. `overridden`
 * records that a breaking change LANDED because the call passed
 * `allowBreaking` -- so `breaking && overridden` is a promote that went
 * through, and `breaking && !overridden` is the refusal.
 *
 * `summary` is the engine's own rendered block, the same text it puts in the
 * refusal error. It is carried so a client can show the engine's wording
 * without rebuilding it out of `changes`.
 */
export interface ConceptSchemaDiff {
  /** Canonical concept id. */
  concept: string;
  breaking: boolean;
  overridden: boolean;
  changes: ConceptSchemaChange[];
  summary: string;
}

/** Durable-promote outcome. */
export interface DurablePromoteBundleResult {
  ok: boolean;
  /** The constructs durably promoted into the shared registry. */
  promoted: AuthoringConstruct[];
  diagnostics: AuthoringDiagnostic[];
  /**
   * Non-empty when the bundle was REJECTED -- validation failed, a per-construct
   * promote failed, or a breaking concept change was refused. When it is set
   * nothing was promoted.
   */
  error: string;
  /**
   * The classification for every CONCEPT the bundle re-promotes over a version
   * the cluster already runs -- present on the REFUSAL as well as on success,
   * because a refusal IS a diff. Empty for a first promote and for an unchanged
   * re-promote, so empty means "no concept changed", never "the diff was lost".
   */
  conceptDiffs: ConceptSchemaDiff[];
}

// DemoteOutcomeRetired / DemoteOutcomeRemoved are the two values
// `DemoteOutcome.outcome` takes. Compare against these rather than against bare
// string literals: they are this SDK's own copy of the engine vocabulary, which
// is what keeps a consumer off the wire types and gives a rename one place to
// happen.

/**
 * The construct is still REGISTERED and its name is still claimed. Only a
 * concept with rows under it reaches this -- its rows stay readable and new
 * writes to it are refused until it is re-promoted.
 */
export const DemoteOutcomeRetired = "retired";

/**
 * The construct is gone from the shared registry and its name is claimable
 * again.
 */
export const DemoteOutcomeRemoved = "removed";

/**
 * What actually happened to ONE construct in a demote (memql#3756).
 *
 * A concept is why this is not implied by success. Rows written under a concept
 * outlive its definition, so demoting one with rows under it RETIRES it --
 * registered, readable, closed to new writes -- and only demoting one with zero
 * rows REMOVES it and frees the name. Both are ok=true, so the outcome is
 * reported rather than inferred: whether the name is claimable again is the
 * next thing the caller needs to know, and no reading of "it worked"
 * establishes it.
 */
export interface DemoteOutcome {
  /**
   * The construct as the SOURCE named it -- for a concept, `name` is the
   * declaration name, not the canonical id.
   */
  kind: string;
  name: string;
  /** The canonical concept id; EMPTY for every non-concept kind. */
  conceptId: string;
  /** `DemoteOutcomeRetired` or `DemoteOutcomeRemoved`. */
  outcome: string;
  /**
   * The number of DISTINCT rows the engine found under the concept, across
   * every owner -- the evidence that chose the outcome. Always 0 for a
   * non-concept kind, which has no rows of its own.
   */
  rowCount: number;
}

/** Durable-demote outcome, the inverse of `DurablePromoteBundleResult`. */
export interface DurableDemoteBundleResult {
  ok: boolean;
  /** The constructs durably demoted out of the shared registry. */
  demoted: AuthoringConstruct[];
  /**
   * The same constructs as `demoted`, in the same order, each with what
   * happened to it. Read THIS -- not the presence of a name in `demoted` -- to
   * learn whether a demoted concept's name is free again.
   */
  outcomes: DemoteOutcome[];
  diagnostics: AuthoringDiagnostic[];
  /**
   * Non-empty when the demote was REJECTED -- no demotable constructs in the
   * source, or a per-construct demote failed.
   */
  error: string;
}

export interface AuthoringCallOptions {
  signal?: AbortSignal;
}

/**
 * Options for `durablePromoteBundle`.
 *
 * The override rides an options object rather than a positional boolean
 * (`durablePromoteBundle(src, true)` says nothing at the call site about what
 * is being allowed) and rather than a second method (a
 * `durablePromoteBundleAllowingBreaking` would give the dangerous call the
 * shorter, more obvious name). This is the TypeScript reading of the same
 * choice sdk/go makes with its `authoring.AllowBreaking()` functional option,
 * and it keeps the `(sources, opts)` shape the other two methods already have.
 */
export interface DurablePromoteBundleOptions extends AuthoringCallOptions {
  /**
   * Land a CONCEPT re-promote whose schema change would strand rows already
   * written -- a removed field, a changed field type, a new required field, a
   * narrowed enum (memql#3757).
   *
   * Without it such a promote is REFUSED, and the refusal names the field, the
   * rows it affects and the constructs that reference it. With it the change
   * lands and the engine AUDITS the override on v1:identity:auditEvent, naming
   * the concept and the fields -- so an override is a recorded decision rather
   * than a quieter call.
   *
   * It moves nothing else. Every other refusal a promote can produce
   * (core-first, a failed compile, a broken invariant) is unmoved by it,
   * because none of them is a judgement about data.
   */
  allowBreaking?: boolean;
}

// -----------------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------------

export class AuthoringClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("AuthoringClient: dispatcher is required");
  }

  /**
   * Gate-1 compile + bind of `sources` against a read-only clone of the live
   * registry. NO ENGINE MUTATION -- safe to run on every keystroke if a
   * consumer wants to, and safe to run against production.
   *
   * A bundle that fails validation resolves normally with `ok: false` and the
   * per-construct diagnostics; it does not throw. Only a transport-level or
   * envelope-level failure throws, because those mean the answer is unknown
   * rather than negative.
   */
  async validateBundle(
    sources: string,
    opts: AuthoringCallOptions = {},
  ): Promise<ValidateBundleResult> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { authoringValidateBundle: { requestId, sources } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(
        `validateBundle: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "authoringValidateBundleResult") {
      throw new Error("validateBundle: unexpected reply envelope");
    }
    return {
      ok: payload.value.ok === true,
      diagnostics: (payload.value.diagnostics ?? []).map(diagnosticFromWire),
    };
  }

  /**
   * Validate `sources`, then register it into the caller's owner-scoped
   * authored registry for the LIFETIME OF THIS STREAM.
   *
   * See the module comment: the registration is stream-scoped and its loss on
   * reconnect is silent, so the caller owns re-injection.
   */
  async sessionDefineBundle(
    sources: string,
    opts: AuthoringCallOptions = {},
  ): Promise<SessionDefineBundleResult> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { authoringSessionDefineBundle: { requestId, sources } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(
        `sessionDefineBundle: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "authoringSessionDefineBundleResult") {
      throw new Error("sessionDefineBundle: unexpected reply envelope");
    }
    return {
      ok: payload.value.ok === true,
      defined: (payload.value.defined ?? []).map(constructFromWire),
      diagnostics: (payload.value.diagnostics ?? []).map(diagnosticFromWire),
      error: payload.value.error ?? "",
    };
  }

  /**
   * Validate `sources`, then DURABLY promote every construct in it into the
   * SHARED engine registry: persisted so a restart replays it, registered so
   * every session on this node can call it, and broadcast so every other node
   * serves it within seconds.
   *
   * OWNER-ONLY, and deliberately unchecked here -- see the module comment. A
   * non-owner caller gets the engine's own refusal naming the required role,
   * thrown like any other wire-level refusal.
   *
   * A bundle the engine REJECTS -- a failed validation, a failed promote, or a
   * breaking concept change refused -- resolves normally with `ok: false`, the
   * `error`, and (for a concept) the `conceptDiffs`; it does not throw. Only a
   * transport-level or envelope-level failure throws, because those mean the
   * answer is unknown rather than negative. A refusal is very much an answer,
   * and it is the one carrying the diff.
   *
   * Pass `{ allowBreaking: true }` to land a breaking concept change
   * deliberately; the engine audits the override.
   */
  async durablePromoteBundle(
    sources: string,
    opts: DurablePromoteBundleOptions = {},
  ): Promise<DurablePromoteBundleResult> {
    const requestId = newShortId();
    const body: DurablePromoteBundlePayload = { requestId, sources };
    // Set only when asked for: proto3 reads absent and false identically, so
    // omitting it keeps the flag off the frame of every ordinary promote.
    if (opts.allowBreaking) body.allowBreaking = true;
    const reply = await this.dispatcher.sendAndWait(
      { durablePromoteBundle: body },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(
        `durablePromoteBundle: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "durablePromoteBundleResult") {
      throw new Error("durablePromoteBundle: unexpected reply envelope");
    }
    return {
      ok: payload.value.ok === true,
      promoted: (payload.value.promoted ?? []).map(constructFromWire),
      diagnostics: (payload.value.diagnostics ?? []).map(diagnosticFromWire),
      error: payload.value.error ?? "",
      conceptDiffs: (payload.value.conceptDiffs ?? []).map(conceptDiffFromWire),
    };
  }

  /**
   * DURABLY demote every construct the bundle names OUT of the shared engine
   * registry -- the inverse of `durablePromoteBundle`. The withdrawal is
   * persisted, applied here, and broadcast so every node applies it within
   * seconds. A core construct can never be demoted; the engine's gate is
   * author-promoted-only.
   *
   * A demote needs only the kinds and names the source declares -- it never
   * compiles the construct -- so a construct whose source no longer compiles
   * can still be demoted. That is why there is no `allowBreaking` counterpart
   * here and why `diagnostics` is normally empty.
   *
   * OWNER-ONLY on the same terms as the promote. CHECK `outcomes`, not just
   * `ok`: a concept with rows under it is RETIRED rather than removed, and both
   * come back `ok: true`.
   */
  async durableDemoteBundle(
    sources: string,
    opts: AuthoringCallOptions = {},
  ): Promise<DurableDemoteBundleResult> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { durableDemoteBundle: { requestId, sources } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(
        `durableDemoteBundle: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "durableDemoteBundleResult") {
      throw new Error("durableDemoteBundle: unexpected reply envelope");
    }
    return {
      ok: payload.value.ok === true,
      demoted: (payload.value.demoted ?? []).map(constructFromWire),
      outcomes: (payload.value.outcomes ?? []).map(demoteOutcomeFromWire),
      diagnostics: (payload.value.diagnostics ?? []).map(diagnosticFromWire),
      error: payload.value.error ?? "",
    };
  }
}

// -----------------------------------------------------------------------------
// Wire -> plain
// -----------------------------------------------------------------------------

// Every numeric field is coerced through numberOrZero rather than `?? 0`:
// protojson omits a zero int32 entirely, so `line` arrives as undefined for
// the positionless case, and a runtime that stringifies int32 would hand back
// "12". Both must land as a number, and both must land as 0 when absent --
// that zero IS the "no position" signal the consumer keys on.
function diagnosticFromWire(d: AuthoringDiagnosticWire): AuthoringDiagnostic {
  return {
    name: d.name ?? "",
    kind: d.kind ?? "",
    ok: d.ok === true,
    skipped: d.skipped === true,
    error: d.error ?? "",
    line: numberOrZero(d.line),
    column: numberOrZero(d.column),
    endLine: numberOrZero(d.endLine),
    endColumn: numberOrZero(d.endColumn),
  };
}

function constructFromWire(c: AuthoringConstructWire): AuthoringConstruct {
  return { kind: c.kind ?? "", name: c.name ?? "" };
}

// The concept classification is copied field for field and DERIVES NOTHING.
// The engine already decided what is breaking and already counted the rows; an
// SDK that recomputed either would be a second authority on the same question,
// and the two would disagree the first time the engine's rules moved.
//
// Both int64 fields below (rowsAffected here, rowCount in the demote outcome)
// go through the same numberOrZero as the int32 positions, for a different
// reason: protojson renders int64 as a STRING on most runtimes, so the value
// arrives as "1204" and would compare and sort as text.
function conceptDiffFromWire(d: ConceptSchemaDiffWire): ConceptSchemaDiff {
  return {
    concept: d.concept ?? "",
    breaking: d.breaking === true,
    overridden: d.overridden === true,
    changes: (d.changes ?? []).map(conceptChangeFromWire),
    summary: d.summary ?? "",
  };
}

function conceptChangeFromWire(c: ConceptSchemaChangeWire): ConceptSchemaChange {
  return {
    concept: c.concept ?? "",
    field: c.field ?? "",
    kind: c.kind ?? "",
    breaking: c.breaking === true,
    was: c.was ?? "",
    now: c.now ?? "",
    rowsAffected: numberOrZero(c.rowsAffected),
    // Strictly `=== true`, so an absent field lands as false. That is the
    // honest reading: protojson omits a false bool, and "the engine did not say
    // it counted" is exactly "the count is not known".
    rowCountKnown: c.rowCountKnown === true,
    referencedBy: c.referencedBy ?? [],
    detail: c.detail ?? "",
  };
}

function demoteOutcomeFromWire(o: DurableDemoteOutcomeWire): DemoteOutcome {
  return {
    kind: o.kind ?? "",
    name: o.name ?? "",
    conceptId: o.conceptId ?? "",
    // Left verbatim rather than validated against the two constants: an
    // outcome this SDK build has never heard of is a fact about the cluster,
    // and rewriting it to "" or to one of the known values would hide that.
    outcome: o.outcome ?? "",
    rowCount: numberOrZero(o.rowCount),
  };
}

function numberOrZero(v: unknown): number {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string") {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

/**
 * failedDiagnostics filters a diagnostic set to the entries that actually
 * FAILED -- `!ok && !skipped`.
 *
 * Exported because getting this test wrong is the single most likely bug in a
 * consumer: a bundle routinely carries constructs whose kind this pass does
 * not compile (a concept, a shape), and each of those reports ok=false with
 * skipped=true. Filtering on `!ok` alone renders them as compile errors on a
 * bundle the engine considers perfectly valid.
 */
export function failedDiagnostics(
  diagnostics: readonly AuthoringDiagnostic[],
): AuthoringDiagnostic[] {
  return diagnostics.filter((d) => !d.ok && !d.skipped);
}
