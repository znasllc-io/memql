import { useCallback, useMemo, useState } from "react";

import { useReading, type Reading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import {
  openedPreviewFromRow,
  previewGrantFromRow,
  previewObservationFromRow,
  readinessFromRow,
  type OpenedPreview,
  type PreviewGrantRow,
  type PreviewObservationRow,
  type PreviewReadiness,
} from "./rows";

// The reads and the writes a storefront deployable makes about its preview.
//
// ===========================================================================
// NOTHING HERE IS A LIVE FEED, AND SAYING SO IS THE HONEST ANSWER
// ===========================================================================
// The same reasoning `useStore` records, and it is sharper here. Readiness is
// COMPUTED -- it reads the site and then a cluster-owner-tier store row and
// applies the guard's own rules -- so no row subscription could produce it. The
// grant and observation concepts carry no broadcast routing rule, so a live
// collection over either would render "Loading from the cluster" and then a
// panel that never moves.
//
// So each read happens once, says when it looked, and offers to look again; and
// every write re-reads, because an accepted write that did not would leave a
// promoted candidate still drawn as a candidate -- which looks exactly like a
// write the engine ignored.

export interface ReadinessReading extends Reading<PreviewReadiness | null> {
  readiness: PreviewReadiness | null;
}

/**
 * What is legal for this deployable right now.
 *
 * THE ENGINE ANSWERS, AND THE SHELL DRAWS. Every boolean and every refusal
 * comes from `sitePreviewReadiness`, which runs the same functions the write
 * guard refuses with -- so a control this surface offers is one the engine will
 * accept, and a control it withholds is one the engine would refuse.
 */
export function usePreviewReadiness(siteId: string): ReadinessReading {
  const connection = useOsConnection();
  const id = siteId.trim();
  const read = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<PreviewReadiness | null> => {
      const result = await connection.query.sitePreviewReadiness({ siteId: id }, { signal });
      const first = result.rows()[0];
      return first === undefined ? null : readinessFromRow(first);
    };
  }, [connection, id]);
  const reading = useReading<PreviewReadiness | null>(
    connection === null || id === "" ? "no-site" : `preview-readiness:${id}`,
    read,
  );
  return { ...reading, readiness: reading.value ?? null };
}

export interface ObservationsReading extends Reading<PreviewObservationRow[]> {
  observations: PreviewObservationRow[];
}

/** Everything the engine has observed of any preview of this deployable, newest first. */
export function usePreviewObservations(siteId: string): ObservationsReading {
  const connection = useOsConnection();
  const id = siteId.trim();
  const read = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<PreviewObservationRow[]> => {
      const result = await connection.query.sitePreviewObservationsForSite({ siteId: id }, { signal });
      return result.rows().map(previewObservationFromRow);
    };
  }, [connection, id]);
  const reading = useReading<PreviewObservationRow[]>(
    connection === null || id === "" ? "no-site" : `preview-observations:${id}`,
    read,
  );
  return { ...reading, observations: reading.value ?? [] };
}

export interface GrantsReading extends Reading<PreviewGrantRow[]> {
  grants: PreviewGrantRow[];
}

/**
 * Every preview ever opened against this deployable, newest first.
 *
 * SPENT ONES INCLUDED, and that is the read's point rather than an oversight.
 * The question this list answers after something unexpected turns up in a
 * development store is "who opened a preview of this, and when" -- and a list
 * filtered to the live ones answers it for the last half hour only.
 */
export function usePreviewGrants(siteId: string): GrantsReading {
  const connection = useOsConnection();
  const id = siteId.trim();
  const read = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<PreviewGrantRow[]> => {
      const result = await connection.query.sitePreviewGrantsForSite({ siteId: id }, { signal });
      return result.rows().map(previewGrantFromRow);
    };
  }, [connection, id]);
  const reading = useReading<PreviewGrantRow[]>(
    connection === null || id === "" ? "no-site" : `preview-grants:${id}`,
    read,
  );
  return { ...reading, grants: reading.value ?? [] };
}

