import { useId, useMemo, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import { Archive, FolderSync } from "lucide-react";
import { Caption, Field, Input, Notice, Select, type Stop } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import { PageHelp } from "../../../kit/pageHelp";
import { machineName, type MachineRow } from "../../fleet/rows";
import type { FolderRow } from "../rows";
import { folderChoices, pathPlaceholderFor } from "./BackupForm";
import { absoluteBackupPath, validArchive } from "./transfers";
import type { NewBackup } from "./rows";

export function BackupSetup({ machines, folders, busy, error, onFolder, onArchive, onCancel }: {
  machines: readonly MachineRow[]; folders: readonly FolderRow[]; busy: boolean; error: string;
  onFolder: (spec: NewBackup) => Promise<void>; onArchive: (workerId: string, path: string) => Promise<void>; onCancel: () => void;
}) {
  const ids=useId();
  const [step, setStep] = useState("source");
  const [requestId] = useState(newShortId);
  const [kind, setKind] = useState("folder");
  const [workerId, setWorkerId] = useState(machines[0]?.id ?? "");
  const [path, setPath] = useState("");
  const [folderId, setFolderId] = useState("");
  const [minutes, setMinutes] = useState("5");
  const chosen = machines.find(m => m.id === workerId);
  const sourceReady = !!chosen && absoluteBackupPath(path.trim()) && (kind === "folder" || validArchive(path));
  const interval = Number(minutes);
  const ready = sourceReady && (kind !== "folder" || Number.isInteger(interval) && interval >= 5 && interval <= 525600);
  const choices = useMemo(() => folderChoices(folders), [folders]);
  const steps: Stop[] = [
    { id: "source", name: "Source", sentence: "A folder or archive on a machine", state: sourceReady ? "done" : "open", answer: sourceReady ? `${machineName(chosen!)} · ${path.trim()}` : undefined, body: <>
      <Field label="Backup type"><Select id={`${ids}-type`} label="Backup type" value={kind} onChange={setKind}><option value="folder">Folder sync</option><option value="archive">Archive upload</option></Select></Field>
      <Field label="Machine"><Select id={`${ids}-machine`} label="Machine" value={workerId} onChange={setWorkerId}><option value="">Choose a machine</option>{machines.map(m => <option key={m.id} value={m.id}>{machineName(m)}</option>)}</Select></Field>
      <Field label={kind === "folder" ? "Folder on that machine" : "Archive on that machine"}><Input id={`${ids}-source`} label={kind === "folder" ? "Folder on that machine" : "Archive on that machine"} value={path} onChange={setPath} placeholder={pathPlaceholderFor(chosen)} /></Field>
      {machines.length === 0 && <Notice tone="info" sentence="Pair a machine in Fleet to start a backup." />}
    </> },
    { id: "options", name: kind === "folder" ? "Destination and schedule" : "Transfer", sentence: kind === "folder" ? "Where it goes, and how often" : "How the upload runs", state: !sourceReady ? "ahead" : step === "review" ? "done" : "waiting", answer: step === "review" ? kind === "folder" ? `Every ${minutes} minutes` : "One-time upload" : undefined, body: kind === "folder" ? <>
      <Field label="Files destination"><Select id={`${ids}-folder`} label="Files destination" value={folderId} onChange={setFolderId}><option value="">Library root</option>{choices.map(c => <option key={c.id} value={c.id}>{c.label}</option>)}</Select></Field>
      <Field label="Check every (minutes)"><Input id={`${ids}-schedule`} label="Check every (minutes)" value={minutes} onChange={setMinutes} /></Field>
    </> : null },
    { id: "review", name: "Review", sentence: "Check, then start", state: ready ? "waiting" : "ahead", body: <>
      <Caption>{kind === "folder" ? `Sync ${path.trim()} from ${chosen ? machineName(chosen) : "this machine"} every ${minutes} minutes.` : `Upload ${path.trim()} once from ${chosen ? machineName(chosen) : "this machine"}.`}</Caption>
    </> },
  ];
  const submit = async () => { if (!ready || busy) return; if (kind === "folder") await onFolder({ requestId, workerId, localPath: path.trim(), folderId, includeHidden: true, excludeGlobs: [], intervalMinutes: interval, mode: "sync" }); else await onArchive(workerId, path.trim()); };
  // The explanations live behind the window's (i) button, not on the steps (Jesús, Nov 23:
  // standing text is an eyesore; only what the person must see stays on screen).
  const help = <PageHelp title="Adding a backup">
    <p>Folder sync keeps a folder on one of your paired machines in step with Files. Files deleted there move to the Bin here, where they stay recoverable and keep using storage until purged. A folder that goes missing never deletes anything.</p>
    <p>Archive upload copies one ZIP, TAR, TAR.GZ or TGZ file once and does not watch it afterwards. You can close the window, pause or resume without starting over.</p>
    <p>Enter the full path on the machine, for example /Users/you/Documents. That machine must allow access to the folder. All regular files are included, hidden ones too; links and special files are not followed. A missed run resumes when the machine is back.</p>
    <p>Each backup can be up to 1 TB, within your allowance and the cluster's 5 TB. Space is reserved before the upload starts.</p>
  </PageHelp>;
  return <>{help}<Wizard icon={kind === "folder" ? <FolderSync /> : <Archive />} title="Add backup" label="Backup setup" open={step} onOpen={setStep} steps={steps}
    back={{ label: "Backups", onSelect: onCancel }} status={{ word: busy ? "Starting backup" : step === "source" && !sourceReady ? "Choose a source" : step === "review" ? "Ready to start" : "Review your choices", tone: busy ? "busy" : undefined }}
    notices={error ? <Notice tone="error" sentence="The backup could not be started." detail={error} /> : undefined}
    acts={busy ? [] : [{ label: "Cancel", onAct: onCancel }, ...(step === "source" && sourceReady ? [{ label: "Continue", onAct: () => setStep("options"), tone: "primary" as const }] : step === "options" && ready ? [{ label: "Review", onAct: () => setStep("review"), tone: "primary" as const }] : step === "review" && ready ? [{ label: kind === "folder" ? "Start sync" : "Upload archive", onAct: () => submit(), tone: "primary" as const }] : [])]} /></>;
}
