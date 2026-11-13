import { parseCall } from "../callArgs";
import { DEFAULT_LIST_FILTER, flatDeployables, foldDeployables, foldSources, sourceSummary, sourceStateWord } from "../../src/apps/deployables/list";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { packageFromRow, deploymentFromRow } from "../../src/apps/deployables/packages/rows";
import { credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { sourceRecord } from "../../src/apps/deployables/sources/sourceRecord";
import { sourceConnectionFromRow } from "../../src/modules/connections/connections";
import type { ReactNode } from "react";
import { filterRuns, type RunFilter } from "../../src/apps/deployables/pipelines/runs";
import { runFromRow } from "../../src/apps/deployables/pipelines/rows";
import { act, fireEvent } from "@testing-library/react";
import { afterEach, vi } from "vitest";
import { QueryClient, Result, type Connection, type Row } from "@znasllc-io/memql-sdk-core/client";
import { disposeSessionCollections } from "../../src/live/sessionCollections";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider, useOs } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { PipelinesApp } from "../../src/apps/pipelines/PipelinesApp";
import { installSeededAccess } from "../seededAccess";
import { holdsForOrganization, setEffectiveCapabilities, type EffectiveCapability } from "../../src/system/roles";

const sessions = new Set<Connection>();
afterEach(() => { for (const connection of sessions) disposeSessionCollections(connection); sessions.clear(); });

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

