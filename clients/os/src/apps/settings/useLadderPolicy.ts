import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { ladderPolicyFromRow, type LadderPolicy } from "../nexus/ladder";

// The certification ladder's VALUES, for Settings -> Procedures (epic
// memql#5408, #5412).
//
// A READ, ONCE, AND IT SAYS WHAT IT FOUND. `v1:authoring:ladderPolicy` is a
// seeded singleton at a literal id with no broadcast rule, and the seed
// re-asserts it on every boot -- so its values change with a deploy, not
// while somebody watches. There is nothing to subscribe to and nothing a
// periodic re-read would catch.
//
// THREE ANSWERS, KEPT APART. A row is the policy. NO row is a cluster that has
// not published one, which is said as such -- never filled in with the
// numbers the Go fallback happens to carry, because a Settings page that
// shows a value is claiming the cluster holds it. A refusal is the server's
// own sentence, verbatim.

export interface LadderPolicyRead {
  /** The values, or null when there is no row (or it has not been read yet). */
  policy: LadderPolicy | null;
  state: "loading" | "ready" | "error";
  /** The server's refusal, verbatim. "" when the read worked. */
  error: string;
  reload: () => void;
}

export function useLadderPolicy(): LadderPolicyRead {
  const connection = useOsConnection();
  const [nonce, setNonce] = useState(0);
  const [state, setState] = useState<Omit<LadderPolicyRead, "reload">>({
    policy: null,
    state: "loading",
    error: "",
  });

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) {
      setState({ policy: null, state: "error", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    setState((prev) => ({ ...prev, state: "loading", error: "" }));
    void (async () => {
      try {
        const result = await query.ladderPolicyCurrent({}, { signal: controller.signal });
        if (controller.signal.aborted) return;
        const row = (result.rows() as Row[])[0] ?? null;
        setState({ policy: ladderPolicyFromRow(row), state: "ready", error: "" });
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setState({ policy: null, state: "error", error: err instanceof Error ? err.message : String(err) });
      }
    })();
    return () => controller.abort();
  }, [connection, nonce]);

  return { ...state, reload };
}
