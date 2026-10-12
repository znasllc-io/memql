import { useEffect, useState, type ReactNode } from "react";
import { FilePlus } from "lucide-react";
import { fileNameProblem } from "@znasllc-io/memql-sdk-core/client";
import { ChoiceStack, ContentSkeleton, Input, Notice, TextArea, type Stop } from "../../kit";
import { Wizard } from "../../kit/Wizard";
import type { Act } from "../../kit/ActionBar";
import { FolderSelect, type FolderOption } from "../../items/FolderSelect";
import { AccountPicker } from "../accounts/AccountPicker";
import type { AccountRow } from "../accounts/rows";
import { Ingredients, type IngredientsProps } from "./Ingredients";
import { answerOf, blocker, filedName, fileKindEntry, FILE_KINDS, floorActs, formatOf, MAKE_STEPS, readyWords, stepsOf, stepState, templatesFor, withKind, type AnswerNames, type MakeDraft, type MakeFacts, type MakeStepId } from "./make";

export interface MakeWizardProps {
  draft: MakeDraft;
  onDraft: (draft: MakeDraft) => void;
  facts: MakeFacts;
  names: AnswerNames;
  open: MakeStepId;
  onOpen: (step: MakeStepId) => void;
  ingredients: Omit<IngredientsProps, "picked" | "onChange">;
  folders: FolderOption[];
  foldersLoaded: boolean;
  foldersError: string;
  accounts: AccountRow[];
  refusal: string;
  starting: boolean;
  making?: { detail: string; since: number };
  canSetUpInference: boolean;
  inferenceElsewhere?: string;
  onMake: () => void;
  onCancel: () => void;
  onStop: () => void;
  onLeave: () => void;
  onSetUpInference: () => void;
}
export function MakeWizard({ draft, onDraft, facts, names, open, onOpen, ingredients, folders, foldersLoaded, foldersError,
  accounts, refusal, starting, making, canSetUpInference, inferenceElsewhere, onMake, onCancel, onStop, onLeave, onSetUpInference }: MakeWizardProps) {
  const [nameTouched, setNameTouched] = useState(false);
  const [clock, setClock] = useState(Date.now());
  useEffect(() => { if (!making) return; const timer = setInterval(() => setClock(Date.now()), 1000); return () => clearInterval(timer); }, [making?.since]);
  const steps = stepsOf(draft, facts);
  const change = (patch: Partial<MakeDraft>) => onDraft({ ...draft, ...patch });
  function show(step: MakeStepId) {
    if (making || starting) return;
    if (step === "from" || step === "describe") change({ passed: { ...draft.passed, [step]: true } });
    onOpen(step);
  }
  function next() {
    const current = steps.indexOf(open);
    const nextStep = steps.slice(current + 1).find(step => stepState(step, draft, facts) === "waiting") || steps[current + 1];
    if (nextStep) show(nextStep);
  }
  const problem = fileNameProblem(draft.name, formatOf(draft.fileKind));
  const body = (step: MakeStepId): ReactNode => {
    switch (step) {
      case "kind": return <ChoiceStack name="mz-kind" label="Kind of file" voice="prose" value={fileKindEntry(draft.fileKind).value}
        options={FILE_KINDS.filter(choice => choice.kind.kind !== "emailTemplate" || draft.fileKind.kind === "emailTemplate")}
        onChange={value => { const picked = FILE_KINDS.find(kind => kind.value === value); if (picked) onDraft(withKind(draft, picked.kind, facts)); }} />;
      case "template": return <ChoiceStack name="mz-template" label="Template" voice="prose" value={draft.templateId}
        options={[{ value: "", label: "No template" }, ...templatesFor(draft, facts).map(t => ({ value: t.id, label: t.name, description: t.description }))]}
        onChange={templateId => change({ templateId })} />;
      case "from": return <Ingredients {...ingredients} picked={draft.ingredients} onChange={picked => change({ ingredients: picked })} />;
      case "describe": return <TextArea id="mz-description" label="Describe what you want" placeholder="A one-page summary of the Q3 results for the board" rows={4} value={draft.statement} onChange={statement => change({ statement })} />;
      case "name": return <><Input id="mz-name" label="Name" placeholder="Q3 summary" value={draft.name} onChange={name => { setNameTouched(true); change({ name }); }}
        onEnter={() => { setNameTouched(true); if (!problem) { if (!blocker(draft, facts)) onMake(); else next(); } }} />
        {nameTouched && problem ? <p className="os-caption" role="status">{problem}</p> : null}</>;
      case "folder": return <>{!foldersLoaded && !folders.length ? <ContentSkeleton label="Reading folders" /> : null}
        {foldersError ? <Notice tone="error" sentence={foldersError} /> : null}
        <FolderSelect id="mz-folder" label="Folder" value={draft.folderId} options={folders} missingLabel={foldersLoaded ? "Unavailable folder" : "Reading folder"}
          onChange={folderId => { change({ folderId }); const after = steps[steps.indexOf("folder") + 1]; if (after) onOpen(after); }} /></>;
      case "organization": return <AccountPicker id="mz-organization" label="Organization" value={draft.accountId} accounts={accounts}
        required={draft.fileKind.kind === "emailTemplate"} emptyLabel="No organization" onChange={accountId => change({ accountId })} />;
      case "file": return <div className="os-mz-file-result"><FilePlus size={32} aria-hidden /><strong>{filedName(draft)}</strong><span>{names.folder(draft.folderId) || (draft.folderId ? "Unavailable folder" : "Library")}</span></div>;
    }
  };
  const stopList: Stop[] = steps.map(step => ({ id: step, ...MAKE_STEPS[step], state: step === "file" ? (making ? "current" : "ahead") : stepState(step, draft, facts),
    answer: answerOf(step, draft, names), openable: step !== "file" && !making && !starting, body: body(step) }));
  const elapsed = making ? Math.max(0, Math.floor((clock - making.since) / 1000)) : 0;
  const status = making ? { word: "Making the file", detail: making.detail, tone: "busy" as const, meta: `${Math.floor(elapsed/60)}:${String(elapsed%60).padStart(2,"0")}` }
    : starting ? { word: "Making the file", detail: "Starting", tone: "busy" as const }
      : blocker(draft, facts)?.words || readyWords(draft, facts);
  const handlers = { cancel: onCancel, next, make: onMake, setUpInference: onSetUpInference, stop: onStop, leave: onLeave };
  const labels = { cancel: "Cancel", next: "Next", make: "Make the file", setUpInference: "Set up Inference", stop: "Stop", leave: "Leave" };
  const acts: Act[] = (starting ? [] : floorActs({ draft, facts, open, making: !!making, canSetUpInference })).map(id => ({ label: labels[id], onAct: handlers[id], text: id === "cancel" || id === "stop", tone: id === "cancel" || id === "stop" ? "quiet" : "primary" }));
  return <Wizard icon={<FilePlus />} title={making || starting ? "Making a file" : "Make a file"} label="Make a file" className="os-mz-make"
    breadcrumbs={[{ label: "Materialized", onSelect: making ? onLeave : onCancel }, { label: "Make" }]}
    steps={stopList} open={making ? "file" : open} onOpen={step => show(step as MakeStepId)} status={status} acts={acts}
    notices={<>{refusal ? <Notice tone="error" sentence="The file was not made." detail={refusal} /> : null}
      {!canSetUpInference && blocker(draft, facts)?.reason === "inference" && inferenceElsewhere ? <Notice tone="info" sentence={inferenceElsewhere} /> : null}</>} />;
}
