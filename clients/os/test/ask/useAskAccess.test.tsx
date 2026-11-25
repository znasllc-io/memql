import { cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it } from "vitest";

import { useAskAccess } from "../../src/ask/useAskAccess";
import { DesktopAccessContext } from "../../src/chrome/desktopAccessContext";
import type { DesktopAccessAnswer } from "../../src/chrome/useDesktopAccess";

afterEach(cleanup);
const within = (answer: DesktopAccessAnswer | null) =>
  ({ children }: { children: ReactNode }) => <DesktopAccessContext.Provider value={answer}>{children}</DesktopAccessContext.Provider>;

it("takes the sources from the gate's answer, with no shell meaning no Fleet", () => {
  const sources = [{ kind: "key", state: "outOfCredit", own: true }];
  const { result } = renderHook(useAskAccess, { wrapper: within({ state: "admitted", exempt: false, sources }) });
  expect(result.current.canOpenFleet).toBe(false);
  expect(result.current.sources).toEqual(sources);
  expect(result.current.nameOf?.("anyone")).toBe("the person who lent it");
});

it("has no sources for an unknown or held answer, or outside the gate", () => {
  for (const answer of [{ state: "unknown" } as const, { state: "held", reference: "", reasonsDigest: "" } as const, null]) {
    const { result } = renderHook(useAskAccess, { wrapper: within(answer) });
    expect(result.current.sources).toEqual([]);
  }
});
