import { useState } from "react";

import { Button, Caption, Notice, Panel, Subhead } from "../../kit";
import {
  ruleSentence,
  simulationSentence,
  type RuleActions,
  type RuleRow,
  type Simulation,
} from "./rulesFacts";

// "Describe a rule" (epic memql#5153, D1; the entry's name is the owner's
// pick of the three offered on 2026-09-07).
//
// ===========================================================================
// THREE STATES, AND THE ACTS APPEAR AS THEY BECOME LEGAL
// ===========================================================================
//   typing     one text field. Nothing else is offered, because nothing else
//              is possible yet.
//   compiled   the rule the sentence became, written back as a SENTENCE in
//              the same words the list uses. "Check it" appears.
//   simulated  what the rule would have done to real decisions. "Activate"
//              appears -- and not one moment earlier.
//
// DESIGN.md rule 12: an act that is not legal is ABSENT, never disabled. A
// greyed-out Activate would advertise that activating is a thing you can do
// from here without checking, and the whole reason this panel exists is that
// it is not.
//
// ===========================================================================
// WHY THE COMPILED RULE IS SHOWN AS A SENTENCE, NOT AS FIELDS
// ===========================================================================
// The person wrote a sentence. Showing them seven populated fields asks them
// to verify a translation into a notation they did not choose and may not
// read. Showing them the rule in the SAME sentence form the list uses asks
// them the question they can actually answer -- "is this what I meant?" --
// and it is the exact line that will appear in the list afterwards, so there
// is no second thing to learn.
//
// Fields are still there for people who prefer them, behind "Edit as fields".
// That is a preference, not a fallback.
//
// ===========================================================================
// A COMPILER REFUSAL IS RENDERED VERBATIM AND THE TEXT IS KEPT
// ===========================================================================
// The compiler knows which clause it could not read; this panel does not. So
// its sentence is shown as it came, and the person's own words stay in the
// box -- clearing the field on a refusal throws away the thing they are in
// the middle of fixing.

export function DescribeRulePanel({
  actions,
  onActivated,
  onEditAsFields,
  onCancel,
}: {
  actions: RuleActions;
  onActivated: () => void;
  onEditAsFields: (rule: RuleRow) => void;
  onCancel: () => void;
}) {
  const [sentence, setSentence] = useState("");
  const [compiled, setCompiled] = useState<RuleRow | null>(null);
  const [problem, setProblem] = useState("");
  const [simulation, setSimulation] = useState<Simulation | null>(null);
  const [working, setWorking] = useState(false);

  // ANY EDIT INVALIDATES WHAT WAS CHECKED. A simulation belongs to one exact
  // rule; keeping it visible while the sentence changes underneath would let
  // somebody activate a rule on the strength of a check of a different one.
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

  return (
    <Panel label="Describe a rule">
      <div className="os-rule-describe">
        <Subhead>Describe a rule</Subhead>
        <Caption>
          Describe the routing you want. Review the compiled rule, check its
          definition, then activate it. This check does not replay past calls.
        </Caption>

        <form
          className="os-form"
          onSubmit={(e) => {
            e.preventDefault();
            void compile();
          }}
        >
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
          {sentence.trim() === "" ? null : (
            <Button type="submit" tone="primary" busy={working} busyLabel="Compiling">
              Compile it
            </Button>
          )}
        </form>

        {problem === "" ? null : (
          <Notice
            tone="warn"
            sentence="That could not be compiled into a rule."
            next="Your words are still in the box. The compiler's own reason is below."
            detail={problem}
          />
        )}

        {compiled === null ? null : (
          <div className="os-rule-compiled">
            <p className="os-rule-compiled-label">This is the rule it becomes</p>
            <p className="os-rule-compiled-said">{ruleSentence(compiled)}</p>
            <Caption>
              It will sit at precedence {compiled.precedence}, after matching shipped rules and before the shipped catch-all.
            </Caption>
          </div>
        )}

        {simulation === null ? null : (
          <div className="os-rule-simulation">
            <p className="os-rule-simulation-said">{simulationSentence(simulation)}</p>
          </div>
        )}

        {/* ONE ACTS ROW, ALWAYS RENDERED, and Cancel is always in it.
            
            The first cut had two conditional rows -- one under `compiled` and
            one under `simulation` -- and between them they left the
            compiled-but-not-yet-checked state with NO way out except emptying
            the textarea by hand. That is rule 12 read backwards: the rule says
            an illegal act is absent, not that a legal one may go missing
            because the state that offers it was never enumerated. Cancel is
            legal in every state of this panel, so it is unconditional. */}
        <div className="os-rule-acts">
          {compiled !== null && simulation === null ? (
            <Button tone="primary" onClick={() => void check()} busy={working} busyLabel="Checking">
              Check it
            </Button>
          ) : null}
          {/* ACTIVATE APPEARS ONLY AFTER A SIMULATION, and only when the
              simulation is an ANSWER rather than a refusal: a refusal means
              nothing was compared, so nothing was learned. */}
          {compiled !== null && simulation !== null && simulation.refusal === "" ? (
            <Button
              tone="primary"
              onClick={() => void activate()}
              busy={actions.state.busy}
              busyLabel="Activating"
            >
              Activate
            </Button>
          ) : null}
          {compiled === null ? null : (
            <Button onClick={() => onEditAsFields(compiled)}>Edit as fields</Button>
          )}
          <Button onClick={onCancel}>Cancel</Button>
        </div>

        {actions.state.message === "" ? null : (
          <Notice
            tone={actions.state.failed ? "error" : "info"}
            sentence={actions.state.failed ? "That did not go through." : "Done."}
            detail={actions.state.message}
          />
        )}
      </div>
    </Panel>
  );
}
