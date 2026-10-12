import { readiness, verdict } from "../setup/harness";
import type { Readiness } from "../../src/live/readiness";
import type { ReactNode } from "react";
import { vi, type Mock } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Email app's test harness: a connection-shaped double.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason the
// Accounts, Users and Fleet harnesses record: every read goes through
// `connection.query.<generated method>` and every subscription through
// `connection.subscriptions`, so a fake that answers those exercises the real
// LiveCollection, the real retain/seed path, the real projections and the real
// arrival fold -- which is where the behaviour under test actually lives.

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

/** A full page with a continuation cursor, as the engine answers a list it has more of.
 *  An empty cursor is the last page, which is what `rowsResult` already gives. */
export function pagedResult(rows: Row[], cursor: string): Result {
  return new Result({ data: rows, meta: { cursor } } as never);
}

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
  /** A spy, so a test can say how many subscriptions a concept was asked for. */
  subscribeGraph: Mock<(handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void>;
  emit: (concept: string, payload: Row, kind?: string) => void;
  /** Concept -> subscriptions open right now (a released one no longer counts). */
  open: () => Map<string, number>;
  /** Concept -> the most subscriptions it ever had open at once. */
  peak: () => Map<string, number>;
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<{ handler: (event: FakeEvent) => void }>>();
  const peaks = new Map<string, number>();
  return {
    subscribeGraph: vi.fn((handler: (event: FakeEvent) => void, opts: { concept?: string }) => {
      const concept = opts.concept ?? "*";
      // One entry per CALL, so the same function subscribed twice counts twice.
      const entry = { handler };
      const set = handlers.get(concept) ?? new Set();
      set.add(entry);
      handlers.set(concept, set);
      peaks.set(concept, Math.max(peaks.get(concept) ?? 0, set.size));
      return () => { set.delete(entry); };
    }),
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const { handler } of [...(handlers.get(concept) ?? [])]) {
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
    open: () => new Map([...handlers].filter(([, set]) => set.size > 0).map(([concept, set]) => [concept, set.size])),
    peak: () => new Map(peaks),
  };
}

/** Concept -> how many live subscriptions the connection holds open now. The
 *  one-subscription-per-concept rule reads this: no value may exceed one. */
export function liveSubscriptions(connection: { subscriptions: FakeSubscriptions }): Map<string, number> {
  return connection.subscriptions.open();
}

/** Concept -> the most subscriptions it ever had open together. `liveSubscriptions`
 *  is a photograph; this is the film, so a doubling that lasted between two
 *  assertions (a lingering release, a restart on a changed key) is still seen. */
export function peakSubscriptions(connection: { subscriptions: FakeSubscriptions }): Map<string, number> {
  return connection.subscriptions.peak();
}

export interface FakeSeed {
  campaigns?: Row[];
  testSettings?: Row[];
  testRuns?: Row[];
  campaignSeries?: Row[];
  newsletters?: Row[];
  welcomes?: Row[];
  sites?: Row[];
  accounts?: Row[];
  audiences?: Row[];
  templates?: Row[];
  senderIdentities?: Row[];
  emailRules?: Row[];
  /** Pass an Error to make the on-demand read REFUSE, which is the
   *  interesting case: a refusal is not a zero. */
  deliveriesForCampaign?: Row[] | Error;
  recipientsForAudience?: Row[] | Error;
  campaignStats?: Row[] | Error;
  campaignsForAccount?: Row[] | Error;
  /**
   * The `clusterSettingsCurrent` reply. Absent = a cluster whose authored
   * automations are ON, because the halted state is the surprising one and a
   * harness whose default produced it would put a banner on every rules test.
   * Pass `[]` for a cluster with no settings row, `[{}]` for a row that does
   * not carry the field, and an Error for a read that failed.
   */
  clusterSettings?: Row[] | Error;
  /** The `integrationStatus` reply. Absent = a healthy, configured cluster,
   *  because the "needs configuration" banner is the surprising state and a
   *  harness whose default produced it would put a warning on every test. */
  integrationStatus?: Row[] | Error;
  sendingReadiness?: Row[] | Error;
  /**
   * The `emailAzureSetup` reply for one `{ accountId, action }`. Absent = a
   * cluster whose Azure is connected and whose organizations have no sending
   * domain yet, because the unset state is the one the Settings page draws its
   * Add control in and a harness whose default was a finished domain would hide
   * it. An Error rejects, which is how the cluster refuses.
   */
  azure?: (args: { accountId: string; action: string }) => Row | Error | Promise<Row | Error>;
  /** Audience id -> `audienceSubscriberCount`'s answer. An Error is a refused
   *  read; an audience not named here is an empty one (the engine answers 0). */
  subscriberCounts?: Record<string, number | Error>;
  concepts?: { id: string; domain: string; entity: string }[];
}

