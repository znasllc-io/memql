import { memo, useRef, useState } from "react";
import { FileText } from "lucide-react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

import { Button, Field, Input, Notice, TextArea } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { InPlaceAsk } from "../../../kit/pressHold";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { documentSizeSentence, MAX_DOCUMENT_BYTES, publishWords, type DocumentKind } from "./model";
import { useWholesaleChecks, type Found, type WholesaleScope, type WholesaleWrites } from "./useWholesale";

// A NEW VERSION OF ONE OF THE STORE'S DOCUMENTS (wholesalePublishDocument),
// through the kit's Wizard (DESIGN.md, "One wizard for adding"): its Label,
// its Text -- typed, or loaded from a .md or .txt file -- and a Preview of the
// text as it will read; Publish is the last step's forward act. The kind is
// not a step: the section's own control chose it, and the title names it.
//
// NOTHING LEAVES THE BROWSER UNTIL PUBLISH. A file is read here, by the
// browser, into the text; the engine is called once, by the last act. A file
// the engine would refuse -- past its 204800 bytes, or empty -- is refused as
// it is chosen, in the engine's own sentence, and nothing of it is loaded. A
// file over text already typed asks first, in place.
//
// THE PREVIEW IS DRAWN WHEN ITS STEP IS SHOWN, never on a keystroke: a
// document at the size limit takes most of a second to render. It never sets
// HTML from the text: it is drawn as elements by the renderer the OS already
// uses for Markdown (react-markdown, as Ask's answers are), with any raw HTML
// skipped and images drawn as their words. The storefront has its own
// renderer, which the OS does not carry.
//
// A LOST ANSWER IS NOT PRESSED AGAIN BLIND: versions are append-only, so a
// second Publish would be a second version. The wizard reads the documents
// again and says whether this label and text are the kind's current version.

const READABLE = /\.(md|markdown|txt)$/i;

type Step = "label" | "text" | "preview";
const ORDER: readonly Step[] = ["label", "text", "preview"];

