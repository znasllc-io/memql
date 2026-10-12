import { useCallback, useRef, useState } from "react";
import { newShortId, refusalSentence, type Connection, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import { lifecycleContent } from "./templates/templateActs";
import {
  campaignName,
  layoutRecordFromRow,
  templateRecordFromRow,
  type AudienceRow,
  type CampaignRow,
  type LayoutRow,
  type RecipientMode,
  type TemplateRow,
} from "./rows";
import { WORDS } from "./words";

// Every write the Email app makes, and the busy/error pair each one owns.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// Every operator-facing campaigns concept declares the composite tier, so
// `guardRowAuthzWrite` resolves the target row and admits its owner, with the
// cluster-owner path as the separate explicit escape. The five builtins go
// further: their FIRST act is an owned-tier read of the campaign, so a caller
// who cannot read a campaign cannot start, schedule, pause, resume or test it,
// and every value that reaches the send job is copied off that row rather than
// taken from an argument. Editing a boolean in a browser changes none of it.
//
// THE SEND LIFECYCLE GOES THROUGH THE BUILTINS AND NOWHERE ELSE.
// `startCampaign`, `pauseCampaign`, `resumeCampaign`, `scheduleCampaign`,
// `updateCampaignProgress` and `recordCampaignDelivery` are all `@serverOnly`
// (memql#4820's hardening), so a browser cannot reach them at all -- which is
// the point: flipping a status to "sending" with no send job behind it, or
// stamping counters for deliveries that never happened, desyncs the row from
// the engine while owning it perfectly. What this file calls is
// `campaignStartSend` and its four siblings, which do the preflight first.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE, AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast. The preflight refusals are the most useful sentences in this
// app -- "no email sender is registered on this node", "the template is not
// marked ready", "the audience is at the ceiling" -- and each names the exact
// thing to go and fix. A paraphrase would drop that. Every hook below owns its
// own error slot so a refusal always appears under the control that caused it.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Whether a write's answer was LOST rather than refused (memql#6033): the
 * request went out and the stream closed before anything came back, so the
 * cluster may well have done it. A refusal is the engine's own sentence about
 * a write it did not do; a request that never left ("send failed") did nothing.
 * Saying "it wasn't sent" over a send that may have started is the one thing
 * a page must not do.
 */
export function answerLost(err: unknown): boolean {
  const reason = typeof err === "object" && err !== null ? (err as { reason?: unknown }).reason : undefined;
  if (reason !== "transport" && reason !== "closed") return false;
  return !(err instanceof Error && err.message.startsWith("send failed"));
}

/**
 * Blank strings are OMITTED rather than sent.
 *
 * Every update mutation here read-merges, so an omitted field inherits its
 * stored value -- which is what lets an edit form send only what changed. An
 * explicit "" is a VALUE and would blank the stored one.
 */
function omitBlank(value: string | undefined): string | undefined {
  if (value === undefined) return undefined;
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

export interface WriteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  reset: () => void;
}

/** A write whose answer can be lost on the way back, and which says when it was. */
export interface MayBeLost {
  /** True when the last call's answer never came back (`answerLost`): it may have been done. Read once the call has returned. */
  lost: () => boolean;
}

/**
 * The busy/error/reset triple, once.
 *
 * ONE FACTORY RATHER THAN TWENTY COPIES. The Accounts app writes its three
 * hooks out longhand and that is right at three; this app makes twenty
 * distinct writes, and twenty copies of the same eleven lines is twenty places
 * for one of them to forget `finally { setBusy(false) }` -- which presents as
 * a button that never comes back and is invisible in review.
 *
 * What is NOT shared is the state itself: every call to this returns its own
 * `useState` pair, so each hook owns its own slot and a refusal from the
 * import panel can never appear under the send button.
 */
function useWrite<A extends unknown[], R>(
  run: (query: Connection["query"], ...args: A) => Promise<R>,
  onDisconnected: R,
): { busy: boolean; error: string; lost: () => boolean; reset: () => void; call: (...args: A) => Promise<R> } {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // A ref, not state: it is read in the same handler that awaited the call.
  const lostAnswer = useRef(false);

  const call = useCallback(
    async (...args: A): Promise<R> => {
      const query = connection?.query ?? null;
      lostAnswer.current = false;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return onDisconnected;
      }
      setBusy(true);
      setError("");
      try {
        return await run(query, ...args);
      } catch (err: unknown) {
        lostAnswer.current = answerLost(err);
        setError(describe(err));
        return onDisconnected;
      } finally {
        setBusy(false);
      }
    },
    // `run` is a fresh closure each render and closes over nothing that
    // changes what the call DOES, so it is deliberately not a dependency --
    // the same reasoning `useLiveCollection` records for its spec.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [connection],
  );
  const lost = useCallback(() => lostAnswer.current, []);

  return { busy, error, lost, reset: () => setError(""), call };
}

// ---------------------------------------------------------------------------
// Campaigns
// ---------------------------------------------------------------------------

export interface CampaignFacts {
  name: string;
  audienceId: string;
  templateId: string;
  fromName: string;
  replyTo: string;
  scheduledAt: string;
  accountId: string;
  senderIdentityId: string;
  trackOpens: boolean;
  trackClicks: boolean;
}

export interface CreateCampaignState extends WriteState {
  /**
   * `campaignId` is the CALLER'S, held for the life of the flow that adds the
   * campaign: an add whose answer was lost, or whose refusal the person fixed,
   * is tried again as the same campaign -- never a second draft somebody then
   * has to find and cancel. True when the cluster took it.
   */
  create: (facts: CampaignFacts, campaignId: string) => Promise<boolean>;
}