/**
 * A named read answering the seeded rows.
 *
 * `scope` is the query's own `filter`, for the reads the engine answers PER ID
 * (`campaignSeriesForCampaign`, `campaignTestSettings`, `newslettersForAudience`):
 * a double that handed every id the whole seed would let a feed that mixed one
 * id's rows into another's pass, and the composed feeds read exactly these.
 */
function reader(rows: Row[] | Error | undefined, scope?: (row: Row, args: Record<string, unknown>) => boolean) {
  return vi.fn(async (args?: unknown, _opts?: unknown) => {
    if (rows instanceof Error) throw rows;
    const wanted = (args ?? {}) as Record<string, unknown>;
    return rowsResult(scope ? (rows ?? []).filter((row) => scope(row, wanted)) : rows ?? []);
  });
}

const HEALTHY_EMAIL: Row = {
  checkedAt: "2026-09-01T12:00:00Z",
  probed: false,
  integrations: [
    { name: "email", registered: true, configured: "yes", health: "unknown", mode: "graph", detail: "" },
  ],
};

/** A cluster whose Azure is connected and whose organizations have no domain. */
const CONNECTED_AZURE = (args: { action: string }): Row =>
  args.action === "clusterStatus"
    ? { status: "connected", resourceGroup: "mail", dataLocation: "United States", capture: false }
    : { status: "unconfigured" };

/** A cluster whose authored automations are running. */
const RUNNING_AUTOMATIONS: Row = { id: "cluster", authoredAutomationsEnabled: true };

export function fakeConnection(seed: FakeSeed = {}) {
  return {
    query: {
      sitesAll: reader(seed.sites),
      newslettersForAudience: reader(seed.newsletters, (row, args) => row.audienceId === args.audienceId),
      newsletterWelcomesForAudience: reader(seed.welcomes),
      campaignConfigureNewsletter: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ saved: true }])),
      campaignRetryNewsletterWelcome: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ queued: true }])),
      // The five live seeds.
      campaigns: reader(seed.campaigns),
      campaignTestSettings: reader(seed.testSettings, (row, args) => row.accountId === args.accountId),
      campaignTestRuns: reader(seed.testRuns),
      campaignConfigureTestAudience: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ saved: true }])),
      campaignTestAudienceSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ testRunId: "test-run" }])),
      campaignSeriesForCampaign: reader(seed.campaignSeries, (row, args) => row.sourceCampaignId === args.campaignId),
      campaignConfigureSeries: reader([]),
      audiences: reader(seed.audiences),
      templates: reader(seed.templates),
      senderIdentities: reader(seed.senderIdentities),
      emailRules: reader(seed.emailRules),

      // The on-demand reads.
      deliveriesForCampaign: reader(seed.deliveriesForCampaign),
      recipientsForAudience: reader(seed.recipientsForAudience),
      audienceSubscriberCount: vi.fn(async (args: { audienceId?: string }, _opts?: unknown) => {
        const answer = seed.subscriberCounts?.[args.audienceId ?? ""];
        if (answer instanceof Error) throw answer;
        return rowsResult([{ count: answer ?? 0 }]);
      }),
      campaignStats: reader(seed.campaignStats),
      campaignsForAccount: reader(seed.campaignsForAccount),
      integrationStatus: reader(seed.integrationStatus ?? [HEALTHY_EMAIL]),
      campaignSendingReadiness: reader(seed.sendingReadiness ?? [{ ready: true, reason: "" }]),
      clusterSettingsCurrent: reader(seed.clusterSettings ?? [RUNNING_AUTOMATIONS]),
      // Settings -> Sending domain, and Package. The same two reads the
      // Connections page makes, because the domain's wizard is one component.
      emailAzureSetup: vi.fn(async (args: { accountId: string; action: string }, _opts?: unknown) => {
        const reply = await (seed.azure ?? CONNECTED_AZURE)(args);
        if (reply instanceof Error) throw reply;
        return rowsResult([reply]);
      }),
      packageCampaigns: vi.fn(async (_args: Record<string, unknown>, _opts?: unknown) => rowsResult([{ status: "installed", manifest: null }])),
      listConcepts: vi.fn(async () =>
        (seed.concepts ?? []).map((c) => ({
          ...c,
          version: "v1",
          description: "",
          type: "object",
        })),
      ),

      // The Accounts tie surface's own read, because the campaign form mounts
      // an AccountPicker.
      clientAccountsAll: vi.fn(async () => rowsResult(seed.accounts ?? [{ id: "self", name: "Operator organization", status: "active" }])),

      // TYPED ARGS, so `.mock.calls[0][0]` is a record rather than `never` --
      // a test that asserts WHICH arguments a write received cannot do it
      // through a `vi.fn(async () => ...)` whose parameter list is empty.
      createCampaign: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateCampaign: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      cancelCampaign: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignStartSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignScheduleSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignPauseSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignResumeSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignTestSend: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignImportRecipients: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      createAudience: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      archiveAudience: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      addRecipient: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      setRecipientSubscription: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignSaveTemplate: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ saved: true, revision: "2026-10-01T00:00:00Z" }])),
      createSenderIdentity: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateSenderIdentity: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      setSenderIdentityStatus: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      createEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      setEmailRuleStatus: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignActivateEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignRetireEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),

      executeNamed: vi.fn(async (_name: string, _filter: string) => bundleResult([])),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export type FakeConnection = ReturnType<typeof fakeConnection>;

