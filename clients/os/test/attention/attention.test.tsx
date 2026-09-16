import { attentionDeclarationErrors } from "../../src/attention/declarations";
import { useState } from "react";
import { fireEvent, render, screen, waitFor, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { AttentionProvider, AttentionMarker, AttentionDestination, usePublishAttention } from "../../src/attention/Attention";
import { packageAttention } from "../../src/apps/deployables/attention";
import { packageFromRow } from "../../src/apps/deployables/packages/rows";
import { OS_REGISTRY } from "../../src/apps/registry";
import { rowsResult, withSession } from "../deployables/harness";
import type { AttentionChange } from "../../src/attention/model";
import { SourceView } from "../../src/apps/deployables/page/SourceView";

const change: AttentionChange = { id: "update:p", revision: "one", appId: "deployables", sectionId: "deployables", ancestors: ["map"], target: "version:p", label: "New version", kind: "runtime" };
function Surface({ revision = "one", visible = true }: { revision?: string; visible?: boolean }) {
  const [opened, setOpened] = useState(false);
  usePublishAttention("test", [{ ...change, revision }, { ...change, id: "update:q", target: "version:q" }]);
  return <AttentionDestination appId="deployables" sectionId="deployables" visible={visible}>
    <div data-testid="app"><AttentionMarker appId="deployables" /></div>
    <div data-testid="overview"><AttentionMarker appId="deployables" sectionId="map" /></div>
    <button onClick={() => setOpened(true)}>Available version<AttentionMarker appId="deployables" target="version:p" /></button>
    {opened ? <AttentionDestination appId="deployables" sectionId="deployables" target="version:p"><span>Version {revision}</span></AttentionDestination> : null}
  </AttentionDestination>;
}
function setup() {
  const receipts = new Map<string, Record<string, string>[]>();
  let user = "alice";
  const executeNamed = vi.fn(async (name: string, call: string) => {
    if (name === "myAttentionReceipts") return rowsResult(receipts.get(user) ?? []);
    if (name === "acknowledgeAttention") {
      const changeId = /changeId: "([^"]+)"/.exec(call)![1]!;
      const revision = /revision: "([^"]+)"/.exec(call)![1]!;
      receipts.set(user, [...(receipts.get(user) ?? []), { id: changeId+revision, changeId, revision }]);
    }
    return rowsResult([]);
  });
  const query = Object.assign(Object.create(QueryClient.prototype), { executeNamed });
  h.connection = { query, subscriptions: null };
  return { executeNamed, user: (next: string) => { user = next; } };
}
const wrap = (node: React.ReactNode, userId = "alice") => withSession(<AttentionProvider apps={OS_REGISTRY.apps}>{node}</AttentionProvider>, { userId });
afterEach(cleanup);

