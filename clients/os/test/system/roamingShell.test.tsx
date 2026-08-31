import { act, render, screen, fireEvent, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { StubAskTransport } from "../../src/ask/askController";
import { Shell } from "../../src/chrome/Shell";
import { adoptDocument, seedDocument, type OsState } from "../../src/chrome/state";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import {
  addDesk,
  initialShell,
  nextIdAvoiding,
  openApp,
  resetIdsForTest,
} from "../../src/system/desks";
import {
  documentFromState,
  LocalDesktopStore,
  type DesktopDocument,
  type DesktopStore,
  type DesktopStoreEvent,
} from "../../src/system/store";

// The shell's half of the roaming desktop (epic memql#4746): taking on a
// document that arrived from another machine, minting ids that cannot
// collide with one, and reporting it once.

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};

const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", clusterRole: "owner" };

const GRID = { cols: 10, rows: 6 };

function remoteDocument(overrides: Partial<DesktopDocument> = {}): DesktopDocument {
  return {
    version: 1,
    // Desk ids from ANOTHER machine's session counter, deliberately outside
    // the range this test process mints, so "the local desk is gone" is
    // actually the case under test rather than an accidental collision.
    desks: [
      { id: "desk-70", createdBy: "user" },
      { id: "desk-71", createdBy: "user" },
    ],
    activeDeskId: "desk-71",
    surfaces: {
      "desk-70": {
        items: {
          "item-1": { kind: "folder", id: "item-1", name: "Taxes", children: [] },
        },
        positions: { "item-1": { col: 2, row: 2 } },
      },
      "desk-71": { items: {}, positions: {} },
    },
    dock: { pinned: ["settings", "fleet"] },
    themePack: "midnight",
    ...overrides,
  };
}

beforeEach(() => {
  resetIdsForTest();
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});

describe("adoptDocument", () => {
  function runningState(): OsState {
    const seeded = seedDocument(OS_REGISTRY, GRID);
    const { state: shell } = openApp(seeded.shell, "settings");
    return { ...seeded, shell };
  }

  it("keeps the windows the person has open", () => {
    // ADOPTING IS NOT LOADING. stateFromDocument builds a shell with no
    // windows, and using it here would close everything open -- in response
    // to something that happened on a different computer.
    const before = runningState();
    const openIds = Object.keys(before.shell.windows);
    expect(openIds).toHaveLength(1);

    const after = adoptDocument(before, remoteDocument({
      desks: [{ id: before.shell.activeDeskId, createdBy: "user" }],
      activeDeskId: before.shell.activeDeskId,
      surfaces: { [before.shell.activeDeskId]: { items: {}, positions: {} } },
    }));

    expect(Object.keys(after.shell.windows)).toEqual(openIds);
    expect(after.shell.desks[0]?.windows).toEqual(openIds);
    expect(after.shell.focusedWindowId).toBe(openIds[0]);
  });

  it("drops a window whose desk the arriving document no longer has", () => {
    const before = runningState();
    const after = adoptDocument(before, remoteDocument());
    expect(after.shell.windows).toEqual({});
    expect(after.shell.focusedWindowId).toBeNull();
  });

  it("keeps the desk on screen when the document still has it", () => {
    const before = runningState();
    const local = before.shell.activeDeskId;
    const after = adoptDocument(before, remoteDocument({
      desks: [{ id: local, createdBy: "user" }, { id: "desk-71", createdBy: "user" }],
      activeDeskId: "desk-71",
      surfaces: { [local]: { items: {}, positions: {} }, "desk-71": { items: {}, positions: {} } },
    }));
    expect(after.shell.activeDeskId).toBe(local);
  });

  it("takes the document's desk when the local one is gone -- the cold sign-in", () => {
    const before = runningState();
    const after = adoptDocument(before, remoteDocument());
    expect(after.shell.activeDeskId).toBe("desk-71");
  });

  it("takes the arriving surfaces, pins and theme whole", () => {
    const after = adoptDocument(runningState(), remoteDocument());
    expect(after.surfaces["desk-70"]?.items["item-1"]).toMatchObject({ name: "Taxes" });
    expect(after.dock.pinned).toEqual(["settings", "fleet"]);
    expect(after.themePack).toBe("midnight");
  });
});

