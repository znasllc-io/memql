import { AccountPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import { useSendingReadiness } from "./useCampaigns";
import { useCallback, useState } from "react";
import { Mail, Users, FileText } from "lucide-react";
import { useSession } from "../../chrome/access";
import type { UploadProvider } from "../../items/upload";
import {
  Button,
  Caption,
  EmptyState,
  Field,
  Head,
  Notice,
  Panel,
  Select,
  SetupGroup,
  Subhead,
} from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { InfoDetail } from "../../kit/InfoDetail";
import { JourneyTrail } from "../../kit/JourneyTrail";
import { AudienceDetail, AudienceForm } from "./AudiencesSection";
import {
  CampaignForm,
  audienceProjection,
  senderProjection,
  templateProjection,
} from "./CampaignsSection";
import { SenderForm } from "./SendersSection";
import { NewTemplate } from "./TemplatesSection";
import { editorTemplateURL } from "../../items/editorPreference";
import { openHandoff } from "../../items/vscode";
import type { CampaignWrites } from "./actions";
import type { CampaignFeeds } from "./useCampaigns";

const STEPS = ["Sending setup", "Sender", "Audience", "Content", "Review"];

/** No step sends mail. Saving creates a draft; the existing campaign page
 * retains the explicit test, schedule and send controls and server preflight. */
export function CampaignJourney({
  feeds,
  writes,
  uploads,
  trackByDefault,
  onDone,
}: {
  feeds: CampaignFeeds;
  writes: CampaignWrites;
  uploads: UploadProvider;
  trackByDefault: boolean;
  onDone: (id: string) => void;
}) {
  const { readiness, config } = useSession();
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  const [pickedAccountId, setAccountId] = useState("");
  const accountId = pickedAccountId || defaultAccountId;
  const [step, setStep] = useState(0);
  const [senderId, setSenderId] = useState("");
  const [audienceId, setAudienceId] = useState("");
  const [templateId, setTemplateId] = useState("");
  const [roster, setRoster] = useState<{ id: string; count: number | null }>({
    id: "",
    count: null,
  });
  const onRoster = useCallback((id: string, count: number | null) => setRoster({ id, count }), []);
  const [editors, setEditors] = useState<Record<number, boolean>>({});
  const creating = editors[step] ?? false;
  const [reviewed, setReviewed] = useState(false);
  function setCreating(value: boolean) {
    setEditors((previous) => ({ ...previous, [step]: value }));
  }
  const audiences = audienceProjection(feeds.audiences.snapshot.rows).filter(
    (a) => a.accountId === accountId && a.status !== "archived",
  );
  const senders = senderProjection(feeds.senders.snapshot.rows).filter(
    (s) => s.accountId === accountId && s.status !== "disabled",
  );
  const templates = templateProjection(feeds.templates.snapshot.rows).filter(
    (t) => t.accountId === accountId && t.status !== "archived",
  );
  const audience = audiences.find((a) => a.id === audienceId);
  const template = templates.find((t) => t.id === templateId);
  const sending = useSendingReadiness(accountId, senderId);
  const configured = sending.ready;
  const complete = [
    !!configured,
    (accountId === "self" && senderId === "") || senders.some((s) => s.id === senderId),
    !!audience && roster.id === audience.id && roster.count !== null && roster.count > 0,
    template?.status === "ready",
    false,
  ];
  // A missing provider must not trap a person who wants to prepare a draft.
  // It is never painted complete, and remains visible on the final review.
  const canContinue = organizationChosen(accounts, accountId) && (step < 2 || (step === 2 ? !!audience : !!template));
  function select(next: number) {
    if (next === 4) setReviewed(true);
    setStep(next);
  }
  const feed =
    step === 1 ? feeds.senders : step === 2 ? feeds.audiences : step === 3 ? feeds.templates : null;
  return (
    <div
      className="os-deploy-pane os-campaign-journey"
      data-os-page-context={JSON.stringify({
        page: "New campaign",
        view: STEPS[step],
      })}
    >
      <div className="os-deploy-scroll os-app-stack">
        <Head title="New campaign" back={{ label: "Campaigns", onSelect: () => onDone("") }}>
          <InfoDetail title="Campaign setup">
            <p>
              Prepare your sending service, choose a sender and audience, finish your content, then
              save a draft. Test and schedule or send from the saved campaign. No mail is sent by
              this setup.
            </p>
          </InfoDetail>
        </Head>
        <Field label="Organization">
          <AccountPicker id="campaign-organization" label="Campaign organization" required value={accountId} accounts={accounts} onChange={(next) => {
            setAccountId(next); setSenderId(""); setAudienceId(""); setTemplateId(""); setStep(0); setReviewed(false); setEditors({});
          }} />
        </Field>
        <JourneyTrail
          label="Campaign setup"
          steps={STEPS.map((label, i) => ({
            id: String(i),
            label,
            state: complete[i] ? "done" : i === step ? "current" : "pending",
            available: i <= step,
          }))}
          selected={String(step)}
          onSelect={(id) => select(Number(id))}
        />
        {feed && (feed.snapshot.error || feed.snapshot.state !== "live") ? (
          <Notice
            tone="warn"
            sentence="Campaign resources are not current."
            next="Wait for the connection or retry before choosing a resource."
            detail={feed.snapshot.error}
          >
            <Button onClick={feed.reseed}>Retry</Button>
          </Notice>
        ) : null}
        <div hidden={step !== 0}>
          <>
            <Panel label="Sending service">
              <Subhead>Sending service</Subhead>
              <Caption>
                Campaigns sends through the selected organization’s email connection.
                Choose its sender and check setup before sending.
              </Caption>
              {configured ? (
                <Notice
                  tone="info"
                  sentence="Sending settings are configured."
                  next={sending.capture ? "Messages appear in the Email app. External delivery is disabled." : "Send a test email and check the recipient’s inbox to verify delivery."}
                />
              ) : (
                <Notice
                  tone="warn"
                  sentence={
                    sending.state === "ready"
                      ? "Email sending needs setup."
                      : "Sending readiness is not confirmed."
                  }
                  next="Check the organization’s sending domain in Campaigns settings. You can continue preparing a draft."
                  detail={sending.reason}
                />
              )}
              <Button
                onClick={() => {
                  sending.reload();
                  readiness?.reseed();
                }}
                busy={sending.state === "loading"}
                busyLabel="Checking"
              >
                Check sending service
              </Button>
            </Panel>
            <SetupGroup
              app="Campaigns"
              requires={["campaigns"]}
              wants={[]}
              readiness={readiness}
            />
            <Panel label="Provider and domain">
              <Subhead>Provider and domain</Subhead>
              <Caption>
                An owner or developer can connect the organization’s Azure email domain in
                Campaigns → Settings. Complete the DNS verification there,
                then choose the verified sender here.
              </Caption>
              <Caption>
                Installation prepares the unsubscribe link automatically. Each organization uses its own
                verified domain and sender.
              </Caption>
            </Panel>
          </>
        </div>
        <div hidden={step !== 1}>
          <Panel label="Choose sender">
            <Subhead>Sender</Subhead>
            <Field label="Sends as">
              <Select
                id="journey-sender"
                label="Campaign sending mailbox"
                value={senderId}
                onChange={setSenderId}
              >
                <option value="">{accountId === "self" ? "Operator organization’s default mailbox" : "Choose this organization’s sender"}</option>
                {senders.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.fromName} — {s.address}
                  </option>
                ))}
              </Select>
            </Field>
            <Caption>
              Adding a mailbox records its address. Your email provider must separately allow this
              cluster to send from it.
            </Caption>
            {!senders.length && !editors[1] ? (
              <EmptyState icon={Mail} title="No sending mailboxes">
                Use the configured default or add a mailbox you are authorized to use.
              </EmptyState>
            ) : null}
            {editors[1] ? (
              <SenderForm
                initialAccountId={accountId}
                writes={writes}
                onDone={(id) => {
                  if (id) {
                    setSenderId(id);
                    feeds.senders.reseed();
                  }
                  setCreating(false);
                }}
              />
            ) : (
              <Button onClick={() => setCreating(true)}>Add a mailbox</Button>
            )}
          </Panel>
        </div>
        <div hidden={step !== 2}>
          <>
            <Panel label="Choose audience">
              <Subhead>Audience</Subhead>
              <Field label="Send to">
                <Select
                  id="journey-audience"
                  label="Campaign audience"
                  value={audienceId}
                  onChange={setAudienceId}
                >
                  <option value="">Choose an audience</option>
                  {audiences.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                    </option>
                  ))}
                </Select>
              </Field>
              {!audiences.length && !editors[2] ? (
                <EmptyState icon={Users} title="No audiences yet">
                  Create an audience, then add people who agreed to receive your email.
                </EmptyState>
              ) : null}
              {editors[2] ? (
                <AudienceForm
                  initialAccountId={accountId}
                  writes={writes}
                  onDone={(id) => {
                    if (id) {
                      setAudienceId(id);
                      feeds.audiences.reseed();
                    }
                    setCreating(false);
                  }}
                />
              ) : (
                <Button onClick={() => setCreating(true)}>Create an audience</Button>
              )}
            </Panel>
            {audience ? (
              <AudienceDetail
                key={audience.id}
                audience={audience}
                writes={writes}
                uploads={uploads}
                onArchived={() => setAudienceId("")}
                onReadiness={onRoster}
              />
            ) : null}
          </>
        </div>
        <div hidden={step !== 3}>
          <Panel label="Choose content">
            <Subhead>Content</Subhead>
            <Field label="Template">
              <Select
                id="journey-template"
                label="Campaign content"
                value={templateId}
                onChange={setTemplateId}
              >
                <option value="">Choose a template</option>
                {templates.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name} ({t.status})
                  </option>
                ))}
              </Select>
            </Field>
            {!templates.length && !editors[3] ? (
              <EmptyState icon={FileText} title="No templates yet">
                Create a template, then review and publish its content in Productivity Tools.
              </EmptyState>
            ) : null}
            {editors[3] ? (
              <NewTemplate
                initialAccountId={accountId}
                writes={writes}
                onCreated={(id, name, tab) => {
                  const url = editorTemplateURL(config.domain, id, name);
                  if (tab) tab.navigate(url); else openHandoff(url, () => {});
                  if (id) {
                    setTemplateId(id);
                    feeds.templates.reseed();
                  }
                  setCreating(false);
                }}
              />
            ) : (
              <Button onClick={() => setCreating(true)}>Create a template</Button>
            )}
            {template && !editors[3] ? (
              <Button onClick={() => openHandoff(editorTemplateURL(config.domain, template.id, template.name), () => {})}>Open template in editor</Button>
            ) : null}
          </Panel>
        </div>
        {reviewed ? (
          <div hidden={step !== 4}>
            <Panel label="Before sending">
              <Subhead>Review and save</Subhead>
              <Caption>
                Save a draft and send a test copy. Review and publish the template in the editor
                before choosing Send now or Schedule from the campaign.
              </Caption>
              {!configured ? (
                <Notice
                  tone="warn"
                  sentence="Sending setup is still incomplete or unconfirmed."
                  next="You can save this draft. Finish the sending service setup before testing or sending."
                />
              ) : null}
              {template?.status !== "ready" ? (
                <Notice
                  tone="warn"
                  sentence="The selected content is not ready to send."
                  next="Finish the template in Content before sending."
                />
              ) : null}
            </Panel>
            {!audience ||
            roster.id !== audience.id ||
            roster.count === null ||
            roster.count === 0 ? (
              <Notice
                tone="warn"
                sentence="The audience has no confirmed subscribed recipients."
                next="Review the audience roster before sending. Suppression is checked again at send time."
              />
            ) : null}
            <CampaignForm
              audiences={audiences}
              templates={templates}
              senders={senders}
              initial={{ audienceId, templateId, senderIdentityId: senderId, accountId }}
              writes={writes}
              trackByDefault={trackByDefault}
              onDone={onDone}
            />
          </div>
        ) : null}
      </div>
      <ActionBar
        state="Preparing a draft"
        detail="No email is sent during setup."
        acts={
          step === 4
            ? []
            : [
                ...(step > 0 ? [{ label: "Previous", onAct: () => select(step - 1) }] : []),
                ...(canContinue && !creating
                  ? [
                      {
                        label: step === 0 && !configured ? "Continue with draft" : "Continue",
                        tone: "primary" as const,
                        onAct: () => select(step + 1),
                      },
                    ]
                  : []),
              ]
        }
      />
    </div>
  );
}
