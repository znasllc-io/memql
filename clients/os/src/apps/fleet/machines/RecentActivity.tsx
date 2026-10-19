import { ArrowUpRight, History } from "lucide-react";
import { AutoRefresh, EmptyState, Notice, RecordList, RecordListSkeleton, Subhead } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { PageHelp } from "../../../kit/pageHelp";
import { ActivityRow, useMachineActivity } from "./MachineActivity";

export const RECENT_ACTIVITY_LIMIT = 5;

/** Equipment's glimpse of the Activity tab: the same entries in the same
 *  order, the first five. The arrow opens the full list; a row opens that
 *  entry's details there. */
export function RecentActivity({ machineId, onActivity, onOpenActivity }: {
  machineId: string; onActivity: () => void; onOpenActivity: (id: string) => void;
}) {
  const { items, loading, error, behind, refresh } = useMachineActivity(machineId);
  const recent = items.slice(0, RECENT_ACTIVITY_LIMIT);
  return <section className="fleet-recent-activity" aria-label="Recent activity">
    <div className="fleet-bank-heading">
      <Subhead meta={loading || behind ? undefined : recent.length}>Recent activity</Subhead>
      <PageHelp title="Recent activity"><p>The latest five entries from this machine’s Activity: model calls, tool calls and app sessions, with queued and running work first. Updates arrive automatically. Open an entry to see its details in Activity, or use the arrow to see all of it.</p></PageHelp>
      <span className="fleet-heading-actions">
        <AutoRefresh busy={loading} enabled={behind} onRefresh={refresh} />
        <IconButton label="All activity" onClick={onActivity}><ArrowUpRight size={16} aria-hidden /></IconButton>
      </span>
    </div>
    {behind ? <Notice tone="warn" sentence="Activity updates are interrupted." next="Showing the last available records." detail={error || undefined} /> : null}
    {loading && !recent.length ? <RecordListSkeleton label="Loading recent activity" rows={3} />
      : recent.length ? <RecordList as="ul" label="Recent activity">{recent.map(item => <ActivityRow key={item.id} item={item} onOpen={() => onOpenActivity(item.id)} />)}</RecordList>
      : behind ? null : <EmptyState icon={History} title="No activity yet">Work assigned to this machine will appear here.</EmptyState>}
  </section>;
}
