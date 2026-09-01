import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AtSign, Plus } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
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
  Subhead,
  formatMoment,
} from "../../kit";
import { useLiveView } from "../../live/liveView";
import type { CampaignWrites } from "./actions";
import {
  senderFingerprint,
  senderIdentityFromRow,
  senderIsRetired,
  senderLabel,
  type SenderIdentityRow,
} from "./rows";
import type { CampaignFeeds } from "./useCampaigns";

// Senders: the mailboxes this deployment may send campaign mail as.
//
// ===========================================================================
// WHY THIS SECTION EXISTS AT ALL
// ===========================================================================
// Without it the campaign editor's identity picker is empty on every fresh
// cluster, and the only way to get a row into it is a raw mutation. It is not
// a Settings concern either, even though it sits next to one: Settings ->
// Integrations is about CREDENTIALS -- the Graph client secret, the SMTP
// password, the things an operator rotates from a shell -- and a sending
// identity carries none. It is a campaigns record that says a mailbox exists
// and may be used.
//
// NO CREDENTIAL CROSSES THIS BOUNDARY, which is what makes it an ordinary
// client-reachable write. Authentication stays the cluster's single mail
// credential. What declaring an address here CANNOT do is make a mailbox
// sendable -- that is the tenant's own policy, and an address declared here
// but missing from it comes back as the provider's own 403 on the campaign's
// lastError.

