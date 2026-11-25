import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { readTheBarNow } from "../readTheBar";
import { SetupFactsScope } from "../../src/apps/setup/SetupFactsScope";
import { ConnectionRecoveryContext, ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";
import { CoreGate } from "../../src/chrome/CoreGate";
import { useOs } from "../../src/chrome/state";

// THE HELD PAGE (design record 2026-11-24-desktop-access, section 4): a person
// who is neither a cluster owner nor a developer and has no model access set
// up sees one page in place of the desktop, with a reference an administrator
// finds in Logs. It is mounted the way Shell.tsx mounts the gate -- inside the
// facts scope and the shell provider, with the desk as the gate's children --
// so "no desk" means the children were not drawn at all.
//
// Every answer here goes through the builtin's own wire shape (`builtinReply`),
// so the page reads what the engine sends and not a flat row it never does.

afterEach(() => {
  cleanup();
  h.connection = null;
  vi.useRealTimers();
  vi.restoreAllMocks();
  installSeededAccess("owner");
});

const DESK = <p data-testid="desk">the desk</p>;
const HEADING = "Your access is not set up yet";
const SENTENCE = "MemQL needs a model it can use for you. Ask your administrator to set one up, and give them this reference.";
const REFERENCE = "ERR-3f9a1c";
// Opaque to the page: it only hands it back.
const DIGEST = "p.5d41402a.r.7c4a8d09";
// A fixture, never a key.
const TYPED_KEY = "sk-ant-typed-on-the-held-page-0001";

const CONFIGURED = () => coreAt("configured", "configured", "configured");
const UNCONFIGURED = () => coreAt("unconfigured", "configured", "configured");

/** A held answer, with the reference and digest the engine wrote its line under. */
function held(over: Row = {}): Row {
  return { id: "current", admitted: false, exempt: false, sources: [], reference: REFERENCE, reasonsDigest: DIGEST, ...over };
}

/** A held answer whose line could not be written: no reference, no digest. */
function heldWithoutReference(): Row {
  return { id: "current", admitted: false, exempt: false, sources: [] };
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

/** The key builtins the add wizard reads and writes. */
function keys(connection: FakeConnection) {
  const add = vi.fn(async (args: Record<string, unknown>) =>
    builtinReply("vendorKeyAdd", [{
      id: "k-new-1",
      vendor: args["vendor"],
      label: args["label"],
      fingerprint: "...9z9z",
      isDefault: true,
      checkedBy: "list",
      status: "active",
      state: "good",
      relation: "mine",
      ownerUserId: "me",
    }]));
  Object.assign(connection.query, {
    vendorKeysForActor: vi.fn(async () => builtinReply("vendorKeysForActor", [{ id: "list", mine: [], lent: [], all: [] }])),
    vendorKeyAdd: add,
  });
  return add;
}

function clipboard() {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  return writeText;
}

/** Which windows are open, and what each was asked to do. */
function WindowProbe() {
  const { state } = useOs();
  const open = Object.values(state.shell.windows).map((w) => ({ app: w.appId, section: w.sectionId }));
  return <pre data-testid="windows">{JSON.stringify(open)}</pre>;
}

/** Opens a real window on the desk, the way a person working there does. */
function WindowDriver() {
  const { actions } = useOs();
  return <button type="button" onClick={() => actions.openApp("settings", "apikeys")}>open settings</button>;
}

function gate(role: string, feed = CONFIGURED(), opts: { userId?: string; beside?: ReactNode; onSignOut?: () => void } = {}): ReactNode {
  return withSession(
    <SetupFactsScope>
      {withOs(
        <>
          <CoreGate onSignOut={opts.onSignOut ?? (() => {})}>{DESK}</CoreGate>
          {opts.beside}
        </>,
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

const bar = () => screen.getByRole("group", { name: "What you can do with this" });

async function pressBar(name: string) {
  readTheBarNow();
  fireEvent.click(within(bar()).getByRole("button", { name }));
  await settle();
}

const calls = (connection: FakeConnection) => connection.query.desktopAccess.mock.calls.map((call) => call[0]);

describe("the held page", () => {
  it("a held person sees the reference and can copy it", async () => {
    installSeededAccess("writer");
    use(held());
    const writeText = clipboard();
    const signOut = vi.fn();
    render(gate("writer", CONFIGURED(), { onSignOut: signOut }));

    expect(await screen.findByRole("heading", { name: HEADING })).toBeTruthy();
    expect(screen.getByText(SENTENCE)).toBeTruthy();
    expect(screen.getByText(REFERENCE)).toBeTruthy();
    expect(screen.getByText("This page opens by itself once it is set up.")).toBeTruthy();
    // On a cluster whose own verdict is that it is set up: the page is about
    // the person, not the cluster.
    expect(screen.queryByTestId("desk")).toBeNull();
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Copy reference" }));
    expect(writeText).toHaveBeenCalledExactlyOnceWith(REFERENCE);
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(signOut).toHaveBeenCalledTimes(1);
  });

  it("an empty reference shows no reference line", async () => {
    // The hold line could not be written, so there is nothing Logs could find:
    // the page holds the person and shows no reference, and does not ask them
    // to hand one over.
    installSeededAccess("writer");
    use(heldWithoutReference());
    render(gate("writer"));

    expect(await screen.findByRole("heading", { name: HEADING })).toBeTruthy();
    expect(screen.getByText("MemQL needs a model it can use for you. Ask your administrator to set one up.")).toBeTruthy();
    expect(screen.queryByText(/give them this reference/)).toBeNull();
    expect(screen.queryByText(/ERR-/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy reference" })).toBeNull();
    expect(screen.getByText("This page opens by itself once it is set up.")).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();
  });
});

describe("who is let in", () => {
  it("an unknown answer lets the person in", async () => {
    // A check that cannot be answered is not evidence that a person lacks
    // access, whatever the cluster's own verdict: the model calls are still
    // refused on the server.
    for (const feed of [CONFIGURED(), UNCONFIGURED()]) {
      for (const failure of ["refused", "no answer"]) {
        cleanup();
        installSeededAccess("writer");
        const connection = use(null);
        if (failure === "refused") connection.query.desktopAccess.mockRejectedValue(new Error("desktop access: the stored role could not be read"));
        render(gate("writer", feed));
        expect(await screen.findByTestId("desk")).toBeTruthy();
        expect(screen.queryByText(HEADING)).toBeNull();
      }
    }
  });

  it("an admitted answer lets the person in", async () => {
    // Set up counts, asleep or not, and the cluster's own verdict does not
    // hold somebody who has access of their own.
    for (const feed of [CONFIGURED(), UNCONFIGURED()]) {
      cleanup();
      installSeededAccess("writer");
      use(admitted({ sources: [{ kind: "machine", state: "asleep", own: false, lenderUserId: "ana", label: "Ana's laptop" }] }));
      render(gate("writer", feed));
      expect(await screen.findByTestId("desk")).toBeTruthy();
      expect(screen.queryByText(HEADING)).toBeNull();
    }
  });

  it("an owner with nothing set up reaches the desktop, or Set up this cluster when the cluster has no model", async () => {
    for (const role of ["owner", "developer"]) {
      cleanup();
      installSeededAccess(role);
      use(admitted({ exempt: true, sources: [] }), { passkeysForSelf: [passkeyRow({ id: "v1:identity:identity:pk-1" })] });
      const view = render(gate(role, CONFIGURED()));
      expect(await screen.findByTestId("desk")).toBeTruthy();

      view.rerender(gate(role, UNCONFIGURED()));
      expect(await screen.findByRole("list", { name: "Set up this cluster" })).toBeTruthy();
      expect(screen.queryByText(HEADING)).toBeNull();
      expect(screen.queryByTestId("desk")).toBeNull();
    }
  });
});

describe("checking again", () => {
  it("it checks again every 30 seconds while visible and lifts when admitted", async () => {
    vi.useFakeTimers();
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    installSeededAccess("writer");
    const connection = use(held());
    render(gate("writer"));
    // The first check lands, then the entry frame's exit runs its course.
    await advance(1);
    await advance(200);
    expect(screen.getByRole("heading", { name: HEADING })).toBeTruthy();
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // A hidden tab does not ask.
    visibility.mockReturnValue("hidden");
    await advance(30_000);
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // Back in view, after an administrator lent a machine: the next tick asks,
    // and the page lifts by itself.
    visibility.mockReturnValue("visible");
    connection.query.desktopAccess.mockResolvedValue(answer(admitted({ sources: [{ kind: "machine", state: "asleep", own: false, lenderUserId: "ana" }] })));
    await advance(30_000);
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("desk")).toBeTruthy();
    expect(screen.queryByText(HEADING)).toBeNull();
  });

  it("it checks again on focus", async () => {
    // Access taken away while somebody works: coming back to the window asks
    // again, and the page takes the screen whatever windows are open.
    installSeededAccess("writer");
    const connection = use(admitted());
    render(gate("writer", CONFIGURED(), { beside: <><WindowDriver /><WindowProbe /></> }));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "open settings" }));
    await waitFor(() => expect(JSON.parse(screen.getByTestId("windows").textContent ?? "[]")).toHaveLength(1));

    connection.query.desktopAccess.mockResolvedValue(answer(held()));
    fireEvent.focus(window);
    expect(await screen.findByRole("heading", { name: HEADING })).toBeTruthy();
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(screen.queryByTestId("desk")).toBeNull();
  });

  it("a check again passes the reference back", async () => {
    installSeededAccess("writer");
    const connection = use(held());
    render(gate("writer"));
    await screen.findByRole("heading", { name: HEADING });
    expect(calls(connection)[0]).toEqual({ reference: "", reasonsDigest: "" });

    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    expect(calls(connection)[1]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
    expect(screen.getByText(REFERENCE)).toBeTruthy();
  });

  it("a check that fails while the page is shown keeps the page and its reference", async () => {
    // Only a first check that cannot be answered lets a person in. Once the
    // engine has held them, a check that fails is no evidence they now have
    // access, and the next one still hands the reference back.
    vi.useFakeTimers({ toFake: ["Date"] });
    installSeededAccess("writer");
    const connection = use(held());
    render(gate("writer"));
    await screen.findByRole("heading", { name: HEADING });

    connection.query.desktopAccess.mockRejectedValueOnce(new Error("stream closed"));
    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    await settle();
    expect(screen.getByRole("heading", { name: HEADING })).toBeTruthy();
    expect(screen.getByText(REFERENCE)).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();

    // Past the resume signal's coalescing window, the next focus asks again.
    vi.setSystemTime(Date.now() + 1_000);
    fireEvent.focus(window);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(3));
    expect(calls(connection)[2]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
  });

  it("checks again when the connection returns", async () => {
    installSeededAccess("writer");
    const connection = use(held());
    const shell = (status: ShellConnectionStatus) => (
      <ConnectionRecoveryContext.Provider value={{ managed: true, retry: () => {} }}>
        <ConnectionStatusContext.Provider value={status}>{gate("writer")}</ConnectionStatusContext.Provider>
      </ConnectionRecoveryContext.Provider>
    );
    const view = render(shell("connected"));
    await screen.findByRole("heading", { name: HEADING });
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);

    // While the stream is down nothing is asked, and the page stays.
    view.rerender(shell("reconnecting"));
    await settle();
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("heading", { name: HEADING })).toBeTruthy();

    connection.query.desktopAccess.mockResolvedValue(answer(admitted()));
    view.rerender(shell("connected"));
    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2);
    expect(calls(connection)[1]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
  });

  it("does not hand one person's reference to the next person signed in", async () => {
    installSeededAccess("writer");
    const connection = use(held());
    const view = render(gate("writer"));
    await screen.findByRole("heading", { name: HEADING });

    view.rerender(gate("writer", CONFIGURED(), { userId: "v1:identity:user:somebody-else" }));
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    expect(calls(connection)[1]).toEqual({ reference: "", reasonsDigest: "" });
  });
});

describe("adding a key on the page", () => {
  it("adding a key happens on the page, and no dock, launcher or app window is drawn", async () => {
    installSeededAccess("writer");
    const connection = use(held());
    keys(connection);
    render(gate("writer", CONFIGURED(), { beside: <WindowProbe /> }));

    fireEvent.click(await screen.findByRole("button", { name: "Add an API key" }));
    const title = await screen.findByRole("heading", { name: "Add an API key" });
    // In the entry frame, where the page was, and nothing behind it.
    expect(title.closest(".os-entry-layout")).not.toBeNull();
    expect(screen.getByRole("radiogroup", { name: "Vendor" })).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();
    expect(document.querySelector("[data-os-root]")).toBeNull();
    expect(document.querySelector("[data-os-window]")).toBeNull();
    expect(JSON.parse(screen.getByTestId("windows").textContent ?? "[]")).toEqual([]);
    expect(screen.queryByText(HEADING)).toBeNull();
    // No trail back to a list of keys this page never showed, as the entry
    // frame's other wizards draw none.
    expect(screen.queryByRole("button", { name: "Back to API keys" })).toBeNull();
    expect(screen.queryByText("API keys")).toBeNull();

    // Its way out returns to the page.
    vi.useFakeTimers({ toFake: ["Date"] });
    await pressBar("Cancel");
    expect(screen.getByRole("heading", { name: HEADING })).toBeTruthy();
    expect(screen.queryByTestId("desk")).toBeNull();
  });

  it("a key added checks again", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    installSeededAccess("writer");
    const connection = use(held());
    const add = keys(connection);
    render(gate("writer"));

    fireEvent.click(await screen.findByRole("button", { name: "Add an API key" }));
    fireEvent.click(await screen.findByRole("radio", { name: /^Anthropic/ }));
    await settle();
    fireEvent.change(screen.getByLabelText("A name for this key"), { target: { value: "Personal" } });
    await pressBar("Continue");
    fireEvent.change(screen.getByLabelText("The API key"), { target: { value: TYPED_KEY } });

    connection.query.desktopAccess.mockResolvedValue(answer(admitted()));
    await pressBar("Add key");

    expect(add).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
    expect(calls(connection)[1]).toEqual({ reference: REFERENCE, reasonsDigest: DIGEST });
    expect(await screen.findByTestId("desk")).toBeTruthy();
    expect(screen.queryByText(HEADING)).toBeNull();
  });

  it("a key whose reply never came returns to the page and checks again, not to a list of keys", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    installSeededAccess("writer");
    const connection = use(held());
    const add = keys(connection);
    add.mockRejectedValueOnce(Object.assign(new Error("stream closed"), { reason: "transport" }));
    render(gate("writer"));

    fireEvent.click(await screen.findByRole("button", { name: "Add an API key" }));
    fireEvent.click(await screen.findByRole("radio", { name: /^Anthropic/ }));
    await settle();
    fireEvent.change(screen.getByLabelText("A name for this key"), { target: { value: "Personal" } });
    await pressBar("Continue");
    fireEvent.change(screen.getByLabelText("The API key"), { target: { value: TYPED_KEY } });
    await pressBar("Add key");

    expect(screen.getByText("The key may have been added.")).toBeTruthy();
    expect(within(bar()).queryAllByRole("button").map((b) => b.textContent?.trim())).toEqual(["Done"]);
    await pressBar("Done");
    expect(screen.getByRole("heading", { name: HEADING })).toBeTruthy();
    await waitFor(() => expect(connection.query.desktopAccess).toHaveBeenCalledTimes(2));
  });
});
