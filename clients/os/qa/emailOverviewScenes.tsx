import { useEffect, useMemo, useState, type ComponentType, type ReactElement, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { EmailApp } from "../src/apps/email/EmailApp";
import { EmailNavigationContext } from "../src/apps/email/openRequest";
import { OverviewSection } from "../src/apps/email/overview/OverviewSection";
import { DEFAULT_EMAIL_SETTINGS, LocalEmailSettingsStore, type EmailSettings } from "../src/apps/email/settings";
import { useEmailFeeds } from "../src/apps/email/useEmail";
import {
  audienceRow,
  campaignRow,
  fakeConnection,
  ruleRow,
  statisticsRow,
  twelveMonthsStatisticsRow,
  unmeasuredStatisticsRow,
  withSession,
  type FakeSeed,
} from "../test/email/harness";
import { installSeededAccess } from "../test/seededAccess";

// Email's Overview, rendered (the 2026-11-08 statistics design, section 9).
// Fixtures only: the statistics are the Email harness's own payloads
// (`statisticsRow` and its siblings), keyed exactly as the read writes them.
//
//   view=email-overview               thirty days of mail, staff, every client
//   view=email-overview-empty         first use: nothing sent yet
//   view=email-overview-reading       every read still on its way: shapes only
//   view=email-overview-refused       the statistics, the automations and a count refused
//   view=email-overview-unmeasured    a cluster that has recorded no email yet
//   view=email-overview-untracked     only automated mail went out: no rate to state
//   view=email-overview-floor         an audience past the engine's count window
//   view=email-overview-twelve-months twelve months, with no comparison
//   view=email-overview-member        a member of one client: no client control
//   view=email-overview-interrupted   2.5 s in, the cluster refuses the refreshes
//   view=email-overview-reconnecting  2.5 s in, the connection drops
//
// With `mode=light|dark`, `theme=` and `width=narrow` as everywhere in the
// harness; `&click=Add a campaign` follows first use to the wizard.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
type Session = Parameters<typeof withSession>[1];

const ACCOUNTS = [
  { id: "self", name: "Operator organization", status: "active" },
  { id: "acme", name: "Acme", status: "active" },
  { id: "borden", name: "Borden Ltd", status: "active" },
];

function journeyRow(over: Partial<Row> & { id: string }): Row {
  return { accountId: "self", name: "Welcome series", description: "", status: "draft", trigger: { kind: "joined", audienceId: "a-news" }, steps: [], createdAt: "2026-08-01T00:00:00Z", ...over };
}

/** A cluster with a month of mail: the records the statistics' rows are named from. */
const SEED: FakeSeed = {
  accounts: ACCOUNTS,
  campaigns: [
    campaignRow({ id: "c-winter", name: "Winter launch", status: "sent", startedAt: "2026-10-20T09:00:00Z", recipientCount: 600, sentCount: 600 }),
    campaignRow({ id: "c-restock", name: "Restock alert", status: "sending", startedAt: "2026-11-07T09:00:00Z", recipientCount: 700, sentCount: 604 }),
    campaignRow({ id: "c-holiday", name: "Holiday hours", status: "sent", startedAt: "2026-10-28T09:00:00Z", recipientCount: 211, sentCount: 211 }),
    campaignRow({ id: "c-draft", name: "Autumn sale" }),
    campaignRow({ id: "c-launch", name: "Launch day", status: "scheduled", scheduledAt: "2031-10-14T16:00:00Z" }),
  ],
  audiences: [
    audienceRow({ id: "a-news", name: "Newsletter" }),
    audienceRow({ id: "a-signups", name: "Signups" }),
    audienceRow({ id: "a-old", name: "Old list", status: "archived" }),
  ],
  subscriberCounts: { "a-news": 1204, "a-signups": 86, "a-old": 500 },
  emailRules: [ruleRow({ id: "r-welcome", name: "Welcome admins", status: "active" }), ruleRow({ id: "r-paused", name: "Old rule", status: "paused" })],
  journeys: [journeyRow({ id: "j-series", name: "Welcome series", status: "active" })],
};

/** The leading campaigns, in the read's order: most unique opens, then unique clicks. */
const CAMPAIGNS = [
  { id: "c-winter", sent: 600, uniqueOpens: 300, uniqueClicks: 34, openRate: 0.5, clickRate: 0.057 },
  { id: "c-restock", sent: 604, uniqueOpens: 200, uniqueClicks: 19, openRate: 0.33, clickRate: 0.032 },
  { id: "c-holiday", sent: 211, uniqueOpens: 61, uniqueClicks: 0, openRate: 0.29, clickRate: 0 },
];

/** Each client with sends, most sent first: what staff reading every client see. */
const CLIENTS = [
  { id: "acme", sent: 900, uniqueOpens: 400, uniqueClicks: 30, openRate: 0.45, clickRate: 0.033 },
  { id: "self", sent: 304, uniqueOpens: 101, uniqueClicks: 23, openRate: 0.38, clickRate: 0.05 },
  { id: "borden", sent: 120, uniqueOpens: 0, uniqueClicks: 0, openRate: null, clickRate: null },
];

const MONTH = statisticsRow({ campaigns: CAMPAIGNS, accounts: CLIENTS });

/** A read that never answers, so the page stays on its shapes. */
const never = () => new Promise<never>(() => {});

/** The Overview in an Email window, under settings of its own, following its own `navigate`. */
function OverviewWindow({ frame: Frame, settings }: { frame: Frame; settings: Partial<EmailSettings> }) {
  const [store] = useState(() => {
    const data = new Map<string, string>();
    const held = new LocalEmailSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
    held.save({ ...held.load(), ...settings });
    return held;
  });
  // The window follows `navigate`, as the shell's does: a press that leaves for
  // another tab (Add a campaign, a row) can be captured arriving.
  const [current, setCurrent] = useState("overview");
  return <Frame fallback="Email"><EmailApp sectionId={current} navigate={(id) => setCurrent(id)} askContext={() => {}} store={store} /></Frame>;
}

/**
 * The Overview on its own over the root's feeds, a few seconds before the
 * cluster stops answering: it refuses the refreshes ("refused"), or the
 * connection drops ("disconnected"). What is judged is what stays on screen
 * and what the heading's line says.
 */
function InterruptedPane({ frame: Frame, how, refuse }: { frame: Frame; how: "refused" | "disconnected"; refuse: () => void }) {
  const feeds = useEmailFeeds();
  const [settings, setSettings] = useState<EmailSettings>(DEFAULT_EMAIL_SETTINGS);
  useEffect(() => {
    const timer = window.setTimeout(() => {
      if (how === "disconnected") {
        (feeds.campaigns.source as unknown as { markDisconnected: () => void } | null)?.markDisconnected();
        return;
      }
      refuse();
      feeds.campaigns.reseed();
      // Coming back to the window reads the statistics and the counts again.
      window.dispatchEvent(new Event("focus"));
    }, 2500);
    return () => window.clearTimeout(timer);
    // Once, a few seconds in.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const navigation = useMemo(() => ({ openRecord: () => {}, openSettings: () => {}, openInbox: () => {} }), []);
  return (
    <Frame fallback="Email">
      <EmailNavigationContext.Provider value={navigation}>
        <OverviewSection feeds={feeds} settings={settings} update={(patch) => setSettings((held) => ({ ...held, ...patch }))} />
      </EmailNavigationContext.Provider>
    </Frame>
  );
}

/** The scenes, framed as the harness frames every window body. */
export function emailOverviewScenes(frame: Frame) {
  function view(seed: FakeSeed, { settings = {}, session = {}, adjust }: { settings?: Partial<EmailSettings>; session?: Session; adjust?: (connection: ReturnType<typeof fakeConnection>) => void } = {}) {
    return {
      connect: () => {
        installSeededAccess("owner");
        const connection = fakeConnection(seed);
        adjust?.(connection);
        return connection;
      },
      wrap: (element: ReactElement) => withSession(element, session),
      render: () => <OverviewWindow frame={frame} settings={settings} />,
    };
  }
  function interrupted(how: "refused" | "disconnected") {
    let held: ReturnType<typeof fakeConnection> | null = null;
    return {
      connect: () => {
        installSeededAccess("owner");
        held = fakeConnection({ ...SEED, emailStatistics: [MONTH] });
        return held;
      },
      wrap: (element: ReactElement) => withSession(element),
      render: () => (
        <InterruptedPane
          frame={frame}
          how={how}
          refuse={() => {
            const refuse = async () => { throw new Error("The cluster did not answer in time."); };
            // The harness's `vi.fn` is a shim without `mockImplementation`: answers are assigned.
            if (held !== null) Object.assign(held.query, { campaigns: refuse, emailStatistics: refuse, audienceSubscriberCount: refuse });
          }}
        />
      ),
    };
  }
  return {
    "email-overview": view({ ...SEED, emailStatistics: [MONTH] }),
    "email-overview-empty": view({ accounts: ACCOUNTS, emailStatistics: [unmeasuredStatisticsRow()] }),
    "email-overview-reading": view({ ...SEED, emailStatistics: [MONTH] }, {
      adjust: (connection) => Object.assign(connection.query, {
        emailStatistics: never, campaigns: never, audiences: never, emailRules: never, journeys: never, audienceSubscriberCount: never,
      }),
    }),
    "email-overview-refused": view({ ...SEED, subscriberCounts: { "a-news": new Error("Counting subscribers is not yours to do."), "a-signups": 86 } }, {
      adjust: (connection) => Object.assign(connection.query, {
        emailStatistics: async () => { throw new Error("Email statistics are not yours to read."); },
        emailRules: async () => { throw new Error("Reading automations is not yours to do."); },
      }),
    }),
    "email-overview-unmeasured": view({ ...SEED, emailStatistics: [unmeasuredStatisticsRow()] }),
    "email-overview-untracked": view({
      ...SEED,
      // Rule and journey mail only: its opens are not tracked, and no campaign
      // mail went out for a rate to be a share of.
      emailStatistics: [statisticsRow({ totals: { sent: 160, opens: 0, clicks: 0, uniqueOpens: 0, uniqueClicks: 0, openRate: null, clickRate: null }, campaigns: [], accounts: [] })],
    }),
    "email-overview-floor": view({ ...SEED, subscriberCounts: { "a-news": { atLeast: 5000 }, "a-signups": 86, "a-old": 500 }, emailStatistics: [MONTH] }),
    "email-overview-twelve-months": view({ ...SEED, emailStatistics: [twelveMonthsStatisticsRow()] }, { settings: { statisticsPeriod: "12m" } }),
    "email-overview-member": view({ ...SEED, emailStatistics: [statisticsRow({ campaigns: CAMPAIGNS })] }, { session: { role: "user", everyAccount: false, accountIds: ["acme"] } }),
    "email-overview-interrupted": interrupted("refused"),
    "email-overview-reconnecting": interrupted("disconnected"),
  };
}
