import { useCallback, useEffect, useState } from "react";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";

// Where an upload goes.
//
// ===========================================================================
// THE DAILY SPACE, RESOLVED RATHER THAN CHOSEN
// ===========================================================================
// `POST /spaces/{partitionId}/attachments` authorizes SPACE MEMBERSHIP before
// it parses a byte, so this app has to name a space the caller owns. It does
// not offer a picker: the dailyspace pack provisions today's space for every
// user on create and on login, so by the time anyone can open this window the
// value exists -- and asking somebody to choose a space before they can drop a
// file would be a question with one right answer.
//
// `userActiveSpace` is `@public` and projects `id` + `activePartitionId` and
// nothing else (`userActiveSpaceProjection`). It takes the caller's OWN user
// id, which this app passes from the resolved session.
//
// AN EMPTY RESULT IS A STATE, NOT A CRASH. A cluster whose dailyspace
// automations have not run -- or a session whose access has not resolved --
// renders an in-surface "no active space yet" and keeps the dropzone
// disabled, because an upload with no space is a 400 nobody could act on.

export type ActiveSpaceState = "resolving" | "ready" | "absent" | "error";

export interface ActiveSpace {
  spaceId: string;
  state: ActiveSpaceState;
  /** The server's own sentence when the read failed. "" otherwise. */
  error: string;
  reload: () => void;
}

export function useActiveSpace(viewerUserId: string): ActiveSpace {
  const connection = useOsConnection();
  const [spaceId, setSpaceId] = useState("");
  const [state, setState] = useState<ActiveSpaceState>("resolving");
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);

  const reload = useCallback(() => setAttempt((n) => n + 1), []);

  useEffect(() => {
    if (connection === null || viewerUserId.trim() === "") {
      // Not an error: the connection dials and access resolves after the first
      // render, and reporting "no space" while we do not yet know who is
      // asking would be a wrong answer rendered confidently.
      setState("resolving");
      setSpaceId("");
      setError("");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setState("resolving");
    setError("");
    void (async () => {
      try {
        const result = await connection.query.userActiveSpace(
          { userId: viewerUserId },
          { signal: controller.signal },
        );
        if (!live) return;
        const row = result.rows()[0] ?? null;
        const resolved = rowString(row, "activePartitionId").trim();
        setSpaceId(resolved);
        setState(resolved === "" ? "absent" : "ready");
      } catch (err: unknown) {
        if (!live) return;
        setSpaceId("");
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, viewerUserId, attempt]);

  return { spaceId, state, error, reload };
}
