import { useId, useState } from "react";

import { Button, Notice, RecordListSkeleton } from "../../kit";
import { SearchPicker } from "../../kit/SearchPicker";
import { useConceptRegistry } from "../concepts/useConceptRegistry";
import { areaLabel } from "./inventory";

export interface AccountKindChoice {
  concept: string;
  name: string;
}

// A known kind stays reachable without a complete account inventory or
// assignment counters. The caller chooses this account or no account; the
// registry never infers ownership or whether a kind supports accounts.
// accountRows validates the kind and account when the page opens.
// Neither reading the registry nor choosing a kind writes anything.
export function AccountKindPicker({ onChoose }: { onChoose: (kind: AccountKindChoice) => void }) {
  const [attempt, setAttempt] = useState(0);
  return <KindChoices key={attempt} onChoose={onChoose} onRetry={() => setAttempt((n) => n + 1)} />;
}

function KindChoices({ onChoose, onRetry }: { onChoose: (kind: AccountKindChoice) => void; onRetry: () => void }) {
  const id = useId();
  const registry = useConceptRegistry();

  if (registry.state === "failed") {
    return (
      <Notice tone="error" sentence="The kinds were not read." detail={registry.error}>
        <Button onClick={onRetry}>Try again</Button>
      </Notice>
    );
  }
  if (registry.state === "seeding") return <RecordListSkeleton label="Loading kinds" rows={2} />;

  return (
    <SearchPicker
      id={id}
      label="Kind"
      listLabel="Kinds"
      choices={registry.concepts.map((concept) => ({
        id: concept.id,
        name: concept.entity,
        detail: areaLabel(concept.domain),
        ariaLabel: `${concept.entity}, ${areaLabel(concept.domain)}`,
      }))}
      onChoose={(choice) => onChoose({ concept: choice.id, name: choice.name })}
      emptyText="No kinds match."
      placeholder="Search kinds"
      autoFocus
    />
  );
}
