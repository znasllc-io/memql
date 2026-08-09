import { useCallback, useEffect, useState } from "react";
import type { Role } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { buildCall, readStatusPayload, type IntegrationStatus } from "./calls";

// Reading this node's integration registry (memql#3323).
//
// TWO QUESTIONS, TWO FETCHES, and keeping them apart is the whole point of
// this hook's shape:
//
//   the CONFIGURATION read runs on mount and on every refresh. It is cheap,
//   it touches no third party, and it is what the page is about.
//
//   the PROBE runs only when an operator asks. It costs a round trip to
//   Microsoft or to a mail relay, and a page that probed on mount would send
//   the operator's provider a request every time they navigated back.
//
// So `probing` is a distinct action with its own busy flag and its own error
// channel, and the status it returns REPLACES the configuration read's answer
// (the probed reply is a superset -- same configuration, plus a verdict).

// Reading integration configuration is an owner/admin question. The gate is
// enforced SERVER-SIDE in the capability handler; this constant only decides
// whether the page offers the control, so a caller below the floor gets an
// explanation instead of a refusal they have to interpret.
const CAN_VIEW: readonly Role[] = ["owner", "admin"];

export interface IntegrationStatusState {
  role: Role;
  canView: boolean;
  status: IntegrationStatus | null;
  loading: boolean;
  // The configuration read's failure. Separate from probeError so a failed
  // live check does not read as "the registry is unreadable".
  error: string;
  probing: boolean;
  probeError: string;
  refresh: () => void;
  probe: () => void;
}

export function useIntegrationStatus(): IntegrationStatusState {
  const { query } = useCluster();
  const { access } = useMyAccess();
  const role: Role = access?.clusterRole ?? "";
  const canView = CAN_VIEW.includes(role);

  const [status, setStatus] = useState<IntegrationStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || !canView) {
      setStatus(null);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void query
      .executeNamed("integrationStatus", buildCall("builtin", "integrationStatus", { probe: false }))
      .then((result) => {
        if (!live) return;
        const payload = readStatusPayload(result.rows());
        if (payload === null) {
          setError("The cluster answered, but the reply carried no integration report.");
          return;
        }
        setStatus(payload);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, canView, epoch]);

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);

  const probe = useCallback(() => {
    if (query === null || !canView) return;
    setProbing(true);
    setProbeError("");
    void query
      .executeNamed("integrationStatus", buildCall("builtin", "integrationStatus", { probe: true }))
      .then((result) => {
        const payload = readStatusPayload(result.rows());
        if (payload === null) {
          setProbeError("The check ran, but the reply carried no integration report.");
          return;
        }
        setStatus(payload);
      })
      .catch((err: unknown) => setProbeError(describe(err)))
      .finally(() => setProbing(false));
  }, [query, canView]);

  return { role, canView, status, loading, error, probing, probeError, refresh, probe };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
