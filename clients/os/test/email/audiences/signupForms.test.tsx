import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption, openSelect } from "../../selectControl";
import { readTheBar } from "../../readTheBar";
import { audienceRow, fakeConnection, senderRow, templateRow, withSession } from "../harness";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { useEmailFeeds, useEmailRecordFeeds } = await import("../../../src/apps/email/useEmail");
const { useSites } = await import("../../../src/apps/deployables/useSites");
const { audienceFromRow } = await import("../../../src/apps/email/rows");
const { ConnectSignupFormWizard, SignupFormPage, newsletterFormHTML } = await import("../../../src/apps/email/audiences/SignupForms");
const binding = { id: "n1", siteId: "site", accountId: "client", audienceId: "audience", templateId: "t1", senderIdentityId: "s1", enabled: true, consentText: "Email me the client newsletter.", createdAt: "2026-10-01T12:00:00Z" };
const site = { id: "site", accountId: "client", title: "Client storefront", status: "live" };
function Surface({ page = false, required = false }: { page?: boolean; required?: boolean }) {
  const feeds = useEmailFeeds(); const records = useEmailRecordFeeds(feeds); const sites = useSites();
  const audience = audienceFromRow(audienceRow({ id: "audience", accountId: "client", confirmation: required ? "required" : "none" }));
  const shared = { audience, feeds, signupForms: records.signupForms, sites, onBack: () => {} };
  return page ? <SignupFormPage {...shared} binding={binding} /> : <ConnectSignupFormWizard {...shared} onDone={() => {}} />;
}
function mount(page = false, required = false) {
  const conn = fakeConnection({ audiences: [audienceRow({ id: "audience", accountId: "client" })], sites: [site, { id: "foreign", accountId: "other", title: "Other storefront", status: "live" }], templates: [templateRow({ id: "t1", accountId: "client", name: "Welcome" }), templateRow({ id: "t2", accountId: "other", name: "Other welcome" })], senderIdentities: [senderRow({ id: "s1", accountId: "client", address: "hello@client.test" })], newsletters: page ? [binding] : [], welcomes: [{ id: "signup", status: "blocked", createdAt: "revision", lastError: "Sender needs attention." }] });
  h.connection = conn; render(withSession(<Surface page={page} required={required} />)); return conn;
}
async function press(name: string) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name })); }
describe("Signup forms", () => {
  it.each([false, true])("connects within the organization, with optional welcome; confirmation=%s", async required => {
    const conn = mount(false, required);
    fireEvent.click(await screen.findByRole("radio", { name: "Client storefront" }));
    expect(screen.queryByRole("radio", { name: "Other storefront" })).toBeNull(); await press("Next");
    const select = screen.getByLabelText("Welcome email");
    const options = openSelect(select);
    expect(within(options).queryByRole("option", { name: "Other welcome" })).toBeNull();
    expect(!!within(options).queryByRole("option", { name: "Welcome" })).toBe(!required);
    chooseOption(select, required ? "No welcome email" : "Welcome"); await press("Next");
    if (!required) { chooseOption(screen.getByLabelText("Sender"), "hello@client.test"); await press("Next"); } else expect(screen.queryByLabelText("Sender")).toBeNull();
    await press("Next"); await press("Connect");
    await waitFor(() => expect(conn.query.campaignConfigureNewsletter).toHaveBeenCalledWith(expect.objectContaining({ siteId: "site", audienceId: "audience", templateId: required ? undefined : "t1", senderIdentityId: required ? undefined : "s1", enabled: true })));
  });
  it("uses the shown revision to stop accepting and rechecks a blocked welcome", async () => {
    const conn = mount(true);
    await screen.findByRole("button", { name: "Check again" }); await press("Stop accepting signups");
    await waitFor(() => expect(conn.query.campaignConfigureNewsletter).toHaveBeenCalledWith(expect.objectContaining({ enabled: false, expectedRevision: binding.createdAt })));
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(conn.query.campaignRetryNewsletterWelcome).toHaveBeenCalledWith({ signupId: "signup", expectedRevision: "revision" }));
    expect(screen.getByText(/Add it to the deployable's page/)).toBeTruthy();
  });
  it("escapes affirmative consent and includes no organization or sender controls", () => {
    const form = newsletterFormHTML({ ...binding, consentText: '<img src=x onerror="bad()"> & newsletter' });
    expect(form).toContain('name="consent" value="yes" required'); expect(form).toContain(`name="consentRevision" value="${binding.createdAt}"`); expect(form).toContain("&lt;img"); expect(form).not.toContain("<img"); expect(form).not.toContain("senderIdentityId"); expect(form).not.toContain("accountId");
  });
});
