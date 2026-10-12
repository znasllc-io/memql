import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  getRowByConceptAndId,
  type Concept,
  type Connection,
  type Result,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../chrome/access";
import { absent, figureFrom, type Figure } from "../../kit/measure";
import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { feedIsBehind, useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import {
  AUDIENCE_CONCEPT,
  CAMPAIGN_CONCEPT,
  EMAIL_RULE_CONCEPT,
  SENDER_IDENTITY_CONCEPT,
  TEMPLATE_CONCEPT,
  audienceFromRow,
  authoredAutomationsFrom,
  campaignFromRow,
  statsFromPayload,
  type AuthoredAutomationsState,
  type CampaignStats,
} from "./rows";

// Operator records use graph subscriptions. High-volume delivery, engagement
// and recipient rows stay out of mesh broadcasts, so those are bounded
// on-demand READINGS -- and a reading has no clock of its own. The surface that
// shows one mounts the kit's `AutoRefresh` beside it, which re-reads it every
// EMAIL_REFRESH_MS once the previous read has settled, and only while the
// surface is SEEN: not while its pane is parked behind another tab, not while
// the browser tab is hidden, not while the window is offline.
export const EMAIL_REFRESH_MS = 5_000;

// ---------------------------------------------------------------------------
// The five live feeds
// ---------------------------------------------------------------------------
//
// ONE FEED PER CONCEPT, ALL FIVE RETAINED AT THE APP ROOT. The one-feed rule
// is per CONCEPT, not per app (the Packages rule): what must never happen is
// two subscriptions over the SAME concept, free to disagree about what the
// cluster holds. Five concepts cannot disagree, because they describe
// different things.
//
// They are all at the root rather than one per section because they are all
// needed in more than one place: the campaign editor picks an audience, a
// template and a sender; the rules builder picks a template, an audience and a
// sender; the audiences list wants to say which campaigns used a roster. A
// per-section feed would mean the same concept subscribed twice the moment
// somebody opened two of those, which is the failure the rule names.
//
// EVERY PAGE, NOT THE FIRST. Each list read answers a page and a cursor, and
// the seed walks the cursor inside the one collection, so paging never tears
// the subscription down. A feed that stopped at the first page would show an
// operator fifty campaigns and let them believe it was all of them.

/** The options for one page of a walked list: the first page carries no cursor. */
const pageOf = (cursor: string, signal: AbortSignal) => ({ signal, ...(cursor !== "" ? { cursor } : {}) });

/** A concept's own list, every page, retained once. */
function useListFeed(
  key: string,
  concept: string,
  list: (connection: Connection, cursor: string, signal: AbortSignal) => Promise<Result>,
  inScope?: (row: Row) => boolean,
): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(key, (connection) => ({
    concept,
    inScope,
    seed: async (cursor, signal) => {
      const result = await list(connection, cursor, signal);
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, concept, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));
}

export function useCampaignFeed(): LiveCollectionHandle<Row> {
  return useListFeed(
    "campaigns:campaigns",
    CAMPAIGN_CONCEPT,
    // NO STATUS ARGUMENT. `campaigns` takes an optional one and the filed
    // toggle deliberately does not pass it: seeding filtered would make the
    // toggle re-run the read and re-baseline every arrival cue, so revealing
    // rows the browser already had would announce them as new.
    (connection, cursor, signal) => connection.query.campaigns({}, pageOf(cursor, signal)),
    (row) => !flatten(row).testSourceCampaignId,
  );
}

export function useAudienceFeed(): LiveCollectionHandle<Row> {
  return useListFeed("campaigns:audiences", AUDIENCE_CONCEPT, (connection, cursor, signal) =>
    connection.query.audiences({}, pageOf(cursor, signal)));
}

export function useTemplateFeed(): LiveCollectionHandle<Row> {
  return useListFeed("campaigns:templates", TEMPLATE_CONCEPT, (connection, cursor, signal) =>
    connection.query.templates({}, pageOf(cursor, signal)));
}

export function useSenderFeed(): LiveCollectionHandle<Row> {
  return useListFeed("campaigns:senders", SENDER_IDENTITY_CONCEPT, (connection, cursor, signal) =>
    connection.query.senderIdentities({}, pageOf(cursor, signal)));
}

export function useRuleFeed(): LiveCollectionHandle<Row> {
  return useListFeed("campaigns:rules", EMAIL_RULE_CONCEPT, (connection, cursor, signal) =>
    connection.query.emailRules({}, pageOf(cursor, signal)));
}

export interface EmailFeeds {
  campaigns: LiveCollectionHandle<Row>;
  audiences: LiveCollectionHandle<Row>;
  templates: LiveCollectionHandle<Row>;
  senders: LiveCollectionHandle<Row>;
  rules: LiveCollectionHandle<Row>;
}

/** The app root's five, retained together so every section is a reading of
 *  the same snapshots. */
export function useEmailFeeds(): EmailFeeds {
  return {
    campaigns: useCampaignFeed(),
    audiences: useAudienceFeed(),
    templates: useTemplateFeed(),
    senders: useSenderFeed(),
    rules: useRuleFeed(),
  };
}

// ---------------------------------------------------------------------------
// The feeds composed over ids
// ---------------------------------------------------------------------------
//
// A series, a testing audience and a signup form are each read PER ID -- the
// engine answers "the series of THIS campaign", "the testing settings of THIS
// organization", "the forms of THIS audience" -- and each used to be its own
// collection per open page: a subscription opened for every campaign, every
// Settings visit and every audience somebody looked at. A concept that is read
// per id is still ONE concept. So each has one collection, retained at the app
// root, whose seed reads the ids in view and whose subscription carries every
// row of the concept for the one stream.
//
// THE KEY NAMES THE CONCEPT ONLY. `useLiveCollection` says a key must encode
// what is READ, and the ids are what is read -- but a key that carried them
// would restart the collection, and so open a SECOND subscription over the
// concept for as long as the released one lingers, every time a draft arrives
// or leaves. The ids travel in a ref the seed and the event filter read, and a
// change to the SET re-runs the one seed. The collection keeps its rows while
// it does (the read replaces membership once it lands), so nothing a person is
// looking at is unmounted for it.

/** How many ids a composed seed reads at once. A burst of one request per
 *  draft would be a burst of hundreds in a cluster that keeps many. */
const READ_CONCURRENCY = 6;
/** The runaway-cursor guard for a walked per-id read, as LiveCollection's own. */
const MAX_PAGES = 50;

interface Composition {
  concept: string;
  /** The id, of the set the feed is read over, that a row answers to. */
  ownerOf: (row: Row) => string;
  /** One id's rows. */
  readFor: (connection: Connection, id: string, signal: AbortSignal) => Promise<Row[]>;
}

/** `read` for each id, a few at a time. */
async function readEach<T>(ids: readonly string[], read: (id: string) => Promise<T>, signal: AbortSignal): Promise<T[]> {
  const answers: T[] = [];
  let next = 0;
  let failed = false;
  const worker = async () => {
    while (next < ids.length && !failed && !signal.aborted) {
      const at = next++;
      try { answers[at] = await read(ids[at]!); }
      catch (err) { failed = true; throw err; }
    }
  };
  await Promise.all(Array.from({ length: Math.min(READ_CONCURRENCY, ids.length) }, worker));
  return answers;
}

/** Every page of one id's read, by the cursor each page hands back. */
async function everyPage(read: (cursor: string) => Promise<Result>): Promise<Row[]> {
  const rows: Row[] = [];
  let cursor = "";
  for (let page = 0; page < MAX_PAGES; page++) {
    const result = await read(cursor);
    rows.push(...result.rows());
    cursor = result.meta()?.cursor ?? "";
    if (cursor === "") break;
  }
  return rows;
}

const textOf = (row: Row, key: string): string => (typeof row[key] === "string" ? (row[key] as string) : "");

/**
 * A composed feed: the collection, and what it knows about each id it was read
 * over.
 *
 * THE ROWS ARE ONE COLLECTION'S, but a per-id read has three answers a list of
 * rows cannot tell apart: the id has rows, the id has none, and the id has not
 * been read -- or was refused. "No row" for an organization's testing audience
 * is a real answer ("none chosen") and also what an unread or refused
 * organization looks like, so a surface that draws the first for the other two
 * invents a fact about somebody's organization.
 */
export interface ComposedFeed extends LiveCollectionHandle<Row> {
  /** Whether this id has been read since the feed began, answered or refused. Before that, no rows is "not read yet". */
  settled: (id: string) => boolean;
  /** The engine's sentence when the last read of this id was refused; "" when it was answered. */
  refusal: (id: string) => string;
  /**
   * Read this id's rows again and leave every other id's as they are -- what a
   * write to one id's row asks for, in place of a `reseed()` that reads them all.
   * When another read is already running the next one is a whole read instead,
   * because the running one may be a recovery from a gap in the stream that a
   * single id must not stand in for.
   */
  refresh: (id: string) => void;
}

const NO_ANSWERS: ReadonlyMap<string, string> = new Map();

function useComposedFeed(name: string, composition: Composition, ids: readonly string[]): ComposedFeed {
  // The SET, not the array: callers hand a new array every render.
  const signature = [...new Set(ids)].filter((id) => id !== "").sort().join("\n");
  const wanted = useMemo(() => new Set(signature === "" ? [] : signature.split("\n")), [signature]);
  const latest = useRef({ wanted, signature });
  latest.current = { wanted, signature };
  /** The set the last seed STARTED with, so a change after it re-reads. */
  const seeded = useRef<string | null>(null);
  /** Per id, once read: "" when it was answered, else the engine's refusal. */
  const [answers, setAnswers] = useState<ReadonlyMap<string, string>>(NO_ANSWERS);
  /** The ids a `refresh()` is asking the seed it is about to start to read. */
  const only = useRef<ReadonlySet<string> | null>(null);
  /** The handle itself, for the seed to read what the collection holds now. */
  const handle = useRef<LiveCollectionHandle<Row> | null>(null);

  const feed = useLiveCollection<Row>(`campaigns:${name}`, (connection) => ({
    concept: composition.concept,
    seed: async (_cursor, signal) => {
      const { wanted: asked, signature: set } = latest.current;
      // Taken by the seed that starts first, so it can only ever mean the one a
      // `refresh()` started: any other seed is a whole read.
      const partial = only.current;
      only.current = null;
      // ONLY A WHOLE READ MARKS THE SET AS READ. A single-id refresh that lands
      // between the render that added an id and the effect that reads it must
      // leave that effect something to do, or the new id is never asked.
      if (partial === null) seeded.current = set;
      // What the collection holds now, by the id each row answers to.
      const held = new Map<string, Row[]>();
      for (const row of handle.current?.source?.snapshot.rows ?? []) {
        const owner = composition.ownerOf(flatten(row));
        const bucket = held.get(owner);
        if (bucket === undefined) held.set(owner, [row]);
        else bucket.push(row);
      }
      const heldFor = (id: string) => held.get(id) ?? [];

      // EACH ID IS ITS OWN ANSWER. One organization this person may not read
      // must not empty the feed for the ones they may, so a refusal is kept
      // against its id (and the rows that id last had are kept too: a refusal
      // is not a zero) while every other id lands.
      const reading = [...asked].filter((id) => partial === null || partial.has(id));
      const read = await readEach(reading, async (id) => {
        try {
          return { id, rows: await composition.readFor(connection, id, signal), refusal: "" };
        } catch (err) {
          if (signal.aborted) throw err;
          return { id, rows: [] as Row[], refusal: describe(err) };
        }
      }, signal);
      const fresh = new Map(read.map((answer) => [answer.id, answer]));
      const rows = [...asked].flatMap((id) => {
        const answer = fresh.get(id);
        return answer !== undefined && answer.refusal === "" ? answer.rows : heldFor(id);
      });
      setAnswers((previous) => {
        const next = new Map<string, string>();
        for (const id of asked) {
          const answer = fresh.get(id);
          const known = answer !== undefined ? answer.refusal : previous.get(id);
          if (known !== undefined) next.set(id, known);
        }
        const same = next.size === previous.size && [...next].every(([id, refusal]) => previous.get(id) === refusal);
        return same ? previous : next;
      });
      return { rows, nextCursor: "" };
    },
    // The stream carries every row of the concept the caller may see; this
    // feed holds the ones it was asked about.
    inScope: (row) => latest.current.wanted.has(composition.ownerOf(flatten(row))),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, composition.concept, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
  handle.current = feed;

  const reseed = feed.reseed;
  useEffect(() => {
    if (seeded.current !== null && seeded.current !== signature) reseed();
    // The set is the only trigger; `reseed` is read fresh each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature]);

  const refresh = useCallback((id: string) => {
    only.current = new Set([id]);
    // A seed that is not already running starts INSIDE this call and takes
    // `only`; one that is running makes this a no-op that queues a follow-up,
    // and the follow-up is a whole read.
    handle.current?.reseed();
    only.current = null;
  }, []);

  // The feed's own error, or else the first id's refusal: a surface that reads
  // `snapshot.error` still hears that something could not be read, and one that
  // cares WHICH id asks `refusal(id)`.
  const firstRefusal = useMemo(() => [...answers.values()].find((refusal) => refusal !== "") ?? "", [answers]);
  const snapshot = useMemo(
    () => (feed.snapshot.error !== "" || firstRefusal === "" ? feed.snapshot : { ...feed.snapshot, error: firstRefusal }),
    [feed.snapshot, firstRefusal],
  );
  const settled = useCallback((id: string) => answers.has(id), [answers]);
  const refusal = useCallback((id: string) => answers.get(id) ?? "", [answers]);
  return { ...feed, snapshot, settled, refusal, refresh };
}

/** The repeating schedule of every campaign in view: one subscription over
 *  `v1:campaigns:campaignSeries`, `campaignSeriesForCampaign` read per id. */
export function useSeriesFeed(campaignIds: readonly string[]): ComposedFeed {
  return useComposedFeed("series", {
    concept: "v1:campaigns:campaignSeries",
    ownerOf: (row) => textOf(row, "sourceCampaignId"),
    readFor: async (connection, campaignId, signal) =>
      (await connection.query.campaignSeriesForCampaign({ campaignId }, { signal })).rows(),
  }, campaignIds);
}

/** The testing audience of every organization in view: one subscription over
 *  `v1:campaigns:testSettings`, `campaignTestSettings` read per id. */
export function useTestSettingsFeed(accountIds: readonly string[]): ComposedFeed {
  return useComposedFeed("test-settings", {
    concept: "v1:campaigns:testSettings",
    ownerOf: (row) => textOf(row, "accountId"),
    readFor: async (connection, accountId, signal) =>
      (await connection.query.campaignTestSettings({ accountId }, { signal })).rows(),
  }, accountIds);
}

/** The signup forms of every audience in view: one subscription over
 *  `v1:campaigns:newsletterBinding`, `newslettersForAudience` read per id and
 *  walked to its last page. */
export function useSignupFormFeed(audienceIds: readonly string[]): ComposedFeed {
  return useComposedFeed("signup-forms", {
    concept: "v1:campaigns:newsletterBinding",
    ownerOf: (row) => textOf(row, "audienceId"),
    readFor: (connection, audienceId, signal) =>
      everyPage((cursor) => connection.query.newslettersForAudience({ audienceId }, pageOf(cursor, signal))),
  }, audienceIds);
}

export interface EmailRecordFeeds {
  series: ComposedFeed;
  testSettings: ComposedFeed;
  signupForms: ComposedFeed;
}

const NO_IDS: readonly string[] = [];

/**
 * The three composed feeds, over what the root's five are showing.
 *
 * IN VIEW means what the person can act on, which the root's lists already
 * say: a DRAFT can source a series, and so can a campaign whose page is open --
 * a draft a series was made from may have been sent by hand since, and the
 * series it left running is reached from that page alone; an organization with
 * a campaign or an audience here is one whose testing audience a page may name;
 * every audience may have a signup form. Reading these only from the feeds that
 * exist keeps a cluster with no mail from reading anything per id at all.
 *
 * `alsoOrganizations` are organizations a page asks for whatever the lists
 * hold: Settings chooses an organization's testing audience, and an
 * organization with no campaign or audience has nothing in view to ask it by.
 * `alsoCampaigns` are the campaigns whose pages have been opened, whatever
 * their state.
 */
export function useEmailRecordFeeds(
  feeds: EmailFeeds,
  alsoOrganizations: readonly string[] = NO_IDS,
  alsoCampaigns: readonly string[] = NO_IDS,
): EmailRecordFeeds {
  const campaignRows = feeds.campaigns.snapshot.rows;
  const audienceRows = feeds.audiences.snapshot.rows;
  const drafts = useMemo(
    () => [...campaignRows.map(campaignFromRow).filter((c) => c.id !== "" && c.status === "draft").map((c) => c.id), ...alsoCampaigns],
    [campaignRows, alsoCampaigns],
  );
  const organizations = useMemo(() => {
    const accounts = [...campaignRows.map(campaignFromRow), ...audienceRows.map(audienceFromRow)].map((r) => r.accountId);
    return [...new Set([...accounts, ...alsoOrganizations].filter((id) => id !== ""))];
  }, [campaignRows, audienceRows, alsoOrganizations]);
  const audiences = useMemo(() => audienceRows.map(audienceFromRow).map((a) => a.id).filter((id) => id !== ""), [audienceRows]);
  return {
    series: useSeriesFeed(drafts),
    testSettings: useTestSettingsFeed(organizations),
    signupForms: useSignupFormFeed(audiences),
  };
}

/**
 * What a surface may honestly say about something a feed holds:
 *
 *   reading      the feed has not answered yet -- a skeleton, never a warning
 *   refused      the engine refused this id's read -- its sentence, in place
 *   unconfirmed  the feed is behind (degraded or disconnected) -- the last rows,
 *                with a notice that they cannot be confirmed
 *   known        read, and current
 *
 * Reading is not a fault. A notice that says "not confirmed" during the first
 * read is a warning drawn about nothing wrong, on every page load.
 */
export type Knowledge = "reading" | "refused" | "unconfirmed" | "known";

/** What one id of a composed feed knows, and the sentence when it was refused. */
export function knowledgeOf(feed: ComposedFeed, id: string): { knowledge: Knowledge; refusal: string } {
  const refusal = feed.refusal(id);
  if (refusal !== "") return { knowledge: "refused", refusal };
  if (feedIsBehind(feed.snapshot.state)) return { knowledge: "unconfirmed", refusal: "" };
  return { knowledge: feed.settled(id) ? "known" : "reading", refusal: "" };
}

/**
 * What a plain list feed knows: refused, behind, still reading, or known.
 *
 * A LIST THAT FAILED TO READ IS NOT AN EMPTY ONE, and the feed's own state does
 * not say so: a first read that is refused with nothing held ends `live`, with
 * the engine's sentence on `snapshot.error`. A surface that read only the state
 * would call it known and draw an empty list -- and offer a choice made against
 * it. So a sentence on the feed is a refusal, whatever it holds. (A composed
 * feed's `snapshot.error` carries any id's refusal, so ask `knowledgeOf` about
 * the id instead.)
 */
export function listKnowledge(feed: LiveCollectionHandle<Row>): Knowledge {
  if (feed.snapshot.error !== "") return "refused";
  if (feedIsBehind(feed.snapshot.state)) return "unconfirmed";
  return feed.snapshot.state === "seeding" ? "reading" : "known";
}

/**
 * What a surface that depends on several feeds may say: the least it can claim.
 * A refusal outranks a feed that is behind, which outranks one still reading.
 */
export function weakest(...answers: Knowledge[]): Knowledge {
  for (const worst of ["refused", "unconfirmed", "reading"] as const) {
    if (answers.includes(worst)) return worst;
  }
  return "known";
}

// ---------------------------------------------------------------------------
// The on-demand reads
// ---------------------------------------------------------------------------

/**
 * One on-demand read's state.
 *
 * `error` is the SERVER's sentence, kept verbatim and rendered in surface. It
 * is a first-class state rather than an empty list, because A REFUSAL IS NOT A
 * ZERO (the Accounts rule): a caller below a gate gets a refusal, and rendering
 * that as "0 deliveries" would be this window inventing a fact about a send.
 */
export interface Reading<T> {
  value: T;
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  /** Last successful read. Errors retain this timestamp and the last answer. */
  readAt: string;
  /**
   * Read again now. Whatever is still on its way is stale from this moment and
   * is aborted; the last answer stays on screen until the new one lands.
   *
   * The promise settles when the read that SUPERSEDES the others does -- and
   * never rejects, because a failed read is this reading's `error`. That is
   * what lets `AutoRefresh` (which awaits it) count its interval from the end
   * of a read: a slow cluster gets one request at a time, not a new one every
   * tick aborting the last before it can answer.
   */
  reload: () => Promise<void>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

interface Snapshot<T> {
  reader: ((signal: AbortSignal) => Promise<T>) | null;
  value: T;
  state: Reading<T>["state"];
  error: string;
  readAt: string;
}

/**
 * Reads once when `deps` change, and again when asked. NO TIMER: the surface
 * that shows the answer mounts `AutoRefresh` beside it and gives it `reload`,
 * so a pane that is parked, a tab that is hidden and a window that is offline
 * stop reading without this hook knowing about any of them.
 *
 * It keeps the last good answer through a background read and through a
 * failure, aborts a read its successor makes stale, and ignores a late answer
 * from one that was aborted.
 */
export function useReading<T>(
  empty: T,
  read: ((signal: AbortSignal) => Promise<T>) | null,
  deps: readonly unknown[],
): Reading<T> {
  const [snapshot, setSnapshot] = useState<Snapshot<T>>({ reader: read, value: empty, state: "idle", error: "", readAt: "" });
  const latest = useRef(read);
  latest.current = read;
  const inFlight = useRef<AbortController | null>(null);
  /** The `reload()` promises still waiting for a read to settle. */
  const waiting = useRef<Array<() => void>>([]);
  const settle = useCallback(() => {
    const done = waiting.current;
    waiting.current = [];
    for (const resolve of done) resolve();
  }, []);

  /** Start a read, and make every read before it stale. THE READ STARTS HERE,
   *  not on the next render: a caller that awaits `reload()` is waiting on the
   *  read itself, never on React getting round to scheduling it. */
  const start = useCallback((reader: (signal: AbortSignal) => Promise<T>) => {
    inFlight.current?.abort();
    const controller = new AbortController();
    inFlight.current = controller;
    void (async () => {
      try {
        const value = await reader(controller.signal);
        if (controller.signal.aborted) return;
        setSnapshot({ reader, value, state: "ready", error: "", readAt: new Date().toISOString() });
      } catch (err) {
        if (controller.signal.aborted) return;
        setSnapshot((previous) => ({ ...previous, reader, state: "error", error: describe(err) }));
      }
      // A read that was aborted settles nothing: its waiters stay waiting for
      // the read that replaced it.
      settle();
    })();
  }, [settle]);

  const reload = useCallback(() => {
    const reader = latest.current;
    if (reader === null) return Promise.resolve();
    return new Promise<void>((resolve) => { waiting.current.push(resolve); start(reader); });
  }, [start]);

  // A window that closes with a refresher mid-wait must not leave it waiting.
  useEffect(() => () => { inFlight.current?.abort(); settle(); }, [settle]);

  useEffect(() => {
    if (read === null) { inFlight.current?.abort(); settle(); return; }
    setSnapshot((previous) => previous.reader === read && previous.readAt ? previous :
      { reader: read, value: empty, state: "loading", error: "", readAt: "" });
    start(read);
    // The caller's deps say when what is read has changed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  if (read === null || snapshot.reader !== read) return { value: empty, state: read ? "loading" : "idle", error: "", readAt: "", reload };
  return { value: snapshot.value, state: snapshot.state, error: snapshot.error, readAt: snapshot.readAt, reload };
}

const NO_ROWS: Row[] = [];

/** One page of a walked read: its rows, and the cursor that continues it ("" when it is the last). */
interface Page {
  rows: Row[];
  cursor: string;
}

const NO_PAGE: Page = { rows: NO_ROWS, cursor: "" };

/**
 * The per-recipient ledger of one campaign (or one test run), newest first.
 *
 * `v1:campaigns:delivery` stays out of broadcasts because it produces one row
 * per recipient per send, so the surface that shows it keeps it fresh with an
 * `AutoRefresh` on `reload` -- which reads the NEWEST page again -- and asks for
 * older pages one at a time with `loadMore`.
 */
export interface DeliveryLedger {
  /** Every row read so far, each once, newest first. */
  rows: Row[];
  /** The newest page's read: "loading" until it first lands, "error" when the last one was refused. */
  state: Reading<Page>["state"];
  /** The engine's sentence when the newest page's last read was refused; the rows read before stay. */
  error: string;
  /** The engine has older rows than these. */
  more: boolean;
  /** An older page is on its way. */
  loadingMore: boolean;
  /** The engine's sentence when the last older page was refused. */
  moreError: string;
  loadMore: () => void;
  reload: () => Promise<void>;
}

/**
 * NOTHING READ IS FORGOTTEN. A row is kept by its id from the first time it is
 * read, and a newer read of it replaces it -- so a refresh of the newest page
 * updates the rows it names without dropping the older ones somebody asked for,
 * and rows a busy send pushes off the newest page stay on screen. The ledger is
 * never deleted from, so what was read is still true.
 */
export function useDeliveryLedger(campaignId: string): DeliveryLedger {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || campaignId === "") return null;
    return async (signal: AbortSignal): Promise<Page> => {
      const result = await query.deliveriesForCampaign({ campaignId }, { signal });
      return { rows: result.rows(), cursor: result.meta()?.cursor ?? "" };
    };
  }, [query, campaignId]);
  const newest = useReading<Page>(NO_PAGE, read, [read]);

  // The older pages read so far, for this campaign: another campaign's are not its.
  const [walked, setWalked] = useState<{ campaignId: string; rows: Row[]; cursor: string; loading: boolean; error: string } | null>(null);
  const older = walked?.campaignId === campaignId ? walked : null;
  const seen = useRef<{ campaignId: string; rows: Map<string, Row> }>({ campaignId, rows: new Map() });
  const walking = useRef(false);

  const rows = useMemo(() => {
    if (seen.current.campaignId !== campaignId) seen.current = { campaignId, rows: new Map() };
    const kept = seen.current.rows;
    for (const row of older?.rows ?? NO_ROWS) kept.set(rowKey(row), row);
    for (const row of newest.value.rows) kept.set(rowKey(row), row);
    return [...kept.values()].sort((a, b) => Date.parse(textOf(flatten(b), "createdAt")) - Date.parse(textOf(flatten(a), "createdAt")));
  }, [campaignId, newest.value.rows, older?.rows]);

  // Older pages continue from the oldest page read; before any, from the newest.
  const cursor = older === null ? newest.value.cursor : older.cursor;

  const loadMore = useCallback(() => {
    if (query === null || cursor === "" || walking.current) return;
    walking.current = true;
    const held = older?.rows ?? NO_ROWS;
    setWalked({ campaignId, rows: held, cursor, loading: true, error: "" });
    void (async () => {
      try {
        const result = await query.deliveriesForCampaign({ campaignId }, { cursor });
        setWalked({ campaignId, rows: [...held, ...result.rows()], cursor: result.meta()?.cursor ?? "", loading: false, error: "" });
      } catch (err) {
        setWalked({ campaignId, rows: held, cursor, loading: false, error: describe(err) });
      } finally {
        walking.current = false;
      }
    })();
  }, [query, campaignId, cursor, older]);

  return {
    rows,
    state: newest.state,
    error: newest.error,
    more: cursor !== "",
    loadingMore: older?.loading ?? false,
    moreError: older?.error ?? "",
    loadMore,
    reload: newest.reload,
  };
}

/** A row's id, or its place when a row has none (a mid-write row is still a row). */
function rowKey(row: Row): string {
  return textOf(flatten(row), "id") || JSON.stringify(row);
}

/**
 * The recent test runs of one campaign, newest first (`campaignTestRuns`).
 *
 * A test run is a campaign of its own, with its own audience, counters and
 * ledger, and the campaigns feed leaves it out: the source's page reads them
 * and keeps them fresh with an `AutoRefresh` while it is seen.
 */
export function useTestRuns(campaignId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || campaignId === "") return null;
    return async (signal: AbortSignal) => (await query.campaignTestRuns({ campaignId }, { signal })).rows();
  }, [query, campaignId]);
  return useReading<Row[]>(NO_ROWS, read, [read]);
}

/**
 * The roster of one audience, INCLUDING suppressed rows.
 *
 * `recipientsForAudience` returns unsubscribes and bounces deliberately -- the
 * difference between its length and the sendable count IS the suppression
 * rate, and an operator reviewing an audience needs to see who is on it and
 * cannot be mailed. A filtered read would make those people invisible, which
 * is precisely the state that gets a list re-imported.
 */
export function useAudienceRecipients(audienceId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || audienceId === "") return null;
    return async (signal: AbortSignal) => {
      const result = await query.recipientsForAudience({ audienceId }, { signal });
      return result.rows();
    };
  }, [query, audienceId]);
  return useReading<Row[]>(NO_ROWS, read, [query, audienceId]);
}

/**
 * The server-computed outcome breakdown for one campaign.
 *
 * COMPUTED SERVER-SIDE, and that is the point of the builtin: the portal
 * counted a capped page of delivery rows in the browser, which under-reported
 * every campaign past the page bound and did so silently. Every bucket that
 * can be an exact COUNT is one here, at any audience size.
 *
 * It is refreshed while it is seen, including after a send finishes because
 * opens, clicks and unsubscribes can arrive later. The main bar also receives
 * the campaign row's progress through its existing subscription.
 */
export function useCampaignStats(campaignId: string): Reading<CampaignStats | null> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || campaignId === "") return null;
    return async (signal: AbortSignal) => {
      const result = await query.campaignStats({ campaignId }, { signal });
      const rows = result.rows();
      const first = rows[0];
      return first ? statsFromPayload(first) : null;
    };
  }, [query, campaignId]);
  return useReading<CampaignStats | null>(null, read, [query, campaignId]);
}

