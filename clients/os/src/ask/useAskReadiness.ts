import { useCallback, useEffect, useMemo, useState } from "react";

import { inferenceFrom } from "../apps/settings/routingFacts";
import { useSession } from "../chrome/access";
import { useConnectionStatus, type ShellConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "../live/connection";

export interface AskAvailability {
  state: "checking" | "ready" | "unavailable" | "error" | "disconnected" | "reconnecting";
  message: string;
  refresh: () => void;
}

// Both entry points default to checking until the authoritative read lands.
export const READY_ASK: AskAvailability = { state: "ready", message: "", refresh: () => {} };
export const CHECKING_ASK: AskAvailability = { state: "checking", message: "", refresh: () => {} };

/**
 * Dock connection-dot tone from transport + inference readiness.
 *
 * Connection ≠ inference. A live WebSocket with no usable chat route must not
 * read as "reachable" (the blue/green dot Jose saw while Ask could not run).
 * Only `ready` is reachable; reconnecting / checking / unavailable / error on
 * a live or recovering transport are unreachable; a final disconnect is off.
 */
export function connectionDotTone(
  connection: ShellConnectionStatus,
  ask: Pick<AskAvailability, "state">,
): "reachable" | "unreachable" | "off" {
  if (connection === "disconnected") return "off";
  // Reconnecting means the transport cannot serve Ask right now, even if the
  // last inferenceStatus said ready.
  if (connection === "reconnecting") return "unreachable";
  if (ask.state === "ready") return "reachable";
  return "unreachable";
}

/** One caller in ShellTransports, shared by sheet and widget. Shared Fleet
 * models are absent from the owner's machine feed, so read the authoritative
 * inferenceStatus used by Settings. Refresh on reconnect, module state changes,
 * explicitly, and every 30s; heartbeat timestamps never trigger queries. */
export function useAskReadiness(): AskAvailability {
  const connection = useOsConnection();
  const status = useConnectionStatus();
  const { access, readiness } = useSession();
  const userId = access?.userId ?? "";
  const moduleState = readiness?.of("ai")?.state;
  const [epoch, setEpoch] = useState(0);
  const refresh = useCallback(() => setEpoch((n) => n + 1), []);
  const connected = status === "connected";
  const scope = useMemo(
    () => ({ connection, connected, userId, epoch, moduleState }),
    [connection, connected, userId, epoch, moduleState],
  );
  const [answer, setAnswer] = useState<{ scope: typeof scope; state: "ready" | "unavailable" | "error"; message: string } | null>(null);

  useEffect(() => {
    if (!connected || !connection || !userId) return;
    const timer = window.setInterval(refresh, 30_000);
    return () => window.clearInterval(timer);
  }, [connected, connection, userId, refresh]);

  useEffect(() => {
    if (!scope.connected || !scope.connection || !scope.userId) return;
    const abort = new AbortController();
    let stale = false;
    void Promise.resolve().then(() => scope.connection!.query.inferenceStatus({}, { signal: abort.signal })).then((result) => {
      if (stale) return;
      const statusRow = inferenceFrom(result.rows()[0], "");
      if (statusRow.read && statusRow.streamingChatEligible === true) {
        setAnswer({ scope, state: "ready", message: "" });
      } else if (statusRow.read && statusRow.streamingChatEligible === false) {
        setAnswer({ scope, state: "unavailable", message: "No chat model is available. Open Fleet to connect a machine or check its models." });
      } else {
        setAnswer({ scope, state: "error", message: "The cluster has not reported whether chat is available. Check again." });
      }
    }).catch(() => {
      if (!stale) setAnswer({ scope, state: "error", message: "Could not check whether chat is available. Check the connection and try again." });
    });
    return () => { stale = true; abort.abort(); };
  }, [scope]);

  // Transport gaps: only a FINAL disconnect is "lost connection". SDK
  // reconnecting is expected under production-grade keepalive and must not
  // flash the Ask banner Jose saw after a successful answer.
  if (status === "reconnecting") {
    return {
      state: "reconnecting",
      message: "Reconnecting to the cluster. Your draft stays here.",
      refresh,
    };
  }
  if (status === "disconnected" || !connection || !userId) {
    return {
      state: "disconnected",
      message: "Not connected to the cluster. Your draft stays here while it reconnects.",
      refresh,
    };
  }
  if (!answer || answer.scope !== scope) return { ...CHECKING_ASK, refresh };
  return { state: answer.state, message: answer.message, refresh };
}
