import { useMemo } from "react";

import { ladderPolicyFromRow, type LadderPolicy } from "../nexus/ladder";
import { feedAnswered, useLadderPolicyFeed } from "../nexus/useAutomations";

// The certification ladder's VALUES, for Settings -> Procedures (epic
// memql#5408, #5412).
//
// FOLLOWED, NOT READ ONCE. `v1:authoring:ladderPolicy` broadcasts through the
// `v1:authoring:*` rules in component/node/routing.go, and the seed rewrites
// the row on every boot -- so a deploy that changes a value changes it here,
// on every open window, without anybody asking.
//
// THREE ANSWERS, KEPT APART. A row is the policy. NO row is a cluster that has
// not published one, which is said as such -- never filled in with the
// numbers the Go fallback happens to carry, because a Settings page that
// shows a value is claiming the cluster holds it. A refusal is the server's
// own sentence, verbatim.

export interface LadderPolicyRead {
  /** The values, or null when there is no row (or it has not arrived yet). */
  policy: LadderPolicy | null;
  state: "loading" | "ready" | "error";
  /** The server's refusal, verbatim. "" when the read worked. */
  error: string;
}

export function useLadderPolicy(): LadderPolicyRead {
  const feed = useLadderPolicyFeed();
  const snapshot = feed.snapshot;
  const connected = feed.source !== null;
  return useMemo((): LadderPolicyRead => {
    if (snapshot.error !== "" && snapshot.rows.length === 0) {
      return { policy: null, state: "error", error: snapshot.error };
    }
    // The last known values stay on screen through a dropped connection: they
    // are a seeded row, and a blank would say less than the truth.
    if (feedAnswered(snapshot) || (snapshot.state === "disconnected" && snapshot.rows.length > 0)) {
      return { policy: ladderPolicyFromRow(snapshot.rows[0] ?? null), state: "ready", error: "" };
    }
    if (!connected || snapshot.state === "disconnected") {
      return { policy: null, state: "error", error: "Not connected to the cluster." };
    }
    return { policy: null, state: "loading", error: "" };
  }, [snapshot, connected]);
}
