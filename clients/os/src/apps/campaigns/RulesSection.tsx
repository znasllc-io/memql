import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Plus, Zap } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import {
  Button,
  Caption,
  Check,
  Chip,
  ChoiceStack,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Row as ListRow,
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
  emailRuleFromRow,
  recipientModeLabel,
  ruleFingerprint,
  ruleName,
  ruleSentence,
  type EmailRuleRow,
  type RecipientMode,
} from "./rows";
import { useTriggerConcepts, type CampaignFeeds } from "./useCampaigns";

// Rules: "when this happens, email that to those people."
//
// ===========================================================================
// THE BUILDER IS A SENTENCE, AND THE LANE IS NEVER A TOGGLE
// ===========================================================================
// A rule has six fields and every one of them is a clause in one English
// sentence: when a [thing] is [created or changed] -- optionally [only when
// ...] -- email [template] to [who]. Laying that out as six labelled form
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
  onToggleFiled,
}: {
  feeds: CampaignFeeds;
  writes: CampaignWrites;
  showFiled: boolean;
  onToggleFiled: (next: boolean) => void;
}) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);

  const source = useLiveView<Row, EmailRuleRow>(feeds.rules.source, `filed:${showFiled}`, (rows) => {
    const rules = rows.map(emailRuleFromRow).filter((r) => r.id !== "");
    // "Filed" for a rule means paused: a paused rule is one somebody turned
    // off, which is a different thing from a draft (never armed) and a
    // different thing again from failed (the engine refused it). Drafts and
    // failures stay visible because both are waiting on somebody.
    return showFiled ? rules : rules.filter((r) => r.status !== "paused");
  });

  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const senders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  const concepts = useTriggerConcepts();

  const open = useMemo(
    () => source?.snapshot.rows.find((r) => r.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Rules">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> New rule
        </Button>
      </Head>

      {feeds.rules.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your rules."
          next="Nothing below is current."
        >
          <Button onClick={feeds.rules.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
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
      ) : null}

      <div className="os-campaign-filters">
        <Check checked={showFiled} onChange={onToggleFiled}>
          Show paused rules
        </Check>
      </div>

      <LiveList<EmailRuleRow>
        key={`rules:${showFiled}`}
        source={source}
        rowId={(r) => r.id}
        fingerprint={ruleFingerprint}
        label="Your event-email rules"
        emptyText="No rules yet. A rule sends one email whenever something happens in the cluster -- a new sign-up, a status change, an order."
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

      {open === null ? null : (
        <RuleDetail
          key={open.id}
          rule={open}
          concepts={concepts}
          templates={templates}
          audiences={audiences}
          senders={senders}
          writes={writes}
        />
      )}
    </div>
  );
}

/** Only two statuses carry colour: one that is running and one that broke.
 *  Four coloured chips is a list with no emphasis at all. */
function ruleTone(status: string): "neutral" | "accent" | "muted" {
  if (status === "active") return "accent";
  if (status === "draft" || status === "paused") return "muted";
  return "neutral";
}

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
  const sentence = ruleSentence(rule, {
    template: nameOfTemplate(templates, rule.templateId),
    audience: nameOfAudience(audiences, rule.audienceId),
  });
  return (
    <ListRow
      icon={<Zap size={16} aria-hidden />}
      name={ruleName(rule)}
      current={rule.status === "active"}
      dim={rule.status === "paused"}
      open={open}
      onOpen={onToggle}
      state={
        <>
          <Chip tone={ruleTone(rule.status)}>{rule.status || "draft"}</Chip>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {/* THE LIST READS THE WAY THE BUILDER DOES. Somebody who built a rule by
          filling in a sentence should recognise it here without translating. */}
      <span className="os-caption os-campaign-rule-sentence">{sentence}</span>
      {/* LIVENESS IS DISPLAYED, NEVER RUNG. */}
      {rule.firedCount === 0 ? null : (
        <span className="os-caption os-mono">{rule.firedCount}x</span>
      )}
    </ListRow>
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
          {ruleSentence(rule, {
            template: nameOfTemplate(templates, rule.templateId),
            audience: nameOfAudience(audiences, rule.audienceId),
          })}
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
          {arming.error === "" ? null : (
            <Notice
              tone="error"
              sentence="The cluster refused to arm this rule."
              next="Nothing is running. What it says below is what stopped it."
              detail={arming.error}
            />
          )}
          <div className="os-campaign-actions">
            <Button
              tone="primary"
              busy={arming.busy}
              busyLabel="Turning on"
              onClick={async () => {
                const ok = await arming.activate(rule.id);
                if (ok) setAsking(false);
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
              <Button tone="primary" onClick={() => setAsking(true)}>
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
          {arming.error === "" ? null : (
            <Notice
              tone="error"
              sentence="The cluster refused that."
              next="Nothing changed."
              detail={arming.error}
            />
          )}
        </>
      )}
    </Panel>
  );
}

function runningLine(rule: EmailRuleRow): string {
  switch (rule.status) {
    case "active":
      return "This rule is running. It sends mail on its own whenever the event happens.";
    case "paused":
      return "Paused. Nothing is sent, and what it generated is still there waiting.";
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

  const ready =
    draft.name.trim() !== "" &&
    draft.triggerConcept !== "" &&
    draft.templateId !== "" &&
    (draft.recipientMode !== "audience" || draft.audienceId !== "") &&
    (draft.recipientMode !== "row_address" || draft.recipientField.trim() !== "");

  async function submit() {
    if (editing && rule) {
      const ok = await writes.updateRule.update(rule.id, draft);
      if (ok) onDone(rule.id);
      return;
    }
    const id = await writes.createRule.create(draft);
    if (id !== "") onDone(id);
  }

  return (
    <Panel label={editing ? "Edit rule" : "New rule"}>
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
            .filter((t) => t.status !== "archived" || t.id === draft.templateId)
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
                .filter((a) => a.status !== "archived" || a.id === draft.audienceId)
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
        <div className="os-campaign-form">
          <Field label="Only when">
            <Input
              id="os-rule-condition"
              label="Condition that must hold for the rule to fire"
              value={draft.condition}
              onChange={(v) => setDraft({ ...draft, condition: v })}
              placeholder={'role=="admin"'}
            />
          </Field>
          <Field label="Sends as">
            <Select
              id="os-rule-sender"
              label="Sending mailbox"
              value={draft.senderIdentityId}
              onChange={(v) => setDraft({ ...draft, senderIdentityId: v })}
            >
              <option value="">This cluster's default mailbox</option>
              {senders
                .filter((s) => s.status !== "disabled" || s.id === draft.senderIdentityId)
                .map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.address}
                  </option>
                ))}
            </Select>
          </Field>
          <Field label="Client">
            <AccountPicker
              id="os-rule-account"
              label="Client this rule is for"
              value={draft.accountId}
              onChange={(v) => setDraft({ ...draft, accountId: v })}
              accounts={accounts}
            />
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
        <Caption>
          Leave the condition empty and the rule fires every time. When it is set, it must hold for
          the mail to go out -- it is checked before anything is sent, and a condition that will not
          compile is refused when the rule is turned on rather than at 3am.
        </Caption>
        {draft.recipientMode === "cluster_roles" ? (
          <Caption>
            The mailbox is not used for this rule: internal mail leaves through the cluster&apos;s own
            configured sender.
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
        <Button tone="primary" busy={write.busy} busyLabel="Saving" onClick={submit} disabled={!ready}>
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
