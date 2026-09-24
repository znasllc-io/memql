import { listCount } from "../../kit/RecordRow";
import { AddButton } from "../../kit/AddButton";
import { Fragment, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Zap } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import {
  Button,
  EmptyState,
  Caption,
  ChoiceStack,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  RecordRow,
  Select,
  Subhead,
  formatMoment,
} from "../../kit";
import { useLiveView } from "../../live/liveView";
import type { CampaignWrites, RuleFacts } from "./actions";
import {
  audienceProjection,
  nameOfAudience,
  nameOfTemplate,
  senderProjection,
  templateProjection,
  useProjected,
} from "./CampaignsSection";
import {
  RECIPIENT_MODES,
  conceptEntity,
  conditionRowIs,
  emailRuleFromRow,
  recipientModeLabel,
  ruleFingerprint,
  ruleName,
  pauseReading,
  ruleSentence,
  ruleSentenceParts,
  type EmailRuleRow,
  type RecipientMode,
  type SentencePart,
} from "./rows";
import { useAuthoredAutomations, useTriggerConcepts, type CampaignFeeds } from "./useCampaigns";

// Rules: "when this happens, email that to those people."
//
// ===========================================================================
// THE BUILDER IS A SENTENCE, AND THE LANE IS NEVER A TOGGLE
// ===========================================================================
// A rule has six fields and every one of them is a clause in one English
// sentence: when a [thing] is [created or changed], email [template] to [who]
// -- optionally, but only when [...]. Laying that out as six labelled form
// fields would make somebody assemble the meaning themselves, every time, from
// parts that only mean anything together.
//
// THE `who` CONTROL IS THE ONLY PLACE THE RECIPIENT MODE IS CHOSEN, and the
// two delivery lanes are a CONSEQUENCE of it rather than a control. Choosing
// "people in this cluster" sends internal mail with no unsubscribe footer and
// no do-not-mail check; choosing an audience or an address on the row sends it
// the way a campaign does. That is a real difference somebody must understand
// -- so each choice says what it MEANS, in one plain line, as an effect. It
// never says "operational lane" or "marketing lane": those are our words for
// our machinery, and a person choosing who gets an email is not choosing a
// lane.
//
// The rules list is LIVE (`v1:campaigns:emailRule` broadcasts both verbs), and
// `lastFiredAt` / `firedCount` are deliberately absent from the fingerprint --
// the concept itself says so: "A LIVENESS field: it moves on its own... Display
// it; do not ring on it."

