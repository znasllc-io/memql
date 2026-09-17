import { Mail } from "lucide-react";
import { Button, EmptyState, Notice, Panel, Subhead } from "../../kit";
import { Overview, OverviewBreakdown } from "../../kit/Overview";
import { InfoDetail } from "../../kit/InfoDetail";
import { absent, figureOf } from "../../kit/measure";
import { campaignFromRow, audienceFromRow, templateFromRow } from "./rows";
import type { CampaignFeeds } from "./useCampaigns";

export function CampaignsOverview({
  feeds,
  navigate,
}: {
  feeds: CampaignFeeds;
  navigate: (id: string) => void;
}) {
  const campaigns = feeds.campaigns.snapshot.rows.map(campaignFromRow);
  const current = feeds.campaigns.snapshot.state === "live" && !feeds.campaigns.snapshot.error;
  const count = (key: keyof CampaignFeeds, n: number) =>
    feeds[key].snapshot.error
      ? absent("failed", feeds[key].snapshot.error)
      : feeds[key].snapshot.state === "live"
        ? figureOf(n)
        : absent("unread");
  const drafts = campaigns.filter((c) => c.status === "draft").length;
  const sending = campaigns.filter((c) => c.status === "sending").length;
  const scheduled = campaigns.filter((c) => c.status === "scheduled").length;
  const finished = campaigns.filter((c) => c.status === "sent").length;
  return (
    <Overview
      scope="Campaigns visible to you"
      metrics={[
        { label: "Campaigns", figure: count("campaigns", campaigns.length) },
        { label: "Sending", figure: count("campaigns", sending) },
        {
          label: "Audiences",
          figure: count(
            "audiences",
            feeds.audiences.snapshot.rows
              .map(audienceFromRow)
              .filter((a) => a.status !== "archived").length,
          ),
        },
        {
          label: "Ready templates",
          figure: count(
            "templates",
            feeds.templates.snapshot.rows.map(templateFromRow).filter((t) => t.status === "ready")
              .length,
          ),
        },
      ]}
    >
      {!current ? (
        <Notice
          tone="warn"
          sentence="Campaign information is not current."
          next="Counts will return when the connection recovers."
          detail={feeds.campaigns.snapshot.error}
        />
      ) : null}
      {current && !campaigns.length ? (
        <EmptyState
          icon={Mail}
          title="Your first campaign starts here"
          action={<Button onClick={() => navigate("campaigns")}>Open campaigns</Button>}
        >
          The guided setup covers your sending service, sender, audience and content before you save
          a draft.
        </EmptyState>
      ) : null}
      {current ? (
        <OverviewBreakdown
          title="Campaign status"
          segments={[
            { label: "Draft", count: drafts },
            { label: "Scheduled", count: scheduled },
            { label: "Sending", count: sending, tone: "good" },
            { label: "Sent", count: finished },
            {
              label: "Other",
              count: campaigns.length - drafts - scheduled - sending - finished,
            },
          ]}
        />
      ) : null}
      <Panel label="Sending preparation">
        <div className="os-campaign-detail-head">
          <Subhead>Sending preparation</Subhead>
          <InfoDetail title="Sending preparation">
            <p>
              A configured service is only the first check. Your provider must authorize the
              mailbox, the audience must contain eligible recipients and the template must be ready.
              Send a test before scheduling a campaign.
            </p>
          </InfoDetail>
        </div>
        <div className="os-campaign-actions">
          <Button onClick={() => navigate("settings")}>Review sending setup</Button>
        </div>
      </Panel>
    </Overview>
  );
}
