import { useCallback, useRef, useState } from "react";
import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { EMPTY_MANIFEST, branchNamesFrom, manifestFrom, type ArtifactProbeReply, type SourceProbeReply } from "./probe";

// The two compose probes, as hooks (epic memql#4885, task memql#4891).
//
// ===========================================================================
// A COURTESY, WITH A COURTESY'S FAILURE MODE
// ===========================================================================
// Neither probe writes anything, and neither is a gate. A probe that could
// not RUN leaves the stop editable and its refusal is rendered in place, in
// the server's own words -- design H is explicit that it must never block
// Analyze on a public repository, because the fetch is the authority.
//
// ===========================================================================
// THE LAST ANSWER WINS, AND ONLY THE LAST ONE
// ===========================================================================
// The repository probe fires on BLUR, and a person who tabs through a field
// twice has two in flight. A sequence number is compared on return, so a
// slow first answer cannot overwrite a fast second one -- the failure that
// would otherwise show "private, or not there" beside a URL that had already
// probed `ok`.

interface ProbeState<T> {
  reply: T | null;
  /** The server's own sentence when the probe could not run. "" otherwise. */
  error: string;
  busy: boolean;
}

const IDLE = { reply: null, error: "", busy: false };

export interface SourceProbeHandle extends ProbeState<SourceProbeReply> {
  probe: (repoUrl: string, credentialId: string) => Promise<void>;
  /** Forget the last answer -- the URL changed, so the answer is about a different repository. */
  clear: () => void;
}

export function useSourceProbe(): SourceProbeHandle {
  const connection = useOsConnection();
  const [state, setState] = useState<ProbeState<SourceProbeReply>>(IDLE);
  const seq = useRef(0);

  const clear = useCallback(() => {
    seq.current += 1;
    setState(IDLE);
  }, []);

  const probe = useCallback(
    async (repoUrl: string, credentialId: string) => {
      const query = connection?.query ?? null;
      const url = repoUrl.trim();
      if (url === "") {
        clear();
        return;
      }
      if (query === null) {
        setState({ reply: null, error: "Not connected to the cluster, so the source was not checked.", busy: false });
        return;
      }
      seq.current += 1;
      const mine = seq.current;
      setState((held) => ({ ...held, busy: true, error: "" }));
      try {
        const result = await query.sourceProbe({
          repoUrl: url,
          ...(credentialId.trim() === "" ? {} : { credentialId: credentialId.trim() }),
        });
        if (mine !== seq.current) return;
        setState({ reply: sourceProbeFromRow(result.rows()[0] ?? null), error: "", busy: false });
      } catch (err) {
        if (mine !== seq.current) return;
        setState({ reply: null, error: messageOf(err), busy: false });
      }
    },
    [connection, clear],
  );

  return { ...state, probe, clear };
}

export interface ArtifactProbeHandle extends ProbeState<ArtifactProbeReply> {
  probe: (artifactId: string) => Promise<void>;
  clear: () => void;
}

export function useArtifactProbe(): ArtifactProbeHandle {
  const connection = useOsConnection();
  const [state, setState] = useState<ProbeState<ArtifactProbeReply>>(IDLE);
  const seq = useRef(0);

  const clear = useCallback(() => {
    seq.current += 1;
    setState(IDLE);
  }, []);

  const probe = useCallback(
    async (artifactId: string) => {
      const query = connection?.query ?? null;
      const id = artifactId.trim();
      if (id === "") {
        clear();
        return;
      }
      if (query === null) {
        setState({ reply: null, error: "Not connected to the cluster, so the zip was not opened.", busy: false });
        return;
      }
      seq.current += 1;
      const mine = seq.current;
      setState({ reply: null, error: "", busy: true });
      try {
        const result = await query.artifactProbe({ artifactId: id });
        if (mine !== seq.current) return;
        setState({ reply: artifactProbeFromRow(result.rows()[0] ?? null), error: "", busy: false });
      } catch (err) {
        if (mine !== seq.current) return;
        setState({ reply: null, error: messageOf(err), busy: false });
      }
    },
    [connection, clear],
  );

  return { ...state, probe, clear };
}

/**
 * A missing row is a reply this build cannot read, and it answers with an
 * UNKNOWN reason rather than with `ok`: the stop then says nothing and leaves
 * Analyze reachable, which is the fail-open posture design H asks for.
 */
function sourceProbeFromRow(raw: Row | null): SourceProbeReply {
  const row = raw === null ? ({} as Row) : flatten(raw);
  return {
    host: rowString(row, "host"),
    reachable: boolOr(row, "reachable", false),
    private: boolOr(row, "private", false),
    defaultBranch: rowString(row, "defaultBranch"),
    reason: rowString(row, "reason"),
    // THE TWO KEYS A GRANT MAKES ANSWERABLE (epic memql#4915). Both are
    // always present on the wire and empty when there is nothing to say, so
    // there is no absent-versus-empty question to get wrong -- but they are
    // read through the tolerant readers anyway, because a probe answered by
    // an older engine carries neither and a pasted-token probe answers both
    // empty. An empty answer is a stop with a text ref field and no preview,
    // which is exactly what this surface did before either key existed.
    branches: branchNamesFrom(row["branches"]),
    manifest: raw === null ? { ...EMPTY_MANIFEST } : manifestFrom(row["manifest"]),
  };
}

function artifactProbeFromRow(raw: Row | null): ArtifactProbeReply {
  const row = raw === null ? ({} as Row) : flatten(raw);
  return {
    isPackage: boolOr(row, "isPackage", false),
    isBuiltSite: boolOr(row, "isBuiltSite", false),
    fileCount: rowNumber(row, "fileCount"),
    totalBytes: rowNumber(row, "totalBytes"),
  };
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
