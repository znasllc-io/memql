import type { ReactNode } from "react";
import { ActionBar } from "../../kit/ActionBar";
import { Button, Caption, Fact, Facts, Head, InlineSkeleton, Notice, Panel, formatMoment } from "../../kit";
import { pageState, type ActId, type ActSpec } from "./acts";
import type { CompositionRow, ModelRow, SourceRow } from "./rows";
import { deployableWord, formatWord, modelsPhrase, provenanceClaim, DESCRIPTION_NOT_SENT, DESCRIPTION_NOT_SENT_NEXT, NOT_ATTACHED, notSentDetail } from "./words";

export interface CompositionPageProps {
  composition: CompositionRow;
  sources: SourceRow[];
  models: ModelRow[];
  filedIn: string | undefined;
  templateName: string;
  organizationName: string;
  caption: string;
  description?: { failure: string; unattached: string[] };
  recipeSaved: boolean;
  editorSilent?: string;
  tabRefusal?: string;
  acts: ActSpec[];
  busy: boolean;
  error: string;
  canOpenRun: boolean;
  onAct: (act: ActId) => void;
  onOpenRun: () => void;
  onBack: () => void;
}

export function CompositionPage({ composition: c, sources, models, filedIn, templateName, organizationName,
  caption, description, recipeSaved, editorSilent, tabRefusal, acts, busy, error, canOpenRun, onAct, onOpenRun, onBack }: CompositionPageProps) {
  const state = pageState(c);
  const provenance = c.provenanceNote || provenanceClaim(c.provenanceEmbedded, c.format);
  const notices: { sentence: string; detail?: string; next?: string }[] = [];
  if (description?.failure) notices.push(description.failure === "Stopped before the description was sent."
    ? { sentence: description.failure }
    : { sentence: DESCRIPTION_NOT_SENT, detail: notSentDetail(description.failure), next: DESCRIPTION_NOT_SENT_NEXT });
  for (const label of description?.unattached ?? []) notices.push({ sentence: NOT_ATTACHED, detail: label });
  if (recipeSaved) notices.push({ sentence: "Saved as a recipe. It is in Recipes." });
  for (const sentence of [editorSilent, tabRefusal]) if (sentence) notices.push({ sentence });
  return <div className="os-deploy-pane">
    <div className="os-deploy-scroll">
      <Head title={c.name || "Untitled"} breadcrumbs={[{ label: "Materialized", onSelect: onBack }, { label: c.name || "Untitled" }]}
        back={{ label: "Back to Materialized", onSelect: onBack }} />
      <Panel label="The file"><Facts>
        <PageFact label="Kind of file">{c.outputKind === "email_template" ? "Email template" : c.deployableKind ? `${deployableWord(c.deployableKind)} package` : formatWord(c.format)}</PageFact>
        <PageFact label="Filed in">{filedIn === undefined ? <InlineSkeleton label="Reading the folder" /> : filedIn}</PageFact>
        <PageFact label="Made from">{sources.length ? sources.map(s => s.label || s.ref).join(", ") : "Nothing"}</PageFact>
        <PageFact label="Description">{c.statement || "None"}</PageFact>
        {c.templateId ? <PageFact label="Template">{templateName || "Unavailable template"}</PageFact> : null}
        <PageFact label="Made by"><span className="mz-rowmark"><span className="mz-mark" data-mark={models.length ? "filled" : "hollow"} aria-hidden />{models.length ? modelsPhrase(models) : "No model"}</span>
          {canOpenRun ? <Button tone="quiet" onClick={onOpenRun}>Open the run</Button> : null}</PageFact>
        {provenance ? <PageFact label="Provenance">{provenance}</PageFact> : null}
        {c.accountIds.length ? <PageFact label="Organization">{organizationName || "Unavailable organization"}</PageFact> : null}
        <PageFact label="Made">{formatMoment(c.createdAt)}</PageFact>
      </Facts></Panel>
      {caption ? <Caption>{caption}</Caption> : null}
      {error ? <Notice tone="error" sentence={error} /> : null}
      {notices.map((notice, i) => <Notice key={i} tone="info" {...notice} />)}
      <p className="os-sr-only mz-announce" role="status" aria-live="polite" aria-atomic="true">{notices.flatMap(n => [n.sentence, n.detail, n.next]).filter(Boolean).join(" ")}</p>
    </div>
    <ActionBar state={state.word} detail={state.detail} acts={acts.map(act => ({ ...act, tone: act.tone === "primary" ? "primary" : "quiet", busy, onAct: () => onAct(act.id) }))} />
  </div>;
}

function PageFact({ label, children }: { label: string; children: ReactNode }) {
  return <Fact label={label} value={children} />;
}
