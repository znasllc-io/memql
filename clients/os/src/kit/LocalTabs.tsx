import type { ReactNode } from "react";

/** Sibling views within an app page; top-level sections belong to AppFrame. */
export function LocalTabs<T extends string>({ label, value, onChange, options, adornment }: {
  label: string; value: T; onChange: (value: T) => void; options: readonly (readonly [T, string])[];
  adornment?: (value: T) => ReactNode;
}) {
  return <nav className="os-local-tabs" aria-label={label}>{options.map(([id, name]) =>
    <button type="button" key={id} className={adornment ? "os-attention-anchor" : undefined} aria-current={value === id ? "page" : undefined} onClick={() => onChange(id)}>{name}{adornment?.(id)}</button>
  )}</nav>;
}