describe("ids minted against the document", () => {
  it("nextIdAvoiding skips ids the desktop already holds", () => {
    resetIdsForTest();
    expect(nextIdAvoiding("item", new Set(["item-1", "item-2"]))).toBe("item-3");
  });

  it("a desk added after a reload does not duplicate a restored desk id", () => {
    // The counter restarts at 0 on every page load; the document does not.
    resetIdsForTest();
    const restored = { ...initialShell(), desks: [{ id: "desk-1", createdBy: "user" as const, windows: [] }] };
    resetIdsForTest(); // the reload
    const after = addDesk(restored);
    const ids = after.desks.map((d) => d.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("folders created after a reload do not replace the seeded widget", () => {
    // THE BUG, exactly as a person meets it. `nextId` is ONE counter shared
    // by every prefix, so a fresh session seeds `desk-1` then `item-2`; the
    // counter restarts at 0 on reload and hands out `item-1`, then `item-2`
    // -- and items live in a map keyed by id, so the second folder took the
    // widget's key and the widget was gone, with no error anywhere.
    //
    // Two folders, not one, and that IS the bug rather than a workaround for
    // it: the collision lands on whichever creation reaches the id the
    // document already holds, and with the shared counter that is the second.
    const storage = new Map<string, string>();
    const store = new LocalDesktopStore({
      getItem: (k) => storage.get(k) ?? null,
      setItem: (k, v) => void storage.set(k, v),
    });

    const first = render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={OWNER}
        config={CONFIG}
        ports={{ store, disableConnection: true, askTransport: new StubAskTransport() }}
      />,
    );
    const seeded = store.load();
    const widgetIds = Object.keys(seeded?.surfaces[seeded.activeDeskId]?.items ?? {});
    expect(widgetIds).toHaveLength(1);
    first.unmount();

    resetIdsForTest(); // the reload
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={OWNER}
        config={CONFIG}
        ports={{ store, disableConnection: true, askTransport: new StubAskTransport() }}
      />,
    );
    // Through the real surface: right-click the active desk plate, take
    // "New folder". No conditionals -- if this path stops being reachable
    // the test must fail rather than quietly assert nothing.
    const newFolder = () => {
      const plate = document.querySelector(".os-plate");
      expect(plate).not.toBeNull();
      fireEvent.contextMenu(plate as Element, { clientX: 400, clientY: 300 });
      const menu = screen.getByRole("menu");
      fireEvent.click(within(menu).getByRole("menuitem", { name: "New folder" }));
    };
    newFolder();
    newFolder();

    const after = store.load();
    const items = after?.surfaces[after.activeDeskId]?.items ?? {};
    // Two things were ADDED and nothing was taken away. Before the fix the
    // count came back one short: the second folder landed on the widget's key.
    expect(Object.keys(items)).toHaveLength(widgetIds.length + 2);
    for (const id of widgetIds) expect(items[id]).toBeDefined();
  });
});

describe("the roaming report", () => {
  /** A store whose remote half the test drives by hand. */
  class ScriptedStore implements DesktopStore {
    private listener: ((event: DesktopStoreEvent) => void) | null = null;
    constructor(private readonly local = new LocalDesktopStore()) {}
    load() {
      return this.local.load();
    }
    save(d: DesktopDocument) {
      this.local.save(d);
    }
    subscribe(listener: (event: DesktopStoreEvent) => void) {
      this.listener = listener;
      return () => {
        this.listener = null;
      };
    }
    emit(event: DesktopStoreEvent) {
      act(() => this.listener?.(event));
    }
  }

  function renderWith(store: DesktopStore) {
    return render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={OWNER}
        config={CONFIG}
        ports={{ store, disableConnection: true, askTransport: new StubAskTransport() }}
      />,
    );
  }

  it("says nothing when the desktop merely resolves", () => {
    const store = new ScriptedStore();
    renderWith(store);
    store.emit({ kind: "document", document: remoteDocument(), origin: "hydrate" });
    expect(screen.queryByText(/another device/i)).toBeNull();
  });

  it("reports a document that arrived from another machine, and applies it", () => {
    const store = new ScriptedStore();
    renderWith(store);
    store.emit({ kind: "document", document: remoteDocument(), origin: "remote" });
    expect(screen.getByText(/Desktop updated on another device/i)).toBeTruthy();
    // Applied, not merely announced: the arriving theme is on the document.
    expect(document.documentElement.getAttribute("data-os-theme")).toBe("midnight");
  });

  it("clears itself", () => {
    vi.useFakeTimers();
    try {
      const store = new ScriptedStore();
      renderWith(store);
      store.emit({ kind: "document", document: remoteDocument(), origin: "remote" });
      expect(screen.queryByText(/another device/i)).not.toBeNull();
      act(() => {
        vi.advanceTimersByTime(6_500);
      });
      expect(screen.queryByText(/another device/i)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the stale report up, because the condition stays true", () => {
    vi.useFakeTimers();
    try {
      const store = new ScriptedStore();
      renderWith(store);
      store.emit({ kind: "stale" });
      act(() => {
        vi.advanceTimersByTime(60_000);
      });
      expect(screen.getByText(/out of date/i)).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it("the live region exists before there is anything to announce", () => {
    // A role="status" element that appears at the same moment its text does
    // is not reliably announced -- there was no region for the change to
    // happen inside.
    const store = new ScriptedStore();
    const view = renderWith(store);
    expect(view.container.querySelector(".os-roam[role='status']")).not.toBeNull();
  });
});

describe("documentFromState round trip", () => {
  it("an adopted document rebuilds to the same shared content", () => {
    // The store's echo test compares documents, so a shell that reshaped what
    // it adopted would write it straight back on the next render.
    const adopted = adoptDocument(seedDocument(OS_REGISTRY, GRID), remoteDocument());
    const rebuilt = documentFromState(adopted.shell, adopted.surfaces, adopted.dock, adopted.themePack);
    const { activeDeskId: _a, ...want } = remoteDocument();
    const { activeDeskId: _b, ...got } = rebuilt;
    expect(got).toEqual(want);
  });
});
