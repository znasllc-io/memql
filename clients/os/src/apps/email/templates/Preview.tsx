import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";

import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { LocalTabs } from "../../../kit/LocalTabs";
import { PREVIEW_MARKS, emailPreview } from "../preview";

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
// the Brand page). The document then carries two empty marks, at its start and
// its end, and the page asks the frame to go to one: a fragment of the frame's
// own document, which a page may ask of its frame. It never reaches the
// document, and the sandbox is as it is for every other preview. Each new
// email is a new document, which opens at its top, so the part asked for is
// asked again once it has loaded -- until the person reaches into the frame
// (the pointer or the focus), after which the frame is theirs until the page
// asks again.

/** What the frame may do: open a link in a new tab. No scripts, no same origin, no forms. */
export const PREVIEW_SANDBOX = "allow-popups allow-popups-to-escape-sandbox";

type View = "formatted" | "plain";

/** The part of the email a page keeps in view, asked again with each new `asked`, even for the same part. */
export interface PreviewPlace {
  part: keyof typeof PREVIEW_MARKS;
  asked: number;
}

/**
 * Whether the frame shows the email it was given. The sandboxed document has
 * no origin the page shares, so its address cannot be read; the blank page a
 * frame starts on can be, and is not the email.
 */
function showsEmail(frame: HTMLIFrameElement): boolean {
  try {
    return frame.contentWindow?.location.href.startsWith("about:srcdoc") ?? false;
  } catch {
    return true;
  }
}

/** Ask the frame to go to one of its marks. */
function goTo(frame: HTMLIFrameElement | null, part: PreviewPlace["part"]): void {
  if (frame === null || !showsEmail(frame)) return;
  try {
    frame.contentWindow?.location.replace(`about:srcdoc#${PREVIEW_MARKS[part]}`);
  } catch {
    // A browser that will not move the frame leaves it where the person has it.
  }
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
  const document = useMemo(() => (html === "" ? "" : emailPreview(html, { marks: marked })), [html, marked]);

  // Whether the frame has loaded the email it shows now, and whether it is
  // still kept at the part asked for.
  const frame = useRef<HTMLIFrameElement>(null);
  const loaded = useRef(false);
  const following = useRef(true);
  useLayoutEffect(() => {
    loaded.current = false;
  }, [document]);
  useEffect(() => {
    if (place === undefined) return;
    following.current = true;
    if (loaded.current) goTo(frame.current, place.part);
    // A new ask is a new part, or the same part asked again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [place?.part, place?.asked]);
  const arrived = () => {
    loaded.current = true;
    if (place !== undefined && following.current) goTo(frame.current, place.part);
  };
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
          ref={frame}
          title="Email preview"
          className="email-preview-frame"
          sandbox={PREVIEW_SANDBOX}
          referrerPolicy="no-referrer"
          srcDoc={document}
          onLoad={marked ? arrived : undefined}
          onPointerEnter={marked ? reachedIn : undefined}
          onFocus={marked ? reachedIn : undefined}
        />
      ) : (
        <pre className="email-preview-text">{text}</pre>
      )}
    </>
  );
}
