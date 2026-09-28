import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it } from "vitest";

import { AskProvider, useAsk } from "../../src/ask/AskProvider";
import { useWindowAsk } from "../../src/chrome/windowAsk";
import { StubAskTransport } from "../ask/stubTransport";

// What an app's two Ask props do (clients/os/src/system/registry.ts).
//
// `askContext` notes what the person is looking at. It used to OPEN Ask: the
// frame wired it to openAsk, so opening a run, a goal or a procedure in Nexus,
// a Bin item, a Materializer composition or a Training upload threw the Ask
// window over the app the person was using. It now only remembers, and the
// window's own Ask button carries what it remembered. `askAbout` is the one
// that opens -- for an explicit "Ask about this" the person chose.

function wrapper({ children }: { children: ReactNode }) {
  return <AskProvider transport={new StubAskTransport()}>{children}</AskProvider>;
}

function useHarness() {
  const ask = useAsk();
  const windowAsk = useWindowAsk("app:nexus section:runs");
  return { ask, windowAsk };
}

describe("an app's Ask props", () => {
  it("askContext never opens Ask", () => {
    const { result } = renderHook(useHarness, { wrapper });
    act(() => result.current.windowAsk.askContext("nexus run:abc123"));
    expect(result.current.ask.sheet.open).toBe(false);
  });

  it("the window's Ask carries the context the app noted", () => {
    const { result } = renderHook(useHarness, { wrapper });
    act(() => result.current.windowAsk.askContext("nexus run:abc123"));
    expect(result.current.windowAsk.contextTag()).toBe("app:nexus section:runs nexus run:abc123");
  });

  it("a newer note replaces an older one", () => {
    const { result } = renderHook(useHarness, { wrapper });
    act(() => result.current.windowAsk.askContext("nexus run:abc123"));
    act(() => result.current.windowAsk.askContext("nexus goal:def456"));
    expect(result.current.windowAsk.contextTag()).toBe("app:nexus section:runs nexus goal:def456");
  });

  it("askAbout opens Ask about the thing the person chose", () => {
    const { result } = renderHook(useHarness, { wrapper });
    act(() => result.current.windowAsk.askAbout("app:files/browse file:notes.md"));
    expect(result.current.ask.sheet.open).toBe(true);
    expect(result.current.ask.sheet.context).toBe("app:files/browse file:notes.md");
  });
});
