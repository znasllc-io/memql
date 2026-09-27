import { useEffect } from "react";

import { useBranchRun, useRerunStep, type BranchReply, type RerunReply } from "./actions";
import { Composer } from "./Composer";
import type { Axes } from "./feedback";
import { overridden, type Head, type StepRow } from "./rows";
import { kindCalledAModel } from "./words";
import { useSessionPrompt } from "./useInterventions";
import {
  BRANCH_CONSEQUENCE,
  askedFor,
  composerArgs,
  freshDraft,
  inputText,
  nextVersion,
  rerunConsequence,
  stepsAfter,
  type ComposerDraft,
  type StepVersion,
} from "./versions";

// The composer, wired: the reads it starts from, the draft it keeps, and the
// two writes it can make (epic memql#5414).
//
// ONE HOST FOR BOTH SURFACES THAT OPEN IT. The run page offers "Run again" and
// "Branch from here"; the goal view offers "Branch from here". Both are this,
// so the act keeps one name, one form and one consequence wherever it is
// offered -- a second wiring would be a second answer to "what does Branch
// from here send".
//
// THE DRAFTS ARE THE CALLER'S. They outlive the dialog on purpose: Escape
// closes it and keeps what was typed, so the next "Run again" on the same step
// opens where the person left off. Only Cancel and a success throw one away.

export interface ComposerRequest {
  mode: "rerun" | "branch";
  stepKey: string;
  /**
   * Words to start the instructions with -- the validator's reason, from "Run
   * again with this". For a session step they are added after the recorded
   * prompt, once it has been read, rather than replacing it.
   */
  seed?: string;
}

export function draftKey(request: ComposerRequest): string {
  return `${request.mode}:${request.stepKey}`;
}

