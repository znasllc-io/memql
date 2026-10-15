import { ArrowUpRight, History } from "lucide-react";
import { EmptyState, Notice, RecordRow, Subhead } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { PageHelp } from "../../../kit/pageHelp";
import { LiveList } from "../../../live/LiveList";
import { useLiveView } from "../../../live/liveView";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { appLabel, statusTone } from "../rows";
import { useAppSessions } from "./useAppSessions";

export const RECENT_WORK_LIMIT = 5;

export function RecentWork({ machineId, onOpenSession, onAllSessions }: {
  machineId: string; onOpenSession?: (id: string) => void; onAllSessions: () => void;
}) {
  const sessions = useAppSessions();
  const source = useLiveView(sessions.source, machineId, rows => rows.filter(session => session.workerId === machineId).slice(0, RECENT_WORK_LIMIT));
  const count = sessions.state === "live" && !sessions.error ? source?.snapshot.rows.length : undefined;
  return <section className="fleet-recent-work" aria-label="Recent app sessions">
    <div className="fleet-bank-heading">
      <Subhead meta={count}>Recent work</Subhead>
      <PageHelp title="Recent work"><p>The latest five sessions for this machine from your recent app-session history. New work and status changes appear automatically. Open a session to inspect it, or use All app sessions to browse your full activity list.</p></PageHelp>
      <span className="fleet-heading-actions">
        <AutoRefresh busy={sessions.loading} enabled={feedIsBehind(sessions.state, sessions.error)} onRefresh={sessions.reread} />
        <IconButton label="All app sessions" onClick={onAllSessions}><ArrowUpRight size={16} aria-hidden /></IconButton>
      </span>
    </div>
    <LiveList source={source} label="Recent app sessions" rowId={session => session.id}
      fingerprint={session => `${session.app}:${session.status}:${session.runId}`}
      renderRow={session => <RecordRow name={appLabel(session.app)} secondary={session.runId || session.id}
        onOpen={onOpenSession ? () => onOpenSession(session.id) : undefined} state={session.status} tone={statusTone(session.status) === "ok" ? "accent" : statusTone(session.status) === "neutral" ? "muted" : "warn"} />}
      errorContent={<Notice sentence="Recent app sessions could not be read." detail={sessions.error} />}
      emptyContent={<EmptyState icon={History} title="No recent work">App sessions will appear here when an app runs on this machine.</EmptyState>} />
  </section>;
}
