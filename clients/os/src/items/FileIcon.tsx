import { FileGlyph, typeLabel } from "../apps/files/Tiles";
import type { DeskLine, Hold } from "./deskFileState";
import type { FileEntry } from "../system/desktop";

// A desktop file icon. Open (double-click / Enter) is the VS Code handoff;
// the desktop layer owns the handoff ports and its no-answer timeout, so
// this component only renders what it is told (spec D3). It is drawn with the
// Library tile's own glyph -- the page with the file's kind written on it --
// at the one size the Library uses too (José, Nov 19 call; Jesús picked
// Draft B, Nov 20 2026), so a file looks the same in both places. No dot: a
// file that cannot open yet is drawn dim, with one line under its name saying
// why (items/deskFileState.ts holds the rules).
const LINES: Record<Exclude<DeskLine, "failed" | null>, string> = {
  offline: "Computer offline",
  ready: "Ready",
};

export function FileIcon({
  entry,
  hold = null,
  line = null,
  glow = false,
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
  /** Just arrived on the desk: a soft glow around the tile, no words. */
  glow?: boolean;
  selected: boolean;
  /** Non-null after a handoff nothing answered; rendered as a status. */
  noAnswerMessage?: string | null;
  onOpen: () => void;
  onSelect: () => void;
  onRetryUpload?: () => void;
}) {
  const dim = hold === "uploading" || hold === "offline";

  return (
    <div className="os-file" data-os-file={entry.id} data-selected={selected || undefined} data-dim={dim || undefined} data-new={glow || undefined}>
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
        <span className="os-files-tile-icon os-file-glyph">
          <FileGlyph label={typeLabel(entry.title, entry.format)} />
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
