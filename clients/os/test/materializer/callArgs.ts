// Reads back the call string the SDK's generated builders render, so a test
// can assert on the ARGUMENTS a call carried rather than on its text.
//
// `renderMemQLValue` (sdk/ts/src/client/memqlValue.ts) writes a call as
// `builtin composeMaterialize(name: "Q3", sources: [{kind: "query", ...}], ...)`:
// strings as JSON literals, `nil`, booleans, numbers, arrays, and objects whose
// keys are bare identifiers or JSON strings. That is the whole grammar, so this
// is a small reader for it and not a MemQL parser -- anything outside it throws,
// which is what a test wants from a call it did not expect.
//
// It exists because the Materializer now creates through the SDK helper, which
// speaks only `executeNamed(name, call)`: the harness hands each named mock the
// args parsed from here, so every assertion on `mock.calls[n][0]` keeps reading
// an object, exactly as it did when the app called the typed method directly.

export interface ParsedCall {
  /** `builtin`, `query`, `mutation` or `logic`. */
  kind: string;
  name: string;
  args: Record<string, unknown>;
}

export function parseCall(call: string): ParsedCall {
  const head = /^\s*(\w+)\s+(\w+)\s*\(/.exec(call);
  if (head === null) throw new Error(`not a rendered call: ${call}`);
  const reader = new Reader(call, head[0].length);
  return { kind: head[1] ?? "", name: head[2] ?? "", args: reader.pairs(")") };
}

class Reader {
  constructor(
    private readonly text: string,
    private at: number,
  ) {}

  private fail(expected: string): never {
    throw new Error(`expected ${expected} at ${this.at} in: ${this.text}`);
  }

  private skip(): void {
    while (/\s/.test(this.text[this.at] ?? "")) this.at++;
  }

  private take(char: string): void {
    this.skip();
    if (this.text[this.at] !== char) this.fail(`"${char}"`);
    this.at++;
  }

  /** Reads `key: value` pairs up to and including the closing character. */
  pairs(close: string): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    this.skip();
    if (this.text[this.at] === close) {
      this.at++;
      return out;
    }
    for (;;) {
      this.skip();
      const key = this.text[this.at] === '"' ? this.string() : this.word(/^[A-Za-z_][A-Za-z0-9_]*/, "a key");
      this.take(":");
      out[key] = this.value();
      this.skip();
      const next = this.text[this.at++];
      if (next === close) return out;
      if (next !== ",") this.fail(`"," or "${close}"`);
    }
  }

  private value(): unknown {
    this.skip();
    const char = this.text[this.at];
    if (char === '"') return this.string();
    if (char === "{") {
      this.at++;
      return this.pairs("}");
    }
    if (char === "[") {
      this.at++;
      const items: unknown[] = [];
      this.skip();
      if (this.text[this.at] === "]") {
        this.at++;
        return items;
      }
      for (;;) {
        items.push(this.value());
        this.skip();
        const next = this.text[this.at++];
        if (next === "]") return items;
        if (next !== ",") this.fail('"," or "]"');
      }
    }
    const literal = this.word(/^[^\s,\]})]+/, "a value");
    if (literal === "nil") return null;
    if (literal === "true") return true;
    if (literal === "false") return false;
    const number = Number(literal);
    if (Number.isNaN(number)) this.fail("a value");
    return number;
  }

  private word(pattern: RegExp, expected: string): string {
    const found = pattern.exec(this.text.slice(this.at));
    if (found === null) this.fail(expected);
    this.at += found[0].length;
    return found[0];
  }

  /** A JSON string literal, escapes included. */
  private string(): string {
    let end = this.at + 1;
    while (end < this.text.length && this.text[end] !== '"') end += this.text[end] === "\\" ? 2 : 1;
    if (this.text[end] !== '"') this.fail("a closing quote");
    const literal = this.text.slice(this.at, end + 1);
    this.at = end + 1;
    return JSON.parse(literal) as string;
  }
}
