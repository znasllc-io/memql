import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { IdentityScreen } from "../../src/auth/IdentityScreen";
import { scrollNextPage } from "../scrollNextPage";

vi.mock("../../src/auth/AuthProvider", () => ({ useAuth: () => auth }));
const auth = {
  config: { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os", authEnabled: true, domain: "example.test" },
  authSource: { bearer: async () => "fixture-bearer" }, status: "signed-in",
};
const token = (id: string, label: string) => ({ ID: id, Label: label, Active: true });
const json = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status });
const page = (tokens: unknown[], cursor = "", fresh = "") => json({ page: "me/tokens", csrf: "fixture-csrf", data: { Tokens: tokens, NextCursor: cursor, NewToken: fresh } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("personal token scroll pages", () => {
  it("retains token rows and the one-time secret through a slow failure, then retries the same cursor and deduplicates", async () => {
    let fail!: () => void;
    const fetcher = vi.fn().mockResolvedValueOnce(page([token("a", "First")], "older", "fixture-one-time"))
      .mockImplementationOnce(() => new Promise(resolve => { fail = () => resolve(json({ error: "temporarily unavailable" }, 503)); }))
      .mockResolvedValueOnce(page([token("a", "First updated"), token("b", "Older")], ""));
    vi.stubGlobal("fetch", fetcher);
    render(<IdentityScreen initialPath="/me/tokens" embedded />);
    await screen.findByText("First");
    const row = within(screen.getByRole("list", { name: "Tokens" })).getByText("First");
    await scrollNextPage();
    expect(within(screen.getByRole("list", { name: "Tokens" })).getByText("First")).toBe(row);
    expect(screen.getByText("fixture-one-time")).toBeTruthy();
    await act(async () => fail());
    await screen.findByRole("button", { name: "Try again" });
    expect(screen.getByText("First")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByText("Older");
    const list = screen.getByRole("list", { name: "Tokens" });
    expect(list.querySelectorAll(".os-record-row")).toHaveLength(2);
    expect(within(list).getByText("First updated")).toBeTruthy();
    expect(screen.getByText("fixture-one-time")).toBeTruthy();
    expect(screen.getAllByText("End of list")).toHaveLength(1);
    expect(fetcher).toHaveBeenCalledTimes(3);
    for (const [url, options] of fetcher.mock.calls.slice(1)) {
      expect(String(url)).toContain("/me/tokens?cursor=older");
      expect(options.headers.Authorization).toBe("Bearer fixture-bearer");
    }
  });

  it("discards a late token page after navigating to another personal section", async () => {
    let finish!: () => void;
    const fetcher = vi.fn().mockResolvedValueOnce(page([token("a", "First")], "older"))
      .mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(page([token("b", "Late token")])); }))
      .mockResolvedValueOnce(json({ page: "me/settings", data: {} }));
    vi.stubGlobal("fetch", fetcher);
    const view = render(<IdentityScreen initialPath="/me/tokens" embedded />);
    await screen.findByText("First");
    await scrollNextPage();
    view.rerender(<IdentityScreen initialPath="/me/settings" embedded />);
    await screen.findByText("Manage how you sign in and which devices can access your account.");
    await act(async () => finish());
    expect(screen.queryByText("Late token")).toBeNull();
    expect(screen.queryByRole("list", { name: "Tokens" })).toBeNull();
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  });
});
