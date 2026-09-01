import { useCallback, useState } from "react";

import { useOsConnection } from "../../../live/connection";

// The Files app's client-labelling write (epic memql#4800, D5).
//
// ITS OWN BUSY/ERROR PAIR, like every other action in this app: a refusal
// belongs beside the control that produced it, and this control sits in the
// inspector alongside download, archive and move, each of which reports
// separately for the same reason.

export interface ArtifactAccountsState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  /** Pass [] to remove every client label. */
  setAccounts: (artifactId: string, accountIds: string[]) => Promise<boolean>;
}

export function useArtifactAccounts(): ArtifactAccountsState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const setAccounts = useCallback(
    async (artifactId: string, accountIds: string[]): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        // `setArtifactAccounts` STAMPS the list, so an empty array clears
        // every label rather than inheriting the stored one -- see the
        // mutation for why removing the last label would otherwise silently
        // re-save it.
        await query.setArtifactAccounts({ artifactId, accountIds });
        // NOTHING IS UPDATED LOCALLY. v1:library:artifact broadcasts
        // `updated`, so the row arrives on the feed the browse, the inspector
        // and the desk popover all read.
        return true;
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, setAccounts };
}