/** One campaign's answer within a set read together: its breakdown, or the engine's sentence. */
interface StatsAnswer {
  value: CampaignStats | null;
  error: string;
}

const NO_STATS: ReadonlyMap<string, StatsAnswer> = new Map();

/**
 * The breakdown of every campaign in a set, read together -- what the Overview
 * adds up to say how many people opened and clicked (`campaignStats`, the same
 * server-computed counts a campaign's own page shows, never a client's tally of
 * a capped page of deliveries).
 *
 * EACH CAMPAIGN IS ITS OWN ANSWER, because the engine answers one at a time and
 * a refusal of one says nothing about the rest. The map holds a `Reading` for
 * every id asked about, in the shape `useCampaignStats` answers a single one in:
 * not read yet is `loading` with no value, a refusal is `error` with the
 * engine's sentence, and an answer is `ready` -- with a value of `null` when the
 * engine had no breakdown to give, which is not a breakdown of zeros.
 *
 * A FEW AT A TIME (`readEach`), so an organization with a month of campaigns is
 * not that many requests at once, and WHAT IS KNOWN STAYS KNOWN: an answer
 * outlasts the read of a set that has changed under it and outlasts a later
 * read of the same campaign that is refused (the sentence is said beside it),
 * the way `useReading` keeps its last answer through a failure.
 *
 * It has no clock. The surface that shows the figures mounts `AutoRefresh` on
 * `reload`.
 */
