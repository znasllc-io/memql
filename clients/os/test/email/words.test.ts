import { describe, expect, it } from "vitest";
import { emailState, WORDS } from "../../src/apps/email/words";
describe("Email vocabulary", () => {
  it.each([
    ["campaign", "draft", "Draft"], ["template", "ready", "Published"], ["audience", "archived", "Archived"], ["sender", "disabled", "Archived"], ["layout", "active", "Active"], ["journey", "active", "On"], ["journey", "archived", "Retired"], ["rule", "failed", "Refused"], ["recipient", "pending", "Pending confirmation"], ["delivery", "pending", "Not yet sent"],
  ] as const)("%s/%s reads %s", (noun, status, word) => expect(emailState(noun, status)).toBe(word));
  it("keeps unknown and built-in layout states unstated", () => { expect(emailState("layout", "active", true)).toBe(""); expect(emailState("rule", "surprise")).toBe(""); expect(WORDS.sendingNotSetUp("Acme")).toBe("Sending isn't set up for Acme."); });
});
