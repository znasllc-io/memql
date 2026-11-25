import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

import type { ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { coreAt, withOs } from "../setup/harness";
import { fakeConnection, passkeyRow, withSession, type FakeConnection } from "../cluster/harness";
import { builtinReply } from "../fleet/harness";
import { installSeededAccess } from "../seededAccess";
import { SetupFactsScope } from "../../src/apps/setup/SetupFactsScope";
import { ConnectionRecoveryContext, ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";
import { CoreGate } from "../../src/chrome/CoreGate";
import { DesktopAccessScope, useHasNoModelAccess } from "../../src/chrome/desktopAccessContext";
import { useOs } from "../../src/chrome/state";

// THE PERSON'S OWN MODEL ACCESS, AS THE DESK READS IT (design record
// 2026-11-25-who-gets-into-memql-os). The engine's `desktopAccess` answers for
// the caller: exempt, something set up, or nothing. It keeps nobody out of a
// cluster that is set up. A person it finds nothing for enters and has no Ask.
//
// It is mounted the way Shell.tsx mounts it: one read (DesktopAccessScope)
// above the gate, with the desk as the gate's children. The desk here is one
// line saying whether Ask belongs on it, read through the same predicate
// AskProvider and the roster read (`useHasNoModelAccess`). That the dock, the
// phone bar and the widget follow it is test/system/readinessReactivity.test.tsx.
//
// Every answer here goes through the builtin's own wire shape (`builtinReply`),
// so the read takes what the engine sends and not a flat row it never does.

afterEach(() => {
  cleanup();
  h.connection = null;
  vi.useRealTimers();
  vi.restoreAllMocks();
  installSeededAccess("owner");
});

/** What the desk would draw: whether Ask belongs on it. */
function Desk() {
  return <p data-testid="desk">{useHasNoModelAccess() ? "no ask" : "ask"}</p>;
}
const desk = () => screen.getByTestId("desk").textContent;

/** The page release 0.31.41 drew in place of the desk for such a person. It is gone. */
const REMOVED_PAGE = "Your access is not set up yet";
const REFERENCE = "ERR-3f9a1c";
// Opaque to the client: it only hands it back.
const DIGEST = "p.5d41402a.r.7c4a8d09";

const CONFIGURED = () => coreAt("configured", "configured", "configured");
const UNCONFIGURED = () => coreAt("unconfigured", "configured", "configured");

/** Nothing set up, with the reference and digest the engine wrote its line under. */
function noAccess(over: Row = {}): Row {
  return { id: "current", admitted: false, exempt: false, sources: [], reference: REFERENCE, reasonsDigest: DIGEST, ...over };
}

function admitted(over: Row = {}): Row {
  return { id: "current", admitted: true, exempt: false, sources: [{ kind: "key", state: "ready", own: true, label: "Personal" }], ...over };
}

const answer = (row: Row) => builtinReply("desktopAccess", [row]);

/** The cluster's double, answering the access check with `row` until a test says otherwise. */
function use(row: Row | null, seed: Parameters<typeof fakeConnection>[0] = {}): FakeConnection {
  const connection = fakeConnection({ passkeysForSelf: [], ...seed });
  if (row !== null) connection.query.desktopAccess.mockResolvedValue(answer(row));
  h.connection = connection;
  return connection;
}

/** How many windows are open. */
function WindowProbe() {
  const { state } = useOs();
  return <pre data-testid="windows">{Object.keys(state.shell.windows).length}</pre>;
}

/** Opens a real window on the desk, the way a person working there does. */
function WindowDriver() {
  const { actions } = useOs();
  return <button type="button" onClick={() => actions.openApp("settings", "apikeys")}>open settings</button>;
}

function gate(role: string, feed = CONFIGURED(), opts: { userId?: string; beside?: ReactNode } = {}): ReactNode {
  return withSession(
    <SetupFactsScope>
      {withOs(
        <DesktopAccessScope>
          <CoreGate onSignOut={() => {}}><Desk /></CoreGate>
          {opts.beside}
        </DesktopAccessScope>,
        role,
      )}
    </SetupFactsScope>,
    { role, readiness: feed, userId: opts.userId },
  );
}

/** Move fake timers, letting the reads they start land. */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

const calls = (connection: FakeConnection) => connection.query.desktopAccess.mock.calls.map((call) => call[0]);

describe("who has Ask", () => {
  it("a person with nothing set up enters, and has no Ask", async () => {
    installSeededAccess("writer");
    use(noAccess());
    render(gate("writer"));

    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(desk()).toBe("no ask");
    // No page stands in place of the desk, and the reference is shown nowhere.
    expect(document.querySelector("[data-os-core-gate]")).toBeNull();
    expect(screen.queryByText(REMOVED_PAGE)).toBeNull();
    expect(screen.queryByText(REFERENCE)).toBeNull();
  });

  it("waits at the door for the first check, so the desk is not drawn with an Ask that then goes", async () => {
    // The cluster is set up and its facts are in: only the person's own check
    // is out, and it alone keeps the opening frame up.
    installSeededAccess("writer");
    const connection = use(null);
    let reply!: (value: unknown) => void;
    connection.query.desktopAccess.mockReturnValueOnce(new Promise((resolve) => { reply = resolve; }));
    render(gate("writer"));
    await settle();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.getByRole("status").textContent).toBe("Opening MemQL OS");
    expect(screen.queryByTestId("desk")).toBeNull();

    await act(async () => reply(answer(noAccess())));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(desk()).toBe("no ask");
  });

  it("an unknown answer takes nothing away", async () => {
    // A check that cannot be answered is not evidence that a person lacks
    // access: the model calls are still refused on the server.
    for (const failure of ["refused", "no answer"]) {
      cleanup();
      installSeededAccess("writer");
      const connection = use(null);
      if (failure === "refused") connection.query.desktopAccess.mockRejectedValue(new Error("desktop access: the stored role could not be read"));
      render(gate("writer"));
      expect(await screen.findByTestId("desk")).toBeTruthy();
      expect(desk()).toBe("ask");
    }
  });

  it("set up counts, asleep or out of credit", async () => {
    for (const source of [
      { kind: "machine", state: "asleep", own: false, lenderUserId: "ana", label: "Ana's laptop" },
      { kind: "key", state: "outOfCredit", own: true, label: "Anthropic" },
      { kind: "app", state: "outOfUsage", own: true, app: "claude-code" },
    ]) {
      cleanup();
      installSeededAccess("writer");
      use(admitted({ sources: [source] }));
      render(gate("writer"));
      expect(await screen.findByTestId("desk")).toBeTruthy();
      expect(desk()).toBe("ask");
    }
  });

  it("an owner or developer with nothing set up keeps Ask, and gets Set up this cluster when the cluster has no model", async () => {
    for (const role of ["owner", "developer"]) {
      cleanup();
      installSeededAccess(role);
      use(admitted({ exempt: true, sources: [] }), { passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
      const view = render(gate(role, CONFIGURED()));
      expect(await screen.findByTestId("desk")).toBeTruthy();
      expect(desk()).toBe("ask");

      view.rerender(gate(role, UNCONFIGURED()));
      expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
      expect(screen.queryByTestId("desk")).toBeNull();
    }
  });
});

describe("checking again", () => {
  it("it checks again every 30 seconds while the person has no access and the tab is visible, and Ask comes by itself", async () => {
    vi.useFakeTimers();
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    installSeededAccess("writer");
    const connection = use(noAccess());
    render(gate("writer"));
    // The first check lands, then the entry frame's exit runs its course.
    await advance(1);
    await advance(200);
    expect(desk()).toBe("no ask");
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // A hidden tab does not ask.
    visibility.mockReturnValue("hidden");
    await advance(30_000);
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // Back in view, after an administrator lent a machine: the next tick asks.
    visibility.mockReturnValue("visible");
    connection.query.desktopAccess.mockResolvedValue(answer(admitted({ sources: [{ kind: "machine", state: "asleep", own: false, lenderUserId: "ana" }] })));
    await advance(30_000);
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(desk()).toBe("ask");

    // With access set up, nothing asks on a timer.
    await advance(60_000);
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
  });

  it("it checks again on focus: access taken away takes Ask, and the desk and its windows stay", async () => {
    installSeededAccess("writer");
    const connection = use(admitted());
    render(gate("writer", CONFIGURED(), { beside: <><WindowDriver /><WindowProbe /></> }));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(desk()).toBe("ask");
    fireEvent.click(screen.getByRole("button", { name: "open settings" }));
    await waitFor(() => expect(screen.getByTestId("windows").textContent).toBe("1"));

    connection.query.desktopAccess.mockResolvedValue(answer(noAccess()));
    fireEvent.focus(window);
    await waitFor(() => expect(desk()).toBe("no ask"));
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("windows").textContent).toBe("1");
    expect(screen.queryByText(REMOVED_PAGE)).toBeNull();
  });

  it("a check again passes the reference back", async () => {
    // So a person re-checked every 30 seconds adds one line to Logs, not one a poll.
    installSeededAccess("writer");
    const connection = use(noAccess());
    render(gate("writer"));
    await screen.findByTestId("desk");
    expect(calls(connection)[0]).toEqual({ reference: "", reasonsDigest: "" });

    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    expect(calls(connection)[1]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
  });

  it("a check that fails keeps the answer it had, and its reference", async () => {
    // Only a first check that cannot be answered leaves Ask. Once the engine
    // has answered, a check that fails is no evidence that anything changed.
    vi.useFakeTimers({ toFake: ["Date"] });
    installSeededAccess("writer");
    const connection = use(noAccess());
    render(gate("writer"));
    await screen.findByTestId("desk");
    expect(desk()).toBe("no ask");

    connection.query.desktopAccess.mockRejectedValueOnce(new Error("stream closed"));
    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    await settle();
    expect(desk()).toBe("no ask");

    // Past the resume signal's coalescing window, the next focus asks again.
    vi.setSystemTime(Date.now() + 1_000);
    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(3));
    expect(calls(connection)[2]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
  });

  it("checks again when the connection returns", async () => {
    installSeededAccess("writer");
    const connection = use(noAccess());
    const shell = (status: ShellConnectionStatus) => (
      <ConnectionRecoveryContext.Provider value={{ managed: true, retry: () => {} }}>
        <ConnectionStatusContext.Provider value={status}>{gate("writer")}</ConnectionStatusContext.Provider>
      </ConnectionRecoveryContext.Provider>
    );
    const view = render(shell("connected"));
    await screen.findByTestId("desk");
    expect(desk()).toBe("no ask");
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // While the stream is down nothing is asked, and the answer stays.
    view.rerender(shell("reconnecting"));
    await settle();
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);
    expect(desk()).toBe("no ask");

    connection.query.desktopAccess.mockResolvedValue(answer(admitted()));
    view.rerender(shell("connected"));
    await waitFor(() => expect(desk()).toBe("ask"));
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(calls(connection)[1]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
  });

  it("does not hand one person's reference to the next person signed in", async () => {
    installSeededAccess("writer");
    const connection = use(noAccess());
    const view = render(gate("writer"));
    await screen.findByTestId("desk");

    view.rerender(gate("writer", CONFIGURED(), { userId: "v1:identity:user:somebody-else" }));
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    expect(calls(connection)[1]).toEqual({ reference: "", reasonsDigest: "" });
  });
});
