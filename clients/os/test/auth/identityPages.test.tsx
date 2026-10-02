import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { IdentityScreen } from "../../src/auth/IdentityScreen";

vi.mock("../../src/auth/AuthProvider", () => {
  const auth = { config: { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os" }, authSource: { bearer: async () => "user-bearer" }, status: "signed-in" };
  return { useAuth: () => auth };
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const pending = { ClientName: "MemQL Editor", ClientId: "memql-vscode", UserCode: "ABCD-2345", UserAgent: "Safari test", SourceIP: "203.0.113.1", ExpiresAt: "Later", RequestedAt: "Now", ClientSelfRegistered: true };

describe("native identity screens", () => {
  it("keeps approval evidence accessible and sends the chosen decision once on the OS origin", async () => {
    const fetcher = vi.fn(async (_url: URL, init?: RequestInit) => json(init?.method === "POST"
      ? { page: "device", data: { Done: true, DoneApproved: true } }
      : { page: "device", csrf: "pair", data: { Pending: pending } }));
    vi.stubGlobal("fetch", fetcher);
    render(<IdentityScreen initialPath="/device?user_code=ABCD-2345" />);
    await screen.findByRole("heading", { name: "Connect this device?" });
    expect(screen.getByText("ABCD-2345")).toBeTruthy();
    expect(screen.getByText(/name has not been verified/)).toBeTruthy();
    expect(screen.getByText("Safari test").closest("details")?.open).toBe(false);
    expect(screen.getByText("203.0.113.1")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Approve device" }));
    await screen.findByRole("heading", { name: "Device connected" });
    const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0]?.[0].origin).toBe(window.location.origin);
    expect(posts[0]?.[1]?.headers).toMatchObject({ "X-CSRF-Token": "pair", Authorization: "Bearer user-bearer" });
    expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toEqual({ action: "approve", user_code: "ABCD-2345" });
    expect(screen.queryByRole("button", { name: "Approve device" })).toBeNull();
  });
  it("refreshes an expired security pair without automatically approving", async () => {
    let reads = 0;
    const fetcher = vi.fn(async (_url: URL, init?: RequestInit) => init?.method === "POST"
      ? json({ error: "Refresh this page to continue.", code: "csrf_expired" }, 403)
      : json({ page: "device", csrf: `pair-${++reads}`, data: { Pending: pending } }));
    vi.stubGlobal("fetch", fetcher);
    render(<IdentityScreen initialPath="/device?user_code=ABCD-2345" />);
    fireEvent.click(await screen.findByRole("button", { name: "Approve device" }));
    await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: "Approve device" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Refresh page" }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(1);
    expect(reads).toBe(2);
    expect(screen.getByRole("button", { name: "Approve device" }).hasAttribute("disabled")).toBe(false);
  });
  it.each([
    ["check_email", { Email: "owner@example.test", ExpiresIn: "10 minutes" }, "Check your email"],
    ["landing", { Approved: true }, "Sign-in confirmed"],
    ["enroll", { LiveHeading: "Recover your account", AccountLabel: "Owner", AuthScheme: "Recovery" }, "Recover your account"],
    ["enroll", { Heading: "Link expired", Rejection: "expired", Message: "Request a new link." }, "Link expired"],
    ["invitation", { Heading: "Accept invitation", InviterName: "Ada" }, "Accept invitation"],
    ["logout_complete", {}, "You’re signed out"],
    ["setup_resume", { Local: true, HasProof: true }, "Resume ownership setup"],
    ["error", { Heading: "Access unavailable", Message: "Contact your administrator." }, "Access unavailable"],
    ["device", { Done: true, DoneApproved: false }, "Request denied"],
    ["legal_view", { Layout: { Title: "Privacy", BrandName: "MemQL" }, Body: "## Your data\n\nPrivacy content." }, "Privacy"],
  ])("renders %s in the shared branded frame with an accessible heading", async (page, data, title) => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ page, data })));
    const { container } = render(<IdentityScreen initialPath="/test-entry" />);
    await screen.findByRole("heading", { level: 1, name: title });
    expect(screen.getByRole("main", { name: title })).toBeTruthy();
    expect(container.querySelector(".os-identity-brand svg")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Return to MemQL OS" })).toBeTruthy();
    if (page === "enroll" && "Rejection" in data) expect(screen.queryByRole("button", { name: "Add passkey" })).toBeNull();
  });
});
