import type { AnalysisReport, DeploymentRow, PackageRow, ReportDeployable } from "../packages/rows";
import { sourceLabel } from "../packages/rows";
import { bundleForm, bundleFormLabel, type SiteRow } from "../rows";
import { WEB_TARGET, kindLabel, type StopDef, type StopId } from "../targets";

// THE RAIL -- this surface's one signature element, read three ways.
//
// ===========================================================================
// THE RAIL IS THE FORM
// ===========================================================================
// Composing a deployable, watching it deploy and reading its standing status
// are one vertical device, top to bottom: Source, What it is, Where it lives,
// Build, Live. One reading, `railFor`, over three inputs:
//
//   deploy    today's six-stage reading over a DeploymentRow, reproduced
//             exactly (the D6 law: stage -> roll -> publish, reversed on a
//             rollback). Drawn under Every attempt.
//   standing  the target's five stops read off the rows -- the package, the
//             latest run's report, the site -- with an in-flight run reported
//             as progress on the same stops rather than through a fourth
//             mode.
//   compose   the same five stops as INPUTS: answered, being answered, not
//             reachable yet, or parked by a refusal.
//
// It is PURE and has no DOM, because what the rail SAYS is the assertion
// worth making: "a prebuilt app skips Build, with the reason" is a statement
// about this function, not about a picture of it. Rail.tsx draws the result.
//
// ===========================================================================
// WHY A SEQUENCE IS THE RIGHT SHAPE HERE, AND ALMOST NOWHERE ELSE
// ===========================================================================
// Numbered, ordered devices are the most over-used structure in software
// design, and they are right exactly when the order carries information the
// reader needs. A deploy is that case: `stage -> roll -> publish` is a LAW
// (design D6), not a rendering choice, and a failure before `publish` leaves
// every site serving what it was serving -- which is only legible if you can
// see where the run stopped. The five stops earn the same shape the same way:
// an address is chosen once and kept, a build happens after the analysis has
// said what to build, and nothing is live before it is published.
//
// ===========================================================================
// THE SKIPPED STOPS ARE THE POINT
// ===========================================================================
// A progress bar shows what happened. This shows what did not, and says why:
//
//     build  ->  stage DSL  ->  roll  ->  publish
//                 skipped       skipped
//                 this package ships no DSL
//
// That is the single most useful sentence this surface can show, because it is
// exactly why an SPA-only redeploy lands in seconds and restarts nothing --
// and a person who cannot see it has no way to know whether their deploy was
// fast or broken. Rendering a skipped stage as absent would leave them
// counting steps to find out. The five-stop rail keeps the rule: a prebuilt
// Build is drawn skipped, "its built output is in the source".

/**
 * One closed set of states across the three modes. `done` / `complete` draw
 * alike (compose says a stop is complete; a record says a stage is done);
 * `ahead` / `pending` draw alike (not reached; not reachable yet). `open` is a
 * stop waiting on the person -- a held ring, never a pulse, because nothing
 * is moving until they act. `current` is the one thing that moves.
 */
export type StopState = "done" | "current" | "skipped" | "stopped" | "ahead" | "open" | "pending" | "complete";

export interface RailStage {
  id: string;
  label: string;
  blurb: string;
  state: StopState;
  /**
   * What the stop's note says, in the words a person can act on: why a stage
   * was skipped, the refusal a stopped stop carries (the server's sentence,
   * verbatim -- the OS headline belongs to the notice the stop renders
   * beneath it), or a settled stop's answer. Empty when the blurb is the note.
   */
  reason: string;
}

export interface Rail {
  stages: RailStage[];
  /** Read bottom-up: a rollback. The array order never changes. */
  reversed: boolean;
}

/** A refusal as the rows carry it: a stable code and the server's sentence. */
export interface RailProblem {
  code: string;
  message: string;
  scope?: string;
}

// ---------------------------------------------------------------------------
// The inputs
// ---------------------------------------------------------------------------

export interface DeployInput {
  mode: "deploy";
  deployment: DeploymentRow;
}

