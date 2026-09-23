import type { ReactNode } from "react";
import { act, fireEvent } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import { installSeededAccess } from "../seededAccess";
import { setEffectiveCapabilities, type EffectiveCapability } from "../../src/system/roles";

// The Deployables app's test harness.
//
// ===========================================================================
// THE FAKE SITS UNDER `executeNamed`, NOT OVER THE GENERATED METHODS
// ===========================================================================
// A double that stubs `query.createSite` records the ARGUMENTS and never
// renders the call, so the generated builder -- the thing that turns those
// arguments into MemQL text the engine has to parse -- runs in production and
// nowhere else. That is how a feature ships green and fails at parse on every
// call.
//
// So the stub is given `QueryClient.prototype` and answers at `executeNamed`,
// which is what every generated method funnels through. `query.createSite({...})`
// therefore runs the real `buildCreateSite` and the test asserts the STRING
// that reaches the wire.
//
// Everything else is connection-shaped for the reason the Fleet's and Users'
// harnesses are: every read goes through `connection.query`, every subscription
// through `connection.subscriptions`, so a fake answering those two exercises
// the real LiveCollection, the real retain/seed path and the real projections.

/** The `data` envelope a SHAPE-PROJECTED query returns. */
export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

/**
 * What a top-level `builtin X(...)` answers ON THE WIRE: one data value keyed
 * by node id, each value the node envelope with the handler's fields under
 * `payload`, stamped with DECREASING createdAt in slice order the way the
 * engine's PreserveOrder path does (executor_builtin.go). A fake answering
 * flat rows here passed every test against a shape the engine never sends --
 * which is how the Logs app and Connect GitHub shipped reading nothing.
 */
export function builtinReply(name: string, rows: Row[]): Result {
  const wrapper: Record<string, unknown> = {};
  rows.forEach((row, index) => {
    const own = typeof row["id"] === "string" && row["id"] !== "" ? (row["id"] as string) : name;
    const id = own in wrapper ? `${own}-${index}` : own;
    wrapper[id] = {
      id,
      concept: `integration:test:${name}`,
      type: "object",
      createdAt: `2026-01-01T00:00:00.${String(999_999_999 - index).padStart(9, "0")}Z`,
      payload: row,
    };
  });
  return new Result({ data: rows.length === 0 ? [] : [wrapper] } as never);
}

/**
 * A BUNDLE envelope -- a different wire shape, and not interchangeable.
 *
 * `getRowByConceptAndId` reads `rawNodes()`, which looks at
 * `payload.bundle.nodes` and nothing else, so a `data` envelope makes a by-id
 * re-read answer null. That failure is silent by design (a null re-read means
 * "keep what you have"), so the wrong envelope would make a re-read look like
 * it was correctly falling back when in fact it never read anything.
 */
export function bundleResult(rows: Row[]): Result {
  const nodes = rows.map((row) => {
    const { id, createdAt, ...fields } = row as Record<string, unknown>;
    return { id, createdAt, payload: fields };
  });
  return new Result({ bundle: { nodes } } as never);
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  /** The test's hand on the wire. */
  emit: (concept: string, payload: Row, kind?: string) => void;
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler, opts) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const handler of handlers.get(concept) ?? []) {
        handler({
          subscriptionId: "sub-1",
          kind,
          timestamp: new Date(),
          payload,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
        });
      }
    },
  };
}

