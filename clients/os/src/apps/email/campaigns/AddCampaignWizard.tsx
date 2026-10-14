import { accountDefaultSender } from "../../accounts/defaults";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Send } from "lucide-react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { VSCODE_NO_ANSWER_MESSAGE } from "../../../items/vscode";
import {
  AutoRefresh,
  ChoiceStack,
  DateTimeInput,
  Field,
  Input,
  Notice,
  Select,
  Switch,
  canConfigure,
  useNow,
  type ChoiceOption,
} from "../../../kit";
import { ContentSkeleton, InlineSkeleton } from "../../../kit/ContentSkeleton";
import { absent } from "../../../kit/measure";
import type { Stop } from "../../../kit/Rail";
import { Wizard, type WizardStatus } from "../../../kit/Wizard";
import { accessAdmits } from "../../../system/registry";
import { useDefaultOrganization } from "../../accounts/organization";
import { accountName } from "../../accounts/rows";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { useCreateCampaign, useSendControls, useSeriesControls, useTestSend, useUpdateCampaign, type CampaignFacts } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { recordNamed, useOpenInbox, useOpenRecord, useOpenSettings } from "../openRequest";
import { isOperatorOrganization, organizationName, selectableOrganizations } from "../organizations";
import {
  audienceIsArchived,
  audienceName,
  audienceProjection,
  campaignFromRow,
  isPublished,
  senderIsRetired,
  senderLabel,
  senderProjection,
  seriesFor,
  templateIsArchived,
  templateName,
  templateProjection,
  testSettingFor,
  useProjected,
  type AudienceRow,
} from "../rows";
import { EMAIL_SECTIONS } from "../settings";
import { arrivedTemplate, beginTemplateWait, openNewEmail, waitWords, type TemplateWait } from "../templates/newTemplate";
import {
  EMAIL_REFRESH_MS,
  knowledgeOf,
  listKnowledge,
  useSendingReadiness,
  useSubscriberCounts,
  type EmailFeeds,
  type EmailRecordFeeds,
  type Knowledge,
} from "../useEmail";
import { browserZone, elapsedClock, instantToLocal, localToInstant, whenWords } from "../when";
import { WORDS } from "../words";
import { useLanding } from "./landing";
import {
  askingActs,
  campaignNameFor,
  campaignSentence,
  firstOpen,
  reviewActs,
  reviewQuestion,
  reviewReading,
  stepAfter,
  stepAnswered,
  stepReachable,
  stepsFor,
  type CampaignDraft,
  type CampaignStep,
  type ReviewFacts,
  type When,
} from "./wizardModel";

// ADD A CAMPAIGN -- on the shell's one wizard (DESIGN.md, "One wizard for adding").
//
//   Organization (only when there is a choice) -> Audience -> Template -> Sender
//   -> When -> Review
//
// ONE QUESTION A STEP, AND A STEP THAT IS ONE CHOICE IS ANSWERED BY CHOOSING:
// there is no Next to press after picking an audience, a template or a sender.
// When asks for a time when the answer is Schedule or Repeat, so it is the one
// step with a Next. A step whose answer has to be made somewhere else -- an
// audience, sending set up -- carries the way there as its floor's act, and the
// wizard waits in its pane for the person to come back (Ruling 19).
//
// NOTHING IS WRITTEN UNTIL REVIEW, AND THEN UNDER ONE ID. The campaign's id is
// minted when the wizard opens and held for as long as it is: a test, a send
// that was refused and the send tried again are all the same draft, and a
// second press while one is on its way is the same press. Until then the way
// out is the trail's Back and costs nothing; once a draft exists the floor
// offers Cancel, which asks by name and cancels it.
//
// THE MODEL IS PURE (`wizardModel.ts`); this file holds the reads, the writes and
// what each step draws.

/** The value the sender choice uses for this cluster's own sender (`senderIdentityId` ""). */
const CLUSTER_SENDER = ":cluster";

const STEP_NAMES: Record<CampaignStep, string> = {
  organization: "Organization",
  audience: "Audience",
  template: "Template",
  sender: "Sender",
  when: "When",
  review: "Review",
};

/** What each step asks, under its name. Review asks nothing: it is where the acting is. */
const STEP_ASKS: Partial<Record<CampaignStep, string>> = {
  organization: "Whose campaign this is.",
  audience: "Who receives it.",
  template: "What it says.",
  sender: "Who it comes from.",
  when: "Now, at a set time, or every few weeks.",
};

const WEEKS = Array.from({ length: 52 }, (_, n) => n + 1);

