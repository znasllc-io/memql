import { useEffect, useRef, useState } from "react";
import { useSession } from "../../../chrome/access";
import { Trash2 } from "lucide-react";
import { Button, Caption } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { DetailDialog } from "../page/DetailDialog";
import { sourceName } from "../list";
import { useWrite } from "../packages/actions";
import { setPackageSourceRemoved } from "../packages/calls";
import { ProblemNotice } from "../packages/ReportView";
import type { PackageRow } from "../packages/rows";

/** Removes a catalog entry only. Archive deliberately remains a separate,
 * explicitly cascading lifecycle operation on the source detail. */
export function RemoveSource({ pkg, onRemoved }: { pkg: PackageRow; onRemoved?: () => void }) {
  const [armed, setArmed] = useState(false);
  const write = useWrite();
  const pending = useRef(false);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const { access } = useSession();
  const boundary = `${access?.userId}:${pkg.id}`;
  const currentBoundary = useRef(boundary); currentBoundary.current = boundary;
  const name = sourceName(pkg);
  return <>
    <IconButton label={`Remove source ${name}`} disabled={write.busy} onClick={() => { write.clear(); setArmed(true); }}><Trash2 size={16} aria-hidden /></IconButton>
    {armed ? <DetailDialog title={`Remove ${name}?`} onClose={() => { if (!pending.current) setArmed(false); }}>
      <div className="os-app-stack">
      <Caption>Remove this source from the list and saved choices? Existing deployables and automatic updates keep running. Other sources and GitHub connections are unchanged. You can restore it through Add deployable.</Caption>
      {write.refusal ? <ProblemNotice problem={write.refusal} tone="error" /> : null}
      <div className="os-confirm-row">
        <Button disabled={write.busy} onClick={() => setArmed(false)}>Cancel</Button>
        <Button tone="danger" busy={write.busy} onClick={() => {
          if (pending.current) return;
          pending.current = true;
          const startedFor = boundary;
          void write.run(query => setPackageSourceRemoved(query, pkg.id, true)).then(ok => {
            pending.current = false;
            if (ok && mounted.current && currentBoundary.current === startedFor) { setArmed(false); onRemoved?.(); }
          });
        }}>Remove source</Button>
      </div>
      </div>
    </DetailDialog> : null}
  </>;
}
