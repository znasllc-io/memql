import { useMemo, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Plus, Users } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import type { UploadProvider } from "../../items/upload";
import {
  Button,
  Caption,
  Check,
  Chip,
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
import type { CampaignWrites, ImportReport } from "./actions";
import {
  audienceFingerprint,
  audienceFromRow,
  audienceIsArchived,
  audienceName,
  recipientFromRow,
  recipientIsSendable,
  sendableCount,
  type AudienceRow,
  type RecipientRow,
} from "./rows";
import { useAudienceRecipients, type CampaignFeeds } from "./useCampaigns";

// Audiences: who a campaign goes to, and how they got there.
//
// The list is LIVE. The ROSTER is not, and that is a recorded exclusion rather
// than an omission: hand-editing an audience is human-paced and would be
// affordable to broadcast, but a CSV import is the same concept and is not --
// a 20,000-address file is a 20,000-event burst proportional to a FILE rather
// than to anything a person did.
//
// So the roster prints when it was read and offers to look again, and it says
// what that costs: an address somebody adds in another window does not appear
// here, and an unsubscribe does not flip a row under you. A stale list that
// looks current is the failure this copy exists to prevent.

export function AudiencesSection({
  feeds,
  writes,
  uploads,
  showFiled,
  onToggleFiled,
}: {
  feeds: CampaignFeeds;
  writes: CampaignWrites;
  uploads: UploadProvider;
  showFiled: boolean;
  onToggleFiled: (next: boolean) => void;
}) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);

  const source = useLiveView<Row, AudienceRow>(
    feeds.audiences.source,
    `filed:${showFiled}`,
    (rows) => {
      const audiences = rows.map(audienceFromRow).filter((a) => a.id !== "");
      return showFiled ? audiences : audiences.filter((a) => !audienceIsArchived(a));
    },
  );

  const open = useMemo(
    () => source?.snapshot.rows.find((a) => a.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Audiences">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> New audience
        </Button>
      </Head>

      {feeds.audiences.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your audiences."
          next="Nothing below is current."
        >
          <Button onClick={feeds.audiences.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <AudienceForm
          writes={writes}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      ) : null}

      <div className="os-campaign-filters">
        <Check checked={showFiled} onChange={onToggleFiled}>
          Show archived audiences
        </Check>
      </div>

      <LiveList<AudienceRow>
        key={`audiences:${showFiled}`}
        source={source}
        rowId={(a) => a.id}
        fingerprint={audienceFingerprint}
        label="Your audiences"
        emptyText="No audiences yet. Create one above, then import a CSV or add an address by hand."
        renderRow={(audience, tick) => (
          <AudienceLine
            audience={audience}
            tick={tick}
            open={openId === audience.id}
            onToggle={() => setOpenId((held) => (held === audience.id ? "" : audience.id))}
          />
        )}
      />

      {open === null ? null : (
        <AudienceDetail
          key={open.id}
          audience={open}
          writes={writes}
          uploads={uploads}
          onArchived={() => setOpenId("")}
        />
      )}
    </div>
  );
}

