import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// The kit's LEAF rather than its barrel: page/rail.ts imports `sourceLabel`
// from here and is pure by contract, and the barrel re-exports every
// component the kit has, React included.
import { boolOr, flatten } from "../../../kit/rows";

// What the Packages surface reads, projected once.
//
// Both concepts are declared in dsl/platform/concepts.memql and both are
// BROADCAST (component/node/routing.go carries created + updated for each), so
// everything here is live with no polling: a deploy advances by writing its own
// status six times, and those writes are what a person watching a deploy is
// watching.

/** The tracked source (`dsl/platform/concepts.memql`). */
export const PACKAGE_CONCEPT = "v1:platform:package";

/** One deployment attempt -- the append-only timeline. */
export const DEPLOYMENT_CONCEPT = "v1:platform:packageDeployment";

export interface PackageRow {
  id: string;
  ownerUserId: string;
  name: string;
  sourceKind: string;
  repoUrl: string;
  repoRef: string;
  /** A v1:platform:sourceCredential id, or "" for a public repository. Never a value. */
  credentialId: string;
  artifactId: string;
  deployedVersion: string;
  latestKnownVersion: string;
  updateAvailable: boolean;
  /** Whether a push deploys itself when the plan has not changed (memql#4900). */
  autoDeploy: boolean;
  status: string;
  createdAt: string;
}

