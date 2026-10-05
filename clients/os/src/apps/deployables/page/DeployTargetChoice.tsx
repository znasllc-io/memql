import { Split } from "lucide-react";

import { ChoiceStack, type ChoiceOption } from "../../../kit";
import type { Refusal } from "../packages/actions";
import type { DeployTarget } from "../packages/calls";
import { ProblemNotice } from "../packages/ReportView";

// WHERE THE GATE'S VERSION GOES (memql#5601): the live version, or the
// candidate.
//
// ===========================================================================
// A CHOICE BESIDE THE ATTEMPT, AND THE ACT STAYS ON THE BAR
// ===========================================================================
// A redeploy parks at the gate with its report, and the bar's Deploy confirms
// it. This is the one question that confirm now asks first -- and a question
// with a default is a choice, not a second button. It sits under the attempt
// it is about, each option saying what it does to the people visiting the
// site, and the bar's act takes the answer's name ("Deploy as candidate"), so
// what the click does is on the button too (DESIGN.md rule 12).
//
// A CHOICE STACK, NOT PILLS, because the two options need explaining -- the
// kit's own line between the two controls -- and because one of them can be
// unavailable with a reason: a version that ships MemQL cannot be held back
// as a candidate, and the card says so where it would have been chosen.
//
// THE ENGINE'S REFUSAL LANDS HERE while the gate is still the question, the
// stop-owns-its-refusal rule this app keeps everywhere: a person who chose
// Candidate and was refused reads why beside the choice, not at the top of a
// page they have scrolled away from.

export interface DeployChoice {
  /** What is chosen now. Serving by default: what every deploy did before. */
  target: DeployTarget;
  onChoose: (target: DeployTarget) => void;
  /** The deployable's own address, which is what "the live site" is. */
  hostname: string;
  /** Why a candidate cannot be had for this version, or "". */
  blocked: string;
  /** A confirm the engine refused while this gate is still the question. */
  refusal: Refusal | null;
}

export function DeployTargetChoice({ target, onChoose, hostname, blocked, refusal }: DeployChoice) {
  const options: ChoiceOption[] = [
    {
      value: "serving",
      label: "Live version",
      description: `Visitors to ${hostname} get it as soon as it is in place.`,
    },
    {
      value: "candidate",
      label: "Candidate",
      description: blocked !== "" ? blocked : `${hostname} keeps serving its current version. Preview the new one, then promote it.`,
      unavailable: blocked !== "",
    },
  ];
  return (
    <div className="deployable-version-row deployable-deploy-target">
      <span>
        <Split size={14} aria-hidden />
        Deploy as
      </span>
      <div className="deployable-deploy-target-body">
        <ChoiceStack
          name="os-deploy-target"
          label="Deploy this version as"
          value={target}
          onChange={(next) => onChoose(next === "candidate" ? "candidate" : "serving")}
          options={options}
          voice="prose"
        />
        {refusal === null ? null : <ProblemNotice problem={{ ...refusal, fatal: true }} tone="error" />}
      </div>
    </div>
  );
}
