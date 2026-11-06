import { accountDefaultDomain } from "../../accounts/defaults";
import { useEffect, useRef, useState } from "react";
import { AtSign } from "lucide-react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Field, Input, Notice } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import { senderSteps, type SenderStep } from "../../../modules/connections/senderSteps";
import { AccountPicker } from "../../accounts/AccountPicker";
import type { AccountRow } from "../../accounts/rows";
import type { CampaignWrites } from "../actions";
import { organizationName } from "../organizations";

// ADD A SENDER -- on the shell's one wizard (DESIGN.md, "One wizard for adding").
//
//   Organization (only when there is a choice) -> Address -> Display name
//   -> Reply to (optional) -> Review
//
// THIS ONE ONLY RECORDS. The steps are the ones every Add a sender shares
// (modules/connections/senderSteps). An owner or developer whose organization
// sends through an Azure domain of its own adds senders through that domain's
// Add a sender instead, which registers the address with Azure as it records
// the sender. Everyone else, and every organization without such a domain,
// records here, and what is recorded is not proof that a provider can send as
// it: a sender a send would be refused as says so in Settings' Senders, for
// anyone who may send, and an automation that sends as it cannot be turned on.
//
// THE ADD IS RETRY-SAFE. The sender's id is minted when the wizard opens and
// held for as long as it is open, so an Add whose answer was lost, or whose
// refusal the person fixed, is tried again as the same sender -- never a second
// one the person then has to find and archive. A press while one is in flight
// does nothing.

/** `news@acme.com` and nothing grander: one @, text either side, no spaces. The engine has the last word. */
const looksLikeAddress = (value: string) => /^[^\s@]+@[^\s@]+$/.test(value.trim());

export function AddSenderWizard({ organizations, initialAccountId, writes, azureOwnDomain = "", onBack, onCreated }: {
  /** The organizations a person can choose between. One or none means nothing to ask. */
  organizations: AccountRow[];
  /** The organization the page is on: the answer to the first question, already given. */
  initialAccountId: string;
  writes: CampaignWrites;
  /**
   * The one sender of an organization whose mail goes out through Azure's own
   * domain, which registers no other, when the page knows it: Review says so.
   */
  azureOwnDomain?: string;
  /** Leave without adding. */
  onBack: () => void;
  onCreated: (senderId: string) => void;
}) {
  const choosing = organizations.length > 1;
  const create = writes.createSender;
  const [senderIdentityId] = useState(newShortId);
  const [step, setStep] = useState<SenderStep>("address");
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

  const { steps, acts, word, reached } = senderSteps({
    step,
    go: setStep,
    organization: choosing ? {
      id: "organization",
      name: "Organization",
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
    } : null,
    address: {
      value: address,
      answered: addressed,
      word: "Enter the address",
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
    fromName,
    onFromName: setFromName,
    replyTo,
    onReplyTo: setReplyTo,
    replyPlaceholder: "help@acme.com",
    reviewLine: "Check, then add.",
    review: (
      <Caption>
        {azureOwnDomain
          ? `This organization's mail goes out through Azure's own domain, which sends only as ${azureOwnDomain}. A sender recorded here cannot send until the organization has a sending domain of its own.`
          : "Adding a sender only records it here. Your email provider has to have the address, and has to allow this cluster to send as it."}
      </Caption>
    ),
    add: { label: "Add sender", tone: "primary", busy: create.busy, onAct: () => void add() },
    reviewWord: create.busy ? "Adding the sender" : "Ready to add",
    onCancel: onBack,
    idPrefix: "email-sender",
  });

  return (
    <Wizard
      icon={<AtSign size={20} aria-hidden />}
      title="Add a sender"
      breadcrumbs={[{ label: "Settings", onSelect: onBack }, { label: "Add a sender" }]}
      back={{ label: "Settings", onSelect: onBack }}
      label="Adding a sender"
      steps={steps}
      open={step}
      onOpen={(next) => { if (reached(next as SenderStep)) setStep(next as SenderStep); }}
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
