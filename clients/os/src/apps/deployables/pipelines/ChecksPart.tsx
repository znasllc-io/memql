import type { PackageRow } from "../packages/rows";
import type { PipelineRow, RunRow } from "./rows";

// The source page's Checks (D14), between the source's facts and Apps it
// produces. STUB -- Task 8 builds it.

export interface ChecksPartProps {
  pkg: PackageRow;
  /** The source's pipeline, or null when it has none. */
  pipeline: PipelineRow | null;
  /** This pipeline's runs, newest first. */
  runs: readonly RunRow[];
  /** Whether the pipelines and runs feeds have answered: before that nothing is said. */
  settled: boolean;
  onOpenRun: (runId: string) => void;
  /** Open the Runs tab refined to this source. */
  onOpenRuns: () => void;
}

export function ChecksPart(props: ChecksPartProps) {
  void props;
  return null;
}
