import { useCallback, useEffect, useMemo, useState } from "react";
import { getRowByConceptAndId, type Concept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../chrome/access";
import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import {
  AUDIENCE_CONCEPT,
  CAMPAIGN_CONCEPT,
  EMAIL_RULE_CONCEPT,
  SENDER_IDENTITY_CONCEPT,
  TEMPLATE_CONCEPT,
  authoredAutomationsFrom,
  statsFromPayload,
  type AuthoredAutomationsState,
  type CampaignStats,
} from "./rows";

// Operator records use graph subscriptions. High-volume delivery, engagement
// and recipient rows stay out of mesh broadcasts; visible views re-read their
// bounded queries automatically. Requests never overlap and pause in hidden tabs.
export const CAMPAIGN_REFRESH_MS = 5_000;

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

export function useCampaignFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("campaigns:campaigns", (connection) => ({
    concept: CAMPAIGN_CONCEPT,
    inScope: row => !flatten(row).testSourceCampaignId,
    // NO STATUS ARGUMENT. `campaigns` takes an optional one and the filed
    // toggle deliberately does not pass it: seeding filtered would make the
    // toggle re-run the read and re-baseline every arrival cue, so revealing
    // rows the browser already had would announce them as new.
    seed: async (_cursor, signal) => {
      const result = await connection.query.campaigns({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CAMPAIGN_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

export function useAudienceFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("campaigns:audiences", (connection) => ({
    concept: AUDIENCE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.audiences({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, AUDIENCE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

export function useTemplateFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("campaigns:templates", (connection) => ({
    concept: TEMPLATE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.templates({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, TEMPLATE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

export function useSenderFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("campaigns:senders", (connection) => ({
    concept: SENDER_IDENTITY_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.senderIdentities({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(
        connection.query,
        SENDER_IDENTITY_CONCEPT,
        rowId,
        { signal },
      );
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

export function useRuleFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("campaigns:rules", (connection) => ({
    concept: EMAIL_RULE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.emailRules({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, EMAIL_RULE_CONCEPT, rowId, {
        signal,
      });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

export interface CampaignFeeds {
  campaigns: LiveCollectionHandle<Row>;
  audiences: LiveCollectionHandle<Row>;
  templates: LiveCollectionHandle<Row>;
  senders: LiveCollectionHandle<Row>;
  rules: LiveCollectionHandle<Row>;
}

/** The app root's five, retained together so every section is a reading of
 *  the same snapshots. */
export function useCampaignFeeds(): CampaignFeeds {
  return {
    campaigns: useCampaignFeed(),
    audiences: useAudienceFeed(),
    templates: useTemplateFeed(),
    senders: useSenderFeed(),
    rules: useRuleFeed(),
  };
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
  reload: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Reads immediately, optionally refreshing while visible. Keep the last good
 * answer during background reads, abort stale requests, and retry failures. */
export function useReading<T>(
  empty: T,
  read: ((signal: AbortSignal) => Promise<T>) | null,
  deps: readonly unknown[],
  refreshEveryMs = 0,
): Reading<T> {
  const [snapshot, setSnapshot] = useState({ reader: read, value: empty,
    state: "idle" as Reading<T>["state"], error: "", readAt: "" });
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce(n => n + 1), []);

  useEffect(() => {
    if (read === null) return;
    let disposed = false;
    let inFlight = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | undefined;
    setSnapshot(previous => previous.reader === read && previous.readAt ? previous :
      { reader: read, value: empty, state: "loading", error: "", readAt: "" });
    const visible = () => document.visibilityState !== "hidden";
    const run = async () => {
      if (disposed || inFlight || (refreshEveryMs > 0 && !visible())) return;
      clearTimeout(timer);
      inFlight = true;
      controller = new AbortController();
      try {
        const value = await read(controller.signal);
        if (!disposed) setSnapshot({ reader: read, value, state: "ready", error: "", readAt: new Date().toISOString() });
      } catch (err) {
        if (!disposed) setSnapshot(previous => ({ ...previous, state: "error", error: describe(err) }));
      } finally {
        inFlight = false;
        if (!disposed && refreshEveryMs > 0 && visible()) timer = setTimeout(() => void run(), refreshEveryMs);
      }
    };
    const wake = () => { clearTimeout(timer); if (visible()) void run(); };
    if (refreshEveryMs > 0) {
      document.addEventListener("visibilitychange", wake);
      window.addEventListener("focus", wake);
      window.addEventListener("online", wake);
    }
    void run();
    return () => {
      disposed = true;
      clearTimeout(timer);
      controller?.abort();
      document.removeEventListener("visibilitychange", wake);
      window.removeEventListener("focus", wake);
      window.removeEventListener("online", wake);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce, refreshEveryMs]);

  if (read === null || snapshot.reader !== read) return { value: empty, state: read ? "loading" : "idle", error: "", readAt: "", reload };
  return { ...snapshot, reload };
}

const NO_ROWS: Row[] = [];

/**
 * The per-recipient ledger for one campaign.
 *
 * Automatically re-read while visible. `v1:campaigns:delivery` stays out of
 * broadcasts because it produces one row per recipient per send.
 */
export function useCampaignDeliveries(campaignId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null || campaignId === "") return null;
    return async (signal: AbortSignal) => {
      const result = await query.deliveriesForCampaign({ campaignId }, { signal });
      return result.rows();
    };
  }, [query, campaignId]);
  return useReading<Row[]>(NO_ROWS, read, [query, campaignId], CAMPAIGN_REFRESH_MS);
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
  return useReading<Row[]>(NO_ROWS, read, [query, audienceId], CAMPAIGN_REFRESH_MS);
}

/**
 * The server-computed outcome breakdown for one campaign.
 *
 * COMPUTED SERVER-SIDE, and that is the point of the builtin: the portal
 * counted a capped page of delivery rows in the browser, which under-reported
 * every campaign past the page bound and did so silently. Every bucket that
 * can be an exact COUNT is one here, at any audience size.
 *
 * It refreshes automatically while visible, including after a send finishes
 * because opens, clicks and unsubscribes can arrive later. The main bar also
 * receives the campaign row's progress through its existing subscription.
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
  return useReading<CampaignStats | null>(null, read, [query, campaignId], CAMPAIGN_REFRESH_MS);
}

/** Organization-specific setup, read on demand and checked again by every send.
 * A cluster operator's provider verdict cannot stand in for a client's domain. */
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
    reason: reading.error || reading.value?.reason || "Sending readiness is not confirmed." };
}

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
 * campaigns app's to own. Nothing in this module imports from `apps/accounts`,
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


/** Signup receipts are shopper-volume data; refresh the bounded visible view. */
export function useNewsletterWelcomes(audienceId: string): Reading<Row[]> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => query === null ? null : async (signal: AbortSignal) =>
    (await query.newsletterWelcomesForAudience({ audienceId }, { signal })).rows(), [query, audienceId]);
  return useReading<Row[]>(NO_ROWS, read, [query, audienceId], CAMPAIGN_REFRESH_MS);
}
