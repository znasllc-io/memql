import { AtSign } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, EmptyState, LiveList, Notice, RecordRow, Subhead } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { useLiveView } from "../../../live/liveView";
import { isOperatorOrganization } from "../organizations";
import { senderFingerprint, senderIdentityFromRow, senderIsRetired, senderLabel, type SenderIdentityRow } from "../rows";
import { senderRefusalWord, type SenderRefusal } from "../senderChecks";
import type { EmailFeeds } from "../useEmail";
import { WORDS } from "../words";

// Senders: the addresses an organization's mail can be sent as. A subject of
// Settings -- not a tab -- because a sender is how the app is set up to send.
//
// It reads the app's one senders feed and narrows it to the organization the
// page is on; it opens no subscription and no read of its own. NEWEST FIRST,
// which the feed's own order is not once a sender arrives live (an arrival
// joins the end), so the order is made here, from when each was added.

/** Newest first. A time is compared as a time, not as text: `...00Z` and `...00.5Z` do not sort alike. */
function newestFirst(senders: SenderIdentityRow[]): SenderIdentityRow[] {
  const at = (sender: SenderIdentityRow) => {
    const time = Date.parse(sender.createdAt);
    return Number.isNaN(time) ? 0 : time;
  };
  return [...senders].sort((a, b) => at(b) - at(a));
}

export function SendersPanel({ feeds, accountId, includeArchived, checks, onOpen, onAdd }: {
  feeds: EmailFeeds;
  /** The organization the page is on. */
  accountId: string;
  includeArchived: boolean;
  /**
   * The engine's sending check for each sender, by id (`useSenderChecks`): a
   * sender a send would be refused as says so in place of Active.
   */
  checks?: ReadonlyMap<string, SenderRefusal>;
  onOpen: (senderId: string) => void;
  /** Absent while the page cannot yet say which way of adding a sender is this organization's. */
  onAdd?: () => void;
}) {
  const source = useLiveView<Row, SenderIdentityRow>(
    feeds.senders.source,
    `${accountId}:${includeArchived}`,
    (rows) =>
      newestFirst(
        rows
          .map(senderIdentityFromRow)
          .filter((sender) => sender.id !== "" && sender.accountId === accountId && (includeArchived || !senderIsRetired(sender))),
      ),
  );
  const failed = feeds.senders.snapshot.error !== "";

  return (
    <section className="os-app-stack" aria-label="Senders">
      <Subhead actions={onAdd ? <AddButton label="Add a sender" onClick={onAdd} /> : null}>Senders</Subhead>
      <LiveList<SenderIdentityRow>
        // Keyed on what narrows it, so choosing another organization or turning
        // on archived senders re-baselines the arrival cues: rows the browser
        // already had are not news.
        key={`${accountId}:${includeArchived}`}
        source={source}
        rowId={(sender) => sender.id}
        fingerprint={senderFingerprint}
        label="Senders"
        // A failed read is not an empty list: the list says what failed and how
        // to try again, and the empty state is not drawn beside it.
        errorContent={
          <Notice tone="error" sentence="This cluster did not return the senders." detail={feeds.senders.snapshot.error}>
            <Button onClick={feeds.senders.reseed}>Try again</Button>
          </Notice>
        }
        emptyContent={
          failed ? null : (
            <EmptyState icon={AtSign} title="No senders yet">
              {isOperatorOrganization(accountId)
                ? "Without one, this organization sends from the cluster's sender."
                : "Add one before this organization sends mail."}
            </EmptyState>
          )
        }
        renderRow={(sender, tick) => {
          const archived = senderIsRetired(sender);
          const refused = archived ? "" : senderRefusalWord(checks?.get(sender.id) ?? "");
          return (
            <RecordRow
              icon={<AtSign size={16} aria-hidden />}
              name={<span className="os-mono">{senderLabel(sender)}</span>}
              secondary={sender.fromName}
              state={archived ? WORDS.archived : refused || WORDS.active}
              tone={archived ? "muted" : refused ? "warn" : "accent"}
              current={!archived && !refused}
              dim={archived}
              stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
              onOpen={() => onOpen(sender.id)}
            />
          );
        }}
      />
    </section>
  );
}
