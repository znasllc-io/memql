import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { chooseOption, openSelect } from "../selectControl";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { adminOk, adminRefusal, fakeConnection, userRow, withSession } = await import("./harness");
const { LocalUsersSettingsStore } = await import("../../src/apps/users/settings");
const { countLiveSessions } = await import("../../src/apps/users/useSessions");

type Conn = ReturnType<typeof fakeConnection>;

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalUsersSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn, role = "owner") {
  h.connection = connection;
  return render(
    withSession(
      <UsersApp sectionId="people" navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
      { role },
    ),
  );
}

async function openAda(connection: Conn, role = "owner") {
  const view = mount(connection, role);
  fireEvent.click(await screen.findByRole("button", { name: /Ada/ }));
  await screen.findByRole("region", { name: /Details for/ });
  return view;
}

/**
 * The value of one labelled Fact in the panel.
 *
 * By its <dt> rather than by text, because a role name legitimately appears
 * twice on this surface -- once as the person's current role and once as an
 * option in the role select -- and `getByText("developer")` cannot tell the
 * statement from the choice. Asserting the statement is the point.
 */
function factValue(label: string): string {
  const panel = screen.getByRole("region", { name: /Details for/ });
  const dt = within(panel)
    .getAllByRole("term")
    .find((node) => node.textContent === label);
  if (!dt) throw new Error(`no Fact labelled "${label}"`);
  const dd = dt.nextElementSibling;
  if (!dd) throw new Error(`Fact "${label}" has no value`);
  return (dd.textContent ?? "").trim();
}

/**
 * The identity-admin request a mocked dispatcher was handed, by call index.
 *
 * Typed as `unknown` and narrowed here rather than at each call site: the
 * dispatcher double is a vi.fn with no signature, and a bare
 * `mock.calls[0][0]` is `any` under this project's strict config the moment
 * anything reads a property off it.
 */
function adminRequest(fn: unknown, index = 0): Record<string, unknown> {
  const calls = (fn as { mock: { calls: unknown[][] } }).mock.calls;
  const msg = calls[index]?.[0] as { identityAdmin?: Record<string, unknown> } | undefined;
  if (!msg?.identityAdmin) throw new Error(`no identityAdmin request at call ${index}`);
  return msg.identityAdmin;
}

function ada(over: Record<string, unknown> = {}) {
  return userRow({
    id: "v1:identity:user:ada",
    displayName: "Ada",
    primaryEmail: "ada@example.com",
    role: "reader",
    ...over,
  });
}

describe("the detail panel", () => {
  it("re-reads its person on open rather than trusting the list's copy", async () => {
    // There is no `graph.node.updated.v1:identity:user` broadcast, so the
    // list's copy is only as fresh as the seed. A panel is opened
    // deliberately, once, about one row -- exactly the right place to pay for
    // one authorized read.
    const connection = fakeConnection({
      searchUsers: [ada({ role: "reader" })],
      byId: { "v1:identity:user:ada": ada({ role: "developer" }) },
    });
    const view = await openAda(connection);

    await waitFor(() => expect(factValue("Role")).toBe("developer"));
    expect(connection.query.executeNamed).toHaveBeenCalled();
    view.unmount();
  });

  it("degrades to what it has when the re-read fails, and says so", async () => {
    const connection = fakeConnection({ searchUsers: [ada()] });
    connection.query.executeNamed = vi.fn(async () => {
      throw new Error("conceptRow: refused");
    });
    const view = await openAda(connection);

    expect(await screen.findByText(/values this window already had/i)).toBeTruthy();
    // And the panel still rendered -- a failed re-read must not empty it.
    expect(factValue("Email")).toBe("ada@example.com");
    view.unmount();
  });

  it("shows a sessions count, and '--' when the read does not answer", async () => {
    const connection = fakeConnection({
      searchUsers: [ada()],
      byId: { "v1:identity:user:ada": ada() },
      sessionsForSubjectAdmin: [
        { id: "s1", revokedAt: "", expiresAt: "2099-01-01T00:00:00Z" },
        { id: "s2", revokedAt: "2026-08-02T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" },
      ],
    });
    const view = await openAda(connection);
    expect(await screen.findByText("1 recent")).toBeTruthy();
    // It reads the ADMIN-gated, hash-free query and never authSessionsForSubject.
    expect(connection.query.sessionsForSubjectAdmin).toHaveBeenCalledWith(
      { subject: "v1:identity:user:ada" },
      expect.anything(),
    );
    view.unmount();

    const refusing = fakeConnection({
      searchUsers: [ada()],
      byId: { "v1:identity:user:ada": ada() },
    });
    refusing.query.sessionsForSubjectAdmin = vi.fn(async () => {
      throw new Error("refused");
    });
    const second = await openAda(refusing);
    // Best-effort by contract: "--", and the rest of the panel still renders.
    await waitFor(() => expect(factValue("Recent sessions")).toBe("--"));
    expect(factValue("Email")).toBe("ada@example.com");
    second.unmount();
  });
});

describe("counting live sessions", () => {
  const now = Date.parse("2026-08-31T00:00:00Z");

  it("skips revoked and expired rows and keeps undateable ones", () => {
    expect(
      countLiveSessions(
        [
          { revokedAt: "", expiresAt: "2099-01-01T00:00:00Z" }, // live
          { revokedAt: "2026-08-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" }, // revoked
          { revokedAt: "", expiresAt: "2026-01-01T00:00:00Z" }, // expired
          { revokedAt: "" }, // no expiry -- counted, see the note in the source
        ],
        now,
      ),
    ).toBe(2);
  });
});

