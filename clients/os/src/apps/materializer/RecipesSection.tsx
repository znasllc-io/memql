import { useState } from "react";
import { rowString, sameEntityId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { ActionBar } from "../../kit/ActionBar";
import { Button, ContentSkeleton, EmptyState, Fact, Facts, Head, LiveList, Notice, Panel, RecordRow, Refine, formatMoment, listCount, useLiveView, type LiveListSource } from "../../kit";
import { recipeFingerprint, recipeFromRow, type RecipeRow } from "./rows";
import { formatWord } from "./words";

export function RecipesSection({source,showArchived,busy,error,names,onRun,onArchive,onRestore,onRetry,onReset}:{
  source:LiveListSource<Row>|null; showArchived:boolean; busy:boolean; error:string;
  names:{folder:(id:string)=>string|null|undefined;template:(id:string)=>string;account:(id:string)=>string};
  onRun:(recipe:RecipeRow)=>void;onArchive:(id:string)=>void;onRestore:(id:string)=>void;onRetry:()=>void;onReset:()=>void;
}) {
  const [search,setSearch]=useState(""),[page,setPage]=useState("");
  const view=useLiveView(source,`${showArchived}|${search}`,rows=>rows.filter(row=>{const r=recipeFromRow(row);return (showArchived||!r.archived)&&`${r.name} ${r.description}`.toLowerCase().includes(search.trim().toLowerCase());}));
  const raw=source?.snapshot.rows.find(row=>sameEntityId(rowString(row,"id"),page)),recipe=raw?recipeFromRow(raw):null;
  function back(){setPage("");onReset();}
  return <>
    <div hidden={!!page} className="mz-list-host"><div className="mz-list-section">
      <Head title="Recipes" navigation={!page} meta={listCount(view?.snapshot)}><Refine search={search} onSearch={setSearch} label="Refine recipes"/></Head>
      <LiveList source={view} label="Recipes" rowId={row=>recipeFromRow(row).id} fingerprint={row=>recipeFingerprint(recipeFromRow(row))}
        errorContent={<><Notice tone="error" sentence="This cluster did not return your recipes." detail={source?.snapshot.error}/><Button tone="quiet" onClick={onRetry}>Try again</Button></>}
        emptyContent={<EmptyState title={search.trim()?"No matches":"No recipes yet"}>{search.trim()?"Clear the filters to see everything.":"Save a recipe from a made file to make it again."}</EmptyState>}
        renderRow={row=>{const r=recipeFromRow(row);return <RecordRow name={r.name} secondary={r.description} state={r.archived?"Archived":undefined} dim={r.archived} onOpen={()=>{onReset();setPage(r.id);}}>{r.runCount?`Made ${r.runCount} ${r.runCount===1?"time":"times"}${r.lastRunAt?`, last on ${formatMoment(r.lastRunAt)}`:""}`:"Not run yet"}</RecordRow>;}}/>
    </div></div>
    {page?recipe?<div className="os-deploy-pane"><div className="os-deploy-scroll">
      <Head title={recipe.name} breadcrumbs={[{label:"Recipes",onSelect:back},{label:recipe.name}]} back={{label:"Back to Recipes",onSelect:back}}/>
      <Panel label="The recipe"><Facts>
        <Fact label="Kind of file" value={formatWord(recipe.format)}/><Fact label="Filed in" value={names.folder(recipe.folderId) ?? "Unavailable folder"}/>
        <Fact label="Made from" value={recipe.sourceSelectors.map(s=>s.label||s.selector).join(", ")||"Nothing"}/><Fact label="Description" value={recipe.description||"None"}/>
        {recipe.templateId?<Fact label="Template" value={names.template(recipe.templateId)}/>:null}
        {recipe.accountIds.length?<Fact label="Organization" value={recipe.accountIds.map(names.account).join(", ")}/>:null}
      </Facts></Panel>{error?<Notice tone="error" sentence={error}/>:null}
    </div><ActionBar state={recipe.archived?"Archived":"Ready"} acts={recipe.archived?[{label:"Restore",tone:"primary",busy,onAct:()=>onRestore(recipe.id)}]:[{label:"Archive",text:true,busy,onAct:()=>onArchive(recipe.id)},{label:"Run it again",tone:"primary",busy,onAct:()=>onRun(recipe)}]}/></div>
      :listCount(source?.snapshot)===undefined?<ContentSkeleton label="Reading the recipe"/>:<><Head title="Recipe" back={{label:"Back to Recipes",onSelect:back}}/><Notice tone="info" sentence="That recipe is not available."/></>:null}
  </>;
}
