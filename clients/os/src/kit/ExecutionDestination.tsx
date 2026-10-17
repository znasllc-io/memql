import { useId } from "react";
import { Select } from "./controls";

export type ExecutionDestination = "auto" | "workbench" | "computer";

/** One goal's execution choice. Selecting a computer never grants file access. */
export function ExecutionDestinationSelect({ value, onChange }: {
  value: ExecutionDestination;
  onChange: (value: ExecutionDestination) => void;
}) {
  const id = useId();
  return <Select id={id} label="Run work on" value={value} onChange={next => onChange(next as ExecutionDestination)}>
    <option value="auto">Auto</option>
    <option value="workbench">Workbench</option>
    <option value="computer">Computer</option>
  </Select>;
}
