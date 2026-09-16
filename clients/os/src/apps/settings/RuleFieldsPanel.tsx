import { useState } from "react";

import { Button, Caption, Check, Field, Input, Notice, Panel, Select, Subhead } from "../../kit";
import type { TaskPolicy } from "../fleet/taskPolicies";
import { PolicyChain } from "../fleet/PolicyChain";
import { LEVELS } from "./routingFacts";
import {
  RULE_WHEN_KEYS,
  RULE_WHEN_MEANING,
  precedenceTaken,
  ruleSentence,
  simulationSentence,
  whenObjectFrom,
  type RuleActions,
  type RuleRow,
  type RuleWhenKey,
  type Simulation,
} from "./rulesFacts";

// "Edit as fields" (epic memql#5153, D1): the structured editor, for people
// who would rather set the conditions than describe them.
//
// ===========================================================================
// EACH CONDITION IS A CHECKBOX PLUS A VALUE, AND THE CHECKBOX IS THE POINT
// ===========================================================================
// The engine distinguishes three states per condition, and the middle one is
// the trap:
//
//   the key is ABSENT           -> the rule does not look at this at all
//   the key is present, EMPTY   -> it matches only an empty value
//   the key is present, "x"     -> it matches "x"
//
// Seven text boxes cannot say the first two apart. Every box the person left
// alone would be sent as "", turning six non-conditions into six conditions
// that almost nothing satisfies -- and the rule then silently never fires,
// while looking completely correct in the list.
//
// So presence is its own control. Ticking "the level asked for"
// means the rule looks at the level; what is typed beside it is what it looks
// FOR, empty included. `whenObjectFrom` emits a key only for a ticked
// condition, and it is the only path from this form to the wire.
//
// DESIGN.md rule 10 bans standing checkboxes on BROWSING surfaces, where they
// are chrome in front of content. This is a form, and rule 10's own sentence
// says checkboxes belong to forms, "where they state a choice". This is
// exactly that: the choice of whether a condition exists.

const ON_UNAVAILABLE = ["degrade", "park"] as const;

