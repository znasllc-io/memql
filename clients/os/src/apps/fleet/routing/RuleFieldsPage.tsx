import { useState } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Caption, Check, Field, Head, Input, Notice, Select } from "../../../kit";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { LEVELS } from "../../settings/routingFacts";
import {
  RULE_WHEN_KEYS,
  RULE_WHEN_MEANING,
  precedenceTaken,
  simulationSentence,
  whenObjectFrom,
  type RuleActions,
  type RuleRow,
  type RuleWhenKey,
  type Simulation,
} from "../../settings/rulesFacts";
import type { TaskPolicy } from "../taskPolicies";
import { RuleSentence } from "./RuleSentence";
import { levelTitle, routeTitle } from "./vocabulary";

// A rule's fields, as a page of its own (was Settings > Rules' "Edit as
// fields" panel). It is where every part of a rule is reachable: the
// conditions, the level it asks for, its route, what happens when nothing is
// ready, and its precedence. The Add wizard asks only When and Route; this is
// the rest.
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
// Seven text boxes cannot say the first two apart: every box left alone would
// be sent as "", turning non-conditions into conditions almost nothing
// satisfies, and the rule silently never fires. So presence is its own
// control, and `whenObjectFrom` -- the only path from this form to the wire --
// emits a key only for a ticked condition. (DESIGN.md rule 10 keeps checkboxes
// off BROWSING surfaces; this is a form, where they state a choice.)
//
// ===========================================================================
// THE BAR CARRIES THE ACTS, AND ACTIVATE WAITS FOR A CHECK
// ===========================================================================
// Check it, then Activate -- absent, never disabled, until the check answered
// (rule 12). Any edit withdraws the check: it belonged to one exact rule.
// Remove is a text act beside them, only on a rule the person wrote, and it
// asks first, in the bar.

const ON_UNAVAILABLE = ["degrade", "park"] as const;

