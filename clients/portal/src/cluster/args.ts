// omitBlank is the one argument-shaping rule the generated typed calls
// need from this app (memql#4232).
//
// The generated builders omit an optional argument when it is UNDEFINED
// and send everything else -- including "". The DSL's `when(args.x)`
// guard drops when its argument is ABSENT, and an empty string is
// present: sending `status: ""` to a query that guards on status
// filters for rows whose status is the empty string, which is nothing
// at all. Form state in this app models "not provided" as "", so every
// migrated call site maps blank optionals through this helper instead
// of each inventing the ternary. (The retired hand-built call layer,
// integrations/calls.ts's callArgs, applied the same rule implicitly to
// every argument; the generated surface makes it a per-field decision,
// which is stricter -- a REQUIRED field can no longer be silently
// dropped by being blank.)
export function omitBlank(value: string): string | undefined {
  return value === "" ? undefined : value;
}
