import { useEffect } from "react";

import { Button, CopyValue, Fact, Facts, Panel, Subhead } from "../kit";
import { formatMoment } from "../kit/format";
import { levelWord, type LogRow } from "./rows";

// The selected line, in full (epic memql#4895, spec H "Rows").
//
// A Panel below the list rather than a dialog, for the reason the package
// confirm gate is one: a line somebody is reading against the stream above
// it has to stay beside that stream, and a modal that closes is a line
// nobody can re-read. The message is wrapped in the mono voice -- the row
// truncates it and this is where the whole of it lives -- then the facts,
// the subject as a CopyValue (it is one unbreakable id somebody is about to
// paste somewhere), and every attribute as a key/value list.

function renderValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null || value === undefined) return "--";
  try {
    return JSON.stringify(value, null, 1);
  } catch {
    return String(value);
  }
}

export function LogDetail({ row, onClose }: { row: LogRow; onClose: () => void }) {
  // Escape clears the selection from anywhere in the window, not only while
  // the list has focus -- the reader may be in this panel, selecting text.
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const attributes = Object.entries(row.attributes);

  return (
    <Panel label="Log line">
      <div className="os-head">
        <Subhead>
          {levelWord(row.level)}
          {row.component !== "" ? ` · ${row.component}` : ""}
        </Subhead>
        <div className="os-head-actions">
          <Button onClick={onClose} ariaLabel="Close the line">
            Close
          </Button>
        </div>
      </div>
      <p className="os-logs-detail-message os-mono">{row.message}</p>
      <Facts>
        <Fact
          label="When"
          value={row.occurredAt}
          mono
          title={row.occurredAt === "" ? undefined : formatMoment(row.occurredAt)}
        />
        <Fact label="Node" value={row.node} mono />
        <Fact label="Node type" value={row.nodeType} mono />
        <Fact label="Component" value={row.component} mono />
        <Fact label="App" value={row.app} mono />
        <Fact label="Session" value={row.session} mono />
        <Fact label="User" value={row.userId} mono />
        <Fact label="Subject" value={row.subject === "" ? "" : <CopyValue value={row.subject} label="Subject" />} />
        {row.subjectConcept === "" ? null : <Fact label="Subject concept" value={row.subjectConcept} mono />}
      </Facts>
      {attributes.length === 0 ? (
        <p className="os-caption">No attributes on this line.</p>
      ) : (
        <dl className="os-logs-attr-list os-mono" aria-label="Attributes">
          {attributes.map(([key, value]) => (
            <div key={key} className="os-logs-attr">
              <dt>{key}</dt>
              <dd>{renderValue(value)}</dd>
            </div>
          ))}
        </dl>
      )}
    </Panel>
  );
}
