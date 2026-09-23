import { act, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { PACKAGE_CONCEPT, packageFromRow } from "../../src/apps/deployables/packages/rows";
import { SOURCE_CREDENTIAL_CONCEPT, credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { sourceConnectionFromRow } from "../../src/apps/deployables/sources/connections";
import { sourceRecord } from "../../src/apps/deployables/sources/sourceRecord";
import { RemoveSource } from "../../src/apps/deployables/sources/RemoveSource";
import { builtinReply, click, emit, fakeConnection, githubGrantRow, sourceConnectionRow, withSession, type FakeSeed } from "./harness";

const ALICE = githubGrantRow({ id: "alice", login: "alice" });
const BOB = githubGrantRow({ id: "bob", login: "bob" });
const A = { id: "pkg-a", ownerUserId: "u-me", accountId: "self", name: "Website", sourceKind: "repo", repoUrl: "https://github.com/acme/website", repoRef: "main", credentialId: "alice", sourceConnectionId: "binding-a", status: "active" };
const B = { ...A, id: "pkg-b", credentialId: "bob", sourceConnectionId: "binding-b" };
const bindings = [sourceConnectionRow({ id: "binding-a", credentialId: "alice" }), sourceConnectionRow({ id: "binding-b", credentialId: "bob" }), sourceConnectionRow({ id: "unused", accountLogin: "unconfigured" })];
function mount(seed: FakeSeed = {}) {
  const connection = fakeConnection({ sites: [], packages: [A, B], credentials: [ALICE, BOB], sourceConnections: bindings, ...seed });
  h.connection = connection;
  const view = render(withSession(<DeployablesApp sectionId="sources" navigate={vi.fn()} askContext={vi.fn()} store={new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} })} />));
  return { connection, view };
}

describe("configured Sources catalog", () => {
  it("lists complete configured paths, distinguishes identities and retains unknown legacy identity", async () => {
    mount({ packages: [A, B, { ...A, id: "legacy", name: "Legacy", credentialId: "unreadable", sourceConnectionId: "" }, { ...A, id: "hidden", name: "Removed", sourceRemoved: true }] });
    const list = await screen.findByRole("list", { name: "Sources in this cluster" });
    expect(within(list).getByRole("button", { name: /Open Website, @alice · acme · acme\/website · main/ })).toBeTruthy();
    expect(within(list).getByRole("button", { name: /Open Website, @bob · acme · acme\/website · main/ })).toBeTruthy();
    expect(within(list).getByText("Unknown GitHub identity · acme · acme/website · main")).toBeTruthy();
    expect(within(list).queryByText("Removed")).toBeNull();
    expect(screen.queryByText("unconfigured")).toBeNull();
    expect(screen.queryByRole("list", { name: "Connected GitHub accounts" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Add (source|GitHub account|a deployable)/ })).toBeNull();
    expect(document.querySelector(".os-head-meta")?.textContent).toBe("3");
    const refresh = screen.getByRole("button", { name: "Refresh sources" });
    expect(refresh.closest(".os-head")?.querySelector(".os-head-actions")?.lastElementChild).toBe(refresh);
    expect(screen.getAllByRole("button", { name: /Refresh/ })).toHaveLength(1);
  });
  it("updates provenance and removal from authorized live feeds", async () => {
    const { connection } = mount();
    await screen.findByRole("button", { name: /Open Website, @alice/ });
    await emit(connection, SOURCE_CREDENTIAL_CONCEPT, { ...ALICE, login: "alice-renamed" });
    await screen.findByRole("button", { name: /Open Website, @alice-renamed/ });
    await emit(connection, PACKAGE_CONCEPT, { ...A, sourceRemoved: true });
    await waitFor(() => expect(screen.queryByRole("button", { name: /Open Website, @alice-renamed/ })).toBeNull());
    expect(screen.getByRole("button", { name: /Open Website, @bob/ })).toBeTruthy();
  });
  it("confirms catalog removal and calls only the narrow visibility mutation", async () => {
    const { connection } = mount({ packages: [A] });
    const remove = await screen.findByRole("button", { name: "Remove source Website" });
    expect(remove.title).toBe("Remove source Website");
    expect(remove.querySelector(".lucide-trash2, .lucide-trash-2")).toBeTruthy();
    await click(remove);
    const dialog = screen.getByRole("dialog", { name: "Remove Website?" });
    expect(within(dialog).getByText(/Existing deployables and automatic updates keep running/)).toBeTruthy();
    await click(within(dialog).getByRole("button", { name: "Remove source" }));
    await waitFor(() => expect(connection.callsNamed("setPackageSourceRemoved")).toHaveLength(1));
    expect(connection.callsNamed("setPackageSourceRemoved")[0]).toContain('packageId: "pkg-a"');
    for (const name of ["packageArchive", "sourceCredentialRevoke", "sourceConnectionRemove", "deactivateDeployable"]) expect(connection.callsNamed(name)).toHaveLength(0);
  });
  it("does not offer another owner's removal or source access edit", async () => {
    mount({ packages: [{ ...A, ownerUserId: "other" }] });
    await click(await screen.findByRole("button", { name: /Open Website,/ }));
    expect(screen.queryByRole("button", { name: "Remove source Website" })).toBeNull();
    expect(screen.queryByRole("region", { name: "GitHub source access" })).toBeNull();
  });
  it("joins provenance only when package, binding and credential ownership agree", () => {
    const row = packageFromRow({ ...A, ownerUserId: "other" });
    const record = sourceRecord(row, [credentialFromRow(ALICE)], bindings.map(sourceConnectionFromRow));
    expect(record.identity).toBe("Unknown GitHub identity");
    expect(record.binding).toBeUndefined();
    expect(record.target).toBe("acme");
  });
  it("ignores removal completion after its detail has closed", async () => {
    const connection = fakeConnection(); h.connection = connection;
    let resolve!: (value: ReturnType<typeof builtinReply>) => void;
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async () => new Promise(done => { resolve = done; }));
    const onRemoved = vi.fn();
    const view = render(withSession(<RemoveSource pkg={packageFromRow(A)} onRemoved={onRemoved} />));
    await click(screen.getByRole("button", { name: "Remove source Website" }));
    await click(screen.getByRole("button", { name: "Remove source" }));
    await waitFor(() => expect(resolve).toBeTruthy());
    view.unmount();
    await act(async () => resolve(builtinReply("setPackageSourceRemoved", [])));
    expect(onRemoved).not.toHaveBeenCalled();
  });
});
