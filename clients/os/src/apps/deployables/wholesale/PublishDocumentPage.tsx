import { useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

import { Button, Details, DetailsSection, Field, Head, Input, Notice, TextArea } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { documentSizeSentence, MAX_DOCUMENT_BYTES, type DocumentKind } from "./model";
import type { WholesaleWrites } from "./useWholesale";

// A NEW VERSION OF ONE OF THE STORE'S DOCUMENTS (wholesalePublishDocument):
// its label and its Markdown text, typed or loaded from a .md or .txt file,
// and a preview of the text as it will read.
//
// NOTHING LEAVES THE BROWSER UNTIL PUBLISH. A file is read here, by the
// browser, into the text; the engine is called once, by the bar's act. A file
// the engine would refuse -- past its 204800 bytes, or empty -- is refused as
// it is chosen, in the engine's own sentence, and nothing of it is loaded.
//
// THE PREVIEW NEVER SETS HTML FROM THE TEXT: it is drawn as elements by the
// renderer the OS already uses for Markdown (react-markdown, as Ask's answers
// are), with any raw HTML in the text skipped and images drawn as their
// words. The storefront has its own renderer, which the OS does not carry.

const READABLE = /\.(md|markdown|txt)$/i;

export function PublishDocumentPage({ kind, kindName, writes, trail, back, onPublished }: {
  kind: DocumentKind;
  /** The document's name, as the Documents tab heads its section. */
  kindName: string;
  writes: WholesaleWrites;
  /** The trail down to the Documents tab: the page is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  onPublished: () => void;
}) {
  const [label, setLabel] = useState("");
  const [body, setBody] = useState("");
  const [fileProblem, setFileProblem] = useState("");
  const picker = useRef<HTMLInputElement>(null);
  const publishing = writes.busy === "publish";
  const ready = label.trim() !== "" && body !== "";

  async function load(file: File | undefined) {
    if (file === undefined) return;
    writes.clearRefusal();
    if (!READABLE.test(file.name)) {
      setFileProblem("Choose a .md or .txt file.");
      return;
    }
    if (file.size === 0 || file.size > MAX_DOCUMENT_BYTES) {
      setFileProblem(documentSizeSentence(file.size));
      return;
    }
    setFileProblem("");
    setBody(await file.text());
  }

  async function publish() {
    const outcome = await writes.publishDocument(kind, label, body);
    if (outcome === "done") onPublished();
  }

  const acts: Act[] = [
    { label: "Cancel", text: true, onAct: back.onSelect },
    ...(ready ? [{ label: "Publish", tone: "primary" as const, busy: publishing, onAct: () => void publish() }] : []),
  ];

  return (
    <ReviewsPane
      heading={<Head navigationKey={`wholesale-publish:${kind}`} title="Publish a new version" meta={kindName} breadcrumbs={[...trail, { label: "Publish a new version" }]} back={back} />}
      bar={<ActionBar state="" tone={publishing ? "busy" : "none"} acts={acts} />}
      context={{ page: "Wholesale publish document", kind }}>
      <div className="os-review-step">
        <Field label="Label">
          <Input id="wholesale-document-label" label="Label" value={label} onChange={setLabel} />
        </Field>
        <Field label="Text">
          <TextArea id="wholesale-document-text" label="Text" rows={12} value={body} onChange={(next) => { setFileProblem(""); setBody(next); }} />
        </Field>
        <div>
          <Button onClick={() => picker.current?.click()}>Load from a file</Button>
          <input ref={picker} type="file" accept=".md,.markdown,.txt,text/markdown,text/plain" hidden aria-hidden tabIndex={-1}
            onChange={(e) => { const file = e.currentTarget.files?.[0]; e.currentTarget.value = ""; void load(file); }} />
        </div>
        {fileProblem ? <Notice tone="warn" sentence={fileProblem} /> : null}
      </div>
      {body !== "" ? (
        <Details>
          <DetailsSection title="Preview">
            <DocumentText text={body} />
          </DetailsSection>
        </Details>
      ) : null}
      {/* THE ENGINE'S REFUSAL, standing on the bar it answers. */}
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
    </ReviewsPane>
  );
}

/** A document's Markdown, drawn as elements: no HTML from the text, no embedded media. */
export function DocumentText({ text }: { text: string }) {
  return (
    <div className="os-wholesale-document">
      <Markdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          a: ({ children, href }) => <a className="os-link" href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
          img: ({ alt }) => <span>{alt}</span>,
        }}>
        {text}
      </Markdown>
    </div>
  );
}
