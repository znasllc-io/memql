import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h=vi.hoisted(()=>({connection:null as unknown}));
vi.mock("../../src/live/connection",()=>({useOsConnection:()=>h.connection}));
import { AutomationsSection } from "../../src/apps/cluster/automations/AutomationsSection";
import type { Automation } from "../../src/apps/cluster/automations/layout";
const row=(name:string,over:Partial<Automation>={}):Automation=>({name,stratum:0,writes:[],publishes:[],opaque:[],edgesOut:[],...over});
const rows=[row("route",{edgesOut:[{to:"record",concept:"v1:test:item",decided:false,reason:"The filter depends on a builtin."}],loop:{maxDepth:4,until:"row.done"},opaque:["checkItem"]}),row("record",{stratum:1}),row("cleanup")];
function connection(graph=rows,stops:unknown[]=[]) {return {query:{automationGraph:vi.fn(async()=>({rows:()=>graph})),automationLoopStops:vi.fn(async()=>({rows:()=>stops}))}};}
beforeEach(()=>{h.connection=null;});
describe("Automations",()=>{
 it("describes bounded parallel refusals",async()=>{h.connection=connection([row("limited",{mode:{kind:"parallel",max:3}})]);render(<AutomationsSection/>);fireEvent.click(await screen.findByRole("button",{name:"limited"}));expect(screen.getByText("Parallel: up to 3 runs at once. A fire at the limit is refused.")).not.toBeNull();});
 it("renders loaded structure, selection and keyboard navigation",async()=>{
  h.connection=connection();render(<AutomationsSection/>);
  const route=await screen.findByRole("button",{name:/route, stratum/});fireEvent.focus(route);fireEvent.keyDown(route,{key:"ArrowRight"});expect(document.activeElement?.getAttribute("aria-label")).toContain("record, stratum");
  fireEvent.keyDown(route,{key:"Enter"});expect(screen.getByRole("region",{name:"Automation route"})).not.toBeNull();expect(screen.getByText(/Stops when row.done/)).not.toBeNull();expect(screen.getByText("Calls the map cannot see")).not.toBeNull();fireEvent.keyDown(route,{key:"Escape"});expect(screen.getByText("Read the reactions")).not.toBeNull();
 });
 it("keeps successful graph evidence when stops are refused",async()=>{const c=connection();c.query.automationLoopStops.mockRejectedValue(new Error("capability_not_held"));h.connection=c;render(<AutomationsSection/>);expect(await screen.findByText("capability_not_held")).not.toBeNull();expect(screen.getByRole("button",{name:/route, stratum/})).not.toBeNull();expect(screen.queryByText(/No loop has been stopped/)).toBeNull();});
 it("shows empty and disconnected states honestly",async()=>{const view=render(<AutomationsSection/>);expect(screen.getAllByText("Not connected to the cluster.")).toHaveLength(2);h.connection=connection([]);view.rerender(<AutomationsSection/>);expect(await screen.findByText("This cluster loaded no automations.")).not.toBeNull();expect(screen.getByText(/No loop has been stopped/)).not.toBeNull();});
 it("rereads both sources and removes stale graph on failure",async()=>{const c=connection();h.connection=c;render(<AutomationsSection/>);await screen.findByRole("button",{name:/route, stratum/});c.query.automationGraph.mockRejectedValue(new Error("Graph unavailable"));fireEvent.click(screen.getByRole("button",{name:"Read again"}));expect(await screen.findByText("Graph unavailable")).not.toBeNull();expect(screen.queryByRole("button",{name:/route, stratum/})).toBeNull();expect(c.query.automationLoopStops).toHaveBeenCalledTimes(2);});
 it("expands long chains and preserves missing bounds",async()=>{h.connection=connection([],[{runId:"stop",automationName:"refused",reason:"depth",chain:Array.from({length:10},(_,i)=>({automation:`step${i}`,runId:`r${i}`}))}]);render(<AutomationsSection/>);fireEvent.click(await screen.findByRole("button",{name:"6 more"}));expect(screen.getByText("step5")).not.toBeNull();expect(screen.getByText("Depth —, past the cap of —.")).not.toBeNull();});
 it("shows loading until a read lands",async()=>{const c=connection();let resolve!:(value:{rows:()=>Automation[]})=>void;c.query.automationGraph.mockImplementation(()=>new Promise(r=>{resolve=r;}));h.connection=c;render(<AutomationsSection/>);expect(screen.getByText("Loading automation graph")).not.toBeNull();await act(async()=>resolve({rows:()=>[]}));await waitFor(()=>expect(screen.getByText("This cluster loaded no automations.")).not.toBeNull());});
});
