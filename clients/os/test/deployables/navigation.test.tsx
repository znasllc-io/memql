import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import type { OsAppProps } from "../../src/system/registry";
import { SHOP, DOCS, click, fakeConnection, withSession } from "./harness";

function mount(sectionId = "deployables") {
  h.connection = fakeConnection({ sites: [SHOP, DOCS] });
  const saved = new Map<string, string>();
  const store = new LocalDeployablesSettingsStore({
    getItem: key => saved.get(key) ?? null,
    setItem: (key, value) => { saved.set(key, value); },
  });
  let revision = 0;
  let visible = true;
  let currentSection = sectionId;
  let currentOrigin: NonNullable<OsAppProps["navigation"]>["origin"] = "peer";
  const navigate = vi.fn((section: string, options?: { fromContent?: boolean }) => {
    go(section, options?.fromContent ? "content" : "peer");
  });
  const app = (section: string, origin: NonNullable<OsAppProps["navigation"]>["origin"]) => withSession(
    <DeployablesApp sectionId={section} navigation={{ origin, revision }} windowVisible={visible} navigate={navigate} askContext={vi.fn()} store={store} />,
    { role: "owner", userId: "u-me" },
  );
  const result = render(app(sectionId, "peer"));
  function go(section: string, origin: NonNullable<OsAppProps["navigation"]>["origin"] = "peer") {
    revision += 1;
    currentSection = section;
    currentOrigin = origin;
    result.rerender(app(section, origin));
  }
  function setVisible(next: boolean) {
    visible = next;
    result.rerender(app(currentSection, currentOrigin));
  }
  return { go, navigate, setVisible };
}

describe("shared shell navigation", () => {
  it("keeps address setup inside the app and retains its form when the window is hidden", async () => {
    const { setVisible } = mount("map");
    await click(await screen.findByLabelText(/^Deployable shop.memql.example.com/));
    await click(await screen.findByRole("button", { name: /^Cluster address/ }));
    expect(screen.queryByRole("dialog", { name: "Addresses and client" })).toBeNull();
    const input = screen.getByRole("textbox", { name: "Domain to bind" });
    fireEvent.change(input, { target: { value: "www.acme.com" } });
    setVisible(false);
    setVisible(true);
    expect(screen.getByRole("textbox", { name: "Domain to bind" })).toBe(input);
    expect((input as HTMLInputElement).value).toBe("www.acme.com");
    await click(screen.getByRole("button", { name: "Back to Storefront" }));
    expect(screen.getByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
  });

  it("shows the landing with no Back on an explicit same-tab click", async () => {
    const { go } = mount();
    await click(await screen.findByRole("button", { name: /New deployable/ }));
    expect(screen.getByRole("button", { name: "Back to Deployables" })).toBeTruthy();
    go("deployables");
    expect(screen.queryByRole("region", { name: "New deployable" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Back to Deployables" })).toBeNull();
    expect(screen.getByRole("button", { name: /New deployable/ })).toBeTruthy();
  });

  it("retains an unfinished source form across peer tabs and resumes it from New", async () => {
    const { go } = mount();
    await click(await screen.findByRole("button", { name: /New deployable/ }));
    const draft = screen.getByRole("region", { name: "New deployable" });
    await click(within(draft).getByRole("radio", { name: /A repository/ }));
    await click(within(draft).getByRole("button", { name: "Use a token instead" }));
    const input = within(draft).getByRole("textbox", { name: "The repository this deployable is built from" });
    fireEvent.change(input, { target: { value: "https://github.com/acme/unfinished" } });
    go("settings");
    expect(screen.queryByRole("region", { name: "New deployable" })).toBeNull();
    go("deployables");
    expect(screen.queryByRole("button", { name: "Back to Deployables" })).toBeNull();
    await click(screen.getByRole("button", { name: /New deployable/ }));
    expect(screen.getByRole("textbox", { name: "The repository this deployable is built from" })).toBe(input);
    expect((input as HTMLInputElement).value).toBe("https://github.com/acme/unfinished");
  });

  it("restores the nested origin on contextual Back", async () => {
    const { go } = mount();
    await click(await screen.findByRole("button", { name: /New deployable/ }));
    const draft = screen.getByRole("region", { name: "New deployable" });
    go("logs", "content");
    expect(screen.queryByRole("region", { name: "New deployable" })).toBeNull();
    go("deployables", "back");
    expect(screen.getByRole("region", { name: "New deployable" })).toBe(draft);
    expect(screen.getByRole("button", { name: "Back to Deployables" })).toBeTruthy();
  });

  it("restores map pan and selection, then opens a different app in the retained pane", async () => {
    const { go, navigate } = mount("map");
    await screen.findByLabelText(/^Deployable shop.memql.example.com/);
    fireEvent.keyDown(screen.getByRole("application", { name: "Deploy map" }), { key: "ArrowRight" });
    await click(screen.getByRole("button", { name: /^Deployable shop.memql.example.com/ }));
    expect(navigate).toHaveBeenLastCalledWith("deployables", { fromContent: true });
    expect(await screen.findByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
    go("map", "back");
    expect(document.querySelector("[data-os-map-view]")?.getAttribute("transform")).toBe("translate(-48 0) scale(1)");
    expect(screen.getByRole("button", { name: /^Deployable shop.memql.example.com/ }).getAttribute("aria-pressed")).toBe("true");
    await click(screen.getByRole("button", { name: /^Deployable docs.memql.example.com/ }));
    expect(await screen.findByRole("region", { name: "Deployable docs.memql.example.com" })).toBeTruthy();
  });
});
