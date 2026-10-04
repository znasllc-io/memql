import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ rows: [] as Record<string, string>[], review: vi.fn() }));
vi.mock("@znasllc-io/memql-sdk-core/identityadmin", () => ({ IdentityAdminClient: class { reviewAccessRequest = h.review; } }));
const connection = { dispatcher: {}, subscriptions: null, query: { pendingAccessRequests: async () => ({ rows: () => h.rows, meta: () => null }) } };
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => connection }));
import { AccessRequestsSection } from "../../src/apps/users/AccessRequestsSection";

afterEach(cleanup);
beforeEach(() => {
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
