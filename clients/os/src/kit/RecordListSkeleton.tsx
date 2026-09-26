import { RecordRow } from "./RecordRow";

/** The same geometry as a record list, without fake records or focus targets. */
export function RecordListSkeleton({ label = "Loading", rows = 2 }: { label?: string; rows?: number }) {
  return <div className="os-record-skeleton" role="status" aria-busy="true">
    <span className="os-sr-only">{label}</span>
    <div aria-hidden="true">{Array.from({ length: rows }, (_, i) => <RecordRow key={i}
      icon={<span className="os-skeleton-block os-skeleton-icon" />}
      name={<span className="os-skeleton-block os-skeleton-name" />}
      secondary={<span className="os-skeleton-block os-skeleton-detail" />}
      state={<span className="os-skeleton-block os-skeleton-state" />} />)}</div>
  </div>;
}
