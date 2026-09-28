import { useState } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Head, Notice } from "../../../kit";
import { shadowedBy, simulationSentence, type RuleActions, type RuleRow, type Simulation } from "../../settings/rulesFacts";
import { RuleSentence } from "./RuleSentence";
import { routeTitle, whenWords } from "./vocabulary";

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
//
// A COMPILED RULE A SHIPPED ONE ALREADY DECIDES is not checked or activated:
// shipped rules run first, so it would save and never fire. The bar names the
// route that decides that work and offers to change it (the wizard's rule).

export function DescribeRulePage({
  actions,
  revision,
  rules = [],
  onActivated,
  onEditAsFields,
  onBack,
  onOpenRoute,
}: {
  actions: RuleActions;
  /** Every rule, to see whether a shipped one already decides the compiled rule's work. */
  rules?: readonly RuleRow[];
  /** Open a route's page: offered when a shipped rule decides this work. */
  onOpenRoute?: (route: string) => void;
  /**
   * The configuration revision as the rules list holds it NOW. The check names
   * it rather than whatever the compiled row carried: routes and rules share
   * one revision, and the section re-reads both after every write.
   */
  revision?: number;
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
    setSimulation(await actions.simulate({ ...compiled, revision: revision ?? compiled.revision }));
    setWorking(false);
  };

  const activate = async () => {
    if (compiled === null) return;
    const ok = await actions.activate({ ...compiled, revision: simulation?.revision ?? revision ?? compiled.revision, described: sentence });
    if (ok) onActivated();
    // The check named the revision it was made at, and a refusal read
    // everything again: check once more rather than resend a stale one.
    else setSimulation(null);
  };

  const shadow = compiled === null ? null : shadowedBy(compiled.when, rules);
  const forward: Act | null =
    compiled === null
      ? sentence.trim() === "" ? null : { label: "Compile it", tone: "primary", busy: working, onAct: () => void compile() }
      : shadow !== null
        ? onOpenRoute ? { label: `Change ${routeTitle(shadow.policy)}`, tone: "primary", onAct: () => onOpenRoute(shadow.policy) } : null
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

  const state =
    compiled === null
      ? sentence.trim() === "" ? "Describe the rule" : "Not compiled"
      : shadow !== null
        ? "Decided by a shipped rule"
        : simulation === null ? "Compiled" : simulation.refusal === "" ? "Checked" : "Refused";
  const detail = shadow === null ? undefined : `${whenWords(shadow)} already takes ${routeTitle(shadow.policy)}; change that route instead`;

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
      <ActionBar state={state} detail={detail} tone={working || actions.state.busy ? "busy" : "none"} live acts={acts} />
    </div>
  );
}
