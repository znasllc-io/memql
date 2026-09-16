import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { UsersApp } = await import("../../src/apps/users/UsersApp");
const {
  accountRow,
  adminRefusal,
  fakeConnection,
  grantRow,
  groupRow,
  roleRow,
  withSession,
} = await import("./harness");
const { click, memoryStore, renderApp } = await import("./mount");
const { domainMatch } = await import("../../src/apps/users/invite");
const { accountFromRow } = await import("../../src/apps/accounts/rows");

type Conn = ReturnType<typeof fakeConnection>;

const LADDER = [
  roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
  roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
  roleRow({ slug: "admin", name: "Admin", rank: 200 }),
  roleRow({ slug: "owner", name: "Owner", rank: 400 }),
];

const GRANTS = [
  grantRow("owner", "read", "principal"),
  grantRow("owner", "create", "principal"),
  grantRow("owner", "update", "principal"),
  grantRow("owner", "create", "admission"),
];

const ACME = accountRow({
  id: "acct-acme",
  name: "Acme",
  domain: "acme.com",
  domainStatus: "verified",
  domainVerifiedAt: "2026-09-01T00:00:00Z",
  joinOnDomain: true,
});

function seed(extra: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    activeRoles: LADDER,
    activeCapabilities: GRANTS,
    clientAccountsAll: [ACME],
    groupsAll: [groupRow({ id: "g-acme", name: "Acme", kind: "account", accountId: "acct-acme" })],
    ...extra,
  });
}

async function openInvite(connection: Conn, role = "owner") {
  h.connection = connection;
  const view = renderApp(
    withSession(
      <UsersApp sectionId="people" navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
      { role },
    ),
  );
  await click(await screen.findByRole("button", { name: "Invite" }));
  await screen.findByRole("heading", { name: "Invite somebody" });
  return view;
}

async function type(label: string, value: string): Promise<void> {
  const { act, fireEvent } = await import("@testing-library/react");
  const field = screen.getByLabelText(label);
  await act(async () => {
    fireEvent.change(field, { target: { value } });
  });
}

// ===========================================================================
// THE DOMAIN MATCH
// ===========================================================================
// BOTH CONDITIONS, and neither is decoration: an UNVERIFIED domain proves
// nothing (the engine refuses `joinOnDomain` on one), and joining switched off
// means the client has asked for arrivals NOT to be placed.

describe("matching an address to a client", () => {
  const accounts = [
    accountFromRow(ACME),
    accountFromRow(
      accountRow({ id: "acct-b", name: "Beta", domain: "beta.com", domainStatus: "verifying", joinOnDomain: true }),
    ),
    accountFromRow(
      accountRow({ id: "acct-c", name: "Gamma", domain: "gamma.com", domainStatus: "verified", joinOnDomain: false }),
    ),
  ];

  it("matches a verified domain that takes people on it", () => {
    expect(domainMatch("kit@acme.com", accounts)?.id).toBe("acct-acme");
  });

  it("does not match an unproven domain", () => {
    expect(domainMatch("kit@beta.com", accounts)).toBeNull();
  });

  it("does not match a client who has joining switched off", () => {
    expect(domainMatch("kit@gamma.com", accounts)).toBeNull();
  });
});

// ===========================================================================
// THE RAIL
// ===========================================================================

describe("the Invite rail", () => {
  it("answers the Groups stop from the address's domain, and says so", async () => {
    const view = await openInvite(seed());
    await type("Email address", "kit@acme.com");

    expect(await screen.findByText(/Acme's domain, and they take people on it/)).toBeTruthy();
    // The stop's own answer line names the group that was filled in.
    await waitFor(() => expect(screen.getByText(/Acme is filled in below/)).toBeTruthy());
    view.unmount();
  });

  it("keeps the Head's action absent until the stops are answered", async () => {
    const view = await openInvite(seed());
    const bar = () => screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar()).queryByRole("button", { name: "Send invitation" })).toBeNull();

    await type("Email address", "kit@example.com");
    expect(within(bar()).queryByRole("button", { name: "Send invitation" })).toBeNull();

    const ladder = screen.getByRole("list", { name: /The role this invitation grants/ });
    await click(within(ladder).getByRole("button", { name: /Member/ }));

    await waitFor(() =>
      expect(within(bar()).getByRole("button", { name: "Send invitation" })).toBeTruthy(),
    );
    view.unmount();
  });

  it("puts group_ids on the wire", async () => {
    const connection = seed();
    connection.dispatcher.sendAndWait.mockResolvedValue({
      identityAdminResult: {
        ok: true,
        errorCode: 0,
        auditEventId: "v1:identity:auditEvent:a1",
        invitationUrl: "https://identity.example.com/invite/abc",
        emailSent: true,
      },
    });
    const view = await openInvite(connection);
    await type("Email address", "kit@acme.com");
    const ladder = screen.getByRole("list", { name: /The role this invitation grants/ });
    await click(within(ladder).getByRole("button", { name: /Member/ }));
    await click(screen.getByRole("button", { name: "Send invitation" }));

    await waitFor(() =>
      expect(connection.dispatcher.sendAndWait).toHaveBeenCalledWith(
        expect.objectContaining({
          identityAdmin: expect.objectContaining({
            issueUserInvitation: expect.objectContaining({
              email: "kit@acme.com",
              role: "user",
              groupIds: ["g-acme"],
            }),
          }),
        }),
        undefined,
      ),
    );

    // The rail BECOMES the invited person's page, with the link shown once.
    expect(await screen.findByText(/shown once/)).toBeTruthy();
    view.unmount();
  });

  it("lands a refusal at the stop that owns the value", async () => {
    const connection = seed();
    connection.dispatcher.sendAndWait.mockResolvedValue(
      adminRefusal("role_above_inviter: an inviter cannot grant above their own role"),
    );
    const view = await openInvite(connection);
    await type("Email address", "kit@example.com");
    const ladder = screen.getByRole("list", { name: /The role this invitation grants/ });
    await click(within(ladder).getByRole("button", { name: /Member/ }));
    await click(screen.getByRole("button", { name: "Send invitation" }));

    // The Role stop is where the refused value was entered; a refusal at the
    // bottom of a three-stop rail makes somebody re-read all three.
    const roleStop = await screen.findByText(/an inviter cannot grant above their own role/);
    expect(roleStop.closest("li")?.textContent).toContain("Role");
    view.unmount();
  });

  it("renders the email gate's sentence where Invite would be", async () => {
    // An app that hides Invite with no account of itself reads as a missing
    // feature, and somebody looking for it has nowhere to find out why.
    h.connection = seed();
    const view = renderApp(
      withSession(
        <UsersApp sectionId="people" navigate={() => {}} askContext={() => {}} store={memoryStore()} />,
        { role: "owner", emailReady: false },
      ),
    );
    await screen.findByText("People");
    expect(screen.queryByRole("button", { name: "Invite" })).toBeNull();
    expect(screen.getByText(/needs a mailbox/)).toBeTruthy();
    view.unmount();
  });
});

