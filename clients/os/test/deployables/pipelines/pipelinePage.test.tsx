import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { PIPELINE_CONCEPT, RUN_CONCEPT } from "../../../src/apps/deployables/pipelines/rows";
import { formatMoment } from "../../../src/kit/format";
import { click, emit, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { chooseOption, openSelect } from "../../selectControl";
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
    // Primary last (DESIGN.md rule 12), the two ways to run side by side.
    expect(bar().acts).toEqual(["Connect again", "Other branch", "Run latest"]);
    expect(bar().buttons).toEqual(["Run latest"]);
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

  it("opens a manual rehearsal only for a disconnected pipeline and follows its run row", async () => {
    const connection = fakeConnection(seed({ pipelines: [pipelineRow({ status: "disconnected" })] }));
    mount(connection);
    const page = await openSettings();
    expect(bar().word).toBe("Disconnected");
    await click(screen.getByRole("button", { name: "Run latest" }));
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual(['builtin pipelinesRunLatest(pipelineId: "pl-shop")']);
    await waitFor(() => expect(bar().word).toBe("Manual run queued"));
    expect(bar().detail).toBe("main · 3f9c2ab");
    expect(within(page).getByRole("button", { name: "All runs" })).toBeTruthy();

    // The live collection applies the owner's read scope to graph events too.
    await emit(connection, RUN_CONCEPT, runRow({ id: "run-manual", ownerUserId: VIEWER, event: "manual", trigger: "manual", headBranch: "main", status: "queued" }), "NODE_CREATED");
    await waitFor(() => expect(screen.getByRole("region", { name: "Run Show the cart" })).toBeTruthy());
  });

  it("runs another branch, chosen from the repository's branches", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: ["main", "feature/cart"] } as unknown as Row },
      pipelinesRunLatestResult: { runId: "run-branch", sha: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b", branch: "feature/cart" } as unknown as Row,
    }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    expect(bar().word).toBe("Run a branch");
    expect(bar().detail).toBe("Runs its newest commit.");
    expect(bar().acts).toEqual(["Cancel"]);
    const trigger = await screen.findByLabelText("Branch to run");
    // The branches are read under the source's own GitHub grant.
    expect(connection.callsNamed("sourceProbe")).toEqual(['builtin sourceProbe(repoUrl: "https://github.com/acme/shop", credentialId: "cred-1")']);
    await waitFor(() => expect(document.activeElement).toBe(trigger));
    // The default branch is Run latest's; the list offers the others.
    expect(within(openSelect(trigger)).queryByRole("option", { name: "main" })).toBeNull();
    // Escape closes the open list, not the choice.
    fireEvent.keyDown(trigger, { key: "Escape" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(bar().word).toBe("Run a branch");
    chooseOption(trigger, "feature/cart");
    await waitFor(() => expect(bar().acts).toEqual(["Cancel", "Run"]));
    expect(bar().buttons).toEqual(["Run"]);
    await click(screen.getByRole("button", { name: "Run" }));
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual(['builtin pipelinesRunLatest(pipelineId: "pl-shop", branch: "feature/cart")']);
    await waitFor(() => expect(bar().word).toBe("Manual run queued"));
    expect(bar().detail).toBe("feature/cart · 9a8b7c6");
    expect(screen.queryByLabelText("Branch to run")).toBeNull();
  });

  it("takes a typed branch when the repository lists none, and sends nothing for a blank one", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: [] } as unknown as Row },
    }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    const field = await screen.findByLabelText("Branch to run");
    await waitFor(() => expect(document.activeElement).toBe(field));
    fireEvent.change(field, { target: { value: "   " } });
    expect(bar().acts).toEqual(["Cancel"]);
    fireEvent.keyDown(field, { key: "Enter" });
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual([]);
    fireEvent.change(field, { target: { value: "feature/typed" } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(connection.callsNamed("pipelinesRunLatest")).toEqual(['builtin pipelinesRunLatest(pipelineId: "pl-shop", branch: "feature/typed")']));
  });

  it("cancels without a call, by Cancel or by Escape", async () => {
    const connection = fakeConnection(seed({ pipelines: [pipelineRow({ status: "disconnected" })] }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(bar().word).toBe("Disconnected");
    expect(screen.queryByLabelText("Branch to run")).toBeNull();
    await click(screen.getByRole("button", { name: "Other branch" }));
    fireEvent.keyDown(await screen.findByLabelText("Branch to run"), { key: "Escape" });
    await waitFor(() => expect(bar().word).toBe("Disconnected"));
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual([]);
  });

  it("keeps the typed name and says why when the branch is refused", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: [] } as unknown as Row },
      pipelinesRunLatestError: 'pipeline_branch_invalid: "a..b" is not a branch name GitHub accepts',
    }));
    mount(connection);
    const page = await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    const field = await screen.findByLabelText("Branch to run");
    fireEvent.change(field, { target: { value: "a..b" } });
    await click(screen.getByRole("button", { name: "Run" }));
    expect(await within(page).findByText("That is not a branch name GitHub accepts")).toBeTruthy();
    expect((screen.getByLabelText("Branch to run") as HTMLInputElement).value).toBe("a..b");
    expect(bar().word).toBe("Run a branch");
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
