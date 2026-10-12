import { useRef, useState, type ReactNode } from "react";

import { useSession } from "../../../chrome/access";
import { DateTimeInput, Field, Head, Input, Notice, Panel, Select, Switch, canConfigure, useNow } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { absent } from "../../../kit/measure";
import { AccountPicker } from "../../accounts/AccountPicker";
import { useDefaultOrganization } from "../../accounts/organization";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { seriesSaveNeeded, useEditCampaign, useSendControls, useSeriesControls, type CampaignChanges } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { useOpenSettings } from "../openRequest";
import { isOperatorOrganization, organizationName, selectableOrganizations } from "../organizations";
import {
  audienceIsArchived,
  audienceName,
  audienceProjection,
  campaignName,
  senderIsRetired,
  senderLabel,
  senderProjection,
  templateIsArchived,
  templateName,
  templateProjection,
  useProjected,
  type CampaignRow,
  type SeriesRow,
} from "../rows";
import type { EmailFeeds, EmailRecordFeeds } from "../useEmail";
import { browserZone, instantToLocal, localToInstant } from "../when";
import { WORDS } from "../words";
import { confirmSentence, repeatSentence } from "./campaignActs";
import { useLanding } from "./landing";
import type { When } from "./wizardModel";

// A CAMPAIGN'S EDIT PAGE (memql#6033, Ruling 7): a draft's fields, or the fields
// of a draft a series repeats from, and one bar.
//
//   nothing changed      Cancel campaign (asks)
//   changed              Cancel campaign, Discard changes, Save
//   Repeat newly chosen  Cancel campaign, Discard changes, Start repeating (asks)
//
// SAVE WRITES WHAT CHANGED AND NOTHING ELSE (`useEditCampaign`), so a change
// somebody else made meanwhile to another field survives. A draft a series
// repeats from is saved twice over when what it repeats changed: the campaign,
// then the series -- an occurrence is made from what the series captured, so
// the draft's edits reach the mail only through that second save, under the
// revision it was read at (`seriesSaveNeeded`). Saving a series never restarts
// it: a paused or stopped one stays so until somebody resumes it, on its page.
// When the first lands and the second is refused, the series is still owed:
// Save stays, and writes only the series.
//
// THE ORGANIZATION IS ASKED ONLY WHEN THERE IS A CHOICE, and a campaign from
// before organizations starts on the person's own. Choosing another clears the
// audience, template and sender: they were the other organization's. A
// campaign a series repeats from is never moved: a series cannot change
// organizations, so the move would leave the two in different ones.

/** The value the sender choice uses for this cluster's own sender (`senderIdentityId` ""). */
const CLUSTER_SENDER = ":cluster";

const WEEKS = Array.from({ length: 52 }, (_, n) => n + 1);

interface EditDraft {
  name: string;
  audienceId: string;
  templateId: string;
  /** "" is no sender chosen -- or, for the operator organization, this cluster's own. */
  senderIdentityId: string;
  replyTo: string;
  trackOpens: boolean;
  trackClicks: boolean;
}

/** An instant as the browser's own `datetime-local` value; "" for none. */
const localOf = (instant: string): string => {
  const at = Date.parse(instant);
  return instant === "" || Number.isNaN(at) ? "" : instantToLocal(new Date(at));
};

/** Whether two instants (either may be empty) are the same moment. */
const sameInstant = (a: string, b: string): boolean => (a === "" || b === "" ? a === b : Date.parse(a) === Date.parse(b));

function draftOf(campaign: CampaignRow): EditDraft {
  return {
    name: campaign.name,
    audienceId: campaign.audienceId,
    templateId: campaign.templateId,
    senderIdentityId: campaign.senderIdentityId,
    replyTo: campaign.replyTo,
    trackOpens: campaign.trackOpens,
    trackClicks: campaign.trackClicks,
  };
}

/** When it goes, as what is saved says: the series' cadence, the draft's time, or nothing. */
function savedWhen(campaign: CampaignRow, series: SeriesRow | null): When {
  if (series) return { kind: "repeat", weeks: series.intervalWeeks || 1, at: localOf(series.anchorAt) };
  return campaign.scheduledAt !== "" ? { kind: "schedule", at: localOf(campaign.scheduledAt) } : { kind: "now" };
}

