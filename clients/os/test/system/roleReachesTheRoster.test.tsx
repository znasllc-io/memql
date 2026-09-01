import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type AccessSummary } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection provider dials a real websocket, so the hook is replaced.
// Everything else in the shell is the real thing.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { Shell } from "../../src/chrome/Shell";
import { resetIdsForTest } from "../../src/system/desks";
import { LocalDesktopStore } from "../../src/system/store";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";

// THE TEST THAT WAS MISSING (memql#4775).
//
// ===========================================================================
// WHAT BROKE, AND WHY EVERY EXISTING TEST STAYED GREEN THROUGH IT
// ===========================================================================
// The shell read the signed-in role over HTTP from
// `{identityUrl}/me/api/profile` -- a route registered in no Go file in this
// repo, which the identity service answers with its own HTML at 200. The read
// slipped past `!response.ok`, `response.json()` threw on the markup, the
// try/catch swallowed it, and `clusterRole` became "". `roleAdmits` refuses an
// unrankable role, so EVERY role-gated app was invisible to EVERY user in
// EVERY cluster -- the owner included. It presented as "the Users app was
// never built".
//
// The suite did not notice because the only test of that path handed
// `fetchMyAccess` a stub `fetch` returning the JSON it wanted, then asserted
// the URL STRING. A double that answers the call you are about to make cannot
// tell you whether anything serves it, and asserting the URL pins the wrong
// endpoint rather than catching it.
//
// So this test asserts the PROPERTY that actually matters and that no unit
// test covered: a role reported by the cluster reaches the app roster. It
// drives the real Shell, the real SessionProvider, the real role predicate and
// the real registry, and it fails against the old code -- which never asked
// the connection anything at all.

function summary(clusterRole: string, over: Partial<AccessSummary> = {}): AccessSummary {
  return {
    requestId: "req-1",
    userId: "v1:identity:user:u-42",
    primaryEmail: "ada@example.test",
    sessionId: "sess-1",
    clusterRole,
    ...over,
  } as AccessSummary;
}

/** A connection whose only job is to answer `getMyAccess`. */
function fakeConnection(access: AccessSummary | null, opts: { fail?: boolean } = {}) {
  const stub = {
    getMyAccess: vi.fn(async () => {
      if (opts.fail) throw new Error("stream refused");
      return access;
    }),
    executeNamed: vi.fn(async () => ({ rows: () => [], meta: () => null })),
  };
  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    dispatcher: { sendAndWait: vi.fn() },
    subscriptions: { subscribeGraph: () => () => {} },
  };
}

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function mountShell() {
  resetIdsForTest();
  return render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      config={{ ...UNKNOWN_RUNTIME_CONFIG, domain: "example.test" }}
      ports={{ store: new LocalDesktopStore(memStorage()) }}
    />,
  );
}

/** The launcher's app tiles, by name. */
async function launcherApps(): Promise<string[]> {
  const open = await screen.findByRole("button", { name: "Launcher" });
  const { fireEvent } = await import("@testing-library/react");
  fireEvent.click(open);
  const dialog = await screen.findByRole("dialog", { name: "Launcher" });
  return within(dialog)
    .getAllByRole("button")
    .map((b) => b.textContent?.trim() ?? "")
    .filter(Boolean);
}

beforeEach(() => {
  h.connection = null;
});

describe("the cluster's role reaches the app roster", () => {
  it("an OWNER is offered the admin-gated app", async () => {
    h.connection = fakeConnection(summary("owner"));
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
  });

  it("a WRITER is offered Training and NOT Users", async () => {
    // The discriminating case. Both apps are real and both are gated; only the
    // thresholds differ, so this fails for a shell that resolves no role at
    // all AND for one that resolves the wrong one.
    h.connection = fakeConnection(summary("writer"));
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Training");
    });
    expect(await launcherApps()).not.toContain("Users");
  });

  it("a role the cluster does not report admits NOTHING gated, and says nothing else broke", async () => {
    // Fail-closed, and the reachable positive is in the same assertion: the
    // ungated apps are still there, so an empty gated set is evidence about
    // the role rather than about a shell that failed to render.
    h.connection = fakeConnection(null);
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Settings");
    });
    const apps = await launcherApps();
    expect(apps).not.toContain("Users");
    expect(apps).not.toContain("Training");
    expect(apps).toContain("Deployables");
  });

  it("a REFUSED read is unknown, not a crash", async () => {
    h.connection = fakeConnection(null, { fail: true });
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Settings");
    });
    expect(await launcherApps()).not.toContain("Users");
  });

  it("asks the CLUSTER, rather than composing an identity URL", async () => {
    // The shape of the original defect: the facts were fetched from a route
    // nothing served. `getMyAccess` is a message the engine implements and the
    // SDK is contract-tested against, so "did we ask the right thing" is
    // answerable here in a way a URL string never was.
    const connection = fakeConnection(summary("owner"));
    h.connection = connection;
    mountShell();

    await waitFor(() => {
      expect(connection.query.getMyAccess).toHaveBeenCalled();
    });
  });

  it("an explicitly supplied access WINS and makes no read", async () => {
    // What keeps every existing harness working: a caller that already knows
    // who is signed in is not asking.
    const connection = fakeConnection(summary("reader"));
    h.connection = connection;
    resetIdsForTest();
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={{ userId: "u-1", primaryEmail: "a@b.c", clusterRole: "owner" }}
        config={{ ...UNKNOWN_RUNTIME_CONFIG, domain: "example.test" }}
        ports={{ store: new LocalDesktopStore(memStorage()) }}
      />,
    );

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
    expect(connection.query.getMyAccess).not.toHaveBeenCalled();
  });
});
