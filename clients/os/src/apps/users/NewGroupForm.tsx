import { useRef, useState } from "react";
import { UsersRound } from "lucide-react";
import { Fact, Facts, Field, Input } from "../../kit";
import { Wizard } from "../../kit/Wizard";
import type { Act } from "../../kit/ActionBar";
import type { Stop } from "../../kit/Rail";
import { AccountPicker } from "../accounts/AccountPicker";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import { accountNameFrom, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { RefusalLine } from "./PersonPage";

export function NewGroupForm({ accounts, actions, onCancel, onCreated }: {
  accounts: readonly AccountRow[];
  actions: UsersActions;
  onCancel: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [accountId, setAccountId] = useState("");
  const [step, setStep] = useState(0);
  const [furthest, setFurthest] = useState(0);
  const saving = useRef(false);
  const defaultAccountId = useDefaultOrganization(accounts);
  const selectedAccountId = accountId || defaultAccountId;
  const named = name.trim() !== "";
  const ready = named && organizationChosen(accounts, selectedAccountId);
  const busy = actions.busyKey === `group:new:${name}`;
  function go(index: number) { setStep(index); setFurthest(held => Math.max(held, index)); }
  async function create() {
    if (!ready || saving.current) return;
    saving.current = true;
    try { if (await actions.groupCreate(name.trim(), description.trim(), selectedAccountId) !== null) onCreated(); }
    finally { saving.current = false; }
  }
  const steps: Stop[] = [
    { id: "name", sentence: "What to call it", name: "Name", state: "ahead", answer: name.trim(), body: <Field label="Name"><Input id="group-name" label="Name" value={name} onChange={setName} placeholder="Acme engineering" onEnter={() => { if (named) go(1); }} /></Field> },
    { id: "organization", sentence: "Whose group it is", name: "Organization", state: "ahead", answer: accountNameFrom(accounts, selectedAccountId), body: <Field label="Organization"><AccountPicker value={selectedAccountId} onChange={value => { setAccountId(value); go(2); }} accounts={[...accounts]} id="group-account" label="Organization this group belongs to" required /></Field> },
    { id: "purpose", sentence: "What it is for, if you like", name: "Purpose", state: "ahead", answer: description.trim() || "Optional", body: <Field label="What it is for"><Input id="group-description" label="What it is for" value={description} onChange={setDescription} placeholder="Optional" onEnter={() => go(3)} /></Field> },
    { id: "review", sentence: "Check, then create", name: "Review", state: "ahead", body: <Facts><Fact label="Group" value={name.trim()} /><Fact label="Organization" value={accountNameFrom(accounts, selectedAccountId)} /><Fact label="Purpose" value={description.trim() || "Not added"} /></Facts> },
  ].map((stop, index) => ({ ...stop, state: index === step ? "open" : index < step ? "done" : index <= furthest ? "waiting" : "ahead" }));
  const acts: Act[] = busy ? [] : [{ label: "Cancel", text: true, onAct: onCancel }];
  if (step === 3 && ready) acts.push({ label: "Create group", tone: "primary", busy, onAct: () => void create() });
  else if (!busy && (step === 0 ? named : ready)) acts.push({ label: "Next", tone: "primary", onAct: () => go(step + 1) });
  return <Wizard icon={<UsersRound size={20} aria-hidden />} title="New group" label="A new group" back={{ label: "Groups", onSelect: onCancel }} steps={steps} open={steps[step]!.id} onOpen={id => { const index = steps.findIndex(stop => stop.id === id); if (!busy && index >= 0 && index <= furthest) setStep(index); }} status={{ word: busy ? "Creating group" : step === 3 ? "Ready to create" : steps[step]!.name, tone: busy ? "busy" : "none" }} acts={acts} notices={<RefusalLine actions={actions} />} />;
}
