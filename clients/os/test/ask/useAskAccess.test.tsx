import { cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { rowsResult } from "../cluster/harness";

import { useAskAccess } from "../../src/ask/useAskAccess";
import { DesktopAccessContext } from "../../src/chrome/desktopAccessContext";
import type { DesktopAccessAnswer } from "../../src/chrome/useDesktopAccess";

afterEach(() => { cleanup(); h.connection = null; });
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

it("names a lender by display name for a person who holds no files part, asking once per id", async () => {
  const userDisplayById = vi.fn(async ({ userId }: { userId: string }) => rowsResult(userId === "u1" ? [{ id: userId, displayName: "Priya" }] : []));
  h.connection = { query: { userDisplayById } };
  const sources = [
    { kind: "machine", state: "asleep", own: false, lenderUserId: "u1" },
    { kind: "key", state: "outOfCredit", own: false, lenderUserId: "u1" },
    { kind: "key", state: "outOfCredit", own: false, lenderUserId: "u2" },
  ];
  const { result, rerender } = renderHook(useAskAccess, { wrapper: within({ state: "admitted", exempt: false, sources }) });
  await waitFor(() => expect(result.current.nameOf?.("u1")).toBe("Priya"));
  expect(result.current.nameOf?.("u2")).toBe("the person who lent it");
  rerender();
  expect(userDisplayById).toHaveBeenCalledTimes(2);
});