describe("admin actions", () => {
  it("changes a role and shows the new value from the accepted write", async () => {
    const connection = fakeConnection({
      searchUsers: [ada({ role: "reader" })],
      byId: { "v1:identity:user:ada": ada({ role: "reader" }) },
    });
    connection.dispatcher.sendAndWait = vi.fn(async () => adminOk());
    const view = await openAda(connection);

    chooseOption(await screen.findByLabelText(/Cluster role for/), "developer");

    await waitFor(() => {
      expect(adminRequest(connection.dispatcher.sendAndWait).setUserRole).toEqual({
        userId: "v1:identity:user:ada",
        role: "developer",
      });
    });
    await waitFor(() => expect(factValue("Role")).toBe("developer"));
    view.unmount();
  });

  it("leaves the old role on screen when the write is refused, and says why", async () => {
    const connection = fakeConnection({
      searchUsers: [ada({ role: "reader" })],
      byId: { "v1:identity:user:ada": ada({ role: "reader" }) },
    });
    connection.dispatcher.sendAndWait = vi.fn(async () =>
      adminRefusal("role_above_inviter: an inviter cannot grant above their own role"),
    );
    const view = await openAda(connection);

    chooseOption(await screen.findByLabelText(/Cluster role for/), "owner");

    // Setting the value optimistically would leave a refused change on screen
    // as though it had happened.
    expect(await screen.findByText(/role_above_inviter/)).toBeTruthy();
    expect(factValue("Role")).toBe("reader");
    view.unmount();
  });

  it("offers the sign-in reset only to somebody actually on passkey_only", async () => {
    const locked = fakeConnection({
      searchUsers: [ada({ signInPolicy: "passkey_only" })],
      byId: { "v1:identity:user:ada": ada({ signInPolicy: "passkey_only" }) },
    });
    locked.dispatcher.sendAndWait = vi.fn(async () => adminOk());
    const first = await openAda(locked);
    const reset = await screen.findByRole("button", { name: "Re-enable sign-in links" });
    fireEvent.click(reset);
    await waitFor(() => {
      expect(adminRequest(locked.dispatcher.sendAndWait).resetSignInPolicy).toEqual({
        userId: "v1:identity:user:ada",
      });
    });
    first.unmount();

    const open = fakeConnection({
      searchUsers: [ada({ signInPolicy: "any" })],
      byId: { "v1:identity:user:ada": ada({ signInPolicy: "any" }) },
    });
    const second = await openAda(open);
    // ONE DIRECTION ONLY. There is deliberately no control that SETS
    // passkey_only for somebody else -- that would lock a colleague out of
    // their own account in a single call.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Re-enable sign-in links" })).toBeNull(),
    );
    expect(screen.getByText(/self-service and requires the person's own active passkey/)).toBeTruthy();
    second.unmount();
  });

  it("shows a minted enrolment link once, with copy, and discards it on close", async () => {
    const connection = fakeConnection({
      searchUsers: [ada()],
      byId: { "v1:identity:user:ada": ada() },
    });
    connection.dispatcher.sendAndWait = vi.fn(async () =>
      adminOk({ enrolmentUrl: "https://memql.example.com/enroll/xyz" }),
    );
    const view = await openAda(connection);

    fireEvent.click(await screen.findByRole("button", { name: "Mint enrolment link" }));
    expect(await screen.findByText("https://memql.example.com/enroll/xyz")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy the enrolment link" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    // It exists nowhere else -- the cluster kept only its hash -- so dismissing
    // it is the end of it.
    await waitFor(() =>
      expect(screen.queryByText("https://memql.example.com/enroll/xyz")).toBeNull(),
    );
    view.unmount();
  });

  it("does not offer an admin the grants their own rank does not carry", async () => {
    const connection = fakeConnection({
      searchUsers: [ada({ role: "reader" })],
      byId: { "v1:identity:user:ada": ada({ role: "reader" }) },
    });
    const view = await openAda(connection, "admin");

    const list = openSelect(await screen.findByLabelText(/Cluster role for/));
    const option = (name: string) => within(list).getByRole("option", { name });

    // PRESENTATION GATING ONLY (spec section E). `adminops.authorize` is the
    // authority, and a forced call still fails server-side -- which is
    // documented rather than tested here, because it is not this window's
    // behaviour.
    expect(option("admin").getAttribute("aria-disabled")).toBeNull();
    expect(option("owner").getAttribute("aria-disabled")).toBe("true");
    view.unmount();
  });

  it("keeps the current role selectable even when it outranks the viewer", async () => {
    // Otherwise the select would show somebody ELSE'S role rather than this
    // person's, which is a worse lie than an option that cannot be chosen.
    const connection = fakeConnection({
      searchUsers: [ada({ role: "owner" })],
      byId: { "v1:identity:user:ada": ada({ role: "owner" }) },
    });
    const view = await openAda(connection, "admin");
    // The CLOSED control shows it, which is the half a person sees: the option
    // is unchoosable and still has to be what the field reads.
    const trigger = await screen.findByLabelText(/Cluster role for/);
    expect(trigger.textContent).toBe("owner");
    expect(within(openSelect(trigger)).getByRole("option", { name: "owner" }).getAttribute("aria-selected")).toBe("true");
    view.unmount();
  });
});
