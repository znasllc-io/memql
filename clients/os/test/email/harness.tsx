import { readiness, verdict } from "../setup/harness";
import type { Readiness } from "../../src/live/readiness";
import type { ReactNode } from "react";
import { afterEach, vi, type Mock } from "vitest";
import { Result, SubscriptionManager, type Connection, type Dispatcher, type Row } from "@znasllc-io/memql-sdk-core/client";

import { disposeSessionCollections } from "../../src/live/sessionCollections";

const sessions = new Set<Connection>();
afterEach(() => { for (const connection of sessions) disposeSessionCollections(connection); sessions.clear(); });

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
  const active = new Map<string, { concept?: string }>();
  const peaks = new Map<string, number>();
  let deliver: (message: unknown) => void = () => {};
  const counts = () => {
    const result = new Map<string, number>();
    for (const options of active.values()) { const concept = options.concept ?? "*"; result.set(concept, (result.get(concept) ?? 0) + 1); }
    return result;
  };
  const dispatcher = {
    send(message: { subscribe?: { subscriptionId: string; concept?: string }; unsubscribe?: { subscriptionId: string } }) {
      if (message.subscribe) active.set(message.subscribe.subscriptionId, message.subscribe);
      if (message.unsubscribe) active.delete(message.unsubscribe.subscriptionId);
      for (const [concept, count] of counts()) peaks.set(concept, Math.max(peaks.get(concept) ?? 0, count));
      return "message";
    },
    addEventListener(handler: (message: unknown) => void) { deliver = handler; return () => {}; },
  } as unknown as Dispatcher;
  const manager = new SubscriptionManager(dispatcher);
  return {
    subscribeGraph: vi.fn((handler, opts) => manager.subscribeGraph(handler, opts)),
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const [subscriptionId, options] of [...active]) {
        if (options.concept && options.concept !== concept) continue;
        deliver({ event: { subscriptionId, kind: `EVENT_KIND_${kind}`, payload, seq: 0, gapBefore: false } });
      }
    },
    open: counts,
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
  journeys?: Row[];
  enrollmentCounts?: Record<string, number>;
  /** Automation runs, as `journeyEnrollmentsByStatus` lists them (journeyEnrollmentFull). */
  enrollments?: Row[];
  proposal?: Row;
  /** Pass an Error to make the on-demand read REFUSE, which is the
   *  interesting case: a refusal is not a zero. */
  deliveriesForCampaign?: Row[] | Error;
  recipientsForAudience?: Row[] | Error;
  campaignStats?: Row[] | Error;
  /** `emailStatistics`' answer, whatever it is asked (`statisticsRow` builds one). Absent is an answer with no row in it. */
  emailStatistics?: Row[] | Error;
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
   * `campaignSendingReadiness` for one sender (`{ accountId, senderIdentityId }`),
   * by sender id, or as a function of it (asked on every read). A sender not
   * answered here gets `sendingReadiness`. An Error refuses that sender's check.
   */
  senderReadiness?: Record<string, Row | Error> | ((senderIdentityId: string) => Row | Error | undefined);
  /**
   * The `emailAzureSetup` reply for one `{ accountId, action }`. Absent = a
   * cluster whose Azure is connected and whose organizations have no sending
   * domain yet, because the unset state is the one the Settings page draws its
   * Add control in and a harness whose default was a finished domain would hide
   * it. An Error rejects, which is how the cluster refuses.
   */
  azure?: (args: { accountId: string; action: string }) => Row | Error | Promise<Row | Error>;
  /** Audience id -> `audienceSubscriberCount`'s answer. A number is an exact
   *  count; `{ atLeast: n }` is a count that stopped at the engine's window n
   *  (memql#6033); an Error is a refused read; an audience not named here is an
   *  empty one (the engine answers 0). */
  subscriberCounts?: Record<string, number | { atLeast: number } | Error>;
  concepts?: { id: string; domain: string; entity: string }[];
  /**
   * Template id -> what `templateById` answers beyond the list row: the
   * document and the bodies (the engine's templateFull). A template not named
   * here is read as its list row with an empty custom body and no document; an
   * Error refuses the read. A template the `templates` seed does not hold is
   * not found.
   */
  templateContent?: Record<string, { document?: Row | null; textBody?: string; htmlBody?: string } | Error>;
  /**
   * Organization id -> its OWN layouts, as `emailLayouts` lists them (the
   * builtin puts the four built-ins first, and so does this double). An Error
   * refuses that organization's reading; an organization not named here has
   * the built-ins alone.
   */
  layouts?: Record<string, Row[] | Error>;
  /** `emailRender`'s answer for a call, or one answer for every call. Absent: the subject, as HTML and as text, and no issues. */
  render?: Row | Error | ((args: { document?: Record<string, unknown>; accountId?: string }) => Row | Error);
  /** `searchUsers`: the people this session can name. An Error is a reader below the roster's floor. */
  people?: Row[] | Error;
  /** `libraryArtifactById`: Library files the session owns. */
  artifacts?: Row[];
  /**
   * `brandKitForAccount`'s rows (brandKitFull): `{id, ownerUserId, accountId,
   * kit, createdAt}`, read by organization. An organization not named has no
   * kit; an Error refuses the read. A `campaignSaveBrandKit` is read back, as
   * the engine's is.
   */
  brandKits?: Row[] | Error;
  /** `campaignBrandPreview`'s answer for a call, or one answer for every call. Absent: a line in the kit's primary colour, and no warnings. */
  brandPreview?: Row | Error | ((args: { accountId?: string; kit?: Record<string, unknown>; layoutId?: string }) => Row | Error);
}

