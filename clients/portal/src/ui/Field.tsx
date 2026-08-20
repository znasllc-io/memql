import type { ReactNode } from "react";

// The form vocabulary: one inset style for every text-shaped input, and one
// Field wrapper that owns the label / hint / error lines. Before this file
// the portal carried two Fields and three input recipes that disagreed on
// background, radius and padding; a form should not reveal which page it was
// written on.
//
// Inputs take (value, onChange: string) rather than the raw event -- every
// existing call site already worked at that level, and the event is noise to
// a form that only wants the text.

const INSET =
  "w-full rounded border border-line bg-surface px-3 py-2 text-sm text-fg " +
  "placeholder:text-subtle disabled:cursor-not-allowed disabled:opacity-40";

export function Field({
  label,
  hint,
  error,
  grow = false,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  // Let the field flex inside a wrapping form row.
  grow?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <label className={"flex flex-col gap-1 " + (grow ? "min-w-48 flex-1" : "")}>
      <span className="text-xs font-medium text-muted">{label}</span>
      {children}
      {hint === undefined ? null : <span className="text-xs text-subtle">{hint}</span>}
      {error === undefined ? null : (
        <span className="text-xs text-danger" role="alert">
          {error}
        </span>
      )}
    </label>
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  type = "text",
  disabled = false,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  type?: string;
  disabled?: boolean;
}): ReactNode {
  return (
    <input
      type={type}
      value={value}
      disabled={disabled}
      {...(placeholder === undefined ? {} : { placeholder })}
      onChange={(event) => onChange(event.target.value)}
      className={INSET}
    />
  );
}

export function Textarea({
  value,
  onChange,
  placeholder,
  rows = 4,
  disabled = false,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  rows?: number;
  disabled?: boolean;
}): ReactNode {
  return (
    <textarea
      value={value}
      rows={rows}
      disabled={disabled}
      {...(placeholder === undefined ? {} : { placeholder })}
      onChange={(event) => onChange(event.target.value)}
      className={INSET + " font-mono"}
    />
  );
}

export function Select({
  value,
  onChange,
  disabled = false,
  ariaLabel,
  children,
}: {
  value: string;
  onChange: (next: string) => void;
  disabled?: boolean;
  ariaLabel?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <select
      value={value}
      disabled={disabled}
      {...(ariaLabel === undefined ? {} : { "aria-label": ariaLabel })}
      onChange={(event) => onChange(event.target.value)}
      className={INSET}
    >
      {children}
    </select>
  );
}
