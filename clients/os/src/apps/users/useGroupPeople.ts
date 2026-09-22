import { useEffect, useState } from "react";
import { useOsConnection } from "../../live/connection";
import { personFromRow, type PersonRow } from "./rows";

/** Scoped membership managers can select existing people of this organization
 * without querying the cluster's identity directory. Reads are bounded by the
 * server and repeated when the group's membership changes. */
export function useGroupPeople(groupId: string, revision: number) {
  const connection = useOsConnection();
  const [people, setPeople] = useState<PersonRow[]>([]);
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    setPeople([]);
    setError("");
    if (!connection || !groupId) return;
    const controller = new AbortController();
    void connection.query.groupPeople({ groupId }, { signal: controller.signal }).then((result) => {
      if (!controller.signal.aborted) setPeople(result.rows().map(personFromRow));
    }).catch((cause: unknown) => {
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : String(cause));
    });
    return () => controller.abort();
  }, [connection, groupId, revision, nonce]);
  return { people, error, reload: () => setNonce((previous) => previous + 1) };
}