/** What the Source stop reads off a tracked package. */
export type StandingSource = Pick<PackageRow, "sourceKind" | "repoUrl" | "repoRef">;

/** What the Where-it-lives and Live stops read off the site row. */
export type StandingSite = Pick<SiteRow, "hostname" | "kind" | "status" | "bundleRef">;

export interface StandingInput {
  mode: "standing";
  /** The tracked source, or null for a hand-made deployable, whose bundle form is its source. */
  pkg: StandingSource | null;
  /** The app's name in the package manifest; "" for a hand-made site, or for the package as a whole. */
  app: string;
  /**
   * The NEWEST run of the source, whatever its status: in flight, parked,
   * finished or refused. Null when nothing has been attempted. Its report is
   * the What-it-is verdict; its error is where a refused run stopped.
   */
  run: DeploymentRow | null;
  /** The site row this deployable answers at; null before its first publish. */
  site: StandingSite | null;
}

export interface ComposeInput {
  mode: "compose";
  /** The stops the person has answered. Order is the rail's, not this list's. */
  answered: readonly StopId[];
  /** The stop being answered now; null while parked or finished. */
  open: StopId | null;
  /**
   * The Source probe's verdict when it could not reach the source (one of
   * `PROBE_REASONS`), or "" when it could, or has not run. A reason parks the
   * Source stop: the credential field is what moves it on.
   */
  probeReason: string;
  /** The parked run's report, once Analyze has run. */
  report: AnalysisReport | null;
  /** The refusal that parked the flow, if any: the run's error, or the report's first fatal problem. */
  problem: RailProblem | null;
  /** A settled stop's one-line answer ("acme/shop at main", the hostname), when the caller has one. */
  answers?: Partial<Record<StopId, string>>;
  /**
   * The stop a RUN is at right now, while the compose flow is watching one.
   *
   * It draws as `current` -- the one thing on the rail that moves -- and
   * every later stop reads as not reachable yet. Without it a flow watching
   * its own analysis would have to draw that stop `open`, which is a HELD
   * ring meaning "waiting on you", while the person is waiting on the
   * cluster. The design's rule that the ring is the only motion is why this
   * is a stop id and not a second spinner.
   */
  moving?: StopId | null;
  /**
   * The source carries its built output, so Build reads skipped with the
   * reason before anything runs. A hand-made zip and a CI push are always
   * this -- they ARE the built output -- and a package answers the same
   * question through its report instead.
   */
  prebuilt?: boolean;
}

export type RailInput = DeployInput | StandingInput | ComposeInput;

// ---------------------------------------------------------------------------
// The sentences the design fixes
// ---------------------------------------------------------------------------

/** What the Source probe says when it could not see the source. */
export const PROBE_REASONS = {
  notReachable: "private, or not there",
  credentialCannotSee: "this token cannot see it",
  hostUnsupported: "only github.com today, or upload a zip",
} as const;

const PREBUILT_REASON = "its built output is in the source";
const NOTHING_PUBLISHED = "Nothing published yet.";
const WAITING_FOR_PUSH = "waiting for the first push";
const PAUSED = "Paused. It answers 503 rather than 404, so a deliberately paused site stays distinguishable from a typo.";
const ARCHIVED = "Archived. It answers nothing, like a site that never existed.";

/**
 * Scoped to one app, fatal to that app and NOT to the package (design section
 * B), exactly as go_pack_not_deployable is reported today: the What-it-is
 * stop carries the server's sentence and the rest of the rail proceeds.
 */
const NOT_OFFERED_CODE = "deployable_target_not_offered";

// ---------------------------------------------------------------------------
// The stop set
// ---------------------------------------------------------------------------

/** The five stops, read from the target so the target defines the set. */
export const RAIL_STOPS: readonly StopDef[] = WEB_TARGET.stops;

const STOP_INDEX: Readonly<Record<StopId, number>> = {
  source: 0,
  whatItIs: 1,
  whereItLives: 2,
  build: 3,
  live: 4,
};

