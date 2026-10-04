import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { PIPELINE_CONCEPT } from "../../../src/apps/deployables/pipelines/rows";
import { formatMoment } from "../../../src/kit/format";
import { click, emit, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { PACKAGE_ID, VIEWER, minutesAgo, packageRow, pipelineRow, runRow } from "./fixtures";

// Pipeline settings (epic memql#5479, D14; issue memql#5501): a source's
// pipeline, read and changed, opened from the source page's bar. Through the
// real app, so the disconnect the page sends is the generated builder's own
// text, and the state it shows afterwards is the row the feed carries --
// never one the page flipped for itself.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function mount(connection: FakeConnection, navigate: (id: string, options?: { fromContent?: boolean }) => void = vi.fn()) {
  h.connection = connection;
  return render(
    withSession(<DeployablesApp sectionId="sources" navigate={navigate} askContext={vi.fn()} store={memStore()} />, { role: "owner", userId: VIEWER }),
  );
}

const APP = siteRow({ id: "site-store", hostname: "store.memql.example.com", bundleRef: "blob://sites/site-store/v2/", packageId: PACKAGE_ID, packageDeployableName: "storefront" });

function seed(over: Partial<FakeSeed> = {}): FakeSeed {
  return {
    sites: [APP],
    packages: [packageRow()],
    pipelines: [pipelineRow()],
    pipelineRuns: [runRow({ id: "r-main", ownerUserId: VIEWER, event: "push", headBranch: "main", conclusion: "success", queuedAt: minutesAgo(20) })],
    ...over,
  };
}

/** From the Sources list to the source's pipeline settings, the way a person gets there. */
async function openSettings(): Promise<HTMLElement> {
  await click(await screen.findByRole("button", { name: (label) => label.startsWith("Open shop,") }));
  await screen.findByRole("region", { name: "Source shop" });
  await click(await screen.findByRole("button", { name: "Pipeline settings" }));
  return screen.findByRole("region", { name: "Pipeline of shop" });
}

function fact(page: HTMLElement, label: string): HTMLElement {
  const value = within(page).getByText(label, { selector: "dt" }).nextElementSibling;
  if (!(value instanceof HTMLElement)) throw new Error(`no value for ${label}`);
  return value;
}

function bar(): { word: string; detail: string; acts: string[]; buttons: string[] } {
  const el = document.querySelector(".os-actbar") as HTMLElement;
  return {
    word: el.querySelector(".os-actbar-word")?.textContent ?? "",
    detail: el.querySelector(".os-actbar-detail")?.textContent ?? "",
    acts: [...el.querySelectorAll(".os-actbar-acts button")].map((b) => b.textContent ?? ""),
    buttons: [...el.querySelectorAll(".os-actbar-acts .os-button")].map((b) => b.textContent ?? ""),
  };
}

beforeEach(() => {
  h.connection = null;
});

