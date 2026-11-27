import { accountAccessFixture } from "../accountAccessFixture";
import { rowsResult } from "./harness";
import { chooseOption } from "../selectControl";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, opened: [] as unknown[] }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// THE SHELL, replaced by a spy on the one act this band performs. The band's
// whole handoff is `openApp("users", "groups", { groupId })`, and the assertion
// worth making is that it dispatches THAT -- not that a window appeared.
vi.mock("../../src/chrome/state", async () => {
  const real = await vi.importActual<typeof import("../../src/chrome/state")>(
    "../../src/chrome/state",
  );
  return {
    ...real,
    useOsIfPresent: () => ({
      actions: {
        openApp: (appId: string, sectionId: string, payload?: Record<string, unknown>) =>
          h.opened.push({ appId, sectionId, payload }),
      },
    }),
  };
});

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { accountRow, fakeConnection, openAccountTab, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalAccountsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

async function openDetail(connection: Conn): Promise<HTMLElement> {
  h.connection = connection;
  h.opened = [];
  render(
    withSession(
      <AccountsApp sectionId="accounts" navigate={vi.fn()} askContext={() => {}} store={memoryStore()} />,
    ),
  );
  const row = await screen.findByText("Acme Consulting");
  await act(async () => {
    fireEvent.click(row.closest("button") as HTMLElement);
  });
  return openAccountTab("People");
}

const ada = { id: "ada", name: "Ada", email: "ada@example.test", role: "viewer", apps: [] };
const grace = { id: "grace", name: "Grace", email: "grace@example.test", role: "viewer", apps: [] };
function connectionFor(over: Parameters<typeof accountAccessFixture>[0] = {}) {
  return fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountAccess: accountAccessFixture(over) as never });
}
async function pick(label: string, name: string) {
  fireEvent.change(screen.getByRole("combobox", { name: label }), { target: { value: name } });
  fireEvent.click(await screen.findByRole("option", { name: new RegExp(name) }));
}

describe("account people management", () => {
  it("shows each member once with one owner and plain role labels", async () => {
    const state = accountAccessFixture();
    await openDetail(connectionFor({ people: [...state.people, ada] }));
    const members = await screen.findByRole("list", { name: "Account members" });
    expect(within(members).getAllByRole("listitem")).toHaveLength(2);
    expect(within(members).getByText("Owner")).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: "Role for Account owner" })).toBeNull();
    expect(screen.getByRole("combobox", { name: "Role for Ada" }).textContent).toBe("Viewer");
  });
  it("keeps a batch selected when saving fails, then submits it atomically", async () => {
    const state = accountAccessFixture({ candidates: [ada, grace] });
    const conn = connectionFor(state);
    const read = conn.query.executeNamed.getMockImplementation()!;
    let fail = true;
    conn.query.executeNamed.mockImplementation(async (name, call) => {
      if (name !== "accountAccessUpdate") return read(name, call);
      if (fail) throw new Error("Access changed; refresh before retrying");
      return rowsResult([accountAccessFixture({ people: [...state.people, ada, grace], revision: "revision-two" }) as never]);
    });
    await openDetail(conn);
    await screen.findByText("Account owner");
    await pick("People", "Ada"); await pick("People", "Grace");
    chooseOption(screen.getByRole("combobox", { name: "Role for selected people" }), "Member");
    fireEvent.click(screen.getByRole("button", { name: "Add selected people" }));
    expect(await screen.findByText("Access changed; refresh before retrying")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add selected people" }).hasAttribute("disabled")).toBe(false);
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Add selected people" }));
    await waitFor(() => expect(screen.getByRole("list", { name: "Account members" }).textContent).toContain("Grace"));
    const writes = conn.query.executeNamed.mock.calls.filter(([name]) => name === "accountAccessUpdate");
    expect(writes).toHaveLength(2);
    expect(writes[1]?.[1]).toContain('userIds: ["ada", "grace"]');
    expect(writes[1]?.[1]).toContain('role: "member"');
    expect(writes[1]?.[1]).toContain('revision: "revision-one"');
  });
  it("requires selecting a successor before the explicit transfer action", async () => {
    const state = accountAccessFixture();
    const conn = connectionFor({ people: [...state.people, ada] });
    await openDetail(conn); await screen.findByText("Account owner");
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership" }));
    expect(screen.getByRole("button", { name: "Transfer ownership" }).hasAttribute("disabled")).toBe(true);
    await pick("New owner", "Ada");
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership" }));
    await waitFor(() => expect(conn.query.executeNamed.mock.calls.some(([name, call]) => name === "accountAccessUpdate" && call.includes('operation: "transfer"') && call.includes('userId: "ada"'))).toBe(true));
  });
  it("offers Edit apps to a staff developer the engine lets assign them", async () => {
    await openDetail(connectionFor({ actorRole: "developer", clusterOwner: false, assignsApps: true }));
    expect(await screen.findByRole("button", { name: "Edit apps" })).toBeTruthy();
  });
  it("hides Edit apps from a client's own owner, who may not assign them", async () => {
    await openDetail(connectionFor({ actorRole: "owner", clusterOwner: false, assignsApps: false }));
    await screen.findByRole("list", { name: "Account members" });
    expect(screen.queryByRole("button", { name: "Edit apps" })).toBeNull();
  });
  it("opens a custom group in Users", async () => {
    await openDetail(connectionFor({ groups: [{ id: "reviewers", name: "Reviewers", kind: "custom", apps: [], userIds: [] }] }));
    fireEvent.click(await screen.findByRole("button", { name: "Reviewers" }));
    expect(h.opened).toEqual([{ appId: "users", sectionId: "groups", payload: { groupId: "reviewers", accountId: "a1" } }]);
  });
  it("keeps a refusal distinct from an empty roster and allows retry", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountAccess: new Error("This account requires a manager") });
    await openDetail(conn);
    expect(await screen.findByText("This account requires a manager")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Account members" })).toBeNull();
    conn.query.executeNamed.mockResolvedValue(rowsResult([accountAccessFixture() as never]));
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Account owner")).toBeTruthy();
  });
  it("retains the invitation link when email delivery fails", async () => {
    const conn = connectionFor();
    conn.dispatcher.sendAndWait.mockResolvedValue({ identityAdminResult: { ok: true, errorCode: 0, invitationUrl: "https://identity.example.test/invitation?code=example", invitationEmailSent: false, invitationEmailError: "Mail is unavailable" } });
    await openDetail(conn); await screen.findByText("Account owner");
    fireEvent.change(screen.getByLabelText("Invite by email"), { target: { value: "new@example.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Invite" }));
    expect(await screen.findByText("Mail is unavailable")).toBeTruthy();
    expect(screen.getByText("https://identity.example.test/invitation?code=example")).toBeTruthy();
    expect(conn.dispatcher.sendAndWait.mock.calls[0]?.[0]).toMatchObject({ identityAdmin: { issueUserInvitation: { role: "viewer", groupIds: ["acct-a1"] } } });
  });
});
