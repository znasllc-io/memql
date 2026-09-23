import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { DEPLOYMENT_CONCEPT } from "../../src/apps/deployables/packages/rows";
import { SOURCE_CONNECTION_CONCEPT } from "../../src/apps/deployables/sources/connections";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, emit, fakeConnection, githubGrantRow, probeReply, repositoriesReply, repositoryFixture, siteRow, rowsResult, sourceConnectionRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

const SELF: Row = { id: "self", name: "Operator organization", status: "active" };
const CLIENT: Row = { id: "acme", name: "Acme", status: "active" };
const GRANT = githubGrantRow({ id: "github-me", login: "octocat" });
const REPO = "https://github.com/acme/new-project";

function source(id: string, name: string, accountId = "self"): Row {
  return { id, name, accountId, ownerUserId: "u-colleague", sourceKind: "repo",
    repoUrl: `https://github.com/acme/${id}`, repoRef: "main", credentialId: "colleague-grant",
    artifactId: "", status: "active", deployedVersion: "", latestKnownVersion: "", updateAvailable: false,
    createdAt: "2026-09-01T10:00:00Z" };
}

const ALPHA = source("source-alpha", "Alpha source");
const BETA = source("source-beta", "Beta source");
const REPORT = { name: "saved project", formatVersion: 1, ok: true, problems: [], dslDomains: [],
  deployables: [{ name: "web", kind: "spa", path: "clients/web", output: "dist", prebuilt: true, buildPlan: "already built: dist" }] };

afterEach(() => { cleanup(); h.connection = null; });

async function open(seed: FakeSeed = {}, prepare?: (connection: FakeConnection) => void) {
  const connection = fakeConnection({ accounts: [SELF, CLIENT], packages: [ALPHA, BETA], ...connectedSeed(), ...seed });
  prepare?.(connection);
  h.connection = connection;
  const app = <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()}
    store={new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} })} />;
  const view = render(withSession(app, { userId: "u-me", role: "owner" }));
  await click(await screen.findByRole("button", { name: /Add a deployable/ }));
  const region = await screen.findByRole("region", { name: "Add a deployable" });
  await click(within(region).getByRole("radio", { name: /A repository/ }));
  return { connection, region, view,
    changeViewer: (userId: string) => view.rerender(withSession(app, { userId, role: "owner" })) };
}

function floorAct(name: string): HTMLButtonElement | null {
  const floor = document.querySelector(".os-actbar-acts");
  return floor ? within(floor as HTMLElement).queryByRole("button", { name }) : null;
}

async function forward(name: string) {
  await waitFor(() => expect(floorAct(name)).toBeTruthy());
  await click(floorAct(name)!);
}

function connectedSeed(): FakeSeed {
  return { credentials: [GRANT], githubApp: { configured: true },
    repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/new-project" }), repositoryFixture({ fullName: "acme/source-alpha" }), repositoryFixture({ fullName: "acme/source-beta" })] }),
    sourceProbe: { "github-me": probeReply({ branches: ["main", "release"] }) } };
}

async function addRepository(region: HTMLElement, name = "new-project") {
  await click(await within(region).findByRole("button", { name: /^acme Organization/ }));
  await click(await within(region).findByRole("button", { name: new RegExp(name) }));
}

function mintedId(connection: FakeConnection): string {
  const id = /packageId: "([^"]+)"/.exec(connection.callsNamed("createPackage")[0] ?? "")?.[1];
  expect(id).toBeTruthy();
  return id!;
}

