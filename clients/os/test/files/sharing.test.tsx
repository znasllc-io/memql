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

import { MIN_HOLD_MS } from "../../src/kit/oneAtATime";
import { setEffectiveCapabilities } from "../../src/system/roles";
import { chooseOption, openSelect } from "../selectControl";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";
import { timeLeftLabel } from "../../src/apps/files/useSharing";
import { intersect } from "../intersectionObserver";
import { artifactRow, click, emit, fakeConnection, fileRow, noticeRow, renderFiles, shareRow, type FakeConnection, type FakeSeed } from "./harness";

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

const rail = () => screen.getByRole("navigation", { name: "File locations" });

beforeEach(() => {
  h.download.mockClear();
});
afterEach(() => {
  installSeededAccess("owner");
});

// The share view (Nov 20 2026): pressing Share puts it in place of the tiles.
async function openShare(file = /brief\.pdf/) {
  await click(screen.getByRole("button", { name: file }));
  const icons = screen.getByRole("group", { name: /^Selected: / });
  await click(within(icons).getByRole("button", { name: "Share" }));
  return screen.getByRole("region", { name: /^Share / });
}
const expires = () => screen.getByLabelText("How long the file is shared", { selector: "button" });
const searchBox = () => screen.getByRole("combobox", { name: "Search users and groups" });
async function pick(name: string) {
  fireEvent.focus(searchBox());
  await click(await screen.findByRole("option", { name }));
}
const addButton = () => screen.getByRole("button", { name: "Add selected users and groups" });

/** Waits out the half second a press holds its button (kit/oneAtATime). */
const holdOver = () => act(async () => { await new Promise((resolve) => setTimeout(resolve, MIN_HOLD_MS)); });