describe("shared attention", () => {
  it("keeps declared feature metadata tied to known app sections and unique stable IDs", () => {
    expect(attentionDeclarationErrors(OS_REGISTRY.apps)).toEqual([]);
    const base = { id: "fleet", sections: [{ id: "routing" }] };
    const valid = { id: "fleet:policy-editor", revision: "v2", sectionId: "routing", label: "Policy editor improved" };
    expect(attentionDeclarationErrors([{ ...base, attentionChanges: [valid] }])).toEqual([]);
    expect(attentionDeclarationErrors([{ ...base, attentionChanges: [{ ...valid, id: "other:unrelated", revision: "", sectionId: "typo", target: "" }, valid, valid] }]).join("\n"))
      .toMatch(/prefixed with fleet:[\s\S]*meaningful revision[\s\S]*unknown section typo[\s\S]*exact destination target[\s\S]*duplicate attention ID/);
  });

  it("lets an undeployed source acknowledge its version without deploying", async () => {
    const fake = setup();
    const pkg = packageFromRow({ id: "never-deployed", name: "New source", status: "active", sourceKind: "repo", latestKnownVersion: "new-head", updateAvailable: true });
    function UndeployedSource() {
      usePublishAttention("source", packageAttention(pkg));
      return <><AttentionMarker appId="deployables" /><SourceView pkg={pkg} apps={[]} credentials={[]} can={{ deploy: false, sources: false, retire: false, domains: false, publish: false }} onBack={() => {}} onOpenHistory={() => {}} onOpenApp={() => {}} onOpenDeclared={() => {}} attempts={0} deployedBy="" /></>;
    }
    render(wrap(<UndeployedSource />));
    await screen.findAllByRole("img", { name: "Unseen change" });
    expect(fake.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: /Available version/ }));
    await waitFor(() => expect(screen.queryAllByRole("img", { name: "Unseen change" })).toHaveLength(0));
    expect(pkg.updateAvailable).toBe(true);
    expect(fake.executeNamed.mock.calls.filter(([name]) => name !== "myAttentionReceipts").map(([name]) => name)).toEqual(["acknowledgeAttention"]);
  });
  it("acknowledges only the viewed leaf; retains other ancestors, persists, and scopes receipts to the user", async () => {
    const fake = setup();
    const view = render(wrap(<Surface />));
    await waitFor(() => expect(screen.getAllByRole("img", { name: "Unseen change" })).toHaveLength(3));
    expect(fake.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: /Available version/ }));
    await waitFor(() => expect(screen.getAllByRole("img", { name: "Unseen change" })).toHaveLength(2));
    view.unmount();
    const again = render(wrap(<Surface />));
    await waitFor(() => expect(screen.getAllByRole("img", { name: "Unseen change" })).toHaveLength(2));
    again.unmount();
    fake.user("bob");
    render(wrap(<Surface />, "bob"));
    await waitFor(() => expect(screen.getAllByRole("img", { name: "Unseen change" })).toHaveLength(3));
  });
  it("does not consume a hidden destination and gives each new revision its own receipt", async () => {
    const fake = setup();
    const view = render(wrap(<Surface visible={false} />));
    await screen.findAllByRole("img", { name: "Unseen change" });
    fireEvent.click(screen.getByRole("button", { name: /Available version/ }));
    expect(fake.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);
    view.rerender(wrap(<Surface visible />));
    await waitFor(() => expect(fake.executeNamed.mock.calls.filter(([name]) => name === "acknowledgeAttention")).toHaveLength(1));
    view.rerender(wrap(<Surface revision="two" visible />));
    await waitFor(() => expect(fake.executeNamed.mock.calls.filter(([name]) => name === "acknowledgeAttention")).toHaveLength(2));
    expect(fake.executeNamed.mock.calls.at(-1)?.[1]).toContain('revision: "two"');
  });
  it("supports feature declarations from any registered app and clears the last ancestor at its actual section", async () => {
    setup();
    const app = { ...OS_REGISTRY.apps.find(app => app.id === "fleet")!, attentionChanges: [{ id: "fleet:policy-editor", revision: "v2", sectionId: "routing", label: "Policy editor improved" }] };
    const node = (open: boolean) => withSession(<AttentionProvider apps={[app]}><AttentionMarker appId="fleet" />{open ? <AttentionDestination appId="fleet" sectionId="routing">Routing</AttentionDestination> : <AttentionDestination appId="fleet" sectionId="machines">Machines</AttentionDestination>}</AttentionProvider>);
    const view = render(node(false));
    await screen.findByRole("img", { name: "Unseen change" });
    view.rerender(node(true));
    await waitFor(() => expect(screen.queryByRole("img", { name: "Unseen change" })).toBeNull());
  });
  it("reports only manual pending revisions; automatic success and acknowledgments do not change availability", () => {
    const pkg = packageFromRow({ id: "p", status: "active", sourceKind: "repo", deployedVersion: "a", latestKnownVersion: "b", updateAvailable: true });
    expect(packageAttention(pkg)).toHaveLength(1);
    expect(packageAttention({ ...pkg, autoDeploy: true })).toEqual([]);
    expect(packageAttention({ ...pkg, latestKnownVersion: "a" })).toEqual([]);
    expect(packageAttention({ ...pkg, status: "archived" })).toEqual([]);
    expect(pkg.updateAvailable).toBe(true);
  });
});