export function useCampaignStatsFor(campaignIds: readonly string[]): {
  stats: ReadonlyMap<string, Reading<CampaignStats | null>>;
  reload: () => Promise<void>;
} {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  // The set, not the array: callers hand a new array every render.
  const signature = [...new Set(campaignIds)].filter((id) => id !== "").sort().join("\n");
  const wanted = useMemo(() => (signature === "" ? [] : signature.split("\n")), [signature]);
  const read = useMemo(() => {
    if (query === null || wanted.length === 0) return null;
    return async (signal: AbortSignal): Promise<ReadonlyMap<string, StatsAnswer>> => {
      const answers = await readEach(wanted, async (campaignId): Promise<[string, StatsAnswer]> => {
        try {
          const first = (await query.campaignStats({ campaignId }, { signal })).rows()[0];
          return [campaignId, { value: first ? statsFromPayload(first) : null, error: "" }];
        } catch (err) {
          // An aborted read is somebody leaving, not an answer.
          if (signal.aborted) throw err;
          return [campaignId, { value: null, error: describe(err) }];
        }
      }, signal);
      return new Map(answers);
    };
  }, [query, wanted]);
  const reading = useReading<ReadonlyMap<string, StatsAnswer>>(NO_STATS, read, [query, signature]);

  // The last answer each campaign gave and when, whatever set it was read over.
  const known = useRef(new Map<string, { value: CampaignStats | null; readAt: string }>());
  const { reload } = reading;
  const stats = useMemo(() => {
    const held = known.current;
    const out = new Map<string, Reading<CampaignStats | null>>();
    for (const id of wanted) {
      const answer = reading.value.get(id);
      const last = held.get(id);
      if (answer === undefined) {
        // Not in the read that landed: a read still on its way, or one for a
        // set this campaign joined after. What it said before stays.
        out.set(id, last === undefined
          ? { value: null, state: "loading", error: "", readAt: "", reload }
          : { value: last.value, state: "ready", error: "", readAt: last.readAt, reload });
      } else if (answer.error === "") {
        held.set(id, { value: answer.value, readAt: reading.readAt });
        out.set(id, { value: answer.value, state: "ready", error: "", readAt: reading.readAt, reload });
      } else {
        out.set(id, { value: last?.value ?? null, state: "error", error: answer.error, readAt: last?.readAt ?? "", reload });
      }
    }
    // A campaign that left the set takes its answer with it.
    const asked = new Set(wanted);
    for (const id of held.keys()) if (!asked.has(id)) held.delete(id);
    return out;
  }, [wanted, reading.value, reading.readAt, reload]);
  return { stats, reload };
}