export function RulesSection({
  feeds,
  writes,
  showFiled,
}: {
  feeds: CampaignFeeds;
  writes: CampaignWrites;
  showFiled: boolean;
}) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);

  const source = useLiveView<Row, EmailRuleRow>(
    feeds.rules.source,
    `filed:${showFiled}`,
    (rows) => {
      const rules = rows.map(emailRuleFromRow).filter((r) => r.id !== "");
      // "Filed" for a rule means paused: a paused rule is one somebody turned
      // off, which is a different thing from a draft (never armed) and a
      // different thing again from failed (the engine refused it). Drafts and
      // failures stay visible because both are waiting on somebody.
      return showFiled ? rules : rules.filter((r) => r.status !== "paused");
    },
  );

  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const senders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  const concepts = useTriggerConcepts();
  const authored = useAuthoredAutomations();

  const open = useMemo(
    () => source?.snapshot.rows.find((r) => r.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  if (adding)
    return (
      <div className="os-app-stack">
        <Head title="New rule" back={{ label: "Rules", onSelect: () => setAdding(false) }} />
        <RuleBuilder
          concepts={concepts}
          templates={templates}
          audiences={audiences}
          senders={senders}
          writes={writes}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      </div>
    );
  if (open)
    return (
      <div className="os-app-stack">
        <Head title={ruleName(open)} back={{ label: "Rules", onSelect: () => setOpenId("") }} />
        <AuthoredAutomationsBanner authored={authored} />
        <RuleDetail
          key={open.id}
          rule={open}
          concepts={concepts}
          templates={templates}
          audiences={audiences}
          senders={senders}
          writes={writes}
        />
      </div>
    );

  return (
    <div className="os-app-stack">
      <Head title="Rules" meta={listCount(source?.snapshot)}>
        <AddButton onClick={() => setAdding((v) => !v)} label="New rule" />
      </Head>

      <AuthoredAutomationsBanner authored={authored} />

      {feeds.rules.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your rules."
          next="Nothing below is current."
        >
          <Button onClick={feeds.rules.reseed}>Try again</Button>
        </Notice>
      ) : null}

      <LiveList<EmailRuleRow>
        key={`rules:${showFiled}`}
        source={source}
        rowId={(r) => r.id}
        fingerprint={ruleFingerprint}
        label="Your event-email rules"
        emptyText="No rules yet. A rule sends one email whenever something happens in the cluster -- a new sign-up, a status change, an order."
        emptyContent={
          <EmptyState icon={Zap} title="No email rules yet">
            Create a rule to email people when something happens in this cluster.
          </EmptyState>
        }
        renderRow={(rule, tick) => (
          <RuleLine
            rule={rule}
            templates={templates}
            audiences={audiences}
            tick={tick}
            open={openId === rule.id}
            onToggle={() => setOpenId((held) => (held === rule.id ? "" : rule.id))}
          />
        )}
      />
    </div>
  );
}

/**
 * The cluster-wide kill switch, over the list.
 *
 * ===========================================================================
 * A BANNER OVER THE LIST, NEVER A STATUS ON EACH ROW
 * ===========================================================================
 * This is not per-rule state. With the switch off, every rule is
 * simultaneously "active and inert" -- the rows are untouched, the scheduler's
 * global gate refuses each firing before it reaches owner-gating or the
 * breaker, and nothing about any individual rule is different from how it was
 * yesterday. Painting "halted" onto each row would claim something about each
 * rule that is not true of any of them; the fact belongs to the cluster, so it
 * is stated once, above the thing it applies to.
 *
 * ONLY AN EXPLICIT `false` SHOWS IT. A missing row, a shape that does not
 * carry the field, and a read that failed are all "unknown", and rendering
 * "your rules are halted" from any of them would be inventing a cluster fact
 * out of a missing one -- a scare on a cluster where nothing is wrong. That
 * asymmetry is the whole design of `authoredAutomationsFrom`, and
 * test/campaigns/app.test.tsx pins all three silences.
 *
 * A REFUSED READ SAYS SO QUIETLY rather than claiming the switch is on. It is
 * a caption and not a warning, because "we could not check" is a fact about
 * this window, not about the cluster.
 */
function AuthoredAutomationsBanner({
  authored,
}: {
  authored: ReturnType<typeof useAuthoredAutomations>;
}) {
  if (authored.value === "halted") {
    return (
      <Notice
        tone="warn"
        sentence="Rules are switched off across this whole cluster, so none of them are sending -- including the ones below that say active."
        next="A cluster-wide switch stops every automated email on every node, whatever an individual rule says. An owner can turn it back on in Settings, under Cluster."
      >
        <Caption>
          Read at {new Date(authored.readAt || Date.now()).toLocaleTimeString()}. This is not live
          -- reopen this section to check it again.
        </Caption>
      </Notice>
    );
  }
  if (authored.state === "error") {
    // QUIET, and it does not say the switch is on. "We could not check" is a
    // fact about this window; claiming the cluster is fine would be a fact
    // about the cluster that nothing here established.
    return (
      <Caption>
        This cluster did not say whether rules are switched on globally, so nothing below can
        promise it is sending.
      </Caption>
    );
  }
  return null;
}

/** Only two statuses carry colour: one that is running and one that broke.
 *  Four coloured chips is a list with no emphasis at all. */


