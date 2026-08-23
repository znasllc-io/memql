// The profile surface: /me, /me/sessions, /me/security (memql#4318, #4319).
//
// Two fake boundaries, and the split is the same one the app has. The READS
// are named queries, so they are stubbed at executeNamed -- which means the
// real generated builders run above the stub and a test exercises the call
// string it asserts on. The WRITES go through the SDK's identity wrappers, so
// they are stubbed at the dispatcher, where the envelope is visible: that is
// how a test can assert the portal sends `revokeSession` rather than
// composing a mutation of its own.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const AUTH_ENABLED_CLUSTER = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
  domain: "example.com",
};

const CURRENT_SESSION = "v1:identity:authSession:this-one";

function userRow(overrides: Record<string, unknown> = {}) {
  return {
    id: "v1:identity:user:u-42",
    displayName: "Ada Lovelace",
    primaryEmail: "ada@example.test",
    role: "owner",
    createdAt: "2026-01-04T10:00:00Z",
    lastSeenAt: "2026-08-22T09:30:00Z",
    sharedMailbox: false,
    signInPolicy: "any",
    ...overrides,
  };
}

function passkeyRow(overrides: Record<string, unknown> = {}) {
  return {
    id: "v1:identity:identity:pk-1",
    label: "MacBook",
    active: true,
    backupState: true,
    createdAt: "2026-03-02T08:00:00Z",
    ...overrides,
  };
}

function sessionRow(overrides: Record<string, unknown> = {}) {
  return {
    id: "v1:identity:authSession:other",
    source: "oidc_cookie",
    clientLabel: "Mozilla/5.0 (X11; Linux x86_64)",
    firstAuthenticatedAt: "2026-08-20T12:00:00Z",
    lastActivityAt: "2026-08-22T08:00:00Z",
    createdAt: "2026-08-20T12:00:00Z",
    ...overrides,
  };
}

// result wraps rows in the shape QueryClient.executeNamed resolves to.
function result(rows: Array<Record<string, unknown>>) {
  return {
    rows: () => rows,
    rawNodes: () => rows,
    single: () => rows[0] ?? null,
    meta: () => null,
  };
}

interface Fixture {
  user?: Record<string, unknown>;
  passkeys?: Array<Record<string, unknown>>;
  sessions?: Array<Record<string, unknown>>;
  sessionId?: string;
  // Throws instead of resolving, for the "a read failure is not an empty
  // list" cases. failPasskeys narrows that to the passkey read alone.
  failReads?: boolean;
  failPasskeys?: boolean;
  replies?: Record<string, unknown>;
  sent?: Array<Record<string, unknown>>;
}

function fakeConnection(fx: Fixture): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "v1:identity:user:u-42",
      primaryEmail: "ada@example.test",
      clusterRole: "owner",
      displayName: "Ada Lovelace",
      sessionId: fx.sessionId ?? CURRENT_SESSION,
    })),
    executeNamed: vi.fn(async (name: string) => {
      if (fx.failReads) throw new Error("stream refused the read");
      if (name === "currentUser") return result([fx.user ?? userRow()]);
      if (name === "passkeysForSelf") {
        if (fx.failPasskeys) throw new Error("stream refused the passkey read");
        return result(fx.passkeys ?? [passkeyRow()]);
      }
      if (name === "authSessionsForSelf") return result(fx.sessions ?? []);
      return result([]);
    }),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    engineVersion: "v0.19.5",
    query,
    dispatcher: {
      send: vi.fn(),
      addEventListener: vi.fn(() => () => {}),
      registerStream: vi.fn(() => () => {}),
      sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
        fx.sent?.push(msg);
        const key = Object.keys(msg)[0] ?? "";
        return { correlateTo: "x", ...(fx.replies?.[key] ?? {}) };
      }),
    },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderMe(path: string, fx: Fixture = {}) {
  const dial = vi.fn(async () => fakeConnection(fx)) as unknown as typeof Connection.dial;
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_ENABLED_CLUSTER}
        fetchImpl={async () =>
          ({
            ok: true,
            status: 200,
            json: async () => ({ access_token: "AT-1", expires_in: 900 }),
          }) as unknown as Response
        }
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

