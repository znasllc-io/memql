import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { ConnectionsPanel } from "../../src/modules/connections/ConnectionsPanel";
import { SourceConnectionsProvider } from "../../src/modules/connections/connections";
import { SharedCredentialsProvider } from "../../src/modules/connections/useSourceCredentials";
import { SharedPackagesProvider } from "../../src/apps/deployables/packages/usePackages";
import { SOURCE_CREDENTIAL_CONCEPT } from "../../src/apps/deployables/sources/rows";
import { correlateConnectReturn, readConnectReturn, rememberConnectAttempt, returnPathFor } from "../../src/apps/deployables/sources/connectReturn";
import { emit, fakeConnection, githubGrantRow, withSession } from "../deployables/harness";

describe("connections shared across apps", () => {
  it("uses one credential subscription and updates both settings surfaces from the same record", async () => {
    const grant = githubGrantRow({ id: "shared", login: "shared-user" });
    const connection = fakeConnection({ credentials: [grant] }); h.connection = connection;
    render(withSession(<SharedPackagesProvider><SharedCredentialsProvider><SourceConnectionsProvider>
      <div data-testid="global"><ConnectionsPanel appId="settings" /></div>
      <div data-testid="deployables"><ConnectionsPanel appId="deployables" /></div>
    </SourceConnectionsProvider></SharedCredentialsProvider></SharedPackagesProvider>));
    for (const surface of ["global", "deployables"]) expect(await within(screen.getByTestId(surface)).findByRole("button", { name: "Manage GitHub account shared-user" })).toBeTruthy();
    expect(connection.callsNamed("sourceCredentialsMine")).toHaveLength(1);
    await emit(connection, SOURCE_CREDENTIAL_CONCEPT, { ...grant, status: "revoked" });
    for (const surface of ["global", "deployables"]) expect(await within(screen.getByTestId(surface)).findByText("No GitHub accounts connected")).toBeTruthy();
  });
  it("preserves the global settings destination through successful and refused GitHub callbacks", () => {
    const path = returnPathFor("connections", "settings");
    expect(path).toBe("/?connect=connections&connectApp=settings");
    rememberConnectAttempt("flow", "viewer", "", "connections", "settings");
    const reply = readConnectReturn(path.slice(1) + "&github=connected&githubFlowId=flow&githubCredentialId=grant")!;
    expect(correlateConnectReturn(reply, "viewer")).toMatchObject({ appId: "settings", section: "connections", reason: "connected" });
    rememberConnectAttempt("flow", "viewer", "", "connections", "settings");
    expect(correlateConnectReturn({ reason: "connect_state_invalid", section: "sources" }, "viewer")).toMatchObject({ appId: "settings", section: "connections" });
  });
});
