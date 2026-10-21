import { accountDefaultDomain } from "../../accounts/defaults";
import { useEffect, useRef, useState } from "react";
import { AtSign } from "lucide-react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Field, Input, Notice } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { AccountPicker } from "../../accounts/AccountPicker";
import type { AccountRow } from "../../accounts/rows";
import type { CampaignWrites } from "../actions";
import { organizationName } from "../organizations";

// ADD A SENDER -- on the shell's one wizard (DESIGN.md, "One wizard for adding").
//
//   Organization (only when there is a choice) -> Address -> Display name
//   -> Reply to (optional) -> Review
//
// One question a step. The forward act lives on the floor and nowhere else, and
// it is ABSENT until the step is answered: the words on the left say what is
// missing. Nothing is written until Review, so Cancel is simply going.
//
// THE ADD IS RETRY-SAFE. The sender's id is minted when the wizard opens and
// held for as long as it is open, so an Add whose answer was lost, or whose
// refusal the person fixed, is tried again as the same sender -- never a second
// one the person then has to find and archive. A press while one is in flight
// does nothing.

type Step = "organization" | "address" | "name" | "reply" | "review";

/** `news@acme.com` and nothing grander: one @, text either side, no spaces. The engine has the last word. */
const looksLikeAddress = (value: string) => /^[^\s@]+@[^\s@]+$/.test(value.trim());

/** The sentence Review says, which is also what the sender will do. */
function sendAs(fromName: string, address: string, replyTo: string): string {
  const base = `Send as ${fromName.trim()} <${address.trim()}>`;
  return replyTo.trim() === "" ? `${base}.` : `${base}, replies to ${replyTo.trim()}.`;
}

