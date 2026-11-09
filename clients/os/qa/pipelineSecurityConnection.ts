import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { parseCall } from "../test/callArgs";
import { fakeConnection, rowsResult } from "../test/deployables/harness";

/** Browser-only fixture: switching changes this in-memory row, never a cluster. */
export function pipelineSecurityConnection(state: "off" | "on" | "unavailable" | "refused") {
  const fake = fakeConnection();
  const base = fake.query.executeNamed.bind(fake.query);
  let row: Row | null = state === "on" ? {
    id: "var-global-memql-pipelines-security", name: "MEMQL_PIPELINES_SECURITY", value: '{"enabled":true}',
  } : null;
  (fake.query as unknown as { executeNamed: unknown }).executeNamed = async (name: string, call: string, opts?: unknown) => {
    const parsed = parseCall(call);
    if (parsed.name === "globalVariable" && parsed.args["name"] === "MEMQL_PIPELINES_SECURITY") {
      if (state === "unavailable") throw new Error("The cluster could not read its saved configuration.");
      return rowsResult(row ? [row] : []);
    }
    if (parsed.name === "setGlobalVariable" && parsed.args["name"] === "MEMQL_PIPELINES_SECURITY") {
      if (state === "refused") throw new Error("You do not have permission to change this installation's settings.");
      row = parsed.args;
      return rowsResult([]);
    }
    return base(name, call, opts as never);
  };
  return fake;
}
