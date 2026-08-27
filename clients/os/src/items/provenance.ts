// The provenance dot (spec D3): ONE derivation for "where is this from and
// can I reach it now", consumed by desktop files, and exported through kit/
// so the app epics reuse it instead of respelling it. The dot never
// guesses: input we cannot classify renders NO dot, not a green one.

import type { FileEntry } from "../system/desktop";

export type ProvenanceTone = "reachable" | "unreachable" | "unknown";

export interface ProvenanceFacts {
  tone: ProvenanceTone;
  /** Short origin sentence for hover/inspection ("Uploaded here"). */
  origin: string;
}

/** Library sources whose bytes live in the cluster once indexed. */
const CLUSTER_SOURCES = new Set([
  "uploaded",
  "exported",
  "workbench_generated",
  "agent_generated",
  "derived",
  "user_created",
  "live",
]);

export interface MachinePresence {
  /** Machine display name when known. */
  name?: string;
  online: boolean;
}

/**
 * Derive the dot from the artifact's provenance plus (for computer-use
 * files) the producing machine's presence. `machine` is null when the
 * caller has no fleet facts yet -- that renders "unknown", never green.
 */
export function deriveProvenance(
  file: Pick<FileEntry, "source" | "producedByWorkerId" | "uploadState">,
  machine: MachinePresence | null = null,
): ProvenanceFacts {
  if (file.uploadState === "uploading") return { tone: "unknown", origin: "Uploading" };
  if (file.uploadState === "failed") return { tone: "unreachable", origin: "Upload failed" };

  if (file.source === "computer_use") {
    if (!file.producedByWorkerId) {
      return { tone: "unknown", origin: "Made by computer use" };
    }
    if (!machine) {
      return { tone: "unknown", origin: "Made on one of your machines" };
    }
    const name = machine.name || "your machine";
    return machine.online
      ? { tone: "reachable", origin: `Made on ${name} (online)` }
      : { tone: "unreachable", origin: `Made on ${name} (offline)` };
  }

  if (CLUSTER_SOURCES.has(file.source)) {
    const labels: Record<string, string> = {
      uploaded: "Uploaded here",
      exported: "Exported from an artifact",
      workbench_generated: "Made on the workbench",
      agent_generated: "Made by an agent",
      derived: "Derived from an artifact",
      user_created: "Written here",
      live: "Live source",
    };
    return { tone: "reachable", origin: labels[file.source] ?? "In the Library" };
  }

  return { tone: "unknown", origin: "" };
}
