import { useState } from "react";
import { Clock, FileText, User, Users } from "lucide-react";

import { Button, LiveList, RecordRow } from "../../kit";
import { useNow } from "../../kit/useNow";
import type { LiveView } from "../../live/liveView";
import { timeLeftLabel, type OutgoingShare } from "./useSharing";

// THE SHARING PLACE (Jesús, Nov 19 2026, draft A of "One Shared section with
// two groups"): the files the caller is sharing, one row per share, each
// saying four things -- the file, who has it (a person, or a whole group with
// a group icon), how long until it ends on its own, and Stop, which ends it
// now. A share is one file with one person or one group, so a file shared with
// two is two rows: each has its own time and its own Stop.
//
// The column names sit once above the rows (the drawn rows carry them), on the
// record row's own grid so they line up; on a narrow window the rows fold into
// two lines and the names would point at nothing, so they go.

export function SharingPlace({
  source,
  label,
  emptyText,
  titleOf,
  onStop,
}: {
  source: LiveView<OutgoingShare> | null;
  label: string;
  emptyText: string;
  /** The file's name for a share made before shares carried one. */
  titleOf: (share: OutgoingShare) => string;
  /** Ends a share; a refusal is the caller's to say. */
  onStop: (share: OutgoingShare) => Promise<void>;
}) {
  // One clock for the list, so every row's time left agrees (kit/useNow).
  const now = useNow(60_000);
  const [stopping, setStopping] = useState<ReadonlySet<string>>(new Set());
  const stop = async (share: OutgoingShare) => {
    setStopping((prev) => new Set([...prev, share.id]));
    try {
      await onStop(share);
    } finally {
      setStopping((prev) => {
        const next = new Set(prev);
        next.delete(share.id);
        return next;
      });
    }
  };
  const rows = source?.snapshot.rows.length ?? 0;
  return (
    <div className="os-files-outgoing">
      {rows > 0 ? (
        <div className="os-record-list os-files-outgoing-columns" aria-hidden="true">
          <RecordRow
            icon={<span />}
            name="File"
            trailing="Time left"
            actions={<span className="os-button os-files-outgoing-ghost">Stop</span>}
          >
            Shared with
          </RecordRow>
        </div>
      ) : null}
      <LiveList<OutgoingShare>
        source={source}
        rowId={(share) => share.id}
        // A renewal moves expiresAt: the same share, for longer.
        fingerprint={(share) => `${share.title}|${share.expiresAt}`}
        label={label}
        emptyText={emptyText}
        renderRow={(share) => {
          const title = share.title || titleOf(share);
          return (
            <RecordRow
              icon={<FileText size={15} aria-hidden />}
              name={title}
              trailing={
                <span className="os-files-outgoing-left">
                  <Clock size={13} aria-hidden /> {timeLeftLabel(share.expiresAt, now)}
                </span>
              }
              actions={
                <Button
                  busy={stopping.has(share.id)}
                  busyLabel="Stopping"
                  ariaLabel={`Stop sharing ${title} with ${share.recipientDisplayName || "this person"}`}
                  onClick={() => void stop(share)}
                >
                  Stop
                </Button>
              }
            >
              <span className="os-files-outgoing-who">
                {share.recipientGroupId
                  ? <><Users size={13} aria-label="Group" /> {share.recipientDisplayName || "Unnamed group"}</>
                  : <><User size={13} aria-hidden /> {share.recipientDisplayName || "Unnamed person"}</>}
              </span>
            </RecordRow>
          );
        }}
      />
    </div>
  );
}
