import { act, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake -- the same
// shape browse.test.tsx uses, and it has to be per-file: `vi.hoisted` runs
// before imports, so a shared one exported from the harness would be read
// after the mock factory needed it.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { chooseOption } from "../selectControl";
import { artifactRow, click, fakeConnection, folderRow, renderFiles, openDetails } from "./harness";

// The rail's disclosures, the one Add control, and the row's menu.
//
// Three surfaces that were reported together by the owner and that share one
// cause: the Files browse had grown a rail that rendered every place's
// folders at once, an action wedged between two of those places, and a
// right-click that offered a single verb. Each is a small change; what they
// have in common is that the app was making the person work out where things
// were rather than saying so.

beforeEach(() => {
  h.connection = fakeConnection();
});

describe("the Add control", () => {
  it("is the one way to put something here, and names where 'here' is", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
    });
    await renderFiles();

    // The rail action is gone -- it read as a folder you could open, sitting
    // between the Library tree and the Desktop place.
    expect(screen.queryByRole("button", { name: /^New folder$/ })).toBeNull();

    await click(screen.getByRole("button", { name: /Add/ }));
    expect(screen.getByRole("menuitem", { name: /Upload files/ })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /New folder/ })).toBeTruthy();
    // No heading naming the folder (Jesús, Nov 20 2026: "Remove it.").
    expect(screen.queryByText("Into Library")).toBeNull();
  });

  it("is absent where nothing can be added: the Bin, Shared with me and Sharing", async () => {
    await renderFiles();
    const rail = screen.getByRole("navigation", { name: "File locations" });
    for (const place of ["Bin", "Shared with me", "Sharing"]) {
      await click(within(rail).getByRole("button", { name: place }));
      expect(screen.queryByRole("button", { name: /^Add$/ }), place).toBeNull();
    }
    await click(within(rail).getByRole("button", { name: "Library" }));
    expect(screen.getByRole("button", { name: /^Add$/ })).toBeTruthy();
  });

  it("creates the folder inside the folder being looked at", async () => {
    const connection = fakeConnection({ folders: [folderRow({ id: "f-a", name: "Contracts" })] });
    h.connection = connection;
    await renderFiles();

    await click(screen.getByRole("button", { name: /Open folder Contracts/ }));
    await click(screen.getByRole("button", { name: /Add/ }));
    await click(screen.getByRole("menuitem", { name: /New folder/ }));

    const calls = connection.callsNamed("createLibraryFolder");
    expect(calls).toHaveLength(1);
    expect(calls[0]).toContain('parentFolderId: "f-a"');
  });
});

describe("the row's right-click menu", () => {
  it("carries the actions the panel carries, not just one", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    const menu = screen.getByRole("menu", { name: "File" });
    // The owner's order: Open and Download first, Delete last, and
    // Details -- the file's page, which a click no longer opens -- among them.
    expect(within(menu).getAllByRole("menuitem").map((item) => item.textContent)).toEqual([
      "Open",
      "Download",
      "Share",
      "Details",
      "Send to desktop",
      "Move to folder",
      "Ask about this file",
      "Delete",
    ]);
    expect(within(menu).queryByRole("menuitem", { name: "Upload new version" })).toBeNull();
    expect(screen.queryByLabelText("Choose a file to upload as the new version")).toBeNull();
  });

  // Asking about a file is the person's explicit request, so it goes through
  // askAbout -- the prop that opens Ask. askContext only notes context and
  // never opens anything, so routing this through it would do nothing.
  it("asks about the file through askAbout, not the context note", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    const askAbout = vi.fn();
    const askContext = vi.fn();
    await renderFiles({ askAbout, askContext });

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    fireEvent.click(within(screen.getByRole("menu", { name: "File" })).getByRole("menuitem", { name: "Ask about this file" }));
    expect(askAbout).toHaveBeenCalledWith("app:files/browse file:brief.pdf");
    expect(askContext).not.toHaveBeenCalledWith("app:files/browse file:brief.pdf");
  });

  it("offers an archived row what an archived row can do", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "old.zip", archived: true })],
    });
    await renderFiles();
    await click(screen.getByRole("button", { name: /^Bin/ }));

    fireEvent.contextMenu(screen.getByRole("button", { name: /old\.zip/ }));
    const menu = screen.getByRole("menu", { name: "File" });
    expect(within(menu).getByRole("menuitem", { name: "Restore" })).toBeTruthy();
    // Archiving something already archived is not an action, and offering it
    // would be the menu describing a state it can see is not the case.
    expect(within(menu).queryByRole("menuitem", { name: "Delete" })).toBeNull();
  });

  it("re-files a row from the menu, which is where re-filing lives now", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(screen.getByRole("menuitem", { name: "Move to folder" }));
    // Driven the way a person drives it: the picker is the kit's own listbox
    // now (#4862), so a `change` event fired at the element would reach
    // nothing and leave the assertion below standing over no interaction.
    chooseOption(screen.getByLabelText("Move to folder", { selector: ":not(.os-icon-button)" }), "Contracts");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-a")',
    ]);
  });
});


