export function LocalTabs<T extends string>({ label, value, onChange, options }: {
  label: string; value: T; onChange: (value: T) => void; options: readonly (readonly [T, string])[];
}) {
  return <nav className="os-local-tabs" aria-label={label}>{options.map(([id, name]) =>
    <button type="button" key={id} aria-current={value === id ? "page" : undefined} onClick={() => onChange(id)}>{name}</button>
  )}</nav>;
}
