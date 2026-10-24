import { usageLabel, type UsageSummary } from "./modelUsage";
import { Caption, Fact, Facts, Subhead } from "../../kit";
import { stepOutcome, stepReason, stepTitle } from "../../nexus/presentation";
import { formatTokens, formatMoney, type StepRow } from "./rows";
import { stepStatusWord } from "./words";

export function StepSummary({ step, usage }: { step: StepRow; usage?: UsageSummary }) {
  const reason = stepReason(step);
  return <section className="nexus-step-summary" aria-label="Selected action">
    <Subhead>{stepTitle(step)}</Subhead>
    <Caption>{stepStatusWord(step.status)}</Caption>
    <p className="nexus-recorded-outcome">{stepOutcome(step)}</p>
    {reason ? <section><Subhead>Why</Subhead><p>{reason}</p></section> : null}
    <Facts><Fact label="Tokens used" value={usage && usage.calls > 0 ? usageLabel(usage) : step.tokens === null ? "Not reported" : formatTokens(step.tokens)} />{step.cost !== null ? <Fact label="Recorded cost" value={formatMoney(step.cost)} /> : null}</Facts>
    <details className="nexus-technical"><summary>Technical details</summary><Facts>
      <Fact label="Step reference" value={step.key} copyValue={step.key} />
      <Fact label="Operation" value={step.callName || step.stepType} />
      <Fact label="Kind" value={step.kind || "Not recorded"} />
      {step.errorMessage ? <Fact label="Failure details" value={step.errorMessage} /> : null}
      {step.dependsOn.length ? <Fact label="Dependencies" value={step.dependsOn.join(", ")} /> : null}
    </Facts></details>
  </section>;
}