/** The four layouts every organization starts from, as `emailLayouts` lists them. */
export const BUILT_IN_LAYOUTS: Row[] = [
  { id: "builtin:confirmation", kind: "builtin", name: "Confirmation", description: "A welcome on your brand colour with one button that confirms the subscription.", status: "active", document: { subject: "Confirm your subscription", blocks: [] } },
  { id: "builtin:letter", kind: "builtin", name: "Letter", description: "A personal note with a signature band, a promotion and social links.", status: "active", document: { subject: "A note from us", blocks: [] } },
  { id: "builtin:article", kind: "builtin", name: "Article", description: "A banner, a tinted article body with a highlighted statement, and a signature.", status: "active", document: { subject: "Three small changes", blocks: [] } },
  { id: "builtin:spotlight", kind: "builtin", name: "Spotlight", description: "One large picture, a short message and one button.", status: "active", document: { subject: "One thing to see", blocks: [] } },
];

/** An organization's own layout, as `emailLayouts` lists it: an opaque id, the bare one beside it, and its revision. */
export function layoutRow(over: Partial<Row> & { layoutId: string }): Row {
  return {
    id: `org:${over.layoutId}`,
    kind: "organization",
    name: "Acme letter",
    description: "Our letter, with the logo and the footer.",
    document: { subject: "From Acme", blocks: [] },
    status: "active",
    sourceArtifactId: "",
    revision: "2026-10-03T09:00:00Z",
    ...over,
  };
}

/** What `campaignBrandPreview` answers by default: a line in the kit's primary colour, and no warnings. */
function plainBrandPreview(args: { kit?: Record<string, unknown> }): Row {
  const theme = args.kit?.["theme"] as Record<string, unknown> | undefined;
  const primary = typeof theme?.["primary"] === "string" ? (theme["primary"] as string) : "#000000";
  return { html: `<p style="color:${primary}">Three small changes for calmer mornings</p>`, warnings: [] };
}

