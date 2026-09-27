import type { ReactNode } from "react";
import { Minus, ShieldAlert, ShieldCheck, ThumbsDown, ThumbsUp } from "lucide-react";

import { Button, Caption, InlineSkeleton, Notice } from "../../kit";
import { InfoDetail } from "../../kit/InfoDetail";
import {
  anyAxis,
  dislikeSummary,
  validatorSentence,
  type Axes,
  type FeedbackEntry,
  type ValidatorEntry,
} from "./feedback";
import { AXES, VERDICTS, axisWord, verdictWord, type Axis, type Verdict } from "./words";

// A person's verdict, and the validator's beside it (epic memql#5414, D21-D22).
//
// ===========================================================================
// SAVED IS SAVED -- NOTHING SHOWS SELECTED UNTIL THE SERVER SAID SO
// ===========================================================================
// Like and Neutral write on click; the pill turns pressed when the write's
// reply lands, never on the click (SUPERVISED-VISUAL-COMPOSITION.md: success
// appears only after the operation succeeds). Dislike asks first: the
// framework's point is that a dislike names WHAT was wrong, the server refuses
// one that names nothing, and so `Save` is ABSENT until an axis is chosen --
// with the words beside it saying what is missing, which is the wizard floor's
// rule for an act that is not legal yet.
//
// A LATER VERDICT IS A NEW ROW. Changing one's mind writes again, and the
// earlier verdict stays in the journal as the history it is. Pressing the
// verdict already saved writes nothing -- there is nothing new to say.
//
// ===========================================================================
// PRESSED IS NOT OPEN
// ===========================================================================
// An open dislike question is a PROPOSAL, and it must not look like a saved
// dislike: the pill is pressed only for what was saved, and the open question
// is carried by `aria-expanded` and a dashed edge -- two different states,
// neither of them drawn like selection or like keyboard focus.
//
// ===========================================================================
// TWO PARTS, BECAUSE THEY SIT IN TWO PLACES
// ===========================================================================
// The pills sit on the version's own heading line, where a person opening a
// step finds them without scrolling past the evidence; what a dislike asks,
// what was said last time and what the validator found sit under that line.
// They share one set of props, so the two halves cannot disagree about what
// is saved or what is being asked.

export interface DislikeDraft {
  axes: Axes;
  reason: string;
}

export interface VerdictProps {
  /** A DOM-safe id for this target, unique on the page: it names the open question. */
  id: string;
  /** "Your verdict", or "Your verdict on this run". */
  label: string;
  /** What a dislike's reason does next, in words: it differs between a step and a run. */
  scope: "step" | "run";
  /** The newest verdict the server confirmed for this target, or null. */
  saved: FeedbackEntry | null;
  /** Whether the earlier verdicts have been read yet. */
  state: "loading" | "ready" | "error";
  /** The read's refusal, shown once -- by the run's own control. */
  readError?: string;
  draft: DislikeDraft | null;
  onDraft: (next: DislikeDraft | null) => void;
  /** A write for THIS target is in flight. */
  busy: boolean;
  /** The server's refusal for this target, verbatim. */
  error: string;
  onRecord: (verdict: Verdict, axes?: Axes, reason?: string) => void;
}

const ICONS: Record<Verdict, ReactNode> = {
  like: <ThumbsUp size={13} aria-hidden />,
  dislike: <ThumbsDown size={13} aria-hidden />,
  neutral: <Minus size={13} aria-hidden />,
};

/** The label and the three pills. */
export function VerdictChoices({ id, label, saved, state, draft, onDraft, busy, onRecord }: VerdictProps) {
  const pressed = saved?.verdict ?? null;
  const questionId = `${id}-question`;

  function choose(verdict: Verdict): void {
    if (busy) return;
    if (verdict === "dislike") {
      if (draft !== null) {
        onDraft(null);
        return;
      }
      // Open the question from what was said last time when that was a
      // dislike -- changing the reason is the common reason to come back.
      onDraft(
        saved?.verdict === "dislike"
          ? { axes: { ...saved.axes }, reason: saved.reason }
          : { axes: { product: false, process: false, performance: false }, reason: "" },
      );
      return;
    }
    onDraft(null);
    if (pressed === verdict) return;
    onRecord(verdict);
  }

  return (
    <div className="os-nexus-verdict-line" role="group" aria-label={label}>
      <span className="os-nexus-verdict-label" aria-hidden>
        {label}
      </span>
      {state === "loading" ? (
        <InlineSkeleton label="Loading your earlier verdict" />
      ) : (
        <span className="os-nexus-verdict-choices">
          {VERDICTS.map((verdict) => (
            <button
              key={verdict}
              type="button"
              className="os-choice os-nexus-verdict-choice"
              data-verdict={verdict}
              aria-pressed={pressed === verdict}
              aria-expanded={verdict === "dislike" ? draft !== null : undefined}
              aria-controls={verdict === "dislike" && draft !== null ? questionId : undefined}
              data-open={(verdict === "dislike" && draft !== null) || undefined}
              disabled={busy}
              aria-busy={busy || undefined}
              onClick={() => choose(verdict)}
            >
              {ICONS[verdict]}
              {verdictWord(verdict)}
            </button>
          ))}
        </span>
      )}
    </div>
  );
}

