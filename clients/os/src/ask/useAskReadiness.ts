import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { inferenceFrom } from "../apps/settings/routingFacts";
import { useSession } from "../chrome/access";
import { useConnectionStatus, type ShellConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "../live/connection";

export interface AskAvailability {
  state: "checking" | "ready" | "unavailable" | "error" | "disconnected" | "reconnecting";
  /** Always empty for readiness: Send disable + the dock indicator carry the signal. */
  message: string;
  refresh: () => void;
}

// Both entry points default to checking until the authoritative read lands.
export const READY_ASK: AskAvailability = { state: "ready", message: "", refresh: () => {} };
export const CHECKING_ASK: AskAvailability = { state: "checking", message: "", refresh: () => {} };

/** Provisional probe budget: yellow while trying, then red and stop. */
export const ASK_READINESS_MAX_PROBES = 8;
/** Fast interval while provisional so a just-paired machine lights up without a 30s wait. */
export const ASK_READINESS_PROBE_MS = 4_000;

/**
 * Dock connection-dot tone from transport + inference readiness.
 *
 * Yellow (unreachable) = provisional / connecting. Red (failed) = bounded
 * probes exhausted without a held stream. Blue (reachable) only when Ask can
 * send. Brief BFF reconnecting must not flap yellow when the last read was ready.
 */
export function connectionDotTone(
  connection: ShellConnectionStatus,
  ask: Pick<AskAvailability, "state">,
): "reachable" | "unreachable" | "failed" | "off" {
  if (connection === "disconnected") return "off";
  if (ask.state === "ready") return "reachable";
  if (ask.state === "unavailable" || ask.state === "error") return "failed";
  // checking / reconnecting → yellow provisional
  return "unreachable";
}

type ProbeAnswer = {
  scopeKey: string;
  state: "ready" | "unavailable" | "error";
  probes: number;
  probing: boolean;
};

function scopeKeyOf(parts: { connected: boolean; userId: string; epoch: number; moduleState: string | undefined }): string {
  return `${parts.connected}|${parts.userId}|${parts.epoch}|${parts.moduleState ?? ""}`;
}

/** One caller in ShellTransports, shared by sheet and widget. */
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
  const key = scopeKeyOf(scope);
  const [answer, setAnswer] = useState<ProbeAnswer | null>(null);
  const answerRef = useRef(answer);
  answerRef.current = answer;

  // Bounded probe loop: while not ready and under budget, re-query on an
  // interval; once exhausted, stay red and stop. Reconnect / module / refresh
  // bumps epoch and restarts the budget.
  useEffect(() => {
    if (!scope.connected || !scope.connection || !scope.userId) return;
    let stale = false;
    let timer: number | undefined;
    const abort = new AbortController();

    const run = (probesSoFar: number) => {
      void Promise.resolve()
        .then(() => scope.connection!.query.inferenceStatus({}, { signal: abort.signal }))
        .then((result) => {
          if (stale) return;
          const statusRow = inferenceFrom(result.rows()[0], "");
          if (statusRow.read && statusRow.streamingChatEligible === true) {
            setAnswer({ scopeKey: key, state: "ready", probes: probesSoFar + 1, probing: false });
            return;
          }
          const nextProbes = probesSoFar + 1;
          const exhausted = nextProbes >= ASK_READINESS_MAX_PROBES;
          const nextState: "unavailable" | "error" =
            statusRow.read && statusRow.streamingChatEligible === false ? "unavailable" : "error";
          setAnswer({ scopeKey: key, state: nextState, probes: nextProbes, probing: !exhausted });
          if (!exhausted) {
            timer = window.setTimeout(() => run(nextProbes), ASK_READINESS_PROBE_MS);
          }
        })
        .catch(() => {
          if (stale) return;
          const nextProbes = probesSoFar + 1;
          const exhausted = nextProbes >= ASK_READINESS_MAX_PROBES;
          setAnswer({ scopeKey: key, state: "error", probes: nextProbes, probing: !exhausted });
          if (!exhausted) {
            timer = window.setTimeout(() => run(nextProbes), ASK_READINESS_PROBE_MS);
          }
        });
    };

    setAnswer({ scopeKey: key, state: "error", probes: 0, probing: true });
    run(0);
    return () => {
      stale = true;
      abort.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [scope, key]);

  // Final disconnect blanks Ask. Brief SDK reconnecting keeps the last ready
  // reading so a BFF GoingAway / roll does not yellow-flap Send + the dock.
  if (status === "disconnected" || !connection || !userId) {
    return { state: "disconnected", message: "", refresh };
  }
  if (status === "reconnecting") {
    if (answer && answer.state === "ready") {
      return { state: "ready", message: "", refresh };
    }
    return { state: "checking", message: "", refresh };
  }
  if (!answer || answer.scopeKey !== key || (answer.probing && answer.state !== "ready" && answer.probes === 0)) {
    return { ...CHECKING_ASK, refresh };
  }
  if (answer.state === "ready") {
    return { state: "ready", message: "", refresh };
  }
  if (answer.probing) {
    return { state: "checking", message: "", refresh };
  }
  return { state: answer.state, message: "", refresh };
}
