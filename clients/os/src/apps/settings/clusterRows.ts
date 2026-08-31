// Projections for the Cluster facts panel (memql#4742). PURE -- a function
// of a wire row, unit-testable with no browser and no cluster, for the
// reason `apps/fleet/rows.ts` states: a projection asserted through render()
// is asserted through three layers that can each fail for unrelated reasons.

import {
  rowArray,
  rowNumber,
  rowObject,
  rowString,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../fleet/rows";

export interface ClusterRow {
  id: string;
  name: string;
  region: string;
  status: string;
  version: string;
  provider: string;
}

export function clusterFromRow(raw: Row): ClusterRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id") ?? "",
    name: rowString(row, "name") ?? "",
    region: rowString(row, "region") ?? "",
    status: rowString(row, "status") ?? "",
    version: rowString(row, "version") ?? "",
    provider: rowString(row, "provider") ?? "",
  };
}

export interface DeploymentRow {
  id: string;
  deploymentId: string;
  status: string;
  /** The deployment's engine version -- the SPINE a node spec resolves against. */
  version: string;
  provider: string;
  region: string;
  createdAt: string;
}

export function deploymentFromRow(raw: Row): DeploymentRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id") ?? "",
    deploymentId: rowString(row, "deploymentId") ?? "",
    status: rowString(row, "status") ?? "",
    version: rowString(row, "version") ?? "",
    provider: rowString(row, "provider") ?? "",
    region: rowString(row, "region") ?? "",
    createdAt: rowString(row, "createdAt") ?? "",
  };
}

/**
 * Newest first by `createdAt`. `deploymentsForCluster` declares no sort --
 * its own comment says ordering is the consumer's job -- so a panel that
 * rendered the reply order would show whichever deployment the planner
 * happened to return first and call it current.
 */
export function latestDeployment(rows: readonly DeploymentRow[]): DeploymentRow | null {
  let newest: DeploymentRow | null = null;
  for (const row of rows) {
    if (row.deploymentId === "") continue;
    if (newest === null || row.createdAt > newest.createdAt) newest = row;
  }
  return newest;
}

export interface NodeSpecRow {
  nodeType: string;
  /** As stored: EMPTY means engine-as-spine. Resolve with `resolvedVersion`. */
  version: string;
  replicas: number;
  imageDigest: string;
}

export function nodeSpecFromRow(raw: Row): NodeSpecRow {
  const row = flatten(raw);
  return {
    nodeType: rowString(row, "nodeType") ?? "",
    version: rowString(row, "version") ?? "",
    replicas: rowNumber(row, "replicas") ?? 0,
    imageDigest: rowString(row, "imageDigest") ?? "",
  };
}

/**
 * Engine-as-spine, which the engine will never do for you: an empty spec
 * version means "the deployment's engine version", and the query's own
 * comment names the consumer as the party responsible. Rendering the raw
 * value shows "no version" for the NORMAL case -- most node types are
 * unpinned -- which reads as a broken deployment.
 */
export function resolvedVersion(spec: NodeSpecRow, deploymentVersion: string): string {
  return spec.version.trim() !== "" ? spec.version : deploymentVersion;
}

/** True when the node type rides the spine rather than its own pin. */
export function ridesTheSpine(spec: NodeSpecRow): boolean {
  return spec.version.trim() === "";
}

export interface ProviderStatusRow {
  name: string;
  vendor: string;
  model: string;
  available: boolean;
  authSource: string;
  reason: string;
}

export function providerFromRow(raw: Row): ProviderStatusRow {
  const row = flatten(raw);
  return {
    name: rowString(row, "name") ?? "",
    vendor: rowString(row, "vendor") ?? "",
    model: rowString(row, "model") ?? "",
    available: rowBool(row, "available"),
    authSource: rowString(row, "authSource") ?? "",
    reason: rowString(row, "reason") ?? "",
  };
}

/**
 * The wire can carry a boolean as the string "true" (the portal's own
 * reader absorbs both), and a panel that only accepted the boolean would
 * render every provider unavailable on the day that changed.
 */
function rowBool(row: Row, key: string): boolean {
  const value = row[key];
  if (typeof value === "boolean") return value;
  return value === "true";
}

export function stringList(row: Row, key: string): string[] {
  return (rowArray(row, key) ?? []).filter((entry): entry is string => typeof entry === "string");
}

// ---------------------------------------------------------------------------
// The cluster's infrastructure rows (memql#4766)
// ---------------------------------------------------------------------------
//
// These two carried fields that were declared and written by nothing until
// memql#4766, which is why this panel could not render them and said so. They
// have writers now: `engineVersion` / `extensions` / `extensionVersions` are
// probed from the live connection at startup, and `jwksUrl` /
// `acceptedAudiences` were always computed and are now forwarded.
//
// The `status` fields the brief originally wanted are NOT here because they no
// longer exist. `database.status` was removed as structurally unanswerable --
// the row lives in the database it describes, so a successful read can only
// ever say "healthy" -- and `identityProvider.status` / `lastVerifiedAt` were
// removed for want of an honest writer. Do not reintroduce a health line off
// either concept.

export interface DatabaseRow {
  id: string;
  host: string;
  port: number;
  dbName: string;
  engine: string;
  engineVersion: string;
  extensions: readonly string[];
  extensionVersions: Readonly<Record<string, string>>;
  sslMode: string;
}

export function databaseFromRow(raw: Row): DatabaseRow {
  const row = flatten(raw);
  const versions = rowObject(row, "extensionVersions") ?? {};
  const mapped: Record<string, string> = {};
  for (const [name, version] of Object.entries(versions)) {
    if (typeof version === "string") mapped[name] = version;
  }
  return {
    id: rowString(row, "id") ?? "",
    host: rowString(row, "host") ?? "",
    port: rowNumber(row, "port") ?? 0,
    dbName: rowString(row, "dbName") ?? "",
    engine: rowString(row, "engine") ?? "",
    engineVersion: rowString(row, "engineVersion") ?? "",
    extensions: (rowArray(row, "extensions") ?? []).filter(
      (v): v is string => typeof v === "string",
    ),
    extensionVersions: mapped,
    sslMode: rowString(row, "sslMode") ?? "",
  };
}

export interface IdentityProviderRow {
  id: string;
  name: string;
  providerType: string;
  issuerUrl: string;
  jwksUrl: string;
  acceptedAudiences: readonly string[];
  redirectUrl: string;
  /** The first eight characters of the OAuth client id. Documented non-secret;
   *  it identifies the client without being one. */
  clientIdPrefix: string;
}

export function identityProviderFromRow(raw: Row): IdentityProviderRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id") ?? "",
    name: rowString(row, "name") ?? "",
    providerType: rowString(row, "providerType") ?? "",
    issuerUrl: rowString(row, "issuerUrl") ?? "",
    jwksUrl: rowString(row, "jwksUrl") ?? "",
    acceptedAudiences: (rowArray(row, "acceptedAudiences") ?? []).filter(
      (v): v is string => typeof v === "string",
    ),
    redirectUrl: rowString(row, "redirectUrl") ?? "",
    clientIdPrefix: rowString(row, "clientIdPrefix") ?? "",
  };
}
