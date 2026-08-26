import type { ReactNode } from "react";
import { Button, Dialog, ErrorNotice, Panel, Skeleton } from "../ui";
import { Empty } from "./StatusMessage";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { RowDetail } from "./RowDetail";

// The full-row read for a table (or timeline) selection. Same RowDetail the
// retired RowAside used, hosted in the existing Dialog so every TABLE_ELEMENT
// page shares one surface instead of stacking an aside beside a modal.
//
// It also carries the row's ACTIONS (memql#4264): whatever an owner or admin
// can do to the row they have open. Passed in as an opaque child rather than
// built here, because the verbs are per-concept -- a person is suspended, a
// site is not -- and because they iterate option lists, which
// portal_view_composition_test.go rightly forbids inside src/views/. See
// src/people/PersonActions.tsx.
//
// The verbs sit UNDER the reading, never above it: an operator confirms they
// are looking at the right row before they change it.

export function RowDetailDialog({
  open,
  onClose,
  rowId,
  row,
  loading = false,
  error = "",
  missing = false,
  actions,
}: {
  open: boolean;
  onClose: () => void;
  rowId: string;
  row: Record<string, unknown> | null;
  loading?: boolean;
  error?: string;
  missing?: boolean;
  actions?: ReactNode;
}): ReactNode {
  return (
    <Dialog open={open} onClose={onClose} labelledBy="row-detail-dialog-title" size="xl">
      <div className="flex flex-col gap-3 p-5">
        <div className="flex items-baseline justify-between gap-2">
          <h2 id="row-detail-dialog-title" className="text-base font-semibold">
            Row detail
          </h2>
          <Button size="xs" tone="quiet" onClick={onClose}>
            Close
          </Button>
        </div>
        <p className="font-mono text-xs break-all text-subtle">{rowId}</p>
        <Panel>
          {error ? (
            <ErrorNotice sentence="Could not read this row." detail={error} />
          ) : loading ? (
            <Skeleton variant="kv" rows={4} />
          ) : missing ? (
            <Empty>
              This cluster has no row with that id. It may have been deleted, or the
              link may name a row from another cluster.
            </Empty>
          ) : row ? (
            <RowDetail row={row as Row} />
          ) : null}
        </Panel>
        {actions === undefined || row === null ? null : actions}
      </div>
    </Dialog>
  );
}