/** What `emailRender` answers by default: the document's subject, as HTML and as text, and no issues. */
function plainRender(args: { document?: Record<string, unknown> }): Row {
  const subject = typeof args.document?.["subject"] === "string" ? (args.document["subject"] as string) : "";
  return { subject, html: `<p style="color:#2b2b2b">${subject}</p>`, text: subject, issues: [] };
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
  // The cluster as the live feed has told it: a template written while a test
  // runs (emitted) is read back whole by id, as the engine would answer it.
  const subscriptions = fakeSubscriptions();
  const written = new Map<string, Row>();
  const changes = new Map<string, Map<string, Row>>();
  // Each organization's brand kit, as saved: a save is read back by the next read.
  const brandKits: Row[] = seed.brandKits instanceof Error ? [] : [...(seed.brandKits ?? [])];
  let brandSaves = 0;
  const emit = subscriptions.emit;
  subscriptions.emit = (concept, payload, kind) => {
    const rows = changes.get(concept) ?? new Map<string, Row>();
    rows.set(String(payload.id), payload);
    changes.set(concept, rows);
    if (concept === "v1:campaigns:template" && typeof payload.id === "string") written.set(payload.id, payload);
    emit(concept, payload, kind);
  };
  const concepts: Record<string, Row[] | undefined> = { "v1:campaigns:campaign": seed.campaigns, "v1:campaigns:audience": seed.audiences, "v1:campaigns:template": seed.templates, "v1:campaigns:journey": seed.journeys, "v1:campaigns:emailRule": seed.emailRules, "v1:platform:site": seed.sites };
  const latest = (concept: string) => [...new Map([...(concepts[concept] ?? []).map(row => [String(row.id), row] as const), ...[...(changes.get(concept) ?? new Map())]]).values()];
  const browse = (kind: string, pageSize = 10) => vi.fn(async (args: Record<string, unknown> = {}, opts?: { cursor?: string }) => {
    const concept = `v1:campaigns:${kind}`;
    const needle = String(args.search ?? "").trim().toLowerCase();
    const rows = latest(concept).filter(row => {
      if (kind === "campaign" && row.testSourceCampaignId) return false;
      if (kind !== "campaign" && pageSize === 10 && !args.includeArchived && row.status === "archived") return false;
      if (args.accountId && row.accountId !== args.accountId) return false;
      if (args.confirmation && row.confirmation !== args.confirmation) return false;
      if (args.status && (args.status === "progress" ? ["sent", "cancelled", "failed"].includes(String(row.status)) : row.status !== args.status)) return false;
      if (args.trigger && (kind === "emailRule" ? "event" : (row.trigger as { kind?: string } | undefined)?.kind) !== args.trigger) return false;
      const keys = kind === "campaign" ? ["name", "fromName", "replyTo"] : kind === "template" ? ["name", "subject", "preheader"] : kind === "audience" && pageSize === 200 ? ["name"] : ["name", "description", "triggerConcept"];
      return !needle || keys.some(key => String(row[key] ?? "").toLowerCase().includes(needle)) || kind === "campaign" && Array.isArray(args.audienceIds) && args.audienceIds.includes(row.audienceId);
    }).sort((a, b) => String(b.createdAt ?? "").localeCompare(String(a.createdAt ?? "")) || String(a.id).localeCompare(String(b.id)));
    const offset = Number(opts?.cursor?.replace("offset:", "") ?? 0);
    return pagedResult(rows.slice(offset, offset + pageSize), offset + pageSize < rows.length ? `offset:${offset + pageSize}` : "");
  });
  const connection = {
    status: "connected",
    query: {
      campaignsBrowse: browse("campaign"),
      audiencesBrowse: browse("audience"),
      templatesBrowse: browse("template"),
      journeysBrowse: browse("journey"),
      emailRulesBrowse: browse("emailRule"),
      audienceNamesMatching: browse("audience", 200),
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
      journeys: reader(seed.journeys),
      journeyById: reader(seed.journeys, (row, args) => row.id === args.journeyId),
      journeyEnrollmentCounts: vi.fn(async (args: Record<string, unknown>, _opts?: unknown) => rowsResult([{ count: seed.enrollmentCounts?.[`${args.stepId ?? ""}:${args.status ?? ""}`] ?? 0, atLeast: false }])),
      campaignSaveJourney: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ revision: "2026-10-12T00:00:00Z", issues: [] }])),
      campaignSetJourneyState: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ revision: "2026-10-12T00:01:00Z", issues: [] }])),
      campaignDescribeJourney: vi.fn(async (_args: Record<string, unknown>) => rowsResult([seed.proposal ?? { proposal: { name: "Welcome series", trigger: { kind: "joined", audienceId: "a1" }, steps: [{ id: "send1", kind: "send", templateId: "t1", senderIdentityId: "s1" }] }, explanation: "Welcome new subscribers.", issues: [] }])),

      // The on-demand reads.
      deliveriesForCampaign: reader(seed.deliveriesForCampaign),
      recipientsForAudience: reader(seed.recipientsForAudience),
      audienceRosterSize: reader([{ count: Array.isArray(seed.recipientsForAudience) ? seed.recipientsForAudience.length : 0, atLeast: false }]),
      audienceSubscriberCount: vi.fn(async (args: { audienceId?: string }, _opts?: unknown) => {
        const answer = seed.subscriberCounts?.[args.audienceId ?? ""];
        if (answer instanceof Error) throw answer;
        // The engine's own envelope: the count, and whether it stopped at the window.
        if (typeof answer === "object") return rowsResult([{ count: answer.atLeast, atLeast: true }]);
        return rowsResult([{ count: answer ?? 0, atLeast: false }]);
      }),
      campaignStats: reader(seed.campaignStats),
      emailStatistics: reader(seed.emailStatistics),
      campaignsForAccount: reader(seed.campaignsForAccount),
      integrationStatus: reader(seed.integrationStatus ?? [HEALTHY_EMAIL]),
      campaignSendingReadiness: vi.fn(async (args?: { senderIdentityId?: string }, _opts?: unknown) => {
        const id = args?.senderIdentityId ?? "";
        const own = typeof seed.senderReadiness === "function" ? seed.senderReadiness(id) : seed.senderReadiness?.[id];
        if (own instanceof Error) throw own;
        if (own) return rowsResult([own]);
        const rows = seed.sendingReadiness ?? [{ ready: true, reason: "" }];
        if (rows instanceof Error) throw rows;
        return rowsResult(rows);
      }),
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

      // Templates and layouts (memql#6033, Task 11). The list row is a summary;
      // a template is read whole by id.
      templateById: vi.fn(async (args: { templateId?: string }, _opts?: unknown) => {
        const id = args.templateId ?? "";
        const content = seed.templateContent?.[id];
        if (content instanceof Error) throw content;
        const listed = written.get(id) ?? (seed.templates ?? []).find((row) => row.id === id);
        if (listed === undefined) return rowsResult([]);
        return rowsResult([{ ...listed, textBody: "", htmlBody: "", document: null, ...content }]);
      }),
      emailLayouts: vi.fn(async (args: { accountId?: string; includeArchived?: boolean }, _opts?: unknown) => {
        const own = seed.layouts?.[args.accountId ?? ""] ?? [];
        if (own instanceof Error) throw own;
        const listed = [...BUILT_IN_LAYOUTS, ...own];
        return rowsResult(args.includeArchived === true ? listed : listed.filter((row) => row.status !== "archived"));
      }),
      emailRender: vi.fn(async (args: { document?: Record<string, unknown>; accountId?: string }, _opts?: unknown) => {
        const answer = typeof seed.render === "function" ? seed.render(args) : seed.render ?? plainRender(args);
        if (answer instanceof Error) throw answer;
        return rowsResult([answer]);
      }),
      libraryArtifactById: reader(seed.artifacts, (row, args) => row.id === args.artifactId),
      searchUsers: reader(seed.people ?? []),

      // An organization's brand kit (Task 24): read by organization, saved
      // with the revision it was read at, and previewed without saving.
      brandKitForAccount: vi.fn(async (args: { accountId?: string }, _opts?: unknown) => {
        if (seed.brandKits instanceof Error) throw seed.brandKits;
        return rowsResult(brandKits.filter((row) => row.accountId === args.accountId));
      }),
      campaignSaveBrandKit: vi.fn(async (args: { accountId?: string; kit?: Record<string, unknown>; expectedRevision?: string }) => {
        brandSaves += 1;
        const revision = `2026-11-05T09:00:${String(brandSaves).padStart(2, "0")}Z`;
        const accountId = args.accountId ?? "";
        const row: Row = { id: `kit-${accountId}`, ownerUserId: "v1:identity:user:me", accountId, kit: args.kit ?? {}, createdAt: revision };
        const at = brandKits.findIndex((held) => held.accountId === accountId);
        if (at >= 0) brandKits[at] = row;
        else brandKits.push(row);
        return rowsResult([{ revision, warnings: [] }]);
      }),
      campaignBrandPreview: vi.fn(async (args: { accountId?: string; kit?: Record<string, unknown>; layoutId?: string }, _opts?: unknown) => {
        const answer = typeof seed.brandPreview === "function" ? seed.brandPreview(args) : seed.brandPreview ?? plainBrandPreview(args);
        if (answer instanceof Error) throw answer;
        return rowsResult([answer]);
      }),

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
      restoreAudience: reader([]),
      setAudienceConfirmation: reader([]),
      archiveAudience: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignAddRecipient: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignSetRecipientSubscription: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignRetryEnrollments: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ retried: 0 }])),
      journeyEnrollmentsByStatus: reader(seed.enrollments, (row, args) => row.journeyId === args.journeyId && row.status === args.status),
      // The seeded people, found by id and by the digest of their address.
      recipientById: reader(seed.recipientsForAudience, (row, args) => row.id === args.recipientId),
      recipientInAudienceByDigest: reader(seed.recipientsForAudience, (row, args) => row.emailDigest === args.emailDigest),
      campaignSaveTemplate: vi.fn(async (_args: Record<string, unknown>) => rowsResult([{ saved: true, revision: "2026-10-01T00:00:00Z" }])),
      // An email MemQL is writing (Task 23): cancelled, or written again.
      campaignCancelComposition: vi.fn(async (args: Record<string, unknown>) => rowsResult([{ templateId: args.templateId, revision: "2026-10-01T00:00:01Z", state: "failed" }])),
      campaignRetryComposition: vi.fn(async (args: Record<string, unknown>) => rowsResult([{ templateId: args.templateId, goalId: "goal-2", revision: "2026-10-01T00:00:02Z", state: "writing" }])),
      createSenderIdentity: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateSenderIdentity: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      setSenderIdentityStatus: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      createEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      setEmailRuleStatus: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignActivateEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      campaignRetireEmailRule: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),

      executeNamed: vi.fn(async (name: string, filter: string) => {
        if (name !== "conceptRow") return bundleResult([]);
        const concept = /concept==([^ ]+)/.exec(filter)?.[1] ?? "";
        const idText = /id==("(?:[^"\\]|\\.)*")/.exec(filter)?.[1];
        const id = idText ? JSON.parse(idText) as string : "";
        return bundleResult(latest(concept).filter(row => String(row.id).split(":").pop() === id.split(":").pop()));
      }),
    },
    subscriptions,
    dispatcher: { sendAndWait: vi.fn() },
  };
  sessions.add(connection as unknown as Connection);
  return connection;
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

