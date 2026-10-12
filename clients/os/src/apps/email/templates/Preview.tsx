import { useMemo, useState, type ReactNode } from "react";

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

/** What the frame may do: open a link in a new tab. No scripts, no same origin, no forms. */
export const PREVIEW_SANDBOX = "allow-popups allow-popups-to-escape-sandbox";

type View = "formatted" | "plain";

const VIEWS: readonly (readonly [View, string])[] = [
  ["formatted", "Formatted"],
  ["plain", "Plain text"],
];

export function Preview({ html, text, loading = false, failed = null }: {
  /** The email's HTML; "" for one that has only text, which is then all there is to show. */
  html: string;
  text: string;
  /** Nothing to show yet: the frame's shape stands in its place. */
  loading?: boolean;
  /** Drawn in the frame's place when the email could not be rendered. */
  failed?: ReactNode;
}) {
  const [view, setView] = useState<View>("formatted");
  const document = useMemo(() => (html === "" ? "" : emailPreview(html)), [html]);

  if (failed !== null && failed !== undefined && failed !== false) return <>{failed}</>;
  // A message with only text has nothing to format.
  if (!loading && html === "") return <pre className="email-preview-text">{text}</pre>;

  return (
    <>
      <LocalTabs label="Preview" value={view} onChange={setView} options={VIEWS} />
      {loading ? (
        <ContentSkeleton kind="detail" label="Loading the preview" />
      ) : view === "formatted" ? (
        <iframe title="Email preview" className="email-preview-frame" sandbox={PREVIEW_SANDBOX} referrerPolicy="no-referrer" srcDoc={document} />
      ) : (
        <pre className="email-preview-text">{text}</pre>
      )}
    </>
  );
}
