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
//     caller. Durable promotion is a separate, owner-gated message
//     (DurablePromoteBundleMsg) that this module does not wrap.
//
// Validation (validateBundle) is the Gate-1 half on its own: a compile + bind
// against a READ-ONLY CLONE of the live registry, with no engine mutation at
// all. Running it before session-define is what lets an editor put compile
// errors in front of the developer without touching the cluster.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type {
  AuthoringConstructWire,
  AuthoringDiagnosticWire,
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

export interface AuthoringCallOptions {
  signal?: AbortSignal;
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
