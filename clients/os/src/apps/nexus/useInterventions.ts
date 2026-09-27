import { useCallback, useEffect, useMemo, useState } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { CONSTRUCT_CONCEPT } from "./concepts";
import {
  feedbackFromObservation,
  validatorFromObservation,
  type FeedbackEntry,
  type ValidatorEntry,
} from "./feedback";
import { idTail, observationFromRow } from "./rows";
import { stepVersionFromRow, type StepVersion } from "./versions";

// The reads behind stepping into a run (epic memql#5414). None of them is a
// feed, and each says so by holding what it last read and when it failed.
//
// ===========================================================================
// A READ THAT FOLLOWS A FEED IS STILL A READ
// ===========================================================================
// `workStepVersions` is a builtin and `v1:work:observation` does not
// broadcast, so neither can be subscribed to. The versions read is taken
// again when the live feeds say something it answers has changed -- a new
// version appeared, or the head moved (`versionsSignature`) -- and never on a
// status flip, which the steps feed already carries for the one version it
// holds. What was read stays on screen while the next read runs: a refresh
// that blanked the version picker to a skeleton would be the surface
// replacing usable content because it was asked to be more current.

export interface VersionsRead {
  versions: StepVersion[];
  state: "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  retry: () => void;
}

/**
 * Every version of every step of one run.
 *
 * READ ON OPEN, unlike the journal. The spine draws a step's versions beside
 * its node, so the page needs them before anybody asks -- and this is one
 * builtin answering one run's step rows, not the journal's burst of one row
 * per model request.
 */
export function useStepVersions(runId: string, signature: string): VersionsRead {
  const connection = useOsConnection();
  const [versions, setVersions] = useState<StepVersion[]>([]);
  const [state, setState] = useState<VersionsRead["state"]>("loading");
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  const retry = useCallback(() => setNonce((n) => n + 1), []);

  // A different run is a different answer. What the previous run read is not
  // this run's, so it is dropped rather than shown under the new heading.
  useEffect(() => {
    setVersions([]);
    setState("loading");
    setError("");
  }, [runId]);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || runId.trim() === "") {
      setState("error");
      setError("Not connected to the cluster.");
      return;
    }
    const controller = new AbortController();
    void (async () => {
      try {
        const result = await query.workStepVersions({ runId }, { signal: controller.signal });
        if (controller.signal.aborted) return;
        setVersions(result.rows().map(stepVersionFromRow).filter((v) => v.key !== ""));
        setState("ready");
        setError("");
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => controller.abort();
  }, [connection, runId, signature, nonce]);

  return { versions, state, error, retry };
}

export interface VerdictsRead {
  feedback: FeedbackEntry[];
  validator: ValidatorEntry[];
  state: "loading" | "ready" | "error";
  error: string;
  /** Add a verdict the server just CONFIRMED -- the write's own reply -- so it shows without a re-read. */
  add: (entry: FeedbackEntry) => void;
}

/**
 * A person's verdicts and the validator's, for one run.
 *
 * `refresh` is what re-reads it: the run page passes the run's status and the
 * validator's observation id, so a run that finishes -- and the validator that
 * checks it a moment later -- is read again without anybody asking. A verdict
 * this window writes is added from the write's reply instead (see `add`).
 */
export function useRunVerdicts(runId: string, refresh: string): VerdictsRead {
  const connection = useOsConnection();
  const [feedback, setFeedback] = useState<FeedbackEntry[]>([]);
  const [validator, setValidator] = useState<ValidatorEntry[]>([]);
  const [confirmed, setConfirmed] = useState<FeedbackEntry[]>([]);
  const [state, setState] = useState<VerdictsRead["state"]>("loading");
  const [error, setError] = useState("");

  useEffect(() => {
    setFeedback([]);
    setValidator([]);
    setConfirmed([]);
    setState("loading");
    setError("");
  }, [runId]);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || runId.trim() === "") {
      setState("error");
      setError("Not connected to the cluster.");
      return;
    }
    const controller = new AbortController();
    void (async () => {
      try {
        const result = await query.workObservationsForOwnerRun({ runId }, { signal: controller.signal });
        if (controller.signal.aborted) return;
        const observations = result.rows().map(observationFromRow);
        const read = observations.map(feedbackFromObservation).filter((f): f is FeedbackEntry => f !== null);
        setFeedback(read);
        setValidator(observations.map(validatorFromObservation).filter((v): v is ValidatorEntry => v !== null));
        // A confirmed write the read now carries is the read's; one it does
        // not carry yet (the write landed after the read began) is kept.
        const ids = new Set(read.map((f) => f.id));
        setConfirmed((held) => held.filter((entry) => entry.id === "" || !ids.has(entry.id)));
        setState("ready");
        setError("");
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => controller.abort();
  }, [connection, runId, refresh]);

  const add = useCallback((entry: FeedbackEntry) => setConfirmed((held) => [...held, entry]), []);
  const all = useMemo(() => [...feedback, ...confirmed], [feedback, confirmed]);
  return { feedback: all, validator, state, error, add };
}

export interface SessionPromptRead {
  state: "idle" | "loading" | "ready" | "error";
  prompt: string;
  error: string;
}

const IDLE_PROMPT: SessionPromptRead = { state: "idle", prompt: "", error: "" };

