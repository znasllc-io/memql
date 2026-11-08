import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { accountRow, adminOk, adminRefusal, fakeConnection, roleRow, withSession } = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");

// The ladder in miniature, with four custom roles: one below a developer, one
// scoped to an organization, one between developer and owner, one retired.
const LADDER = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "developer", name: "Developer", rank: 300 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
  roleRow({ slug: "support-lead", name: "Support lead", rank: 150, predefined: false }),
  roleRow({ slug: "site-moderator", name: "Site moderator", rank: 20, predefined: false, accountId: "acct-acme" }),
  roleRow({ slug: "release-captain", name: "Release captain", rank: 350, predefined: false }),
  roleRow({ slug: "retired-helper", name: "Retired helper", rank: 30, predefined: false, active: false }),
];

type Message = { identityAdmin?: { roleTest?: { operation?: string; role?: string } } };

function mount(role: string, sessions: Record<string, unknown>[] = []) {
  const connection = fakeConnection({ activeRoles: LADDER, clientAccountsAll: [accountRow({ id: "acct-acme", name: "Acme" })] });
  // The server is the only judge of whether an offered role may be tested: a
  // start here is refused exactly as the engine refuses one.
  connection.dispatcher.sendAndWait.mockImplementation(async (message: Message) => {
    const operation = message.identityAdmin?.roleTest?.operation;
    if (operation === "list") return adminOk({ roleTestSessions: sessions });
    if (operation === "start") return adminRefusal("role testing is unavailable for this identity or role");
    return adminOk();
  });
  h.connection = connection;
  const view = renderApp(
    withSession(<UsersApp sectionId="testing" navigate={() => {}} askContext={() => {}} store={memoryStore()} />, { role }),
  );
  return { connection, view };
}

/** The rows of the "Test as role" list, once the catalog read has landed. */
async function offered(): Promise<string[]> {
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
    const developer = mount("developer", [{ id: "s1", userId: "role-test-1", role: "site-moderator", expiresAt: String(expires) }]);
    // The predefined roles as before, then every active custom role ranked
    // strictly below the operator, an organization role naming its
    // organization. Release captain outranks a developer and the retired role
    // is retired: neither is offered.
    expect(await offered()).toEqual([
      "Test as Viewer",
      "Test as Member",
      "Test as Developer",
      "Test as Support lead",
      "Test as Site moderator (Acme)",
    ]);
    const active = document.querySelector('[aria-label="Active tests"]');
    if (!(active instanceof HTMLElement)) throw new Error("no Active tests list");
    expect(within(active).getByText("Site moderator (Acme)")).toBeTruthy();

    // Starting one asks for that role by slug; the capability check is the
    // server's alone, and its refusal is the error Notice's sentence.
    const tab = { close: vi.fn(), location: { replace: vi.fn() }, postMessage: vi.fn() };
    vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    await click(screen.getByRole("button", { name: /^Test as Site moderator \(Acme\)/ }));
    const starts = developer.connection.dispatcher.sendAndWait.mock.calls
      .map(([message]) => (message as Message).identityAdmin?.roleTest)
      .filter((request) => request?.operation === "start");
    expect(starts).toEqual([{ operation: "start", role: "site-moderator" }]);
    expect(await screen.findByText(/role testing is unavailable for this identity or role/)).toBeTruthy();
    expect(tab.close).toHaveBeenCalled();
    developer.view.unmount();

    const owner = mount("owner");
    expect(await offered()).toEqual([
      "Test as Viewer",
      "Test as Member",
      "Test as Developer",
      "Test as Admin",
      "Test as Release captain",
      "Test as Support lead",
      "Test as Site moderator (Acme)",
    ]);
    owner.view.unmount();
  });
});
