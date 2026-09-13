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
import { clearEffectiveCapabilities } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { installSeededAccess, seededAccessFor } from "../seededAccess";

// THE TEST THAT WAS MISSING (memql#4775), RE-KEYED TO THE EFFECTIVE SET (epic
// memql#5289).
//
// ===========================================================================
// WHAT BROKE, AND WHY EVERY EXISTING TEST STAYED GREEN THROUGH IT
// ===========================================================================
// The shell read the signed-in role over HTTP from a route nothing served,
// the read failed silently, the role became "" and EVERY role-gated app was
// invisible to EVERY user in EVERY cluster -- the owner included. The suite
// did not notice because the only test of that path stubbed the call it was
// about to make.
//
// The shell no longer decides from the ROLE at all. It asks the cluster what
// this person may open -- `effectiveCapabilitiesForActor`, the role catalog
// overlaid with their group and user grants -- and holds the answer as the
// one access predicate. So the property that matters now is the same one
// one level up: an answer the cluster gives reaches the app roster, and an
// answer it does not give admits nothing. This drives the real Shell, the
// real session scope, the real read hook, the real predicate and the real
// registry, and it fails against a shell that never asked.

function summary(role: string, over: Partial<AccessSummary> = {}): AccessSummary {
  return {
    requestId: "req-1",
    userId: "v1:identity:user:u-42",
    primaryEmail: "ada@example.test",
    sessionId: "sess-1",
    role,
    ...over,
  } as AccessSummary;
}

/** The effective read's one reply row, for a role's seeded set. */
function effectiveRow(role: string) {
  return { ok: true, code: "", role, userId: "u-42", entries: seededAccessFor(role) };
}

/**
 * A connection that answers `getMyAccess` and the effective read.
 *
 * The effective read goes through the GENERATED builtin, which lands on
 * `executeNamed` -- so the fake dispatches on the name, and a stub that
 * answered only the identity would leave the roster empty for everybody.
 */
function fakeConnection(
  access: AccessSummary | null,
  opts: { fail?: boolean; effectiveFor?: string | null; failEffective?: boolean } = {},
) {
  const effectiveFor = opts.effectiveFor === undefined ? (access?.role ?? null) : opts.effectiveFor;
  const stub = {
    getMyAccess: vi.fn(async () => {
      if (opts.fail) throw new Error("stream refused");
      return access;
    }),
    executeNamed: vi.fn(async (name: string) => {
      if (name === "effectiveCapabilitiesForActor") {
        if (opts.failEffective) throw new Error("stream refused");
        return { rows: () => (effectiveFor === null ? [] : [effectiveRow(effectiveFor)]), meta: () => null };
      }
      return { rows: () => [], meta: () => null };
    }),
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
  // Start from the production COLD state: nothing the harness pre-seeded.
  clearEffectiveCapabilities();
});

describe("the cluster's answer reaches the app roster", () => {
  it("an OWNER is offered the admin-gated app", async () => {
    h.connection = fakeConnection(summary("owner"));
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
  });

  it("a WRITER is offered Training and NOT Users", async () => {
    // The discriminating case. Both apps are real and both are gated; only
    // the seeded sets differ, so this fails for a shell that installs no set
    // at all AND for one that installs the wrong one.
    h.connection = fakeConnection(summary("writer"));
    mountShell();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Training");
    });
    expect(await launcherApps()).not.toContain("Users");
  });

  it("a set the cluster does not report admits NOTHING gated", async () => {
    // Fail-closed. Every app is a resource now, so an empty set is an empty
    // launcher -- and the identity is what shows the desk at all, so the
    // shell is drawn (the Launcher button is there) with nothing in it.
    h.connection = fakeConnection(summary("owner"), { effectiveFor: null });
    mountShell();

    await screen.findByRole("button", { name: "Launcher" });
    const apps = await launcherApps();
    expect(apps).not.toContain("Users");
    expect(apps).not.toContain("Training");
    expect(apps).not.toContain("Deployables");
  });

  it("a REFUSED effective read is unknown, not a crash", async () => {
    h.connection = fakeConnection(summary("owner"), { failEffective: true });
    mountShell();

    await screen.findByRole("button", { name: "Launcher" });
    expect(await launcherApps()).not.toContain("Users");
  });

  it("asks the CLUSTER for the set, through the generated read", async () => {
    // The shape of the original defect: the facts were fetched from a route
    // nothing served. Both reads here are messages the engine implements and
    // the SDK is contract-tested against, so "did we ask the right thing" is
    // answerable in a way a URL string never was.
    const connection = fakeConnection(summary("owner"));
    h.connection = connection;
    mountShell();

    await waitFor(() => {
      expect(connection.query.getMyAccess).toHaveBeenCalled();
      expect(connection.query.executeNamed).toHaveBeenCalledWith(
        "effectiveCapabilitiesForActor",
        expect.any(String),
        expect.anything(),
      );
    });
  });

  it("an explicitly supplied access WINS for the identity and makes no identity read", async () => {
    // What keeps every existing harness working: a caller that already knows
    // who is signed in is not asking. The EFFECTIVE SET is still the
    // cluster's to answer -- an identity handed in by a harness says who is
    // here, never what they may open.
    const connection = fakeConnection(summary("reader"), { effectiveFor: "owner" });
    h.connection = connection;
    resetIdsForTest();
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={{ userId: "u-1", primaryEmail: "a@b.c", role: "owner", roleName: "", rank: 0 }}
        config={{ ...UNKNOWN_RUNTIME_CONFIG, domain: "example.test" }}
        ports={{ store: new LocalDesktopStore(memStorage()) }}
      />,
    );

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
    expect(connection.query.getMyAccess).not.toHaveBeenCalled();
  });

  it("a set already held survives a connection going away", async () => {
    // The ladder's rule, for the ladder's reason: a dropped connection is
    // "the shell cannot reach the cluster right now", not "this person lost
    // access they hold". The desk follows the identity, which IS cleared.
    installSeededAccess("owner");
    h.connection = null;
    mountShell();
    // No identity, no desk: the core gate draws nothing gated. The set is
    // simply still there for the reconnect.
    const { holds } = await import("../../src/system/roles");
    expect(holds("read", "app:users")).toBe(true);
  });
});