export function PublishDocumentPage({ scope, kind, writes, trail, back, onPublished }: {
  scope: WholesaleScope;
  kind: DocumentKind;
  writes: WholesaleWrites;
  /** The trail down to the Documents tab: the wizard is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  onPublished: () => void;
}) {
  const { findVersion } = useWholesaleChecks(scope);
  const words = publishWords(kind);
  const [step, setStep] = useState<Step>("label");
  const [label, setLabel] = useState("");
  const [body, setBody] = useState("");
  const [fileProblem, setFileProblem] = useState("");
  // A file chosen over text already typed, waiting on the person's answer.
  const [waiting, setWaiting] = useState<{ name: string; text: string } | null>(null);
  const [refusal, setRefusal] = useState("");
  const [settled, setSettled] = useState<Found | null>(null);
  const [checking, setChecking] = useState(false);
  const picker = useRef<HTMLInputElement>(null);
  const publishing = writes.busy === "publish";

  const answered: Record<Step, boolean> = { label: label.trim() !== "", text: body !== "", preview: false };
  const reached = (candidate: Step) => ORDER.slice(0, ORDER.indexOf(candidate)).every((s) => answered[s]);
  const stateOf = (candidate: Step): Stop["state"] =>
    candidate === step ? "open" : !reached(candidate) ? "ahead" : answered[candidate] ? "done" : "waiting";
  const go = (next: Step) => { if (reached(next)) setStep(next); };

  async function load(file: File | undefined) {
    if (file === undefined) return;
    setRefusal("");
    setWaiting(null);
    if (!READABLE.test(file.name)) {
      setFileProblem("Choose a .md or .txt file.");
      return;
    }
    if (file.size === 0 || file.size > MAX_DOCUMENT_BYTES) {
      setFileProblem(documentSizeSentence(file.size));
      return;
    }
    setFileProblem("");
    const text = await file.text();
    if (body.trim() === "") setBody(text);
    else setWaiting({ name: file.name, text });
  }

  async function check() {
    setChecking(true);
    setSettled(await findVersion(kind, label, body));
    setChecking(false);
  }

  async function publish() {
    setRefusal("");
    setSettled(null);
    const { outcome, sentence } = await writes.publishDocument(kind, label, body);
    if (outcome === "done") onPublished();
    else if (outcome === "unknown") await check();
    else setRefusal(sentence);
  }

  const settledSentence = settled === null ? "" :
    settled === "there" ? `The connection dropped before the cluster answered. The version is there: it is ${words.subject}'s current version.`
    : settled === "absent" ? "The connection dropped before the cluster answered. The version is not there: nothing was published."
    : "The connection dropped before the cluster answered, and the documents could not be read. Check again before publishing.";

  const steps: Stop[] = [
    { id: "label", name: "Label", state: stateOf("label"), sentence: "What the version is called.", answer: answered.label ? label.trim() : undefined,
      body: (
        <Field label="Label">
          <Input id="wholesale-document-label" label="Label" value={label} onChange={(v) => { setRefusal(""); setLabel(v); }}
            onEnter={() => { if (answered.label) go("text"); }} />
        </Field>
      ) },
    { id: "text", name: "Text", state: stateOf("text"), sentence: "Markdown, typed or from a file.", answer: answered.text ? `${body.length.toLocaleString()} characters` : undefined,
      body: (
        <>
          <Field label="Text">
            <TextArea id="wholesale-document-text" label="Text" rows={12} value={body} onChange={(next) => { setFileProblem(""); setRefusal(""); setBody(next); }} />
          </Field>
          <div>
            <Button onClick={() => picker.current?.click()}>Load from a file</Button>
            <input ref={picker} type="file" accept=".md,.markdown,.txt,text/markdown,text/plain" hidden aria-hidden tabIndex={-1}
              onChange={(e) => { const file = e.currentTarget.files?.[0]; e.currentTarget.value = ""; void load(file); }} />
          </div>
          {/* A WRONG FILE IS SAID TO A SCREEN READER: an error Notice is an alert. */}
          {fileProblem ? <Notice tone="error" sentence={fileProblem} /> : null}
          {waiting !== null ? (
            <InPlaceAsk>
              <Notice sentence={`Replace the text typed here with ${waiting.name}?`}>
                <div className="os-panel-actions">
                  <Button tone="quiet" onClick={() => setWaiting(null)}>Keep what is typed</Button>
                  <Button tone="primary" onClick={() => { setBody(waiting.text); setWaiting(null); }}>Replace it</Button>
                </div>
              </Notice>
            </InPlaceAsk>
          ) : null}
        </>
      ) },
    { id: "preview", name: "Preview", state: stateOf("preview"), sentence: "The text as it will read.",
      body: step === "preview" ? (
        <>
          <DocumentText text={body} />
          {/* THE ENGINE'S REFUSAL, on the step whose act it answers. */}
          {refusal ? <Notice tone="error" sentence={refusal} /> : null}
          {settledSentence ? <Notice tone={settled === "there" ? "info" : "warn"} sentence={settledSentence} /> : null}
        </>
      ) : null },
  ];

  const cancel: Act = { label: "Cancel", text: true, onAct: back.onSelect };
  const acts: Act[] =
    settled === "there" ? [{ label: "Done", tone: "primary", onAct: onPublished }]
    : settled === "unread" ? [cancel, { label: "Check again", tone: "primary", busy: checking, onAct: () => void check() }]
    : step === "label" ? [cancel, ...(answered.label ? [{ label: "Continue", tone: "primary" as const, onAct: () => go("text") }] : [])]
    : step === "text" ? [cancel, ...(answered.text && waiting === null ? [{ label: "Continue", tone: "primary" as const, onAct: () => go("preview") }] : [])]
    : [cancel, ...(reached("preview") ? [{ label: "Publish", tone: "primary" as const, busy: publishing, onAct: () => void publish() }] : [])];

  const word = publishing ? "Publishing"
    : checking ? "Reading the documents"
    : settled === "there" ? "Published"
    : step === "label" ? "Name the version"
    : step === "text" ? (answered.text ? "Text ready" : "Enter the text")
    : "Ready to publish";

  return (
    <Wizard
      icon={<FileText size={20} aria-hidden />}
      title={words.title}
      breadcrumbs={[...trail, { label: words.title }]}
      back={back}
      label="Publishing a version"
      steps={steps}
      open={step}
      onOpen={(next) => go(next as Step)}
      status={{ word, tone: "none" }}
      acts={acts}
      context={{ page: "Wholesale publish document", kind, step }}
    />
  );
}

/** A document's Markdown, drawn as elements: no HTML from the text, no embedded media. */
export const DocumentText = memo(function DocumentText({ text }: { text: string }) {
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
});
