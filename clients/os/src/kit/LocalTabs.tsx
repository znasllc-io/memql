import type { ReactNode } from "react";

/** Sibling views within an app page; top-level sections belong to AppFrame. */
export function LocalTabs<T extends string>({ label, value, onChange, options, actions }: {
  label: string; value: T; onChange: (value: T) => void; options: readonly (readonly [T, string])[];
  actions?: ReactNode;
}) {
  const tabs = <nav className="os-local-tabs" aria-label={label}>{options.map(([id, name]) =>
    <button type="button" data-os-scroll-navigation="reset" key={id} aria-current={value === id ? "page" : undefined} onClick={() => onChange(id)}>{name}</button>
  )}</nav>;
  return actions ? <div className="os-local-tabs-row">{tabs}<div className="os-local-tabs-actions">{actions}</div></div> : tabs;
}
