import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import {
  EMPTY_SET,
  measurementFrom,
  recommendedSetFrom,
  type Measurement,
  type RecommendedSet,
} from "./recommended";

// The two READS the Models group needs beyond the pull feed, and the two acts
// that go with them (epic memql#5146, D2 and D3).
//
// ===========================================================================
// NEITHER IS LIVE, AND THAT IS THE RIGHT CHOICE HERE
// ===========================================================================
// `fleetRecommended` is a virtual projection with no `graph.node.*` event
// behind it -- a collection over one would render "Loading from the cluster"
// and then a list that never moves, which is the trap useModelPulls' header
// already records for `fleetModels`.
//
// Measurements are stored rows without a browser broadcast route. A probe
// queues work on an agent and returns before the measurement lands. Visible
// consumers use AutoRefresh for these reads, including probes started elsewhere.

export interface MachineInference {
  recommended: RecommendedSet;
  /** Keyed by model id, for the machine this page is about. */
  measurements: Map<string, Measurement>;
  loading: boolean;
  error: string;
  /** Pull every model in the recommended set, in order. Resolves to "" on
   *  success or the refusal to show. */
  pullRecommended: () => Promise<string>;
  pullingSet: boolean;
  /** Measure one model this machine already has. */
  probe: (modelId: string) => Promise<string>;
  probing: string;
  /** Re-read the measurements and recommendation. */
  refreshMeasurements: () => void;
}

function refusalFrom(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  const trimmed = raw.trim();
  return trimmed === "" ? "The cluster refused and said nothing about why." : trimmed;
}

export function useMachineInference(workerId: string, enabled = true): MachineInference {
  const connection = useOsConnection();
  const [recommended, setRecommended] = useState<RecommendedSet>(EMPTY_SET);
  const [measurements, setMeasurements] = useState<Map<string, Measurement>>(new Map());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [pullingSet, setPullingSet] = useState(false);
  const [probing, setProbing] = useState("");
  const [measurementNonce, setMeasurementNonce] = useState(0);

  useEffect(() => {
    if (!enabled || connection === null || workerId === "") {
      setLoading(false);
      return;
    }
    const controller = new AbortController();
    let cancelled = false;
    setLoading(true);
    setError("");

    void (async () => {
      try {
        const [set, measured] = await Promise.all([
          connection.query.fleetRecommended({ registrationId: workerId }, { signal: controller.signal }),
          connection.query.measurementsForMachine(
            { machineId: workerId },
            { signal: controller.signal },
          ),
        ]);
        if (cancelled) return;
        setRecommended(recommendedSetFrom((set.rows()[0] as Row | undefined) ?? null));
        // NEWEST WINS PER MODEL. The read is sorted newest first, so the first
        // row for a model id is the one to keep -- a re-probe under the same
        // suite is a new version of one logical row, and an older version
        // arriving second must not overwrite it.
        const byModel = new Map<string, Measurement>();
        for (const raw of measured.rows()) {
          const m = measurementFrom(raw as Row);
          if (m.modelId === "" || byModel.has(m.modelId)) continue;
          byModel.set(m.modelId, m);
        }
        setMeasurements(byModel);
      } catch (err: unknown) {
        if (cancelled) return;
        // A FAILED READ HERE DOES NOT EMPTY THE GROUP. The models this machine
        // reports come from the registration row and are already on screen;
        // this read only adds the recommendation and the figures, so its
        // failure is a note rather than a blank page.
        setError(refusalFrom(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [connection, workerId, measurementNonce, enabled]);

  const pullRecommended = useCallback(async (): Promise<string> => {
    if (connection === null) return "A pull can only be started over a live connection.";
    setPullingSet(true);
    try {
      await connection.query.fleetPullRecommended({ registrationId: workerId });
      return "";
    } catch (err: unknown) {
      return refusalFrom(err);
    } finally {
      setPullingSet(false);
    }
  }, [connection, workerId]);

  const probe = useCallback(
    async (modelId: string): Promise<string> => {
      if (connection === null) return "A probe can only be started over a live connection.";
      setProbing(modelId);
      try {
        await connection.query.fleetModelProbe({ registrationId: workerId, model: modelId });
        return "";
      } catch (err: unknown) {
        return refusalFrom(err);
      } finally {
        setProbing("");
      }
    },
    [connection, workerId],
  );

  const refreshMeasurements = useCallback(() => {
    setMeasurementNonce((n) => n + 1);
  }, []);

  return {
    recommended,
    measurements,
    loading,
    error,
    pullRecommended,
    pullingSet,
    probe,
    probing,
    refreshMeasurements,
  };
}
