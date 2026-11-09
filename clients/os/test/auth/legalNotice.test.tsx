import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LegalNoticeBanner, LegalNoticeView } from "../../src/auth/LegalNoticeBanner";
import { dismissLegalNotices, legalNoticesFrom, loadLegalNotices, legalNoticesDismissed, type LegalNotice } from "../../src/auth/legalNotice";
import type { OsRuntimeConfig } from "../../src/cluster/config";

// The documents promise notice in the platform when a version changes. A
// sign-in that recorded a version is exactly that moment, identity names it
// on the account overview, and the OS says so once, without blocking anything.

const auth = vi.hoisted(() => ({
  value: {
    config: { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os", authEnabled: true, domain: "example.test" },
    authSource: { bearer: async () => "test-bearer", refresh: async () => "test-bearer" },
    status: "signed-in",
  } as { config: Record<string, unknown>; authSource: { bearer: () => Promise<string | null>; refresh: () => Promise<string | null> }; status: string },
}));
vi.mock("../../src/auth/AuthProvider", () => ({ useAuth: () => auth.value }));
vi.mock("../../src/auth/roleTest", () => ({ isRoleTestTab: () => false }));

const config = auth.value.config as unknown as OsRuntimeConfig;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const tos: LegalNotice = { documentType: "tos", document: "Terms of Service", version: "2.0.0", effectiveDate: "2026-11-09", changed: true };
const privacy: LegalNotice = { documentType: "privacy", document: "Privacy Notice", version: "2.0.0", effectiveDate: "2026-11-09", changed: false };
const overview = (notices: unknown) => json({ page: "me/profile", data: { Profile: { Email: "me@example.test" }, LegalNotices: notices } });

beforeEach(() => { sessionStorage.clear(); auth.value.status = "signed-in"; });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); sessionStorage.clear(); });

describe("legal notices from the account overview", () => {
  it("reads what identity says this sign-in accepted and ignores malformed items", () => {
    const page = { page: "me/profile", data: { LegalNotices: [
      { DocumentType: "tos", Document: "Terms of Service", Version: "2.0.0", EffectiveDate: "2026-11-09", Changed: true },
      { DocumentType: "privacy", Version: "2.0.0" },
      { Document: "nothing" },
      "junk",
    ] } };
    expect(legalNoticesFrom(page)).toEqual([
      tos,
      { documentType: "privacy", document: "privacy", version: "2.0.0", effectiveDate: "", changed: false },
    ]);
    expect(legalNoticesFrom({ page: "me/profile", data: {} })).toEqual([]);
    expect(legalNoticesFrom({ page: "me/profile", data: { LegalNotices: null } })).toEqual([]);
  });

  it("fetches the overview with the bearer and answers nothing on any failure", async () => {
    const fetcher = vi.fn(async () => overview([{ DocumentType: "tos", Document: "Terms of Service", Version: "2.0.0", EffectiveDate: "2026-11-09", Changed: true }]));
    vi.stubGlobal("fetch", fetcher);
    expect(await loadLegalNotices(config, auth.value.authSource)).toEqual([tos]);
    const [url, init] = fetcher.mock.calls[0] as unknown as [URL, RequestInit];
    expect(String(url)).toBe(`${location.origin}/me/`);
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer test-bearer");
    expect((init.headers as Record<string, string>).Accept).toContain("vnd.memql.identity");

    vi.stubGlobal("fetch", vi.fn(async () => json({ error: "Identity is temporarily unavailable" }, 503)));
    expect(await loadLegalNotices(config, auth.value.authSource)).toEqual([]);
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("Failed to fetch"); }));
    expect(await loadLegalNotices(config, auth.value.authSource)).toEqual([]);
    expect(await loadLegalNotices(config, { bearer: async () => null, refresh: async () => null })).toEqual([]);
  });

  it("remembers a dismissal for exactly these versions, in this tab", () => {
    expect(legalNoticesDismissed([tos, privacy])).toBe(false);
    dismissLegalNotices([tos, privacy]);
    expect(legalNoticesDismissed([tos, privacy])).toBe(true);
    expect(legalNoticesDismissed([{ ...tos, version: "2.1.0" }, privacy])).toBe(false);
    expect(legalNoticesDismissed([])).toBe(true);
  });
});

describe("the notice", () => {
  it("names each document, its version and its effective date, says what signing in did, and links to the text", () => {
    const onDismiss = vi.fn();
    render(<LegalNoticeView notices={[tos, privacy]} onDismiss={onDismiss} />);
    const notice = screen.getByRole("status", { name: "Legal documents" });
    expect(notice.textContent).toContain("Terms of Service");
    expect(notice.textContent).toContain("2.0.0");
    expect(notice.textContent).toContain("2026-11-09");
    expect(notice.textContent).toMatch(/changed/);
    expect(notice.textContent).toContain("Privacy Notice");
    expect(notice.textContent).toMatch(/accepted/);
    expect((screen.getByRole("link", { name: "Read the Terms of Service" }) as HTMLAnchorElement).getAttribute("href")).toBe("/identity/legal/tos#");
    expect((screen.getByRole("link", { name: "Read the Privacy Notice" }) as HTMLAnchorElement).getAttribute("href")).toBe("/identity/legal/privacy#");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("appears after sign-in with what this sign-in accepted, and goes away for the tab when dismissed", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => overview([{ DocumentType: "tos", Document: "Terms of Service", Version: "2.0.0", EffectiveDate: "2026-11-09", Changed: true }])));
    const view = render(<LegalNoticeBanner />);
    expect(await screen.findByRole("status", { name: "Legal documents" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("status", { name: "Legal documents" })).toBeNull();
    view.unmount();
    render(<LegalNoticeBanner />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("status", { name: "Legal documents" })).toBeNull();
  });

  it("shows nothing when the sign-in recorded nothing, when signed out, or when the overview cannot be read", async () => {
    const fetcher = vi.fn(async () => overview([]));
    vi.stubGlobal("fetch", fetcher);
    const view = render(<LegalNoticeBanner />);
    await waitFor(() => expect(fetcher).toHaveBeenCalled());
    expect(screen.queryByRole("status")).toBeNull();
    view.unmount();

    vi.stubGlobal("fetch", vi.fn(async () => json({ error: "away" }, 503)));
    const failed = render(<LegalNoticeBanner />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("status")).toBeNull();
    failed.unmount();

    const idle = vi.fn(async () => overview([]));
    vi.stubGlobal("fetch", idle);
    auth.value.status = "signed-out";
    render(<LegalNoticeBanner />);
    await act(async () => { await Promise.resolve(); });
    expect(idle).not.toHaveBeenCalled();
    expect(screen.queryByRole("status")).toBeNull();
  });
});