describe("GitHub Sources in Add a deployable", () => {
  it("asks for Source before repository and MemQL ownership, then registers once at Analyze", async () => {
    const { region, connection } = await open();
    expect(within(region).queryByLabelText("Accounts")).toBeNull();
    expect(within(region).queryByRole("button", { name: /Alpha source/ })).toBeNull();
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    await addRepository(region);
    expect(within(region).getByLabelText("Accounts")).toBeTruthy();
    await click(within(region).getByLabelText("Which branch or tag to deploy"));
    await click(await screen.findByRole("option", { name: "release" }));
    fireEvent.input(within(region).getByLabelText("What this deployable is called"), { target: { value: "New project" } });
    await forward("Analyze");
    expect(connection.callsNamed("createPackage")).toHaveLength(1);
    const call = connection.callsNamed("createPackage")[0]!;
    for (const fragment of [`repoUrl: "${REPO}"`, 'repoRef: "release"', 'accountId: "self"', 'credentialId: "github-me"', 'sourceConnectionId: "source-github-me-i-acme"']) expect(call).toContain(fragment);
    expect(connection.callsNamed("packageDeploy")[0]).toContain(`packageId: "${mintedId(connection)}"`);
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
  });

  it("cancels adding a Source without creating a repository or deployment", async () => {
    const { region, connection } = await open();
    await click(within(region).getByRole("button", { name: "Add source" }));
    await click(within(region).getByRole("button", { name: "Cancel adding source" }));
    expect(within(region).getByRole("button", { name: /^acme Organization/ })).toBeTruthy();
    expect(connection.callsNamed("sourceConnectionCreate")).toHaveLength(0);
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
  });

  it("invalidates repository and branch when the selected binding is removed", async () => {
    const binding = sourceConnectionRow({ id: "source-github-me-i-acme", credentialId: "github-me" });
    const { region, connection } = await open({ sourceConnections: [binding] });
    await addRepository(region);
    await emit(connection, SOURCE_CONNECTION_CONCEPT, { ...binding, status: "removed" });
    await waitFor(() => expect(floorAct("Analyze")).toBeNull());
    expect(within(region).queryByLabelText("Which branch or tag to deploy")).toBeNull();
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
  });

  it("does not reuse a historical success as this flow's deployment", async () => {
    const old: Row = { id: "old-run", packageId: "source-alpha", status: "succeeded", report: REPORT,
      createdAt: "2026-09-01T10:00:00Z", finishedAt: "2026-09-01T10:01:00Z", deployables: [] };
    const { region, connection } = await open({ deployments: { "source-alpha": [old] } });
    await addRepository(region, "source-alpha");
    await waitFor(() => expect(connection.callsNamed("packageDeployments").length).toBeGreaterThan(0));
    await waitFor(() => expect(floorAct("Analyze")).toBeTruthy());
    expect(floorAct("Done")).toBeNull();
    expect(within(region).queryByText("clients/web")).toBeNull();
    await forward("Analyze");
    await emit(connection, DEPLOYMENT_CONCEPT, { ...old, createdAt: "2026-09-23T10:00:00Z" });
    expect(floorAct("Done")).toBeNull();
    expect(within(region).queryByText("clients/web")).toBeNull();
    await emit(connection, DEPLOYMENT_CONCEPT, { ...old, id: "dep-new", status: "awaiting_confirm", createdAt: "2026-09-23T10:01:00Z", finishedAt: "" }, "NODE_CREATED");
    expect(await within(region).findByText("clients/web")).toBeTruthy();
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
  });

  it("retries the selected source with a fresh run id and preserves skips for already placed apps", async () => {
    const outcome: Row = { deploymentId: "dep-new", status: "awaiting_confirm", awaitingConfirm: "true" };
    const { region, connection } = await open({
      packages: [{ ...ALPHA, declares: [{ name: "web", kind: "spa" }, { name: "api", kind: "service" }] }],
      sites: [siteRow({ id: "placed-web", packageId: "source-alpha", packageDeployableName: "web", accountId: "self" })],
      deployResult: outcome,
    });
    await addRepository(region, "source-alpha");
    await forward("Analyze");
    const firstCall = connection.callsNamed("packageDeploy")[0]!;
    expect(firstCall).toContain('packageId: "source-alpha"');
    expect(firstCall).toContain("web:");
    expect(firstCall).toContain("skip: true");
    await emit(connection, DEPLOYMENT_CONCEPT, { id: "dep-new", packageId: "source-alpha", status: "refused",
      report: null, error: { code: "package_manifest_missing", message: "Manifest unavailable on first attempt" },
      createdAt: "2026-09-23T10:00:00Z" }, "NODE_CREATED");
    expect(await within(region).findByText("Manifest unavailable on first attempt")).toBeTruthy();
    outcome["deploymentId"] = "dep-retry";
    await forward("Retry");
    expect(connection.callsNamed("packageDeploy")).toEqual([firstCall, firstCall]);
    await emit(connection, DEPLOYMENT_CONCEPT, { id: "dep-retry", packageId: "source-alpha", status: "awaiting_confirm",
      report: REPORT, createdAt: "2026-09-23T10:01:00Z" }, "NODE_CREATED");
    expect(await within(region).findByText("clients/web")).toBeTruthy();
    expect(within(region).queryByText("Manifest unavailable on first attempt")).toBeNull();
    expect(floorAct("Retry")).toBeNull();
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
  });

  it("waits for the placement feed and stays blocked when that read fails", async () => {
    let rejectRead!: (error: Error) => void;
    const { region, connection } = await open({}, connection => {
      vi.spyOn(connection.query, "sitesAll").mockImplementation(() => new Promise<ReturnType<typeof rowsResult>>((_resolve, reject) => { rejectRead = reject; }));
    });
    await addRepository(region, "source-alpha");
    expect(await within(region).findByText("Checking existing deployables…")).toBeTruthy();
    expect(floorAct("Analyze")).toBeNull();
    await act(async () => rejectRead(new Error("Placements cannot be read")));
    expect(await within(region).findByText("Existing deployables could not be read. Refresh Deployables before continuing.")).toBeTruthy();
    expect(floorAct("Analyze")).toBeNull();
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
  });

  it("does not analyze a source whose declared apps are all already placed", async () => {
    const { region, connection } = await open({
      packages: [{ ...ALPHA, declares: [{ name: "web", kind: "spa" }] }],
      sites: [siteRow({ id: "placed-web", packageId: "source-alpha", packageDeployableName: "web", accountId: "self" })],
    });
    await addRepository(region, "source-alpha");
    expect(await within(region).findByText("All apps from this repository already have deployables. Open them from Deployables.")).toBeTruthy();
    expect(floorAct("Analyze")).toBeNull();
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
    expect(connection.callsNamed("createPackage")).toHaveLength(0);
  });

  it.each(["unmount", "viewer change"])("ignores a deferred save after %s without selecting, deploying, or reseeding the replacement flow", async boundary => {
    let finishSave!: () => void;
    const pending = new Promise<void>(resolve => { finishSave = resolve; });
    const first = await open(connectedSeed(), connection => {
      const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
      vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, opts) => {
        const result = await execute(name, call, opts);
        if (name === "createPackage") await pending;
        return result;
      });
    });
    await addRepository(first.region);
    await forward("Analyze");
    expect(first.connection.callsNamed("createPackage")).toHaveLength(1);

    let replacement = first;
    if (boundary === "unmount") {
      first.view.unmount();
      replacement = await open(connectedSeed());
    } else {
      await act(async () => first.changeViewer("u-replacement"));
    }
    expect(floorAct("Analyze")).toBeNull();
    const firstReads = first.connection.callsNamed("packagesAll").length;
    const replacementReads = replacement.connection.callsNamed("packagesAll").length;
    await act(async () => finishSave());

    expect(floorAct("Analyze")).toBeNull();
    expect(within(replacement.region).queryByRole("button", { name: /new-project/ })).toBeNull();
    expect(first.connection.callsNamed("packageDeploy")).toHaveLength(0);
    expect(replacement.connection.callsNamed("packageDeploy")).toHaveLength(0);
    expect(first.connection.callsNamed("packagesAll")).toHaveLength(firstReads);
    expect(replacement.connection.callsNamed("packagesAll")).toHaveLength(replacementReads);
  });

  it("clears repository, ref and owning-account UI before listing another Source", async () => {
    const beta = sourceConnectionRow({ id: "source-beta-org", credentialId: "github-me", installationId: "i-beta", accountLogin: "beta" });
    const acme = sourceConnectionRow({ id: "source-github-me-i-acme", credentialId: "github-me" });
    const { region, connection } = await open({ sourceConnections: [acme, beta], repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/new-project" }), repositoryFixture({ fullName: "beta/portal", installationId: "i-beta" })] }) });
    await addRepository(region);
    await click(within(region).getByLabelText("Which branch or tag to deploy"));
    await click(await screen.findByRole("option", { name: "release" }));
    await click(within(region).getByRole("button", { name: "Source acme" }));
    await click(within(region).getByRole("button", { name: /^beta Organization/ }));
    expect(await within(region).findByRole("button", { name: /^portal main/ })).toBeTruthy();
    expect(within(region).queryByRole("button", { name: /^new-project main/ })).toBeNull();
    expect(within(region).queryByLabelText("Which branch or tag to deploy")).toBeNull();
    expect(within(region).queryByLabelText("Accounts")).toBeNull();
    expect(floorAct("Analyze")).toBeNull();
    expect(connection.callsNamed("sourceRepositories").at(-1)).toContain('connectionId: "source-beta-org"');
  });

  it("does not register twice when Analyze is pressed twice before the write returns", async () => {
    let finish!: () => void;
    const pending = new Promise<void>(resolve => { finish = resolve; });
    const { region, connection } = await open({}, connection => {
      const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
      vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, opts) => {
        const result = await execute(name, call, opts);
        if (name === "createPackage") await pending;
        return result;
      });
    });
    await addRepository(region);
    await waitFor(() => expect(floorAct("Analyze")).toBeTruthy());
    const analyze = floorAct("Analyze")!;
    await act(async () => { analyze.click(); analyze.click(); });
    expect(connection.callsNamed("createPackage")).toHaveLength(1);
    await act(async () => finish());
    expect(connection.callsNamed("packageDeploy")).toHaveLength(1);
  });
});
