import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { RepositoryChange, validRequestPaths } from "../../src/apps/pipelines/RepositoryChange";
import { fakeConnection, withSession } from "../deployables/harness";
function mount(over: Parameters<typeof fakeConnection>[0] = {}) { const c = fakeConnection(over); h.connection=c; render(withSession(<RepositoryChange sourceId="source-one" />)); return c; }
beforeEach(() => { h.connection=null; });
const permission = { sourceId:"source-one", repository:"acme/shop", repositoryId:42, canWrite:false, canRequest:true, route:"maintainer" };
async function compose() {
  fireEvent.click(await screen.findByRole("button", {name:"Request changes"}));
  fireEvent.change(screen.getByLabelText("What should change?"), {target:{value:"Make the client list easier to read"}});
  fireEvent.change(screen.getByLabelText("Files or folders this request may change"), {target:{value:"clients/os/"}});
}
describe("repository change authority", () => {
  it("does not turn installed owner status into repository write authority", async () => {
    const c=mount({repositoryAuthority:permission}); await compose();
    expect(screen.getByText("Read access")).toBeTruthy();
    expect(screen.queryByRole("button",{name:"Start requested work"})).toBeNull();
    fireEvent.click(screen.getByRole("button",{name:"Send for approval"}));
    await screen.findByText("Waiting for the repository maintainer");
    const call=c.callsNamed("repositoryChangeRequest")[0]!;
    expect(call).toContain('operation: "propose_change"'); expect(call).toContain('paths: ["clients/os/"]');
    expect(call).not.toMatch(/owner|template|approved|endpoint|merge|deploy/);
  });
  it("reuses the exact request key after an uncertain submission", async () => {
    const c=mount({repositoryAuthority:permission,repositoryChangeError:"Connection interrupted"}); await compose();
    fireEvent.click(screen.getByRole("button",{name:"Send for approval"})); await screen.findByText("The request could not be confirmed.");
    fireEvent.click(screen.getByRole("button",{name:"Send for approval"}));
    await waitFor(()=>expect(c.callsNamed("repositoryChangeRequest")).toHaveLength(2));
    expect(c.callsNamed("repositoryChangeRequest")[0]).toBe(c.callsNamed("repositoryChangeRequest")[1]);
  });
  it("keeps unavailable permission checks distinct from read access", async () => {
    mount({repositoryAuthorityError:"GitHub could not confirm the identity"});
    await screen.findByText("Repository permissions are unavailable.");
    expect(screen.queryByText("Read access")).toBeNull(); expect(screen.queryByRole("button",{name:"Request changes"})).toBeNull();
  });
  it("reads durable request labels after returning to the repository", async () => {
    mount({repositoryAuthority:permission, changeRequests:[{id:"request-previous",state:"declined",statement:"Add client billing filters",repository:"acme/shop",reason:"Please narrow the requested change"}]});
    fireEvent.click(await screen.findByRole("button",{name:/Add client billing filters/}));
    await screen.findByText("Please narrow the requested change"); expect(screen.getByText("Declined by the maintainer")).toBeTruthy();
  });
  it("accepts bounded repository paths and rejects unbounded or escaping scopes", () => {
    expect(validRequestPaths("clients/os/\nREADME.md")).toEqual(["clients/os/","README.md"]);
    for (const value of ["", "/", "../private", "clients/../", "**", "C:/secrets", "clients//file"]) expect(validRequestPaths(value)).toBeNull();
  });
});
