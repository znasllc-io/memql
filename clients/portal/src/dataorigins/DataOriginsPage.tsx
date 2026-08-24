import { useCallback, useMemo, useState, type ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { ErrorMessage } from "../components/StatusMessage";
import { useAdminAccess } from "../admin/useAdminConsole";
import { ModulesRefused } from "../modules/ModulesRefused";
import { Badge, Band, Button, ConfirmDialog, Container, DataText, EmptyState, PageHeader, Skeleton } from "../ui";
import { OriginBadge } from "./OriginBadge";
import {
  healthFor,
  toDeadLetterRows,
  useDataOrigins,
  type DataOriginRow,
  type DeadLetterRow,
  type SyncStateRow,
} from "./useDataOrigins";

// The Data origins page (epic memql#4378).
//
// Two questions, and the page is organised around the fact that they are
// different questions:
//
//   WHAT IS DECLARED -- every concept's data state and the connectors it
//     names. A projection of the live registry; it changes on a deploy.
//   HOW IS IT GOING -- per-(concept, connector) health that accumulates:
//     backfill cursor, lag, drift, queue depth, dead letters.
//
// The inventory is filtered to the concepts that HAVE a connector. All but a
// handful of a cluster's concepts are native, and listing 120 rows saying
// "Native / memql / no connectors" would bury the four that can actually be
// behind.
//
// Cluster owners only, and refused rather than hidden-and-broken: every read
// and every action here is clusterOwner-tier in the engine, so a page shown
// to anybody else would be a list of doors that do not open.

export function DataOriginsPage(): ReactNode {
  const { role, canAdminister, resolved } = useAdminAccess();
  const state = useDataOrigins(canAdminister);

  if (!canAdminister) {
    return <ModulesRefused role={role} resolved={resolved} />;
  }

  const connected = state.origins.filter((row) => row.connectors.length > 0);
  const lookup = healthFor(state.health);

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          title="Data origins"
          blurb="MemQL is the origin of what it owns, a faithful mirror of what it does not, and every concept says which. A mirror is read-only here by construction — change it at the origin."
          actions={
            <Button size="xs" onClick={state.reload} busy={state.loading} busyLabel="Reading…">
              Refresh
            </Button>
          }
          meta={
            <span className="text-xs text-subtle">
              {connected.length} concept{connected.length === 1 ? "" : "s"} with a connector,{" "}
              {state.origins.length} declared in all
            </span>
          }
        />

        {state.error !== "" ? (
          <ErrorMessage>Could not read the data origins: {state.error}</ErrorMessage>
        ) : state.loading && state.origins.length === 0 ? (
          <Skeleton variant="rows" rows={6} />
        ) : connected.length === 0 ? (
          <EmptyState statement="Every concept in this cluster is native: MemQL owns its data and nobody else holds a copy. A concept becomes a mirror or an origin when it declares @origin or @mirroredTo." />
        ) : (
          <Band
            title="Domains"
            panel
            meta={
              <span className="text-xs text-subtle">
                one row per (concept, connector); health is absent until the domain has been worked
              </span>
            }
          >
            <DomainTable rows={connected} lookup={lookup} onChanged={state.reload} />
          </Band>
        )}

        <DeadLetterBand connectors={connectorsIn(connected)} />
      </section>
    </Container>
  );
}

// connectorsIn collects every connector the inventory names, sorted, so the
// dead-letter band reads one queue per connector rather than guessing.
export function connectorsIn(rows: readonly DataOriginRow[]): string[] {
  const seen = new Set<string>();
  for (const row of rows) {
    for (const name of row.connectors) seen.add(name);
  }
  return [...seen].sort();
}

