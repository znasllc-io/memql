import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// ASK TYPES IN THE SAME TEXT IT SHOWS (José, Nov 20 2026 call: "the font that
// shows in the messages does not match the font that we're typing"). The log
// (.os-ask-content) is app content at --os-text-md; the field a person types
// in used the chrome size, --os-text-base, so a sent message grew on arrival.

const css = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

function declaration(selector: string, property: string): string | undefined {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const block = new RegExp(`(?:^|\\n)${escaped}\\s*\\{([^}]*)\\}`).exec(css)?.[1] ?? "";
  return new RegExp(`(?:^|;|\\s)${property}\\s*:\\s*([^;]+)`).exec(block)?.[1]?.trim();
}

describe("Ask's typing field and its messages", () => {
  it("use one text size", () => {
    const messages = declaration(".os-ask-content", "font-size");
    expect(messages).toBe("var(--os-text-md)");
    expect(declaration(".os-ask-field", "font-size")).toBe(messages);
    // Both inherit the one face; neither names its own.
    expect(declaration(".os-ask-field", "font")).toBe("inherit");
  });
});
