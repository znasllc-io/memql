import { act, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam and the download, mocked at the MODULE so the real
// LiveCollection retain/seed path runs against the harness's executeNamed fake
// (rail.test.tsx's shape), and a download never reaches a fetch.
const h = vi.hoisted(() => ({ connection: null as unknown, download: vi.fn(async () => {}) }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));
vi.mock("../../src/apps/files/actions/download", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/apps/files/actions/download")>()),
  downloadArtifact: h.download,
}));

import { setEffectiveCapabilities } from "../../src/system/roles";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";
import { artifactRow, click, emit, fakeConnection, noticeRow, renderFiles, shareRow, type FakeConnection, type FakeSeed } from "./harness";

// Wiring the Files app to the engine's file sharing (Nov 2026): the Share
// panel's three acts, where the panel is absent, and the Shared with me place.

const people = [
  { userId: "u-sam", displayName: "Sam Lee" },
  { userId: "u-dana", displayName: "Dana Cole" },
];
const brief = artifactRow({ id: "a-1", title: "brief.pdf" });
const samShare = shareRow({ id: "s-1", artifactId: "a-1", recipientUserId: "u-sam", recipientDisplayName: "Sam Lee", days: 10 });
const notices = [
  noticeRow({ id: "n-1", artifactId: "dana-budget", sharerDisplayName: "Dana Cole", title: "Budget 2027.xlsx", format: "xlsx" }),
  noticeRow({ id: "n-2", artifactId: "dana-brief", sharerDisplayName: "Dana Cole", title: "Client brief.pdf", createdAt: "2026-11-17T12:00:00Z" }),
];

function connect(seed: FakeSeed): FakeConnection {
  const connection = fakeConnection({ artifacts: [brief], shareDirectory: people, shares: [samShare], sharedWithMe: notices, ...seed });
  h.connection = connection;
  return connection;
}

const shareRegion = () => screen.queryByRole("region", { name: "Share with" });
const rail = () => screen.getByRole("navigation", { name: "File locations" });

beforeEach(() => {
  h.download.mockClear();
});
afterEach(() => {
  installSeededAccess("owner");
});

describe("the Share panel's acts", () => {
  it("shares the picked file with the chosen person for the chosen days", async () => {
    const connection = connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(within(await screen.findByRole("region", { name: "Share with" })).getByRole("button", { name: /Dana Cole/ }));
    await click(screen.getByRole("radio", { name: "30 days" }));
    await click(screen.getByRole("button", { name: "Share" }));
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientUserId: "u-dana", days: 30)',
    ]);
  });

  it("renews a share for the days it was given", async () => {
    const connection = connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(within(await screen.findByRole("region", { name: "Share with" })).getByRole("button", { name: /Sam Lee/ }));
    await click(screen.getByRole("button", { name: "Renew" }));
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientUserId: "u-sam", days: 10)',
    ]);
  });

  it("stops a share by the share row's id", async () => {
    const connection = connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(within(await screen.findByRole("region", { name: "Share with" })).getByRole("button", { name: /Sam Lee/ }));
    await click(screen.getByRole("button", { name: "Stop sharing" }));
    expect(connection.callsNamed("libraryShareStop")).toEqual(['builtin libraryShareStop(shareId: "s-1")']);
  });
});

