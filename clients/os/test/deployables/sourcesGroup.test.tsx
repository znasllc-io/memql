import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { credentialRow, fakeConnection, githubGrantRow, withSession, type FakeSeed } from "./harness";

function mount(seed: FakeSeed = {}, role = "owner", result?: string) {
  const connection = fakeConnection({ credentials: [githubGrantRow({ id: "own" }), credentialRow({ id: "stored-token" }), githubGrantRow({ id: "foreign", ownerUserId: "other", login: "other-user" })], ...seed });
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="settings" navigate={vi.fn()} askContext={vi.fn()}
    store={new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} })}
    intent={result ? { id: "setup-return", payload: { connect: { reason: result, section: "settings" } } } : undefined}
    consumeIntent={vi.fn()} />, { role, userId: "u-me" }));
  return connection;
}

afterEach(() => { cleanup(); h.connection = null; });

describe("Deployables Settings manages accounts independently of sources", () => {
  it("lists only the signed-in person's GitHub accounts without sources or tokens", async () => {
    const connection = mount();
    const settings = await screen.findByRole("region", { name: "GitHub accounts settings" });
    expect(await within(settings).findByRole("button", { name: "Manage GitHub account octocat" })).toBeTruthy();
    expect(within(settings).getByRole("list", { name: "GitHub accounts" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Source connections" })).toBeNull();
    expect(screen.queryByText("@other-user")).toBeNull();
    expect(screen.queryByText("stored-token")).toBeNull();
    expect(screen.getByRole("heading", { name: "Settings", level: 3 })).toBeTruthy();
    expect(within(settings).getByRole("heading", { name: "GitHub Accounts", level: 4 })).toBeTruthy();
    const add = within(settings).getByRole("button", { name: "Add GitHub account" });
    expect(add.classList.contains("os-icon-button")).toBe(true);
    expect(add.textContent).toBe("");
    for (const name of ["sourceCredentialRevoke", "sourceConnectionCreate", "sourceConnectionRemove", "createPackage", "packageArchive"]) expect(connection.callsNamed(name)).toHaveLength(0);
  });

  it("removes landing-page and list-density preferences", async () => {
    mount();
    await screen.findByRole("region", { name: "GitHub accounts settings" });
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByText("Open Deployables on")).toBeNull();
    expect(screen.queryByText("List density")).toBeNull();
  });

  it("preserves cluster-owner GitHub App setup", async () => {
    mount({ githubApp: { configured: false, canSetup: true } });
    const app = await screen.findByRole("region", { name: "GitHub" });
    expect(within(app).getByRole("button", { name: "Set up GitHub" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "GitHub accounts settings" })).toBeTruthy();
  });

  it("does not expose cluster setup to an ordinary member", async () => {
    mount({ githubApp: { configured: false, canSetup: false } }, "user");
    expect(await screen.findByText("This cluster is not linked to GitHub yet. Ask a cluster owner to set it up before choosing a repository.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "GitHub App" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
  });

  it("shows the GitHub setup return without creating a source or reading repositories", async () => {
    const connection = mount({}, "owner", "github_app_registered");
    expect(await screen.findByText("GitHub is set up for this cluster. Connect your account to choose its repositories.")).toBeTruthy();
    expect(screen.getByRole("list", { name: "GitHub accounts" })).toBeTruthy();
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
  });
});
