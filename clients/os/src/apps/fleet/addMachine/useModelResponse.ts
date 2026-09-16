import { useCallback, useEffect, useState } from "react";
import { aiChat } from "@znasllc-io/memql-sdk-core/ai";
import { useOsConnection } from "../../../live/connection";

export type ModelResponse = { state: "waiting" | "running" | "passed" | "failed"; error?: string };
export const RESPONSE_TIMEOUT_MS = 120_000;
const WAITING: ModelResponse = { state: "waiting" };

/** Held by the Fleet flow so changing sections does not restart the check. */
export function useModelResponse(registrationId: string, modelId: string, enabled: boolean) {
  const connection = useOsConnection();
  const [attempt, setAttempt] = useState(0);
  const key = JSON.stringify([registrationId, modelId, attempt]);
  const [result, setResult] = useState<{ key: string; response: ModelResponse } | null>(null);
  const retry = useCallback(() => setAttempt(value => value + 1), []);

  useEffect(() => {
    if (!enabled || !connection || !registrationId || !modelId) return;
    let live = true;
    const controller = new AbortController();
    let deadline: ReturnType<typeof setTimeout> | undefined;
    setResult({ key, response: { state: "running" } });
    // Defer dispatch so React StrictMode's setup/cleanup rehearsal cannot
    // send a duplicate request. Heartbeat objects are not dependencies.
    const start = setTimeout(() => {
      const timeout = new Promise<never>((_, reject) => {
        deadline = setTimeout(() => {
          reject(new Error("The model did not respond within two minutes."));
          controller.abort();
        }, RESPONSE_TIMEOUT_MS);
      });
      void Promise.race([
        aiChat(connection.dispatcher, [{ role: "user", content: "Reply with exactly one word: hello." }], {
          provider: `fleet:${modelId}`,
          fleetRegistrationId: registrationId,
          signal: controller.signal,
        }),
        timeout,
      ]).then(reply => {
        if (!reply.message.content.trim()) throw new Error("The model returned an empty response.");
        if (live) setResult({ key, response: { state: "passed" } });
      }).catch((error: unknown) => {
        if (live) setResult({ key, response: { state: "failed", error: error instanceof Error ? error.message : String(error) } });
      }).finally(() => clearTimeout(deadline));
    }, 0);
    return () => {
      live = false;
      clearTimeout(start);
      clearTimeout(deadline);
      controller.abort();
    };
  }, [connection, registrationId, modelId, enabled, key]);

  return { response: result?.key === key ? result.response : WAITING, retry };
}