export interface FakeSeed {
  accounts?: Row[];
  sites?: Row[];
  sitesError?: string;
  /** The v1:shopify:store rows `stores()` and `storeById()` answer with. */
  stores?: Row[];
  storesError?: string;
  /** The `shopifyStoreHealth` report, one entry per store. */
  storeHealth?: Row[];
  storeHealthError?: string;
  createStoreError?: string;
  bindStoreError?: string;
  setStoreStatusError?: string;
  ensureSubscriptionsError?: string;
  artifacts?: Row[];
  /** v1:platform:customDomain rows the domains feed seeds with. */
  domains?: Row[];
  domainDNSError?: string;
  /** Fails the next `customDomainAdd` with this server message. */
  addDomainError?: string;
  /** Fails the next `removeCustomDomain` with this server message. */
  removeDomainError?: string;
  /**
   * The `sitePreviewReadiness` answer (epic memql#5531), keyed by site id.
   *
   * ABSENT IS NOT "NO". A site with no entry gets a readiness whose booleans
   * are all false because the ENGINE said so, which is what a deployable with
   * no candidate and no development store actually resolves to -- so the
   * default fixture is the honest empty state rather than an error.
   */
  previewReadiness?: Record<string, Row>;
  previewReadinessError?: string;
  /** `sitePreviewObservationsForSite` rows, newest first, keyed by site id. */
  previewObservations?: Record<string, Row[]>;
  previewObservationsError?: string;
  /** `sitePreviewGrantsForSite` rows, newest first, keyed by site id. */
  previewGrants?: Record<string, Row[]>;
  /** What `sitePreviewOpen` answers, or the refusal it throws. */
  previewOpenResult?: Row;
  previewOpenError?: string;
  previewProbeError?: string;
  setCandidateError?: string;
  promoteError?: string;
  /** v1:platform:package rows `packagesAll` answers with. */
  packages?: Row[];
  /** v1:platform:packageDeployment rows, keyed by packageId. */
  deployments?: Record<string, Row[]>;
  /** Fails the next `packageDeploy` with this server message. */
  deployError?: string;
  /** Fails the next `packageArchive` with this server message. */
  archiveError?: string;
  /** Fails the next `packageDeactivateDeployable` with this server message. */
  deactivateError?: string;
  enableDeployablesError?: string;
  disableDeployablesError?: string;
  siteStatusErrors?: Record<string, string>;
  /**
   * Hostnames `siteHostnameCheck` / `customDomainCheck` answer TAKEN for
   * (2026-09-05, D7). Everything else answers available -- the fake mirrors
   * the engine's shape: one row, {hostname, available, reason, problem}.
   */
  takenHostnames?: string[];
  /** Fails the next address check with this server message. */
  hostnameCheckError?: string;
  /** What `packageDeploy` answers with when it succeeds. */
  deployResult?: Row;
  /** Rows a by-id re-read answers with, keyed by row id. */
  byId?: Record<string, Row>;
  /** Fails the next `createSite` with this server message. */
  createError?: string;
  /** Fails the next `sitePublishFromArtifact` with this server message. */
  publishError?: string;
  /** v1:platform:sourceCredential CARDS `sourceCredentialsMine` answers with -- never a value. */
  credentials?: Row[];
  /** Explicit source bindings; [] means none even with connected identities. */
  sourceConnections?: Row[];
  sourceConnectionsError?: string;
  /** Installation readings by credential ID; absent derives only from fixture repositories. */
  sourceInstallations?: Record<string, Row>;
  sourceInstallationsError?: string;
  sourceConnectionCreateError?: string;
  sourceConnectionRemoveError?: string;
  /** v1:identity:user rows `searchUsers` answers with, for "deployed by" (epic memql#5289). */
  people?: Row[];
  /** Fails the roster read with this server message -- what a reader gets. */
  peopleError?: string;
  /**
   * What `sourceProbe` answers, keyed by the credentialId the call carries
   * ("" for an anonymous probe). A key that is not present falls back to
   * `""`, so a test that cares about only one answer names only that one.
   */
  sourceProbe?: Record<string, Row>;
  /** Fails the next `sourceProbe` with this server message -- the probe that could not RUN. */
  sourceProbeError?: string;
  /** What `artifactProbe` answers, keyed by artifactId. */
  artifactProbe?: Record<string, Row>;
  /** Fails the next `artifactProbe` with this server message. */
  artifactProbeError?: string;
  /** What `sourceCredentialCreate` answers with. The token NEVER comes back. */
  credentialCreated?: Row;
  /** Fails the next `sourceCredentialCreate` with this server message. */
  credentialCreateError?: string;
  /** Fails the next `sourceCredentialRevoke` with this server message. */
  credentialRevokeError?: string;
  /** Whether `sourceCredentialRevoke` also ended the authorization at GitHub.
   *  Defaults to true, which is what a grant answers when both halves
   *  happened and what a pasted token's revoke is never asked about. */
  credentialRevokeRemote?: boolean;
  /** Fails the next `updatePackageSource` with this server message. */
  updateSourceError?: string;
  /**
   * v1:platform:packageDeployment rows at `awaiting_confirm`, which
   * `packageDeploymentsAwaitingConfirm` answers with: the list's fourth feed,
   * for the waiting mark (epic memql#4885, design section A).
   */
  awaitingConfirm?: Row[];
  /**
   * What `siteTrafficInWindow` answers with, keyed by the mode the call is
   * in: "series" for a stop's per-bucket read, "summary" for the list's
   * one-row-per-deployable read.
   *
   * ABSENT MEANS UNMEASURED, which is exactly what the server does -- it
   * sends no row for a window it measured nothing in -- so a seed that says
   * nothing about traffic exercises the unmeasured path rather than a
   * zero-filled one.
   */
  traffic?: { series?: Row[]; summary?: Row[] };
  /** Fails the next `siteTrafficInWindow` with this server message. */
  trafficError?: string;
  /** Fails the next `updateSiteSettings` with this server message. */
  settingsError?: string;
  /** Fails the next `updateSiteStatus` with this server message. */
  siteStatusError?: string;
  /** Fails the next `siteArchive` with this server message. */
  siteArchiveError?: string;
  /** Fails the next `siteRestore` with this server message. */
  siteRestoreError?: string;
  /** Fails the next `updateSiteBundle` (a roll back to a version) with this server message. */
  repointError?: string;
  /** Fails the next `packageRollback` with this server message. */
  rollbackError?: string;
  /** Fails the next `packageRestore` with this server message. */
  restoreError?: string;
  /**
   * A site's own row history, NEWEST FIRST, for the version walk. `siteById`
   * answers the first; each `asOf(siteById(...), <t>)` answers the newest
   * row written at or before `t`, which is exactly the read the walk makes.
   */
  siteHistory?: Row[];

  // -- GitHub Connect (epic memql#4915) --

  /** The whole `sourceRepositories` reply row: repositories, installations,
   *  pending, nextPage, reason. Absent answers no rows at all, which is the
   *  shape a picker sees before anybody has pressed anything. */
  repositories?: Row;
  /** Fails the next `sourceRepositories` with this server message. */
  repositoriesError?: string;
  /** What `githubConnectBegin` answers as the URL to navigate to. */
  connectUrl?: string;
  /** What `githubConnectBegin` answers as its reason. Defaults to `ok`. */
  connectReason?: string;
  /** What `githubConnectBegin` answers as the app's installation page. */
  installUrl?: string;
  /** Fails the next `githubConnectBegin` with this server message. */
  connectError?: string;
  /**
   * What `githubAppStatus` answers. ABSENT ANSWERS NO ROW, which a surface reads
   * as "not known" and treats as a cluster that has an app -- so every test
   * written before the status existed sees exactly what it saw.
   */
  githubApp?: { configured: boolean; source?: string; slug?: string; installUrl?: string; canSetup?: boolean };
  /** Fails `githubAppStatus` with this server message. */
  githubAppError?: string;
  /** What `githubAppSetupBegin` answers as the page to navigate to. */
  appSetupUrl?: string;
  /** What `githubAppSetupBegin` answers as its reason. Defaults to `ok`. */
  appSetupReason?: string;
  /** Fails the next `githubAppSetupBegin` with this server message. */
  appSetupError?: string;
  /** What `githubAppRemove` answers as its reason. Defaults to `ok`, removed. */
  appRemoveReason?: string;
}

export interface FakeConnection {
  query: QueryClient;
  /** Every call string that reached the wire, in order. */
  calls: string[];
  callsNamed: (construct: string) => string[];
  subscriptions: FakeSubscriptions;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
}

export const PUBLISH_RESULT = {
  siteId: "site-shop",
  artifactId: "artifact-zip",
  fileId: "file-1",
  version: "v7f3c19a2bb01",
  bundleRef: "blob://sites/site-shop/v7f3c19a2bb01/",
  fileCount: 12,
  totalBytes: 2097152,
};

