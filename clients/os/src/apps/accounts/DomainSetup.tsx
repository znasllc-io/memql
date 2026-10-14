import { roleAdmits } from "../../system/roles";
import { useRef, useState } from "react";
import { Globe } from "lucide-react";
import { Button, Caption, Chip, ContentSkeleton, Fact, Facts, Field, Input, Notice, Panel, Select, Subhead } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import type { Act } from "../../kit/ActionBar";
import type { Stop } from "../../kit/Rail";
import { Wizard } from "../../kit/Wizard";
import { useSession } from "../../chrome/access";
import { useSenderFeed } from "../email/useEmail";
import { senderIdentityFromRow } from "../email/rows";
import { useConfigureAccountDomains } from "./actions";
import { accountDefaultDomain, accountDomains, normalizeAccountDomain } from "./defaults";
import { accountName, type AccountRow } from "./rows";

export function AccountDomainsPanel({ account, onConfigure }: { account: AccountRow; onConfigure: () => void }) {
  const { access } = useSession();
  const domains = accountDomains(account);
  const editable = account.status !== "archived" && roleAdmits(access?.role ?? "", { min: "admin" });
  return <Panel label="Domains">
    <Subhead actions={editable ? domains.length ? <Button tone="quiet" onClick={onConfigure}>Edit</Button> : <AddButton label="Assign domains" onClick={onConfigure} /> : undefined}>Domains</Subhead>
    {domains.length ? <Facts>{domains.map(domain => <Fact key={domain} label={domain} value={domain === accountDefaultDomain(account) ? <Chip tone="accent">default</Chip> : "Assigned"} />)}</Facts> : <Caption>No domains assigned.</Caption>}
    <Caption>New apps and email setup use these defaults. Existing addresses stay as they are.</Caption>
  </Panel>;
}

