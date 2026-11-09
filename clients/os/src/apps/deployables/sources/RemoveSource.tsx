import { useEffect, useRef, useState } from "react";
import { useSession } from "../../../chrome/access";
import { Trash2 } from "lucide-react";
import { Button, Caption } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { InPlaceAsk } from "../../../kit/pressHold";
import { sourceName } from "../list";
import { useWrite } from "../packages/actions";
import { setPackageSourceRemoved } from "../packages/calls";
import { ProblemNotice } from "../packages/ReportView";
import type { PackageRow } from "../packages/rows";

/** Removes a catalog entry only. Archive deliberately remains a separate,
 * explicitly cascading lifecycle operation on the source detail.
 *
 * IT ASKS WHERE ITS TRASH STANDS (DESIGN.md "Edits together"; design record
 * 2026-11-08-site-and-email-statistics, section 10), as Settings' GitHub App
 * removal does (`apps/settings/PipelinesSection.tsx`). It used to ask in a
 * dialog over the page. The question takes the trash's place: what removal
 * keeps, then Cancel and Remove source. The second click of a double-click on
 * the trash lands on that question, so `InPlaceAsk` holds its presses for a
 * moment. Focus goes to Cancel when it asks and back to the trash when it
 * closes without removing. */
export function RemoveSource({ pkg, onRemoved }: { pkg: PackageRow; onRemoved?: () => void }) {
  const [armed, setArmed] = useState(false);
  const write = useWrite();
  const pending = useRef(false);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const { access } = useSession();
  const boundary = `${access?.userId}:${pkg.id}`;
  const currentBoundary = useRef(boundary); currentBoundary.current = boundary;
  // The first button in the region is Cancel while it asks and the trash
  // while it does not, so one move covers both directions.
  const region = useRef<HTMLDivElement>(null);
  const wasArmed = useRef(false);
  useEffect(() => {
    if (wasArmed.current !== armed) region.current?.querySelector<HTMLButtonElement>("button")?.focus();
    wasArmed.current = armed;
  }, [armed]);
  const name = sourceName(pkg);
  return <div ref={region} className="deployable-remove-source os-app-stack" data-armed={armed || undefined}>
    {armed ? <>
      <Caption>Remove this source from the list and saved choices? Existing deployables and automatic updates keep running. Other sources and GitHub connections are unchanged. You can restore it through Add deployable.</Caption>
      {write.refusal ? <ProblemNotice problem={write.refusal} tone="error" /> : null}
      <InPlaceAsk className="os-confirm-row os-panel-actions" role="group" aria-label="Confirm source removal">
        <Button tone="quiet" disabled={write.busy} onClick={() => { write.clear(); setArmed(false); }}>Cancel</Button>
        <Button tone="danger" busy={write.busy} busyLabel="Removing source" onClick={() => {
          if (pending.current) return;
          pending.current = true;
          const startedFor = boundary;
          void write.run(query => setPackageSourceRemoved(query, pkg.id, true)).then(ok => {
            pending.current = false;
            if (ok && mounted.current && currentBoundary.current === startedFor) { setArmed(false); onRemoved?.(); }
          });
        }}>Remove source</Button>
      </InPlaceAsk>
    </> : <IconButton label={`Remove source ${name}`} disabled={write.busy} onClick={() => { write.clear(); setArmed(true); }}><Trash2 size={16} aria-hidden /></IconButton>}
  </div>;
}
