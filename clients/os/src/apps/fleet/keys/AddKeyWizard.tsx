import { useEffect, useRef, useState } from "react";
import { KeyRound } from "lucide-react";

import { Caption, ChoiceStack, Field, Input, Notice } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { useElapsed } from "./useElapsed";
import type { VendorKeys } from "./useVendorKeys";
import { nameProblem, vendorName, VENDORS, type KeyCard, type Vendor } from "./words";

// Add an API key: Vendor -> Name -> Key -> Check, on the shell's one wizard
// (DESIGN.md, "One wizard for adding"; design record
// 2026-11-21-vendor-accounts-and-api-keys, U2).
//
// One question a step. Vendor is one choice and is answered by choosing. The
// forward act of every other step is on the floor and nowhere else. Check is
// the cluster's turn: it lists the vendor's models with the key, which sends no
// prompt and costs nothing, and saves the key only if the vendor accepts it.
//
// ===========================================================================
// THE KEY IS HELD IN ONE PLACE, FOR AS LONG AS IT IS NEEDED
// ===========================================================================
// What a person types lives in this component's state and in the field's own
// value. It is not in a draft store, not in the browser's storage, not in the
// page context Ask reads, not in the step's answer line, and not in the page's
// markup (the field is the kit's concealed input). It leaves in ONE call,
// `add`, and it is dropped the moment that call says the key was saved.
//
// It is KEPT through a refusal, on purpose: a refused key is usually a paste
// that missed a character, and the person should be able to correct it where
// it stands rather than find it again.
//
// ===========================================================================
// NOTHING IS WRITTEN BEFORE THE CHECK PASSES
// ===========================================================================
// So Cancel, on the first three steps, is simply leaving. During the check the
// way out is LEAVE and not Cancel: the request is with the cluster, the key may
// be saved a moment later, and a control named Cancel would promise to undo
// that. A person who leaves finds the key in the list if it was saved.

type Step = "vendor" | "name" | "key" | "check";

type Ending =
  | null
  /** The vendor accepts the key and it may not list models: said before finishing (K12). */
  | { kind: "useOnly"; card: KeyCard; note: string }
  /** The reply never came. The key may have been saved, so nothing is sent again from here. */
  | { kind: "unknown"; sentence: string };

const USE_ONLY_NOTE = "The vendor accepted this key, but it may not list models. It is checked by the calls it pays for.";

