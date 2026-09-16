import { describe, expect, it } from "vitest";
import { resolveLocalCockpit } from "../../src/apps/fleet/addMachine/localInstall";
import { installCommand, uninstallCommand } from "../../src/apps/fleet/addMachine/install";
import { policyDescription } from "../../src/apps/fleet/taskPolicies";

const source = { base: "http://127.0.0.1:4330", version: "0.15.0-dev.fixture" };
describe("explicit local Cockpit test build", () => {
  it("requires the local OS, local cluster, literal loopback and a safe dev version", () => {
    expect(resolveLocalCockpit(source, "os.memql.localhost", "memql.localhost")).toEqual(source);
    for (const [value, host, domain] of [
      [source, "os.example.com", "memql.localhost"], [source, "os.memql.localhost", "example.com"],
      [{ ...source, base: "http://example.com:4330" }, "os.memql.localhost", "memql.localhost"],
      [{ ...source, base: "http://127.0.0.1:4330/?run=x" }, "os.memql.localhost", "memql.localhost"],
      [{ ...source, version: "0.15.0-dev.$(whoami)" }, "os.memql.localhost", "memql.localhost"],
    ] as const) expect(resolveLocalCockpit(value, host, domain)).toBeNull();
  });
  it("uses the matching frozen scripts, version and assets for install and scoped uninstall", () => {
    const clusterUrl = "https://api.memql.localhost";
    const command = installCommand({ platform: "mac", token: "test-token", clusterUrl, computerUse: false, inference: false, localTest: source });
    expect(command).toContain("http://127.0.0.1:4330/scripts/install/install-mac.sh");
    expect(command).toContain("MEMQL_INSTALL_VERSION=0.15.0-dev.fixture");
    expect(command).toContain("MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=1");
    expect(command).toContain("--token test-token --cluster https://api.memql.localhost --computeruse --user-local");
    expect(command).toContain("--download-base=http://127.0.0.1:4330/releases/download/v0.15.0-dev.fixture");
    expect(command).not.toMatch(/[\n\r]/);
    const uninstall = uninstallCommand("mac", { localTest: source, clusterUrl });
    expect(uninstall).toContain("/scripts/install/uninstall-mac.sh");
    expect(uninstall).toContain("--user-local --cluster=https://api.memql.localhost");
    expect(uninstall).not.toContain("--purge");
    expect(uninstallCommand("mac", { localTest: source, clusterUrl, purge: true })).toContain("--purge");
  });
  it("leaves the published path intact outside the explicit override", () => {
    expect(installCommand({ platform: "mac", token: "test", clusterUrl: "https://api.example.com", computerUse: false, inference: false })).toBe("curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-mac.sh | bash -s -- --token test --cluster https://api.example.com");
  });
});

it("uses product descriptions for shipped defaults while preserving custom descriptions", () => {
  const policy = { name: "embeddingsBinding", chain: ["embedder:active"], description: "epic memql#5137 INTERNAL NOTES", shipped: true };
  expect(policyDescription(policy)).not.toMatch(/epic|INTERNAL/);
  expect(policyDescription({ ...policy, customized: true, description: "Our policy" })).toBe("Our policy");
});
