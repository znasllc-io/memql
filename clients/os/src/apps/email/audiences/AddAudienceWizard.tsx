import { useRef, useState } from "react";
import { Users } from "lucide-react";
import { newShortId, refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { ChoiceStack, Field, Input, Notice, ContentSkeleton } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { useDefaultOrganization } from "../../accounts/organization";
import { organizationName, selectableOrganizations } from "../organizations";
import type { CampaignWrites } from "../actions";

/** What each step is, under its name on the rail. */
const AUDIENCE_SUBTITLES: Readonly<Record<string, string>> = { organization: "Whose audience it is", name: "What to call it", confirmation: "Whether people confirm first", review: "Check, then add" };

export function AddAudienceWizard({ writes, onBack, onDone }: { writes: CampaignWrites; onBack: () => void; onDone: (id: string) => void }) {
  const { accounts, state } = useAccountOptionsFeed();
  const choices = selectableOrganizations(accounts);
  const fallback = useDefaultOrganization(accounts);
  const [picked, setPicked] = useState("");
  const accountId = picked || fallback || (choices.length === 1 ? choices[0]!.id : "");
  const [audienceId] = useState(newShortId);
  const [step, setStep] = useState("name");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [confirmation, setConfirmation] = useState<"none" | "required">("none");
  const working = useRef(false);
  const open = !accountId ? "organization" : step;
  const order = [...(choices.length !== 1 ? ["organization"] : []), "name", "confirmation", "review"];
  const bodies = {
    organization: state === "seeding" ? <ContentSkeleton kind="form" /> : <ChoiceStack name="audience-organization" label="Organization" value={accountId} options={choices.map(a => ({ value: a.id, label: organizationName(accounts, a.id) }))} onChange={id => { setPicked(id); setStep("name"); }} />,
    name: <><Field label="Name"><Input id="email-audience-name" label="Audience name" value={name} onChange={setName} /></Field><Field label="Description (optional)"><Input id="email-audience-description" label="Description (optional)" value={description} onChange={setDescription} /></Field></>,
    confirmation: <ChoiceStack name="audience-confirmation" label="Confirmation" value={confirmation} options={[{ value: "none", label: "Subscribe them right away", description: "People join as soon as they're added." }, { value: "required", label: "Ask them to confirm by email", description: "People get a confirmation email first. Only people who confirm get other emails." }]} onChange={value => setConfirmation(value as "none" | "required")} />,
    review: <p>Add “{name.trim()}” to {organizationName(accounts, accountId)}. {confirmation === "required" ? "People confirm by email before they get other emails." : "People are subscribed right away."}</p>,
  };
  const steps: Stop[] = order.map((id, i) => ({ id, name: id[0]!.toUpperCase() + id.slice(1), sentence: AUDIENCE_SUBTITLES[id], state: id === open ? "open" : i < order.indexOf(open) ? "done" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const create = writes.createAudience;
  async function add() { if (working.current || !name.trim() || !accountId) return; working.current = true; try { const id = await create.create({ audienceId, name, description, accountId, confirmation }); if (id) onDone(id); } finally { working.current = false; } }
  return <Wizard title="Add an audience" icon={<Users size={20} aria-hidden />} label="Adding an audience" back={{ label: "Audiences", onSelect: onBack }} steps={steps} open={open} onOpen={setStep} status={{ word: create.busy ? "Adding audience" : open === "name" ? "Name the audience" : open === "review" ? "Ready to add" : "Choose how people join", tone: create.busy ? "busy" : undefined }} notices={create.error ? <Notice tone="warn" sentence={refusalSentence(create.error, "createAudience")} /> : undefined} acts={create.busy ? [] : [{ label: "Cancel", text: true, onAct: onBack }, ...(open === "review" ? [{ label: "Add audience", tone: "primary" as const, onAct: () => { void add(); } }] : open !== "organization" && (open !== "name" || !!name.trim()) ? [{ label: "Next", tone: "primary" as const, onAct: () => setStep(order[order.indexOf(open) + 1]!) }] : [])]} />;
}
