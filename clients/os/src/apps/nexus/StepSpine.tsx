import type { ReactNode } from "react";
import { ChevronRight } from "lucide-react";

import { Chip, formatDuration } from "../../kit";
import {
  decisionLine,
  formatMoney,
  formatTokens,
  stepCallLine,
  stepThought,
  type StepDecision,
  type StepRow,
} from "./rows";
import {
  stepKindMeaning,
  stepKindWord,
  stepStatusWord,
  symptomMeaning,
  symptomWord,
} from "./words";

// THE SPINE: a run's steps in order, with the thinking drawn in ink.
//
// ===========================================================================
// THE ONE DEVICE THIS APP HAS THAT NO OTHER APP HAS
// ===========================================================================
// The product's whole claim is that the system works a goal out ONCE and
// replays it afterwards without a model unless reasoning is genuinely needed.
// A person cannot check that claim from a table of forty-seven rows that all
// look alike -- they have to read every row to find the three that cost
// something.
//
// So the run has a spine down its left edge, and the spine's WEIGHT is the
// claim. A deterministic step is a hollow node on a hairline: a hundred of
// them read as texture rather than as a hundred things to read. A reasoning
// step is a filled node on a thick segment of solid ink. Scanning a long run,
// the eye finds where the machine thought before a word has been read -- the
// run looks like what it is, a thin grey thread with a few dense knots in it.
//
// The cost readout obeys the same rule: only a step that thought shows tokens
// and money, so the visual weight and the bill are in the same places.
//
// ===========================================================================
// A STEPPED RAIL IS HONEST HERE, WHICH IS UNUSUAL
// ===========================================================================
// The most over-used structure in software design is a numbered sequence
// applied to content that is not one. This content IS one: `seq` is the
// template's own execution order and `dependsOn` is a real edge, so the order
// carries information -- "it failed at step 3 of 47" is a different situation
// from "it failed at step 46". The Deployables rail earns its shape the same
// way, from a pipeline order the engine enforces.
//
// The number is the SEQUENCE POSITION and not a decoration: it is what the
// run's `stepOrder`, a fork's `atStepKey` and every error message name.
//
// ===========================================================================
// COLOUR IS NEVER THE ONLY CARRIER
// ===========================================================================
// The kind is a word on the row and in the accessible name; the status is a
// word too. The ink/hairline contrast is a second channel, not the only one,
// so the timeline survives greyscale and reads the same under every theme
// pack -- a pack carries colour, and this contrast does not use colour to mean
// anything.

/**
 * A step's versions, as the spine draws them (epic memql#5414, D18).
 *
 * `count` is how many versions the page knows of and `current` which of them
 * the run's head points at. A step with one version draws nothing: the whole
 * device is a way to find the few steps somebody stepped into.
 */
export interface SpineVersions {
  count: number;
  current: number | null;
}

/** More ticks than this read as a bar, not as versions; the count is in the name either way. */
const MAX_TICKS = 5;

export interface StepSpineRowProps {
  step: StepRow;
  /** Sequence position as drawn, 1-based. `seq` is 0-based on the row. */
  position: number;
  /** The last step draws no tail below its node. */
  last: boolean;
  open: boolean;
  onOpen: () => void;
  /** Omitted, or one version: no ticks. */
  versions?: SpineVersions;
  /**
   * Its current version was made from an upstream version that is no longer
   * current, so it runs again (`run.staleSteps`). Said in words on the row --
   * a state that is only a tint is a state half the readers never get.
   */
  stale?: boolean;
  /** The unseen-change marker for what opening this row reveals. */
  marker?: ReactNode;
  /**
   * Which door answered for this step, when the journal has been read.
   *
   * OPTIONAL, AND ABSENT IS THE ORDINARY CASE rather than a gap to fill in.
   * Most steps are deterministic and never call a model, and the journal is an
   * on-demand read (`useJournal` deliberately does not read on open), so a
   * caller that has not read it hands nothing here -- as `GoalView` does. Both
   * of those render no decision line at all, which is the same answer for two
   * different reasons and the right one for both: an empty slot on a step that
   * never called a model is a claim that something is missing.
   */
  decision?: StepDecision | null;
}