function tabs(): HTMLElement {
  return screen.getByRole("navigation", { name: "Your account" });
}

describe("/me -- the page frame (memql#4318)", () => {
  it("renders the three tabs, each a real address", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Ada Lovelace" })).toBeTruthy());

    for (const [label, href] of [
      ["Account", "/me"],
      ["Sessions", "/me/sessions"],
      ["Security", "/me/security"],
    ]) {
      expect(within(tabs()).getByRole("link", { name: label }).getAttribute("href")).toBe(href);
    }
  });

  it("deep-links straight into a facet", async () => {
    renderMe("/me/security");
    // The Security tab's own band, not the Account facts.
    await waitFor(() => expect(screen.getByText("Sign-in links")).toBeTruthy());
    expect(screen.queryByText("Member since")).toBeNull();
  });

  it("shows the email, the role and the join date under the name", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Ada Lovelace" })).toBeTruthy());
    expect(screen.getAllByText("ada@example.test").length).toBeGreaterThan(0);
    expect(screen.getAllByText("owner").length).toBeGreaterThan(0);
    expect(screen.getByText(/member since/)).toBeTruthy();
  });
});

describe("/me -- Account (memql#4318, #4319)", () => {
  it("lists the account facts", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByText("Display name")).toBeTruthy());
    for (const label of ["Display name", "Email", "Cluster role", "Member since", "Last seen"]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
  });

  it("links out to identity at the CONFIGURED origin, never a hardcoded host", async () => {
    renderMe("/me");
    const link = await waitFor(() => screen.getByRole("link", { name: /Edit on identity/ }));
    expect(link.getAttribute("href")).toBe("https://identity.example.com/me/settings");
  });

  it("shows the shared-mailbox note only when the account is flagged", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByText("Display name")).toBeTruthy());
    expect(screen.queryByText(/looks like a shared mailbox/)).toBeNull();
  });

  it("points a flagged account at the remedy, not just at the risk", async () => {
    renderMe("/me", { user: userRow({ sharedMailbox: true }) });
    await waitFor(() => expect(screen.getByText(/looks like a shared mailbox/)).toBeTruthy());
    // The note carries a link to the tab where sign-in links are turned off.
    const link = screen.getByRole("link", { name: "Security tab" });
    expect(link.getAttribute("href")).toBe("/me/security");
  });

  it("says the read failed rather than rendering an empty account", async () => {
    renderMe("/me", { failReads: true });
    await waitFor(() => expect(screen.getByText(/could not read your account/)).toBeTruthy());
    expect(screen.queryByText("Display name")).toBeNull();
  });
});