describe("Files workspace navigation", () => {
 // The rail's places and folders, not its disclosure buttons.
 const placesIn = (rail: HTMLElement) => within(rail).getAllByRole("button").filter(button => !button.hasAttribute("aria-expanded")).map(button => button.textContent);

 it("lists the Library's folders, then the places, and returns from details to the same folder", async () => {
  h.connection = fakeConnection({folders:[folderRow({id:"research",name:"Research"})],artifacts:[artifactRow({id:"paper",title:"paper.md",folderId:"research"})]});
  await renderFiles();
  const rail=screen.getByRole("navigation",{name:"File locations"});
  expect(placesIn(rail)).toEqual(["Library","Research","Desktop","Materializer","Shared","Shared with me","Sharing","Bin"]);
  expect(within(rail).getByRole("button",{name:"Library"}).getAttribute("aria-current")).toBe("page");
  expect(screen.getByRole("button",{name:/paper.md/})).toBeTruthy();
  await click(within(rail).getByRole("button",{name:"Research"}));
  expect(within(rail).getByRole("button",{name:"Research"}).getAttribute("aria-current")).toBe("page");
  expect(screen.getByRole("heading",{name:"Research"})).toBeTruthy();
  await openDetails(/paper.md/);
  expect(screen.queryByRole("navigation",{name:"File locations"})).toBeNull();
  await click(screen.getByRole("button",{name:"Back to Research"}));
  expect(screen.getByRole("heading",{name:"Research"})).toBeTruthy();
  expect(screen.getByRole("button",{name:/paper.md/})).toBeTruthy();
 });

 it("selects a file on a click and opens nothing", async () => {
  h.connection = fakeConnection({artifacts:[artifactRow({id:"paper",title:"paper.md"})]});
  await renderFiles();
  const tile = screen.getByRole("button",{name:/paper.md/});
  expect(tile.getAttribute("aria-pressed")).toBe("false");
  await click(tile);
  expect(tile.getAttribute("aria-pressed")).toBe("true");
  expect(screen.queryByRole("region",{name:"File details"})).toBeNull();
  expect(screen.getByRole("navigation",{name:"File locations"})).toBeTruthy();
 });

 it("folds a folder's children away and keeps the way to the open folder showing", async () => {
  h.connection = fakeConnection({folders:[folderRow({id:"research",name:"Research"}),folderRow({id:"lab",name:"Lab data",parentFolderId:"research"})]});
  await renderFiles();
  const rail=screen.getByRole("navigation",{name:"File locations"});
  expect(within(rail).queryByRole("button",{name:"Lab data"})).toBeNull();
  await click(within(rail).getByRole("button",{name:"Show folders in Research"}));
  await click(within(rail).getByRole("button",{name:"Lab data"}));
  expect(within(rail).getByRole("button",{name:"Lab data"}).getAttribute("aria-current")).toBe("page");
  await click(within(rail).getByRole("button",{name:"Hide folders in Library"}));
  expect(within(rail).queryByRole("button",{name:"Research"})).toBeNull();
 });
});

describe("on a touch screen", () => {
 // Safari on iPhone and iPad sends no contextmenu on a long press, and a tap
 // on a file only selects it, so a held finger is how the menu is reached.
 it("opens a file's menu on a held finger, and a tap only selects", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
   h.connection = fakeConnection({artifacts:[artifactRow({id:"paper",title:"paper.md"})]});
   await renderFiles();
   const tile = screen.getByRole("button",{name:/paper.md/});
   fireEvent.pointerDown(tile, { pointerType: "touch", clientX: 40, clientY: 40 });
   fireEvent.pointerUp(tile, { pointerType: "touch" });
   expect(screen.queryByRole("menu",{name:"File"})).toBeNull();
   fireEvent.pointerDown(tile, { pointerType: "touch", clientX: 40, clientY: 40 });
   await act(async () => { vi.advanceTimersByTime(600); });
   expect(screen.getByRole("menu",{name:"File"})).toBeTruthy();
   expect(tile.getAttribute("aria-pressed")).toBe("true");
  } finally {
   vi.useRealTimers();
  }
 });
});

// The owner asked (Nov 18 2026) for a picked file's acts as icons on the
// Head, the way a deployable's page carries its own. They are the row menu's
// entries, so the test holds the two together rather than listing them twice.
describe("the picked file's icons on the Head", () => {
  it("are absent until a click picks a file, then mirror its right-click menu", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();
    expect(screen.queryByRole("group", { name: /^Selected:/ })).toBeNull();

    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const acts = screen.getByRole("group", { name: "Selected: brief.pdf" });
    const icons = within(acts).getAllByRole("button");
    for (const icon of icons) expect(icon.querySelector("svg")).not.toBeNull();

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    const menu = screen.getByRole("menu", { name: "File" });
    expect(icons.map((icon) => icon.getAttribute("aria-label")))
      .toEqual(within(menu).getAllByRole("menuitem").map((item) => item.textContent));
  });

  it("name themselves at once, above the icon, not with the browser's slow tooltip", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const download = within(screen.getByRole("group", { name: "Selected: brief.pdf" })).getByRole("button", { name: "Download" });
    expect(download.hasAttribute("title")).toBe(false);
    fireEvent.pointerEnter(download);
    expect(document.body.querySelector(".os-tip")?.textContent).toBe("Download");
    fireEvent.pointerLeave(download);
    expect(document.body.querySelector(".os-tip")).toBeNull();
  });

  it("act on the picked file: Details opens its page", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(within(screen.getByRole("group", { name: "Selected: brief.pdf" })).getByRole("button", { name: "Details" }));
    expect(screen.getByRole("region", { name: "File details" })).toBeTruthy();
  });
});
