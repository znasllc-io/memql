import { useState } from "react";
import { User, Users } from "lucide-react";

import { LiveList } from "../../kit";
import { CardCarousel } from "../../kit/CardCarousel";
import { useNow } from "../../kit/useNow";
import { useLiveView, type LiveView } from "../../live/liveView";
import { artifactFromRow } from "./rows";
import { ShareCardActions, shareMenuEntries } from "./ShareView";
import { FileTile } from "./Tiles";
import { timeLeftLabel, type OutgoingShare } from "./useSharing";

// THE SHARING PLACE: the files the caller is sharing, in the same tiles as
// Shared with me and the Library (José, Nov 22 2026: Sharing should look like
// Shared with me; Jesús picked draft 7B). One tile per FILE, however many
// people and groups have it. Picking a tile opens, below the tiles, one card
// per person or group it is shared with -- the name, then "User · 6 days left"
// or "Group · 6 days left" -- each with the share view's own "⋯" menu (Renew,
// Remove sharing; ShareView's shareMenuEntries), so a share is renewed or
// stopped the same way in both places. Nothing picked: only the tiles.
//
// A share is one file with one person or one group, so a card is one share:
// each has its own time and its own menu.

/** Renew here is for the share view's starting length: there is no Expires on this page. */
const RENEW_DAYS = 7;

interface SharedFile {
  id: string;
  createdAt: string;
  shares: OutgoingShare[];
}

export function SharingPlace({
  source,
  label,
  emptyText,
  titleOf,
  onStop,
  onRenew,
}: {
  source: LiveView<OutgoingShare> | null;
  label: string;
  emptyText: string;
  /** The file's name for a share made before shares carried one. */
  titleOf: (share: OutgoingShare) => string;
  /** Ends a share; a refusal is the caller's to say. */
  onStop: (share: OutgoingShare) => Promise<void>;
  /** Renews a share for `days`; absent where the caller may not renew (other people's shares). */
  onRenew?: (share: OutgoingShare, days: number) => Promise<void>;
}) {
  // One clock for the cards, so every card's time left agrees (kit/useNow).
  const now = useNow(60_000);
  const [picked, setPicked] = useState("");
  const [busy, setBusy] = useState<ReadonlySet<string>>(new Set());
  const run = async (share: OutgoingShare, act: () => Promise<void>) => {
    setBusy((prev) => new Set([...prev, share.id]));
    try {
      await act();
    } finally {
      setBusy((prev) => {
        const next = new Set(prev);
        next.delete(share.id);
        return next;
      });
    }
  };
  // The shares, folded into their files in the order the Head's sort gave them.
  const files = useLiveView<OutgoingShare, SharedFile>(source, "sharing", (shares) => {
    const byFile = new Map<string, SharedFile>();
    for (const share of shares) {
      const key = share.artifactId || share.id;
      const file = byFile.get(key);
      if (file) file.shares.push(share);
      else byFile.set(key, { id: key, createdAt: share.createdAt, shares: [share] });
    }
    return [...byFile.values()];
  });
  // Named at render, not in the fold: an older share's name arrives later
  // (useShareTitles) without a new snapshot behind it.
  const titleOfFile = (file: SharedFile) => file.shares[0]!.title || titleOf(file.shares[0]!);
  const pickedFile = picked === "" ? null : files?.snapshot.rows.find((file) => file.id === picked) ?? null;
  const pick = (id: string) => setPicked((prev) => (prev === id ? "" : id));
  return (
    <div className="os-files-outgoing">
      <div className="os-files-files">
        <LiveList<SharedFile>
          source={files}
          rowId={(file) => file.id}
          // A renewal moves expiresAt: the same file, shared for longer.
          fingerprint={(file) => `${file.shares[0]!.title}|${file.shares.map((s) => `${s.id}:${s.expiresAt}`).join(",")}`}
          label={label}
          emptyText={emptyText}
          renderRow={(file, tick) => (
            <FileTile
              row={artifactFromRow({ id: file.id, kind: "file", title: titleOfFile(file), createdAt: file.createdAt })}
              fixed
              picked={picked === file.id}
              dim={false}
              hint={`Shared with ${file.shares.map((s) => s.recipientDisplayName || (s.recipientGroupId ? "Unnamed group" : "Unnamed person")).join(", ")}`}
              tick={tick}
              onPick={() => pick(file.id)}
              onOpen={() => setPicked(file.id)}
              onMenu={() => {}}
            />
          )}
        />
      </div>
      {pickedFile ? (
        <div className="os-files-outgoing-people">
          <CardCarousel
            key={pickedFile.id}
            items={pickedFile.shares}
            itemKey={(share) => share.id}
            minCardWidth={240}
            label={`Who has ${titleOfFile(pickedFile)}`}
            noun="entry"
            plural="entries"
            renderItem={(share) => {
              const group = share.recipientGroupId !== "";
              const who = share.recipientDisplayName || (group ? "Unnamed group" : "Unnamed person");
              return (
                <div className="os-editable-card os-files-share-card">
                  <div className="os-editable-card-open">
                    <strong>{group ? <Users size={13} aria-label="Group" /> : <User size={13} aria-hidden />} {who}</strong>
                    <span>{group ? "Group" : "User"} · {timeLeftLabel(share.expiresAt, now)}</span>
                  </div>
                  <div className="os-editable-card-actions">
                    <ShareCardActions who={who} disabled={busy.has(share.id)} entries={shareMenuEntries(
                      onRenew ? { label: `Renew for ${RENEW_DAYS} days`, onSelect: () => run(share, () => onRenew(share, RENEW_DAYS)) } : null,
                      () => run(share, () => onStop(share)))} />
                  </div>
                </div>
              );
            }}
          />
        </div>
      ) : null}
    </div>
  );
}
