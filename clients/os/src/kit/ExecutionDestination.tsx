import { useId, type ReactNode } from "react";
import { Select } from "./controls";

export type ExecutionDestination = "auto" | "workbench" | "computer";

export const EXECUTION_DESTINATIONS: readonly ExecutionDestination[] = ["auto", "workbench", "computer"];

/** One goal's execution choice. Selecting a computer never grants file access. */
export function ExecutionDestinationSelect({ value, onChange, tone, label = "Run work on", icon, title, options }: {
  value: ExecutionDestination;
  onChange: (value: ExecutionDestination) => void;
  /** "quiet" inside another frame, such as Ask's message field. */
  tone?: "field" | "quiet";
  label?: string;
  icon?: ReactNode;
  title?: string;
  options?: Partial<Record<ExecutionDestination, { disabled?: boolean; hint?: string }>>;
}) {
  const id = useId();
  return <Select id={id} label={label} tone={tone} icon={icon} title={title} value={value} onChange={next => {
    const destination = next as ExecutionDestination;
    if (!options?.[destination]?.disabled) onChange(destination);
  }}>
    <option value="auto" disabled={options?.auto?.disabled} data-hint={options?.auto?.hint}>Auto</option>
    <option value="workbench" disabled={options?.workbench?.disabled} data-hint={options?.workbench?.hint}>Workbench</option>
    <option value="computer" disabled={options?.computer?.disabled} data-hint={options?.computer?.hint}>Computer</option>
  </Select>;
}