/** Where an in-flight run IS, in stop terms. */
const RUN_STOP: Readonly<Record<string, StopId>> = {
  analyzing: "whatItIs",
  awaiting_confirm: "whatItIs",
  building: "build",
  staging_dsl: "live",
  rolling: "live",
  publishing: "live",
};

/**
 * Which stop a refusal belongs to, by its code, for the codes whose home
 * does not follow from where the run got to. A source refusal is the Source
 * stop's wherever the flow was open; a build refusal is Build's. Codes absent
 * here park the stop that was open when they arrived -- that is where the
 * flow was, and guessing a different stop would send the person to repair
 * the wrong thing.
 */
const STOP_FOR_CODE: Readonly<Record<string, StopId>> = {
  source_unreadable: "source",
  source_too_large: "source",
  bundle_path_invalid: "source",
  source_host_unsupported: "source",
  credential_not_found: "source",
  credential_revoked: "source",
  // The GitHub App grant codes (epic memql#4912) all park the SOURCE stop,
  // because every repair they name is a choice about where this deployable
  // comes from: reconnect, install the app, ask an organisation owner, or use
  // a token instead. Without an entry here a grant refusal would park
  // whichever stop happened to be open, which is the one place a person would
  // not look for it.
  reconnect_required: "source",
  repository_not_installed: "source",
  installation_pending: "source",
  github_app_not_configured: "source",
  connect_state_invalid: "source",
  package_manifest_missing: "whatItIs",
  package_manifest_invalid: "whatItIs",
  deployable_path_missing: "whatItIs",
  deployable_kind_unknown: "whatItIs",
  deployable_binding_missing: "whatItIs",
  dsl_domain_reserved: "whatItIs",
  dsl_refuses_boot: "whatItIs",
  dsl_requires_cluster_owner: "whatItIs",
  deployable_build_failed: "build",
  deployable_publish_failed: "live",
};

function stage(stop: StopDef, state: StopState, reason = ""): RailStage {
  return { id: stop.id, label: stop.label, blurb: stop.blurb, state, reason };
}

// ---------------------------------------------------------------------------
// railFor
// ---------------------------------------------------------------------------

/**
 * railFor turns one input into the stops to draw.
 *
 * Pure, and exported, because what a rail SAYS is the assertion worth making
 * in a test -- "an SPA-only run marks stage and roll skipped, with the reason"
 * is a statement about this function, not about a DOM.
 */
export function railFor(input: RailInput): Rail {
  switch (input.mode) {
    case "deploy":
      return { stages: deployRail(input.deployment), reversed: false };
    case "standing":
      return { stages: standingRail(input), reversed: false };
    case "compose":
      return { stages: composeRail(input), reversed: false };
  }
}

// ---------------------------------------------------------------------------
// deploy -- the six stages, exactly as before
// ---------------------------------------------------------------------------

/** The D6 order, and its labels. Mirrors component/packages/pipeline.go. */
const STAGES: readonly { id: string; label: string; blurb: string }[] = [
  { id: "analyzing", label: "Analyze", blurb: "Read the tree and run the gates a node runs at boot" },
  { id: "awaiting_confirm", label: "Confirm", blurb: "Show what deploying would do, and wait" },
  { id: "building", label: "Build", blurb: "Turn each app's source into the files that get served" },
  { id: "staging_dsl", label: "Stage DSL", blurb: "Write the package's MemQL into storage, by content" },
  { id: "rolling", label: "Roll", blurb: "Restart the cluster onto the staged MemQL" },
  { id: "publishing", label: "Publish", blurb: "Point each site at its new files" },
];

const TERMINAL: Record<string, "done" | "refused" | "failed" | "abandoned"> = {
  succeeded: "done",
  refused: "refused",
  failed: "failed",
  // memql#4900: a run whose node stopped saying it was alive. Terminal, and
  // deliberately NOT a flavour of `failed` -- nothing failed that this
  // cluster can name, and the D6 order guarantees every site is still
  // serving what it was serving. The MARK is the same, because the run did
  // stop where it stopped; the SENTENCE is what differs, and it comes from
  // the row's own error naming the node and when it was last heard.
  abandoned: "abandoned",
};

