import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { inferenceFrom } from "../apps/settings/routingFacts";
import { useSession } from "../chrome/access";
import { useConnectionStatus, type ShellConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "../live/connection";

export interface AskAvailability {
  state: "checking" | "ready" | "unavailable" | "error" | "disconnected" | "reconnecting";
  /** Always empty for readiness: Send disable + the dock indicator/tooltip carry the signal. */
  message: string;
  refresh: () => void;
}

export const READY_ASK: AskAvailability = { state: "ready", message: "", refresh: () => {} };
export const CHECKING_ASK: AskAvailability = { state: "checking", message: "", refresh: () => {} };

/** Provisional probe budget: yellow while trying, then red and stop. */
export const ASK_READINESS_MAX_PROBES = 8;
/** First retry delay while provisional. Each failure multiplies (see probeDelayMs). */
export const ASK_READINESS_PROBE_MS = 2_000;
/** Cap so a long provisional window does not hammer inferenceStatus. */
export const ASK_READINESS_PROBE_MAX_MS = 16_000;

/** Increasing backoff after each failed/unavailable probe. */
export function probeDelayMs(probesSoFar: number): number {
  const n = Math.max(0, probesSoFar);
  const delay = ASK_READINESS_PROBE_MS * 2 ** Math.min(n, 3);
  return Math.min(delay, ASK_READINESS_PROBE_MAX_MS);
}

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

/** Hover tooltip copy by dock tone — replaces bouncing Open Fleet banners. */
export function connectionDotTooltip(
  tone: ReturnType<typeof connectionDotTone>,
): string {
  switch (tone) {
    case "reachable":
      return "Ask is ready — fleet stream held.";
    case "unreachable":
      return "Connecting Ask to inference…";
    case "failed":
      return "Ask has no usable inference. Open Fleet to connect a machine or check its models.";
    case "off":
      return "Not connected to the cluster.";
  }
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

  // Bounded probe loop with increasing backoff. Once exhausted, stay red and
  // stop. Reconnect / module / refresh bumps epoch and restarts the budget.
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
            timer = window.setTimeout(() => run(nextProbes), probeDelayMs(nextProbes));
          }
        })
        .catch(() => {
          if (stale) return;
          const nextProbes = probesSoFar + 1;
          const exhausted = nextProbes >= ASK_READINESS_MAX_PROBES;
          setAnswer({ scopeKey: key, state: "error", probes: nextProbes, probing: !exhausted });
          if (!exhausted) {
            timer = window.setTimeout(() => run(nextProbes), probeDelayMs(nextProbes));
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
