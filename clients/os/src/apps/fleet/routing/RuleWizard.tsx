import { useState } from "react";
import { ListOrdered } from "lucide-react";

import { ChoiceStack, Field, Input, Notice, Select } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { shadowedBy, type RuleActions, type RuleRow, type RuleWhenKey } from "../../settings/rulesFacts";
import type { TaskPolicy } from "../taskPolicies";
import { routeEntries } from "./routes";
import { RuleSentence } from "./RuleSentence";
import { routeStatus, type RoutingFacts } from "./sources";
import { MODALITY_TITLES, routeIdFrom, routeTitle, whenWords } from "./vocabulary";

// Add a rule: When -> Route -> Review, on the shell's one wizard.
//
// ONE QUESTION A STEP, AND A STEP THAT IS ONE CHOICE IS ANSWERED BY CHOOSING.
// Picking "Fast work" IS the When step, so the wizard moves on; picking "A
// prompt" asks one more thing (which prompt), and only then is there a Next.
// The route is one choice too. Review is the rule as the list will draw it,
// and its one forward act checks the definition and adds it.
//
// "Describe it instead" is the other way in -- the sentence compiler -- and is
// offered on the first step, where the choice between the two is being made.
//
// The rest of a rule (the level it asks for, what happens when nothing is
// ready, its precedence) keeps its defaults here and is on the rule's own page
// afterwards. A wizard adds; the page edits (DESIGN.md, "Adding only").
//
// A RULE A SHIPPED ONE ALREADY DECIDES IS NOT ADDED. Shipped rules evaluate
// before yours regardless of precedence, so "Fast work -> Claude Code" beside
// the shipped "Fast work -> Fast local first" saves, checks clean, and never
// fires. The choice says so where it is made ("Already takes Fast local
// first"), and Review offers the act that WOULD work -- changing that route --
// instead of Add rule.

type Step = "when" | "route" | "review";

type WhenChoice = "level:fast" | "level:strong" | "level:reasoning" | "modality" | "prompt" | "tag" | "role" | "actorRole" | "touches";

const CHOICES: readonly { value: WhenChoice; label: string }[] = [
  { value: "level:fast", label: "Fast work" },
  { value: "level:strong", label: "Strong work" },
  { value: "level:reasoning", label: "Reasoning work" },
  { value: "modality", label: "A kind of call" },
  { value: "prompt", label: "A prompt" },
  { value: "tag", label: "A tag" },
  { value: "role", label: "Who is acting" },
  { value: "actorRole", label: "Who is watching" },
  { value: "touches", label: "What it touches" },
];

const VALUE_LABEL: Record<string, string> = {
  prompt: "Prompt name",
  tag: "Tag",
  role: "Role",
  actorRole: "Role watching",
  touches: "What it touches",
};