function deployRail(deployment: DeploymentRow): RailStage[] {
  const terminal = TERMINAL[deployment.status];
  const reachedIndex = STAGES.findIndex((s) => s.id === deployment.status);

  // A run with no DSL in its report never enters stage or roll, and that is
  // the D6 fast path rather than an omission. Read from the REPORT rather than
  // from dslVersion, because a run that was refused before it staged has no
  // dslVersion either and those are different stories.
  const domains = deployment.report?.dslDomains ?? [];
  const carriesDsl = domains.length > 0;
  const dslSkipped = !carriesDsl || (terminal === "done" && deployment.dslVersion === "");

  // THE TWO SKIPPED STAGES SAY DIFFERENT THINGS, and stacking one sentence
  // twice read as a stutter rather than as two facts. They are a CHAIN: the
  // first says why there was nothing to stage, the second says what follows
  // from that -- which is also the more useful half, because "nothing had to
  // restart" is the answer to the question somebody actually has.
  const skipReasonFor = (id: string): string => {
    if (id === "staging_dsl") {
      return carriesDsl
        ? "this package's MemQL is already the version this cluster is running"
        : "this package ships no MemQL, so there is nothing to stage";
    }
    return "nothing was staged, so nothing had to restart";
  };

  return STAGES.map((stage, index): RailStage => {
    const isDslStage = stage.id === "staging_dsl" || stage.id === "rolling";

    if (isDslStage && dslSkipped) {
      return { ...stage, state: "skipped", reason: skipReasonFor(stage.id) };
    }
    if (terminal === "done") {
      return { ...stage, state: "done", reason: "" };
    }
    if (terminal !== undefined) {
      // A refused or failed run stopped somewhere. The row records the last
      // stage it reached, so everything before it ran and everything after it
      // never started -- which is the guarantee that nothing was published.
      const stoppedAt = lastReachedIndex(deployment);
      if (index < stoppedAt) return { ...stage, state: "done", reason: "" };
      if (index === stoppedAt) return { ...stage, state: "stopped", reason: "" };
      return { ...stage, state: "ahead", reason: "" };
    }
    if (reachedIndex < 0) return { ...stage, state: "ahead", reason: "" };
    if (index < reachedIndex) return { ...stage, state: "done", reason: "" };
    if (index === reachedIndex) return { ...stage, state: "current", reason: "" };
    return { ...stage, state: "ahead", reason: "" };
  });
}

// lastReachedIndex is where a terminal run got to.
//
// A refusal that happened during analysis leaves no later stage on the row, so
// the honest answer for a run whose status is now `refused` is the FURTHEST
// stage its evidence supports: a report means analysis ran, deployables mean
// publishing did.
function lastReachedIndex(d: DeploymentRow): number {
  // A run the SWEEP closed kept the stage it was at (memql#4900), because
  // closing the row is what destroys it: the status field held `building`,
  // and `abandoned` replaced it. Without this the evidence below would draw
  // a run that died mid-build as having stopped at Analyze -- understating
  // what it did and sending somebody to look in the wrong place.
  if (d.stoppedAt !== "") {
    const at = indexOf(d.stoppedAt);
    if (at >= 0) return at;
  }
  if (d.deployables.length > 0) return indexOf("publishing");
  if (d.dslVersion !== "") return indexOf("rolling");
  if (d.buildLogTail !== "") return indexOf("building");
  if (d.report !== null) return indexOf("analyzing");
  return 0;
}

function indexOf(id: string): number {
  return STAGES.findIndex((s) => s.id === id);
}

// ---------------------------------------------------------------------------
// standing -- the five stops read off the rows
// ---------------------------------------------------------------------------

