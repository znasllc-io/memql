import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { accountRow, adminOk, fakeConnection, roleRow, withSession } = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");

// The cluster's catalog carries more than the server offers this operator:
// Admin, a role ranked above them, one holding what they lack. The section
// draws the server's list and nothing else, so none of those may appear.
const CATALOG = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
  roleRow({ slug: "support-lead", name: "Support lead", rank: 150, predefined: false }),
  roleRow({ slug: "people-steward", name: "People steward", rank: 140, predefined: false }),
  roleRow({ slug: "site-moderator", name: "Site moderator", rank: 20, predefined: false, accountId: "acct-acme" }),
  roleRow({ slug: "release-captain", name: "Release captain", rank: 350, predefined: false }),
];

// What the server decided this operator may test, as the list answer carries it.
const OFFERED = [
  { role: "reader", name: "Viewer" },
  { role: "writer", name: "Member" },
  { role: "developer", name: "Developer" },
  { role: "support-lead", name: "Support lead" },
  { role: "site-moderator", name: "Site moderator", accountId: "acct-acme", organization: "Acme" },
];

type Message = { identityAdmin?: { roleTest?: { operation?: string; role?: string } } };

function mount(role: string, offered: Record<string, unknown>[], sessions: Record<string, unknown>[] = []) {
  const connection = fakeConnection({ activeRoles: CATALOG, clientAccountsAll: [accountRow({ id: "acct-acme", name: "Acme" })] });
  connection.dispatcher.sendAndWait.mockImplementation(async (message: Message) => {
    const operation = message.identityAdmin?.roleTest?.operation;
    if (operation === "list") return adminOk({ roleTestSessions: sessions, ...(offered.length > 0 ? { roleTestRoles: offered } : {}) });
    return adminOk();
  });
  h.connection = connection;
  const view = renderApp(
    withSession(<UsersApp sectionId="testing" navigate={() => {}} askContext={() => {}} store={memoryStore()} />, { role }),
  );
  return { connection, view };
}

/** The rows of the "Test as role" list, once the list answer has landed. */
async function offeredRows(): Promise<string[]> {
  await screen.findByRole("button", { name: /^Test as Site moderator \(Acme\)/ });
  const list = document.querySelector('[aria-label="Test as role"]');
  if (!(list instanceof HTMLElement)) throw new Error("no Test as role list");
  // The rows stand apart by their spacing: the surface draws no dividers.
  expect(list.closest('[data-os-dividers="none"]')).not.toBeNull();
  return within(list).getAllByRole("button").map((button) => button.textContent?.trim() ?? "");
}

afterEach(() => vi.restoreAllMocks());

describe("role testing", () => {
  it("role testing offers custom and organization roles", async () => {
    const expires = Math.floor(Date.now() / 1000) + 1800;
    // An owner, whom the old client-side filter would have shown Admin and
    // every catalog role below them: the server's answer is the whole list.
    const owner = mount("owner", OFFERED, [{ id: "s1", userId: "role-test-1", role: "site-moderator", expiresAt: String(expires) }]);
    expect(await offeredRows()).toEqual([
      "Test as Viewer",
      "Test as Member",
      "Test as Developer",
      "Test as Support lead",
      "Test as Site moderator (Acme)",
    ]);
    // Absent, not disabled: what the server does not offer is not drawn.
    for (const name of [/Test as Admin/, /People steward/, /Release captain/, /Owner/]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
    const active = document.querySelector('[aria-label="Active tests"]');
    if (!(active instanceof HTMLElement)) throw new Error("no Active tests list");
    expect(within(active).getByText("Site moderator (Acme)")).toBeTruthy();

    // Starting one names its role by slug.
    const tab = { close: vi.fn(), location: { replace: vi.fn() }, postMessage: vi.fn() };
    vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    await click(screen.getByRole("button", { name: /^Test as Site moderator \(Acme\)/ }));
    const starts = owner.connection.dispatcher.sendAndWait.mock.calls
      .map(([message]) => (message as Message).identityAdmin?.roleTest)
      .filter((request) => request?.operation === "start");
    expect(starts).toEqual([{ operation: "start", role: "site-moderator" }]);
    owner.view.unmount();
  });

  it("an operator offered no role is told why instead of shown an empty list", async () => {
    const developer = mount("developer", []);
    expect(
      await screen.findByText("No role can be tested from your account, because it is restricted or lacks something each role would allow."),
    ).toBeTruthy();
    expect(document.querySelector('[aria-label="Test as role"]')).toBeNull();
    developer.view.unmount();
  });
});
