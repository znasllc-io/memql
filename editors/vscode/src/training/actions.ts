// The four training actions: dry-run, try in session, promote, demote.
//
// One class, because the four are variations on one sequence -- preflight,
// assemble, ask, submit, report -- and because three properties have to hold
// across all of them at once and would otherwise be four separate near-misses.
//
// FIVE THINGS ARE LOAD-BEARING, in rough order of how badly getting them wrong
// would fail.
//
//  1. A DRY-RUN MUTATES NOTHING. `validateBundle` is a compile and bind against
//     a READ-ONLY CLONE of the live registry, so it is the one action that is
//     safe against production and the one that needs no confirmation. What makes
//     that true here is not a comment: `dryRun` reaches exactly one method on
//     TrainingEngine and the other three are unreachable from it. A test asserts
//     the registry a fake engine mutates is byte-identical afterwards, because
//     "we did not call the mutating method" is a claim about this file and the
//     acceptance criterion is a claim about the engine.
//
//  2. VALIDATE BEFORE DEFINE, AND BEFORE PROMOTE. Gate-1 is the only place a
//     compile error can be put in front of a developer WITHOUT touching the
//     cluster. Define and promote both validate on their own, so skipping it
//     would work -- and would lose the distinction between "your buffer does not
//     compile" and "the engine refused what you asked for", which for a promote
//     is the difference between a typo and an authorization refusal.
//
//     DEMOTE IS THE EXCEPTION, and deliberately: the engine demotes BY NAME and
//     never compiles the source, so a construct whose source no longer compiles
//     can still be withdrawn. Validating first would refuse exactly the case a
//     demote exists to clean up.
//
//  3. ONE SUPERSESSION GUARD OVER EVERY ROUND-TRIP. Each action is several
//     awaits in sequence and one of them is a modal, which the user can leave
//     open for as long as they like -- the longest window in the extension for
//     the world to move on. A single Latest<"training"> token, taken once per
//     action and checked after every await, is what stops a superseded action
//     from publishing diagnostics over a newer one's or reporting an outcome
//     that no longer describes anything. See src/async/latest.ts.
//
//  4. THE BREAKING OVERRIDE IS A SECOND ACT, NOT A RETRY. `allowBreaking` is
//     never carried on a first attempt and never set by this class on its own.
//     A refused promote comes back as its own outcome carrying the classified
//     diff AND THE EXACT BUNDLE that produced it; the override path re-submits
//     those bytes rather than re-assembling, so what lands is what the developer
//     was shown a diff for even if the buffer moved underneath them. It is gated
//     on `confirmOverride`, which is a different dep from `confirm` so that an
//     ordinary yes cannot answer it.
//
//  5. THE ENGINE ENFORCES, THE EDITOR EXPLAINS. Promote and demote are
//     owner-only, stricter than validate and define. Nothing here checks a role:
//     this process has no trustworthy view of one, and a client-side guess would
//     either block a legitimate owner or promise a non-owner a call the engine
//     refuses anyway. The refusal arrives as the engine's own message naming the
//     required role, and gets rendered as-is.
//
// Deliberately free of `vscode` imports -- everything the editor supplies (the
// cluster, the connection, the bundle, the modals, the Problems panel) arrives
// through TrainingDeps as a plain function, so all of this runs under bare
// `node --test`.
//
// Refs: #3763 #3745

import { failedDiagnostics } from "@znasllc-io/memql-sdk-core/authoring";
import type {
  AuthoringConstruct,
  ConceptSchemaDiff,
  DemoteOutcome,
  DurableDemoteBundleResult,
  DurablePromoteBundleResult,
  SessionDefineBundleResult,
  ValidateBundleResult,
} from "@znasllc-io/memql-sdk-core/authoring";

import { Latest, type LatestToken } from "../async/latest.js";
import { extractErrorId } from "../run/call.js";
import { mapBundleDiagnostics, type MappedDiagnostic } from "../run/diagnostics.js";
import { bundleOrigin } from "../run/bundle.js";
import type { TrainingBundle } from "./closure.js";
import {
  breakingOverridePrompt,
  constructCount,
  demotePrompt,
  promotePrompt,
  sessionPrompt,
  type TrainingCluster,
  type TrainingPrompt,
} from "./report.js";
import { SessionDefinitions } from "./session.js";