function standingRail(input: StandingInput): RailStage[] {
  const { pkg, app, run, site } = input;
  const terminal = run === null ? undefined : TERMINAL[run.status];
  const inFlightAt = run !== null && terminal === undefined ? (RUN_STOP[run.status] ?? null) : null;
  const stoppedAt = run !== null && terminal !== undefined && terminal !== "done" ? stoppedStopFor(run) : null;
  const report = run?.report ?? null;
  const appReport = findApp(report, app);

  return RAIL_STOPS.map((stop): RailStage => {
    const index = STOP_INDEX[stop.id];

    // DURING A RUN THE SAME STOPS REPORT PROGRESS: the stop the run is at is
    // current and every later one is ahead, which is how "the rail moves" is
    // true without a fourth mode. Earlier stops keep their standing facts.
    if (inFlightAt !== null) {
      const at = STOP_INDEX[inFlightAt];
      if (index === at) return stage(stop, "current");
      if (index > at) return stage(stop, "ahead");
    }

    // A refused or failed run stopped somewhere, and that stop says why in
    // the server's own words. The stops AFTER it are not dimmed wholesale:
    // a site that was live before a redeploy failed at Build is still
    // serving what it was serving (design H), and a rail that read it as
    // "not reached" would be lying about the one fact somebody came to
    // check. Each later stop reads its own standing fact, and a stop with no
    // fact is ahead anyway.
    if (stoppedAt === stop.id) return stage(stop, "stopped", refusalOf(run));

    switch (stop.id) {
      case "source":
        return sourceStop(stop, pkg, site);
      case "whatItIs":
        if (report !== null) return stage(stop, "done", whatItIsNote(report, app, null));
        // A hand-made site's kind is what it is; a package that has never
        // been analyzed has no verdict yet.
        if (pkg === null && site !== null) return stage(stop, "done", kindLabel(site.kind));
        return stage(stop, "ahead");
      case "whereItLives":
        if (site !== null && site.hostname.trim() !== "") return stage(stop, "done", site.hostname);
        return stage(stop, "ahead");
      case "build":
        return buildStop(stop, pkg, site, appReport, terminal === "done" || stoppedAt === "live");
      case "live":
        return liveStop(stop, site);
    }
  });
}

function sourceStop(stop: StopDef, pkg: StandingSource | null, site: StandingSite | null): RailStage {
  if (pkg !== null) return stage(stop, "done", sourceLabel(pkg));
  if (site !== null) {
    // The placeholder a new deployable starts with (deployables.md) is not
    // a source that has arrived: its bytes are still on somebody's CI.
    if (isPlaceholderBundle(site.bundleRef)) return stage(stop, "done", WAITING_FOR_PUSH);
    return stage(stop, "done", bundleFormLabel(bundleForm(site.bundleRef)));
  }
  return stage(stop, "ahead");
}

function buildStop(
  stop: StopDef,
  pkg: StandingSource | null,
  site: StandingSite | null,
  appReport: ReportDeployable | null,
  built: boolean,
): RailStage {
  // A hand-made site IS its built output, and so is a prebuilt app: the D4
  // fast path, drawn rather than omitted, with the reason.
  if (pkg === null && site !== null) return stage(stop, "skipped", PREBUILT_REASON);
  if (appReport?.prebuilt === true) return stage(stop, "skipped", PREBUILT_REASON);
  if (appReport !== null && built) return stage(stop, "done");
  return stage(stop, "ahead");
}

function liveStop(stop: StopDef, site: StandingSite | null): RailStage {
  if (site === null) return stage(stop, "ahead", NOTHING_PUBLISHED);
  switch (site.status) {
    case "live":
      return stage(stop, "done", `Live at ${site.hostname}.`);
    case "disabled":
      // Amber is what the shell says "not reachable" with; a paused site is
      // exactly that, and the sentence says it was chosen.
      return stage(stop, "stopped", PAUSED);
    case "archived":
      // Neither warn nor error -- nothing is wrong with it, it is filed -- so
      // the muted "did not happen" mark, with the reason.
      return stage(stop, "skipped", ARCHIVED);
    case "draft":
      // Published and not yet made live is the stop WAITING ON THE PERSON:
      // a held ring, because the next thing that happens is theirs.
      if (isPublished(site)) return stage(stop, "open", `Published to ${site.hostname}. Not serving yet.`);
      return stage(stop, "ahead", NOTHING_PUBLISHED);
    default:
      return stage(stop, "ahead");
  }
}

