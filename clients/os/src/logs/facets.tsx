import { ProvenanceDot } from "../kit";
import {
  LEVEL_FLOORS,
  windowShort,
  type LevelFloor,
  type WindowPreset,
} from "./filters";

// The facet controls every logs surface shares (epic memql#4895). They live
// inside the Refine affordance (DESIGN.md rule 2) and speak the shell's one
// selection language, the accent-bordered choice pill -- the same control
// Settings uses for mode and theme, and the Deployables settings for density.

const FLOOR_LABEL: Record<LevelFloor, string> = {
  all: "All",
  info: "Info",
  warn: "Warnings",
  error: "Errors",
};

export function LevelFloorChoice({
  value,
  onChange,
  label = "Level",
}: {
  value: LevelFloor;
  onChange: (next: LevelFloor) => void;
  label?: string;
}) {
  return (
    <div className="os-logs-facet">
      <span className="os-form-field-label" aria-hidden>
        {label}
      </span>
      <div className="os-choice-row os-logs-choice-row" role="radiogroup" aria-label={label}>
        {LEVEL_FLOORS.map((floor) => (
          <button
            key={floor}
            type="button"
            role="radio"
            aria-checked={value === floor}
            className="os-choice"
            onClick={() => onChange(floor)}
          >
            {FLOOR_LABEL[floor]}
          </button>
        ))}
      </div>
    </div>
  );
}

export function WindowChoice({
  value,
  onChange,
  options,
  label = "Window",
}: {
  value: WindowPreset;
  onChange: (next: WindowPreset) => void;
  options: readonly WindowPreset[];
  label?: string;
}) {
  return (
    <div className="os-logs-facet">
      <span className="os-form-field-label" aria-hidden>
        {label}
      </span>
      <div className="os-choice-row os-logs-choice-row" role="radiogroup" aria-label={label}>
        {options.map((preset) => (
          <button
            key={preset}
            type="button"
            role="radio"
            aria-checked={value === preset}
            className="os-choice"
            onClick={() => onChange(preset)}
          >
            {windowShort(preset)}
          </button>
        ))}
      </div>
    </div>
  );
}

/**
 * The scope line's follow control: "Following" with the live dot, or
 * "Paused". Quiet text, not a button-shaped button -- rule 3's sort control
 * is the precedent: click swaps the state, and the accessible name says what
 * a click does.
 */
export function FollowControl({
  following,
  onToggle,
}: {
  following: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      className="os-sort os-logs-follow"
      data-following={following || undefined}
      aria-label={following ? "Following -- click to pause" : "Paused -- click to jump to the latest lines"}
      onClick={onToggle}
    >
      <ProvenanceDot tone={following ? "reachable" : "unreachable"} />
      {following ? "Following" : "Paused"}
    </button>
  );
}