/** The four commands the training CodeLens offers. */
export type TrainingActionKind = "dryRun" | "tryInSession" | "promote" | "demote";

/**
 * What a lens hands over: the document and the construct it was rendered above.
 *
 * Exactly `constructs/trainingLens.ts`'s `arguments` payload, which is the
 * contract memql#3761 shipped. Nothing else is available at the click and
 * nothing else is needed -- the state, the position and the file's other
 * constructs are all re-read from the language server at action time, which is
 * the only copy that is current.
 */
export interface TrainingRequest {
  uri: string;
  name: string;
}

/** Which bundle an action submits. See closure.ts for why they differ. */
export type TrainingScope =
  /** The construct and every dependency the cluster does not already have. */
  | "closure"
  /** That construct alone. Demote only. */
  | "construct";

/**
 * The narrow engine surface a training action needs.
 *
 * Functions rather than the SDK's AuthoringClient, for RunEngine's reason: this
 * is the module whose behaviour is worth testing exhaustively, and four stub
 * functions are a far cheaper fixture than a live Connection.
 */
export interface TrainingEngine {
  // `origin` supplies the engine's ambient domain (memql#3800).
  validateBundle(sources: string, origin?: string): Promise<ValidateBundleResult>;
  sessionDefineBundle(sources: string, origin?: string): Promise<SessionDefineBundleResult>;
  durablePromoteBundle(
    sources: string,
    options: { allowBreaking?: boolean },
  ): Promise<DurablePromoteBundleResult>;
  durableDemoteBundle(sources: string): Promise<DurableDemoteBundleResult>;
}

export interface TrainingDeps {
  /** The selected cluster, or undefined when none is selected. */
  cluster(): TrainingCluster | undefined;
  /** The live engine surface, or undefined when not connected. */
  engine(): TrainingEngine | undefined;
  /**
   * Assembles the bundle for a request.
   *
   * `undefined` means the document no longer declares that construct -- an
   * ordinary race between a lens rendering and a buffer changing, reported as an
   * error rather than crashed on.
   */
  assemble(request: TrainingRequest, scope: TrainingScope): Promise<TrainingBundle | undefined>;
  /** The ordinary confirmation modal. Resolves true to proceed. */
  confirm(prompt: TrainingPrompt): Promise<boolean>;
  /**
   * The breaking-change override modal. A DIFFERENT dep from `confirm` on
   * purpose: the override must be its own deliberate answer, and two channels
   * are what make "an ordinary yes cannot unlock it" structural rather than
   * careful.
   */
  confirmOverride(prompt: TrainingPrompt): Promise<boolean>;
  /** Publishes validation diagnostics to the Problems panel. An empty array CLEARS them. */
  publishDiagnostics(diagnostics: MappedDiagnostic[]): void;
  /** Renders an absolute path for display -- workspace-relative in the editor. */
  display(path: string): string;
  /**
   * Called after a promote or demote actually changed the cluster, so the
   * catalog the training state is computed against is re-read. Without it every
   * lens in the workspace keeps reporting the state from before the act that
   * changed it.
   */
  catalogChanged(): void | Promise<void>;
}

/** What a successful action did, in the terms that action reports in. */
export type TrainingResult =
  | { kind: "dryRun"; constructs: number }
  | { kind: "tryInSession"; defined: AuthoringConstruct[] }
  | {
      kind: "promote";
      promoted: AuthoringConstruct[];
      conceptDiffs: ConceptSchemaDiff[];
      /** True when this promote carried the breaking-change override. */
      overridden: boolean;
    }
  | { kind: "demote"; demoted: AuthoringConstruct[]; outcomes: DemoteOutcome[] };