export function withSession(children: ReactNode, overrides: { role?: string; readiness?: Readiness; accountIds?: string[]; everyAccount?: boolean } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          role: overrides.role ?? "owner",
          roleName: "",
          rank: 0,
          everyAccount: overrides.everyAccount ?? (overrides.role === undefined || overrides.role === "owner"),
          accountIds: overrides.accountIds ?? ["self"],
        },
        config,
        readiness: overrides.readiness ?? readiness(true, [verdict("email", "configured"), verdict("campaigns", "configured", false)]),
      }}
    >
      {children}
    </SessionProvider>
  );
}

/**
 * A campaign row with sane defaults, overridable field by field.
 *
 * A DRAFT WITH ZEROED COUNTERS is the default, because that is what
 * `createCampaign` writes and because a test that wants a send in flight
 * should have to ASK for one -- a harness whose default was mid-send would
 * make every unrelated test render a progress bar.
 */
export function campaignRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "August update",
    audienceId: "v1:campaigns:audience:a1",
    templateId: "v1:campaigns:template:t1",
    fromName: "",
    replyTo: "",
    scheduledAt: "",
    status: "draft",
    startedAt: "",
    completedAt: "",
    recipientCount: 0,
    sentCount: 0,
    failedCount: 0,
    skippedCount: 0,
    lastError: "",
    accountId: "self",
    senderIdentityId: "",
    trackOpens: true,
    trackClicks: true,
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function audienceRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Newsletter",
    description: "",
    status: "active",
    accountId: "self",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function templateRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "August copy",
    subject: "What we shipped",
    textBody: "Hello {{displayName}},",
    htmlBody: "",
    status: "ready",
    accountId: "self",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function senderRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    address: "news@acme.com",
    fromName: "Acme News",
    replyTo: "",
    accountId: "self",
    status: "active",
    notes: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function ruleRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Tell the owner about new admins",
    description: "",
    accountId: "self",
    triggerConcept: "v1:identity:user",
    eventKind: "created",
    condition: "",
    templateId: "v1:campaigns:template:t1",
    recipientMode: "cluster_roles",
    recipientRoles: [],
    audienceId: "",
    recipientField: "",
    senderIdentityId: "",
    status: "draft",
    bundleId: "",
    constructName: "",
    lastError: "",
    lastFiredAt: "",
    firedCount: 0,
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function recipientRow(over: Partial<Row> & { id: string }): Row {
  return {
    audienceId: "v1:campaigns:audience:a1",
    email: "dana@acme.com",
    displayName: "Dana",
    subscriptionStatus: "subscribed",
    unsubscribedAt: "",
    source: "import",
    fields: {},
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function deliveryRow(over: Partial<Row> & { id: string }): Row {
  return {
    campaignId: "v1:campaigns:campaign:c1",
    recipientId: "v1:campaigns:recipient:r1",
    email: "dana@acme.com",
    status: "sent",
    outboundRequestId: "",
    skipReason: "",
    lastError: "",
    sentAt: "2026-08-02T09:00:00Z",
    createdAt: "2026-08-02T09:00:00Z",
    ...over,
  };
}
