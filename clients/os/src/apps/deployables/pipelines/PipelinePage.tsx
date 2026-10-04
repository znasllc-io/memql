import type { PackageRow } from "../packages/rows";
import type { PartsHeld } from "../parts";
import type { PipelineRow, RunRow } from "./rows";

// Pipeline settings: the source's pipeline, read and changed (D14's "Pipeline
// settings"). STUB -- Task 8 builds it.

export interface PipelinePageProps {
  pkg: PackageRow;
  pipeline: PipelineRow;
  runs: readonly RunRow[];
  can: PartsHeld;
  backLabel: string;
  onBack: () => void;
  /** Reopen the connect rail over this source, prefilled: Change, or Connect again. */
  onChange: () => void;
  onOpenRuns: () => void;
}

export function PipelinePage(props: PipelinePageProps) {
  void props;
  return null;
}