export type TrainingOutcome =
  | {
      status: "ok";
      action: TrainingActionKind;
      request: TrainingRequest;
      cluster: TrainingCluster;
      result: TrainingResult;
    }
  | { status: "superseded" }
  | { status: "declined"; action: TrainingActionKind; request: TrainingRequest }
  | {
      status: "invalid";
      action: TrainingActionKind;
      request: TrainingRequest;
      diagnostics: MappedDiagnostic[];
    }
  | {
      /**
       * A promote the engine REFUSED for a breaking concept change. Not an
       * error: the refusal IS the diff, and it is the only outcome that offers
       * an override.
       *
       * `bundle` is the exact one that produced it, carried whole rather than as
       * its sources alone: the override re-submits those bytes, and if THAT
       * attempt reports a per-construct diagnostic it needs the same offset
       * table to map it back to a file.
       */
      status: "breaking";
      request: TrainingRequest;
      cluster: TrainingCluster;
      bundle: TrainingBundle;
      diffs: ConceptSchemaDiff[];
      error: string;
    }
  | {
      status: "error";
      action: TrainingActionKind;
      request: TrainingRequest;
      message: string;
      errorId: string;
    };

export class TrainingActions {
  // ONE guard for every round-trip of every action. See point 3.
  private readonly latest = new Latest<"training">();
  private readonly sessions = new SessionDefinitions();

  constructor(private readonly deps: TrainingDeps) {}

  /** Called by the adapter on every connection-state change. */
  noteStreamReset(): void {
    this.sessions.noteStreamReset();
  }

  /** The session-defined set, for the lens that keeps "temporary" on screen. */
  get sessionDefinitions(): SessionDefinitions {
    return this.sessions;
  }

  /**
   * Gate-1 compile and bind, in a sandbox, against a read-only clone of the
   * live registry.
   *
   * NO CONFIRMATION, and that is the correct asymmetry rather than an omission:
   * this is the one action with nothing to confirm. It mutates no registry, it
   * writes no row, it is safe against production, and a modal in front of it
   * would train a developer to click through the modals that matter.
   */
  async dryRun(request: TrainingRequest): Promise<TrainingOutcome> {
    const token = this.latest.begin();
    const context = this.preflight("dryRun", request);
    if ("outcome" in context) return context.outcome;

    const bundle = await this.assemble("dryRun", request, "closure");
    if ("outcome" in bundle) return bundle.outcome;
    if (!this.latest.isCurrent(token)) return superseded();

    const validated = await this.validate("dryRun", request, context.engine, bundle.bundle, token);
    if ("outcome" in validated) return validated.outcome;

    return {
      status: "ok",
      action: "dryRun",
      request,
      cluster: context.cluster,
      result: { kind: "dryRun", constructs: constructCount(bundle.bundle) },
    };
  }

  /**
   * Register the bundle into the caller's own registry FOR THE LIFETIME OF THIS
   * STREAM, so it is callable by name and nowhere else.
   *
   * Confirmed even though it commits nothing, because what the confirmation is
   * for is not the risk -- it is the RESEMBLANCE. A session-defined construct
   * answers calls exactly as a promoted one does, and a developer who does not
   * know which of the two just happened is the failure this whole design exists
   * to prevent.
   */
  async tryInSession(request: TrainingRequest): Promise<TrainingOutcome> {
    const token = this.latest.begin();
    const context = this.preflight("tryInSession", request);
    if ("outcome" in context) return context.outcome;

    const bundle = await this.assemble("tryInSession", request, "closure");
    if ("outcome" in bundle) return bundle.outcome;
    if (!this.latest.isCurrent(token)) return superseded();

    const confirmed = await this.deps.confirm(
      sessionPrompt(context.cluster, request.name, bundle.bundle, this.deps.display),
    );
    if (!this.latest.isCurrent(token)) return superseded();
    if (!confirmed) return { status: "declined", action: "tryInSession", request };

    const validated = await this.validate(
      "tryInSession",
      request,
      context.engine,
      bundle.bundle,
      token,
    );
    if ("outcome" in validated) return validated.outcome;

    let defined: SessionDefineBundleResult;
    try {
      defined = await context.engine.sessionDefineBundle(bundle.bundle.sources);
    } catch (err) {
      if (!this.latest.isCurrent(token)) return superseded();
      // The registration state is now UNKNOWN -- the request may have been
      // applied before the transport failed. Forgetting is the safe direction:
      // a lens that stops claiming something true costs nothing, and one that
      // keeps claiming something false is the failure this class is about.
      this.sessions.forget(context.cluster.name);
      return this.fail("tryInSession", request, err);
    }
    if (!this.latest.isCurrent(token)) return superseded();

    if (!defined.ok) {
      this.sessions.forget(context.cluster.name);
      const mapped = mapBundleDiagnostics(defined.diagnostics, bundle.bundle);
      if (mapped.length > 0) {
        this.deps.publishDiagnostics(mapped);
        return { status: "invalid", action: "tryInSession", request, diagnostics: mapped };
      }
      return this.refuse("tryInSession", request, defined.error, "The engine refused the bundle.");
    }

    this.sessions.record(context.cluster.name, defined.defined);
    return {
      status: "ok",
      action: "tryInSession",
      request,
      cluster: context.cluster,
      result: { kind: "tryInSession", defined: defined.defined },
    };
  }

