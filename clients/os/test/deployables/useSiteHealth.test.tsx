import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({connection:null as unknown}));
vi.mock("../../src/live/connection", () => ({useOsConnection:() => h.connection}));
import { useSiteHealth } from "../../src/apps/deployables/useSiteHealth";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { siteStateWord } from "../../src/apps/deployables/words";
import { actsFor } from "../../src/apps/deployables/page/acts";
import { ALL_PARTS } from "../../src/apps/deployables/parts";
const site=siteFromRow({id:"one",hostname:"one.example.com",bundleRef:"blob://one",status:"live",systemOwned:true});
afterEach(()=>{vi.useRealTimers();h.connection=null;});
describe("shared website observations",()=>{
 it("abandons a stalled read and retries on the next poll",async()=>{
  vi.useFakeTimers();
  let calls=0;
  const executeNamed=vi.fn((_name:string,_call:string,opts?:{signal:AbortSignal})=>{
   calls++;
   if(calls===1) return new Promise((_,reject)=>opts?.signal.addEventListener("abort",()=>reject(new Error("aborted")),{once:true}));
   return Promise.resolve({rows:()=>[{siteId:site.id,hostname:site.hostname,bundleRef:site.bundleRef,checkedAt:new Date().toISOString(),state:"reachable",httpStatus:200}]});
  });
  h.connection={query:{executeNamed}};
  const {result}=renderHook(()=>useSiteHealth([site],true));
  await act(async()=>{vi.advanceTimersByTime(10_000);});
  expect(executeNamed.mock.calls[0]![2]!.signal.aborted).toBe(true);
  await act(async()=>{vi.advanceTimersByTime(20_000);});
  expect(siteStateWord(result.current[0]!)).toBe("Live");
 });
 it("expires green while the connection is absent and recovers from fresh checks",async()=>{
  vi.useFakeTimers();
  const observation={siteId:site.id,hostname:site.hostname,bundleRef:site.bundleRef,checkedAt:new Date().toISOString(),state:"reachable",httpStatus:200,reason:"Website answered successfully"};
  const executeNamed=vi.fn(async()=>({rows:()=>[observation]}));
  h.connection={query:{executeNamed}};
  const {result,rerender}=renderHook(()=>useSiteHealth([site],true));
  await act(async()=>{});
  expect(siteStateWord(result.current[0]!)).toBe("Live");
  h.connection=null;rerender();
  await act(async()=>{vi.advanceTimersByTime(330_000);});
  expect(siteStateWord(result.current[0]!)).toBe("Unknown");
  expect(actsFor({site:result.current[0]!,pkg:null,run:null,can:ALL_PARTS}).tone).toBe("none");
  observation.checkedAt=new Date().toISOString();observation.state="unavailable";observation.httpStatus=404;
  h.connection={query:{executeNamed}};rerender();
  await act(async()=>{});
  expect(siteStateWord(result.current[0]!)).toBe("Unavailable");
  expect(actsFor({site:result.current[0]!,pkg:null,run:null,can:ALL_PARTS}).state).toBe("Unavailable");
 });
 it("ignores a late response for a replaced deployment and clears unreadable observations",async()=>{
  let finish: (value:unknown)=>void=()=>{};
  h.connection={query:{executeNamed:()=>new Promise(resolve=>{finish=resolve;})}};
  const {result,rerender}=renderHook(({sites})=>useSiteHealth(sites,true),{initialProps:{sites:[site]}});
  const first=finish;
  const replacement={...site,bundleRef:"blob://replacement"};
  rerender({sites:[replacement]});
  await act(async()=>{first({rows:()=>[{siteId:site.id,hostname:site.hostname,bundleRef:site.bundleRef,checkedAt:new Date().toISOString(),state:"reachable"}]});});
  expect(siteStateWord(result.current[0]!)).toBe("Unknown");
  h.connection={query:{executeNamed:async()=>{throw new Error("disconnected");}}};rerender({sites:[replacement]});
  await act(async()=>{});
  expect(result.current[0]!.health).toBeUndefined();
 });
});
