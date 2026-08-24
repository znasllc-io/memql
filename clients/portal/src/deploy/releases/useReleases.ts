import { useCallback, useEffect, useState } from "react";
import type { Result, Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../../cluster/ClusterProvider";
import { useMyAccess } from "../../cluster/useMyAccess";

// The Releases card's state: the cut history, the cut itself, and the
// per-version image check.
//
// ===========================================================================
// SETUP IS A STATE, NOT AN ERROR
// ===========================================================================
// Two refusals -- release_repo_unconfigured and credential_unavailable -- mean
// this installation has not been told what to cut or with what. They are true
// sentences about how the instance is set up, not faults, and the card renders
// the step to take INSTEAD of the button. Everything else is an error.
//
// The setup state is discovered by asking: a `dryRun` on mount, which computes
// the plan and creates nothing. That doubles as the card's headline -- it is
// the only way to learn what the NEXT version would be, and it answers from
// GitHub's tags, which is the source of truth for "newest" (see below).
//
// ===========================================================================
// TAGS ARE THE TRUTH FOR "NEWEST"; ROWS ARE HISTORY
// ===========================================================================
// release.sh's break-glass path still lets a human cut by hand, and that tag
// reaches no row here. A card that read "newest" off the newest ROW would name
// a superseded version confidently. So the headline comes from the dry run
// (which reads tags) and the list comes from `releaseCuts` (which is this
// cluster's own history) -- and the card says which is which, because an
// operator who cut by hand needs to understand why their release is absent
// from the list below.

export type ReleaseSetup = "" | "release_repo_unconfigured" | "credential_unavailable";

export interface ReleasePlan {
  version: string;
  previousTag: string;
  baseSha: string;
  repository: string;
}

export interface ReleaseRow {
  id: string;
  version: string;
  bump: string;
  status: string;
  baseSha: string;
  releaseUrl: string;
  tagName: string;
  error: string;
  pinBumpPrUrl: string;
  pinBumpNote: string;
  requestedByEmail: string;
  dispatchedAt: string;
  checkedAt: string;
}

export interface ImageDetail {
  repository: string;
  present: boolean;
}

// CheckResult is deliberately three-valued, mirroring the builtin. `error`
// non-empty means the STATUS is the row's previous value rather than a fresh
// verdict, and the card says so in those words -- collapsing it into "absent"
// here would undo the whole point of the honest-error rule on the server.
export interface CheckResult {
  version: string;
  status: string;
  images: ImageDetail[];
  age: string;
  error: string;
}

export interface ReleasesState {
  // Whether the cluster resolved this connection as an owner. Decides what
  // the card OFFERS; the engine decides what it allows.
  isOwner: boolean;
  accessResolved: boolean;

  setup: ReleaseSetup;
  plan: ReleasePlan | null;
  rows: ReleaseRow[];
  loading: boolean;
  error: string;

  busy: boolean;
  actionError: string;
  lastCut: string;

  cut: (input: CutInput) => Promise<boolean>;
  check: (version: string) => Promise<void>;
  checks: Record<string, CheckResult>;
  checking: string;
  reload: () => void;
}

export interface CutInput {
  bump: "major" | "minor" | "patch";
  notes: string;
  bumpExtensionPin: boolean;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// setupFrom reads a refusal code out of an error message.
//
// MATCHED ON THE CODE, never on prose. The codes are stable identifiers the
// server treats as a contract (integrations/release/refusals.go says so in as
// many words); the messages around them are free to be reworded, and a card
// that branched on wording would break silently the first time one was.
function setupFrom(message: string): ReleaseSetup {
  if (message.includes("release_repo_unconfigured")) return "release_repo_unconfigured";
  if (message.includes("credential_unavailable")) return "credential_unavailable";
  return "";
}

// firstRow reads the single synthetic node a builtin returns.
//
// A builtin's answer is ONE node whose payload is the whole result -- the
// integration:release:result convention, never persisted. So there is no
// pagination to consider here and no second row to miss.
function firstRow(result: Result): Record<string, unknown> | null {
  const row = result.rows()[0];
  return row === undefined ? null : (row as Record<string, unknown>);
}

function str(row: Record<string, unknown> | null, key: string): string {
  const v = row?.[key];
  return typeof v === "string" ? v : "";
}

function bool(row: Record<string, unknown> | null, key: string): boolean {
  return row?.[key] === true;
}

export function useReleases(): ReleasesState {
  const { query } = useCluster();
  const { access, loading: accessLoading } = useMyAccess();
  const isOwner = access?.clusterRole === "owner";
  const accessResolved = !accessLoading && access !== null;

  const [setup, setSetup] = useState<ReleaseSetup>("");
  const [plan, setPlan] = useState<ReleasePlan | null>(null);
  const [rows, setRows] = useState<ReleaseRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState("");
  const [lastCut, setLastCut] = useState("");
  const [checks, setChecks] = useState<Record<string, CheckResult>>({});
  const [checking, setChecking] = useState("");
  const [reloadToken, setReloadToken] = useState(0);

  const reload = useCallback(() => setReloadToken((n) => n + 1), []);

  useEffect(() => {
    // NOT ISSUED FOR A NON-OWNER. Both calls refuse for anyone else, and
    // firing them anyway would fill the console with refusals for a card
    // that is not being rendered -- which trains an operator to ignore
    // exactly the errors that matter.
    if (query === null || !isOwner) {
      setPlan(null);
      setRows([]);
      setSetup("");
      setError("");
      return;
    }

    let live = true;
    setLoading(true);
    setError("");

    void (async () => {
      // The dry run first: it establishes the setup state, and until that
      // is known the history read's own failure cannot be interpreted.
      try {
        const result = await query.releaseCut({ bump: "patch", dryRun: true });
        if (!live) return;
        const row = firstRow(result);
        setSetup("");
        setPlan({
          version: str(row, "version"),
          previousTag: str(row, "previousTag"),
          baseSha: str(row, "baseSha"),
          repository: str(row, "repository"),
        });
      } catch (err) {
        if (!live) return;
        const message = describe(err);
        const state = setupFrom(message);
        setSetup(state);
        setPlan(null);
        // A setup state is not an error; anything else is.
        setError(state === "" ? message : "");
      }

      try {
        const result = await query.releaseCuts({});
        if (!live) return;
        setRows(result.rows().map(toReleaseRow));
      } catch (err) {
        if (!live) return;
        // Only surfaced when the setup is fine -- otherwise the operator
        // gets two messages about one cause.
        setError((prev) => (prev === "" && setupFrom(describe(err)) === "" ? describe(err) : prev));
      } finally {
        if (live) setLoading(false);
      }
    })();

    return () => {
      live = false;
    };
  }, [query, isOwner, reloadToken]);

  const cut = useCallback(
    async (input: CutInput): Promise<boolean> => {
      if (query === null) return false;
      setBusy(true);
      setActionError("");
      try {
        const result = await query.releaseCut({
          bump: input.bump,
          ...(input.notes.trim() === "" ? {} : { notes: input.notes.trim() }),
          ...(input.bumpExtensionPin ? { bumpExtensionPin: true } : {}),
        });
        const row = firstRow(result);
        setLastCut(str(row, "version"));
        reload();
        return true;
      } catch (err) {
        setActionError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [query, reload],
  );

  const check = useCallback(
    async (version: string): Promise<void> => {
      if (query === null) return;
      setChecking(version);
      try {
        const result = await query.releaseCutStatus({ version });
        const row = firstRow(result);
        const images = Array.isArray(row?.["images"]) ? (row["images"] as Record<string, unknown>[]) : [];
        setChecks((prev) => ({
          ...prev,
          [version]: {
            version,
            status: str(row, "status"),
            age: str(row, "age"),
            error: str(row, "checkError"),
            images: images.map((img) => ({
              repository: typeof img["repository"] === "string" ? (img["repository"] as string) : "",
              present: img["present"] === true,
            })),
          },
        }));
        // A check that moved the row is a change to the history the list
        // is showing, so the list is re-read rather than patched -- the
        // server decides the status, and a client-side guess about which
        // way it went is exactly what this feature refuses to make.
        reload();
      } catch (err) {
        setChecks((prev) => ({
          ...prev,
          [version]: { version, status: "", images: [], age: "", error: describe(err) },
        }));
      } finally {
        setChecking("");
      }
    },
    [query, reload],
  );

  return {
    isOwner,
    accessResolved,
    setup,
    plan,
    rows,
    loading,
    error,
    busy,
    actionError,
    lastCut,
    cut,
    check,
    checks,
    checking,
    reload,
  };
}

function toReleaseRow(row: Row): ReleaseRow {
  const r = row as unknown as Record<string, unknown>;
  return {
    id: typeof row.id === "string" ? row.id : "",
    version: str(r, "version"),
    bump: str(r, "bump"),
    status: str(r, "status"),
    baseSha: str(r, "baseSha"),
    releaseUrl: str(r, "releaseUrl"),
    tagName: str(r, "tagName"),
    error: str(r, "error"),
    pinBumpPrUrl: str(r, "pinBumpPrUrl"),
    pinBumpNote: str(r, "pinBumpNote"),
    requestedByEmail: str(r, "requestedByEmail"),
    dispatchedAt: str(r, "dispatchedAt"),
    checkedAt: str(r, "checkedAt"),
  };
}

// Exported for the tests, which assert the code-matching rule directly rather
// than through a rendered card -- the branch is a contract with the server and
// deserves to be pinned where it is written.
export { setupFrom, bool };
