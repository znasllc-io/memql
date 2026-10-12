import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { installSeededAccess } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const { fakeConnection, rowsResult, withSession } = await import("./harness");

// THE EMAIL APP ON ITS TWO GATED SECTIONS (memql#6033).
//
// Inbox and Logs are the app's two sections with a resource of their own, above
// the app's door: `app:email/inbox` for the owner and developer (what it holds
// is sign-in links for any test recipient, which is not an organization's to
// read) and `app:email/logs` from admin up (the log store's floor). The app
// root mounts one pane per visited section and re-checks each pane's resource
// on every render, so the wiring to pin is: the pane for a section a person may
// open is the section, and the pane for one they may not is nothing at all --
// not an empty pane, and not a read that is then refused.

afterEach(cleanup);

const MESSAGE = {
  id: "m1",
  createdAt: "2026-10-01T12:00:00Z",
  from: "news@client.test",
  fromName: "Client",
  To: "person@example.test",
  Subject: "Verify your email",
  TextBody: "Verify",
  HTMLBody: "",
};

/** The reads the two sections make, which the Email harness does not answer. */
function connection() {
  const conn = fakeConnection({});
  const emailInbox = vi.fn(async () => ({ rows: () => [{ mode: "capture", messages: [MESSAGE] }] }));
  const logsTail = vi.fn(async (_args: Record<string, unknown>, _opts?: unknown) => rowsResult([]));
  Object.assign(conn.query, { emailInbox, logsTail });
  return { conn, emailInbox, logsTail };
}

function mount(conn: ReturnType<typeof fakeConnection>, sectionId: string, role: string) {
  installSeededAccess(role);
  h.connection = conn;
  const store = new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} });
  return render(
    withSession(
      <EmailApp sectionId={sectionId} navigate={vi.fn()} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />,
      { role },
    ),
  );
}

const pane = (section: string) => document.querySelector<HTMLElement>(`[data-email-section="${section}"]`);

describe("the Inbox section", () => {
  it.each(["owner", "developer"])("is the capture inbox for the %s role, and it reads the captured mail", async (role) => {
    const { conn, emailInbox } = connection();
    mount(conn, "inbox", role);

    const inbox = await waitFor(() => {
      const found = pane("inbox");
      if (found === null) throw new Error("no inbox pane");
      return found;
    });
    expect(inbox.hidden).toBe(false);
    expect(await within(inbox).findByRole("button", { name: /Verify your email/ })).toBeTruthy();
    expect(within(inbox).getByRole("heading", { name: "Inbox" })).toBeTruthy();
    expect(emailInbox).toHaveBeenCalled();
  });

  it.each(["admin", "writer"])("is nothing at all for the %s role, which may not open it: no pane and no read", async (role) => {
    const { conn, emailInbox } = connection();
    mount(conn, "inbox", role);

    // Let the root's feeds settle, so an absent read is not just an early look.
    await waitFor(() => expect(conn.query.campaigns).toHaveBeenCalled());
    expect(pane("inbox")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Inbox" })).toBeNull();
    expect(emailInbox).not.toHaveBeenCalled();
  });
});

describe("the Logs section", () => {
  it.each(["owner", "developer", "admin"])("is the app's slice of the log store for the %s role", async (role) => {
    const { conn, logsTail } = connection();
    mount(conn, "logs", role);

    const logs = await waitFor(() => {
      const found = pane("logs");
      if (found === null) throw new Error("no logs pane");
      return found;
    });
    expect(logs.hidden).toBe(false);
    expect(within(logs).getByRole("heading", { name: "Logs" })).toBeTruthy();

    // The app's id, and every concept a send is made of -- the generated
    // constants, plus the messages a capture cluster keeps instead of delivering.
    await waitFor(() => expect(logsTail).toHaveBeenCalled());
    const [args] = logsTail.mock.calls[0] as [Record<string, unknown>];
    expect(args["apps"]).toEqual(["email"]);
    expect([...(args["subjectConcepts"] as string[])].sort()).toEqual(
      [
        Concepts.CAMPAIGNS_CAMPAIGN,
        Concepts.CAMPAIGNS_AUDIENCE,
        Concepts.CAMPAIGNS_TEMPLATE,
        Concepts.CAMPAIGNS_SENDER_IDENTITY,
        Concepts.CAMPAIGNS_EMAIL_RULE,
        Concepts.CAMPAIGNS_SEND_JOB,
        Concepts.CAMPAIGNS_DELIVERY,
        Concepts.EMAIL_CAPTURED_MESSAGE,
      ].sort(),
    );
  });

  it("is nothing at all for a writer, who is below the log store's floor: no pane and no read", async () => {
    const { conn, logsTail } = connection();
    mount(conn, "logs", "writer");

    await waitFor(() => expect(conn.query.campaigns).toHaveBeenCalled());
    expect(pane("logs")).toBeNull();
    expect(logsTail).not.toHaveBeenCalled();
  });

  it("opens beside the Inbox without either taking the other's resource: an admin has Logs and not Inbox", async () => {
    const { conn, logsTail, emailInbox } = connection();
    const view = mount(conn, "logs", "admin");
    await waitFor(() => expect(logsTail).toHaveBeenCalled());

    // The window moves to the Inbox: the admin's pane for it is still nothing.
    view.rerender(
      withSession(
        <EmailApp sectionId="inbox" navigate={vi.fn()} askContext={() => {}} store={new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} })} uploads={{ upload: vi.fn() }} />,
        { role: "admin" },
      ),
    );
    expect(pane("inbox")).toBeNull();
    expect(emailInbox).not.toHaveBeenCalled();
    // And the Logs pane the admin already opened stays, parked.
    expect(pane("logs")?.hidden).toBe(true);
  });
});