/** Organization-specific setup, read on demand and checked again by every send.
 * A cluster operator's provider verdict cannot stand in for a client's domain.
 *
 * ONE READ PER PAGE. A campaign's page asks once and hands the answer to every
 * panel that needs it (`SendingReadiness`); two panels each asking was two
 * identical `campaignSendingReadiness` reads for one fact. */
export function useSendingReadiness(accountId: string, senderIdentityId = "", campaignId = "") {
  const connection = useOsConnection();
  const { readiness } = useSession();
  const query = connection?.query ?? null;
  const live = readiness?.loaded && readiness.state === "live";
  const scope = `${accountId}:${senderIdentityId}:${campaignId}`;
  const read = useMemo(() => {
    if (!query || !live || (!accountId && !campaignId)) return null;
    const execute = async (signal: AbortSignal): Promise<{ reader: unknown; ready: boolean; reason: string; capture: boolean }> => {
      const result = await query.campaignSendingReadiness(campaignId ? { campaignId } : { accountId, senderIdentityId }, { signal });
      const row = flatten(result.rows()[0] ?? {});
      return { reader: execute, ready: row.ready === true, capture: row.capture === true, reason: typeof row.reason === "string" ? row.reason : "" };
    };
    return execute;
  }, [query, scope, live]);
  const reading = useReading<Awaited<ReturnType<NonNullable<typeof read>>> | null>(null, read, [read]);
  return { ...reading, ready: !!live && !!read && reading.state === "ready" && reading.value?.reader === read && reading.value.ready,
    capture: !!live && !!read && reading.state === "ready" && reading.value?.reader === read && reading.value.capture,
    // THE CHECK FAILED, which says nothing about sending: a refused or broken
    // read is not an organization whose sending isn't set up, and saying so
    // would send somebody to fix a setting that is fine. `reason` is then the
    // read's own error.
    failed: !!live && !!read && reading.state === "error",
    reason: reading.error || reading.value?.reason || "Sending readiness is not confirmed." };
}

