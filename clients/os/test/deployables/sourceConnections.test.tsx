import { render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { SourceConnections } from "../../src/apps/deployables/sources/SourceConnections";
import { SourceConnectionsProvider, sourceConnectionFromRow, useSourceInstallations, SOURCE_CONNECTION_CONCEPT } from "../../src/apps/deployables/sources/connections";
import { credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { correlateConnectReturn, readConnectReturn, rememberConnectAttempt, scrubbedSearch } from "../../src/apps/deployables/sources/connectReturn";
import { builtinReply, click, emit, fakeConnection, githubGrantRow, sourceConnectionRow, withSession, type FakeSeed } from "./harness";

const ALICE = githubGrantRow({ id: "grant-alice", login: "alice" });
const BOB = githubGrantRow({ id: "grant-bob", login: "bob" });
const ACME = sourceConnectionRow({ credentialId: "grant-alice" });
const BETA = sourceConnectionRow({ id: "source-beta", credentialId: "grant-bob", installationId: "i-beta", accountLogin: "beta" });
const installation = (id: string, account: string, accountType = "Organization") => ({ id, account, accountType, accountId: `provider-${id}`, repositorySelection: "selected", suspended: false });
const access = { "grant-alice": { reason: "ok", installations: [installation("i-acme", "acme"), installation("i-alice", "alice", "User")], pending: [{ login: "pending-org" }] }, "grant-bob": { reason: "ok", installations: [installation("i-beta", "beta")], pending: [] } } as Record<string, Row>;
function mount(seed: FakeSeed = {}, mode: "choose" | "manage" = "manage", onChoose = vi.fn()) {
  const credentials = seed.credentials ?? [ALICE, BOB];
  const connection = fakeConnection({ credentials, sourceConnections: [ACME, BETA], sourceInstallations: access, ...seed });
  h.connection = connection;
  const view = render(withSession(<SourceConnectionsProvider><SourceConnections mode={mode} credentials={credentials.map(credentialFromRow)} onChoose={onChoose} credentialFeed={{ state: "live", error: "", retry: vi.fn() }} /></SourceConnectionsProvider>));
  return { connection, view, onChoose };
}
beforeEach(() => { h.connection = null; sessionStorage.clear(); });

describe("GitHub source connections", () => {
  it("retains one feed and offers both identities' sources, excluding foreign ownership", async () => {
    const { connection, onChoose } = mount({ sourceConnections: [ACME, BETA, sourceConnectionRow({ id: "foreign", ownerUserId: "other", accountLogin: "foreign" })] }, "choose");
    const list = await screen.findByRole("list", { name: "Sources" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    await click(within(list).getByRole("button", { name: /^beta / }));
    expect(onChoose).toHaveBeenCalledWith(expect.objectContaining({ id: "source-beta", credentialId: "grant-bob" }));
    expect(screen.queryByText("foreign")).toBeNull();
    expect(connection.callsNamed("sourceConnectionsMine")).toHaveLength(1);
    await emit(connection, SOURCE_CONNECTION_CONCEPT, { ...BETA, status: "removed" });
    await waitFor(() => expect(within(list).queryByRole("button", { name: /^beta / })).toBeNull());
  });
  it("adds a named installation under the explicitly chosen GitHub identity without creating a repository", async () => {
    const { connection } = mount({ sourceConnections: [] });
    await click(screen.getByRole("button", { name: "Add source" }));
    const identities = screen.getByRole("list", { name: "GitHub accounts" });
    expect(within(identities).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: "Save source" })).toBeNull();
    await click(within(identities).getByRole("button", { name: /@alice/ }));
    await click(await screen.findByRole("button", { name: "acme Organization" }));
    expect(screen.getByText(/pending-org is awaiting/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Save source" }));
    await screen.findByRole("list", { name: "Sources" });
    expect(connection.callsNamed("sourceConnectionCreate")).toEqual(['builtin sourceConnectionCreate(credentialId: "grant-alice", installationId: "i-acme")']);
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
    expect(connection.callsNamed("sourceCredentialCreate")).toHaveLength(0);
  });
  it("removes only the selected binding after confirmation, preserving other sources and the shared grant", async () => {
    const { connection } = mount();
    const list = await screen.findByRole("list", { name: "Sources" });
    const first = within(list).getAllByRole("listitem")[0]!;
    await click(within(first).getByRole("button", { name: "Remove source acme" }));
    expect(within(first).getByText(/Existing repositories and deployables remain/)).toBeTruthy();
    await click(within(first).getByRole("button", { name: "Cancel" }));
    expect(connection.callsNamed("sourceConnectionRemove")).toHaveLength(0);
    await click(within(first).getByRole("button", { name: "Remove source acme" }));
    await click(within(first).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(within(list).queryByText("acme")).toBeNull());
    expect(within(list).getByText("beta")).toBeTruthy();
    expect(connection.callsNamed("sourceConnectionRemove")).toEqual(['builtin sourceConnectionRemove(connectionId: "source-acme")']);
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
    expect(connection.callsNamed("packageArchive")).toHaveLength(0);
  });
  it("keeps a refused removal visible and retryable", async () => {
    const { connection } = mount({ sourceConnectionRemoveError: "write_refused: Source was not removed." });
    const first = within(await screen.findByRole("list", { name: "Sources" })).getAllByRole("listitem")[0]!;
    await click(within(first).getByRole("button", { name: "Remove source acme" }));
    await click(within(first).getByRole("button", { name: "Remove" }));
    expect(await within(first).findByText("Source was not removed.")).toBeTruthy();
    expect(within(first).getByText("acme")).toBeTruthy();
    expect(within(first).getByRole("button", { name: "Remove" })).toBeTruthy();
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
  });
  it("does not call unavailable sources empty or show a false zero", async () => {
    mount({ sourceConnections: [], sourceConnectionsError: "read denied" });
    expect(await screen.findByText("Sources could not be read.")).toBeTruthy();
    expect(screen.queryByText(/No sources yet/)).toBeNull();
    expect(screen.queryByText("0")).toBeNull();
    expect(screen.getByRole("button", { name: "Refresh sources" })).toBeTruthy();
  });
  it("keeps failed source creation in place and exposes no token entry", async () => {
    mount({ sourceConnections: [], sourceConnectionCreateError: "source_connection_unavailable: Access changed. Choose another source." });
    await click(screen.getByRole("button", { name: "Add source" }));
    await click(screen.getByRole("button", { name: "@alice GitHub account Connected" }));
    await click(await screen.findByRole("button", { name: "acme Organization" }));
    await click(screen.getByRole("button", { name: "Save source" }));
    expect(await screen.findByText("Access changed. Choose another source.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save source" })).toBeTruthy();
    expect(screen.queryByLabelText(/access token/i)).toBeNull();
  });
  it("disconnects only the named GitHub identity and immediately disables its sources before a feed event", async () => {
    const { connection } = mount({ credentialRevokeRemote: false });
    const identities = await screen.findByRole("list", { name: "Connected GitHub accounts" });
    const alice = within(identities).getAllByRole("listitem")[0]!;
    await click(within(alice).getByRole("button", { name: "Disconnect GitHub" }));
    expect(within(alice).getByText(/personal authorization is revoked here and at GitHub/)).toBeTruthy();
    await click(within(alice).getByRole("button", { name: "Cancel" }));
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
    await click(within(alice).getByRole("button", { name: "Disconnect GitHub" }));
    await click(within(alice).getByRole("button", { name: "Disconnect" }));
    await within(alice).findByText("Disconnected");
    expect(await within(alice).findByText(/GitHub did not confirm revocation/)).toBeTruthy();
    const sources = screen.getByRole("list", { name: "Sources" });
    expect(within(sources).getByText("Reconnect needed")).toBeTruthy();
    expect(within(identities).getAllByRole("listitem")[1]!.textContent).toContain("Connected");
    expect(connection.callsNamed("sourceCredentialRevoke")).toEqual(['builtin sourceCredentialRevoke(credentialId: "grant-alice")']);
    expect(connection.callsNamed("sourceConnectionRemove")).toHaveLength(0);
  });
  it("offers authorized GitHub setup inside Add source, preserving refusal and cancellation", async () => {
    const { connection } = mount({ credentials: [], sourceConnections: [], githubApp: { configured: false, canSetup: true }, appSetupReason: "github_app_setup_failed" });
    await click(screen.getByRole("button", { name: "Add source" }));
    await click(await screen.findByRole("button", { name: "Set up GitHub" }));
    expect(await screen.findByText("GitHub could not be set up")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add GitHub account" })).toBeNull();
    await click(screen.getByRole("button", { name: "Cancel adding source" }));
    expect(screen.getByRole("button", { name: "Add source" })).toBeTruthy();
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
  });
  it("explains missing app setup without offering it to a non-owner", async () => {
    mount({ credentials: [], sourceConnections: [], githubApp: { configured: false, canSetup: false } });
    await click(screen.getByRole("button", { name: "Add source" }));
    expect(await screen.findByText(/Ask a cluster owner/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add GitHub account" })).toBeNull();
  });
  it("projects nested wire rows without losing installation ownership", () => {
    expect(sourceConnectionFromRow({ id: "source-acme", payload: { ...ACME } })).toEqual(expect.objectContaining({ id: "source-acme", ownerUserId: "u-me", installationId: "i-acme" }));
  });
});

function Reading({ credentialId }: { credentialId: string }) {
  const read = useSourceInstallations(credentialId);
  return <div>{read.installations.map(row => <span key={row.id}>{row.login}</span>)}</div>;
}
it("ignores an installation reading that completes after switching identities", async () => {
  const connection = fakeConnection(); h.connection = connection;
  let resolveAlice!: (value: ReturnType<typeof builtinReply>) => void;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (_name, call) => {
    if (call.includes('"grant-alice"')) return await new Promise(resolve => { resolveAlice = resolve; });
    return builtinReply("sourceInstallations", [access["grant-bob"]!]);
  });
  const view = render(withSession(<Reading credentialId="grant-alice" />));
  await waitFor(() => expect(resolveAlice).toBeTruthy());
  view.rerender(withSession(<Reading credentialId="grant-bob" />));
  await screen.findByText("beta");
  await act(async () => resolveAlice(builtinReply("sourceInstallations", [access["grant-alice"]!])));
  expect(screen.queryByText("acme")).toBeNull();
  expect(screen.getByText("beta")).toBeTruthy();
});

it("does not save twice or close Add source when an in-flight save loses its grant", async () => {
  const connection = fakeConnection({ credentials: [ALICE], sourceConnections: [], sourceInstallations: access });
  h.connection = connection;
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  let finish!: (reply: ReturnType<typeof builtinReply>) => void;
  let creates = 0;
  vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, options) => {
    if (name === "sourceConnectionCreate") { creates++; return new Promise(resolve => { finish = resolve; }); }
    return original(name, call, options);
  });
  const panel = (status: string) => withSession(<SourceConnectionsProvider><SourceConnections mode="manage" credentials={[credentialFromRow({ ...ALICE, status })]} /></SourceConnectionsProvider>);
  const view = render(panel("active"));
  await click(screen.getByRole("button", { name: "Add source" }));
  await click(screen.getByRole("button", { name: "@alice GitHub account Connected" }));
  await click(await screen.findByRole("button", { name: "acme Organization" }));
  const save = screen.getByRole("button", { name: "Save source" });
  act(() => { save.click(); save.click(); });
  expect(creates).toBe(1);
  view.rerender(panel("revoked"));
  await act(async () => finish(builtinReply("sourceConnectionCreate", [{ connectionId: "confirmed-source", status: "active" }])));
  expect(screen.getByRole("button", { name: "Cancel adding source" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Save source" })).toBeNull();
  expect(screen.getByRole("button", { name: "Reconnect @alice" })).toBeTruthy();
});

describe("GitHub callback identity correlation", () => {
  it("returns to Sources and scrubs identity metadata without disturbing auth parameters", () => {
    const search = "?github=connected&githubCredentialId=grant-bob&githubFlowId=flow&code=auth-code";
    expect(readConnectReturn(search)).toEqual({ reason: "connected", section: "sources", credentialId: "grant-bob", flowId: "flow" });
    expect(scrubbedSearch(search)).toBe("?code=auth-code");
  });
  it("accepts only the matching viewer and targeted reconnect, once", () => {
    const result = { reason: "reconnected", section: "sources", credentialId: "grant-alice", flowId: "flow" };
    rememberConnectAttempt("flow", "u-me", "grant-alice");
    expect(correlateConnectReturn(result, "u-me")).toEqual(result);
    expect(correlateConnectReturn(result, "u-me").reason).toBe("connect_state_invalid");
    rememberConnectAttempt("flow", "u-other", "grant-alice");
    expect(correlateConnectReturn(result, "u-me").reason).toBe("connect_state_invalid");
    rememberConnectAttempt("flow", "u-me", "grant-bob");
    expect(correlateConnectReturn(result, "u-me").reason).toBe("connect_state_invalid");
  });
});
