import { useCallback, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import {
  archivePackage,
  archiveSite,
  createPackage,
  deployPackage,
  repointSite,
  restorePackage,
  restoreSite,
  rollbackPackage,
  setSiteLive,
  type NewPackageInput,
} from "./calls";

// The writes this surface makes, each with its own busy/refusal pair.
//
// ONE HOOK PER SURFACE, not one per call, and the reason is the same one
// `../actions.ts` records: a refusal belongs beside the control that produced
// it, never as a toast. A shared pair across the list, the detail panel and
// the archive control would put a hostname refusal under a button somebody was
// not looking at -- worse than no message, because it names a failure they did
// not cause.
//
// NOTHING HERE CHECKS A ROLE, and nothing here is the authorization. Every
// capability re-resolves its rows under the caller's own actor, the D9 gate
// reads the verified access context rather than an argument, and the D10 law
// is enforced beside the engine's write path. The admin gate on these controls
// is presentation.

/** A refusal, split into the code this build may recognise and the server's
 *  own sentence -- which is rendered verbatim either way. */
export interface Refusal {
  code: string;
  message: string;
}

/**
 * describe turns a thrown engine error into a code and a sentence.
 *
 * The code is read out of the message because that is where the engine puts it
 * (`<code>: <detail>`, or `<code> (<scope>): <detail>`). An unrecognised shape
 * keeps the WHOLE message as its sentence and carries no code, which is what
 * makes the renderer fall back to a neutral heading. Inventing a friendly
 * sentence for an unknown failure is how a real fault gets mistaken for
 * somebody's mistake.
 */
export function describe(err: unknown): Refusal {
  const raw = err instanceof Error ? err.message : String(err);
  const match = /(?:^|[\s])([a-z][a-z0-9_]{4,})\s*(?:\([^)]*\))?:\s*([\s\S]+)$/.exec(raw.trim());
  if (match && match[1] && match[2]) {
    return { code: match[1], message: match[2].trim() };
  }
  return { code: "", message: raw };
}

interface WriteState {
  busy: boolean;
  refusal: Refusal | null;
  clear: () => void;
}

function useWrite(): WriteState & { run: <T>(fn: (query: NonNullable<ReturnType<typeof useOsConnection>>["query"]) => Promise<T>) => Promise<T | null> } {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const run = useCallback(
    async <T,>(fn: (query: NonNullable<ReturnType<typeof useOsConnection>>["query"]) => Promise<T>): Promise<T | null> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRefusal({ code: "", message: "Not connected to the cluster, so nothing was written." });
        return null;
      }
      setBusy(true);
      setRefusal(null);
      try {
        return await fn(query);
      } catch (err) {
        setRefusal(describe(err));
        return null;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, refusal, clear: () => setRefusal(null), run };
}

// ---------------------------------------------------------------------------
// Packages
// ---------------------------------------------------------------------------

export interface PackageActions extends WriteState {
  /** Start a run. `confirm: false` parks it at the gate with its report. */
  deploy: (packageId: string, confirm: boolean, hostnames?: Record<string, string>) => Promise<void>;
  rollback: (packageId: string, deploymentId: string) => Promise<void>;
  archive: (packageId: string, confirmName: string) => Promise<void>;
  restore: (packageId: string) => Promise<void>;
}

export function usePackageActions(): PackageActions {
  const { busy, refusal, clear, run } = useWrite();
  return {
    busy,
    refusal,
    clear,
    deploy: async (packageId, confirm, hostnames) => {
      await run((query) => deployPackage(query, packageId, { confirm, ...(hostnames ? { hostnames } : {}) }));
    },
    rollback: async (packageId, deploymentId) => {
      await run((query) => rollbackPackage(query, packageId, deploymentId));
    },
    archive: async (packageId, confirmName) => {
      await run((query) => archivePackage(query, packageId, confirmName));
    },
    restore: async (packageId) => {
      await run((query) => restorePackage(query, packageId));
    },
  };
}

export interface NewPackageActions extends WriteState {
  /** Returns the new package's id, or "" when the write was refused. */
  create: (input: NewPackageInput) => Promise<string>;
}

export function useNewPackage(): NewPackageActions {
  const { busy, refusal, clear, run } = useWrite();
  return {
    busy,
    refusal,
    clear,
    create: async (input) => (await run((query) => createPackage(query, input))) ?? "",
  };
}

// ---------------------------------------------------------------------------
// Site lifecycle (per-site parity)
// ---------------------------------------------------------------------------

export interface SiteLifecycleActions extends WriteState {
  setStatus: (siteId: string, status: "live" | "disabled" | "draft") => Promise<void>;
  archive: (siteId: string, confirmHostname: string) => Promise<void>;
  restore: (siteId: string) => Promise<void>;
  rollTo: (siteId: string, bundleRef: string) => Promise<void>;
}

export function useSiteLifecycle(): SiteLifecycleActions {
  const { busy, refusal, clear, run } = useWrite();
  return {
    busy,
    refusal,
    clear,
    setStatus: async (siteId, status) => {
      await run((query) => setSiteLive(query, siteId, status));
    },
    archive: async (siteId, confirmHostname) => {
      await run((query) => archiveSite(query, siteId, confirmHostname));
    },
    restore: async (siteId) => {
      await run((query) => restoreSite(query, siteId));
    },
    rollTo: async (siteId, bundleRef) => {
      await run((query) => repointSite(query, siteId, bundleRef));
    },
  };
}