/**
 * What sits under the pills: the dislike question while it is open, what was
 * said last time, a refusal, and the validator's line (`children`).
 * Renders nothing when there is nothing to say.
 */
export function VerdictBelow({
  id,
  scope,
  saved,
  readError = "",
  draft,
  onDraft,
  busy,
  error,
  onRecord,
  children,
}: VerdictProps & { children?: ReactNode }) {
  const summary = draft === null ? dislikeSummary(saved) : "";
  const reasonId = `${id}-reason`;

  function toggle(axis: Axis): void {
    if (draft === null) return;
    onDraft({ ...draft, axes: { ...draft.axes, [axis]: !draft.axes[axis] } });
  }

  if (summary === "" && draft === null && error === "" && readError === "" && !children) return null;

  return (
    <div className="os-nexus-verdict">
      {summary === "" ? null : <p className="os-nexus-verdict-said">{summary}</p>}

      {draft === null ? null : (
        <div id={`${id}-question`} className="os-nexus-dislike" role="group" aria-label="What was wrong?">
          <div className="os-nexus-dislike-ask">
            <span>What was wrong?</span>
            <InfoDetail title="What was wrong?">
              <p>
                These are the three things the AI Fluency framework asks you to judge in work an AI
                did. Naming the one that was wrong tells the next attempt where to look.
              </p>
              <p>
                <strong>The result</strong> -- what it produced: whether it is right, complete and
                usable. The framework calls this the product.
              </p>
              <p>
                <strong>The approach</strong> -- how it went about the work: the steps it took and the
                reasoning behind them. The framework calls this the process.
              </p>
              <p>
                <strong>The behaviour</strong> -- how it acted while it worked: its tone, its pace and
                whether it followed your instructions. The framework calls this the performance.
              </p>
            </InfoDetail>
          </div>
          <div className="os-nexus-dislike-axes">
            {AXES.map((axis) => (
              <button
                key={axis}
                type="button"
                className="os-choice os-nexus-axis"
                aria-pressed={draft.axes[axis]}
                disabled={busy}
                onClick={() => toggle(axis)}
              >
                {axisWord(axis)}
              </button>
            ))}
          </div>
          <div className="os-form-field">
            <label className="os-form-field-label" htmlFor={reasonId}>
              Why?
            </label>
            <textarea
              id={reasonId}
              className="os-nexus-statement os-nexus-why"
              rows={2}
              value={draft.reason}
              placeholder="Optional"
              disabled={busy}
              onChange={(event) => onDraft({ ...draft, reason: event.target.value })}
            />
          </div>
          <div className="os-nexus-dislike-floor">
            <span className="os-nexus-dislike-state">
              {anyAxis(draft.axes)
                ? scope === "step"
                  ? "It goes with this step when it runs again."
                  : "It steers the next run of this goal."
                : "Choose what was wrong"}
            </span>
            <span className="os-nexus-dislike-acts">
              <button type="button" className="os-actbar-text" disabled={busy} onClick={() => onDraft(null)}>
                Cancel
              </button>
              {anyAxis(draft.axes) ? (
                <Button tone="primary" busy={busy} onClick={() => onRecord("dislike", draft.axes, draft.reason.trim())}>
                  Save
                </Button>
              ) : null}
            </span>
          </div>
        </div>
      )}

      {error === "" ? null : (
        <Notice tone="error" sentence="That verdict was not saved." next="Nothing you chose was lost." detail={error} />
      )}
      {readError === "" ? null : <Caption>Your earlier verdicts could not be read: {readError}</Caption>}
      {children}
    </div>
  );
}

/**
 * The validator's one quiet line.
 *
 * AN ICON AND WORDS, never a colour alone: the shield says "a check ran" and
 * its two shapes say which way it came out; the sentence says the same thing
 * for anybody the shapes do not reach. The act that follows a flag is part of
 * the sentence, so it reads as what to do about it.
 */
export function ValidatorLine({
  verdict,
  entry,
  stepKey = "",
  disagreesWithYou,
  onRunAgain,
}: {
  verdict: string;
  entry: ValidatorEntry | null;
  /** Named when the line stands away from the step it checked -- on the run's own verdict. */
  stepKey?: string;
  disagreesWithYou: boolean;
  /** Offered while a flagged answer can still be run again. */
  onRunAgain?: () => void;
}) {
  const flagged = verdict !== "pass";
  return (
    <p className="os-nexus-validator" data-verdict={flagged ? "flag" : "pass"}>
      {flagged ? <ShieldAlert size={13} aria-hidden /> : <ShieldCheck size={13} aria-hidden />}
      <span className="os-nexus-validator-words">
        {validatorSentence(verdict, entry, stepKey)}
        {disagreesWithYou ? <strong className="os-nexus-validator-disagrees"> It disagrees with you.</strong> : null}
        {flagged && onRunAgain ? (
          <>
            {" "}
            <button type="button" className="os-nexus-link os-nexus-validator-act" onClick={onRunAgain}>
              Run again with this
            </button>
          </>
        ) : null}
      </span>
    </p>
  );
}