function RuleLine({
  rule,
  templates,
  audiences,
  tick,
  open,
  onToggle,
}: {
  rule: EmailRuleRow;
  templates: ReturnType<typeof templateProjection>;
  audiences: ReturnType<typeof audienceProjection>;
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const names = {
    template: nameOfTemplate(templates, rule.templateId),
    audience: nameOfAudience(audiences, rule.audienceId),
  };
  return (
    <RecordRow
      icon={<Zap size={16} aria-hidden />}
      name={ruleName(rule)}
      current={rule.status === "active"}
      dim={rule.status === "paused"}
      open={open}
      onOpen={onToggle}
      secondary={<span title={ruleSentence(rule, names)}><SentenceText parts={ruleSentenceParts(rule, names)} /></span>}
      state={rule.status || "draft"}
      tone={rule.status === "active" ? "accent" : "muted"}
      stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
    >
      {/* THE LIST READS THE WAY THE BUILDER DOES. Somebody who built a rule by
          filling in a sentence should recognise it here without translating. */}
      {/* LIVENESS IS DISPLAYED, NEVER RUNG. */}
      {rule.firedCount === 0 ? null : (
        <span className="os-caption os-mono">{rule.firedCount}x</span>
      )}
    </RecordRow>
  );
}

/** A rule's sentence, with what the person typed set in the code face -- the
 *  way a raw value sits inside prose everywhere else in the OS. */
function SentenceText({ parts }: { parts: SentencePart[] }) {
  return (
    <>
      {parts.map((part, i) =>
        part.code ? (
          <code key={i} className="os-mono">
            {part.text}
          </code>
        ) : (
          <Fragment key={i}>{part.text}</Fragment>
        ),
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// One rule
// ---------------------------------------------------------------------------

function RuleDetail({
  rule,
  concepts,
  templates,
  audiences,
  senders,
  writes,
}: {
  rule: EmailRuleRow;
  concepts: ReturnType<typeof useTriggerConcepts>;
  templates: ReturnType<typeof templateProjection>;
  audiences: ReturnType<typeof audienceProjection>;
  senders: ReturnType<typeof senderProjection>;
  writes: CampaignWrites;
}) {
  const accounts = useAccountOptions();
  const [editing, setEditing] = useState(false);
  const arming = writes.ruleArming;

  return (
    <div className="os-campaign-detail">
      <Panel label={`${ruleName(rule)} details`}>
        <div className="os-campaign-detail-head">
          <Subhead>{ruleName(rule)}</Subhead>
          <AccountChip name={accountNameFrom(accounts, rule.accountId)} />
        </div>

        <p className="os-campaign-rule-headline">
          <SentenceText
            parts={ruleSentenceParts(rule, {
              template: nameOfTemplate(templates, rule.templateId),
              audience: nameOfAudience(audiences, rule.audienceId),
            })}
          />
        </p>

        {rule.description === "" ? null : <Caption>{rule.description}</Caption>}

        {/* THE ENGINE'S OWN SENTENCE, VERBATIM. A rule whose bundle failed
            validation, or whose circuit breaker tripped, says exactly what the
            engine said -- never a paraphrase and never a generic apology. The
            engine's message names the construct and the line; ours could not. */}
        {rule.lastError === "" ? null : (
          <Notice
            tone={rule.status === "failed" ? "error" : "warn"}
            sentence={
              rule.status === "failed"
                ? "This rule is not running. The cluster refused it."
                : "The cluster reported a problem with this rule."
            }
            next={
              rule.status === "failed"
                ? "Fix what it names below and turn the rule on again."
                : "It may have stopped itself. What it said is below."
            }
            detail={rule.lastError}
          />
        )}

        <Facts>
          <Fact
            label="Fires on"
            value={
              rule.triggerConcept === ""
                ? ""
                : `${conceptEntity(rule.triggerConcept)} ${rule.eventKind === "updated" ? "changed" : "created"}`
            }
          />
          <Fact label="Concept" value={rule.triggerConcept} mono />
          <Fact label="Only when" value={rule.condition} mono />
          <Fact label="Sends" value={nameOfTemplate(templates, rule.templateId)} />
          <Fact label="To" value={recipientModeLabel(rule.recipientMode)} />
          <Fact
            label="Sends as"
            value={senders.find((s) => s.id === rule.senderIdentityId)?.address ?? ""}
            mono
          />
          {/* "WHICH AUTOMATION IS THIS RULE" is the first question anybody
              debugging one asks, and the answer living only in a log line is
              how it stays unanswered. */}
          <Fact label="Runs as" value={rule.constructName} mono />
          <Fact label="Bundle" value={rule.bundleId} mono />
          <Fact label="Times fired" value={rule.firedCount} />
          <Fact
            label="Last fired"
            value={rule.lastFiredAt === "" ? "" : formatMoment(rule.lastFiredAt)}
          />
        </Facts>
      </Panel>

      <ArmingPanel rule={rule} arming={arming} />

      <Panel label="Edit this rule">
        <div className="os-campaign-detail-head">
          <Subhead>The rule</Subhead>
          <Button onClick={() => setEditing((v) => !v)}>{editing ? "Cancel" : "Edit"}</Button>
        </div>
        {editing ? (
          <RuleBuilder
            rule={rule}
            concepts={concepts}
            templates={templates}
            audiences={audiences}
            senders={senders}
            writes={writes}
            onDone={() => setEditing(false)}
          />
        ) : (
          <Caption>
            Editing a rule that is running rewrites what it does. Turn it on again afterwards -- the
            old version is retired first, so a rule is never armed twice.
          </Caption>
        )}
      </Panel>
    </div>
  );
}

/**
 * Turn a rule on, pause it, or retire it.
 *
 * THREE VERBS, THREE MEANINGS, kept distinct because the concept keeps them
 * distinct: pausing keeps the generated automation and disarms it, retiring
 * removes the automation and keeps the rule's history, and the circuit breaker
 * tripping is a fourth thing that happens without anybody asking. All four are
 * visible as separate statuses, so nobody has to guess which one happened.
 *
 * A REFUSAL IS SAID ONCE. When activation fails the engine records why on the
 * rule (`lastError`), and the rule's own notice above renders that sentence
 * -- so this panel repeating the call's error put one refusal on the page
 * twice, the second time wrapped in "MemQL engine failed to execute query.
 * Details:". Here a refusal appears only when the rule does not carry it: the
 * engine refused before recording anything (who may author, which rule), or
 * its record has not reached this window yet -- and the moment it does, this
 * copy steps aside. A refused activation also closes the question: the line
 * below then says the cluster refused it and where the reason is.
 */
function ArmingPanel({
  rule,
  arming,
}: {
  rule: EmailRuleRow;
  arming: CampaignWrites["ruleArming"];
}) {
  const [asking, setAsking] = useState(false);
  const active = rule.status === "active";
  const paused = rule.status === "paused";
  const refusal = arming.error;
  const refusalIsOnTheRule = refusal !== "" && refusal === rule.lastError.trim();

  return (
    <Panel label="Turn this rule on or off">
      <Subhead>Running</Subhead>

      {asking ? (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">Turn on {ruleName(rule)}?</p>
          <Caption>
            From then on, this sends mail on its own every time the event happens -- to real people,
            with no further confirmation. Send yourself a test from the template first if you have
            not.
          </Caption>
          <div className="os-campaign-actions">
            <Button
              tone="primary"
              busy={arming.busy}
              busyLabel="Turning on"
              onClick={async () => {
                await arming.activate(rule.id);
                // Asked and answered either way: armed, or refused for a
                // reason this page now shows once.
                setAsking(false);
              }}
            >
              Turn it on
            </Button>
            <Button
              onClick={() => {
                setAsking(false);
                arming.reset();
              }}
            >
              Not yet
            </Button>
          </div>
        </div>
      ) : (
        <>
          <Caption>{runningLine(rule)}</Caption>
          <div className="os-campaign-actions">
            {active ? (
              <Button
                busy={arming.busy}
                busyLabel="Pausing"
                onClick={() => arming.setStatus(rule.id, "paused")}
              >
                Pause
              </Button>
            ) : (
              <Button
                tone="primary"
                onClick={() => {
                  // A new question starts clean: an earlier act's refusal is
                  // not an answer to this one.
                  arming.reset();
                  setAsking(true);
                }}
              >
                {paused ? "Start it again" : "Turn it on"}
              </Button>
            )}
            {rule.constructName === "" ? null : (
              <Button
                tone="danger"
                busy={arming.busy}
                busyLabel="Retiring"
                onClick={() => arming.retire(rule.id)}
              >
                Retire
              </Button>
            )}
          </div>
          {rule.constructName === "" ? null : (
            <Caption>
              Retiring stops the rule and removes what it generated. The rule itself stays here with
              its history -- a rule somebody turned off is a different thing from one that never
              existed.
            </Caption>
          )}
          {refusal === "" || refusalIsOnTheRule ? null : (
            <Notice
              tone="error"
              sentence="The cluster refused that."
              next="Nothing changed."
              detail={refusal}
            />
          )}
        </>
      )}
    </Panel>
  );
}

/**
 * What is true of this rule right now.
 *
 * THE TWO WAYS A RULE COMES TO BE PAUSED ARE DIFFERENT SITUATIONS, and only
 * one of them is somebody's decision. `setEmailRuleStatus` is the operator's
 * stop button; a rule can also stop after its runs kept failing, and the
 * failure that did it is on `lastError`. "You paused this" over the second
 * case throws away the only diagnostic there is -- and it is wrong about who
 * did it.
 *
 * The line reports the EVIDENCE and does not name a mechanism this window
 * cannot observe: paused, and the last run failed. What failed is rendered
 * verbatim above, which is where somebody acts on it.
 */
function runningLine(rule: EmailRuleRow): string {
  switch (rule.status) {
    case "active":
      return rule.lastError === ""
        ? "This rule is running. It sends mail on its own whenever the event happens."
        : "Running, but its last run failed -- and enough failures in a row stop a rule on their own. What went wrong is above.";
    case "paused":
      return pauseReading(rule) === "operator"
        ? "Paused. Nothing is sent, and what it generated is still there waiting."
        : "Paused, and its last run failed. What went wrong is above -- start it again once that is dealt with.";
    case "failed":
      return "Not running -- the cluster refused it. The reason is above.";
    default:
      return "A draft. Nothing runs, and nothing has been generated yet.";
  }
}

// ---------------------------------------------------------------------------
// The sentence builder
// ---------------------------------------------------------------------------

const EVENT_KINDS = [
  { value: "created", label: "is created" },
  { value: "updated", label: "changes" },
];

// The condition field's two examples, and they differ on purpose. The
// placeholder is the smallest condition there is; the caption's shows the two
// things nobody can guess -- how a list is written and how two tests join.
const CONDITION_PLACEHOLDER = 'row.role == "admin"';
const CONDITION_EXAMPLE = 'row.status in ["active", "trial"] && row.plan != "free"';

/**
 * Build a rule by finishing a sentence.
 *
 * THE TRIGGER CONCEPTS COME FROM THE LIVE REGISTRY, never a fixed list. A rule
 * can name a concept a product bundle added after this release, and a
 * hardcoded list would make the newest half of a cluster's schema untriggerable
 * with no way to tell. The read is `listConcepts` -- the SDK's own accessor for
 * `ConceptsListMsg`, riding the same stream as every other read here (see
 * useCampaigns.ts; the OS had no accessor for it before this surface).
 */
function RuleBuilder({
  rule,
  concepts,
  templates,
  audiences,
  senders,
  writes,
  onDone,
}: {
  rule?: EmailRuleRow;
  concepts: ReturnType<typeof useTriggerConcepts>;
  templates: ReturnType<typeof templateProjection>;
  audiences: ReturnType<typeof audienceProjection>;
  senders: ReturnType<typeof senderProjection>;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  const editing = rule !== undefined;
  const write = editing ? writes.updateRule : writes.createRule;
  const [draft, setDraft] = useState<RuleFacts>(() => ({
    name: rule?.name ?? "",
    description: rule?.description ?? "",
    triggerConcept: rule?.triggerConcept ?? "",
    eventKind: rule?.eventKind ?? "created",
    condition: rule?.condition ?? "",
    templateId: rule?.templateId ?? "",
    recipientMode: (rule?.recipientMode as RecipientMode) || "cluster_roles",
    recipientRoles: rule?.recipientRoles ?? [],
    audienceId: rule?.audienceId ?? "",
    recipientField: rule?.recipientField ?? "",
    accountId: rule?.accountId ?? "",
    senderIdentityId: rule?.senderIdentityId ?? "",
  }));

  const sorted = useMemo(
    () => [...concepts.value].sort((a, b) => a.id.localeCompare(b.id)),
    [concepts.value],
  );

  const accountId = draft.accountId || (rule === undefined ? defaultAccountId : "");

  const ready = organizationChosen(accounts, accountId) &&
    draft.name.trim() !== "" &&
    draft.triggerConcept !== "" &&
    templates.some((t) => t.id === draft.templateId && t.accountId === accountId) &&
    ((accountId === "self" && draft.senderIdentityId === "") || senders.some((s) => s.id === draft.senderIdentityId && s.accountId === accountId)) &&
    (draft.recipientMode !== "audience" || audiences.some((a) => a.id === draft.audienceId && a.accountId === accountId)) &&
    (draft.recipientMode !== "row_address" || draft.recipientField.trim() !== "");

  async function submit() {
    if (!ready) return;
    if (editing && rule) {
      const ok = await writes.updateRule.update(rule.id, { ...draft, accountId });
      if (ok) onDone(rule.id);
      return;
    }
    const id = await writes.createRule.create({ ...draft, accountId });
    if (id !== "") onDone(id);
  }

  return (
    <Panel label={editing ? "Edit rule" : "New rule"}>
      <Field label="Organization">
            <AccountPicker
              id="os-rule-account"
              label="Organization this rule is for"
              required
            value={accountId}
              onChange={(v) => setDraft({ ...draft, accountId: v, audienceId: "", templateId: "", senderIdentityId: "" })}
              accounts={accounts}
            />
          </Field>
      <Field label="Call this rule">
        <Input
          id="os-rule-name"
          label="Rule name"
          value={draft.name}
          onChange={(v) => setDraft({ ...draft, name: v })}
          placeholder="Tell the owner about new admins"
        />
      </Field>

      {/* THE SENTENCE. Inline controls in reading order, so the meaning is
          assembled by reading rather than by the reader. */}
      <div className="os-campaign-sentence">
        <span className="os-campaign-sentence-word">When a</span>
        <Select
          id="os-rule-concept"
          label="The kind of thing that fires this rule"
          value={draft.triggerConcept}
          onChange={(v) => setDraft({ ...draft, triggerConcept: v })}
        >
          <option value="">choose something</option>
          {sorted.map((concept) => (
            <option key={concept.id} value={concept.id}>
              {conceptEntity(concept.id)} ({concept.domain})
            </option>
          ))}
        </Select>
        <Select
          id="os-rule-eventkind"
          label="Which event fires this rule"
          value={draft.eventKind}
          onChange={(v) => setDraft({ ...draft, eventKind: v })}
        >
          {EVENT_KINDS.map((kind) => (
            <option key={kind.value} value={kind.value}>
              {kind.label}
            </option>
          ))}
        </Select>
        <span className="os-campaign-sentence-word">, email</span>
        <Select
          id="os-rule-template"
          label="Template to send"
          value={draft.templateId}
          onChange={(v) => setDraft({ ...draft, templateId: v })}
        >
          <option value="">choose a template</option>
          {templates
            .filter((t) => t.accountId === accountId && (t.status !== "archived" || t.id === draft.templateId))
            .map((t) => (
              <option key={t.id} value={t.id}>
                {t.name || t.id}
              </option>
            ))}
        </Select>
        <span className="os-campaign-sentence-word">to...</span>
      </div>

      {concepts.state === "error" ? (
        <Notice
          tone="warn"
          sentence="This cluster did not list what it can be told about."
          next="Without that list there is nothing to pick, so the rule cannot say when it should fire."
          detail={concepts.error}
        >
          <Button onClick={concepts.reload}>Try again</Button>
        </Notice>
      ) : null}

      {/* WHO RECEIVES -- the one place the mode is chosen, and the only place
          the delivery difference is explained. Never a lane toggle. */}
      <div className="os-campaign-who">
        <ChoiceStack
          name="os-rule-who"
          label="Who receives this email"
          value={draft.recipientMode}
          onChange={(v) => setDraft({ ...draft, recipientMode: v as RecipientMode })}
          options={RECIPIENT_MODES.map((mode) => ({
            value: mode.value,
            label: mode.label,
            description: mode.effect,
          }))}
        />

        {draft.recipientMode === "cluster_roles" ? (
          <RolePicker
            selected={draft.recipientRoles}
            onChange={(next) => setDraft({ ...draft, recipientRoles: next })}
          />
        ) : null}

        {draft.recipientMode === "audience" ? (
          <Field label="Which audience">
            <Select
              id="os-rule-audience"
              label="Audience to mail"
              value={draft.audienceId}
              onChange={(v) => setDraft({ ...draft, audienceId: v })}
            >
              <option value="">Choose an audience</option>
              {audiences
                .filter((a) => a.accountId === accountId && (a.status !== "archived" || a.id === draft.audienceId))
                .map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.name || a.id}
                  </option>
                ))}
            </Select>
          </Field>
        ) : null}

        {draft.recipientMode === "row_address" ? (
          <>
            <Field label="Which field holds the address">
              <Input
                id="os-rule-field"
                label="Field on the triggering row holding the address"
                value={draft.recipientField}
                onChange={(v) => setDraft({ ...draft, recipientField: v })}
                placeholder="primaryContactEmail"
              />
            </Field>
            <Caption>
              The name of a field on the thing that fired -- use a dot for a nested one
              (contact.email). It is checked against that concept when the rule is turned on, so a
              name that resolves to nothing is caught then rather than by a rule that fires forever
              and mails nobody.
            </Caption>
          </>
        ) : null}
      </div>

      <details className="os-campaign-more">
        <summary>Only sometimes, and other details</summary>
        {/* THE ONE PLACE IN THE OS WHERE A PERSON WRITES AN EXPRESSION, so the
            field carries its whole reference directly beneath it: what `row`
            is, one example, the operators, and when a mistake is caught. It
            stands on its own full-width line rather than in the grid below,
            because a real condition is longer than half a form, and help that
            sits three fields away is help nobody connects to the field. */}
        <div className="os-campaign-condition">
          <Field label="Only when">
            <Input
              id="os-rule-condition"
              label="Condition that must hold for the rule to fire"
              value={draft.condition}
              onChange={(v) => setDraft({ ...draft, condition: v })}
              placeholder={CONDITION_PLACEHOLDER}
              code
            />
          </Field>
          <Caption>
            Leave the condition empty and the rule fires every time.{" "}
            <code className="os-mono">row</code> is {conditionRowIs(draft)}. For example,{" "}
            <code className="os-mono">{CONDITION_EXAMPLE}</code> means the status is active or trial
            and the plan is not free. Use <code className="os-mono">{"=="}</code> (is),{" "}
            <code className="os-mono">{"!="}</code> (is not), <code className="os-mono">in</code>{" "}
            (is one of), <code className="os-mono">{"&&"}</code> (and),{" "}
            <code className="os-mono">{"||"}</code> (or) and <code className="os-mono">{"!"}</code>{" "}
            (not). A condition with a mistake in it is refused when you turn the rule on, not later
            when it fires.
          </Caption>
        </div>
        <div className="os-campaign-form">
          <Field label="Sends as">
            <Select
              id="os-rule-sender"
              label="Sending mailbox"
              value={draft.senderIdentityId}
              onChange={(v) => setDraft({ ...draft, senderIdentityId: v })}
            >
              <option value="" disabled={accountId !== "self"}>{accountId === "self" ? "This cluster's default mailbox" : "Choose an organization mailbox"}</option>
              {senders
                .filter((s) => s.accountId === accountId && (s.status !== "disabled" || s.id === draft.senderIdentityId))
                .map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.address}
                  </option>
                ))}
            </Select>
          </Field>

          <Field label="What it is for">
            <Input
              id="os-rule-description"
              label="What this rule is for, in your own words"
              value={draft.description}
              onChange={(v) => setDraft({ ...draft, description: v })}
            />
          </Field>
        </div>
        {draft.recipientMode === "cluster_roles" ? (
          <Caption>
            The mailbox is not used for this rule: internal mail leaves through the cluster&apos;s
            own configured sender.
          </Caption>
        ) : null}
      </details>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence={editing ? "This did not save." : "This rule was not created."}
          next="Nothing was written; what is above is still as you typed it."
          detail={write.error}
        />
      )}

      <div className="os-campaign-actions">
        <Button
          tone="primary"
          busy={write.busy}
          busyLabel="Saving"
          onClick={submit}
          disabled={!ready}
        >
          {editing ? "Save" : "Create rule"}
        </Button>
        <Button
          onClick={() => {
            write.reset();
            onDone("");
          }}
        >
          Cancel
        </Button>
      </div>
      {/* A NEW RULE DOES NOTHING UNTIL SOMEBODY TURNS IT ON, and saying so is
          what makes creating one safe to try. */}
      <Caption>
        {ready
          ? "Saving writes the rule down. It sends nothing until you turn it on."
          : "A name, something to fire on, a template and someone to send it to are all needed."}
      </Caption>
    </Panel>
  );
}

