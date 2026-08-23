import { useCallback, useEffect, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";

// The machines list: the caller's cockpit registrations and the local apps
// each one reports (memql#4363).
//
// # `runnable` is computed the same way the ENGINE computes it
//
// A machine is only routable for an app that is BOTH allowed by its own
// policy.yaml AND signed in. Rendering "has Claude Code" off the presence of
// the entry alone would show a green row for a machine the router will never
// select -- and the person reading it would go looking for a routing bug that
// is not there. So the badge says exactly what selection says.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface MachineApp {
  id: string;
  version: string;
  signedIn: boolean;
  allowed: boolean;
  subscription: string;
  // runnable mirrors the engine's rule: known id AND allowed AND signed in.
  runnable: boolean;
}

export interface Machine {
  id: string;
  name: string;
  version: string;
  buildTag: string;
  lastSeenAt: string;
  revokedAt: string;
  apps: MachineApp[];
}

export interface MachinesState {
  machines: Machine[];
  loading: boolean;
  error: string;
  reload: () => void;
}

// The engine's closed runnable set, mirrored so the badge agrees with
// selection. An id outside it is displayed (the machine reported it) and
// never marked runnable (the engine cannot drive it).
const KNOWN_APP_IDS = new Set(["claude-code", "codex"]);

export const APP_LABELS: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
};

export function appLabel(appId: string): string {
  return APP_LABELS[appId] ?? appId;
}

function appsFromRow(row: Row): MachineApp[] {
  const raw = (row as Record<string, unknown>)["apps"];
  if (!Array.isArray(raw)) return [];
  return raw
    .map((item): MachineApp | null => {
      if (typeof item !== "object" || item === null) return null;
      const entry = item as Record<string, unknown>;
      const id = typeof entry["id"] === "string" ? entry["id"] : "";
      if (id === "") return null;
      const allowed = entry["allowed"] === true;
      const signedIn = entry["signedIn"] === true;
      return {
        id,
        version: typeof entry["version"] === "string" ? entry["version"] : "",
        signedIn,
        allowed,
        subscription: typeof entry["subscription"] === "string" ? entry["subscription"] : "unknown",
        runnable: KNOWN_APP_IDS.has(id) && allowed && signedIn,
      };
    })
    .filter((app): app is MachineApp => app !== null)
    .sort((a, b) => a.id.localeCompare(b.id));
}

function machineFromRow(row: Row): Machine {
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name") || "Unnamed machine",
    version: rowString(row, "version"),
    buildTag: rowString(row, "buildTag"),
    lastSeenAt: rowString(row, "lastSeenAt"),
    revokedAt: rowString(row, "revokedAt"),
    apps: appsFromRow(row),
  };
}

export function useMachines(): MachinesState {
  const { query } = useCluster();
  const { access } = useMyAccess();
  const ownerUserId = access?.userId ?? "";

  const [machines, setMachines] = useState<Machine[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || ownerUserId === "") return;
    let live = true;
    setLoading(true);
    setError("");

    void query
      .workersForUser({ ownerUserId })
      .then((res) => {
        if (!live) return;
        setMachines(res.rows().map(machineFromRow));
      })
      .catch((err: unknown) => {
        // A LISTING ERROR IS NOT AN EMPTY LIST. An empty table here would
        // read as "you have no machines", which is the answer that stops
        // somebody investigating.
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, ownerUserId, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  return { machines, loading, error, reload };
}