function DomainTable({
  rows,
  lookup,
  onChanged,
}: {
  rows: readonly DataOriginRow[];
  lookup: (conceptId: string, connector: string) => SyncStateRow | null;
  onChanged: () => void;
}): ReactNode {
  const pairs = rows.flatMap((row) =>
    row.connectors.map((connector) => ({ row, connector, health: lookup(row.conceptId, connector) })),
  );
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead className="text-left text-xs text-subtle">
          <tr>
            <th className="py-2 pr-4">Concept</th>
            <th className="py-2 pr-4">State</th>
            <th className="py-2 pr-4">Connector</th>
            <th className="py-2 pr-4">Lag</th>
            <th className="py-2 pr-4">Drift</th>
            <th className="py-2 pr-4">Queue</th>
            <th className="py-2 pr-4">Backfill</th>
            <th className="py-2">Actions</th>
          </tr>
        </thead>
        <tbody>
          {pairs.map(({ row, connector, health }) => (
            <DomainRow
              key={`${row.conceptId}|${connector}`}
              row={row}
              connector={connector}
              health={health}
              onChanged={onChanged}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function DomainRow({
  row,
  connector,
  health,
  onChanged,
}: {
  row: DataOriginRow;
  connector: string;
  health: SyncStateRow | null;
  onChanged: () => void;
}): ReactNode {
  const { query } = useCluster();
  const [busy, setBusy] = useState("");
  const [failure, setFailure] = useState("");

  const run = useCallback(
    async (label: string, call: () => Promise<unknown>) => {
      setBusy(label);
      setFailure("");
      try {
        await call();
        onChanged();
      } catch (err: unknown) {
        setFailure(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy("");
      }
    },
    [onChanged],
  );

  const paused = health?.paused ?? false;
  return (
    <tr className="border-t border-line align-top">
      <td className="py-2 pr-4">
        <DataText kind="id">{row.conceptId}</DataText>
      </td>
      <td className="py-2 pr-4">
        <OriginBadge
          dataState={row.dataState}
          dataOrigin={row.origin}
          dataMirroredTo={row.mirroredTo}
        />
      </td>
      <td className="py-2 pr-4">
        <DataText kind="id">{connector}</DataText>
        {paused ? (
          <>
            {" "}
            <Badge tone="warn">paused</Badge>
          </>
        ) : null}
      </td>
      {/* Health that has never been written renders as an em dash, not as a
          zero: "never run" and "ran and found nothing" are different answers
          and a zero would tell the reader the wrong one. */}
      <td className="py-2 pr-4">{health === null ? "—" : `${health.lagSeconds}s`}</td>
      <td className="py-2 pr-4">{health === null ? "—" : String(health.driftCount)}</td>
      <td className="py-2 pr-4">
        {health === null ? (
          "—"
        ) : (
          <>
            {health.outboxDepth}
            {health.deadLetterCount > 0 ? (
              <>
                {" "}
                <Badge tone="danger">{health.deadLetterCount} dead</Badge>
              </>
            ) : null}
          </>
        )}
      </td>
      <td className="py-2 pr-4">{health === null ? "—" : health.backfillStatus || "none"}</td>
      <td className="py-2">
        <div className="flex flex-wrap gap-2">
          <Button
            size="xs"
            busy={busy === "backfill"}
            busyLabel="Backfilling…"
            onClick={() =>
              void run("backfill", () =>
                query!.datasyncStartBackfill({ connector, conceptId: row.conceptId }),
              )
            }
          >
            Backfill now
          </Button>
          <Button
            size="xs"
            busy={busy === "pause"}
            busyLabel="Saving…"
            onClick={() =>
              void run("pause", () =>
                query!.datasyncSetSyncPaused({
                  connector,
                  conceptId: row.conceptId,
                  paused: !paused,
                }),
              )
            }
          >
            {paused ? "Resume" : "Pause"}
          </Button>
        </div>
        {failure !== "" ? <p className="mt-1 text-xs text-danger">{failure}</p> : null}
        {health?.lastError ? (
          <p className="mt-1 text-xs text-muted">last error: {health.lastError}</p>
        ) : null}
      </td>
    </tr>
  );
}

// The dead-letter queue.
//
// Read per connector, because that is how the engine indexes it and because
// an operator acts on one integration at a time. Both verbs sit behind a
// confirmation: retry re-enters a delivery that has already failed as many
// times as the ceiling allows, and discard is the decision that a change
// will NEVER reach the other system. Neither is a click to make casually.
function DeadLetterBand({ connectors }: { connectors: readonly string[] }): ReactNode {
  const { query, status } = useCluster();
  const [entries, setEntries] = useState<DeadLetterRow[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<{ verb: "retry" | "discard"; entry: DeadLetterRow } | null>(
    null,
  );

  const load = useCallback(async () => {
    // Nothing to ask, so DO NOT report an answer. The queue is read per
    // connector, and the connector list comes from the inventory above --
    // which arrives asynchronously. Loading with an empty list would resolve
    // instantly and render "Nothing is dead-lettered", which is a SILENT
    // WRONG ANSWER: the operator sees a clean queue because the page had
    // nobody to ask, not because nothing is stuck. The button is disabled
    // until the inventory lands, and says why.
    if (connectors.length === 0) return;
    if (!query || status !== "connected") return;
    setError("");
    try {
      const results = await Promise.all(
        connectors.map((target) => query.outboxDeadLetters({ target })),
      );
      const rows = results.flatMap((result) => {
        const bag = result as { rows?: () => unknown };
        const raw = typeof bag.rows === "function" ? bag.rows() : [];
        return Array.isArray(raw) ? (raw as Record<string, unknown>[]) : [];
      });
      setEntries(toDeadLetterRows(rows));
      setLoaded(true);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [query, status, connectors]);


  const confirm = useCallback(async () => {
    if (!pending || !query) return;
    const { verb, entry } = pending;
    setPending(null);
    try {
      if (verb === "retry") {
        await query.datasyncRetryOutboxEntry({ entryId: entry.id });
      } else {
        await query.datasyncDiscardOutboxEntry({
          entryId: entry.id,
          reason: "discarded from the Data origins page",
        });
      }
      await load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [pending, query, load]);

  const title = useMemo(
    () => (loaded ? `Dead letters (${entries.length})` : "Dead letters"),
    [loaded, entries.length],
  );

  return (
    <Band
      title={title}
      panel
      meta={
        <span className="flex items-baseline gap-3 text-xs text-subtle">
          <span>
            deliveries that exhausted their attempts; nothing retries these automatically, which is
            what makes them dead rather than failed
          </span>
          <Button
            size="xs"
            disabled={connectors.length === 0}
            title={
              connectors.length === 0
                ? "No connector to ask yet — the inventory is still loading"
                : undefined
            }
            onClick={() => void load()}
          >
            {loaded ? "Refresh" : "Load"}
          </Button>
        </span>
      }
    >
      {error !== "" ? <ErrorMessage>Could not read the queue: {error}</ErrorMessage> : null}
      {!loaded ? (
        <p className="text-sm text-muted">Not loaded. The queue is empty on a healthy cluster.</p>
      ) : entries.length === 0 ? (
        <EmptyState statement="Nothing is dead-lettered. Every change that had to go out has gone out." />
      ) : (
        <ul className="flex flex-col gap-3">
          {entries.map((entry) => (
            <li key={entry.id} className="flex flex-wrap items-baseline gap-2 border-t border-line pt-3">
              <DataText kind="id">{entry.conceptId}</DataText>
              <DataText kind="id">{entry.rowRef}</DataText>
              <Badge tone="neutral">{entry.action}</Badge>
              <Badge tone="neutral">→ {entry.target}</Badge>
              <span className="text-xs text-subtle">{entry.attempts} attempts</span>
              <span className="basis-full text-xs text-muted">{entry.lastError}</span>
              <Button size="xs" onClick={() => setPending({ verb: "retry", entry })}>
                Retry
              </Button>
              <Button size="xs" tone="danger" onClick={() => setPending({ verb: "discard", entry })}>
                Discard
              </Button>
            </li>
          ))}
        </ul>
      )}

      <ConfirmDialog
        open={pending !== null}
        title={pending?.verb === "discard" ? "Discard this change?" : "Retry this delivery?"}
        confirmLabel={pending?.verb === "discard" ? "Discard" : "Retry"}
        tone={pending?.verb === "discard" ? "danger" : "primary"}
        onConfirm={() => void confirm()}
        onCancel={() => setPending(null)}
      >
        {pending?.verb === "discard"
          ? "The change will never reach the other system. The entry stays as audit history carrying the reason."
          : "The entry returns to the queue with its attempts reset. Whatever was failing should be fixed first, or it will dead-letter again."}
      </ConfirmDialog>
    </Band>
  );
}
