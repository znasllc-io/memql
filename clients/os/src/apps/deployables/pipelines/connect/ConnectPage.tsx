import type { PackageRow } from "../../packages/rows";
import type { PartsHeld } from "../../parts";
import type { PipelineRow } from "../rows";
import type { ConnectFlow } from "./useConnectFlow";

// Connect pipeline: the rail-as-form page over the source (D14). STUB -- the
// connect rail (Task 9) builds it.

export interface ConnectPageProps {
  pkg: PackageRow;
  /** The source's pipeline when it has one: a reconnect is prefilled from it. */
  pipeline: PipelineRow | null;
  flow: ConnectFlow;
  can: PartsHeld;
  /** Where Back and the trail's parent go: the source page. */
  backLabel: string;
  onBack: () => void;
  /** The pipeline is connected: back to the source page, which now shows its checks. */
  onDone: () => void;
}

export function ConnectPage(props: ConnectPageProps) {
  void props;
  return null;
}
