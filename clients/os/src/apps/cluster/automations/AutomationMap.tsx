import { useId, useRef, useState } from "react";
import { layout, trigger } from "./layout";
export function AutomationMap({map, selected, select}: {map: ReturnType<typeof layout>; selected: string; select: (name: string)=>void}) {
  const marker=useId().replaceAll(":", "");
  const [focus,setFocus]=useState("");
  const refs=useRef(new Map<string,SVGGElement>());
  const active=map.cards.some(c=>c.row.name===focus)?focus:map.cards[0]?.row.name;
  return <svg className="os-automation-map" width={map.width} height={map.height} role="group" aria-label="Automation graph. Use arrow keys to move and Enter to select.">
    <defs><marker id={marker} markerWidth="6" markerHeight="6" refX="5" refY="3" orient="auto"><path d="M 0 0 L 6 3 L 0 6" fill="none" stroke="context-stroke" /></marker></defs>
    {map.columns.map((n,i)=><g key={n} aria-hidden="true"><rect className="os-automation-column" x={i*308+12} y="0" width="220" height={map.height} opacity={i%2 ? 1 : 0}/><text className="os-automation-numeral" x={i*308+24} y="34">{n}</text><text className="os-automation-caption" x={i*308+24} y="58">{n===0?"Fired from outside":`Reacts after earlier strata`}</text></g>)}
    {map.groups.map(g=><g key={g.key} aria-hidden="true"><title>{g.members.filter(c=>c.row.loop).map(c=>`${c.row.name}: stops when ${c.row.loop!.until}, at most ${c.row.loop!.maxDepth} runs`).join("; ")}</title><rect className={`os-automation-cycle ${g.permitted?"":"is-refused"}`} x={g.x} y={g.y} width={g.width} height={g.height} rx="8"/><text className="os-automation-caption" x={g.x+8} y={g.y+g.height-10}>{g.permitted?`Permitted: ${g.members.find(c=>c.row.loop)?.row.loop?.maxDepth ?? "bounded"} runs maximum`:"Refused cycle"}</text></g>)}
    {map.edges.map(e=><g key={`${e.from}:${e.to}`} className={`os-automation-edge ${e.decided?"":"is-undecided"} ${selected && selected!==e.from && selected!==e.to?"is-dim":""} ${selected===e.from || selected===e.to?"is-selected":""}`}><title>{e.reasons.join("\n")}</title><path d={e.path} markerEnd={`url(#${marker})`}/>{e.labels.map((label,i)=><text key={i} x={e.x} y={e.y+i*(e.decided?15:28)} textAnchor="middle">{label.length > 14 ? label.slice(0,13)+"…" : label}{!e.decided && i===0?<tspan x={e.x} dy="13">undecided</tspan>:null}</text>)}</g>)}
    {map.cards.map(c=><g key={c.row.name} ref={el=>{if(el)refs.current.set(c.row.name,el);else refs.current.delete(c.row.name);}} role="button" aria-label={`${c.row.name}, stratum ${c.row.stratum}, ${trigger(c.row)}`} aria-pressed={selected===c.row.name} tabIndex={active===c.row.name?0:-1} className={`os-automation-card ${selected===c.row.name?"is-selected":""} ${selected && selected!==c.row.name && !map.edges.some(e=>(e.from===selected&&e.to===c.row.name)||(e.to===selected&&e.from===c.row.name))?"is-dim":""}`} onFocus={()=>setFocus(c.row.name)} onClick={()=>select(c.row.name)} onKeyDown={event=>{
      if(event.key==="Enter" || event.key===" "){event.preventDefault();select(c.row.name);return;}
      if(!["ArrowUp","ArrowDown","ArrowLeft","ArrowRight"].includes(event.key))return;
      event.preventDefault();
      const horizontal=event.key==="ArrowLeft" || event.key==="ArrowRight";
      const sign=event.key==="ArrowLeft" || event.key==="ArrowUp"?-1:1;
      const next=map.cards.filter(n=>horizontal ? Math.sign(n.x-c.x)===sign : n.x===c.x && Math.sign(n.y-c.y)===sign).sort((a,b)=>horizontal?Math.abs(a.x-c.x)-Math.abs(b.x-c.x)||Math.abs(a.y-c.y)-Math.abs(b.y-c.y):Math.abs(a.y-c.y)-Math.abs(b.y-c.y))[0];
      if(next){setFocus(next.row.name);refs.current.get(next.row.name)?.focus();}
    }}><title>{c.row.name}\n{trigger(c.row)}{c.row.triggerConcept?` (${c.row.triggerConcept})`:""}</title><rect x={c.x} y={c.y} width={c.width} height={c.height} rx="4"/><text x={c.x+10} y={c.y+22}>{c.row.name.length>(c.row.mode?16:23)?c.row.name.slice(0,c.row.mode?15:22)+"…":c.row.name}</text><text className="os-automation-caption" x={c.x+10} y={c.y+42}>{trigger(c.row).length>29?trigger(c.row).slice(0,28)+"…":trigger(c.row)}</text>{c.row.mode?<text className="os-automation-mode" x={c.x+188} y={c.y+21} textAnchor="end">{c.row.mode.kind}{c.row.mode.max?` ${c.row.mode.max}`:""}</text>:null}{c.row.loop?<path className="os-automation-loop" d={`M ${c.x+179} ${c.y+4} q 12 0 12 12`}/>:null}</g>)}
  </svg>;
}