describe("/me/sessions (memql#4319)", () => {
  it("marks THIS device from session_id, not from the user agent", async () => {
    renderMe("/me/sessions", {
      sessions: [
        sessionRow(),
        sessionRow({ id: CURRENT_SESSION, clientLabel: "Mozilla/5.0 (X11; Linux x86_64)" }),
      ],
    });
    await waitFor(() => expect(screen.getByText("This device")).toBeTruthy());
    // Both rows share a user agent -- which is the point: two tabs of one
    // browser do too, so a label derived from it would mark the wrong row and
    // invite somebody to revoke the session they are sitting in.
    expect(screen.getAllByText("This device")).toHaveLength(1);
    const marked = screen.getByText("This device").closest("tr");
    expect(marked?.textContent).toContain("Linux");
  });

  it("marks nothing when the caller's credential carries no session", async () => {
    renderMe("/me/sessions", { sessionId: "", sessions: [sessionRow()] });
    await waitFor(() => expect(screen.getByText("Browser")).toBeTruthy());
    // A PAT / operator key / service account has no session row to name. Not
    // an error -- the list simply marks nothing.
    expect(screen.queryByText("This device")).toBeNull();
  });

  it("revokes one row through the SDK wrapper, after confirming", async () => {
    const sent: Array<Record<string, unknown>> = [];
    renderMe("/me/sessions", {
      sessions: [sessionRow()],
      sent,
      replies: {
        revokeSession: {
          revokeSessionResult: {
            requestId: "r",
            success: true,
            sessionId: "v1:identity:authSession:other",
            wasCurrent: false,
          },
        },
      },
    });

    await waitFor(() => expect(screen.getByText("Browser")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    const dialog = await waitFor(() => screen.getByRole("dialog"));
    expect(dialog.textContent).toContain("stops being able to reach this account");
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(sent.some((m) => "revokeSession" in m)).toBe(true));
    const msg = sent.find((m) => "revokeSession" in m) as {
      revokeSession: { sessionId: string };
    };
    expect(msg.revokeSession.sessionId).toBe("v1:identity:authSession:other");
  });

  it("warns in its own words when the row is the one you are sitting in", async () => {
    renderMe("/me/sessions", { sessions: [sessionRow({ id: CURRENT_SESSION })] });
    // Wait for the BADGE, not the button. The rows and the session_id arrive
    // on two different reads, so a click that only waited for the row can
    // land while thisDevice is still false -- and would then assert against
    // the generic copy while appearing to test the specific one.
    await waitFor(() => expect(screen.getByText("This device")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    const dialog = await waitFor(() => screen.getByRole("dialog"));
    expect(dialog.textContent).toContain("signs you out here");
  });

  it("lands on the sign-in page after revoking this device", async () => {
    const sent: Array<Record<string, unknown>> = [];
    renderMe("/me/sessions", {
      sessions: [sessionRow({ id: CURRENT_SESSION })],
      sent,
      replies: {
        revokeSession: {
          revokeSessionResult: {
            requestId: "r",
            success: true,
            sessionId: CURRENT_SESSION,
            // The server says so; the client does not infer it.
            wasCurrent: true,
          },
        },
      },
    });

    await waitFor(() => expect(screen.getByText("This device")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Continue with/ })).toBeTruthy(),
    );
  });

  it("names Sign out everywhere for what it does -- this session included", async () => {
    renderMe("/me/sessions", { sessions: [sessionRow()] });
    // Wait for the ROWS: the button is held disabled until there is something
    // to sign out of, so clicking it earlier would be a click on nothing.
    await waitFor(() => expect(screen.getByText("Browser")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    // There is no everywhere-ELSE call on the engine, so the button must not
    // imply one.
    expect(dialog.textContent).toContain("this browser included");
  });

  it("says the read failed rather than showing an empty table", async () => {
    renderMe("/me/sessions", { failReads: true });
    await waitFor(() => expect(screen.getByText(/could not read your sessions/)).toBeTruthy());
    // An empty table here reads as "no other device can reach your account",
    // which is the one wrong answer that reassures.
    expect(screen.queryByRole("table")).toBeNull();
  });
});

describe("/me/security (memql#4318, #4319)", () => {
  it("lists enrolled passkeys with the fact that decides recoverability", async () => {
    renderMe("/me/security", {
      passkeys: [passkeyRow(), passkeyRow({ id: "pk-2", label: "YubiKey", backupState: false })],
    });
    await waitFor(() => expect(screen.getByText("MacBook")).toBeTruthy());
    expect(screen.getByText("YubiKey")).toBeTruthy();
    expect(screen.getByText("Recoverable")).toBeTruthy();
    expect(screen.getByText("This device only")).toBeTruthy();
    expect(screen.getByText("2 enrolled")).toBeTruthy();
  });

  it("carries the identity links at the configured origin", async () => {
    renderMe("/me/security");
    await waitFor(() => expect(screen.getByRole("link", { name: /Manage passkeys/ })).toBeTruthy());
    const expected: Array<[RegExp, string]> = [
      [/Manage passkeys/, "https://identity.example.com/me/devices"],
      [/Personal access tokens/, "https://identity.example.com/me/tokens"],
      [/Account settings/, "https://identity.example.com/me/settings"],
      [/Export or delete your data/, "https://identity.example.com/me/export"],
    ];
    for (const [name, href] of expected) {
      expect(screen.getByRole("link", { name }).getAttribute("href")).toBe(href);
    }
  });

  it("disables the passkey-only switch, and SAYS WHY, with none enrolled", async () => {
    renderMe("/me/security", { passkeys: [] });
    const button = await waitFor(() =>
      screen.getByRole("button", { name: "Turn sign-in links off" }),
    );
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/Add a passkey first/)).toBeTruthy();
  });

  it("holds the switch back rather than offering it blind when the list cannot be read", async () => {
    // FAIL CLOSED, matching the server: a transport blip and "no passkeys"
    // must not reach the same decision when the difference is a lockout.
    //
    // The ACCOUNT read succeeds here and only the PASSKEY read fails, which
    // is the shape the guard is for -- and the shape Promise.allSettled in
    // useMe is what makes expressible.
    renderMe("/me/security", { failPasskeys: true });
    const button = await waitFor(() =>
      screen.getByRole("button", { name: "Turn sign-in links off" }),
    );
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/could not check your passkeys/)).toBeTruthy();
    expect(screen.getByText("count unavailable")).toBeTruthy();
    // ...and it must NOT say "add a passkey first", which would be a reason
    // that is not true.
    expect(screen.queryByText(/Add a passkey first/)).toBeNull();
  });

  it("sends the policy through the SDK wrapper, with no user id to aim", async () => {
    const sent: Array<Record<string, unknown>> = [];
    renderMe("/me/security", {
      sent,
      replies: {
        setSignInPolicy: {
          setSignInPolicyResult: { requestId: "r", success: true, policy: "passkey_only" },
        },
      },
    });

    const button = await waitFor(() =>
      screen.getByRole("button", { name: "Turn sign-in links off" }),
    );
    expect(button.hasAttribute("disabled")).toBe(false);
    fireEvent.click(button);

    await waitFor(() => expect(sent.some((m) => "setSignInPolicy" in m)).toBe(true));
    const msg = sent.find((m) => "setSignInPolicy" in m) as {
      setSignInPolicy: Record<string, unknown>;
    };
    expect(msg.setSignInPolicy.policy).toBe("passkey_only");
    // The authorization IS the absence of a target.
    expect(Object.keys(msg.setSignInPolicy).sort()).toEqual(["policy", "requestId"]);
  });

  it("surfaces a server refusal verbatim, never as a silent no-op", async () => {
    renderMe("/me/security", {
      replies: {
        setSignInPolicy: {
          setSignInPolicyResult: {
            requestId: "r",
            success: false,
            policy: "any",
            errorCode: "no_passkey",
            errorMessage:
              "Add a passkey first. Turning off sign-in links with no passkey enrolled would leave you unable to sign in at all.",
          },
        },
      },
    });

    fireEvent.click(
      await waitFor(() => screen.getByRole("button", { name: "Turn sign-in links off" })),
    );
    await waitFor(() => expect(screen.getByText(/That change was refused/)).toBeTruthy());
    expect(screen.getByText(/Add a passkey first/)).toBeTruthy();
  });

  it("reflects an account already on passkey-only, and offers the way back", async () => {
    renderMe("/me/security", { user: userRow({ signInPolicy: "passkey_only" }) });
    await waitFor(() => expect(screen.getByText("Passkey only")).toBeTruthy());
    const button = screen.getByRole("button", { name: "Turn sign-in links back on" });
    // Never disabled: a policy whose owner cannot reverse it is not a
    // security control.
    expect(button.hasAttribute("disabled")).toBe(false);
    expect(button.getAttribute("aria-pressed")).toBe("true");
  });
});
