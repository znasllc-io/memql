import { render, screen, waitFor } from "@testing-library/react";
import { act, useEffect } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { SourceConnectionsProvider, sourceConnectionFromRow, useSourceInstallations, useSourceConnections, SOURCE_CONNECTION_CONCEPT } from "../../src/apps/deployables/sources/connections";
import { credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { correlateConnectReturn, readConnectReturn, rememberConnectAttempt, scrubbedSearch } from "../../src/apps/deployables/sources/connectReturn";
import { builtinReply, click, emit, fakeConnection, githubGrantRow, sourceConnectionRow, withSession } from "./harness";

const ALICE = githubGrantRow({ id: "grant-alice", login: "alice" });
const ACME = sourceConnectionRow({ credentialId: "grant-alice" });
const BETA = sourceConnectionRow({ id: "source-beta", credentialId: "grant-bob", installationId: "i-beta", accountLogin: "beta" });
const installation = (id: string, account: string, accountType = "Organization") => ({ id, account, accountType, accountId: `provider-${id}`, repositorySelection: "selected", suspended: false });
const access = { "grant-alice": { reason: "ok", installations: [installation("i-acme", "acme"), installation("i-alice", "alice", "User")], pending: [{ login: "pending-org" }] }, "grant-bob": { reason: "ok", installations: [installation("i-beta", "beta")], pending: [] } } as Record<string, Row>;
function ConnectionFeed({ status = "active" }: { status?: string }) {
  const feed = useSourceConnections();
  useEffect(() => feed.observeCredentials([credentialFromRow({ ...ALICE, status })]), [status, feed.observeCredentials]);
  return <><output data-testid="rows">{feed.rows.map(row => row.id).join(",")}</output>
    <output data-testid="state">{feed.state}:{feed.error}</output>
    <output data-testid="revoked">{feed.revokedCredentialIds.join(",")}</output>
    <button onClick={() => feed.noteCredentialRevoked("grant-alice")}>Mark revoked</button></>;
}
beforeEach(() => { h.connection = null; sessionStorage.clear(); });

describe("source connection provider", () => {
  it("retains one caller-scoped feed and folds removal without admitting foreign rows", async () => {
    const connection = fakeConnection({ sourceConnections: [ACME, BETA, sourceConnectionRow({ id: "foreign", ownerUserId: "other" })] });
    h.connection = connection;
    render(withSession(<SourceConnectionsProvider><ConnectionFeed /></SourceConnectionsProvider>));
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("source-acme,source-beta"));
    expect(connection.callsNamed("sourceConnectionsMine")).toHaveLength(1);
    await emit(connection, SOURCE_CONNECTION_CONCEPT, { ...BETA, status: "removed" });
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("source-acme"));
    await emit(connection, SOURCE_CONNECTION_CONCEPT, { ...BETA, id: "foreign-new", ownerUserId: "other" });
    expect(screen.getByTestId("rows").textContent).toBe("source-acme");
  });

  it("retains a refusal as unavailable rather than a successful empty feed", async () => {
    h.connection = fakeConnection({ sourceConnectionsError: "forbidden: connections unavailable" });
    render(withSession(<SourceConnectionsProvider><ConnectionFeed /></SourceConnectionsProvider>));
    await waitFor(() => expect(screen.getByTestId("state").textContent).toContain("connections unavailable"));
  });

  it("does not clear a local revoke on stale active data, but accepts a confirmed revoked-to-active transition", async () => {
    h.connection = fakeConnection({ sourceConnections: [ACME] });
    const component = (status: string) => withSession(<SourceConnectionsProvider><ConnectionFeed status={status} /></SourceConnectionsProvider>);
    const view = render(component("active"));
    await click(screen.getByRole("button", { name: "Mark revoked" }));
    expect(screen.getByTestId("revoked").textContent).toBe("grant-alice");
    view.rerender(component("active"));
    expect(screen.getByTestId("revoked").textContent).toBe("grant-alice");
    view.rerender(component("revoked"));
    expect(screen.getByTestId("revoked").textContent).toBe("grant-alice");
    view.rerender(component("active"));
    await waitFor(() => expect(screen.getByTestId("revoked").textContent).toBe(""));
  });

  it("projects nested rows without losing installation ownership", () => {
    expect(sourceConnectionFromRow({ id: "source-acme", payload: { ...ACME } })).toMatchObject({ id: "source-acme", ownerUserId: "u-me", installationId: "i-acme" });
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

describe("GitHub callback identity correlation", () => {
  it.each(["settings", "deployables"])("returns a refused callback to its originating %s surface without trusting a credential", section => {
    rememberConnectAttempt("flow", "u-me", "", section);
    expect(correlateConnectReturn({ reason: "connect_state_invalid", section: "sources", credentialId: "unverified" }, "u-me"))
      .toEqual({ reason: "connect_state_invalid", section });
  });
  it("does not restore another person's return destination", () => {
    rememberConnectAttempt("flow", "someone-else", "", "settings");
    expect(correlateConnectReturn({ reason: "connect_state_invalid", section: "sources" }, "u-me").section).toBe("sources");
  });
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