/** A row of the templates list: a summary, as the engine's templateSummary shape
 *  projects it. It has no body and no document; a template is read whole by id. */
export function templateRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "August copy",
    subject: "What we shipped",
    status: "ready",
    accountId: "self",
    layoutId: "",
    preheader: "",
    sourceArtifactId: "",
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

// ---------------------------------------------------------------------------
// The email statistics read (`builtin emailStatistics`)
// ---------------------------------------------------------------------------
//
// THE PAYLOAD AS THE SERVER WRITES IT (component/emailstats/statistics.go,
// `Payload`): lowerCamel keys, instants in RFC3339, `measuredSince` as a UTC
// day, and every figure nothing measured PRESENT AND NULL. The builders below
// are the answers a test asks for: a measured month (`statisticsRow`), a
// cluster that has recorded nothing (`unmeasuredStatisticsRow`) and twelve
// months, which have no previous period (`twelveMonthsStatisticsRow`).

type Figures = Record<string, number | null>;

const TOTAL_KEYS = ["sent", "delivered", "undelivered", "failed", "skipped", "suppressed", "opens", "clicks", "uniqueOpens", "uniqueClicks", "unsubscribes", "bounces", "complaints", "subscribes", "openRate", "clickRate"] as const;
const POINT_KEYS = ["sent", "uniqueOpens", "uniqueClicks", "unsubscribes", "subscribes", "bounces", "complaints", "failed", "suppressed", "openRate", "clickRate"] as const;