/** What `customDomainAdd` answers with -- the token is minted server-side. */
export const ADD_DOMAIN_RESULT = {
  domainId: "cd-new",
  siteId: "site-shop",
  hostname: "www.acme.com",
  accountId: "",
  token: "tok-minted-server-side",
  status: "pending_dns",
  verifyRecordName: "_memql-verify.www.acme.com",
  pointsToKind: "CNAME",
  pointsToTarget: "os.memql.example.com",
};

export function fakeConnection(seed: FakeSeed = {}): FakeConnection {
  const calls: string[] = [];
  const sites = seed.sites ?? [];
  const artifacts = seed.artifacts ?? [];
  const domains = seed.domains ?? [];
  const installationRows = (credentialId: string): Row[] => {
    const explicit = seed.sourceInstallations?.[credentialId]?.["installations"];
    if (Array.isArray(explicit)) return explicit as Row[];
    const declared = seed.repositories?.["installations"];
    if (Array.isArray(declared) && declared.length > 0) return declared as Row[];
    const repos = seed.repositories?.["repositories"];
    const derived = new Map<string, Row>();
    for (const repo of Array.isArray(repos) ? repos as Row[] : []) {
      const id = String(repo["installationId"] ?? "");
      if (id) derived.set(id, { id, account: repo["owner"], accountType: "Organization", accountId: `provider-${id}`, repositorySelection: "selected", suspended: false });
    }
    return [...derived.values()];
  };
  const sourceConnections = seed.sourceConnections?.map(row => ({ ...row })) ?? (seed.credentials ?? []).flatMap(grant =>
    grant["kind"] !== "github_app" ? [] : installationRows(String(grant["id"])).map(installation => ({
      id: `source-${grant["id"]}-${installation["id"]}`, ownerUserId: grant["ownerUserId"], credentialId: grant["id"],
      installationId: installation["id"], providerAccountId: installation["accountId"] ?? "", accountLogin: installation["account"] ?? installation["login"] ?? "",
      accountType: installation["accountType"], status: "active",
    }) as Row));

  const stub = {
    executeNamed: vi.fn(async (_name: string, call: string) => {
      calls.push(call);

      if (call.startsWith("query clientAccountsAll(")) return rowsResult(seed.accounts ?? [{ id: "self", name: "Operator organization", status: "active" }]);
      if (call === "query sitesAll()") { if (seed.sitesError) throw new Error(seed.sitesError); return rowsResult(sites); }
      if (call.startsWith("query searchUsers(")) {
        if (seed.peopleError !== undefined) throw new Error(seed.peopleError);
        return rowsResult(seed.people ?? []);
      }
      if (call === "query libraryArtifacts()") return rowsResult(artifacts);
      if (call === "query customDomainsAll()") return rowsResult(domains);

      if (call.startsWith("builtin customDomainDNSGuidance(")) {
        if (seed.domainDNSError) throw new Error(seed.domainDNSError);
        return builtinReply("customDomainDNSGuidance", [{ edgeHost: "routing.example.net", ipv4: ["203.0.113.10"], ipv6: [] }]);
      }

      if (call.startsWith("builtin customDomainAdd(")) {
        if (seed.addDomainError !== undefined) throw new Error(seed.addDomainError);
        return builtinReply("customDomainAdd", [ADD_DOMAIN_RESULT as unknown as Row]);
      }

      if (call.startsWith("mutation removeCustomDomain(")) {
        if (seed.removeDomainError !== undefined) throw new Error(seed.removeDomainError);
        return rowsResult([]);
      }

      if (call === "query packagesAll()") return rowsResult(seed.packages ?? []);
      if (call === "query packageDeploymentsAwaitingConfirm()") return rowsResult(seed.awaitingConfirm ?? []);

      if (call.startsWith("query packageDeployments(")) {
        const id = /packageId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(seed.deployments?.[id] ?? []);
      }

      if (call.startsWith("builtin packageDeploy(")) {
        if (seed.deployError !== undefined) throw new Error(seed.deployError);
        return builtinReply("packageDeploy", [
          seed.deployResult ??
            ({ deploymentId: "dep-new", status: "awaiting_confirm", awaitingConfirm: "true" } as unknown as Row),
        ]);
      }

      if (call.startsWith("builtin siteTrafficInWindow(")) {
        if (seed.trafficError !== undefined) throw new Error(seed.trafficError);
        // The MODE is read off the rendered call string rather than from a
        // separate stub, so the generated builder is what decides which
        // fixture answers -- the same reason this fake sits under
        // executeNamed at all.
        const summary = call.includes("summary: true");
        return builtinReply("siteTrafficInWindow", (summary ? seed.traffic?.summary : seed.traffic?.series) ?? []);
      }

      if (call.startsWith("mutation updateSiteSettings(")) {
        if (seed.settingsError !== undefined) throw new Error(seed.settingsError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin packageArchive(")) {
        if (seed.archiveError !== undefined) throw new Error(seed.archiveError);
        return builtinReply("packageArchive", []);
      }

      if (call.startsWith("builtin packageDeactivateDeployable(")) {
        if (seed.deactivateError !== undefined) throw new Error(seed.deactivateError);
        return builtinReply("packageDeactivateDeployable", []);
      }

      if (call.startsWith("builtin siteHostnameCheck(") || call.startsWith("builtin customDomainCheck(")) {
        if (seed.hostnameCheckError !== undefined) throw new Error(seed.hostnameCheckError);
        const hostname = /hostname: "([^"]*)"/.exec(call)?.[1] ?? "";
        const taken = (seed.takenHostnames ?? []).includes(hostname);
        const name = call.startsWith("builtin siteHostnameCheck(") ? "siteHostnameCheck" : "customDomainCheck";
        return builtinReply(name, [
          {
            id: hostname,
            hostname,
            available: !taken,
            reason: taken ? "taken" : "ok",
            problem: taken ? `${hostname} is already taken by another deployable in this cluster. Pick another name.` : "",
          } as unknown as Row,
        ]);
      }

      if (call.startsWith("builtin packageRollback(")) {
        if (seed.rollbackError !== undefined) throw new Error(seed.rollbackError);
        return builtinReply("packageRollback", []);
      }

      if (call.startsWith("builtin packageRestore(")) {
        if (seed.restoreError !== undefined) throw new Error(seed.restoreError);
        return builtinReply("packageRestore", []);
      }

      // THE PREVIEW READS AND WRITES (epic memql#5531). The readiness is a
      // BUILTIN and the two lists are SHAPED QUERIES, and the fake answers
      // each in its own wire shape for the reason the store arm below gives:
      // a fake that answered flat rows to a builtin would pass every test
      // against a shape the engine never sends.
      if (call.startsWith("builtin sitePreviewReadiness(")) {
        if (seed.previewReadinessError !== undefined) throw new Error(seed.previewReadinessError);
        const id = /siteId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const row = seed.previewReadiness?.[id] ?? previewReadinessRow({ siteId: id });
        return builtinReply("sitePreviewReadiness", [row]);
      }
      if (call.startsWith("builtin sitePreviewOpen(")) {
        if (seed.previewOpenError !== undefined) throw new Error(seed.previewOpenError);
        return builtinReply("sitePreviewOpen", [seed.previewOpenResult ?? OPENED_PREVIEW]);
      }
      if (call.startsWith("builtin sitePreviewProbe(")) {
        if (seed.previewProbeError !== undefined) throw new Error(seed.previewProbeError);
        return builtinReply("sitePreviewProbe", [{ id: "probe", recorded: 3 } as unknown as Row]);
      }
      if (call.startsWith("query sitePreviewObservationsForSite(")) {
        if (seed.previewObservationsError !== undefined) throw new Error(seed.previewObservationsError);
        const id = /siteId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(seed.previewObservations?.[id] ?? []);
      }
      if (call.startsWith("query sitePreviewGrantsForSite(")) {
        const id = /siteId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(seed.previewGrants?.[id] ?? []);
      }
      if (call.startsWith("mutation setSiteCandidate(") || call.startsWith("mutation clearSiteCandidate(")) {
        if (seed.setCandidateError !== undefined) throw new Error(seed.setCandidateError);
        return rowsResult([]);
      }
      if (call.startsWith("mutation promoteSiteCandidate(")) {
        if (seed.promoteError !== undefined) throw new Error(seed.promoteError);
        return rowsResult([]);
      }
      if (call.startsWith("mutation revokeSitePreviewGrant(") || call.startsWith("mutation updateSitePreviewBinding(")) {
        return rowsResult([]);
      }

      // THE STORE READS (epic memql#5530). All three are SHAPED queries, so
      // they answer through `rows()` and never through a bundle envelope --
      // the same reading the app makes, which is what keeps the fake honest
      // about the one way a shaped read can be got wrong.
      if (call === "query stores()") {
        if (seed.storesError) throw new Error(seed.storesError);
        return rowsResult(seed.stores ?? []);
      }
      if (call.startsWith("query storeById(")) {
        if (seed.storesError) throw new Error(seed.storesError);
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult((seed.stores ?? []).filter((row) => row["id"] === id));
      }
      if (call.startsWith("query developmentStoresFor(")) {
        if (seed.storesError) throw new Error(seed.storesError);
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(
          (seed.stores ?? []).filter((row) => row["isDevelopment"] === true && row["developmentOfStoreId"] === id),
        );
      }
      if (call.startsWith("builtin shopifyStoreHealth(")) {
        if (seed.storeHealthError !== undefined) throw new Error(seed.storeHealthError);
        return builtinReply("shopifyStoreHealth", [{ stores: seed.storeHealth ?? [] } as unknown as Row]);
      }
      if (call.startsWith("mutation createStore(")) {
        if (seed.createStoreError !== undefined) throw new Error(seed.createStoreError);
        return rowsResult([]);
      }
      if (call.startsWith("mutation updateSiteStoreBinding(")) {
        if (seed.bindStoreError !== undefined) throw new Error(seed.bindStoreError);
        return rowsResult([]);
      }
      if (call.startsWith("mutation setStoreStatus(")) {
        if (seed.setStoreStatusError !== undefined) throw new Error(seed.setStoreStatusError);
        return rowsResult([]);
      }
      if (call.startsWith("builtin shopifyEnsureSubscriptions(")) {
        if (seed.ensureSubscriptionsError !== undefined) throw new Error(seed.ensureSubscriptionsError);
        return builtinReply("shopifyEnsureSubscriptions", []);
      }

      if (call === "query sourceConnectionsMine()") {
        if (seed.sourceConnectionsError) throw new Error(seed.sourceConnectionsError);
        return rowsResult(sourceConnections.filter(row => row["status"] === "active"));
      }
      if (call.startsWith("builtin sourceInstallations(")) {
        if (seed.sourceInstallationsError) throw new Error(seed.sourceInstallationsError);
        const credentialId = /credentialId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("sourceInstallations", [seed.sourceInstallations?.[credentialId] ?? { reason: "ok", installations: installationRows(credentialId), pending: [] }]);
      }
      if (call.startsWith("builtin sourceConnectionCreate(")) {
        if (seed.sourceConnectionCreateError) throw new Error(seed.sourceConnectionCreateError);
        const credentialId = /credentialId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const installationId = /installationId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const installation = installationRows(credentialId).find(row => row["id"] === installationId);
        const grant = seed.credentials?.find(row => row["id"] === credentialId);
        if (!installation || !grant) throw new Error("source_connection_unavailable: Source access is unavailable.");
        const connectionId = `source-${credentialId}-${installationId}`;
        const existing = sourceConnections.find(row => row["id"] === connectionId);
        if (existing) existing["status"] = "active";
        else sourceConnections.push({ id: connectionId, ownerUserId: grant["ownerUserId"], credentialId, installationId,
          providerAccountId: installation["accountId"] ?? "", accountLogin: installation["account"] ?? installation["login"], accountType: installation["accountType"], status: "active" });
        return builtinReply("sourceConnectionCreate", [{ connectionId, status: "active" }]);
      }
      if (call.startsWith("builtin sourceConnectionRemove(")) {
        if (seed.sourceConnectionRemoveError) throw new Error(seed.sourceConnectionRemoveError);
        const connectionId = /connectionId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const row = sourceConnections.find(row => row["id"] === connectionId);
        if (!row) throw new Error("source_connection_unavailable: Source access is unavailable.");
        row["status"] = "removed";
        return builtinReply("sourceConnectionRemove", [{ connectionId, status: "removed" }]);
      }
      if (call === "query sourceCredentialsMine()") return rowsResult(seed.credentials ?? []);

      if (call.startsWith("builtin sourceProbe(")) {
        if (seed.sourceProbeError !== undefined) throw new Error(seed.sourceProbeError);
        const credentialId = /credentialId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const answers = seed.sourceProbe ?? {};
        const reply = answers[credentialId] ?? answers[""] ?? null;
        return builtinReply("sourceProbe", reply === null ? [] : [reply]);
      }

      if (call.startsWith("builtin artifactProbe(")) {
        if (seed.artifactProbeError !== undefined) throw new Error(seed.artifactProbeError);
        const artifactId = /artifactId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const reply = (seed.artifactProbe ?? {})[artifactId] ?? null;
        return builtinReply("artifactProbe", reply === null ? [] : [reply]);
      }

      if (call.startsWith("builtin sourceCredentialCreate(")) {
        if (seed.credentialCreateError !== undefined) throw new Error(seed.credentialCreateError);
        return builtinReply("sourceCredentialCreate", [
          seed.credentialCreated ?? ({ credentialId: "cred-new", fingerprint: "...9f2c" } as unknown as Row),
        ]);
      }

      if (call.startsWith("builtin sourceCredentialRevoke(")) {
        if (seed.credentialRevokeError !== undefined) throw new Error(seed.credentialRevokeError);
        const credentialId = /credentialId: "([^"]*)"/.exec(call)?.[1] ?? "";
        // THE REPLY'S THIRD KEY, AS TEXT. A scalar boolean crosses the wire
        // as the STRING "true" on a builtin's reply row, and the connected-
        // account card only says "GitHub did not confirm" for a false one --
        // so a fake answering a real boolean, or no row at all, would make
        // every disconnect in every test look like a half-finished one.
        return builtinReply("sourceCredentialRevoke", [
          {
            credentialId,
            status: "revoked",
            remoteRevoked: seed.credentialRevokeRemote === false ? "false" : "true",
          } as unknown as Row,
        ]);
      }

      if (call.startsWith("builtin githubConnectBegin(")) {
        if (seed.connectError !== undefined) throw new Error(seed.connectError);
        return builtinReply("githubConnectBegin", [
          {
            authorizeUrl: seed.connectUrl ?? "",
            reason: seed.connectReason ?? "ok",
            installUrl: seed.installUrl ?? "",
          } as unknown as Row,
        ]);
      }

      // THE FLAGS AS TEXT, for `sourceCredentialRevoke`'s reason above: that is
      // how a scalar boolean has reached this client on a builtin's reply row.
      if (call === "builtin githubAppStatus()") {
        if (seed.githubAppError !== undefined) throw new Error(seed.githubAppError);
        const app = seed.githubApp;
        if (app === undefined) return builtinReply("githubAppStatus", []);
        return builtinReply("githubAppStatus", [
          {
            configured: app.configured ? "true" : "false",
            source: app.source ?? (app.configured ? "environment" : ""),
            slug: app.slug ?? (app.configured ? "memql-on-example" : ""),
            installUrl: app.installUrl ?? "",
            canSetup: app.canSetup ? "true" : "false",
          } as unknown as Row,
        ]);
      }

      if (call.startsWith("builtin githubAppSetupBegin(")) {
        if (seed.appSetupError !== undefined) throw new Error(seed.appSetupError);
        const reason = seed.appSetupReason ?? "ok";
        return builtinReply("githubAppSetupBegin", [
          { startUrl: reason === "ok" ? (seed.appSetupUrl ?? "") : "", reason } as unknown as Row,
        ]);
      }

      if (call === "builtin githubAppRemove()") {
        const reason = seed.appRemoveReason ?? "ok";
        return builtinReply("githubAppRemove", [{ removed: reason === "ok" ? "true" : "false", reason } as unknown as Row]);
      }

      if (call.startsWith("builtin sourceRepositories(")) {
        if (seed.repositoriesError !== undefined) throw new Error(seed.repositoriesError);
        const connectionId = /connectionId: "([^"]*)"/.exec(call)?.[1];
        if (!connectionId || !seed.repositories) return builtinReply("sourceRepositories", seed.repositories ? [seed.repositories] : []);
        const binding = sourceConnections.find(row => row["id"] === connectionId && row["status"] === "active");
        if (!binding) return builtinReply("sourceRepositories", [{ reason: "source_connection_unavailable", repositories: [], installations: [], pending: [], nextPage: 0 }]);
        const repositories = Array.isArray(seed.repositories["repositories"]) ? (seed.repositories["repositories"] as Row[]).filter(row => row["installationId"] === binding["installationId"]) : [];
        return builtinReply("sourceRepositories", [{ ...seed.repositories, repositories }]);
      }

      if (call.startsWith("mutation updatePackageSource(")) {
        if (seed.updateSourceError !== undefined) throw new Error(seed.updateSourceError);
        return rowsResult([]);
      }

      if (call.startsWith("mutation enablePackageDeployables(") && seed.enableDeployablesError) throw new Error(seed.enableDeployablesError);
      if (call.startsWith("mutation disablePackageDeployables(") && seed.disableDeployablesError) throw new Error(seed.disableDeployablesError);
      if (call.startsWith("mutation updateSiteStatus(")) {
        const siteId = /siteId: "([^"]*)"/.exec(call)?.[1] ?? "";
        if (seed.siteStatusErrors?.[siteId]) throw new Error(seed.siteStatusErrors[siteId]);
        if (seed.siteStatusError !== undefined) throw new Error(seed.siteStatusError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin siteArchive(")) {
        if (seed.siteArchiveError !== undefined) throw new Error(seed.siteArchiveError);
        return builtinReply("siteArchive", []);
      }

      if (call.startsWith("builtin siteRestore(")) {
        if (seed.siteRestoreError !== undefined) throw new Error(seed.siteRestoreError);
        return builtinReply("siteRestore", []);
      }

      if (call.startsWith("mutation updateSiteBundle(")) {
        if (seed.repointError !== undefined) throw new Error(seed.repointError);
        return rowsResult([]);
      }

      if (call.startsWith("mutation updateSiteAccount(")) return rowsResult([]);

      // The version walk: the current row, then the newest row at or before
      // each `asOf` instant. History is the seed's `siteHistory`, newest first.
      if (call.startsWith("query siteById(")) {
        const history = seed.siteHistory ?? [];
        return rowsResult(history[0] ? [history[0]] : []);
      }
      if (call.startsWith("asOf(siteById(")) {
        const at = /\), "([^"]+)"\)$/.exec(call)?.[1] ?? "";
        const history = seed.siteHistory ?? [];
        const found = history.find((row) => String(row["createdAt"] ?? "") <= at);
        return rowsResult(found ? [found] : []);
      }

      if (call.startsWith("mutation createPackage(")) {
        return rowsResult([]);
      }

      if (call.startsWith("mutation createSite(")) {
        if (seed.createError !== undefined) throw new Error(seed.createError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin sitePublishFromArtifact(")) {
        if (seed.publishError !== undefined) throw new Error(seed.publishError);
        return builtinReply("sitePublishFromArtifact", [PUBLISH_RESULT as unknown as Row]);
      }

      // `getRowByConceptAndId` composes `concept==<c> && id==<id>`.
      const match = /id==(\S+)/.exec(call);
      const wanted = match?.[1] ?? "";
      const row = wanted === "" ? undefined : seed.byId?.[wanted] ?? sourceConnections.find(row => row["id"] === wanted);
      return bundleResult(row ? [row] : []);
    }),
  };

  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; role?: string; domain?: string; capabilities?: EffectiveCapability[]; everyAccount?: boolean; accountIds?: string[] } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  const role = overrides.role ?? "owner";
  // THE EFFECTIVE SET FOLLOWS THE ROLE (epic memql#5289): the parts this app
  // gates on are read from it, and a harness with no cluster installs the
  // role's seeded set -- what a cluster with no grants resolves.
  // An EXPLICIT set outranks the role's: a test about a grant -- a part
  // withheld from a developer, say -- names the set it means.
  if (overrides.capabilities !== undefined) setEffectiveCapabilities(overrides.capabilities);
  else installSeededAccess(role);
  // THE SHELL PROVIDER TOO, since epic memql#4895: the site and package
  // details carry a "Logs" action that opens another app, and opening an
  // app is the shell's -- `useOs` throws outside its provider, exactly as
  // the Files harness found first. The same role on both, so the session's
  // reading and the shell's cannot disagree.
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "u-me",
          primaryEmail: "owner@example.com",
          role: role,
          everyAccount: overrides.everyAccount ?? ["owner", "admin", "developer"].includes(role),
          accountIds: overrides.accountIds ?? ["self"],
          roleName: "",
          rank: 0,
        },
        config,
      }}
    >
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 8, rows: 5 }}>
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

