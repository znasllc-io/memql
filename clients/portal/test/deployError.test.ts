// How the deploy console renders a failed call (memql#3339).
//
// The portal is the consumer the issue singled out: it renders the code as its
// own element and had no VS Code-style helper to compensate with, so pasting
// the SDK's log-formatted `message` in after it printed the code twice and the
// verb once more besides.
//
// The shape it must NOT produce is invisible in a rendering test -- the line
// still appears, it is just wrong -- so it is asserted directly here.

import { describe as suite, expect, test } from "vitest";
import { DeployControlError } from "@znasllc-io/memql-sdk-core/deploy";

import { describeDeployError } from "../src/deploy/useDeployConsole";

suite("describeDeployError", () => {
  test("names the code and the engine's sentence, each exactly once", () => {
    const line = describeDeployError(
      new DeployControlError("rollback_deployment", 7, "requires the owner role"),
    );
    expect(line).toBe("PERMISSION_DENIED: requires the owner role");
  });

  test("does not restate the code or the verb the SDK bakes into `message`", () => {
    const err = new DeployControlError("rollback_deployment", 7, "requires the owner role");
    const line = describeDeployError(err);
    // The regression this fixes: `${err.code}: ${err.message}` rendered
    // "7: deploy console: rollback_deployment: PERMISSION_DENIED: requires the
    // owner role".
    expect(line).not.toContain("deploy console:");
    expect(line).not.toContain("rollback_deployment");
    expect(line.match(/PERMISSION_DENIED/g)).toHaveLength(1);
  });

  test("a refusal with no engine message shows the code alone, not a sentinel", () => {
    // `(no message)` belongs to `message`'s log formatting. It reads fine in a
    // log line and badly as the entire error an operator is shown.
    const line = describeDeployError(new DeployControlError("cut_version", 13, ""));
    expect(line).toBe("INTERNAL");
    expect(line).not.toContain("no message");
  });

  test("an unrecognised code falls back to its number rather than dropping it", () => {
    const line = describeDeployError(new DeployControlError("cut_version", 99, "odd"));
    expect(line).toBe("99: odd");
  });

  test("a non-DeployControlError is passed through unchanged", () => {
    expect(describeDeployError(new Error("socket closed"))).toBe("socket closed");
    expect(describeDeployError("plain string")).toBe("plain string");
  });
});
