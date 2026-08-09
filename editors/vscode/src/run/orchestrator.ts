// The run loop: preflight -> bundle -> validate -> session-define -> invoke.
//
// This is the whole of memql#3309's behaviour in one place, and it is
// deliberately free of `vscode` imports so all of it is exercised under bare
// `node --test`. Everything the editor supplies -- the current cluster, the
// live connection, the assembled bundle, the confirmation modal, the Problems
// panel -- arrives through RunDeps as a plain function.
//
// FOUR THINGS ARE LOAD-BEARING, in rough order of how badly getting them wrong
// would fail:
//
//  1. RE-INJECTION AFTER A RECONNECT. A session-defined construct dies with
//     the stream, and its absence is silent: the next call by that name
//     resolves against the DEPLOYED construct and returns a perfectly good
//     result computed from code the developer is not looking at. So the
//     decision to inject is not "have the sources changed" but "have the
//     sources changed OR is the record from a dead stream" -- see
//     SessionRegistry.needsInjection.
//
//  2. VALIDATE BEFORE DEFINE. Gate-1 is a compile against a read-only clone
//     with no engine mutation, so it is the only place a compile error can be
//     shown WITHOUT touching the cluster. Skipping straight to define would
//     work (define validates too) but would lose the distinction between "your
//     buffer does not compile" and "the engine refused the bundle".
//
//  3. ONE SUPERSESSION GUARD OVER ALL THREE ROUND-TRIPS. Validate, define and
//     invoke are three awaits in sequence; a second Run click during any of
//     them makes the first run's remaining steps stale. A single
//     Latest<"run"> token, taken once and checked after every await, is what
//     keeps a superseded run from publishing diagnostics over a newer one's or
//     landing an older result in the panel. See src/async/latest.ts for why
//     there is exactly one such guard type in this extension.
//
//  4. TOOLS ARE NOT THE BUFFER. A `tool` is a declaration bound to a Go-backed
//     handler; it cannot be session-defined, so Run invokes the DEPLOYED
//     definition. The outcome says so explicitly (`ranDeployedDefinition`) and
//     the result view is required to show it. Same button, different
//     semantics.

import { failedDiagnostics } from "@znasllc-io/memql-sdk-core/authoring";
import type {
  SessionDefineBundleResult,
  ValidateBundleResult,
} from "@znasllc-io/memql-sdk-core/authoring";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Latest } from "../async/latest.js";
import {
  isSessionDefinable,
  isWriteKind,
  type RunTarget,
} from "../constructs/runnable.js";
import { buildNamedCall, extractErrorId } from "./call.js";
import type { Bundle } from "./bundle.js";
import { mapBundleDiagnostics, type MappedDiagnostic } from "./diagnostics.js";
import { WriteConfirmationGate } from "./preflight.js";
import { SessionRegistry } from "./session.js";

/** The phase a failure happened in, so the UI can say WHERE it broke rather than only that it did. */
export type RunPhase = "preflight" | "bundle" | "validate" | "define" | "invoke";

/**
 * The narrow engine surface a run needs.
 *
 * Functions rather than the SDK's client objects: the orchestrator is the one
 * module whose behaviour is worth testing exhaustively, and four stub
 * functions are a far cheaper test fixture than a Connection. The adapter
 * builds this from ConnectionManager.
 */
export interface RunEngine {
  validateBundle(sources: string): Promise<ValidateBundleResult>;
  sessionDefineBundle(sources: string): Promise<SessionDefineBundleResult>;
  /** query / mutation / logic, as a rendered named call. */
  executeNamed(name: string, call: string): Promise<{ rows: Row[]; raw: unknown }>;
  /** tool, against the DEPLOYED definition. */
  callTool(name: string, args: Record<string, unknown>): Promise<{ content: ToolContent[]; isError: boolean }>;
}

export interface ToolContent {
  type: string;
  text: string;
  mimeType: string;
  data: string;
  uri: string;
}

/** The cluster facts a run needs. Deliberately not the whole ClusterConfig -- a run has no business seeing the PAT. */
export interface RunCluster {
  name: string;
  label: string;
  /** True only when clusters.yaml marks it `local: true`. Absent means NOT local. */
  local: boolean;
}