export function siteRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
    hostname: "example.memql.example.com",
    kind: "spa",
    status: "live",
    bundleRef: "blob://sites/example/v1/",
    artifactId: "",
    title: "",
    notes: "",
    apiProxy: false,
    systemOwned: false,
    deleted: false,
    binding: {},
    candidateRef: "",
    previewBinding: {},
    settings: {},
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** The seeded platform row: cluster-owned, system-owned, baked into the image. */
export const PLATFORM_SITE = siteRow({
  id: "site-os",
  ownerUserId: "",
  hostname: "os.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "file:///app/os",
  systemOwned: true,
  title: "MemQL OS",
});

/** A storefront, published from the Library. */
/**
 * The store SHOP is bound to, and a development store standing in for it.
 *
 * TWO ROWS, PAIRED BY `developmentOfStoreId` (design D8). A development store
 * is attached and mirrored like any other, so what makes it one is a flag on
 * its own row and a pointer at the store it stands in for -- not a mode on the
 * site, and not a second field on the live store.
 */
export const STORE: Row = {
  id: "store-example",
  domain: "example.myshopify.com",
  name: "Example Shop",
  appClientId: "app-1234",
  adminTokenRef: "EXAMPLE_ADMIN_TOKEN",
  storefrontTokenRef: "EXAMPLE_STOREFRONT_TOKEN",
  webhookSecretRef: "EXAMPLE_WEBHOOK_SECRET",
  apiVersion: "2026-07",
  protectedDataLevel: "level1",
  plan: "Shopify Plus",
  status: "live",
  isDevelopment: false,
  developmentOfStoreId: "",
} as unknown as Row;

