import { useEffect, useState } from "react";

import { Fact, Facts, Panel, Subhead } from "../../kit";
import { InfoDetail } from "../../kit/InfoDetail";
import type { ReuseFacts } from "./reuse";
import { useReusableAfter } from "./useInterventions";
import { useSetConstructReuse, type ReuseWrite } from "./useAutomations";
import { reuseMeaning, reuseWord, usedForSentence, type ReuseLabel } from "./words";

// WHAT AN AUTOMATION IS FOR -- the evidence's answer, and the person's own
// (epic memql#5414, design D24).
//
// ===========================================================================
// THE CHOICE IS THE PERSON'S, THE FACTS ARE THE EVIDENCE'S
// ===========================================================================
// Four pills in the shell's one selection language: Automatic -- follow what
// the evidence decides -- or a label of their own. Under them, what the
// evidence says, always, even while their own label overrules it: the design
// keeps both, and a person who overruled the evidence should see when it has
// come round to agree (or has not).
//
// ===========================================================================
// SAVED IS SAVED
// ===========================================================================
// A pill turns checked when the server confirmed the write, never on the
// click (SUPERVISED-VISUAL-COMPOSITION.md). The confirmed label is held until
// the construct's feed carries the same override version, so a slow feed
// does not flick the choice back to what it was.

const CHOICES: ReadonlyArray<{ value: ReuseLabel | ""; label: string }> = [
  { value: "", label: "Automatic" },
  { value: "reusable", label: reuseWord("reusable") },
  { value: "goalSpecific", label: reuseWord("goalSpecific") },
  { value: "accountSpecific", label: reuseWord("accountSpecific") },
];

export interface ReusePanelProps {
  constructId: string;
  facts: ReuseFacts;
}

/** Keyed on the construct by its caller: another automation is another answer. */
export function ReusePanel({ constructId, facts }: ReusePanelProps) {
  const write = useSetConstructReuse();
  const reusableAfter = useReusableAfter();
  const [confirmed, setConfirmed] = useState<ReuseWrite | null>(null);

  // THE FEED HAS CAUGHT UP once the row carries the version this window wrote.
  useEffect(() => {
    if (confirmed !== null && facts.overrideVersion >= confirmed.version) setConfirmed(null);
  }, [confirmed, facts.overrideVersion]);

  const chosen: ReuseLabel | "" = confirmed !== null ? confirmed.label : facts.override;
  const busy = write.busy === constructId;
  const error = write.errors[constructId] ?? "";

  async function choose(value: ReuseLabel | ""): Promise<void> {
    // Choosing what is already chosen says nothing new, so it writes nothing.
    if (busy || value === chosen) return;
    const reply = await write.set(constructId, value === "" ? "evidence" : value);
    if (reply !== null) setConfirmed(reply);
  }

  return (
    <Panel label="Reuse">
      <div className="os-record-heading">
        <Subhead>Reuse</Subhead>
        <InfoDetail title="Reuse labels">
          <Facts>
            {CHOICES.map((choice) => (
              <Fact key={choice.label} label={choice.label} value={reuseMeaning(choice.value, reusableAfter)} />
            ))}
          </Facts>
          <p className="os-caption">
            Your own label wins. How the automation is used keeps being counted underneath it.
          </p>
        </InfoDetail>
      </div>
      <div className="os-choice-row os-nexus-reuse-choices" role="radiogroup" aria-label="Reuse label">
        {CHOICES.map((choice) => (
          <button
            key={choice.label}
            type="button"
            role="radio"
            className="os-choice"
            aria-checked={chosen === choice.value}
            disabled={busy}
            aria-busy={busy || undefined}
            onClick={() => void choose(choice.value)}
          >
            {choice.label}
          </button>
        ))}
      </div>
      {error === "" ? null : (
        <p className="os-nexus-reuse-error os-mono" role="alert">
          {error}
        </p>
      )}
      <Facts>
        <Fact label="From its use" value={reuseWord(facts.evidence)} />
        {facts.signatureCount === null ? null : (
          <Fact
            label="Used for"
            value={usedForSentence(facts.signatureCount, facts.uses, reusableAfter, facts.evidence)}
          />
        )}
        {facts.accountCount === 0 ? null : (
          <Fact
            label="Accounts"
            value={facts.accountCount === 1 ? "every use was for one account" : `${facts.accountCount} accounts`}
          />
        )}
      </Facts>
    </Panel>
  );
}