export function ComposerHost({
  request,
  runId,
  stepOrder,
  head,
  timelineKeys,
  step,
  versions,
  current,
  currentVersion = null,
  session,
  ownLevel,
  passedOn,
  drafts,
  onDrafts,
  onClose,
  onRerun,
  onBranched,
}: {
  request: ComposerRequest | null;
  runId: string;
  stepOrder: readonly string[];
  head: Head;
  /** The step keys in the order the timeline draws them: the fallback for "how many come after". */
  timelineKeys: readonly string[];
  /** The live row of the requested step. */
  step: StepRow | null;
  versions: readonly StepVersion[];
  current: number | null;
  /** The current version's row, when this page has it: what it was run with. */
  currentVersion?: StepRow | null;
  session: boolean;
  ownLevel: string;
  passedOn: { axes: Axes; reason: string } | null;
  drafts: Readonly<Record<string, ComposerDraft>>;
  onDrafts: (next: Record<string, ComposerDraft>) => void;
  onClose: () => void;
  onRerun: (reply: RerunReply) => void;
  onBranched: (reply: BranchReply) => void;
}) {
  const rerun = useRerunStep();
  const branch = useBranchRun();
  const prompt = useSessionPrompt({
    enabled: request !== null && session,
    runId,
    stepId: step?.id ?? "",
    stepKey: request?.stepKey ?? "",
    childRunId: step?.childRunId ?? "",
  });

  const key = request === null ? "" : draftKey(request);
  const draft: ComposerDraft | null =
    request === null ? null : (drafts[key] ?? freshDraft(step?.input ?? null, session));

  // A NEW REQUEST STARTS CLEAN: a refusal from the last time the composer was
  // open belongs to that attempt, not to this one.
  useEffect(() => {
    rerun.reset();
    branch.reset();
    if (request === null) return;
    // THE SEED, when "Run again with this" opened it: added to what is already
    // there rather than replacing it, and only once. A session step with no
    // draft yet waits for its recorded prompt below, so the seed lands after
    // the words the app was actually given.
    const seed = request.seed?.trim() ?? "";
    if (seed === "") return;
    const held = drafts[key] ?? (session ? null : freshDraft(step?.input ?? null, false));
    if (held === null || held.text === null || held.text.includes(seed)) return;
    onDrafts({ ...drafts, [key]: { ...held, text: held.text.trim() === "" ? seed : `${held.text}\n\n${seed}` } });
    // Keyed on the request itself -- a new act, or the same act on another step.
  }, [key, request?.seed]);

  // A SESSION STEP'S PROMPT, ONCE IT HAS BEEN READ. The field stays a skeleton
  // until then, so nobody types into a box the recorded prompt is about to
  // arrive in. A read that fails leaves the field empty to write the whole
  // prompt in, and says why.
  useEffect(() => {
    if (request === null || !session || draft === null || draft.text !== null) return;
    if (prompt.state !== "ready" && prompt.state !== "error") return;
    const seed = request.seed?.trim() ?? "";
    const base = prompt.state === "ready" ? prompt.prompt : "";
    const text = seed === "" ? base : base === "" ? seed : `${base}\n\n${seed}`;
    onDrafts({ ...drafts, [key]: { ...draft, text } });
  }, [request, session, prompt.state, prompt.prompt, draft?.text]);

  if (request === null || draft === null) return null;

  const modelled = step === null || kindCalledAModel(step.kind) !== false || step.childRunId !== "";
  const busy = request.mode === "rerun" ? rerun.busy : branch.busy;
  const error = request.mode === "rerun" ? rerun.error : branch.error;
  const baseline = { session, prompt: prompt.state === "ready" ? prompt.prompt : "" };
  const consequence =
    request.mode === "rerun"
      ? rerunConsequence(
          nextVersion(versions, head, step, request.stepKey),
          stepsAfter(stepOrder, timelineKeys, request.stepKey),
        )
      : BRANCH_CONSEQUENCE;

  // WHAT THE VERSION BEING REPLACED WAS RUN WITH. An override is one
  // version's and never carries, so a person who changed the level last time
  // and presses Run again would otherwise be surprised to find it gone -- the
  // form says so, and one click starts from it.
  const override = currentVersion?.override ?? null;
  // Only what the form can hold: a dislike that rode along as guidance is not
  // a field, and the server attaches the newest one on its own.
  const copyable =
    override !== null &&
    overridden(override) &&
    (override.level !== "" || override.model !== "" || override.effort !== "" || override.prompt !== "" || Object.keys(override.inputs).length > 0);
  const carried =
    override === null || !copyable
      ? null
      : {
          summary: [
            askedFor(override),
            override.prompt === "" ? "" : session ? "its own prompt" : "instructions",
            Object.keys(override.inputs).length === 0
              ? ""
              : `${Object.keys(override.inputs).length} ${Object.keys(override.inputs).length === 1 ? "input" : "inputs"}`,
          ]
            .filter((part) => part !== "")
            .join(", "),
          onApply: () => {
            const inputs = draft.inputs.map((row) =>
              Object.prototype.hasOwnProperty.call(override.inputs, row.key)
                ? { ...row, value: inputText(override.inputs[row.key]) }
                : row,
            );
            for (const [inputKey, value] of Object.entries(override.inputs)) {
              if (inputs.some((row) => row.key === inputKey)) continue;
              inputs.push({ id: `carried:${inputKey}`, key: inputKey, value: inputText(value), original: null });
            }
            onDrafts({
              ...drafts,
              [key]: {
                ...draft,
                level: override.level || draft.level,
                model: override.model || draft.model,
                effort: override.effort || draft.effort,
                text: override.prompt === "" ? draft.text : override.prompt,
                inputs,
              },
            });
          },
        };

  function forget(): void {
    const next = { ...drafts };
    delete next[key];
    onDrafts(next);
  }

  async function submit(): Promise<void> {
    if (request === null || draft === null) return;
    const args = { runId, stepKey: request.stepKey, ...composerArgs(draft, baseline) };
    if (request.mode === "rerun") {
      const reply = await rerun.act(args);
      if (reply === null) return;
      forget();
      onRerun(reply);
    } else {
      const reply = await branch.act(args);
      if (reply === null) return;
      forget();
      onBranched(reply);
    }
  }

  return (
    <Composer
      mode={request.mode}
      stepKey={request.stepKey}
      modelled={modelled}
      fromVersion={current}
      consequence={consequence}
      baseline={{ session, state: prompt.state, prompt: prompt.prompt, error: prompt.error }}
      ownLevel={ownLevel}
      passedOn={passedOn}
      carried={carried}
      draft={draft}
      onDraft={(next) => onDrafts({ ...drafts, [key]: next })}
      busy={busy}
      error={error}
      onDismiss={onClose}
      onCancel={() => {
        forget();
        onClose();
      }}
      onSubmit={() => void submit()}
    />
  );
}
