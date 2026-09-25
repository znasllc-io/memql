import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { ACCOUNT_CONCEPT } from "../../src/apps/accounts/rows";
import { click, emit, fakeConnection, rowsResult, withSession, type FakeConnection } from "./harness";

const SELF: Row = { id: "self", name: "Operator organization", status: "active" };
const ACME: Row = { id: "acme", name: "Acme", status: "active" };
const BETA: Row = { id: "beta", name: "Beta", status: "active" };
const cue = "Selected by default. Review account.";
afterEach(cleanup);

async function open(connection: FakeConnection, scope: { everyAccount?: boolean; accountIds?: string[] } = {}) {
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()}
    store={new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} })} />,
  { role: "owner", userId: "u-me", ...scope }));
  await click(await screen.findByRole("button", { name: /Add a deployable/ }));
  const region = await screen.findByRole("region", { name: "Add a deployable" });
  await click(within(region).getByRole("radio", { name: /Pushed by your CI/ }));
  return region;
}
function selection() { return screen.getByLabelText("Accounts").textContent; }
async function select(name: string) {
  await click(screen.getByLabelText("Accounts"));
  await click(await screen.findByRole("option", { name }));
}

describe("compose account default", () => {
  it("selects canonical self for an operator regardless of account row order", async () => {
    await open(fakeConnection({ accounts: [ACME, BETA, SELF] }));
    await waitFor(() => expect(selection()).toContain("Operator organization"));
    expect(screen.getByText(cue)).toBeTruthy();
  });

  it("defaults a restricted scope to its sole authorized active account, not role or row order", async () => {
    await open(fakeConnection({ accounts: [SELF, BETA, ACME] }), { everyAccount: false, accountIds: ["acme"] });
    await waitFor(() => expect(selection()).toContain("Acme"));
    expect(screen.getByText(cue)).toBeTruthy();
  });

  it.each([{ label: "multiple memberships", accountIds: ["acme", "beta"] }, { label: "no scope", accountIds: [] }])("does not invent a default for $label", async ({ accountIds }) => {
    await open(fakeConnection({ accounts: [BETA, SELF, ACME] }), { everyAccount: false, accountIds });
    await waitFor(() => expect(selection()).toContain("Choose an account"));
    expect(screen.queryByText(cue)).toBeNull();
  });

  it("waits for delayed account rows instead of defaulting to the first available organization", async () => {
    const connection = fakeConnection({ accounts: [] });
    let answer!: (value: ReturnType<typeof rowsResult>) => void;
    vi.spyOn(connection.query, "clientAccountsAll").mockImplementation(() => new Promise(resolve => { answer = resolve; }));
    await open(connection);
    expect(selection()).toContain("Choose an account");
    expect(screen.getByText("Loading accounts")).toBeTruthy();
    expect(screen.queryByText(cue)).toBeNull();
    await act(async () => answer(rowsResult([BETA, SELF])));
    await waitFor(() => expect(selection()).toContain("Operator organization"));
    expect(screen.getByText(cue)).toBeTruthy();
  });

  it("keeps an already selected default when a later feed update changes the available choices", async () => {
    const connection = fakeConnection({ accounts: [ACME] });
    await open(connection, { everyAccount: false, accountIds: ["acme", "beta"] });
    await waitFor(() => expect(selection()).toContain("Acme"));
    await emit(connection, ACCOUNT_CONCEPT, BETA);
    expect(selection()).toContain("Acme");
    expect(screen.getByText(cue)).toBeTruthy();
  });

  it("preserves a manual account through feed refresh and revisiting the source step", async () => {
    const connection = fakeConnection({ accounts: [SELF, ACME] });
    await open(connection);
    await select("Acme");
    expect(screen.queryByText(cue)).toBeNull();
    await emit(connection, ACCOUNT_CONCEPT, { ...ACME, name: "Acme renamed" });
    await waitFor(() => expect(selection()).toContain("Acme renamed"));
    await click(screen.getByRole("button", { name: /^Method / }));
    await click(screen.getByRole("radio", { name: /Pushed by your CI/ }));
    expect(selection()).toContain("Acme renamed");
    expect(screen.queryByText(cue)).toBeNull();
  });

  it("removes the default cue even when the person explicitly chooses self again", async () => {
    await open(fakeConnection({ accounts: [SELF, ACME] }));
    expect(screen.getByText(cue)).toBeTruthy();
    await select("Operator organization");
    expect(selection()).toContain("Operator organization");
    expect(screen.queryByText(cue)).toBeNull();
  });
});
