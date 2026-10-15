import { useEffect, useState } from "react";
import { Info } from "lucide-react";
import { DialogFrame } from "./DialogFrame";
import { usePageHelp } from "./pageHelp";

/** The only information control in its page or modal. */
export function PageHelpButton({ title, pageKey = title, always = false }: { title: string; pageKey?: string; always?: boolean }) {
  const sections = usePageHelp();
  const [openFor, setOpenFor] = useState<string | null>(null);
  useEffect(() => setOpenFor(null), [pageKey]);
  if (!always && sections.length === 0) return null;
  return <>
    <button type="button" className="os-trail-back os-trail-help" aria-label={`Help with ${title}`} title={`Help with ${title}`} aria-haspopup="dialog" onClick={() => setOpenFor(pageKey)}>
      <Info size={14} aria-hidden />
    </button>
    {openFor === pageKey ? <DialogFrame title={title} subtitle="Help" closeLabel="Close help" onDismiss={() => setOpenFor(null)} bodyClassName="os-dialog-body os-page-help-body">
      {sections.length ? sections.map(section => <section key={section.id} aria-labelledby={`${section.id}-help`}>
        <h4 id={`${section.id}-help`}>{section.title}</h4>
        {section.children}
      </section>) : <p>Documentation is coming soon.</p>}
    </DialogFrame> : null}
  </>;
}
