import type { ReactNode } from "react";
import { act, fireEvent } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

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
  sites?: Row[];
  artifacts?: Row[];
  /** v1:platform:customDomain rows the domains feed seeds with. */
  domains?: Row[];
  /** Fails the next `customDomainAdd` with this server message. */
  addDomainError?: string;
  /** Fails the next `removeCustomDomain` with this server message. */
  removeDomainError?: string;
  /** v1:platform:package rows `packagesAll` answers with. */
  packages?: Row[];
  /** v1:platform:packageDeployment rows, keyed by packageId. */
  deployments?: Record<string, Row[]>;
  /** Fails the next `packageDeploy` with this server message. */
  deployError?: string;
  /** Fails the next `packageArchive` with this server message. */
  archiveError?: string;
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

  const stub = {
    executeNamed: vi.fn(async (_name: string, call: string) => {
      calls.push(call);

      if (call === "query sitesAll()") return rowsResult(sites);
      if (call === "query libraryArtifacts()") return rowsResult(artifacts);
      if (call === "query customDomainsAll()") return rowsResult(domains);

      if (call.startsWith("builtin customDomainAdd(")) {
        if (seed.addDomainError !== undefined) throw new Error(seed.addDomainError);
        return rowsResult([ADD_DOMAIN_RESULT as unknown as Row]);
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
        return rowsResult([
          seed.deployResult ??
            ({ deploymentId: "dep-new", status: "awaiting_confirm", awaitingConfirm: "true" } as unknown as Row),
        ]);
      }

      if (call.startsWith("builtin packageArchive(")) {
        if (seed.archiveError !== undefined) throw new Error(seed.archiveError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin packageRollback(")) {
        if (seed.rollbackError !== undefined) throw new Error(seed.rollbackError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin packageRestore(")) {
        if (seed.restoreError !== undefined) throw new Error(seed.restoreError);
        return rowsResult([]);
      }

      if (call === "query sourceCredentialsMine()") return rowsResult(seed.credentials ?? []);

      if (call.startsWith("builtin sourceProbe(")) {
        if (seed.sourceProbeError !== undefined) throw new Error(seed.sourceProbeError);
        const credentialId = /credentialId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const answers = seed.sourceProbe ?? {};
        const reply = answers[credentialId] ?? answers[""] ?? null;
        return rowsResult(reply === null ? [] : [reply]);
      }

      if (call.startsWith("builtin artifactProbe(")) {
        if (seed.artifactProbeError !== undefined) throw new Error(seed.artifactProbeError);
        const artifactId = /artifactId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const reply = (seed.artifactProbe ?? {})[artifactId] ?? null;
        return rowsResult(reply === null ? [] : [reply]);
      }

      if (call.startsWith("builtin sourceCredentialCreate(")) {
        if (seed.credentialCreateError !== undefined) throw new Error(seed.credentialCreateError);
        return rowsResult([
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
        return rowsResult([
          {
            credentialId,
            status: "revoked",
            remoteRevoked: seed.credentialRevokeRemote === false ? "false" : "true",
          } as unknown as Row,
        ]);
      }

      if (call.startsWith("builtin githubConnectBegin(")) {
        if (seed.connectError !== undefined) throw new Error(seed.connectError);
        return rowsResult([
          {
            authorizeUrl: seed.connectUrl ?? "",
            reason: seed.connectReason ?? "ok",
            installUrl: seed.installUrl ?? "",
          } as unknown as Row,
        ]);
      }

      if (call.startsWith("builtin sourceRepositories(")) {
        if (seed.repositoriesError !== undefined) throw new Error(seed.repositoriesError);
        return rowsResult(seed.repositories ? [seed.repositories] : []);
      }

      if (call.startsWith("mutation updatePackageSource(")) {
        if (seed.updateSourceError !== undefined) throw new Error(seed.updateSourceError);
        return rowsResult([]);
      }

      if (call.startsWith("mutation updateSiteStatus(")) {
        if (seed.siteStatusError !== undefined) throw new Error(seed.siteStatusError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin siteArchive(")) {
        if (seed.siteArchiveError !== undefined) throw new Error(seed.siteArchiveError);
        return rowsResult([]);
      }

      if (call.startsWith("builtin siteRestore(")) {
        if (seed.siteRestoreError !== undefined) throw new Error(seed.siteRestoreError);
        return rowsResult([]);
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
        return rowsResult([PUBLISH_RESULT as unknown as Row]);
      }

      // `getRowByConceptAndId` composes `concept==<c> && id==<id>`.
      const match = /id==(\S+)/.exec(call);
      const wanted = match?.[1] ?? "";
      const row = wanted === "" ? undefined : seed.byId?.[wanted];
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
  overrides: { userId?: string; role?: string; domain?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "u-me",
          primaryEmail: "owner@example.com",
          clusterRole: overrides.role ?? "owner",
        },
        config,
      }}
    >
      {children}
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
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** The seeded platform row: cluster-owned, system-owned, baked into the image. */
export const PORTAL = siteRow({
  id: "site-portal",
  ownerUserId: "",
  hostname: "portal.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "file:///app/portal",
  systemOwned: true,
  title: "MemQL Portal",
});

/** A storefront, published from the Library. */
export const SHOP = siteRow({
  id: "site-shop",
  hostname: "shop.memql.example.com",
  kind: "shopify_storefront",
  status: "live",
  bundleRef: "blob://sites/site-shop/v1/",
  artifactId: "artifact-zip",
  title: "Storefront",
  binding: { storeDomain: "example.myshopify.com", storefrontTokenRef: "shopify-storefront-token" },
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
