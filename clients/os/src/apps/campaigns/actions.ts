import { useCallback, useState } from "react";
import { newShortId, type Connection, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import type { RecipientMode } from "./rows";

// Every write the Campaigns app makes, and the busy/error pair each one owns.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// Every operator-facing campaigns concept declares the composite tier, so
// `guardRowAuthzWrite` resolves the target row and admits its owner, with the
// cluster-owner path as the separate explicit escape. The five builtins go
// further: their FIRST act is an owned-tier read of the campaign, so a caller
// who cannot read a campaign cannot start, schedule, pause, resume or test it,
// and every value that reaches the send job is copied off that row rather than
// taken from an argument. Editing a boolean in a browser changes none of it.
//
// THE SEND LIFECYCLE GOES THROUGH THE BUILTINS AND NOWHERE ELSE.
// `startCampaign`, `pauseCampaign`, `resumeCampaign`, `scheduleCampaign`,
// `updateCampaignProgress` and `recordCampaignDelivery` are all `@serverOnly`
// (memql#4820's hardening), so a browser cannot reach them at all -- which is
// the point: flipping a status to "sending" with no send job behind it, or
// stamping counters for deliveries that never happened, desyncs the row from
// the engine while owning it perfectly. What this file calls is
// `campaignStartSend` and its four siblings, which do the preflight first.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE, AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast. The preflight refusals are the most useful sentences in this
// app -- "no email sender is registered on this node", "the template is not
// marked ready", "the audience is at the ceiling" -- and each names the exact
// thing to go and fix. A paraphrase would drop that. Every hook below owns its
// own error slot so a refusal always appears under the control that caused it.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Blank strings are OMITTED rather than sent.
 *
 * Every update mutation here read-merges, so an omitted field inherits its
 * stored value -- which is what lets an edit form send only what changed. An
 * explicit "" is a VALUE and would blank the stored one.
 */
function omitBlank(value: string | undefined): string | undefined {
  if (value === undefined) return undefined;
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

export interface WriteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  reset: () => void;
}

/**
 * The busy/error/reset triple, once.
 *
 * ONE FACTORY RATHER THAN TWENTY COPIES. The Accounts app writes its three
 * hooks out longhand and that is right at three; this app makes twenty
 * distinct writes, and twenty copies of the same eleven lines is twenty places
 * for one of them to forget `finally { setBusy(false) }` -- which presents as
 * a button that never comes back and is invisible in review.
 *
 * What is NOT shared is the state itself: every call to this returns its own
 * `useState` pair, so each hook owns its own slot and a refusal from the
 * import panel can never appear under the send button.
 */
function useWrite<A extends unknown[], R>(
  run: (query: Connection["query"], ...args: A) => Promise<R>,
  onDisconnected: R,
): { busy: boolean; error: string; reset: () => void; call: (...args: A) => Promise<R> } {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const call = useCallback(
    async (...args: A): Promise<R> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return onDisconnected;
      }
      setBusy(true);
      setError("");
      try {
        return await run(query, ...args);
      } catch (err: unknown) {
        setError(describe(err));
        return onDisconnected;
      } finally {
        setBusy(false);
      }
    },
    // `run` is a fresh closure each render and closes over nothing that
    // changes what the call DOES, so it is deliberately not a dependency --
    // the same reasoning `useLiveCollection` records for its spec.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [connection],
  );

  return { busy, error, reset: () => setError(""), call };
}

// ---------------------------------------------------------------------------
// Campaigns
// ---------------------------------------------------------------------------

export interface CampaignFacts {
  name: string;
  audienceId: string;
  templateId: string;
  fromName: string;
  replyTo: string;
  scheduledAt: string;
  accountId: string;
  senderIdentityId: string;
  trackOpens: boolean;
  trackClicks: boolean;
}

export interface CreateCampaignState extends WriteState {
  create: (facts: CampaignFacts) => Promise<string>;
}

