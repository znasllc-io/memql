import { useId } from "react";

import { Select } from "../../../kit/controls";

// WHICH DESTINATION'S STORE THE PAGE READS, at the right of the Head before
// the period, in the period's manner: the kit's quiet select. Drawn only when
// the two destinations name two stores; the choice is not remembered.

export type DestinationChoice = "production" | "testing";

const OPTIONS: readonly (readonly [DestinationChoice, string])[] = [
  ["production", "Production"],
  ["testing", "Testing"],
];

export function DestinationControl({ value, onChange }: { value: DestinationChoice; onChange: (next: DestinationChoice) => void }) {
  const id = useId();
  return (
    <Select
      tone="quiet"
      id={`store-statistics-destination${id}`}
      label="Destination"
      title="Destination"
      value={value}
      onChange={(next) => {
        if (next === "production" || next === "testing") onChange(next);
      }}
    >
      {OPTIONS.map(([choice, label]) => (
        <option key={choice} value={choice}>
          {label}
        </option>
      ))}
    </Select>
  );
}
