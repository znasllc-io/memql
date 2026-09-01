import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Campaigns app's test harness: a connection-shaped double.
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
  campaigns?: Row[];
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
  /** The `integrationStatus` reply. Absent = a healthy, configured cluster,
   *  because the "needs configuration" banner is the surprising state and a
   *  harness whose default produced it would put a warning on every test. */
  integrationStatus?: Row[] | Error;
  concepts?: { id: string; domain: string; entity: string }[];
}

function reader(rows: Row[] | Error | undefined) {
  return vi.fn(async (_args?: unknown, _opts?: unknown) => {
    if (rows instanceof Error) throw rows;
    return rowsResult(rows ?? []);
  });
}

const HEALTHY_EMAIL: Row = {
  checkedAt: "2026-09-01T12:00:00Z",
  probed: false,
  integrations: [
    { name: "email", registered: true, configured: "yes", health: "unknown", mode: "graph", detail: "" },
  ],
};

export function fakeConnection(seed: FakeSeed = {}) {
  return {
    query: {
      // The five live seeds.
      campaigns: reader(seed.campaigns),
      audiences: reader(seed.audiences),
      templates: reader(seed.templates),
      senderIdentities: reader(seed.senderIdentities),
      emailRules: reader(seed.emailRules),

      // The on-demand reads.
      deliveriesForCampaign: reader(seed.deliveriesForCampaign),
      recipientsForAudience: reader(seed.recipientsForAudience),
      campaignStats: reader(seed.campaignStats),
      campaignsForAccount: reader(seed.campaignsForAccount),
      integrationStatus: reader(seed.integrationStatus ?? [HEALTHY_EMAIL]),
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
      clientAccountsAll: vi.fn(async () => rowsResult([])),

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
      createTemplate: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateTemplate: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
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

export function withSession(children: ReactNode, overrides: { role?: string } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
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
    accountId: "",
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
    accountId: "",
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
    accountId: "",
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
    accountId: "",
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
    accountId: "",
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
