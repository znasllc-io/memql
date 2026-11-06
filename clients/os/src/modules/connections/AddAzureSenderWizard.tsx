import { useRef, useState } from "react";
import { AtSign } from "lucide-react";

import { Caption, Field, Input, Notice } from "../../kit";
import { Wizard } from "../../kit/Wizard";
import { useAzureEmailCall } from "./azureEmailSetup";
import type { AzureEmailReply } from "./azureEmailReply";
import { senderSteps, type SenderStep } from "./senderSteps";

// ADD A SENDER ON AN AZURE DOMAIN -- the steps every Add a sender shares
// (senderSteps), for an organization whose mail Azure sends from a domain of its
// own. It is the one place such a sender is added, from the domain's page or
// from Settings' Senders.
//
//   Address (on the domain) -> Display name -> Reply to (optional) -> Review
//
// ONE ACT. Add sender registers the address with Azure and records the sender
// in MemQL together. A sender the organization already recorded at the address
// is taken over rather than recorded twice, and carries the display name Azure
// holds, which is the only one Azure sends with. It does not move the default:
// the domain's page has Make default for that.
//
// AN ADDRESS ALREADY RECORDED STARTS FROM WHAT IT HOLDS. Its display name and
// reply address fill the next steps, and Review says that this updates that
// sender and what changes, because the sender gets exactly what Review says:
// a reply address let go of is removed, and nothing is changed unsaid.
//
// REGISTER WITH AZURE is the same act opened from such a recorded sender, with
// its address, display name and reply address already given.
//
// THE ADD IS RETRY-SAFE. Azure keeps one username per address and MemQL one
// record per address, so an Add whose answer was lost is the same sender again.
// A press while one is on its way does nothing.

/** The part before the @ that Azure accepts. The engine has the last word. */
const USERNAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/;

/** A sender already recorded, whose address, name and reply address the wizard starts from. */
export interface RecordedSender { username: string; fromName: string; replyTo: string }

export function AddAzureSenderWizard({ accountId, planId, domain, registered, defaultSender, organization, connected, recorded, recordedAt = {}, backLabel, onBack, onAdded }: {
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
  /** Opened from a recorded sender Azure does not have: register that one. */
  recorded?: RecordedSender | null;
  /** What the organization has recorded at each address (lower case): what adding one updates. */
  recordedAt?: Readonly<Record<string, { fromName: string; replyTo: string }>>;
  /** Where Back and Cancel go: the page this was opened from. */
  backLabel: string;
  onBack: () => void;
  /** The domain as the cluster now has it, with the new sender. */
  onAdded: (reply: AzureEmailReply) => void;
}) {
  const { call, busy, error } = useAzureEmailCall(accountId);
  const registering = !!recorded;
  const words = registering
    ? { title: "Register with Azure", act: "Register", busyWord: "Registering", ready: "Ready to register", line: "Check, then register.", refused: "The sender wasn't registered." }
    : { title: "Add a sender", act: "Add sender", busyWord: "Adding the sender", ready: "Ready to add", line: "Check, then add.", refused: "The sender wasn't added." };
  const [username, setUsername] = useState(recorded?.username ?? "");
  const [fromName, setFromName] = useState(recorded?.fromName ?? organization);
  const [replyTo, setReplyTo] = useState(recorded?.replyTo ?? "");

  // Addresses are one case: Azure and MemQL keep a sender's in lower case.
  const address = `${username.trim().toLowerCase()}@${domain}`;
  const taken = registered.some(known => known.toLowerCase() === address.toLowerCase());
  const addressed = USERNAME.test(username.trim()) && !taken;
  // A recorded sender has every answer already: it opens on Review.
  const [step, setStep] = useState<SenderStep>(registering && addressed && fromName.trim() !== "" ? "review" : "address");
  // Leaving the address for an address already recorded, the next steps start
  // from that sender's display name and reply address (once per address).
  const existing = recordedAt[address.toLowerCase()];
  const startedFrom = useRef(registering ? address.toLowerCase() : "");
  const goTo = (next: SenderStep) => {
    if (step === "address" && next !== "address" && existing && startedFrom.current !== address.toLowerCase()) {
      startedFrom.current = address.toLowerCase();
      setFromName(existing.fromName);
      setReplyTo(existing.replyTo);
    }
    setStep(next);
  };
  const changes: string[] = [];
  if (existing) {
    if (fromName.trim() !== existing.fromName.trim()) changes.push(`its display name becomes “${fromName.trim()}”`);
    const was = existing.replyTo.trim(), now = replyTo.trim();
    if (was !== now) changes.push(now === "" ? `its reply address, ${was}, is removed` : was === "" ? `it replies to ${now}` : `its reply address becomes ${now}`);
  }
  const caption = existing
    ? `${address} is already a sender here: this registers it with Azure${changes.length ? `, and ${changes.join(", and ")}` : " and keeps its display name and reply address"}.`
    : registering ? "Registering adds the address to Azure under this display name, the one every send then uses."
    : "Adding it registers the address with Azure and records the sender here.";

  async function add() {
    const reply = await call("sender", { username: username.trim().toLowerCase(), displayName: fromName.trim(), replyTo: replyTo.trim(), planId }, "", words.act);
    if (reply) onAdded(reply);
  }

  const { steps, acts, word, reached } = senderSteps({
    step,
    go: goTo,
    address: {
      value: address,
      answered: addressed,
      word: taken ? "Already a sender on this domain" : "Enter the address",
      body: (
        <Field label="Address">
          <Input id="email-azure-sender-address" label="Address" value={username} placeholder="news" onChange={setUsername} onEnter={() => { if (addressed) goTo("name"); }} />
          <span className="os-caption">@{domain}</span>
        </Field>
      ),
    },
    fromName,
    onFromName: setFromName,
    replyTo,
    onReplyTo: setReplyTo,
    replyPlaceholder: `help@${domain}`,
    reviewLine: words.line,
    review: <Caption>{`${caption}${defaultSender ? ` ${defaultSender} stays the default.` : ""}`}</Caption>,
    // Azure is asked through the cluster's connection: without it, there is no add to offer.
    add: connected ? { label: words.act, tone: "primary", busy, onAct: () => void add() } : null,
    reviewWord: busy ? words.busyWord : connected ? words.ready : "Cluster setup required",
    onCancel: onBack,
    idPrefix: "email-azure-sender",
  });

  return (
    <Wizard
      icon={<AtSign size={20} aria-hidden />}
      title={words.title}
      breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: words.title }]}
      back={{ label: backLabel, onSelect: onBack }}
      label="Adding a sender"
      steps={steps}
      open={step}
      onOpen={(next) => { if (reached(next as SenderStep)) goTo(next as SenderStep); }}
      status={{ word, tone: "none" }}
      acts={acts}
      notices={
        error !== "" ? <Notice tone="error" sentence={words.refused} next="What you entered is still here." detail={error} />
        : !connected ? <Notice sentence="Set up the cluster's Azure configuration before adding a sender." />
        : null
      }
      context={{ page: words.title, step }}
    />
  );
}
