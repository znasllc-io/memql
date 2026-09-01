import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { domainMetaFromRow, rollupDomains, type DomainMeta, type DomainRollup } from "./rows";

// What MemQL knows, by domain -- read on demand, and honest about it.
//
// ===========================================================================
// THIS FEED IS NOT LIVE, AND SAYS SO RATHER THAN PRETENDING
// ===========================================================================
// `component/node/routing.go` carries broadcast rules for `v1:cluster:*`,
// `v1:planner:*`, `v1:worker:*`, `v1:workbench:workspace` and
// `v1:platform:site`. It carries NONE for `v1:knowledge:*`. So a chunk written
// on an agent node does not reach this browser, and a `LiveList` here would
// render "Loading from the cluster" and then a list that silently never moves
// -- which is worse than a plain one, because the caption would be claiming
// liveness the wiring does not provide.
//
// So this is a query that says WHEN IT WAS READ and re-runs on window focus.
// That is the same shape the README records for `v1:worker:invocation`: the
// one Fleet population deliberately left off the bus, rendered as an
// on-demand read rather than as a list that looks live.
//
// ADDING A ROUTING RULE IS NOT THIS APP'S CALL. Chunk writes are high-volume
// (a seeded domain is hundreds of rows in one pass), and `v1:worker:invocation`
// is excluded from the bus on exactly that ground.
//
// ===========================================================================
// ONE READ, TWO SURFACES
// ===========================================================================
// `allDocumentChunkDomains` is `@unbounded` -- "a bounded dedupe enumeration
// consumed whole" -- so it returns one row per chunk and this hook folds it.
// It is retained at the app root and passed down, not called twice: the
// Domains cards and the Review queue's domain list are two readings of one
// answer, and two reads would be free to disagree about what the cluster
// currently holds.

export interface DomainsFeed {
  rollups: DomainRollup[];
  /**
   * The domain rows themselves, keyed by id -- the catalog read this page has
   * never had (epic memql#4800).
   *
   * Until `v1:knowledge:knowledgeDomain` was declared, the domain's own
   * payload had no client read surface at all, which is why every card is
   * labelled by its raw `domainId` and says so. `knowledgeDomainsAll` is the
   * read that ends that; it is folded in beside the chunk rollups rather than
   * replacing them, because the ROLLUPS are still what counts the chunks and
   * the two answer different questions.
   *
   * EMPTY IS A REAL ANSWER, not a failure. The catalog seeder's
   * `createKnowledgeDomain` is declared in no .memql file in this tree (see
   * the concept's own header), so an engine-only cluster genuinely has no
   * domain rows even where it has chunks. A card with no matching row keeps
   * rendering its id, exactly as it did before.
   */
  domains: Map<string, DomainMeta>;
  state: "loading" | "ready" | "error";
  /** The server's own sentence when the read failed. "" otherwise. */
  error: string;
  /** When this answer was read, for the caption. Null before the first one. */
  readAt: Date | null;
  reload: () => void;
}

export function useDomains(): DomainsFeed {
  const connection = useOsConnection();
  const [rollups, setRollups] = useState<DomainRollup[]>([]);
  const [domains, setDomains] = useState<Map<string, DomainMeta>>(() => new Map());
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState<Date | null>(null);
  const [attempt, setAttempt] = useState(0);

  const reload = useCallback(() => setAttempt((n) => n + 1), []);

  useEffect(() => {
    if (connection === null) {
      setState("loading");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setState((held) => (held === "ready" ? held : "loading"));
    void (async () => {
      try {
        const result = await connection.query.allDocumentChunkDomains({}, {
          signal: controller.signal,
        });
        if (!live) return;
        setRollups(rollupDomains(result.rows() as Row[]));
        setError("");
        setState("ready");
        setReadAt(new Date());

        // THE CATALOG READ IS BEST-EFFORT AND SETTLES SEPARATELY. It is a
        // different question from "what chunks exist", and a cluster can
        // honestly answer one and not the other -- so a catalog that comes
        // back empty, or refuses, must not take the chunk rollups down with
        // it. The cards fall back to the id, which is what they did before
        // this read existed.
        try {
          const catalog = await connection.query.knowledgeDomainsAll({}, {
            signal: controller.signal,
          });
          if (!live) return;
          const next = new Map<string, DomainMeta>();
          for (const row of catalog.rows() as Row[]) {
            const meta = domainMetaFromRow(row);
            if (meta.id !== "") next.set(meta.id, meta);
          }
          setDomains(next);
        } catch {
          if (!live) return;
          setDomains(new Map());
        }
      } catch (err: unknown) {
        if (!live) return;
        setError(err instanceof Error ? err.message : String(err));
        setState("error");
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, attempt]);

  // Re-read when somebody comes back to the window. A person who uploaded a
  // file, went away while it was analyzed and came back is the case this
  // exists for -- and it is a re-read rather than a subscription because there
  // is no subscription to have (see the header).
  //
  // BOTH EVENTS, because they answer different questions: `focus` fires when
  // this window is picked out of several, `visibilitychange` when the tab
  // comes back from a background it was never focused out of.
  useEffect(() => {
    const onWake = () => {
      if (document.visibilityState === "hidden") return;
      reload();
    };
    window.addEventListener("focus", onWake);
    document.addEventListener("visibilitychange", onWake);
    return () => {
      window.removeEventListener("focus", onWake);
      document.removeEventListener("visibilitychange", onWake);
    };
  }, [reload]);

  return { rollups, domains, state, error, readAt, reload };
}
