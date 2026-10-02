import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";

export interface DnsRecordValue { kind: string; name: string; value: string; purpose: string }

/**
 * One DNS record, in the three parts a registrar's own form asks for.
 *
 * TYPE / NAME / VALUE, separately copyable, because that is literally the shape
 * of the task: three fields, in another application, in another tab. A single
 * "copy record" button would hand somebody a line they then have to take apart.
 */
export function DnsRecord({ record, awaited = false, status }: { record: DnsRecordValue; awaited?: boolean; status?: string }) {
  return (
    <div className="os-record" data-awaited={awaited} role="group" aria-label={record.purpose}>
      <div className="os-record-parts">
        {/* TYPE IS NOT COPYABLE, and that is the point rather than an
            oversight: every registrar offers it as a dropdown, so nobody
            pastes "TXT". A copy button there would be an affordance for
            something no one does, crowding the two that matter. */}
        <div className="os-record-part">
          <span className="os-record-label">Type</span>
          <span className="os-record-kind">{record.kind}</span>
        </div>
        <RecordPart label="Name" value={record.name} grow />
        <RecordPart label="Value" value={record.value} grow />
      </div>
      <p className="os-record-purpose">{record.purpose}{status ? <span className="os-record-status">{status}</span> : null}</p>
    </div>
  );
}

function RecordPart({ label, value, grow = false }: { label: string; value: string; grow?: boolean }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1200);
    } catch {
      // A CLIPBOARD REFUSAL IS NOT AN ERROR TO REPORT. The value is on screen
      // and selectable, so the fallback is the one somebody already has; a
      // notice here would be a message about the browser rather than about the
      // domain.
      setCopied(false);
    }
  }

  return (
    <div className="os-record-part" data-grow={grow}>
      <span className="os-record-label">{label}</span>
      <button
        type="button"
        className="os-record-value"
        onClick={() => void copy()}
        title={copied ? "Copied" : `Copy ${label.toLowerCase()}`}
        aria-label={`Copy ${label.toLowerCase()}: ${value}`}
      >
        <code>{value}</code>
        {copied ? <Check size={11} aria-hidden /> : <Copy size={11} aria-hidden />}
      </button>
    </div>
  );
}