export function useCreateCampaign(): CreateCampaignState {
  const { busy, error, reset, call } = useWrite(async (query, facts: CampaignFacts, campaignId: string) => {
    await query.createCampaign({
      campaignId,
      name: facts.name.trim(),
      audienceId: facts.audienceId,
      templateId: facts.templateId,
      fromName: omitBlank(facts.fromName),
      replyTo: omitBlank(facts.replyTo),
      scheduledAt: omitBlank(facts.scheduledAt),
      accountId: facts.accountId.trim(),
      senderIdentityId: omitBlank(facts.senderIdentityId),
      trackOpens: facts.trackOpens,
      trackClicks: facts.trackClicks,
    });
    // NOTHING IS INSERTED LOCALLY. `v1:campaigns:campaign` broadcasts both
    // verbs, so the row arrives on the feed the list already draws -- with the
    // arrival cue, exactly like a campaign somebody else created.
    return true;
  }, false);
  return { busy, error, reset, create: call };
}

export interface UpdateCampaignState extends WriteState {
  update: (campaignId: string, facts: CampaignFacts) => Promise<boolean>;
}

/**
 * `updateCampaign` takes name / audience / template as REQUIRED arguments, so
 * this is a whole-form save rather than a patch -- the form holds the current
 * values and sends them back. The optional half is still omitted when blank,
 * so clearing a reply-to means clearing it and leaving it alone means leaving
 * it alone.
 */
