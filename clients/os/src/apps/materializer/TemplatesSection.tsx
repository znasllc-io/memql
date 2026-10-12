import { useEffect, useState } from "react";
import { rowBool, rowString, sameEntityId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, ContentSkeleton, EmptyState, Fact, Facts, Head, LiveList, Notice, Panel, RecordRow, Refine, listCount, useLiveView, type LiveListSource } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { ActionBar } from "../../kit/ActionBar";
import { accountName, type AccountRow } from "../accounts/rows";
import { AddTemplateWizard } from "./AddTemplateWizard";
import type { NewTemplateFacts } from "./actions";
import { templateFingerprint, templateFromRow } from "./rows";
import { fileWord, type FileOption } from "./ingredientFiles";
import { formatWord } from "./words";

export interface TemplatesSectionProps {
  source: LiveListSource<Row> | null; accounts: AccountRow[]; files: readonly Row[]; filesLoading: boolean; filesError: string;
  busy: boolean; error: string; showArchived: boolean; onRetry: () => void; onReset: () => void;
  onCreateTemplate: (facts: NewTemplateFacts) => Promise<string>; onArchiveTemplate: (id:string)=>void; onRestoreTemplate:(id:string)=>void;
  onFilesNeeded: (needed:boolean)=>void;
}
export function TemplatesSection({source,accounts,files,filesLoading,filesError,busy,error,showArchived,onRetry,onReset,onCreateTemplate,onArchiveTemplate,onRestoreTemplate,onFilesNeeded}:TemplatesSectionProps){
  const [search,setSearch]=useState(""),[page,setPage]=useState(""),[adding,setAdding]=useState(false);
  const view=useLiveView(source,`${showArchived}|${search}`,rows=>rows.filter(row=>{const t=templateFromRow(row);return (showArchived||!t.archived)&&`${t.name} ${t.description}`.toLowerCase().includes(search.trim().toLowerCase());}));
  const selected=source?.snapshot.rows.find(row=>sameEntityId(rowString(row,"id"),page)); const template=selected?templateFromRow(selected):null;
  useEffect(()=>{onFilesNeeded(adding||!!page);return()=>onFilesNeeded(false);},[adding,page,onFilesNeeded]);
  function back(){setAdding(false);setPage("");onReset();}
  const fileOptions: FileOption[]=files.filter(row=>!rowBool(row,"archived")&&!rowBool(row,"deleted")&&fileWord(rowString(row,"name"))!=="ZIP")
    .map(row=>({id:rowString(row,"id"),name:rowString(row,"name"),kind:fileWord(rowString(row,"name")),createdAt:rowString(row,"createdAt")})).sort((a,b)=>b.createdAt.localeCompare(a.createdAt));
  const organizationName=(id:string)=>{const a=accounts.find(a=>sameEntityId(a.id,id));return a?accountName(a):"Unavailable organization";};
  return <>
    <div hidden={adding||!!page} className="mz-list-host"><div className="mz-list-section">
      <Head title="Templates" navigation={!adding&&!page} meta={listCount(view?.snapshot)}><Refine search={search} onSearch={setSearch} label="Refine templates"/><AddButton label="Add a template" onClick={()=>{onReset();setAdding(true);}}/></Head>
      <LiveList source={view} label="Templates" rowId={row=>templateFromRow(row).id} fingerprint={row=>templateFingerprint(templateFromRow(row))}
        errorContent={<><Notice tone="error" sentence="This cluster did not return your templates." detail={source?.snapshot.error}/><Button tone="quiet" onClick={onRetry}>Try again</Button></>}
        emptyContent={<EmptyState title={search.trim()?"No matches":"No templates yet"}>{search.trim()?"Clear the filters to see everything.":"A template is a Library file whose look a new file can take."}</EmptyState>}
        renderRow={row=>{const t=templateFromRow(row);return <RecordRow name={t.name} secondary={t.description} state={t.archived?"Archived":"Active"} dim={t.archived} onOpen={()=>{onReset();setPage(t.id);}}>{t.placeholders.length?`Asks for ${t.placeholders.map(p=>p.name).join(", ")}`:null}</RecordRow>;}}/>
    </div></div>
    {adding?<AddTemplateWizard files={fileOptions} filesLoading={filesLoading} filesError={filesError} accounts={accounts} busy={busy} refusal={error} onCancel={back}
      onAdd={facts=>{void onCreateTemplate(facts).then(id=>{if(id){setAdding(false);setPage(id);}});}}/>:null}
    {page&&!adding?template?<div className="os-deploy-pane"><div className="os-deploy-scroll">
      <Head title={template.name} breadcrumbs={[{label:"Templates",onSelect:back},{label:template.name}]} back={{label:"Back to Templates",onSelect:back}}/>
      <Panel label="The template"><Facts>
        <Fact label="Makes" value={formatWord(template.format)}/><Fact label="File" value={rowString(files.find(row=>sameEntityId(rowString(row,"id"),template.fileId)) ?? {},"name") || (filesLoading?<ContentSkeleton label="Reading the file"/>:"Unavailable file")}/>
        <Fact label="Description" value={template.description||"None"}/><Fact label="Organization" value={template.accountIds.map(organizationName).join(", ")||"None"}/>
        <Fact label="Asks for" value={template.placeholders.map(p=>p.name).join(", ")||"Nothing"}/>
      </Facts></Panel>{error?<Notice tone="error" sentence={error}/>:null}
    </div><ActionBar state={template.archived?"Archived":"Active"} acts={[template.archived?{label:"Restore",tone:"primary",busy,onAct:()=>onRestoreTemplate(template.id)}:{label:"Archive",text:true,busy,onAct:()=>onArchiveTemplate(template.id)}]}/></div>
      : <ContentSkeleton label="Reading the template"/>:null}
  </>;
}