/** What `useSendingReadiness` answers, for a page to pass down. */
export type SendingReadiness = ReturnType<typeof useSendingReadiness>;

const NO_CONCEPTS: Concept[] = [];

/**
 * Every concept this cluster publishes, for the rules builder's trigger picker.
 *
 * FROM THE LIVE REGISTRY, never a hardcoded list. A rule can name a concept a
 * product bundle added after this release, which is exactly what the
 * `triggerConcept` field's own doc asks for -- and a fixed list would make the
 * newest half of a cluster's schema untriggerable with no way to tell.
 *
 * `listConcepts` is the SDK's hand-rolled escape for surfaces that need the
 * registry (`sdk/ts/src/client/query.ts`); it rides `ConceptsListMsg` on the
 * same stream every other read uses. There is no OS-wide accessor for it --
 * this is the shell's first surface to need one -- so it is read here, in the
 * app that needs it, rather than promoted on a single use.
 */
export function useTriggerConcepts(): Reading<Concept[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null) return null;
    return async (signal: AbortSignal) => query.listConcepts({ signal });
  }, [query]);
  return useReading<Concept[]>(NO_CONCEPTS, read, [query]);
}

/**
 * The campaigns filed under one account, for the Accounts ledger's fifth band.
 *
 * IT LIVES HERE BECAUSE THE CONCEPT DOES. `apps/accounts/tie.tsx` states the
 * rule for the other direction -- a tie surface belongs to the domain that
 * owns the concept -- and this is the same rule read the other way round: the
 * Accounts detail renders the band, and the read that fills it is the
 * Email app's to own. Nothing in this module imports from `apps/accounts`,
 * so the two apps do not form a cycle.
 *
 * The shape is the Accounts ledger's own `Rollup`, deliberately: five bands
 * that settle independently and print one read time between them only work if
 * the fifth answers in the same vocabulary as the four.
 */