export interface PreviewWrites {
  /** The act in flight, or "". One at a time: a busy flag, never a disabled control. */
  busy: string;
  /** The last refusal, in the server's own words. */
  error: string;
  /** What the last accepted write did, in this surface's voice. */
  note: string;
  clearNote: () => void;
  /**
   * The link the last `open` returned, held in memory only.
   *
   * IT IS READABLE ONCE. The engine stores only the token's digest, so nothing
   * -- not this app, not a row read, not a backup -- can produce it again. The
   * panel offers it and then drops it on the next act.
   */
  opened: OpenedPreview | null;
  clearOpened: () => void;
  openPreview: (siteId: string) => Promise<OpenedPreview | null>;
  endPreview: (grantId: string) => Promise<boolean>;
  runChecks: (grantId: string) => Promise<boolean>;
  setCandidate: (siteId: string, candidateRef: string) => Promise<boolean>;
  clearCandidate: (siteId: string) => Promise<boolean>;
  promote: (siteId: string, candidateRef: string) => Promise<boolean>;
  bindPreviewStore: (siteId: string, storeId: string) => Promise<boolean>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The writes a storefront's Preview section owns. */
export function usePreviewWrites(onWritten: () => void): PreviewWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [opened, setOpened] = useState<OpenedPreview | null>(null);

  const run = useCallback(
    async <T,>(label: string, work: () => Promise<T>, done: string): Promise<T | null> => {
      if (connection === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return null;
      }
      setBusy(label);
      setError("");
      setNote("");
      try {
        const value = await work();
        setNote(done);
        return value;
      } catch (err: unknown) {
        setError(describe(err));
        return null;
      } finally {
        setBusy("");
        onWritten();
      }
    },
    [connection, onWritten],
  );

  const openPreview = useCallback(
    async (siteId: string): Promise<OpenedPreview | null> => {
      // THE PREVIOUS LINK GOES FIRST. Two links on one screen is two
      // credentials a person has to tell apart, and the older one is the one
      // they would copy by accident.
      setOpened(null);
      const value = await run(
        "open",
        async () => {
          const result = await connection!.query.sitePreviewOpen({ siteId });
          const first = result.rows()[0];
          return first === undefined ? null : openedPreviewFromRow(first);
        },
        "The preview is open. The link works once you follow it, and only for you.",
      );
      if (value !== null && value !== undefined) setOpened(value);
      return value ?? null;
    },
    [connection, run],
  );

  const endPreview = useCallback(
    async (grantId: string): Promise<boolean> => {
      const at = new Date().toISOString();
      const done = await run(
        "end",
        () => connection!.query.revokeSitePreviewGrant({ grantId, revokedAt: at }),
        "That preview is ended. Its link no longer works for anybody.",
      );
      if (done !== null) setOpened(null);
      return done !== null;
    },
    [connection, run],
  );

  const runChecks = useCallback(
    async (grantId: string): Promise<boolean> =>
      (await run(
        "checks",
        () => connection!.query.sitePreviewProbe({ grantId }),
        "The checks ran against the development store.",
      )) !== null,
    [connection, run],
  );

  const setCandidate = useCallback(
    async (siteId: string, candidateRef: string): Promise<boolean> =>
      (await run(
        "candidate",
        () => connection!.query.setSiteCandidate({ siteId, candidateRef: candidateRef.trim() }),
        "That version is the candidate. Nothing the public sees has changed.",
      )) !== null,
    [connection, run],
  );

  const clearCandidate = useCallback(
    async (siteId: string): Promise<boolean> =>
      (await run(
        "candidate",
        () => connection!.query.clearSiteCandidate({ siteId }),
        "The candidate is withdrawn, and every preview of it has ended.",
      )) !== null,
    [connection, run],
  );

  const promote = useCallback(
    async (siteId: string, candidateRef: string): Promise<boolean> =>
      (await run(
        "promote",
        () => connection!.query.promoteSiteCandidate({ siteId, candidateRef: candidateRef.trim() }),
        "That version is serving now. The one it replaced is still in place to go back to.",
      )) !== null,
    [connection, run],
  );

  const bindPreviewStore = useCallback(
    async (siteId: string, storeId: string): Promise<boolean> =>
      (await run(
        "bind",
        () => connection!.query.updateSitePreviewBinding({ siteId, storeId: storeId.trim() }),
        storeId.trim() === ""
          ? "No development store is attached. There is nothing to exercise a candidate against."
          : "Candidates are exercised against that development store.",
      )) !== null,
    [connection, run],
  );

  return {
    busy,
    error,
    note,
    clearNote: useCallback(() => setNote(""), []),
    opened,
    clearOpened: useCallback(() => setOpened(null), []),
    openPreview,
    endPreview,
    runChecks,
    setCandidate,
    clearCandidate,
    promote,
    bindPreviewStore,
  };
}