export function AddSenderWizard({ organizations, initialAccountId, writes, onBack, onCreated }: {
  /** The organizations a person can choose between. One or none means nothing to ask. */
  organizations: AccountRow[];
  /** The organization the page is on: the answer to the first question, already given. */
  initialAccountId: string;
  writes: CampaignWrites;
  /** Leave without adding. */
  onBack: () => void;
  onCreated: (senderId: string) => void;
}) {
  const choosing = organizations.length > 1;
  const create = writes.createSender;
  const [senderIdentityId] = useState(newShortId);
  const [step, setStep] = useState<Step>("address");
  const [accountId, setAccountId] = useState(initialAccountId);
  const [addressDraft, setAddress] = useState<string | null>(null);
  const domain = accountDefaultDomain(organizations.find(account => account.id === accountId));
  const address = addressDraft ?? (domain ? `news@${domain}` : "");
  const [nameDraft, setFromName] = useState<string | null>(null);
  const fromName = nameDraft ?? organizationName(organizations, accountId);
  const [replyTo, setReplyTo] = useState("");
  const adding = useRef(false);

  // A refusal from an earlier visit is not this one's.
  useEffect(() => { create.reset(); return () => create.reset(); }, []);

  const addressed = looksLikeAddress(address);
  const named = fromName.trim() !== "";
  const reached = (candidate: Step) =>
    candidate === "organization" ? choosing
    : candidate === "address" ? true
    : candidate === "name" ? addressed
    : addressed && named;
  const order: Step[] = ["organization", "address", "name", "reply", "review"];
  const stateOf = (candidate: Step): Stop["state"] =>
    candidate === step ? "open" : reached(candidate) ? (order.indexOf(candidate) < order.indexOf(step) ? "done" : "waiting") : "ahead";

  async function add() {
    // A second press while the first is on its way is the same press.
    if (adding.current) return;
    adding.current = true;
    try {
      const written = await create.create(senderIdentityId, { address, fromName, replyTo, accountId, notes: "" });
      if (written) onCreated(senderIdentityId);
    } finally {
      adding.current = false;
    }
  }

  const steps: Stop[] = [];
  if (choosing) {
    steps.push({
      id: "organization",
      name: "Organization",
      state: stateOf("organization"),
      sentence: "Whose email this sender sends.",
      answer: organizationName(organizations, accountId) || undefined,
      body: (
        <Field label="Organization">
          <AccountPicker
            id="email-sender-organization"
            label="Organization this sender is for"
            required
            accounts={organizations}
            value={accountId}
            // A step that is one choice is answered by choosing.
            onChange={(next) => { setAccountId(next); setAddress(null); setFromName(null); setReplyTo(""); setStep("address"); }}
          />
        </Field>
      ),
    });
  }
  steps.push(
    {
      id: "address",
      name: "Address",
      state: stateOf("address"),
      sentence: "The address mail is sent from.",
      answer: addressed ? address.trim() : undefined,
      body: (
        <Field label="Address">
          <Input
            id="email-sender-address"
            label="Address"
            value={address}
            placeholder="news@acme.com"
            onChange={setAddress}
            onEnter={() => { if (addressed) setStep("name"); }}
          />
        </Field>
      ),
    },
    {
      id: "name",
      name: "Display name",
      state: stateOf("name"),
      sentence: "The name recipients see.",
      answer: named ? fromName.trim() : undefined,
      body: (
        <Field label="Display name">
          <Input
            id="email-sender-name"
            label="Display name"
            value={fromName}
            placeholder="Acme News"
            onChange={setFromName}
            onEnter={() => { if (named) setStep("reply"); }}
          />
        </Field>
      ),
    },
    {
      id: "reply",
      name: "Reply to",
      state: stateOf("reply"),
      sentence: "Where replies go, if elsewhere.",
      answer: replyTo.trim() !== "" ? replyTo.trim() : step === "review" ? "Skipped" : undefined,
      body: (
        <Field label="Reply to">
          <Input
            id="email-sender-reply"
            label="Reply to"
            value={replyTo}
            placeholder="help@acme.com"
            onChange={setReplyTo}
            onEnter={() => { if (replyTo.trim() !== "") setStep("review"); }}
          />
        </Field>
      ),
    },
    {
      id: "review",
      name: "Review",
      sentence: "Check, then add.",
      state: stateOf("review"),
      body: (
        <div className="os-app-stack">
          <p>{sendAs(fromName, address, replyTo)}</p>
          <Caption>
            Adding a sender only records it here. Your email provider has to have the address, and has to allow this cluster to send as it.
          </Caption>
        </div>
      ),
    },
  );

  // THE WAY OUT FIRST AND QUIET, the forward act last. Nothing has been written
  // before Review, so Cancel is simply going.
  const acts: Act[] = [{ label: "Cancel", text: true, onAct: onBack }];
  if (step === "address" && addressed) acts.push({ label: "Next", tone: "primary", onAct: () => setStep("name") });
  if (step === "name" && named) acts.push({ label: "Next", tone: "primary", onAct: () => setStep("reply") });
  if (step === "reply") {
    // Reply to is the one question that may go unanswered. Skip is the quiet
    // way past it and lets go of anything typed; Next keeps what was.
    acts.push(
      { label: "Skip", text: true, onAct: () => { setReplyTo(""); setStep("review"); } },
      { label: "Next", tone: "primary", onAct: () => setStep("review") },
    );
  }
  if (step === "review") acts.push({ label: "Add sender", tone: "primary", busy: create.busy, onAct: () => void add() });

  const word =
    step === "organization" ? "Choose the organization"
    : step === "address" ? "Enter the address"
    : step === "name" ? "Enter the display name"
    : step === "reply" ? "Reply to is optional"
    : create.busy ? "Adding the sender" : "Ready to add";

  return (
    <Wizard
      icon={<AtSign size={20} aria-hidden />}
      title="Add a sender"
      breadcrumbs={[{ label: "Settings", onSelect: onBack }, { label: "Add a sender" }]}
      back={{ label: "Settings", onSelect: onBack }}
      label="Adding a sender"
      steps={steps}
      open={step}
      onOpen={(next) => { if (reached(next as Step)) setStep(next as Step); }}
      status={{ word, tone: "none" }}
      acts={acts}
      notices={
        create.error === "" ? null : (
          <Notice
            tone="error"
            sentence="The sender wasn't added."
            next="Nothing was written. What you entered is still here."
            detail={create.error}
          />
        )
      }
      context={{ page: "Add a sender", step }}
    />
  );
}
