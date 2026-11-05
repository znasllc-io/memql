import { useMemo, useRef, useState, type ReactNode } from "react";

import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { LocalTabs } from "../../../kit/LocalTabs";
import { emailPreview } from "../preview";

// AN EMAIL AS IT IS SENT, IN TWO VIEWS (memql#6033, Ruling 21): Formatted, the
// email's own HTML in a frame at its own width on its own background, and Plain
// text, the text part a send carries. Shared by a template's page, a layout's
// page and the capture inbox, so all three show an email the same way.
//
// THE FRAME CAN NEITHER RUN NOR FETCH ANYTHING: the HTML goes through
// `emailPreview` (which keeps the email's styles and drops whatever would load
// something, and writes a Content-Security-Policy first), and the frame is
// sandboxed without scripts and without an origin of its own. Links may open a
// new tab, which is what a link in an email is for, and nothing else leaves it.
//
// Its look is the stylesheet's (`email.css`), never an inline style.
//
// A PAGE THAT EDITS ONE PART OF AN EMAIL CAN KEEP THAT PART IN VIEW (`place`,
// the Brand page). For the end of the email the document is drawn end-first
// (see `emailPreview`): a scroll box that opens at its end, with no script. The
// page NEVER navigates the frame -- a frame at a fragment address makes browser
// automation refuse every action on the page. Changing group redraws the frame
// (rarely), and a keystroke redraws it as it always did. Once the person
// reaches into the frame (the pointer or the focus) a new email opens at its
// top, theirs, until the page asks again.

/** What the frame may do: open a link in a new tab. No scripts, no same origin, no forms. */
export const PREVIEW_SANDBOX = "allow-popups allow-popups-to-escape-sandbox";

type View = "formatted" | "plain";

/** The part of the email a page keeps in view, asked again with each new `asked`, even for the same part. */
export interface PreviewPlace {
  part: "start" | "end";
  asked: number;
}

const VIEWS: readonly (readonly [View, string])[] = [
  ["formatted", "Formatted"],
  ["plain", "Plain text"],
];

export function Preview({ html, text, loading = false, failed = null, place }: {
  /** The email's HTML; "" for one that has only text, which is then all there is to show. */
  html: string;
  /**
   * The text part a send carries. Absent for a preview drawn as HTML alone (a
   * brand's, Task 24): there is then one view, and no tab bar for it.
   */
  text?: string;
  /** Nothing to show yet: the frame's shape stands in its place. */
  loading?: boolean;
  /** Drawn in the frame's place when the email could not be rendered. */
  failed?: ReactNode;
  /** The part of the email to keep in view. Absent: the frame is the person's alone. */
  place?: PreviewPlace;
}) {
  const [view, setView] = useState<View>("formatted");
  const marked = place !== undefined;
  // Following: the part asked for is kept (end-first) until the person reaches
  // into the frame; a new ask follows again. Read at render, so a redraw that
  // the page causes carries the right document.
  const following = useRef(true);
  const lastAsk = useRef<string>("");
  const ask = place === undefined ? "" : `${place.part}:${place.asked}`;
  if (ask !== lastAsk.current) {
    lastAsk.current = ask;
    following.current = true;
  }
  // Drawn again for a new email or a new ask, never because the person reached in.
  const document = useMemo(
    () => (html === "" ? "" : emailPreview(html, { end: place?.part === "end" && following.current })),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [html, ask],
  );
  const reachedIn = () => {
    following.current = false;
  };

  if (failed !== null && failed !== undefined && failed !== false) return <>{failed}</>;
  // A message with only text has nothing to format.
  if (!loading && html === "") return text === undefined ? null : <pre className="email-preview-text">{text}</pre>;

  return (
    <>
      {text === undefined ? null : <LocalTabs label="Preview" value={view} onChange={setView} options={VIEWS} />}
      {loading ? (
        <ContentSkeleton kind="detail" label="Loading the preview" />
      ) : view === "formatted" || text === undefined ? (
        <iframe
          title="Email preview"
          className="email-preview-frame"
          sandbox={PREVIEW_SANDBOX}
          referrerPolicy="no-referrer"
          srcDoc={document}
          onPointerEnter={marked ? reachedIn : undefined}
          onFocus={marked ? reachedIn : undefined}
        />
      ) : (
        <pre className="email-preview-text">{text}</pre>
      )}
    </>
  );
}
