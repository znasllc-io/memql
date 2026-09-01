import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { StubAskTransport } from "../src/ask/askController";
import { resetIdsForTest } from "../src/system/desks";
import { LocalDesktopStore } from "../src/system/store";
import type { OsRuntimeConfig } from "../src/cluster/config";
import { artifactHandoffUrl, openInVsCode } from "../src/items/vscode";

// The shell behavior suite (spec K): rendered against the REAL registry,
// an in-memory store per test, and the stub transports PR A ships.

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};

const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", clusterRole: "owner" };
const READER = { userId: "u-2", primaryEmail: "reader@example.test", clusterRole: "reader" };

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function renderShell({
  access = OWNER,
  layout = "desktop" as const,
  storage = memStorage(),
}: {
  access?: typeof OWNER;
  layout?: "desktop" | "ipad" | "phone";
  storage?: Pick<Storage, "getItem" | "setItem">;
} = {}) {
  const view = render(
    <Shell
      layout={layout}
      onSignOut={vi.fn()}
      access={access}
      config={CONFIG}
      ports={{ store: new LocalDesktopStore(storage), disableConnection: true, askTransport: new StubAskTransport(), askVoice: null }}
    />,
  );
  return { view, storage };
}

function openFromLauncher(name: string) {
  fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name }));
}

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
});

describe("the desktop lands (spec K bullet 1)", () => {
  it("shows the desk world and none of the slot chrome", () => {
    renderShell();
    expect(document.querySelector("[data-os-desktop]")).not.toBeNull();
    expect(document.querySelector("[data-os-dock]")).not.toBeNull();
    expect(document.querySelector("[data-os-field]")).not.toBeNull();
    expect(document.querySelector("[data-os-desk-numeral]")).not.toBeNull();
    expect(document.querySelector("[data-os-slot]")).toBeNull();
  });

  it("seeds the Ask widget on a fresh desktop", () => {
    renderShell();
    expect(document.querySelector("[data-os-widget='ask']")).not.toBeNull();
  });
});

describe("windows and desks (spec K bullets 2-3)", () => {
  it("opens two apps on one desk, spills the third onto a new desk", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    expect(document.querySelectorAll(".os-window")).toHaveLength(2);
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();

    openFromLauncher("Deployables");
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
    const pager = screen.getByRole("navigation", { name: "Desks" });
    expect(within(pager).getAllByRole("button", { name: /^Desk \d/ })).toHaveLength(2);
  });

  it("relaunching an open app focuses it instead of duplicating it", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Files");
    expect(document.querySelectorAll("[data-os-window='files']")).toHaveLength(1);
  });

  it("minimizes to the dock and restores from it", () => {
    renderShell();
    openFromLauncher("Files");
    fireEvent.click(screen.getByRole("button", { name: "Minimize Files" }));
    expect(document.querySelector("[data-os-window='files']")).toBeNull();

    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    fireEvent.click(within(dock).getByRole("button", { name: "Files (running)" }));
    expect(document.querySelector("[data-os-window='files']")).not.toBeNull();
  });

  it("full-screens and closes", () => {
    renderShell();
    openFromLauncher("Files");
    fireEvent.click(screen.getByRole("button", { name: "Full screen Files" }));
    expect(document.querySelector("[data-os-window='files']")?.hasAttribute("data-fullscreen")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Close Files" }));
    expect(document.querySelector("[data-os-window='files']")).toBeNull();
  });

  it("switches desks from the keyboard", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    openFromLauncher("Deployables"); // desk 2
    fireEvent.keyDown(window, { key: "ArrowLeft", ctrlKey: true, shiftKey: true });
    expect(screen.getByText("Desk 1 of 2")).toBeTruthy();
    fireEvent.keyDown(window, { key: "2", ctrlKey: true, shiftKey: true });
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
  });
});

