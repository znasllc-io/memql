import { useRef, useState } from "react";
import { UserPlus } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { ChoiceStack, Field, Input, Notice, Switch } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import type { UploadProvider } from "../../../items/upload";
import type { CampaignWrites } from "../actions";
import type { AudienceRow } from "../rows";
import { ImportReport } from "./ImportReport";

export function AddPeopleWizard({ audience, writes, uploads, onBack, onDone }: { audience: AudienceRow; writes: CampaignWrites; uploads: UploadProvider; onBack: () => void; onDone: () => void }) {
  const [how, setHow] = useState("");
  const [step, setStep] = useState("how");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [pending, setPending] = useState(audience.confirmation === "required");
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
  async function add() { if (working.current) return; working.current = true; setBusy(true); try { if (await writes.addRecipient.add(audience, email, name, pending ? "pending" : "subscribed")) onDone(); } finally { working.current = false; setBusy(false); } }
  const order = ["how", ...(how === "file" ? ["file", "report"] : how === "address" ? ["address"] : [])];
  const bodies = {
    how: <ChoiceStack name="email-people-how" label="How" value={how} options={[{ value: "file", label: "Import a CSV file" }, { value: "address", label: "Add one address" }]} onChange={value => { importer.dismiss(); setHow(value); setStep(value); }} />,
    file: <><p>The first row needs an <span className="os-mono">email</span> column. Name and other columns are kept with each person.</p><input ref={input} className="os-sr-only" aria-label="CSV file" type="file" accept=".csv,text/csv" onChange={e => { void chosen(e.target.files?.[0]); }} /></>,
    report: importer.report ? <ImportReport report={importer.report} /> : null,
    address: <><Field label="Email"><Input id="email-person-address" label="Email" value={email} onChange={setEmail} /></Field><Field label="Name"><Input id="email-person-name" label="Name" value={name} onChange={setName} /></Field>{audience.confirmation === "required" ? <Switch checked={pending} onChange={setPending}>Ask them to confirm</Switch> : null}</>,
  };
  const steps: Stop[] = order.map((id, i) => ({ id, name: id[0]!.toUpperCase() + id.slice(1), state: id === step ? "open" : i < order.indexOf(step) ? "done" : "ahead", body: bodies[id as keyof typeof bodies] }));
  const failure = error || importer.error || writes.addRecipient.error;
  return <Wizard title="Add people" icon={<UserPlus size={20} aria-hidden />} label="Adding people" back={{ label: audience.name, onSelect: onBack }} steps={steps} open={step} onOpen={setStep} status={{ word: busy ? how === "file" ? `Importing ${fileName}` : "Adding address" : step === "report" ? "Import finished" : "Add people", tone: busy ? "busy" : undefined }} notices={failure ? <Notice tone="warn" sentence={refusalSentence(failure)} /> : undefined} acts={busy ? [] : step === "report" ? [{ label: "Done", tone: "primary", onAct: () => { importer.dismiss(); onDone(); } }] : [{ label: "Cancel", text: true, onAct: onBack }, ...(step === "file" ? [{ label: "Choose a file", tone: "primary" as const, onAct: () => input.current?.click() }] : step === "address" && /^[^\s@]+@[^\s@]+$/.test(email.trim()) ? [{ label: "Add", tone: "primary" as const, onAct: () => { void add(); } }] : [])]} />;
}