export function AddKeyWizard({
  keys,
  taken,
  onCancel,
  onAdded,
}: {
  keys: Pick<VendorKeys, "add">;
  /** The names of the person's own keys, which a new one may not repeat. */
  taken: readonly string[];
  onCancel: () => void;
  onAdded: (card: KeyCard | null) => void;
}) {
  const [step, setStep] = useState<Step>("vendor");
  const [vendor, setVendor] = useState<Vendor | "">("");
  const [name, setName] = useState("");
  const [key, setKey] = useState("");
  const [refusal, setRefusal] = useState("");
  const [ending, setEnding] = useState<Ending>(null);
  // The page it reports to may be gone by the time the cluster answers.
  const here = useRef(true);
  useEffect(() => {
    here.current = true;
    return () => {
      here.current = false;
    };
  }, []);

  const problem = nameProblem(name, taken);
  const named = name.trim() !== "" && problem === "";
  const typed = key.trim() !== "";
  const vendorLabel = vendor === "" ? "the vendor" : vendorName(vendor);
  const placeholder = VENDORS.find((v) => v.value === vendor)?.placeholder ?? "";

  async function add() {
    if (vendor === "" || !named || !typed || step === "check") return;
    setRefusal("");
    setStep("check");
    const result = await keys.add(vendor, name.trim(), key.trim());
    if (result.outcome === "done") {
      // THE KEY GOES FIRST, before anything is drawn again. It has done the
      // one thing it was typed for.
      if (here.current) setKey("");
      if (!here.current) return;
      if (result.card.checkedBy === "use") {
        setEnding({ kind: "useOnly", card: result.card, note: result.card.note || USE_ONLY_NOTE });
        return;
      }
      onAdded(result.card);
      return;
    }
    if (!here.current) return;
    if (result.outcome === "unknown") {
      setKey("");
      setEnding({ kind: "unknown", sentence: result.sentence });
      return;
    }
    // Refused: back to the field, with what was typed still in it.
    setRefusal(result.sentence);
    setStep("key");
  }

  const order: Step[] = ["vendor", "name", "key", "check"];
  const reached = (s: Step) => s === "vendor" || (s === "name" && vendor !== "") || (s === "key" && vendor !== "" && named) || s === step;
  const stateOf = (s: Step): Stop["state"] =>
    s === step ? "open" : reached(s) ? (order.indexOf(s) < order.indexOf(step) ? "done" : "waiting") : "ahead";
  const checking = step === "check" && ending === null;
  const waited = useElapsed(checking);

  const steps: Stop[] = [
    {
      id: "vendor",
      name: "Vendor",
      sentence: "Anthropic or OpenAI",
      state: stateOf("vendor"),
      answer: vendor !== "" && step !== "vendor" ? vendorName(vendor) : undefined,
      body: (
        <ChoiceStack
          name="fleet-add-key-vendor"
          label="Vendor"
          voice="prose"
          value={vendor}
          onChange={(next) => {
            setVendor(next as Vendor);
            setRefusal("");
            setStep("name");
          }}
          options={VENDORS.map((v) => ({ value: v.value, label: v.label, description: v.description }))}
        />
      ),
    },
    {
      id: "name",
      name: "Name",
      sentence: "What you call this key",
      state: stateOf("name"),
      answer: named && step !== "name" ? name.trim() : undefined,
      body: (
        <Field label="Name">
          <Input
            id="fleet-add-key-name"
            label="A name for this key"
            value={name}
            placeholder="Personal"
            invalid={problem !== ""}
            onChange={setName}
            onEnter={() => {
              if (named) setStep("key");
            }}
          />
        </Field>
      ),
    },
    {
      id: "key",
      name: "Key",
      sentence: `Paste the key from ${vendorLabel}`,
      state: stateOf("key"),
      // Never the value, and never part of it.
      answer: step === "check" ? "Entered" : undefined,
      body: (
        <>
          <Field label="API key">
            <Input id="fleet-add-key-value" label="The API key" value={key} placeholder={placeholder} conceal invalid={refusal !== ""} onChange={setKey} onEnter={() => void add()} />
          </Field>
          <Caption>
            {vendorLabel} is asked to list its models with this key, which sends no prompt and costs nothing. If it accepts the key, the key is sealed in this cluster and nothing shows it again.
          </Caption>
          {refusal !== "" ? <Notice tone="error" sentence="The key was not added." next="Nothing was saved. Correct the key and try again." detail={refusal} /> : null}
        </>
      ),
    },
    {
      id: "check",
      name: "Check",
      sentence: "The vendor is asked about the key",
      state: ending === null ? stateOf("check") : "done",
      body:
        ending === null ? (
          <Caption>{vendorLabel} is listing its models with the key. Nothing is saved unless it accepts the key.</Caption>
        ) : ending.kind === "useOnly" ? (
          <Notice tone="info" sentence="The key was added." next={ending.note} />
        ) : (
          <Notice tone="warn" sentence="The key may have been added." next={ending.sentence} />
        ),
    },
  ];

  const forward: Act | null =
    ending !== null
      ? ending.kind === "useOnly"
        ? { label: "Done", tone: "primary", onAct: () => onAdded(ending.card) }
        : { label: "Back to API keys", tone: "primary", onAct: () => onAdded(null) }
      : step === "name"
        ? named
          ? { label: "Continue", tone: "primary", onAct: () => setStep("key") }
          : null
        : step === "key"
          ? typed
            ? { label: "Add key", tone: "primary", onAct: () => void add() }
            : null
          : null;

  // The way out. Before anything is sent it is Cancel, a text act beside the
  // forward one. While the cluster works there is no forward act, so Leave is
  // the button. Once it has ended, the forward act is the only one.
  const acts: Act[] = ending !== null ? [] : checking ? [{ label: "Leave", onAct: onCancel }] : [{ label: "Cancel", text: true, onAct: onCancel }];
  if (forward !== null) acts.push(forward);

  const status = ending !== null
    ? ending.kind === "useOnly"
      ? { word: "Added", detail: "Checked by the calls it pays for" }
      : { word: "Not known", detail: "Check the list" }
    : step === "vendor"
      ? { word: "Choose a vendor", detail: "" }
      : step === "name"
        ? { word: named ? "Ready to continue" : "Name this key", detail: problem }
        : step === "key"
          ? { word: typed ? "Ready to add" : "Paste the key", detail: refusal !== "" ? "The key was not added" : "" }
          : { word: `Checking with ${vendorLabel}`, detail: "Listing models. No prompt is sent." };

  return (
    <Wizard
      className="fleet-add-key"
      icon={<KeyRound aria-hidden />}
      title="Add an API key"
      lead="Pay for Anthropic or OpenAI models with an account of your own."
      breadcrumbs={[{ label: "API keys", onSelect: onCancel }, { label: "Add an API key" }]}
      back={{ label: "API keys", onSelect: onCancel }}
      label="Adding an API key"
      steps={steps}
      open={step}
      onOpen={(next) => {
        if (!checking && ending === null && reached(next as Step) && next !== "check") setStep(next as Step);
      }}
      status={{ word: status.word, detail: status.detail, tone: checking ? "busy" : "none", meta: checking ? waited : undefined }}
      acts={acts}
      // The step and nothing about what was typed.
      context={{ page: "Add an API key", step }}
    />
  );
}
