// The profile surface: /me, /me/settings, /me/sessions, /me/security
// (memql#4318, #4319, #4523).
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
    // The preferences bag rides currentUser's `userFull` shape (memql#4523).
    // Deliberately PARTIAL: `notifications`, `takeoverMode` and `voiceMode` are
    // absent, so the tests below also cover the absent-reads-as-its-documented
    // default rule -- which matters because two of the defaults are TRUE and a
    // falsy read would render them as off.
    preferences: {
      language: "en-GB",
      theme: "dark",
      timezone: "Europe/London",
      archiveRetentionDays: 60,
      dailySpaceEnabled: true,
      dailySpaceRolloverAction: "save",
      cursorTweenMs: 1500,
      interactivePace: "quick",
      computerUseEnabled: true,
      activeAssistantId: "v1:agents:agent:pointer",
    },
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
  // Every executeNamed call, as (name, composed MemQL text). The fake is
  // re-parented onto the real QueryClient.prototype, so the REAL generated
  // builder runs above the stub -- which is what lets a test assert on the
  // call string the portal actually puts on the wire rather than on a
  // hand-typed copy of it.
  calls?: Array<[string, string]>;
  // Overrides the runtime config, for the identity-origin cases.
  config?: Record<string, unknown>;
}

function fakeConnection(fx: Fixture): Connection {
  const query = asQueryClient({
    // The registry a real cluster publishes. It was empty here until epic
    // memql#4661 made the Me tabs ARRANGEMENTS: a section resolves its concept
    // through the registry like every other page, so an empty one means "this
    // cluster publishes no such concept" -- true of this fixture and false of
    // any cluster the page runs against.
    listConcepts: vi.fn(async () => [
      {
        id: "v1:identity:user",
        version: "v1",
        domain: "identity",
        entity: "user",
        type: "concept",
        description: "A person who can sign in",
      },
      {
        id: "v1:identity:authSession",
        version: "v1",
        domain: "identity",
        entity: "authSession",
        type: "concept",
        description: "A live session",
      },
    ]),
    getMyAccess: vi.fn(async () => ({
      userId: "v1:identity:user:u-42",
      primaryEmail: "ada@example.test",
      clusterRole: "owner",
      displayName: "Ada Lovelace",
      sessionId: fx.sessionId ?? CURRENT_SESSION,
    })),
    executeNamed: vi.fn(async (name: string, text?: string) => {
      fx.calls?.push([name, text ?? ""]);
      if (fx.failReads) throw new Error("stream refused the read");
      if (name === "updateMyPreferences" || name === "toggleComputerUseEnabled") {
        return result([]);
      }
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
        config={{ ...AUTH_ENABLED_CLUSTER, ...(fx.config ?? {}) }}
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
  it("renders the four tabs, each a real address", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Ada Lovelace" })).toBeTruthy());

    // Order is load-bearing (memql#4523): Settings sits beside the account
    // facts, and Sessions/Security stay adjacent as the pair they read as.
    for (const [label, href] of [
      ["Account", "/me"],
      ["Settings", "/me/settings"],
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

  // The "Edit on identity" link that lived here MOVED to /me/settings
  // (memql#4523, one door per destination). The claim it carried -- the origin
  // is the CONFIGURED one, never a hardcoded host -- moved with it and is
  // asserted in the /me/settings block below, along with the assertion that
  // this tab no longer carries it.

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
    await waitFor(() => expect(screen.getByText(/Could not read your account/)).toBeTruthy());
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
    const table = await waitFor(() => screen.getByRole("table"));
    // Both rows share a user agent -- which is the point: two tabs of one
    // browser do too, so a label derived from it would mark the wrong row and
    // invite somebody to revoke the session they are sitting in. Scoped to the
    // TABLE because the revoke band below repeats the badge deliberately: it
    // is what tells a person which row they are about to end.
    await waitFor(() => expect(within(table).getAllByText("This device")).toHaveLength(1));
    const marked = within(table).getByText("This device").closest("tr");
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
    await waitFor(() => expect(screen.getAllByText("This device").length).toBeGreaterThan(0));
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

    await waitFor(() => expect(screen.getAllByText("This device").length).toBeGreaterThan(0));
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

  it("draws the list through the shared table element, not a hand-rolled one", async () => {
    // One implementation draws every table in the portal. The only other
    // hand-rolled <table> in the app is ConceptSchemaPane, whose content is a
    // concept's declared FIELDS rather than a row set.
    renderMe("/me/sessions", { sessions: [sessionRow()] });
    const table = await waitFor(() => screen.getByRole("table"));
    // view-kit stamps its own class prefix on everything it draws, so this
    // fails the moment somebody replaces the element with hand-written markup.
    const drawnByViewKit =
      table.className.includes("vk-") || table.closest("[class*='vk-']") !== null;
    expect(drawnByViewKit).toBe(true);
  });

  it("says the read failed rather than showing an empty table", async () => {
    renderMe("/me/sessions", { failReads: true });
    await waitFor(() => expect(screen.getByText(/Could not read your sessions/)).toBeTruthy());
    // The rationale that used to be in the callout is now a NEXT STEP written
    // for the reader rather than about the interface (memql#4657).
    expect(screen.getByText(/the answer is unknown/)).toBeTruthy();
    // An empty table here reads as "no other device can reach your account",
    // which is the one wrong answer that reassures -- and neither the table
    // nor the revoke band may render.
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
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
      [/Export your data/, "https://identity.example.com/me/export"],
      // Deletion is on SETTINGS, not on the export page. Identity's
      // me_settings.templ carries "Delete account" and the cooldown copy;
      // me_export.templ carries neither.
      [/Account settings and deletion/, "https://identity.example.com/me/settings"],
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
    // ...and it does NOT claim to be a toggle. The label says what the press
    // will do and the Badge says what is true now; aria-pressed on top of a
    // changing label announces "Turn sign-in links back on, pressed".
    expect(button.hasAttribute("aria-pressed")).toBe(false);
  });
});

describe("/me/settings -- the user settings surface (memql#4523)", () => {
  // A save's composed call string, for the group whose Save was pressed.
  function saveCall(calls: Array<[string, string]>): string {
    const write = calls.filter(([name]) => name === "updateMyPreferences");
    expect(write.length).toBe(1);
    return write[0]?.[1] ?? "";
  }

  // Field renders label, control and hint inside ONE <label>, so a control's
  // accessible name is "<label> <hint>" -- which is why these match on the
  // prefix rather than exactly. That is the wrapper every form in the portal
  // uses; the hint belongs in the accessible name.
  it("renders the stored values, and absent keys as their documented default", async () => {
    renderMe("/me/settings");
    await waitFor(() => expect(screen.getByText("Locale")).toBeTruthy());

    // Stored.
    expect((screen.getByLabelText(/^Language/) as HTMLInputElement).value).toBe("en-GB");
    expect((screen.getByLabelText(/^Timezone/) as HTMLInputElement).value).toBe("Europe/London");
    expect((screen.getByLabelText(/^Pace/) as HTMLSelectElement).value).toBe("quick");
    expect((screen.getByLabelText(/^Cursor travel time/) as HTMLInputElement).value).toBe("1500");

    // Absent from the fixture bag. A concept @default is never applied on
    // write, so these keys genuinely are not on the row -- and the form must
    // show what the cluster ACTS on, which is the documented default. Both of
    // the first two default to TRUE, which is exactly where a falsy read would
    // render "off" and quietly lie.
    expect((screen.getByRole("checkbox", { name: /Send me notifications/ }) as HTMLInputElement).checked).toBe(
      true,
    );
    expect((screen.getByLabelText(/^During a takeover/) as HTMLSelectElement).value).toBe("clean");
    expect((screen.getByLabelText(/^Microphone/) as HTMLSelectElement).value).toBe("toggle");
  });

  it("saves ONE group, and cannot spell either protected key", async () => {
    const calls: Array<[string, string]> = [];
    renderMe("/me/settings", { calls });
    await waitFor(() => expect(screen.getByText("Locale")).toBeTruthy());

    fireEvent.change(screen.getByLabelText(/^Timezone/), {
      target: { value: "America/Denver" },
    });
    const locale = screen.getByText("Locale").closest("section") ?? document.body;
    fireEvent.click(within(locale).getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(calls.some(([name]) => name === "updateMyPreferences")).toBe(true),
    );
    const text = saveCall(calls);

    // Its own group's fields, and nothing else. A save that shipped the whole
    // bag would let this tab clobber an edit made in another one.
    expect(text).toContain('timezone: "America/Denver"');
    expect(text).toContain('language: "en-GB"');
    expect(text).not.toContain("cursorTweenMs");
    expect(text).not.toContain("dailySpaceEnabled");

    // The headline claim of memql#4522, asserted on the wire text: the general
    // preferences write has no way to name either protected key, so no
    // sequence of clicks on this page can produce a call that carries one.
    expect(text).not.toContain("computerUseEnabled");
    expect(text).not.toContain("activeAssistantId");
  });

  it("routes the kill switch through its own mutation, behind a confirm", async () => {
    const calls: Array<[string, string]> = [];
    renderMe("/me/settings", { calls });
    await waitFor(() => expect(screen.getByText("Computer use")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Turn computer use off" }));

    // Nothing is written until the consequence has been read. Disabling
    // suspends running plans, so an immediate write on the first click would
    // be a side effect the person never agreed to.
    expect(calls.some(([name]) => name === "toggleComputerUseEnabled")).toBe(false);
    expect(screen.getByText(/kill_switch_engaged/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Turn it off" }));
    await waitFor(() =>
      expect(calls.some(([name]) => name === "toggleComputerUseEnabled")).toBe(true),
    );

    const toggle = calls.find(([name]) => name === "toggleComputerUseEnabled");
    expect(toggle?.[1]).toContain("enabled: false");
    // And it did NOT go through the general write -- which could not have
    // carried it anyway, and that is the point of it being a separate control.
    expect(calls.some(([name]) => name === "updateMyPreferences")).toBe(false);
  });

  it("cancelling the confirm writes nothing", async () => {
    const calls: Array<[string, string]> = [];
    renderMe("/me/settings", { calls });
    await waitFor(() => expect(screen.getByText("Computer use")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Turn computer use off" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(calls.some(([name]) => name === "toggleComputerUseEnabled")).toBe(false);
  });

  // The origin is the CONFIGURED one, never a hardcoded host: a literal here
  // would send an operator to somebody else's cluster to manage their own
  // account. This is the assertion that moved off the Account tab.
  it("names identity's own pages when an origin is configured", async () => {
    renderMe("/me/settings");
    await waitFor(() => expect(screen.getByText("Identity and data")).toBeTruthy());

    const email = screen.getByRole("link", { name: /Email and account deletion/ });
    expect(email.getAttribute("href")).toBe("https://identity.example.com/me/settings");
    const exported = screen.getByRole("link", { name: /Export your data/ });
    expect(exported.getAttribute("href")).toBe("https://identity.example.com/me/export");
  });

  // A link to nowhere is worse than an absent one: the reader concludes the
  // capability is broken rather than that this cluster does not have it.
  it("renders no identity link-outs when no origin is configured", async () => {
    // authEnabled:false is what "no identity origin" actually looks like. An
    // empty identityUrl with auth ON is `misconfigured` -- the portal refuses
    // to render at all rather than showing a console nobody can sign into --
    // so that fixture would prove nothing about this band.
    renderMe("/me/settings", { config: { identityUrl: "", authEnabled: false } });
    await waitFor(() => expect(screen.getByText("Locale")).toBeTruthy());
    expect(screen.queryByText("Identity and data")).toBeNull();
  });

  // The link MOVED; it did not disappear. Both halves are asserted, because
  // "removed from Account" alone would pass if it had been dropped outright.
  it("moved the identity link off the Account tab", async () => {
    renderMe("/me");
    await waitFor(() => expect(screen.getByText("Member since")).toBeTruthy());
    expect(screen.queryByRole("link", { name: /Edit on identity/ })).toBeNull();
  });

  it("does not render the app-managed assistant pointer", async () => {
    renderMe("/me/settings");
    await waitFor(() => expect(screen.getByText("Locale")).toBeTruthy());
    // It is on the row (the fixture sets it) and it is deliberately not a
    // control: a value with no meaning to a person, which they would break by
    // editing (memql#406).
    expect(screen.queryByText(/pointer/)).toBeNull();
  });
});
