import { useState, type FormEvent } from "react";

// One component, two hosts (memql#4706): desktop/iPad strip vs phone sheet.
// Always on. Not a module. Does not take a slot. Text is first-class; voice
// is the existing MemQL path and is not stacked here. Off or mic-refused
// means this well still works.

export type ResearchHost = "strip" | "sheet";

export type ResearchVoice = "off" | "existing";

export function Research({
  host,
  voice = "off",
}: {
  host: ResearchHost;
  voice?: ResearchVoice;
}) {
  const [draft, setDraft] = useState("");
  const [lines, setLines] = useState<string[]>([]);

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const text = draft.trim();
    if (!text) return;
    setLines((prev) => [...prev, text]);
    setDraft("");
  }

  return (
    <aside
      className="os-research"
      data-os-research={host}
      data-os-host={host}
      data-os-research-voice={voice}
      aria-label="Research"
    >
      <h2 className="os-research-title">Research</h2>
      <ol className="os-research-log">
        {lines.map((line, index) => (
          <li key={`${index}-${line}`}>{line}</li>
        ))}
      </ol>
      <form className="os-research-form" onSubmit={onSubmit}>
        <label className="os-sr-only" htmlFor={`os-research-input-${host}`}>
          Research
        </label>
        <textarea
          id={`os-research-input-${host}`}
          className="os-research-well"
          data-os-research-input
          aria-label="Research"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          placeholder="Ask in text"
          rows={host === "sheet" ? 3 : 2}
        />
        <button type="submit" className="os-primary">
          Send
        </button>
      </form>
    </aside>
  );
}