export function useCreateCampaign(): CreateCampaignState {
  const { busy, error, reset, call } = useWrite(async (query, facts: CampaignFacts) => {
    const campaignId = newShortId();
    await query.createCampaign({
      campaignId,
      name: facts.name.trim(),
      audienceId: facts.audienceId,
      templateId: facts.templateId,
      fromName: omitBlank(facts.fromName),
      replyTo: omitBlank(facts.replyTo),
      scheduledAt: omitBlank(facts.scheduledAt),
      accountId: omitBlank(facts.accountId),
      senderIdentityId: omitBlank(facts.senderIdentityId),
      trackOpens: facts.trackOpens,
      trackClicks: facts.trackClicks,
    });
    // NOTHING IS INSERTED LOCALLY. `v1:campaigns:campaign` broadcasts both
    // verbs, so the row arrives on the feed the list already draws -- with the
    // arrival cue, exactly like a campaign somebody else created.
    return campaignId;
  }, "");
  return { busy, error, reset, create: call };
}

export interface UpdateCampaignState extends WriteState {
  update: (campaignId: string, facts: CampaignFacts) => Promise<boolean>;
}

/**
 * `updateCampaign` takes name / audience / template as REQUIRED arguments, so
 * this is a whole-form save rather than a patch -- the form holds the current
 * values and sends them back. The optional half is still omitted when blank,
 * so clearing a reply-to means clearing it and leaving it alone means leaving
 * it alone.
 */