export function StepSpineRow({
  step,
  position,
  last,
  open,
  onOpen,
  decision = null,
  versions,
  stale = false,
  marker = null,
}: StepSpineRowProps) {
  const thought = stepThought(step);
  const kind = step.kind === "" ? "unclassified" : step.kind;
  const failed = step.status === "failed";
  const waiting = step.status === "waiting";

  // The decision is rendered from the SAME string the accessible name carries,
  // so the two cannot drift apart. A step with no model call contributes "",
  // which the filter below drops.
  const decided = decision === null ? "" : decisionLine(decision);

  const count = versions?.count ?? 1;
  const current = versions?.current ?? null;
  // THE ONE STATE THE TICKS ALONE CANNOT CARRY: somebody went back to an
  // earlier version, so the newest one on the row is not what the run uses.
  // Said in words beside the key. The ordinary case -- the newest is current --
  // needs no words; the ticks and the name carry it.
  const behind = count > 1 && current !== null && current < count;
  const versionWords =
    count <= 1 ? "" : current === null ? `${count} versions` : `${count} versions, version ${current} is current`;

  // The accessible name says everything the drawing says, in words. A reader
  // who cannot see the spine gets "step 3, reasoning, called a model, done".
  // That contract is why the decision is appended here and not only drawn: a
  // line about who was billed, visible to sighted readers only, is the whole
  // point of this epic withheld from half of them. The versions and the stale
  // mark ride on the same contract.
  const spoken = [
    `Step ${position}`,
    step.key,
    stepKindWord(step.kind),
    stepKindMeaning(step.kind),
    stepStatusWord(step.status),
    decided,
    versionWords,
    stale ? "runs again" : "",
  ]
    .filter((part) => part !== "")
    .join(", ");

  // Oldest first, newest last: the tally reads left to right, and past the
  // cap it shows the newest ones.
  const shown = Math.min(count, MAX_TICKS);
  const firstShown = count - shown + 1;

  return (
    <button
      type="button"
      className="os-nexus-step"
      data-kind={kind}
      data-thought={thought || undefined}
      data-status={step.status}
      data-open={open || undefined}
      aria-expanded={open}
      aria-label={spoken}
      onClick={onOpen}
    >
      <span className="os-nexus-step-spine" aria-hidden>
        <span className="os-nexus-step-line" data-head />
        <span className="os-nexus-step-knot">
          <span className="os-nexus-step-node" />
          {/* THE VERSIONS, ON THE SPINE ITSELF. A quiet stack of ticks beside
              the node, one per version, the current one inked -- the thread's
              own vocabulary of weight rather than hue, so it survives
              greyscale and every theme pack, and a run of forty steps shows
              the three somebody stepped into before a word is read. */}
          {count > 1 ? (
            <span className="os-nexus-step-ticks">
              {Array.from({ length: shown }, (_, index) => {
                const version = firstShown + index;
                return (
                  <span
                    key={version}
                    className="os-nexus-step-tick"
                    data-current={version === current || undefined}
                  />
                );
              })}
            </span>
          ) : null}
        </span>
        {last ? null : <span className="os-nexus-step-line" data-tail />}
      </span>

      <span className="os-nexus-step-seq" aria-hidden>
        {position}
      </span>

      <span className="os-nexus-step-body">
        <span className="os-nexus-step-name">
          <span className="os-nexus-step-key os-mono">{step.key}</span>
          {behind ? (
            <Chip tone="muted" title={`Version ${current} of ${count} is current; the newer ones are kept`}>
              version {current} of {count}
            </Chip>
          ) : null}
        </span>
        <span className="os-nexus-step-call">{stepCallLine(step)}</span>
        {/* WHICH DOOR ANSWERED, ON THE STEP THAT ASKED. It sits in the body
            slot with the symptom and the error, for the same reason they do:
            it is a sentence about this step, and a sentence in a column is a
            sentence nobody can read.

            A STEP THAT CALLED NO MODEL RENDERS NOTHING HERE -- not a dash, not
            an empty line. Most of a run is deterministic, and a dash on
            forty-four rows to say "no model was involved" is forty-four things
            to read past; the spine has already said which steps thought. */}
        {decided === "" ? null : (
          /* IT BORROWS THE CALL LINE'S TYPE rather than inventing a second
             one. Both are the same voice -- a quiet line under the step's own
             name, saying more about the same call -- and a third size in this
             row would be a size nobody chose. `os-nexus-step-decision` and
             `data-served` are the hooks for giving a door its own weight, so
             that decision can be made in the stylesheet: a fleet line and a
             billed line want to read differently. */
          <span
            className="os-nexus-step-call os-nexus-step-decision"
            data-served={decision?.served || undefined}
          >
            {decided}
          </span>
        )}
        {/* The symptom is the classifier's answer and belongs UNDER the step
            it is about, not in a column: it is a sentence, and a sentence in a
            column is a sentence nobody can read. */}
        {failed && step.symptom !== "" ? (
          <span className="os-nexus-step-symptom" title={symptomMeaning(step.symptom)}>
            {symptomWord(step.symptom)}
          </span>
        ) : null}
        {failed && step.errorMessage !== "" ? (
          <span className="os-nexus-step-error os-mono">{step.errorMessage}</span>
        ) : null}
      </span>

      <span className="os-nexus-step-state">
        {/* THE COST SITS ONLY ON THE STEPS THAT COST SOMETHING. A dash on
            forty-four rows to say "free" is forty-four things to read past;
            silence says it better, and the band above has already said how
            many of each there are. */}
        {thought ? (
          <span className="os-nexus-step-spend os-mono">
            {step.tokens === null ? null : <span>{formatTokens(step.tokens)} tok</span>}
            {step.cost === null ? null : <span>{formatMoney(step.cost)}</span>}
          </span>
        ) : null}
        <span className="os-nexus-step-kind">{stepKindWord(step.kind)}</span>
        <span className="os-nexus-step-duration os-mono">
          {step.durationMs === null ? "" : formatDuration(step.durationMs)}
        </span>
        <span className="os-nexus-step-status" data-status={step.status}>
          {stepStatusWord(step.status)}
        </span>
        {/* Postcondition: three answers, and the third is not "false".
            NULL means this step declares no postcondition, which epic A1
            leaves true of every step -- rendering that as "did not pass"
            would fail every run in the cluster on the strength of an absent
            field. */}
        {step.postconditionPassed === false ? (
          <Chip tone="neutral" title={step.postconditionMessage || "The postcondition did not hold."}>
            postcondition failed
          </Chip>
        ) : null}
        {waiting ? <Chip tone="accent">waiting</Chip> : null}
        {stale ? (
          <Chip tone="muted" title="What it was made from is no longer current, so it runs again">
            runs again
          </Chip>
        ) : null}
        <span className="os-nexus-step-open os-attention-anchor">
          <ChevronRight size={13} className="os-nexus-step-chevron" aria-hidden />
          {marker}
        </span>
      </span>
    </button>
  );
}