/**
 * The stop a refused or failed run's error belongs to, so the page can render
 * the OS headline beneath the sentence the rail already shows there (design
 * H: the copy above, the server's sentence beneath). Null for a run that is
 * in flight, parked or finished, and for no run at all: those have no
 * refusal to place, and a page that guessed a stop for one would send the
 * person to repair the wrong thing.
 */
export function refusalStopFor(run: DeploymentRow | null): StopId | null {
  if (run === null) return null;
  const terminal = TERMINAL[run.status];
  // `abandoned` is placed here too (memql#4900): the sweep writes a typed
  // error naming the node, so there IS a sentence to put at a stop, and the
  // stop it belongs at is where the run got to.
  if (terminal === undefined || terminal === "done") return null;
  return stoppedStopFor(run);
}

/**
 * Where a terminal run stopped, in stop terms: the furthest stop its
 * evidence supports, the same rule the deploy reading uses -- deployables
 * mean publishing ran, a staged version means the cluster rolled, a build
 * log means the build ran, a report means the analysis did, and nothing at
 * all means the source never arrived. The error's code settles the one case
 * the evidence cannot: a build or publish refusal leaves no later row field.
 */
function stoppedStopFor(run: DeploymentRow): StopId {
  // A run the sweep closed RECORDED the stage it was at (memql#4900), which
  // outranks every inference below: the evidence rule reads row fields a
  // stage WRITES, and a run that died part-way through a stage wrote none of
  // them. Without this a build that was lost places its sentence on the
  // What-it-is stop, which is where the person then looks for a node that
  // went away.
  if (run.stoppedAt !== "") {
    const at = RUN_STOP[run.stoppedAt];
    if (at !== undefined) return at;
  }
  const code = run.error?.code ?? "";
  if (run.deployables.length > 0 || code === "deployable_publish_failed") return "live";
  if (run.dslVersion !== "") return "live";
  if (run.buildLogTail !== "" || code === "deployable_build_failed") return "build";
  if (run.report !== null) return "whatItIs";
  return "source";
}

/** The refused run's own sentence: its error, or the report's first fatal problem. */
function refusalOf(run: DeploymentRow | null): string {
  if (run === null) return "";
  if (run.error !== null && run.error.message.trim() !== "") return run.error.message;
  const fatal = (run.report?.problems ?? []).find((p) => p.fatal);
  return fatal?.message ?? "";
}

/**
 * The placeholder a new deployable starts with -- `blob://sites/<id>/pending/`
 * (deployables.md). It is written so the row can exist before any bytes do,
 * and reading it as a publish would tell a person their CI had pushed when
 * nothing has.
 */
export function isPlaceholderBundle(bundleRef: string): boolean {
  return /\/pending\/?$/.test(bundleRef.trim());
}

/** Whether the site holds a bundle somebody actually published. */
export function isPublished(site: Pick<StandingSite, "bundleRef">): boolean {
  return bundleForm(site.bundleRef) !== "none" && !isPlaceholderBundle(site.bundleRef);
}

// ---------------------------------------------------------------------------
// compose -- the five stops as inputs
// ---------------------------------------------------------------------------