export interface RunDeps {
  /** The selected cluster, or undefined when none is selected. */
  cluster(): RunCluster | undefined;
  /** The live engine surface, or undefined when not connected. */
  engine(): RunEngine | undefined;
  /**
   * Assembles the bundle for this target.
   *
   * Awaitable since memql#3335: walking the import graph asks the language
   * server which files a buffer imports, rather than scanning for `use` lines
   * with a regex. A synchronous implementation is still accepted (the tests
   * hand in one) -- `await` on a plain value is a no-op.
   */
  assemble(target: RunTarget): Promise<Bundle> | Bundle;
  /** Shows the write confirmation. Resolves true to proceed. */
  confirmWrite(message: string): Promise<boolean>;
  /** Publishes validation diagnostics to the Problems panel. An empty array CLEARS them. */
  publishDiagnostics(diagnostics: MappedDiagnostic[]): void;
}

export type RunOutcome =
  | {
      status: "ok";
      target: RunTarget;
      rows: Row[];
      raw: unknown;
      toolContent?: ToolContent[];
      /** True when what ran was the DEPLOYED definition rather than the buffer. Tools, always. */
      ranDeployedDefinition: boolean;
      /** True when this run session-defined the bundle (as opposed to reusing a live injection). */
      injected: boolean;
    }
  | { status: "superseded" }
  | { status: "declined"; target: RunTarget }
  | { status: "invalid"; target: RunTarget; diagnostics: MappedDiagnostic[]; phase: RunPhase }
  | { status: "error"; target: RunTarget; phase: RunPhase; message: string; errorId: string };

export class RunOrchestrator {
  // ONE guard for all three round-trips. See point 3 in the module comment.
  private readonly latest = new Latest<"run">();
  private readonly session = new SessionRegistry();
  private readonly writes = new WriteConfirmationGate();

  constructor(private readonly deps: RunDeps) {}

  /** Called by the adapter on every connection-state change. See SessionRegistry.noteStreamReset. */
  noteStreamReset(): void {
    this.session.noteStreamReset();
  }

  /** Called by the adapter when clusters.yaml changes. See WriteConfirmationGate.reset. */
  noteClusterRegistryChanged(): void {
    this.writes.reset();
  }

  /** Exposed for tests and for the run panel's "this bundle is live" hint. */
  get sessionRegistry(): SessionRegistry {
    return this.session;
  }

  /**
   * The write-confirmation gate, exposed so the AUTOMATION run surface
   * (memql#3310) shares this one rather than constructing a second.
   *
   * Sharing is the point, not a convenience. "Prompts once per (cluster,
   * construct)" and "every acknowledgement is dropped when clusters.yaml
   * changes" are properties of ONE gate; two instances would each hold half
   * the session's acknowledgements, and noteClusterRegistryChanged() -- which
   * only this class receives -- would reset one of them.
   */
  get writeGate(): WriteConfirmationGate {
    return this.writes;
  }

