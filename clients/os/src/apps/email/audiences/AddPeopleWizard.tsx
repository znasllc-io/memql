import { useRef, useState } from "react";
import { UserPlus } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, ChoiceStack, Field, Input, Notice, Switch } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import type { UploadProvider } from "../../../items/upload";
import type { CampaignWrites } from "../actions";
import type { AudienceRow } from "../rows";
import { ImportReport } from "./ImportReport";
import { useExistingPerson } from "./useExistingPerson";

/** What each step is, under its name on the rail. */
const PEOPLE_SUBTITLES: Readonly<Record<string, string>> = { how: "A CSV file or one address", file: "A CSV with an email column", report: "What the import found", address: "Who to add" };

export function AddPeopleWizard({ audience, writes, uploads, onBack, onDone, onOpenPerson }: { audience: AudienceRow; writes: CampaignWrites; uploads: UploadProvider; onBack: () => void; onDone: () => void; onOpenPerson: (person: Row) => void }) {
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
    address: <><Field label="Email"><Input id="email-person-address" label="Email" value={email} onChange={setEmail} /></Field><Field label="Name"><Input id="email-person-name" label="Name" value={name} onChange={setName} /></Field>{confirming ? <Switch checked={pending} onChange={next => { setPending(next); setAgreed(false); }}>Ask them to confirm</Switch> : null}{confirming && pending ? <p className="os-caption">A confirmation email goes out to this address. They are subscribed once they confirm.</p> : null}{confirming && !pending ? <Switch checked={agreed} onChange={setAgreed}>They already agreed to receive these emails</Switch> : null}</>,
  };
  const steps: Stop[] = order.map((id, i) => ({ id, name: id[0]!.toUpperCase() + id.slice(1), sentence: PEOPLE_SUBTITLES[id], state: id === step ? "open" : i < order.indexOf(step) ? "done" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const failure = error || importer.error || writes.addRecipient.error;
  const notices = [
    failure ? <Notice key="refused" tone="warn" sentence={refusalSentence(failure)} /> : null,
    step === "address" && existing.state === "taken" ? <Notice key="taken" tone="warn" sentence="This address is already in this audience."><Button onClick={() => { if (existing.person) onOpenPerson(existing.person); }}>Show them in the list</Button></Notice> : null,
    step === "address" && existing.state === "failed" ? <Notice key="unchecked" tone="warn" sentence={refusalSentence(existing.error)}><Button onClick={existing.again}>Try again</Button></Notice> : null,
  ].filter(Boolean);
  return <Wizard title="Add people" icon={<UserPlus size={20} aria-hidden />} label="Adding people" back={{ label: audience.name, onSelect: onBack }} steps={steps} open={step} onOpen={setStep} status={{ word: busy ? how === "file" ? `Importing ${fileName}` : "Adding address" : step === "report" ? "Import finished" : "Add people", tone: busy ? "busy" : undefined }} notices={notices.length ? <>{notices}</> : undefined} acts={busy ? [] : step === "report" ? [{ label: "Done", tone: "primary", onAct: () => { importer.dismiss(); onDone(); } }] : [{ label: "Cancel", text: true, onAct: onBack }, ...(step === "file" ? [{ label: "Choose a file", tone: "primary" as const, onAct: () => input.current?.click() }] : step === "address" && shaped && existing.state === "free" && (!confirming || pending || agreed) ? [{ label: "Add", tone: "primary" as const, onAct: () => { void add(); } }] : [])]} />;
}