function composeRail(input: ComposeInput): RailStage[] {
  const answered = new Set<StopId>(input.answered);
  // The not-offered code parks nothing, whoever hands it in as THE problem.
  const problem = input.problem !== null && input.problem.code !== NOT_OFFERED_CODE ? input.problem : null;

  // The probe outranks a refusal: it is the Source stop's, and Source comes
  // first. A refusal with no known home parks the stop that was open.
  let parkedAt: StopId | null = null;
  let parkedReason = "";
  if (input.probeReason.trim() !== "") {
    parkedAt = "source";
    parkedReason = input.probeReason;
  } else if (problem !== null) {
    parkedAt = STOP_FOR_CODE[problem.code] ?? input.open ?? "whatItIs";
    parkedReason = problem.message;
  }
  const parkedIndex = parkedAt === null ? -1 : STOP_INDEX[parkedAt];
  const movingIndex = input.moving === null || input.moving === undefined ? -1 : STOP_INDEX[input.moving];

  return RAIL_STOPS.map((stop): RailStage => {
    const index = STOP_INDEX[stop.id];
    if (parkedAt !== null) {
      if (index === parkedIndex) return stage(stop, "stopped", parkedReason);
      if (index > parkedIndex) return stage(stop, "pending");
    }
    // A FORECAST, the way the confirm gate's rail always was: once the
    // report says every app is prebuilt -- or the source IS its built output
    // -- Build reads skipped before anything runs, so a person learns before
    // the click that this source needs no build. It is read BEFORE the moving
    // stop below, because a skipped stage is a fact about the source and not
    // about where a run has got to.
    if (stop.id === "build" && (input.prebuilt === true || (input.report !== null && everyAppPrebuilt(input.report)))) {
      return stage(stop, "skipped", PREBUILT_REASON);
    }
    if (movingIndex >= 0) {
      if (index === movingIndex) return stage(stop, "current");
      if (index > movingIndex) return stage(stop, "pending");
    }
    if (answered.has(stop.id)) {
      const answer = input.answers?.[stop.id] ?? "";
      if (answer !== "") return stage(stop, "complete", answer);
      if (stop.id === "whatItIs") return stage(stop, "complete", whatItIsNote(input.report, "", input.problem));
      return stage(stop, "complete");
    }
    if (input.open === stop.id) return stage(stop, "open");
    return stage(stop, "pending");
  });
}

// ---------------------------------------------------------------------------
// What it is, in one line
// ---------------------------------------------------------------------------

function findApp(report: AnalysisReport | null, app: string): ReportDeployable | null {
  if (report === null || app === "") return null;
  return (report.deployables ?? []).find((d) => d.name === app) ?? null;
}

function offeredApps(report: AnalysisReport): ReportDeployable[] {
  return (report.deployables ?? []).filter((d) => d.problem === undefined);
}

function everyAppPrebuilt(report: AnalysisReport): boolean {
  const apps = offeredApps(report);
  return apps.length > 0 && apps.every((d) => d.prebuilt);
}

/**
 * The verdict: one app's kind and build plan when the rail is about one app,
 * the package's count otherwise -- and, after it, the sentence for every app
 * the cluster knows and does not offer, verbatim from the server, because
 * "iOS is not offered on this cluster yet" is the whole of what a person
 * needs and nothing this build could add would help.
 */
function whatItIsNote(report: AnalysisReport | null, app: string, problem: RailProblem | null): string {
  const parts: string[] = [];
  const notOffered: string[] = [];
  for (const d of report?.deployables ?? []) {
    if (d.problem?.code === NOT_OFFERED_CODE && d.problem.message.trim() !== "") notOffered.push(d.problem.message);
  }
  if (problem?.code === NOT_OFFERED_CODE && problem.message.trim() !== "" && !notOffered.includes(problem.message)) {
    notOffered.push(problem.message);
  }

  const mine = findApp(report, app);
  if (mine !== null) {
    if (mine.problem?.code !== NOT_OFFERED_CODE) parts.push(appVerdict(mine));
  } else if (report !== null) {
    parts.push(reportSummary(report));
  }
  return [...parts, ...notOffered].join(". ");
}

function appVerdict(d: ReportDeployable): string {
  const kind = kindLabel(d.kind);
  if (d.prebuilt) return `${kind}, already built`;
  const command = (d.command ?? "").trim();
  return command === "" ? `${kind}, needs a build` : `${kind}, builds with ${command}`;
}

function reportSummary(report: AnalysisReport): string {
  const apps = offeredApps(report).length;
  const domains = (report.dslDomains ?? []).filter((d) => d.reserved !== true).length;
  const out = [`${apps} ${apps === 1 ? "app" : "apps"}`];
  if (domains > 0) out.push(`${domains} MemQL ${domains === 1 ? "domain" : "domains"}`);
  return out.join(", ");
}

