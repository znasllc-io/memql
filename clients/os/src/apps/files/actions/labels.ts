import { useCallback, useState } from "react";

import { useOsConnection } from "../../../live/connection";

// The Files app's label write (epic memql#5009).
//
// ===========================================================================
// OPTIMISTIC, AND IT ROLLS BACK -- WHICH IS THE WHOLE POINT
// ===========================================================================
// The sibling write in this directory, `useArtifactAccounts`, deliberately
// patches NOTHING locally: `v1:library:artifact` broadcasts `updated`, so the
// row arrives on the feed the browse, the inspector and the desk popover all
// read, and a local patch would be a second answer racing the real one.
//
// Labels are the case where that is not enough, and the difference is the
// CONTROL. A client tie is a set of toggles that renders from the row; a
// label is TYPED, and a chip that appears only after a round trip reads as a
// keystroke that was dropped. So the editor holds its own list, applies the
// change immediately, and -- the part that makes it honest -- puts the list
// back exactly as it was when the write is refused, with the SERVER'S OWN
// SENTENCE beside it. A label that appears and is silently not there at the
// next reload is worse than one that visibly refuses.
//
// The overlay is per-artifact and the caller re-seeds it from the row (see
// the inspector): the arriving broadcast is still the authority, and this
// only covers the gap between the click and the echo.
//
// ONE BUSY/ERROR PAIR, like every other action in this app: a refusal belongs
// beside the control that produced it, never in a toast.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface ArtifactLabelsState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  /** A one-line announcement for a role="status" region. */
  announcement: string;
  /**
   * Add one label. Resolves TRUE only when the cluster took the write.
   *
   * Not void: the caller has already applied the change optimistically, and
   * only a boolean lets it put the list back on a refusal at the same moment
   * the surface says the write did not happen.
   */
  add: (artifactId: string, label: string) => Promise<boolean>;
  remove: (artifactId: string, label: string) => Promise<boolean>;
  /** Drop a standing refusal -- the editor clears it when a new edit starts. */
  clearError: () => void;
}

export function useArtifactLabels(): ArtifactLabelsState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [announcement, setAnnouncement] = useState("");

  const write = useCallback(
    async (
      run: (query: NonNullable<typeof connection>["query"]) => Promise<unknown>,
      said: string,
    ): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        await run(query);
        setAnnouncement(said);
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  const add = useCallback(
    (artifactId: string, label: string) =>
      // Both builtins are IDEMPOTENT server-side -- a label already present is
      // left alone and nothing is written -- so a double click costs a
      // no-op rather than a duplicate.
      write(
        (query) => query.libraryAddArtifactLabel({ artifactId, label }),
        `Added the label "${label}".`,
      ),
    [write],
  );

  const remove = useCallback(
    (artifactId: string, label: string) =>
      write(
        (query) => query.libraryRemoveArtifactLabel({ artifactId, label }),
        `Removed the label "${label}".`,
      ),
    [write],
  );

  return { busy, error, announcement, add, remove, clearError: () => setError("") };
}