describe("the share view", () => {
  it("takes the place of the tiles, the rail beside it, and the back arrow returns", async () => {
    connect({});
    await renderFiles();
    const view = await openShare();
    expect(within(view).getByRole("heading", { name: "brief.pdf" })).toBeTruthy();
    // Not a modal: the rail is still there and still works.
    expect(rail()).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    // The tiles are out of sight while it stands in their place.
    expect(screen.getByRole("button", { name: /brief\.pdf/, hidden: true }).closest("[hidden]")).not.toBeNull();
    await click(screen.getByRole("button", { name: "Back to Library" }));
    expect(screen.queryByRole("region", { name: /^Share / })).toBeNull();
    expect(screen.getByRole("button", { name: /brief\.pdf/ }).closest("[hidden]")).toBeNull();
  });

  // Option 2A (Nov 22): no "Share · icon · name" header; the file's glyph on a
  // small tile, its name as the heading, and kind · size · where under it.
  it("heads the page with the file: its glyph, its name, and kind · size · where", async () => {
    connect({
      artifacts: [artifactRow({ id: "a-1", title: "logo.png", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", name: "logo.png", mimeType: "image/png", size: 1363149 })],
    });
    await renderFiles();
    const view = await openShare(/logo\.png/);
    expect(within(view).queryByRole("heading", { name: "Share" })).toBeNull();
    expect(within(view).getByRole("heading", { name: "logo.png" })).toBeTruthy();
    expect(await within(view).findByText("Image · 1.3 MiB · in Library")).toBeTruthy();
    expect(view.querySelector(".os-files-share-preview .os-files-sheet b")?.textContent).toBe("PNG");
  });

  it("is a verb on the right-click menu too", async () => {
    connect({});
    await renderFiles();
    await act(async () => { fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ })); });
    await click(within(screen.getByRole("menu", { name: "File" })).getByRole("menuitem", { name: "Share" }));
    expect(screen.getByRole("region", { name: "Share brief.pdf" })).toBeTruthy();
  });

  it("expires in 7 days to start, saying the end in words", async () => {
    connect({});
    await renderFiles();
    await openShare();
    expect(expires().textContent).toContain("7 days");
    const end = new Date(Date.now() + 7 * 86_400_000).toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    expect(screen.getByText(`until ${end}`)).toBeTruthy();
    expect(within(openSelect(expires())).getAllByRole("option").map((o) => o.textContent)).toEqual(["1 day", "7 days", "30 days", "Pick a date…"]);
  });

  it("shares with the people picked, for the length chosen", async () => {
    const connection = connect({});
    await renderFiles();
    await openShare();
    chooseOption(expires(), "30 days");
    await pick("Dana Cole");
    await click(addButton());
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientUserId: "u-dana", days: 30)',
    ]);
  });

  it("shows who is picked as chips inside the search box, not as 'N selected' (Nov 20 call)", async () => {
    const connection = connect({ shareDirectory: [...people, { userId: "u-ada", displayName: "Ada Park" }, { groupId: "g-design", displayName: "Design team", members: 4 }] });
    await renderFiles();
    await openShare();
    const box = () => searchBox().closest(".os-card-search-input")!;
    expect(box().hasAttribute("data-picker-field")).toBe(false);
    await pick("Dana Cole");
    await click(screen.getByRole("option", { name: "Design team" }));
    // Both stand in the box, in the order picked, each with its own x.
    expect([...box().querySelectorAll(".os-chip")].map((chip) => chip.textContent)).toEqual(["Dana Cole", "Design team"]);
    expect(box().hasAttribute("data-picker-field")).toBe(true);
    // The count is for screen readers only now.
    expect(screen.getByText("2 selected").className).toBe("os-sr-only");
    // The x un-picks that one; Backspace in the empty box takes off the last.
    await click(within(box() as HTMLElement).getByRole("button", { name: "Remove Dana Cole" }));
    expect([...box().querySelectorAll(".os-chip")].map((chip) => chip.textContent)).toEqual(["Design team"]);
    await pick("Ada Park");
    await act(async () => { fireEvent.keyDown(searchBox(), { key: "Backspace" }); });
    expect([...box().querySelectorAll(".os-chip")].map((chip) => chip.textContent)).toEqual(["Design team"]);
    // What stays picked is what is shared.
    await click(addButton());
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientGroupId: "g-design", days: 7)',
    ]);
    expect(box().querySelectorAll(".os-chip")).toHaveLength(0);
  });

  it("shows the people it is shared with two a row, and more as the list end comes into view (Nov 20 call)", async () => {
    const many = Array.from({ length: 8 }, (_, i) =>
      shareRow({ id: `s-${i}`, artifactId: "a-1", recipientUserId: `u-${i}`, recipientDisplayName: `Person ${i}`, days: 7 }));
    connect({ shares: many });
    await renderFiles();
    await openShare();
    const list = await screen.findByRole("list", { name: "Shared users and groups" });
    // No sideways pager: a list, three rows of two to start.
    expect(screen.queryByRole("region", { name: "Sharing access cards" })).toBeNull();
    expect(within(list).getAllByRole("listitem")).toHaveLength(6);
    const sentinel = document.querySelector(".os-files-share .os-next-page-sentinel");
    expect(sentinel).toBeTruthy();
    await act(async () => intersect(sentinel!));
    expect(within(screen.getByRole("list", { name: "Shared users and groups" })).getAllByRole("listitem")).toHaveLength(8);
  });

  it("offers groups beside people, and shares with a whole group", async () => {
    const connection = connect({ shareDirectory: [...people, { groupId: "g-design", displayName: "Design team", members: 4 }] });
    await renderFiles();
    await openShare();
    fireEvent.focus(searchBox());
    const option = await screen.findByRole("option", { name: "Design team" });
    expect(option.textContent).toContain("Group of 4 people");
    // Groups narrows the list to groups.
    chooseOption(screen.getByLabelText("Search users and groups by", { selector: "button" }), "Groups");
    fireEvent.focus(searchBox());
    expect(screen.getAllByRole("option").map((o) => o.getAttribute("aria-label"))).toEqual(["Design team"]);
    await click(screen.getByRole("option", { name: "Design team" }));
    await click(addButton());
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientGroupId: "g-design", days: 7)',
    ]);
  });

  it("does not offer who already has the file, and removes sharing with them", async () => {
    const connection = connect({});
    await renderFiles();
    await openShare();
    fireEvent.focus(searchBox());
    expect(screen.queryByRole("option", { name: "Sam Lee" })).toBeNull();
    expect(screen.getByRole("option", { name: "Dana Cole" })).toBeTruthy();
    const actions = screen.getByRole("button", { name: "Actions for Sam Lee" });
    expect(actions.closest(".os-editable-card")?.textContent).toMatch(/User · \d+ days left/);
    // The card's bin moved into its "⋯" menu (Nov 20, draft p18 Option C).
    expect(screen.queryByRole("button", { name: /^Remove sharing/ })).toBeNull();
    await click(actions);
    const menu = screen.getByRole("menu", { name: "Actions for Sam Lee" });
    expect(within(menu).getAllByRole("menuitem").map((item) => item.textContent)).toEqual(["Renew for 7 days", "Remove sharing"]);
    // A hairline sets "Remove sharing" apart, in the danger colour.
    expect(menu.querySelector('[role="separator"] + [role="menuitem"]')?.textContent).toBe("Remove sharing");
    expect(within(menu).getByRole("menuitem", { name: "Remove sharing" }).getAttribute("data-tone")).toBe("danger");
    await click(within(menu).getByRole("menuitem", { name: "Remove sharing" }));
    expect(connection.callsNamed("libraryShareStop")).toEqual(['builtin libraryShareStop(shareId: "s-1")']);
    expect(connection.callsNamed("libraryShareFile")).toEqual([]);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("renews a person's share for the length chosen in Expires, from the card's menu", async () => {
    const connection = connect({});
    await renderFiles();
    await openShare();
    chooseOption(expires(), "30 days");
    await click(screen.getByRole("button", { name: "Actions for Sam Lee" }));
    await click(screen.getByRole("menuitem", { name: "Renew for 30 days" }));
    // A re-share: the engine renews the share in place.
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      'builtin libraryShareFile(artifactId: "a-1", recipientUserId: "u-sam", days: 30)',
    ]);
    expect(connection.callsNamed("libraryShareStop")).toEqual([]);
  });

  it("renews a group's share until a picked day", async () => {
    const groupShare = shareRow({ id: "s-g", artifactId: "a-1", recipientUserId: "", recipientGroupId: "g-design", recipientDisplayName: "Design team" });
    const connection = connect({ shares: [groupShare] });
    await renderFiles();
    await openShare();
    chooseOption(expires(), "Pick a date…");
    const calendar = screen.getByRole("dialog", { name: "Pick the day the share ends" });
    const today = new Date();
    const inTwo = new Date(today.getFullYear(), today.getMonth(), today.getDate() + 2);
    if (inTwo.getMonth() !== today.getMonth()) await click(within(calendar).getByRole("button", { name: "Next month" }));
    await click(within(calendar).getByRole("button", { name: inTwo.toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" }) }));
    const words = inTwo.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    await click(screen.getByRole("button", { name: "Actions for Design team" }));
    await click(screen.getByRole("menuitem", { name: `Renew until ${words}` }));
    const end = new Date(inTwo.getFullYear(), inTwo.getMonth(), inTwo.getDate(), 23, 59, 59, 999).toISOString();
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      `builtin libraryShareFile(artifactId: "a-1", recipientGroupId: "g-design", until: "${end}")`,
    ]);
  });

  it("says in place when the engine refuses a renewal", async () => {
    connect({ refuse: { libraryShareFile: "this share has ended" } });
    await renderFiles();
    await openShare();
    await click(screen.getByRole("button", { name: "Actions for Sam Lee" }));
    await click(screen.getByRole("menuitem", { name: "Renew for 7 days" }));
    expect(screen.getByText("Sharing could not be updated.")).toBeTruthy();
    expect(screen.getByText("this share has ended")).toBeTruthy();
  });

  it("names a group share with its group", async () => {
    const groupShare = shareRow({ id: "s-g", artifactId: "a-1", recipientUserId: "", recipientGroupId: "g-design", recipientDisplayName: "Design team" });
    connect({ shares: [groupShare] });
    await renderFiles();
    await openShare();
    const remove = await screen.findByRole("button", { name: "Actions for Design team" });
    expect(remove.closest(".os-editable-card")?.textContent).toMatch(/Group · \d+ days left/);
  });

  it("says in place when the engine refuses", async () => {
    connect({ refuse: { libraryShareFile: "choose someone from the people and groups offered to share with" } });
    await renderFiles();
    await openShare();
    await pick("Dana Cole");
    await click(addButton());
    expect(screen.getByText("Sharing could not be updated.")).toBeTruthy();
    expect(screen.getByText(/people and groups offered/)).toBeTruthy();
  });

  it("picks an end date on the OS's own calendar, past days disabled", async () => {
    const connection = connect({});
    await renderFiles();
    await openShare();
    chooseOption(expires(), "Pick a date…");
    const calendar = screen.getByRole("dialog", { name: "Pick the day the share ends" });
    const today = new Date();
    const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
    const inTwo = new Date(today.getFullYear(), today.getMonth(), today.getDate() + 2);
    const label = (d: Date) => d.toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" });
    // Yesterday is drawn (or a month away), and never pickable.
    const past = within(calendar).queryByRole("button", { name: label(yesterday) });
    if (past) expect((past as HTMLButtonElement).disabled).toBe(true);
    if (inTwo.getMonth() !== today.getMonth()) await click(within(calendar).getByRole("button", { name: "Next month" }));
    await click(within(calendar).getByRole("button", { name: label(inTwo) }));
    expect(screen.queryByRole("dialog", { name: "Pick the day the share ends" })).toBeNull();
    const words = inTwo.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    expect(screen.getByText(`until ${words}`)).toBeTruthy();
    expect(expires().textContent).toContain(words);
    await pick("Dana Cole");
    await click(addButton());
    const end = new Date(inTwo.getFullYear(), inTwo.getMonth(), inTwo.getDate(), 23, 59, 59, 999).toISOString();
    expect(connection.callsNamed("libraryShareFile")).toEqual([
      `builtin libraryShareFile(artifactId: "a-1", recipientUserId: "u-dana", until: "${end}")`,
    ]);
  });

  it("goes back to the length it replaced when the calendar is shut with no day picked", async () => {
    connect({});
    await renderFiles();
    await openShare();
    chooseOption(expires(), "Pick a date…");
    expect(screen.getByRole("dialog", { name: "Pick the day the share ends" })).toBeTruthy();
    await act(async () => { fireEvent.mouseDown(document.body); });
    expect(screen.queryByRole("dialog", { name: "Pick the day the share ends" })).toBeNull();
    expect(expires().textContent).toContain("7 days");
  });
});

