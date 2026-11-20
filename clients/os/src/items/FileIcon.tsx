import type { ComponentType } from "react";
import {
  CalendarDays,
  File as FileGlyph,
  FileText,
  ListTodo,
  NotebookPen,
  Radio,
} from "lucide-react";

import { GeneratedGlyph } from "../apps/files/glyphs";
import type { DeskLine, Hold } from "./deskFileState";
import type { FileEntry } from "../system/desktop";

// SECOND MAP, SAME CONCEPT: `apps/files/glyphs.tsx` maps kind -> glyph too, and
// its own comment claims to be the only one. Nothing enforces that they agree.
// They must, so `generated_output` is IMPORTED from there rather than drawn
// again here -- one glyph, two call sites. Consolidating the maps themselves is
// its own job (memql#5263 records it) and is not done here.
//
// The value type is widened from `typeof FileGlyph` to the lucide-shaped
// signature, because the composite glyph is a plain component rather than a
// lucide forwardRef.
const GLYPHS: Record<string, ComponentType<{ size?: number | string; "aria-hidden"?: boolean }>> = {
  document: FileText,
  generated_output: GeneratedGlyph,
  note: NotebookPen,
  todo: ListTodo,
  calendar_event: CalendarDays,
  live_source: Radio,
  file: FileGlyph,
};

// A desktop file icon. Open (double-click / Enter) is the VS Code handoff;
// the desktop layer owns the handoff ports and its no-answer timeout, so
// this component only renders what it is told (spec D3). No dot: a file that
// cannot open yet is drawn dim, with one line under its name saying why or
// what happens next (items/deskFileState.ts holds the rules).
const LINES: Record<Exclude<DeskLine, "failed" | null>, string> = {
  opening: "Opening when done",
  offline: "Computer offline",
  ready: "Ready",
};

export function FileIcon({
  entry,
  hold = null,
  line = null,
  selected,
  noAnswerMessage = null,
  onOpen,
  onSelect,
  onRetryUpload,
}: {
  entry: FileEntry;
  /** What stops it opening right now: drawn dim while uploading or offline. */
  hold?: Hold;
  line?: DeskLine;
  selected: boolean;
  /** Non-null after a handoff nothing answered; rendered as a status. */
  noAnswerMessage?: string | null;
  onOpen: () => void;
  onSelect: () => void;
  onRetryUpload?: () => void;
}) {
  const Glyph = GLYPHS[entry.fileKind] ?? FileGlyph;
  const dim = hold === "uploading" || hold === "offline";

  return (
    <div className="os-file" data-os-file={entry.id} data-selected={selected || undefined} data-dim={dim || undefined}>
      <button
        type="button"
        className="os-file-button"
        aria-label={`${entry.title} -- open in VS Code`}
        onClick={onSelect}
        onDoubleClick={onOpen}
        onKeyDown={(event) => {
          if (event.key === "Enter") onOpen();
        }}
      >
        <span className="os-file-glyph">
          <Glyph size={26} aria-hidden />
        </span>
        <span className="os-file-title">{entry.title}</span>
      </button>
      {line === "failed" ? (
        <button type="button" className="os-file-retry" onClick={onRetryUpload}>
          Upload failed -- retry
        </button>
      ) : line ? (
        <span role="status" className="os-file-line" data-line={line}>
          {LINES[line]}
        </span>
      ) : null}
      {noAnswerMessage ? (
        <span role="status" className="os-file-noanswer">
          {noAnswerMessage}
        </span>
      ) : null}
    </div>
  );
}
