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
// why (items/deskFileState.ts holds the rules). A failed upload says nothing
// under its name: it stays grey with a small retry arrow on the glyph's
// corner, and opening it tries the upload again (Jesús picked 5B, Nov 22).
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
  const failed = line === "failed";
  const dim = failed || hold === "uploading" || hold === "offline";
  const open = failed ? onRetryUpload : onOpen;

  return (
    <div className="os-file" data-os-file={entry.id} data-selected={selected || undefined} data-dim={dim || undefined} data-new={glow || undefined}>
      <button
        type="button"
        className="os-file-button"
        aria-label={failed ? `${entry.title}. Upload failed. Open to try again.` : `${entry.title} -- open in VS Code`}
        title={failed ? "Upload failed. Open to try again." : undefined}
        onClick={onSelect}
        onDoubleClick={open}
        onKeyDown={(event) => {
          if (event.key === "Enter") open?.();
        }}
      >
        <span className="os-files-tile-icon os-file-glyph">
          <FileGlyph label={typeLabel(entry.title, entry.format)} />
          {failed ? (
            <span className="os-file-retry-badge" role="img" aria-label="Upload failed">
              <svg viewBox="0 0 16 16" aria-hidden="true">
                <path d="M12.5 8a4.5 4.5 0 1 1-1.3-3.2M12.5 2.5v2.8H9.7" />
              </svg>
            </span>
          ) : null}
        </span>
        <span className="os-file-title">{entry.title}</span>
      </button>
      {line && !failed ? (
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
