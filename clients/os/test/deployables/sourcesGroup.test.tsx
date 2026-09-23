import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { credentialRow, fakeConnection, githubGrantRow, withSession, type FakeSeed } from "./harness";

const guidance = "Manage saved sources in Sources. Add a deployable to connect a GitHub account and choose a repository.";

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

describe("Deployables Settings has no competing source list", () => {
  it("keeps source management and creation guidance without account, token or binding lists", async () => {
    const connection = mount();
    const settings = await screen.findByRole("region", { name: "Source settings" });
    expect(within(settings).getByText(guidance)).toBeTruthy();
    expect(within(settings).queryByRole("list")).toBeNull();
    expect(screen.queryByRole("region", { name: "Source connections" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Other people's connections" })).toBeNull();
    expect(screen.queryByText("@other-user")).toBeNull();
    expect(screen.queryByRole("button", { name: /Add source|Add GitHub account|Disconnect GitHub|Revoke/ })).toBeNull();
    for (const name of ["sourceCredentialRevoke", "sourceConnectionCreate", "sourceConnectionRemove", "createPackage", "packageArchive"]) expect(connection.callsNamed(name)).toHaveLength(0);
  });

  it("offers the single Sources destination among defaults without Repositories or Accounts", async () => {
    mount();
    const choices = await screen.findByRole("radiogroup", { name: "Default section" });
    expect(within(choices).getAllByRole("radio").map(choice => choice.textContent)).toEqual(["Overview", "Deployables", "Sources", "Logs", "Settings"]);
  });

  it("preserves cluster-owner GitHub App setup", async () => {
    mount({ githubApp: { configured: false, canSetup: true } });
    const app = await screen.findByRole("region", { name: "GitHub" });
    expect(within(app).getByRole("button", { name: "Set up GitHub" })).toBeTruthy();
    expect(screen.getByText(guidance)).toBeTruthy();
  });

  it("does not expose cluster setup to an ordinary member", async () => {
    mount({ githubApp: { configured: false, canSetup: false } }, "user");
    expect(await screen.findByText(guidance)).toBeTruthy();
    expect(screen.queryByRole("region", { name: "GitHub App" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
  });

  it("shows the GitHub setup return without creating a connection or opening another list", async () => {
    const connection = mount({}, "owner", "github_app_registered");
    expect(await screen.findByText("GitHub is set up for this cluster. Connect your account to choose its repositories.")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Connected GitHub accounts" })).toBeNull();
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
  });
});