describe("where Share is absent", () => {
  it("in the Bin, for an archived file", async () => {
    connect({ artifacts: [artifactRow({ id: "a-9", title: "old.pdf", archived: true })] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Bin" }));
    await click(await screen.findByRole("button", { name: /old\.pdf/ }));
    const icons = screen.getByRole("group", { name: "Selected: old.pdf" });
    expect(within(icons).queryByRole("button", { name: "Share" })).toBeNull();
  });

  it("in Shared with me, for a file somebody shared with you", async () => {
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    await click(await screen.findByRole("button", { name: /Budget 2027\.xlsx/ }));
    const icons = screen.getByRole("group", { name: "Selected: Budget 2027.xlsx" });
    expect(within(icons).queryByRole("button", { name: "Share" })).toBeNull();
  });

  it("for a file that is not yours", async () => {
    connect({ artifacts: [artifactRow({ id: "a-x", title: "theirs.pdf", ownerUserId: "u-someone-else" })] });
    await renderFiles();
    await click(await screen.findByRole("button", { name: /theirs\.pdf/ }));
    const icons = screen.getByRole("group", { name: "Selected: theirs.pdf" });
    expect(within(icons).queryByRole("button", { name: "Share" })).toBeNull();
  });

  it("without the share part, and the directory is never read", async () => {
    setEffectiveCapabilities(seededAccessWithout("owner", "app:files/share"));
    const connection = connect({});
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const icons = screen.getByRole("group", { name: "Selected: brief.pdf" });
    expect(within(icons).queryByRole("button", { name: "Share" })).toBeNull();
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
    // A fast save still holds the icon for its half second (Jesús, Nov 20).
    expect(save.hasAttribute("data-busy")).toBe(true);
    await holdOver();
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
    // A local download finishes in hundredths of a second; the icon still
    // holds for half a second, so spamming it downloads once (Jesús, Nov 20).
    expect(download.hasAttribute("data-busy")).toBe(true);
    await holdOver();
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

  // Jesús, Nov 20: a double-click on a Shared-with-me tile downloads too, so
  // spamming it is held like the Download icon -- one download, then another
  // only after the half second.
  it("a burst of double-clicks on a shared file downloads it once", async () => {
    connect({});
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Shared with me" }));
    const tile = await screen.findByRole("button", { name: /Budget 2027\.xlsx/ });
    await act(async () => { for (let i = 0; i < 20; i++) fireEvent.doubleClick(tile); });
    expect(h.download).toHaveBeenCalledTimes(1);
    await holdOver();
    await act(async () => { fireEvent.doubleClick(tile); });
    expect(h.download).toHaveBeenCalledTimes(2);
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

describe("the Shared folder and the Sharing place (Nov 19 2026)", () => {
  const plan = artifactRow({ id: "a-2", title: "plan.md" });
  const shares = [
    shareRow({ id: "s-1", artifactId: "a-1", recipientUserId: "u-sam", recipientDisplayName: "Sam Lee", title: "brief.pdf", expiresAt: "2099-01-08T00:00:00Z", createdAt: "2026-11-18T12:00:00Z" }),
    shareRow({ id: "s-2", artifactId: "a-2", recipientUserId: "u-dana", recipientDisplayName: "Dana Cole", title: "plan.md", createdAt: "2026-11-19T12:00:00Z" }),
  ];

  it("opens and closes like a folder, with Shared with me and Sharing inside", async () => {
    connect({});
    await renderFiles();
    expect(within(rail()).getByRole("button", { name: "Shared with me" })).toBeTruthy();
    expect(within(rail()).getByRole("button", { name: "Sharing" })).toBeTruthy();
    // "Shared" is not a place: its name opens and shuts it, like its arrow.
    await click(within(rail()).getByRole("button", { name: "Shared" }));
    expect(within(rail()).queryByRole("button", { name: "Sharing" })).toBeNull();
    expect(within(rail()).queryByRole("button", { name: "Shared with me" })).toBeNull();
    await click(within(rail()).getByRole("button", { name: "Show Shared with me and Sharing" }));
    expect(within(rail()).getByRole("button", { name: "Sharing" })).toBeTruthy();
  });

  it("lists every file you are sharing: the file, who has it, the time left and Stop", async () => {
    const connection = connect({ artifacts: [brief, plan], shares });
    await renderFiles();
    const browsed = connection.callsNamed("libraryArtifactsBrowse").length;
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    expect(screen.getByRole("heading", { name: "Sharing" })).toBeTruthy();
    const list = await screen.findByRole("list", { name: "Sharing" });
    const rows = within(list).getAllByRole("listitem");
    // Newest first, as the Head's sort says.
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("plan.md"),
      expect.stringContaining("brief.pdf"),
    ]);
    expect(rows[0]!.textContent).toContain("Dana Cole");
    expect(rows[0]!.textContent).toMatch(/\d+ days left/);
    expect(connection.callsNamed("librarySharing")).toEqual(["query librarySharing()"]);
    // The Library is not read for this place.
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(browsed);

    await click(within(rows[1]!).getByRole("button", { name: "Stop sharing brief.pdf with Sam Lee" }));
    expect(connection.callsNamed("libraryShareStop")).toEqual(['builtin libraryShareStop(shareId: "s-1")']);
    // The stop lands on the share's own event, and the row leaves.
    await emit(connection, "v1:library:share", { ...shares[0]!, revokedAt: "2026-11-20T10:00:00Z" });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 50)); });
    expect(within(screen.getByRole("list", { name: "Sharing" })).getAllByRole("listitem")).toHaveLength(1);
  });

  it("names a file shared before shares carried its name, from your own Library", async () => {
    const old = shareRow({ id: "s-old", artifactId: "a-1", recipientUserId: "u-sam", recipientDisplayName: "Sam Lee" });
    connect({ shares: [old] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    expect(await screen.findByRole("button", { name: "Stop sharing brief.pdf with Sam Lee" })).toBeTruthy();
  });

  it("says so in place when the engine refuses the stop", async () => {
    connect({ shares, refuse: { libraryShareStop: "That share is not one of yours." } });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    await click(await screen.findByRole("button", { name: "Stop sharing plan.md with Dana Cole" }));
    expect(screen.getByText("Sharing did not stop.")).toBeTruthy();
    expect(screen.getByText(/That share is not one of yours\./)).toBeTruthy();
  });

  it("names a whole group, with a group icon", async () => {
    connect({ shares: [shareRow({ id: "s-g", artifactId: "a-1", recipientUserId: "", recipientGroupId: "g-design", recipientDisplayName: "Design team", title: "brief.pdf" })] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    const stop = await screen.findByRole("button", { name: "Stop sharing brief.pdf with Design team" });
    const row = stop.closest("li")!;
    expect(within(row).getByLabelText("Group")).toBeTruthy();
  });

  it("says so when you are sharing nothing", async () => {
    connect({ shares: [] });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    expect(await screen.findByText("You are not sharing any files.")).toBeTruthy();
  });
});

describe("timeLeftLabel", () => {
  const now = new Date("2026-11-20T12:00:00Z");
  it("says hours under a day, rounded up, and days above it, to the nearest day", () => {
    expect(timeLeftLabel("2026-11-27T12:00:00Z", now)).toBe("7 days left");
    expect(timeLeftLabel("2026-11-21T12:00:00Z", now)).toBe("1 day left");
    expect(timeLeftLabel("2026-11-21T06:00:00Z", now)).toBe("18 hours left");
    expect(timeLeftLabel("2026-11-20T12:30:00Z", now)).toBe("1 hour left");
    expect(timeLeftLabel("2026-11-20T11:00:00Z", now)).toBe("Ended");
    // 23.5 hours is "24 hours" rounded up: a day.
    expect(timeLeftLabel("2026-11-21T11:30:00Z", now)).toBe("1 day left");
    expect(timeLeftLabel("2026-11-21T23:00:00Z", now)).toBe("1 day left");
  });

  // Jesús, Nov 20: "It should show the number that you're picking."
  it("reads 7 for a share made for 7 days, though the clock behind the reading lags", () => {
    // The engine ends it 7 x 24 hours after it was made, and the reading's
    // clock ticks once a minute (or the cluster's runs ahead): a little MORE
    // than 7 days away, which rounding up read as "8 days left".
    expect(timeLeftLabel("2026-11-27T12:00:59Z", now)).toBe("7 days left");
    expect(timeLeftLabel("2026-11-27T11:00:00Z", now)).toBe("7 days left");
    expect(timeLeftLabel("2026-11-27T00:00:01Z", now)).toBe("7 days left");
    expect(timeLeftLabel("2026-11-26T23:00:00Z", now)).toBe("6 days left");
    expect(timeLeftLabel("2026-12-20T12:00:30Z", now)).toBe("30 days left");
  });

  it("agrees in the share view and in Sharing, right after sharing for 7 days", async () => {
    // As the engine stamps it, with its clock 40 seconds ahead of this one.
    const fresh = shareRow({ id: "s-7", artifactId: "a-1", recipientUserId: "u-sam", recipientDisplayName: "Sam Lee", title: "brief.pdf",
      expiresAt: new Date(Date.now() + 7 * 86_400_000 + 40_000).toISOString() });
    connect({ shares: [fresh] });
    await renderFiles();
    await openShare();
    const card = (await screen.findByRole("button", { name: "Actions for Sam Lee" })).closest(".os-editable-card");
    expect(card?.textContent).toContain("User · 7 days left");
    // The rail stays beside the share view.
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    const list = await screen.findByRole("list", { name: "Sharing" });
    expect(within(list).getAllByRole("listitem")[0]!.textContent).toContain("7 days left");
  });
});

describe("Shared by, for someone who oversees others' shares (item 13, Nov 20 2026)", () => {
  const mine = shareRow({ id: "s-mine", artifactId: "a-1", recipientUserId: "u-sam", recipientDisplayName: "Sam Lee", title: "brief.pdf" });
  const overseen = [
    { ...mine, ownerUserId: "u-me", ownerDisplayName: "Me Myself", mine: true },
    shareRow({ id: "s-enr", artifactId: "x-1", ownerUserId: "u-enr", recipientUserId: "u-casey", recipientDisplayName: "Casey", title: "Lab results summary.pdf", createdAt: "2026-11-19T10:00:00Z" }),
    { ...shareRow({ id: "s-majo", artifactId: "x-2", ownerUserId: "u-majo", recipientUserId: "", recipientDisplayName: "ZNAS team", title: "Site visit 03.jpg", createdAt: "2026-11-19T11:00:00Z" }), recipientGroupId: "g-znas" },
  ].map((row, i) => ({ ...row, ownerDisplayName: (row as Record<string, unknown>).ownerDisplayName ?? ["", "Enrique Mendivil", "Majo Zepeda"][i] }));
  const sharedBy = () => screen.getByLabelText("Shared by", { selector: "button" });

  it("starts on Me, and Everyone lists each share with who shared it", async () => {
    const connection = connect({ shares: [mine], overseen });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    expect(sharedBy().textContent).toContain("Me");
    expect(within(openSelect(sharedBy())).getAllByRole("option").map((o) => o.textContent)).toEqual(["Me", "Everyone", "Enrique Mendivil", "Majo Zepeda"]);
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    // On Me, only your own, with no "by" line.
    expect((await screen.findByRole("list", { name: "Sharing" })).textContent).not.toContain("by ");

    chooseOption(sharedBy(), "Everyone");
    const rows = within(await screen.findByRole("list", { name: "Sharing" })).getAllByRole("listitem");
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("by Majo Zepeda"),
      expect.stringContaining("by Enrique Mendivil"),
      expect.stringContaining("by Me Myself"),
    ]);
    expect(connection.callsNamed("librarySharesOverseen").length).toBeGreaterThan(0);

    chooseOption(sharedBy(), "Enrique Mendivil");
    const only = within(await screen.findByRole("list", { name: "Sharing" })).getAllByRole("listitem");
    expect(only).toHaveLength(1);
    expect(only[0]!.textContent).toContain("Lab results summary.pdf");
  });

  it("stops somebody else's share and reads the list again", async () => {
    const connection = connect({ shares: [mine], overseen });
    await renderFiles();
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    chooseOption(sharedBy(), "Everyone");
    const reads = connection.callsNamed("librarySharesOverseen").length;
    await click(await screen.findByRole("button", { name: "Stop sharing Lab results summary.pdf with Casey" }));
    expect(connection.callsNamed("libraryShareStop")).toEqual(['builtin libraryShareStop(shareId: "s-enr")']);
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 50)); });
    expect(connection.callsNamed("librarySharesOverseen").length).toBeGreaterThan(reads);
  });

  it("is offered to a member, who oversees viewers (Nov 20 rule)", async () => {
    connect({ shares: [mine], overseen });
    await renderFiles({ role: "writer" });
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    await screen.findByRole("list", { name: "Sharing" });
    expect(await screen.findByLabelText("Shared by", { selector: "button" })).toBeTruthy();
  });

  it("is absent for a viewer, who oversees no one", async () => {
    const connection = connect({ shares: [mine], overseen });
    await renderFiles({ role: "reader" });
    await click(within(rail()).getByRole("button", { name: "Sharing" }));
    await screen.findByRole("list", { name: "Sharing" });
    expect(screen.queryByLabelText("Shared by", { selector: "button" })).toBeNull();
    expect(connection.callsNamed("librarySharesOverseen")).toEqual([]);
  });
});