// ---------------------------------------------------------------------------
// The Head's action, by state (design section C)
// ---------------------------------------------------------------------------

/**
 * The nine states the design's table names, as a discriminated input: the
 * page derives `sourceComplete`, `placementsComplete` and `updateAvailable`
 * from what it holds and asks for the one action that follows.
 */
export type HeadState =
  | { at: "composing"; sourceComplete: boolean }
  | { at: "awaiting_confirm"; placementsComplete: boolean }
  | { at: "running" }
  | { at: "draft_with_bundle" }
  | { at: "live"; updateAvailable: boolean }
  | { at: "refused_or_failed" };

/** Every state, in the table's order, so a test can walk the whole table. */
export const HEAD_STATES: readonly HeadState[] = [
  { at: "composing", sourceComplete: false },
  { at: "composing", sourceComplete: true },
  { at: "awaiting_confirm", placementsComplete: false },
  { at: "awaiting_confirm", placementsComplete: true },
  { at: "running" },
  { at: "draft_with_bundle" },
  { at: "live", updateAvailable: true },
  { at: "refused_or_failed" },
  { at: "live", updateAvailable: false },
];

export type HeadActionLabel = "Analyze" | "Deploy" | "Make it live" | "Deploy the update" | "Retry" | "Redeploy";

export interface HeadAction {
  label: HeadActionLabel;
  disabled: boolean;
  /** Redeploy is the quiet one: a live site with nothing newer needs no urging. */
  tone: "primary" | "quiet";
}

/**
 * headActionFor answers the design's table. Null for a run at a non-terminal
 * stage: the rail is moving, and a button beside a moving rail would be a
 * button that competes with it.
 */
export function headActionFor(state: HeadState): HeadAction | null {
  switch (state.at) {
    case "composing":
      return { label: "Analyze", disabled: !state.sourceComplete, tone: "primary" };
    case "awaiting_confirm":
      return { label: "Deploy", disabled: !state.placementsComplete, tone: "primary" };
    case "running":
      return null;
    case "draft_with_bundle":
      return { label: "Make it live", disabled: false, tone: "primary" };
    case "live":
      return state.updateAvailable
        ? { label: "Deploy the update", disabled: false, tone: "primary" }
        : { label: "Redeploy", disabled: false, tone: "quiet" };
    case "refused_or_failed":
      return { label: "Retry", disabled: false, tone: "primary" };
  }
}

// ---------------------------------------------------------------------------
// openStopFor -- which stop is open (epic memql#4937, design section C)
// ---------------------------------------------------------------------------

/**
 * The one stop a settled deployable opens, chosen by WHAT IS A QUESTION.
 *
 * Every stop used to render its body at once, which is what put thirteen rails
 * and 5,069px on one page. Four of the five are settled facts and read as one
 * line each -- mark, label, its answer, a chevron -- and exactly one is open.
 *
 * The order of the checks is the design:
 *
 *  1. A RUN IN FLIGHT opens the stop the run is at. The moving thing is the
 *     question, and nothing else on the page competes with it.
 *  2. A REFUSED RUN opens the stop it stopped at, WITH the refusal. Sending
 *     somebody to Live to read "the build failed" would be sending them to
 *     repair the wrong thing.
 *  3. EVERYTHING ELSE opens Live, because for a settled deployable the live
 *     question is the only one: is it serving, since when, and who is using it.
 *
 * PURE, and the assertion is case 2 -- a test that only covered the happy path
 * would pass against a rail that always opened Live.
 */
export function openStopFor(input: StandingInput): StopId | "" {
  const run = input.run;

  // 1. Moving.
  if (run !== null && run.status !== "" && RUN_STOP[run.status] !== undefined) {
    return RUN_STOP[run.status] ?? "live";
  }

  // 2. Stopped, at the stop it stopped at.
  const stopped = refusalStopFor(run);
  if (stopped !== null) return stopped;

  // 3. Settled.
  return "live";
}