export function RuleFieldsPage({
  actions,
  routes,
  seed,
  existing,
  onDone,
  onBack,
}: {
  actions: RuleActions;
  routes: readonly TaskPolicy[];
  /** A rule to open on -- from the compiler, or an existing custom rule. */
  seed: RuleRow | null;
  /** Every rule, for the name and precedence-collision checks. */
  existing: readonly RuleRow[];
  onDone: () => void;
  onBack: () => void;
}) {
  const editing = seed !== null && existing.some((r) => r.name === seed.name);
  const [name, setName] = useState(seed?.name ?? "");
  const [values, setValues] = useState<Partial<Record<RuleWhenKey, string>>>(seed?.when ?? {});
  const [used, setUsed] = useState<Set<RuleWhenKey>>(new Set(seed ? (Object.keys(seed.when) as RuleWhenKey[]) : []));
  const [level, setLevel] = useState(seed?.level ?? "");
  const [route, setRoute] = useState(seed?.policy ?? "localFirst");
  const [precedence, setPrecedence] = useState(String(seed?.precedence ?? 10));
  const [onUnavailable, setOnUnavailable] = useState(seed?.onUnavailable || "degrade");
  const [simulation, setSimulation] = useState<Simulation | null>(null);
  const [working, setWorking] = useState(false);
  const [removing, setRemoving] = useState(false);

  const precedenceNumber = Number(precedence);
  const precedenceValid = precedence.trim() !== "" && Number.isInteger(precedenceNumber) && precedenceNumber >= 0;
  const collision = precedenceValid ? precedenceTaken(existing, precedenceNumber, name) : null;
  const nameValid = /^[a-z][A-Za-z0-9]*$/.test(name);
  const nameTaken = !editing && existing.some((r) => r.name === name);

  const draft: RuleRow = {
    name,
    revision: simulation?.revision ?? seed?.revision,
    when: whenObjectFrom(values, used),
    level,
    policy: route,
    precedence: precedenceValid ? precedenceNumber : 0,
    onUnavailable,
    excludes: seed?.excludes ?? [],
    locked: false,
    described: seed?.described ?? "",
  };

  const invalidate = () => setSimulation(null);
  const toggle = (key: RuleWhenKey, on: boolean) => {
    const next = new Set(used);
    if (on) next.add(key);
    else next.delete(key);
    setUsed(next);
    invalidate();
  };

  const ready = !nameTaken && nameValid && precedenceValid && collision === null && route.trim() !== "";
  const checked = simulation !== null && simulation.refusal === "";
  const title = editing ? "Edit rule" : "New rule";

  const acts: Act[] = removing
    ? [
        { label: "Keep", text: true, onAct: () => setRemoving(false) },
        {
          label: "Remove rule",
          tone: "danger",
          busy: actions.state.busy,
          onAct: () => {
            void actions.retire(seed!.name, seed!.revision).then((ok) => {
              if (ok) onDone();
              else setRemoving(false);
            });
          },
        },
      ]
    : [
        { label: "Cancel", text: true, onAct: onBack },
        ...(editing ? [{ label: "Remove", text: true, tone: "danger" as const, onAct: () => setRemoving(true) }] : []),
        ...(ready && simulation === null
          ? [{
              label: "Check it",
              tone: "primary" as const,
              busy: working,
              onAct: () => {
                setWorking(true);
                void actions.simulate(draft).then((s) => {
                  setSimulation(s);
                  setWorking(false);
                });
              },
            }]
          : []),
        ...(ready && checked
          ? [{
              label: "Activate",
              tone: "primary" as const,
              busy: actions.state.busy,
              onAct: () => {
                void actions.activate(draft).then((ok) => {
                  if (ok) onDone();
                });
              },
            }]
          : []),
      ];

  const state = removing
    ? "Remove this rule?"
    : simulation === null
      ? ready ? "Not checked yet" : "Unfinished"
      : simulation.refusal === "" ? "Checked" : "Refused";
  const detail = removing
    ? "Calls it matched fall to the rules below it"
    : simulation !== null
      ? simulationSentence(simulation)
      : !nameValid
        ? "A name starts with a lower-case letter, then letters and digits"
        : nameTaken
          ? "A rule with that name exists"
          : collision !== null
            ? "Another rule has that precedence"
            : "";

  return (
    <div className="os-deploy-pane fleet-rule-page">
      <div className="os-deploy-scroll">
        <ActivityTarget target={seed ? `fleet:rule:${seed.name}` : "fleet:rule:draft"}>
          <Head title={title} breadcrumbs={[{ label: "Rules", onSelect: onBack }, { label: title }]} back={{ label: "Rules", onSelect: onBack }} />
          <div className="fleet-rule-said">
            <RuleSentence rule={draft} />
          </div>
          <fieldset disabled={working || actions.state.busy} className="fleet-rule-edit-fields">
            <form className="os-form" onSubmit={(e) => e.preventDefault()}>
              <Field label="Name">
                <Input
                  id="rule-field-name"
                  label="Name"
                  value={name}
                  disabled={working || actions.state.busy || editing}
                  placeholder="planningStaysLocal"
                  onChange={(next) => {
                    setName(next);
                    invalidate();
                  }}
                />
              </Field>

              <fieldset className="os-field-group">
                <legend>When</legend>
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
                {used.size === 0 ? <Caption>Nothing ticked, so this rule matches every call.</Caption> : null}
              </fieldset>

              <fieldset className="os-field-group">
                <legend>Then</legend>
                <Field label="Route">
                  <Select id="rule-field-route" label="Route" value={route} onChange={(next) => { setRoute(next); invalidate(); }}>
                    {!routes.some((r) => r.name === route) ? <option value={route}>{routeTitle(route)} (not read)</option> : null}
                    {routes.map((r) => <option key={r.name} value={r.name}>{routeTitle(r.name)}</option>)}
                  </Select>
                </Field>
                <Field label="Ask for this level instead">
                  <Select id="rule-field-level" label="Ask for this level instead" value={level} onChange={(next) => { setLevel(next); invalidate(); }}>
                    <option value="">Leave the level alone</option>
                    {LEVELS.map((one) => <option key={one} value={one}>{levelTitle(one)}</option>)}
                  </Select>
                </Field>
                <Field label="When nothing there is ready">
                  <Select id="rule-field-unavailable" label="When nothing there is ready" value={onUnavailable} onChange={(next) => { setOnUnavailable(next); invalidate(); }}>
                    {ON_UNAVAILABLE.map((one) => <option key={one} value={one}>{one === "degrade" ? "Step down a level" : "Wait for a person"}</option>)}
                  </Select>
                </Field>
                <Field label="Precedence">
                  <Input id="rule-field-precedence" label="Precedence" value={precedence} placeholder="10" onChange={(next) => { setPrecedence(next); invalidate(); }} />
                </Field>
                <Caption>Higher runs first among your rules. A matching shipped rule still runs before them.</Caption>
                {collision === null ? null : (
                  <Notice
                    tone="warn"
                    sentence={`${collision.name} already sits at precedence ${precedenceNumber}.`}
                    next="Two rules at one precedence are resolved by nothing, so replicas could disagree about which wins. Pick another number."
                  />
                )}
              </fieldset>
            </form>
          </fieldset>

          {actions.state.message === "" ? null : (
            <Notice tone={actions.state.failed ? "error" : "info"} sentence={actions.state.failed ? "That did not go through." : "Done."} detail={actions.state.message} />
          )}
        </ActivityTarget>
      </div>
      <ActionBar state={state} detail={detail} tone={working || actions.state.busy ? "busy" : removing ? "paused" : "none"} live acts={acts} />
    </div>
  );
}
