import type { ReactNode } from "react";

import { Select } from "../ui";

// A <select> populated from a concept field's DECLARED enum values rather
// than a hardcoded option list (memql#3717, ruling 5). `values` comes from
// concepts/schema.ts's enumValuesForField -- reading the SAME JSON Schema
// document every row already carries on its `schema` intrinsic. A value the
// schema adds later (the planned "server" site kind, epic #3718) appears
// here with no edit to this component or its caller: a data change, not a
// UI rewrite.
export function EnumSelect({
  value,
  onChange,
  values,
  placeholder = "Choose…",
}: {
  value: string;
  onChange: (next: string) => void;
  values: readonly string[];
  placeholder?: string;
}): ReactNode {
  return (
    <Select value={value} onChange={onChange}>
      <option value="">{placeholder}</option>
      {values.map((v) => (
        <option key={v} value={v}>
          {v}
        </option>
      ))}
    </Select>
  );
}
