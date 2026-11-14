import { useId, useState } from "react";

import { Button, Notice, RecordListSkeleton } from "../../kit";
import { SearchPicker } from "../../kit/SearchPicker";
import { useConceptRegistry } from "../concepts/useConceptRegistry";
import { areaLabel } from "./inventory";
import type { UnplacedKind } from "./moves";

export type UnplacedKindChoice = Pick<UnplacedKind, "concept" | "name">;

// Assignment progress only counts engine-managed kinds. Stated kinds may
// still have unassigned rows, so choosing a declaration must not depend on
// those counters or on an account inventory scan. The registry does not say
// whether a kind belongs to accounts: accountRows validates that on opening.
// Neither reading the registry nor choosing a kind writes anything.
export function UnplacedKindPicker({ onChoose }: { onChoose: (kind: UnplacedKindChoice) => void }) {
  const [attempt, setAttempt] = useState(0);
  return <KindChoices key={attempt} onChoose={onChoose} onRetry={() => setAttempt((n) => n + 1)} />;
}

function KindChoices({ onChoose, onRetry }: { onChoose: (kind: UnplacedKindChoice) => void; onRetry: () => void }) {
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