export function DomainSetup({ account, onBack, onSaved }: { account: AccountRow; onBack: () => void; onSaved: () => void }) {
  const save = useConfigureAccountDomains();
  const saving = useRef(false);
  const feed = useSenderFeed();
  const senders = feed.snapshot.rows.map(senderIdentityFromRow).filter(sender => sender.accountId === account.id && sender.status === "active");
  const [domains, setDomains] = useState(() => accountDomains(account));
  const [domain, setDomain] = useState("");
  const [selected, setSelected] = useState(() => accountDefaultDomain(account));
  const [sender, setSender] = useState(account.defaultSenderIdentityId ?? "");
  const [step, setStep] = useState("domains");
  const [furthest, setFurthest] = useState(0);
  const normalized = normalizeAccountDomain(domain);
  const pending = domain.trim() !== "";
  const valid = !pending || normalized !== "" && (domains.includes(normalized) || domains.length < 32);
  const nextDomains = pending && normalized && !domains.includes(normalized) ? [...domains, normalized] : domains;
  const defaultDomain = nextDomains.includes(selected) ? selected : nextDomains[0] ?? "";
  const senderKnown = feed.snapshot.state === "live";
  const senderValid = sender === "" || senders.some(candidate => candidate.id === sender);
  const order = ["domains", "default", "sender", "review"];
  const reached = (id: string) => order.indexOf(id) <= furthest && (id === "domains" || valid && (id !== "review" || senderValid));
  function go(id: string) { setFurthest(held => Math.max(held, order.indexOf(id))); setStep(id); }
  function forward() {
    setDomains(nextDomains); setDomain(""); setSelected(defaultDomain);
    go(order[order.indexOf(step) + 1]!);
  }
  async function submit() {
    if (saving.current) return;
    saving.current = true;
    try { if (await save.save(account.id, nextDomains, defaultDomain, sender)) onSaved(); }
    finally { saving.current = false; }
  }
  const bodies = {
    domains: <div className="os-app-stack">
      {domains.map(name => <div className="os-account-profile-head" key={name}><span>{name}</span><Button tone="quiet" ariaLabel={`Remove ${name}`} onClick={() => setDomains(held => held.filter(value => value !== name))}>Remove</Button></div>)}
      <Field label="Domain"><Input id="account-assigned-domain" label="Domain" value={domain} onChange={setDomain} placeholder="example.com" onEnter={() => { if (valid) forward(); }} /></Field>
      {pending && !valid ? <Caption>Enter a domain such as example.com, without a URL or wildcard. Up to 32 domains.</Caption> : <Caption>Use a domain you own. DNS and email verification happen when you set up each service.</Caption>}
      {pending && valid ? <Button tone="quiet" onClick={() => { setDomains(nextDomains); setSelected(defaultDomain); setDomain(""); }}>Add another domain</Button> : null}
    </div>,
    default: nextDomains.length ? <Field label="Default domain"><Select id="account-default-domain" label="Default domain" value={defaultDomain} onChange={value => { setSelected(value); go("sender"); }}>{nextDomains.map(name => <option key={name} value={name}>{name}</option>)}</Select></Field> : <p>New apps and email setup will ask for a domain.</p>,
    sender: <div className="os-app-stack">
      {feed.snapshot.error ? <Notice tone="error" sentence="Senders could not be loaded." detail={feed.snapshot.error}><Button onClick={feed.reseed}>Try again</Button></Notice> : !senderKnown ? <ContentSkeleton kind="form" label="Loading senders" /> : null}
      <Field label="Default sender"><Select id="account-default-sender" label="Default sender" value={sender} onChange={value => { setSender(value); go("review"); }}>
        <option value="">Choose automatically</option>
        {sender && !senderValid ? <option value={sender}>Unavailable sender</option> : null}
        {senders.map(value => <option key={value.id} value={value.id}>{value.fromName ? `${value.fromName} · ` : ""}{value.address}</option>)}
      </Select></Field>
      <Caption>Used by new campaigns, automations and signup confirmations. Automatic selection uses the only matching sender; otherwise each flow asks you to choose.</Caption>
      {senderKnown && !senders.length ? <Caption>Add a sender in Email → Settings after setting up your sending domain.</Caption> : null}
    </div>,
    review: <div className="os-app-stack"><Facts>
      <Fact label="Account" value={accountName(account)} />
      <Fact label="Domains" value={nextDomains.join(", ") || "None"} />
      <Fact label="Default domain" value={defaultDomain || "None"} />
      <Fact label="Default sender" value={senders.find(value => value.id === sender)?.address || "Automatic"} />
    </Facts><Caption>These defaults apply to new work. Domain assignment does not enable automatic user access.</Caption></div>,
  };
  const steps: Stop[] = order.map(id => ({ id, name: ({ domains: "Domains", default: "Default domain", sender: "Sender", review: "Review" } as Record<string, string>)[id]!, state: id === step ? "open" : reached(id) ? order.indexOf(id) < order.indexOf(step) ? "done" : "waiting" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const acts: Act[] = save.busy ? [] : [{ label: "Cancel", text: true, onAct: onBack }];
  if (step === "review" && valid && senderValid) acts.push({ label: "Save defaults", tone: "primary", busy: save.busy, onAct: () => void submit() });
  else if (!save.busy && step !== "review" && valid && (step !== "sender" || senderValid)) acts.push({ label: "Next", tone: "primary", onAct: forward });
  return <Wizard icon={<Globe size={20} aria-hidden />} title="Set up domains" label="Account domain setup" back={{ label: accountName(account), onSelect: onBack }} steps={steps} open={step} onOpen={id => { if (!save.busy && reached(id)) setStep(id); }} status={{ word: save.busy ? "Saving defaults" : step === "review" ? "Ready to save" : "Defaults for new work", tone: save.busy ? "busy" : "none" }} acts={acts} notices={save.error ? <Notice tone="error" sentence="Defaults were not saved." detail={save.error} /> : null} />;
}
