import { useId, useRef, useEffect } from "react";
import { X } from "lucide-react";

import { Caption, ContentSkeleton, Dialog, Field, Input, Notice, Select } from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { AddButton } from "../../kit/AddButton";
import { IconButton } from "../../kit/IconButton";
import { InfoDetail } from "../../kit/InfoDetail";
import type { Axes } from "./feedback";
import type { ComposerDraft, InputRow } from "./versions";
import { OVERRIDE_EFFORTS, OVERRIDE_LEVELS, axesPhrase, effortWord, levelWord } from "./words";

// "Run again" and "Branch from here" -- ONE composer for both (epic memql#5414,
// design D19-D20).
//
// ===========================================================================
// ONE FORM, BECAUSE IT IS ONE QUESTION
// ===========================================================================
// A re-run makes a new version of a step in this run; a branch makes a new run
// that reuses everything before the step. What a person may change is the
// same set in both -- the level, the model, the effort, the prompt or the
// instructions, the inputs -- so it is the same form, titled with the act's
// own name, and the floor says which consequence this one has.
//
// ===========================================================================
// ONLY WHAT WAS CHANGED IS SENT
// ===========================================================================
// Every field starts at "the step's own", and sending a value the person did
// not touch would pin the version to whatever the form happened to show --
// and mark it as authored by somebody who wrote nothing (see
// `composerArgs`). The form therefore shows the step's own settings as the
// EMPTY choice rather than pre-selecting them.
//
// ===========================================================================
// THE FLOOR IS THE WIZARD'S
// ===========================================================================
// The consequence in words on the left; Cancel as a text act and the act as
// the ONE button on the right. The dialog closes only after the builtin
// answered success; a refusal is the server's sentence, pinned above the floor
// with every field as it was. Escape closes WITHOUT discarding -- the draft is
// kept for the next time this act is opened on this step -- and Cancel throws
// it away.

export interface ComposerBaselineRead {
  session: boolean;
  state: "idle" | "loading" | "ready" | "error";
  prompt: string;
  error: string;
}

export interface ComposerProps {
  mode: "rerun" | "branch";
  stepKey: string;
  /** The version the act starts from: the current one. */
  fromVersion: number | null;
  consequence: string;
  baseline: ComposerBaselineRead;
  /** The step's own level when this page knows it, named in the empty choice. */
  ownLevel: string;
  /** The newest dislike of the version being replaced: the server passes it on. */
  passedOn: { axes: Axes; reason: string } | null;
  /**
   * What the version being replaced was run WITH, when somebody changed it --
   * an override applies to one version and never carries to the next, so the
   * form says so and offers to start from it.
   */
  carried: { summary: string; onApply: () => void } | null;
  draft: ComposerDraft;
  onDraft: (next: ComposerDraft) => void;
  busy: boolean;
  error: string;
  /** Escape or a close request: nothing written, the draft kept. */
  onDismiss: () => void;
  /** Cancel: nothing written, the draft thrown away. */
  onCancel: () => void;
  onSubmit: () => void;
}

export function actName(mode: "rerun" | "branch"): string {
  return mode === "rerun" ? "Run again" : "Branch from here";
}

