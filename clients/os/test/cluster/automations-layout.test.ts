import { describe, expect, it } from "vitest";
import { layout, trigger, type Automation } from "../../src/apps/cluster/automations/layout";
export const automation = (name:string,stratum=0,over:Partial<Automation>={}):Automation => ({name,stratum,writes:[],publishes:[],opaque:[],edgesOut:[],...over});
const edge=(to:string)=>({to,concept:"v1:test:item",decided:true,reason:"Writes the trigger concept."});
describe("automation graph layout",()=>{
 it("describes each before-write timing",()=>{for(const timing of ["create","update","write"])expect(trigger(automation("adjust",0,{triggerKind:`before.${timing}`,triggerConcept:"v1:test:item"}))).toBe(`before ${timing} of item`);});
 const cycle={members:["alpha","beta"],permittedBy:["alpha"],permitted:true};
 const rows=[automation("alpha",0,{edgesOut:[edge("beta")],cycle}),automation("beta",0,{edgesOut:[edge("alpha"),edge("gamma")],cycle}),automation("gamma",1),automation("alone")];
 it("sorts its own inputs and excludes stand-alone nodes from the graph",()=>{
  const a=layout(rows); expect(layout([...rows].reverse())).toEqual(a);expect(a.columns).toEqual([0,1]);expect(a.standalone.map(x=>x.name)).toEqual(["alone"]);
 });
 it("encloses every SCC member without overlapping cards",()=>{
  const a=layout(rows);expect(a.groups).toHaveLength(1);
  for(const c of a.groups[0]!.members){const g=a.groups[0]!;expect(c.x).toBeGreaterThan(g.x);expect(c.y+c.height).toBeLessThan(g.y+g.height);}
  for(const c of a.cards)for(const d of a.cards)if(c!==d)expect(c.x+c.width<=d.x||d.x+d.width<=c.x||c.y+c.height<=d.y||d.y+d.height<=c.y).toBe(true);
 });
 it("puts every edge endpoint on a card edge, including self loops",()=>{
  const a=layout([...rows,automation("self",2,{edgesOut:[edge("self")]})]);
  for(const e of a.edges){const from=a.cards.find(c=>c.row.name===e.from)!;const to=a.cards.find(c=>c.row.name===e.to)!;expect(e.start[0]).toBe(from.x+from.width);expect([to.x,to.x+to.width]).toContain(e.end[0]);expect(e.end[1]).toBeGreaterThanOrEqual(to.y);expect(e.end[1]).toBeLessThanOrEqual(to.y+to.height);}
 });
 it("coalesces concepts on one pair and preserves undecided evidence",()=>{const a=layout([automation("a",0,{edgesOut:[edge("b"),{...edge("b"),concept:"v1:test:other",decided:false}]}),automation("b",1)]);expect(a.edges).toHaveLength(1);expect(a.edges[0]!.labels).toEqual(["item","other"]);expect(a.edges[0]!.decided).toBe(false);});
});
