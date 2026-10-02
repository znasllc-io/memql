import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption } from "../selectControl";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { RecurrencePanel } from "../../src/apps/campaigns/RecurrencePanel";
import { AttentionDestination, AttentionProvider } from "../../src/attention/Attention";
import { OS_REGISTRY } from "../../src/apps/registry";
import { fakeConnection, rowsResult, withSession } from "./harness";

const series = { id: "series", sourceCampaignId: "c1", status: "active", intervalWeeks: 2, timeZone: "America/Phoenix", anchorAt: "2028-01-02T16:00:00Z", nextAt: "2028-01-16T16:00:00Z", createdAt: "2027-10-01T12:00:00.000001Z", lastError: "" };

describe("recurring campaign controls", () => {
  it("opens the actual attention destination and submits a selected date and cadence", async () => {
    const conn = Object.assign(fakeConnection(), {});
    const query = Object.assign(conn.query, { myAttentionReceipts: vi.fn(async () => rowsResult([])), acknowledgeAttention: vi.fn(async () => rowsResult([])) });
    h.connection = conn;
    const campaigns = OS_REGISTRY.apps.find(app => app.id === "campaigns")!;
    const apps = [{ ...campaigns, attentionChanges: campaigns.attentionChanges!.filter(change => change.id === "campaigns:recurring") }];
    render(withSession(<AttentionProvider apps={apps}><AttentionDestination appId="campaigns" sectionId="campaigns"><RecurrencePanel campaignId="c1" /></AttentionDestination></AttentionProvider>));
    const setup = await screen.findByRole("button", { name: /Set up repeating sends/ });
    await waitFor(() => expect(setup.hasAttribute("disabled")).toBe(false));
    expect(query.acknowledgeAttention).not.toHaveBeenCalled();
    fireEvent.click(setup);
    await waitFor(() => expect(query.acknowledgeAttention).toHaveBeenCalledWith({ changeId: "campaigns:recurring", revision: "recurring-1" }));
    chooseOption(screen.getByLabelText("Repeat interval"), "3 weeks");
    fireEvent.change(screen.getByLabelText("First recurring send"), { target: { value: "2028-01-02T09:00" } });
    fireEvent.click(screen.getByRole("button", { name: "Start repeating schedule" }));
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith(expect.objectContaining({ campaignId: "c1", action: "save", intervalWeeks: 3, firstSendAt: new Date("2028-01-02T09:00").toISOString(), timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone })));
  });
  it("uses the saved revision to pause and ignores another campaign's live changes", async () => {
    const conn = fakeConnection({ campaignSeries: [series] }); h.connection = conn;
    render(withSession(<RecurrencePanel campaignId="c1" />));
    const pause = await screen.findByRole("button", { name: "Pause future sends" });
    await waitFor(() => expect(pause.hasAttribute("disabled")).toBe(false));
    await act(async () => conn.subscriptions.emit("v1:campaigns:campaignSeries", { ...series, id: "other", sourceCampaignId: "c2", intervalWeeks: 40 }));
    expect(screen.queryByText(/Every 40 weeks/)).toBeNull();
    fireEvent.click(pause);
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({ campaignId: "c1", action: "pause", expectedRevision: series.createdAt }));
    await act(async () => conn.subscriptions.emit("v1:campaigns:campaignSeries", { ...series, status: "blocked", lastError: "Template needs publication.", createdAt: "2027-10-01T12:01:00Z" }));
    expect(await screen.findByText("Template needs publication.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Resume future sends" }));
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenLastCalledWith({ campaignId: "c1", action: "resume", expectedRevision: "2027-10-01T12:01:00Z" }));
  });
});
