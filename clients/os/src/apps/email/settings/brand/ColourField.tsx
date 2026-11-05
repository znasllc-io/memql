import { useEffect, useId, useState, type FocusEvent } from "react";

import { Input } from "../../../../kit";
import { isHexColour } from "./kit";

// ONE COLOUR, TWO WAYS TO GIVE IT, ON ONE CONTROL LINE (DESIGN.md rule 5): the
// browser's colour picker, and the hex for a person who already has the
// brand's value. Local to the Brand page; a second use moves it into the kit.
//
// A hex that is not `#rrggbb` stays in its field as typed, and says so beneath
// it once the person leaves the field rather than on every keystroke of a
// value still being typed. The picker goes on showing the last colour that was
// one, and the page never sends the value (`draftProblem`).

export function ColourField({ id, name, value, onChange }: {
  id: string;
  /** The colour's name, as its row says it: "Primary". It names the hex field, as the row's label does. */
  name: string;
  value: string;
  onChange: (next: string) => void;
}) {
  const problemId = useId();
  // The picker can only show a colour: while the hex is not one, the last that was.
  const [lastColour, setLastColour] = useState(isHexColour(value) ? value.toLowerCase() : "#000000");
  useEffect(() => {
    if (isHexColour(value)) setLastColour(value.toLowerCase());
  }, [value]);
  const swatch = isHexColour(value) ? value.toLowerCase() : lastColour;
  // Whether the person is in the field: a value still being typed is not yet wrong.
  const [inside, setInside] = useState(false);
  const wrong = !isHexColour(value) && !inside;

  function left(event: FocusEvent<HTMLDivElement>) {
    // Moving between the picker and the hex is staying in the field.
    if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setInside(false);
  }

  return (
    <div className="email-colour-field">
      <div className="email-colour" onFocus={() => setInside(true)} onBlur={left}>
        <input
          type="color"
          className="email-colour-swatch"
          aria-label={`Choose the ${name.toLowerCase()} colour`}
          title={`Choose the ${name.toLowerCase()} colour`}
          value={swatch}
          onChange={(event) => onChange(event.target.value)}
        />
        <Input id={id} label={name} value={value} onChange={onChange} code invalid={wrong} describedBy={wrong ? problemId : undefined} />
      </div>
      {wrong ? (
        <p id={problemId} className="email-field-problem" role="alert">
          Use a hex colour like #1a625c.
        </p>
      ) : null}
    </div>
  );
}