export function RuleFieldsPanel({
  actions,
  policies,
  seed,
  existing,
  onActivated,
  onCancel,
}: {
  actions: RuleActions;
  policies?: readonly TaskPolicy[];
  /** A rule to open on -- from the compiler, or an existing custom rule. */
  seed: RuleRow | null;
  /** Every rule, for the precedence-collision check. */
  existing: readonly RuleRow[];
  onActivated: () => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState(seed?.name ?? "");
  const [values, setValues] = useState<Partial<Record<RuleWhenKey, string>>>(seed?.when ?? {});
  const [used, setUsed] = useState<Set<RuleWhenKey>>(
    new Set(seed ? (Object.keys(seed.when) as RuleWhenKey[]) : []),
  );
  const [level, setLevel] = useState(seed?.level ?? "");
  const [policy, setPolicy] = useState(seed?.policy ?? "localFirst");
  const [precedence, setPrecedence] = useState(String(seed?.precedence ?? 10));
  const [onUnavailable, setOnUnavailable] = useState(seed?.onUnavailable ?? "degrade");
  const [simulation, setSimulation] = useState<Simulation | null>(null);
  const [working, setWorking] = useState(false);

  const precedenceNumber = Number(precedence);
  const precedenceValid = Number.isInteger(precedenceNumber) && precedenceNumber >= 0;
  const collision = precedenceValid ? precedenceTaken(existing, precedenceNumber, name) : null;
  const nameValid = /^[a-z][A-Za-z0-9]*$/.test(name);

  const draft: RuleRow = {
    name,
    revision: simulation?.revision ?? seed?.revision,
    when: whenObjectFrom(values, used),
    level,
    policy,
    precedence: precedenceValid ? precedenceNumber : 0,
    onUnavailable,
    excludes: seed?.excludes ?? [],
    locked: false,
    described: seed?.described ?? "",
  };

  // ANY EDIT INVALIDATES THE CHECK, for the same reason as the describe panel:
  // a simulation belongs to one exact rule.
  const invalidate = () => setSimulation(null);

  const toggle = (key: RuleWhenKey, on: boolean) => {
    const next = new Set(used);
    if (on) next.add(key);
    else next.delete(key);
    setUsed(next);
    invalidate();
  };

  const nameTaken = name !== seed?.name && existing.some(r => r.name === name);
  const ready = !nameTaken && nameValid && precedenceValid && collision === null && policy.trim() !== "";

  return (
    <Panel label="Rule fields">
      <div className="os-rule-fields">
        <Subhead>Rule fields</Subhead>
        <Caption>
          A rule looks at what a call is and picks where it goes. Tick a
          condition to make the rule look at it; leave it unticked and the rule
          ignores it entirely.
        </Caption>

        <fieldset disabled={working || actions.state.busy} className="fleet-rule-edit-fields"><form
          className="os-form"
          onSubmit={(e) => {
            e.preventDefault();
          }}
        >
          <Field label="Name">
            <Input
              id="rule-field-name"
              label="Name"
              value={name}
              disabled={working || actions.state.busy || existing.some(r => r.name === seed?.name)}
              placeholder="planningStaysLocal"
              onChange={(next) => {
                setName(next);
                invalidate();
              }}
            />
          </Field>
          {nameTaken ? <Notice tone="warn" sentence="That rule already exists. Choose a new name, or cancel and edit the existing rule." /> : null}
          {name !== "" && !nameValid ? (
            <Caption>
              A rule name starts with a lower-case letter and carries letters and
              digits only.
            </Caption>
          ) : null}

          <fieldset className="os-field-group">
            <legend>Conditions</legend>
            {RULE_WHEN_KEYS.map((key) => (
              <div className="os-rule-cond" key={key}>
                <Check checked={used.has(key)} onChange={(on) => toggle(key, on)}>
                  {RULE_WHEN_MEANING[key]}
                </Check>
                {used.has(key) ? (
                  <Input
                    id={`rule-field-when-${key}`}
                    label={`Value for ${RULE_WHEN_MEANING[key]}`}
                    value={values[key] ?? ""}
                    onChange={(next) => {
                      setValues({ ...values, [key]: next });
                      invalidate();
                    }}
                  />
                ) : null}
              </div>
            ))}
            {used.size === 0 ? (
              <Caption>No conditions, so this rule matches every call.</Caption>
            ) : null}
          </fieldset>

          <fieldset className="os-field-group">
            <legend>What it does</legend>
            <Field label="Ask for this level instead">
              <Select
                id="rule-field-level"
                label="Ask for this level instead"
                value={level}
                onChange={(next) => {
                  setLevel(next);
                  invalidate();
                }}
              >
                <option value="">Leave the level alone</option>
                {LEVELS.map((one) => (
                  <option key={one} value={one}>
                    {one}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Policy">
              {policies ? <>
                <Select id="rule-field-policy" label="Policy" value={policy} onChange={next => { setPolicy(next); invalidate(); }}>
                  {!policies.some(p => p.name === policy) ? <option value={policy}>{policy} (not reported)</option> : null}
                  {policies.map(p => <option key={p.name} value={p.name}>{p.name}</option>)}
                </Select>
                {policies.find(p => p.name === policy) ? <PolicyChain policy={policies.find(p => p.name === policy)!} /> : <Caption>This policy is not in the latest cluster reading.</Caption>}
              </> : <Input id="rule-field-policy" label="Policy" value={policy} placeholder="localFirst" onChange={next => { setPolicy(next); invalidate(); }} />}

            </Field>
            <Field label="When nothing there is available">
              <Select
                id="rule-field-unavailable"
                label="When nothing there is available"
                value={onUnavailable}
                onChange={(next) => {
                  setOnUnavailable(next);
                  invalidate();
                }}
              >
                {ON_UNAVAILABLE.map((one) => (
                  <option key={one} value={one}>
                    {one === "degrade" ? "Step down a level" : "Park and wait for a person"}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Precedence">
              <Input
                id="rule-field-precedence"
                label="Precedence"
                value={precedence}
                placeholder="10"
                onChange={(next) => {
                  setPrecedence(next);
                  invalidate();
                }}
              />
            </Field>
            <Caption>
              Higher runs first among custom rules. A matching shipped rule takes
              priority; the shipped catch-all runs after custom rules.
            </Caption>
            {collision === null ? null : (
              <Notice
                tone="warn"
                sentence={`${collision.name} already sits at precedence ${precedenceNumber}.`}
                next="Two rules at one precedence are resolved by nothing, so replicas could disagree about which wins. Pick another number."
              />
            )}
          </fieldset>
        </form></fieldset>

        <div className="os-rule-compiled">
          <p className="os-rule-compiled-label">This is the rule you are writing</p>
          <p className="os-rule-compiled-said">{ruleSentence(draft)}</p>
        </div>

        {simulation === null ? null : (
          <p className="os-rule-simulation-said">{simulationSentence(simulation)}</p>
        )}

        <div className="os-rule-acts">
          {ready && simulation === null ? (
            <Button
              tone="primary"
              busy={working}
              busyLabel="Checking"
              onClick={() => {
                setWorking(true);
                void actions.simulate(draft).then((s) => {
                  setSimulation(s);
                  setWorking(false);
                });
              }}
            >
              Check it
            </Button>
          ) : null}
          {/* ABSENT UNTIL CHECKED, and absent when the check refused: a
              refusal means nothing was compared, so nothing was learned. */}
          {ready && simulation !== null && simulation.refusal === "" ? (
            <Button
              tone="primary"
              busy={actions.state.busy}
              busyLabel="Activating"
              onClick={() => {
                void actions.activate(draft).then((ok) => {
                  if (ok) onActivated();
                });
              }}
            >
              Activate
            </Button>
          ) : null}
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
