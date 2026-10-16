import type { ReactNode } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

/** Model output is text: no HTML, embedded media, or executable UI. */
export function AskMessage({ text, trailing }: { text: string; trailing?: ReactNode }) {
  return <div className={`os-ask-answer${trailing ? " os-ask-answer-with-link" : ""}`}><Markdown
    remarkPlugins={[remarkGfm]}
    skipHtml
    components={{
      a: ({ children, href }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
      img: ({ alt }) => <span>{alt}</span>,
    }}
  >{text}</Markdown>{trailing}</div>;
}