function AudienceLine({
  audience,
  tick,
  open,
  onToggle,
}: {
  audience: AudienceRow;
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const archived = audienceIsArchived(audience);
  return (
    <ListRow
      icon={<Users size={16} aria-hidden />}
      name={audienceName(audience)}
      current={!archived}
      dim={archived}
      open={open}
      onOpen={onToggle}
      state={
        <>
          {archived ? <Chip tone="muted">archived</Chip> : null}
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {audience.description === "" ? null : (
        <span className="os-caption">{audience.description}</span>
      )}
    </ListRow>
  );
}

// ---------------------------------------------------------------------------
// One audience
// ---------------------------------------------------------------------------

function AudienceDetail({
  audience,
  writes,
  uploads,
  onArchived,
}: {
  audience: AudienceRow;
  writes: CampaignWrites;
  uploads: UploadProvider;
  onArchived: () => void;
}) {
  const roster = useAudienceRecipients(audience.id);
  const accounts = useAccountOptions();
  const recipients = useMemo(() => roster.value.map(recipientFromRow), [roster.value]);
  const sendable = sendableCount(recipients);

  return (
    <div className="os-campaign-detail">
      <Panel label={`${audienceName(audience)} details`}>
        <div className="os-campaign-detail-head">
          <Subhead>{audienceName(audience)}</Subhead>
          <AccountChip name={accountNameFrom(accounts, audience.accountId)} />
        </div>
        <Facts>
          <Fact label="Description" value={audience.description} />
          <Fact label="On the list" value={recipients.length} />
          {/* THE DIFFERENCE BETWEEN THESE TWO IS THE SUPPRESSION RATE, which
              is the number somebody about to schedule actually wants -- and it
              is one the roster read already contains, because
              recipientsForAudience returns suppressed rows deliberately. */}
          <Fact label="A send would reach" value={sendable} />
          <Fact label="Created" value={formatMoment(audience.createdAt)} />
        </Facts>
        {recipients.length > 0 && sendable < recipients.length ? (
          <Caption>
            {recipients.length - sendable} of these cannot be mailed -- they unsubscribed, bounced or
            reported a message. They stay on the list on purpose: removing them destroys the record
            and lets the next import bring them back.
          </Caption>
        ) : null}
      </Panel>

      <ImportPanel audience={audience} writes={writes} uploads={uploads} onImported={roster.reload} />

      <AddOnePanel audience={audience} writes={writes} onAdded={roster.reload} />

      <RosterPanel roster={roster} recipients={recipients} writes={writes} />

      {audienceIsArchived(audience) ? null : (
        <ArchivePanel audience={audience} writes={writes} onArchived={onArchived} />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The CSV import, and the report that stays on screen
// ---------------------------------------------------------------------------

/**
 * Import a CSV.
 *
 * ===========================================================================
 * THE REPORT IS A PANEL, NOT A TOAST
 * ===========================================================================
 * `campaignImportRecipients` returns {added, duplicates, invalid, total} plus
 * up to twenty sample bad lines with their line numbers. The operator's NEXT
 * ACTION is fixing the file -- open it, go to line 412, see what is wrong --
 * so the evidence has to stay on screen until they say they are done with it.
 * A toast puts that evidence on a timer, and somebody who looked away has lost
 * the only account of what happened. This shell has no toasts for exactly this
 * reason.
 *
 * THE UPLOAD RIDES THE SHELL'S ONE PATH. `items/edgeUpload.ts` is the only
 * place in `clients/os` that speaks the artifact upload wire, and
 * `test/files/onePath.test.ts` fails the build on a second speaker -- so
 * chunking, resume, retry, progress and verbatim refusals all apply here
 * without this panel learning any of them. The provider comes from the app
 * root, which takes it from the shell.
 *
 * TWO STEPS, AND THE SECOND IS SERVER-SIDE. The file becomes a Library
 * artifact, and the artifact id goes to the engine, which reads the file under
 * the CALLER'S OWN actor. A file the caller cannot read is a file this cannot
 * import: the id is not a capability.
 */
function ImportPanel({
  audience,
  writes,
  uploads,
  onImported,
}: {
  audience: AudienceRow;
  writes: CampaignWrites;
  uploads: UploadProvider;
  onImported: () => void;
}) {
  const input = useRef<HTMLInputElement | null>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState("");
  const importer = writes.importRecipients;

  async function chosen(file: File | null) {
    if (file === null) return;
    setUploading(true);
    setUploadError("");
    try {
      const result = await uploads.upload(file).done;
      const ok = await importer.run(audience.id, result.artifactId);
      if (ok) onImported();
    } catch (err: unknown) {
      // The provider's refusals are already the server's own sentence.
      setUploadError(err instanceof Error ? err.message : String(err));
    } finally {
      setUploading(false);
      // Clear the control so choosing the SAME file again re-fires `change`.
      // Without this, fixing a file and re-picking it does nothing.
      if (input.current) input.current.value = "";
    }
  }

  return (
    <Panel label="Import addresses">
      <Subhead>Import a CSV</Subhead>
      <Caption>
        The first row must be a header with an <span className="os-mono">email</span> column.{" "}
        <span className="os-mono">displayName</span> and <span className="os-mono">name</span> are
        recognised too, and every other column becomes merge data you can put in a template as{" "}
        {"{{fields.<column>}}"}.
      </Caption>

      <div className="os-campaign-actions">
        <input
          ref={input}
          className="os-sr-only"
          id={`os-audience-csv-${audience.id}`}
          type="file"
          accept=".csv,text/csv"
          onChange={(e) => void chosen(e.target.files?.[0] ?? null)}
        />
        <label className="os-button" data-tone="primary" htmlFor={`os-audience-csv-${audience.id}`}>
          {uploading || importer.busy ? "Importing" : "Choose a CSV"}
        </label>
      </div>

      {uploadError === "" ? null : (
        <Notice
          tone="error"
          sentence="The file did not reach the cluster."
          next="Nothing was imported."
          detail={uploadError}
        />
      )}

      {importer.error === "" ? null : (
        <Notice
          tone="error"
          sentence="Nothing was imported."
          next="An import refuses whole rather than adding part of a list -- a partly-imported list is one nobody knows is partial."
          detail={importer.error}
        />
      )}

      {importer.report === null ? null : (
        <ImportReportPanel report={importer.report} onDismiss={importer.dismiss} />
      )}
    </Panel>
  );
}

/**
 * What the import made of the file, stated as a sentence and then as evidence.
 *
 * THE SENTENCE FIRST, because "412 added, 38 already here, 6 unreadable" is
 * the whole answer for most imports and a table makes somebody read four
 * numbers to reconstruct it. The bad lines follow in the mono face with their
 * line numbers, because that is the form somebody needs to go and fix them.
 */
export function ImportReportPanel({
  report,
  onDismiss,
}: {
  report: ImportReport;
  onDismiss: () => void;
}) {
  const clean = report.invalid === 0;
  return (
    <div className="os-campaign-import" data-clean={clean || undefined}>
      <Notice
        tone={clean ? "info" : "warn"}
        sentence={sentenceFor(report)}
        next={
          clean
            ? undefined
            : "Fix those lines and import the file again -- addresses already here are recognised and not added twice."
        }
      >
        {report.samples.length === 0 ? null : (
          <ul className="os-campaign-import-lines" aria-label="Lines that could not be read">
            {report.samples.map((sample, i) => (
              <li key={`${sample.line}-${i}`}>
                <span className="os-campaign-import-line">
                  {sample.line > 0 ? `Line ${sample.line}` : "A line"}
                </span>
                <span className="os-mono">{sample.text || "(empty)"}</span>
                {sample.reason === "" ? null : (
                  <span className="os-caption">{sample.reason}</span>
                )}
              </li>
            ))}
          </ul>
        )}
        {report.invalid > report.samples.length && report.samples.length > 0 ? (
          <Caption>
            Showing the first {report.samples.length} of {report.invalid}.
          </Caption>
        ) : null}
        <div className="os-campaign-actions">
          <Button onClick={onDismiss}>Done with this</Button>
        </div>
      </Notice>
    </div>
  );
}

/** The outcome as one sentence. Every clause is dropped when its figure is
 *  zero -- "and 0 were already here" is noise that makes the real numbers
 *  harder to find. */
function sentenceFor(report: ImportReport): string {
  const parts: string[] = [`${report.added} added`];
  if (report.duplicates > 0) parts.push(`${report.duplicates} already here`);
  if (report.invalid > 0) parts.push(`${report.invalid} could not be read`);
  const from = report.total > 0 ? ` from ${report.total} rows` : "";
  return `${parts.join(", ")}${from}.`;
}

// ---------------------------------------------------------------------------
// One address at a time
// ---------------------------------------------------------------------------

function AddOnePanel({
  audience,
  writes,
  onAdded,
}: {
  audience: AudienceRow;
  writes: CampaignWrites;
  onAdded: () => void;
}) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const add = writes.addRecipient;

  async function submit() {
    if (email.trim() === "") return;
    const ok = await add.add(audience.id, email, name);
    if (ok) {
      setEmail("");
      setName("");
      onAdded();
    }
  }

  return (
    <Panel label="Add one address">
      <Subhead>Add one address</Subhead>
      <div className="os-campaign-form">
        <Field label="Email">
          <Input
            id={`os-audience-email-${audience.id}`}
            label="Address to add"
            value={email}
            onChange={setEmail}
            placeholder="dana@acme.com"
            onEnter={submit}
          />
        </Field>
        <Field label="Name">
          <Input
            id={`os-audience-name-${audience.id}`}
            label="Their name"
            value={name}
            onChange={setName}
            placeholder="Dana"
          />
        </Field>
      </div>
      {add.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That address was not added."
          next="Nothing was written."
          detail={add.error}
        />
      )}
      <div className="os-campaign-actions">
        <Button tone="primary" busy={add.busy} busyLabel="Adding" onClick={submit} disabled={email.trim() === ""}>
          Add
        </Button>
      </div>
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The roster
// ---------------------------------------------------------------------------

/** How many roster rows are named. The read is capped at 100 by the query. */
const ROSTER_ROWS = 50;

const SUBSCRIPTION_STATES = [
  { value: "subscribed", label: "Can be mailed" },
  { value: "unsubscribed", label: "Unsubscribed" },
  { value: "bounced", label: "Address bounced" },
  { value: "complained", label: "Reported as spam" },
];

function RosterPanel({
  roster,
  recipients,
  writes,
}: {
  roster: ReturnType<typeof useAudienceRecipients>;
  recipients: RecipientRow[];
  writes: CampaignWrites;
}) {
  const setSubscription = writes.setSubscription;

  return (
    <Panel label="Who is on this list">
      <div className="os-campaign-detail-head">
        <Subhead>Who is on this list</Subhead>
        <Button busy={roster.state === "loading"} busyLabel="Reading" onClick={roster.reload}>
          Read again
        </Button>
      </div>

      {roster.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return the list."
          next="Nothing is shown below -- that is silence, not an empty audience."
          detail={roster.error}
        />
      ) : recipients.length === 0 ? (
        <Caption>
          {roster.state === "loading"
            ? "Reading the list"
            : "Nobody on this list yet. Import a CSV or add an address above."}
        </Caption>
      ) : (
        <ul className="os-campaign-roster" aria-label="Addresses in this audience">
          {recipients.slice(0, ROSTER_ROWS).map((recipient) => (
            <li
              key={recipient.id}
              className="os-campaign-roster-row"
              data-sendable={recipientIsSendable(recipient) || undefined}
            >
              <span className="os-mono os-campaign-roster-address">{recipient.email || "--"}</span>
              {recipient.displayName === "" ? null : (
                <span className="os-campaign-roster-name">{recipient.displayName}</span>
              )}
              {recipient.source === "" ? null : (
                <Chip tone="muted" title="How this address got here">
                  {recipient.source}
                </Chip>
              )}
              {/* CHANGING SOMEBODY'S STATE IS A SELECT, not four buttons. All
                  four states are reachable from all four -- an operator
                  putting somebody back after a mistaken unsubscribe is a real
                  thing -- and four buttons per row on a fifty-row list is a
                  wall. */}
              <Select
                id={`os-recipient-state-${recipient.id}`}
                label={`Subscription state for ${recipient.email}`}
                value={recipient.subscriptionStatus}
                onChange={async (next) => {
                  const ok = await setSubscription.set(recipient.id, next);
                  if (ok) roster.reload();
                }}
              >
                {SUBSCRIPTION_STATES.map((state) => (
                  <option key={state.value} value={state.value}>
                    {state.label}
                  </option>
                ))}
              </Select>
              {recipient.unsubscribedAt === "" ? null : (
                <span className="os-caption">{formatMoment(recipient.unsubscribedAt)}</span>
              )}
            </li>
          ))}
          {recipients.length > ROSTER_ROWS ? (
            <li className="os-caption">
              and {recipients.length - ROSTER_ROWS} more in this page
            </li>
          ) : null}
        </ul>
      )}

      {setSubscription.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That change did not save."
          next="The list above still shows what the cluster holds."
          detail={setSubscription.error}
        />
      )}

      {roster.readAt === "" ? null : (
        <Caption>
          Read at {new Date(roster.readAt).toLocaleTimeString()}, and not updated since. An audience
          can be a whole imported file, so it is read when you ask rather than streamed -- an address
          added in another window, or an unsubscribe that arrived a minute ago, shows up when you read
          again. {subscriptionNote(recipients)}
        </Caption>
      )}
    </Panel>
  );
}

function subscriptionNote(recipients: RecipientRow[]): string {
  const blocked = recipients.length - sendableCount(recipients);
  return blocked === 0 ? "" : "Anyone who cannot be mailed is still listed, on purpose.";
}

// ---------------------------------------------------------------------------
// Creating and archiving
// ---------------------------------------------------------------------------

function AudienceForm({
  writes,
  onDone,
}: {
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const create = writes.createAudience;
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [accountId, setAccountId] = useState("");

  async function submit() {
    if (name.trim() === "") return;
    const id = await create.create({ name, description, accountId });
    if (id !== "") onDone(id);
  }

  return (
    <Panel label="New audience">
      <div className="os-campaign-form">
        <Field label="Name">
          <Input
            id="os-audience-new-name"
            label="Audience name"
            value={name}
            onChange={setName}
            placeholder="August newsletter"
            onEnter={submit}
          />
        </Field>
        <Field label="What it is for">
          <Input
            id="os-audience-new-description"
            label="What this audience is for"
            value={description}
            onChange={setDescription}
          />
        </Field>
        <Field label="Client">
          <AccountPicker
            id="os-audience-new-account"
            label="Client this audience is for"
            value={accountId}
            onChange={setAccountId}
            accounts={accounts}
          />
        </Field>
      </div>
      {create.error === "" ? null : (
        <Notice
          tone="error"
          sentence="This audience was not created."
          next="Nothing was written; what is above is still as you typed it."
          detail={create.error}
        />
      )}
      <div className="os-campaign-actions">
        <Button
          tone="primary"
          busy={create.busy}
          busyLabel="Creating"
          onClick={submit}
          disabled={name.trim() === ""}
        >
          Create audience
        </Button>
        <Button
          onClick={() => {
            create.reset();
            onDone("");
          }}
        >
          Cancel
        </Button>
      </div>
      <Caption>Only the name is required. Addresses go in next.</Caption>
    </Panel>
  );
}

/**
 * File an audience away.
 *
 * AN IN-SURFACE CONFIRM, never a browser dialog, and it says what archiving
 * does NOT do -- because that is what somebody hesitating actually needs. An
 * archived audience keeps every delivery record that names it; deleting one
 * would orphan the history of every campaign that used it, which is why there
 * is no delete anywhere in this app.
 */
function ArchivePanel({
  audience,
  writes,
  onArchived,
}: {
  audience: AudienceRow;
  writes: CampaignWrites;
  onArchived: () => void;
}) {
  const archive = writes.archiveAudience;
  const [asking, setAsking] = useState(false);
  const [understood, setUnderstood] = useState(false);

  return (
    <Panel label="Archive">
      <Subhead>Archive</Subhead>
      {!asking ? (
        <>
          <Caption>
            Filing an audience away takes it out of the campaign picker. Every campaign that used it
            keeps its record.
          </Caption>
          <Button onClick={() => setAsking(true)}>Archive this audience</Button>
        </>
      ) : (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">Archive {audienceName(audience)}?</p>
          <Caption>
            It stops being offered when a campaign is written, and it appears under the archived
            filter. Everyone on it stays on it, and past sends keep naming it -- an archived audience
            is filed, not deleted.
          </Caption>
          <Check checked={understood} onChange={setUnderstood}>
            I want to file this audience away
          </Check>
          {archive.error === "" ? null : (
            <Notice
              tone="error"
              sentence="This audience was not archived."
              next="Nothing changed."
              detail={archive.error}
            />
          )}
          <div className="os-campaign-actions">
            <Button
              tone="danger"
              busy={archive.busy}
              busyLabel="Archiving"
              disabled={!understood}
              onClick={async () => {
                const ok = await archive.archive(audience.id);
                if (ok) {
                  setAsking(false);
                  setUnderstood(false);
                  onArchived();
                }
              }}
            >
              Archive
            </Button>
            <Button
              onClick={() => {
                setAsking(false);
                setUnderstood(false);
                archive.reset();
              }}
            >
              Cancel
            </Button>
          </div>
        </div>
      )}
    </Panel>
  );
}