// CAN A DEVELOPER ACTUALLY CLICK ADMIN? (memql#5236)
//
// The engine learned that a developer may invite an admin, and this app kept
// refusing it: rule 5 in assign.ts ran on both seams, pickers.tsx feeds
// `rungRefusal` straight to `disabled`, so the rung was drawn dashed and could
// not be clicked. The fix was live and unreachable, and nothing failed.
//
// assign.test.ts pins the RULE. This pins the SURFACE -- the real rail, the
// real ladder, the real picker, rendered under jsdom through this suite's own
// harness. It is the check that would have caught the shipped bug, and the one
// that was skipped: the browser could not reach it (CoreGate gates the desktop
// on inference, which a local k3d cluster cannot configure) and that was
// mistaken for the check being unavailable. It was available here all along.
describe("the rungs a developer is offered", () => {
  // developer holds create-on-admission and only READ on principal; admin
  // holds all four. That pair is the whole subject: rank says developer is
  // above admin, the grants say neither contains the other.
  const RUNGS = [
    roleRow({ slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] }),
    roleRow({ slug: "user", name: "Member", rank: 100, aliases: ["writer"] }),
    roleRow({ slug: "admin", name: "Admin", rank: 200 }),
    roleRow({ slug: "developer", name: "Developer", rank: 300 }),
    roleRow({ slug: "owner", name: "Owner", rank: 400 }),
  ];
  const RUNG_GRANTS = [
    grantRow("owner", "read", "principal"),
    grantRow("owner", "create", "principal"),
    grantRow("owner", "update", "principal"),
    grantRow("owner", "delete", "principal"),
    grantRow("owner", "create", "admission"),
    grantRow("admin", "read", "principal"),
    grantRow("admin", "create", "principal"),
    grantRow("admin", "update", "principal"),
    grantRow("admin", "delete", "principal"),
    grantRow("admin", "create", "admission"),
    grantRow("developer", "read", "principal"),
    grantRow("developer", "create", "admission"),
  ];

  function rungSeed() {
    return fakeConnection({ activeRoles: RUNGS, activeCapabilities: RUNG_GRANTS });
  }

  // BY SLUG, NOT BY ACCESSIBLE NAME. Each rung carries a RankMark whose
  // aria-label names the ACTOR's role as well as the rung's
  // (clients/os/src/kit/RankMark.tsx), so `getByRole("button", {name:/Admin/})`
  // matches every rung on the ladder when an admin is looking at it. The slug
  // span is the one text unique to its own rung.
  function rung(slug: string): HTMLButtonElement {
    const ladder = screen.getByRole("list", { name: /The role this invitation grants/ });
    for (const line of Array.from(ladder.querySelectorAll("li.os-role-rung"))) {
      const badge = line.querySelector(".os-role-slug");
      if ((badge?.textContent ?? "").trim() === slug) {
        const button = line.querySelector("button");
        if (button === null) throw new Error(`rung ${slug} has no button`);
        return button as HTMLButtonElement;
      }
    }
    throw new Error(`no rung with slug ${slug} on the ladder`);
  }

  it("offers admin to a developer, and the button is clickable", async () => {
    await openInvite(rungSeed(), "developer");
    const admin = rung("admin");
    expect(admin.disabled, "the admin rung must be clickable for a developer").toBe(false);
    expect(admin.title ?? "", "an offered rung carries no refusal sentence").toBe("");
  });

  it("still refuses owner to a developer -- the rank cap is untouched", async () => {
    await openInvite(rungSeed(), "developer");
    const owner = rung("owner");
    expect(owner.disabled).toBe(true);
    expect(owner.title).toContain("at or above your own");
  });

  it("still refuses a peer admin to an admin", async () => {
    await openInvite(rungSeed(), "admin");
    expect(rung("admin").disabled).toBe(true);
  });

  it("a developer picking admin can send the invitation", async () => {
    const connection = rungSeed();
    await openInvite(connection, "developer");
    await type("Email address", "colleague@example.test");
    await click(rung("admin"));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Send invitation" })).toBeTruthy(),
    );
  });
});