export function useAccountCampaignsRollup(accountId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || accountId === "") return null;
    return async (signal: AbortSignal) => {
      const result = await query.campaignsForAccount({ accountId }, { signal });
      return result.rows();
    };
  }, [query, accountId]);
  return useReading<Row[]>(NO_ROWS, read, [query, accountId]);
}

/**
 * Whether authored automations are running at all, cluster-wide.
 *
 * ===========================================================================
 * THE SILENT FAILURE THIS READ EXISTS TO REMOVE
 * ===========================================================================
 * `v1:identity:clusterSettings.authoredAutomationsEnabled` is a GLOBAL hard
 * stop: with it off, `AuthoredScheduler`'s `GlobalGate` suppresses every
 * firing for every owner on every node, checked before owner-gating and before
 * the breaker. Nothing about a rule row changes -- each one still reads
 * `active` -- so an operator sees a list of active rules that send nothing,
 * with no way anywhere on screen to find out that a cluster-level switch is
 * the reason.
 *
 * ON DEMAND, because `v1:identity:clusterSettings` carries no broadcast
 * routing rule; the section prints when it looked, the way every other
 * non-live surface in this app does.
 *
 * IT IS READ ONCE, IN THE RULES SECTION. It is not a campaigns fact and it
 * says nothing about a send, so putting it at the app root would carry a line
 * about automations onto four surfaces that have none.
 */
