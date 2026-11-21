import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ShareFoot, type FileShare, type SharePerson } from "../../src/apps/files/sharing";
import { MIN_HOLD_MS } from "../../src/kit/oneAtATime";

// The share panel at the foot of the Files rail (Jesús, Nov 18 2026): names
// only until a person is chosen; then, above the names, the days and Share
// for someone new, or what is left, Renew and Stop sharing for someone who
// already has the file.

const people: SharePerson[] = [
  { userId: "u-dana", displayName: "Dana Cole" },
  { userId: "u-lee", displayName: "Lee Park" },
];
const NOW = new Date("2026-11-18T12:00:00Z");
const danaShare: FileShare = {
  shareId: "s-1", recipientUserId: "u-dana", recipientDisplayName: "Dana Cole", days: 7,
  expiresAt: "2026-11-25T12:00:00Z",
};

type ShareAct = (recipientUserId: string, days: number) => Promise<void>;
type ShareRowAct = (share: FileShare) => Promise<void>;

function renderFoot(shares: FileShare[] = [], acts: { onShare?: ShareAct; onRenew?: ShareRowAct; onStop?: ShareRowAct } = {}) {
  const onShare = acts.onShare ?? vi.fn<ShareAct>(async () => {});
  const onRenew = acts.onRenew ?? vi.fn<ShareRowAct>(async () => {});
  const onStop = acts.onStop ?? vi.fn<ShareRowAct>(async () => {});
  render(<ShareFoot people={people} shares={shares} onShare={onShare} onRenew={onRenew} onStop={onStop} now={() => NOW} />);
  return { onShare, onRenew, onStop };
}

const click = async (el: Element) => { await act(async () => { fireEvent.click(el); }); };


/** Waits out the half second a press holds its button (kit/oneAtATime). */
const holdOver = () => act(async () => { await new Promise((resolve) => setTimeout(resolve, MIN_HOLD_MS)); });
describe("the share panel at the foot of the rail", () => {
  it("shows only Share with and the names until a person is chosen", () => {
    renderFoot([danaShare]);
    const foot = screen.getByRole("region", { name: "Share with" });
    expect(within(foot).getAllByRole("button").map((b) => b.textContent)).toEqual(["DCDana Colesharing", "LPLee Park"]);
    expect(within(foot).queryByRole("radiogroup")).toBeNull();
    expect(within(foot).queryByRole("button", { name: "Share" })).toBeNull();
  });

  it("asks for how long, above the names, when someone new is chosen, and shares for 7 days by default", async () => {
    const { onShare } = renderFoot();
    await click(screen.getByRole("button", { name: /Lee Park/ }));
    const ask = screen.getByRole("group", { name: "Lee Park" });
    const list = screen.getByRole("list");
    expect(ask.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(ask).getByRole("radio", { name: "7 days" }).getAttribute("aria-checked")).toBe("true");
    await click(within(ask).getByRole("radio", { name: "10 days" }));
    await click(within(ask).getByRole("button", { name: "Share" }));
    expect(onShare).toHaveBeenCalledWith("u-lee", 10);
  });

  it("Cancel closes the question without sharing", async () => {
    const { onShare } = renderFoot();
    await click(screen.getByRole("button", { name: /Dana Cole/ }));
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("group", { name: "Dana Cole" })).toBeNull();
    expect(onShare).not.toHaveBeenCalled();
  });

  it("shows what is left, Renew and Stop sharing for someone who already has it", async () => {
    const { onRenew, onStop } = renderFoot([danaShare]);
    await click(screen.getByRole("button", { name: /Dana Cole/ }));
    const ask = screen.getByRole("group", { name: "Dana Cole" });
    expect(ask.textContent).toContain("7 days left");
    expect(within(ask).queryByRole("radiogroup")).toBeNull();
    await click(within(ask).getByRole("button", { name: "Renew" }));
    expect(onRenew).toHaveBeenCalledWith(danaShare);
    await click(within(ask).getByRole("button", { name: "Stop sharing" }));
    expect(onStop).toHaveBeenCalledWith(danaShare);
    expect(screen.queryByRole("group", { name: "Dana Cole" })).toBeNull();
  });

  it("says what the engine refused, in place", async () => {
    renderFoot([], { onShare: vi.fn<ShareAct>(async () => { throw new Error("Lee Park is not in your organization."); }) });
    await click(screen.getByRole("button", { name: /Lee Park/ }));
    await click(screen.getByRole("button", { name: "Share" }));
    expect(screen.getByRole("alert").textContent).toBe("Lee Park is not in your organization.");
  });

  // José, Nov 19 (item 7, draft B): a burst of presses is one share; the
  // button says "Sharing…", then "Shared" with a tick.
  it("counts a burst of Share presses as one and says it is shared", async () => {
    let answer = () => {};
    const onShare = vi.fn<ShareAct>(() => new Promise<void>((resolve) => { answer = resolve; }));
    renderFoot([], { onShare });
    await click(screen.getByRole("button", { name: /Lee Park/ }));
    const share = screen.getByRole("button", { name: "Share" });
    await act(async () => { for (let i = 0; i < 20; i++) fireEvent.click(share); });
    expect(onShare).toHaveBeenCalledOnce();
    expect(share.textContent).toBe("Sharing…");
    await act(async () => { answer(); });
    await holdOver();
    expect(share.textContent).toBe("Shared");
    expect(share.getAttribute("data-state")).toBe("done");
  });

  it("never says Shared when the engine refused", async () => {
    renderFoot([], { onShare: vi.fn<ShareAct>(async () => { throw new Error("No."); }) });
    await click(screen.getByRole("button", { name: /Lee Park/ }));
    await click(screen.getByRole("button", { name: "Share" }));
    expect(screen.getByRole("alert").textContent).toBe("No.");
    await holdOver();
    expect(screen.getByRole("button", { name: "Share" }).textContent).toBe("Share");
  });

  it("keeps listing someone who still has the file after leaving the organization", () => {
    renderFoot([{ ...danaShare, recipientUserId: "u-gone", recipientDisplayName: "Ana Gone" }]);
    expect(screen.getByRole("button", { name: /Ana Gone/ }).textContent).toContain("sharing");
  });
});