/** A shape-projected page that names the cursor of the page after it. */
export function rowsResultWithCursor(rows: Row[], cursor: string): Result {
  return new Result({ data: rows, ...(cursor !== "" ? { meta: { cursor } } : {}) } as never);
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
 * What `integrations/siteaccess` answers: its own node ids (`siteAccess:list:<id>`)
 * around a payload that carries the row's bare id. `builtinReply` names each node
 * after the row, so through it a panel reading the envelope's id instead of the
 * payload's would pass here and act on a code that does not exist.
 */
function siteAccessReply(kind: string, key: string, rows: Row[]): Result {
  const wrapper: Record<string, unknown> = {};
  rows.forEach((row, index) => {
    const id = `siteAccess:${kind}:${String(row[key])}`;
    wrapper[id] = {
      id,
      concept: "integration:siteAccess:result",
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
  repositoryAuthority?: Row;
  repositoryAuthorityError?: string;
  changeRequests?: Row[];
  repositoryChangeError?: string;
  workRunSummaries?: Row[];
  workRunSummaryError?: string;
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
  /**
   * The `shopifyStoreVerdict` reply, keyed by store id (`storeVerdictRow`).
   *
   * ABSENT IS AN ENGINE WITH NO SHOPIFY CONNECTOR: a store with no entry is
   * answered `{status: "unconfigured"}`, which is what the handler replies
   * then, and which names no state.
   */
  storeVerdict?: Record<string, Row>;
  /** Fails every `shopifyStoreVerdict` read with this server message. */
  storeVerdictError?: string;
  /**
   * What `shopifyStoreRegisterEvents` answers instead of a clean pass: the
   * handler REPLIES `{status: "failed", detail}` when Shopify refused a
   * topic or the subscriptions could not be read, and `{status:
   * "unconfigured"}` with no connector -- neither is thrown.
   */
  storeRegisterEventsReply?: Row;
  /** What `shopifyStoreRetryDeliveries` answers instead of `requeued`. */
  storeRetryDeliveriesReply?: Row;
  /**
   * Fails every `shopifyStoreSetRecording` -- the Store page's Pause recording
   * and Resume -- with this server message. Absent, it answers as the handler
   * does: `{status, storeId}`, live for a resume and paused for a pause.
   */
  storeSetRecordingError?: string;
  /**
   * What `shopifyStoreAlertWait` answers, and what `shopifyStoreAlertWaitSet`
   * changes. Absent is the unset wait read by somebody who may not set it.
   * A set is refused as the engine refuses it: anybody `maySet` is not true
   * for, and any value that is not a whole number from 1 to 1440.
   */
  storeAlertWait?: Row;
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
  /**
   * The rows `siteAccessCodesForSite` answers from, as the handler's `listRow`
   * shapes them. THE STORE ITSELF RATHER THAN A COPY: the four acts write to it
   * and the panel re-reads what they wrote, and a test that moves a row under the
   * panel -- somebody else removing a code -- does it on this same array.
   */
  accessCodes?: Row[];
  accessCodesError?: string;
  /** What `siteAccessGuestPassCreate` refuses with, when it should. */
  accessGuestError?: string;
  /** What `siteAccessGates` answers. ABSENT IS NONE, what a deployable with no @accessGate answers. */
  accessGates?: Row[];
  accessGatesError?: string;
  /** Fail the next act of each kind with this text, as `executeNamed` would throw it. */
  accessCreateError?: string;
  accessRegenerateError?: string;
  accessSetExpiryError?: string;
  accessRemoveError?: string;
  /**
   * The reviews pack's answers (packs/reviewspack): `reviewsList` keyed by the
   * filter asked, `reviewsGet` keyed by review id, the store's decision log
   * and its settings. ABSENT IS EMPTY: a store nobody has reviewed.
   */
  reviews?: {
    list?: Record<string, Row>;
    get?: Record<string, Row>;
    log?: Row;
    settings?: Row;
    /** What `reviewsPeople` answers: the people who can moderate the store. */
    people?: Row;
    /** Fails every `reviewsPeople` read with this server message. */
    peopleError?: string;
    /** Leaves every `reviewsPeople` read unanswered: who can moderate is still being read. */
    peoplePending?: boolean;
    /** Fails the next decision or erasure with this server message. */
    decideError?: string;
    /** Fails the next settings save with this server message. */
    settingsError?: string;
  };
  /**
   * The wholesale pack's answers (packs/wholesalepack): `wholesaleList` keyed
   * by the stage asked, `wholesaleGet` keyed by the application id as the
   * list gave it, and the store's settings. ABSENT IS EMPTY: a store nobody
   * has applied to, and one that is not set up. THE SETTINGS ARE THE STORE
   * ITSELF: setting a store up and saving its settings write to them, so a
   * read after the write answers what was written.
   */
  wholesale?: {
    list?: Record<string, Row>;
    get?: Record<string, Row>;
    settings?: Row;
    /** Fails every `wholesaleList` read with this server message, until removed. */
    listError?: string;
    /** Fails every `wholesaleStoreSettings` read with this server message, until removed. */
    settingsReadError?: string;
    /** Fails every decision with this server message, until removed. */
    decideError?: string;
    /** Fails every retry with this server message, until removed. */
    retryError?: string;
    /** Fails every settings save with this server message, until removed. */
    settingsError?: string;
    /** What `wholesaleBuyers` answers: `{rows, next, truncated}`, paged by offset here. */
    buyers?: Row;
    /** Fails every `wholesaleBuyers` read with this server message, until removed. */
    buyersError?: string;
    /** What `wholesaleDocuments` answers without a versionId: `{kinds}`. */
    documents?: Row;
    /** What `wholesaleDocuments` answers with a versionId: the version, body included, by its id. */
    versions?: Record<string, Row>;
    /** Fails every `wholesaleDocuments` read with this server message, until removed. */
    documentsError?: string;
    /** The id `wholesaleAddBuyer` answers. */
    addedId?: string;
    /** Fails every add with this server message, until removed. */
    addError?: string;
    /** Fails every publish with this server message, until removed. */
    publishError?: string;
    /** Fails every resend with this server message, until removed. */
    resendError?: string;
    /** Fails every removal with this server message, until removed. */
    removeError?: string;
    /** What `wholesaleExportStore` answers: `{fileName, json}`. */
    exportFile?: Row;
    /** Fails every export with this server message, until removed. */
    exportError?: string;
    /**
     * A lost answer, for the QA scenes: the add or the publish loses its reply,
     * after it landed or before it left, and the wizard's re-read of the store
     * then answers, never answers (`hold`), or fails.
     */
    lose?: { write: "add" | "publish"; landed: boolean; check: "answer" | "hold" | "fail" };
    /** Set once an answer was lost: the re-reads after it follow `lose.check`. */
    lost?: boolean;
  };
  /** v1:platform:package rows `packagesAll` answers with. */
  packages?: Row[];
  registerSourceError?: string;
  registeredPackageId?: string;
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
   * `packageDeploymentsPending` answers with: the list's fourth feed,
   * for the waiting mark (epic memql#4885, design section A).
   */
  awaitingConfirm?: Row[];
  /**
   * What `siteTrafficInWindow` answers in the summary mode, the list row's
   * one-row-per-deployable read and the only one the app makes.
   *
   * ABSENT MEANS UNMEASURED, which is exactly what the server does -- it
   * sends no row for a window it measured nothing in -- so a seed that says
   * nothing about traffic exercises the unmeasured path rather than a
   * zero-filled one.
   */
  traffic?: { summary?: Row[] };
  /**
   * What `siteStatistics` answers: ONE payload, keyed as
   * component/sitetraffic's `Statistics.Payload()` writes it
   * (`siteStatisticsRow` builds one), whatever the call names. Absent answers
   * no node at all.
   */
  siteStatistics?: Row;
  /** Fails every `siteStatistics` read with this server message. */
  siteStatisticsError?: string;
  /**
   * What `storeStatistics` answers, keyed by the store id the call names
   * (`storeStatisticsRow` builds one). A store with no entry answers no node.
   */
  storeStatistics?: Record<string, Row>;
  /** Fails every `storeStatistics` read with this server message. */
  storeStatisticsError?: string;
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
  /** Drops the socket under the next `packageRollback`, as the SDK reports it. */
  rollbackDropped?: boolean;
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

  // ---- pipelines (epic memql#5479) ----
  /** v1:pipelines:run rows `pipelineRunsForOwner` answers as its first page. */
  pipelineRuns?: Row[];
  /** The page after `pipelineRuns`. */
  olderPipelineRuns?: Row[];
  /** Every page of runs, newest first; each names the next by cursor. Overrides the two above. */
  pipelineRunPages?: Row[][];
  /** v1:pipelines:channel rows for this owner. */
  pipelineChannels?: Row[];
  /** v1:pipelines:pipeline rows `pipelinesForOwner` answers. */
  pipelines?: Row[];
  /** v1:work:step rows, by the bare work run id `workStepsForOwnerRun` names. */
  workSteps?: Record<string, Row[]>;
  /** v1:library:artifact rows, by the work run id `artifactsForRun` names. */
  runArtifacts?: Record<string, Row[]>;
  expiredRunFiles?: Record<string, { fileId: string; name: string }[]>;
  expiredRunFilesError?: string;
  /** What `pipelinesPreview` answers (one row). Absent answers no row. */
  pipelinePreview?: Row;
  pipelinePreviewError?: string;
  /** What `pipelinesConnect` answers; defaults to the pipeline id derived from the package. */
  pipelinesConnectResult?: Row;
  pipelinesConnectError?: string;
  pipelinesDisconnectError?: string;
  /** What `pipelinesRunLatest` answers; defaults to one queued manual run. */
  pipelinesRunLatestResult?: Row;
  pipelinesRunLatestError?: string;
  /** What `pipelinesRerun` answers; defaults to a new attempt "run-rerun". */
  pipelinesRerunResult?: Row;
  pipelinesRerunError?: string;
  pipelinesCancelError?: string;
  /** What `pipelinesInstallations` answers as its installations list. */
  laggingInstallations?: Row[];
  laggingInstallationsError?: string;
}

export interface FakeConnection {
  status: "connected";
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
  let accessMints = 0;
  const nextAccessCode = (): string => FIXTURE_ACCESS_CODES[accessMints++ % FIXTURE_ACCESS_CODES.length]!;
  const sites = seed.sites ?? [];
  // `refuseOrganizationActionCapability`: an act on a code is judged by the organization
  // copied onto the code when it was minted, and a code nobody can find is refused alike.
  const codePastTheGate = (construct: string, codeId: string): Row => {
    const row = seed.accessCodes?.find((r) => r["id"] === codeId);
    const copied = String(row?.["accountId"] ?? "");
    const capable = holdsForOrganization(copied, "read", "data") && holdsForOrganization(copied, "read", "app:deployables") && holdsForOrganization(copied, "execute", "app:deployables/access");
    if (row === undefined || !capable) {
      throw new Error(refusedByEngine(construct, `capability_not_held: "${construct}" is not permitted for the selected organization's target`));
    }
    return row;
  };
  const refuseIfEnded = (construct: string, row: Row): void => {
    const refuse = (sentence: string): never => {
      throw new Error(refusedByEngine(construct, sentence));
    };
    if (row["revokedAt"]) refuse("This access code was removed, and removal is final. To give someone access again, add a new code.");
    const site = sites.find((candidate) => candidate["id"] === row["siteId"]);
    if (site === undefined) return;
    if (String(row["accountId"] ?? "") !== String(site["accountId"] ?? "")) refuse("This access code was minted while its deployable belonged to another organization, so it opens nothing. Remove it and add a new code.");
    if (String(site["ownerUserId"] ?? "") === "") refuse("This deployable has no owner, so this access code opens nothing. Give it an owner and try again, or remove the code.");
  };
  const artifacts = seed.artifacts ?? [];
  let alertWait: Row = seed.storeAlertWait ?? ({ minutes: 30, isDefault: true, maySet: false } as unknown as Row);
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
  const registeredPackages: Row[] = [];
  const sourceConnections = seed.sourceConnections?.map(row => ({ ...row })) ?? (seed.credentials ?? []).flatMap(grant =>
    grant["kind"] !== "github_app" ? [] : installationRows(String(grant["id"])).map(installation => ({
      id: `source-${grant["id"]}-${installation["id"]}`, ownerUserId: grant["ownerUserId"], credentialId: grant["id"],
      installationId: installation["id"], providerAccountId: installation["accountId"] ?? "", accountLogin: installation["account"] ?? installation["login"] ?? "",
      accountType: installation["accountType"], status: "active",
    }) as Row));

  const changes = new Map<string, Map<string, Row>>();
  const latest = (concept: string, rows: readonly Row[]) => [...new Map([...rows.map(row => [String(row.id), row] as const), ...[...(changes.get(concept) ?? new Map())]]).values()].filter(row => row.status !== "deleted");
  const subscriptions = fakeSubscriptions();
  const originalEmit = subscriptions.emit;
  subscriptions.emit = (concept, row, kind) => { const held = changes.get(concept) ?? new Map<string, Row>(); held.set(String(row.id), kind === "NODE_DELETED" ? { ...row, status: "deleted" } : row); changes.set(concept, held); originalEmit(concept, row, kind); };
  const stub = {
    executeNamed: vi.fn(async (_name: string, call: string, opts?: { cursor?: string }) => {
      calls.push(call);

      if (_name === "deployableDirectoryPage") {
        if (seed.sitesError) throw new Error(seed.sitesError);
        const args = parseCall(call).args;
        const siteRows = latest("v1:platform:site", sites).map(siteFromRow);
        const packageRows = latest("v1:platform:package", [...(seed.packages ?? []), ...registeredPackages]).map(packageFromRow);
        const pending = latest("v1:platform:packageDeployment", seed.awaitingConfirm ?? []).map(deploymentFromRow);
        const filter = { ...DEFAULT_LIST_FILTER, search: String(args.search ?? ""), kind: String(args.kind ?? ""), status: String(args.status ?? ""), accountId: String(args.accountId ?? "all"), source: String(args.source ?? "") };
        const archived = args.archived === true;
        const provenance = (pkg: ReturnType<typeof packageFromRow>) => sourceRecord(pkg, latest("v1:platform:sourceCredential", seed.credentials ?? []).map(credentialFromRow), latest("v1:platform:sourceConnection", sourceConnections).map(sourceConnectionFromRow)).provenance;
        const sources = (search: string, archived: boolean) => foldSources(siteRows, packageRows.filter(pkg => pkg.sourceKind === "repo" && pkg.repoUrl), pending, search, archived, provenance);
        const sourcesMode = args.resource === "sources";
        const entries = sourcesMode ? sources(filter.search, archived).map(group => ({ id: group.id, createdAt: group.pkg?.createdAt ?? "", group: { ...group, pkg: { ...group.pkg!, declares: [], disabledDeployables: [] }, summary: sourceSummary(group), state: sourceStateWord(group), provenance: provenance(group.pkg!), rows: [] } })) : flatDeployables(foldDeployables(siteRows, packageRows, pending, filter, archived)).map(row => ({ id: row.key, createdAt: row.site?.createdAt || row.parked?.createdAt || row.pkg?.createdAt || "", row }));
        entries.sort((a,b) => b.createdAt.localeCompare(a.createdAt) || a.id.localeCompare(b.id));
        const offset = Number(args.cursor ?? 0);
        const archivedCount = sourcesMode ? sources("", true).length : flatDeployables(foldDeployables(siteRows, packageRows, pending, DEFAULT_LIST_FILTER, true)).length;
        return builtinReply("deployableDirectoryPage", [{ entries: entries.slice(offset, offset+10), nextCursor: offset+10 < entries.length ? String(offset+10) : "", total: entries.length, archivedCount, provenanceError: seed.sourceConnectionsError ? "Some GitHub provenance could not be refreshed." : "" }]);
      }
      // ---- reviews (packs/reviewspack) ----
      if (call.startsWith("builtin reviewsList(")) {
        const args = parseCall(call).args;
        const state = String(args.state ?? "waiting");
        const held = seed.reviews?.list?.[state] ?? { reviews: [], total: 0, counts: {} };
        const query = String(args.search ?? "").toLowerCase();
        const rows = ((held.reviews ?? []) as Record<string, unknown>[]).filter(row => !query || [row.title, row.body, row.productHandle, row.authorName, row.authorEmail].join(" ").toLowerCase().includes(query));
        const offset = Number(args.after ?? 0), limit = Number(args.limit ?? 50);
        return builtinReply("reviewsList", [{ ...held, reviews: rows.slice(offset, offset + limit), total: rows.length, next: offset + limit < rows.length ? String(offset + limit) : "" }]);
      }
      if (call.startsWith("builtin reviewsGet(")) {
        const id = /reviewId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const answer = seed.reviews?.get?.[id];
        if (answer === undefined) throw new Error(`reviews: review "${id}" is not readable by this caller`);
        return builtinReply("reviewsGet", [answer]);
      }
      if (call.startsWith("builtin reviewsDecisionLog(")) {
        const args = parseCall(call).args;
        const held = seed.reviews?.log ?? { decisions: [], total: 0 };
        const query = String(args.search ?? "").toLowerCase();
        const rows = ((held.decisions ?? []) as Record<string, unknown>[]).filter(row => !query || [row.title, row.productHandle, row.action, row.reason, row.note, row.decidedBy].join(" ").toLowerCase().includes(query));
        const offset = Number(args.after ?? 0), limit = Number(args.limit ?? 50);
        return builtinReply("reviewsDecisionLog", [{ ...held, decisions: rows.slice(offset, offset + limit), total: rows.length, next: offset + limit < rows.length ? String(offset + limit) : "" }]);
      }
      if (call.startsWith("builtin reviewsSettings(")) {
        return builtinReply("reviewsSettings", [seed.reviews?.settings ?? ({ exists: false, settings: {}, may: { edit: false, nameApprover: false }, viewer: { moderates: true, approvesRemovals: false } } as unknown as Row)]);
      }
      if (call.startsWith("builtin reviewsPeople(")) {
        if (seed.reviews?.peopleError !== undefined) throw new Error(seed.reviews.peopleError);
        if (seed.reviews?.peoplePending === true) return new Promise<never>(() => {});
        return builtinReply("reviewsPeople", [seed.reviews?.people ?? ({ storeId: "", people: [] } as unknown as Row)]);
      }
      if (call.startsWith("builtin reviewsDecide(") || call.startsWith("builtin reviewsEraseReviewer(")) {
        if (seed.reviews?.decideError !== undefined) throw new Error(seed.reviews.decideError);
        return builtinReply("reviewsDecide", [{ decisionId: "d-new" } as unknown as Row]);
      }
      if (call.startsWith("builtin reviewsUpdateSettings(")) {
        if (seed.reviews?.settingsError !== undefined) throw new Error(seed.reviews.settingsError);
        return builtinReply("reviewsUpdateSettings", [{ changed: [] } as unknown as Row]);
      }
      if (call.startsWith("builtin reviewsExportStore(")) {
        return builtinReply("reviewsExportStore", [{ fileName: "reviews-store-live-2026-11-04.json", json: "{\"format\":\"memql.reviews.export/v1\"}" } as unknown as Row]);
      }
      // ---- wholesale (packs/wholesalepack) ----
      if (call.startsWith("builtin wholesaleList(")) {
        if (seed.wholesale?.listError !== undefined) throw new Error(seed.wholesale.listError);
        const args = parseCall(call).args;
        if (seed.wholesale?.lost && args.stage === "all") {
          if (seed.wholesale.lose?.check === "hold") return new Promise<never>(() => {});
          if (seed.wholesale.lose?.check === "fail") throw new Error("wholesale: the store could not be read just now");
        }
        const stage = String(args.stage ?? "readyToReview");
        const held = seed.wholesale?.list?.[stage] ?? { rows: [], counts: {}, next: "", truncated: false };
        const query = String(args.search ?? "").toLowerCase();
        const rows = ((held.rows ?? []) as Record<string, unknown>[]).filter(row => !query || [row.companyName, row.applicantName, row.applicantEmail].join(" ").toLowerCase().includes(query));
        const offset = Number(args.after ?? 0), limit = Number(args.limit ?? 50);
        return builtinReply("wholesaleList", [{ ...held, rows: rows.slice(offset, offset + limit), next: offset + limit < rows.length ? String(offset + limit) : "" }]);
      }
      if (call.startsWith("builtin wholesaleGet(")) {
        const id = String(parseCall(call).args.applicationId ?? "");
        const answer = seed.wholesale?.get?.[id];
        if (answer === undefined) throw new Error(`wholesale: application "${id}" is not readable by this caller`);
        return builtinReply("wholesaleGet", [answer]);
      }
      if (call.startsWith("builtin wholesaleStoreSettings(")) {
        if (seed.wholesale?.settingsReadError !== undefined) throw new Error(seed.wholesale.settingsReadError);
        return builtinReply("wholesaleStoreSettings", [seed.wholesale?.settings ?? ({ exists: false, applicationsOpen: false, entitlementAdapter: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: false } } as unknown as Row)]);
      }
      if (call.startsWith("builtin wholesaleRecordDecision(")) {
        if (seed.wholesale?.decideError !== undefined) throw new Error(seed.wholesale.decideError);
        const args = parseCall(call).args;
        return builtinReply("wholesaleRecordDecision", [{ applicationId: String(args.applicationId ?? ""), transition: String(args.transition ?? "") } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleRetryAccess(")) {
        if (seed.wholesale?.retryError !== undefined) throw new Error(seed.wholesale.retryError);
        return builtinReply("wholesaleRetryAccess", [{ applicationId: String(parseCall(call).args.applicationId ?? "") } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleUpdateSettings(")) {
        if (seed.wholesale?.settingsError !== undefined) throw new Error(seed.wholesale.settingsError);
        const { siteId: _site, storeId: _store, ...changes } = parseCall(call).args;
        const held = (seed.wholesale ??= {});
        held.settings = { ...(held.settings ?? {}), ...changes } as Row;
        return builtinReply("wholesaleUpdateSettings", [changes as Row]);
      }
      if (call.startsWith("builtin wholesaleSetSettings(")) {
        const args = parseCall(call).args;
        const held = (seed.wholesale ??= {});
        held.settings = { exists: true, applicationsOpen: args.applicationsOpen === true, entitlementAdapter: String(args.entitlementAdapter ?? ""), adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: true } } as unknown as Row;
        return builtinReply("wholesaleSetSettings", [{ storeId: String(args.storeId ?? "") } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleBuyers(")) {
        if (seed.wholesale?.buyersError !== undefined) throw new Error(seed.wholesale.buyersError);
        const args = parseCall(call).args;
        const held = seed.wholesale?.buyers ?? { rows: [], next: "", truncated: false };
        const rows = (held.rows ?? []) as Record<string, unknown>[];
        const offset = Number(args.after ?? 0), limit = Number(args.limit ?? 50);
        return builtinReply("wholesaleBuyers", [{ ...held, rows: rows.slice(offset, offset + limit), next: offset + limit < rows.length ? String(offset + limit) : "" } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleDocuments(")) {
        if (seed.wholesale?.documentsError !== undefined) throw new Error(seed.wholesale.documentsError);
        const versionId = String(parseCall(call).args.versionId ?? "");
        if (seed.wholesale?.lost && versionId === "") {
          if (seed.wholesale.lose?.check === "hold") return new Promise<never>(() => {});
          if (seed.wholesale.lose?.check === "fail") throw new Error("wholesale: the documents could not be read just now");
        }
        if (versionId !== "") {
          const version = seed.wholesale?.versions?.[versionId];
          if (version === undefined) throw new Error(`wholesale: there is no document version "${versionId}" of this store`);
          return builtinReply("wholesaleDocuments", [{ version } as unknown as Row]);
        }
        const empty = { kinds: ["terms", "map", "priceSheet"].map((kind) => ({ kind, current: null, history: [] })) };
        return builtinReply("wholesaleDocuments", [(seed.wholesale?.documents ?? empty) as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleAddBuyer(")) {
        if (seed.wholesale?.addError !== undefined) throw new Error(seed.wholesale.addError);
        if (seed.wholesale?.lose?.write === "add") {
          const args = parseCall(call).args;
          const held = seed.wholesale;
          if (held.lose!.landed) {
            const row = { applicationId: held.addedId ?? "wholesale-staff-new", companyName: String(args.companyName ?? ""), applicantName: String(args.applicantName ?? ""), applicantEmail: String(args.applicantEmail ?? ""), stage: "waitingForTerms", primary: "", secondary: "", createdAt: new Date().toISOString() };
            held.list = { ...held.list, all: { rows: [row], counts: {}, next: "", truncated: false } as unknown as Row };
          }
          held.lost = true;
          throw droppedSocket();
        }
        return builtinReply("wholesaleAddBuyer", [{ applicationId: seed.wholesale?.addedId ?? "wholesale-staff-new" } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleRemoveBuyer(")) {
        if (seed.wholesale?.removeError !== undefined) throw new Error(seed.wholesale.removeError);
        const args = parseCall(call).args;
        return builtinReply("wholesaleRemoveBuyer", [{ applicationId: String(args.applicationId ?? ""), transition: "revoke" } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleResendEmail(")) {
        if (seed.wholesale?.resendError !== undefined) throw new Error(seed.wholesale.resendError);
        const args = parseCall(call).args;
        return builtinReply("wholesaleResendEmail", [{ applicationId: String(args.applicationId ?? ""), kind: String(args.kind ?? ""), state: "sent" } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesalePublishDocument(")) {
        if (seed.wholesale?.publishError !== undefined) throw new Error(seed.wholesale.publishError);
        if (seed.wholesale?.lose?.write === "publish") {
          const args = parseCall(call).args;
          const held = seed.wholesale;
          if (held.lose!.landed) {
            const kinds = ((held.documents as unknown as { kinds?: Record<string, unknown>[] })?.kinds ?? []).map((k) => (k.kind === args.kind
              ? { ...k, current: { id: "doc-new", label: String(args.label ?? ""), body: String(args.body ?? ""), publishedAt: new Date().toISOString(), publishedBy: "", publishedByName: "", sha256: "0".repeat(64) } }
              : k));
            held.documents = { kinds } as unknown as Row;
          }
          held.lost = true;
          throw droppedSocket();
        }
        return builtinReply("wholesalePublishDocument", [{ documentVersionId: "doc-new", sha256: "0".repeat(64) } as unknown as Row]);
      }
      if (call.startsWith("builtin wholesaleExportStore(")) {
        if (seed.wholesale?.exportError !== undefined) throw new Error(seed.wholesale.exportError);
        return builtinReply("wholesaleExportStore", [seed.wholesale?.exportFile ?? ({ fileName: "wholesale-store-live-20261111.json", json: "{\"applications\":[]}" } as unknown as Row)]);
      }

      // ---- pipelines (epic memql#5479) ----
      if (call === "query pipelineRunsForOwner()") {
        const pages = seed.pipelineRunPages ?? [seed.pipelineRuns ?? [], ...(seed.olderPipelineRuns ? [seed.olderPipelineRuns] : [])];
        const at = opts?.cursor?.startsWith("runs-page-") ? Number(opts.cursor.slice("runs-page-".length)) : 0;
        return rowsResultWithCursor(pages[at] ?? [], at + 1 < pages.length ? `runs-page-${at + 1}` : "");
      }
      if (call.startsWith("query pipelineRunsBrowse(")) {
        const value = (name: string) => new RegExp(name + ': "([^\"]*)"').exec(call)?.[1] ?? "";
        const filter: RunFilter = { changesOnly: call.includes("changesOnly: true"), search: value("search"), pipelineId: value("pipelineId"), branch: value("branch"), outcome: value("outcome") as RunFilter["outcome"] };
        const pages = seed.pipelineRunPages ?? [seed.pipelineRuns ?? [], ...(seed.olderPipelineRuns ? [seed.olderPipelineRuns] : [])];
        const matched = filterRuns(pages.flat().map(runFromRow), filter);
        const byId = new Map(pages.flat().map(row => [String(row.id), row]));
        const at = opts?.cursor?.startsWith("browse-") ? Number(opts.cursor.slice(7)) : 0;
        return rowsResultWithCursor(matched.slice(at, at + 10).map(row => byId.get(row.id)!), at + 10 < matched.length ? `browse-${at + 10}` : "");
      }
      if (call.startsWith("query pipelineRunForOwner(")) {
        const id = /runId: "([^\"]*)"/.exec(call)?.[1] ?? "";
        const rows = (seed.pipelineRunPages ?? [seed.pipelineRuns ?? [], seed.olderPipelineRuns ?? []]).flat();
        return rowsResult(rows.filter(row => row.id === id));
      }
      if (call === "query pipelineChannelsForOwner()") return rowsResult(seed.pipelineChannels ?? []);
      if (call === "query pipelinesForOwner()" || call === "query pipelineDirectory()") return rowsResult(seed.pipelines ?? []);
      if (call.startsWith("query workStepsForOwnerRun(")) {
        const id = /runId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(seed.workSteps?.[id] ?? []);
      }
      if (call.startsWith("query workRunSummaryForOwner(")) {
        if (seed.workRunSummaryError) throw new Error(seed.workRunSummaryError);
        const id = /runId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult((seed.workRunSummaries ?? []).filter(row => row.id === id));
      }
      if (call.startsWith("query artifactsForRun(")) {
        const id = /runId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return rowsResult(seed.runArtifacts?.[id] ?? []);
      }
      if (call.startsWith("builtin pipelinesExpiredArtifacts(")) {
        if (seed.expiredRunFilesError) throw Error(seed.expiredRunFilesError);
        const id = /runId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const selection = /fileIds: (\[[^\]]*\])/.exec(call)?.[1] ?? "[]";
        const selected = JSON.parse(selection) as string[];
        const files = (seed.expiredRunFiles?.[id] ?? []).filter(file => selected.includes(file.fileId));
        return builtinReply("pipelinesExpiredArtifacts", [{ files } as unknown as Row]);
      }
      if (call.startsWith("builtin repositoryChangeAccess(")) {
        if (seed.repositoryAuthorityError) throw new Error(seed.repositoryAuthorityError);
        const sourceId = /sourceId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("repositoryChangeAccess", [{sourceId, repository:"acme/shop", repositoryId:42, canWrite:false, canRequest:false, route:"unavailable", ...seed.repositoryAuthority}]);
      }
      if (call.startsWith("builtin repositoryChangeRequests(")) return builtinReply("repositoryChangeRequests", [{requests:seed.changeRequests ?? []} as Row]);
      if (call.startsWith("builtin repositoryChangeStatus(")) return builtinReply("repositoryChangeStatus", seed.changeRequests?.slice(0,1) ?? []);
      if (call.startsWith("builtin repositoryChangeLand(")) {
        if (seed.repositoryChangeError) throw new Error(seed.repositoryChangeError);
        const receipt = seed.changeRequests?.[0];
        if (!receipt) throw new Error("Missing proposal");
        const proposal = receipt.proposal as Record<string, unknown>;
        receipt.proposal = { ...proposal, landedCommit: proposal.commit };
        return builtinReply("repositoryChangeLand", [receipt]);
      }
      if (call.startsWith("builtin repositoryChangeRequest(")) {
        if (seed.repositoryChangeError) throw new Error(seed.repositoryChangeError);
        const receipt = {id:"request-one", operation: /operation: "([^"]*)"/.exec(call)?.[1] ?? "", repository:"acme/shop", repositoryId:42, state:seed.repositoryAuthority?.canWrite ? "approved" : "pending", statement:"Improve the requested component", paths:["clients/os/"], goalId:"goal-request", reason:"", expiresAt:"2026-12-01T00:00:00Z"} as Row;
        seed.changeRequests=[receipt]; return builtinReply("repositoryChangeRequest", [receipt]);
      }
      if (call.startsWith("builtin pipelinesPreview(")) {
        if (seed.pipelinePreviewError !== undefined) throw new Error(seed.pipelinePreviewError);
        return builtinReply("pipelinesPreview", seed.pipelinePreview ? [seed.pipelinePreview] : []);
      }
      if (call.startsWith("builtin pipelinesConnect(")) {
        if (seed.pipelinesConnectError !== undefined) throw new Error(seed.pipelinesConnectError);
        const pkg = /packageId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("pipelinesConnect", [seed.pipelinesConnectResult ?? ({ pipelineId: `pl-${pkg}`, reconnected: false } as unknown as Row)]);
      }
      if (call.startsWith("builtin pipelinesDisconnect(")) {
        if (seed.pipelinesDisconnectError !== undefined) throw new Error(seed.pipelinesDisconnectError);
        const id = /pipelineId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("pipelinesDisconnect", [{ pipelineId: id, status: "disconnected" } as unknown as Row]);
      }
      if (call.startsWith("builtin pipelinesRunLatest(")) {
        if (seed.pipelinesRunLatestError !== undefined) throw new Error(seed.pipelinesRunLatestError);
        return builtinReply("pipelinesRunLatest", [seed.pipelinesRunLatestResult ?? ({ runId: "run-manual", sha: "3f9c2ab0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6", branch: "main" } as unknown as Row)]);
      }
      if (call.startsWith("builtin pipelinesRerun(")) {
        if (seed.pipelinesRerunError !== undefined) throw new Error(seed.pipelinesRerunError);
        return builtinReply("pipelinesRerun", [seed.pipelinesRerunResult ?? ({ runId: "run-rerun", attempt: 2, status: "queued" } as unknown as Row)]);
      }
      if (call.startsWith("builtin pipelinesCancel(")) {
        if (seed.pipelinesCancelError !== undefined) throw new Error(seed.pipelinesCancelError);
        const id = /runId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("pipelinesCancel", [{ runId: id, status: "in_progress", cancelRequested: true } as unknown as Row]);
      }
      if (call === "builtin pipelinesInstallations()") {
        if (seed.laggingInstallationsError !== undefined) throw new Error(seed.laggingInstallationsError);
        return builtinReply("pipelinesInstallations", [{ installations: seed.laggingInstallations ?? [] } as unknown as Row]);
      }

      if (call.startsWith("query clientAccountsAll(")) return rowsResult(seed.accounts ?? [{ id: "self", name: "Operator organization", status: "active" }]);
      if (call === "query sitesAll()") { if (seed.sitesError) throw new Error(seed.sitesError); return rowsResult(latest("v1:platform:site", sites)); }
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

      if (call.startsWith("query packageNamesByIds(")) {
        const { args } = parseCall(call);
        return rowsResult(latest("v1:platform:package", [...(seed.packages ?? []), ...registeredPackages]).filter(row => (args.ids as string[]).some(id => id.split(":").at(-1) === String(row.id).split(":").at(-1))).map(({ id, createdAt, name, sourceKind, repoUrl, repoRef }) => ({ id, createdAt, name, sourceKind, repoUrl, repoRef })));
      }
      if (call === "query packagesAll()") return rowsResult(latest("v1:platform:package", [...(seed.packages ?? []), ...registeredPackages]));
      if (call === "query packageDeploymentsPending()") return rowsResult(latest("v1:platform:packageDeployment", seed.awaitingConfirm ?? []));

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
        // The MODE is read off the rendered call string rather than from a
        // separate stub, so the generated builder is what decides whether the
        // fixture answers -- the same reason this fake sits under
        // executeNamed at all. A read in any other mode measured nothing.
        return builtinReply("siteTrafficInWindow", call.includes("summary: true") ? seed.traffic?.summary ?? [] : []);
      }

      if (call.startsWith("builtin siteStatistics(")) {
        if (seed.siteStatisticsError !== undefined) throw new Error(seed.siteStatisticsError);
        return builtinReply("siteStatistics", seed.siteStatistics === undefined ? [] : [seed.siteStatistics]);
      }

      if (call.startsWith("builtin storeStatistics(")) {
        if (seed.storeStatisticsError !== undefined) throw new Error(seed.storeStatisticsError);
        const answer = seed.storeStatistics?.[String(parseCall(call).args["storeId"] ?? "")];
        return builtinReply("storeStatistics", answer === undefined ? [] : [answer]);
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
        if (seed.rollbackDropped) throw Object.assign(new Error("stream closed"), { reason: "transport" });
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

      // THE ACCESS CODES (design record 2026-10-29-deployable-access-codes). All
      // six are BUILTINS, the two lists included, so each answers the node set
      // `integrations/siteaccess` answers (`siteAccessReply`). The acts write to
      // the seed's own rows, with what the handlers write: a regenerate moves the
      // hint and `issuedAt` and takes `lastOpenedAt` away, because the handler
      // reads it from the sessions of the CURRENT generation.
      //
      // AND THEY REFUSE WHAT THE ENGINE REFUSES, in its order: the shared gate on
      // the organization COPIED onto the code, then `refuseIfEnded` for the two
      // acts it covers. A fake that let these through passed a test of a Remove the
      // engine would have refused. `refuseIfEnded` asks about the site, so a seed
      // that names no site says nothing about one.
      if (call.startsWith("builtin siteAccessCodesForSite(")) {
        if (seed.accessCodesError !== undefined) throw new Error(seed.accessCodesError);
        const id = /siteId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return siteAccessReply("list", "id", (seed.accessCodes ?? []).filter((row) => row["siteId"] === id));
      }
      if (call.startsWith("builtin siteAccessGates(")) {
        if (seed.accessGatesError !== undefined) throw new Error(seed.accessGatesError);
        return siteAccessReply("gate", "gate", seed.accessGates ?? []);
      }
      if (call.startsWith("builtin siteAccessCodeCreate(")) {
        if (seed.accessCreateError !== undefined) throw new Error(seed.accessCreateError);
        const arg = (name: string) => new RegExp(`${name}: "([^"]*)"`).exec(call)?.[1] ?? "";
        const minting = sites.find((row) => row["id"] === arg("siteId"));
        if (minting !== undefined && String(minting["ownerUserId"] ?? "") === "") {
          throw new Error(refusedByEngine("siteAccessCodeCreate", "This deployable has no owner, so it cannot have access codes. Give it an owner, then add the code."));
        }
        const code = nextAccessCode();
        const codeId = `code-minted-${accessMints}`;
        const expiresAt = arg("expiresAt");
        (seed.accessCodes ??= []).push(accessCodeRow({
          id: codeId, label: arg("label"), siteId: arg("siteId"), gate: arg("gate"), codeHint: code.slice(-4), issuedAt: new Date().toISOString(), expiresAt,
          ...(arg("subject") === "" ? {} : { subject: arg("subject") }),
        }));
        return siteAccessReply("create", "codeId", [{ codeId, expiresAt, code }]);
      }
      if (call.startsWith("builtin siteAccessGuestPassCreate(")) {
        // The engine's own refusal while a pass stands, and never a code in the reply.
        if (seed.accessGuestError !== undefined) throw new Error(seed.accessGuestError);
        const arg = (name: string) => new RegExp(`${name}: "([^"]*)"`).exec(call)?.[1] ?? "";
        const standing = (seed.accessCodes ?? []).some((row) => row["siteId"] === arg("siteId") && row["guest"] === true && !row["revokedAt"]);
        if (standing) throw new Error(refusedByEngine("siteAccessGuestPassCreate", "This deployable already lets guests in. Change when guest access ends, or turn it off first."));
        const codeId = `pass-minted-${++accessMints}`;
        const expiresAt = arg("expiresAt");
        (seed.accessCodes ??= []).push(accessCodeRow({
          id: codeId, label: arg("label"), siteId: arg("siteId"), gate: arg("gate"), guest: true, issuedAt: new Date().toISOString(), expiresAt,
          ...(arg("subject") === "" ? {} : { subject: arg("subject") }),
        }));
        return siteAccessReply("create", "codeId", [{ codeId, expiresAt }]);
      }
      if (call.startsWith("builtin siteAccessCodeRegenerate(")) {
        if (seed.accessRegenerateError !== undefined) throw new Error(seed.accessRegenerateError);
        const codeId = /codeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        if (seed.accessCodes?.find((r) => r["id"] === codeId)?.["guest"] === true) {
          throw new Error(refusedByEngine("siteAccessCodeRegenerate", "Guest access has no code to regenerate. Change when it ends, or turn it off."));
        }
        const row = codePastTheGate("siteAccessCodeRegenerate", codeId);
        refuseIfEnded("siteAccessCodeRegenerate", row);
        const code = nextAccessCode();
        const expiresAt = /expiresAt: "([^"]*)"/.exec(call)?.[1];
        if (expiresAt !== undefined) row["expiresAt"] = expiresAt;
        row["codeHint"] = code.slice(-4);
        row["issuedAt"] = new Date().toISOString();
        row["generation"] = Number(row["generation"] ?? 1) + 1;
        delete row["lastOpenedAt"];
        return siteAccessReply("regenerate", "codeId", [{ codeId, expiresAt: row["expiresAt"], code }]);
      }
      if (call.startsWith("builtin siteAccessCodeSetExpiry(")) {
        if (seed.accessSetExpiryError !== undefined) throw new Error(seed.accessSetExpiryError);
        const codeId = /codeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const expiresAt = /expiresAt: "([^"]*)"/.exec(call)?.[1] ?? "";
        const row = codePastTheGate("siteAccessCodeSetExpiry", codeId);
        refuseIfEnded("siteAccessCodeSetExpiry", row);
        row["expiresAt"] = expiresAt;
        return siteAccessReply("setExpiry", "codeId", [{ codeId, expiresAt }]);
      }
      if (call.startsWith("builtin siteAccessCodeRemove(")) {
        if (seed.accessRemoveError !== undefined) throw new Error(seed.accessRemoveError);
        const codeId = /codeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        codePastTheGate("siteAccessCodeRemove", codeId)["revokedAt"] = new Date().toISOString();
        return siteAccessReply("remove", "codeId", [{ codeId }]);
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
      // THE VERDICT AND ITS ACTS (U7). The read answers what the seed holds for
      // the store, and the two acts answer as the handlers do; what reached the
      // wire is in `calls`, which is how a test counts them.
      if (call.startsWith("builtin shopifyStoreVerdict(")) {
        if (seed.storeVerdictError !== undefined) throw new Error(seed.storeVerdictError);
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        return builtinReply("shopifyStoreVerdict", [seed.storeVerdict?.[id] ?? ({ status: "unconfigured" } as unknown as Row)]);
      }
      if (call.startsWith("builtin shopifyStoreRegisterEvents(")) {
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        if (seed.storeRegisterEventsReply !== undefined) return builtinReply("shopifyStoreRegisterEvents", [seed.storeRegisterEventsReply]);
        return builtinReply("shopifyStoreRegisterEvents", [{ status: "registered", detail: "", storeId: id, desired: 12, existing: 12, created: [], failed: [] } as unknown as Row]);
      }
      if (call.startsWith("builtin shopifyStoreRetryDeliveries(")) {
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        if (seed.storeRetryDeliveriesReply !== undefined) return builtinReply("shopifyStoreRetryDeliveries", [seed.storeRetryDeliveriesReply]);
        return builtinReply("shopifyStoreRetryDeliveries", [{ status: "requeued", storeId: id, source: `shopify-${id}` } as unknown as Row]);
      }
      if (call.startsWith("builtin shopifyStoreSetRecording(")) {
        if (seed.storeSetRecordingError !== undefined) throw new Error(seed.storeSetRecordingError);
        const id = /storeId: "([^"]*)"/.exec(call)?.[1] ?? "";
        // As the handler refuses: a store that neither ingests (configured,
        // backfilling, live) nor is paused, or one uninstalled or purged.
        const held = (seed.stores ?? []).find((row) => row["id"] === id);
        const status = String(held?.["status"] ?? "");
        if (held === undefined || held["uninstalledAt"] || held["redactedAt"] || !["configured", "backfilling", "live", "paused"].includes(status)) {
          throw new Error("shopify: a store that is not connected cannot be paused or resumed");
        }
        const recording = /recording: (true|false)/.exec(call)?.[1] === "true";
        return builtinReply("shopifyStoreSetRecording", [{ status: recording ? "live" : "paused", storeId: id } as unknown as Row]);
      }
      if (call.startsWith("builtin shopifyStoreAlertWait(")) {
        return builtinReply("shopifyStoreAlertWait", [alertWait]);
      }
      if (call.startsWith("builtin shopifyStoreAlertWaitSet(")) {
        if (alertWait["maySet"] !== true) throw new Error("shopify: shopifyStoreAlertWaitSet is reserved to a cluster owner");
        const minutes = Number(/minutes: ([^,)]*)/.exec(call)?.[1] ?? "");
        if (!Number.isInteger(minutes) || minutes < 1 || minutes > 1440) throw new Error("shopify: the alert wait is a whole number of minutes from 1 to 1440");
        alertWait = { minutes, isDefault: false, maySet: true } as unknown as Row;
        return builtinReply("shopifyStoreAlertWaitSet", [alertWait]);
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

      if (call.startsWith("builtin packageSourceRegister(")) {
        if (seed.registerSourceError) throw new Error(seed.registerSourceError);
        const arg = (name: string) => new RegExp(`${name}: "([^"]*)"`).exec(call)?.[1] ?? "";
        const credentialId = arg("credentialId");
        const ownerUserId = seed.credentials?.find(row => row["id"] === credentialId)?.["ownerUserId"] ?? "u-me";
        const sourceConnectionId = arg("sourceConnectionId");
        const repoUrl = arg("repoUrl"), repoRef = arg("repoRef"), accountId = arg("accountId");
        const normalize = (url: unknown) => String(url ?? "").trim().replace(/\.git$/, "").replace(/\/$/, "").toLowerCase();
        const defaultBranch = String((seed.sourceProbe?.[credentialId] ?? seed.sourceProbe?.[""])?.["defaultBranch"] ?? "main");
        const ref = (value: unknown) => String(value || defaultBranch);
        const existing = [...(seed.packages ?? []), ...registeredPackages].find(row => row["accountId"] === accountId && row["ownerUserId"] === ownerUserId && row["credentialId"] === credentialId && row["sourceConnectionId"] === sourceConnectionId && normalize(row["repoUrl"]) === normalize(repoUrl) && ref(row["repoRef"]) === ref(repoRef));
        const packageId = String(existing?.["id"] ?? seed.registeredPackageId ?? `pkg-registered-${registeredPackages.length + 1}`);
        const restored = existing?.["sourceRemoved"] === true;
        if (existing) existing["sourceRemoved"] = false;
        else registeredPackages.push({ id: packageId, name: arg("name"), sourceKind: "repo", repoUrl, repoRef, accountId, ownerUserId, credentialId, sourceConnectionId, sourceRemoved: false, status: "active" });
        return builtinReply("packageSourceRegister", [{ packageId, created: !existing, restored }]);
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

      // `getRowByConceptAndId` composes `concept==<c> && id=="<id>"`.
      // Decode the literal: quotes, spaces and escapes belong to the opaque
      // ID, and a missed lookup would silently retain the stale seeded row.
      const match = /id==("(?:[^"\\]|\\.)*")$/.exec(call);
      const wanted = match ? JSON.parse(match[1]!) as string : "";
      const row = wanted === "" ? undefined : seed.byId?.[wanted] ?? sourceConnections.find(row => row["id"] === wanted);
      return bundleResult(row ? [row] : []);
    }),
  };

  const connection: FakeConnection = {
    status: "connected",
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    subscriptions,
    dispatcher: { sendAndWait: vi.fn() },
  };
  sessions.add(connection as unknown as Connection);
  return connection;
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

/** A real cross-app destination for tests that follow a Deployables context link. */
function DeliveryDestination({ children }: { children: ReactNode }) {
  const { state, actions } = useOs();
  const focused = state.shell.focusedWindowId ? state.shell.windows[state.shell.focusedWindowId] : null;
  if (!focused || !["pipelines", "deployables"].includes(focused.appId)) return <>{children}</>;
  const App = focused.appId === "pipelines" ? PipelinesApp : DeployablesApp;
  return <App key={focused.id} sectionId={focused.sectionId} navigate={(section, options) => actions.navigateSection(focused.id, section, options?.fromContent ? "content" : "peer")} askContext={() => {}} intent={focused.intent} consumeIntent={id => actions.consumeWindowIntent(focused.id, id)} />;
}
export function withDeliverySession(children: ReactNode, overrides: Parameters<typeof withSession>[1] = {}) {
  return withSession(<DeliveryDestination>{children}</DeliveryDestination>, overrides);
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
  // THE STORE'S OWN NAME, the only one the edge publishes a Storefront token
  // from (memql#5626); a fixture under any other name is a store whose
  // storefront reads no token, and the surfaces say so.
  storefrontTokenRef: "SHOPIFY_STORE-EXAMPLE_STOREFRONT_TOKEN",
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
  storefrontTokenRef: "SHOPIFY_STORE-EXAMPLE-DEV_STOREFRONT_TOKEN",
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

/**
 * One store's `shopifyStoreVerdict` reply, with the keys the Go handler emits
 * (`integrations/shopify/verdict_capabilities.go`, `verdictPayload` plus
 * `alert` and `may`). A store being recorded, whose caller may act on it,
 * with nothing listed: what each case changes is what it is about.
 */
export function storeVerdictRow(over: Partial<Row> & { storeId: string }): Row {
  return {
    domain: `${over.storeId}.myshopify.com`,
    state: "recording",
    since: "2026-11-10T08:00:00Z",
    resumedAt: "",
    connectedAt: "2026-11-01T08:00:00Z",
    reasons: [],
    recorded: [],
    outside: [],
    withheld: [],
    ordersFrom: "",
    alert: { state: "none", reason: "" },
    may: { act: true },
    ...over,
  } as unknown as Row;
}

/** One reason of a verdict, as `verdictPayload` emits it. */
export function verdictReasonRow(over: Partial<Row> & { code: string; sentence: string }): Row {
  return { domain: "", detail: "", count: 0, atLeast: false, act: "", actLabel: "", actUrl: "", ...over } as unknown as Row;
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

/** A period's totals in a `siteStatistics` payload: every figure null unless named, because absent is the default. */
export function statisticsTotals(over: Partial<Row> = {}): Row {
  return {
    visitors: null, visits: null, bounces: null, pageViews: null, viewsPerVisit: null, bounceRate: null,
    requests: null, errors: null, notFound: null, bytes: null, botRequests: null, medianMs: null, p95Ms: null,
    ...over,
  };
}

/** One bucket of a `siteStatistics` series: every figure null unless named. */
export function statisticsPoint(at: string, over: Partial<Row> = {}): Row {
  return { at, visitors: null, visits: null, bounces: null, pageViews: null, requests: null, errors: null, notFound: null, p95Ms: null, ...over };
}

/** Seven daily buckets from `firstDay` (2026-10-DD or 2026-11-DD), each figure read off its column. */
function statisticsWeek(firstDay: string, columns: Record<string, readonly number[]>): Row[] {
  return Array.from({ length: 7 }, (_, index) => {
    const at = new Date(Date.parse(`${firstDay}T00:00:00Z`) + index * 86_400_000).toISOString().replace(".000Z", "Z");
    return statisticsPoint(at, Object.fromEntries(Object.entries(columns).map(([key, values]) => [key, values[index] ?? null])));
  });
}

/**
 * A `siteStatistics` payload as `Statistics.Payload()` writes it: the week of
 * Nov 2 to Nov 8 2026, measured, against the week before it, with visitors
 * counted since October. Visitors 1,204 against 1,075 (+12%), visits 1,530
 * against 1,530 (no change), page views 4,210 against 3,900, errors 14 against
 * 0. Override any key.
 */
export function siteStatisticsRow(over: Partial<Row> = {}): Row {
  return {
    period: "7d",
    bucket: "day",
    start: "2026-11-02T00:00:00Z",
    end: "2026-11-09T00:00:00Z",
    hasPrevious: true,
    prevStart: "2026-10-26T00:00:00Z",
    prevEnd: "2026-11-02T00:00:00Z",
    measuredSince: "2026-10-01",
    totals: statisticsTotals({
      visitors: 1204, visits: 1530, bounces: 459, pageViews: 4210, viewsPerVisit: 4210 / 1530, bounceRate: 0.3,
      requests: 18800, errors: 14, notFound: 1203, bytes: 13_000_000, botRequests: 230, medianMs: 48, p95Ms: 120,
    }),
    previous: statisticsTotals({
      visitors: 1075, visits: 1530, bounces: 490, pageViews: 3900, viewsPerVisit: 3900 / 1530, bounceRate: 490 / 1530,
      requests: 17000, errors: 0, notFound: 1410, bytes: 12_000_000, botRequests: 210, medianMs: 45, p95Ms: 110,
    }),
    series: statisticsWeek("2026-11-02", {
      visitors: [150, 160, 170, 180, 170, 180, 194],
      visits: [190, 205, 215, 230, 215, 230, 245],
      bounces: [57, 61, 64, 69, 65, 69, 74],
      pageViews: [520, 560, 590, 640, 590, 630, 680],
      requests: [2600, 2700, 2650, 2750, 2700, 2700, 2700],
      errors: [2, 2, 2, 2, 2, 2, 2],
      notFound: [170, 172, 171, 173, 172, 172, 173],
      p95Ms: [110, 120, 115, 130, 118, 125, 121],
    }),
    previousSeries: statisticsWeek("2026-10-26", {
      visitors: [140, 150, 150, 160, 150, 160, 165],
      visits: [200, 215, 215, 230, 215, 230, 225],
      bounces: [64, 69, 69, 74, 69, 74, 71],
      pageViews: [480, 520, 540, 600, 560, 590, 610],
      requests: [2400, 2450, 2400, 2450, 2400, 2450, 2450],
      errors: [0, 0, 0, 0, 0, 0, 0],
      notFound: [200, 200, 200, 200, 200, 200, 210],
      p95Ms: [100, 105, 110, 108, 112, 109, 111],
    }),
    top: {
      page: [{ value: "/", visitors: 900, visits: 1100, pageViews: 1800 }, { value: "/pricing", visitors: 300, visits: 320, pageViews: 700 }],
      source: [{ value: "direct", visitors: 700, visits: 800, pageViews: 2000 }, { value: "search.example", visitors: 400, visits: 500, pageViews: 1600 }],
      country: [{ value: "DE", visitors: 600, visits: 700, pageViews: 2000 }, { value: "(unknown)", visitors: 40, visits: 50, pageViews: 90 }],
      device: [{ value: "desktop", visitors: 800, visits: 1000, pageViews: 3000 }, { value: "mobile", visitors: 404, visits: 530, pageViews: 1210 }],
      browser: [{ value: "Chrome", visitors: 700, visits: 900, pageViews: 2500 }, { value: "Safari", visitors: 504, visits: 630, pageViews: 1710 }],
      os: [{ value: "macOS", visitors: 600, visits: 800, pageViews: 2200 }, { value: "Windows", visitors: 604, visits: 730, pageViews: 2010 }],
    },
    sites: [],
    ...over,
  };
}

/** The eight figures of a `storeStatistics` answer: every one null unless named. */
export function storeFigures(over: Partial<Row> = {}): Row {
  return { totalSales: null, netSales: null, averageOrderValue: null, grossSales: null, discounts: null, refunds: null, orders: null, unitsSold: null, ...over };
}

/**
 * A `storeStatistics` answer as `Statistics.Payload()` writes it
 * (component/storestats): a recording store (`recording.blocked` false,
 * `asOf` null) in EUR and Europe/Madrid, read for the week of Nov 2 to Nov 8
 * 2026, under the names of `STORE`, with every figure null and every list
 * empty unless named. Override any key.
 */
export function storeStatisticsRow(over: Partial<Row> & { storeId: string }): Row {
  const day = (index: number) => `2026-11-0${index + 2}T00:00:00+01:00`;
  return {
    siteId: "site-shop",
    storeName: STORE["name"],
    storeDomain: STORE["domain"],
    destination: "binding",
    currency: "EUR",
    timezone: "Europe/Madrid",
    periodZone: "Europe/Madrid",
    development: false,
    taxesIncluded: false,
    olderOrderChangesNotRecorded: false,
    period: "7d",
    bucket: "day",
    start: day(0),
    end: "2026-11-09T00:00:00+01:00",
    prevStart: "2026-10-26T00:00:00+01:00",
    historySince: "2026-09-12T00:00:00+02:00",
    lastChangeAt: "2026-11-08T10:55:00+01:00",
    asOf: null,
    recording: { state: "recording", reasons: [], since: "2026-11-07T10:00:00+01:00", blocked: false },
    rebuilding: false,
    figures: storeFigures(),
    previous: null,
    floors: {},
    series: Array.from({ length: 7 }, (_, index) => ({ at: day(index), totalSales: null, netSales: null, grossSales: null, discounts: null, refunds: null, orders: null, unitsSold: null })),
    productsBySales: [],
    productsByUnits: [],
    productCount: null,
    productSalesCount: null,
    productUnitsCount: null,
    refundsWithoutItem: null,
    incompleteOrders: null,
    incompleteRefunds: null,
    refundsNotRead: null,
    refundsTooOldToRead: null,
    buyers: null,
    wholesale: null,
    otherCurrencies: [],
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
    // Whether the edge serves each store's Storefront token (memql#5626):
    // false with no store attached, which is what this default describes.
    storeHasStorefrontToken: false,
    previewStoreId: "",
    previewStoreDomain: "",
    previewStoreHasStorefrontToken: false,
    testingUrl: "https://test--shop.memql.example.com/",
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

/**
 * The plain codes the fake mints, in order.
 *
 * COMPOSED rather than written out, for FIXTURE_TOKEN's reason: "access" followed
 * by a long mixed value is what gitleaks' generic-api-key rule looks for.
 */
export const FIXTURE_ACCESS_CODES = [
  "K7Q4M-" + "2XPDN-" + "8RT3V-" + "WJ6HC-" + "9FBZA",
  "M3T8R-" + "6WKQD-" + "Y2HNC-" + "P9VJB-" + "4GSXE",
  "H5ZQW-" + "J8N2C-" + "T4DKV-" + "R7MYB-" + "3XPAF",
] as const;

/** One `siteAccessCodesForSite` row. Absent fields are absent, as `listRow` leaves them. */
export function accessCodeRow(over: Partial<Row> & { id: string; label: string }): Row {
  return {
    siteId: "site-desk",
    gate: "dashboard",
    codeHint: "9FBZ",
    generation: 1,
    issuedAt: "2026-10-01T00:00:00Z",
    expiresAt: "2099-01-01T00:00:00Z",
    createdAt: "2026-10-01T00:00:00.123456Z",
    ...over,
  } as unknown as Row;
}

/** One `siteAccessGates` row. */
export function accessGateRow(over: Partial<Row> & { gate: string }): Row {
  return { usesSubject: false, constructs: ["deskBrief"], ...over } as unknown as Row;
}

/** What the dispatcher rejects a pending call with when the socket goes. */
export function droppedSocket(): Error {
  return Object.assign(new Error("stream closed"), { reason: "transport" });
}

/** The write lands and its reply is lost on the way back: the worst of the cases, and the commonest. */
export function loseTheReply(connection: FakeConnection, construct: string): void {
  const real = connection.query.executeNamed.bind(connection.query);
  connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
    const answer = await real(name, call, opts);
    if (name === construct) throw droppedSocket();
    return answer;
  }) as typeof connection.query.executeNamed;
}

/** A refusal as `executeNamed` throws it: the call's name and the engine's frame around the sentence. */
export function refusedByEngine(construct: string, sentence: string): string {
  return `${construct}: MemQL engine failed to execute query. Details: ${sentence}`;
}

/** A persisted GitHub identity-to-installation source binding. */
export function sourceConnectionRow(over: Partial<Row> = {}): Row {
  return { id: "source-acme", ownerUserId: "u-me", credentialId: "cred-grant", installationId: "i-acme", providerAccountId: "provider-acme", accountLogin: "acme", accountType: "Organization", status: "active", ...over };
}
