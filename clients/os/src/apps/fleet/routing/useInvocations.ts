import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { useSessionIfPresent } from "../../../chrome/access";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { flatten } from "../../../kit/rows";
import { invocationFromRow } from "../rows";

export function useInvocations(workerId: string, enabled: boolean) {
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const feed = useLiveCollection<Row>(enabled && workerId && userId ? `fleet:invocations:${userId}:${workerId}` : null, connection => ({
    concept: "v1:worker:invocation",
    seed: async (_cursor, signal) => ({ rows: (await connection.query.invocationsForWorker({ workerId }, { signal })).rows(), nextCursor: "" }),
    reread: async (invocationId, signal) => (await connection.query.workerInvocationById({ invocationId }, { signal })).rows()[0] ?? null,
    inScope: raw => { const row = flatten(raw); return row["workerId"] === workerId && row["ownerUserId"] === userId && row["deleted"] !== true; },
    paged: false,
  }));
  const invocations = useMemo(() => feed.snapshot.rows.map(invocationFromRow).sort((a, b) => b.startedAt.localeCompare(a.startedAt)), [feed.snapshot.rows]);
  const readAt = useMemo(() => feed.snapshot.state === "live" ? new Date() : null, [feed.snapshot.version, feed.snapshot.state]);
  return { invocations, loading: feed.snapshot.state === "seeding", error: feed.snapshot.error, readAt, refresh: feed.reseed, state: feed.snapshot.state };
}