export function useUpdateCampaign(): UpdateCampaignState {
  const { busy, error, reset, call } = useWrite(
    async (query, campaignId: string, facts: CampaignFacts) => {
      await query.updateCampaign({
        campaignId,
        name: facts.name.trim(),
        audienceId: facts.audienceId,
        templateId: facts.templateId,
        fromName: omitBlank(facts.fromName),
        replyTo: omitBlank(facts.replyTo),
        scheduledAt: omitBlank(facts.scheduledAt),
        accountId: facts.accountId.trim(),
        senderIdentityId: omitBlank(facts.senderIdentityId),
        trackOpens: facts.trackOpens,
        trackClicks: facts.trackClicks,
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

export interface SendControlState extends WriteState, MayBeLost {
  start: (campaignId: string) => Promise<boolean>;
  schedule: (campaignId: string, scheduledAt: string) => Promise<boolean>;
  pause: (campaignId: string) => Promise<boolean>;
  resume: (campaignId: string) => Promise<boolean>;
  cancel: (campaignId: string) => Promise<boolean>;
}

/**
 * Several writes that share ONE place on screen, and so one refusal: the one
 * pressed LAST. Each write keeps its own refusal until it is called again, so
 * "the first that has one" was an older act's sentence standing over the
 * current one -- a Send now refused an hour ago masking the Cancel refused
 * now, or reading as the reason a later act that worked did not.
 */
function useLastPressed(parts: readonly { error: string; lost: () => boolean }[]): { error: string; lost: () => boolean; press: (index: number) => void } {
  const [last, setLast] = useState(-1);
  // Read in the handler that pressed, before the state above has rendered.
  const pressed = useRef(-1);
  return {
    error: last >= 0 ? (parts[last]?.error ?? "") : "",
    lost: () => (pressed.current >= 0 ? (parts[pressed.current]?.lost() ?? false) : false),
    press: (index) => {
      pressed.current = index;
      setLast(index);
    },
  };
}

/**
 * The five lifecycle controls, sharing ONE error slot -- and here that is
 * correct rather than a shortcut.
 *
 * They are acts of one bar, exactly one can be pressed at a time (a draft has
 * Send now, a run has Pause), and the refusal belongs to the one pressed last.
 * Splitting them would put five slots under one bar. This is the case the
 * Accounts note's "three different places on screen" rule is drawing a line
 * AGAINST, not an exception to it.
 */
export function useSendControls(): SendControlState {
  const start = useWrite(async (query, campaignId: string) => {
    await query.campaignStartSend({ campaignId });
    return true;
  }, false);
  const schedule = useWrite(
    async (query, campaignId: string, scheduledAt: string) => {
      await query.campaignScheduleSend({ campaignId, scheduledAt });
      return true;
    },
    false,
  );
  const pause = useWrite(async (query, campaignId: string) => {
    await query.campaignPauseSend({ campaignId });
    return true;
  }, false);
  const resume = useWrite(async (query, campaignId: string) => {
    await query.campaignResumeSend({ campaignId });
    return true;
  }, false);
  const cancel = useWrite(async (query, campaignId: string) => {
    await query.cancelCampaign({ campaignId });
    return true;
  }, false);

  const parts = [start, schedule, pause, resume, cancel];
  const last = useLastPressed(parts);
  return {
    busy: parts.some((p) => p.busy),
    error: last.error,
    lost: last.lost,
    reset: () => parts.forEach((p) => p.reset()),
    start: (campaignId) => { last.press(0); return start.call(campaignId); },
    schedule: (campaignId, scheduledAt) => { last.press(1); return schedule.call(campaignId, scheduledAt); },
    pause: (campaignId) => { last.press(2); return pause.call(campaignId); },
    resume: (campaignId) => { last.press(3); return resume.call(campaignId); },
    cancel: (campaignId) => { last.press(4); return cancel.call(campaignId); },
  };
}

/** What a repeat is saved with. A new one has no revision; a change to one names the exact one it was read at. */
export interface SeriesSave {
  campaignId: string;
  intervalWeeks: number;
  /** RFC 3339: the first occurrence, which the cadence is counted from. */
  firstSendAt: string;
  /** IANA zone the cadence keeps its local day and time in. */
  timeZone: string;
  expectedRevision?: string;
}

export interface SeriesControlState extends WriteState, MayBeLost {
  save: (series: SeriesSave) => Promise<boolean>;
  pause: (campaignId: string, expectedRevision: string) => Promise<boolean>;
  resume: (campaignId: string, expectedRevision: string) => Promise<boolean>;
}

/**
 * Start, change, pause and resume a campaign that repeats -- the one hook every
 * series write goes through, Add a campaign's Start repeating among them.
 *
 * EVERY CHANGE NAMES THE REVISION IT WAS READ AT, and the engine refuses a
 * stale one ("this recurring campaign changed; reload it before saving") rather
 * than letting two people's changes land on each other. Saving also captures
 * the campaign as it stands now: an occurrence is made from what the series
 * holds, so edits to the draft reach the mail only through a save here.
 */
export function useSeriesControls(): SeriesControlState {
  const save = useWrite(async (query, series: SeriesSave) => {
    const { expectedRevision, ...rest } = series;
    await query.campaignConfigureSeries({ ...rest, action: "save", ...(expectedRevision ? { expectedRevision } : {}) });
    return true;
  }, false);
  const pause = useWrite(async (query, campaignId: string, expectedRevision: string) => {
    await query.campaignConfigureSeries({ campaignId, action: "pause", expectedRevision });
    return true;
  }, false);
  const resume = useWrite(async (query, campaignId: string, expectedRevision: string) => {
    await query.campaignConfigureSeries({ campaignId, action: "resume", expectedRevision });
    return true;
  }, false);

  const parts = [save, pause, resume];
  const last = useLastPressed(parts);
  return {
    busy: parts.some((p) => p.busy),
    error: last.error,
    lost: last.lost,
    reset: () => parts.forEach((p) => p.reset()),
    save: (series) => { last.press(0); return save.call(series); },
    pause: (campaignId, revision) => { last.press(1); return pause.call(campaignId, revision); },
    resume: (campaignId, revision) => { last.press(2); return resume.call(campaignId, revision); },
  };
}

export interface DuplicateCampaignState extends WriteState, MayBeLost {
  /**
   * A new draft with the same audience, template, sender, reply-to and
   * tracking, named "<name> (copy)" and scheduled for nothing. `copyId` is the
   * CALLER'S, held until the copy is made: a duplicate whose answer was lost is
   * tried again as the same copy, never a second one.
   */
  duplicate: (campaign: CampaignRow, copyId: string) => Promise<boolean>;
}

export function useDuplicateCampaign(): DuplicateCampaignState {
  const { busy, error, lost, reset, call } = useWrite(async (query, campaign: CampaignRow, copyId: string) => {
    await query.createCampaign({
      campaignId: copyId,
      name: `${campaignName(campaign)} (copy)`,
      audienceId: campaign.audienceId,
      templateId: campaign.templateId,
      fromName: omitBlank(campaign.fromName),
      replyTo: omitBlank(campaign.replyTo),
      accountId: omitBlank(campaign.accountId),
      senderIdentityId: omitBlank(campaign.senderIdentityId),
      trackOpens: campaign.trackOpens,
      trackClicks: campaign.trackClicks,
    });
    return true;
  }, false);
  return { busy, error, lost, reset, duplicate: call };
}

/** What the Edit page changed. A field left out did not change, and is not sent. */
export interface CampaignChanges {
  name?: string;
  audienceId?: string;
  templateId?: string;
  replyTo?: string;
  scheduledAt?: string;
  accountId?: string;
  senderIdentityId?: string;
  trackOpens?: boolean;
  trackClicks?: boolean;
}

const OPTIONAL_CHANGES = ["replyTo", "scheduledAt", "accountId", "senderIdentityId", "trackOpens", "trackClicks"] as const;

/**
 * The campaign fields a series repeats: the engine's `seriesDefinition`, and
 * the name an occurrence is called by. An occurrence is made from what the
 * series CAPTURED, never from the draft, so an edit to one of these reaches the
 * mail only through a series save. The organization is not among them: a
 * series never moves to another.
 */
const SERIES_CAPTURES = ["name", "audienceId", "templateId", "senderIdentityId", "replyTo", "trackOpens", "trackClicks"] as const satisfies readonly (keyof CampaignChanges)[];

/**
 * Whether an edit of a repeating campaign has to save its series: something
 * the series repeats changed, or how often it repeats, or a series save that
 * was refused is still owed. Never to restart it -- a save keeps the series'
 * status, and only Resume changes that (memql#6033).
 */
export function seriesSaveNeeded(changes: CampaignChanges, cadenceChanged: boolean, owed: boolean): boolean {
  return cadenceChanged || owed || SERIES_CAPTURES.some((key) => changes[key] !== undefined);
}

export interface EditCampaignState extends WriteState, MayBeLost {
  save: (campaign: CampaignRow, changes: CampaignChanges) => Promise<boolean>;
}

/**
 * Save what the Edit page changed, and only that.
 *
 * `updateCampaign` read-merges: a field it is not sent keeps what is stored, so
 * a field nobody touched is left out and a change made meanwhile somewhere else
 * survives. Name, audience and template are REQUIRED by the mutation, so they
 * always go -- changed, or as stored.
 *
 * A FIELD SOMEBODY EMPTIED IS A CHANGE, sent as "": no reply-to (replies come
 * to the sender), this cluster's own sender, no saved time (sent when somebody
 * presses Send now). Each of those is what an empty value means on the
 * concept, and leaving it out would keep the old value while the page said it
 * was saved. (Add a campaign's own write, `useUpdateCampaign`, still omits its
 * blanks: those are fields it never sets.)
 */
export function useEditCampaign(): EditCampaignState {
  const { busy, error, lost, reset, call } = useWrite(async (query, campaign: CampaignRow, changes: CampaignChanges) => {
    const args: Parameters<Connection["query"]["updateCampaign"]>[0] = {
      campaignId: campaign.id,
      name: (changes.name ?? campaign.name).trim(),
      audienceId: changes.audienceId ?? campaign.audienceId,
      templateId: changes.templateId ?? campaign.templateId,
    };
    for (const key of OPTIONAL_CHANGES) {
      const value = changes[key];
      if (value !== undefined) Object.assign(args, { [key]: typeof value === "string" ? value.trim() : value });
    }
    await query.updateCampaign(args);
    return true;
  }, false);
  return { busy, error, lost, reset, save: call };
}

export interface TestSendState extends WriteState, MayBeLost {
  runId: string;
  /** The merge tags the last test to an address could not resolve, as the engine wrote them. */
  unresolved: string[];
  /** A test run to the organization's testing audience. */
  send: (campaignId: string, audienceId: string) => Promise<boolean>;
  /**
   * One test copy to one address -- the person's own, when the organization has
   * no testing audience. It reads a draft template too, and reports the merge
   * tags it could not fill.
   */
  sendToMe: (campaignId: string, to: string) => Promise<boolean>;
}

/** Retain the request id after an uncertain response. Retrying must recover
 * the same persisted run, even when another replica answers the request. */
export function useTestSend(): TestSendState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [runId, setRunId] = useState("");
  const [unresolved, setUnresolved] = useState<string[]>([]);
  const request = useRef<{ scope: string; id: string } | null>(null);
  // ONE TEST AT A TIME, whichever way it goes: a second press while the first is
  // on its way is the same press.
  const pending = useRef(false);
  const lostAnswer = useRef(false);
  const send = useCallback(async (campaignId: string, audienceId: string) => {
    if (pending.current) return false;
    if (!connection || !audienceId) { setError("Choose a testing audience in Settings."); return false; }
    const scope = `${campaignId}:${audienceId}`;
    if (request.current?.scope !== scope) request.current = { scope, id: crypto.randomUUID() };
    pending.current = true; lostAnswer.current = false; setBusy(true); setError(""); setUnresolved([]);
    try {
      const result = await connection.query.campaignTestAudienceSend({ campaignId, audienceId, requestId: request.current.id });
      const row = flatten(result.rows()[0] ?? {});
      if (typeof row.testRunId !== "string" || !row.testRunId) throw new Error("The cluster did not confirm the test run. Try again to recover its result.");
      setRunId(row.testRunId); request.current = null;
      return true;
    } catch (err) { lostAnswer.current = answerLost(err); setError(describe(err)); return false; }
    finally { pending.current = false; setBusy(false); }
  }, [connection]);
  const sendToMe = useCallback(async (campaignId: string, to: string) => {
    if (pending.current) return false;
    if (!connection) { setError("Not connected to the cluster, so nothing was sent."); return false; }
    pending.current = true; lostAnswer.current = false; setBusy(true); setError(""); setRunId(""); setUnresolved([]);
    try {
      const result = await connection.query.campaignTestSend({ campaignId, to });
      setUnresolved(unresolvedTagsFrom(result.rows()[0] ?? null));
      return true;
    } catch (err) { lostAnswer.current = answerLost(err); setError(describe(err)); return false; }
    finally { pending.current = false; setBusy(false); }
  }, [connection]);
  return {
    busy,
    error,
    runId,
    unresolved,
    send,
    sendToMe,
    lost: useCallback(() => lostAnswer.current, []),
    reset: () => { setError(""); setRunId(""); setUnresolved([]); request.current = null; },
  };
}

/**
 * Pull the unresolved-tag list out of a test-send reply.
 *
 * TWO SPELLINGS ARE READ, and neither is guessed at: the builtin's own
 * description says it "returns the list of merge tags it could not resolve"
 * without naming the key, so both the obvious ones are read and anything that
 * is not a list of strings is treated as "it said nothing" -- which renders as
 * a clean test rather than as an error about a reply we did not understand.
 */
export function unresolvedTagsFrom(row: Row | null): string[] {
  if (row === null) return [];
  const flat = flatten(row);
  for (const key of ["unresolved", "unresolvedTags"]) {
    const v = flat[key];
    if (Array.isArray(v)) return v.filter((m): m is string => typeof m === "string");
  }
  return [];
}

// ---------------------------------------------------------------------------
// Audiences and recipients
// ---------------------------------------------------------------------------

export interface CreateAudienceState extends WriteState {
  create: (facts: { name: string; description: string; accountId: string }) => Promise<string>;
}

export function useCreateAudience(): CreateAudienceState {
  const { busy, error, reset, call } = useWrite(
    async (query, facts: { name: string; description: string; accountId: string }) => {
      const audienceId = newShortId();
      await query.createAudience({
        audienceId,
        name: facts.name.trim(),
        description: omitBlank(facts.description),
        accountId: facts.accountId.trim(),
      });
      return audienceId;
    },
    "",
  );
  return { busy, error, reset, create: call };
}

export interface ArchiveAudienceState extends WriteState {
  archive: (audienceId: string) => Promise<boolean>;
}

export function useArchiveAudience(): ArchiveAudienceState {
  const { busy, error, reset, call } = useWrite(async (query, audienceId: string) => {
    await query.archiveAudience({ audienceId });
    return true;
  }, false);
  return { busy, error, reset, archive: call };
}

export interface AddRecipientState extends WriteState {
  add: (audience: Pick<AudienceRow, "id" | "accountId">, email: string, displayName: string) => Promise<boolean>;
}

export function useAddRecipient(): AddRecipientState {
  const { busy, error, reset, call } = useWrite(
    async (query, audience: Pick<AudienceRow, "id" | "accountId">, email: string, displayName: string) => {
      await query.addRecipient({
        recipientId: newShortId(),
        audienceId: audience.id,
        accountId: audience.accountId,
        email: email.trim(),
        displayName: omitBlank(displayName),
        source: "manual",
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, add: call };
}

export interface SetSubscriptionState extends WriteState {
  set: (recipientId: string, subscriptionStatus: string) => Promise<boolean>;
}

/**
 * Honour an unsubscribe by hand.
 *
 * `unsubscribedAt` is stamped HERE, with this browser's clock, only when the
 * state being written is a leaving one. The mutation threads it from the
 * caller rather than stamping `now` because the same write records a change
 * that happened earlier -- support forwarding an unsubscribe, a bounce report
 * read hours late -- and an operator doing it in this panel is doing it now.
 */
export function useSetSubscription(): SetSubscriptionState {
  const { busy, error, reset, call } = useWrite(
    async (query, recipientId: string, subscriptionStatus: string) => {
      await query.setRecipientSubscription({
        recipientId,
        subscriptionStatus,
        unsubscribedAt:
          subscriptionStatus === "subscribed" ? undefined : new Date().toISOString(),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, set: call };
}

/** What an import made of a file. Every figure the builtin reports. */
export interface ImportReport {
  added: number;
  duplicates: number;
  invalid: number;
  total: number;
  /** Up to twenty bad lines with their line numbers, verbatim. */
  samples: { line: number; text: string; reason: string }[];
}

export interface ImportState extends WriteState {
  /** The last report, kept until the person dismisses it. */
  report: ImportReport | null;
  dismiss: () => void;
  run: (audienceId: string, artifactId: string) => Promise<boolean>;
}

function numberAt(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) return Number(v);
  return 0;
}

/**
 * Read the import's report out of the builtin's reply.
 *
 * The sample rows are read defensively for the same reason
 * `unresolvedTagsFrom` is: the shape is documented in prose ("up to 20 sample
 * invalid lines with their line numbers") rather than in a schema this client
 * shares. A sample that carries no recognisable line number keeps its text and
 * shows no number, which is more useful than dropping the evidence.
 */
export function importReportFrom(row: Row | null): ImportReport {
  if (row === null) return { added: 0, duplicates: 0, invalid: 0, total: 0, samples: [] };
  const flat = flatten(row);
  // `invalidLines` FIRST, because that is what the engine actually sends
  // (component/campaigns/import.go). The three spellings after it were written
  // against the builtin's prose description before the Go side existed, and
  // they are kept rather than deleted: this reader's whole job is that an
  // unrecognised reply degrades to "it said nothing" instead of throwing, and
  // a reader that only knew one key would have no fallback the day the reply
  // gains a second shape. What is NOT acceptable is the state this was in --
  // knowing three keys and not the real one, which renders an empty report
  // beside a non-zero invalid count and looks like a clean file.
  const rawSamples =
    flat["invalidLines"] ?? flat["samples"] ?? flat["invalidSamples"] ?? flat["sampleInvalid"];
  const samples = Array.isArray(rawSamples)
    ? rawSamples
        .filter((s): s is Row => !!s && typeof s === "object" && !Array.isArray(s))
        .map((s) => ({
          line: numberAt(s, "line") || numberAt(s, "lineNumber"),
          // `value` is the engine's name for the offending text. `text` and a
          // STRING-valued `line` stay as fallbacks for the same reason above --
          // and note `line` is a NUMBER in the engine's shape, so the string
          // check is what keeps the two readings from colliding.
          text:
            typeof s["value"] === "string"
              ? s["value"]
              : typeof s["text"] === "string"
                ? s["text"]
                : typeof s["line"] === "string"
                  ? s["line"]
                  : "",
          reason: typeof s["reason"] === "string" ? s["reason"] : "",
        }))
    : [];
  return {
    added: numberAt(flat, "added"),
    duplicates: numberAt(flat, "duplicates"),
    invalid: numberAt(flat, "invalid"),
    total: numberAt(flat, "total"),
    samples,
  };
}

/**
 * Import a CSV that is already in the Library.
 *
 * TWO STEPS, ONE OF WHICH IS NOT OURS. The file goes up through the shell's
 * one upload path (`items/edgeUpload.ts` -- `test/files/onePath.test.ts` fails
 * the build on a second speaker of that wire), and this call hands the
 * resulting artifact id to the engine, which reads the file SERVER-SIDE under
 * the caller's own actor. A file the caller cannot read is a file this cannot
 * import: the artifact id is not a capability.
 *
 * THE REPORT PERSISTS. It is held here rather than shown and forgotten,
 * because the operator's next action is fixing the file -- and evidence that
 * disappears on a timer is evidence they have to re-run an import to see
 * again.
 */
export function useImportRecipients(): ImportState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [report, setReport] = useState<ImportReport | null>(null);

  const run = useCallback(
    async (audienceId: string, artifactId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was imported.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        const result = await query.campaignImportRecipients({
          audienceId,
          artifactId,
          hasHeader: true,
        });
        setReport(importReportFrom(result.rows()[0] ?? null));
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    report,
    run,
    dismiss: () => setReport(null),
    reset: () => setError(""),
  };
}

// ---------------------------------------------------------------------------
// Templates and layouts: put away, or brought back -- never written
// ---------------------------------------------------------------------------
//
// THE OS NEVER CREATES A TEMPLATE OR SAVES A LAYOUT (spec 1.6, Rulings 9 and
// 24). A template is written in the editor and published from there; a layout
// is saved from a finished email in the editor. What the OS does to either is
// archive it or restore it, and those are the only two calls to the engine's
// save builtins in the app -- one hook each, below (pinned by
// test/email/templates/templateCreation.test.ts).
//
// NEITHER IS AN EDIT. The engine takes the exact revision and the action, and
// ignores the content; what goes back as content is what is STORED, read whole
// by id for this call -- a list row holds no body, and sending a row's missing
// body back as an edit is how an email would be written empty.

/** A template or a layout put away or brought back: the busy/error pair, and whether the answer was lost. */
export interface LifecycleState extends WriteState, MayBeLost {}

export interface TemplateLifecycle extends LifecycleState {
  /** Archive the template, at the revision this row was read at. */
  archive: (template: TemplateRow) => Promise<boolean>;
  /** Bring an archived template back, as a draft. */
  restore: (template: TemplateRow) => Promise<boolean>;
}

/**
 * Archiving and restoring a template. The revision is the one the page was
 * showing (the list row's `createdAt`), so a template saved somewhere else
 * since is refused in the engine's words rather than archived unseen; the
 * content is the template's own, read by id (`templateById`) for the call.
 */
export function useTemplateLifecycle(): TemplateLifecycle {
  // Which call a refusal came from, so its sentence loses that call's name.
  const calling = useRef("");
  const write = useWrite(async (query, template: TemplateRow, action: "archive" | "restore") => {
    calling.current = "templateById";
    const stored = (await query.templateById({ templateId: template.id })).rows()[0];
    if (stored === undefined) throw new Error(WORDS.unavailable.template);
    const base = {
      templateId: template.id,
      name: template.name,
      accountId: template.accountId,
      expectedRevision: template.createdAt,
      content: lifecycleContent(templateRecordFromRow(stored)),
    };
    calling.current = "campaignSaveTemplate";
    const result =
      action === "archive"
        ? await query.campaignSaveTemplate({ ...base, action: "archive" })
        : await query.campaignSaveTemplate({ ...base, action: "restore" });
    if (!result.rows().map(flatten).some((row) => row.saved === true && typeof row.revision === "string" && row.revision !== "")) {
      throw new Error("The cluster did not confirm the template change.");
    }
    return true;
  }, false);
  return {
    busy: write.busy,
    error: write.error === "" ? "" : refusalSentence(write.error, calling.current),
    lost: write.lost,
    reset: write.reset,
    archive: (template) => write.call(template, "archive"),
    restore: (template) => write.call(template, "restore"),
  };
}

export interface LayoutLifecycle extends LifecycleState {
  /** Archive an organization's layout: the editor stops offering it. */
  archive: (layout: LayoutRow) => Promise<boolean>;
  /** Bring an archived layout back. */
  restore: (layout: LayoutRow) => Promise<boolean>;
}

/**
 * Archiving and restoring an organization's layout (Ruling 24). The engine
 * holds the call to the exact revision `emailLayoutById` answers, so the layout
 * is read by its bare id for the call, and its stored name and document go back
 * as they are -- the engine ignores both for these two actions. A built-in
 * layout has no lifecycle and never reaches here.
 */
export function useLayoutLifecycle(): LayoutLifecycle {
  const calling = useRef("");
  const write = useWrite(async (query, layout: LayoutRow, action: "archive" | "restore") => {
    const layoutId = layout.layoutId ?? "";
    calling.current = "emailLayoutById";
    const stored = layoutId === "" ? undefined : (await query.emailLayoutById({ layoutId })).rows()[0];
    if (stored === undefined) throw new Error(WORDS.unavailable.layout);
    const record = layoutRecordFromRow(stored);
    const base = {
      layoutId,
      accountId: record.accountId || layout.accountId,
      name: record.name,
      ...(record.document === null ? {} : { document: record.document }),
      expectedRevision: record.createdAt,
    };
    calling.current = "campaignSaveLayout";
    const result =
      action === "archive"
        ? await query.campaignSaveLayout({ ...base, action: "archive" })
        : await query.campaignSaveLayout({ ...base, action: "restore" });
    if (!result.rows().map(flatten).some((row) => row.saved === true)) throw new Error("The cluster did not confirm the layout change.");
    return true;
  }, false);
  return {
    busy: write.busy,
    error: write.error === "" ? "" : refusalSentence(write.error, calling.current),
    lost: write.lost,
    reset: write.reset,
    archive: (layout) => write.call(layout, "archive"),
    restore: (layout) => write.call(layout, "restore"),
  };
}

// ---------------------------------------------------------------------------
// Sender identities
// ---------------------------------------------------------------------------

export interface SenderFacts {
  address: string;
  fromName: string;
  replyTo: string;
  accountId: string;
  notes: string;
}

export interface CreateSenderState extends WriteState {
  /**
   * `senderIdentityId` is the CALLER'S, held for the life of the flow that adds
   * the sender. A write whose answer was lost (a dropped connection, a refusal
   * the person fixes and tries again) is retried under the same id and writes
   * the same sender, rather than a second one the person then has to find and
   * archive.
   */
  create: (senderIdentityId: string, facts: SenderFacts) => Promise<boolean>;
}

export function useCreateSender(): CreateSenderState {
  const { busy, error, reset, call } = useWrite(async (query, senderIdentityId: string, facts: SenderFacts) => {
    await query.createSenderIdentity({
      senderIdentityId,
      address: facts.address.trim(),
      fromName: facts.fromName.trim(),
      replyTo: omitBlank(facts.replyTo),
      accountId: facts.accountId.trim(),
      notes: omitBlank(facts.notes),
    });
    return true;
  }, false);
  return { busy, error, reset, create: call };
}

export interface UpdateSenderState extends WriteState {
  update: (senderIdentityId: string, facts: SenderFacts) => Promise<boolean>;
}

/**
 * Save what a sender's page edits.
 *
 * THE FORM'S CONTENTS ARE THE FACTS, so Reply to and Notes go as typed, empty
 * included. Every update here read-merges -- a field left out inherits its
 * stored value -- and that is the right thing for a form that sends only what
 * changed, and the wrong one for this: a person who clears the reply-to means
 * "replies come to the sender itself", and leaving the field out would keep the
 * old address while the page said it was saved. An explicit empty string is a
 * value, and it blanks the stored one.
 */
export function useUpdateSender(): UpdateSenderState {
  const { busy, error, reset, call } = useWrite(
    async (query, senderIdentityId: string, facts: SenderFacts) => {
      await query.updateSenderIdentity({
        senderIdentityId,
        address: facts.address.trim(),
        fromName: facts.fromName.trim(),
        replyTo: facts.replyTo.trim(),
        accountId: facts.accountId.trim(),
        notes: facts.notes.trim(),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

export interface SenderStatusState extends WriteState {
  set: (senderIdentityId: string, status: string) => Promise<boolean>;
}

/**
 * Retire or re-enable a mailbox.
 *
 * A STATUS FLIP, NEVER A DELETE, and there is deliberately no delete anywhere
 * in this app: past campaigns name the row, and the reputation and warmup
 * history are keyed on its address, so removing it would orphan the evidence a
 * deliverability review is made of. The UI says that rather than leaving
 * somebody hunting for a delete button.
 *
 * A separate call from `updateSenderIdentity`, which does not accept `status`:
 * enabling and disabling an identity is a deliverability decision with a
 * send-time consequence (the preflight REFUSES a campaign naming a disabled
 * identity rather than falling back to the default), and the two never overlap
 * in what they accept.
 */
export function useSenderStatus(): SenderStatusState {
  const { busy, error, reset, call } = useWrite(
    async (query, senderIdentityId: string, status: string) => {
      await query.setSenderIdentityStatus({ senderIdentityId, status });
      return true;
    },
    false,
  );
  return { busy, error, reset, set: call };
}

// ---------------------------------------------------------------------------
// Event-email rules
// ---------------------------------------------------------------------------

export interface RuleFacts {
  name: string;
  description: string;
  triggerConcept: string;
  eventKind: string;
  condition: string;
  templateId: string;
  recipientMode: RecipientMode;
  recipientRoles: string[];
  audienceId: string;
  recipientField: string;
  accountId: string;
  senderIdentityId: string;
}

/**
 * The mode-specific fields, sent only for the mode that uses them.
 *
 * A rule that was an audience rule and became a roles rule keeps its stale
 * `audienceId` otherwise -- the mutation read-merges -- and the generated
 * construct would name an audience nothing sends to. Blanking is not an option
 * either (`omitBlank` turns "" into an omission), so the honest write is to
 * send each field only when the mode makes it meaningful and to let the merge
 * keep the rest. The generator reads the mode first, so a stale sibling field
 * is inert; this keeps it from being CONFUSING as well as inert.
 */
function modeFields(facts: RuleFacts) {
  return {
    recipientRoles: facts.recipientMode === "cluster_roles" ? facts.recipientRoles : undefined,
    audienceId: facts.recipientMode === "audience" ? facts.audienceId : undefined,
    recipientField:
      facts.recipientMode === "row_address" ? omitBlank(facts.recipientField) : undefined,
  };
}

export interface CreateRuleState extends WriteState {
  create: (facts: RuleFacts) => Promise<string>;
}

export function useCreateRule(): CreateRuleState {
  const { busy, error, reset, call } = useWrite(async (query, facts: RuleFacts) => {
    const emailRuleId = newShortId();
    await query.createEmailRule({
      emailRuleId,
      name: facts.name.trim(),
      description: omitBlank(facts.description),
      triggerConcept: facts.triggerConcept,
      eventKind: facts.eventKind,
      condition: omitBlank(facts.condition),
      templateId: facts.templateId,
      recipientMode: facts.recipientMode,
      accountId: facts.accountId.trim(),
      senderIdentityId: omitBlank(facts.senderIdentityId),
      ...modeFields(facts),
    });
    // A rule lands as a DRAFT and nothing runs until somebody turns it on --
    // writing this row can never by itself mail anybody.
    return emailRuleId;
  }, "");
  // The rules surface shows a refusal's sentence, never its transport frames.
  return { busy, error: refusalSentence(error, "createEmailRule"), reset, create: call };
}

export interface UpdateRuleState extends WriteState {
  update: (emailRuleId: string, facts: RuleFacts) => Promise<boolean>;
}

export function useUpdateRule(): UpdateRuleState {
  const { busy, error, reset, call } = useWrite(
    async (query, emailRuleId: string, facts: RuleFacts) => {
      await query.updateEmailRule({
        emailRuleId,
        name: facts.name.trim(),
        description: omitBlank(facts.description),
        triggerConcept: facts.triggerConcept,
        eventKind: facts.eventKind,
        condition: omitBlank(facts.condition),
        templateId: facts.templateId,
        recipientMode: facts.recipientMode,
        accountId: facts.accountId.trim(),
        senderIdentityId: omitBlank(facts.senderIdentityId),
        ...modeFields(facts),
      });
      return true;
    },
    false,
  );
  return { busy, error: refusalSentence(error, "updateEmailRule"), reset, update: call };
}

export interface RuleArmingState extends WriteState {
  /** Generate the rule's construct and arm it. */
  activate: (emailRuleId: string) => Promise<boolean>;
  /** Retire the construct. The rule row survives with its history. */
  retire: (emailRuleId: string) => Promise<boolean>;
  /** Pause or resume without discarding the construct. */
  setStatus: (emailRuleId: string, status: string) => Promise<boolean>;
}

/**
 * Turning a rule on, off and back on.
 *
 * THREE VERBS, THREE MEANINGS, and the app keeps them distinct because the
 * concept does: pausing keeps the generated construct and disarms it,
 * retiring removes the construct and keeps the row, and the circuit breaker
 * tripping is a fourth thing that happens to a rule without anybody asking.
 * All four are visible as distinct statuses so nobody has to guess which one
 * happened -- and the refusal, when activation fails, is the engine's own
 * sentence on `lastError`, which the list renders verbatim.
 *
 * One error slot, for the reason `useSendControls` has one: these three are a
 * single cluster of controls on one row, and only one can be pressed at once.
 * The slot holds the ENGINE'S SENTENCE (the SDK's `refusalSentence` of the
 * refused call, in that call's name), not the call's error text: that is what
 * lets the rule page see that the refusal it is about to show is the one
 * `lastError` already shows -- compared in the same words, so a sentence that
 * opens with the engine's package name is still said once -- and say it once.
 */
export function useRuleArming(): RuleArmingState {
  const activate = useWrite(async (query, emailRuleId: string) => {
    await query.campaignActivateEmailRule({ emailRuleId });
    return true;
  }, false);
  const retire = useWrite(async (query, emailRuleId: string) => {
    await query.campaignRetireEmailRule({ emailRuleId });
    return true;
  }, false);
  const status = useWrite(async (query, emailRuleId: string, next: string) => {
    await query.setEmailRuleStatus({ emailRuleId, status: next });
    return true;
  }, false);

  const parts = [activate, retire, status];
  // The first refused call's sentence, in the name that call was made with.
  const refused = (
    [
      ["campaignActivateEmailRule", activate],
      ["campaignRetireEmailRule", retire],
      ["setEmailRuleStatus", status],
    ] as const
  ).find(([, part]) => part.error !== "");
  return {
    busy: parts.some((p) => p.busy),
    error: refused === undefined ? "" : refusalSentence(refused[1].error, refused[0]),
    reset: () => parts.forEach((p) => p.reset()),
    activate: activate.call,
    retire: retire.call,
    setStatus: status.call,
  };
}

/**
 * The writes the audiences, senders and automations sections make,
 * assembled once at the app root.
 *
 * The sections take what they need. Assembling here rather than per section
 * means a write's busy/error state survives a section switch, which matters
 * for exactly one of them: an import running while somebody looks at their
 * campaigns is still running when they come back. A campaign's own writes are
 * its page's (`CampaignPage`, `CampaignEditPage`): a refusal belongs to the
 * campaign it was about, not to the next one opened.
 */
export function useCampaignWrites() {
  const createAudience = useCreateAudience();
  const archiveAudience = useArchiveAudience();
  const addRecipient = useAddRecipient();
  const setSubscription = useSetSubscription();
  const importRecipients = useImportRecipients();
  const createSender = useCreateSender();
  const updateSender = useUpdateSender();
  const senderStatus = useSenderStatus();
  const createRule = useCreateRule();
  const updateRule = useUpdateRule();
  const ruleArming = useRuleArming();

  // NOT memoised. Every hook above returns a fresh `reset` closure on every
  // render, so the object's identity changes regardless; a useMemo over
  // seventeen deps that can never all be equal is a dependency list somebody
  // has to maintain in exchange for nothing.
  return {
    createAudience,
    archiveAudience,
    addRecipient,
    setSubscription,
    importRecipients,
    createSender,
    updateSender,
    senderStatus,
    createRule,
    updateRule,
    ruleArming,
  };
}

export type CampaignWrites = ReturnType<typeof useCampaignWrites>;


export function useConfigureNewsletter() {
  return useWrite(async (query, args: Parameters<Connection["query"]["campaignConfigureNewsletter"]>[0]) => {
    await query.campaignConfigureNewsletter(args); return true;
  }, false);
}

export function useRetryNewsletterWelcome() {
  return useWrite(async (query, args: Parameters<Connection["query"]["campaignRetryNewsletterWelcome"]>[0]) => {
    await query.campaignRetryNewsletterWelcome(args); return true;
  }, false);
}