const nothing = (keys: readonly string[]): Figures => Object.fromEntries(keys.map((key) => [key, null]));

/**
 * The thirty days `statisticsRow` answers for. 900 messages were first opened
 * in them, which would read as 75% of the 1,204 sent; the open rate is the
 * cohort's, 42% of the campaign messages sent in the period.
 */
export const STATISTICS_TOTALS: Figures = {
  sent: 1204, delivered: 0, undelivered: 0, failed: 3, skipped: 10, suppressed: 4,
  opens: 1310, clicks: 120, uniqueOpens: 900, uniqueClicks: 41,
  unsubscribes: 38, bounces: 7, complaints: 1, subscribes: 212,
  openRate: 0.42, clickRate: 0.031,
};

/** The thirty days before them: Sent rises 12%, new subscribers are new, bounces hold. */
export const STATISTICS_PREVIOUS: Figures = {
  sent: 1075, delivered: 0, undelivered: 0, failed: 6, skipped: 8, suppressed: 4,
  opens: 1100, clicks: 100, uniqueOpens: 800, uniqueClicks: 33,
  unsubscribes: 51, bounces: 7, complaints: 0, subscribes: 0,
  openRate: 0.4, clickRate: 0.031,
};

/** One point per bucket from `start` ("2026-10-10", or "2025-11" by the month). */
function statisticsPoints(start: string, count: number, bucket: "day" | "month", figures: (index: number) => Figures): Row[] {
  const [year = 2026, month = 1, day = 1] = start.split("-").map(Number);
  return Array.from({ length: count }, (_, index) => {
    const at = bucket === "day" ? new Date(Date.UTC(year, month - 1, day + index)) : new Date(Date.UTC(year, month - 1 + index, 1));
    return { at: at.toISOString().replace(".000Z", "Z"), ...nothing(POINT_KEYS), ...figures(index) };
  });
}