  /**
   * Durably promote the closure into the SHARED registry: persisted, visible to
   * every caller, live on every node within seconds, replayed on restart.
   *
   * Never carries `allowBreaking`. A promote refused for a breaking concept
   * change comes back as the `breaking` outcome; `promoteWithOverride` is the
   * only path that sets the flag, and it is a separate call reached through a
   * separate confirmation.
   */
  async promote(request: TrainingRequest): Promise<TrainingOutcome> {
    const token = this.latest.begin();
    const context = this.preflight("promote", request);
    if ("outcome" in context) return context.outcome;

    const bundle = await this.assemble("promote", request, "closure");
    if ("outcome" in bundle) return bundle.outcome;
    if (!this.latest.isCurrent(token)) return superseded();

    // THE CLOSURE IS SHOWN BEFORE ANYTHING IS SUBMITTED. What goes is the
    // construct plus every dependency the cluster does not have, which is the
    // one fact about a promote not visible from where the click happened.
    const confirmed = await this.deps.confirm(
      promotePrompt(context.cluster, request.name, bundle.bundle, this.deps.display),
    );
    if (!this.latest.isCurrent(token)) return superseded();
    if (!confirmed) return { status: "declined", action: "promote", request };

    const validated = await this.validate("promote", request, context.engine, bundle.bundle, token);
    if ("outcome" in validated) return validated.outcome;

    return this.submitPromote(request, context.cluster, context.engine, bundle.bundle, {}, token);
  }

  /**
   * Re-submit a refused promote WITH the breaking-change override.
   *
   * Takes the BUNDLE off the refusal rather than re-assembling, so the bytes
   * that land are the bytes the diff described. Re-assembling would have been
   * the obvious implementation and is subtly wrong: the developer read a modal
   * for as long as they liked, and a buffer that changed in the meantime would
   * put a different change through under an approval given for another one.
   *
   * Gated on `confirmOverride`, never on `confirm`.
   */
  async promoteWithOverride(
    request: TrainingRequest,
    cluster: TrainingCluster,
    bundle: TrainingBundle,
    diffs: readonly ConceptSchemaDiff[],
  ): Promise<TrainingOutcome> {
    const token = this.latest.begin();
    const context = this.preflight("promote", request);
    if ("outcome" in context) return context.outcome;
    // THE SAME CLUSTER THE REFUSAL CAME FROM. Everything else about this path is
    // built to make the override apply to exactly what the developer was shown,
    // and a cluster switch between the refusal and the click would defeat all of
    // it -- the bytes would be right and the destination wrong. It is the one
    // thing the supersession guard cannot cover, because the override is a new
    // action rather than a continuation of the refused one.
    if (context.cluster.name !== cluster.name) {
      return this.error(
        "promote",
        request,
        `This refusal came from ${cluster.label} and the selected cluster is now ${context.cluster.label}. Promote again to see the diff for this cluster.`,
      );
    }

    const confirmed = await this.deps.confirmOverride(breakingOverridePrompt(diffs));
    if (!this.latest.isCurrent(token)) return superseded();
    if (!confirmed) return { status: "declined", action: "promote", request };

    // No Gate-1 here. These exact sources were validated and then compiled by
    // the engine on the attempt that produced the diff -- the refusal was a
    // judgement about DATA, not about the source -- so re-validating would only
    // add a round-trip and a second chance for the answer to differ.
    return this.submitPromote(
      request,
      context.cluster,
      context.engine,
      bundle,
      { allowBreaking: true },
      token,
    );
  }

