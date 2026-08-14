import type { ReactNode } from "react";

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
    <select
      value={value}
      onChange={(event) => onChange(event.target.value)}
      className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm text-fg"
    >
      <option value="">{placeholder}</option>
      {values.map((v) => (
        <option key={v} value={v}>
          {v}
        </option>
      ))}
    </select>
  );
}
