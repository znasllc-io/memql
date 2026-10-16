import { useCallback } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { EMPTY_SET, recommendedSetFrom } from "./recommended";

// Settings reads the engine-derived machine class and usable capacity together.
// This virtual projection has no live collection or model-performance dependency.
export function useMachineRecommendation(workerId: string, enabled = true) {
  const connection = useOsConnection();
  const read = useCallback(async (signal: AbortSignal) => {
    const result = await connection!.query.fleetRecommended({ registrationId: workerId }, { signal });
    return recommendedSetFrom(result.rows()[0] as Row | undefined);
  }, [connection, workerId]);
  const reading = useReading(`machine-recommendation:${workerId}`, enabled && connection && workerId ? read : null);
  return { ...reading, recommended: reading.value ?? EMPTY_SET };
}
