import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The wizard is the real thing's edge, not its inside: this is about what the
// section does when the wizard says it made a campaign.
vi.mock("../../../src/apps/email/campaigns/AddCampaignWizard", () => ({
  AddCampaignWizard: ({ onDone, onBack }: { onDone: (id: string) => void; onBack: () => void }) => (
    <div>
      <h3>Add a campaign</h3>
      <button type="button" onClick={() => onDone("made")}>
        Made one
      </button>
      <button type="button" onClick={onBack}>
        Back out
      </button>
    </div>
  ),
}));

const { CampaignsSection } = await import("../../../src/apps/email/campaigns/CampaignsSection");
const { EmailNavigationContext, useEmailRequests } = await import("../../../src/apps/email/openRequest");
const { useEmailFeeds, useEmailRecordFeeds } = await import("../../../src/apps/email/useEmail");
const { campaignRow, fakeConnection, withSession } = await import("../harness");

// A CAMPAIGN THE WIZARD JUST MADE IS NOT A CAMPAIGN SOMEBODY ASKED FOR (memql#6033).
//
// The section says "That campaign isn't available." when search or Ask names a
// campaign the cluster does not hold. The wizard opens the page of the campaign
// it made as soon as it has made it, a moment before the feed carries that row:
// that is not a missing record, and saying so would be saying something false
// for the length of a round trip.

function Section() {
  const { navigation } = useEmailRequests(() => {});
  return (
    <EmailNavigationContext.Provider value={navigation}>
      <SectionBody />
    </EmailNavigationContext.Provider>
  );
}

function SectionBody() {
  const feeds = useEmailFeeds();
  const recordFeeds = useEmailRecordFeeds(feeds);
  return <CampaignsSection feeds={feeds} recordFeeds={recordFeeds} trackByDefault />;
}

describe("a campaign the wizard made", () => {
  it("opens when its row arrives, and nothing is said about it being missing in the meantime", async () => {
    const conn = fakeConnection({ campaigns: [campaignRow({ id: "other", name: "Somebody else's" })] });
    h.connection = conn;
    render(withSession(<Section />));
    await screen.findByText("Somebody else's");

    fireEvent.click(screen.getByRole("button", { name: "Add a campaign" }));
    fireEvent.click(await screen.findByRole("button", { name: "Made one" }));

    // The list again, the page not yet there, and no sentence.
    expect(await screen.findByText("Somebody else's")).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();

    await act(async () => {
      conn.subscriptions.emit("v1:campaigns:campaign", campaignRow({ id: "made", name: "Just made" }), "NODE_CREATED");
    });
    expect(await screen.findByRole("heading", { level: 3, name: "Just made" })).toBeTruthy();
    expect(screen.queryByText("That campaign isn't available.")).toBeNull();
  });
});
