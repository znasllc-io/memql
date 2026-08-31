import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const { InvitesSection } = await import("../../src/apps/users/InvitesSection");
const { PeopleSection } = await import("../../src/apps/users/PeopleSection");
const { useUsersActions } = await import("../../src/apps/users/actions");
const {
  adminOk,
  adminRefusal,
  fakeConnection,
  invitationRow,
  userRow,
  withSession,
} = await import("./harness");
const { LocalUsersSettingsStore } = await import("../../src/apps/users/settings");

type Conn = ReturnType<typeof fakeConnection>;

const INVITATION_CONCEPT = "v1:identity:invitation";
const USER_CONCEPT = "v1:identity:user";

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
      <UsersApp
        sectionId="invites"
        navigate={() => {}}
        askContext={() => {}}
        store={memoryStore()}
      />,
      { role },
    ),
  );
}

describe("Invites", () => {
  it("lists outstanding invitations with their delivery and expiry", async () => {
    const view = mount(
      fakeConnection({
        pendingUserInvitations: [
          invitationRow({
            id: "v1:identity:invitation:i1",
            inviteeEmail: "colleague@example.com",
            inviteeRole: "writer",
            deliveryState: "sent",
          }),
        ],
      }),
    );

    const row = await screen.findByText("colleague@example.com");
    const line = row.closest(".os-row") as HTMLElement;
    expect(within(line).getByText("writer")).toBeTruthy();
    expect(within(line).getByText("emailed")).toBeTruthy();
    expect(within(line).getByText(/expires/)).toBeTruthy();
    view.unmount();
  });

  it("tells 'no mail is wired' apart from 'the send failed'", async () => {
    // THE THREE STATES (memql#4587). Rendering both of these as "not sent" is
    // what let an invitation look delivered when nothing had been sent at all.
    const view = mount(
      fakeConnection({
        pendingUserInvitations: [
          invitationRow({
            id: "v1:identity:invitation:quiet",
            inviteeEmail: "quiet@example.com",
            deliveryState: "not_attempted",
          }),
          invitationRow({
            id: "v1:identity:invitation:broken",
            inviteeEmail: "broken@example.com",
            deliveryState: "failed",
            deliveryError: "550 mailbox unavailable",
          }),
        ],
      }),
    );

    await screen.findByText("quiet@example.com");
    expect(screen.getByText("not emailed")).toBeTruthy();
    const failed = screen.getByText("email failed");
    expect(failed.getAttribute("title")).toContain("550 mailbox unavailable");
    view.unmount();
  });

  it("issues an invitation and renders a server refusal inline, not as a toast", async () => {
    const connection = fakeConnection({ pendingUserInvitations: [] });
    connection.dispatcher.sendAndWait = vi.fn(async () =>
      adminRefusal("role_above_inviter: an inviter cannot grant above their own role"),
    );
    const view = mount(connection);

    const field = await screen.findByLabelText("Email address to invite");
    fireEvent.change(field, { target: { value: "colleague@example.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Invite" }));

    // The engine's own sentence, beside the control that produced it.
    expect(await screen.findByText(/role_above_inviter/)).toBeTruthy();
    // And the audit id, which is populated on a refusal because a denial is
    // audited too.
    expect(screen.getByText(/Audited as/)).toBeTruthy();
    view.unmount();
  });

  it("shows the link exactly when the cluster could not mail it", async () => {
    const connection = fakeConnection({ pendingUserInvitations: [] });
    connection.dispatcher.sendAndWait = vi.fn(async () =>
      adminOk({
        invitationUrl: "https://memql.example.com/invite/abc",
        registrationMode: "invite_only",
        invitationEmailSent: false,
        invitationEmailError: "",
      }),
    );
    const view = mount(connection);

    fireEvent.change(await screen.findByLabelText("Email address to invite"), {
      target: { value: "colleague@example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Invite" }));

    // No mail wired: the link is the ONLY delivery mechanism, so withholding
    // it would leave an invitation nobody can act on.
    expect(await screen.findByText("https://memql.example.com/invite/abc")).toBeTruthy();
    expect(screen.getByText(/No mail is configured/)).toBeTruthy();
    view.unmount();
  });

  it("does NOT show the link when the invitation was emailed", async () => {
    const connection = fakeConnection({ pendingUserInvitations: [] });
    connection.dispatcher.sendAndWait = vi.fn(async () =>
      adminOk({
        invitationUrl: "https://memql.example.com/invite/secret",
        registrationMode: "invite_only",
        invitationEmailSent: true,
      }),
    );
    const view = mount(connection);

    fireEvent.change(await screen.findByLabelText("Email address to invite"), {
      target: { value: "colleague@example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Invite" }));

    expect(await screen.findByText(/Invitation emailed to/)).toBeTruthy();
    // The URL is a credential. It is on its way to the recipient and has no
    // reason to be on an operator's screen.
    expect(screen.queryByText("https://memql.example.com/invite/secret")).toBeNull();
    view.unmount();
  });

  it("re-sends by issuing a fresh invitation and revoking the stale row", async () => {
    const sent: unknown[] = [];
    const connection = fakeConnection({
      pendingUserInvitations: [
        invitationRow({ id: "v1:identity:invitation:i1", inviteeEmail: "colleague@example.com" }),
      ],
    });
    connection.dispatcher.sendAndWait = vi.fn(async (msg: unknown) => {
      sent.push(msg);
      return adminOk({
        invitationUrl: "https://memql.example.com/invite/fresh",
        invitationEmailSent: true,
      });
    });
    const view = mount(connection);

    fireEvent.click(await screen.findByRole("button", { name: /Re-send the invitation/ }));

    // There is NO dedicated resend op on the oneof, so a resend is
    // issue-then-revoke -- and in that order, because revoking first and then
    // failing to issue would leave the person with nothing.
    await waitFor(() => expect(sent.length).toBe(2));
    const first = sent[0] as { identityAdmin?: Record<string, unknown> };
    const second = sent[1] as { identityAdmin?: Record<string, unknown> };
    expect(first.identityAdmin?.issueUserInvitation).toBeTruthy();
    expect(second.identityAdmin?.revokeUserInvitation).toEqual({
      invitationId: "v1:identity:invitation:i1",
    });
    view.unmount();
  });

  it("cancels, and the row leaves on the update event rather than on a refetch", async () => {
    const connection = fakeConnection({
      pendingUserInvitations: [
        invitationRow({ id: "v1:identity:invitation:i1", inviteeEmail: "colleague@example.com" }),
      ],
    });
    connection.dispatcher.sendAndWait = vi.fn(async () => adminOk());
    const view = mount(connection);

    fireEvent.click(await screen.findByRole("button", { name: /Cancel the invitation/ }));

    // The write does not remove the row. The `updated` broadcast does, because
    // the revoked row stops satisfying the read's own membership predicate --
    // which `inScope` says again about arriving events.
    act(() => {
      connection.subscriptions.emit(
        INVITATION_CONCEPT,
        invitationRow({
          id: "v1:identity:invitation:i1",
          inviteeEmail: "colleague@example.com",
          active: false,
        }),
      );
    });

    await waitFor(() => expect(screen.queryByText("colleague@example.com")).toBeNull());
    expect(connection.query.pendingUserInvitations).toHaveBeenCalledTimes(1);
    view.unmount();
  });
});

