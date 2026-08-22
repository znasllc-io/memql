import { useCallback, useEffect, useState } from "react";
import { newShortId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";

// Campaign authoring against the cluster (memql#3323).
//
// Campaigns, audiences and templates are ORDINARY CONCEPT ROWS -- v1:campaigns:*
// with declared @displayCards and per-row authz -- so nothing here adapts or
// reshapes them. The rows go to the elements as they arrive. What this module
// owns is the read/write choreography: three parallel reads the editor needs
// at once, one funnel every write goes through, and the re-read that follows a
// write.
//
// AUTHORIZATION IS NOT HERE, and its absence is deliberate rather than an
// oversight. Every campaigns query filters ownerUserId==actor.userId
// server-side and every concept declares @rowAuthz(owner="ownerUserId"), so
// the row set this receives is already the caller's own. A client-side owner
// filter would imply the isolation lives in the browser.

export interface CampaignsState {
  campaigns: Row[];
  audiences: Row[];
  templates: Row[];
  loading: boolean;
  error: string;
  // The outcome of the last write, either way. One message field rather than a
  // log: an operator wants the most recent thing that happened.
  actionMessage: string;
  actionError: string;
  busy: boolean;
  refresh: () => void;
  createAudience: (input: { name: string; description: string }) => void;
  createTemplate: (input: {
    name: string;
    subject: string;
    textBody: string;
    htmlBody: string;
  }) => void;
  saveCampaign: (input: CampaignInput) => void;
  // A builtin like the three send actions below, not a mutation (memql#3459):
  // committing to a time enqueues the engine's own send job, and the same
  // preflight refusals Start produces arrive here as actionError.
  scheduleCampaign: (input: { campaignId: string; scheduledAt: string }) => void;
  cancelCampaign: (campaignId: string) => void;
  // The three send actions (memql#3348). Builtins rather than mutations:
  // starting a send is a preflight across several rows plus two writes, and
  // the refusals it produces -- "template is not ready", "no email sender is
  // registered on this node", "one-click unsubscribe is not configured" --
  // arrive here as the rejected promise's message and land in actionError.
  // All four ride the generated typed methods (memql#4239), exactly as the
  // mutations do.
  startSend: (campaignId: string) => void;
  pauseSend: (campaignId: string) => void;
  resumeSend: (campaignId: string) => void;
}

export interface CampaignInput {
  // Empty for a new campaign. The id is minted client-side (newShortId) exactly
  // as every other MemQL write does it, so a create is idempotent under a retry
  // and the caller knows the id before the round trip.
  campaignId: string;
  name: string;
  audienceId: string;
  templateId: string;
  fromName: string;
  replyTo: string;
  scheduledAt: string;
}

export function useCampaigns(): CampaignsState {
  const { query } = useCluster();
  const [campaigns, setCampaigns] = useState<Row[]>([]);
  const [audiences, setAudiences] = useState<Row[]>([]);
  const [templates, setTemplates] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [actionMessage, setActionMessage] = useState("");
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  // Bumped by refresh() and by every completed write -- a create changes the
  // answer all three reads give.
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null) {
      setCampaigns([]);
      setAudiences([]);
      setTemplates([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    // One settle for all three: the editor needs the audience and template
    // pickers populated before it can render a campaign, so three staggered
    // arrivals would mean three renders with progressively fewer empty
    // dropdowns.
    // The generated typed methods (memql#4232): the constructs are the
    // same three named queries as ever, but the names and argument
    // shapes now come off the DSL via `make sdk-gen`, so a renamed
    // construct fails this file's typecheck instead of this hook's
    // runtime.
    void Promise.all([query.campaigns({}), query.audiences({}), query.templates({})])
      .then(([nextCampaigns, nextAudiences, nextTemplates]) => {
        if (!live) return;
        setCampaigns(nextCampaigns.rows());
        setAudiences(nextAudiences.rows());
        setTemplates(nextTemplates.rows());
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  // run funnels every write through one place so the busy flag, the message
  // handling and the follow-up re-read cannot be forgotten on the fifth one.
  // Promise<unknown>: a generated mutation or builtin resolves to a Result
  // this hook has no use for -- the success signal is the resolution itself,
  // and a builtin's refusal ("template is not ready", "no email sender is
  // registered on this node") arrives as the rejected promise's message.
  const run = useCallback((what: Promise<unknown> | null, done: string) => {
    if (what === null) return;
    setBusy(true);
    setActionMessage("");
    setActionError("");
    void what
      .then(() => setActionMessage(done))
      .catch((err: unknown) => setActionError(describe(err)))
      .finally(() => {
        setBusy(false);
        setEpoch((n) => n + 1);
      });
  }, []);

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);

  const createAudience = useCallback(
    (input: { name: string; description: string }) =>
      run(
        query
          ? query.createAudience({
              audienceId: newShortId(),
              name: input.name,
              description: omitBlank(input.description),
            })
          : null,
        `Audience "${input.name}" created.`,
      ),
    [query, run],
  );

  const createTemplate = useCallback(
    (input: { name: string; subject: string; textBody: string; htmlBody: string }) =>
      run(
        query
          ? query.createTemplate({
              templateId: newShortId(),
              name: input.name,
              subject: input.subject,
              textBody: input.textBody,
              htmlBody: omitBlank(input.htmlBody),
            })
          : null,
        `Template "${input.name}" created as a draft.`,
      ),
    [query, run],
  );

  const saveCampaign = useCallback(
    (input: CampaignInput) => {
      if (query === null) return;
      const creating = input.campaignId === "";
      const campaignId = creating ? newShortId() : input.campaignId;
      const args = {
        campaignId,
        name: input.name,
        audienceId: input.audienceId,
        templateId: input.templateId,
        fromName: omitBlank(input.fromName),
        replyTo: omitBlank(input.replyTo),
        scheduledAt: omitBlank(input.scheduledAt),
      };
      run(
        creating ? query.createCampaign(args) : query.updateCampaign(args),
        creating ? `Campaign "${input.name}" created as a draft.` : `Campaign "${input.name}" saved.`,
      );
    },
    [query, run],
  );

  const scheduleCampaign = useCallback(
    (input: { campaignId: string; scheduledAt: string }) =>
      run(
        // A BUILTIN, not the scheduleCampaign mutation (memql#3459). The
        // mutation writes the time onto the operator's own row and stops
        // there, which is exactly why a scheduled campaign used to sit
        // forever: nothing could find it. "Which campaigns are due" cannot be
        // asked of an owned-tier concept at all, so the schedule has to be
        // recorded on the engine's own send job -- and only Go holding a
        // campaign it read under the caller's actor may write that.
        query ? query.campaignScheduleSend(input) : null,
        // Now says what actually happens. The preflight has already passed at
        // this point -- template ready, sender configured, audience sane --
        // so the remaining uncertainty is the provider's, not ours.
        "Scheduled. The send starts on its own at that time; nothing else to press.",
      ),
    [query, run],
  );

  const cancelCampaign = useCallback(
    (campaignId: string) =>
      run(query ? query.cancelCampaign({ campaignId }) : null, "Campaign cancelled."),
    [query, run],
  );

  const startSend = useCallback(
    (campaignId: string) =>
      run(
        query ? query.campaignStartSend({ campaignId }) : null,
        // Deliberately not "Sent." The engine has ACCEPTED the send; the
        // messages leave over the following minutes, paced by the send rate
        // and by whatever the provider allows.
        "Sending started. Messages go out in batches -- watch the per-recipient outcomes below.",
      ),
    [query, run],
  );

  const pauseSend = useCallback(
    (campaignId: string) =>
      run(
        query ? query.campaignPauseSend({ campaignId }) : null,
        "Paused. The delivery ledger is untouched, so resuming continues where it stopped.",
      ),
    [query, run],
  );

  const resumeSend = useCallback(
    (campaignId: string) =>
      run(query ? query.campaignResumeSend({ campaignId }) : null, "Resumed."),
    [query, run],
  );

  return {
    campaigns,
    audiences,
    templates,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    refresh,
    createAudience,
    createTemplate,
    saveCampaign,
    scheduleCampaign,
    cancelCampaign,
    startSend,
    pauseSend,
    resumeSend,
  };
}

// useCampaignDetail is the editor's second read: the per-recipient outcomes
// for one campaign, and the subset of the selected audience a send would
// actually reach.
//
// The reach number is the one figure that changes an authoring decision, and
// it is deliberately the SENDABLE count rather than the audience size: the
// difference between the two is the suppression rate (unsubscribed, bounced,
// complained), and an operator about to commit a campaign to a time should be
// looking at the number that will be mailed, not the number on the list.
//
// Deliveries are empty until a send RUNS, which is a different statement from
// the one this comment used to make (memql#3348 supplied the engine). The
// editor still distinguishes the two in words rather than showing an empty
// table, because an empty table reads as "the send went out and reached
// nobody" -- and after a send has started, an empty ledger for a few seconds
// is the normal state rather than a failure.
export interface CampaignDetailState {
  deliveries: Row[];
  sendableCount: number;
  loading: boolean;
  error: string;
}

export function useCampaignDetail(campaignId: string, audienceId: string): CampaignDetailState {
  const { query } = useCluster();
  const [deliveries, setDeliveries] = useState<Row[]>([]);
  const [sendableCount, setSendableCount] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (query === null) {
      setDeliveries([]);
      setSendableCount(0);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void Promise.all([
      campaignId === ""
        ? Promise.resolve([])
        : query.deliveriesForCampaign({ campaignId }).then((r) => r.rows()),
      audienceId === ""
        ? Promise.resolve([])
        : query.sendableRecipientsForAudience({ audienceId }).then((r) => r.rows()),
    ])
      .then(([nextDeliveries, sendable]) => {
        if (!live) return;
        setDeliveries(nextDeliveries);
        setSendableCount(sendable.length);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, campaignId, audienceId]);

  return { deliveries, sendableCount, loading, error };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
