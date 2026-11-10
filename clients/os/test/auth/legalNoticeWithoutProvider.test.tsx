import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LegalNoticeBanner } from "../../src/auth/LegalNoticeBanner";

// The Shell draws the banner, and the Shell's tests render the Shell without
// an AuthProvider. With useAuth the banner threw there and took every Shell
// test down with it (2f0d5ebd5). Without a provider there is no sign-in to
// speak about: nothing is read and nothing is drawn. legalNotice.test.tsx
// covers the signed-in behaviour with the provider mocked.

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("the legal notice outside an AuthProvider", () => {
  it("renders nothing and reads nothing instead of throwing", async () => {
    const fetcher = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    const view = render(<LegalNoticeBanner />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("status", { name: "Legal documents" })).toBeNull();
    expect(fetcher).not.toHaveBeenCalled();
    expect(view.container.innerHTML).toBe("");
  });
});