export const DEV_STORE: Row = {
  id: "store-example-dev",
  domain: "example-dev.myshopify.com",
  name: "Example Shop (development)",
  adminTokenRef: "EXAMPLE_DEV_ADMIN_TOKEN",
  storefrontTokenRef: "EXAMPLE_DEV_STOREFRONT_TOKEN",
  webhookSecretRef: "EXAMPLE_DEV_WEBHOOK_SECRET",
  apiVersion: "2026-07",
  protectedDataLevel: "none",
  plan: "Development",
  status: "configured",
  isDevelopment: true,
  developmentOfStoreId: "store-example",
} as unknown as Row;

/**
 * One entry of the `shopifyStoreHealth` report, with the shape the Go handler
 * emits (`integrations/shopify/capabilities.go`).
 *
 * NOTE WHAT IS ABSENT BY DEFAULT: no `costBucket` (nothing has called the
 * Admin API), no `health.subscriptions` (no reconcile has been recorded) and
 * an EMPTY `domains` (nothing has ever synced). Those are the three states
 * whose honest rendering is a dash, so they are what a fixture starts from --
 * one that filled them in would make the zero-versus-absent cases impossible
 * to write by accident.
 */
export function storeHealthRow(over: Partial<Row> & { storeId: string }): Row {
  return {
    domain: `${over.storeId}.myshopify.com`,
    status: "live",
    apiVersion: "2026-07",
    mirrorApiVersion: "2026-07",
    protectedDataLevel: "level1",
    scopesGranted: ["read_products", "read_orders"],
    scopesNeeded: ["read_products", "read_orders"],
    scopesMissing: [],
    driftLast: 0,
    domains: [],
    health: {},
    ...over,
  } as unknown as Row;
}