describe("pipeline settings", () => {
  it("reads what the pipeline is", async () => {
    mount(fakeConnection(seed()));
    const page = await openSettings();
    expect(within(page).getByRole("heading", { name: "Pipeline" })).toBeTruthy();
    expect(fact(page, "Check on GitHub").textContent).toBe("MemQL / shop");
    const repository = within(fact(page, "Repository")).getByRole("link", { name: "acme/shop" });
    expect(repository.getAttribute("href")).toBe("https://github.com/acme/shop");
    expect(repository.getAttribute("target")).toBe("_blank");
    expect(repository.getAttribute("rel")).toContain("noopener");
    expect(fact(page, "Default branch").textContent).toBe("main");
    expect(fact(page, "How changes arrive").textContent).toBe("GitHub sends each push and pull request");
    expect(fact(page, "Where steps run").textContent).toBe("Cluster only");
    expect(fact(page, "Allowed secrets").textContent).toBe("SHOP_TOKEN");
    expect(fact(page, "Installation").textContent).toBe("7");
    expect(fact(page, "Connected").textContent).toBe(formatMoment("2026-10-01T09:00:00Z"));
  });

  it("says None when the steps may read no secret", async () => {
    mount(fakeConnection(seed({ pipelines: [pipelineRow({ secretNames: [] })] })));
    const page = await openSettings();
    expect(fact(page, "Allowed secrets").textContent).toBe("None");
  });

  it("reads Checks on in the bar, with Disconnect beside Change, Change the one button", async () => {
    mount(fakeConnection(seed()));
    await openSettings();
    expect(bar().word).toBe("Checks on");
    expect(bar().detail).toBe("webhook, cluster");
    expect(bar().acts).toEqual(["Disconnect", "Change"]);
    expect(bar().buttons).toEqual(["Change"]);
  });

  it("asks before disconnecting, in the bar, and Keep takes the question back", async () => {
    const connection = fakeConnection(seed());
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Disconnect" }));
    expect(bar().word).toBe("Disconnect this pipeline?");
    expect(bar().detail).toBe("It opens no more runs. Its runs stay as history.");
    expect(bar().acts).toEqual(["Keep", "Disconnect"]);
    // The one button is the act asked about, dressed as what it is.
    expect(bar().buttons).toEqual(["Disconnect"]);
    expect(document.querySelector(".os-actbar-acts .os-button")?.getAttribute("data-tone")).toBe("danger");
    expect(connection.callsNamed("pipelinesDisconnect")).toEqual([]);
    await click(screen.getByRole("button", { name: "Keep" }));
    expect(bar().word).toBe("Checks on");
    expect(connection.callsNamed("pipelinesDisconnect")).toEqual([]);
  });

  it("sends the disconnect, then waits for the row rather than flipping it", async () => {
    const connection = fakeConnection(seed());
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Disconnect" }));
    await click(screen.getByRole("button", { name: "Disconnect" }));
    expect(connection.callsNamed("pipelinesDisconnect")).toEqual(['builtin pipelinesDisconnect(pipelineId: "pl-shop")']);
    // The cluster said yes; the pipeline row has not arrived yet.
    await waitFor(() => expect(bar().word).toBe("Disconnecting"));
    expect(bar().acts).toEqual([]);
    expect(screen.queryByText("Disconnected")).toBeNull();
    // It arrives on the feed, and the page reads it.
    await emit(connection, PIPELINE_CONCEPT, pipelineRow({ status: "disconnected" }));
    await waitFor(() => expect(bar().word).toBe("Disconnected"));
    expect(bar().acts).toEqual(["Connect again"]);
    expect(bar().buttons).toEqual(["Connect again"]);
  });

  it("renders a refusal in the page, with its copy, and leaves the pipeline as it was", async () => {
    const connection = fakeConnection(seed({ pipelinesDisconnectError: "capability_not_held: the caller does not hold execute on app:deployables/connect" }));
    mount(connection);
    const page = await openSettings();
    await click(screen.getByRole("button", { name: "Disconnect" }));
    await click(screen.getByRole("button", { name: "Disconnect" }));
    expect(await within(page).findByText("This needs a part of Deployables you have not been granted")).toBeTruthy();
    expect(within(page).getByText("the caller does not hold execute on app:deployables/connect")).toBeTruthy();
    expect(bar().word).toBe("Checks on");
    expect(bar().acts).toEqual(["Disconnect", "Change"]);
  });

  it("opens Runs refined to this source from All runs", async () => {
    const navigate = vi.fn();
    mount(fakeConnection(seed()), navigate);
    const page = await openSettings();
    await click(within(page).getByRole("button", { name: "All runs" }));
    expect(navigate).toHaveBeenCalledWith("runs", { fromContent: true });
  });

  it("opens the connect flow over the source for Change, and goes back to the source", async () => {
    mount(fakeConnection(seed()));
    const page = await openSettings();
    await click(within(page).getByRole("button", { name: "Back to shop" }));
    expect(await screen.findByRole("region", { name: "Source shop" })).toBeTruthy();
    await click(screen.getByRole("button", { name: "Pipeline settings" }));
    await screen.findByRole("region", { name: "Pipeline of shop" });
    await click(screen.getByRole("button", { name: "Change" }));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Pipeline of shop" })).toBeNull());
  });
});