export function RuleWizard({
  actions,
  rules,
  routes,
  facts,
  onCancel,
  onDescribe,
  onAdded,
  onOpenRoute,
}: {
  actions: RuleActions;
  rules: readonly RuleRow[];
  routes: readonly TaskPolicy[];
  facts: RoutingFacts;
  onCancel: () => void;
  onDescribe: () => void;
  onAdded: () => void;
  /** Open a route's page: the act a shadowed rule offers instead of Add. */
  onOpenRoute?: (route: string) => void;
}) {
  const [step, setStep] = useState<Step>("when");
  const [choice, setChoice] = useState<WhenChoice | "">("");
  const [value, setValue] = useState("");
  const [route, setRoute] = useState("");
  const [name, setName] = useState("");
  const [refusal, setRefusal] = useState("");
  const [working, setWorking] = useState(false);

  const when: Partial<Record<RuleWhenKey, string>> =
    choice === "" ? {} : choice.startsWith("level:") ? { level: choice.slice("level:".length) } : { [choice]: value.trim() };
  const whenAnswered = choice !== "" && (choice.startsWith("level:") || value.trim() !== "");
  const shadow = whenAnswered ? shadowedBy(when, rules) : null;
  const options = CHOICES.map((one) => {
    if (!one.value.startsWith("level:")) return one;
    const decided = shadowedBy({ level: one.value.slice("level:".length) }, rules);
    return decided ? { ...one, description: `Already takes ${routeTitle(decided.policy)}` } : one;
  });
  const custom = rules.filter((r) => !r.locked);
  const precedence = custom.length === 0 ? 10 : Math.max(...custom.map((r) => r.precedence)) + 10;

  const suggested = uniqueName(routeIdFrom(`${whenWords({ when, locked: false })} ${routeTitle(route)}`), rules);
  const ruleName = name.trim() === "" ? suggested : name.trim();
  const nameValid = /^[a-z][A-Za-z0-9]*$/.test(ruleName) && !rules.some((r) => r.name === ruleName);
  const draft: RuleRow = {
    name: ruleName,
    when,
    level: "",
    policy: route,
    precedence,
    onUnavailable: "degrade",
    excludes: [],
    locked: false,
    described: "",
    revision: rules.find((r) => typeof r.revision === "number")?.revision,
  };

  async function add() {
    setWorking(true);
    setRefusal("");
    const check = await actions.simulate(draft);
    if (check.refusal !== "") {
      setRefusal(check.refusal);
      setWorking(false);
      return;
    }
    const ok = await actions.activate({ ...draft, revision: check.revision ?? draft.revision });
    setWorking(false);
    if (ok) onAdded();
  }

  const reached = (s: Step) => s === "when" || (s === "route" && whenAnswered) || (s === "review" && whenAnswered && route !== "");
  const order: Step[] = ["when", "route", "review"];
  const stateOf = (s: Step): Stop["state"] =>
    s === step ? "open" : reached(s) ? (order.indexOf(s) < order.indexOf(step) ? "done" : "waiting") : "ahead";

  const steps: Stop[] = [
    {
      id: "when",
      name: "When",
      state: stateOf("when"),
      sentence: "What the work looks like.",
      answer: whenAnswered ? whenWords({ when, locked: false }) : undefined,
      body: (
        <div className="fleet-rule-when-step">
          <ChoiceStack
            name="fleet-rule-when"
            label="When work looks like"
            voice="prose"
            value={choice}
            options={options}
            onChange={(next) => {
              const picked = next as WhenChoice;
              setChoice(picked);
              setValue("");
              if (picked.startsWith("level:")) setStep("route");
            }}
          />
          {choice === "modality" ? (
            <Field label="Kind of call">
              <Select
                id="fleet-rule-modality"
                label="Kind of call"
                value={value}
                onChange={(next) => {
                  setValue(next);
                  if (next !== "") setStep("route");
                }}
              >
                <option value="">Choose one</option>
                {Object.entries(MODALITY_TITLES).map(([id, title]) => <option key={id} value={id}>{title}</option>)}
              </Select>
            </Field>
          ) : choice !== "" && !choice.startsWith("level:") ? (
            <Field label={VALUE_LABEL[choice] ?? "Value"}>
              <Input id="fleet-rule-value" label={VALUE_LABEL[choice] ?? "Value"} value={value} onChange={setValue} onEnter={() => { if (value.trim() !== "") setStep("route"); }} />
            </Field>
          ) : null}
          <button type="button" className="fleet-reading-link" onClick={onDescribe}>Describe it instead</button>
        </div>
      ),
    },
    {
      id: "route",
      name: "Route",
      state: stateOf("route"),
      sentence: "Where that work goes.",
      answer: route ? routeTitle(route) : undefined,
      body: (
        <ChoiceStack
          name="fleet-rule-route"
          label="Take the route"
          voice="prose"
          value={route}
          options={routes.map((r) => ({ value: r.name, label: routeTitle(r.name), description: routeStatus(routeEntries(r), facts).word || undefined }))}
          onChange={(next) => {
            setRoute(next);
            setStep("review");
          }}
        />
      ),
    },
    {
      id: "review",
      name: "Review",
      state: stateOf("review"),
      body: (
        <div className="fleet-rule-review">
          <div className="fleet-rule-said"><RuleSentence rule={draft} /></div>
          <Field label="Name">
            <Input id="fleet-rule-name" label="Rule name" value={name} placeholder={suggested} onChange={setName} code />
          </Field>
        </div>
      ),
    },
  ];

  const forward: Act | null =
    step === "when"
      ? whenAnswered && !choice.startsWith("level:") ? { label: "Next", tone: "primary", onAct: () => setStep("route") } : null
      : step === "route"
        ? route !== "" ? { label: "Next", tone: "primary", onAct: () => setStep("review") } : null
        : shadow !== null
          ? onOpenRoute ? { label: `Change ${routeTitle(shadow.policy)}`, tone: "primary", onAct: () => onOpenRoute(shadow.policy) } : null
          : nameValid ? { label: "Add rule", tone: "primary", busy: working || actions.state.busy, onAct: () => void add() } : null;

  const status =
    step === "when"
      ? { word: "Choose the work", detail: "" }
      : step === "route"
        ? { word: "Choose a route", detail: "" }
        : shadow !== null
          ? { word: "Decided by a shipped rule", detail: `${whenWords(shadow)} already takes ${routeTitle(shadow.policy)}; change that route instead` }
          : { word: working ? "Adding" : "Ready to add", detail: nameValid ? "" : "Choose another name" };

  return (
    <Wizard
      className="fleet-rule-wizard"
      icon={<ListOrdered aria-hidden />}
      title="Add a rule"
      breadcrumbs={[{ label: "Rules", onSelect: onCancel }, { label: "Add a rule" }]}
      back={{ label: "Rules", onSelect: onCancel }}
      label="Adding a rule"
      steps={steps}
      open={step}
      onOpen={(next) => {
        if (reached(next as Step)) setStep(next as Step);
      }}
      status={{ word: status.word, detail: status.detail, tone: working ? "busy" : "none" }}
      acts={[{ label: "Cancel", text: true, onAct: onCancel }, ...(forward ? [forward] : [])]}
      notices={
        refusal !== "" ? (
          <Notice tone="warn" sentence="The cluster refused that rule." next="Nothing was written." detail={refusal} />
        ) : actions.state.failed ? (
          <Notice tone="error" sentence="The rule was not added." detail={actions.state.message} />
        ) : null
      }
      context={{ page: "Add a rule", step }}
    />
  );
}

/** A name no rule already has, by numbering the suggestion. */
function uniqueName(base: string, rules: readonly RuleRow[]): string {
  const root = base === "" ? "myRule" : base;
  if (!rules.some((r) => r.name === root)) return root;
  for (let n = 2; ; n++) {
    const next = `${root}${n}`;
    if (!rules.some((r) => r.name === next)) return next;
  }
}
