// renderMemQLValue mirrors sdk/go/client/support.go's renderMemQLValue.
// Converts a JS arg value into its MemQL literal form so the generated
// builders can compose `<name>({...})` call strings from typed args.
//
// Object keys are sorted so the produced literal is byte-identical
// across runs -- the drift gate on the generator depends on it for the
// Go side; we preserve the same property here for consistency.

export function renderMemQLValue(value: unknown): string {
  if (value == null) return "nil";
  if (typeof value === "string") return JSON.stringify(value);
  if (typeof value === "boolean") return value ? "true" : "false";
  if (typeof value === "number") {
    if (!Number.isFinite(value)) return "nil";
    return String(value);
  }
  if (typeof value === "bigint") return value.toString();
  if (Array.isArray(value)) {
    return "[" + value.map(renderMemQLValue).join(", ") + "]";
  }
  if (typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const keys = Object.keys(obj).sort();
    const parts = keys.map((k) => `${renderObjectKey(k)}: ${renderMemQLValue(obj[k])}`);
    return "{" + parts.join(", ") + "}";
  }
  // Symbols, functions, etc. -- emit a JSON string fallback so we
  // surface garbage rather than crash. Callers shouldn't pass these.
  return JSON.stringify(String(value));
}

// bareIdentifier is what the MemQL lexer delivers as a single TokenIdentifier.
const bareIdentifier = /^[A-Za-z_][A-Za-z0-9_]*$/;

// renderObjectKey quotes a key the lexer would not deliver as one identifier.
//
// THE BUG THIS FIXES was live and reachable from the product's own
// documentation: the workers runbook's example label is `has-blender=true`, and
// an object rendered with a bare `has-blender` key lexes as `has`, `-`,
// `blender` -- so the call fails to parse, in a caller that did nothing wrong.
// Anything with a hyphen, a dot or a digit-leading name had the same problem;
// `operatorLabels` made it common, because a label key is user-chosen text
// rather than a schema field name.
//
// The parser's parseObject takes a TokenString key as its FIRST branch, so a
// quoted key is not a workaround -- it is the form the grammar names for
// exactly this case. Keys that ARE identifiers stay bare so nothing else in
// the corpus changes shape.
function renderObjectKey(key: string): string {
  return bareIdentifier.test(key) ? key : JSON.stringify(key);
}
