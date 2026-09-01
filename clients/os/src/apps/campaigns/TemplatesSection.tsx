import { useMemo, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { FileText, Plus } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import {
  Button,
  Caption,
  Check,
  Chip,
  Field,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Row as ListRow,
  Select,
  Subhead,
} from "../../kit";
import { useLiveView } from "../../live/liveView";
import type { CampaignWrites } from "./actions";
import {
  audienceProjection,
  campaignProjection,
  useProjected,
  TestSendPanel,
} from "./CampaignsSection";
import {
  campaignName,
  insertAt,
  mergeTagsFor,
  recipientFromRow,
  templateFingerprint,
  templateFromRow,
  templateIsArchived,
  templateName,
  type CampaignRow,
  type MergeTag,
  type TemplateRow,
} from "./rows";
import { useAudienceRecipients, type CampaignFeeds } from "./useCampaigns";

// Templates: the copy, once, reused by every campaign that sends it.
//
// The list is LIVE. The merge-tag SAMPLE is an on-demand roster read, for the
// reason the Audiences section records.

export function TemplatesSection({
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

  const source = useLiveView<Row, TemplateRow>(
    feeds.templates.source,
    `filed:${showFiled}`,
    (rows) => {
      const templates = rows.map(templateFromRow).filter((t) => t.id !== "");
      return showFiled ? templates : templates.filter((t) => !templateIsArchived(t));
    },
  );

  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const campaigns = useProjected(feeds.campaigns.snapshot.rows, campaignProjection);

  const open = useMemo(
    () => source?.snapshot.rows.find((t) => t.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Templates">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> New template
        </Button>
      </Head>

      {feeds.templates.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your templates."
          next="Nothing below is current."
        >
          <Button onClick={feeds.templates.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <TemplateEditor
          audiences={audiences}
          campaigns={campaigns}
          writes={writes}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      ) : null}


      <LiveList<TemplateRow>
        key={`templates:${showFiled}`}
        source={source}
        rowId={(t) => t.id}
        // THE BODIES ARE FINGERPRINTED. Somebody else fixing a typo in a
        // template a campaign is about to send is precisely the news this cue
        // is for, and no body field moves on a timer.
        fingerprint={templateFingerprint}
        label="Your templates"
        emptyText="No templates yet. Write one above -- a campaign needs one before it can go out."
        renderRow={(template, tick) => (
          <TemplateLine
            template={template}
            tick={tick}
            open={openId === template.id}
            onToggle={() => setOpenId((held) => (held === template.id ? "" : template.id))}
          />
        )}
      />

      {open === null ? null : (
        <TemplateEditor
          key={open.id}
          template={open}
          audiences={audiences}
          campaigns={campaigns}
          writes={writes}
          onDone={() => setOpenId("")}
        />
      )}
    </div>
  );
}

function TemplateLine({
  template,
  tick,
  open,
  onToggle,
}: {
  template: TemplateRow;
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const archived = templateIsArchived(template);
  return (
    <ListRow
      icon={<FileText size={16} aria-hidden />}
      name={templateName(template)}
      current={!archived}
      dim={archived}
      open={open}
      onOpen={onToggle}
      state={
        <>
          <Chip tone={template.status === "ready" ? "accent" : "muted"}>
            {template.status || "draft"}
          </Chip>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {template.subject === "" ? null : (
        <span className="os-caption">{template.subject}</span>
      )}
    </ListRow>
  );
}

// ---------------------------------------------------------------------------
// The editor
// ---------------------------------------------------------------------------

/**
 * Write the copy, with the merge tags beside it.
 *
 * ONE EDITOR FOR WRITING AND EDITING, because the fields are the same and
 * `updateTemplate` takes the same required three. A separate create form would
 * be a second place for the tag strip's behaviour to drift.
 *
 * A TEMPLATE MOVES FROM DRAFT TO READY BY HAND, and the send preflight refuses
 * a campaign whose template is not ready -- so the checkbox here is the thing
 * that unblocks a send, and it says so rather than being an unexplained status
 * field.
 */
function TemplateEditor({
  template,
  audiences,
  campaigns,
  writes,
  onDone,
}: {
  template?: TemplateRow;
  audiences: ReturnType<typeof audienceProjection>;
  campaigns: CampaignRow[];
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const editing = template !== undefined;
  const write = editing ? writes.updateTemplate : writes.createTemplate;
  const textArea = useRef<HTMLTextAreaElement | null>(null);
  const [draft, setDraft] = useState(() => ({
    name: template?.name ?? "",
    subject: template?.subject ?? "",
    textBody: template?.textBody ?? "",
    htmlBody: template?.htmlBody ?? "",
    status: template?.status ?? "draft",
    accountId: template?.accountId ?? "",
  }));

  const ready = draft.name.trim() !== "" && draft.subject.trim() !== "";

  async function submit() {
    if (editing && template) {
      const ok = await writes.updateTemplate.update(template.id, draft);
      if (ok) onDone(template.id);
      return;
    }
    const id = await writes.createTemplate.create(draft);
    if (id !== "") onDone(id);
  }

  /**
   * Put a tag where the cursor is.
   *
   * THE CURSOR FOLLOWS THE TAG. A person clicking a chip is mid-sentence, and
   * dropping them at the end of the body (or back at the start) makes the next
   * keystroke land somewhere they were not. The selection is read off the
   * element rather than tracked in state, because a controlled textarea's
   * selection is the DOM's answer and mirroring it is one more thing to be
   * wrong.
   */
  function insertTag(tag: string) {
    const el = textArea.current;
    const start = el?.selectionStart ?? draft.textBody.length;
    const end = el?.selectionEnd ?? start;
    const next = insertAt(draft.textBody, tag, start, end);
    setDraft({ ...draft, textBody: next.body });
    // After React has written the new value. The cursor is a DOM concern and
    // has to be restored after the controlled re-render, or it snaps to the end.
    queueMicrotask(() => {
      if (!el) return;
      el.focus();
      el.setSelectionRange(next.cursor, next.cursor);
    });
  }

  return (
    <Panel label={editing ? `Edit ${templateName(template)}` : "New template"}>
      <div className="os-campaign-detail-head">
        <Subhead>{editing ? templateName(template) : "New template"}</Subhead>
        <AccountChip name={accountNameFrom(accounts, draft.accountId)} />
      </div>

      <div className="os-campaign-form">
        <Field label="Name">
          <Input
            id="os-template-name"
            label="Template name -- what you call it, not what they see"
            value={draft.name}
            onChange={(v) => setDraft({ ...draft, name: v })}
            placeholder="August product update"
          />
        </Field>
        <Field label="Subject">
          <Input
            id="os-template-subject"
            label="Subject line the recipient sees"
            value={draft.subject}
            onChange={(v) => setDraft({ ...draft, subject: v })}
            placeholder="What we shipped in August"
          />
        </Field>
        <Field label="Client">
          <AccountPicker
            id="os-template-account"
            label="Client this template is for"
            value={draft.accountId}
            onChange={(v) => setDraft({ ...draft, accountId: v })}
            accounts={accounts}
          />
        </Field>
      </div>

      <div className="os-campaign-editor">
        <div className="os-campaign-editor-body">
          <label className="os-form-field-label" htmlFor="os-template-text">
            Message
          </label>
          <textarea
            ref={textArea}
            id="os-template-text"
            className="os-campaign-textarea"
            rows={12}
            value={draft.textBody}
            onChange={(e) => setDraft({ ...draft, textBody: e.target.value })}
            placeholder={"Hello {{displayName}},\n\n..."}
          />
          <Caption>
            Plain text is what everyone receives. An HTML version is optional and is what most mail
            clients will show when there is one.
          </Caption>
          <label className="os-form-field-label" htmlFor="os-template-html">
            HTML version
          </label>
          <textarea
            id="os-template-html"
            className="os-campaign-textarea"
            rows={6}
            value={draft.htmlBody}
            onChange={(e) => setDraft({ ...draft, htmlBody: e.target.value })}
          />
        </div>

        <MergeTags audiences={audiences} onInsert={insertTag} />
      </div>

      <div className="os-campaign-tracking">
        <Check
          checked={draft.status === "ready"}
          onChange={(v) => setDraft({ ...draft, status: v ? "ready" : "draft" })}
        >
          This copy is finished and may be sent
        </Check>
        <Caption>
          A campaign will not start while its template is still a draft. Ticking this is what says
          the writing is done.
        </Caption>
      </div>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence={editing ? "This did not save." : "This template was not created."}
          next="Nothing was written; what is above is still as you typed it."
          detail={write.error}
        />
      )}

      <div className="os-campaign-actions">
        <Button tone="primary" busy={write.busy} busyLabel="Saving" onClick={submit} disabled={!ready}>
          {editing ? "Save" : "Create template"}
        </Button>
        <Button
          onClick={() => {
            write.reset();
            onDone("");
          }}
        >
          {editing ? "Close" : "Cancel"}
        </Button>
      </div>
      {ready ? null : <Caption>A name and a subject line are needed before this can be saved.</Caption>}

      {editing && template ? (
        <TemplateTestSend template={template} campaigns={campaigns} writes={writes} />
      ) : null}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The merge tags
// ---------------------------------------------------------------------------

/**
 * The tags this template can use, showing what each one RENDERS TO.
 *
 * ===========================================================================
 * DOCUMENTATION THAT CANNOT GO STALE
 * ===========================================================================
 * The four base tags are a closed set the renderer knows. `{{fields.*}}` is
 * not: those exist because somebody's CSV had a `company` column, and no list
 * in this repo could know that. Sampling a real recipient is the only way an
 * operator can discover `fields.*` AT ALL -- and showing the resolved value
 * beside the tag catches the case a spelling check cannot, a column that is
 * present but empty for the person you sampled.
 *
 * A TAG WITH NO SAMPLE IS STILL A TAG. Picking no audience shows the four base
 * tags with no previews and no `fields.*` -- which is the truth ("nothing
 * sampled"), not an absence of tags.
 *
 * CLICKING INSERTS AT THE CURSOR. A chip that only displayed would make a
 * person retype a string whose exact spelling is the whole point.
 */
function MergeTags({
  audiences,
  onInsert,
}: {
  audiences: ReturnType<typeof audienceProjection>;
  onInsert: (tag: string) => void;
}) {
  const [sampleFrom, setSampleFrom] = useState("");
  const roster = useAudienceRecipients(sampleFrom);
  const recipient = useMemo(() => {
    const rows = roster.value.map(recipientFromRow);
    return rows[0] ?? null;
  }, [roster.value]);

  const tags = useMemo(
    () =>
      mergeTagsFor({
        recipient,
        // These two resolve at SEND time from the campaign and its client, so
        // there is nothing here to sample them from. The tags are offered and
        // their previews are honestly blank rather than filled with a guess.
        campaignName: "",
        accountName: "",
      }),
    [recipient],
  );

  return (
    <aside className="os-campaign-tagpanel" aria-label="Merge tags">
      <Subhead>Merge tags</Subhead>
      <Caption>
        Click one to put it where your cursor is. It is replaced with that person's own value when
        the message goes out.
      </Caption>

      <Field label="Show values from">
        <Select
          id="os-template-sample"
          label="Audience to sample a recipient from"
          value={sampleFrom}
          onChange={setSampleFrom}
        >
          <option value="">No audience -- just the tag names</option>
          {audiences.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name || a.id}
            </option>
          ))}
        </Select>
      </Field>

      <div className="os-campaign-tags" role="list" aria-label="Available merge tags">
        {tags.map((tag) => (
          <TagChip key={tag.tag} tag={tag} onInsert={onInsert} />
        ))}
      </div>

      {sampleFrom === "" ? (
        <Caption>
          Choose an audience to see what each tag turns into -- and to find the{" "}
          {"{{fields.*}}"} tags an import brought with it.
        </Caption>
      ) : recipient === null ? (
        <Caption>
          {roster.state === "loading"
            ? "Reading that audience"
            : "That audience has nobody on it yet, so there is nothing to sample."}
        </Caption>
      ) : (
        <Caption>
          Values are {recipient.email}&apos;s. Somebody else on the list may have different ones --
          or none, which renders as nothing at all.
        </Caption>
      )}
    </aside>
  );
}

function TagChip({ tag, onInsert }: { tag: MergeTag; onInsert: (tag: string) => void }) {
  return (
    <button
      type="button"
      className="os-campaign-tag"
      role="listitem"
      data-from-import={tag.fromImport || undefined}
      onClick={() => onInsert(tag.tag)}
      title={`Insert ${tag.tag}`}
    >
      <span className="os-mono">{tag.tag}</span>
      {tag.preview === "" ? null : (
        <>
          <span className="os-campaign-tag-arrow" aria-hidden>
            {"->"}
          </span>
          <span className="os-campaign-tag-preview">{tag.preview}</span>
        </>
      )}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Test send, from the template editor
// ---------------------------------------------------------------------------

/**
 * The test send, mounted here against a campaign that uses this template.
 *
 * A TEST IS A CAMPAIGN OPERATION, not a template one: `campaignTestSend`
 * renders the campaign's template through the campaign's resolved sending
 * identity, and there is no such thing as testing copy with no sender. So the
 * panel is the campaign detail's own (`TestSendPanel`), and this wrapper is
 * the one decision the template editor has to make: WHICH campaign.
 *
 * IT SAYS SO WHEN THERE IS NONE, rather than showing a disabled control with
 * no account of itself. "Nothing sends this yet" is a fact with an obvious next
 * action, and a greyed-out button is a question.
 */
function TemplateTestSend({
  template,
  campaigns,
  writes,
}: {
  template: TemplateRow;
  campaigns: CampaignRow[];
  writes: CampaignWrites;
}) {
  const using = useMemo(
    () => campaigns.filter((c) => c.templateId === template.id),
    [campaigns, template.id],
  );
  const [through, setThrough] = useState("");
  const chosen = through === "" ? (using[0]?.id ?? "") : through;

  if (using.length === 0) {
    return (
      <div className="os-campaign-test-empty">
        <Subhead>Send a test</Subhead>
        <Caption>
          No campaign uses this template yet. A test goes out through a campaign -- its audience
          supplies the merge data and its sending identity supplies the mailbox -- so create one in
          Campaigns and the test lands here.
        </Caption>
      </div>
    );
  }

  return (
    <>
      {using.length === 1 ? null : (
        <Field label="Through">
          <Select
            id={`os-template-through-${template.id}`}
            label="Campaign to send the test through"
            value={chosen}
            onChange={setThrough}
          >
            {using.map((c) => (
              <option key={c.id} value={c.id}>
                {campaignName(c)}
              </option>
            ))}
          </Select>
        </Field>
      )}
      <TestSendPanel
        campaignId={chosen}
        testSend={writes.testSend}
        label={
          using.length === 1
            ? `Send a test through ${campaignName(using[0]!)}`
            : "Send a test"
        }
      />
    </>
  );
}
