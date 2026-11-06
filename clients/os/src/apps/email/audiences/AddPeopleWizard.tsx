import { useRef, useState } from "react";
import { UserPlus } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, ChoiceStack, Field, Input, Notice, Switch } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import type { UploadProvider } from "../../../items/upload";
import type { CampaignWrites } from "../actions";
import type { AudienceRow, JourneyRow } from "../rows";
import { ImportReport } from "./ImportReport";
import { useExistingPerson } from "./useExistingPerson";

/** What each step is, under its name on the rail. */
const PEOPLE_SUBTITLES: Readonly<Record<string, string>> = { how: "A CSV file or one address", file: "A CSV with an email column", report: "What the import found", address: "Who to add" };

/**
 * What sends a confirmation email to someone added as pending: an automation on
 * now that people who join enroll in and that asks them to confirm; one that is
 * paused, which takes in the people who joined meanwhile when it is turned on
 * again; or nothing. (An automation turned on for the first time later starts
 * with the people who join from then on, so it sends this person nothing.)
 * `unknown` until the automations have been read.
 */
export type ConfirmationSender = { state: "on" } | { state: "paused"; name: string } | { state: "none" } | { state: "unknown" };

export function confirmationSender(journeys: readonly JourneyRow[], audienceId: string): ConfirmationSender {
  const confirming = journeys.filter((journey) => journey.trigger.kind === "joined" && journey.trigger.audienceId === audienceId && journey.steps.some((step) => step.kind === "confirm"));
  if (confirming.some((journey) => journey.status === "active")) return { state: "on" };
  const paused = confirming.find((journey) => journey.status === "paused");
  return paused ? { state: "paused", name: paused.name } : { state: "none" };
}

/** The sentence under "Ask them to confirm": what will, or will not, send the confirmation email. */
export function confirmationCaption(sender: ConfirmationSender): string {
  switch (sender.state) {
    case "on":
      return "A confirmation email goes out to this address. They are subscribed once they confirm.";
    case "paused":
      return `Nothing sends this address a confirmation email until “${sender.name}” is turned on again. They are subscribed once they confirm.`;
    case "none":
      return "No automation asks the people who join this audience to confirm, so nothing sends this address a confirmation email and they stay pending.";
    default:
      return "They are subscribed once they confirm.";
  }
}

export function AddPeopleWizard({ audience, confirmation = { state: "unknown" }, writes, uploads, onBack, onDone, onOpenPerson }: { audience: AudienceRow; confirmation?: ConfirmationSender; writes: CampaignWrites; uploads: UploadProvider; onBack: () => void; onDone: () => void; onOpenPerson: (person: Row) => void }) {
  const [how, setHow] = useState("");
  const [step, setStep] = useState("how");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const confirming = audience.confirmation === "required";
  const [pending, setPending] = useState(confirming);
  // In an audience that asks people to confirm, subscribed needs the operator to say the person already agreed.
  const [agreed, setAgreed] = useState(false);
  const shaped = /^[^\s@]+@[^\s@]+$/.test(email.trim());
  const existing = useExistingPerson(audience.id, email, shaped);
  const [busy, setBusy] = useState(false);
  const [fileName, setFileName] = useState("");
  const [error, setError] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const working = useRef(false);
  const importer = writes.importRecipients;
  async function chosen(file: File | undefined) {
    if (!file || working.current) return;
    working.current = true; setBusy(true); setFileName(file.name); setError("");
    try { const result = await uploads.upload(file).done; if (await importer.run(audience.id, result.artifactId)) setStep("report"); }
    catch (failure) { setError(refusalSentence(failure)); }
    finally { working.current = false; setBusy(false); if (input.current) input.current.value = ""; }
  }
  async function add() { if (working.current) return; working.current = true; setBusy(true); try { if (await writes.addRecipient.add(audience, email, name, pending ? "pending" : "subscribed", confirming && !pending ? agreed : undefined)) onDone(); } finally { working.current = false; setBusy(false); } }
  const order = ["how", ...(how === "file" ? ["file", "report"] : how === "address" ? ["address"] : [])];
  const bodies = {
    how: <ChoiceStack name="email-people-how" label="How" value={how} options={[{ value: "file", label: "Import a CSV file" }, { value: "address", label: "Add one address" }]} onChange={value => { importer.dismiss(); setHow(value); setStep(value); }} />,
    file: <><p>The first row needs an <span className="os-mono">email</span> column. Name and other columns are kept with each person.</p><input ref={input} className="os-sr-only" aria-label="CSV file" type="file" accept=".csv,text/csv" onChange={e => { void chosen(e.target.files?.[0]); }} /></>,
    report: importer.report ? <ImportReport report={importer.report} /> : null,
    address: <><Field label="Email"><Input id="email-person-address" label="Email" value={email} onChange={setEmail} /></Field><Field label="Name"><Input id="email-person-name" label="Name" value={name} onChange={setName} /></Field>{confirming ? <Switch checked={pending} onChange={next => { setPending(next); setAgreed(false); }}>Ask them to confirm</Switch> : null}{confirming && pending ? <p className="os-caption">{confirmationCaption(confirmation)}</p> : null}{confirming && !pending ? <Switch checked={agreed} onChange={setAgreed}>They already agreed to receive these emails</Switch> : null}</>,
  };
  const steps: Stop[] = order.map((id, i) => ({ id, name: id[0]!.toUpperCase() + id.slice(1), sentence: PEOPLE_SUBTITLES[id], state: id === step ? "open" : i < order.indexOf(step) ? "done" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const addable = step === "address" && shaped && existing.state === "free" && (!confirming || pending || agreed);
  // The bar says whose turn it is; while Add is absent, what is missing (DESIGN, one wizard for adding).
  const word = busy ? how === "file" ? `Importing ${fileName}` : "Adding address"
    : step === "report" ? "Import finished"
    : step === "how" ? "Choose how to add people"
    : step === "file" ? "Choose a CSV file"
    : !shaped ? "Enter an email address"
    : existing.state === "taken" ? "Already in this audience"
    : existing.state === "failed" ? "Couldn't check the address"
    : existing.state !== "free" ? "Checking the address"
    : !addable ? "Say they already agreed"
    : "Ready to add";
  const failure = error || importer.error || writes.addRecipient.error;
  const notices = [
    failure ? <Notice key="refused" tone="warn" sentence={refusalSentence(failure)} /> : null,
    step === "address" && existing.state === "taken" ? <Notice key="taken" tone="warn" sentence="This address is already in this audience."><Button onClick={() => { if (existing.person) onOpenPerson(existing.person); }}>Show them in the list</Button></Notice> : null,
    step === "address" && existing.state === "failed" ? <Notice key="unchecked" tone="warn" sentence={refusalSentence(existing.error)}><Button onClick={existing.again}>Try again</Button></Notice> : null,
  ].filter(Boolean);
  return <Wizard title="Add people" icon={<UserPlus size={20} aria-hidden />} label="Adding people" back={{ label: audience.name, onSelect: onBack }} steps={steps} open={step} onOpen={setStep} status={{ word, tone: busy ? "busy" : undefined }} notices={notices.length ? <>{notices}</> : undefined} acts={busy ? [] : step === "report" ? [{ label: "Done", tone: "primary", onAct: () => { importer.dismiss(); onDone(); } }] : [{ label: "Cancel", text: true, onAct: onBack }, ...(step === "file" ? [{ label: "Choose a file", tone: "primary" as const, onAct: () => input.current?.click() }] : addable ? [{ label: "Add", tone: "primary" as const, onAct: () => { void add(); } }] : [])]} />;
}
