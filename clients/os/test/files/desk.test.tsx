import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { Shell } from "../../src/chrome/Shell";
import { StubAskTransport } from "../../src/ask/askController";
import { resetIdsForTest } from "../../src/system/desks";
import { LocalDesktopStore, DESKTOP_STORE_KEY } from "../../src/system/store";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import type { UploadProvider } from "../../src/items/upload";
import { ARTIFACT_CONCEPT } from "../../src/apps/files/concepts";
import { artifactRow, click, fakeConnection, renderFiles } from "./harness";

// The desk half of the unification (epic #4723): the popover is a live view
// the popover itself retains, a host drop on a folder lands in that folder
// exactly once, and send-to-desktop dedupes by focusing.

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};
const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", clusterRole: "owner" };

function storageWithFolderShortcut() {
  const data = new Map<string, string>();
  data.set(
    DESKTOP_STORE_KEY,
    JSON.stringify({
      version: 1,
      desks: [{ id: "desk-1", createdBy: "user" }],
      activeDeskId: "desk-1",
      surfaces: {
        "desk-1": {
          items: {
            "item-1": { kind: "folder", id: "item-1", folderId: "f-lib", name: "Client videos" },
          },
          positions: { "item-1": { col: 1, row: 1 } },
        },
      },
      dock: { pinned: [] },
      themePack: "graphite",
    }),
  );
  return { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => void data.set(k, v) };
}

function renderShellWithFolder(uploads?: UploadProvider) {
  return render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      access={OWNER}
      config={CONFIG}
      ports={{
        store: new LocalDesktopStore(storageWithFolderShortcut()),
        disableConnection: true,
        askTransport: new StubAskTransport(), askVoice: null,
        ...(uploads ? { uploads } : {}),
      }}
    />,
  );
}

beforeEach(() => {
  resetIdsForTest();
  h.connection = null;
});

describe("the desk folder popover", () => {
  it("retains its own live view while open and releases it on close", async () => {
    const connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-in", title: "inside.mp4", folderId: "f-lib" }),
        artifactRow({ id: "a-out", title: "outside.txt" }),
      ],
    });
    h.connection = connection;
    vi.useFakeTimers();
    try {
      renderShellWithFolder();
      expect(connection.subscriptions.activeCount(ARTIFACT_CONCEPT)).toBe(0);

      fireEvent.click(screen.getByRole("button", { name: "Client videos, folder" }));
      await act(async () => {
        await vi.runOnlyPendingTimersAsync();
      });
      // The popover's collection is live, folder-scoped, and its own.
      expect(connection.subscriptions.activeCount(ARTIFACT_CONCEPT)).toBe(1);
      const popover = screen.getByRole("group", { name: "Folder contents" });
      expect(within(popover).getByText("inside.mp4")).toBeTruthy();
      expect(within(popover).queryByText("outside.txt")).toBeNull();

      // Close: the subscription unmounts (after the release linger that
      // absorbs a close-and-reopen).
      fireEvent.click(screen.getByRole("button", { name: "Client videos, folder" }));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(6_000);
      });
      expect(connection.subscriptions.activeCount(ARTIFACT_CONCEPT)).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("a host file dropped on a desk folder", () => {
  it("uploads into that folder exactly once -- the desk underneath never sees the drop", async () => {
    const uploads: Array<{ name: string; folderId: string | undefined }> = [];
    const provider: UploadProvider = {
      upload: (file, opts) => {
        uploads.push({ name: file.name, folderId: opts?.folderId });
        return {
          done: Promise.resolve({ artifactId: "art-x", title: file.name, fileKind: "file", source: "uploaded" }),
          abort: () => {},
        };
      },
    };
    renderShellWithFolder(provider);
    const icon = screen.getByRole("button", { name: "Client videos, folder" });
    const target = icon.closest(".os-surface-item") as HTMLElement;
    const file = new File(["x"], "clip.mp4");
    await act(async () => {
      fireEvent.drop(target, {
        dataTransfer: { files: [file], items: [], types: ["Files"] },
      });
    });
    expect(uploads).toEqual([{ name: "clip.mp4", folderId: "f-lib" }]);
    // No desk icon was minted for it: the folder's popover is where it
    // appears, live.
    expect(screen.queryByRole("button", { name: /clip\.mp4/ })).toBeNull();
  });
});

describe("send to desktop", () => {
  it("places once and focuses on the second send instead of duplicating", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    await click(within(inspector).getByRole("button", { name: "Send to desktop" }));
    expect(within(inspector).getByText("On the desk.")).toBeTruthy();
    await click(within(inspector).getByRole("button", { name: "Send to desktop" }));
    expect(within(inspector).getByText(/Already on the desk/)).toBeTruthy();
  });
});
