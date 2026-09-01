import { useCallback, useEffect, useMemo, useState } from "react";
import { getRowByConceptAndId, type Concept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import {
  AUDIENCE_CONCEPT,
  CAMPAIGN_CONCEPT,
  EMAIL_RULE_CONCEPT,
  EMAIL_UNKNOWN,
  SENDER_IDENTITY_CONCEPT,
  TEMPLATE_CONCEPT,
  emailReadinessFrom,
  statsFromPayload,
  type CampaignStats,
  type EmailReadiness,
} from "./rows";

// The Campaigns app's reads: five live feeds and four on-demand ones.
//
// ===========================================================================
// WHAT IS LIVE, WHAT IS NOT, AND WHY THE LINE IS WHERE IT IS
// ===========================================================================
// The README's rule is to read `component/node/routing.go` before deciding
// what a feed does, and this domain is the first where the answer is a
// deliberate SPLIT rather than a fact about the tree.
//
// LIVE -- `campaign`, `audience`, `template`, `senderIdentity`, `emailRule`
// carry broadcast rules. They are low-volume operator rows: one per thing a
// person authored. A campaign moving from `scheduled` to `sending` while
// somebody watches is the app's headline, and a rule the circuit breaker just
// tripped is the news an operator most needs unasked.
//
// NOT LIVE -- three concepts are RECORDED EXCLUSIONS rather than omissions
// (`RoutingExclusions()` in the same file carries each one's reason), and the
// ground is volume in every case:
//
//   delivery         one row per recipient per send.
//   engagementEvent  one row per open and per click -- delivery's volume and
//                    then some, because mail clients prefetch the pixel.
//   recipient        the surprising one. Hand-editing an audience is
//                    human-paced and would be affordable; a CSV IMPORT is the
//                    same concept and is not, because a 20k-address file is a
//                    20k-event burst proportional to a FILE rather than to
//                    anything a person did.
//
// That is the same ground `v1:worker:invocation` is excluded on.
//
// The campaign row is the interesting near-miss and the reason the fingerprint
// rule matters here: `updateCampaignProgress` fires once per drain tick per
// RUNNING job -- single-digit events per minute, not one per message -- so the
// row is affordable to broadcast AND moves constantly while a send runs. It
// must re-render live without ringing, which is exactly what
// `campaignFingerprint` is for.
//
// So every surface over those three is an ON-DEMAND read that prints WHEN it
// was read and offers to look again. A `LiveList` over them would render
// "Loading from the cluster" and then a list that silently never moved --
// worse than a plain one, because the caption would be claiming wiring that is
// not there.

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
  /** When this browser last looked. Printed, because these do not move. */
  readAt: string;
  reload: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * The one on-demand reader every non-live surface here uses.
 *
 * ONE IMPLEMENTATION rather than four copies of the same effect, because the
 * part that is easy to get wrong is identical every time: abort on unmount,
 * ignore a resolved promise whose request was cancelled, and stamp `readAt`
 * on BOTH outcomes. A read that failed is still a read that happened, and a
 * surface that only timestamps success tells somebody their refusal is
 * current when it is an hour old.
 */
function useReading<T>(
  empty: T,
  read: ((signal: AbortSignal) => Promise<T>) | null,
  deps: readonly unknown[],
): Reading<T> {
  const [value, setValue] = useState<T>(empty);
  const [state, setState] = useState<Reading<T>["state"]>("idle");
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState("");
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (read === null) return;
    const controller = new AbortController();
    setState("loading");
    setError("");
    void (async () => {
      try {
        const next = await read(controller.signal);
        if (controller.signal.aborted) return;
        setValue(next);
        setState("ready");
        setReadAt(new Date().toISOString());
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setValue(empty);
        setError(describe(err));
        setState("error");
        setReadAt(new Date().toISOString());
      }
    })();
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  return { value, state, error, readAt, reload };
}

const NO_ROWS: Row[] = [];

/**
 * The per-recipient ledger for one campaign.
 *
 * ON DEMAND, because `v1:campaigns:delivery` carries no broadcast rule and
 * should not: one row per recipient per send. The panel says when it looked
 * and offers to look again, which is the Files version-history shape.
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
  return useReading<Row[]>(NO_ROWS, read, [query, campaignId]);
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
 * It is on demand for the same reason the ledger is -- it reads the ledger --
 * and it re-reads when the caller asks. During a live send the BAR is fed by
 * the campaign row's own counters, which do arrive live; the stats are the
 * finer breakdown underneath it.
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

/**
 * Whether this cluster can actually send mail.
 *
 * READ ONCE, AT THE APP ROOT, and said once at the top. The alternative -- a
 * failure per action -- tells somebody five times that the same thing is
 * missing, and tells them only after they have written a campaign.
 *
 * `probe: false`. The configuration question is answerable with no network
 * call; the health question needs a round trip whose "yes" goes stale, and
 * running one on every window open would dial a mail provider because
 * somebody opened an app.
 */
export function useEmailReadiness(): Reading<EmailReadiness> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (query === null) return null;
    return async (signal: AbortSignal) => {
      const result = await query.integrationStatus({ probe: false }, { signal });
      const first = result.rows()[0];
      return first ? emailReadinessFrom(first) : EMAIL_UNKNOWN;
    };
  }, [query]);
  return useReading<EmailReadiness>(EMAIL_UNKNOWN, read, [query]);
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
