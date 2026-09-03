import { renderMemQLValue, rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// Every write and every on-demand read this surface makes, in one place.
//
// The generated typed builders are used wherever they exist -- they are the
// point of sdk-gen. The two exceptions are the version WALK, which needs a
// caller-chosen `asOf` wrap the builders cannot express, and creating a
// package, whose id this surface mints.
//
// Quoting goes through renderMemQLValue and never through interpolation. That
// is the one rule every call-building path in this tree is held to, and it is
// not paranoia: a repository URL or a package name is text somebody typed.

export interface DeployOutcome {
  deploymentId: string;
  status: string;
  awaitingConfirm: boolean;
}

/** Mint an id for a row this surface is about to create. */
export function newPackageId(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `v1:platform:package:${hex}`;
}

export interface NewPackageInput {
  name: string;
  sourceKind: "repo" | "artifact";
  repoUrl: string;
  repoRef: string;
  /** A v1:platform:sourceCredential id. This surface never handles a token value. */
  credentialId: string;
  artifactId: string;
}

export async function createPackage(query: QueryClient, input: NewPackageInput): Promise<string> {
  const packageId = newPackageId();
  const parts = [
    `packageId: ${renderMemQLValue(packageId)}`,
    `name: ${renderMemQLValue(input.name)}`,
    `sourceKind: ${renderMemQLValue(input.sourceKind)}`,
  ];
  if (input.repoUrl !== "") parts.push(`repoUrl: ${renderMemQLValue(input.repoUrl)}`);
  if (input.repoRef !== "") parts.push(`repoRef: ${renderMemQLValue(input.repoRef)}`);
  if (input.credentialId !== "") parts.push(`credentialId: ${renderMemQLValue(input.credentialId)}`);
  if (input.artifactId !== "") parts.push(`artifactId: ${renderMemQLValue(input.artifactId)}`);
  await query.executeNamed("createPackage", `mutation createPackage(${parts.join(", ")})`);
  return packageId;
}

/**
 * WHERE ONE APP GOES, on its first deploy and never again (design D8).
 *
 * The three halves are applied by the pipeline itself once the site exists:
 * the hostname on `EnsureSite`, then `updateSiteAccount` and
 * `customDomainAdd` under the caller's own actor -- the same two calls the
 * page makes -- so the guards that already run decide, and a refused half
 * lands on the outcome without failing the publish.
 */
export interface Placement {
  /** The site's own hostname under the cluster domain. Required for a never-deployed app. */
  hostname: string;
  /** The client it is for. "" ties it to nobody. */
  accountId: string;
  /** The client's own domain. "" binds none. */
  ownDomain: string;
}

/**
 * The wire form of a placement set: blank halves are OMITTED rather than sent.
 *
 * An explicit "" is a VALUE the pipeline reads -- an empty accountId asks for
 * a tie to nothing -- so a half nobody answered must be absent, the same rule
 * `createSite`'s omitBlank keeps. An entry with nothing in it is dropped
 * whole, so a package whose apps all already have addresses sends no
 * `placements` argument at all.
 */
export function placementsPayload(placements: Record<string, Placement>): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  for (const [app, placement] of Object.entries(placements)) {
    const entry: Record<string, string> = {};
    if (placement.hostname.trim() !== "") entry["hostname"] = placement.hostname.trim();
    if (placement.accountId.trim() !== "") entry["accountId"] = placement.accountId.trim();
    if (placement.ownDomain.trim() !== "") entry["ownDomain"] = placement.ownDomain.trim();
    if (Object.keys(entry).length > 0) out[app] = entry;
  }
  return out;
}

/**
 * Start or continue a deployment.
 *
 * `confirm: false` is the always-present gate: the run parks with its report
 * and nothing is built. The SAME call with `confirm: true` is what a person's
 * click sends, which is why there is one function rather than two -- a
 * redeploy is not a different operation, it is this one with the answer
 * already in hand.
 */
export async function deployPackage(
  query: QueryClient,
  packageId: string,
  opts: { confirm: boolean; placements?: Record<string, Placement>; fromDeploymentId?: string },
): Promise<DeployOutcome> {
  const placements = opts.placements === undefined ? {} : placementsPayload(opts.placements);
  const result = await query.packageDeploy({
    packageId,
    confirm: opts.confirm,
    ...(Object.keys(placements).length > 0 ? { placements } : {}),
    // Retrying a run that was lost deploys the bytes IT fetched (memql#4900),
    // not whatever the branch has moved to since. Sent only when there is a
    // run to retry, because an empty value would ask the engine to look up
    // nothing.
    ...(opts.fromDeploymentId ? { fromDeploymentId: opts.fromDeploymentId } : {}),
  });
  const row = result.rows()[0];
  return {
    deploymentId: row ? rowString(row, "deploymentId") : "",
    status: row ? rowString(row, "status") : "",
    awaitingConfirm: row ? rowString(row, "awaitingConfirm") === "true" : false,
  };
}

/** Switch which of the caller's credentials a tracked source fetches under. */
export async function setPackageCredential(query: QueryClient, packageId: string, credentialId: string): Promise<void> {
  await query.updatePackageSource({ packageId, credentialId });
}

// ---------------------------------------------------------------------------
// Personal source credentials
// ---------------------------------------------------------------------------

export interface NewCredential {
  credentialId: string;
  fingerprint: string;
}

/**
 * Seal a token in the cluster and answer its card.
 *
 * THE TOKEN CROSSES THE WIRE HERE AND NOWHERE ELSE (design G). It is a
 * parameter of this one function, is never stored on this surface, and no
 * other call in this file takes one -- a package names a credential ID.
 */