export function useUpdateCampaign(): UpdateCampaignState {
  const { busy, error, reset, call } = useWrite(
    async (query, campaignId: string, facts: CampaignFacts) => {
      await query.updateCampaign({
        campaignId,
        name: facts.name.trim(),
        audienceId: facts.audienceId,
        templateId: facts.templateId,
        fromName: omitBlank(facts.fromName),
        replyTo: omitBlank(facts.replyTo),
        scheduledAt: omitBlank(facts.scheduledAt),
        accountId: omitBlank(facts.accountId),
        senderIdentityId: omitBlank(facts.senderIdentityId),
        trackOpens: facts.trackOpens,
        trackClicks: facts.trackClicks,
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

export interface SendControlState extends WriteState {
  start: (campaignId: string) => Promise<boolean>;
  schedule: (campaignId: string, scheduledAt: string) => Promise<boolean>;
  pause: (campaignId: string) => Promise<boolean>;
  resume: (campaignId: string) => Promise<boolean>;
  cancel: (campaignId: string) => Promise<boolean>;
}

/**
 * The five lifecycle controls, sharing ONE error slot -- and here that is
 * correct rather than a shortcut.
 *
 * They sit in one cluster on one panel, exactly one can be pressed at a time
 * (a draft has Start, a run has Pause), and the refusal always belongs to the
 * one that was just pressed. Splitting them would put five empty slots under
 * five buttons in a row. This is the case the Accounts note's "three different
 * places on screen" rule is drawing a line AGAINST, not an exception to it.
 */
export function useSendControls(): SendControlState {
  const start = useWrite(async (query, campaignId: string) => {
    await query.campaignStartSend({ campaignId });
    return true;
  }, false);
  const schedule = useWrite(
    async (query, campaignId: string, scheduledAt: string) => {
      await query.campaignScheduleSend({ campaignId, scheduledAt });
      return true;
    },
    false,
  );
  const pause = useWrite(async (query, campaignId: string) => {
    await query.campaignPauseSend({ campaignId });
    return true;
  }, false);
  const resume = useWrite(async (query, campaignId: string) => {
    await query.campaignResumeSend({ campaignId });
    return true;
  }, false);
  const cancel = useWrite(async (query, campaignId: string) => {
    await query.cancelCampaign({ campaignId });
    return true;
  }, false);

  const parts = [start, schedule, pause, resume, cancel];
  return {
    busy: parts.some((p) => p.busy),
    // The one that actually failed. At most one of these can be in flight, so
    // "the first non-empty" is "the last thing that was pressed".
    error: parts.map((p) => p.error).find((e) => e !== "") ?? "",
    reset: () => parts.forEach((p) => p.reset()),
    start: start.call,
    schedule: schedule.call,
    pause: pause.call,
    resume: resume.call,
    cancel: cancel.call,
  };
}

export interface TestSendState extends WriteState {
  /** Merge tags the render could not resolve. Empty after a clean test. */
  unresolved: string[];
  /** True once a test has come back, so the panel can say "Test sent". */
  sent: boolean;
  send: (campaignId: string, to: string) => Promise<boolean>;
}

/**
 * Send one test copy, and report the merge tags it could not resolve.
 *
 * THE UNRESOLVED LIST IS THE POINT, not a side note. It is the check that
 * catches a typo'd `{{fields.compnay}}` before the whole audience gets it, and
 * it is the only check that can: a spelling mistake inside a tag renders as
 * its own literal text into somebody's inbox, and nothing about the message
 * looks wrong from this side.
 */
export function useTestSend(): TestSendState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [unresolved, setUnresolved] = useState<string[]>([]);
  const [sent, setSent] = useState(false);

  const send = useCallback(
    async (campaignId: string, to: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was sent.");
        return false;
      }
      const address = to.trim();
      if (address === "") {
        // The one rule a browser can answer. `to` is required and never
        // defaults to the caller's own address -- deliberately, on the
        // builtin's side -- so an empty box is a question, not a default.
        setError("Say where the test should go.");
        return false;
      }
      setBusy(true);
      setError("");
      setSent(false);
      setUnresolved([]);
      try {
        const result = await query.campaignTestSend({ campaignId, to: address });
        setUnresolved(unresolvedTagsFrom(result.rows()[0] ?? null));
        setSent(true);
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    unresolved,
    sent,
    send,
    reset: () => {
      setError("");
      setUnresolved([]);
      setSent(false);
    },
  };
}

/**
 * Pull the unresolved-tag list out of a test-send reply.
 *
 * TWO SPELLINGS ARE READ, and neither is guessed at: the builtin's own
 * description says it "returns the list of merge tags it could not resolve"
 * without naming the key, so both the obvious ones are read and anything that
 * is not a list of strings is treated as "it said nothing" -- which renders as
 * a clean test rather than as an error about a reply we did not understand.
 */
export function unresolvedTagsFrom(row: Row | null): string[] {
  if (row === null) return [];
  const flat = flatten(row);
  for (const key of ["unresolved", "unresolvedTags"]) {
    const v = flat[key];
    if (Array.isArray(v)) return v.filter((m): m is string => typeof m === "string");
  }
  return [];
}

// ---------------------------------------------------------------------------
// Audiences and recipients
// ---------------------------------------------------------------------------

export interface CreateAudienceState extends WriteState {
  create: (facts: { name: string; description: string; accountId: string }) => Promise<string>;
}

export function useCreateAudience(): CreateAudienceState {
  const { busy, error, reset, call } = useWrite(
    async (query, facts: { name: string; description: string; accountId: string }) => {
      const audienceId = newShortId();
      await query.createAudience({
        audienceId,
        name: facts.name.trim(),
        description: omitBlank(facts.description),
        accountId: omitBlank(facts.accountId),
      });
      return audienceId;
    },
    "",
  );
  return { busy, error, reset, create: call };
}

export interface ArchiveAudienceState extends WriteState {
  archive: (audienceId: string) => Promise<boolean>;
}

export function useArchiveAudience(): ArchiveAudienceState {
  const { busy, error, reset, call } = useWrite(async (query, audienceId: string) => {
    await query.archiveAudience({ audienceId });
    return true;
  }, false);
  return { busy, error, reset, archive: call };
}

export interface AddRecipientState extends WriteState {
  add: (audienceId: string, email: string, displayName: string) => Promise<boolean>;
}

export function useAddRecipient(): AddRecipientState {
  const { busy, error, reset, call } = useWrite(
    async (query, audienceId: string, email: string, displayName: string) => {
      await query.addRecipient({
        recipientId: newShortId(),
        audienceId,
        email: email.trim(),
        displayName: omitBlank(displayName),
        source: "manual",
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, add: call };
}

export interface SetSubscriptionState extends WriteState {
  set: (recipientId: string, subscriptionStatus: string) => Promise<boolean>;
}

/**
 * Honour an unsubscribe by hand.
 *
 * `unsubscribedAt` is stamped HERE, with this browser's clock, only when the
 * state being written is a leaving one. The mutation threads it from the
 * caller rather than stamping `now` because the same write records a change
 * that happened earlier -- support forwarding an unsubscribe, a bounce report
 * read hours late -- and an operator doing it in this panel is doing it now.
 */
export function useSetSubscription(): SetSubscriptionState {
  const { busy, error, reset, call } = useWrite(
    async (query, recipientId: string, subscriptionStatus: string) => {
      await query.setRecipientSubscription({
        recipientId,
        subscriptionStatus,
        unsubscribedAt:
          subscriptionStatus === "subscribed" ? undefined : new Date().toISOString(),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, set: call };
}

/** What an import made of a file. Every figure the builtin reports. */
export interface ImportReport {
  added: number;
  duplicates: number;
  invalid: number;
  total: number;
  /** Up to twenty bad lines with their line numbers, verbatim. */
  samples: { line: number; text: string; reason: string }[];
}

export interface ImportState extends WriteState {
  /** The last report, kept until the person dismisses it. */
  report: ImportReport | null;
  dismiss: () => void;
  run: (audienceId: string, artifactId: string) => Promise<boolean>;
}

function numberAt(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) return Number(v);
  return 0;
}

/**
 * Read the import's report out of the builtin's reply.
 *
 * The sample rows are read defensively for the same reason
 * `unresolvedTagsFrom` is: the shape is documented in prose ("up to 20 sample
 * invalid lines with their line numbers") rather than in a schema this client
 * shares. A sample that carries no recognisable line number keeps its text and
 * shows no number, which is more useful than dropping the evidence.
 */
export function importReportFrom(row: Row | null): ImportReport {
  if (row === null) return { added: 0, duplicates: 0, invalid: 0, total: 0, samples: [] };
  const flat = flatten(row);
  const rawSamples = flat["samples"] ?? flat["invalidSamples"] ?? flat["sampleInvalid"];
  const samples = Array.isArray(rawSamples)
    ? rawSamples
        .filter((s): s is Row => !!s && typeof s === "object" && !Array.isArray(s))
        .map((s) => ({
          line: numberAt(s, "line") || numberAt(s, "lineNumber"),
          text: typeof s["text"] === "string" ? s["text"] : typeof s["line"] === "string" ? s["line"] : "",
          reason: typeof s["reason"] === "string" ? s["reason"] : "",
        }))
    : [];
  return {
    added: numberAt(flat, "added"),
    duplicates: numberAt(flat, "duplicates"),
    invalid: numberAt(flat, "invalid"),
    total: numberAt(flat, "total"),
    samples,
  };
}

/**
 * Import a CSV that is already in the Library.
 *
 * TWO STEPS, ONE OF WHICH IS NOT OURS. The file goes up through the shell's
 * one upload path (`items/edgeUpload.ts` -- `test/files/onePath.test.ts` fails
 * the build on a second speaker of that wire), and this call hands the
 * resulting artifact id to the engine, which reads the file SERVER-SIDE under
 * the caller's own actor. A file the caller cannot read is a file this cannot
 * import: the artifact id is not a capability.
 *
 * THE REPORT PERSISTS. It is held here rather than shown and forgotten,
 * because the operator's next action is fixing the file -- and evidence that
 * disappears on a timer is evidence they have to re-run an import to see
 * again.
 */
export function useImportRecipients(): ImportState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [report, setReport] = useState<ImportReport | null>(null);

  const run = useCallback(
    async (audienceId: string, artifactId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was imported.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        const result = await query.campaignImportRecipients({
          audienceId,
          artifactId,
          hasHeader: true,
        });
        setReport(importReportFrom(result.rows()[0] ?? null));
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    report,
    run,
    dismiss: () => setReport(null),
    reset: () => setError(""),
  };
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

export interface TemplateFacts {
  name: string;
  subject: string;
  textBody: string;
  htmlBody: string;
  status: string;
  accountId: string;
}

export interface CreateTemplateState extends WriteState {
  create: (facts: TemplateFacts) => Promise<string>;
}

export function useCreateTemplate(): CreateTemplateState {
  const { busy, error, reset, call } = useWrite(async (query, facts: TemplateFacts) => {
    const templateId = newShortId();
    await query.createTemplate({
      templateId,
      name: facts.name.trim(),
      subject: facts.subject.trim(),
      textBody: facts.textBody,
      htmlBody: omitBlank(facts.htmlBody),
      accountId: omitBlank(facts.accountId),
    });
    return templateId;
  }, "");
  return { busy, error, reset, create: call };
}

export interface UpdateTemplateState extends WriteState {
  update: (templateId: string, facts: TemplateFacts) => Promise<boolean>;
}

export function useUpdateTemplate(): UpdateTemplateState {
  const { busy, error, reset, call } = useWrite(
    async (query, templateId: string, facts: TemplateFacts) => {
      await query.updateTemplate({
        templateId,
        name: facts.name.trim(),
        subject: facts.subject.trim(),
        // THE BODIES ARE SENT AS TYPED, blank included. `textBody` is required
        // on the mutation and a body somebody deliberately emptied is a value,
        // not an omission -- this is the one place `omitBlank` would be wrong.
        textBody: facts.textBody,
        htmlBody: omitBlank(facts.htmlBody),
        status: omitBlank(facts.status),
        accountId: omitBlank(facts.accountId),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

// ---------------------------------------------------------------------------
// Sender identities
// ---------------------------------------------------------------------------

export interface SenderFacts {
  address: string;
  fromName: string;
  replyTo: string;
  accountId: string;
  notes: string;
}

export interface CreateSenderState extends WriteState {
  create: (facts: SenderFacts) => Promise<string>;
}

export function useCreateSender(): CreateSenderState {
  const { busy, error, reset, call } = useWrite(async (query, facts: SenderFacts) => {
    const senderIdentityId = newShortId();
    await query.createSenderIdentity({
      senderIdentityId,
      address: facts.address.trim(),
      fromName: facts.fromName.trim(),
      replyTo: omitBlank(facts.replyTo),
      accountId: omitBlank(facts.accountId),
      notes: omitBlank(facts.notes),
    });
    return senderIdentityId;
  }, "");
  return { busy, error, reset, create: call };
}

export interface UpdateSenderState extends WriteState {
  update: (senderIdentityId: string, facts: SenderFacts) => Promise<boolean>;
}

export function useUpdateSender(): UpdateSenderState {
  const { busy, error, reset, call } = useWrite(
    async (query, senderIdentityId: string, facts: SenderFacts) => {
      await query.updateSenderIdentity({
        senderIdentityId,
        address: facts.address.trim(),
        fromName: facts.fromName.trim(),
        replyTo: omitBlank(facts.replyTo),
        accountId: omitBlank(facts.accountId),
        notes: omitBlank(facts.notes),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

export interface SenderStatusState extends WriteState {
  set: (senderIdentityId: string, status: string) => Promise<boolean>;
}

/**
 * Retire or re-enable a mailbox.
 *
 * A STATUS FLIP, NEVER A DELETE, and there is deliberately no delete anywhere
 * in this app: past campaigns name the row, and the reputation and warmup
 * history are keyed on its address, so removing it would orphan the evidence a
 * deliverability review is made of. The UI says that rather than leaving
 * somebody hunting for a delete button.
 *
 * A separate call from `updateSenderIdentity`, which does not accept `status`:
 * enabling and disabling an identity is a deliverability decision with a
 * send-time consequence (the preflight REFUSES a campaign naming a disabled
 * identity rather than falling back to the default), and the two never overlap
 * in what they accept.
 */
export function useSenderStatus(): SenderStatusState {
  const { busy, error, reset, call } = useWrite(
    async (query, senderIdentityId: string, status: string) => {
      await query.setSenderIdentityStatus({ senderIdentityId, status });
      return true;
    },
    false,
  );
  return { busy, error, reset, set: call };
}

// ---------------------------------------------------------------------------
// Event-email rules
// ---------------------------------------------------------------------------

export interface RuleFacts {
  name: string;
  description: string;
  triggerConcept: string;
  eventKind: string;
  condition: string;
  templateId: string;
  recipientMode: RecipientMode;
  recipientRoles: string[];
  audienceId: string;
  recipientField: string;
  accountId: string;
  senderIdentityId: string;
}

/**
 * The mode-specific fields, sent only for the mode that uses them.
 *
 * A rule that was an audience rule and became a roles rule keeps its stale
 * `audienceId` otherwise -- the mutation read-merges -- and the generated
 * construct would name an audience nothing sends to. Blanking is not an option
 * either (`omitBlank` turns "" into an omission), so the honest write is to
 * send each field only when the mode makes it meaningful and to let the merge
 * keep the rest. The generator reads the mode first, so a stale sibling field
 * is inert; this keeps it from being CONFUSING as well as inert.
 */
function modeFields(facts: RuleFacts) {
  return {
    recipientRoles: facts.recipientMode === "cluster_roles" ? facts.recipientRoles : undefined,
    audienceId: facts.recipientMode === "audience" ? facts.audienceId : undefined,
    recipientField:
      facts.recipientMode === "row_address" ? omitBlank(facts.recipientField) : undefined,
  };
}

export interface CreateRuleState extends WriteState {
  create: (facts: RuleFacts) => Promise<string>;
}

export function useCreateRule(): CreateRuleState {
  const { busy, error, reset, call } = useWrite(async (query, facts: RuleFacts) => {
    const emailRuleId = newShortId();
    await query.createEmailRule({
      emailRuleId,
      name: facts.name.trim(),
      description: omitBlank(facts.description),
      triggerConcept: facts.triggerConcept,
      eventKind: facts.eventKind,
      condition: omitBlank(facts.condition),
      templateId: facts.templateId,
      recipientMode: facts.recipientMode,
      accountId: omitBlank(facts.accountId),
      senderIdentityId: omitBlank(facts.senderIdentityId),
      ...modeFields(facts),
    });
    // A rule lands as a DRAFT and nothing runs until somebody turns it on --
    // writing this row can never by itself mail anybody.
    return emailRuleId;
  }, "");
  return { busy, error, reset, create: call };
}

export interface UpdateRuleState extends WriteState {
  update: (emailRuleId: string, facts: RuleFacts) => Promise<boolean>;
}

export function useUpdateRule(): UpdateRuleState {
  const { busy, error, reset, call } = useWrite(
    async (query, emailRuleId: string, facts: RuleFacts) => {
      await query.updateEmailRule({
        emailRuleId,
        name: facts.name.trim(),
        description: omitBlank(facts.description),
        triggerConcept: facts.triggerConcept,
        eventKind: facts.eventKind,
        condition: omitBlank(facts.condition),
        templateId: facts.templateId,
        recipientMode: facts.recipientMode,
        accountId: omitBlank(facts.accountId),
        senderIdentityId: omitBlank(facts.senderIdentityId),
        ...modeFields(facts),
      });
      return true;
    },
    false,
  );
  return { busy, error, reset, update: call };
}

export interface RuleArmingState extends WriteState {
  /** Generate the rule's construct and arm it. */
  activate: (emailRuleId: string) => Promise<boolean>;
  /** Retire the construct. The rule row survives with its history. */
  retire: (emailRuleId: string) => Promise<boolean>;
  /** Pause or resume without discarding the construct. */
  setStatus: (emailRuleId: string, status: string) => Promise<boolean>;
}

/**
 * Turning a rule on, off and back on.
 *
 * THREE VERBS, THREE MEANINGS, and the app keeps them distinct because the
 * concept does: pausing keeps the generated construct and disarms it,
 * retiring removes the construct and keeps the row, and the circuit breaker
 * tripping is a fourth thing that happens to a rule without anybody asking.
 * All four are visible as distinct statuses so nobody has to guess which one
 * happened -- and the refusal, when activation fails, is the engine's own
 * sentence on `lastError`, which the list renders verbatim.
 *
 * One error slot, for the reason `useSendControls` has one: these three are a
 * single cluster of controls on one row, and only one can be pressed at once.
 */
export function useRuleArming(): RuleArmingState {
  const activate = useWrite(async (query, emailRuleId: string) => {
    await query.campaignActivateEmailRule({ emailRuleId });
    return true;
  }, false);
  const retire = useWrite(async (query, emailRuleId: string) => {
    await query.campaignRetireEmailRule({ emailRuleId });
    return true;
  }, false);
  const status = useWrite(async (query, emailRuleId: string, next: string) => {
    await query.setEmailRuleStatus({ emailRuleId, status: next });
    return true;
  }, false);

  const parts = [activate, retire, status];
  return {
    busy: parts.some((p) => p.busy),
    error: parts.map((p) => p.error).find((e) => e !== "") ?? "",
    reset: () => parts.forEach((p) => p.reset()),
    activate: activate.call,
    retire: retire.call,
    setStatus: status.call,
  };
}

/**
 * Every write this app makes, assembled once at the app root.
 *
 * The sections take what they need. Assembling here rather than per section
 * means a write's busy/error state survives a section switch, which matters
 * for exactly one of them: an import running while somebody looks at their
 * campaigns is still running when they come back.
 */
export function useCampaignWrites() {
  const createCampaign = useCreateCampaign();
  const updateCampaign = useUpdateCampaign();
  const sendControls = useSendControls();
  const testSend = useTestSend();
  const createAudience = useCreateAudience();
  const archiveAudience = useArchiveAudience();
  const addRecipient = useAddRecipient();
  const setSubscription = useSetSubscription();
  const importRecipients = useImportRecipients();
  const createTemplate = useCreateTemplate();
  const updateTemplate = useUpdateTemplate();
  const createSender = useCreateSender();
  const updateSender = useUpdateSender();
  const senderStatus = useSenderStatus();
  const createRule = useCreateRule();
  const updateRule = useUpdateRule();
  const ruleArming = useRuleArming();

  // NOT memoised. Every hook above returns a fresh `reset` closure on every
  // render, so the object's identity changes regardless; a useMemo over
  // seventeen deps that can never all be equal is a dependency list somebody
  // has to maintain in exchange for nothing.
  return {
    createCampaign,
    updateCampaign,
    sendControls,
    testSend,
    createAudience,
    archiveAudience,
    addRecipient,
    setSubscription,
    importRecipients,
    createTemplate,
    updateTemplate,
    createSender,
    updateSender,
    senderStatus,
    createRule,
    updateRule,
    ruleArming,
  };
}

export type CampaignWrites = ReturnType<typeof useCampaignWrites>;