describe("where the Share panel is absent", () => {
  it("in the Bin, for an archived file", async () => {
    connect({ artifacts: [artifactRow({ id: "a-9", title: "old.pdf", archived: true })] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Bin" }));
    await click(await screen.findByRole("button", { name: /old\.pdf/ }));
    expect(screen.getByRole("group", { name: "Selected: old.pdf" })).toBeTruthy();
    expect(shareRegion()).toBeNull();
  });

  it("in Shared with me, for a file somebody shared with you", async () => {
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    expect(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).toBeTruthy();
    expect(shareRegion()).toBeNull();
  });

  it("without the share part, and the directory is never read", async () => {
    setEffectiveCapabilities(seededAccessWithout("owner", "app:files/share"));
    const connection = connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    expect(screen.getByRole("group", { name: "Selected: brief.pdf" })).toBeTruthy();
    expect(shareRegion()).toBeNull();
    expect(connection.callsNamed("libraryShareDirectory")).toEqual([]);
  });
});

describe("Shared with me", () => {
  it("lists what was shared with you and from whom, and never reads your Library for it", async () => {
    const connection = connect({});
    await renderFiles();
    const browsed = connection.callsNamed("libraryArtifactsBrowse").length;
    const faceted = connection.callsNamed("libraryBrowseFacets").length;

    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    expect(screen.getByRole("heading", { name: "Shared with me" })).toBeTruthy();
    const budget = await screen.findByRole("button", { name: /Budget 2027\.xlsx/ });
    expect(budget.textContent).toContain("from Dana Cole");
    expect(screen.getByRole("button", { name: /Client brief\.pdf/ }).textContent).toContain("from Dana Cole");
    expect(screen.queryByRole("button", { name: /^brief\.pdf/ })).toBeNull();
    expect(connection.callsNamed("librarySharedWithMe")).toHaveLength(1);
    // A change to your own Library is not this place's news: nothing here
    // listens for it, so nothing is read again.
    await emit(connection, "v1:library:artifact", artifactRow({ id: "a-2", title: "new.pdf" }), "NODE_CREATED");
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(browsed);
    expect(connection.callsNamed("libraryBrowseFacets")).toHaveLength(faceted);
  });

  it("says so when nothing has been shared with you", async () => {
    connect({ sharedWithMe: [] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    expect(await screen.findByText("Nothing has been shared with you.")).toBeTruthy();
  });

  it("shows a file the moment it is shared with you", async () => {
    const connection = connect({ sharedWithMe: [] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await screen.findByText("Nothing has been shared with you.");
    await emit(connection, "v1:library:shareNotice", noticeRow({ id: "n-3", artifactId: "sam-plan", sharerDisplayName: "Sam Lee", title: "Plan.md" }), "NODE_CREATED");
    expect((await screen.findByRole("button", { name: /Plan\.md/ })).textContent).toContain("from Sam Lee");
  });

  it("offers exactly Download and Save to my Library, on the Head and on a right-click", async () => {
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    const tile = await screen.findByRole("button", { name: /Budget 2027\.xlsx/ });
    await click(tile);
    const icons = within(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).getAllByRole("button");
    expect(icons.map((icon) => icon.getAttribute("aria-label"))).toEqual(["Download", "Save to my Library"]);

    await act(async () => { fireEvent.contextMenu(tile); });
    const menu = screen.getByRole("menu", { name: "Shared file" });
    expect(within(menu).getAllByRole("menuitem").map((item) => item.textContent)).toEqual(["Download", "Save to my Library"]);
  });

  it("Save to my Library keeps a copy of your own, and says it did", async () => {
    const connection = connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    await click(within(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).getByRole("button", { name: "Save to my Library" }));
    expect(connection.callsNamed("librarySaveSharedCopy")).toEqual(['builtin librarySaveSharedCopy(artifactId: "dana-budget")']);
    expect(screen.getByText('Saved "Budget 2027.xlsx" to your Library.')).toBeTruthy();
  });

  // José, Nov 19: twenty quick clicks on Save made eighteen copies; "it
  // should only count as one click". The Head's icon takes no second press
  // until the save has answered, and dims meanwhile; once it has, a click is a
  // new save.
  it("Save to my Library counts a burst of clicks as one, then saves again once it has finished", async () => {
    let answer = () => {};
    const connection = connect({ hold: { librarySaveSharedCopy: new Promise<void>((resolve) => { answer = resolve; }) } });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    const save = within(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).getByRole("button", { name: "Save to my Library" });
    await act(async () => { for (let i = 0; i < 20; i++) fireEvent.click(save); });
    expect(connection.callsNamed("librarySaveSharedCopy")).toHaveLength(1);
    expect(save.getAttribute("aria-busy")).toBe("true");
    expect(save.hasAttribute("data-busy")).toBe(true);

    await act(async () => { answer(); });
    expect(screen.getByText('Saved "Budget 2027.xlsx" to your Library.')).toBeTruthy();
    expect(save.hasAttribute("data-busy")).toBe(false);
    await click(save);
    expect(connection.callsNamed("librarySaveSharedCopy")).toHaveLength(2);
  });

  // Jesús, Nov 20: Download behaves like Save -- a burst is one download, the
  // icon dims while it is prepared, and a line says it landed.
  it("Download counts a burst of clicks as one, dims meanwhile, then says it landed", async () => {
    let answer = () => {};
    h.download.mockImplementationOnce(() => new Promise<void>((resolve) => { answer = resolve; }));
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    const download = within(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).getByRole("button", { name: "Download" });
    await act(async () => { for (let i = 0; i < 20; i++) fireEvent.click(download); });
    expect(h.download).toHaveBeenCalledTimes(1);
    expect(download.getAttribute("aria-busy")).toBe("true");

    await act(async () => { answer(); });
    expect(screen.getByText('Downloaded "Budget 2027.xlsx".')).toBeTruthy();
    expect(download.hasAttribute("data-busy")).toBe(false);
    await click(download);
    expect(h.download).toHaveBeenCalledTimes(2);
  });

  it("Download on your own file's Head counts a burst as one too", async () => {
    let answer = () => {};
    h.download.mockImplementationOnce(() => new Promise<void>((resolve) => { answer = resolve; }));
    connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const download = within(screen.getByRole("group", { name: "Selected: brief.pdf" })).getByRole("button", { name: "Download" });
    await act(async () => { for (let i = 0; i < 20; i++) fireEvent.click(download); });
    expect(h.download).toHaveBeenCalledTimes(1);
    expect(download.getAttribute("aria-busy")).toBe("true");
    await act(async () => { answer(); });
    expect(screen.getByText('Downloaded "brief.pdf".')).toBeTruthy();
  });

  it("says in place when the engine refuses the copy", async () => {
    connect({ refuse: { librarySaveSharedCopy: "This share has ended." } });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    await click(within(screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" })).getByRole("button", { name: "Save to my Library" }));
    expect(screen.getByText("The copy was not saved.")).toBeTruthy();
    expect(screen.getByText(/This share has ended\./)).toBeTruthy();
  });

  it("downloads the shared file on a double-click", async () => {
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    const tile = await screen.findByRole("button", { name: /Budget 2027\.xlsx/ });
    await act(async () => { fireEvent.doubleClick(tile); });
    expect(h.download).toHaveBeenCalledWith(expect.objectContaining({ artifactId: "dana-budget", name: "Budget 2027.xlsx" }));
  });
});
