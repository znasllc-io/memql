import { act, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { packageFromRow } from "../../src/apps/deployables/packages/rows";
import { credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { SourceAccess } from "../../src/apps/deployables/sources/SourceAccess";
import { SourceConnectionsProvider, useSourceConnections } from "../../src/apps/deployables/sources/connections";
import { click, fakeConnection, githubGrantRow, siteRow, sourceConnectionRow, type, withSession, type FakeSeed } from "./harness";

const ALICE = githubGrantRow({ id: "grant-alice", login: "alice" });
const BOB = githubGrantRow({ id: "grant-bob", login: "bob" });
const SOURCE = { id: "pkg-review", ownerUserId: "u-me", accountId: "self", name: "Website", sourceKind: "repo", repoUrl: "https://github.com/acme/website", repoRef: "main", credentialId: "grant-alice", sourceConnectionId: "binding-alice", status: "active" };
const BINDINGS = [sourceConnectionRow({ id: "binding-alice", credentialId: "grant-alice" }), sourceConnectionRow({ id: "binding-bob", credentialId: "grant-bob" })];
function connectionFor(seed: FakeSeed = {}) {
  const connection = fakeConnection({ sites: [], packages: [SOURCE], credentials: [ALICE, BOB], sourceConnections: BINDINGS, ...seed });
  h.connection = connection;
  return connection;
}
function mountCatalog(sectionId = "sources") {
  return render(withSession(<DeployablesApp sectionId={sectionId} navigate={vi.fn()} askContext={vi.fn()} store={new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} })} />));
}
function sourcesHeading() {
  return screen.getByRole("heading", { name: "Sources" }).closest<HTMLElement>(".os-head")!;
}
function RevokeBob() {
  const connections = useSourceConnections();
  return <button onClick={() => connections.noteCredentialRevoked("grant-bob")}>Record Bob disconnect</button>;
}

describe("Sources catalog review regressions", () => {
  it("retains a removed source's parked Review through its existing deployable", async () => {
    const report = { name: "Website", formatVersion: 1, deployables: [{ name: "web", kind: "spa", path: "clients/web", buildPlan: "already built: dist", output: "dist", prebuilt: true }], dslDomains: [], problems: [], ok: true };
    const connection = connectionFor({ packages: [{ ...SOURCE, sourceRemoved: true }], sites: [siteRow({ id: "site-web", hostname: "web.memql.example.com", packageId: SOURCE.id, packageDeployableName: "web" })], awaitingConfirm: [{ id: "run-review", packageId: SOURCE.id, sourceVersion: "abc123", status: "awaiting_confirm", report, requestedBy: "u-me", startedAt: "2026-09-23T10:00:00Z", createdAt: "2026-09-23T10:00:00Z" }] });
    mountCatalog("deployables");
    await click((await screen.findByText("web")).closest("button"));
    await click(await screen.findByRole("button", { name: "Open acme/website at main" }));
    const detail = await screen.findByRole("region", { name: "Source Website" });
    expect(within(detail).getByText(/Removed from Sources/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Review" }));
    await screen.findByRole("region", { name: "Deploy Website" });
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
    expect(connection.callsNamed("setPackageSourceRemoved")).toHaveLength(0);
  });

  it("withholds a settled count and empty search while provenance is loading, then resolves the match", async () => {
    const connection = connectionFor();
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
      if (call === "query sourceCredentialsMine()") await pending;
      return original(name, call, options);
    });
    mountCatalog();
    await screen.findByRole("button", { name: /^Open Website,/ });
    await click(screen.getByRole("button", { name: "Find sources" }));
    await type(screen.getByLabelText("Search") as HTMLInputElement, "@alice");
    await screen.findByText("Reading sources and GitHub access…");
    expect(sourcesHeading().querySelector(".os-head-meta")).toBeNull();
    expect(screen.queryByText(/No matching sources|No sources yet/)).toBeNull();
    await act(async () => release());
    await screen.findByRole("button", { name: /^Open Website, @alice/ });
    await waitFor(() => expect(sourcesHeading().querySelector(".os-head-meta")?.textContent).toBe("1"));
  });

  it("shows unavailable provenance without claiming a complete zero-result search", async () => {
    connectionFor({ sourceConnectionsError: "forbidden: source metadata unavailable" });
    mountCatalog();
    await screen.findByText("Some GitHub provenance could not be refreshed.");
    await click(screen.getByRole("button", { name: "Find sources" }));
    await type(screen.getByLabelText("Search") as HTMLInputElement, "no-match");
    await screen.findByText("Source provenance is unavailable. Refresh sources to finish this reading.");
    expect(sourcesHeading().querySelector(".os-head-meta")).toBeNull();
    expect(screen.queryByText(/No matching sources|No sources yet/)).toBeNull();
  });

  it("changes credential and binding together and rejects a just-disconnected option before broadcast", async () => {
    const connection = connectionFor();
    render(withSession(<SourceConnectionsProvider><SourceAccess pkg={packageFromRow(SOURCE)} credentials={[ALICE, BOB].map(credentialFromRow)} /><RevokeBob /></SourceConnectionsProvider>));
    await waitFor(() => expect(screen.getByLabelText("Account and organization").closest("fieldset")?.disabled).toBe(false));
    await click(screen.getByLabelText("Account and organization"));
    await click(await screen.findByRole("option", { name: "@bob · acme" }));
    await click(screen.getByRole("button", { name: "Save access" }));
    await waitFor(() => expect(connection.callsNamed("updatePackageSource")).toHaveLength(1));
    const call = connection.callsNamed("updatePackageSource")[0]!;
    expect(call).toContain('packageId: "pkg-review"');
    expect(call).toContain('credentialId: "grant-bob"');
    expect(call).toContain('sourceConnectionId: "binding-bob"');
    await click(screen.getByRole("button", { name: "Record Bob disconnect" }));
    expect((screen.getByRole("button", { name: "Save access" }) as HTMLButtonElement).disabled).toBe(true);
    await click(screen.getByLabelText("Account and organization"));
    expect(screen.queryByRole("option", { name: "@bob · acme" })).toBeNull();
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
    expect(connection.callsNamed("sourceConnectionRemove")).toHaveLength(0);
  });
});