/** The last write on the page that did not go as asked, which says what its notice reads. */
type Refused = "update" | "series" | "repeat" | "cancel";

/** The confirmed acts that change what the campaign is, and hold the bar until that lands. */
type Lifecycle = "repeat" | "cancel";

export function CampaignEditPage({ campaign, series, feeds, recordFeeds, onBack, onBackToList }: {
  campaign: CampaignRow;
  series: SeriesRow | null;
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  /** Back to the campaign's page: after a save, a discard, or a cancel. */
  onBack: () => void;
  onBackToList: () => void;
}) {
  const { access } = useSession();
  const openSettings = useOpenSettings();
  const zone = browserZone();
  const now = useNow();

  // ---- whose campaign ------------------------------------------------------
  const { accounts } = useAccountOptionsFeed();
  const choices = selectableOrganizations(accounts);
  const fallback = useDefaultOrganization(accounts);
  const [picked, setPicked] = useState<string | null>(null);
  // A campaign keeps its organization; one from before organizations starts on
  // the person's own, or the only one there is.
  const accountId =
    picked ?? (campaign.accountId !== "" ? campaign.accountId : choices.some((c) => c.id === fallback) ? fallback : choices.length === 1 ? choices[0]!.id : "");
  const organization = organizationName(accounts, accountId);
  const operator = isOperatorOrganization(accountId);

  // ---- the answers -----------------------------------------------------------
  const [draft, setDraft] = useState<EditDraft>(() => draftOf(campaign));
  const set = (patch: Partial<EditDraft>) => setDraft((held) => ({ ...held, ...patch }));
  // WHEN IS WHAT IS SAVED until the person changes it -- so a series that
  // arrives after the page opened, or changes while it is open, is what the
  // fields show rather than a guess made before it was read.
  const [whenChosen, setWhen] = useState<When | null>(null);
  const when = whenChosen ?? savedWhen(campaign, series);
  /** The series' first send as it is saved, so leaving it alone sends it back exactly. */
  const anchorAsSaved = series ? localOf(series.anchorAt) : "";

  function pickOrganization(next: string) {
    if (next === accountId) return;
    setPicked(next);
    // Its audiences, templates and senders are the organization's own.
    set({ audienceId: "", templateId: "", senderIdentityId: "" });
  }

  // ---- what the organization has -------------------------------------------
  const allAudiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const allTemplates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const allSenders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  // Put-away records are not offered -- except the one the campaign already
  // names, which an edit must never silently re-point.
  const audiences = allAudiences.filter((a) => a.accountId === accountId && (!audienceIsArchived(a) || a.id === draft.audienceId));
  const templates = allTemplates
    .filter((t) => t.accountId === accountId && (!templateIsArchived(t) || t.id === draft.templateId))
    .sort((a, b) => Number(b.status === "ready") - Number(a.status === "ready"));
  const senders = allSenders.filter((s) => s.accountId === accountId && (!senderIsRetired(s) || s.id === draft.senderIdentityId));
  const audience = audiences.find((a) => a.id === draft.audienceId);
  const senderChosen = draft.senderIdentityId === "" ? operator : senders.some((s) => s.id === draft.senderIdentityId);

  // ---- what changed ----------------------------------------------------------
  const repeat = when.kind === "repeat" ? when : null;
  const edits: CampaignChanges = {};
  if (draft.name.trim() !== campaign.name.trim()) edits.name = draft.name.trim();
  if (draft.audienceId !== campaign.audienceId) edits.audienceId = draft.audienceId;
  if (draft.templateId !== campaign.templateId) edits.templateId = draft.templateId;
  if (accountId !== campaign.accountId) edits.accountId = accountId;
  if (draft.senderIdentityId !== campaign.senderIdentityId) edits.senderIdentityId = draft.senderIdentityId;
  if (draft.replyTo.trim() !== campaign.replyTo.trim()) edits.replyTo = draft.replyTo.trim();
  if (draft.trackOpens !== campaign.trackOpens) edits.trackOpens = draft.trackOpens;
  if (draft.trackClicks !== campaign.trackClicks) edits.trackClicks = draft.trackClicks;
  // The time a draft is saved for. A repeat is the series', never the campaign's.
  if (series === null && repeat === null) {
    const at = when.kind === "schedule" ? (localToInstant(when.at) ?? "") : "";
    if (!sameInstant(at, campaign.scheduledAt)) edits.scheduledAt = at;
  }
  const campaignChanged = Object.keys(edits).length > 0;
  const timeMoved = repeat !== null && (series === null || repeat.at !== anchorAsSaved);
  const seriesChanged = series !== null && repeat !== null && (repeat.weeks !== series.intervalWeeks || timeMoved);
  const startsRepeating = series === null && repeat !== null;
  /** The campaign was saved and its series was refused: the series still has to capture it. */
  const [seriesOwed, setSeriesOwed] = useState(false);
  const dirty = campaignChanged || seriesChanged || startsRepeating || seriesOwed;

  /** The first thing still missing, in the order the page asks for it; "" for nothing. */
  function missing(): string {
    if (accountId === "") return "Choose an organization";
    if (draft.name.trim() === "") return "Add a name";
    if (audience === undefined) return "Choose an audience";
    if (!templates.some((t) => t.id === draft.templateId)) return "Choose a template";
    if (!senderChosen) return "Choose a sender";
    if (when.kind !== "now") {
      const at = localToInstant(when.at);
      if (at === null) return "Choose a time";
      // A time left as it was read stands; a time changed must still be to come.
      const changed = when.kind === "repeat" ? timeMoved : !sameInstant(at, campaign.scheduledAt);
      if (changed && Date.parse(at) <= now.getTime()) return "Choose a time in the future.";
    }
    return "";
  }
  const blocked = missing();

  // ---- the writes ----------------------------------------------------------
  const edit = useEditCampaign();
  const repeats = useSeriesControls();
  const controls = useSendControls();
  const [asking, setAsking] = useState<"" | "cancel" | "repeat">("");
  const [busyId, setBusyId] = useState("");
  /**
   * The last write that did not go as asked: refused, or with its answer lost
   * (it may have been done). `seen` is the campaign and series versions it was
   * said over: once either is read back newer, a lost answer has its answer.
   */
  const [refused, setRefused] = useState<{ write: Refused; campaignSaved: boolean; lost: boolean; seen: string } | null>(null);
  const versions = `${campaign.createdAt}|${series?.createdAt ?? ""}`;
  const working = useRef(false);

  // A CHANGE ON ITS WAY HOLDS THE BAR (`useLanding`): Start repeating until the
  // series arrives, Cancel campaign until the campaign is cancelled -- then the
  // campaign's page, which a cancel reaches on its own (only a draft is edited).
  const markerOf = (act: Lifecycle): string => (act === "repeat" ? (series?.createdAt ?? "") : campaign.status);
  /** Read again what a write moves, or may have moved. */
  const reread = (write: Refused | Lifecycle) => (write === "series" || write === "repeat" ? recordFeeds.series.refresh(campaign.id) : feeds.campaigns.reseed());
  const flight = useLanding<Lifecycle>({ marker: markerOf, reread, onLanded: () => onBack() });
  const landing = flight.landing;

  /** A write that did not go as asked; one whose answer was lost has its subject read again. */
  function failed(write: Refused, campaignSaved: boolean, lost: boolean) {
    setRefused({ write, campaignSaved, lost, seen: versions });
    if (lost) reread(write);
  }

  /** One write at a time: a second press while the first is on its way is the same press. */
  async function once(id: string, work: () => Promise<void>) {
    if (working.current) return;
    working.current = true;
    setBusyId(id);
    setRefused(null);
    try {
      await work();
    } finally {
      working.current = false;
      setBusyId("");
    }
  }

  /** The repeat as the series is saved with it: the first send left alone goes back exactly as it was read. */
  function seriesSave(expectedRevision?: string) {
    const at = repeat === null ? "" : (localToInstant(repeat.at) ?? "");
    return {
      campaignId: campaign.id,
      intervalWeeks: repeat?.weeks ?? series?.intervalWeeks ?? 1,
      firstSendAt: series !== null && !timeMoved ? series.anchorAt : at,
      timeZone: series !== null && !timeMoved && series.timeZone !== "" ? series.timeZone : zone,
      expectedRevision,
    };
  }

  /** The campaign's own changes, if there are any. False, with the refusal noted, when they were not taken. */
  async function saveCampaign(): Promise<boolean> {
    if (!campaignChanged) return true;
    if (await edit.save(campaign, edits)) return true;
    failed("update", false, edit.lost());
    return false;
  }

  const save = () =>
    once("save", async () => {
      if (!(await saveCampaign())) return;
      // The series only when something it repeats changed -- and saving it never
      // restarts it: a paused or stopped series stays so until Resume.
      if (series !== null && seriesSaveNeeded(edits, seriesChanged, seriesOwed)) {
        if (!(await repeats.save(seriesSave(series.createdAt)))) {
          setSeriesOwed(true);
          failed("series", campaignChanged || seriesOwed, repeats.lost());
          return;
        }
        setSeriesOwed(false);
        recordFeeds.series.refresh(campaign.id);
      }
      onBack();
    });

  const startRepeating = (spec: ActSpec) =>
    once("repeat-confirmed", async () => {
      setAsking("");
      flight.begin("repeat", spec);
      if (!(await saveCampaign())) {
        flight.drop();
        return;
      }
      if (!(await repeats.save(seriesSave()))) {
        flight.drop();
        failed("repeat", campaignChanged, repeats.lost());
        return;
      }
      flight.taken();
      recordFeeds.series.refresh(campaign.id);
    });

  const cancelCampaign = (spec: ActSpec) =>
    once("cancel-confirmed", async () => {
      setAsking("");
      flight.begin("cancel", spec);
      if (!(await controls.cancel(campaign.id))) {
        flight.drop();
        failed("cancel", false, controls.lost());
        return;
      }
      flight.taken();
    });

  /** The act a bar press named, as the bar drew it. */
  const specOf = (id: string): ActSpec => barSpecs.find((spec) => spec.id === id) ?? { id, label: id };

  function run(id: string) {
    switch (id) {
      case "cancel":
        setAsking("cancel");
        return;
      case "repeat":
        setAsking("repeat");
        return;
      case "keep":
      case "not-yet":
        setAsking("");
        return;
      case "cancel-confirmed":
        void cancelCampaign(specOf(id));
        return;
      case "repeat-confirmed":
        void startRepeating(specOf(id));
        return;
      case "discard":
        onBack();
        return;
      case "save":
        void save();
        return;
    }
  }

  // ---- the bar ---------------------------------------------------------------
  const forward: ActSpec | null =
    !dirty || blocked !== "" ? null : startsRepeating ? { id: "repeat", label: "Start repeating", tone: "primary" } : { id: "save", label: "Save", tone: "primary" };
  // A question about an act that stopped being legal asks nothing.
  const askingNow = asking === "repeat" && forward?.id !== "repeat" ? "" : asking;
  const specs: ActSpec[] =
    askingNow === "cancel"
      ? [
          { id: "keep", label: "Keep", text: true },
          { id: "cancel-confirmed", label: "Cancel campaign", tone: "danger" },
        ]
      : askingNow === "repeat"
        ? [
            { id: "not-yet", label: "Not yet", text: true },
            { id: "repeat-confirmed", label: "Start repeating", tone: "primary" },
          ]
        : [
            { id: "cancel", label: "Cancel campaign", text: true, tone: "danger" },
            ...(dirty ? [{ id: "discard", label: "Discard changes", text: true }] : []),
            ...(forward === null ? [] : [forward]),
          ];
  // A change on its way holds the bar: the act it was, busy, and nothing else.
  const barSpecs = landing !== null ? [landing.spec] : specs;
  const name = draft.name.trim() || campaignName(campaign);
  const question =
    landing !== null
      ? null
      : askingNow === "cancel"
      ? confirmSentence("cancel-draft", { campaign, series, sendingReady: false, sendingReason: "", canTest: false, organizationName: organization, now }, absent("unread"))
      : askingNow === "repeat" && repeat !== null
        ? repeatSentence(name, audience ? audienceName(audience) : "", repeat, zone)
        : null;

  // ---- what each field says ----------------------------------------------------
  function chooseWhen(kind: string) {
    const at = when.kind === "now" ? "" : when.at;
    if (kind === "now") setWhen({ kind: "now" });
    else if (kind === "schedule") setWhen({ kind: "schedule", at });
    else setWhen({ kind: "repeat", weeks: when.kind === "repeat" ? when.weeks : 1, at });
  }

  const senderValue = draft.senderIdentityId === "" ? (operator ? CLUSTER_SENDER : "") : draft.senderIdentityId;

  function noSenders(): ReactNode {
    if (operator || senders.length > 0 || accountId === "") return null;
    return canConfigure(access?.role ?? "") ? (
      <p className="os-caption">
        {`${organization} has no senders yet. `}
        <button type="button" className="os-link" onClick={() => openSettings(accountId)}>
          Open Settings
        </button>
      </p>
    ) : (
      <p className="os-caption">{`An owner or developer sets up sending for ${organization}.`}</p>
    );
  }

  function outcome(): ReactNode {
    if (refused === null) return null;
    // AN ANSWER THAT NEVER CAME BACK IS NOT A REFUSAL: the write may have been
    // done. What it moves is being read again, and once it is back, newer, the
    // page shows what happened and the notice has nothing left to say.
    if (refused.lost) {
      if (refused.seen !== versions) return null;
      switch (refused.write) {
        case "update":
          return <Notice tone="warn" sentence="Couldn't confirm that your changes were saved." next="What you changed is still here." detail={edit.error} />;
        case "series":
          return <Notice tone="warn" sentence="Couldn't confirm that its repeating schedule was saved." next="What you changed is still here." detail={repeats.error} />;
        case "repeat":
          return <Notice tone="warn" sentence="Couldn't confirm that repeating started." next="Its status will update here." detail={repeats.error} />;
        case "cancel":
          return <Notice tone="warn" sentence="Couldn't confirm that it was cancelled." next="Its status will update here." detail={controls.error} />;
      }
    }
    switch (refused.write) {
      case "update":
        return <Notice tone="error" sentence="Your changes weren't saved." next="What you changed is still here." detail={edit.error} />;
      case "series":
        return (
          <Notice
            tone="error"
            sentence={refused.campaignSaved ? "The campaign was saved, but its repeating schedule wasn't." : "Your changes weren't saved."}
            detail={repeats.error}
          />
        );
      case "repeat":
        return <Notice tone="error" sentence={refused.campaignSaved ? "The campaign was saved, but repeating didn't start." : "Repeating didn't start."} detail={repeats.error} />;
      case "cancel":
        return <Notice tone="error" sentence="The campaign wasn't cancelled." detail={controls.error} />;
    }
  }

  const repeatFields = (at: string, weeks: number) => (
    <>
      <div className="email-field">
        <Field label="Every">
          <Select id="email-campaign-edit-every" label="Every" value={String(weeks)} onChange={(next) => setWhen({ kind: "repeat", weeks: Number(next), at })}>
            {WEEKS.map((n) => (
              <option key={n} value={String(n)}>
                {n === 1 ? "1 week" : `${n} weeks`}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      <div className="email-field">
        <Field label={`Starting · ${zone}`}>
          <DateTimeInput id="email-campaign-edit-starting" label={`Starting · ${zone}`} value={at} onChange={(next) => setWhen({ kind: "repeat", weeks, at: next })} />
        </Field>
      </div>
    </>
  );

  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Edit campaign", name: campaignName(campaign), id: campaign.id })}>
      <div className="os-action-body os-app-stack">
        <Head
          title="Edit campaign"
          breadcrumbs={[{ label: "Campaigns", onSelect: onBackToList }, { label: campaignName(campaign), onSelect: onBack }, { label: "Edit campaign" }]}
          back={{ label: campaignName(campaign), onSelect: onBack }}
        />
        {outcome()}
        <Panel label="Campaign">
          {choices.length > 1 && series === null ? (
            <div className="email-field">
              <Field label="Organization">
                <AccountPicker id="email-campaign-edit-organization" label="Organization" required accounts={choices} value={accountId} onChange={pickOrganization} />
              </Field>
            </div>
          ) : null}
          <div className="email-field">
            <Field label="Name">
              <Input id="email-campaign-edit-name" label="Name" value={draft.name} onChange={(next) => set({ name: next })} />
            </Field>
          </div>
          <div className="email-field">
            <Field label="Audience">
              <Select id="email-campaign-edit-audience" label="Audience" value={draft.audienceId} onChange={(next) => set({ audienceId: next })}>
                {draft.audienceId === "" ? (
                  <option value="" disabled>
                    Choose an audience
                  </option>
                ) : null}
                {audiences.map((a) => (
                  <option key={a.id} value={a.id}>
                    {audienceName(a)}
                    {audienceIsArchived(a) ? ` (${WORDS.archived.toLowerCase()})` : ""}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <div className="email-field">
            <Field label="Template">
              <Select id="email-campaign-edit-template" label="Template" value={draft.templateId} onChange={(next) => set({ templateId: next })}>
                {draft.templateId === "" ? (
                  <option value="" disabled>
                    Choose a template
                  </option>
                ) : null}
                {templates.map((t) => (
                  <option key={t.id} value={t.id}>
                    {templateName(t)}
                    {t.status === "ready" ? "" : ` (${templateIsArchived(t) ? WORDS.archived.toLowerCase() : WORDS.templateDraft})`}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <div className="email-field">
            <Field label="Sender">
              <Select
                id="email-campaign-edit-sender"
                label="Sender"
                value={senderValue}
                onChange={(next) => set({ senderIdentityId: next === CLUSTER_SENDER ? "" : next })}
              >
                {senderValue === "" ? (
                  <option value="" disabled>
                    Choose a sender
                  </option>
                ) : null}
                {operator ? <option value={CLUSTER_SENDER}>{WORDS.clusterSender.charAt(0).toUpperCase() + WORDS.clusterSender.slice(1)}</option> : null}
                {senders.map((s) => (
                  <option key={s.id} value={s.id}>
                    {senderLabel(s)}
                    {senderIsRetired(s) ? ` (${WORDS.archived.toLowerCase()})` : ""}
                  </option>
                ))}
              </Select>
            </Field>
            {noSenders()}
          </div>
          <div className="email-field">
            <Field label="Reply to">
              <Input id="email-campaign-edit-reply" label="Reply to" value={draft.replyTo} onChange={(next) => set({ replyTo: next })} />
            </Field>
          </div>
          <Switch checked={draft.trackOpens} onChange={(trackOpens) => set({ trackOpens })}>
            Count who opens it
          </Switch>
          <Switch checked={draft.trackClicks} onChange={(trackClicks) => set({ trackClicks })}>
            Count who clicks a link
          </Switch>
          {series !== null && repeat !== null ? (
            // It repeats: how often, and from when. Stopping it is Pause, on its page.
            repeatFields(repeat.at, repeat.weeks)
          ) : (
            <>
              <div className="email-field">
                <Field label="When">
                  <Select id="email-campaign-edit-when" label="When" value={when.kind} onChange={chooseWhen}>
                    <option value="now">Send now</option>
                    <option value="schedule">Schedule</option>
                    <option value="repeat">Repeat</option>
                  </Select>
                </Field>
              </div>
              {when.kind === "schedule" ? (
                <div className="email-field">
                  <Field label={`Send at · ${zone}`}>
                    <DateTimeInput
                      id="email-campaign-edit-send-at"
                      label={`Send at · ${zone}`}
                      value={when.at}
                      min={instantToLocal(now)}
                      onChange={(at) => setWhen({ kind: "schedule", at })}
                    />
                  </Field>
                </div>
              ) : null}
              {repeat !== null ? repeatFields(repeat.at, repeat.weeks) : null}
            </>
          )}
        </Panel>
      </div>

      <ActionBar
        state={landing === null ? "Editing" : WORDS.changing[landing.act]}
        detail={landing === null && askingNow === "" ? blocked : undefined}
        tone={landing === null ? "none" : "busy"}
        acts={toKitActs(barSpecs, run, landing !== null ? landing.spec.id : busyId)}
      >
        {question === null ? null : <span className="os-actbar-confirm">{question}</span>}
      </ActionBar>
    </div>
  );
}