  /**
   * Withdraw ONE construct from the shared registry.
   *
   * Not the closure. Promote carries a construct's dependencies because they
   * have to arrive with it; withdrawing them would break other promoted
   * constructs still bound against them, and nothing about clicking Demote on
   * one construct asks for that.
   *
   * No Gate-1. The engine demotes by name and never compiles the source, so
   * validating first would refuse exactly the case a demote exists to clean up:
   * a construct whose source no longer compiles.
   */
  async demote(request: TrainingRequest): Promise<TrainingOutcome> {
    const token = this.latest.begin();
    const context = this.preflight("demote", request);
    if ("outcome" in context) return context.outcome;

    const bundle = await this.assemble("demote", request, "construct");
    if ("outcome" in bundle) return bundle.outcome;
    if (!this.latest.isCurrent(token)) return superseded();

    const target = bundle.bundle.members[0]?.constructs[0];
    const confirmed = await this.deps.confirm(
      demotePrompt(context.cluster, target?.kind ?? "construct", request.name),
    );
    if (!this.latest.isCurrent(token)) return superseded();
    if (!confirmed) return { status: "declined", action: "demote", request };

    let demoted: DurableDemoteBundleResult;
    try {
      demoted = await context.engine.durableDemoteBundle(bundle.bundle.sources);
    } catch (err) {
      if (!this.latest.isCurrent(token)) return superseded();
      return this.fail("demote", request, err);
    }
    if (!this.latest.isCurrent(token)) return superseded();

    if (!demoted.ok) {
      // NOT mapped into the Problems panel. A demote never compiles anything, so
      // a failure here is a bundle-level refusal -- an authorization refusal, a
      // construct the cluster does not carry -- and the message is the whole of
      // what the engine has to say. Publishing it as a file diagnostic would put
      // "you are not the owner" on a line of source.
      return this.refuse("demote", request, demoted.error, "The engine refused the demote.");
    }

    await this.deps.catalogChanged();
    return {
      status: "ok",
      action: "demote",
      request,
      cluster: context.cluster,
      result: { kind: "demote", demoted: demoted.demoted, outcomes: demoted.outcomes },
    };
  }

  // ---------------------------------------------------------------------------
  // The shared sequence
  // ---------------------------------------------------------------------------

  private async submitPromote(
    request: TrainingRequest,
    cluster: TrainingCluster,
    engine: TrainingEngine,
    bundle: TrainingBundle,
    options: { allowBreaking?: boolean },
    token: LatestToken<"training">,
  ): Promise<TrainingOutcome> {
    let promoted: DurablePromoteBundleResult;
    try {
      promoted = await engine.durablePromoteBundle(bundle.sources, options);
    } catch (err) {
      if (!this.latest.isCurrent(token)) return superseded();
      return this.fail("promote", request, err);
    }
    if (!this.latest.isCurrent(token)) return superseded();

    if (!promoted.ok) {
      // A BREAKING REFUSAL IS NOT AN ERROR. It is an answer, it carries the
      // classification, and it is the only outcome with an override to offer --
      // so it is checked before the generic failure paths, which would otherwise
      // render the engine's prose and throw the diff away.
      const breaking = promoted.conceptDiffs.filter((d) => d.breaking && !d.overridden);
      if (breaking.length > 0 && options.allowBreaking !== true) {
        return {
          status: "breaking",
          request,
          cluster,
          bundle,
          diffs: promoted.conceptDiffs,
          error: promoted.error,
        };
      }
      const mapped = mapBundleDiagnostics(promoted.diagnostics, bundle);
      if (mapped.length > 0) {
        this.deps.publishDiagnostics(mapped);
        return { status: "invalid", action: "promote", request, diagnostics: mapped };
      }
      // Where the owner-only refusal lands, verbatim. The engine names the role
      // it required; this adds nothing to it and takes nothing away.
      return this.refuse("promote", request, promoted.error, "The engine refused the promote.");
    }

    await this.deps.catalogChanged();
    return {
      status: "ok",
      action: "promote",
      request,
      cluster,
      result: {
        kind: "promote",
        promoted: promoted.promoted,
        conceptDiffs: promoted.conceptDiffs,
        overridden: options.allowBreaking === true,
      },
    };
  }

