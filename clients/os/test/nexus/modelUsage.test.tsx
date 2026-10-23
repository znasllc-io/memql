import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { useModelUsage } from "../../src/apps/nexus/useModelUsage";
import { UsageDetails } from "../../src/apps/nexus/UsageDetails";
import { fakeConnection, rowsResult } from "./harness";
import capture from "./fixtures/liveQuickReply.json";
const h=vi.hoisted(()=>({connection:null as unknown}));
vi.mock("../../src/live/connection",()=>({useOsConnection:()=>h.connection}));
function View({runId=capture.run.id,active=false}:{runId?:string;active?:boolean}) { const usage=useModelUsage(runId,active);return <UsageDetails usage={usage} />; }

describe("bounded usage reading",()=>{
  it("loads an additional cursor page only on request and deduplicates its overlapping call",async()=>{
    const conn=fakeConnection();h.connection=conn;
    conn.query.workModelUsageForOwnerRun.mockResolvedValueOnce(rowsResult([capture.modelUsage[0]!],"older-page")).mockResolvedValueOnce(rowsResult(capture.modelUsage));
    render(<View />);
    await screen.findByText("7.5k estimated tokens shown");
    expect(conn.query.workModelUsageForOwnerRun).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button",{name:"Load more usage"}));
    await screen.findByText("7.9k estimated tokens");
    expect(conn.query.workModelUsageForOwnerRun.mock.calls[1]?.[1]).toMatchObject({cursor:"older-page"});
    expect(screen.queryByRole("button",{name:"Load more usage"})).toBeNull();
    expect(screen.getByText("2")).toBeTruthy();
  });
  it("refreshes the head when a paged live run finishes, then keeps the new page partial until read",async()=>{
    vi.useFakeTimers();
    try {
      const conn=fakeConnection();h.connection=conn;
      const newer={...capture.modelUsage[0]!,id:"new-completed-call",inputTokens:100,outputTokens:20};
      conn.query.workModelUsageForOwnerRun
        .mockResolvedValueOnce(rowsResult([capture.modelUsage[0]!],"older-page"))
        .mockResolvedValueOnce(rowsResult([capture.modelUsage[1]!]))
        .mockResolvedValueOnce(rowsResult([newer],"final-older-page"))
        .mockResolvedValueOnce(rowsResult(capture.modelUsage))
        .mockResolvedValueOnce(rowsResult([newer,...capture.modelUsage]));
      const view=render(<View active />);
      await act(async()=>{});
      await act(async()=>{fireEvent.click(screen.getByRole("button",{name:"Load more usage"}));});
      expect(screen.getByText("7.9k estimated tokens shown")).toBeTruthy();
      // Reading history pauses automatic head refresh, without claiming that
      // this growing run's loaded snapshot is complete or resetting the page.
      await act(async()=>{await vi.advanceTimersByTimeAsync(8000);});
      expect(conn.query.workModelUsageForOwnerRun).toHaveBeenCalledTimes(2);
      await act(async()=>{view.rerender(<View active={false} />);});
      expect(conn.query.workModelUsageForOwnerRun).toHaveBeenCalledTimes(3);
      expect(conn.query.workModelUsageForOwnerRun.mock.calls[2]?.[1]).not.toHaveProperty("cursor");
      expect(screen.getByText("120 estimated tokens shown")).toBeTruthy();
      await act(async()=>{fireEvent.click(screen.getByRole("button",{name:"Load more usage"}));});
      expect(conn.query.workModelUsageForOwnerRun.mock.calls[3]?.[1]).toMatchObject({cursor:"final-older-page"});
      expect(screen.getByText("8.0k estimated tokens")).toBeTruthy();
      expect(screen.getByText("3")).toBeTruthy();
      // A fully loaded older snapshot can also be refreshed explicitly.
      await act(async()=>{fireEvent.click(screen.getByRole("button",{name:"Refresh usage"}));});
      expect(conn.query.workModelUsageForOwnerRun.mock.calls[4]?.[1]).not.toHaveProperty("cursor");
      expect(screen.getByText("8.0k estimated tokens")).toBeTruthy();
    } finally { vi.useRealTimers(); }
  });
  it("retains clearly incomplete usage after a refresh failure and offers retry",async()=>{
    const conn=fakeConnection();h.connection=conn;
    conn.query.workModelUsageForOwnerRun.mockResolvedValueOnce(rowsResult([capture.modelUsage[0]!],"older-page")).mockRejectedValueOnce(Error("Receipts temporarily unavailable")).mockResolvedValueOnce(rowsResult(capture.modelUsage));
    render(<View />);
    fireEvent.click(await screen.findByRole("button",{name:"Load more usage"}));
    await screen.findByText("Usage could not be refreshed.");
    expect(screen.getByText("7.5k estimated tokens shown")).toBeTruthy();
    fireEvent.click(screen.getByRole("button",{name:"Read usage again"}));
    await screen.findByText("7.9k estimated tokens");
  });
  it("does not expose the previous run's receipts while another read fails or a late response arrives",async()=>{
    const conn=fakeConnection();h.connection=conn;
    let finish!:(value:Result)=>void;
    conn.query.workModelUsageForOwnerRun.mockImplementationOnce(()=>new Promise(resolve=>{finish=resolve;})).mockRejectedValueOnce(Error("Another run unavailable"));
    const view=render(<View />);
    await waitFor(()=>expect(conn.query.workModelUsageForOwnerRun).toHaveBeenCalledTimes(1));
    view.rerender(<View runId="another-run" />);
    await screen.findByText("Usage could not be refreshed.");
    await act(async()=>{finish(rowsResult(capture.modelUsage));});
    expect(screen.getByText("Usage not reported")).toBeTruthy();
    expect(screen.queryByText("7.9k estimated tokens")).toBeNull();
  });
});