/** One row of a store's per-domain sync table. */
export function domainStateRow(over: Partial<Row> & { concept: string }): Row {
  return {
    phase: "idle",
    lastAppliedAt: "",
    lastReconciledAt: "",
    driftLast: 0,
    lagSeconds: 0,
    outboxDepth: 0,
    lastError: "",
    ...over,
  } as unknown as Row;
}

export const SHOP = siteRow({
  id: "site-shop",
  hostname: "shop.memql.example.com",
  kind: "shopify_storefront",
  status: "live",
  bundleRef: "blob://sites/site-shop/v1/",
  artifactId: "artifact-zip",
  title: "Storefront",
  binding: { storeId: "store-example" },
});

/** A draft, baked into the edge image. */
export const DOCS = siteRow({
  id: "site-docs",
  hostname: "docs.memql.example.com",
  kind: "static",
  status: "draft",
  bundleRef: "file:///app/sites/docs",
});

/** Somebody else's site, serving THE SAME bundle as DOCS. */
export const MIRROR = siteRow({
  id: "site-mirror",
  ownerUserId: "u-other",
  hostname: "mirror.memql.example.com",
  kind: "static",
  status: "disabled",
  bundleRef: "file:///app/sites/docs",
});

/** A custom apex, which forms its own domain group. */
export const APEX = siteRow({
  id: "site-apex",
  hostname: "example.org",
  kind: "static",
  status: "live",
  bundleRef: "blob://sites/site-apex/v3/",
});

