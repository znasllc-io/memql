import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption, openSelect } from "../selectControl";
import { audienceRow, fakeConnection, senderRow, templateRow, withSession } from "./harness";
import { audienceProjection, senderProjection, templateProjection } from "../../src/apps/campaigns/CampaignsSection";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { CampaignForm } from "../../src/apps/campaigns/CampaignsSection";
import { useCampaignWrites } from "../../src/apps/campaigns/actions";

const audiences = audienceProjection([audienceRow({ id: "a-acme", accountId: "acme", name: "Acme audience" }), audienceRow({ id: "a-other", accountId: "other", name: "Other audience" })]);
const templates = templateProjection([templateRow({ id: "t-acme", accountId: "acme", name: "Acme copy" }), templateRow({ id: "t-other", accountId: "other", name: "Other copy" })]);
const senders = senderProjection([senderRow({ id: "s-acme", accountId: "acme", address: "news@acme.test" })]);
function Form() {
  return <CampaignForm audiences={audiences} templates={templates} senders={senders} writes={useCampaignWrites()} trackByDefault onDone={() => {}} />;
}
function mount(accountIds: string[]) {
  const connection = fakeConnection({ accounts: accountIds.map((id) => ({ id, name: id === "acme" ? "Acme" : "Other", status: "active" })) });
  h.connection = connection;
  render(withSession(<Form />, { role: "client-member", everyAccount: false, accountIds }));
  return connection;
}

describe("campaign organization ownership", () => {
  it("defaults to the client's organization, filters resources and persists that choice with no forged owner", async () => {
    const connection = mount(["acme"]);
    await waitFor(() => expect(screen.getByLabelText("Organization this campaign is for").textContent).toContain("Acme"));
    fireEvent.change(screen.getByLabelText("Campaign name"), { target: { value: "Acme newsletter" } });
    const options = openSelect(screen.getByLabelText("Audience to send to"));
    expect(within(options).queryByRole("option", { name: "Other audience" })).toBeNull();
    fireEvent.click(within(options).getByRole("option", { name: "Acme audience" }));
    chooseOption(screen.getByLabelText("Template to send"), "Acme copy");
    expect((screen.getByRole("button", { name: "Create campaign" }) as HTMLButtonElement).disabled).toBe(true);
    chooseOption(screen.getByLabelText("Sending mailbox"), "news@acme.test");
    fireEvent.click(screen.getByRole("button", { name: "Create campaign" }));
    await waitFor(() => expect(connection.query.createCampaign).toHaveBeenCalledOnce());
    const args = connection.query.createCampaign.mock.calls[0]![0];
    expect(args).toMatchObject({ accountId: "acme", audienceId: "a-acme", templateId: "t-acme", senderIdentityId: "s-acme" });
    expect(args).not.toHaveProperty("ownerUserId");
  });
  it("requires a deliberate organization for multiple memberships and clears dependent selections when it changes", async () => {
    const connection = mount(["other", "acme"]);
    const picker = screen.getByLabelText("Organization this campaign is for");
    expect(picker.textContent).toContain("Choose an organization");
    await waitFor(() => expect(connection.query.clientAccountsAll).toHaveBeenCalled());
    chooseOption(picker, "Acme");
    chooseOption(screen.getByLabelText("Audience to send to"), "Acme audience");
    chooseOption(picker, "Other");
    expect(screen.getByLabelText("Audience to send to").textContent).toContain("Choose an audience");
    expect(connection.query.createCampaign).not.toHaveBeenCalled();
  });
});
