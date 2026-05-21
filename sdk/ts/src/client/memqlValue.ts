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
    const parts = keys.map((k) => `${k}: ${renderMemQLValue(obj[k])}`);
    return "{" + parts.join(", ") + "}";
  }
  // Symbols, functions, etc. -- emit a JSON string fallback so we
  // surface garbage rather than crash. Callers shouldn't pass these.
  return JSON.stringify(String(value));
}
