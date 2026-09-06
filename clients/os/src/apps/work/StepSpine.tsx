import { ChevronRight } from "lucide-react";

import { Chip, formatDuration } from "../../kit";
import {
  formatMoney,
  formatTokens,
  stepCallLine,
  stepThought,
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

export interface StepSpineRowProps {
  step: StepRow;
  /** Sequence position as drawn, 1-based. `seq` is 0-based on the row. */
  position: number;
  /** The last step draws no tail below its node. */
  last: boolean;
  open: boolean;
  onOpen: () => void;
}

export function StepSpineRow({ step, position, last, open, onOpen }: StepSpineRowProps) {
  const thought = stepThought(step);
  const kind = step.kind === "" ? "unclassified" : step.kind;
  const failed = step.status === "failed";
  const waiting = step.status === "waiting";

  // The accessible name says everything the drawing says, in words. A reader
  // who cannot see the spine gets "step 3, reasoning, called a model, done".
  const spoken = [
    `Step ${position}`,
    step.key,
    stepKindWord(step.kind),
    stepKindMeaning(step.kind),
    stepStatusWord(step.status),
  ]
    .filter((part) => part !== "")
    .join(", ");

  return (
    <button
      type="button"
      className="os-work-step"
      data-kind={kind}
      data-thought={thought || undefined}
      data-status={step.status}
      data-open={open || undefined}
      aria-expanded={open}
      aria-label={spoken}
      onClick={onOpen}
    >
      <span className="os-work-step-spine" aria-hidden>
        <span className="os-work-step-line" data-head />
        <span className="os-work-step-node" />
        {last ? null : <span className="os-work-step-line" data-tail />}
      </span>

      <span className="os-work-step-seq" aria-hidden>
        {position}
      </span>

      <span className="os-work-step-body">
        <span className="os-work-step-name">
          <span className="os-work-step-key os-mono">{step.key}</span>
          {step.attempt > 1 ? (
            <Chip tone="muted" title={`This is attempt ${step.attempt} of this step`}>
              attempt {step.attempt}
            </Chip>
          ) : null}
        </span>
        <span className="os-work-step-call">{stepCallLine(step)}</span>
        {/* The symptom is the classifier's answer and belongs UNDER the step
            it is about, not in a column: it is a sentence, and a sentence in a
            column is a sentence nobody can read. */}
        {failed && step.symptom !== "" ? (
          <span className="os-work-step-symptom" title={symptomMeaning(step.symptom)}>
            {symptomWord(step.symptom)}
          </span>
        ) : null}
        {failed && step.errorMessage !== "" ? (
          <span className="os-work-step-error os-mono">{step.errorMessage}</span>
        ) : null}
      </span>

      <span className="os-work-step-state">
        {/* THE COST SITS ONLY ON THE STEPS THAT COST SOMETHING. A dash on
            forty-four rows to say "free" is forty-four things to read past;
            silence says it better, and the band above has already said how
            many of each there are. */}
        {thought ? (
          <span className="os-work-step-spend os-mono">
            {step.tokens === null ? null : <span>{formatTokens(step.tokens)} tok</span>}
            {step.cost === null ? null : <span>{formatMoney(step.cost)}</span>}
          </span>
        ) : null}
        <span className="os-work-step-kind">{stepKindWord(step.kind)}</span>
        <span className="os-work-step-duration os-mono">
          {step.durationMs === null ? "" : formatDuration(step.durationMs)}
        </span>
        <span className="os-work-step-status" data-status={step.status}>
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
        <ChevronRight size={13} className="os-work-step-chevron" aria-hidden />
      </span>
    </button>
  );
}