export function Composer({
  mode,
  stepKey,
  fromVersion,
  consequence,
  baseline,
  ownLevel,
  passedOn,
  carried,
  draft,
  onDraft,
  busy,
  error,
  onDismiss,
  onCancel,
  onSubmit,
}: ComposerProps) {
  const ids = useId();
  const refusal = useRef<HTMLDivElement>(null);
  const name = actName(mode);

  // THE REASON GOES WHERE THE PERSON IS LOOKING. The busy button took focus
  // with it; the refusal is pinned above the floor and takes focus, which
  // keeps a keyboard inside the dialog (the share dialog's rule).
  useEffect(() => {
    if (error !== "") refusal.current?.focus();
  }, [error]);

  const loadingPrompt = baseline.session && (baseline.state === "loading" || draft.text === null);
  const textLabel = baseline.session ? "Prompt" : "Instructions";

  function setInput(id: string, patch: Partial<InputRow>): void {
    onDraft({ ...draft, inputs: draft.inputs.map((row) => (row.id === id ? { ...row, ...patch } : row)) });
  }

  return (
    <Dialog
      title={name}
      subtitle={fromVersion === null ? stepKey : `${stepKey}, version ${fromVersion}`}
      onDismiss={onDismiss}
      busy={busy}
      className="os-dialog os-nexus-composer"
      // THE FIRST FIELD, not the first focusable thing: "Start from them" sits
      // above the fields and rewrites the form in one click.
      initialFocus={(dialog) => dialog.querySelector<HTMLElement>(".os-nexus-composer-pair .os-select")}
      floor={
        <>
          {error === "" ? null : (
            <div ref={refusal} tabIndex={-1} className="os-dialog-refusal">
              <Notice
                tone="error"
                sentence={mode === "rerun" ? "It was not run again." : "No branch was made."}
                next="Everything you changed is still here."
                detail={error}
              />
            </div>
          )}
          <ActionBar
            state=""
            detail={consequence}
            acts={[
              { label: "Cancel", text: true, busy, onAct: onCancel },
              { label: name, tone: "primary", busy, onAct: onSubmit },
            ]}
          />
        </>
      }
    >
      {carried === null ? null : (
        <p className="os-nexus-carried">
          {fromVersion === null ? "The version it replaces" : `Version ${fromVersion}`} ran with changes:{" "}
          {carried.summary}. {mode === "rerun" ? "A new version starts without them." : "The branch starts without them."}{" "}
          <button type="button" className="os-nexus-link" onClick={carried.onApply}>
            Start from them
          </button>
        </p>
      )}

      <div className="os-nexus-composer-pair">
        <Field label="Level">
          <Select
            id={`${ids}-level`}
            label="Level"
            value={draft.level}
            onChange={(level) => onDraft({ ...draft, level })}
          >
            <option value="">{ownLevel === "" ? "Its own" : `Its own (${levelWord(ownLevel).toLowerCase()})`}</option>
            {OVERRIDE_LEVELS.map((level) => (
              <option key={level} value={level}>
                {levelWord(level)}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Effort">
          <Select
            id={`${ids}-effort`}
            label="Effort"
            value={draft.effort}
            onChange={(effort) => onDraft({ ...draft, effort })}
          >
            <option value="">The level decides</option>
            {OVERRIDE_EFFORTS.map((effort) => (
              <option key={effort} value={effort}>
                {effortWord(effort)}
              </option>
            ))}
          </Select>
          <InfoDetail title="Effort">
            <p>
              How hard an app works on the step -- Claude Code or Codex, when one answers it. A step a
              model answers directly has no such setting and ignores it.
            </p>
            <p>Left alone, the level decides.</p>
          </InfoDetail>
        </Field>
      </div>

      <Field label="Model">
        <Input
          id={`${ids}-model`}
          label="Model"
          code
          value={draft.model}
          placeholder="The level decides"
          onChange={(model) => onDraft({ ...draft, model })}
        />
        <InfoDetail title="Model">
          <p>Pins one place to answer the step instead of letting the level choose. Any one of:</p>
          <p>
            <code>chat54Mini</code> -- a provider by its name.
            <br />
            <code>fleet:qwen3:8b</code> -- a model on your own machines.
            <br />
            <code>app:claude-code</code> -- an app, with the model it chooses.
            <br />
            <code>app:claude-code:opus</code> -- an app, and the model it should use.
          </p>
          <p>Left empty, the level decides.</p>
        </InfoDetail>
      </Field>

      <div className="os-form-field">
        <label className="os-form-field-label" htmlFor={`${ids}-text`}>
          {textLabel}
        </label>
        {loadingPrompt ? (
          <ContentSkeleton kind="form" label="Loading the prompt the session ran with" />
        ) : (
          <textarea
            id={`${ids}-text`}
            className="os-nexus-statement os-nexus-composer-text"
            rows={baseline.session ? 7 : 3}
            value={draft.text ?? ""}
            placeholder={baseline.session ? "" : "Added to the step's prompt, for this version only"}
            onChange={(event) => onDraft({ ...draft, text: event.target.value })}
          />
        )}
        {baseline.session && !loadingPrompt ? (
          <Caption>
            {baseline.state === "error"
              ? `The prompt it ran with could not be read: ${baseline.error}. Write the whole prompt the session should run with.`
              : "The whole prompt the session runs with."}
          </Caption>
        ) : null}
      </div>

      <div className="os-form-field os-nexus-inputs">
        <div className="os-nexus-inputs-head">
          <span className="os-form-field-label">Inputs</span>
          <InfoDetail title="Inputs">
            <p>Arguments to use instead of the step's own, by name. An input you leave alone keeps its value.</p>
            <p>
              A value is read as JSON when it looks like JSON -- <code>42</code>, <code>true</code>,{" "}
              <code>["a", "b"]</code> -- and as text otherwise.
            </p>
          </InfoDetail>
          <AddButton
            label="Add an input"
            onClick={() =>
              onDraft({
                ...draft,
                inputs: [...draft.inputs, { id: `added:${Date.now()}:${draft.inputs.length}`, key: "", value: "", original: null }],
              })
            }
          />
        </div>
        {draft.inputs.length === 0 ? null : (
          <ul className="os-nexus-input-rows" aria-label="Inputs">
            {draft.inputs.map((row, index) => (
              <li key={row.id} className="os-nexus-input-row">
                {row.original !== null ? (
                  <span className="os-nexus-input-key os-mono" title={row.key}>
                    {row.key}
                  </span>
                ) : (
                  <Input
                    id={`${ids}-input-key-${index}`}
                    label="Input name"
                    code
                    value={row.key}
                    placeholder="name"
                    onChange={(key) => setInput(row.id, { key })}
                  />
                )}
                <Input
                  id={`${ids}-input-value-${index}`}
                  label={row.key.trim() === "" ? "Value of the new input" : `Value of ${row.key}`}
                  code
                  value={row.value}
                  placeholder="value"
                  onChange={(value) => setInput(row.id, { value })}
                />
                {row.original === null ? (
                  <IconButton
                    label={row.key.trim() === "" ? "Remove the new input" : `Remove ${row.key}`}
                    onClick={() => onDraft({ ...draft, inputs: draft.inputs.filter((held) => held.id !== row.id) })}
                  >
                    <X size={14} aria-hidden />
                  </IconButton>
                ) : (
                  <span className="os-nexus-input-state">{row.value === row.original ? "" : "changed"}</span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      {passedOn === null ? null : (
        <div className="os-form-field">
          <span className="os-form-field-label">What will be passed on</span>
          <p className="os-nexus-passed-on">
            {axesPhrase(passedOn.axes) === "" ? "What was wrong" : `What was wrong with ${axesPhrase(passedOn.axes)}`}
            {passedOn.reason.trim() === "" ? "." : `: ${passedOn.reason.trim()}`}
          </p>
        </div>
      )}
    </Dialog>
  );
}