export function useAuthoredAutomations(): Reading<AuthoredAutomationsState> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null) return null;
    return async (signal: AbortSignal) => {
      const result = await query.clusterSettingsCurrent({}, { signal });
      // A read that comes back with NO ROW is "unknown", not "running". The
      // concept declares no authz tier today, and the day one is declared an
      // unadmitted read returns zero rows rather than an error (memql#4309) --
      // so an empty result is the shape a future refusal will arrive in, and
      // reading it as "the switch is on" would be inventing the answer.
      return authoredAutomationsFrom(result.rows()[0] ?? null);
    };
  }, [query]);
  return useReading<AuthoredAutomationsState>("unknown", read, [query]);
}

/** Signup receipts are shopper-volume data: a bounded view, kept fresh by the
 *  `AutoRefresh` beside it while it is seen. */
export function useNewsletterWelcomes(audienceId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => query === null ? null : async (signal: AbortSignal) =>
    (await query.newsletterWelcomesForAudience({ audienceId }, { signal })).rows(), [query, audienceId]);
  return useReading<Row[]>(NO_ROWS, read, [query, audienceId]);
}

const NO_COUNTS: ReadonlyMap<string, Figure> = new Map();

/**
 * How many people each audience in view can be mailed now.
 *
 * ONE READ PER AUDIENCE, because the engine counts one audience at a time
 * (`audienceSubscriberCount`, a server-side aggregate: the only honest number
 * for a list too large to page through). The figures are `Figure`s, so the
 * three things a count can be stay three things:
 *
 *   - NOT READ YET is `absent("unread")` -- every audience asked about starts
 *     there, and a zero would be this window claiming an empty list;
 *   - a number is `measured`, zero included: a list the engine counted and
 *     found empty is genuinely zero;
 *   - a refusal or a failure is `absent("failed", <the engine's sentence>)`,
 *     for THAT audience alone -- one list the caller may not count does not
 *     take the others' numbers with it.
 *
 * Subscribers join and leave through imports and signups that are routing
 * exclusions, so there is no event to follow: the surface that shows the
 * numbers mounts an `AutoRefresh` on `reload`.
 *
 * A FEW AT A TIME, and WHAT IS KNOWN STAYS KNOWN. The audiences are counted six
 * at once (`readEach`), so an organization with forty is not forty requests on
 * every refresh. And when the set changes -- an audience somebody just added in
 * another tab arrives -- the counts already read stay on screen while the new
 * read runs, rather than every one going back to "not read yet" for it.
 */