  async run(target: RunTarget, values: Record<string, unknown>): Promise<RunOutcome> {
    // begin(), not current(): starting a run IS what makes every earlier one
    // stale. A second Run click supersedes the first outright.
    const token = this.latest.begin();

    const cluster = this.deps.cluster();
    if (cluster === undefined) {
      return fail(target, "preflight", "No cluster selected. Pick one in the Clusters view.");
    }
    const engine = this.deps.engine();
    if (engine === undefined) {
      return fail(
        target,
        "preflight",
        `Not connected to ${cluster.label}. Select the cluster in the Clusters view to connect.`,
      );
    }

    if (this.writes.required(target.kind, cluster.local, cluster.name, target.name)) {
      const confirmed = await this.deps.confirmWrite(
        `Run the mutation "${target.name}" against ${cluster.label}? That cluster is not marked local in clusters.yaml, so this writes real rows.`,
      );
      // The modal is an await like any other, and the user can take as long as
      // they like over it -- which makes it the LONGEST window in the run for
      // the world to move on underneath.
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      if (!confirmed) return { status: "declined", target };
      this.writes.acknowledge(cluster.name, target.name);
    }

    let bundle: Bundle;
    try {
      bundle = await this.deps.assemble(target);
    } catch (err) {
      return fail(target, "bundle", err instanceof Error ? err.message : String(err));
    }
    // Assembly can await now (the import walk asks the language server), so it
    // is another window for a second Run click to supersede this one. Checked
    // here for the same reason every other await in this method is.
    if (!this.latest.isCurrent(token)) return { status: "superseded" };

    // Validation runs for EVERY kind, including tool. It is free of engine
    // mutation, and a buffer whose other constructs do not compile is
    // something the developer wants to know about before reading a result --
    // even when the thing being invoked is the deployed tool.
    let validated: ValidateBundleResult;
    try {
      validated = await engine.validateBundle(bundle.sources);
    } catch (err) {
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      return fail(target, "validate", err instanceof Error ? err.message : String(err));
    }
    if (!this.latest.isCurrent(token)) return { status: "superseded" };

    const failures = failedDiagnostics(validated.diagnostics);
    if (!validated.ok || failures.length > 0) {
      const mapped = mapBundleDiagnostics(validated.diagnostics, bundle);
      this.deps.publishDiagnostics(mapped);
      // A bundle can be !ok with nothing mapped -- the engine refused it
      // without attributing the refusal to a construct. Reporting "invalid"
      // with an empty diagnostic list would leave the developer with a
      // failed run and an empty Problems panel, so that case becomes an
      // ordinary error carrying the reason we do have.
      if (mapped.length === 0) {
        return fail(target, "validate", "The engine rejected the bundle without a per-construct diagnostic.");
      }
      return { status: "invalid", target, diagnostics: mapped, phase: "validate" };
    }
    // Clear stale diagnostics from an earlier failing run. Without this a
    // fixed error stays in the Problems panel until the next failure.
    this.deps.publishDiagnostics([]);

    let injected = false;
    if (isSessionDefinable(target.kind)) {
      if (this.session.needsInjection(cluster.name, bundle.sources)) {
        let defined: SessionDefineBundleResult;
        try {
          defined = await engine.sessionDefineBundle(bundle.sources);
        } catch (err) {
          if (!this.latest.isCurrent(token)) return { status: "superseded" };
          // The registration state is now UNKNOWN -- the request may have been
          // applied before the transport failed. Forgetting is the safe
          // direction: the next run re-injects, which is idempotent, whereas
          // keeping a record we cannot vouch for is how a run silently
          // executes the deployed construct.
          this.session.forget(cluster.name);
          return fail(target, "define", err instanceof Error ? err.message : String(err));
        }
        if (!this.latest.isCurrent(token)) return { status: "superseded" };

        if (!defined.ok) {
          this.session.forget(cluster.name);
          const mapped = mapBundleDiagnostics(defined.diagnostics, bundle);
          if (mapped.length > 0) {
            this.deps.publishDiagnostics(mapped);
            return { status: "invalid", target, diagnostics: mapped, phase: "define" };
          }
          return fail(
            target,
            "define",
            defined.error === "" ? "The engine refused the bundle." : defined.error,
          );
        }
        this.session.recordInjection(cluster.name, bundle.sources);
        injected = true;
      }
    }

    // Invoke.
    if (target.kind === "tool") {
      let result: { content: ToolContent[]; isError: boolean };
      try {
        result = await engine.callTool(target.name, values);
      } catch (err) {
        if (!this.latest.isCurrent(token)) return { status: "superseded" };
        return fail(target, "invoke", err instanceof Error ? err.message : String(err));
      }
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      if (result.isError) {
        return fail(target, "invoke", toolErrorText(result.content));
      }
      return {
        status: "ok",
        target,
        rows: [],
        raw: result.content,
        toolContent: result.content,
        // Always. A tool cannot be session-defined, so this is never the buffer.
        ranDeployedDefinition: true,
        injected: false,
      };
    }

    let call: string;
    try {
      call = buildNamedCall(target.kind, target.name, target.args.map((a) => a.name), values);
    } catch (err) {
      return fail(target, "invoke", err instanceof Error ? err.message : String(err));
    }

    try {
      const result = await engine.executeNamed(target.name, call);
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      return {
        status: "ok",
        target,
        rows: result.rows,
        raw: result.raw,
        ranDeployedDefinition: false,
        injected,
      };
    } catch (err) {
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      return fail(target, "invoke", err instanceof Error ? err.message : String(err));
    }
  }
}

// fail builds the error outcome, pulling out the engine's ERR- id so the UI
// can render it separately and copyably. The id is the only handle a developer
// has on the server-side log entry for their failure, so burying it inside a
// prose message is the difference between a support thread that resolves and
// one that does not.
function fail(target: RunTarget, phase: RunPhase, message: string): RunOutcome {
  return { status: "error", target, phase, message, errorId: extractErrorId(message) };
}

// A tool reports failure IN BAND -- isError with the reason in the content
// blocks -- rather than by rejecting, so the text has to be lifted out or the
// result view shows a successful-looking empty panel for a failed call.
function toolErrorText(content: readonly ToolContent[]): string {
  const text = content
    .map((c) => c.text)
    .filter((t) => t !== "")
    .join("\n");
  return text === "" ? "The tool reported an error with no message." : text;
}

/** Re-exported so the adapter does not have to know which module each piece lives in. */
export { isWriteKind };