export function SendersSection({
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

  const source = useLiveView<Row, SenderIdentityRow>(
    feeds.senders.source,
    `filed:${showFiled}`,
    (rows) => {
      const senders = rows.map(senderIdentityFromRow).filter((s) => s.id !== "");
      return showFiled ? senders : senders.filter((s) => !senderIsRetired(s));
    },
  );

  const open = useMemo(
    () => source?.snapshot.rows.find((s) => s.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Senders">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> Add a mailbox
        </Button>
      </Head>

      {feeds.senders.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its sending mailboxes."
          next="Nothing below is current."
        >
          <Button onClick={feeds.senders.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <SenderForm
          writes={writes}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      ) : null}

      <div className="os-campaign-filters">
        <Check checked={showFiled} onChange={onToggleFiled}>
          Show retired mailboxes
        </Check>
      </div>

      <LiveList<SenderIdentityRow>
        key={`senders:${showFiled}`}
        source={source}
        rowId={(s) => s.id}
        fingerprint={senderFingerprint}
        label="Mailboxes this cluster can send as"
        emptyText="No mailboxes declared. Campaigns will use this cluster's configured default -- add one here to send as a specific address."
        renderRow={(sender, tick) => (
          <SenderLine
            sender={sender}
            tick={tick}
            open={openId === sender.id}
            onToggle={() => setOpenId((held) => (held === sender.id ? "" : sender.id))}
          />
        )}
      />

      {open === null ? null : (
        <SenderDetail key={open.id} sender={open} writes={writes} />
      )}
    </div>
  );
}

function SenderLine({
  sender,
  tick,
  open,
  onToggle,
}: {
  sender: SenderIdentityRow;
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const retired = senderIsRetired(sender);
  return (
    <ListRow
      icon={<AtSign size={16} aria-hidden />}
      name={<span className="os-mono">{senderLabel(sender)}</span>}
      current={!retired}
      dim={retired}
      open={open}
      onOpen={onToggle}
      state={
        <>
          {retired ? <Chip tone="muted">retired</Chip> : null}
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {sender.fromName === "" ? null : <span className="os-caption">{sender.fromName}</span>}
    </ListRow>
  );
}

function SenderDetail({ sender, writes }: { sender: SenderIdentityRow; writes: CampaignWrites }) {
  const accounts = useAccountOptions();
  const [editing, setEditing] = useState(false);
  const retired = senderIsRetired(sender);

  return (
    <div className="os-campaign-detail">
      <Panel label={`${senderLabel(sender)} details`}>
        <div className="os-campaign-detail-head">
          <Subhead>
            <span className="os-mono">{senderLabel(sender)}</span>
          </Subhead>
          <AccountChip name={accountNameFrom(accounts, sender.accountId)} />
        </div>
        {editing ? (
          <SenderForm sender={sender} writes={writes} onDone={() => setEditing(false)} />
        ) : (
          <>
            <Facts>
              <Fact label="Address" value={sender.address} mono />
              <Fact label="Shows as" value={sender.fromName} />
              <Fact label="Replies go to" value={sender.replyTo} mono />
              <Fact label="Notes" value={sender.notes} />
              <Fact label="Declared" value={formatMoment(sender.createdAt)} />
            </Facts>
            <div className="os-campaign-actions">
              <Button onClick={() => setEditing(true)}>Edit</Button>
            </div>
          </>
        )}
      </Panel>

      <RetirePanel sender={sender} writes={writes} retired={retired} />
    </div>
  );
}

/**
 * Retire a mailbox, or bring it back.
 *
 * RETIRING IS A STATUS FLIP AND NEVER A DELETE, and the panel says WHY rather
 * than leaving somebody hunting for a delete button that is deliberately not
 * there: past campaigns name this row, and the reputation and warmup history
 * is keyed on its address. Removing it would orphan the evidence a
 * deliverability review is made of.
 *
 * IT ALSO SAYS WHAT RETIRING DOES TO A CAMPAIGN THAT NAMES IT. The preflight
 * REFUSES a campaign whose identity is retired rather than falling back to the
 * default -- because a silent fallback mails a client's list from the wrong
 * mailbox and nothing says so. Somebody retiring a mailbox needs to know that
 * before they do it, not from a refused send tomorrow.
 */
function RetirePanel({
  sender,
  writes,
  retired,
}: {
  sender: SenderIdentityRow;
  writes: CampaignWrites;
  retired: boolean;
}) {
  const status = writes.senderStatus;
  const [asking, setAsking] = useState(false);

  if (retired) {
    return (
      <Panel label="Retired">
        <Subhead>Retired</Subhead>
        <Caption>
          This mailbox is not offered when a campaign is written, and a campaign that still names it
          will be refused rather than quietly sent from somewhere else. Everything it has already
          sent keeps naming it.
        </Caption>
        <div className="os-campaign-actions">
          <Button
            busy={status.busy}
            busyLabel="Bringing back"
            onClick={() => status.set(sender.id, "active")}
          >
            Use this mailbox again
          </Button>
        </div>
        {status.error === "" ? null : (
          <Notice
            tone="error"
            sentence="That did not change."
            next="The mailbox is still retired."
            detail={status.error}
          />
        )}
      </Panel>
    );
  }

  return (
    <Panel label="Retire">
      <Subhead>Retire</Subhead>
      {!asking ? (
        <>
          <Caption>
            Retiring takes a mailbox out of the picker. It is never deleted -- past campaigns name
            it, and its sending reputation is kept against its address.
          </Caption>
          <Button onClick={() => setAsking(true)}>Retire this mailbox</Button>
        </>
      ) : (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">Retire {senderLabel(sender)}?</p>
          <Caption>
            It stops being offered for new campaigns. Any campaign that already names it will be
            REFUSED rather than sent from the default mailbox -- sending a client&apos;s list from
            the wrong address is worse than not sending it. Change those campaigns first if any are
            waiting.
          </Caption>
          {status.error === "" ? null : (
            <Notice
              tone="error"
              sentence="This mailbox was not retired."
              next="Nothing changed."
              detail={status.error}
            />
          )}
          <div className="os-campaign-actions">
            <Button
              tone="danger"
              busy={status.busy}
              busyLabel="Retiring"
              onClick={async () => {
                const ok = await status.set(sender.id, "disabled");
                if (ok) setAsking(false);
              }}
            >
              Retire
            </Button>
            <Button
              onClick={() => {
                setAsking(false);
                status.reset();
              }}
            >
              Keep it
            </Button>
          </div>
        </div>
      )}
    </Panel>
  );
}

function SenderForm({
  sender,
  writes,
  onDone,
}: {
  sender?: SenderIdentityRow;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const editing = sender !== undefined;
  const write = editing ? writes.updateSender : writes.createSender;
  const [draft, setDraft] = useState(() => ({
    address: sender?.address ?? "",
    fromName: sender?.fromName ?? "",
    replyTo: sender?.replyTo ?? "",
    accountId: sender?.accountId ?? "",
    notes: sender?.notes ?? "",
  }));

  const ready = draft.address.trim() !== "" && draft.fromName.trim() !== "";

  async function submit() {
    if (editing && sender) {
      const ok = await writes.updateSender.update(sender.id, draft);
      if (ok) onDone(sender.id);
      return;
    }
    const id = await writes.createSender.create(draft);
    if (id !== "") onDone(id);
  }

  return (
    <Panel label={editing ? "Edit mailbox" : "Add a mailbox"}>
      <div className="os-campaign-form">
        <Field label="Address">
          <Input
            id="os-sender-address"
            label="Mailbox address mail is sent from"
            value={draft.address}
            onChange={(v) => setDraft({ ...draft, address: v })}
            placeholder="news@acme.com"
          />
        </Field>
        <Field label="Shows as">
          <Input
            id="os-sender-fromname"
            label="Display name on the From line"
            value={draft.fromName}
            onChange={(v) => setDraft({ ...draft, fromName: v })}
            placeholder="Acme News"
          />
        </Field>
        <Field label="Replies go to">
          <Input
            id="os-sender-replyto"
            label="Reply-to address"
            value={draft.replyTo}
            onChange={(v) => setDraft({ ...draft, replyTo: v })}
          />
        </Field>
        <Field label="Client">
          <AccountPicker
            id="os-sender-account"
            label="Client this mailbox is for"
            value={draft.accountId}
            onChange={(v) => setDraft({ ...draft, accountId: v })}
            accounts={accounts}
          />
        </Field>
        <Field label="Notes">
          <Input
            id="os-sender-notes"
            label="Notes about this mailbox"
            value={draft.notes}
            onChange={(v) => setDraft({ ...draft, notes: v })}
          />
        </Field>
      </div>

      <Caption>
        Declaring a mailbox here says this cluster may send as it. It does not create the mailbox or
        grant access to it -- if your mail tenant has not been told to allow it, the first send comes
        back with the provider&apos;s own refusal on the campaign.
      </Caption>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence={editing ? "This did not save." : "This mailbox was not added."}
          next="Nothing was written; what is above is still as you typed it."
          detail={write.error}
        />
      )}

      <div className="os-campaign-actions">
        <Button tone="primary" busy={write.busy} busyLabel="Saving" onClick={submit} disabled={!ready}>
          {editing ? "Save" : "Add mailbox"}
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
      {ready ? null : (
        <Caption>An address and a display name are both needed -- they are what a recipient sees.</Caption>
      )}
    </Panel>
  );
}
