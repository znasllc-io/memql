import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { Shell } from "../../src/chrome/Shell";
import { StubAskTransport } from "../ask/stubTransport";
import { resetIdsForTest } from "../../src/system/desks";
import { LocalDesktopStore } from "../../src/system/store";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import type { UploadProvider, UploadResult } from "../../src/items/upload";

// The desk's upload states, end to end through the shell (Nov 19 item 10):
// a file dropped from the computer is dim and has no dot while it uploads,
// can still be picked, and opening it does nothing: nothing is queued, so
// nothing opens later outside a click (Safari's pop-up blocker refuses that,
// Jesús, Nov 20 evening). Once it lands it says Ready and opens when opened.
// The rules themselves are unit-tested in deskFileState.test.tsx.

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};
const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", role: "owner", roleName: "", rank: 0 };

// Opening a file in the browser editor (the default) first reserves a tab:
// a window.open call is the evidence that an open ran.
let opened: ReturnType<typeof vi.spyOn>;

function heldUploads() {
  const held: Array<{ resolve: (r: UploadResult) => void; reject: (e: Error) => void }> = [];
  const provider: UploadProvider = {
    upload: () => {
      let entry!: (typeof held)[number];
      const done = new Promise<UploadResult>((resolve, reject) => (entry = { resolve, reject }));
      held.push(entry);
      return { done, abort: () => entry.reject(new Error("upload aborted")) };
    },
  };
  return {
    provider,
    land: (n: number) => act(async () => held[n]!.resolve({ artifactId: "art-1", title: "report.pdf", fileKind: "file", source: "uploaded" })),
    fail: (n: number, why: string) => act(async () => held[n]!.reject(new Error(why))),
  };
}

function storage() {
  const data = new Map<string, string>();
  return { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => void data.set(k, v) };
}

async function dropOnDesk(uploads: UploadProvider) {
  render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      access={OWNER}
      config={CONFIG}
      ports={{
        store: new LocalDesktopStore(storage()),
        disableConnection: true,
        askTransport: new StubAskTransport(), askVoice: null,
        uploads,
      }}
    />,
  );
  const plate = document.querySelector("[data-os-desk]") as HTMLElement;
  await act(async () => {
    fireEvent.drop(plate, { dataTransfer: { files: [new File(["x"], "report.pdf")], items: [], types: ["Files"] } });
  });
  const button = screen.getByRole("button", { name: "report.pdf -- open in VS Code" });
  return { button, icon: button.closest(".os-file") as HTMLElement };
}

beforeEach(() => {
  resetIdsForTest();
  h.connection = null;
  opened = vi.spyOn(window, "open").mockReturnValue(null);
});
afterEach(() => opened.mockRestore());

describe("a file uploading onto the desk", () => {
  it("is dim with no dot, can be picked, does nothing when opened, and opens once it has landed", async () => {
    const uploads = heldUploads();
    const { button, icon } = await dropOnDesk(uploads.provider);
    expect(icon.hasAttribute("data-dim")).toBe(true);
    expect(icon.querySelector(".os-dot")).toBeNull();

    fireEvent.click(button);
    expect(icon.hasAttribute("data-selected")).toBe(true);

    fireEvent.doubleClick(button);
    fireEvent.keyDown(button, { key: "Enter" });
    expect(screen.queryByText("Opening when done")).toBeNull();
    expect(opened).not.toHaveBeenCalled();
    fireEvent.contextMenu(button);
    expect((screen.getByRole("menuitem", { name: "Open in VS Code" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });

    await uploads.land(0);
    expect(icon.hasAttribute("data-dim")).toBe(false);
    expect(screen.getByText("Ready")).toBeTruthy();
    expect(opened).not.toHaveBeenCalled();

    fireEvent.doubleClick(button);
    expect(opened).toHaveBeenCalledOnce();
  });

  it("offers the retry line when the upload fails; the retry that lands says Ready and opens nothing", async () => {
    const uploads = heldUploads();
    const { button } = await dropOnDesk(uploads.provider);
    fireEvent.keyDown(button, { key: "Enter" });

    await uploads.fail(0, "The cluster refused the upload.");
    expect(screen.getByRole("alert").textContent).toBe("The cluster refused the upload.");

    fireEvent.click(screen.getByRole("button", { name: "Upload failed -- retry" }));
    await uploads.land(1);
    expect(screen.getByText("Ready")).toBeTruthy();
    expect(opened).not.toHaveBeenCalled();
  });
});
