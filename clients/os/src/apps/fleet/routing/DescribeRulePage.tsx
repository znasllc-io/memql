import { useState } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Head, Notice } from "../../../kit";
import { simulationSentence, type RuleActions, type RuleRow, type Simulation } from "../../settings/rulesFacts";
import { RuleSentence } from "./RuleSentence";

// "Describe it instead" -- the rule compiled from a sentence (was Settings >
// Rules' "Describe a rule" panel; the entry's name is the owner's pick of
// 2026-09-07). It is the Add wizard's alternative first step.
//
// ===========================================================================
// THREE STATES, AND THE ACTS APPEAR AS THEY BECOME LEGAL
// ===========================================================================
//   typing     one text field; the bar offers Compile it once there is text
//   compiled   the rule the sentence became, drawn as the SAME sentence the
//              list draws; the bar offers Check it
//   checked    the bar offers Activate -- and not one moment earlier
//
// An act that is not legal is ABSENT, never disabled (rule 12): a greyed-out
// Activate would advertise that activating without checking is possible from
// here. Any edit to the words throws the compiled rule and its check away,
// because a check belongs to one exact rule.
//
// A COMPILER REFUSAL IS RENDERED VERBATIM AND THE WORDS ARE KEPT. The compiler
// knows which clause it could not read; this page does not. Clearing the box
// would throw away the thing the person is in the middle of fixing.

export function DescribeRulePage({
  actions,
  onActivated,
  onEditAsFields,
  onBack,
}: {
  actions: RuleActions;
  onActivated: () => void;
  onEditAsFields: (rule: RuleRow) => void;
  onBack: () => void;
}) {
  const [sentence, setSentence] = useState("");
  const [compiled, setCompiled] = useState<RuleRow | null>(null);
  const [problem, setProblem] = useState("");
  const [simulation, setSimulation] = useState<Simulation | null>(null);
  const [working, setWorking] = useState(false);

  const edit = (next: string) => {
    setSentence(next);
    setCompiled(null);
    setSimulation(null);
    setProblem("");
  };

  const compile = async () => {
    setWorking(true);
    const result = await actions.describe(sentence);
    setCompiled(result.rule);
    setProblem(result.problem);
    setSimulation(null);
    setWorking(false);
  };

  const check = async () => {
    if (compiled === null) return;
    setWorking(true);
    setSimulation(await actions.simulate(compiled));
    setWorking(false);
  };

  const activate = async () => {
    if (compiled === null) return;
    const ok = await actions.activate({ ...compiled, revision: simulation?.revision ?? compiled.revision, described: sentence });
    if (ok) onActivated();
  };

  const forward: Act | null =
    compiled === null
      ? sentence.trim() === "" ? null : { label: "Compile it", tone: "primary", busy: working, onAct: () => void compile() }
      : simulation === null
        ? { label: "Check it", tone: "primary", busy: working, onAct: () => void check() }
        : simulation.refusal === ""
          ? { label: "Activate", tone: "primary", busy: actions.state.busy, onAct: () => void activate() }
          : null;

  const acts: Act[] = [
    { label: "Cancel", text: true, onAct: onBack },
    ...(compiled === null ? [] : [{ label: "Edit as fields", text: true, onAct: () => onEditAsFields(compiled) }]),
    ...(forward ? [forward] : []),
  ];

  const state = compiled === null ? (sentence.trim() === "" ? "Describe the rule" : "Not compiled") : simulation === null ? "Compiled" : simulation.refusal === "" ? "Checked" : "Refused";

  return (
    <div className="os-deploy-pane fleet-rule-page">
      <div className="os-deploy-scroll">
        <Head title="Describe a rule" breadcrumbs={[{ label: "Rules", onSelect: onBack }, { label: "Describe a rule" }]} back={{ label: "Rules", onSelect: onBack }} />
        <label className="os-sr-only" htmlFor="rule-describe">
          Describe a rule in your own words
        </label>
        <textarea
          id="rule-describe"
          className="os-input os-rule-textarea"
          rows={3}
          value={sentence}
          placeholder="Send planning work to my own machines, and never to a paid vendor"
          onChange={(e) => edit(e.target.value)}
        />

        {problem === "" ? null : (
          <Notice tone="warn" sentence="That could not be compiled into a rule." next="Your words are still in the box. The compiler's own reason is below." detail={problem} />
        )}

        {compiled === null ? null : (
          <div className="fleet-rule-said" aria-label="The rule it becomes" role="group">
            <RuleSentence rule={compiled} />
          </div>
        )}

        {simulation === null ? null : <p className="os-rule-simulation-said">{simulationSentence(simulation)}</p>}

        {actions.state.message === "" ? null : (
          <Notice tone={actions.state.failed ? "error" : "info"} sentence={actions.state.failed ? "That did not go through." : "Done."} detail={actions.state.message} />
        )}
      </div>
      <ActionBar state={state} tone={working || actions.state.busy ? "busy" : "none"} live acts={acts} />
    </div>
  );
}