export async function createSourceCredential(
  query: QueryClient,
  input: { host: string; label: string; token: string },
): Promise<NewCredential> {
  const result = await query.sourceCredentialCreate(input);
  const row = result.rows()[0];
  return {
    credentialId: row ? rowString(row, "credentialId") : "",
    fingerprint: row ? rowString(row, "fingerprint") : "",
  };
}

export async function revokeSourceCredential(query: QueryClient, credentialId: string): Promise<void> {
  await query.sourceCredentialRevoke({ credentialId });
}

export async function rollbackPackage(query: QueryClient, packageId: string, deploymentId: string): Promise<void> {
  await query.packageRollback({ packageId, deploymentId });
}

export async function archivePackage(query: QueryClient, packageId: string, confirmName: string): Promise<void> {
  await query.packageArchive({ packageId, confirmName });
}

export async function restorePackage(query: QueryClient, packageId: string): Promise<void> {
  await query.packageRestore({ packageId });
}

/**
 * Arm or disarm a source's auto-deploy (memql#4900).
 *
 * The write is owned server-side, so this makes no role check of its own: the
 * guard admits the source's owner or a cluster owner, and a refusal renders
 * beside the switch that produced it.
 */
export async function setPackageAutoDeploy(query: QueryClient, packageId: string, autoDeploy: boolean): Promise<void> {
  await query.packageSetAutoDeploy({ packageId, autoDeploy });
}

export async function archiveSite(query: QueryClient, siteId: string, confirmHostname: string): Promise<void> {
  await query.siteArchive({ siteId, confirmHostname });
}

export async function restoreSite(query: QueryClient, siteId: string): Promise<void> {
  await query.siteRestore({ siteId });
}

export async function setSiteLive(query: QueryClient, siteId: string, status: "live" | "disabled" | "draft"): Promise<void> {
  await query.executeNamed(
    "updateSiteStatus",
    `mutation updateSiteStatus(siteId: ${renderMemQLValue(siteId)}, status: ${renderMemQLValue(status)})`,
  );
}

/**
 * Replace a deployable's runtime settings (epic memql#4906).
 *
 * THE WHOLE MAP, because the mutation replaces rather than merges -- which is
 * what makes removing a setting expressible at all. Through the generated
 * builder, which renders the object argument the engine parses; a hand-built
 * one would be a second spelling of a nested literal, and a nested object in
 * a call string is exactly where hand-building goes wrong.
 */
export async function saveSiteSettings(
  query: QueryClient,
  siteId: string,
  settings: Record<string, string>,
): Promise<void> {
  await query.updateSiteSettings({ siteId, settings });
}

// ---------------------------------------------------------------------------
// The version walk
// ---------------------------------------------------------------------------

export interface SiteVersion {
  bundleRef: string;
  status: string;
  createdAt: string;
  artifactId: string;
}

/** How far back the picker looks. Each entry is a round trip. */
export const MAX_HISTORY_VERSIONS = 6;

/**
 * A deployable's version list IS its row history, walked.
 *
 * PORTED FROM THE PORTAL (clients/portal/src/deployables/calls.ts), not
 * reinvented, and the reasoning it carries is the reason this is a walk at
 * all: there is no all-versions query and no builtin that would be prior art
 * for one, because "the graph's own history is the version list" (memql#2880).
 * `siteById` deliberately declares no `asOf` clause of its own precisely so a
 * caller can wrap it -- a query that declares `asOf args.asOf ?? latest`
 * refuses to be wrapped a second time.
 *
 * The wrap sends the BARE call form with no `query` keyword, because that
 * argument is parsed as an EXPRESSION and `query` is a top-level dispatch
 * keyword with no place inside one. That is also why the generated builder
 * cannot be used here: it always prepends the kind keyword.
 */
export async function fetchSiteVersions(
  query: QueryClient,
  siteId: string,
  limit: number = MAX_HISTORY_VERSIONS,
): Promise<SiteVersion[]> {
  const versions: SiteVersion[] = [];
  if (siteId === "" || limit <= 0) return versions;

  const current = await query.siteById({ siteId });
  const first = current.rows()[0];
  if (first === undefined) return versions;
  versions.push(toVersion(first));
  let cursor = rowString(first, "createdAt");

  while (versions.length < limit && cursor !== "") {
    const at = justBefore(cursor);
    if (at === "") break;
    const call = `asOf(siteById(siteId: ${renderMemQLValue(siteId)}), ${renderMemQLValue(at)})`;
    const result = await query.executeNamed("siteById (asOf)", call);
    const row = result.rows()[0];
    if (row === undefined) break;
    const createdAt = rowString(row, "createdAt");
    // A createdAt that did not strictly decrease stops the walk. It should
    // never fire against a real engine, since createdAt is server-stamped and
    // monotonic per id -- but a clock anomaly must not become a loop.
    if (createdAt === "" || createdAt >= cursor) break;
    versions.push(toVersion(row));
    cursor = createdAt;
  }
  return versions;
}

export async function repointSite(query: QueryClient, siteId: string, bundleRef: string): Promise<void> {
  await query.executeNamed(
    "updateSiteBundle",
    `mutation updateSiteBundle(siteId: ${renderMemQLValue(siteId)}, bundleRef: ${renderMemQLValue(bundleRef)})`,
  );
}

function toVersion(row: Row): SiteVersion {
  return {
    bundleRef: rowString(row, "bundleRef"),
    status: rowString(row, "status"),
    createdAt: rowString(row, "createdAt"),
    artifactId: rowString(row, "artifactId"),
  };
}

/**
 * justBefore backs an instant off by one millisecond, so re-issuing the read
 * `asOf` that instant scans strictly BEFORE the row it came from instead of
 * returning the same row again.
 */
export function justBefore(iso: string): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return "";
  return new Date(ms - 1).toISOString();
}
