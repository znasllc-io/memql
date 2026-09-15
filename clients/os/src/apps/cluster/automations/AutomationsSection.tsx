import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Caption, Head, Notice, Subhead } from "../../../kit";
import { layout } from "./layout";
import { useAutomationGraph } from "./useAutomationGraph";
import { AutomationMap } from "./AutomationMap";
import { AutomationDetail } from "./AutomationDetail";
import { LoopStops } from "./LoopStops";
import "./automations.css";
export function AutomationsSection() {
  const {graph,stops,reread}=useAutomationGraph();
  const [selected,select]=useState("");
  const root=useRef<HTMLDivElement>(null);
  const map=useMemo(()=>layout(graph.value ?? []),[graph.value]);
  const row=graph.value?.find(a=>a.name===selected);
  useEffect(() => {
    if (row && root.current && root.current.clientWidth < 720) {
      root.current.querySelector<HTMLElement>(".os-automation-detail")?.focus();
    }
  }, [row?.name]);
  const clear=()=>{select("");requestAnimationFrame(()=>root.current?.querySelector<HTMLElement>('[aria-pressed="true"], [tabindex="0"]')?.focus());};
  return <div ref={root} className={`os-automations ${row?"has-selection":""}`} onKeyDown={e=>{if(e.key==="Escape"){e.stopPropagation();clear();}}}>
    <Head title="Automations" meta={graph.value?`${graph.value.length} automations in ${new Set(graph.value.map(a=>a.stratum)).size} strata. Read at ${graph.at?.toLocaleTimeString()}.`:null}><Button tone="quiet" busy={graph.state==="reading"||stops.state==="reading"} busyLabel="Reading" onClick={reread}>Read again</Button></Head>
    {row?<div className="os-automation-back"><Button tone="quiet" onClick={clear}>Back to automations</Button></div>:null}
    <div className="os-automation-body"><div className="os-automation-overview">
      {graph.state==="failed"?<Notice tone="error" sentence="The cluster did not answer the automation graph." detail={graph.error}/>:graph.value===null?<Caption>{graph.state==="unread"?"Waiting for a cluster connection.":"Reading the automations this cluster loaded."}</Caption>:graph.value.length===0?<Caption>This cluster loaded no automations.</Caption>:<>{map.cards.length?<div className="os-automation-canvas"><AutomationMap map={map} selected={row?.name ?? ""} select={select}/></div>:null}{map.standalone.length?<section className="os-automation-alone"><Subhead>Stand alone ({map.standalone.length})</Subhead><Caption>Nothing these write starts another automation, and nothing another automation writes starts them.</Caption><div>{map.standalone.map(a=><Button key={a.name} tone="quiet" aria-pressed={row?.name===a.name} onClick={()=>select(a.name)}>{a.name}</Button>)}</div></section>:null}</>}
      <LoopStops reading={stops}/>
    </div><aside tabIndex={-1} className="os-automation-detail" aria-label="Automation details"><AutomationDetail row={row} rows={graph.value ?? []} select={select}/></aside></div>
  </div>;
}