/** The resource the Inbox tab is gated on: who may read what a capture cluster kept. */
const INBOX_REQUIRES = EMAIL_SECTIONS.find((section) => section.id === "inbox")?.requires;

/** A sentence's first letter in capitals: a rail line or a fact starts a sentence of its own. */
const capitalized = (words: string) => words.charAt(0).toUpperCase() + words.slice(1);

/** Whether two writes would leave the same draft. */
function sameFacts(a: CampaignFacts, b: CampaignFacts): boolean {
  return (Object.keys(a) as (keyof CampaignFacts)[]).every((key) => a[key] === b[key]);
}

/** Who a test goes to (Ruling 10): the organization's testing audience, else the person, else nobody. */
type TestTarget = { kind: "audience"; audience: AudienceRow } | { kind: "me"; address: string };

/** The write that was refused last, which says what the notice over Review reads. */
type Refused = "add" | "update" | "send" | "test" | "cancel";

/** What Review's forward act does, once confirmed. */
type ForwardAct = "send" | "schedule" | "repeat";

export function AddCampaignWizard({ feeds, recordFeeds, trackByDefault, onBack, onDone }: {
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  /** What the two tracking switches start at: the app's preference. */
  trackByDefault: boolean;
  /** Leave, back to the list. Nothing is undone: a draft that was written stays a draft. */
  onBack: () => void;
  /** The campaign was added and sent, scheduled, started or saved: open its page. */
  onDone: (campaignId: string) => void;
}) {
  const { access, config } = useSession();
  const openRecord = useOpenRecord();
  const openSettings = useOpenSettings();
  const openInbox = useOpenInbox();
  const zone = browserZone();

  // ---- whose campaign ------------------------------------------------------
  const { accounts, state: accountsState } = useAccountOptionsFeed();
  const choices = useMemo(() => selectableOrganizations(accounts), [accounts]);
  const fallback = useDefaultOrganization(accounts);
  const [picked, setPicked] = useState("");
  // What was chosen, if it can be; else the only one there is; else where this
  // person starts. With several and no start, nothing until they choose.
  const accountId = choices.some((choice) => choice.id === picked)
    ? picked
    : choices.length === 1
      ? choices[0]!.id
      : choices.some((choice) => choice.id === fallback)
        ? fallback
        : "";
  const chooseOrganization = choices.length > 1;
  const organization = organizationName(accounts, accountId);

  // ---- the answers ---------------------------------------------------------
  const [campaignId] = useState(newShortId);
  const [answers, setAnswers] = useState<Omit<CampaignDraft, "accountId">>(() => ({
    audienceId: "",
    templateId: "",
    senderIdentityId: null,
    when: null,
    name: "",
    trackOpens: trackByDefault,
    trackClicks: trackByDefault,
  }));
  const allSenders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  const defaultSender = accountDefaultSender(accounts.find(account => account.id === accountId), allSenders);
  const draft: CampaignDraft = { ...answers, accountId, senderIdentityId: answers.senderIdentityId ?? (defaultSender || null) };
  const [chosenStep, setChosenStep] = useState<CampaignStep | null>(null);
  const open: CampaignStep =
    chosenStep !== null && stepReachable(chosenStep, draft, chooseOrganization) ? chosenStep : firstOpen(draft, chooseOrganization);
  const steps = stepsFor(draft, chooseOrganization);

  // ---- what the organization has -------------------------------------------
  const allAudiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const allTemplates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const audiences = allAudiences.filter((a) => a.accountId === accountId && !audienceIsArchived(a));
  // Published first: a draft can be chosen, and Review says what it still needs.
  const templates = allTemplates
    .filter((t) => t.accountId === accountId && !templateIsArchived(t))
    .sort((a, b) => Number(isPublished(b)) - Number(isPublished(a)));
  const senders = allSenders.filter((s) => s.accountId === accountId && !senderIsRetired(s));
  const audience = allAudiences.find((a) => a.id === draft.audienceId);
  const template = allTemplates.find((t) => t.id === draft.templateId);
  const sender = allSenders.find((s) => s.id === draft.senderIdentityId);
  const operator = isOperatorOrganization(accountId);

  const { counts, reload: recount } = useSubscriberCounts(audiences.map((a) => a.id));
  const sending = useSendingReadiness(accountId, draft.senderIdentityId ?? "");

  // ---- the template wait (Ruling 9) ----------------------------------------
  const [wait, setWait] = useState<TemplateWait | null>(null);
  // By the second while a wait is measured; otherwise often enough to notice a
  // chosen time passing.
  const now = useNow(wait !== null ? 1_000 : 15_000);
  const [editorSilent, setEditorSilent] = useState(false);
  const stopListening = useRef<() => void>(() => {});
  useEffect(() => () => stopListening.current(), []);
  // A wait begins only from a templates list that has finished loading: a row
  // that was always there and arrived late would look new.
  const templatesSettled = listKnowledge(feeds.templates) === "known";
  const arrival = wait === null ? null : arrivedTemplate(wait, allTemplates);
  const arrivedId = arrival?.template.id ?? "";
  useEffect(() => {
    if (wait === null || arrivedId === "") return;
    stopWaiting();
    const next = { ...draft, templateId: arrivedId };
    setAnswers((held) => ({ ...held, templateId: arrivedId }));
    setChosenStep(stepAfter("template", next, chooseOrganization));
    // Acts on the arrival only: the draft is read as it stands when it lands.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wait, arrivedId]);

  function stopWaiting() {
    stopListening.current();
    stopListening.current = () => {};
    setWait(null);
  }

  function createTemplate() {
    setEditorSilent(false);
    setWait(beginTemplateWait(allTemplates, accountId, true, Date.now()));
    // INSIDE THE CLICK: a tab opened after anything asynchronous is a blocked pop-up.
    stopListening.current = openNewEmail(config.domain, accountId, () => {
      setWait(null);
      setEditorSilent(true);
    });
  }

  // ---- tests (Ruling 10) ---------------------------------------------------
  const testing = accountId === "" ? { knowledge: "reading" as Knowledge, refusal: "" } : knowledgeOf(recordFeeds.testSettings, accountId);
  const setting = testSettingFor(recordFeeds.testSettings.snapshot.rows, accountId);
  const testingAudience =
    setting === null || setting.audienceId === ""
      ? undefined
      : allAudiences.find((a) => a.id === setting.audienceId && a.accountId === accountId && !audienceIsArchived(a));
  const address = access?.primaryEmail ?? "";
  // The testing audience when one is set -- and not the audience this campaign
  // goes to, which would be a send rather than a test. Otherwise the person's own
  // address. While the setting is still being read, nowhere yet.
  const target: TestTarget | null =
    testing.knowledge === "known" && testingAudience !== undefined && testingAudience.id !== draft.audienceId
      ? { kind: "audience", audience: testingAudience }
      : (testing.knowledge === "known" || testing.knowledge === "refused") && address !== ""
        ? { kind: "me", address }
        : null;
  // A run to the testing audience needs what a real send needs; a copy to an
  // address reads a draft template too. Either needs sending set up.
  const canTest = target !== null && sending.ready && (target.kind === "me" || (template !== undefined && isPublished(template)));

  // ---- the writes ----------------------------------------------------------
  const create = useCreateCampaign();
  const update = useUpdateCampaign();
  const controls = useSendControls();
  const series = useSeriesControls();
  const testSend = useTestSend();
  const [written, setWritten] = useState<CampaignFacts | null>(null);
  // A create whose answer was lost may still have written the draft, and then
  // its row arrives. Only looked for once a create has been tried: the feed
  // ticks with every send in the cluster.
  const [tried, setTried] = useState(false);
  const arrived = useMemo(
    () => tried && written === null && recordNamed(feeds.campaigns.snapshot.rows, campaignFromRow, campaignId) !== null,
    [tried, written, feeds.campaigns.snapshot.rows, campaignId],
  );
  const draftExists = written !== null || arrived;
  const [asking, setAsking] = useState<"" | "forward" | "cancel">("");
  const [busyId, setBusyId] = useState("");
  const [refused, setRefused] = useState<Refused | null>(null);
  /** What the forward act did last, so a refusal of it is in that write's words, never an older one's. */
  const [forwardBy, setForwardBy] = useState<ForwardAct>("send");

  // A CONFIRMED FORWARD ACT HANDS THE CAMPAIGN OVER ONLY ONCE IT HAS LANDED
  // (`useLanding`): its row has moved on from draft -- sending, scheduled -- or
  // its series is there. Handed over at once, its page opened on the draft as
  // it stood, with Send now live over a send already on its way. Until then the
  // floor holds the act, busy, and nothing else.
  const landedFor = (act: ForwardAct): string => {
    if (act === "repeat") return seriesFor(recordFeeds.series.snapshot.rows, campaignId) !== null ? "landed" : "waiting";
    const row = recordNamed(feeds.campaigns.snapshot.rows, campaignFromRow, campaignId);
    return row !== null && row.status !== "draft" ? "landed" : "waiting";
  };
  const flight = useLanding<ForwardAct>({
    marker: landedFor,
    reread: (act) => (act === "repeat" ? recordFeeds.series.refresh(campaignId) : feeds.campaigns.reseed()),
    onLanded: () => onDone(campaignId),
  });
  const landing = flight.landing;
  const [tested, setTested] = useState<TestTarget | null>(null);
  const working = useRef(false);

  const facts: ReviewFacts = {
    subscribers: counts.get(draft.audienceId) ?? absent("unread"),
    template,
    sendingReady: sending.ready,
    sendingKnown: sending.state !== "loading",
    sendingFailed: sending.failed,
    sendingReason: sending.reason,
    organizationName: organization,
    audienceName: audience ? audienceName(audience) : "",
    draftExists,
    canTest,
  };
  const reading = reviewReading(draft, facts, now);
  // A question about an act that stopped being legal (its time passed while it
  // was asked) is dropped, not left to come back when the act does.
  useEffect(() => {
    if (asking === "forward" && reading.forward === null) setAsking("");
  }, [asking, reading.forward]);

  /** The campaign as it would be written now. */
  function factsFor(d: CampaignDraft): CampaignFacts {
    return {
      name: campaignNameFor(d, template),
      audienceId: d.audienceId,
      templateId: d.templateId,
      fromName: "",
      replyTo: "",
      // A schedule's time travels with the draft; a repeat cannot (Ruling 8).
      scheduledAt: d.when?.kind === "schedule" ? (localToInstant(d.when.at) ?? "") : "",
      accountId: d.accountId,
      senderIdentityId: d.senderIdentityId ?? "",
      trackOpens: d.trackOpens,
      trackClicks: d.trackClicks,
    };
  }

  /**
   * The draft, as Review says it now: written once under the wizard's id, and
   * brought up to date when an answer changed after it was (a name typed after a
   * test). False, with the refusal noted, when the cluster did not take it.
   */
  async function ensureDraft(): Promise<boolean> {
    const wanted = factsFor(draft);
    if (!draftExists) {
      setTried(true);
      if (!(await create.create(wanted, campaignId))) {
        setRefused("add");
        return false;
      }
      setWritten(wanted);
      return true;
    }
    if (written !== null && sameFacts(written, wanted)) return true;
    if (!(await update.update(campaignId, wanted))) {
      setRefused("update");
      return false;
    }
    setWritten(wanted);
    return true;
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

  const goForward = (spec: ActSpec) =>
    once("forward-confirmed", async () => {
      const when = draft.when ?? { kind: "now" };
      const act: ForwardAct = when.kind === "now" ? "send" : when.kind === "schedule" ? "schedule" : "repeat";
      setAsking("");
      setForwardBy(act);
      flight.begin(act, spec);
      if (!(await ensureDraft())) {
        flight.drop();
        return;
      }
      const instant = when.kind === "now" ? "" : (localToInstant(when.at) ?? "");
      const done =
        when.kind === "now"
          ? await controls.start(campaignId)
          : when.kind === "schedule"
            ? await controls.schedule(campaignId, instant)
            : await series.save({ campaignId, intervalWeeks: when.weeks, firstSendAt: instant, timeZone: zone });
      if (!done) {
        flight.drop();
        setRefused("send");
        return;
      }
      // Taken: handed over once it lands, or after LANDING_MS with what was read again.
      flight.taken();
      if (act === "repeat") recordFeeds.series.refresh(campaignId);
    });

  const sendTest = () =>
    once("test", async () => {
      if (target === null) return;
      setTested(null);
      if (!(await ensureDraft())) return;
      const sent = target.kind === "audience" ? await testSend.send(campaignId, target.audience.id) : await testSend.sendToMe(campaignId, target.address);
      if (!sent) {
        setRefused("test");
        return;
      }
      setTested(target);
    });

  const saveDraft = () =>
    once("draft", async () => {
      if (await ensureDraft()) onDone(campaignId);
    });

  const cancelDraft = () =>
    once("cancel-confirmed", async () => {
      if (!(await controls.cancel(campaignId))) {
        setRefused("cancel");
        setAsking("");
        return;
      }
      onBack();
    });

  // ---- answering -----------------------------------------------------------
  function answer(step: CampaignStep, patch: Partial<Omit<CampaignDraft, "accountId">>) {
    const next = { ...draft, ...patch };
    setAnswers((held) => ({ ...held, senderIdentityId: held.senderIdentityId ?? draft.senderIdentityId, ...patch }));
    setChosenStep(stepAfter(step, next, chooseOrganization));
    setAsking("");
  }

  function pickOrganization(next: string) {
    // Its audiences, templates and senders are the organization's own: a new one asks again.
    if (next !== accountId) {
      if (wait !== null) stopWaiting();
      setAnswers((held) => ({ ...held, audienceId: "", templateId: "", senderIdentityId: null }));
    }
    setPicked(next);
    setChosenStep(stepAfter("organization", { ...draft, accountId: next, audienceId: "", templateId: "", senderIdentityId: null }, true));
    setAsking("");
  }

  function setWhen(when: When) {
    setAnswers((held) => ({ ...held, senderIdentityId: held.senderIdentityId ?? draft.senderIdentityId, when }));
    setAsking("");
  }

  function chooseWhen(kind: string) {
    const at = draft.when !== null && draft.when.kind !== "now" ? draft.when.at : "";
    const weeks = draft.when?.kind === "repeat" ? draft.when.weeks : 1;
    if (kind === "now") answer("when", { when: { kind: "now" } });
    else if (kind === "schedule") setWhen({ kind: "schedule", at });
    else setWhen({ kind: "repeat", weeks, at });
  }

  /** A schedule or repeat whose time is a time and is still to come. */
  const whenReady =
    draft.when !== null && draft.when.kind !== "now" && localToInstant(draft.when.at) !== null && Date.parse(localToInstant(draft.when.at)!) > now.getTime();

  // ---- the floor -----------------------------------------------------------
  const audiencesKnown = listKnowledge(feeds.audiences);
  const templatesKnown = listKnowledge(feeds.templates);
  const sendersKnown = listKnowledge(feeds.senders);
  const name = campaignNameFor(draft, template);

  function floor(): { status: WizardStatus; specs: ActSpec[]; question: ReactNode } {
    // A change on its way holds the floor: the act it was, busy, and nothing else.
    if (landing !== null) return { status: { word: WORDS.changing[landing.act], tone: "busy" }, specs: [landing.spec], question: null };
    // A question asked in the bar holds the floor, whichever step is showing.
    const askingNow = asking === "forward" && reading.forward === null ? "" : asking;
    if (askingNow === "forward") {
      return { status: { word: reading.word, detail: reading.detail }, specs: askingActs("forward", reading.forward), question: reviewQuestion(draft, facts, zone) };
    }
    if (askingNow === "cancel") {
      return { status: { word: reading.word, detail: reading.detail }, specs: askingActs("cancel", null), question: `Cancel “${name}”?` };
    }
    // Somebody else's turn: the editor has the template.
    if (wait !== null) {
      const words = waitWords(wait, arrivedTemplate({ ...wait, requirePublished: false }, allTemplates));
      return {
        status: { ...words, tone: "busy", meta: elapsedClock(now.getTime() - wait.startedAt) },
        specs: [
          { id: "leave", label: "Cancel", text: true },
          { id: "stop-waiting", label: "Stop waiting", text: true },
        ],
        question: null,
      };
    }
    switch (open) {
      case "organization":
        return { status: { word: "Choose an organization" }, specs: [], question: null };
      case "audience":
        return {
          status: { word: "Choose an audience" },
          specs:
            audiencesKnown === "refused"
              ? [{ id: "retry-audiences", label: "Try again", tone: "primary" }]
              : audiencesKnown === "known" && audiences.length === 0
                ? [{ id: "add-audience", label: "Add an audience", tone: "primary" }]
                : [],
          question: null,
        };
      case "template":
        return {
          status: { word: "Choose a template" },
          specs:
            templatesKnown === "refused"
              ? [{ id: "retry-templates", label: "Try again", tone: "primary" }]
              : templatesSettled && accountId !== ""
                ? [templates.length === 0 ? { id: "create-template", label: "Create new template", tone: "primary" } : { id: "create-template", label: "Create new template", text: true }]
                : [],
          question: null,
        };
      case "sender":
        return {
          status: { word: "Choose a sender" },
          specs:
            sendersKnown === "refused"
              ? [{ id: "retry-senders", label: "Try again", tone: "primary" }]
              : sendersKnown === "known" && !operator && senders.length === 0 && canConfigure(access?.role ?? "")
                ? [{ id: "set-up-sending", label: "Set up sending", tone: "primary" }]
                : [],
          question: null,
        };
      case "when": {
        // What is missing, once a way is chosen: a time, then one still to come.
        const at = draft.when === null || draft.when.kind === "now" ? null : localToInstant(draft.when.at);
        const word = at === null ? (draft.when === null || draft.when.kind === "now" ? "Choose when it goes" : "Choose a time") : whenReady ? "Choose when it goes" : "Choose a time in the future.";
        return { status: { word }, specs: whenReady ? [{ id: "next", label: "Next", tone: "primary" }] : [], question: null };
      }
      case "review":
        return { status: { word: reading.word, detail: reading.detail }, specs: reviewActs(draft, facts, now), question: null };
    }
  }

  function run(id: string) {
    switch (id) {
      case "send":
      case "schedule":
      case "repeat":
        setAsking("forward");
        return;
      case "not-yet":
      case "keep":
        setAsking("");
        return;
      case "forward-confirmed":
        void goForward(specs.find((spec) => spec.id === id) ?? { id, label: id });
        return;
      case "test":
        void sendTest();
        return;
      case "draft":
        void saveDraft();
        return;
      case "cancel":
        setAsking("cancel");
        return;
      case "cancel-confirmed":
        void cancelDraft();
        return;
      case "leave":
        // Cancel while waiting: before anything is written that is simply going;
        // after, it is the draft's Cancel, which asks first.
        stopWaiting();
        if (draftExists) setAsking("cancel");
        else onBack();
        return;
      case "stop-waiting":
        stopWaiting();
        return;
      case "add-audience":
        openRecord("audiences", { add: true });
        return;
      case "create-template":
        createTemplate();
        return;
      case "set-up-sending":
        openSettings(accountId);
        return;
      case "next":
        setChosenStep(stepAfter("when", draft, chooseOrganization));
        return;
      case "retry-audiences":
        feeds.audiences.reseed();
        return;
      case "retry-templates":
        feeds.templates.reseed();
        return;
      case "retry-senders":
        feeds.senders.reseed();
        return;
    }
  }

  // ---- the steps -----------------------------------------------------------
  // The sender as a sentence says it; a line that starts with it capitalizes it.
  const senderWords = draft.senderIdentityId === "" ? WORDS.clusterSender : sender ? senderLabel(sender) : "";

  function answerOf(step: CampaignStep): string | undefined {
    if (!stepAnswered(step, draft)) return undefined;
    switch (step) {
      case "organization":
        return organization || undefined;
      case "audience":
        return audience ? audienceName(audience) : undefined;
      case "template":
        return template ? templateName(template) : undefined;
      case "sender":
        // An address is said as it is spelled; only the cluster's phrase starts a line.
        return draft.senderIdentityId === "" ? capitalized(WORDS.clusterSender) : senderWords || undefined;
      case "when":
        return draft.when === null ? undefined : capitalized(whenWords(draft.when, zone));
      case "review":
        return undefined;
    }
  }

  /** A list a step offers that is still being read, or was refused: never an empty one. */
  function unread(knowledge: Knowledge, refusal: string, what: string): ReactNode | null {
    if (knowledge === "reading") return <ContentSkeleton kind="form" label={`Loading ${what}`} />;
    if (knowledge === "refused") return <Notice tone="error" sentence={`This cluster did not return the ${what}.`} detail={refusal} />;
    return null;
  }

  function bodyOf(step: CampaignStep): ReactNode {
    switch (step) {
      case "organization":
        if (choices.length === 0) {
          return accountsState === "seeding" ? <ContentSkeleton kind="form" label="Loading organizations" /> : <p>No organization is available to you.</p>;
        }
        return (
          <ChoiceStack
            name="email-campaign-organization"
            label="Organization"
            voice="prose"
            value={accountId}
            options={choices.map((account) => ({ value: account.id, label: accountName(account) }))}
            onChange={pickOrganization}
          />
        );

      case "audience":
        return (
          unread(audiencesKnown, feeds.audiences.snapshot.error, "audiences") ??
          (audiences.length === 0 ? (
            <p>This organization has no audiences yet.</p>
          ) : (
            <ChoiceStack
              name="email-campaign-audience"
              label="Audience"
              voice="prose"
              value={draft.audienceId}
              options={audiences.map((a): ChoiceOption => ({ value: a.id, label: audienceName(a), description: audienceNote(a) }))}
              onChange={(audienceId) => answer("audience", { audienceId })}
            />
          ))
        );

      case "template":
        return (
          <>
            {editorSilent ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
            {unread(templatesKnown, feeds.templates.snapshot.error, "templates") ??
              (templates.length === 0 ? (
                <p>This organization has no templates yet.</p>
              ) : (
                <ChoiceStack
                  name="email-campaign-template"
                  label="Template"
                  voice="prose"
                  value={draft.templateId}
                  options={templates.map(
                    (t): ChoiceOption => ({ value: t.id, label: templateName(t), description: isPublished(t) ? undefined : WORDS.template.draft }),
                  )}
                  onChange={(templateId) => {
                    // Choosing one that is already here is an answer too: stop waiting for another.
                    if (wait !== null) stopWaiting();
                    answer("template", { templateId });
                  }}
                />
              ))}
          </>
        );

      case "sender": {
        const notRead = unread(sendersKnown, feeds.senders.snapshot.error, "senders");
        if (notRead !== null) return notRead;
        if (!operator && senders.length === 0) {
          return canConfigure(access?.role ?? "") ? (
            <p>{`${organization} has no senders yet.`}</p>
          ) : (
            <p>{`An owner or developer sets up sending for ${organization}.`}</p>
          );
        }
        const options: ChoiceOption[] = [
          ...(operator ? [{ value: CLUSTER_SENDER, label: capitalized(WORDS.clusterSender) }] : []),
          ...senders.map((s): ChoiceOption => ({ value: s.id, label: senderLabel(s), description: s.fromName || undefined })),
        ];
        return (
          <ChoiceStack
            name="email-campaign-sender"
            label="Sender"
            voice="prose"
            value={draft.senderIdentityId === null ? "" : draft.senderIdentityId === "" ? CLUSTER_SENDER : draft.senderIdentityId}
            options={options}
            onChange={(value) => answer("sender", { senderIdentityId: value === CLUSTER_SENDER ? "" : value })}
          />
        );
      }

      case "when": {
        const earliest = instantToLocal(now);
        return (
          <>
            <ChoiceStack
              name="email-campaign-when"
              label="When it goes"
              voice="prose"
              value={draft.when?.kind ?? ""}
              options={[
                { value: "now", label: "Send now" },
                { value: "schedule", label: "Schedule" },
                { value: "repeat", label: "Repeat" },
              ]}
              onChange={chooseWhen}
            />
            {draft.when?.kind === "schedule" ? (
              <Field label={`Send at · ${zone}`}>
                <DateTimeInput id="email-campaign-send-at" label={`Send at · ${zone}`} value={draft.when.at} min={earliest} onChange={(at) => setWhen({ kind: "schedule", at })} />
              </Field>
            ) : null}
            {draft.when?.kind === "repeat" ? (
              <>
                <Field label="Every">
                  <Select
                    id="email-campaign-every"
                    label="Every"
                    value={String(draft.when.weeks)}
                    onChange={(weeks) => draft.when?.kind === "repeat" && setWhen({ ...draft.when, weeks: Number(weeks) })}
                  >
                    {WEEKS.map((n) => (
                      <option key={n} value={String(n)}>
                        {n === 1 ? "1 week" : `${n} weeks`}
                      </option>
                    ))}
                  </Select>
                </Field>
                <Field label={`Starting · ${zone}`}>
                  <DateTimeInput
                    id="email-campaign-starting"
                    label={`Starting · ${zone}`}
                    value={draft.when.at}
                    min={earliest}
                    onChange={(at) => draft.when?.kind === "repeat" && setWhen({ ...draft.when, at })}
                  />
                </Field>
              </>
            ) : null}
          </>
        );
      }

      case "review":
        return reviewBody();
    }
  }

  /** "1,204 subscribers · Asks people to confirm": what choosing an audience means. */
  function audienceNote(a: AudienceRow): ReactNode {
    const figure = counts.get(a.id) ?? absent("unread");
    const confirms = a.confirmation === "required" ? "Asks people to confirm" : "";
    if (figure.kind === "absent" && figure.reason === "unread") {
      return (
        <>
          <InlineSkeleton label="Counting subscribers" />
          {confirms === "" ? null : ` · ${confirms}`}
        </>
      );
    }
    const parts = [figure.kind === "measured" ? WORDS.subscribers(figure) : "", confirms].filter((part) => part !== "");
    return parts.length === 0 ? undefined : parts.join(" · ");
  }

  /**
   * REVIEW SAYS EACH ANSWER ONCE. The rail beside it lists every answer; the one
   * sentence here adds only what the rail cannot -- how many people it reaches,
   * and whose campaign it is when there was a choice of organization. Then the
   * two things only Review asks: what to call it, and what to count.
   */
  function reviewBody(): ReactNode {
    return (
      <>
        {outcome()}
        {draft.when === null ? null : (
          <p>
            {campaignSentence(
              {
                template: template ? templateName(template) : "",
                audience: facts.audienceName,
                sender: senderWords,
                subscribers: facts.subscribers,
                organization: chooseOrganization ? organization : undefined,
              },
              draft.when,
              zone,
            )}
          </p>
        )}
        <Field label="Name">
          <Input
            id="email-campaign-name"
            label="Name"
            value={draft.name}
            placeholder={template ? templateName(template) : ""}
            onChange={(next) => setAnswers((held) => ({ ...held, name: next }))}
          />
        </Field>
        <Switch checked={draft.trackOpens} onChange={(trackOpens) => setAnswers((held) => ({ ...held, trackOpens }))}>
          Count who opens it
        </Switch>
        <Switch checked={draft.trackClicks} onChange={(trackClicks) => setAnswers((held) => ({ ...held, trackClicks }))}>
          Count who clicks a link
        </Switch>
      </>
    );
  }

  /** What the last write on Review came to: a refusal in the engine's words, or where the test went. */
  function outcome(): ReactNode {
    if (refused !== null) {
      switch (refused) {
        case "add":
          return <Notice tone="error" sentence="The campaign wasn't added." next="Nothing was written. What you chose is still here." detail={create.error} />;
        case "update":
          return <Notice tone="error" sentence="The draft wasn't updated, so nothing was sent." detail={update.error} />;
        case "send":
          // In the words of the write it made last: a send refused earlier is not why repeating didn't start.
          return forwardBy === "repeat" ? (
            <Notice tone="error" sentence="The campaign was saved as a draft, but repeating didn't start." detail={series.error} />
          ) : (
            <Notice tone="error" sentence="The campaign was saved as a draft but not sent." detail={controls.error} />
          );
        case "test":
          return <Notice tone="error" sentence="The test wasn't sent." detail={testSend.error} />;
        case "cancel":
          return <Notice tone="error" sentence="The draft wasn't cancelled." detail={controls.error} />;
      }
    }
    if (tested === null) return null;
    const unresolved = tested.kind === "me" ? testSend.unresolved : [];
    return (
      <Notice tone="info" sentence={tested.kind === "audience" ? `Test queued for ${audienceName(tested.audience)}.` : `Test sent to ${tested.address}.`}>
        {unresolved.length === 0 ? null : <p className="os-caption">{`These merge tags didn't resolve: ${unresolved.join(", ")}.`}</p>}
        {!sending.capture ? null : accessAdmits(INBOX_REQUIRES) ? (
          <p className="os-caption">
            <button type="button" className="os-link" onClick={openInbox}>
              View it in Inbox
            </button>
          </p>
        ) : (
          <p className="os-caption">An owner or developer can read it in Email → Inbox.</p>
        )}
      </Notice>
    );
  }

  const stops: Stop[] = steps.map((step) => ({
    id: step,
    name: STEP_NAMES[step],
    state:
      step === open
        ? "open"
        : stepAnswered(step, draft)
          ? "done"
          : stepReachable(step, draft, chooseOrganization)
            ? "waiting"
            : "ahead",
    sentence: STEP_ASKS[step],
    answer: answerOf(step),
    body: step === open ? bodyOf(step) : undefined,
  }));

  const { status, specs, question } = floor();

  return (
    <Wizard
      icon={<Send size={20} aria-hidden />}
      title="Add a campaign"
      breadcrumbs={[{ label: "Campaigns", onSelect: onBack }, { label: "Add a campaign" }]}
      back={{ label: "Campaigns", onSelect: onBack }}
      label="Adding a campaign"
      steps={stops}
      open={open}
      onOpen={(next) => {
        const step = next as CampaignStep;
        if (stepReachable(step, draft, chooseOrganization)) {
          setChosenStep(step);
          setAsking("");
        }
      }}
      status={status}
      acts={toKitActs(specs, run, landing !== null ? landing.spec.id : busyId)}
      confirm={question === null ? null : <div className="os-actbar-confirm">{question}</div>}
      context={{ page: "Add a campaign", step: open }}
    >
      {/* Counted and checked again while this is on screen: an import, a signup
          or a sending domain set up in another tab changes what Review says. */}
      <AutoRefresh onRefresh={recount} intervalMs={EMAIL_REFRESH_MS} />
      <AutoRefresh onRefresh={sending.reload} busy={sending.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
    </Wizard>
  );
}
