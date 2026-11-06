import { useState } from "react";
import { AtSign } from "lucide-react";

import { Caption, Field, Input, Notice } from "../../kit";
import type { Act } from "../../kit/ActionBar";
import type { Stop } from "../../kit/Rail";
import { Wizard } from "../../kit/Wizard";
import { sendAs } from "../../apps/email/settings/AddSenderWizard";
import { useAzureEmailCall, type AzureEmailReply } from "./azureEmailSetup";

// ADD A SENDER ON AN AZURE DOMAIN -- the same steps as Email's record-only
// wizard (apps/email/settings/AddSenderWizard), for an organization whose mail
// Azure sends. It is the one place such a sender is added, from the domain's
// page or from Settings' Senders.
//
//   Address (on the domain) -> Display name -> Reply to (optional) -> Review
//
// ONE ACT. Add sender registers the address with Azure and records the sender
// in MemQL together, so there is never a sender Azure cannot send as. It does
// not move the default: the domain's page has Make default for that.
//
// THE ADD IS RETRY-SAFE. Azure keeps one username per address and MemQL one
// record per address, so an Add whose answer was lost is the same sender again.
// A press while one is on its way does nothing.

type Step = "address" | "name" | "reply" | "review";

/** The part before the @ that Azure accepts. The engine has the last word. */
const USERNAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/;

export function AddAzureSenderWizard({ accountId, planId, domain, registered, defaultSender, organization, connected, backLabel, onBack, onAdded }: {
  accountId: string;
  /** The plan the domain was set up from: the engine refuses an add made against another. */
  planId: string;
  /** The domain senders are added on. */
  domain: string;
  /** The addresses Azure already has on it. */
  registered: readonly string[];
  /** The default sender, which adding leaves where it is. */
  defaultSender: string;
  /** The organization's name: the display name to start from. */
  organization: string;
  /** Whether the cluster's Azure connection can register a sender now. */
  connected: boolean;
  /** Where Back and Cancel go: the page this was opened from. */
  backLabel: string;
  onBack: () => void;
  /** The domain as the cluster now has it, with the new sender. */
  onAdded: (reply: AzureEmailReply) => void;
}) {
  const { call, busy, error } = useAzureEmailCall(accountId);
  const [step, setStep] = useState<Step>("address");
  const [username, setUsername] = useState("");
  const [fromName, setFromName] = useState(organization);
  const [replyTo, setReplyTo] = useState("");

  const address = `${username.trim()}@${domain}`;
  const taken = registered.some(known => known.toLowerCase() === address.toLowerCase());
  const addressed = USERNAME.test(username.trim()) && !taken;
  const named = fromName.trim() !== "";
  const reached = (candidate: Step) => candidate === "address" || (candidate === "name" ? addressed : addressed && named);
  const order: Step[] = ["address", "name", "reply", "review"];
  const stateOf = (candidate: Step): Stop["state"] =>
    candidate === step ? "open" : reached(candidate) ? (order.indexOf(candidate) < order.indexOf(step) ? "done" : "waiting") : "ahead";

  async function add() {
    const reply = await call("sender", { username: username.trim(), displayName: fromName.trim(), replyTo: replyTo.trim(), planId }, "", "Add sender");
    if (reply) onAdded(reply);
  }

  const steps: Stop[] = [
    {
      id: "address",
      name: "Address",
      state: stateOf("address"),
      sentence: "The address mail is sent from.",
      answer: addressed ? address : undefined,
      body: (
        <Field label="Address">
          <Input id="email-azure-sender-address" label="Address" value={username} placeholder="news" onChange={setUsername} onEnter={() => { if (addressed) setStep("name"); }} />
          <span className="os-caption">@{domain}</span>
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
          <Input id="email-azure-sender-name" label="Display name" value={fromName} onChange={setFromName} onEnter={() => { if (named) setStep("reply"); }} />
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
          <Input id="email-azure-sender-reply" label="Reply to" value={replyTo} placeholder={`help@${domain}`} onChange={setReplyTo} onEnter={() => { if (replyTo.trim() !== "") setStep("review"); }} />
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
            {`Adding it registers the address with Azure and records the sender here.${defaultSender ? ` ${defaultSender} stays the default.` : ""}`}
          </Caption>
        </div>
      ),
    },
  ];

  // THE WAY OUT FIRST AND QUIET, the forward act last. Nothing is written
  // before Review, so Cancel is simply going.
  const acts: Act[] = [{ label: "Cancel", text: true, onAct: onBack }];
  if (step === "address" && addressed) acts.push({ label: "Next", tone: "primary", onAct: () => setStep("name") });
  if (step === "name" && named) acts.push({ label: "Next", tone: "primary", onAct: () => setStep("reply") });
  if (step === "reply") {
    // Skip lets go of anything typed; Next keeps it.
    acts.push(
      { label: "Skip", text: true, onAct: () => { setReplyTo(""); setStep("review"); } },
      { label: "Next", tone: "primary", onAct: () => setStep("review") },
    );
  }
  // Azure is asked through the cluster's connection: without it, there is no add to offer.
  if (step === "review" && connected) acts.push({ label: "Add sender", tone: "primary", busy, onAct: () => void add() });

  const word =
    step === "address" ? (taken ? "Already a sender on this domain" : "Enter the address")
    : step === "name" ? "Enter the display name"
    : step === "reply" ? "Reply to is optional"
    : busy ? "Adding the sender" : connected ? "Ready to add" : "Cluster setup required";

  return (
    <Wizard
      icon={<AtSign size={20} aria-hidden />}
      title="Add a sender"
      breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: "Add a sender" }]}
      back={{ label: backLabel, onSelect: onBack }}
      label="Adding a sender"
      steps={steps}
      open={step}
      onOpen={(next) => { if (reached(next as Step)) setStep(next as Step); }}
      status={{ word, tone: "none" }}
      acts={acts}
      notices={
        error !== "" ? <Notice tone="error" sentence="The sender wasn't added." next="What you entered is still here." detail={error} />
        : !connected ? <Notice sentence="Set up the cluster's Azure configuration before adding a sender." />
        : null
      }
      context={{ page: "Add a sender", step }}
    />
  );
}