export const DELETED = siteRow({
  id: "site-gone",
  hostname: "gone.memql.example.com",
  deleted: true,
});

/** A credential CARD: the projection a browser receives, which has no token. */
export function credentialRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
    host: "github.com",
    label: "acme deploy token",
    fingerprint: "sha256:ab12cd34",
    status: "active",
    lastUsedAt: "",
    revokedAt: "",
    createdAt: "2026-08-20T00:00:00Z",
    ...over,
  };
}

/**
 * A GitHub App GRANT, as the card projection carries one.
 *
 * A separate fixture rather than an argument to `credentialRow`, because the
 * two are genuinely different cards: a grant has a login and a set of
 * installations where a token has a label and a fingerprint, and the default
 * above deliberately carries NO `kind` at all -- which is what every row
 * written before memql#4915 looks like and therefore the case worth having a
 * fixture for.
 */
export function githubGrantRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
    host: "github.com",
    kind: "github_app",
    login: "octocat",
    installationIds: ["i-acme", "i-octocat"],
    label: "",
    fingerprint: "",
    status: "active",
    lastUsedAt: "",
    revokedAt: "",
    createdAt: "2026-08-25T00:00:00Z",
    ...over,
  };
}

/** One repository, as `sourceRepositories` answers it. */
export function repositoryFixture(over: Partial<Row> & { fullName: string }): Row {
  const [owner = "", name = ""] = String(over["fullName"] ?? "").split("/");
  return {
    owner,
    name,
    url: `https://github.com/${over["fullName"]}`,
    private: false,
    visibility: "public",
    defaultBranch: "main",
    pushedAt: "2026-08-30T00:00:00Z",
    installationId: "i-acme",
    ...over,
  };
}

/** A `sourceRepositories` reply, assembled from parts. */
export function repositoriesReply(over: Partial<Row> = {}): Row {
  return {
    repositories: [],
    installations: [],
    pending: [],
    nextPage: 0,
    reason: "ok",
    ...over,
  } as unknown as Row;
}

/** A `sourceProbe` reply. `ok` and public by default: the commonest answer. */
export function probeReply(over: Partial<Row> = {}): Row {
  return {
    host: "github.com",
    reachable: true,
    private: false,
    defaultBranch: "main",
    reason: "ok",
    ...over,
  } as unknown as Row;
}

/**
 * A GitHub personal access token, COMPOSED rather than written out.
 *
 * Same reason as `FIXTURE_TOKEN` above: gitleaks judges a test fixture
 * exactly like production code, and `ghp_` followed by thirty-six characters
 * is its github-pat rule whatever the file is for. This exists so a test can
 * plant a token-shaped string in a seed and prove the page never renders it
 * -- an assertion that would be worthless with nothing to find.
 */
export const FIXTURE_GITHUB_PAT = "gh" + "p_" + "abcdefghijklmnopqrstuvwxyz0123456789";

/** An `artifactProbe` reply. Neither a package nor a built site by default. */
export function zipReply(over: Partial<Row> = {}): Row {
  return {
    isPackage: false,
    isBuiltSite: false,
    fileCount: 12,
    totalBytes: 2097152,
    ...over,
  } as unknown as Row;
}

export function artifactRow(over: Partial<Row> & { id: string }): Row {
  return {
    lens: "artifact",
    kind: "file",
    title: "site.zip",
    mimeType: "application/zip",
    archived: false,
    createdAt: "2026-08-02T00:00:00Z",
    ...over,
  };
}

export const ZIP = artifactRow({ id: "artifact-zip", title: "storefront-build.zip" });
export const PDF = artifactRow({ id: "artifact-pdf", title: "brief.pdf", mimeType: "application/pdf" });
export const NOTE = artifactRow({
  id: "artifact-note",
  lens: "record",
  kind: "note",
  title: "Standup notes",
  mimeType: "",
});

/**
 * One v1:observability:siteTraffic row, as the builtin answers it.
 *
 * The counts are numbers rather than strings, which is what the wire carries;
 * the projection tolerates both, and a fixture in the wrong one would let a
 * string-only projection pass here and fail against a cluster.
 */