/** A day of the measured month: the most sent on Oct 15, the best open rate on Oct 30. */
function measuredDay(index: number): Figures {
  return {
    sent: index === 5 ? 64 : 40, uniqueOpens: 16, uniqueClicks: 2, unsubscribes: 1, subscribes: 7,
    bounces: index === 3 ? 2 : 0, complaints: index === 7 ? 1 : 0, failed: index === 9 ? 3 : 0, suppressed: index === 11 ? 4 : 0,
    openRate: index === 20 ? 0.55 : 0.4, clickRate: 0.03,
  };
}

/** A day of the thirty before them. */
function previousDay(): Figures {
  return { sent: 35, uniqueOpens: 14, uniqueClicks: 1, unsubscribes: 2, subscribes: 0, bounces: 0, complaints: 0, failed: 0, suppressed: 0, openRate: 0.38, clickRate: 0.03 };
}

export type StatisticsOver = Omit<Row, "totals" | "previous"> & { totals?: Figures; previous?: Figures | null };

/**
 * `emailStatistics`' answer for the last thirty days of a cluster that has
 * counted its mail since Sep 1, so the thirty days before them can be compared
 * with (kit `previousComparable`). `totals` and `previous` are merged into the
 * figures above; every other key replaces its own.
 */
export function statisticsRow(over: StatisticsOver = {}): Row {
  const { totals, previous, ...rest } = over;
  return {
    period: "30d",
    bucket: "day",
    start: "2026-10-10T00:00:00Z",
    end: "2026-11-09T00:00:00Z",
    hasPrevious: true,
    prevStart: "2026-09-10T00:00:00Z",
    prevEnd: "2026-10-10T00:00:00Z",
    measuredSince: "2026-09-01",
    totals: { ...STATISTICS_TOTALS, ...totals },
    previous: previous === null ? null : { ...STATISTICS_PREVIOUS, ...previous },
    series: statisticsPoints("2026-10-10", 30, "day", measuredDay),
    previousSeries: statisticsPoints("2026-09-10", 30, "day", previousDay),
    // The read's order: most unique opens first, then unique clicks -- not most sent.
    campaigns: [
      { id: "c-winter", sent: 600, uniqueOpens: 300, uniqueClicks: 34, openRate: 0.5, clickRate: 0.057 },
      { id: "c-restock", sent: 604, uniqueOpens: 200, uniqueClicks: 19, openRate: 0.33, clickRate: 0.032 },
    ],
    automations: [
      { id: "r-welcome", kind: "rule", sent: 120, failed: 2, suppressed: 1 },
      { id: "j-series", kind: "journey", sent: 40, failed: 0, suppressed: 0 },
    ],
    audiences: [
      { id: "a-news", subscribes: 12, unsubscribes: 3, net: 9 },
      { id: "a-signups", subscribes: 2, unsubscribes: 5, net: -3 },
    ],
    accounts: [],
    ...rest,
  };
}

/**
 * The answer on a cluster that has recorded no email -- or to somebody who
 * reaches no organization: the bounds, every figure null, a null point for
 * every bucket, no lists, and no `measuredSince`.
 */
export function unmeasuredStatisticsRow(): Row {
  return {
    ...statisticsRow(),
    measuredSince: null,
    totals: nothing(TOTAL_KEYS),
    previous: nothing(TOTAL_KEYS),
    series: statisticsPoints("2026-10-10", 30, "day", () => ({})),
    previousSeries: statisticsPoints("2026-09-10", 30, "day", () => ({})),
    campaigns: [],
    automations: [],
    audiences: [],
    accounts: [],
  };
}

/** Twelve months: the current month and the twelve before it, by the month, with no previous period (spec D4). */
export function twelveMonthsStatisticsRow(): Row {
  return statisticsRow({
    period: "12m",
    bucket: "month",
    start: "2025-11-01T00:00:00Z",
    end: "2026-12-01T00:00:00Z",
    hasPrevious: false,
    prevStart: null,
    prevEnd: null,
    totals: { sent: 14020 },
    previous: null,
    series: statisticsPoints("2025-11", 13, "month", (index) => ({ ...measuredDay(index), sent: 1000 + index * 20 })),
    previousSeries: null,
  });
}