export function packageFromRow(row: Row): PackageRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    sourceKind: rowString(flat, "sourceKind"),
    repoUrl: rowString(flat, "repoUrl"),
    repoRef: rowString(flat, "repoRef"),
    credentialId: rowString(flat, "credentialId"),
    artifactId: rowString(flat, "artifactId"),
    deployedVersion: rowString(flat, "deployedVersion"),
    latestKnownVersion: rowString(flat, "latestKnownVersion"),
    updateAvailable: boolOr(flat, "updateAvailable", false),
    autoDeploy: boolOr(flat, "autoDeploy", false),
    status: rowString(flat, "status"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * WHAT A PERSON WOULD CALL A CHANGE, and nothing that moves on a timer.
 *
 * This is the arrival cue's whole contract (`clients/os/README.md`): anything
 * named here announces itself, so a liveness field would turn the list into a
 * strobe. There is no `lastSeenAt` on this concept to get wrong -- but
 * `latestKnownVersion` and `updateAvailable` ARE written by a poll on a ten
 * minute cadence, and they are named here deliberately anyway. The engine's
 * feed only writes them when the upstream actually moved
 * (component/packages/feeds.go), so a flip is news by construction: it means
 * somebody pushed to the repository this package tracks, which is exactly the
 * thing a person wants to be told about.
 */
export function packageFingerprint(p: PackageRow): string {
  return [
    p.name,
    p.sourceKind,
    p.repoUrl,
    p.repoRef,
    p.credentialId,
    p.deployedVersion,
    p.latestKnownVersion,
    p.updateAvailable ? "update" : "current",
    // A PERSON WOULD CALL ARMING THIS A CHANGE (memql#4900). It is the one
    // field here somebody else can flip -- a cluster owner, on a source they
    // share -- and the consequence is that pushes start deploying themselves.
    // Silence about that would be the wrong kind of quiet.
    p.autoDeploy ? "auto" : "manual",
    p.status,
  ].join(" ");
}

/**
 * What became of ONE app in a run -- one entry per manifest deployable, as
 * the concept promises, a skipped app included.
 *
 * THE THREE REFUSAL FIELDS ARE THREE DIFFERENT ANSWERS (memql#4887).
 * `refusal` is about the APP -- refused, or skipped as a kind this cluster
 * does not offer -- and carries no siteId. `accountRefusal` and
 * `domainRefusal` are about the optional PLACEMENT halves, applied after the
 * publish under the caller's own actor: the app IS live at its cluster
 * address and one of the two things asked for beside the address did not
 * land. Reading the three as one would report a deploy that succeeded as a
 * deploy that failed.
 */
export interface DeployableOutcome {
  name: string;
  siteId: string;
  hostname: string;
  bundleRef: string;
  version: string;
  created: boolean;
  refusal?: { code: string; message: string; scope?: string };
  /** The client tie that LANDED, when the placement named one. */
  accountId?: string;
  /** The client's own domain that LANDED, when the placement named one. */
  ownDomain?: string;
  accountRefusal?: { code: string; message: string; scope?: string };
  domainRefusal?: { code: string; message: string; scope?: string };
}

export interface DeploymentRow {
  id: string;
  packageId: string;
  sourceVersion: string;
  status: string;
  report: AnalysisReport | null;
  dslVersion: string;
  deployables: DeployableOutcome[];
  snapshotArtifactId: string;
  buildLogTail: string;
  /** Where this run built, and on which node (memql#4900). */
  builtOn: BuiltOn | null;
  error: { code: string; message: string; scope?: string } | null;
  requestedBy: string;
  /** True when the source's own switch started this run rather than a person. */
  automatic: boolean;
  /** The replica that ran the pipeline -- what the abandoned sentence names. */
  nodeId: string;
  /** The stage a lost run had reached, kept by the sweep before it closed the row. */
  stoppedAt: string;
  startedAt: string;
  finishedAt: string;
  /** When the running node last said it was alive. A HEARTBEAT: never in a fingerprint. */
  heartbeatAt: string;
  createdAt: string;
}

/** Where a run built. `surface` is one of the three the engine declares. */
export interface BuiltOn {
  surface: string;
  nodeId?: string;
}

export interface AnalysisReport {
  name?: string;
  formatVersion?: number;
  sourceVersion?: string;
  deployables?: ReportDeployable[];
  dslDomains?: ReportDomain[];
  goPacks?: ReportGoPack[];
  problems?: ReportProblem[];
  ok?: boolean;
}

export interface ReportDeployable {
  name: string;
  kind: string;
  path: string;
  buildPlan: string;
  command?: string;
  output: string;
  prebuilt: boolean;
  binding?: { storeDomain?: string; storefrontTokenRef?: string };
  problem?: ReportProblem;
}

export interface ReportDomain {
  domain: string;
  constructs: Record<string, number>;
  files: number;
  reserved?: boolean;
}

export interface ReportGoPack {
  path: string;
  module?: string;
  note: string;
}

export interface ReportProblem {
  code: string;
  message: string;
  scope?: string;
  fatal: boolean;
}

export function deploymentFromRow(row: Row): DeploymentRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    packageId: rowString(flat, "packageId"),
    sourceVersion: rowString(flat, "sourceVersion"),
    status: rowString(flat, "status"),
    report: objectOf<AnalysisReport>(flat, "report"),
    dslVersion: rowString(flat, "dslVersion"),
    deployables: listOf<DeployableOutcome>(flat, "deployables"),
    snapshotArtifactId: rowString(flat, "snapshotArtifactId"),
    buildLogTail: rowString(flat, "buildLogTail"),
    builtOn: objectOf<BuiltOn>(flat, "builtOn"),
    error: objectOf<{ code: string; message: string; scope?: string }>(flat, "error"),
    requestedBy: rowString(flat, "requestedBy"),
    automatic: boolOr(flat, "automatic", false),
    nodeId: rowString(flat, "nodeId"),
    stoppedAt: rowString(flat, "stoppedAt"),
    startedAt: rowString(flat, "startedAt"),
    finishedAt: rowString(flat, "finishedAt"),
    heartbeatAt: rowString(flat, "heartbeatAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * A deployment row is APPEND-ONLY past a terminal status, so the only thing
 * that ever changes about one is which stage it has reached. That IS the
 * change a person is watching, so `status` is the fingerprint and there is
 * nothing else worth naming.
 *
 * `heartbeatAt` is the field that makes this worth restating (memql#4900). A
 * running deploy now writes it every fifteen seconds, and every one of those
 * writes broadcasts the whole row. Naming it here would turn a deploy into a
 * strobe -- the exact "a heartbeat is not news" failure `clients/os/README.md`
 * describes -- and the cue would then fire hardest for the run somebody is
 * already watching move.
 */
export function deploymentFingerprint(d: DeploymentRow): string {
  return d.status;
}

/**
 * What the Build stop says a run's build ran ON, in the words the design
 * fixes. Empty when the run never reached the build stage, which reads as
 * nothing rather than as a guess.
 */
export function buildSurfaceLabel(d: DeploymentRow): string {
  switch (d.builtOn?.surface) {
    case "prebuilt":
      return "its built output is in the source";
    case "workbench":
      return "built in this cluster's sandbox";
    case "fleet":
      return "built on your own machine";
    default:
      return "";
  }
}

/** The set the engine declares, for the parity test. */
export const BUILD_SURFACES = ["prebuilt", "workbench", "fleet"] as const;

function objectOf<T>(row: Record<string, unknown>, key: string): T | null {
  const raw = row[key];
  if (raw === null || raw === undefined) return null;
  if (typeof raw === "object" && !Array.isArray(raw)) return raw as T;
  if (typeof raw === "string" && raw !== "") {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (parsed !== null && typeof parsed === "object") return parsed as T;
    } catch {
      // A field that is not JSON is a field this surface cannot render, and
      // rendering nothing is the honest answer. Never a thrown parse error:
      // one unreadable report must not blank a whole timeline.
    }
  }
  return null;
}

function listOf<T>(row: Record<string, unknown>, key: string): T[] {
  const raw = row[key];
  if (Array.isArray(raw)) return raw as T[];
  if (typeof raw === "string" && raw !== "") {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (Array.isArray(parsed)) return parsed as T[];
    } catch {
      // Same reasoning as objectOf.
    }
  }
  return [];
}

// ---------------------------------------------------------------------------
// Presentation
// ---------------------------------------------------------------------------

/** What this package's source IS, in the words a person used to add it. */
export function sourceLabel(p: Pick<PackageRow, "sourceKind" | "repoUrl" | "repoRef">): string {
  if (p.sourceKind === "repo") {
    const ref = p.repoRef === "" ? "default branch" : p.repoRef;
    return `${shortRepo(p.repoUrl)} at ${ref}`;
  }
  if (p.sourceKind === "artifact") return "uploaded zip";
  return "source unknown";
}

export function shortRepo(url: string): string {
  const trimmed = url
    .replace(/^https?:\/\//, "")
    .replace(/\.git$/, "")
    .replace(/\/$/, "");
  return trimmed.replace(/^github\.com\//, "");
}

/** A commit sha reads as its first seven characters; a tag reads as itself. */
export function shortVersion(v: string): string {
  const t = v.trim();
  if (t === "") return "";
  if (/^[0-9a-f]{16,}$/i.test(t)) return t.slice(0, 7);
  return t;
}