export function trafficRow(over: Partial<Row> & { windowStart: string }): Row {
  return {
    siteId: "site-shop",
    bucket: "1h",
    windowEnd: "",
    requestCount: 0,
    errorCount: 0,
    clientErrorCount: 0,
    bytesTotal: 0,
    lastServedAt: "",
    ...over,
  };
}

// ---------------------------------------------------------------------------
// Interaction helpers
// ---------------------------------------------------------------------------
//
// Clicks, typing and emitted events all go through act(): a state update
// outside it is not flushed before the next assertion, which reads exactly like
// a control that did nothing. They live here rather than being redeclared in
// each test file because they are stateless -- there is no fixture to leak.

export async function click(el: Element | null | undefined): Promise<void> {
  if (!el) throw new Error("click() was handed nothing to click");
  // `fireEvent`, not `el.click()`: an SVG element is an SVGElement, which does
  // not inherit HTMLElement's `click()` -- and the map's nodes are SVG groups.
  // A helper that only worked on HTML would quietly be untestable exactly on
  // the surface this app was built for.
  await act(async () => {
    fireEvent.click(el);
  });
}

export async function type(el: HTMLInputElement, value: string): Promise<void> {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** Push a graph event onto the fake wire and let React settle. */
export async function emit(
  connection: FakeConnection,
  concept: string,
  payload: Row,
  kind = "NODE_UPDATED",
): Promise<void> {
  await act(async () => {
    connection.subscriptions.emit(concept, payload, kind);
  });
}

// ---------------------------------------------------------------------------
// Custom-domain fixtures (epic memql#4805)
// ---------------------------------------------------------------------------

/**
 * The ownership token a fixture binding carries.
 *
 * COMPOSED rather than written out, and the reason is a scanner rather than a
 * style preference: gitleaks' generic-api-key rule judges a test fixture
 * exactly like production code, so a literal that looks like a key fails the
 * lane whatever the file is for.
 */
export const FIXTURE_TOKEN = "tok-" + "abcdef" + "0123456789";

export function domainRow(over: Partial<Row> & { id: string }): Row {
  return {
    siteId: "site-shop",
    hostname: "www.acme.com",
    accountId: "",
    token: FIXTURE_TOKEN,
    status: "pending_dns",
    failureReason: "",
    failureDetail: "",
    lastCheckedAt: "",
    verifiedAt: "",
    issuedAt: "",
    removedAt: "",
    createdAt: "2026-09-01T00:00:00Z",
    ...over,
  };
}

/**
 * A `sitePreviewReadiness` answer (epic memql#5531).
 *
 * THE DEFAULT IS THE HONEST EMPTY STATE and not a permissive one: no
 * candidate, no store attached, nothing legal, and the refusals the engine
 * would actually give. A fixture that defaulted every boolean to true would
 * make the absence cases -- which are most of this surface -- impossible to
 * write by accident.
 */
export function previewReadinessRow(over: Partial<Row> & { siteId: string }): Row {
  return {
    hostname: "example.memql.example.com",
    kind: "shopify_storefront",
    status: "live",
    storefront: true,
    bundleRef: "blob://sites/example/v1/",
    candidateRef: "",
    hasCandidate: false,
    storeId: "",
    storeDomain: "",
    storeReadable: false,
    storeIsDevelopment: false,
    previewStoreId: "",
    previewStoreDomain: "",
    canPreview: false,
    canPromote: false,
    canGoLive: true,
    previewRefusal: {
      code: "no_preview_binding",
      message: "this storefront has no development store on its preview binding, so there is nothing to exercise a candidate against.",
      remedy: "Attach a development store to the preview binding first.",
    },
    promoteRefusal: {
      code: "no_candidate_version",
      message: "this deployable has no candidate version, so there is nothing to promote.",
      remedy: "Publish a version and set it as the candidate first.",
    },
    goLiveRefusal: { code: "", message: "", remedy: "" },
    ...over,
  } as unknown as Row;
}

/** One `sitePreviewObservationsForSite` row. */
export function previewObservationRow(over: Partial<Row> & { kind: string }): Row {
  return {
    id: `obs-${String(over.kind)}`,
    grantId: "grant-1",
    siteId: "site-shop",
    storeId: "store-example-dev",
    observedAt: "2026-09-20T12:00:00Z",
    ok: true,
    detail: "",
    failure: "",
    durationMs: 120,
    ...over,
  } as unknown as Row;
}

/** One `sitePreviewGrantsForSite` row, open by default. */
export function previewGrantRow(over: Partial<Row> & { id: string }): Row {
  return {
    siteId: "site-shop",
    ownerUserId: "u-me",
    candidateRef: "blob://sites/example/v2/",
    previewStoreId: "store-example-dev",
    issuedAt: "2026-09-20T12:00:00Z",
    // Far enough out that the clock a test runs on cannot expire it.
    expiresAt: "2099-01-01T00:00:00Z",
    revokedAt: "",
    lastSeenAt: "",
    ...over,
  } as unknown as Row;
}

/** What `sitePreviewOpen` answers: the one-time link and what it opens. */
export const OPENED_PREVIEW: Row = {
  id: "grant-new",
  grantId: "grant-new",
  siteId: "site-shop",
  hostname: "shop.memql.example.com",
  candidateRef: "blob://sites/example/v2/",
  previewStoreId: "store-example-dev",
  previewStoreDomain: "example-dev.myshopify.com",
  expiresAt: "2099-01-01T00:00:00Z",
  ttlMinutes: 30,
  url: "https://shop.memql.example.com/_memql/preview?grant=mql_prv_TEST",
} as unknown as Row;

/** A persisted GitHub identity-to-installation source binding. */
export function sourceConnectionRow(over: Partial<Row> = {}): Row {
  return { id: "source-acme", ownerUserId: "u-me", credentialId: "cred-grant", installationId: "i-acme", providerAccountId: "provider-acme", accountLogin: "acme", accountType: "Organization", status: "active", ...over };
}
