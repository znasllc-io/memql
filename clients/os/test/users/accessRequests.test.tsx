import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";
const h = vi.hoisted(() => ({ rows: [] as Record<string, string>[], review: vi.fn() }));
vi.mock("@znasllc-io/memql-sdk-core/identityadmin", () => ({ IdentityAdminClient: class { reviewAccessRequest = h.review; } }));
const connection = { dispatcher: {}, subscriptions: null, query: { pendingAccessRequests: vi.fn(async (args: {search?:string}, opts?: {cursor?:string}) => {
 const values=h.rows.filter(row=>!args.search||[row.name,row.email,row.additionalContext].some(value=>value?.toLowerCase().includes(args.search!.toLowerCase())));
 const offset=Number(opts?.cursor||0);
 return {rows:()=>values.slice(offset,offset+10),meta:()=>({cursor:offset+10<values.length?String(offset+10):""})};
}) } };
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => connection }));
import { disposeSessionCollections } from "../../src/live/sessionCollections";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";
import { AccessRequestsSection } from "../../src/apps/users/AccessRequestsSection";

afterEach(cleanup);
beforeEach(() => {
  disposeSessionCollections(connection as unknown as Connection);
  connection.query.pendingAccessRequests.mockClear();
  h.rows = [{ id: "request-1", name: "Ada", email: "ada@team.test", status: "pending", additionalContext: "Help with our project" }];
  h.review.mockReset().mockResolvedValue({ message: "Invitation issued and emailed.", url: "https://identity.test/invitation?token=example", emailSent: true, emailError: "" });
});
it("keeps review controls unavailable to a viewer without user-management authority", async () => {
  render(<AccessRequestsSection catalog={{ roles: [], grants: [], state: "ready", error: "", reload: () => {} }} viewerRole="developer" canReview={false} />);
  fireEvent.click(await screen.findByRole("button", { name: /Ada/ }));
  expect(screen.getByText("An owner or admin can approve or reject this request.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Approve and invite" })).toBeNull();
  expect(h.review).not.toHaveBeenCalled();
});
it("opens a request, requires a rejection note, and submits its verified ID", async () => {
  render(<AccessRequestsSection catalog={{ roles: [], grants: [], state: "ready", error: "", reload: () => {} }} viewerRole="owner" canReview />);
  fireEvent.click(await screen.findByRole("button", { name: /Ada/ }));
  expect(screen.queryByRole("button", { name: "Reject" })).toBeNull();
  fireEvent.change(screen.getByRole("textbox", { name: "Review note" }), { target: { value: "No available access" } });
  // Reject just joined the bar, and a bar that has just changed takes no press for a moment.
  await readTheBar();
  fireEvent.click(screen.getByRole("button", { name: "Reject" }));
  await waitFor(() => expect(h.review).toHaveBeenCalledWith("request-1", "reject", "", "No available access"));
});
it("shows the delivered invitation once and keeps a failed review available to retry", async () => {
  h.review.mockRejectedValueOnce(new Error("The connection was interrupted."));
  render(<AccessRequestsSection catalog={{ roles: [], grants: [], state: "ready", error: "", reload: () => {} }} viewerRole="owner" canReview />);
  fireEvent.click(await screen.findByRole("button", { name: /Ada/ }));
  fireEvent.click(screen.getByRole("button", { name: "Approve and invite" }));
  await screen.findByText("The connection was interrupted.");
  fireEvent.click(screen.getByRole("button", { name: "Approve and invite" }));
  await screen.findByText("Invitation issued and emailed.");
  expect(screen.queryByRole("button", { name: "Approve and invite" })).toBeNull();
  expect(h.review).toHaveBeenLastCalledWith("request-1", "approve", "", "");
});


it("pages access requests and restores rows without another read", async () => {
 h.rows=Array.from({length:25},(_,n)=>({id:`request-${n}`,name:`Person ${n}`,email:`person${n}@test.invalid`,status:"pending",additionalContext:"Needs access"}));
 const props={catalog:{roles:[],grants:[],state:"ready" as const,error:"",reload:()=>{}},viewerRole:"owner",canReview:true};
 let view=render(<AccessRequestsSection {...props}/>);await screen.findByRole("button",{name:/^Person 9(?: |$)/});expect(screen.queryByRole("button",{name:/^Person 10(?: |$)/})).toBeNull();
 fireEvent.click(screen.getByRole("button",{name:"Load more"}));await screen.findByRole("button",{name:/^Person 19(?: |$)/});
 view.unmount();view=render(<AccessRequestsSection {...props}/>);expect(screen.getByRole("button",{name:/^Person 19(?: |$)/})).toBeTruthy();expect(connection.query.pendingAccessRequests).toHaveBeenCalledTimes(2);
 fireEvent.click(screen.getByRole("button",{name:"Search access requests"}));fireEvent.change(screen.getByRole("textbox",{name:"Search access requests"}),{target:{value:"person24"}});
 await screen.findByRole("button",{name:/^Person 24(?: |$)/});expect(connection.query.pendingAccessRequests).toHaveBeenLastCalledWith({search:"person24"},expect.anything());view.unmount();
});
