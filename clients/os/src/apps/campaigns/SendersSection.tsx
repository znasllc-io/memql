import { AzureEmailConnections } from "../../modules/connections/AzureEmailConnections";
import { listCount } from "../../kit/RecordRow";
import { AddButton } from "../../kit/AddButton";
import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AtSign } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import {
  Button,
  EmptyState,
  Caption,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  RecordRow,
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

// Campaign authors select organization-owned, verified sender identities.
// Cluster Settings owns Azure authorization; domain setup reuses that access.
// Adding a mailbox row alone never grants permission to send from its address.

export function SendersSection({
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
  const [connecting, setConnecting] = useState(false);

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

  if (connecting) return <AzureEmailConnections manageCluster={false} onBack={() => setConnecting(false)} />;

  if (adding)
    return (
      <div className="os-app-stack">
        <Head title="Add a mailbox" back={{ label: "Senders", onSelect: () => setAdding(false) }} />
        <SenderForm
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
      <div
        className="os-app-stack"
        data-os-page-context={JSON.stringify({
          page: "Senders",
          name: senderLabel(open),
          id: open.id,
        })}
      >
        <Head
          title={senderLabel(open)}
          back={{ label: "Senders", onSelect: () => setOpenId("") }}
        />
        <SenderDetail key={open.id} sender={open} writes={writes} />
      </div>
    );

  return (
    <div className="os-app-stack">
      <Head title="Senders" meta={listCount(source?.snapshot)}>
        <AddButton onClick={() => setAdding((v) => !v)} label="Add a mailbox" />
      </Head>
      <Button onClick={() => setConnecting(true)}>Connect email domain</Button>

      {feeds.senders.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its sending mailboxes."
          next="Nothing below is current."
        >
          <Button onClick={feeds.senders.reseed}>Try again</Button>
        </Notice>
      ) : null}

      <LiveList<SenderIdentityRow>
        key={`senders:${showFiled}`}
        source={source}
        rowId={(s) => s.id}
        fingerprint={senderFingerprint}
        label="Mailboxes this cluster can send as"
        emptyText="No sending addresses yet. Connect the organization’s email domain to add a verified sender."
        emptyContent={
          <EmptyState icon={AtSign} title="No mailboxes declared">
            Connect your organization’s email domain, then choose the address your clients will see.
          </EmptyState>
        }
        renderRow={(sender, tick) => (
          <SenderLine
            sender={sender}
            tick={tick}
            open={openId === sender.id}
            onToggle={() => setOpenId((held) => (held === sender.id ? "" : sender.id))}
          />
        )}
      />
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
    <RecordRow
      icon={<AtSign size={16} aria-hidden />}
      name={<span className="os-mono">{senderLabel(sender)}</span>}
      current={!retired}
      dim={retired}
      open={open}
      onOpen={onToggle}
      secondary={sender.fromName}
      state={retired ? "Retired" : "Active"}
      tone={retired ? "muted" : "accent"}
      stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
    >
    </RecordRow>
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

export function SenderForm({
  sender,
  initialAccountId = "",
  writes,
  onDone,
}: {
  sender?: SenderIdentityRow;
  initialAccountId?: string;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  const editing = sender !== undefined;
  const write = editing ? writes.updateSender : writes.createSender;
  const [draft, setDraft] = useState(() => ({
    address: sender?.address ?? "",
    fromName: sender?.fromName ?? "",
    replyTo: sender?.replyTo ?? "",
    accountId: sender?.accountId ?? "",
    notes: sender?.notes ?? "",
  }));

  const accountId = draft.accountId || (sender === undefined ? (initialAccountId || defaultAccountId) : "");

  const ready = organizationChosen(accounts, accountId) && draft.address.trim() !== "" && draft.fromName.trim() !== "";

  async function submit() {
    if (!ready) return;
    if (editing && sender) {
      const ok = await writes.updateSender.update(sender.id, { ...draft, accountId });
      if (ok) onDone(sender.id);
      return;
    }
    const id = await writes.createSender.create({ ...draft, accountId });
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
        <Field label="Organization">
          <AccountPicker
            id="os-sender-account"
            label="Organization this mailbox is for"
            required
            value={accountId}
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
        grant access to it -- if your mail tenant has not been told to allow it, the first send
        comes back with the provider&apos;s own refusal on the campaign.
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
        <Button
          tone="primary"
          busy={write.busy}
          busyLabel="Saving"
          onClick={submit}
          disabled={!ready}
        >
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
        <Caption>
          An address and a display name are both needed -- they are what a recipient sees.
        </Caption>
      )}
    </Panel>
  );
}