// ---------------------------------------------------------------------------
// The exemplar
// ---------------------------------------------------------------------------

/** Both sections on ONE connection, which is the whole point of the test. */
function BothLists() {
  const actions = useUsersActions();
  return (
    <>
      <PeopleSection showDeactivated={false} actions={actions} ownerRole="owner" />
      <InvitesSection actions={actions} ownerRole="owner" />
    </>
  );
}

describe("someone accepts while I am watching", () => {
  it("drops the invitation and gains the person, on one connection, with no refetch", async () => {
    const connection = fakeConnection({
      searchUsers: [userRow({ id: "v1:identity:user:me", displayName: "Owner" })],
      pendingUserInvitations: [
        invitationRow({ id: "v1:identity:invitation:i1", inviteeEmail: "grace@example.com" }),
      ],
    });
    h.connection = connection;
    const view = render(withSession(<BothLists />));

    // Scoped to each list by its own aria-label. Unscoped queries cannot work
    // here: the acceptance puts the SAME address on the people row, so a bare
    // "is grace@example.com gone" would fail on the evidence that it worked.
    const invites = () => screen.getByRole("list", { name: "Outstanding invitations" });
    const people = () => screen.getByRole("list", { name: "People in this cluster" });

    await waitFor(() => expect(within(invites()).getByText("grace@example.com")).toBeTruthy());
    await waitFor(() => expect(within(people()).getByText(/Owner/)).toBeTruthy());

    // ONE acceptance, TWO broadcasts. The invitation's `updated` rule takes
    // the row off the invites list; the user's `created` rule puts a row on
    // the people list with the arrival cue.
    act(() => {
      connection.subscriptions.emit(
        INVITATION_CONCEPT,
        invitationRow({
          id: "v1:identity:invitation:i1",
          inviteeEmail: "grace@example.com",
          status: "accepted",
          respondedAt: new Date().toISOString(),
        }),
      );
      connection.subscriptions.emit(
        USER_CONCEPT,
        userRow({
          id: "v1:identity:user:grace",
          displayName: "Grace",
          primaryEmail: "grace@example.com",
        }),
        "NODE_CREATED",
      );
    });

    // One row shorter here...
    await waitFor(() =>
      expect(within(invites()).queryByText("grace@example.com")).toBeNull(),
    );
    // ...and one row longer there, with the cue shown.
    const person = within(people()).getByRole("button", { name: /Grace/ });
    expect(person.closest("li")?.getAttribute("data-arrival")).toBe("added");
    expect(within(person).getByText("new")).toBeTruthy();

    // NEITHER list refetched.
    expect(connection.query.searchUsers).toHaveBeenCalledTimes(1);
    expect(connection.query.pendingUserInvitations).toHaveBeenCalledTimes(1);
    view.unmount();
  });
});
