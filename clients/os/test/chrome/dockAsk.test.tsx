import { act, fireEvent, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The dock's Ask carries the focused window's context (Jesús, Nov 20 2026,
// checklist item 7: "Yeah, yeah, it should."). Before Nov 2026 each window's
// title-bar Ask read what its app noted through `askContext`; that button is
// gone, so the one Ask in the dock reads the focused window's note instead.
// Real shell, real registry, real Bin; only the cluster connection is faked
// (as the Bin's own suite does) and openAsk is observed on its way through.

const h = vi.hoisted(() => ({
  connection: null as unknown,
  opened: [] as Array<{ context: string | null | undefined; label: string | undefined }>,
}));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));
vi.mock("../../src/ask/AskProvider", async (importOriginal) => {
  const real = await importOriginal<typeof import("../../src/ask/AskProvider")>();
  return {
    ...real,
    useAsk: () => {
      const value = real.useAsk();
      return {
        ...value,
        openAsk: (context?: string | null, label?: string) => {
          h.opened.push({ context, label });
          value.openAsk(context, label);
        },
      };
    },
  };
});

import { artifactRow, fakeConnection } from "../files/harness";
import { openFromLauncher, renderShell } from "../settings/shellHarness";

beforeEach(() => {
  h.opened.length = 0;
  h.connection = fakeConnection({
    archived: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true })],
    archivedFolders: [],
  });
});

describe("the dock's Ask", () => {
  it("opens with the Bin item the person selected", async () => {
    renderShell();
    openFromLauncher("Bin");
    await act(async () => {});
    fireEvent.click(await screen.findByRole("button", { name: /cut-03\.mov/ }));
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    expect(h.opened).toHaveLength(1);
    expect(h.opened[0]?.context).toMatch(/^app:bin section:items bin item:cut-03\.mov/);
    expect(h.opened[0]?.label).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Ask" })).toBeTruthy();
  });

  it("opens with the window's app and section before anything is selected", async () => {
    renderShell();
    openFromLauncher("Bin");
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    expect(h.opened[0]?.context).toMatch(/^app:bin section:items(\n|$)/);
  });

  it("opens plain when no window is focused", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    expect(h.opened).toEqual([{ context: null, label: undefined }]);
  });
});
