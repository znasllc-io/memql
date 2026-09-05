import { useCallback, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import {
  archivePackage,
  cancelDeployment,
  archiveSite,
  deleteSite,
  createPackage,
  createSourceCredential,
  deployPackage,
  repointSite,
  restorePackage,
  restoreSite,
  revokeSourceCredential,
  rollbackPackage,
  setPackageAutoDeploy,
  disableDeployables,
  enableDeployables,
  setPackageCredential,
  setSiteLive,
  type NewCredential,
  type NewPackageInput,
  type Placement,
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

export interface WriteState {
  busy: boolean;
  refusal: Refusal | null;
  clear: () => void;
}

/**
 * The shape every write hook in this app is built from.
 *
 * EXPORTED so `sources/useGithubConnect.ts` can build on it rather than
 * respell it (epic memql#4915). A second copy of this would be a second
 * refusal-parsing path, and the two would drift on exactly the case that
 * matters: what happens to an error whose shape `describe` does not
 * recognise.
 */
export function useWrite(): WriteState & { run: <T>(fn: (query: NonNullable<ReturnType<typeof useOsConnection>>["query"]) => Promise<T>) => Promise<T | null> } {
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
  deploy: (packageId: string, confirm: boolean, placements?: Record<string, Placement>) => Promise<void>;
  /**
   * Retry a run that was lost, from the bytes it already fetched (memql#4900).
   * A separate verb from `deploy` because it is a different promise: deploy
   * takes whatever the source holds now, retry takes what the lost run was
   * deploying.
   */
  retry: (packageId: string, fromDeploymentId: string) => Promise<void>;
  /** Arm or disarm auto-deploy for this source. */
  setAutoDeploy: (packageId: string, autoDeploy: boolean) => Promise<void>;
  /**
   * Turn declared deployables off, by name.
   *
   * NO CURRENT LIST. It used to take one, because the DSL had no way to remove
   * a member of an array and the caller had to compose the whole result
   * (memql#4951). The caller now says what CHANGED, which is what makes two
   * windows toggling two different apps both land.
   */
  disableDeployables: (packageId: string, names: readonly string[]) => Promise<void>;
  /** Turn declared deployables back on, by name. The exact inverse. */
  enableDeployables: (packageId: string, names: readonly string[]) => Promise<void>;
  rollback: (packageId: string, deploymentId: string) => Promise<void>;
  archive: (packageId: string, confirmName: string) => Promise<void>;
  restore: (packageId: string) => Promise<void>;
  /** Switch which of the caller's credentials this source fetches under. */
  setCredential: (packageId: string, credentialId: string) => Promise<void>;
  /**
   * Ask a run in flight to stop (epic memql#4937, D3). It flags the row; the
   * node running the attempt closes it `cancelled` at its next stage boundary.
   */
  cancel: (packageId: string, deploymentId: string) => Promise<void>;
}

export function usePackageActions(): PackageActions {
  const { busy, refusal, clear, run } = useWrite();
  return {
    busy,
    refusal,
    clear,
    deploy: async (packageId, confirm, placements) => {
      await run((query) => deployPackage(query, packageId, { confirm, ...(placements ? { placements } : {}) }));
    },
    retry: async (packageId, fromDeploymentId) => {
      await run((query) => deployPackage(query, packageId, { confirm: false, fromDeploymentId }));
    },
    setAutoDeploy: async (packageId, autoDeploy) => {
      await run((query) => setPackageAutoDeploy(query, packageId, autoDeploy));
    },
    disableDeployables: async (packageId, names) => {
      await run((query) => disableDeployables(query, packageId, names));
    },
    enableDeployables: async (packageId, names) => {
      await run((query) => enableDeployables(query, packageId, names));
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
    setCredential: async (packageId, credentialId) => {
      await run((query) => setPackageCredential(query, packageId, credentialId));
    },
    cancel: async (packageId, deploymentId) => {
      await run((query) => cancelDeployment(query, packageId, deploymentId));
    },
  };
}

// ---------------------------------------------------------------------------
// Personal source credentials
// ---------------------------------------------------------------------------

export interface CredentialActions extends WriteState {
  /** Seal a token and answer its card. "" for `credentialId` means the write was refused. */
  add: (input: { host: string; label: string; token: string }) => Promise<NewCredential>;
  revoke: (credentialId: string) => Promise<void>;
}

/**
 * The two credential writes.
 *
 * THE TOKEN IS A PARAMETER AND NEVER STATE. It reaches `add`, goes into the
 * one call that takes it, and is gone -- there is nothing on this hook that
 * holds it, so a later write cannot carry it by accident (design G).
 */
export function useCredentialActions(): CredentialActions {
  const { busy, refusal, clear, run } = useWrite();
  return {
    busy,
    refusal,
    clear,
    add: async (input) =>
      (await run((query) => createSourceCredential(query, input))) ?? { credentialId: "", fingerprint: "" },
    revoke: async (credentialId) => {
      await run((query) => revokeSourceCredential(query, credentialId));
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
  /**
   * The fourth rung (epic memql#4937). Answers whether the delete was
   * ACCEPTED, because the caller has to know whether to show the teardown --
   * a refusal renders on the bar and the deployable stays exactly as it was.
   */
  remove: (siteId: string, confirmHostname: string) => Promise<boolean>;
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
    remove: async (siteId, confirmHostname) => {
      // `run` answers null on a refusal, which is what separates "the cluster
      // said no" from "it worked and returned nothing".
      const done = await run(async (query) => {
        await deleteSite(query, siteId, confirmHostname);
        return true;
      });
      return done === true;
    },
  };
}
