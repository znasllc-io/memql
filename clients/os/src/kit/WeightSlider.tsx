import { useId } from "react";
import "./weightSlider.css";

export function WeightSlider({
  label,
  hint,
  value,
  recommended,
  readOnly,
  onChange,
}: {
  label: string;
  hint: string;
  value: number;
  recommended: number;
  readOnly: boolean;
  onChange: (value: number) => void;
}) {
  const id = useId();
  return (
    <div className="os-weight" data-read-only={readOnly || undefined}>
      <div className="os-weight-track">
        <input
          id={id}
          className="os-weight-input"
          type="range"
          min={0}
          max={100}
          step={5}
          value={value}
          disabled={readOnly}
          aria-label={label}
          aria-valuetext={`${value}${value === recommended ? ", recommended" : `, recommended ${recommended}`}`}
          style={{ ["--os-weight-fill" as string]: `${value}%` }}
          onChange={(event) => onChange(Number(event.target.value))}
        />
        <span className="os-weight-mark" style={{ left: `${recommended}%` }} aria-hidden title={`Recommended: ${recommended}`} />
      </div>
      <span className="os-weight-value">{value}</span>
      <span className="os-weight-hint">{hint}</span>
    </div>
  );
}
