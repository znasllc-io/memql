import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
const h=vi.hoisted(()=>({connection:null as unknown}));
vi.mock("../../src/live/connection",()=>({useOsConnection:()=>h.connection}));
import { useConceptRows } from "../../src/apps/concepts/useConceptRows";
import { DEFAULT_CONCEPTS_SETTINGS, sanitizeConceptsSettings } from "../../src/apps/concepts/settings";
import { fakeConnection, nodeOf } from "./harness";
const concept="v1:work:goal";
const page=(offset:number)=>Array.from({length:10},(_,n)=>nodeOf(`row-${offset+n}`,{statement:`Goal ${offset+n}`},concept));
describe("generic concept row navigation",()=>{
 it("migrates the old page-size preference to ten",()=>{
  expect(DEFAULT_CONCEPTS_SETTINGS.pageSize).toBe(10);
  expect(sanitizeConceptsSettings({version:1,pageSize:200}).pageSize).toBe(10);
 });
 it("retains loaded pages on close and reopen, with newest-first server order",async()=>{
  const connection=fakeConnection({pages:[{rows:page(0),cursor:"next"},{rows:page(10),cursor:""}]});h.connection=connection;
  let hook=renderHook(()=>useConceptRows(concept,10));
  await waitFor(()=>expect(hook.result.current.rows).toHaveLength(10));
  expect(connection.query.executeNamed).toHaveBeenCalledTimes(1);
  expect(connection.query.executeNamed.mock.calls[0]![1]).toContain('10), "createdAt", "desc")');
  await act(async()=>hook.result.current.loadMore());
  await waitFor(()=>expect(hook.result.current.rows).toHaveLength(20));
  hook.unmount();hook=renderHook(()=>useConceptRows(concept,10));
  expect(hook.result.current.rows).toHaveLength(20);
  expect(connection.query.executeNamed).toHaveBeenCalledTimes(2);
 });
 it("searches the server independently of loaded pages and restores the cached recent list",async()=>{
  const connection=fakeConnection({pages:[{rows:page(0),cursor:"next"}],searchPages:[{rows:[nodeOf("old",{statement:"Earlier needle"},concept)],cursor:""}]});h.connection=connection;
  const hook=renderHook(({search})=>useConceptRows(concept,10,search),{initialProps:{search:""}});
  await waitFor(()=>expect(hook.result.current.rows).toHaveLength(10));
  hook.rerender({search:"needle"});
  await waitFor(()=>expect(hook.result.current.rows.map(row=>row.id)).toEqual(["old"]));
  expect(connection.query.executeNamed.mock.calls[1]![0]).toBe("conceptRowsSearch");
  hook.rerender({search:""});expect(hook.result.current.rows).toHaveLength(10);
  expect(connection.query.executeNamed).toHaveBeenCalledTimes(2);
 });
});
