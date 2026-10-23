import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { RepositoryChange, validRequestPaths } from "../../src/apps/pipelines/RepositoryChange";
import { fakeConnection, withSession } from "../deployables/harness";
function mount(over: Parameters<typeof fakeConnection>[0] = {}) { const c = fakeConnection(over); h.connection=c; render(withSession(<RepositoryChange sourceId="source-one" />)); return c; }
beforeEach(() => { h.connection=null; });
const permission = { sourceId:"source-one", repository:"acme/shop", repositoryId:42, canWrite:false, canRequest:true, route:"maintainer", operations:["propose_change"] };
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

const deliveryPermission = { ...permission, operations: ["propose_change", "deliver_change"] };
const deliveryReceipt = { id:"delivery-one", operation:"deliver_change", statement:"Make saved filters easier to use", repository:"acme/shop", state:"approved", paths:["clients/os/"], review:{approved:true,reason:"The changes and regression cases cover the requested scope."} };

describe("explicit repository delivery scope", () => {
  it("keeps proposal-only as the default even when delivery is available", async () => {
    const c=mount({repositoryAuthority:{...deliveryPermission,operations:["deliver_change","propose_change"]}}); await compose();
    expect(screen.getByRole("radio",{name:/Prepare a proposal/}).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio",{name:/Deliver a reviewed change/}).getAttribute("aria-checked")).toBe("false");
    fireEvent.click(screen.getByRole("button",{name:"Send for approval"}));
    await waitFor(()=>expect(c.callsNamed("repositoryChangeRequest")).toHaveLength(1));
    expect(c.callsNamed("repositoryChangeRequest")[0]).toContain('operation: "propose_change"');
  });
  it("sends explicit delivery intent through the maintainer route without granting merge or production authority", async () => {
    const c=mount({repositoryAuthority:deliveryPermission}); await compose();
    fireEvent.click(screen.getByRole("radio",{name:/Deliver a reviewed change/}));
    expect(screen.queryByText(/Merging and deployment need separate authorization/)).toBeNull();
    expect(screen.getByText(/The repository maintainer must approve this scope/)).toBeTruthy();
    expect(screen.getByText(/Select a production release separately/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button",{name:"Send delivery for approval"}));
    await screen.findByText("Waiting for the repository maintainer");
    const call=c.callsNamed("repositoryChangeRequest")[0]!;
    expect(call).toContain('operation: "deliver_change"'); expect(call).toContain('paths: ["clients/os/"]');
    expect(call).not.toMatch(/ownerUserId|template|approved:|endpoint|deploy:|merge:/);
    expect(screen.queryByText("Merged into the default branch")).toBeNull();
  });
  it("does not turn source write access into self-approval when its route names a maintainer", async () => {
    mount({repositoryAuthority:{...deliveryPermission,canWrite:true,route:"maintainer"}}); await compose();
    fireEvent.click(screen.getByRole("radio",{name:/Deliver a reviewed change/}));
    expect(screen.getByRole("button",{name:"Send delivery for approval"})).toBeTruthy();
    expect(screen.queryByRole("button",{name:"Start reviewed delivery"})).toBeNull();
  });
  it("gives an authorized repository owner the explicit reviewed-delivery action", async () => {
    const c=mount({repositoryAuthority:{...deliveryPermission,canWrite:true,route:"own"}}); await compose();
    fireEvent.click(screen.getByRole("radio",{name:/Deliver a reviewed change/}));
    fireEvent.click(screen.getByRole("button",{name:"Start reviewed delivery"}));
    await screen.findByText("Approved for the requested work");
    expect(c.callsNamed("repositoryChangeRequest")[0]).toContain('operation: "deliver_change"');
    expect(screen.queryByText("Independent review passed")).toBeNull();
    expect(screen.queryByText("Merged into the default branch")).toBeNull();
  });
  it("does not infer delivery availability from repository write access", async () => {
    mount({repositoryAuthority:{...permission,canWrite:true,route:"own"}}); await compose();
    expect(screen.queryByRole("radio",{name:/Deliver a reviewed change/})).toBeNull();
  });
  it.each([undefined,[],["unrecognized_operation"]])("does not offer unreported operations: %j", async operations => {
    mount({repositoryAuthority:{...deliveryPermission,operations}});
    await screen.findByText("Supported change requests were not reported for this repository.");
    expect(screen.queryByRole("button",{name:"Request changes"})).toBeNull();
  });
  it("requires the local request capability even when server permissions report delivery support", async () => {
    const c=fakeConnection({repositoryAuthority:deliveryPermission});h.connection=c;
    render(withSession(<RepositoryChange sourceId="source-one" />,{capabilities:[]}));
    await screen.findByText("Read access");
    expect(screen.queryByRole("button",{name:"Request changes"})).toBeNull();
  });
  it("uses a new immutable request key when the delivery scope changes after an uncertain attempt", async () => {
    const c=mount({repositoryAuthority:deliveryPermission,repositoryChangeError:"Reply interrupted"}); await compose();
    fireEvent.click(screen.getByRole("button",{name:"Send for approval"}));await screen.findByText("The request could not be confirmed.");
    fireEvent.click(screen.getByRole("radio",{name:/Deliver a reviewed change/}));
    fireEvent.click(screen.getByRole("button",{name:"Send delivery for approval"}));
    await waitFor(()=>expect(c.callsNamed("repositoryChangeRequest")).toHaveLength(2));
    const [before,after]=c.callsNamed("repositoryChangeRequest");
    expect(/requestId: "([^"]+)"/.exec(before!)?.[1]).not.toBe(/requestId: "([^"]+)"/.exec(after!)?.[1]);
    expect(after).toContain('operation: "deliver_change"');
  });
  it("blocks a selected delivery scope when refreshed server policy withdraws it", async () => {
    const c=fakeConnection({repositoryAuthority:deliveryPermission});h.connection=c;
    const view=render(withSession(<RepositoryChange sourceId="source-one" />));await compose();
    fireEvent.click(screen.getByRole("radio",{name:/Deliver a reviewed change/}));
    const changed=fakeConnection({repositoryAuthority:permission});h.connection=changed;
    view.rerender(withSession(<RepositoryChange sourceId="source-one" />));
    await waitFor(()=>expect(screen.queryByRole("radio",{name:/Deliver a reviewed change/})).toBeNull());
    expect(screen.getByRole("button",{name:"Send delivery for approval"}).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button",{name:"Send delivery for approval"}));
    expect(changed.callsNamed("repositoryChangeRequest")).toHaveLength(0);
  });
});

describe("readable delivery receipts", () => {
  it.each([
    ["waiting","Waiting to join the merge queue",'Required check "ci-required" is still running.'],
    ["attention","Delivery needs attention","A required GitHub review or CODEOWNER approval is still needed."],
    ["queued","In the merge queue","GitHub is verifying this change in the merge queue."],
    ["merged","Merged into the default branch","GitHub merged this reviewed change."],
    ["future_state","Delivery status unavailable",""],
  ])("shows %s as provider progress, separate from approval and production",async (state,label,reason)=>{
    const c=mount({repositoryAuthority:deliveryPermission,changeRequests:[{...deliveryReceipt,delivery:{state,reason,headSha:"a".repeat(40),queueEntryId:state==="queued"?"queue-reference":""}}]});
    fireEvent.click(await screen.findByRole("button",{name:/Make saved filters easier to use/}));
    await screen.findByText(label);expect(screen.getByText("Approved for the requested work")).toBeTruthy();
    expect(screen.getByText("Independent review passed")).toBeTruthy();if(reason)expect(screen.getByText(reason)).toBeTruthy();
    if(state!=="merged")expect(screen.queryByText("Merged into the default branch")).toBeNull();
    if(state==="queued")expect(screen.getByText(/Queue acceptance is not a production release/)).toBeTruthy();
    if(state==="merged")expect(screen.getByText("Select a release separately to update production.")).toBeTruthy();
    expect(c.callsNamed("repositoryChangeRequest")).toHaveLength(0);
  });
  it("keeps a recorded independent-review refusal visible without claiming tests or queue completion",async()=>{
    mount({repositoryAuthority:deliveryPermission,changeRequests:[{...deliveryReceipt,review:{approved:false,reason:"The requested folder includes unrelated payment changes."}}]});
    fireEvent.click(await screen.findByRole("button",{name:/Make saved filters easier to use/}));
    await screen.findByText("Independent review needs attention");expect(screen.getByText("The requested folder includes unrelated payment changes.")).toBeTruthy();
    expect(screen.queryByText("In the merge queue")).toBeNull();
  });
});

describe("saved proposal navigation", () => {
  it("opens the exact durable proposal for a remote requester without exposing a maintainer goal", async () => {
    const c=mount({repositoryAuthority:{...deliveryPermission,route:"remote"},changeRequests:[{...deliveryReceipt,goalId:"",proposal:{state:"proposed",branch:"memql/change-reviewed",commit:"a".repeat(40),pullRequestUrl:"https://github.com/acme/shop/pull/27"}}]});
    fireEvent.click(await screen.findByRole("button",{name:/Make saved filters easier to use/}));
    const link=await screen.findByRole("link",{name:"Open pull request"});
    expect(link.getAttribute("href")).toBe("https://github.com/acme/shop/pull/27");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(screen.queryByRole("button",{name:"Open goal"})).toBeNull();
    expect(c.callsNamed("repositoryChangeRequest")).toHaveLength(0);
  });
  it.each([
    "javascript:alert(1)", "https://github.com.evil.example/acme/shop/pull/27",
    "https://github.com/another/repo/pull/27", "http://github.com/acme/shop/pull/27",
    "https://github.com/acme/shop/pull/0", "https://github.com/acme/shop/pull/27?next=other",
    "https://github.com/acme/%73hop/pull/27", "https://github.com/acme/shop/pull/27/files",
  ])("refuses an unbound proposal URL: %s",async pullRequestUrl=>{
    mount({repositoryAuthority:deliveryPermission,changeRequests:[{...deliveryReceipt,proposal:{state:"proposed",pullRequestUrl}}]});
    fireEvent.click(await screen.findByRole("button",{name:/Make saved filters easier to use/}));
    await screen.findByText("Independent review passed");
    expect(screen.queryByRole("link",{name:"Open pull request"})).toBeNull();
  });
});
