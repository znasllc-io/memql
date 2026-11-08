import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { Result, type Connection } from "@znasllc-io/memql-sdk-core/client";
import { ConnectionStatusContext } from "../../src/chrome/connection";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { LiveReadVisibility } from "../../src/live/useGraphRevision";
import { disposeSessionCollections } from "../../src/live/sessionCollections";
import { useDeployableDirectory } from "../../src/apps/deployables/useDirectory";
import { DEFAULT_LIST_FILTER } from "../../src/apps/deployables/list";
afterEach(() => { cleanup(); if(h.connection) disposeSessionCollections(h.connection as Connection); vi.useRealTimers(); });

it.each([100, 5100])("repairs a computed directory once after a %i ms parked interval and still follows changes during repair", async pause => {
 vi.useFakeTimers();
 const handlers=new Set<()=>void>();
 const answer=()=>new Result({data:[{entries:[],total:0,archivedCount:0,nextCursor:""}]} as never);
 const read=vi.fn().mockImplementation(async()=>answer());
 h.connection={status:"connected",query:{deployableDirectoryPage:read},subscriptions:{subscribeGraph:(handler:()=>void)=>{handlers.add(handler);return()=>handlers.delete(handler);}}};
 let visible=true;
 const wrapper=({children}:{children:ReactNode})=><ConnectionStatusContext.Provider value="connected"><LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider></ConnectionStatusContext.Provider>;
 const hook=renderHook(()=>useDeployableDirectory("deployables",DEFAULT_LIST_FILTER,false,true),{wrapper});
 await act(async()=>{});
 expect(read).toHaveBeenCalledTimes(1);
 visible=false;hook.rerender();
 await act(async()=>vi.advanceTimersByTime(pause));
 let finish!:()=>void;
 read.mockImplementationOnce(()=>new Promise(resolve=>{finish=()=>resolve(answer());}));
 visible=true;hook.rerender();
 expect(read).toHaveBeenCalledTimes(2);
 await act(async()=>finish());
 expect(read).toHaveBeenCalledTimes(2);
 // An actual input event during an in-flight refresh is different from two
 // notifications of the same resume: it must queue a following repair.
 read.mockImplementationOnce(()=>new Promise(resolve=>{finish=()=>resolve(answer());}));
 act(()=>hook.result.current.reseed());
 expect(read).toHaveBeenCalledTimes(3);
 await act(async()=>{ for(const handler of handlers) handler(); vi.advanceTimersByTime(100); });
 await act(async()=>finish());
 expect(read).toHaveBeenCalledTimes(4);
});