export function useSubscriberCounts(audienceIds: readonly string[]): { counts: ReadonlyMap<string, Figure>; reload: () => Promise<void> } {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  // The set, not the array: callers hand a new array every render.
  const signature = [...new Set(audienceIds)].filter((id) => id !== "").sort().join("\n");
  const wanted = useMemo(() => (signature === "" ? [] : signature.split("\n")), [signature]);
  const read = useMemo(() => {
    if (query === null || wanted.length === 0) return null;
    return async (signal: AbortSignal): Promise<ReadonlyMap<string, Figure>> => {
      const answers = await readEach(wanted, async (audienceId): Promise<[string, Figure]> => {
        try {
          const result = await query.audienceSubscriberCount({ audienceId }, { signal });
          return [audienceId, figureFrom(result.rows()[0] ?? null, "count")];
        } catch (err) {
          // An aborted read is somebody leaving, not an answer.
          if (signal.aborted) throw err;
          return [audienceId, absent("failed", describe(err))];
        }
      }, signal);
      return new Map(answers);
    };
  }, [query, wanted]);
  const reading = useReading<ReadonlyMap<string, Figure>>(NO_COUNTS, read, [query, signature]);
  // The last answer that landed, whatever set it was read over: a reading that
  // starts over for a new set hands back nothing until it lands.
  const known = useRef<ReadonlyMap<string, Figure>>(NO_COUNTS);
  if (reading.value !== NO_COUNTS) known.current = reading.value;
  const held = known.current;
  const counts = useMemo(
    () => new Map(wanted.map((id): [string, Figure] => [id, reading.value.get(id) ?? held.get(id) ?? absent("unread")])),
    [wanted, reading.value, held],
  );
  return { counts, reload: reading.reload };
}