/**
 * The prompt an app session ran a step with, for the composer to start from.
 *
 * `v1:worker:appSession.prompt` is the text the app was actually given, so a
 * person rewriting it is editing the real thing rather than a paraphrase.
 * The session is matched to the version being replaced by its recording
 * (`sessionRunId` is that version's `childRunId`); failing that, the newest
 * session of THIS run. Sessions are looked up by the step's row id, then by
 * its key for sessions written before the delegate stamped the real row --
 * filtered to this run either way, because a bare key is shared by every run
 * of the same template.
 */
export function useSessionPrompt(opts: {
  enabled: boolean;
  runId: string;
  stepId: string;
  stepKey: string;
  childRunId: string;
}): SessionPromptRead {
  const connection = useOsConnection();
  const [read, setRead] = useState<SessionPromptRead>(IDLE_PROMPT);
  const { enabled, runId, stepId, stepKey, childRunId } = opts;

  useEffect(() => {
    if (!enabled) {
      setRead(IDLE_PROMPT);
      return;
    }
    const query = connection?.query ?? null;
    if (query === null) {
      setRead({ state: "error", prompt: "", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    setRead({ state: "loading", prompt: "", error: "" });
    void (async () => {
      try {
        const mine = (rows: Row[]) =>
          rows.map(flatten).filter((row) => idTail(rowString(row, "runId")) === idTail(runId));
        let sessions = stepId === "" ? [] : mine((await query.appSessionsForStep({ stepId }, { signal: controller.signal })).rows());
        if (sessions.length === 0 && stepKey !== "" && stepKey !== stepId) {
          sessions = mine((await query.appSessionsForStep({ stepId: stepKey }, { signal: controller.signal })).rows());
        }
        if (controller.signal.aborted) return;
        const matched =
          (childRunId === ""
            ? undefined
            : sessions.find((row) => idTail(rowString(row, "sessionRunId")) === idTail(childRunId))) ?? sessions[0];
        setRead({ state: "ready", prompt: matched === undefined ? "" : rowString(matched, "prompt"), error: "" });
      } catch (err: unknown) {
        if (controller.signal.aborted) return;
        setRead({ state: "error", prompt: "", error: err instanceof Error ? err.message : String(err) });
      }
    })();
    return () => controller.abort();
  }, [connection, enabled, runId, stepId, stepKey, childRunId]);

  return read;
}

// ---------------------------------------------------------------------------
// The automation library, for the Overview's reuse figures
// ---------------------------------------------------------------------------

/** The authored catalog read's own narrowing: `row.catalogued == true`. */
function isCatalogued(row: Row): boolean {
  return flatten(row)["catalogued"] === true;
}

/** The learned-procedure read's own narrowing: `row.targetNamespace == "procedure"`. */
function isLearnedProcedure(row: Row): boolean {
  return rowString(flatten(row), "targetNamespace") === "procedure";
}

export interface ReuseLibrary {
  catalog: LiveCollectionHandle<Row>;
  procedures: LiveCollectionHandle<Row>;
}

/**
 * The person's automations -- the authored catalog and the learned procedures
 * -- as two LIVE collections over `v1:authoring:construct`.
 *
 * LIVE BECAUSE THE CONCEPT BROADCASTS: `component/node/routing.go` forwards
 * `graph.node.*.v1:authoring:*` to every node, so a label the reuse sweep
 * writes on an agent arrives here without a refresh control (DESIGN.md rule 1
 * forbids one). Each collection re-applies its read's own narrowing to what
 * it folds (`inScope`), because a subscription is scoped by concept alone and
 * would otherwise fold every construct this person owns into both.
 *
 * HELD BY THE OVERVIEW, which exists only while it is on screen, so a window
 * on any other section subscribes to none of it.
 */
export function useReuseLibrary(): ReuseLibrary {
  const catalog = useLiveCollection<Row>("work:reuse:catalog", (connection) => ({
    concept: CONSTRUCT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.cataloguedConstructsForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CONSTRUCT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: isCatalogued,
    paged: false,
  }));
  const procedures = useLiveCollection<Row>("work:reuse:procedures", (connection) => ({
    concept: CONSTRUCT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.learnedProceduresForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CONSTRUCT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: isLearnedProcedure,
    paged: false,
  }));
  return { catalog, procedures };
}

/**
 * How many distinct goals make an automation reusable -- a VALUE on the seeded
 * `v1:work:feedbackPolicy:primary` row, never a constant here (the plan's
 * "values, not constants"). Null until read, and null when the read is
 * refused: a person below the reader rung is told the rule without the
 * number rather than a number this window guessed.
 */
export function useReusableAfter(): number | null {
  const connection = useOsConnection();
  const [value, setValue] = useState<number | null>(null);
  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) return;
    const controller = new AbortController();
    void (async () => {
      try {
        const result = await query.feedbackPolicyCurrent({}, { signal: controller.signal });
        if (controller.signal.aborted) return;
        const raw = flatten(result.rows()[0] ?? {})["reusableAfterSignatures"];
        setValue(typeof raw === "number" && Number.isFinite(raw) && raw >= 1 ? Math.round(raw) : null);
      } catch {
        if (!controller.signal.aborted) setValue(null);
      }
    })();
    return () => controller.abort();
  }, [connection]);
  return value;
}
