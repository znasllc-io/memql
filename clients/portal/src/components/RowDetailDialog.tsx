import type { ReactNode } from "react";
import { Button, Dialog, Panel, Skeleton } from "../ui";
import { Empty, ErrorMessage } from "./StatusMessage";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { RowDetail } from "./RowDetail";

// The full-row read for a table (or timeline) selection. Same RowDetail the
// retired RowAside used, hosted in the existing Dialog so every TABLE_ELEMENT
// page shares one surface instead of stacking an aside beside a modal.

export function RowDetailDialog({
  open,
  onClose,
  rowId,
  row,
  loading = false,
  error = "",
  missing = false,
}: {
  open: boolean;
  onClose: () => void;
  rowId: string;
  row: Record<string, unknown> | null;
  loading?: boolean;
  error?: string;
  missing?: boolean;
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
            <ErrorMessage>Failed to read the row: {error}</ErrorMessage>
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
      </div>
    </Dialog>
  );
}
