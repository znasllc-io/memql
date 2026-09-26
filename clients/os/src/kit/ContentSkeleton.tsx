/** Quiet placeholders for non-list content. Labels are announced, never painted.
 * Keep loaded content mounted during refresh; use these only for missing data.
 */
export function ContentSkeleton({ kind = "detail", label = "Loading content" }: {
  kind?: "detail" | "form" | "metrics" | "map" | "conversation";
  label?: string;
}) {
  return <div className={`os-content-skeleton os-content-skeleton--${kind}`} role="status" aria-busy="true">
    <span className="os-sr-only">{label}</span>
    <div className="os-content-skeleton-shape" aria-hidden="true">
      {Array.from({ length: kind === "metrics" ? 4 : 3 }, (_, i) => <div className="os-content-skeleton-part" key={i}>
        {kind === "conversation" ? <span className="os-skeleton-block os-skeleton-icon" /> : null}
        <div><span className="os-skeleton-block os-skeleton-name" /><span className="os-skeleton-block os-skeleton-detail" /></div>
      </div>)}
    </div>
  </div>;
}

/** A value inside an existing row, without replacing the row or its label. */
export function InlineSkeleton({ label = "Loading value" }: { label?: string }) {
  return <span className="os-inline-skeleton" role="status" aria-busy="true"><span className="os-sr-only">{label}</span><span className="os-skeleton-block os-skeleton-detail" aria-hidden="true" /></span>;
}
