// The OS kit: the shared surface app epics build against (spec H). One
// import site -- apps must not respell these. LiveList joins in the live
// substrate task (PR B) and is exported from here the moment it exists.

import { deriveProvenance, type MachinePresence, type ProvenanceFacts, type ProvenanceTone } from "../items/provenance";
import type { FileEntry } from "../system/desktop";
import {
  roleAdmits,
  roleGrantSlug,
  roleLadder,
  roleLadderLoaded,
  roleRank,
  roleRungOf,
  type ClusterRole,
  type RoleRequirement,
  type RoleRung,
} from "../system/roles";

export { Caption } from "./Caption";
export { findRegion, revealRegion } from "./reveal";
export {
  Rail,
  nextOpen,
  stopIsOpen,
  stopIsReachable,
  stopOpens,
  stopStateSentence,
  type Stop,
  type StopState,
} from "./Rail";
export { RankMark, RoleTag } from "./RankMark";
export { PeerRowReadOnly, SurfaceRefused } from "./RankStates";
export {
  SetupGroup,
  SurfaceUnconfigured,
  canConfigure,
  gateFor,
  markToneFor,
  moduleActFor,
  stateWords,
  useAppReach,
  type Gate,
  type ModuleAct,
} from "./ReadinessStates";
export {
  Button,
  Check,
  Chip,
  Chips,
  ChoiceStack,
  CopyField,
  CopyValue,
  Fact,
  Facts,
  Field,
  FormRow,
  Head,
  Input,
  Notice,
  Panel,
  Refine,
  Row,
  Select,
  SortControl,
  Subhead,
  type ButtonTone,
  type ChipTone,
  type ChoiceOption,
  type NoticeTone,
  type RefineChip,
} from "./controls";
export { LiveList, type LiveListSource } from "../live/LiveList";
export { useLiveView, type LiveView } from "../live/liveView";
export { useThreeFeedView, useTwoFeedView } from "../live/mergedView";
export { useNow } from "./useNow";
export { formatBytes, formatDuration, formatFreshness, formatMoment } from "./format";
// A figure with its own provenance, and the component that refuses to draw
// a number it does not have (epic memql#5153, D3). Promoted from
// src/cluster/; the pure half is importable on its own as "kit/measure" so a
// .ts module need not pull JSX through this barrel.
export { Measure } from "./MeasureView";
export {
  absent,
  absentSentence,
  figureFrom,
  figureOf,
  figureValue,
  isPositive,
  type AbsentFigure,
  type AbsentReason,
  type Figure,
  type MeasuredFigure,
} from "./measure";

export { boolOr, flatten, stringsOf } from "./rows";
export { deriveProvenance, roleAdmits, roleGrantSlug, roleLadder, roleLadderLoaded, roleRank, roleRungOf };
export type { ClusterRole, MachinePresence, ProvenanceFacts, ProvenanceTone, RoleRequirement, RoleRung };

/**
 * The dot language (spec D3): green = reachable now, amber = not reachable,
 * unknown = NO dot. The same component renders dock "running", connection
 * state and fleet "online" so aliveness reads identically everywhere.
 */
/**
 * The dot's full vocabulary. The three provenance tones are ALIVENESS; the
 * two setup tones are STATE, drawn only while a person is needed (design
 * record 2026-09-06-configuration-readiness, section 5.4).
 *
 * Named by MEANING rather than colour, which is what keeps them from
 * colliding: amber here means "partly set up", and amber in Fleet means
 * "machine unreachable". A tone called `amber` would be reused across both
 * and the two readings would silently merge.
 */
export type DotTone = ProvenanceTone | "needsSetup" | "partlySetUp";

export function ProvenanceDot({
  tone,
  label,
}: {
  tone: DotTone;
  /** Accessible name; the dot itself is not text. */
  label?: string;
}) {
  if (tone === "unknown") return null;
  return (
    <span
      className="os-dot"
      data-os-dot={tone}
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    />
  );
}

export function FileProvenanceDot({
  file,
  machine = null,
}: {
  file: Pick<FileEntry, "source" | "producedByWorkerId" | "uploadState">;
  machine?: MachinePresence | null;
}) {
  const facts = deriveProvenance(file, machine);
  return <ProvenanceDot tone={facts.tone} label={facts.origin || undefined} />;
}
