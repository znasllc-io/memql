import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useState } from "react";
import { Button, Caption, Notice, Subhead, RecordList, RecordRow } from "../../../kit";
import type { Reading } from "../../../cluster/reading";
import type { LoopStop } from "./useAutomationGraph";
function Stop({row}:{row:LoopStop}) {
  const [expanded,setExpanded]=useState(false);
  const chain=row.chain ?? [];
  const time=row.finishedAt ? new Date(row.finishedAt) : null;
  return <div><RecordRow name={row.automationName} secondary={time && Number.isFinite(time.getTime())?time.toLocaleString():"Time not recorded"} state="Stopped" tone="warn" /><div className="os-automation-chain">{chain.map((link,i)=>{
    if(!expanded && chain.length>6 && i>=2 && i<chain.length-2)return i===2?<Button key="more" tone="quiet" aria-expanded={false} onClick={()=>setExpanded(true)}>{chain.length-4} more</Button>:null;
    return <code key={`${link.runId}:${i}`} title={link.runId}>{link.automation}</code>;
  })}<code className="os-automation-stop">{row.automationName}</code></div><Caption>{row.reason==="depth"?`Depth ${row.depth ?? "—"}, past the cap of ${row.cap ?? "—"}.`:row.reason==="loop_bound"?`The @loop on ${row.automationName} allows ${row.cap ?? "—"} runs.`:"The chain was stopped by loop protection. Depth and bound were not recorded."}</Caption></div>;
}
export function LoopStops({reading}:{reading:Reading<LoopStop[]>}) {
  return <section className="os-automation-stops" aria-label="Loops stopped"><Subhead meta={reading.state === "read" && !reading.error ? reading.value?.length : undefined}>Loops stopped</Subhead>{reading.state==="failed"?<Notice tone="error" sentence="The cluster did not answer the stopped chains." detail={reading.error}/>:reading.value===null?reading.state==="unread"?<Caption>Not connected to the cluster.</Caption>:<RecordListSkeleton label="Loading stopped chains" />:reading.value.length===0?<Caption>No loop has been stopped in the runs this cluster still keeps.</Caption>:<><Caption>Read at {reading.at?.toLocaleTimeString()}. Latest retained stops.</Caption><RecordList as="ol" label="Stopped automation chains">{reading.value.map((row,i)=><Stop key={row.runId || row.id || i} row={row}/>)}</RecordList></>}</section>;
}