/**
 * Which of this cluster's own people get the mail.
 *
 * EMPTY MEANS THE CLUSTER OWNER, which is the common "tell me when this
 * happens" case -- so no selection is a real answer rather than an unfinished
 * form, and the caption says which answer it is. Toggles rather than a
 * multi-select, for the reason the account label picker uses them: a native
 * `<select multiple>` loses every selection on a click without a modifier.
 */
const CLUSTER_ROLES = ["owner", "admin", "developer", "writer", "reader"];

function RolePicker({
  selected,
  onChange,
}: {
  selected: string[];
  onChange: (next: string[]) => void;
}) {
  return (
    <div className="os-campaign-roles">
      <span className="os-form-field-label" aria-hidden>
        Which people
      </span>
      <div className="os-account-labels" role="group" aria-label="Roles that receive this email">
        {CLUSTER_ROLES.map((role) => {
          const on = selected.includes(role);
          return (
            <button
              key={role}
              type="button"
              className="os-account-label"
              data-on={on || undefined}
              aria-pressed={on}
              onClick={() =>
                onChange(on ? selected.filter((r) => r !== role) : [...selected, role])
              }
            >
              {role}
            </button>
          );
        })}
      </div>
      <Caption>
        {selected.length === 0
          ? "Nothing picked, so this goes to the cluster owner alone."
          : `Everyone who is ${selected.join(" or ")} gets it.`}
      </Caption>
    </div>
  );
}
