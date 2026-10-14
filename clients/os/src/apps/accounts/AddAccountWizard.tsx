import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import { useRef, useState } from "react";
import { Building2 } from "lucide-react";
import { Caption, Fact, Facts, Field, Input, Notice } from "../../kit";
import type { Act } from "../../kit/ActionBar";
import type { Stop } from "../../kit/Rail";
import { Wizard } from "../../kit/Wizard";
import { normalizeAccountDomain } from "./defaults";
import type { CreateAccountState } from "./actions";

export function AddAccountWizard({ create, onBack, onDone }: { create: CreateAccountState; onBack: () => void; onDone: (id: string) => void }) {
  const [accountId] = useState(newShortId);
  const [name, setName] = useState("");
  const [domain, setDomain] = useState("");
  const [primaryContactName, setContactName] = useState("");
  const [primaryContactEmail, setContactEmail] = useState("");
  const [step, setStep] = useState("name");
  const [furthest, setFurthest] = useState(0);
  const saving = useRef(false);
  const named = name.trim() !== "";
  const validDomain = domain.trim() === "" || normalizeAccountDomain(domain) !== "";
  const validContact = primaryContactEmail.trim() === "" || /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(primaryContactEmail.trim());
  const order = ["name", "domain", "contact", "review"];
  const reachable = (id: string) => order.indexOf(id) <= furthest && (id === "name" || named && (id === "domain" || validDomain && (id === "contact" || validContact)));
  const next = () => { const index = order.indexOf(step) + 1; setFurthest(held => Math.max(held, index)); setStep(order[index]!); };
  async function submit() {
    if (saving.current) return;
    saving.current = true;
    try {
      const id = await create.create({ name, domain: normalizeAccountDomain(domain), primaryContactName, primaryContactEmail, notes: "" }, accountId);
      if (id) onDone(id);
    } finally { saving.current = false; }
  }
  const bodies = {
    name: <Field label="Client name"><Input id="os-account-new-name" label="Client name" value={name} onChange={setName} placeholder="Acme" onEnter={() => { if (named) next(); }} /></Field>,
    domain: <div className="os-app-stack"><Field label="Domain"><Input id="os-account-new-domain" label="Client domain" value={domain} onChange={setDomain} placeholder="example.com" onEnter={() => { if (validDomain) next(); }} /></Field><Caption>{validDomain ? "Optional. New apps and email setup will use this domain. You can assign more domains later." : "Enter a domain such as example.com, without a URL or wildcard."}</Caption></div>,
    contact: <div className="os-app-stack"><Field label="Contact name"><Input id="os-account-new-contact" label="Primary contact name" value={primaryContactName} onChange={setContactName} /></Field><Field label="Contact email"><Input id="os-account-new-email" label="Primary contact email" value={primaryContactEmail} onChange={setContactEmail} /></Field><Caption>{validContact ? "Optional. Contact details can be added later." : "Enter an email address such as hello@example.com."}</Caption></div>,
    review: <Facts><Fact label="Client" value={name.trim()} /><Fact label="Domain" value={normalizeAccountDomain(domain) || "Not assigned"} /><Fact label="Contact" value={[primaryContactName, primaryContactEmail].filter(Boolean).join(" · ") || "Not added"} /></Facts>,
  };
  const steps: Stop[] = order.map(id => ({ id, name: ({ name: "Name", domain: "Domain", contact: "Contact", review: "Review" } as Record<string, string>)[id]!, state: id === step ? "open" : reachable(id) ? order.indexOf(id) < order.indexOf(step) ? "done" : "waiting" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const acts: Act[] = create.busy ? [] : [{ label: "Cancel", text: true, onAct: onBack }];
  if (step === "review") acts.push({ label: "Add client", tone: "primary", busy: create.busy, onAct: () => void submit() });
  else if (!create.busy && named && (step !== "domain" || validDomain) && (step !== "contact" || validContact)) acts.push({ label: "Next", tone: "primary", onAct: next });
  return <Wizard icon={<Building2 size={20} aria-hidden />} title="Add a client" label="Adding a client" back={{ label: "Accounts", onSelect: onBack }} steps={steps} open={step} onOpen={id => { if (!create.busy && reachable(id)) setStep(id); }} status={{ word: create.busy ? "Adding client" : step === "review" ? "Ready to add" : step === "name" ? "Name the client" : "Optional", tone: create.busy ? "busy" : "none" }} acts={acts} notices={create.error ? <Notice tone="error" sentence="The client was not added." detail={create.error} /> : null} />;
}
