import { useState } from "react";
import { Store } from "lucide-react";

import { Field, Input, Notice } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { addBuyerWords, type WholesaleSettings } from "./model";
import { useWholesaleChecks, type Found, type WholesaleScope, type WholesaleWrites } from "./useWholesale";

// A PERSON OF THE STORE ADDS A BUYER BY HAND (wholesaleAddBuyer), through the
// kit's Wizard (DESIGN.md, "One wizard for adding"): one question a step --
// the Company, the Buyer's name, their Email -- and the forward act on the
// floor. The buyer skips nothing: the engine writes one application on their
// behalf, which then goes the way an applicant's does.
//
// WHAT IT WILL DO IS THE ENGINE'S ANSWER (addBuyerWords): whether the store
// requires its terms, and whether the terms email would be sent now, with the
// engine's sentence when it would not. The lead says it, and the last act
// names it.
//
// Nothing is written until the last act is pressed. A refusal -- an address
// that already has an application on the store, say -- is the engine's
// sentence on the Email step, every answer kept.
//
// A LOST ANSWER IS NOT PRESSED AGAIN BLIND. When the connection drops before
// the engine answers, the wizard asks the store whether it now holds an
// application for the address, and says which it found: the buyer is there
// (Done opens them), or not (the act is offered again), or the store could not
// be read (Check again). FROM THE PRESS UNTIL THE CHECK ENDS THERE IS NO
// FORWARD ACT TO PRESS: the write's own act is busy, then the check is the
// cluster's turn, and only Cancel stands. It comes back only once the store
// is known not to hold the buyer.

type Step = "company" | "buyer" | "email";
const ORDER: readonly Step[] = ["company", "buyer", "email"];

/** What the wizard learned after a lost answer. */
type Settled = { found: Found; applicationId?: string } | null;

export function AddBuyerPage({ scope, settings, writes, trail, back, onAdded }: {
  scope: WholesaleScope;
  /** The store's settings as last read: what adding will do. */
  settings: WholesaleSettings;
  writes: WholesaleWrites;
  /** The trail down to the Buyers tab: the wizard is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  /** The new application, by the id the engine gave it. */
  onAdded: (applicationId: string, companyName: string) => void;
}) {
  const { findBuyer } = useWholesaleChecks(scope);
  const [step, setStep] = useState<Step>("company");
  const [companyName, setCompanyName] = useState("");
  const [applicantName, setApplicantName] = useState("");
  const [applicantEmail, setApplicantEmail] = useState("");
  const [refusal, setRefusal] = useState("");
  const [settled, setSettled] = useState<Settled>(null);
  // The press's own life: the write on its way, then the check of a lost answer.
  const [phase, setPhase] = useState<"idle" | "writing" | "checking">("idle");
  const adding = phase === "writing";
  const checking = phase === "checking";
  const words = addBuyerWords(settings);

  const answered: Record<Step, boolean> = {
    company: companyName.trim() !== "",
    buyer: applicantName.trim() !== "",
    email: applicantEmail.trim() !== "",
  };
  const reached = (candidate: Step) => ORDER.slice(0, ORDER.indexOf(candidate)).every((s) => answered[s]);
  const stateOf = (candidate: Step): Stop["state"] =>
    candidate === step ? "open" : !reached(candidate) ? "ahead" : answered[candidate] ? "done" : "waiting";
  const go = (next: Step) => { if (reached(next)) setStep(next); };

  async function check() {
    setPhase("checking");
    setSettled(await findBuyer(applicantEmail));
    setPhase("idle");
  }

  async function add() {
    setRefusal("");
    setSettled(null);
    setPhase("writing");
    const { outcome, applicationId, sentence } = await writes.addBuyer({ companyName, applicantName, applicantEmail });
    if (outcome === "done" && applicationId !== undefined) {
      onAdded(applicationId, companyName.trim());
      return;
    }
    if (outcome === "unknown") {
      await check();
      return;
    }
    setPhase("idle");
    setRefusal(sentence);
    setStep("email");
  }

  const settledSentence = settled === null ? "" :
    settled.found === "there" ? "The connection dropped before the cluster answered. The buyer is there: the store holds an application for this address."
    : settled.found === "absent" ? "The connection dropped before the cluster answered. The buyer is not there: nothing was added."
    : "The connection dropped before the cluster answered, and what the store holds could not be read. Check again before adding.";

  const field = (id: string, label: string, value: string, set: (v: string) => void, after: Step | null, type: "text" | "email" = "text") => (
    <Field label={label}>
      <Input id={id} label={label} type={type} value={value} onChange={(v) => { setRefusal(""); set(v); }}
        onEnter={() => { if (value.trim() !== "" && after !== null) go(after); }} />
    </Field>
  );

  const steps: Stop[] = [
    { id: "company", name: "Company", state: stateOf("company"), sentence: "The business buying at trade prices.", answer: answered.company ? companyName.trim() : undefined,
      body: field("wholesale-buyer-company", "Company", companyName, setCompanyName, "buyer") },
    { id: "buyer", name: "Buyer", state: stateOf("buyer"), sentence: "The person at the business.", answer: answered.buyer ? applicantName.trim() : undefined,
      body: field("wholesale-buyer-name", "Name", applicantName, setApplicantName, "email") },
    { id: "email", name: "Email", state: stateOf("email"), sentence: "Where their emails go.", answer: answered.email ? applicantEmail.trim() : undefined,
      body: (
        <>
          {field("wholesale-buyer-email", "Email", applicantEmail, setApplicantEmail, null, "email")}
          {/* THE ENGINE'S REFUSAL, on the step it answers. */}
          {refusal ? <Notice tone="error" sentence={refusal} /> : null}
          {settledSentence ? <Notice tone={settled?.found === "there" ? "info" : "warn"} sentence={settledSentence} /> : null}
        </>
      ) },
  ];

  // THE WAY OUT FIRST AND QUIET, the one forward act last. Nothing is written
  // before the last act, so Cancel is simply going.
  const cancel: Act = { label: "Cancel", text: true, onAct: back.onSelect };
  const acts: Act[] =
    checking ? [cancel]
    : settled?.found === "there" ? [{ label: "Done", tone: "primary", onAct: () => { if (settled.applicationId) onAdded(settled.applicationId, companyName.trim()); else back.onSelect(); } }]
    : settled?.found === "unread" ? [cancel, { label: "Check again", tone: "primary", onAct: () => void check() }]
    : step === "company" ? [cancel, ...(answered.company ? [{ label: "Continue", tone: "primary" as const, onAct: () => go("buyer") }] : [])]
    : step === "buyer" ? [cancel, ...(answered.buyer ? [{ label: "Continue", tone: "primary" as const, onAct: () => go("email") }] : [])]
    : [cancel, ...(answered.email && reached("email") ? [{ label: words.act, tone: "primary" as const, busy: adding, onAct: () => void add() }] : [])];

  const word = adding ? "Adding the buyer"
    : checking ? "Reading the store"
    : settled?.found === "there" ? "Added"
    : step === "company" ? "Enter the company"
    : step === "buyer" ? "Enter their name"
    : answered.email ? "Ready to add" : "Enter their address";

  return (
    <Wizard
      icon={<Store size={20} aria-hidden />}
      title="Add buyer"
      lead={words.lead}
      breadcrumbs={[...trail, { label: "Add buyer" }]}
      back={back}
      label="Adding a buyer"
      steps={steps}
      open={step}
      onOpen={(next) => go(next as Step)}
      status={{ word, tone: checking ? "busy" : "none" }}
      acts={acts}
      context={{ page: "Wholesale add buyer", step }}
    />
  );
}
