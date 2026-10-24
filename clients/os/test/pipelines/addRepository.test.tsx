import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { AddRepository } from "../../src/apps/pipelines/AddRepository";
import { ACCOUNT_CONCEPT } from "../../src/apps/accounts/rows";
import { SourceConnectionsProvider } from "../../src/modules/connections/connections";
import { click, emit, fakeConnection, withSession, type FakeSeed } from "../deployables/harness";
import { readTheBar } from "../readTheBar";

const github: FakeSeed = {
  sourceConnections: [{ id: "source-one", ownerUserId: "u-me", credentialId: "grant-one", installationId: "42", providerAccountId: "100", accountLogin: "acme", accountType: "Organization", status: "active" }],
  repositories: { reason: "ok", repositories: [{ fullName: "acme/widget", name: "widget", owner: "acme", url: "https://github.com/acme/widget", defaultBranch: "main", installationId: "42", private: false, visibility: "public" }], installations: [], pending: [], nextPage: 0 },
};

function mount(seed: FakeSeed = {}, session: Parameters<typeof withSession>[1] = {}, unavailable = false) {
  const connection = fakeConnection({ ...github, ...seed });
  if (unavailable) {
    const execute = connection.query.executeNamed.bind(connection.query);
    connection.query.executeNamed = vi.fn((name, call, options) => {
      if (name === "clientAccountsAll") return Promise.reject(new Error("Organization lookup interrupted"));
      return execute(name, call, options);
    });
  }
  h.connection = connection;
  const added = vi.fn();
  render(withSession(<SourceConnectionsProvider><AddRepository back={vi.fn()} onAdded={added} onSettings={vi.fn()} /></SourceConnectionsProvider>, session));
  return { connection, added };
}

async function chooseRepository() {
  await click(await screen.findByRole("button", { name: /acme Organization/ }));
  await click(await screen.findByRole("button", { name: /widget/ }));
  await screen.findByRole("list", { name: "Repository organization" });
}

beforeEach(() => { h.connection = null; });

describe("repository organization ownership", () => {
  it("reviews the operator organization and registers its exact ownership without deploying", async () => {
    const { connection, added } = mount();
    await chooseRepository();
    expect(screen.queryByRole("button", { name: "Personal" })).toBeNull();
    expect(screen.getByText("Selected by default. Review this organization before continuing.")).toBeTruthy();
    expect(connection.callsNamed("packageSourceRegister")).toHaveLength(0);
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getAllByText("Operator organization").length).toBeGreaterThan(0);
    await readTheBar();
    await click(screen.getByRole("button", { name: "Add repository" }));
    await waitFor(() => expect(added).toHaveBeenCalledWith("pkg-registered-1"));
    const call = connection.callsNamed("packageSourceRegister")[0]!;
    expect(call).toContain('accountId: "self"');
    expect(call).toContain('sourceConnectionId: "source-one"');
    expect(call).toContain('credentialId: "grant-one"');
    expect(call).toContain('repoRef: "main"');
    expect(call).toContain("autoDeploy: false");
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
  });

  it("requires an explicit choice when membership does not resolve one organization", async () => {
    const { connection } = mount({ accounts: [{ id: "team-a", name: "Team A", status: "active" }, { id: "team-b", name: "Team B", status: "active" }] }, { everyAccount: false, accountIds: ["team-a", "team-b"] });
    await chooseRepository();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
    await click(within(screen.getByRole("list", { name: "Repository organization" })).getByRole("button", { name: "Team B" }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Add repository" }));
    await waitFor(() => expect(connection.callsNamed("packageSourceRegister")).toHaveLength(1));
    expect(connection.callsNamed("packageSourceRegister")[0]).toContain('accountId: "team-b"');
  });

  it("does not invent personal ownership when there are no active organizations", async () => {
    const { connection } = mount({ accounts: [{ id: "old", name: "Archived organization", status: "archived" }] });
    await chooseRepository();
    expect(screen.getByText(/No active organization is available/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Archived organization" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
    expect(connection.callsNamed("packageSourceRegister")).toHaveLength(0);
  });

  it("blocks registration when organization access cannot be read", async () => {
    const { connection } = mount({}, {}, true);
    await chooseRepository();
    await screen.findByText("Organizations could not be refreshed.");
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
    expect(connection.callsNamed("packageSourceRegister")).toHaveLength(0);
  });

  it("blocks a reviewed organization archived by another session without choosing a replacement", async () => {
    const { connection } = mount({ accounts: [{ id: "self", name: "Operator organization", status: "active" }, { id: "other", name: "Other organization", status: "active" }] });
    await chooseRepository();
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    await emit(connection, ACCOUNT_CONCEPT, { id: "self", name: "Operator organization", status: "archived" });
    await screen.findByText("The selected organization is unavailable.");
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
    expect(connection.callsNamed("packageSourceRegister")).toHaveLength(0);
    await readTheBar();
    await click(screen.getByRole("button", { name: "Back" }));
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    await click(within(screen.getByRole("list", { name: "Repository organization" })).getByRole("button", { name: "Other organization" }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Add repository" }));
    await waitFor(() => expect(connection.callsNamed("packageSourceRegister")).toHaveLength(1));
    expect(connection.callsNamed("packageSourceRegister")[0]).toContain('accountId: "other"');
  });
});
