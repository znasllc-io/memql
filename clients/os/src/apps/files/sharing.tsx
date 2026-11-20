import { useState } from "react";
import { Users } from "lucide-react";

import { Button, ChoiceRow } from "../../kit";

// SHARING A FILE, AT THE FOOT OF THE RAIL (Jesús, Nov 18 2026; mockup C,
// refined). One click on a file shows "Share with" and the people it can go
// to, nothing else. Clicking a person opens a small panel ABOVE the names:
//   - someone new: 1 / 7 / 10 / 30 days, then Share or Cancel;
//   - someone who already has it: how long is left, Renew (restarts the same
//     length) and Stop sharing (immediate).
// No search for now; a long list scrolls.
//
// The people are the engine's share directory (your organization by display
// name; owners, developers and admins see everyone) and the shares are the
// file's active shares. The engine checks every act again; this only asks.

export interface SharePerson {
  userId: string;
  displayName: string;
}

export interface FileShare {
  shareId: string;
  recipientUserId: string;
  recipientDisplayName: string;
  days: number;
  expiresAt: string;
}

export const SHARE_DAYS = [1, 7, 10, 30] as const;

export function ShareFoot({
  people,
  shares,
  onShare,
  onRenew,
  onStop,
  peopleError = "",
  now = () => new Date(),
}: {
  people: readonly SharePerson[];
  shares: readonly FileShare[];
  onShare: (recipientUserId: string, days: number) => Promise<void>;
  onRenew: (share: FileShare) => Promise<void>;
  onStop: (share: FileShare) => Promise<void>;
  /** The directory read failed: say so, rather than "nobody" (unread is not empty). */
  peopleError?: string;
  now?: () => Date;
}) {
  const [chosen, setChosen] = useState("");
  const [days, setDays] = useState("7");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const shareOf = new Map(shares.map((share) => [share.recipientUserId, share]));
  // Someone who still holds a share but is no longer offered (they left the
  // organization) stays listed, so the share can still be seen and stopped.
  const listed: SharePerson[] = [
    ...people,
    ...shares
      .filter((share) => !people.some((person) => person.userId === share.recipientUserId))
      .map((share) => ({ userId: share.recipientUserId, displayName: share.recipientDisplayName })),
  ];
  const person = listed.find((entry) => entry.userId === chosen);
  const active = person ? shareOf.get(person.userId) : undefined;

  // The act's button holds itself while it runs and then says it is done
  // (kit/oneAtATime), so run answers whether it happened; `busy` only keeps
  // the OTHER buttons of the question still meanwhile.
  const run = async (act: () => Promise<void>, close: boolean): Promise<boolean> => {
    setBusy(true);
    setError("");
    try {
      await act();
      if (close) setChosen("");
      return true;
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const choose = (userId: string) => {
    setError("");
    setDays("7");
    setChosen(chosen === userId ? "" : userId);
  };

  return (
    <section className="os-files-sharing" aria-label="Share with">
      <h3 className="os-files-sharing-head">
        <Users size={14} aria-hidden /> Share with
      </h3>
      {person ? (
        <div className="os-files-share-ask" role="group" aria-label={person.displayName}>
          {active ? (
            <>
              <p className="os-files-share-state">
                Sharing until {untilLabel(active.expiresAt)} · {leftLabel(active.expiresAt, now())}
              </p>
              <div className="os-files-share-acts">
                <Button disabled={busy} busyLabel="Renewing…" doneLabel="Renewed" onClick={() => run(() => onRenew(active), false)}>Renew</Button>
                <Button tone="danger" disabled={busy} busyLabel="Stopping…" onClick={() => run(() => onStop(active), true)}>Stop sharing</Button>
              </div>
            </>
          ) : (
            <>
              <div className="os-files-share-days">
                <ChoiceRow
                  label="For how many days"
                  value={days}
                  onChange={setDays}
                  options={SHARE_DAYS.map((d) => ({ value: String(d), label: String(d), ariaLabel: d === 1 ? "1 day" : `${d} days` }))}
                />
                <span aria-hidden>days</span>
              </div>
              <div className="os-files-share-acts">
                <Button tone="primary" disabled={busy} busyLabel="Sharing…" doneLabel="Shared" onClick={() => run(() => onShare(person.userId, Number(days)), false)}>Share</Button>
                <Button disabled={busy} onClick={() => setChosen("")}>Cancel</Button>
              </div>
            </>
          )}
          {error ? <p className="os-files-share-error" role="alert">{error}</p> : null}
        </div>
      ) : null}
      {peopleError !== "" ? (
        <p className="os-files-share-error" role="alert">The people to share with could not be read. {peopleError}</p>
      ) : null}
      {listed.length === 0 ? (
        peopleError !== "" ? null : <p className="os-files-share-empty">Nobody in your organization to share with yet.</p>
      ) : (
        <ul className="os-files-sharing-list">
          {listed.map((entry) => (
            <li key={entry.userId}>
              <button type="button" aria-pressed={chosen === entry.userId} onClick={() => choose(entry.userId)}>
                <span className="os-files-sharing-ava" aria-hidden>{initials(entry.displayName)}</span>
                <span className="os-files-sharing-name">{entry.displayName}</span>
                {shareOf.has(entry.userId) ? <span className="os-files-sharing-tag">sharing</span> : null}
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function initials(name: string): string {
  return name.split(/\s+/).filter(Boolean).slice(0, 2).map((part) => part[0]!.toUpperCase()).join("");
}

function untilLabel(expiresAt: string): string {
  return new Date(expiresAt).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function leftLabel(expiresAt: string, now: Date): string {
  const ms = new Date(expiresAt).getTime() - now.getTime();
  if (ms <= 0) return "ended";
  // Rounded UP, so a share made a moment ago for 7 days reads "7 days left"
  // and one for 1 day reads "1 day left".
  const days = Math.ceil(ms / 86_400_000);
  return days === 1 ? "1 day left" : `${days} days left`;
}