  private preflight(
    action: TrainingActionKind,
    request: TrainingRequest,
  ): { cluster: TrainingCluster; engine: TrainingEngine } | { outcome: TrainingOutcome } {
    const cluster = this.deps.cluster();
    if (cluster === undefined) {
      return {
        outcome: this.error(action, request, "No cluster selected. Pick one in the Clusters view."),
      };
    }
    const engine = this.deps.engine();
    if (engine === undefined) {
      return {
        outcome: this.error(
          action,
          request,
          `Not connected to ${cluster.label}. Select the cluster in the Clusters view to connect.`,
        ),
      };
    }
    return { cluster, engine };
  }

  private async assemble(
    action: TrainingActionKind,
    request: TrainingRequest,
    scope: TrainingScope,
  ): Promise<{ bundle: TrainingBundle } | { outcome: TrainingOutcome }> {
    let bundle: TrainingBundle | undefined;
    try {
      bundle = await this.deps.assemble(request, scope);
    } catch (err) {
      return { outcome: this.fail(action, request, err) };
    }
    if (bundle === undefined) {
      return {
        outcome: this.error(
          action,
          request,
          `This file no longer declares "${request.name}". It was renamed or removed since the lens was drawn.`,
        ),
      };
    }
    return { bundle };
  }

  /**
   * Gate-1, and the publish/clear of what it found.
   *
   * The CLEAR is as important as the publish: without it a compile error fixed
   * on the next attempt stays in the Problems panel until some later attempt
   * happens to fail in the same file.
   */
  private async validate(
    action: TrainingActionKind,
    request: TrainingRequest,
    engine: TrainingEngine,
    bundle: TrainingBundle,
    token: LatestToken<"training">,
  ): Promise<{ ok: true } | { outcome: TrainingOutcome }> {
    let validated: ValidateBundleResult;
    try {
      validated = await engine.validateBundle(bundle.sources, bundleOrigin(bundle));
    } catch (err) {
      if (!this.latest.isCurrent(token)) return { outcome: superseded() };
      return { outcome: this.fail(action, request, err) };
    }
    if (!this.latest.isCurrent(token)) return { outcome: superseded() };

    // failedDiagnostics, not `!ok`. A bundle routinely carries constructs this
    // pass does not compile -- a concept, a shape -- each reporting ok=false
    // with skipped=true, and reading `!ok` alone renders those as compile errors
    // on a bundle the engine considers perfectly valid.
    const failures = failedDiagnostics(validated.diagnostics);
    if (!validated.ok || failures.length > 0) {
      const mapped = mapBundleDiagnostics(validated.diagnostics, bundle);
      this.deps.publishDiagnostics(mapped);
      if (mapped.length === 0) {
        // !ok with nothing mapped: the engine refused the bundle without
        // attributing it to a construct. Reporting "invalid" with an empty
        // Problems panel would leave the developer with a failure and nothing
        // on screen, so it becomes an ordinary error carrying what is known.
        return {
          outcome: this.error(
            action,
            request,
            "The engine rejected the bundle without a per-construct diagnostic.",
          ),
        };
      }
      return { outcome: { status: "invalid", action, request, diagnostics: mapped } };
    }
    this.deps.publishDiagnostics([]);
    return { ok: true };
  }

  private fail(
    action: TrainingActionKind,
    request: TrainingRequest,
    err: unknown,
  ): TrainingOutcome {
    return this.error(action, request, err instanceof Error ? err.message : String(err));
  }

  private refuse(
    action: TrainingActionKind,
    request: TrainingRequest,
    error: string,
    fallback: string,
  ): TrainingOutcome {
    return this.error(action, request, error === "" ? fallback : error);
  }

  // The engine's ERR- id is pulled out so the UI can render it separately and
  // copyably. It is the only handle a developer has on the server-side log entry
  // for their failure, so burying it inside prose is the difference between a
  // support thread that resolves and one that does not.
  private error(
    action: TrainingActionKind,
    request: TrainingRequest,
    message: string,
  ): TrainingOutcome {
    return { status: "error", action, request, message, errorId: extractErrorId(message) };
  }
}

function superseded(): TrainingOutcome {
  return { status: "superseded" };
}

export type { TrainingCluster, TrainingPrompt };