describe("dock pins (spec K bullet 3)", () => {
  it("pins from the context menu and persists across a remount", () => {
    const storage = memStorage();
    const { view } = renderShell({ storage });
    openFromLauncher("Files");
    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    fireEvent.contextMenu(within(dock).getByRole("button", { name: "Files (running)" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Pin to dock" }));

    view.unmount();
    resetIdsForTest();
    renderShell({ storage });
    const dock2 = document.querySelector("[data-os-dock]") as HTMLElement;
    expect(within(dock2).getByRole("button", { name: "Files" })).toBeTruthy();
  });
});

describe("roles (spec K bullet 7)", () => {
  it("hides admin- and writer-gated apps from a reader everywhere", () => {
    renderShell({ access: READER });
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = screen.getByRole("dialog", { name: "Launcher" });
    expect(within(launcher).queryByRole("button", { name: "Users" })).toBeNull();
    expect(within(launcher).queryByRole("button", { name: "Training" })).toBeNull();
    expect(within(launcher).getByRole("button", { name: "Files" })).toBeTruthy();
  });

  it("gates app sections: reader sees no Cluster section in Settings", () => {
    renderShell({ access: READER });
    openFromLauncher("Settings");
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    expect(within(nav).queryByRole("button", { name: "Cluster" })).toBeNull();
    expect(within(nav).getByRole("button", { name: "About" })).toBeTruthy();
  });

  it("owner sees the Cluster section and the gear jumps to Appearance", () => {
    renderShell();
    openFromLauncher("Settings");
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    expect(within(nav).getByRole("button", { name: "Cluster" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Settings settings" }));
    expect(screen.getByRole("heading", { name: "Appearance" })).toBeTruthy();
  });
});

describe("Ask (spec K bullet 5)", () => {
  it("the orb, the widget and the title bar open the same surface", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    expect(within(sheet).getByRole("textbox", { name: "Ask" })).toBeTruthy();
    fireEvent.keyDown(sheet, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Ask" })).toBeNull();

    openFromLauncher("Files");
    fireEvent.click(screen.getByRole("button", { name: "Ask about Files" }));
    const sheet2 = screen.getByRole("dialog", { name: "Ask" });
    expect(within(sheet2).getByText(/app:files/)).toBeTruthy();

    const widget = document.querySelector("[data-os-widget='ask']") as HTMLElement;
    expect(within(widget).getByRole("textbox", { name: "Ask" })).toBeTruthy();
  });

  it("streams an answer for a text question", async () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    const input = within(sheet).getByRole("textbox", { name: "Ask" });
    fireEvent.change(input, { target: { value: "what is this cluster" } });
    fireEvent.click(within(sheet).getByRole("button", { name: "Send" }));
    expect(await within(sheet).findByText(/Ask is not connected/, undefined, { timeout: 4000 })).toBeTruthy();
  });

  // The harness passes askVoice={null} (jsdom has no audio stack), which is
  // the "this window has no voice wiring" case -- and the rule it pins is
  // that the control stays PRESENT and accounts for itself, rather than
  // disappearing or sitting there dead. Voice's own behaviour is tested
  // against the pure session in test/ask/, with no shell and no DOM.
  it("an unwired mic control says so instead of going quiet", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    const mic = within(sheet).getByRole("button", { name: "Ask by voice" }) as HTMLButtonElement;
    expect(mic.disabled).toBe(true);
    expect(within(sheet).getByText(/Voice is not wired up in this window/)).toBeTruthy();
  });
});

describe("desktop items (spec K bullet 4)", () => {
  it("uploads a dropped host file and lands it as a desk file", async () => {
    renderShell();
    const plate = document.querySelector(".os-plate") as HTMLElement;
    const file = new File(["hello"], "notes.txt", { type: "text/plain" });
    fireEvent.drop(plate, { dataTransfer: { files: [file], types: ["Files"] } });
    expect(await screen.findByText("notes.txt")).toBeTruthy();
    expect(
      await screen.findByRole("button", { name: "notes.txt -- open in VS Code" }),
    ).toBeTruthy();
  });

  it("refuses desk folder creation while no cluster connection exists", () => {
    // A desk folder IS a Library folder now (design D4), so creating one is
    // a write the cluster must confirm. This shell renders with the
    // connection disabled, and the menu says so by refusing -- a shortcut to
    // a folder that was never created would be a control pointing at
    // nothing. The connected path is covered in test/files/desk.test.tsx.
    renderShell();
    const plate = document.querySelector(".os-plate") as HTMLElement;
    fireEvent.contextMenu(plate);
    const entry = screen.getByRole("menuitem", { name: "New folder" });
    expect((entry as HTMLButtonElement).disabled).toBe(true);
  });

  it("removes the seeded widget through its menu, leaving the empty-desk hint", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask widget menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove from desk" }));
    expect(document.querySelector("[data-os-widget='ask']")).toBeNull();
    expect(screen.getByText("Drop a file, or open the Launcher.")).toBeTruthy();
  });
});

describe("the VS Code handoff (spec D3)", () => {
  it("builds the portal-shaped URL with kind=artifact", () => {
    expect(artifactHandoffUrl("acme.example.com", "v1:library:artifact:abc")).toBe(
      "vscode://znasllc.memql/open?v=1&cluster=acme.example.com&kind=artifact&id=v1%3Alibrary%3Aartifact%3Aabc",
    );
  });

  it("fires the URL and reports no-answer only while the page stays visible", () => {
    const fired: string[] = [];
    let armed: (() => void) | null = null;
    const noAnswer = vi.fn();
    openInVsCode("acme.example.com", "a-1", noAnswer, {
      navigate: (url) => fired.push(url),
      schedule: (fn) => {
        armed = fn;
        return () => {};
      },
    });
    expect(fired).toHaveLength(1);
    expect(noAnswer).not.toHaveBeenCalled();
    armed!();
    expect(noAnswer).toHaveBeenCalledTimes(1);
  });
});

describe("phone chrome (spec D13)", () => {
  it("has no desks, windows or pins -- a tab bar and one app at a time", () => {
    renderShell({ layout: "phone" });
    expect(document.querySelector("[data-os-desktop]")).toBeNull();
    expect(document.querySelector("[data-os-dock]")).toBeNull();
    expect(document.querySelector("[data-os-phone]")).not.toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    expect(screen.getByRole("heading", { name: "About this OS" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to home" }));
    expect(screen.queryByRole("heading", { name: "About this OS" })).toBeNull();
  });
});

describe("right-click (the shell owns it)", () => {
  it("suppresses the browser menu on a control inside a window", () => {
    renderShell();
    // Anything the shell renders and does not offer a menu for: the browser's
    // own Back / Reload / View Page Source over a desktop window is the
    // loudest tell that this is a tab.
    const dock = document.querySelector(".os-dock") as HTMLElement;
    expect(dock).toBeTruthy();
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    dock.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it("covers the PHONE shell too, which is a separate root", () => {
    // Two roots render the shell, and a rule attached to one of them is a
    // rule that holds on half the product.
    renderShell({ layout: "phone" });
    const root = document.querySelector("[data-os-root]") as HTMLElement;
    expect(root).toBeTruthy();
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    root.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it("still lets a text field have cut/copy/paste", () => {
    renderShell();
    const input = document.createElement("input");
    (document.querySelector("[data-os-root]") as HTMLElement).appendChild(input);
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    input.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});
