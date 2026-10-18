import { useId, type ReactNode } from "react";
import { Select } from "./controls";

export type ExecutionDestination = "auto" | "workbench" | "computer";

export const EXECUTION_DESTINATIONS: readonly ExecutionDestination[] = ["auto", "workbench", "computer"];

/** One goal's execution choice. Selecting a computer never grants file access. */
export function ExecutionDestinationSelect({ value, onChange, tone, label = "Run work on", icon, title }: {
  value: ExecutionDestination;
  onChange: (value: ExecutionDestination) => void;
  /** "quiet" inside another frame, such as Ask's message field. */
  tone?: "field" | "quiet";
  label?: string;
  icon?: ReactNode;
  title?: string;
}) {
  const id = useId();
  return <Select id={id} label={label} tone={tone} icon={icon} title={title} value={value} onChange={next => onChange(next as ExecutionDestination)}>
    <option value="auto">Auto</option>
    <option value="workbench">Workbench</option>
    <option value="computer">Computer</option>
  </Select>;
}
