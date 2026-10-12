import { act, cleanup, fireEvent, render, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { betweenTheClicks, readTheBar, useFakeDate } from "../readTheBar";

// A FILE DROPPED ON THE BIN IN THE DOCK ASKS FIRST (memql#4784), in a bubble
// over the Bin, outside any bar. The question holds presses for a moment after
// it appears (memql#6033), like every question answered in place.
//
// The drop is reported by the shell's drag scope, which needs a pointer drag
// dnd-kit can measure, and jsdom has no layout. So the dock's own claim on that
// scope is taken as it is made, and called with the event a finished drag hands
// it -- the dock decides everything from that event.

const h = vi.hoisted(() => ({
  connection: null as unknown,
  dock: null as null | { onDragEnd: (event: unknown) => void },
}));

vi.mock("../../src/live/connection", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/live/connection")>()),
  useOsConnection: () => h.connection,
}));

vi.mock("../../src/chrome/dragScope", async (importOriginal) => {
  const real = await importOriginal<typeof import("../../src/chrome/dragScope")>();
  return {
    ...real,
    useShellDragClaim: (prefixes: readonly string[], handlers: { onDragEnd: (event: unknown) => void }) => {
      if (prefixes.includes("pin:")) h.dock = handlers;
      real.useShellDragClaim(prefixes, handlers);
    },
  };
});

const { Shell } = await import("../../src/chrome/Shell");
const { StubAskTransport } = await import("../ask/stubTransport");
const { resetIdsForTest } = await import("../../src/system/desks");
const { LocalDesktopStore } = await import("../../src/system/store");
const { installSeededAccess } = await import("../seededAccess");
const { BIN_DROPPABLE_ID } = await import("../../src/apps/bin/concepts");

const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", role: "owner", roleName: "", rank: 0 };
const CONFIG = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

beforeEach(() => {
  cleanup();
  resetIdsForTest();
  h.connection = null;
  h.dock = null;
});

describe("the question over the Bin, one double-click later", () => {
  useFakeDate();

  /** The shell, and a file dropped on the Bin: the dock asks before it archives. */
  async function drop() {
    installSeededAccess("owner");
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={OWNER}
        config={CONFIG}
        ports={{ store: new LocalDesktopStore(memStorage()), disableConnection: true, askTransport: new StubAskTransport(), askVoice: null }}
      />,
    );
    const archiveArtifact = vi.fn(async (_args: { artifactId: string }) => ({}));
    h.connection = { query: { archiveArtifact } };
    expect(h.dock).not.toBeNull();
    await dropFile("a-1", "cut-03.mov");
    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    expect(within(dock).getByText('Archive "cut-03.mov"?')).toBeTruthy();
    return { archiveArtifact, dock, dropFile };
  }

  /** Another file lands on the Bin, as a finished drag hands the dock it. */
  async function dropFile(artifactId: string, name: string) {
    await act(async () => {
      h.dock!.onDragEnd({
        active: { id: `item:${artifactId}`, data: { current: { artifactId, name, folder: false, deskItemId: "" } } },
        over: { id: BIN_DROPPABLE_ID },
      });
    });
  }

  it("does not archive on a press that comes straight after the question, and does once it is read", async () => {
    const { archiveArtifact, dock } = await drop();
    betweenTheClicks();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Archive" }));
    });
    expect(archiveArtifact).not.toHaveBeenCalled();
    expect(within(dock).getByText('Archive "cut-03.mov"?')).toBeTruthy();

    await readTheBar();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Archive" }));
    });
    expect(archiveArtifact).toHaveBeenCalledTimes(1);
  });

  it("does not cancel on that press either", async () => {
    const { archiveArtifact, dock } = await drop();
    betweenTheClicks();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Cancel" }));
    });
    expect(within(dock).getByText('Archive "cut-03.mov"?')).toBeTruthy();
    await readTheBar();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Cancel" }));
    });
    expect(within(dock).queryByText('Archive "cut-03.mov"?')).toBeNull();
    expect(archiveArtifact).not.toHaveBeenCalled();
  });

  it("holds the buttons again when a second dropped file changes the question under them", async () => {
    // The first question has been read and its buttons answer. A second file is
    // dropped while it is open: the same Archive and Cancel stand where they
    // stood, and the question is about another file. A press that lands now was
    // aimed at the first question.
    const { archiveArtifact, dock, dropFile } = await drop();
    await readTheBar();
    await dropFile("a-2", "cut-04.mov");
    expect(within(dock).getByText('Archive "cut-04.mov"?')).toBeTruthy();
    expect(within(dock).queryByText('Archive "cut-03.mov"?')).toBeNull();

    betweenTheClicks();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Archive" }));
    });
    expect(archiveArtifact).not.toHaveBeenCalled();
    expect(within(dock).getByText('Archive "cut-04.mov"?')).toBeTruthy();

    // Read, it archives the file it asks about -- not the one it asked about before.
    await readTheBar();
    await act(async () => {
      fireEvent.click(within(dock).getByRole("button", { name: "Archive" }));
    });
    expect(archiveArtifact).toHaveBeenCalledTimes(1);
    expect(archiveArtifact).toHaveBeenCalledWith({ artifactId: "a-2" });
  });
});
