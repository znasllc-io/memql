import { fireEvent, screen, within } from "@testing-library/react";
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
import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";

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
    // Both actions land in the folder being looked at, which is invisible
    // from the button, so the menu says it.
    expect(screen.getByText("Into Library")).toBeTruthy();
  });

  it("creates the folder inside the folder being looked at", async () => {
    const connection = fakeConnection({ folders: [folderRow({ id: "f-a", name: "Contracts" })] });
    h.connection = connection;
    await renderFiles();

    await click(screen.getByRole("button", { name: /Open folder Contracts/ }));
    await click(screen.getByRole("button", { name: /Add/ }));
    expect(screen.getByText("Into Contracts")).toBeTruthy();
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
    for (const name of [
      "Open in editor",
      "Send to desktop",
      "Download",
      "Move to folder",
      "Ask about this file",
      "Move to Bin",
    ]) {
      expect(within(menu).getByRole("menuitem", { name })).toBeTruthy();
    }
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
    expect(within(menu).queryByRole("menuitem", { name: "Move to Bin" })).toBeNull();
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
    chooseOption(screen.getByLabelText("Move to folder"), "Contracts");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-a")',
    ]);
  });
});


describe("Files workspace navigation", () => {
 it("uses ordered tabs and returns from details to the same folder", async () => {
  h.connection = fakeConnection({folders:[folderRow({id:"research",name:"Research"})],artifacts:[artifactRow({id:"paper",title:"paper.md",folderId:"research"})]});
  await renderFiles();
  const tabs=screen.getByRole("navigation",{name:"File locations"});
  expect(within(tabs).getAllByRole("button").map(button=>button.textContent)).toEqual(["Library","Desktop","Materializer","Bin"]);
  expect(within(tabs).getByRole("button",{name:"Library"}).getAttribute("aria-current")).toBe("page");
  expect(screen.getByRole("button",{name:/paper.md/})).toBeTruthy();
  await click(screen.getByRole("button",{name:"Open folder Research"}));
  await click(screen.getByRole("button",{name:/paper.md/}));
  expect(screen.queryByRole("navigation",{name:"File locations"})).toBeNull();
  await click(screen.getByRole("button",{name:"Back to Research"}));
  expect(screen.getByRole("heading",{name:"Research"})).toBeTruthy();
  expect(screen.getByRole("button",{name:/paper.md/})).toBeTruthy();
 });
});
