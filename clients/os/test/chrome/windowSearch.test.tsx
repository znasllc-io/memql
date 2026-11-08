import { useState, type ComponentType } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Layers } from "lucide-react";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { AskProvider } from "../../src/ask/AskProvider";
import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { WindowFrame } from "../../src/chrome/WindowFrame";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { Refine } from "../../src/kit/controls";
import { loadApp } from "../../src/system/loadApp";
import type { OsAppManifest, OsAppProps } from "../../src/system/registry";
import { newWindow } from "../../src/system/windows";
import { StubAskTransport } from "../ask/stubTransport";
import { installSeededAccess } from "../seededAccess";

function Directory() {
  const [search, setSearch] = useState("");
  return <Refine label="Find records" search={search} onSearch={setSearch} placeholder="Search records" />;
}

function pendingWindow() {
  let finish!: (component: ComponentType<OsAppProps>) => void;
  const load = vi.fn(() => new Promise<ComponentType<OsAppProps>>(resolve => { finish = resolve; }));
  const app: OsAppManifest = {
    id: "demo", name: "Demo", icon: Layers, requires: "app:email",
    settingsSection: "settings", logsSection: "logs",
    sections: [{ id: "records", name: "Records" }, { id: "other", name: "Other" }, { id: "settings", name: "Settings" }, { id: "logs", name: "Logs" }],
    // Most apps use the current section's search without a default searchSection.
    component: loadApp("Demo", load),
  };
  const registry = { apps: [app], widgets: [] };
  const transport = new StubAskTransport();
  const view = (hidden = false, section = "records", revision = 0) => <SessionProvider value={{
    access: { userId: "owner", primaryEmail: "owner@example.test", role: "owner", roleName: "", rank: 0 },
    config: UNKNOWN_RUNTIME_CONFIG, ladderLoaded: true,
  }}><OsProvider registry={registry} actorRole="owner" grid={{ cols: 12, rows: 8 }}>
    <AskProvider transport={transport}><WindowFrame win={{ ...newWindow("window", app.id, section), sectionNavigation: { origin: "peer", revision } }} manifest={app}
      rect={{ x: 0, y: 0, w: 640, h: 420 }} focused actorRole="owner" hidden={hidden} />
    </AskProvider>
  </OsProvider></SessionProvider>;
  const mounted = render(view());
  return { load, finish: (component: ComponentType<OsAppProps>) => finish(component),
    rerender: (hidden = false, section = "records", revision = 0) => mounted.rerender(view(hidden, section, revision)) };
}

const originalShowModal = Object.getOwnPropertyDescriptor(HTMLDialogElement.prototype, "showModal");
const originalClose = Object.getOwnPropertyDescriptor(HTMLDialogElement.prototype, "close");
beforeAll(() => {
  Object.defineProperties(HTMLDialogElement.prototype, {
    showModal: { configurable: true, value(this: HTMLDialogElement) { this.setAttribute("open", ""); } },
    close: { configurable: true, value(this: HTMLDialogElement) { this.removeAttribute("open"); } },
  });
});
afterAll(() => {
  if (originalShowModal) Object.defineProperty(HTMLDialogElement.prototype, "showModal", originalShowModal);
  else Reflect.deleteProperty(HTMLDialogElement.prototype, "showModal");
  if (originalClose) Object.defineProperty(HTMLDialogElement.prototype, "close", originalClose);
  else Reflect.deleteProperty(HTMLDialogElement.prototype, "close");
});
beforeEach(() => installSeededAccess("owner"));

describe("search requested while an app downloads", () => {
  it("waits for the current section's search even without a default search section", async () => {
    const app = pendingWindow();
    await waitFor(() => expect(app.load).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "Search Demo" }));
    expect(document.querySelector(".os-window-search[open]")).toBeNull();
    await act(async () => app.finish(Directory));
    const search = await screen.findByPlaceholderText("Search records");
    await waitFor(() => expect(search).toBe(document.activeElement));
    expect(document.querySelector(".os-window-search[open]")).toBeNull();
  });

  it("offers destination search only after a loaded section has no resource search", async () => {
    const app = pendingWindow();
    await waitFor(() => expect(app.load).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "Search Demo" }));
    expect(document.querySelector(".os-window-search[open]")).toBeNull();
    await act(async () => app.finish(() => <p>Loaded content</p>));
    await waitFor(() => expect(document.querySelector(".os-window-search[open]")).not.toBeNull());
    expect(screen.getByRole("dialog", { name: "Search Demo destinations" })).toBeTruthy();
  });

  it.each(["hide", "navigate", "navigate-same-section"] as const)("cancels the queued search when the window changes: %s", async change => {
    const app = pendingWindow();
    await waitFor(() => expect(app.load).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "Search Demo" }));
    expect(document.querySelector(".os-window-search[open]")).toBeNull();
    const section = change === "navigate" ? "other" : "records";
    const revision = change === "hide" ? 0 : 1;
    app.rerender(change === "hide", section, revision);
    await act(async () => app.finish(Directory));
    app.rerender(false, section, revision);
    await act(async () => { await new Promise(requestAnimationFrame); });
    expect(screen.queryByPlaceholderText("Search records")).toBeNull();
    expect(document.querySelector(".os-window-search[open]")).toBeNull();
  });
});
